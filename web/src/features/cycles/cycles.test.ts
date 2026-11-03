import { describe, expect, it } from 'vitest';
import type { Cycle } from '../../api/types';
import {
  cycleKind,
  cycleName,
  isLocalMidnight,
  dateRange,
  neighbours,
  percent,
  switcherCycles,
  sectionCycles,
  shortRange,
} from './cycles';
import { cycleDays } from './cycleRules';

const NOW = Date.parse('2026-10-25T12:00:00Z');
const cy = (n: number, startsAt: string, endsAt: string, over: Partial<Cycle> = {}): Cycle => ({
  id: `cy${n}`,
  team: { id: 't', key: 'ORB', name: 'Orbit' },
  number: n,
  name: null,
  description: '',
  startsAt,
  endsAt,
  completedAt: null,
  createdAt: startsAt,
  updatedAt: startsAt,
  archivedAt: null,
  ...over,
});

describe('cycle sections', () => {
  const c1 = cy(1, '2026-10-01T00:00:00Z', '2026-10-11T00:00:00Z', { completedAt: '2026-10-11T00:00:00Z' });
  const c2 = cy(2, '2026-10-11T00:00:00Z', '2026-10-22T00:00:00Z'); // ended, job has not completed it yet
  const c3 = cy(3, '2026-10-22T00:00:00Z', '2026-11-05T00:00:00Z');
  const c4 = cy(4, '2026-11-05T00:00:00Z', '2026-11-19T00:00:00Z');

  it('puts current first, then next, then past with the latest first', () => {
    const s = sectionCycles([c4, c3, c2, c1], NOW);
    expect(s.current.map((c) => c.number)).toEqual([3]);
    expect(s.next.map((c) => c.number)).toEqual([4]);
    expect(s.past.map((c) => c.number)).toEqual([2, 1]);
  });

  it('treats an ended cycle as past even before the job completes it', () => {
    expect(cycleKind(c2, NOW)).toBe('past');
  });

  it('names a cycle by its name or its number', () => {
    expect(cycleName(c3)).toBe('Cycle 3');
    expect(cycleName({ ...c3, name: 'Hardening' })).toBe('Hardening');
  });

  it('computes a percent and handles an empty scope', () => {
    expect(percent(1, 3)).toBe(33);
    expect(percent(0, 0)).toBe(0);
  });
});

describe('dateRange', () => {
  const now = Date.parse('2026-10-25T12:00:00Z');
  // Europe/London: starts 11 Oct 00:00 BST, ends 25 Oct 00:00 BST (exclusive; the clocks change at 02:00 that day).
  const c = { startsAt: '2026-10-10T23:00:00Z', endsAt: '2026-10-24T23:00:00Z' };

  it('shows the first and the last day in the team zone (the end is exclusive)', () => {
    expect(dateRange(c, 'Europe/London', now)).toBe('11 Oct to 24 Oct');
  });

  it('reads the same cycle in a zone west of the team on the days it falls there', () => {
    // 16:00 on 10 Oct and 16:00 on 24 Oct in Los Angeles: the team zone must win, not the viewer zone.
    // 16:00 is not a local midnight there, so the end's own day (24 Oct) is the last day (LAUN-172).
    expect(dateRange(c, 'America/Los_Angeles', now)).toBe('10 Oct to 24 Oct');
    expect(dateRange(c, 'Europe/London', now)).not.toBe(dateRange(c, 'America/Los_Angeles', now));
  });

  it('gives consecutive cycles days that do not overlap', () => {
    const next = { startsAt: c.endsAt, endsAt: '2026-11-07T00:00:00Z' };
    expect(dateRange(next, 'Europe/London', now)).toBe('25 Oct to 6 Nov');
  });

  it('adds the year when the cycle is not in the current year, and survives a bad zone', () => {
    expect(dateRange(c, 'Europe/London', Date.parse('2027-01-05T00:00:00Z'))).toBe(
      '11 Oct 2026 to 24 Oct 2026',
    );
    expect(dateRange(c, 'Not/AZone', now)).toMatch(/ to /);
  });
});

