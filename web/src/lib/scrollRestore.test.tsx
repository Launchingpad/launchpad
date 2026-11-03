import { useEffect, useRef, useState } from 'react';
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { render } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import type { Cycle, Issue } from '../api/types';
import { joinServer, page } from '../test/join';
import { makeIssue, states, team } from '../test/mockApi';
import type { InboxNotification } from '../api/types';
import { currentLocation, renderApp, testClient } from '../test/render';
import { nextPageStep, useEntryMemory, useScrollRestore } from './backNav';

// LAUN-90. jsdom has no layout and does not clamp scrollTop, so these tests prove that the offset is read from
// the container, kept per history entry and written back to the SAME kind of container after Back (a new
// element, scrollTop 0). They cannot prove that a real browser scrolls to it; see web-decisions.md (LAUN-90).

/** Remembers whether `text` was ever in the page after this call, even for one frame (a MutationObserver sees what a poll can miss). */
function watchText(text: string) {
  let seen = false;
  const look = () => {
    if (document.body.textContent?.includes(text)) seen = true;
  };
  const mo = new MutationObserver(look);
  mo.observe(document.body, { childList: true, subtree: true, characterData: true });
  look();
  return { seen: () => seen, stop: () => mo.disconnect() };
}

const settle = (ms = 80) => act(() => new Promise<void>((r) => setTimeout(r, ms)));
const scrolled = (el: HTMLElement, top: number, left = 0) => {
  el.scrollTop = top;
  el.scrollLeft = left;
  act(() => {
    el.dispatchEvent(new Event('scroll'));
  });
};

/** Gives the element a scrollTop that stops at `max()` like a real scroller. Returns an undo. */
function clampScroll(match: (el: Element) => boolean, max: (el: Element) => number) {
  const proto = Element.prototype;
  const orig = Object.getOwnPropertyDescriptor(proto, 'scrollTop')!;
  const values = new WeakMap<Element, number>();
  Object.defineProperty(proto, 'scrollTop', {
    configurable: true,
    get(this: Element) {
      return match(this) ? (values.get(this) ?? 0) : orig.get!.call(this);
    },
    set(this: Element, v: number) {
      if (match(this)) values.set(this, Math.max(0, Math.min(v, max(this))));
      else orig.set!.call(this, v);
    },
  });
  return () => Object.defineProperty(proto, 'scrollTop', orig);
}

describe('useScrollRestore loadMore (LAUN-90)', () => {
  // A pager: 10 rows at first, 10 more for each loadMore call, a row is 10px, the box shows 100px.
  let rowsAtStart = 50;
  function Pager({ withLoadMore }: { withLoadMore: boolean }) {
    const [rows, setRows] = useState(rowsAtStart);
    const [busy, setBusy] = useState(false);
    const ref = useRef<HTMLDivElement>(null);
    const rowsRef = useRef(rows);
    rowsRef.current = rows;
    useScrollRestore<HTMLDivElement>('pager', ref, true, () => {
      if (!withLoadMore || rows >= 50) return false;
      if (!busy) {
        setBusy(true);
        setTimeout(() => {
          setRows((r) => r + 10);
          setBusy(false);
        }, 5);
      }
      return true;
    });
    const nav = useNavigate();
    return (
      <div>
        <div ref={ref} data-testid="pager" data-rows={rows}>
          {Array.from({ length: rows }, (_, i) => (
            <p key={i} />
          ))}
        </div>
        <button onClick={() => nav('/detail')}>open</button>
      </div>
    );
  }
  function Detail() {
    const nav = useNavigate();
    return <button onClick={() => nav(-1)}>back</button>;
  }
  const run = async (withLoadMore: boolean) => {
    const undo = clampScroll(
      (el) => (el as HTMLElement).dataset.testid === 'pager',
      (el) => Number((el as HTMLElement).dataset.rows) * 10 - 100,
    );
    try {
      rowsAtStart = 50;
      render(
        <MemoryRouter initialEntries={[`/p${withLoadMore ? 1 : 2}`]}>
          <Routes>
            <Route path="/p1" element={<Pager withLoadMore />} />
            <Route path="/p2" element={<Pager withLoadMore={false} />} />
            <Route path="/detail" element={<Detail />} />
          </Routes>
        </MemoryRouter>,
      );
      scrolled(screen.getByTestId('pager'), 380); // reachable with 50 rows
      expect(screen.getByTestId('pager').scrollTop).toBe(380);
      act(() => screen.getByText('open').click());
      rowsAtStart = 10; // after Back only the first page is there: 0..0 is the most it can reach
      act(() => screen.getByText('back').click());
      if (withLoadMore)
        await waitFor(() => expect(screen.getByTestId('pager').dataset.rows).toBe('50'), { timeout: 3000 });
      else await settle(300);
      await settle(100);
      const box = screen.getByTestId('pager');
      return { rows: box.dataset.rows, top: box.scrollTop }; // read before the clamp is undone
    } finally {
      undo();
    }
  };

  it('loads the pages it needs until the saved offset is reachable', async () => {
    const r = await run(true);
    expect(r).toEqual({ rows: '50', top: 380 });
  });

  it('without loadMore the same offset is out of reach (the control)', async () => {
    const r = await run(false);
    expect(r).toEqual({ rows: '10', top: 0 });
  });
});

