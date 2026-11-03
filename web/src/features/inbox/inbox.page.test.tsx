import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { InboxNotification } from '../../api/types';
import { FakeEventSource } from '../../test/fakes';
import { callsTo, joinServer } from '../../test/join';
import { makeIssue } from '../../test/mockApi';
import type { Handler } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';

const row = (n: number, over: Partial<InboxNotification> = {}): InboxNotification => ({
  id: `n${n}`,
  kind: 'comment',
  createdAt: `2026-10-26T10:0${9 - n}:00Z`, // n1 is the newest
  readAt: null,
  archivedAt: null,
  actor: { id: 'u2', username: 'ann', name: 'Ann' },
  issue: { id: `i${n}`, identifier: `ORB-${n}`, title: `Issue ${n}`, teamKey: 'ORB', stateName: 'Todo' },
  project: null,
  data: {},
  ...over,
});

/** A small stateful inbox server: honours state / archived, PATCH and read-all like the API. */
function inboxServer(initial: InboxNotification[], extra: Record<string, Handler> = {}) {
  const rows = initial.map((r) => ({ ...r }));
  const counted = (r: InboxNotification) => !r.readAt && !r.archivedAt;
  const api = joinServer({
    routes: {
      'GET /api/v1/orgs/:org/notifications': (r) => {
        const archived = r.url.searchParams.get('archived') === 'true';
        const unread = r.url.searchParams.get('state') === 'unread';
        const items = rows
          .filter((x) => (archived ? !!x.archivedAt : !x.archivedAt) && (!unread || !x.readAt))
          .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
        return { json: { items, nextCursor: null } };
      },
      'GET /api/v1/orgs/:org/notifications/unread-count': () => ({
        json: { count: rows.filter(counted).length },
      }),
      'PATCH /api/v1/orgs/:org/notifications/:id': (r) => {
        const x = rows.find((y) => y.id === r.params.id)!;
        const b = r.body as { read?: boolean; archived?: boolean };
        if (b.read !== undefined) x.readAt = b.read ? (x.readAt ?? '2026-10-26T11:00:00Z') : null;
        if (b.archived !== undefined) x.archivedAt = b.archived ? '2026-10-26T11:00:00Z' : null;
        return { json: x };
      },
      'POST /api/v1/orgs/:org/notifications/read-all': (r) => {
        const before = (r.body as { before: string }).before;
        let marked = 0;
        for (const x of rows)
          if (counted(x) && x.createdAt <= before) {
            x.readAt = '2026-10-26T11:00:00Z';
            marked++;
          }
        return { json: { marked } };
      },
      'GET /api/v1/orgs/:org/issues/:issue': (r) => ({ json: makeIssue(Number(r.params.issue!.slice(4))) }),
      ...extra,
    },
  });
  return { api, rows };
}

const badge = () => screen.queryAllByTestId('unread-badge');
const listRows = () => screen.getAllByRole('listitem');

