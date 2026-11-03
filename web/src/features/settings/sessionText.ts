import { ApiError } from '../../api/errors';
import type { LoginSession } from '../../api/types';

// LAUN-160. Fixed sentences for the Sessions page. No server text reaches the screen.

export function kindText(s: Pick<LoginSession, 'kind'>): string {
  return s.kind === 'native' ? 'App' : 'Web';
}

/** The fixed text for a failed list load. */
export function sessionsLoadText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 401) return 'You are signed out. Sign in again.';
    if (err.status === 403) return 'Sessions are for people. An agent has no login sessions.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return 'Could not load your sessions. Try again.';
}

/** The fixed text for a failed revoke. A 404 means the session is already gone. */
export function revokeText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 401) return 'You are signed out. Sign in again.';
    if (err.status === 403) return 'You cannot change sessions with this account.';
    if (err.status === 404) return 'This session is already gone. The list is updated.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return 'Could not revoke the session. Try again.';
}

export function othersDoneText(revoked: number): string {
  if (revoked === 0) return 'No other sessions were signed in.';
  return revoked === 1 ? 'Logged out 1 other session.' : `Logged out ${revoked} other sessions.`;
}
