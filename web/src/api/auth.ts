// Login routes (openapi.yaml, tag auth):
//   POST /auth/otp {email} -> 202 always; 429 carries Retry-After.
//   POST /auth/otp/verify {email, code} -> 200 {user, orgs, csrfToken}; sets the
//     session cookie (HttpOnly) and the CSRF cookie.
//   GET  /auth/session -> same body, for a reload. POST /auth/logout -> 204.
//   GET/POST /setup: first-run setup. The POST answers like a login plus the first token.
// Cookie names are __Host-odt_* on a Secure server and odt_* on a local one.
import { apiBase } from '../env';
import {
  CsrfMissingError,
  SessionChangedError,
  bumpSessionEpoch,
  getWritesBlocked,
  secureFetch,
  setCsrfRefresher,
  setMemoryCsrf,
  setWritesBlocked,
} from './http';
import { ApiError, networkError, toApiError } from './errors';
import type {
  InviteAcceptInput,
  Session,
  SessionOrg,
  SessionUser,
  SetupRequest,
  SignupCreateRequest,
} from './types';

async function send(path: string, method: string, body?: unknown): Promise<Response> {
  const req = new Request(`${apiBase()}${path}`, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  try {
    return await secureFetch(req);
  } catch (e) {
    if (e instanceof CsrfMissingError || e instanceof SessionChangedError) throw e;
    throw networkError();
  }
}

async function readJson(res: Response): Promise<unknown> {
  try {
    return await res.json();
  } catch {
    return undefined;
  }
}

/** Always resolves for a 202. Never says whether the address exists. */
export async function requestOtp(email: string): Promise<void> {
  const res = await send('/api/v1/auth/otp', 'POST', { email });
  if (res.status === 202 || res.ok) return;
  throw toApiError(res, await readJson(res));
}

export interface LoginResult extends Session {
  csrfToken: string;
}

export async function verifyOtp(email: string, code: string): Promise<LoginResult> {
  const res = await send('/api/v1/auth/otp/verify', 'POST', { email, code });
  const body = await readJson(res);
  if (!res.ok) throw toApiError(res, body);
  const b = body as { user?: SessionUser; orgs?: SessionOrg[]; csrfToken?: string } | undefined;
  if (!b?.user || !Array.isArray(b.orgs))
    throw new ApiError(res.status, 'bad_response', 'The server sent an unexpected reply.');
  setMemoryCsrf(b.csrfToken);
  noteUser(b.user.id);
  setWritesBlocked(false);
  return { user: b.user, orgs: b.orgs, csrfToken: b.csrfToken ?? '' };
}

/** GET /setup: true while the install has no user. Public. */
export async function fetchSetupRequired(): Promise<boolean> {
  const res = await send('/api/v1/setup', 'GET');
  const body = await readJson(res);
  if (!res.ok) throw toApiError(res, body);
  return (body as { setupRequired?: boolean } | undefined)?.setupRequired === true;
}

export interface SetupResult extends Session {
  /** The first admin token. The server returns it once. Keep it in memory only. */
  tokenSecret: string;
}

/**
 * POST /setup. The 201 starts a session (the two cookies) like a login, so the CSRF token and the user are
 * recorded here. The reply also carries the first admin token, which is returned to the caller and nowhere else.
 */
export async function completeSetup(req: SetupRequest): Promise<SetupResult> {
  const res = await send('/api/v1/setup', 'POST', req);
  const body = await readJson(res);
  if (!res.ok) throw toApiError(res, body);
  const b = body as
    { user?: SessionUser; orgs?: SessionOrg[]; csrfToken?: string; token?: { secret?: string } } | undefined;
  if (!b?.user || !Array.isArray(b.orgs) || !b.token?.secret)
    throw new ApiError(res.status, 'bad_response', 'The server sent an unexpected reply.');
  setMemoryCsrf(b.csrfToken);
  noteUser(b.user.id);
  setWritesBlocked(false);
  return { user: b.user, orgs: b.orgs, tokenSecret: b.token.secret };
}

export interface RestoreInput {
  /** The setup link token (from the URL fragment). Sent as a header, never in the URL. */
  token: string;
  /** The move file passphrase. Sent as a header, never in the URL. */
  passphrase: string;
  file: File;
  /** Upload progress, 0 to 1. */
  onProgress?: (fraction: number) => void;
  /** The whole body has left the browser: from now on the server may finish the restore even if this page goes away. */
  onSent?: () => void;
  signal?: AbortSignal;
}

export interface RestoreResult extends SetupResult {
  csrfToken: string;
  /** Absent when the server sent none: show no counts rather than invent them. */
  summary?: { tables: number; rows: number; objects: number; attachmentsIncluded: boolean };
}

/**
 * Installs the session of a finished restore in this tab (CSRF token in memory, signed-in user). A separate step
 * so that only a caller that is still on screen, and so shows the first token, does it.
 */
export function commitRestoreSession(r: RestoreResult): void {
  setMemoryCsrf(r.csrfToken);
  noteUser(r.user.id);
  setWritesBlocked(false);
}

/**
 * POST /setup/restore: the move file is the raw body. XMLHttpRequest, not fetch, because only XHR reports upload
 * progress. `send(file)` streams the File from disk; it is never read into memory here. Like `completeSetup`, the
 * 201 starts a session, and the first admin token goes to the caller only.
 */
export function restoreFromMoveFile(inp: RestoreInput): Promise<RestoreResult> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `${apiBase()}/api/v1/setup/restore`);
    xhr.withCredentials = true;
    xhr.setRequestHeader('X-Launchpad-Setup-Token', inp.token);
    xhr.setRequestHeader('X-Launchpad-Passphrase', inp.passphrase);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && e.total > 0) inp.onProgress?.(e.loaded / e.total);
    };
    xhr.upload.onload = () => inp.onSent?.();
    xhr.onerror = () => reject(networkError());
    xhr.onabort = () => reject(networkError());
    xhr.onload = () => {
      let body: unknown;
      try {
        body = JSON.parse(xhr.responseText);
      } catch {
        body = undefined;
      }
      const headers = new Headers();
      const ra = xhr.getResponseHeader('Retry-After');
      if (ra) headers.set('Retry-After', ra);
      if (xhr.status < 200 || xhr.status > 299) {
        reject(toApiError(new Response(null, { status: xhr.status, headers }), body));
        return;
      }
      const b = body as
        | {
            user?: SessionUser;
            orgs?: SessionOrg[];
            csrfToken?: string;
            token?: { secret?: string };
            summary?: RestoreResult['summary'];
          }
        | undefined;
      if (!b?.user || !Array.isArray(b.orgs) || !b.token?.secret) {
        reject(new ApiError(xhr.status, 'bad_response', 'The server sent an unexpected reply.'));
        return;
      }
      resolve({
        user: b.user,
        orgs: b.orgs,
        tokenSecret: b.token.secret,
        csrfToken: b.csrfToken ?? '',
        summary: b.summary,
      });
    };
    inp.signal?.addEventListener('abort', () => xhr.abort(), { once: true });
    if (inp.signal?.aborted) {
      reject(networkError());
      return;
    }
    xhr.send(inp.file);
  });
}

