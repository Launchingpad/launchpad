import type { Cycle } from '../../api/types';
import { lastDayMs, localDay } from './cycles';
import type { CycleSnapshot } from './queries';

const DAY = 86400_000;

export interface BurnDay {
  /** Days since the first day of the chart. */
  index: number;
  /** UTC midnight of the calendar day. */
  time: number;
  /** A daily row exists for the day. */
  scope: number | null;
  done: number | null;
}

export interface BurnModel {
  days: BurnDay[];
  /** Highest value to draw (at least 1). */
  max: number;
  /** Index of today, when it is inside the chart. */
  today: number | null;
}

const parseDate = (d: string): number | undefined => {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(d);
  return m ? Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : undefined;
};

/**
 * The days of a burn-up chart: every calendar day from the cycle's first day to its last (or to the last
 * row, if later), each with its row's scope and done counts, or `null` where the job wrote no row.
 * Rows are dates in the team's zone; `zone` turns the cycle's instants into the same dates.
 */
export function burnModel(
  cycle: Pick<Cycle, 'startsAt' | 'endsAt'>,
  rows: CycleSnapshot[],
  zone: string | undefined,
  now: number,
): BurnModel | null {
  const byTime = new Map<number, CycleSnapshot>();
  for (const r of rows) {
    const t = parseDate(r.date);
    if (t !== undefined) byTime.set(t, r);
  }
  if (byTime.size === 0) return null;
  const times = [...byTime.keys()];
  const s = localDay(cycle.startsAt, zone);
  const e = lastDayMs(cycle, zone); // a mid-day end (Mark complete) counts its own day
  const first = Math.min(s ?? Infinity, ...times);
  const last = Math.max(e === undefined ? -Infinity : e, ...times);
  const n = Math.round((last - first) / DAY) + 1;
  const days: BurnDay[] = [];
  let max = 1;
  for (let i = 0; i < n && i < 400; i++) {
    const time = first + i * DAY;
    const r = byTime.get(time);
    if (r) max = Math.max(max, r.scopeCount, r.completedCount);
    days.push({ index: i, time, scope: r?.scopeCount ?? null, done: r?.completedCount ?? null });
  }
  if (!days.some((d) => d.scope !== null)) return null;
  const t = localDay(new Date(now).toISOString(), zone);
  const today =
    t !== undefined && t >= first && t <= first + (days.length - 1) * DAY
      ? Math.round((t - first) / DAY)
      : null;
  return { days, max, today };
}

/** Whole-number ticks from 0 up to and including a top of at most about `want` steps (counts are whole issues). */
export function yTicks(max: number, want = 4): number[] {
  const top = Math.max(1, Math.ceil(max));
  const raw = top / want;
  const pow = 10 ** Math.floor(Math.log10(raw));
  const step = Math.max(1, [1, 2, 5, 10].map((k) => k * pow).find((c) => c >= raw) ?? Math.ceil(raw));
  const out: number[] = [];
  for (let v = 0; ; v += step) {
    out.push(v);
    if (v >= top) break;
  }
  return out;
}

const fmt = new Intl.DateTimeFormat('en-GB', { timeZone: 'UTC', day: 'numeric', month: 'short' });
export const dayLabel = (time: number) => fmt.format(time);

/** One line a screen reader gets for the whole chart. */
export function burnSummary(m: BurnModel): string {
  const withRows = m.days.filter((d) => d.scope !== null);
  const last = withRows[withRows.length - 1]!;
  return `Burn-up chart, ${dayLabel(m.days[0]!.time)} to ${dayLabel(m.days[m.days.length - 1]!.time)}. On ${dayLabel(
    last.time,
  )}: ${last.done} of ${last.scope} issues done.`;
}
