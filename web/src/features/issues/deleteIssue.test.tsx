import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient } from '@tanstack/react-query';
import { beforeEach, describe, expect, it } from 'vitest';
import { baseRoutes, makeIssue, mockApi } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { invalidateForEvent } from '../../api/events';
import { keys } from '../../api/keys';

const emptyRel = { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] };

function setup(opts: { role?: 'admin' | 'member'; del?: () => { status: number; json?: unknown } } = {}) {
  let gone = false;
  const base = baseRoutes();
  const api = mockApi({
    ...base,
    'GET /api/v1/auth/session': (r) => {
      const res = base['GET /api/v1/auth/session']!(r) as { json: { orgs: { role: string }[] } };
      return {
        json: { ...res.json, orgs: res.json.orgs.map((o) => ({ ...o, role: opts.role ?? 'admin' })) },
      };
    },
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue': () =>
      gone
        ? { status: 404, json: { error: 'not found', code: 'not_found' } }
        : {
            json: {
              ...makeIssue(1, { description: 'Body' }),
              relations: emptyRel,
              stateHistory: [],
              attachments: [],
            },
          },
    'GET /api/v1/orgs/:org/issues/:issue/comments': () => ({ json: { items: [], nextCursor: null } }),
    'DELETE /api/v1/orgs/:org/issues/:issue': () => {
      const r = opts.del ? opts.del() : { status: 204 };
      if (r.status === 204) gone = true;
      return r;
    },
  });
  return { api, markGone: () => (gone = true) };
}

const deletes = (api: ReturnType<typeof setup>['api']) => api.calls.filter((c) => c.method === 'DELETE');

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe('delete an issue (LAUN-201)', () => {
  it('asks first, names the identifier and the consequence, then deletes and goes to the team list', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Delete issue' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Delete ORB-1?')).toBeInTheDocument();
    expect(within(dialog).getByText(/sub-issues are not deleted/)).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    expect(deletes(api)).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Delete issue' }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
    expect(deletes(api)).toHaveLength(1);
    expect(deletes(api)[0]!.url.pathname).toContain('/issues/ORB-1');
  });

  it('two activations in one tick send one DELETE, and a 404 counts as gone', async () => {
    let n = 0;
    const { api } = setup({
      del: () => (++n === 1 ? { status: 204 } : { status: 404, json: { error: 'x', code: 'not_found' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Delete issue' }));
    const btn = within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete issue' });
    fireEvent.click(btn);
    fireEvent.click(btn);
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
    expect(deletes(api)).toHaveLength(1);
  });

  it('a 404 from the DELETE closes and goes to the list, with no error', async () => {
    setup({ del: () => ({ status: 404, json: { error: 'x', code: 'not_found' } }) });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Delete issue' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete issue' }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('Cancel and Escape send no request', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Delete issue' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Delete issue' }));
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(deletes(api)).toHaveLength(0);
  });

  it('shows the failure, keeps the dialog and the page', async () => {
    setup({ del: () => ({ status: 403, json: { error: 'RAW', code: 'insufficient_scope' } }) });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Delete issue' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete issue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('You need admin access to delete an issue.');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(currentLocation.pathname).toBe('/orbit/issue/ORB-1');
  });

  it('is hidden from a member', async () => {
    setup({ role: 'member' });
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('button', { name: 'Copy link' });
    expect(screen.queryByRole('button', { name: 'Delete issue' })).not.toBeInTheDocument();
  });

  it('an open page shows "This issue was deleted" after the issue.deleted event, not an error loop', async () => {
    const { api, markGone } = setup();
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: Infinity, staleTime: 0 } },
    });
    renderApp('/orbit/issue/ORB-1', qc);
    await screen.findByText('Body');
    markGone();
    invalidateForEvent(qc, 'orbit', { type: 'issue.deleted' });
    expect(await screen.findByText('This issue was deleted')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Back to/ })).toBeInTheDocument();
    const reads = api.calls.filter(
      (c) => c.method === 'GET' && c.url.pathname.endsWith('/issues/ORB-1'),
    ).length;
    await new Promise((r) => setTimeout(r, 50));
    expect(
      api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues/ORB-1')).length,
    ).toBe(reads);
  });

  it('issue.deleted refreshes the lists and the open details', () => {
    const qc = new QueryClient();
    const ks = [
      keys.issues('orbit'),
      keys.issueDetails('orbit'),
      keys.projectsAll('orbit'),
      keys.inboxAll('orbit'),
      keys.unread('orbit'),
    ];
    for (const k of ks) qc.setQueryData(k, {});
    invalidateForEvent(qc, 'orbit', { type: 'issue.deleted' });
    for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, k.join('/')).toBe(true);
  });
});