/**
 * POST /signups. Public, sign-up mode `approval` only. The server answers 202 `{}` in every case, so
 * this resolves with nothing and the caller must not guess anything from the call.
 */
export async function requestSignup(req: SignupCreateRequest): Promise<void> {
  const res = await send('/api/v1/signups', 'POST', req);
  if (res.status === 202 || res.ok) return;
  throw toApiError(res, await readJson(res));
}

/** POST /signups/confirm: the 6-digit code from the confirm email. 204 on success. It starts no session. */
export async function confirmSignup(email: string, code: string): Promise<void> {
  const res = await send('/api/v1/signups/confirm', 'POST', { email, code });
  if (res.ok) return;
  throw toApiError(res, await readJson(res));
}

export type InviteAccepted =
  /** A new account. The 201 started a session, like a login. */
  | ({ status: 'created' } & Session)
  /** The email had an account. The membership is added and NO session starts. */
  | { status: 'joined'; orgName: string };

/**
 * POST /invites/accept. The token goes only in this body. A 201 starts a session (the two cookies), so the
 * CSRF token and the user are recorded here, as a login does.
 */
export async function acceptInvite(req: InviteAcceptInput): Promise<InviteAccepted> {
  const res = await send('/api/v1/invites/accept', 'POST', req);
  const body = await readJson(res);
  if (!res.ok) throw toApiError(res, body);
  const b = body as
    | {
        status?: string;
        org?: { name?: string };
        session?: { user?: SessionUser; orgs?: SessionOrg[]; csrfToken?: string };
      }
    | undefined;
  if (res.status === 200 && b?.status === 'joined') return { status: 'joined', orgName: b.org?.name ?? '' };
  const sess = b?.session;
  if (!sess?.user || !Array.isArray(sess.orgs))
    throw new ApiError(res.status, 'bad_response', 'The server sent an unexpected reply.');
  setMemoryCsrf(sess.csrfToken);
  noteUser(sess.user.id);
  setWritesBlocked(false);
  return { status: 'created', user: sess.user, orgs: sess.orgs };
}

