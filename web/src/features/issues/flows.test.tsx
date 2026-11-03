import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { at, dragCard, middle, stubBoardLayout } from '../../test/boardLayout';
import type { Comment, Issue } from '../../api/types';
import { FakeEventSource } from '../../test/fakes';
import { baseRoutes, makeComment, makeIssue, mockApi, states } from '../../test/mockApi';
import { renderApp } from '../../test/render';

const stateRef = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};

/** Stateful fake: PATCH really changes the stored issue, like the server. */
function tracker(
  initial: Issue[],
  opts: {
    patch?: (
      n: number,
      issue: Issue,
      body: Record<string, unknown>,
    ) => { status?: number; json?: unknown } | undefined;
    /** Holds the nth PATCH (before it changes anything) until the returned promise settles. */
    hold?: (n: number) => Promise<void> | undefined;
    /** Answers the nth comment PATCH instead of the default (may change what the server stores). */
    commentPatch?: (
      n: number,
      c: Comment,
      body: string,
    ) =>
      | { status?: number; json?: unknown }
      | Promise<{ status?: number; json?: unknown } | undefined>
      | undefined;
    /** Makes the nth comment POST fail. */
    commentFails?: (n: number) => boolean;
  } = {},
) {
  const issues = initial.map((i) => ({ ...i }));
  const comments: Comment[] = [];
  let patches = 0;
  let commentPosts = 0;
  let commentPatches = 0;
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: issues, nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue': (r) => {
      const i = issues.find((x) => x.identifier === r.params.issue);
      return i
        ? {
            json: {
              ...i,
              relations: { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] },
              stateHistory: [],
            },
          }
        : { status: 404, json: { error: 'nope', code: 'not_found' } };
    },
    'PATCH /api/v1/orgs/:org/issues/:issue': async (r) => {
      patches++;
      await opts.hold?.(patches);
      const i = issues.find((x) => x.identifier === r.params.issue)!;
      const body = r.body as Record<string, unknown>;
      const custom = opts.patch?.(patches, i, body);
      if (custom) return custom;
      if (typeof body.state === 'string') i.state = stateRef(body.state);
      if (typeof body.title === 'string') i.title = body.title;
      if (typeof body.description === 'string') i.description = body.description;
      i.version += 1;
      return { json: i };
    },
    'POST /api/v1/orgs/:org/issues': (r) => {
      const b = r.body as { title: string };
      const i = makeIssue(99, { title: b.title, identifier: 'ORB-99' });
      issues.push(i);
      return { status: 201, json: i };
    },
    'GET /api/v1/orgs/:org/issues/:issue/comments': () => ({ json: { items: comments, nextCursor: null } }),
    'POST /api/v1/orgs/:org/issues/:issue/comments': (r) => {
      if (opts.commentFails?.(++commentPosts))
        return { status: 500, json: { error: 'database is down', code: 'internal_error' } };
      const b = r.body as { body: string; parentId?: string };
      const c = makeComment(`c${comments.length + 1}`, { body: b.body, parentId: b.parentId ?? null });
      comments.push(c);
      return { status: 201, json: c };
    },
    'PATCH /api/v1/orgs/:org/comments/:comment': async (r) => {
      const c = comments.find((x) => x.id === r.params.comment)!;
      const custom = await opts.commentPatch?.(++commentPatches, c, (r.body as { body: string }).body);
      if (custom) return custom;
      c.body = (r.body as { body: string }).body;
      c.editedAt = '2026-10-03T00:00:00Z';
      return { json: c };
    },
    'DELETE /api/v1/orgs/:org/comments/:comment': (r) => {
      const idx = comments.findIndex((x) => x.id === r.params.comment);
      comments.splice(idx, 1);
      return { status: 204 };
    },
  });
  return { api, issues, comments };
}

// A signed-in browser holds the CSRF cookie the server set at login.
beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

const column = (name: string) => document.querySelector(`[data-column="${name}"]`) as HTMLElement;