describe('inbox page', () => {
  it('lists rows with the actor, the issue and what happened, newest first', async () => {
    inboxServer([
      row(1, { kind: 'assigned' }),
      row(2, { kind: 'state_change', data: { fromStateName: 'Todo', toStateName: 'Done' } }),
    ]);
    renderApp('/orbit/inbox');
    expect(await screen.findByRole('heading', { name: 'Inbox' })).toBeInTheDocument();
    const items = await screen.findAllByRole('listitem');
    const first = items.find((i) => within(i).queryByText('Issue 1'))!;
    expect(within(first).getByText('ORB-1')).toBeInTheDocument();
    expect(within(first).getByText('Ann assigned this to you')).toBeInTheDocument();
    expect(within(first).getByRole('link')).toHaveAttribute('href', '/orbit/issue/ORB-1');
    expect(screen.getByText('Ann moved this from Todo to Done')).toBeInTheDocument();
    expect(document.title).toBe('Orbit › Inbox');
  });

  it('shows the unread badge in the sidebar and follows the stream count', async () => {
    inboxServer([row(1), row(2), row(3, { readAt: '2026-10-26T10:00:00Z' })]);
    renderApp('/orbit/inbox');
    await waitFor(() => expect(badge().length).toBeGreaterThan(0));
    expect(badge()[0]).toHaveTextContent('2');
    const stream = await waitFor(() => {
      const s = FakeEventSource.inboxStreams[0];
      expect(s).toBeDefined();
      return s!;
    });
    stream.open();
    stream.emit('count', { count: 7 });
    await waitFor(() => expect(badge()[0]).toHaveTextContent('7'));
  });

  it('filters: Unread by default, All adds read rows, Archived lists archived ones', async () => {
    const { api } = inboxServer([
      row(1),
      row(2, { readAt: '2026-10-26T10:00:00Z' }),
      row(3, { archivedAt: '2026-10-26T10:00:00Z' }),
    ]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    expect(screen.queryByText('Issue 2')).not.toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'All' }));
    await screen.findByText('Issue 2');
    expect(screen.queryByText('Issue 3')).not.toBeInTheDocument();
    expect(currentLocation.search).toBe('?filter=all');
    await user.click(screen.getByRole('button', { name: 'Archived' }));
    await screen.findByText('Issue 3');
    expect(screen.queryByText('Issue 1')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Mark all as read' })).not.toBeInTheDocument();
    const qs = callsTo(api, 'GET', '/notifications').map((c) => c.url.search);
    expect(qs.some((q) => q.includes('state=unread'))).toBe(true);
    expect(qs.some((q) => q.includes('archived=true'))).toBe(true);
  });

  it('marks one row read and unread, and the badge follows at once', async () => {
    const { api } = inboxServer([row(1), row(2)]);
    renderApp('/orbit/inbox?filter=all');
    await screen.findByText('Issue 1');
    await waitFor(() => expect(badge()[0]).toHaveTextContent('2'));
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Mark as read: ORB-1' }));
    await waitFor(() => expect(badge()[0]).toHaveTextContent('1'));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[0]!.body).toEqual({ read: true });
    await user.click(await screen.findByRole('button', { name: 'Mark as unread: ORB-1' }));
    await waitFor(() => expect(badge()[0]).toHaveTextContent('2'));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[1]!.body).toEqual({ read: false });
  });

  it('archives a row: it leaves the list and the badge drops; Archived can undo it', async () => {
    const { api } = inboxServer([row(1), row(2)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Archive: ORB-1' }));
    await waitFor(() => expect(screen.queryByText('Issue 1')).not.toBeInTheDocument());
    await waitFor(() => expect(badge()[0]).toHaveTextContent('1'));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[0]!.body).toEqual({ archived: true });
    await user.click(screen.getByRole('button', { name: 'Archived' }));
    await user.click(await screen.findByRole('button', { name: 'Unarchive: ORB-1' }));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[1]!.body).toEqual({ archived: false });
  });

  it('mark all read sends before = createdAt of the newest loaded row', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    await waitFor(() => expect(badge()[0]).toHaveTextContent('3'));
    await userEvent.setup().click(screen.getByRole('button', { name: 'Mark all as read' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/notifications/read-all')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/notifications/read-all')[0]!.body).toEqual({
      before: '2026-10-26T10:08:00Z',
    });
    await waitFor(() => expect(badge()).toHaveLength(0));
    expect(await screen.findByText('You are all caught up')).toBeInTheDocument();
  });

  it('opening an issue from the inbox marks it read and goes to the issue', async () => {
    const { api } = inboxServer([row(1), row(2)]);
    renderApp('/orbit/inbox');
    await userEvent.setup().click(await screen.findByRole('link', { name: /Issue 1/ }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-1'));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[0]!.body).toEqual({ read: true });
  });

  it('puts the row back and says why when the server refuses', async () => {
    inboxServer([row(1)], {
      'PATCH /api/v1/orgs/:org/notifications/:id': () => ({
        status: 500,
        json: { error: 'boom', code: 'internal' },
      }),
    });
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Archive: ORB-1' }));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(await screen.findByText('Issue 1')).toBeInTheDocument();
    await waitFor(() => expect(badge()[0]).toHaveTextContent('1'));
  });

  it('shows the empty state of each filter', async () => {
    inboxServer([]);
    renderApp('/orbit/inbox');
    expect(await screen.findByText('You are all caught up')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'All' }));
    expect(await screen.findByText('No notifications yet')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Archived' }));
    expect(await screen.findByText('Nothing archived')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Archived' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('shows an error state with Try again', async () => {
    let fail = true;
    inboxServer([row(1)], {
      'GET /api/v1/orgs/:org/notifications': () =>
        fail
          ? { status: 500, json: { error: 'down', code: 'internal' } }
          : { json: { items: [row(1)], nextCursor: null } },
    });
    renderApp('/orbit/inbox');
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText('Could not load this')).toBeInTheDocument();
    fail = false;
    await userEvent.setup().click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('Issue 1')).toBeInTheDocument();
  });

  it('loads the next page', async () => {
    inboxServer([row(1), row(2)], {
      'GET /api/v1/orgs/:org/notifications': (r) =>
        r.url.searchParams.get('cursor')
          ? { json: { items: [row(2)], nextCursor: null } }
          : { json: { items: [row(1)], nextCursor: 'c1' } },
    });
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('Issue 2')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
  });
});

