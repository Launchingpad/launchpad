import type { Cycle } from '../../api/types';

export const cycleName = (c: Pick<Cycle, 'name' | 'number'>) => c.name ?? `Cycle ${c.number}`;

export type CycleKind = 'current' | 'next' | 'past';

/** Current: running now. Next: not started. Past: ended or completed. */
export function cycleKind(c: Cycle, now: number = Date.now()): CycleKind {
  const start = Date.parse(c.startsAt);
  const end = Date.parse(c.endsAt);
  if (c.completedAt || end <= now) return 'past';
  return start <= now ? 'current' : 'next';
}

export interface CycleSections {
  current: Cycle[];
  next: Cycle[];
  past: Cycle[];
}

/** Current, then next (soonest first), then past (latest first). */
export function sectionCycles(cycles: Cycle[], now: number = Date.now()): CycleSections {
  const out: CycleSections = { current: [], next: [], past: [] };
  for (const c of cycles) out[cycleKind(c, now)].push(c);
  out.next.sort((a, b) => Date.parse(a.startsAt) - Date.parse(b.startsAt));
  out.past.sort((a, b) => Date.parse(b.startsAt) - Date.parse(a.startsAt));
  return out;
}

/** The calendar day of an instant in a zone, as UTC midnight of that day (so day arithmetic is plain). */
export function localDay(iso: string, zone: string | undefined): number | undefined {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return undefined;
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat('en-US', {
      timeZone: zone,
      year: 'numeric',
      month: 'numeric',
      day: 'numeric',
    }).formatToParts(t);
  } catch {
    // An unknown zone name: fall back to the browser zone.
    return localDay(iso, undefined);
  }
  const n = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return Date.UTC(n('year'), n('month') - 1, n('day'));
}

/** True when `iso` is exactly 00:00 on a calendar day of the zone (the usual cycle boundary). */
export function isLocalMidnight(iso: string, zone: string | undefined): boolean {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return false;
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat('en-US', {
      timeZone: zone,
      hourCycle: 'h23',
      hour: 'numeric',
      minute: 'numeric',
      second: 'numeric',
    }).formatToParts(t);
  } catch {
    return isLocalMidnight(iso, undefined);
  }
  const n = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return n('hour') === 0 && n('minute') === 0 && n('second') === 0 && t % 1000 === 0;
}

/**
 * The last day of a cycle, as `localDay` ms. `endsAt` is exclusive, so at a local midnight the last day is the day
 * before. At any other time (a cycle completed mid-day, LAUN-172) the cycle ran during the end's own day, so that
 * day is the last one. Never before the first day.
 */
export function lastDayMs(
  c: Pick<Cycle, 'startsAt' | 'endsAt'>,
  zone: string | undefined,
): number | undefined {
  const a = localDay(c.startsAt, zone);
  const e = localDay(c.endsAt, zone);
  if (a === undefined || e === undefined) return undefined;
  const last = isLocalMidnight(c.endsAt, zone) ? e - 86400_000 : e;
  return Math.max(a, last);
}

const dayFmt = (withYear: boolean) =>
  new Intl.DateTimeFormat('en-GB', {
    timeZone: 'UTC',
    day: 'numeric',
    month: 'short',
    ...(withYear ? { year: 'numeric' } : {}),
  });

/**
 * "11 Oct to 24 Oct": the first and the last day of the cycle, as calendar days in the team's cycle zone.
 * `endsAt` is exclusive (it is the next cycle's `startsAt`), so the last day is the day before it.
 * The year shows only when the cycle does not start in the year of `now`.
 */
export function dateRange(
  c: Pick<Cycle, 'startsAt' | 'endsAt'>,
  zone?: string,
  now: number = Date.now(),
): string {
  const a = localDay(c.startsAt, zone);
  const b = lastDayMs(c, zone);
  if (a === undefined || b === undefined) return '';
  const f = dayFmt(new Date(a).getUTCFullYear() !== new Date(now).getFullYear());
  return `${f.format(a)} to ${f.format(b)}`;
}

export function percent(done: number, scope: number): number {
  return scope > 0 ? Math.round((done / scope) * 100) : 0;
}

const shortFmt = new Intl.DateTimeFormat('en-US', { timeZone: 'UTC', month: 'short', day: 'numeric' });

/** "Nov 9 - Nov 22" for the cycle switcher: the same calendar days as `dateRange`, in the short US form. */
export function shortRange(c: Pick<Cycle, 'startsAt' | 'endsAt'>, zone?: string): string {
  const a = localDay(c.startsAt, zone);
  const b = lastDayMs(c, zone);
  if (a === undefined || b === undefined) return '';
  return `${shortFmt.format(a)} - ${shortFmt.format(b)}`;
}

/** The cycle that starts right after `c`, and the one right before it (null at either end). */
export function neighbours(cycles: Cycle[], c: Cycle): { prev: Cycle | null; next: Cycle | null } {
  const sorted = [...cycles].sort((a, b) => Date.parse(a.startsAt) - Date.parse(b.startsAt));
  const i = sorted.findIndex((x) => x.id === c.id);
  if (i < 0) return { prev: null, next: null };
  return { prev: sorted[i - 1] ?? null, next: sorted[i + 1] ?? null };
}

/**
 * What the cycle switcher menu lists (LAUN-116): the neighbours that are current or upcoming, never a past
 * one (past cycles stay on the Cycles list, reached by "All cycles"). `here` is the cycle being viewed, set
 * only when it is past, so the menu can still show where the person is. The Alt+J / Alt+K keys use
 * `neighbours` directly and are not filtered.
 */
export function switcherCycles(
  cycles: Cycle[],
  c: Cycle,
  now: number = Date.now(),
): { prev: Cycle | null; next: Cycle | null; here: Cycle | null } {
  const n = neighbours(cycles, c);
  const live = (x: Cycle | null) => (x && cycleKind(x, now) !== 'past' ? x : null);
  return { prev: live(n.prev), next: live(n.next), here: cycleKind(c, now) === 'past' ? c : null };
}