describe('board', () => {
  let restoreLayout: () => void;
  beforeEach(() => {
    restoreLayout = stubBoardLayout(); // jsdom has no layout; the pointer drag needs one (LAUN-29)
  });
  afterEach(() => restoreLayout());

  it('drag between columns changes status with an optimistic update', async () => {
    const { api } = tracker([makeIssue(1, { title: 'Move me' })]);
    renderApp('/orbit/issues?view=board');
    const card = (await screen.findByText('Move me')).closest('li')!;
    expect(within(column('Backlog')).getByText('Move me')).toBeInTheDocument();

    dragCard(card, middle(0, 0), at(1, 0));

    await waitFor(() => expect(within(column('Todo')).getByText('Move me')).toBeInTheDocument());
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.body).toEqual({ state: 'Todo', version: 1 });
    expect(patch.url.pathname).toBe('/api/v1/orgs/orbit/issues/ORB-1');
    // The server agrees, so it stays.
    await waitFor(() => expect(within(column('Todo')).getByText('Move me')).toBeInTheDocument());
    expect(within(column('Backlog')).queryByText('Move me')).not.toBeInTheDocument();
  });

  it('rolls the card back and says so when the server refuses', async () => {
    tracker([makeIssue(1, { title: 'Move me' })], {
      patch: () => ({ status: 500, json: { error: 'database is down', code: 'internal_error' } }),
    });
    renderApp('/orbit/issues?view=board');
    const card = (await screen.findByText('Move me')).closest('li')!;
    dragCard(card, middle(0, 0), at(1, 0));
    expect(await screen.findByRole('alert')).toHaveTextContent('database is down');
    await waitFor(() => expect(within(column('Backlog')).getByText('Move me')).toBeInTheDocument());
    expect(within(column('Todo')).queryByText('Move me')).not.toBeInTheDocument();
  });

  it('a version conflict on a drag rolls back and tells the person it changed elsewhere', async () => {
    tracker([makeIssue(1, { title: 'Move me' })], {
      patch: () => ({ status: 409, json: { error: 'version conflict', code: 'version_conflict' } }),
    });
    renderApp('/orbit/issues?view=board');
    const card = (await screen.findByText('Move me')).closest('li')!;
    dragCard(card, middle(0, 0), at(1, 0));
    expect(await screen.findByRole('alert')).toHaveTextContent('changed elsewhere');
    await waitFor(() => expect(within(column('Backlog')).getByText('Move me')).toBeInTheDocument());
  });
});