describe('useEntryMemory (LAUN-90)', () => {
  function Pick({ at }: { at: string }) {
    const [saved, save] = useEntryMemory('pick');
    const nav = useNavigate();
    const [v, setV] = useState(saved);
    useEffect(() => save(v));
    return (
      <div>
        <span data-testid={`v-${at}`}>{v ?? 'none'}</span>
        <button onClick={() => setV('row-7')}>pick</button>
        <button onClick={() => nav('/detail')}>open</button>
        <button onClick={() => nav(at === 'a' ? '/b' : '/a')}>other</button>
      </div>
    );
  }
  function Detail() {
    const nav = useNavigate();
    return <button onClick={() => nav(-1)}>back</button>;
  }
  it('gives the value back to the same entry, and not to another entry', async () => {
    render(
      <MemoryRouter initialEntries={['/a']}>
        <Routes>
          <Route path="/a" element={<Pick key="a" at="a" />} />
          <Route path="/b" element={<Pick key="b" at="b" />} />
          <Route path="/detail" element={<Detail />} />
        </Routes>
      </MemoryRouter>,
    );
    act(() => screen.getByText('pick').click());
    expect(screen.getByTestId('v-a')).toHaveTextContent('row-7');
    act(() => screen.getByText('open').click());
    act(() => screen.getByText('back').click());
    expect(screen.getByTestId('v-a')).toHaveTextContent('row-7');
    act(() => screen.getByText('other').click()); // a new entry for /b: nothing saved
    expect(screen.getByTestId('v-b')).toHaveTextContent('none');
  });
});

// ---- the containers ----

const nrow = (n: number): InboxNotification => ({
  id: `n${n}`,
  kind: 'comment',
  createdAt: `2026-10-26T10:0${9 - n}:00Z`,
  readAt: null,
  archivedAt: null,
  actor: { id: 'u2', username: 'ann', name: 'Ann' },
  issue: { id: `i${n}`, identifier: `ORB-${n}`, title: `Issue ${n}`, teamKey: 'ORB', stateName: 'Todo' },
  project: null,
  data: {},
});