describe('inbox failed refetch (LAUN-88)', () => {
  const refetchViaStream = async (count: number) => {
    const stream = await waitFor(() => {
      const s = FakeEventSource.inboxStreams[0];
      expect(s).toBeDefined();
      return s!;
    });
    stream.open();
    stream.emit('count', { count }); // differs from the loaded count: the lists refetch
  };

  it('keeps the loaded rows when a background refetch fails, and recovers on Try again', async () => {
    let fail = false;
    inboxServer([row(1), row(2)], {
      'GET /api/v1/orgs/:org/notifications': () =>
        fail
          ? { status: 500, json: { error: 'down', code: 'internal' } }
          : { json: { items: [row(1), row(2)], nextCursor: null } },
    });
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    const before = screen.getByText('Issue 1').closest('li');
    fail = true;
    await refetchViaStream(9);
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(/Could not refresh\./)).toBeInTheDocument();
    // No full error state, and the same row elements stayed in place (no flicker, no skeleton).
    expect(screen.queryByText('Could not load this')).not.toBeInTheDocument();
    expect(screen.getByText('Issue 1')).toBeInTheDocument();
    expect(screen.getByText('Issue 2')).toBeInTheDocument();
    expect(screen.getByText('Issue 1').closest('li')).toBe(before);
    expect(screen.queryByRole('status', { name: 'Loading' })).not.toBeInTheDocument();
    fail = false;
    await userEvent.setup().click(within(alert).getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
    expect(screen.getByText('Issue 1').closest('li')).toBe(before);
    expect(screen.getByText('Issue 2')).toBeInTheDocument();
  });

  it('keeps the empty state when the refetch of an empty list fails', async () => {
    let fail = false;
    inboxServer([], {
      'GET /api/v1/orgs/:org/notifications': () =>
        fail
          ? { status: 500, json: { error: 'down', code: 'internal' } }
          : { json: { items: [], nextCursor: null } },
    });
    renderApp('/orbit/inbox');
    expect(await screen.findByText('You are all caught up')).toBeInTheDocument();
    fail = true;
    await refetchViaStream(9);
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(/Could not refresh\./)).toBeInTheDocument();
    expect(screen.getByText('You are all caught up')).toBeInTheDocument();
    expect(screen.queryByText('Could not load this')).not.toBeInTheDocument();
  });

  it('disables Try again while the retry runs', async () => {
    let mode: 'ok' | 'fail' | 'hold' = 'ok';
    let release: () => void = () => {};
    inboxServer([row(1)], {
      'GET /api/v1/orgs/:org/notifications': async () => {
        if (mode === 'fail') return { status: 500, json: { error: 'down', code: 'internal' } };
        if (mode === 'hold') await new Promise<void>((res) => (release = res));
        return { json: { items: [row(1)], nextCursor: null } };
      },
    });
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    mode = 'fail';
    await refetchViaStream(9);
    const alert = await screen.findByRole('alert');
    const btn = within(alert).getByRole('button', { name: 'Try again' });
    expect(btn).toBeEnabled();
    mode = 'hold';
    await userEvent.setup().click(btn);
    await waitFor(() => expect(within(screen.getByRole('alert')).getByRole('button')).toBeDisabled());
    expect(screen.getByText('Issue 1')).toBeInTheDocument();
    release();
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('a refetch that fails after a failed Load more says "Could not refresh"', async () => {
    let failAll = false;
    inboxServer([row(1), row(2)], {
      'GET /api/v1/orgs/:org/notifications': (r) =>
        r.url.searchParams.get('cursor') || failAll
          ? { status: 500, json: { error: 'down', code: 'internal' } }
          : { json: { items: [row(1)], nextCursor: 'c1' } },
    });
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText(/Could not load more\./)).toBeInTheDocument();
    failAll = true;
    await refetchViaStream(9);
    expect(await screen.findByText(/Could not refresh\./)).toBeInTheDocument();
    expect(screen.queryByText(/Could not load more\./)).not.toBeInTheDocument();
    expect(screen.getByText('Issue 1')).toBeInTheDocument();
  });

  it('a failed first load still shows the full error state', async () => {
    inboxServer([row(1)], {
      'GET /api/v1/orgs/:org/notifications': () => ({
        status: 500,
        json: { error: 'down', code: 'internal' },
      }),
    });
    renderApp('/orbit/inbox');
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText('Could not load this')).toBeInTheDocument();
    expect(screen.queryByRole('list', { name: 'Notifications' })).not.toBeInTheDocument();
  });

  it('a failed Load more keeps the rows and offers a retry', async () => {
    let failNext = true;
    inboxServer([row(1), row(2)], {
      'GET /api/v1/orgs/:org/notifications': (r) =>
        r.url.searchParams.get('cursor')
          ? failNext
            ? { status: 500, json: { error: 'down', code: 'internal' } }
            : { json: { items: [row(2)], nextCursor: null } }
          : { json: { items: [row(1)], nextCursor: 'c1' } },
    });
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    const alert = await screen.findByRole('alert');
    expect(within(alert).getByText(/Could not load more\./)).toBeInTheDocument();
    expect(screen.getByText('Issue 1')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Load more' })).toBeInTheDocument();
    failNext = false;
    await user.click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('Issue 2')).toBeInTheDocument();
    expect(screen.getByText('Issue 1')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });
});

