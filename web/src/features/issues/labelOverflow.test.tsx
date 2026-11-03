import { act, render, screen, waitFor } from '@testing-library/react';
import { useRef } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../api/types';
import { baseRoutes, makeIssue, mockApi, statusRef } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import {
  ROW_PILLS,
  clippedFlags,
  hiddenLabels,
  labelsTitle,
  knownLabelBoxes,
  resetLabelObserver,
  useClippedPills,
} from './labelOverflow';

// LAUN-233: a list row shows at most three pills in one clipped line; "+N" counts every label it does not show.

const L = (n: number) => ({ id: `l${n}`, name: `label ${n}`, color: '#d6481f' });

describe('clippedFlags', () => {
  it('marks a pill cut when any part is outside the box (half a pixel of slack)', () => {
    const box = { left: 100, right: 300 };
    expect(
      clippedFlags(box, [
        { left: 100, right: 150 },
        { left: 154, right: 300.4 },
        { left: 280, right: 340 },
        { left: 344, right: 400 },
      ]),
    ).toEqual([false, false, true, true]);
    expect(clippedFlags(box, [{ left: 99, right: 120 }])).toEqual([true]);
  });
});

describe('hiddenLabels', () => {
  const labels = [1, 2, 3, 4, 5].map(L);
  it('counts the labels past the rendered pills', () => {
    expect(hiddenLabels(labels, ROW_PILLS, 0).map((l) => l.id)).toEqual(['l4', 'l5']);
  });
  it('adds the rendered pills the clip cuts (always the last ones)', () => {
    expect(hiddenLabels(labels, ROW_PILLS, 2).map((l) => l.id)).toEqual(['l2', 'l3', 'l4', 'l5']);
    expect(hiddenLabels(labels, ROW_PILLS, 3)).toHaveLength(5);
  });
  it('is empty when every label shows', () => {
    expect(hiddenLabels([L(1), L(2)], ROW_PILLS, 0)).toEqual([]);
  });
  it('never counts more than the row has', () => {
    expect(hiddenLabels([L(1)], ROW_PILLS, 1)).toHaveLength(1);
  });
});

describe('labelsTitle', () => {
  it('names the project and every label', () => {
    expect(labelsTitle('Web', [L(1), L(2)])).toBe('Project: Web\nLabels: label 1, label 2');
    expect(labelsTitle(undefined, [L(1)])).toBe('Labels: label 1');
    expect(labelsTitle('Web', [])).toBe('Project: Web');
    expect(labelsTitle(undefined, [])).toBeUndefined();
  });
});

describe('the labels of a list row', () => {
  beforeEach(() => {
    document.cookie = 'odt_csrf=csrf-test; path=/';
  });
  it('shows three pills, counts the rest and names them all on hover', async () => {
    const PID = '01a00000-0000-7000-8000-000000000001';
    const issue = makeIssue(1, { title: 'Five labels', projectId: PID, labels: [1, 2, 3, 4, 5].map(L) });
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/projects': () => ({
        json: {
          items: [
            {
              id: PID,
              name: 'Web rebuild',
              status: statusRef('started'),
              teams: [{ id: 'team1', key: 'ORB', name: 'Orbit' }],
            } as unknown as Project,
          ],
          nextCursor: null,
        },
      }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [issue], nextCursor: null } }),
    });
    renderApp('/orbit/issues');
    await screen.findByText('Five labels');
    const more = await screen.findByText('+2');
    expect(more).toHaveAttribute('title', 'label 4, label 5');
    expect(screen.getByText('label 1')).toBeInTheDocument();
    expect(screen.queryByText('label 4')).not.toBeInTheDocument();
    const cell = more.parentElement!;
    await screen.findByRole('link', { name: 'Project: Web rebuild' });
    expect(cell).toHaveAttribute(
      'title',
      'Project: Web rebuild\nLabels: label 1, label 2, label 3, label 4, label 5',
    );
  });
});

