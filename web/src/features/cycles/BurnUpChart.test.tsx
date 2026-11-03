import { render, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { BurnUp } from './BurnUp';
import { burnModel, burnSummary, yTicks } from './burnModel';
import type { CycleSnapshot } from './queries';

const cycle = { startsAt: '2026-10-12T00:00:00Z', endsAt: '2026-10-16T00:00:00Z' }; // 12..15 Oct
const NOW = Date.parse('2026-10-14T10:00:00Z');
const row = (date: string, scope: number, done: number): CycleSnapshot => ({
  cycleId: 'c',
  date,
  scopeCount: scope,
  triageCount: 0,
  backlogCount: 0,
  unstartedCount: 0,
  startedCount: 0,
  completedCount: done,
  canceledCount: 0,
  scopePoints: 0,
  startedPoints: 0,
  completedPoints: 0,
  updatedAt: '2026-10-14T00:00:00Z',
});

describe('burnModel', () => {
  it('has no model without rows', () => {
    expect(burnModel(cycle, [], 'UTC', NOW)).toBeNull();
  });
  it('lists every day of the cycle, with null where no row exists, and finds today', () => {
    const m = burnModel(cycle, [row('2026-10-12', 5, 0), row('2026-10-14', 6, 2)], 'UTC', NOW)!;
    expect(m.days.map((d) => d.scope)).toEqual([5, null, 6, null]);
    expect(m.days.map((d) => d.done)).toEqual([0, null, 2, null]);
    expect(m.max).toBe(6);
    expect(m.today).toBe(2);
    expect(burnSummary(m)).toContain('2 of 6 issues done');
  });
  it('has no model when no day carries a row', () => {
    const far = { startsAt: '2020-01-01T00:00:00Z', endsAt: '2026-10-16T00:00:00Z' };
    expect(burnModel(far, [row('2026-10-15', 1, 1)], 'UTC', NOW)).toBeNull();
  });
  it('has no today marker outside the cycle', () => {
    const m = burnModel(cycle, [row('2026-10-12', 5, 0)], 'UTC', Date.parse('2026-11-30T00:00:00Z'))!;
    expect(m.today).toBeNull();
  });
  it('widens to a row outside the cycle days', () => {
    const m = burnModel(cycle, [row('2026-10-17', 3, 3)], 'UTC', NOW)!;
    expect(m.days).toHaveLength(6);
  });
});

describe('yTicks', () => {
  it('uses whole numbers only', () => {
    expect(yTicks(0)).toEqual([0, 1]);
    expect(yTicks(1)).toEqual([0, 1]);
    expect(yTicks(2)).toEqual([0, 1, 2]);
    expect(yTicks(3)).toEqual([0, 1, 2, 3]);
    expect(yTicks(23)).toEqual([0, 10, 20, 30]);
    expect(yTicks(100).at(-1)).toBeGreaterThanOrEqual(100);
    for (const max of [0, 1, 2, 3, 5, 7, 23, 100]) expect(yTicks(max).every(Number.isInteger)).toBe(true);
  });
});

describe('BurnUp', () => {
  const rows = [row('2026-10-12', 5, 0), row('2026-10-13', 5, 1), row('2026-10-14', 6, 2)];
  it('draws the scope and done lines, a today marker and an accessible summary and table', () => {
    const { container } = render(<BurnUp cycle={cycle} rows={rows} zone="UTC" now={NOW} />);
    const img = screen.getByRole('img');
    expect(img).toHaveAttribute('aria-label', expect.stringContaining('2 of 6 issues done'));
    expect(container.querySelectorAll('path')).toHaveLength(2);
    expect(screen.getByTestId('burn-today')).toBeInTheDocument();
    const table = screen.getByRole('table', { name: 'Burn-up by day, in issues' });
    expect(within(table).getAllByRole('row')).toHaveLength(4);
    expect(within(table).getByRole('row', { name: '14 Oct 6 2' })).toBeInTheDocument();
  });
  it('measures the box once data arrives after an empty first render', () => {
    const widths: number[] = [];
    const real = globalThis.ResizeObserver;
    class RO {
      constructor(private cb: () => void) {}
      observe() {
        this.cb();
      }
      disconnect() {}
    }
    globalThis.ResizeObserver = RO as unknown as typeof ResizeObserver;
    Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => 320 });
    try {
      const { rerender, container } = render(<BurnUp cycle={cycle} rows={[]} zone="UTC" now={NOW} />);
      rerender(<BurnUp cycle={cycle} rows={rows} zone="UTC" now={NOW} />);
      widths.push(Number(container.querySelector('svg')!.getAttribute('width')));
    } finally {
      globalThis.ResizeObserver = real;
      delete (HTMLElement.prototype as unknown as Record<string, unknown>).clientWidth;
    }
    expect(widths).toEqual([320]);
  });
  it('shows an empty state for a cycle with no rows', () => {
    render(<BurnUp cycle={cycle} rows={[]} zone="UTC" now={NOW} />);
    expect(screen.getByText(/No burn-up data yet/)).toBeInTheDocument();
    expect(screen.queryByRole('img')).toBeNull();
  });
  it('says the chart starts with the cycle for an upcoming one', () => {
    render(<BurnUp cycle={cycle} rows={undefined} zone="UTC" now={NOW} upcoming />);
    expect(screen.getByText(/starts when the cycle starts/)).toBeInTheDocument();
  });
});
