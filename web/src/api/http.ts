import { DEV_TOKEN } from '../env';

// A Secure server uses the __Host- names. A local dev server (LOG_ENV=local) uses the bare ones.
export const CSRF_COOKIES = ['__Host-odt_csrf', 'odt_csrf'] as const;
export const CSRF_HEADER = 'X-CSRF-Token';
const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);
/** Routes used before a session exists: there is no CSRF token to send yet. */
const CSRF_EXEMPT = new Set([
  '/api/v1/auth/otp',
  '/api/v1/auth/otp/verify',
  '/api/v1/setup',
  // the three public sign-up routes. They read no cookie, so a signed-out person has no CSRF token.
  '/api/v1/signups',
  '/api/v1/signups/confirm',
  '/api/v1/invites/accept',
]);
// LAUN-36: the one-click unsubscribe route. Public, reads no cookie, and the token is in the path.
const CSRF_EXEMPT_PREFIX = '/api/v1/notifications/unsubscribe/';

/** Thrown, without any network call, when an unsafe request has no CSRF token. */
export class CsrfMissingError extends Error {
  constructor() {
    super('No CSRF token is available for this request.');
    this.name = 'CsrfMissingError';
  }
}

/**
 * Thrown, without any network call, after a refused CSRF retry showed that ANOTHER user is now signed in.
 * Writes stay blocked until the session reloads as that user, so nothing is sent as the wrong person.
 */
export class SessionChangedError extends Error {
  constructor() {
    super('The signed-in user changed. Reload the session before writing.');
    this.name = 'SessionChangedError';
  }
}
let writesBlocked = false;
export function setWritesBlocked(blocked: boolean): void {
  writesBlocked = blocked;
}
export function getWritesBlocked(): boolean {
  return writesBlocked;
}

// The session epoch goes up by one every time the signed-in user changes (login as another user, logout,
// or another user found by a refused CSRF retry). A write remembers the epoch it started in. If the
// epoch has moved before it is sent, or before a retry is sent, it was made as someone else: it is not sent.
let sessionEpoch = 0;
export function getSessionEpoch(): number {
  return sessionEpoch;
}
export function bumpSessionEpoch(): void {
  sessionEpoch++;
}
/** Signing out only ends a session, so it is allowed while writes are blocked. */
const BLOCK_EXEMPT = new Set(['/api/v1/auth/logout']);

export function readCookie(name: string, cookieString: string = document.cookie): string | undefined {
  for (const part of cookieString.split(';')) {
    const i = part.indexOf('=');
    if (i < 0) continue;
    if (part.slice(0, i).trim() === name) {
      const v = part.slice(i + 1).trim();
      try {
        return decodeURIComponent(v) || undefined;
      } catch {
        return v || undefined;
      }
    }
  }
  return undefined;
}

// The login and /auth/session replies carry the CSRF token. We keep it in memory
// (never in storage) and prefer it to the cookie, which is only the fallback.
let memoryCsrf: string | undefined;
export function setMemoryCsrf(token: string | undefined): void {
  memoryCsrf = token || undefined;
}
export function currentCsrf(): string | undefined {
  if (memoryCsrf) return memoryCsrf;
  for (const name of CSRF_COOKIES) {
    const v = readCookie(name);
    if (v) return v;
  }
  return undefined;
}

// After a 403 csrf_failed the in-memory token may be stale (the session was rotated, or the
// cookie changed). auth.ts registers a function that reloads it from GET /auth/session.
let csrfRefresher: (() => Promise<boolean>) | undefined;
export function setCsrfRefresher(fn: (() => Promise<boolean>) | undefined): void {
  csrfRefresher = fn;
}

// Dev bearer token: memory only, from the env var, never persisted.
let bearer: string | undefined = DEV_TOKEN;
export function getBearer(): string | undefined {
  return bearer;
}
/** Test hook. */
export function setBearerForTests(token: string | undefined): void {
  bearer = token;
}
export function usingBearer(): boolean {
  return bearer !== undefined;
}

export interface SecureFetchOptions {
  fetchImpl?: typeof fetch;
  getBearer?: () => string | undefined;
  getCsrf?: () => string | undefined;
  /** Reloads the token. Resolves true when the request may be sent again, false when it must not. */
  refreshCsrf?: () => Promise<boolean> | undefined;
}

/**
 * The only function that talks to the network. It
 *  - sends cookies (`credentials: include`),
 *  - adds `X-CSRF-Token` to every unsafe request made with the cookie session,
 *    and throws CsrfMissingError WITHOUT sending when there is none,
 *  - uses the dev bearer token instead (no CSRF needed) when one is set.
 */
export function createSecureFetch(opts: SecureFetchOptions = {}) {
  const impl = opts.fetchImpl ?? ((r: Request) => fetch(r));
  async function attempt(input: Request): Promise<{ res: Response; unsafeCookie: boolean }> {
    const token = (opts.getBearer ?? getBearer)();
    const headers = new Headers(input.headers);
    const method = input.method.toUpperCase();
    let unsafeCookie = false;
    if (token) {
      headers.set('Authorization', `Bearer ${token}`);
    } else if (!SAFE_METHODS.has(method)) {
      let exempt = false;
      try {
        const path = new URL(input.url).pathname;
        exempt = CSRF_EXEMPT.has(path) || path.startsWith(CSRF_EXEMPT_PREFIX);
      } catch {
        exempt = false;
      }
      if (!exempt) {
        let blockExempt = false;
        try {
          blockExempt = BLOCK_EXEMPT.has(new URL(input.url).pathname);
        } catch {
          blockExempt = false;
        }
        if (writesBlocked && !blockExempt) throw new SessionChangedError();
        const csrf = (opts.getCsrf ?? currentCsrf)();
        if (!csrf) throw new CsrfMissingError();
        headers.set(CSRF_HEADER, csrf);
        unsafeCookie = true;
      }
    }
    return { res: await impl(new Request(input, { headers, credentials: 'include' })), unsafeCookie };
  }
  return async function secureFetch(input: Request): Promise<Response> {
    const epoch0 = getSessionEpoch(); // the session this request was built in
    const retryCopy = input.clone(); // the body of `input` is spent after the first send
    const first = await attempt(input);
    if (first.res.status !== 403 || !first.unsafeCookie) return first.res;
    let code: string | undefined;
    try {
      code = ((await first.res.clone().json()) as { code?: string })?.code;
    } catch {
      code = undefined;
    }
    if (code !== 'csrf_failed') return first.res;
    // The session changed while this request was out: do not replay it as whoever is signed in now.
    if (getSessionEpoch() !== epoch0) return first.res;
    // Reload the token once and try again. If it is still refused, the caller sees the 403.
    const refresh = opts.refreshCsrf ?? (() => csrfRefresher?.());
    try {
      if ((await refresh()) !== true) return first.res;
    } catch {
      return first.res;
    }
    if (getSessionEpoch() !== epoch0) return first.res; // changed during the reload
    return (await attempt(retryCopy)).res;
  };
}

export const secureFetch = createSecureFetch();