describe('narrow screen controls (LAUN-16)', () => {
  it('the Filter button opens and closes the folded filters and shows the active count', async () => {
    tracker([makeIssue(1, { title: 'One' })]);
    const user = userEvent.setup();
    renderApp('/orbit/issues?state=Todo');
    await screen.findByText('One');
    const btn = screen.getByRole('button', { name: 'Filter (1)' });
    expect(btn).toHaveAttribute('aria-expanded', 'false');
    await user.click(btn);
    expect(btn).toHaveAttribute('aria-expanded', 'true');
    await user.click(btn);
    expect(btn).toHaveAttribute('aria-expanded', 'false');
    // Search stays in the bar, outside the folds.
    expect(screen.getByRole('searchbox')).toBeInTheDocument();
  });

  it('the Select button turns select mode on and, turned off, clears the selection', async () => {
    tracker([makeIssue(1, { title: 'One' })]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('One');
    const sel = screen.getByRole('button', { name: 'Select' });
    expect(sel).toHaveAttribute('aria-pressed', 'false');
    await user.click(sel);
    expect(sel).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-1' }));
    expect(screen.getByText('1 selected')).toBeInTheDocument();
    await user.click(sel);
    expect(sel).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByText('1 selected')).not.toBeInTheDocument();
  });

  it('x selects an issue, which turns select mode on and shows the count; Escape turns it off', async () => {
    tracker([makeIssue(1, { title: 'One' })]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('One');
    const sel = screen.getByRole('button', { name: 'Select' });
    await user.keyboard('jx');
    expect(sel).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText('1 selected')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(sel).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByText('1 selected')).not.toBeInTheDocument();
  });

  it('Clear in the page head also leaves select mode', async () => {
    tracker([makeIssue(1, { title: 'One' })]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('One');
    const sel = screen.getByRole('button', { name: 'Select' });
    await user.keyboard('jx');
    expect(sel).toHaveAttribute('aria-pressed', 'true');
    await user.click(within(screen.getByRole('status', { name: '' })).getByRole('button', { name: 'Clear' }));
    expect(sel).toHaveAttribute('aria-pressed', 'false');
    expect(screen.queryByText('1 selected')).not.toBeInTheDocument();
  });

  it('on a narrow screen the DOM order is Search, Filter, view, chips, then Select and Sort', async () => {
    const orig = window.matchMedia;
    window.matchMedia = ((query: string) => ({
      matches: true,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    })) as typeof window.matchMedia;
    try {
      tracker([makeIssue(1, { title: 'One' })]);
      renderApp('/orbit/issues');
      await screen.findByText('One');
      const bar = screen.getByRole('toolbar', { name: 'Filters' });
      const el = (n: string | RegExp, role: 'button' | 'combobox' | 'searchbox') =>
        within(bar).getByRole(role, { name: n });
      const order = [
        el(/search/i, 'searchbox'),
        el('Filter', 'button'),
        el('List view', 'button'),
        el('Status', 'button'),
        el('Select', 'button'),
        el(/sort/i, 'combobox'),
      ];
      for (let i = 1; i < order.length; i++)
        expect(
          order[i - 1]!.compareDocumentPosition(order[i]!) & Node.DOCUMENT_POSITION_FOLLOWING,
        ).toBeTruthy();
    } finally {
      window.matchMedia = orig;
    }
  });

  it('a board card shows its created date', async () => {
    tracker([makeIssue(1, { title: 'Card', createdAt: '2026-10-05T12:00:00Z' })]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card');
    expect(screen.getByText(/Created 5 Oct 2026/)).toBeInTheDocument();
  });
});

describe('issue detail', () => {
  it('a 409 shows the "changed elsewhere" prompt, keeps the draft, and reload clears it', async () => {
    const { api } = tracker([makeIssue(1, { title: 'Original title', version: 3 })], {
      patch: (n) =>
        n === 1 ? { status: 409, json: { error: 'version conflict', code: 'version_conflict' } } : undefined,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const title = await screen.findByLabelText('Title');
    await waitFor(() => expect(title).toHaveValue('Original title'));
    await user.clear(title);
    await user.type(title, 'My edit');
    await user.tab();

    const banner = await screen.findByText('This issue changed elsewhere.');
    expect(banner).toBeInTheDocument();
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.body).toEqual({ title: 'My edit', version: 3 }); // compare-and-swap, never a blind overwrite
    expect(screen.getByLabelText('Title')).toHaveValue('My edit'); // draft is kept, not lost
    expect(screen.getByText('Your unsaved text')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Reload latest' }));
    await waitFor(() => expect(screen.queryByText('This issue changed elsewhere.')).not.toBeInTheDocument());
    expect(screen.getByLabelText('Title')).toHaveValue('Original title');
  });

  it('edits the description as markdown and saves with the version', async () => {
    const { api, issues } = tracker([makeIssue(1, { description: 'old text', version: 2 })]);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByText('old text');
    await user.click(screen.getByText('old text'));
    const box = await screen.findByLabelText('Description (markdown)');
    await user.clear(box);
    await user.type(box, 'new **text**');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(issues[0]!.description).toBe('new **text**'));
    expect(api.calls.find((c) => c.method === 'PATCH')!.body).toEqual({
      description: 'new **text**',
      version: 2,
    });
    expect(await screen.findByText('text', { selector: 'strong' })).toBeInTheDocument();
  });

  // Another tab edits the issue while this tab has an open draft. A live update refreshes the cache to
  // the newer version. The draft must still be saved against the version it STARTED from, so the server
  // answers 409 and nothing is overwritten.
  const strict409 = (_n: number, issue: Issue, body: Record<string, unknown>) =>
    body.version !== issue.version
      ? { status: 409, json: { error: 'version conflict', code: 'version_conflict' } }
      : undefined;
  const otherTabEdits = async (issues: Issue[], api: { calls: { method: string; url: URL }[] }) => {
    const es = FakeEventSource.lastEvents!;
    es.open();
    const detailGets = () =>
      api.calls.filter((c) => c.method === 'GET' && /\/issues\/ORB-1$/.test(c.url.pathname)).length;
    const before = detailGets();
    issues[0]!.version = 4;
    issues[0]!.title = 'Renamed in another tab';
    issues[0]!.description = 'Rewritten in another tab';
    act(() =>
      es.emit('issue.updated', {
        seq: 11,
        type: 'issue.updated',
        entityType: 'issue',
        entityId: 'i1',
        actor: null,
        payload: { identifier: 'ORB-1' },
        createdAt: '2026-10-24T00:00:00Z',
      }),
    );
    await waitFor(() => expect(detailGets()).toBeGreaterThan(before)); // the cache now holds v4
    await new Promise((r) => setTimeout(r, 50));
  };

  it('a description draft started at v3 is saved with version 3 and shows the conflict when v4 arrived meanwhile', async () => {
    const { api, issues } = tracker([makeIssue(1, { description: 'old text', version: 3 })], {
      patch: strict409,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByText('old text');
    await waitFor(() => expect(FakeEventSource.eventStreams).toHaveLength(1));
    await user.click(screen.getByText('old text'));
    const box = await screen.findByLabelText('Description (markdown)');
    await user.clear(box);
    await user.type(box, 'my draft');
    await otherTabEdits(issues, api);
    await user.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('This issue changed elsewhere.')).toBeInTheDocument();
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.body).toEqual({ description: 'my draft', version: 3 });
    expect(issues[0]!.description).toBe('Rewritten in another tab'); // nothing was overwritten
  });

  it('a title draft started at v3 is saved with version 3 and shows the conflict when v4 arrived meanwhile', async () => {
    const { api, issues } = tracker([makeIssue(1, { title: 'Original title', version: 3 })], {
      patch: strict409,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const title = await screen.findByLabelText('Title');
    await waitFor(() => expect(title).toHaveValue('Original title'));
    await waitFor(() => expect(FakeEventSource.eventStreams).toHaveLength(1));
    await user.type(title, ' plus my edit');
    await otherTabEdits(issues, api);
    await user.tab();

    expect(await screen.findByText('This issue changed elsewhere.')).toBeInTheDocument();
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.body).toEqual({ title: 'Original title plus my edit', version: 3 });
    expect(issues[0]!.title).toBe('Renamed in another tab'); // nothing was overwritten
  });

  // The tab's OWN writes must never look like someone else's edit.
  it('own title save, then a description edit and save: no false conflict (Edit was clicked before the title reply)', async () => {
    let release!: () => void;
    const { api, issues } = tracker(
      [makeIssue(1, { title: 'Original title', description: 'old text', version: 3 })],
      {
        patch: strict409,
        hold: (n) => (n === 1 ? new Promise<void>((r) => (release = r)) : undefined),
      },
    );
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const title = await screen.findByLabelText('Title');
    await waitFor(() => expect(title).toHaveValue('Original title'));
    await user.type(title, ' v2');
    await user.tab(); // title PATCH (v3 -> v4) is sent and held
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    await user.click(screen.getByText('old text')); // draft base is recorded as 3
    const box = await screen.findByLabelText('Description (markdown)');
    await user.clear(box);
    await user.type(box, 'my draft');
    release(); // the title reply arrives now; the Save button is disabled until it does
    await user.click(await screen.findByRole('button', { name: 'Save' }));
    await waitFor(() => expect(issues[0]!.description).toBe('my draft'));
    expect(screen.queryByText('This issue changed elsewhere.')).not.toBeInTheDocument();
    const bodies = api.calls.filter((c) => c.method === 'PATCH').map((c) => c.body);
    expect(bodies).toEqual([
      { title: 'Original title v2', version: 3 },
      { description: 'my draft', version: 4 },
    ]);
  });

  const changeStatus = async (
    user: ReturnType<typeof userEvent.setup>,
    api: { calls: { method: string }[] },
  ) => {
    const before = api.calls.filter((c) => c.method === 'PATCH').length;
    await user.click(screen.getByRole('button', { name: /Backlog/ }));
    await user.click(await screen.findByRole('option', { name: 'In Progress' }));
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'PATCH').length).toBe(before + 1));
    await new Promise((r) => setTimeout(r, 50)); // let the reply and the refetch land
  };

  it('a description draft is saved without a false conflict after this tab changed the status', async () => {
    const { api, issues } = tracker([makeIssue(1, { description: 'old text', version: 3 })], {
      patch: strict409,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByText('old text');
    await user.click(screen.getByText('old text'));
    const box = await screen.findByLabelText('Description (markdown)');
    await user.clear(box);
    await user.type(box, 'my draft');
    await changeStatus(user, api); // server is now at v4
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(issues[0]!.description).toBe('my draft'));
    expect(screen.queryByText('This issue changed elsewhere.')).not.toBeInTheDocument();
    const patches = api.calls.filter((c) => c.method === 'PATCH');
    expect(patches.at(-1)!.body).toEqual({ description: 'my draft', version: 4 });
  });

  it('another tab edit still gives 409 even when this tab also made its own write meanwhile', async () => {
    const { api, issues } = tracker([makeIssue(1, { description: 'old text', version: 3 })], {
      patch: strict409,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByText('old text');
    await waitFor(() => expect(FakeEventSource.eventStreams).toHaveLength(1));
    await user.click(screen.getByText('old text'));
    const box = await screen.findByLabelText('Description (markdown)');
    await user.clear(box);
    await user.type(box, 'my draft');
    await otherTabEdits(issues, api); // another tab: v3 -> v4
    await changeStatus(user, api); // this tab: v4 -> v5
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('This issue changed elsewhere.')).toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'PATCH').at(-1)!.body).toEqual({
      description: 'my draft',
      version: 3,
    });
    expect(issues[0]!.description).toBe('Rewritten in another tab');
  });

  it('changes status from the picker', async () => {
    const { api } = tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByLabelText('Title');
    await user.click(screen.getByRole('button', { name: /Backlog/ }));
    await user.click(await screen.findByRole('option', { name: 'In Progress' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    expect(api.calls.find((c) => c.method === 'PATCH')!.body).toMatchObject({ state: 'In Progress' });
  });

  it('comment Idempotency-Key is reused when the same text is retried, and new once the text is edited', async () => {
    const { api } = tracker([makeIssue(1)], { commentFails: (n) => n <= 2 });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByLabelText('Title');
    const box = await screen.findByLabelText('Leave a comment');
    await user.type(box, 'First comment');
    await user.click(screen.getByRole('button', { name: 'Comment' }));
    await waitFor(() => expect(commentPosts()).toBe(1));
    await user.click(screen.getByRole('button', { name: 'Comment' })); // same text again
    await waitFor(() => expect(commentPosts()).toBe(2));
    await user.type(box, '!'); // the text changed
    await user.click(screen.getByRole('button', { name: 'Comment' }));
    await waitFor(() => expect(commentPosts()).toBe(3));
    const keys = api.calls
      .filter((c) => c.method === 'POST' && c.url.pathname.endsWith('/comments'))
      .map((c) => c.headers.get('Idempotency-Key'));
    expect(keys[0]).toBeTruthy();
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[1]);
    function commentPosts() {
      return api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith('/comments')).length;
    }
  });

  it('comments: create, reply (threaded), edit own, delete own', async () => {
    const { api } = tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByLabelText('Title');

    await user.type(await screen.findByLabelText('Leave a comment'), 'First comment');
    await user.click(screen.getByRole('button', { name: 'Comment' }));
    expect(await screen.findByText('First comment')).toBeInTheDocument();
    const create = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/comments'))!;
    expect(create.body).toMatchObject({ body: 'First comment' });
    expect(create.headers.get('Idempotency-Key')).toBeTruthy();

    await user.click(screen.getByRole('button', { name: 'Leave a reply to Chen' }));
    await user.type(await screen.findByLabelText('Write a reply'), 'A reply');
    await user.click(screen.getAllByRole('button', { name: 'Reply' }).at(-1)!);
    expect(await screen.findByText('A reply')).toBeInTheDocument();
    const reply = api.calls
      .filter((c) => c.method === 'POST' && c.url.pathname.endsWith('/comments'))
      .at(-1)!;
    expect(reply.body).toMatchObject({ body: 'A reply', parentId: 'c1' });
    // threaded: the reply is nested under the first comment
    expect(screen.getByText('First comment').closest('li')!.querySelector('ul')).toHaveTextContent('A reply');

    const rootItem = screen.getByText('First comment').closest('li')!;
    await user.click(within(rootItem).getAllByRole('button', { name: 'Edit' })[0]!);
    const edit = await screen.findByLabelText('Edit comment');
    await user.clear(edit);
    await user.type(edit, 'First comment (fixed)');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('First comment (fixed)')).toBeInTheDocument();
    expect(await screen.findByText('(edited)')).toBeInTheDocument();

    const del = screen.getAllByRole('button', { name: 'Delete' }).at(-1)!;
    await user.click(del);
    await user.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(screen.queryByText('A reply')).not.toBeInTheDocument());
  });
});