export async function logout(): Promise<void> {
  // Another user may be signed in on this browser (writes are blocked). Load that session first, so the
  // logout carries a token that matches the cookie. If that fails, try anyway: logout is exempt from the block.
  if (getWritesBlocked()) await fetchSession().catch(() => undefined);
  const res = await send('/api/v1/auth/logout', 'POST');
  // Only a confirmed logout (ok, or 401 = already signed out) drops the token. After any other
  // answer the server session may be alive, and the in-memory token is the only CSRF source we have.
  if (!res.ok && res.status !== 401) throw toApiError(res, await readJson(res));
  forgetSignedInUser();
}

/**
 * The in-memory half of signing out: drop the CSRF token, forget the user (which starts a new session epoch) and
 * lift the write block. Used by `logout` and by revoking the session in use (LAUN-160).
 */
export function forgetSignedInUser(): void {
  setMemoryCsrf(undefined);
  noteUser(undefined);
  setWritesBlocked(false);
}

/**
 * The current session, from GET /api/v1/auth/session. 401 means signed out.
 * The reply carries the CSRF token for this session; we keep it in memory and
 * prefer it to the cookie. With the dev bearer token the token is "".
 */
export async function fetchSession(): Promise<Session | null> {
  const loaded = await loadSession();
  if (!loaded) {
    setWritesBlocked(false); // signed out: there is no user to protect any more
    return null;
  }
  commitSession(loaded);
  return loaded.session;
}

interface LoadedSession {
  session: Session;
  csrfToken: string | undefined;
}

/** GET /auth/session with NO side effects: nothing is installed until the caller decides to. */
async function loadSession(): Promise<LoadedSession | null> {
  const res = await send('/api/v1/auth/session', 'GET');
  if (res.status === 401) return null;
  const body = await readJson(res);
  if (!res.ok) throw toApiError(res, body);
  const b = body as { user?: SessionUser; orgs?: SessionOrg[]; csrfToken?: string } | undefined;
  if (!b?.user || !Array.isArray(b.orgs))
    throw new ApiError(res.status, 'bad_response', 'The server sent an unexpected reply.');
  return { session: { user: b.user, orgs: b.orgs }, csrfToken: b.csrfToken };
}

function commitSession(l: LoadedSession): void {
  setMemoryCsrf(l.csrfToken);
  noteUser(l.session.user.id);
  setWritesBlocked(false); // the page now shows this user, so writes are theirs
}

// Who this tab thinks is signed in. The CSRF retry uses it: a token for a DIFFERENT user (another
// tab signed in as someone else) must not be installed, or the next write would go out as that user.
let knownUserId: string | undefined;
/** Records who is signed in. A different user (or none) starts a new session epoch. */
function noteUser(id: string | undefined): void {
  if (knownUserId !== id) bumpSessionEpoch();
  knownUserId = id;
}
const userChangedHandlers = new Set<() => void>();
/** Registers a handler that runs when the retry finds another user. Returns the function that removes it. */
export function addUserChangedHandler(fn: () => void): () => void {
  userChangedHandlers.add(fn);
  return () => void userChangedHandlers.delete(fn);
}

let refreshing: Promise<boolean> | undefined;
/**
 * Reloads the CSRF token for a retry. Answers true only when the SAME user is still signed in, and only
 * then installs the new token. For another user (or no session) it installs nothing, blocks further
 * writes, and tells the page. Concurrent callers share one reload.
 */
function refreshCsrfForRetry(): Promise<boolean> {
  refreshing ??= (async () => {
    try {
      const before = knownUserId;
      const loaded = await loadSession();
      if (!loaded) return false; // 401: signed out, nothing to retry with
      if (before === undefined || loaded.session.user.id !== before) {
        setWritesBlocked(true);
        bumpSessionEpoch(); // writes made as the old user must not be sent now
        for (const fn of [...userChangedHandlers]) fn();
        return false;
      }
      setMemoryCsrf(loaded.csrfToken);
      return true;
    } finally {
      refreshing = undefined;
    }
  })();
  return refreshing;
}

setCsrfRefresher(refreshCsrfForRetry);
