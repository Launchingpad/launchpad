import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BurnUp, CHART_KEY } from './BurnUp';
import type { CycleSnapshot } from './queries';

// LAUN-164: the Hide chart / Show chart button on the burn-up chart.
const cycle = { startsAt: '2026-10-12T00:00:00Z', endsAt: '2026-10-16T00:00:00Z' };
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
const rows = [row('2026-10-12', 5, 0), row('2026-10-14', 6, 2)];
const chart = () => <BurnUp cycle={cycle} rows={rows} zone="UTC" now={NOW} />;

afterEach(() => vi.restoreAllMocks());

describe('burn-up chart toggle', () => {
  it('shows the chart by default, with the button named Hide chart and aria-expanded true', () => {
    render(chart());
    const b = screen.getByRole('button', { name: 'Hide chart' });
    expect(b).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('img')).toBeInTheDocument();
    expect(document.getElementById(b.getAttribute('aria-controls')!)).toContainElement(
      screen.getByRole('img'),
    );
  });
  it('hides the svg and the table on click, and the button becomes Show chart', async () => {
    render(chart());
    await userEvent.setup().click(screen.getByRole('button', { name: 'Hide chart' }));
    const b = screen.getByRole('button', { name: 'Show chart' });
    expect(b).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('img')).toBeNull();
    expect(screen.queryByRole('table')).toBeNull();
    expect(document.querySelector('svg path')).toBeNull();
    await userEvent.setup().click(b);
    expect(screen.getByRole('img')).toBeInTheDocument();
  });
  it('works from the keyboard', async () => {
    const user = userEvent.setup();
    render(chart());
    await user.tab();
    expect(screen.getByRole('button', { name: 'Hide chart' })).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('img')).toBeNull();
    await user.keyboard(' ');
    expect(screen.getByRole('img')).toBeInTheDocument();
  });
  it('remembers the choice across a remount', async () => {
    const first = render(chart());
    await userEvent.setup().click(screen.getByRole('button', { name: 'Hide chart' }));
    expect(localStorage.getItem(CHART_KEY)).toBe('hidden');
    first.unmount();
    render(chart());
    expect(screen.getByRole('button', { name: 'Show chart' })).toBeInTheDocument();
    expect(screen.queryByRole('img')).toBeNull();
  });
  it('shows the chart when the stored value is anything but hidden', () => {
    localStorage.setItem(CHART_KEY, 'garbage');
    render(chart());
    expect(screen.getByRole('img')).toBeInTheDocument();
  });
  it('falls back to shown when storage throws on read, and still toggles when it throws on write', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    render(chart());
    expect(screen.getByRole('img')).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Hide chart' }));
    expect(screen.queryByRole('img')).toBeNull();
  });
  it('has no button when there is no chart to hide', () => {
    render(<BurnUp cycle={cycle} rows={[]} zone="UTC" now={NOW} />);
    expect(screen.queryByRole('button')).toBeNull();
  });
  it('keeps focus on the button after each press', async () => {
    const user = userEvent.setup();
    render(chart());
    const b = screen.getByRole('button', { name: 'Hide chart' });
    await user.click(b);
    expect(screen.getByRole('button', { name: 'Show chart' })).toHaveFocus();
    await user.click(screen.getByRole('button', { name: 'Show chart' }));
    expect(screen.getByRole('button', { name: 'Hide chart' })).toHaveFocus();
  });
  it('measures the box again when the chart is shown after a resize while hidden', async () => {
    const real = globalThis.ResizeObserver;
    class RO {
      constructor(private cb: () => void) {}
      observe() {
        this.cb();
      }
      disconnect() {}
    }
    globalThis.ResizeObserver = RO as unknown as typeof ResizeObserver;
    let width = 320;
    Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => width });
    try {
      const user = userEvent.setup();
      const { container } = render(chart());
      expect(container.querySelector('svg')!.getAttribute('width')).toBe('320');
      await user.click(screen.getByRole('button', { name: 'Hide chart' }));
      width = 500; // the window changed while the chart was hidden
      await user.click(screen.getByRole('button', { name: 'Show chart' }));
      expect(container.querySelector('svg')!.getAttribute('width')).toBe('500');
    } finally {
      globalThis.ResizeObserver = real;
      delete (HTMLElement.prototype as unknown as Record<string, unknown>).clientWidth;
    }
  });
});