// The DOM path of the hook, with a fake ResizeObserver and fake geometry: each element's box comes from its
// `data-l` and `data-r` attributes (jsdom has no layout).
class FakeRO {
  static last: FakeRO | null = null;
  observed = new Set<Element>();
  constructor(public cb: ResizeObserverCallback) {
    FakeRO.last = this;
  }
  observe(el: Element) {
    this.observed.add(el);
  }
  unobserve(el: Element) {
    this.observed.delete(el);
  }
  disconnect() {
    this.observed.clear();
  }
  /** Delivers a notification for the given elements, as the browser does after a layout. */
  fire(els: Element[]) {
    act(() =>
      this.cb(
        els.map((target) => ({ target }) as ResizeObserverEntry),
        this as unknown as ResizeObserver,
      ),
    );
  }
}

function rect(this: Element) {
  const l = Number((this as HTMLElement).dataset.l ?? 0);
  const r = Number((this as HTMLElement).dataset.r ?? 0);
  return {
    left: l,
    right: r,
    width: r - l,
    top: 0,
    bottom: 10,
    height: 10,
    x: l,
    y: 0,
    toJSON() {},
  } as DOMRect;
}

type Pill = { name: string; l: number; r: number };
function Box({ pills, chip, box = 100 }: { pills: Pill[]; chip?: number; box?: number }) {
  const ref = useRef<HTMLSpanElement>(null);
  const n = useClippedPills(ref);
  return (
    <div data-testid="row" data-n={n}>
      <span ref={ref} data-testid="box" data-l={0} data-r={box}>
        {chip !== undefined && (
          <a data-l={0} data-r={chip}>
            chip
          </a>
        )}
        {pills.map((p) => (
          <span key={p.name} data-testid={`pill-${p.name}`} data-l={p.l} data-r={p.r}>
            {p.name}
          </span>
        ))}
      </span>
    </div>
  );
}
const count = () => Number(screen.getByTestId('row').dataset.n);
const cut = (name: string) => screen.getByTestId(`pill-${name}`).hasAttribute('data-clipped');