describe('inbox list (LAUN-90)', () => {
  it('Back returns to the same scroll offset and the same selected row', async () => {
    const rows = [nrow(1), nrow(2), nrow(3), nrow(4)];
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/notifications': () => ({ json: { items: rows, nextCursor: null } }),
        'GET /api/v1/orgs/:org/notifications/unread-count': () => ({ json: { count: 4 } }),
        'PATCH /api/v1/orgs/:org/notifications/:id': (r) => ({
          json: rows.find((x) => x.id === r.params.id),
        }),
        'GET /api/v1/orgs/:org/issues/:issue': (r) => ({ json: makeIssue(Number(r.params.issue!.slice(4))) }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/inbox?filter=all');
    const list = await screen.findByRole('list', { name: 'Notifications' });
    const box = list.parentElement!;
    // The list is the scroll area on the page, not the popover (no popover is open here).
    expect(box.className).toContain('pageList');
    await user.keyboard('jj'); // third row
    expect(document.querySelector('[data-focused="true"]')).toHaveAttribute('data-notification-id', 'n3');
    scrolled(box, 250);
    await user.keyboard('{Enter}');
    await screen.findByRole('group', { name: 'Issue actions' });
    await user.click(screen.getByRole('button', { name: 'Back' }));
    const again = (await screen.findByRole('list', { name: 'Notifications' })).parentElement!;
    expect(again).not.toBe(box); // a new element: the offset is a real restore
    await waitFor(() => expect(again.scrollTop).toBe(250));
    expect(document.querySelector('[data-focused="true"]')).toHaveAttribute('data-notification-id', 'n3');
  });

  it('a new visit to the inbox (a new history entry, same address) starts at the top with the first row', async () => {
    const rows = [nrow(1), nrow(2), nrow(3)];
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/notifications': () => ({ json: { items: rows, nextCursor: null } }),
        'GET /api/v1/orgs/:org/notifications/unread-count': () => ({ json: { count: 3 } }),
        'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/inbox');
    const list = await screen.findByRole('list', { name: 'Notifications' });
    await user.keyboard('jj');
    scrolled(list.parentElement!, 90);
    await user.click(screen.getByRole('link', { name: 'All issues' }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issues'));
    await user.click(screen.getByRole('link', { name: /^Inbox/ }));
    const fresh = (await screen.findByRole('list', { name: 'Notifications' })).parentElement!;
    await settle();
    expect(fresh).not.toBe(list.parentElement);
    expect(fresh.scrollTop).toBe(0);
    expect(document.querySelector('[data-focused="true"]')).toHaveAttribute('data-notification-id', 'n1');
  });
});

const cardIssues = (): Issue[] =>
  Array.from({ length: 6 }, (_, i) =>
    makeIssue(i + 1, {
      title: `Card ${i + 1}`,
      state: (() => {
        const s = states.find((x) => x.name === 'Todo')!;
        return { id: s.id, name: s.name, type: s.type, color: s.color };
      })(),
    }),
  );

describe('board column (LAUN-90)', () => {
  it('Back restores the inner scroll of a column and the sideways scroll', async () => {
    const issues = cardIssues();
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/issues': () => ({ json: page(issues) }),
        'GET /api/v1/orgs/:org/issues/:issue': (r) => ({
          json: issues.find((i) => i.identifier === r.params.issue),
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board&q=col');
    await screen.findByText('Card 1');
    const todo = document.querySelector('[data-column="Todo"] [data-cards]') as HTMLElement;
    const board = screen.getByRole('group', { name: 'Board' });
    scrolled(todo, 140);
    scrolled(board, 0, 310);
    await user.click(screen.getByRole('link', { name: /Card 2/ }));
    await screen.findByRole('group', { name: 'Issue actions' });
    await user.click(screen.getByRole('button', { name: 'Back' }));
    await screen.findByText('Card 1');
    const todo2 = document.querySelector('[data-column="Todo"] [data-cards]') as HTMLElement;
    const board2 = screen.getByRole('group', { name: 'Board' });
    expect(todo2).not.toBe(todo);
    await waitFor(() => expect(todo2.scrollTop).toBe(140));
    expect(board2.scrollLeft).toBe(310);
    // Another column was not scrolled, so it is not moved.
    expect((document.querySelector('[data-column="Backlog"] [data-cards]') as HTMLElement).scrollTop).toBe(0);
  });
});

describe('issue list pages (LAUN-90)', () => {
  it('Back loads more pages until the saved offset is reachable', async () => {
    const all = Array.from({ length: 6 }, (_, i) => makeIssue(i + 1, { title: `Row ${i + 1}` }));
    let size = 6;
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/issues': (r) => {
          const from = Number(r.url.searchParams.get('cursor') ?? 0);
          const next = from + size;
          return {
            json: { items: all.slice(from, next), nextCursor: next < all.length ? String(next) : null },
          };
        },
        'GET /api/v1/orgs/:org/issues/:issue': (r) => ({
          json: all.find((i) => i.identifier === r.params.issue),
        }),
      },
    });
    // 100px per row: the box can scroll to (rows - 1) * 100 at most.
    const undo = clampScroll(
      (el) => (el as HTMLElement).dataset.testid === 'issues-scroll',
      (el) => el.querySelectorAll('[data-issue-id]').length * 100 - 100,
    );
    try {
      const client = testClient();
      const user = userEvent.setup();
      renderApp('/orbit/issues?q=pages', client);
      await screen.findByText('Row 6');
      const box = screen.getByTestId('issues-scroll');
      scrolled(box, 500); // reachable: 6 rows
      await user.click(screen.getByRole('link', { name: /Row 2/ }));
      await screen.findByRole('group', { name: 'Issue actions' });
      // The list cache is gone and the server now gives 2 rows a page: only 2 rows load at first.
      client.removeQueries({ queryKey: ['org', 'orbit', 'issues'], predicate: (q) => q.queryKey.length > 3 });
      size = 2;
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await screen.findByText('Row 1');
      await waitFor(() => expect(screen.getByText('Row 6')).toBeInTheDocument(), { timeout: 4000 });
      await waitFor(() => expect(screen.getByTestId('issues-scroll').scrollTop).toBe(500), { timeout: 4000 });
    } finally {
      undo();
    }
  });
});

