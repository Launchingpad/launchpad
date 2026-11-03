import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { GitWorkflow, GitWorkflowRule, Team, WorkflowState } from '../../../api/types';
import { baseRoutes, mockApi, team as baseTeam, type Handler } from '../../../test/mockApi';
import { renderApp } from '../../../test/render';
import { previewLine } from './GitWorkflowPage';

// LAUN-244: Settings, Git workflow. The requests are the contract: PUT {rules: [{event, stateId, fromStateIds}]}.

const page = <T,>(items: T[]) => ({ items, nextCursor: null });
const team: Team = { ...baseTeam, reviewStateId: 'sr', testingStateId: 'st' };
const st = (id: string, name: string, type: WorkflowState['type'], position: number): WorkflowState => ({
  id,
  name,
  type,
  color: '#bec2c8',
  description: '',
  position,
  teamId: team.id,
});
const states: WorkflowState[] = [
  st('sb', 'Backlog', 'backlog', 0),
  st('sp', 'In Progress', 'started', 1),
  st('sr', 'In Review', 'started', 2),
  st('st', 'In Testing', 'started', 3),
  st('sd', 'Done', 'completed', 4),
  st('sc', 'Canceled', 'canceled', 5),
];

const builtin = (): GitWorkflowRule[] => [
  { event: 'opened', custom: false, stateId: 'sr', fromStateIds: [], problem: null },
  { event: 'ready', custom: false, stateId: 'sr', fromStateIds: [], problem: null },
  { event: 'reopened', custom: false, stateId: 'sr', fromStateIds: [], problem: null },
  { event: 'merged', custom: false, stateId: 'st', fromStateIds: [], problem: null },
  { event: 'closed', custom: false, stateId: null, fromStateIds: [], problem: null },
];

function server(rules: GitWorkflowRule[] = builtin(), routes: Record<string, Handler> = {}) {
  const cur: GitWorkflow = { teamId: team.id, rules };
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
    'GET /api/v1/orgs/:org/teams': () => ({ json: page([team]) }),
    'GET /api/v1/orgs/:org/teams/:team/states': () => ({ json: page(states) }),
    'GET /api/v1/orgs/:org/teams/:team/git-workflow': () => ({ json: cur }),
    'PUT /api/v1/orgs/:org/teams/:team/git-workflow': (r) => {
      const body = r.body as { rules: { event: string; stateId: string | null; fromStateIds: string[] }[] };
      cur.rules = builtin().map((b) => {
        const c = body.rules.find((x) => x.event === b.event);
        return c ? { ...b, custom: true, stateId: c.stateId, fromStateIds: c.fromStateIds } : b;
      });
      return { json: cur };
    },
    ...routes,
  });
}

const picker = (name: string) => screen.findByRole('combobox', { name });