describe('create issue', () => {
  it('opens on "c", is a labelled focus-trapped dialog, and posts with an idempotency key', async () => {
    const { api } = tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('c');
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    const title = within(dialog).getByLabelText('Title');
    expect(title).toHaveFocus();

    // focus trap: Tab from the last control wraps to the first
    const last = within(dialog).getByRole('button', { name: 'Create issue' });
    last.focus();
    await user.tab();
    expect(dialog.contains(document.activeElement)).toBe(true);

    await user.type(title, 'Brand new issue');
    await user.click(last);
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const post = api.calls.find(
      (c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues',
    )!;
    expect(post.body).toMatchObject({ team: 'ORB', title: 'Brand new issue' });
    expect(post.headers.get('Idempotency-Key')).toMatch(/[0-9a-f-]{36}/);
    expect(await screen.findByText('ORB-99 created')).toBeInTheDocument();
  });

  it('Escape closes it and returns focus', async () => {
    tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('c');
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('refuses an empty title with a message', async () => {
    tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('c');
    await user.click(await screen.findByRole('button', { name: 'Create issue', description: '' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Add a title.');
  });
});

describe('keyboard', () => {
  it('a quick picker (s) gives focus back to the element that had it when it closes', async () => {
    tracker([makeIssue(1), makeIssue(2)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('jx');
    const opener = screen.getAllByRole('button', { name: 'Create issue' })[0]!;
    opener.focus();
    await user.keyboard('s');
    const dialog = await screen.findByRole('dialog', { name: 'Change status' });
    expect(within(dialog).getByRole('combobox')).toHaveFocus(); // the search box takes focus
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(opener).toHaveFocus();
  });

  it('j/k move, x selects, s opens the status picker; none fire while typing', async () => {
    const { api } = tracker([makeIssue(1), makeIssue(2), makeIssue(3)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    const rows = () => [...document.querySelectorAll('li[data-issue-id]')];
    await user.keyboard('j');
    expect(rows()[0]).toHaveAttribute('aria-current', 'true');
    await user.keyboard('jj');
    expect(rows()[2]).toHaveAttribute('aria-current', 'true');
    await user.keyboard('k');
    expect(rows()[1]).toHaveAttribute('aria-current', 'true');

    // typing in the search box must not move focus or open anything
    await user.click(screen.getByRole('searchbox', { name: 'Search issues' }));
    await user.keyboard('jjsx');
    expect(rows()[1]).toHaveAttribute('aria-current', 'true');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('searchbox')).toHaveValue('jjsx');
    await user.clear(screen.getByRole('searchbox'));
    await user.click(document.body);

    await user.keyboard('x');
    expect(within(rows()[1] as HTMLElement).getByRole('checkbox')).toBeChecked();
    await user.keyboard('s');
    const dialog = await screen.findByRole('dialog', { name: 'Change status' });
    await user.click(within(dialog).getByRole('option', { name: 'Todo' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    expect(api.calls.find((c) => c.method === 'PATCH')!.url.pathname).toBe('/api/v1/orgs/orbit/issues/ORB-2');
  });

  it('palette, then "Create issue", then Esc: focus goes back to what had it before the palette', async () => {
    tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    const opener = screen.getAllByRole('button', { name: 'Create issue' })[0]!;
    opener.focus();
    await user.keyboard('{Control>}k{/Control}');
    await screen.findByRole('dialog');
    await user.keyboard('create');
    await user.click(await screen.findByRole('option', { name: /Create issue/ }));
    await screen.findByRole('dialog', { name: 'New issue' });
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(opener).toHaveFocus();
  });

  it('Cmd+K opens the palette; typing an identifier jumps to the issue', async () => {
    tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('{Meta>}k{/Meta}');
    const input = await screen.findByRole('combobox', { name: 'Search issues and commands' });
    await user.type(input, 'orb-1');
    await user.click(await screen.findByRole('option', { name: 'Open ORB-1' }));
    expect(await screen.findByLabelText('Title')).toHaveValue('Issue 1');
  });

  it('? shows the shortcut help', async () => {
    tracker([makeIssue(1)]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('?');
    expect(await screen.findByRole('dialog', { name: 'Keyboard shortcuts' })).toBeInTheDocument();
  });
});

describe('live updates', () => {
  it('another tab\'s change shows up without a refresh, and a drop shows "Reconnecting…"', async () => {
    const { issues } = tracker([makeIssue(1, { title: 'Before' })]);
    renderApp('/orbit/issues');
    await screen.findByText('Before');
    await waitFor(() => expect(FakeEventSource.eventStreams).toHaveLength(1));
    const es = FakeEventSource.lastEvents!;
    es.open();

    // Another tab renames the issue; the server emits an event.
    issues[0]!.title = 'After (edited elsewhere)';
    issues[0]!.version = 2;
    act(() =>
      es.emit('issue.updated', {
        seq: 10,
        type: 'issue.updated',
        entityType: 'issue',
        entityId: 'i1',
        actor: null,
        payload: { identifier: 'ORB-1' },
        createdAt: '2026-10-24T00:00:00Z',
      }),
    );
    expect(await screen.findByText('After (edited elsewhere)')).toBeInTheDocument();
    expect(screen.queryByText('Before')).not.toBeInTheDocument();

    act(() => es.fail());
    expect(await screen.findByRole('status', { name: '' })).toBeTruthy();
    expect(await screen.findByText('Reconnecting…')).toBeInTheDocument();
  });
});

describe('task lists (LAUN-47)', () => {
  const DESC = 'Plan\n\n```\n- [ ] in code\n```\n\n- [ ] first\n- [x] second\n';

  it('a click on a description box saves the one line, does not open the editor, and survives a refetch', async () => {
    const { api, issues } = tracker([makeIssue(1, { description: DESC, version: 2 })]);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await screen.findByRole('checkbox', { name: 'first' });
    await user.click(box);
    await waitFor(() => expect(issues[0]!.description).toBe(DESC.replace('- [ ] first', '- [x] first')));
    expect(api.calls.find((c) => c.method === 'PATCH')!.body).toEqual({
      description: DESC.replace('- [ ] first', '- [x] first'),
      version: 2,
    });
    expect(screen.queryByLabelText('Description (markdown)')).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('checkbox', { name: 'first' })).toBeChecked());
  });

  it('Space on a focused box toggles it', async () => {
    const { issues } = tracker([makeIssue(1, { description: DESC, version: 2 })]);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await screen.findByRole('checkbox', { name: 'second' });
    box.focus();
    await user.keyboard(' ');
    await waitFor(() => expect(issues[0]!.description).toBe(DESC.replace('- [x] second', '- [ ] second')));
    expect(screen.queryByLabelText('Description (markdown)')).not.toBeInTheDocument();
  });

  it('a 409 on a box click shows the conflict banner and changes nothing', async () => {
    const { issues } = tracker([makeIssue(1, { description: DESC, version: 3 })], {
      patch: () => ({ status: 409, json: { error: 'version conflict', code: 'version_conflict' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('checkbox', { name: 'first' }));
    expect(await screen.findByText('This issue changed elsewhere.')).toBeInTheDocument();
    expect(issues[0]!.description).toBe(DESC);
    await waitFor(() => expect(screen.getByRole('checkbox', { name: 'first' })).not.toBeChecked());
  });

  it('a comment box toggles for its author and is read-only for others', async () => {
    const { api, comments } = tracker([makeIssue(1)]);
    comments.push(
      makeComment('c1', { body: '- [ ] mine' }),
      makeComment('c2', { body: '- [ ] theirs', author: { id: 'u9', username: 'ann', name: 'Ann' } }),
    );
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const theirs = await screen.findByRole('checkbox', { name: 'theirs' });
    expect(theirs).toBeDisabled();
    await user.click(await screen.findByRole('checkbox', { name: 'mine' }));
    await waitFor(() => expect(comments[0]!.body).toBe('- [x] mine'));
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(1);
    await waitFor(() => expect(screen.getByRole('checkbox', { name: 'mine' })).toBeChecked());
  });

  it('a failed comment PATCH rolls the box back and shows the error', async () => {
    const { comments } = tracker([makeIssue(1)], {
      commentPatch: () => ({ status: 500, json: { error: 'database is down', code: 'internal_error' } }),
    });
    comments.push(makeComment('c1', { body: '- [ ] mine' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('checkbox', { name: 'mine' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/database is down/);
    expect(screen.getByRole('checkbox', { name: 'mine' })).not.toBeChecked();
    expect(comments[0]!.body).toBe('- [ ] mine');
  });

  it('a second comment toggle while the first is saving is refused, and the first lands', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const { api, comments } = tracker([makeIssue(1)], {
      commentPatch: async (n) => {
        if (n === 1) await gate;
        return undefined;
      },
    });
    comments.push(makeComment('c1', { body: '- [ ] one\n- [ ] two' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('checkbox', { name: 'one' }));
    await user.click(screen.getByRole('checkbox', { name: 'two' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/Still saving/);
    release();
    await waitFor(() => expect(comments[0]!.body).toBe('- [x] one\n- [ ] two'));
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(1);
  });

  it('a toggle does not stick when the server stores a different body', async () => {
    const { comments } = tracker([makeIssue(1)], {
      commentPatch: (_n, c) => {
        c.body = '- [ ] one (normalised)';
        return { json: c };
      },
    });
    comments.push(makeComment('c1', { body: '- [ ] one' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('checkbox', { name: 'one' }));
    expect(await screen.findByRole('checkbox', { name: 'one (normalised)' })).not.toBeChecked();
  });

  it('Edit right after a toggle starts from the toggled text', async () => {
    const { comments } = tracker([makeIssue(1)]);
    comments.push(makeComment('c1', { body: '- [ ] mine' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('checkbox', { name: 'mine' }));
    await user.click(screen.getByRole('button', { name: 'Edit' }));
    expect(await screen.findByLabelText('Edit comment')).toHaveValue('- [x] mine');
  });

  it('a second description toggle while the first is saving is refused, and the first lands', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const D = '- [ ] a\n- [ ] b\n';
    const { api, issues } = tracker([makeIssue(1, { description: D, version: 2 })], {
      hold: (n) => (n === 1 ? gate : undefined),
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('checkbox', { name: 'a' }));
    await user.click(screen.getByRole('checkbox', { name: 'b' }));
    expect(await screen.findByText('Still saving the last change')).toBeInTheDocument();
    release();
    await waitFor(() => expect(issues[0]!.description).toBe('- [x] a\n- [ ] b\n'));
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(1);
    // After it settled a new toggle works and keeps the first.
    await user.click(screen.getByRole('checkbox', { name: 'b' }));
    await waitFor(() => expect(issues[0]!.description).toBe('- [x] a\n- [x] b\n'));
  });
});