// ---- cycles ----
const day = 86400_000;
const iso = (d: number) => new Date(Date.now() + d * day).toISOString();
const cy = (n: number, from: number, to: number, over: Partial<Cycle> = {}): Cycle => ({
  id: `cy${n}`,
  team: { id: 'team1', key: 'ORB', name: 'Orbit' },
  number: n,
  name: null,
  description: '',
  startsAt: iso(from),
  endsAt: iso(to),
  completedAt: null,
  createdAt: iso(from),
  updatedAt: iso(from),
  archivedAt: null,
  ...over,
});
function cycleServer() {
  // Current 20, next 21, and twelve past cycles (the page shows 10 until "Show all").
  const all = [
    cy(20, -2, 12),
    cy(21, 12, 26),
    ...Array.from({ length: 12 }, (_, i) =>
      cy(8 + i, -16 - i * 14, -2 - i * 14, { completedAt: iso(-2 - i * 14) }),
    ),
  ];
  joinServer({
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([{ ...team, cyclesEnabled: true }]) }),
      'GET /api/v1/orgs/:org/cycles': () => ({ json: page(all) }),
      'GET /api/v1/orgs/:org/cycles/:cycle/snapshots': () => ({ json: page([]) }),
    },
  });
}

describe('cycle pages (LAUN-90)', () => {
  it('the cycle list restores its scroll, and the expanded past list that makes the offset reachable', async () => {
    cycleServer();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycles');
    await screen.findByRole('region', { name: 'Current' });
    await user.click(await screen.findByRole('button', { name: /Show all 12 past cycles/ }));
    const box = screen.getByRole('region', { name: 'Past' }).parentElement!;
    expect(within(box).getAllByRole('link').length).toBeGreaterThan(12);
    scrolled(box, 640);
    await user.click(within(box).getAllByRole('link')[0]!);
    await waitFor(() => expect(currentLocation.pathname).toMatch(/\/cycle\/\d+$/));
    act(() => currentLocation.go(-1));
    const back = (await screen.findByRole('region', { name: 'Past' })).parentElement!;
    expect(back).not.toBe(box);
    expect(within(back).getAllByRole('link').length).toBeGreaterThan(12); // still expanded
    await waitFor(() => expect(back.scrollTop).toBe(640));
  });

  it('the cycle page that is not an issue browser restores its container', async () => {
    cycleServer();
    const user = userEvent.setup();
    // A cycle number that does not exist: "Cycle not found" is in the page's own scroll container.
    renderApp('/orbit/team/ORB/cycle/999');
    const link = await screen.findByRole('link', { name: 'Back to the cycles' });
    const box = link.closest('div[class*="scroll"]') as HTMLElement;
    expect(box).toBeTruthy();
    scrolled(box, 75);
    await user.click(link);
    await screen.findByRole('region', { name: 'Current' });
    act(() => currentLocation.go(-1));
    const again = (await screen.findByRole('link', { name: 'Back to the cycles' })).closest(
      'div[class*="scroll"]',
    ) as HTMLElement;
    expect(again).not.toBe(box);
    await waitFor(() => expect(again.scrollTop).toBe(75));
  });
});