describe('inbox keyboard', () => {
  it('j and k move the focus, e archives the focused row and the next one takes the focus', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 3');
    const user = userEvent.setup();
    const current = () => listRows().find((r) => r.getAttribute('data-focused') === 'true');
    expect(current()).toHaveTextContent('Issue 1'); // the first row starts focused
    await user.keyboard('j');
    expect(current()).toHaveTextContent('Issue 2');
    await user.keyboard('jj'); // stops at the last row
    expect(current()).toHaveTextContent('Issue 3');
    await user.keyboard('k');
    expect(current()).toHaveTextContent('Issue 2');
    await user.keyboard('e');
    await waitFor(() => expect(screen.queryByText('Issue 2')).not.toBeInTheDocument());
    expect(callsTo(api, 'PATCH', '/notifications/n2')[0]!.body).toEqual({ archived: true });
    expect(current()).toHaveTextContent('Issue 3');
  });

  it('u toggles read, Enter opens the focused row and marks it read', async () => {
    const { api } = inboxServer([row(1), row(2)]);
    renderApp('/orbit/inbox?filter=all');
    await screen.findByText('Issue 2');
    const user = userEvent.setup();
    await user.keyboard('u');
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[0]!.body).toEqual({ read: true });
    await user.keyboard('j{Enter}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-2'));
    expect(callsTo(api, 'PATCH', '/notifications/n2')[0]!.body).toEqual({ read: true });
  });

  it('keys stay quiet while typing in a field', async () => {
    const { api } = inboxServer([row(1)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    await userEvent.setup().keyboard('e');
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
    input.remove();
  });

  it('has names for assistive technology: the list, the filter group, the row buttons', async () => {
    inboxServer([row(1)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    expect(screen.getByRole('list', { name: 'Notifications' })).toBeInTheDocument();
    expect(screen.getByRole('group', { name: 'Show' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Unread' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText(/Unread\./)).toHaveClass('sr-only');
    expect(screen.getAllByText('1 unread')[0]).toHaveClass('sr-only');
  });
});

describe('inbox page focus', () => {
  it('j j Enter opens the third row', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 3');
    await userEvent.setup().keyboard('jj{Enter}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-3'));
    expect(callsTo(api, 'PATCH', '/notifications/n3')[0]!.body).toEqual({ read: true });
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
  });

  it('focusing a row link makes it the row that e acts on', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 3');
    act(() => screen.getByRole('link', { name: /Issue 3/ }).focus());
    await userEvent.setup().keyboard('e');
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n3')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
  });
});

describe('inbox page focus after a key action', () => {
  it('moves DOM focus to the next row link when a focused row is archived', async () => {
    inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 3');
    const user = userEvent.setup();
    await user.keyboard('j'); // DOM focus on row 2's link
    expect(screen.getByRole('link', { name: /Issue 2/ })).toHaveFocus();
    await user.keyboard('e');
    await waitFor(() => expect(screen.queryByText('Issue 2')).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole('link', { name: /Issue 3/ })).toHaveFocus());
  });
});

describe('inbox focus flag', () => {
  it('a refetch does not take focus back after a click on blank space', async () => {
    inboxServer([row(1), row(2)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 2');
    act(() => screen.getByRole('link', { name: /Issue 2/ }).focus());
    const user = userEvent.setup();
    await user.click(document.body); // focus goes to nowhere
    await new Promise((r) => setTimeout(r, 20));
    const stream = FakeEventSource.inboxStreams[0]!;
    stream.open();
    stream.emit('count', { count: 9 }); // differs: the lists refetch
    await waitFor(() => expect(badge()[0]).toHaveTextContent('9'));
    await new Promise((r) => setTimeout(r, 50));
    expect(document.body).toHaveFocus();
  });

  it('a middle click on a row link marks it read', async () => {
    const { api } = inboxServer([row(1)]);
    renderApp('/orbit/inbox');
    const link = await screen.findByRole('link', { name: /Issue 1/ });
    link.dispatchEvent(new MouseEvent('auxclick', { bubbles: true, button: 1 }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(1));
  });
});

describe('inbox popover', () => {
  it('opens from the bell for quick triage and closes on Escape', async () => {
    const { api } = inboxServer([row(1), row(2)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await waitFor(() => expect(badge()[0]).toHaveTextContent('2'));
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    expect(await within(pop).findByText('Issue 1')).toBeInTheDocument();
    await user.click(within(pop).getByRole('button', { name: 'Archive: ORB-1' }));
    expect(callsTo(api, 'PATCH', '/notifications/n1')[0]!.body).toEqual({ archived: true });
    await waitFor(() => expect(within(pop).queryByText('Issue 1')).not.toBeInTheDocument());
    await user.keyboard('{Escape}');
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Inbox quick view' })).not.toBeInTheDocument(),
    );
  });

  it('opening a row closes it, goes to the issue and marks it read; keys work inside it', async () => {
    const { api } = inboxServer([row(1), row(2)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('Issue 2');
    await user.keyboard('je');
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n2')).toHaveLength(1));
    await user.click(await within(pop).findByRole('link', { name: /Issue 1/ }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-1'));
    expect(screen.queryByRole('dialog', { name: 'Inbox quick view' })).not.toBeInTheDocument();
    expect(callsTo(api, 'PATCH', '/notifications/n1')[0]!.body).toEqual({ read: true });
  });

  it('Tab and Shift+Tab move inside the popover without closing it; Esc closes it and focus returns to the bell', async () => {
    inboxServer([row(1), row(2)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    const bell = (await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!;
    await user.click(bell);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    const link1 = await within(pop).findByRole('link', { name: /Issue 1/ });
    await waitFor(() => expect(link1).toHaveFocus());
    await user.tab();
    expect(screen.getByRole('dialog', { name: 'Inbox quick view' })).toBeInTheDocument();
    expect(pop.contains(document.activeElement)).toBe(true);
    expect(document.activeElement).not.toBe(link1);
    await user.tab({ shift: true });
    expect(link1).toHaveFocus();
    expect(screen.getByRole('dialog', { name: 'Inbox quick view' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Inbox quick view' })).not.toBeInTheDocument(),
    );
    expect(bell).toHaveFocus();
  });

  it('reaches the tabs and Mark all as read from the keyboard when the list is empty', async () => {
    inboxServer([]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('You are all caught up');
    await waitFor(() => expect(within(pop).getByRole('button', { name: 'Unread' })).toHaveFocus());
    await user.tab();
    expect(within(pop).getByRole('button', { name: 'All' })).toHaveFocus();
    expect(screen.getByRole('dialog', { name: 'Inbox quick view' })).toBeInTheDocument();
  });

  it('after a row leaves the popover list, the keys still work', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('Issue 3');
    await waitFor(() => expect(within(pop).getByRole('link', { name: /Issue 1/ })).toHaveFocus());
    await user.keyboard('e'); // archives Issue 1, whose link had the focus
    await waitFor(() => expect(within(pop).queryByText('Issue 1')).not.toBeInTheDocument());
    await waitFor(() => expect(pop.contains(document.activeElement)).toBe(true));
    await user.keyboard('e'); // the next row took over
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n2')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/notifications/n2')[0]!.body).toEqual({ archived: true });
  });

  it('j j Enter opens the third row: highlight and focus are one thing', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('Issue 3');
    await waitFor(() => expect(within(pop).getByRole('link', { name: /Issue 1/ })).toHaveFocus());
    await user.keyboard('jj{Enter}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-3'));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n3')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
  });

  it('Tab to a row, then e archives that row', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('Issue 3');
    await waitFor(() => expect(within(pop).getByRole('link', { name: /Issue 1/ })).toHaveFocus());
    await user.tab(); // row 1 archive/read buttons
    await user.tab();
    await user.tab(); // now on row 2's link
    expect(within(pop).getByRole('link', { name: /Issue 2/ })).toHaveFocus();
    await user.keyboard('e');
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n2')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
  });

  it('focuses a panel control while loading, then moves to the first row when it arrives', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    inboxServer([row(1)], {
      'GET /api/v1/orgs/:org/notifications': async () => {
        await gate;
        return { json: { items: [row(1)], nextCursor: null } };
      },
    });
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await waitFor(() => expect(within(pop).getByRole('button', { name: 'Unread' })).toHaveFocus());
    await user.tab();
    expect(screen.getByRole('dialog', { name: 'Inbox quick view' })).toBeInTheDocument();
    await user.tab({ shift: true });
    release();
    await waitFor(() => expect(within(pop).getByRole('link', { name: /Issue 1/ })).toHaveFocus());
  });

  it('a mouse click on a row control moves focus there, so e acts on that row; the click acts once', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('Issue 3');
    await user.click(within(pop).getByRole('button', { name: 'All' })); // read rows stay in All
    await user.click(await within(pop).findByRole('button', { name: 'Mark as read: ORB-3' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n3')).toHaveLength(1));
    await waitFor(() => expect(pop.contains(document.activeElement)).toBe(true));
    await user.keyboard('e');
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n3')).toHaveLength(2));
    expect(callsTo(api, 'PATCH', '/notifications/n3')[0]!.body).toEqual({ read: true });
    expect(callsTo(api, 'PATCH', '/notifications/n3')[1]!.body).toEqual({ archived: true });
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
  });

  it('a click on a row, then Enter, opens that row', async () => {
    const { api } = inboxServer([row(1), row(2), row(3)]);
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    const li = (await within(pop).findByText('Issue 3')).closest('li')!;
    await user.pointer({ keys: '[MouseLeft>]', target: li }); // press on the row, not on a control
    expect(within(pop).getByRole('link', { name: /Issue 3/ })).toHaveFocus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-3'));
    expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(0);
  });

  it('the bell names the unread count', async () => {
    inboxServer([row(1), row(2)]);
    renderApp('/orbit/issues');
    expect(
      (await screen.findAllByRole('button', { name: 'Inbox quick view, 2 unread' })).length,
    ).toBeGreaterThan(0);
  });

  it('page keys do not fire behind an open popover', async () => {
    const { api } = inboxServer([row(1)]);
    renderApp('/orbit/inbox');
    await screen.findByText('Issue 1');
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole('button', { name: /^Inbox quick view/ }))[0]!);
    const pop = await screen.findByRole('dialog', { name: 'Inbox quick view' });
    await within(pop).findByText('Issue 1');
    await user.keyboard('e');
    // one archive, from the popover list, not two
    await waitFor(() => expect(callsTo(api, 'PATCH', '/notifications/n1')).toHaveLength(1));
  });
});

it('lists the Inbox entry first in the sidebar', async () => {
  inboxServer([]);
  renderApp('/orbit/issues');
  const nav = await screen.findByRole('navigation', { name: 'Primary' });
  const links = within(nav).getAllByRole('link');
  expect(links[0]).toHaveTextContent('Inbox');
  expect(links[0]).toHaveAttribute('href', '/orbit/inbox');
});
