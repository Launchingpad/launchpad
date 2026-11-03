import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import { baseRoutes, makeComment, makeIssue, mockApi, team } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { reasonText } from './SubscribeControl';

type Sub = { state: 'subscribed' | 'muted' | 'default'; reasons: string[] };

const emptyRel = { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] };

/** A stateful fake of the subscription routes: the implicit reasons are fixed, the row is what PUT / DELETE keep. */
function setup(
  initial: string[],
  start: Sub['state'] = 'default',
  opts: { getStatus?: number; putStatus?: number } = {},
) {
  let implicit = initial;
  let row: 'subscribed' | 'muted' | null = start === 'default' ? null : start;
  const view = (): Sub => ({
    state: row ?? 'default',
    reasons: [...implicit, ...(row === 'subscribed' ? ['manual'] : [])],
  });
  const issue = makeIssue(1, {});
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/teams': () => ({ json: { items: [team], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue': () => ({
      json: { ...issue, relations: emptyRel, stateHistory: [], attachments: [] },
    }),
    'GET /api/v1/orgs/:org/issues/:issue/comments': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/projects': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue/subscription': () =>
      opts.getStatus
        ? { status: opts.getStatus, json: { error: 'no', code: 'forbidden' } }
        : { json: view() },
    'POST /api/v1/orgs/:org/issues/:issue/comments': () => {
      implicit = [...new Set([...implicit, 'commenter'])]; // the server reads commenters live
      return { status: 201, json: makeComment('c1', {}) };
    },
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => {
      const a = (r.body as { assignee?: string | null }).assignee;
      implicit = a ? [...implicit, 'assignee'] : implicit.filter((x) => x !== 'assignee');
      return {
        json: {
          ...issue,
          assignee: null,
          version: 2,
          relations: emptyRel,
          stateHistory: [],
          attachments: [],
        },
      };
    },
    'PUT /api/v1/orgs/:org/issues/:issue/subscription': (r) => {
      if (opts.putStatus) return { status: opts.putStatus, json: { error: 'no', code: 'forbidden' } };
      row = (r.body as { subscribed: boolean }).subscribed ? 'subscribed' : 'muted';
      return { json: view() };
    },
    'DELETE /api/v1/orgs/:org/issues/:issue/subscription': () => {
      row = null;
      return { status: 204 };
    },
  });
}