describe('loadMore wiring and failed pages (LAUN-90)', () => {
  const manyRows = Array.from({ length: 6 }, (_, i) => nrow(i + 1));
  function inboxPaged(size: () => number, failSecond: () => boolean) {
    return joinServer({
      routes: {
        'GET /api/v1/orgs/:org/notifications': (r) => {
          const from = Number(r.url.searchParams.get('cursor') ?? 0);
          if (from > 0 && failSecond()) return { status: 500, json: { error: 'boom', code: 'internal' } };
          const next = from + size();
          return {
            json: {
              items: manyRows.slice(from, next),
              nextCursor: next < manyRows.length ? String(next) : null,
            },
          };
        },
        'GET /api/v1/orgs/:org/notifications/unread-count': () => ({ json: { count: 6 } }),
        'GET /api/v1/orgs/:org/issues/:issue': (r) => ({ json: makeIssue(Number(r.params.issue!.slice(4))) }),
      },
    });
  }
  const inboxBox = () => screen.getByRole('list', { name: 'Notifications' }).parentElement!;
  const undoable = () =>
    clampScroll(
      (el) => (el as HTMLElement).className.includes('pageList'),
      (el) => el.querySelectorAll('[data-notification-id]').length * 100 - 100,
    );

  it('inbox: Back loads pages (cache dropped, server pages by 2) until the offset is reachable', async () => {
    let size = 6;
    inboxPaged(
      () => size,
      () => false,
    );
    const undo = undoable();
    try {
      const client = testClient();
      const user = userEvent.setup();
      renderApp('/orbit/inbox?filter=all&q=paged', client);
      await screen.findByText('Issue 6');
      scrolled(inboxBox(), 500);
      await user.click(screen.getByRole('link', { name: /Issue 2/ }));
      await screen.findByRole('group', { name: 'Issue actions' });
      client.removeQueries({ queryKey: ['org', 'orbit', 'notifications'] });
      size = 2;
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await screen.findByText('Issue 1');
      await waitFor(() => expect(screen.getByText('Issue 6')).toBeInTheDocument(), { timeout: 4000 });
      await waitFor(() => expect(inboxBox().scrollTop).toBe(500), { timeout: 4000 });
    } finally {
      undo();
    }
  });

  it('inbox: a failed page stops the loading (no retry storm) and the restore gives up', async () => {
    let size = 6;
    let fail = false;
    const api = inboxPaged(
      () => size,
      () => fail,
    );
    const undo = undoable();
    try {
      const client = testClient();
      const user = userEvent.setup();
      renderApp('/orbit/inbox?filter=all&q=fail', client);
      await screen.findByText('Issue 6');
      scrolled(inboxBox(), 500);
      await user.click(screen.getByRole('link', { name: /Issue 2/ }));
      await screen.findByRole('group', { name: 'Issue actions' });
      client.removeQueries({ queryKey: ['org', 'orbit', 'notifications'] });
      size = 2;
      fail = true;
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await screen.findByText('Issue 1');
      await screen.findByText(/Could not load more/);
      const page2 = () =>
        api.calls.filter(
          (c) => c.url.pathname.endsWith('/notifications') && c.url.searchParams.get('cursor'),
        );
      // The inbox keeps its two rows, so the restore stops at the end of them (nothing thrown).
      await waitFor(() => expect(inboxBox().scrollTop).toBe(100));
      expect(page2().length).toBeLessThanOrEqual(2);
    } finally {
      undo();
    }
  });

  it('issue list: a failed page stops the loading (no retry storm) and the restore gives up', async () => {
    const all = Array.from({ length: 6 }, (_, i) => makeIssue(i + 1, { title: `Row ${i + 1}` }));
    let size = 6;
    let fail = false;
    const api = joinServer({
      routes: {
        'GET /api/v1/orgs/:org/issues': (r) => {
          const from = Number(r.url.searchParams.get('cursor') ?? 0);
          if (from > 0 && fail) return { status: 500, json: { error: 'boom', code: 'internal' } };
          const next = from + size;
          return {
            json: { items: all.slice(from, next), nextCursor: next < all.length ? String(next) : null },
          };
        },
        'GET /api/v1/orgs/:org/issues/:issue': (r) => ({
          json: all.find((i) => i.identifier === r.params.issue),
        }),
      },
    });
    const undo = clampScroll(
      (el) => (el as HTMLElement).dataset.testid === 'issues-scroll',
      (el) => el.querySelectorAll('[data-issue-id]').length * 100 - 100,
    );
    let rowShown: ReturnType<typeof watchText> | undefined;
    try {
      const client = testClient();
      const user = userEvent.setup();
      renderApp('/orbit/issues?q=failpages', client);
      await screen.findByText('Row 6');
      scrolled(screen.getByTestId('issues-scroll'), 500);
      await user.click(screen.getByRole('link', { name: /Row 2/ }));
      await screen.findByRole('group', { name: 'Issue actions' });
      client.removeQueries({ queryKey: ['org', 'orbit', 'issues'], predicate: (q) => q.queryKey.length > 3 });
      size = 2;
      fail = true;
      // "Row 1" shows only until page 2 fails and the list turns into its error state. On a loaded runner the
      // failure can come before a poll, so watch the DOM from before the click instead of polling for the row.
      rowShown = watchText('Row 1');
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await waitFor(() => expect(rowShown?.seen()).toBe(true));
      // The failed page makes the issue list show its full error state (the rows leave the DOM), so no scrollTop
      // is asserted here: only that the loading stopped and nothing threw.
      await screen.findByText('Could not load issues');
      await settle(300);
      const page2 = api.calls.filter(
        (c) => c.url.pathname.endsWith('/issues') && c.url.searchParams.get('cursor'),
      );
      expect(page2.length).toBeLessThanOrEqual(2);
    } finally {
      rowShown?.stop();
      undo();
    }
  });
});

describe('nextPageStep (LAUN-90)', () => {
  const base = { hasNextPage: true, failed: false, busy: false, pages: 1, asked: '', entry: 'k' };
  it('asks for the next page once per entry and page count', () => {
    expect(nextPageStep(base)).toEqual({ fetch: true, wait: true });
    expect(nextPageStep({ ...base, asked: 'k|1' })).toEqual({ fetch: false, wait: true });
    expect(nextPageStep({ ...base, asked: 'other|1' }).fetch).toBe(true); // another history entry asks again
    expect(nextPageStep({ ...base, pages: 2, asked: 'k|1' }).fetch).toBe(true);
  });
  it('does nothing and does not wait once a page failed, when nothing is left, or at 10 pages', () => {
    expect(nextPageStep({ ...base, failed: true })).toEqual({ fetch: false, wait: false });
    expect(nextPageStep({ ...base, hasNextPage: false })).toEqual({ fetch: false, wait: false });
    expect(nextPageStep({ ...base, pages: 10 })).toEqual({ fetch: false, wait: false });
  });
});
