import { ApiError } from '../../api/errors';
import type { Cycle } from '../../api/types';
import { cycleKind, isLocalMidnight, lastDayMs, localDay } from './cycles';

/** The reason an option cannot be used now, or undefined when it can. The server judges again (decisions-T27). */
export interface OptionReasons {
  dates?: string;
  complete?: string;
  remove?: string;
}

/**
 * Which rows of the cycle menu a rule forbids now (LAUN-172, decisions-T27 #4 to #6). `next` is the cycle that
 * starts after this one, if any. Past: ended or complete. Current: running. Next: not started.
 */
export const ADMIN_ONLY = 'Only admins can complete or delete a cycle.';

export function optionReasons(c: Cycle, next: Cycle | null, now: number, isAdmin = true): OptionReasons {
  const kind = cycleKind(c, now);
  const r: OptionReasons = {};
  if (kind === 'past') r.dates = 'The dates of a past cycle cannot change.';
  if (c.completedAt) r.complete = 'This cycle is already complete.';
  else if (kind === 'past') r.complete = 'This cycle has ended.';
  else if (kind === 'next') r.complete = 'Only the current cycle can be completed.';
  if (c.completedAt) r.remove = 'A completed cycle cannot be deleted.';
  else if (!next) r.remove = 'The last cycle cannot be deleted.';
  // Complete and delete need the admin scope (decisions-T27 #2); the role is the cue, the server judges.
  if (!isAdmin) {
    r.complete = ADMIN_ONLY;
    r.remove = ADMIN_ONLY;
  }
  return r;
}

const DAY = 86400_000;

/** A calendar day (UTC midnight, from `localDay`) as YYYY-MM-DD. */
export const dayText = (ms: number): string => new Date(ms).toISOString().slice(0, 10);

/**
 * The first and the last day of a cycle in the team zone, as YYYY-MM-DD. `exclusiveEnd`: the end is a local midnight,
 * so the server's `endsAt` for a last day is the day after it; otherwise (a cycle with a mid-day end) `endsAt` is on
 * the last day itself (see `lastDayMs`).
 */
export function cycleDays(
  c: Pick<Cycle, 'startsAt' | 'endsAt'>,
  zone: string | undefined,
): { first: string; last: string; exclusiveEnd: boolean } | null {
  const a = localDay(c.startsAt, zone);
  const l = lastDayMs(c, zone);
  if (a === undefined || l === undefined) return null;
  return { first: dayText(a), last: dayText(l), exclusiveEnd: isLocalMidnight(c.endsAt, zone) };
}

/** The day after a YYYY-MM-DD day: what the server wants as `endsAt` for a given last day. */
export function dayAfter(day: string): string {
  return dayText(Date.parse(`${day}T00:00:00Z`) + DAY);
}

/** Text for a failed cycle write. A 400 or 409 carries the server's own sentence (the rule that said no). */
export function cycleErrorText(err: unknown, what: string): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 400 || err.status === 409) return err.message;
    if (err.status === 403) return `You do not have permission to ${what}.`;
    if (err.status === 404) return 'This cycle is gone. Reload the page.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return `Could not ${what}. Try again.`;
}