const activity = () => screen.getByRole('heading', { name: 'Activity' }).closest('section')!;

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe('Subscribe control (LAUN-38)', () => {
  it('words every state and reason', () => {
    expect(reasonText({ state: 'default', reasons: [] })).toBe('You are not subscribed.');
    expect(reasonText({ state: 'muted', reasons: ['assignee'] })).toMatch(/^You muted this issue/);
    expect(reasonText({ state: 'subscribed', reasons: ['assignee', 'commenter', 'manual'] })).toBe(
      'You are subscribed because you are the assignee, you commented, you subscribed.',
    );
    expect(reasonText({ state: 'default', reasons: ['creator'] })).toContain('you created it');
    expect(reasonText({ state: 'subscribed', reasons: ['mentioned'] })).toContain('you were mentioned');
  });

  it('shows the reason and Unsubscribe for an implicit subscriber; a mute sends false and survives a reload', async () => {
    const api = setup(['assignee']);
    const { unmount } = renderApp('/orbit/issue/ORB-1');
    const btn = await within(
      await screen.findByRole('heading', { name: 'Activity' }).then(activity),
    ).findByRole('button', {
      name: 'Unsubscribe',
    });
    expect(screen.getByText('You are subscribed because you are the assignee.')).toBeInTheDocument();
    await userEvent.click(btn);
    expect(await screen.findByRole('button', { name: 'Subscribe' })).toBeInTheDocument();
    expect(screen.getByText(/You muted this issue/)).toBeInTheDocument();
    const put = api.calls.find((c) => c.method === 'PUT' && c.url.pathname.endsWith('/subscription'))!;
    expect(put.body).toEqual({ subscribed: false });
    unmount();
    renderApp('/orbit/issue/ORB-1'); // "reload": a fresh cache reads the server's state
    expect(await screen.findByRole('button', { name: 'Subscribe' })).toBeInTheDocument();
    expect(screen.getByText(/You muted this issue/)).toBeInTheDocument();
  });

  it('subscribes from the keyboard and shows "you subscribed"', async () => {
    const api = setup([]);
    renderApp('/orbit/issue/ORB-1');
    const btn = await screen.findByRole('button', { name: 'Subscribe' });
    expect(screen.getByText('You are not subscribed.')).toBeInTheDocument();
    await userEvent.tab(); // the user reaches the button by keyboard alone
    btn.focus();
    expect(btn).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('button', { name: 'Unsubscribe' })).toBeInTheDocument();
    expect(screen.getByText('You are subscribed because you subscribed.')).toBeInTheDocument();
    expect(
      api.calls.some((c) => c.method === 'PUT' && (c.body as { subscribed: boolean }).subscribed === true),
    ).toBe(true);
  });

  it('removes the row (DELETE) when only a manual subscription remains', async () => {
    const api = setup([], 'subscribed');
    renderApp('/orbit/issue/ORB-1');
    await userEvent.click(await screen.findByRole('button', { name: 'Unsubscribe' }));
    expect(await screen.findByRole('button', { name: 'Subscribe' })).toBeInTheDocument();
    expect(api.calls.some((c) => c.method === 'DELETE')).toBe(true);
  });

  it('shows no control when the server refuses the read (an agent or read-only token)', async () => {
    setup(['assignee'], 'default', { getStatus: 403 });
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('heading', { name: 'Activity' });
    await waitFor(() => expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument());
    expect(screen.queryByText(/subscribed/i)).not.toBeInTheDocument();
  });

  it('says so, in fixed words, when a write is refused, and keeps the button', async () => {
    setup(['assignee'], 'default', { putStatus: 403 });
    renderApp('/orbit/issue/ORB-1');
    await userEvent.click(await screen.findByRole('button', { name: 'Unsubscribe' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('You cannot change this subscription.');
    expect(screen.getByRole('button', { name: 'Unsubscribe' })).toBeEnabled();
  });

  it('is a labelled, named button inside the Activity section and a status line', async () => {
    setup(['creator']);
    renderApp('/orbit/issue/ORB-1');
    const btn = await screen.findByRole('button', { name: 'Unsubscribe' });
    expect(activity().contains(btn)).toBe(true);
    expect(btn).toHaveAttribute('type', 'button');
    expect(within(activity()).getByRole('status')).toHaveTextContent('you created it');
  });

  it('Space also presses it, and the button keeps focus while the write runs', async () => {
    setup([]);
    renderApp('/orbit/issue/ORB-1');
    const btn = await screen.findByRole('button', { name: 'Subscribe' });
    btn.focus();
    await userEvent.keyboard(' ');
    expect(btn).toHaveFocus();
    expect(await screen.findByRole('button', { name: 'Unsubscribe' })).toBeInTheDocument();
  });

  it('shows the fixed words for each failed write', async () => {
    for (const [status, text] of [
      [404, 'This issue no longer exists. Reload the page.'],
      [500, 'Could not save. Try again.'],
    ] as const) {
      setup(['assignee'], 'default', { putStatus: status });
      const { unmount } = renderApp('/orbit/issue/ORB-1');
      await userEvent.click(await screen.findByRole('button', { name: 'Unsubscribe' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      unmount();
    }
  });

  it('a comment makes you a commenter: the reason text changes without a reload', async () => {
    setup([]);
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByText('You are not subscribed.')).toBeInTheDocument();
    await userEvent.type(await screen.findByLabelText('Leave a comment'), 'Hello');
    await userEvent.click(screen.getByRole('button', { name: 'Comment' }));
    expect(await screen.findByText('You are subscribed because you commented.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Unsubscribe' })).toBeInTheDocument();
  });

  it('assigning yourself changes the reason; clearing the assignee changes it back', async () => {
    setup([]);
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByText('You are not subscribed.')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Assignee: Assign' }));
    await userEvent.click(await screen.findByRole('option', { name: /Chen/ }));
    expect(await screen.findByText('You are subscribed because you are the assignee.')).toBeInTheDocument();
  });
});