describe('Git workflow page (LAUN-244)', () => {
  it('shows the built-in move of each event and a preview line', async () => {
    server();
    renderApp('/orbit/settings/git-workflow');
    expect(await screen.findByRole('heading', { name: 'Git workflow' })).toBeInTheDocument();
    expect(await screen.findByRole('heading', { name: 'Orbit (ORB)' })).toBeInTheDocument();
    const opened = await picker('Opened');
    expect(opened).toHaveValue('builtin');
    expect(within(opened).getByRole('option', { name: 'Default: In Review' })).toBeInTheDocument();
    expect(
      within(await picker('Merged')).getByRole('option', { name: 'Default: In Testing' }),
    ).toBeInTheDocument();
    expect(
      within(await picker('Closed without merge')).getByRole('option', { name: 'Default: no move' }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText('ORB preview')).toHaveTextContent(
      'Opened → In Review · Ready for review → In Review · Reopened → In Review · Merged → In Testing · Closed without merge → no move',
    );
    expect(screen.getByRole('button', { name: 'Reset to defaults' })).toHaveAttribute(
      'aria-disabled',
      'true',
    );
  });

  it('saves merged -> Done and opened -> In Testing as custom rules, and the preview follows', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/git-workflow');
    await user.selectOptions(await picker('Merged'), 'sd');
    await user.selectOptions(await picker('Opened'), 'st');
    await user.selectOptions(await picker('Closed without merge'), 'none');
    expect(screen.getByLabelText('ORB preview')).toHaveTextContent('Opened → In Testing');
    expect(screen.getByLabelText('ORB preview')).toHaveTextContent('Merged → Done');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    const put = api.calls.find((c) => c.method === 'PUT')!;
    expect(put.url.pathname).toBe('/api/v1/orgs/orbit/teams/ORB/git-workflow');
    expect(put.body).toEqual({
      rules: [
        { event: 'opened', stateId: 'st', fromStateIds: [] },
        { event: 'merged', stateId: 'sd', fromStateIds: [] },
        { event: 'closed', stateId: null, fromStateIds: [] },
      ],
    });
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Reset to defaults' })).not.toHaveAttribute('aria-disabled'),
    );
  });

  it('Reset to defaults sends an empty list', async () => {
    const rules = builtin();
    rules[3] = { event: 'merged', custom: true, stateId: 'sd', fromStateIds: [], problem: null };
    const api = server(rules);
    const user = userEvent.setup();
    renderApp('/orbit/settings/git-workflow');
    expect(await picker('Merged')).toHaveValue('sd');
    await user.click(screen.getByRole('button', { name: 'Reset to defaults' }));
    expect(await screen.findByText('Reset to defaults.')).toBeInTheDocument();
    expect(api.calls.find((c) => c.method === 'PUT')?.body).toEqual({ rules: [] });
    await waitFor(async () => expect(await picker('Merged')).toHaveValue('builtin'));
  });

  it('keeps the only-from set of an unchanged rule and shows it', async () => {
    const rules = builtin();
    rules[3] = { event: 'merged', custom: true, stateId: 'sd', fromStateIds: ['sr', 'st'], problem: null };
    const api = server(rules);
    const user = userEvent.setup();
    renderApp('/orbit/settings/git-workflow');
    expect(await screen.findByText(/Only from: In Review, In Testing\./)).toBeInTheDocument();
    await user.selectOptions(await picker('Opened'), 'sp');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText('Saved.');
    expect(api.calls.find((c) => c.method === 'PUT')?.body).toEqual({
      rules: [
        { event: 'opened', stateId: 'sp', fromStateIds: [] },
        { event: 'merged', stateId: 'sd', fromStateIds: ['sr', 'st'] },
      ],
    });
  });

  it('flags a rule whose state is archived and does not save it', async () => {
    const rules = builtin();
    rules[3] = {
      event: 'merged',
      custom: true,
      stateId: 'gone',
      fromStateIds: [],
      problem: 'state_archived',
    };
    const api = server(rules);
    const user = userEvent.setup();
    renderApp('/orbit/settings/git-workflow');
    const merged = await picker('Merged');
    expect(merged).toHaveValue('gone');
    expect(merged).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText(/The state of this move is archived/)).toBeInTheDocument();
    expect(screen.getByLabelText('ORB preview')).toHaveTextContent('Merged → an archived state');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Choose another state for Merged: its state is archived.',
    );
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false);
    await user.selectOptions(merged, 'sd');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
  });

  it('flags an archived only-from state on its row and leaves it out of the save', async () => {
    const rules = builtin();
    rules[3] = {
      event: 'merged',
      custom: true,
      stateId: 'sd',
      fromStateIds: ['sr', 'old'],
      problem: 'from_state_archived',
    };
    const api = server(rules);
    const user = userEvent.setup();
    renderApp('/orbit/settings/git-workflow');
    const merged = await picker('Merged');
    expect(merged).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText(/Only from: In Review\./)).toBeInTheDocument();
    expect(
      screen.getByText(/An .only from. state of this move is archived\. Save leaves it out/),
    ).toBeInTheDocument();
    expect(await picker('Opened')).not.toHaveAttribute('aria-invalid');
    // No other change: Save still sends the fix.
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(api.calls.find((c) => c.method === 'PUT')?.body).toEqual({
      rules: [{ event: 'merged', stateId: 'sd', fromStateIds: ['sr'] }],
    });
  });

  it('says nothing changed, and shows a fixed sentence for a refused save', async () => {
    server(builtin(), {
      'PUT /api/v1/orgs/:org/teams/:team/git-workflow': () => ({
        status: 400,
        json: {
          error: 'stateId must be a state of this team that is not archived',
          code: 'validation_failed',
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/git-workflow');
    await picker('Opened');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Nothing changed.');
    await user.selectOptions(await picker('Merged'), 'sd');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(
        'The server did not accept these rules. A state may be archived or removed.',
      ),
    );
    expect(screen.queryByText(/must be a state of this team/)).not.toBeInTheDocument();
  });

  it('a team with no anchors previews no move for every built-in event', () => {
    const names = new Map(states.map((s) => [s.id, s.name]));
    const none = { ...team, reviewStateId: null, testingStateId: null };
    const all = {
      opened: 'builtin',
      ready: 'builtin',
      reopened: 'builtin',
      merged: 'builtin',
      closed: 'builtin',
    } as const;
    expect(previewLine(none, all, names)).toBe(
      'Opened → no move · Ready for review → no move · Reopened → no move · Merged → no move · Closed without merge → no move',
    );
  });
});
