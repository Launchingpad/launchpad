export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly retryAfter?: number;
  constructor(status: number, code: string, message: string, retryAfter?: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f]+/g;

/** Server text is untrusted: strip control characters, cap the length, drop anything that looks like markup or JSON. */
export function cleanServerText(text: unknown): string {
  if (typeof text !== 'string') return '';
  const t = text.replace(CONTROL, ' ').replace(/\s+/g, ' ').trim();
  if (!t || /^[{[<]/.test(t)) return '';
  return t.length > 200 ? `${t.slice(0, 199)}…` : t;
}

const FALLBACK: Record<number, string> = {
  400: 'The request was not accepted.',
  401: 'You are signed out. Sign in again.',
  403: 'You do not have permission to do that.',
  404: 'That item was not found.',
  409: 'That conflicts with the current state.',
  413: 'That is too large to send.',
  429: 'Too many requests. Wait a moment and try again.',
};

export function toApiError(response: Response, body: unknown): ApiError {
  const obj = body && typeof body === 'object' ? (body as Record<string, unknown>) : {};
  const code = typeof obj.code === 'string' ? obj.code.replace(/[^a-z_]/g, '').slice(0, 60) : 'error';
  const serverText = cleanServerText(obj.error);
  const fallback =
    FALLBACK[response.status] ??
    (response.status >= 500 ? 'The server had a problem. Try again.' : 'Something went wrong.');
  const ra = Number(response.headers.get('Retry-After'));
  return new ApiError(
    response.status,
    code || 'error',
    serverText || fallback,
    Number.isFinite(ra) && ra > 0 ? ra : undefined,
  );
}

export function networkError(): ApiError {
  return new ApiError(0, 'network_error', 'Could not reach the server. Check your connection.');
}

/** A message safe to show a person. Never raw JSON. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error && err.name === 'CsrfMissingError')
    return 'Your session needs refreshing. Sign in again.';
  if (err instanceof Error && err.name === 'SessionChangedError')
    return 'Another user signed in on this browser. Reloading your session.';
  return 'Something went wrong.';
}

export function isConflict(err: unknown): boolean {
  return err instanceof ApiError && err.status === 409 && err.code === 'version_conflict';
}
export function isUnauthorized(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401;
}

/** LAUN-51: the neighbour of a board drop left its column. The column must be fetched again. */
export function isStalePlacement(err: unknown): boolean {
  return err instanceof ApiError && err.status === 409 && err.code === 'stale_placement';
}
