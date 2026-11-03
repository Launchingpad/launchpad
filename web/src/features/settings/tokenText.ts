import type { ApiToken } from '../../api/types';

/** A date for a table cell, in UTC so it reads the same for every admin. `null` is "Never". */
export function day(iso: string | null): string {
  return iso ? new Date(iso).toISOString().slice(0, 10) : 'Never';
}

export function accessText(scopes: readonly string[]): string {
  if (scopes.includes('admin')) return 'Read, write, admin';
  return scopes.includes('write') ? 'Read and write' : 'Read only';
}

/** A token works while it is not revoked and not past its expiry. */
export function isLive(t: ApiToken, now = Date.now()): boolean {
  return t.revokedAt === null && (t.expiresAt === null || Date.parse(t.expiresAt) > now);
}

export function waitText(seconds?: number): string {
  if (!seconds) return 'a little while';
  if (seconds === 1) return '1 second';
  if (seconds < 90) return `${seconds} seconds`;
  return `${Math.ceil(seconds / 60)} minutes`;
}