describe('shortRange and neighbours', () => {
  it('writes "Nov 9 - Nov 22" with the last day inclusive', () => {
    const c = cy(1, '2026-11-09T00:00:00Z', '2026-11-23T00:00:00Z');
    expect(shortRange(c, 'UTC')).toBe('Nov 9 - Nov 22');
  });
  it('finds the cycle before and after, by start date', () => {
    const a = cy(1, '2026-10-01T00:00:00Z', '2026-10-15T00:00:00Z');
    const b = cy(2, '2026-10-15T00:00:00Z', '2026-10-29T00:00:00Z');
    const c = cy(3, '2026-10-29T00:00:00Z', '2026-11-12T00:00:00Z');
    expect(neighbours([c, a, b], b)).toEqual({ prev: a, next: c });
    expect(neighbours([c, a, b], a).prev).toBeNull();
    expect(neighbours([c, a, b], c).next).toBeNull();
  });
});

describe('switcherCycles (LAUN-116)', () => {
  const now = Date.parse('2026-10-29T12:00:00Z');
  const mk = (n: number, from: number, to: number, over: Partial<Cycle> = {}) =>
    ({
      id: `c${n}`,
      number: n,
      name: null,
      startsAt: new Date(now + from * 864e5).toISOString(),
      endsAt: new Date(now + to * 864e5).toISOString(),
      completedAt: null,
      ...over,
    }) as unknown as Cycle;
  const c1 = mk(1, -30, -16, { completedAt: 'x' });
  const c2 = mk(2, -16, -2);
  const c3 = mk(3, -2, 12);
  const c4 = mk(4, 12, 26);
  const all = [c4, c1, c3, c2];

  it('drops past neighbours, keeps current and upcoming', () => {
    expect(switcherCycles(all, c3, now)).toEqual({ prev: null, next: c4, here: null });
    expect(switcherCycles(all, c4, now)).toEqual({ prev: c3, next: null, here: null });
  });
  it('keeps the viewed past cycle as here, and ended-but-not-completed counts as past', () => {
    expect(switcherCycles(all, c2, now)).toEqual({ prev: null, next: c3, here: c2 });
    expect(switcherCycles(all, c1, now)).toEqual({ prev: null, next: null, here: c1 });
  });
});

describe('last day of a cycle (LAUN-172)', () => {
  const L = 'Europe/London';
  const c = (startsAt: string, endsAt: string) => ({ startsAt, endsAt });
  it('a local midnight end is exclusive: the last day is the day before', () => {
    // Sunday 25 Oct 00:00 BST to Sunday 8 Nov 00:00 GMT (the clocks changed in between).
    const cy = c('2026-10-24T23:00:00Z', '2026-11-08T00:00:00Z');
    expect(dateRange(cy, L, Date.parse('2026-10-30T00:00:00Z'))).toBe('25 Oct to 7 Nov');
    expect(shortRange(cy, L)).toBe('Oct 25 - Nov 7');
    expect(cycleDays(cy, L)).toEqual({ first: '2026-10-25', last: '2026-11-07', exclusiveEnd: true });
  });
  it('a mid-day end (completed early) is its own last day', () => {
    const cy = c('2026-10-25T00:00:00Z', '2026-10-30T20:54:00Z');
    expect(dateRange(cy, L, Date.parse('2026-10-30T00:00:00Z'))).toBe('25 Oct to 30 Oct');
    expect(shortRange(cy, L)).toBe('Oct 25 - Oct 30');
    expect(cycleDays(cy, L)).toEqual({ first: '2026-10-25', last: '2026-10-30', exclusiveEnd: false });
  });
  it('a cycle completed on its first day shows one day, not an inverted range', () => {
    const cy = c('2026-10-30T00:00:00Z', '2026-10-30T09:15:00Z');
    expect(dateRange(cy, L, Date.parse('2026-10-30T00:00:00Z'))).toBe('30 Oct to 30 Oct');
  });
  it('reads midnight in the team zone, not in UTC', () => {
    // 23:00Z is local midnight in London in summer, but 23:00 in UTC.
    expect(isLocalMidnight('2026-10-24T23:00:00Z', L)).toBe(true);
    expect(isLocalMidnight('2026-10-25T00:00:00Z', L)).toBe(false); // 01:00 BST
    expect(isLocalMidnight('2026-11-08T00:00:00Z', L)).toBe(true); // GMT
    expect(isLocalMidnight('2026-11-08T00:00:30Z', L)).toBe(false);
  });
  it('a 09:00 cadence end is not midnight, so the end day is the last day', () => {
    const cy = c('2026-11-01T09:00:00Z', '2026-11-15T09:00:00Z');
    expect(cycleDays(cy, L)).toEqual({ first: '2026-11-01', last: '2026-11-15', exclusiveEnd: false });
  });
});
