import { describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { formatDay, isOverdue, pct, sortProjects, todayString } from './projects';

const p = (name: string, state: Project['state'], targetDate: string | null = null) =>
  ({ id: name, name, state, targetDate }) as unknown as Project;

describe('project helpers', () => {
  it('sorts live work first and by name inside a state', () => {
    const out = sortProjects([
      p('b', 'completed'),
      p('z', 'started'),
      p('a', 'started'),
      p('m', 'planned'),
      p('c', 'canceled'),
      p('x', 'paused'),
      p('y', 'backlog'),
    ]).map((x) => x.name);
    expect(out).toEqual(['a', 'z', 'm', 'y', 'x', 'b', 'c']);
  });
  it('formats a day without a time zone shift', () => {
    expect(formatDay('2026-11-03')).toBe('3 Nov 2026');
    expect(formatDay(null)).toBe('');
    expect(formatDay('bad')).toBe('bad');
  });
  it('is overdue only for an open project past its target', () => {
    expect(isOverdue(p('a', 'started', '2026-10-01'), '2026-10-02')).toBe(true);
    expect(isOverdue(p('a', 'started', '2026-10-02'), '2026-10-02')).toBe(false);
    expect(isOverdue(p('a', 'completed', '2026-10-01'), '2026-10-02')).toBe(false);
    expect(isOverdue(p('a', 'canceled', '2026-10-01'), '2026-10-02')).toBe(false);
    expect(isOverdue(p('a', 'started', null), '2026-10-02')).toBe(false);
  });
  it('computes percent safely', () => {
    expect(pct(1, 3)).toBe(33);
    expect(pct(0, 0)).toBe(0);
    expect(pct(5, 4)).toBe(100);
  });
  it('writes the local day', () => {
    expect(todayString(new Date(2026, 0, 5, 12).getTime())).toBe('2026-01-05');
  });
});
