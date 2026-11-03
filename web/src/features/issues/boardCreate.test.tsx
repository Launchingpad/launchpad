import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Issue } from '../../api/types';
import { stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states, team } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { resetDragged } from './cardLink';
import { BoardDrag } from './boardDrag';

// LAUN-173: the "+" at the foot of a lane opens New issue with that lane's state.

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const card = (n: number, state: string) => makeIssue(n, { state: ref(state), title: `Card ${n}` });
const lane = (name: string) => document.querySelector(`[data-column="${name}"]`) as HTMLElement;
const plus = (name: string) => within(lane(name)).queryByRole('button', { name: `Create issue in ${name}` });

let restore: () => void;
function serve(issues: Issue[], teamStates?: (team: string) => typeof states) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/teams': () => ({
      json: { items: [team, { ...team, id: 'team2', key: 'WEB', name: 'Web' }], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: issues, nextCursor: null } }),
    ...(teamStates
      ? {
          'GET /api/v1/orgs/:org/teams/:team/states': (r: { params: Record<string, string> }) => ({
            json: { items: teamStates(r.params.team!), nextCursor: null },
          }),
        }
      : {}),
    'POST /api/v1/orgs/:org/issues': (r) => ({
      status: 201,
      json: makeIssue(90, { title: (r.body as { title: string }).title, identifier: 'ORB-90' }),
    }),
  });
}
beforeEach(() => {
  localStorage.clear();
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubBoardLayout();
  resetDragged();
});
afterEach(() => {
  restore();
  vi.restoreAllMocks();
});

describe('create in a lane (LAUN-173)', () => {
  it('has a named button in every lane, reachable by keyboard', async () => {
    serve([card(1, 'Todo')]);
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    for (const n of ['Backlog', 'Todo', 'In Progress', 'Done']) expect(plus(n)).toBeInTheDocument();
    const b = plus('Todo')!;
    b.focus();
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
  });

  it('opens New issue with the lane state, and posts it', async () => {
    const api = serve([card(1, 'Todo')]);
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB?view=board');
    await screen.findByText('Card 1');
    await user.click(plus('In Progress')!);
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    expect(within(dialog).getByRole('button', { name: /In Progress/ })).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Title'), 'From a lane');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const post = api.calls.find(
      (c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues',
    )!;
    expect(post.body).toMatchObject({ title: 'From a lane', state: 'In Progress', team: 'ORB' });
  });

  it('presets the team of the page it is on, not the first team', async () => {
    const api = serve([card(1, 'Todo')]);
    const user = userEvent.setup();
    renderApp('/orbit/team/WEB?view=board');
    await screen.findByText('Card 1');
    await user.click(plus('Done')!);
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(dialog).getByLabelText('Title'), 'Web one');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const post = api.calls.find(
      (c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues',
    )!;
    expect(post.body).toMatchObject({ team: 'WEB', state: 'Done' });
  });

  it('sends no state when the dialog team has no state of that name', async () => {
    const api = serve([card(1, 'Todo')], (t) =>
      t.toUpperCase() === 'WEB' ? states.filter((x) => x.name !== 'Done') : states,
    );
    const user = userEvent.setup();
    renderApp('/orbit/team/WEB?view=board');
    await screen.findByText('Card 1');
    await user.click(plus('Done')!);
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await waitFor(() =>
      expect(within(dialog).getByRole('button', { name: /Todo|Backlog/ })).toBeInTheDocument(),
    );
    await user.type(within(dialog).getByLabelText('Title'), 'No Done here');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const post = api.calls.find(
      (c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues',
    )!;
    expect(post.body).not.toHaveProperty('state');
  });

  it('has no button in a hidden lane', async () => {
    serve([card(1, 'Todo')]);
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    await user.click(screen.getByRole('button', { name: /^Columns/ }));
    const panel = screen.getByRole('group', { name: 'Board columns' });
    await user.click(within(panel).getByRole('checkbox', { name: /^Show Backlog,/ }));
    expect(lane('Backlog')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Create issue in Backlog' })).toBeNull();
    expect(plus('Todo')).toBeInTheDocument();
  });

  it('is not on the list view', async () => {
    serve([card(1, 'Todo')]);
    renderApp('/orbit/issues?view=list');
    await screen.findByText('Card 1');
    expect(screen.queryByRole('button', { name: /^Create issue in / })).toBeNull();
  });
});

describe('board re-measure (LAUN-169)', () => {
  it('does not re-measure the columns when IssueBrowser re-renders with the same data', async () => {
    serve([card(1, 'Todo'), card(2, 'Todo')]);
    const spy = vi.spyOn(BoardDrag.prototype, 'columnsChanged');
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    await new Promise((r) => setTimeout(r, 300)); // let the teams and states settle
    const before = spy.mock.calls.length;
    // `j` moves the focused card: IssueBrowser re-renders each time (focusedId) with no data change.
    for (let i = 0; i < 4; i++) await user.keyboard('j');
    expect(spy.mock.calls.length).toBe(before);
  });
});