describe('useClippedPills (the DOM path)', () => {
  beforeEach(() => {
    resetLabelObserver();
    vi.stubGlobal('ResizeObserver', FakeRO);
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => setTimeout(() => cb(0), 0));
    vi.stubGlobal('cancelAnimationFrame', (id: number) => clearTimeout(id));
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(rect);
  });
  afterEach(() => {
    resetLabelObserver();
    vi.restoreAllMocks();
  });

  it('observes the box and every child, and marks the cut pills', () => {
    render(
      <Box
        pills={[
          { name: 'a', l: 0, r: 40 },
          { name: 'b', l: 44, r: 100.5 },
          { name: 'c', l: 104, r: 140 },
        ]}
      />,
    );
    const ro = FakeRO.last!;
    const box = screen.getByTestId('box');
    expect([...ro.observed]).toEqual([box, ...box.children]);
    ro.fire([box]);
    expect([cut('a'), cut('b'), cut('c')]).toEqual([false, false, true]); // b ends 0.5px past: inside the slack
    expect(count()).toBe(1);
  });

  it('the slack is half a pixel, no more', () => {
    render(<Box pills={[{ name: 'a', l: 0, r: 100.6 }]} />);
    FakeRO.last!.fire([screen.getByTestId('box')]);
    expect(cut('a')).toBe(true);
  });

  it('a notification for a child re-measures its box; a pill that fits again is shown', () => {
    const { rerender } = render(
      <Box
        pills={[
          { name: 'a', l: 0, r: 40 },
          { name: 'b', l: 44, r: 140 },
        ]}
      />,
    );
    const ro = FakeRO.last!;
    ro.fire([screen.getByTestId('box')]);
    expect(cut('b')).toBe(true);
    rerender(
      <Box
        pills={[
          { name: 'a', l: 0, r: 40 },
          { name: 'b', l: 44, r: 90 },
        ]}
      />,
    );
    ro.fire([screen.getByTestId('pill-b')]); // the pill shrank; the box did not change
    expect(cut('b')).toBe(false);
    expect(count()).toBe(0);
  });

  it('writes nothing when nothing changed', () => {
    render(
      <Box
        pills={[
          { name: 'a', l: 0, r: 40 },
          { name: 'b', l: 44, r: 140 },
        ]}
      />,
    );
    const ro = FakeRO.last!;
    const box = screen.getByTestId('box');
    ro.fire([box]);
    const set = vi.spyOn(Element.prototype, 'setAttribute');
    const remove = vi.spyOn(Element.prototype, 'removeAttribute');
    ro.fire([box]);
    expect(set).not.toHaveBeenCalledWith('data-clipped', '');
    expect(remove).not.toHaveBeenCalledWith('data-clipped');
  });

  it('re-measures when the project chip mounts later and pushes the pills right', async () => {
    const { rerender } = render(
      <Box
        pills={[
          { name: 'a', l: 0, r: 40 },
          { name: 'b', l: 44, r: 90 },
        ]}
      />,
    );
    const ro = FakeRO.last!;
    ro.fire([screen.getByTestId('box')]);
    expect(count()).toBe(0);
    rerender(
      <Box
        chip={30}
        pills={[
          { name: 'a', l: 34, r: 74 },
          { name: 'b', l: 78, r: 124 },
        ]}
      />,
    );
    // No size changed, so no notification: the mutation observer must re-measure, and observe the new chip.
    await waitFor(() => expect(cut('b')).toBe(true));
    expect(count()).toBe(1);
    expect(ro.observed.has(screen.getByText('chip'))).toBe(true);
  });

  it('re-measures when a label is renamed (same key, new text)', async () => {
    const { rerender } = render(<Box pills={[{ name: 'a', l: 0, r: 40 }]} />);
    FakeRO.last!.fire([screen.getByTestId('box')]);
    const el = screen.getByTestId('pill-a');
    act(() => {
      el.dataset.r = '130';
      el.firstChild!.textContent = 'a much longer name';
    });
    await waitFor(() => expect(cut('a')).toBe(true));
    rerender(<Box pills={[{ name: 'a', l: 0, r: 130 }]} />);
    expect(count()).toBe(1);
  });

  it('stops observing on unmount', () => {
    const { unmount } = render(<Box pills={[{ name: 'a', l: 0, r: 40 }]} />);
    const ro = FakeRO.last!;
    unmount();
    expect(ro.observed.size).toBe(0);
  });

  it('disconnects its mutation observer on unmount', () => {
    const disconnect = vi.spyOn(MutationObserver.prototype, 'disconnect');
    const { unmount } = render(<Box pills={[{ name: 'a', l: 0, r: 40 }]} />);
    expect(disconnect).not.toHaveBeenCalled();
    unmount();
    expect(disconnect).toHaveBeenCalledTimes(1);
  });

  it('a late font measures the mounted boxes again, and nothing after unmount', async () => {
    const fonts = new EventTarget();
    Object.defineProperty(document, 'fonts', { value: fonts, configurable: true });
    try {
      const { unmount } = render(<Box pills={[{ name: 'a', l: 0, r: 40 }]} />);
      FakeRO.last!.fire([screen.getByTestId('box')]);
      const el = screen.getByTestId('pill-a');
      el.dataset.r = '130'; // the font made the pill wider; no size notification
      fonts.dispatchEvent(new Event('loadingdone'));
      await waitFor(() => expect(cut('a')).toBe(true));
      expect(knownLabelBoxes()).toBe(1);
      unmount();
      expect(knownLabelBoxes()).toBe(0);
      const reads = vi.mocked(Element.prototype.getBoundingClientRect);
      reads.mockClear();
      fonts.dispatchEvent(new Event('loadingdone'));
      await new Promise((r) => setTimeout(r, 10));
      expect(reads).not.toHaveBeenCalled();
    } finally {
      delete (document as unknown as { fonts?: unknown }).fonts;
    }
  });

  it('stops observing a child that leaves the box', async () => {
    const { rerender } = render(
      <Box
        pills={[
          { name: 'a', l: 0, r: 40 },
          { name: 'b', l: 44, r: 80 },
        ]}
      />,
    );
    const ro = FakeRO.last!;
    const b = screen.getByTestId('pill-b');
    expect(ro.observed.has(b)).toBe(true);
    rerender(<Box pills={[{ name: 'a', l: 0, r: 40 }]} />);
    await waitFor(() => expect(ro.observed.has(b)).toBe(false));
    expect(ro.observed.has(screen.getByTestId('pill-a'))).toBe(true);
  });
});
