import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { beforeEach, describe, expect, it } from 'vitest';
import type { Issue, Label } from '../../api/types';
import { baseRoutes, makeIssue, mockApi, team } from '../../test/mockApi';
import { renderApp, renderAt } from '../../test/render';
import { LabelsField } from './LabelsField';
import { labelSections, type UsageIssue } from './LabelList';

// The Linear-style label picker (LAUN-53): search, three groups, boxes, multi-select.

const lab = (id: string, name: string, color = '#d6481f', over: Partial<Label> = {}): Label => ({
  id,
  name,
  color,
  description: '',
  parentId: null,
  teamId: null,
  teamKey: null,
  issueCount: 0,
  createdAt: '2026-01-01T00:00:00Z',
  ...over,
});
// issueCount comes from the server (LAUN-101): bug 3, ios 3, api 2, the rest are used less than twice.
const L = {
  bug: lab('l1', 'bug', '#d6481f', { issueCount: 3 }),
  ios: lab('l2', 'ios', '#2255cc', { issueCount: 3 }),
  web: lab('l3', 'web', '#22cc55', { issueCount: 1 }),
  api: lab('l4', 'api', '#aa22cc', { issueCount: 2 }),
  docs: lab('l5', 'docs', '#cccc22'),
  zed: lab('l6', 'zed', '#888888'),
};
const flaky = lab('l7', 'flaky', '#ff00aa', { teamId: 'team1', teamKey: 'ORB', issueCount: 1 });
const ref = (l: Label) => ({ id: l.id, name: l.name, color: l.color });
const used = (n: number, ls: Label[], over: Partial<UsageIssue> = {}): UsageIssue => ({
  id: `u${n}`,
  identifier: `ORB-${100 + n}`,
  projectId: null,
  labels: ls.map(ref),
  ...over,
});

describe('labelSections (the rules)', () => {
  const all = Object.values(L);
  it('frequent: top 3 by the server issueCount, at least 2 issues, ties by name', () => {
    const g = labelSections([...all, lab('l8', 'cli', '#111111', { issueCount: 3 })], [], {});
    expect(g.frequent.map((l) => l.name)).toEqual(['bug', 'cli', 'ios']);
    expect(g.suggestions).toEqual([]);
    expect(g.rest.map((l) => l.name)).toEqual(['api', 'docs', 'web', 'zed']);
  });
  it('a label used once is not frequent', () => {
    const g = labelSections([L.web, L.docs], [], {});
    expect(g.frequent).toEqual([]);
    expect(g.rest).toHaveLength(2);
  });
  it('team labels are their own group, after Frequently used; org labels are the rest', () => {
    const g = labelSections(
      [...all, flaky, lab('l9', 'pixels', '#000', { teamId: 'team1', teamKey: 'ORB', issueCount: 9 })],
      [],
      {},
    );
    expect(g.frequent.map((l) => l.name)).toEqual(['pixels', 'bug', 'ios']);
    expect(g.team.map((l) => l.name)).toEqual(['flaky']);
    expect(g.rest.map((l) => l.name)).toEqual(['api', 'docs', 'web', 'zed']);
  });
  it('suggestions: labels of the related issues, not the issue itself', () => {
    const related = [
      used(1, [L.docs]),
      used(2, [L.docs, L.zed]),
      used(3, [L.api]),
      used(4, [L.web]),
      used(6, [L.bug], { id: 'me' }),
    ];
    const g = labelSections(all, related, { issueId: 'me' });
    expect(g.suggestions.map((l) => l.name)).toEqual(['docs', 'api', 'web', 'zed']);
  });
  it('no related issues: no suggestions', () => {
    expect(labelSections(all, [], { issueId: 'x' }).suggestions).toEqual([]);
  });
  it('a label that is both frequent and suggested lands only in Suggestions', () => {
    const g = labelSections(all, [used(1, [L.bug])], {});
    expect(g.suggestions.map((l) => l.name)).toEqual(['bug']);
    expect(g.frequent.map((l) => l.name)).not.toContain('bug');
    expect(g.rest.map((l) => l.name)).not.toContain('bug');
  });
  it('each label is in one group only', () => {
    const g = labelSections([...all, flaky], [used(1, [L.bug]), used(2, [L.bug, flaky])], {});
    const names = [...g.suggestions, ...g.frequent, ...g.team, ...g.rest].map((l) => l.name);
    expect(new Set(names).size).toBe(names.length);
    expect(names).toHaveLength(7);
  });
});

const noop = () => undefined;
const emptyRel = { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] };

function setup(issue: Issue, usage: UsageIssue[], patch: 'ok' | 'fail' = 'ok', extra: Label[] = []) {
  let cur = issue;
  const made: Label[] = [];
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/labels': () => ({
      json: { items: [...Object.values(L), ...extra, ...made], nextCursor: null },
    }),
    'POST /api/v1/orgs/:org/labels': (r) => {
      const b = r.body as { name: string; team?: string };
      const l = lab(`n${made.length}`, b.name, '#95a2b3', b.team ? { teamId: 'team1', teamKey: b.team } : {});
      made.push(l);
      return { status: 201, json: l };
    },
    // Only a project query has issues (a parent query is the page's sub-issue list: none).
    'GET /api/v1/orgs/:org/issues': (r) => ({
      json: {
        items: r.url.searchParams.has('project') ? usage : [],
        nextCursor: null,
      },
    }),
    'GET /api/v1/orgs/:org/issues/:issue': () => ({
      json: { ...cur, relations: emptyRel, stateHistory: [], attachments: [] },
    }),
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => {
      if (patch === 'fail') return { status: 500, json: { error: 'boom', code: 'internal' } };
      const b = r.body as { addLabels?: string[]; removeLabels?: string[] };
      const add = Object.values(L).filter((l) => b.addLabels?.includes(l.name));
      cur = {
        ...cur,
        labels: [...cur.labels.filter((l) => !b.removeLabels?.includes(l.name)), ...add.map(ref)],
        version: cur.version + 1,
      };
      return { json: cur };
    },
  });
}

const popular: UsageIssue[] = [used(4, [L.docs], { projectId: 'p1' })];

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe('the label picker on the issue page', () => {
  async function open(issue: Issue, usage = popular, patch: 'ok' | 'fail' = 'ok') {
    const api = setup(issue, usage, patch);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const add = await screen.findByRole('button', { name: 'Add label' });
    await user.click(add);
    const box = await screen.findByRole('combobox', { name: 'Labels' });
    return { api, user, add, box };
  }
  const rows = () =>
    within(screen.getByRole('listbox', { name: 'Labels' }))
      .getAllByRole('option')
      .filter((r) => !r.textContent?.startsWith('Create'));
  const heads = () =>
    [...document.querySelectorAll('[role="listbox"] > li[aria-hidden="true"]')].map((n) => n.textContent);

  it('shows the key hint, the groups with headings, and rows in group order', async () => {
    await open(makeIssue(1, { projectId: 'p1' }));
    expect(screen.getByRole('combobox', { name: 'Labels' })).toHaveAttribute('placeholder', 'Add labels…');
    await waitFor(() => expect(heads()).toEqual(['Suggestions', 'Frequently used', 'Org labels']));
    expect(rows().map((r) => r.textContent)).toEqual(['docs', 'bug', 'ios', 'api', 'web', 'zed']);
    expect(
      screen.getByRole('combobox', { name: 'Labels' }).parentElement!.querySelector('kbd'),
    ).toHaveTextContent('L');
  });

  it('search filters every group and hides a group with no match; empty search shows all', async () => {
    const { user, box } = await open(makeIssue(1, { projectId: 'p1' }));
    await waitFor(() => expect(heads()).toHaveLength(3));
    await user.type(box, 'doc');
    expect(rows().map((r) => r.textContent)).toEqual(['docs']);
    expect(heads()).toEqual([]);
    await user.clear(box);
    await user.type(box, 'bug');
    expect(rows().map((r) => r.textContent)).toEqual(['bug']);
    await user.clear(box);
    expect(heads()).toEqual(['Suggestions', 'Frequently used', 'Org labels']);
  });

  it('with no project and no parent there is no Suggestions group', async () => {
    await open(makeIssue(1));
    await waitFor(() => expect(heads()).toEqual(['Frequently used', 'Org labels']));
  });

  it('ticks several labels, stays open, saves each toggle, and does not move rows', async () => {
    const { api, user } = await open(makeIssue(1));
    await waitFor(() => expect(heads()).toHaveLength(2));
    const before = rows().map((r) => r.textContent);
    await user.click(screen.getByRole('option', { name: 'bug' }));
    await waitFor(() =>
      expect(screen.getByRole('option', { name: 'bug' })).toHaveAttribute('aria-selected', 'true'),
    );
    await user.click(screen.getByRole('option', { name: 'web' }));
    await waitFor(() =>
      expect(api.calls.filter((c) => c.method === 'PATCH').map((c) => c.body)).toEqual([
        expect.objectContaining({ addLabels: ['bug'] }),
        expect.objectContaining({ addLabels: ['web'] }),
      ]),
    );
    expect(screen.getByRole('listbox', { name: 'Labels' })).toBeInTheDocument();
    expect(screen.getByRole('listbox', { name: 'Labels' })).toHaveAttribute('aria-multiselectable', 'true');
    expect(rows().map((r) => r.textContent)).toEqual(before);
    // the ticked row has a checked box
    expect(screen.getByRole('option', { name: 'web' }).querySelector('[class*="checkOn"]')).not.toBeNull();
  });

  it('rolls back a label when the save fails', async () => {
    const { user } = await open(makeIssue(1), popular, 'fail');
    const opt = await screen.findByRole('option', { name: 'bug' });
    await user.click(opt);
    await waitFor(() =>
      expect(screen.getByRole('option', { name: 'bug' })).toHaveAttribute('aria-selected', 'false'),
    );
    expect(screen.getByRole('listbox', { name: 'Labels' })).toBeInTheDocument();
  });

  it('keyboard: arrows move, Enter and Space toggle, Escape closes and focus returns to the trigger', async () => {
    const { api, user, add } = await open(makeIssue(1));
    await waitFor(() => expect(heads()).toHaveLength(2));
    await user.keyboard('{ArrowDown}'); // bug -> ios
    expect(screen.getByRole('combobox', { name: 'Labels' }).getAttribute('aria-activedescendant')).toBe(
      rows()[1]!.id,
    );
    await user.keyboard('{Enter}');
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ addLabels: ['ios'] }),
    );
    api.calls.length = 0;
    await user.keyboard('{ArrowDown} '); // -> api, Space toggles
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ addLabels: ['api'] }),
    );
    expect(screen.getByRole('combobox', { name: 'Labels' })).toHaveValue('');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox', { name: 'Labels' })).not.toBeInTheDocument();
    expect(add).toHaveFocus();
  });

  it('Space types a space once the search box has text', async () => {
    const { api, user, box } = await open(makeIssue(1));
    await user.type(box, 'a b');
    expect(box).toHaveValue('a b');
    expect(api.calls.some((c) => c.method === 'PATCH')).toBe(false);
  });

  it('L on the page opens the picker', async () => {
    setup(makeIssue(1), popular);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('button', { name: 'Add label' });
    await user.keyboard('l');
    const box = await screen.findByRole('combobox', { name: 'Labels' });
    expect(box).toHaveFocus();
    expect(box).toHaveValue('');
    // an "l" typed in the search box is text: it neither closes nor reopens the picker
    await user.keyboard('l');
    expect(box).toHaveValue('l');
    expect(screen.getByRole('combobox', { name: 'Labels' })).toBe(box);
    await user.keyboard('{Escape}');
    expect(screen.getByRole('button', { name: 'Add label' })).toHaveFocus();
  });

  it('a field that mounts with an old open request stays shut; a later request opens it', async () => {
    setup(makeIssue(1), popular);
    const issue = makeIssue(1);
    function Harness() {
      const [n, setN] = useState(3);
      return (
        <>
          <button onClick={() => setN(n + 1)}>request</button>
          <LabelsField
            issue={issue}
            all={[L.bug]}
            headingId="h"
            openRequest={n}
            onAdd={noop}
            onRemove={noop}
            onCreate={noop}
          />
        </>
      );
    }
    const user = userEvent.setup();
    renderAt(<Harness />);
    await screen.findByRole('button', { name: 'Add label' });
    expect(screen.queryByRole('combobox', { name: 'Labels' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'request' }));
    expect(await screen.findByRole('combobox', { name: 'Labels' })).toBeInTheDocument();
  });

  it("asks for the issue's team labels, never for a sample of the team's issues", async () => {
    const api = setup(makeIssue(1), popular);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const add = await screen.findByRole('button', { name: 'Add label' });
    await user.click(add);
    await waitFor(() => expect(heads()).toHaveLength(2));
    const label = api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/labels'));
    expect(label.length).toBeGreaterThan(0);
    for (const c of label) expect(c.url.searchParams.get('team')).toBe(team.key);
    // no 250-issue sample
    expect(
      api.calls.filter(
        (c) =>
          c.method === 'GET' &&
          c.url.pathname.endsWith('/issues') &&
          c.url.searchParams.get('limit') === '250',
      ),
    ).toHaveLength(0);
  });

  it('a project asks for its own issues (Suggestions) with a small limit', async () => {
    const api = setup(makeIssue(1, { projectId: 'p1' }), popular);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Add label' }));
    await waitFor(() => expect(heads()).toContain('Suggestions'));
    const hit = api.calls.find((c) => c.method === 'GET' && c.url.searchParams.get('project') === 'p1');
    expect(hit?.url.searchParams.get('limit')).toBe('100');
  });

  it('shows a Team labels group with the server counts and the org labels in their own group', async () => {
    setup(makeIssue(1), [], 'ok', [flaky]);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Add label' }));
    await waitFor(() => expect(heads()).toEqual(['Frequently used', 'Team labels', 'Org labels']));
    expect(rows().map((r) => r.textContent)).toEqual(['bug', 'ios', 'api', 'flakyORB', 'docs', 'web', 'zed']);
  });

  it('a label made from the picker is a team label of the issue (team is sent)', async () => {
    const api = setup(makeIssue(1), []);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Add label' }));
    const box = await screen.findByRole('combobox', { name: 'Labels' });
    await user.type(box, 'brandnew');
    await user.click(await screen.findByRole('option', { name: /Create/ }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/labels'))).toBeDefined(),
    );
    const post = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/labels'))!;
    expect(post.body).toMatchObject({ name: 'brandnew', team: team.key });
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ addLabels: ['brandnew'] }),
    );
  });
});

describe('the label filter on a team page (LAUN-101)', () => {
  it("asks for that team's labels, groups them, and sends label with team to the issue list", async () => {
    const api = setup(makeIssue(1), []);
    const user = userEvent.setup();
    renderApp(`/orbit/team/${team.key}`);
    await user.click(await screen.findByRole('button', { name: 'Label' }));
    await screen.findByRole('option', { name: 'bug' });
    const asked = api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/labels'));
    expect(asked.some((c) => c.url.searchParams.get('team') === team.key)).toBe(true);
    await user.click(screen.getByRole('option', { name: 'bug' }));
    await waitFor(() =>
      expect(
        api.calls.some(
          (c) =>
            c.method === 'GET' &&
            c.url.pathname.endsWith('/issues') &&
            c.url.searchParams.get('label') === 'bug' &&
            c.url.searchParams.get('team') === team.key,
        ),
      ).toBe(true),
    );
  });
});

describe('the label filter groups (LAUN-101)', () => {
  const headsNow = () =>
    [...document.querySelectorAll('[role="listbox"] > li[aria-hidden="true"]')].map((n) => n.textContent);
  const twin = lab('l8', 'bug', '#000000', { teamId: 'team1', teamKey: 'ORB' });

  it('a team label and an org label of one name show once, under both headings', async () => {
    setup(makeIssue(1), [], 'ok', [twin, flaky]);
    const user = userEvent.setup();
    renderApp(`/orbit/team/${team.key}`);
    await user.click(await screen.findByRole('button', { name: 'Label' }));
    await screen.findByRole('option', { name: 'flaky' });
    expect(screen.getAllByRole('option', { name: 'bug' })).toHaveLength(1);
    expect(headsNow()).toEqual(['Org labels', 'Team labels']);
  });

  it('no headings when the team has no team labels', async () => {
    setup(makeIssue(1), []);
    const user = userEvent.setup();
    renderApp(`/orbit/team/${team.key}`);
    await user.click(await screen.findByRole('button', { name: 'Label' }));
    await screen.findByRole('option', { name: 'bug' });
    expect(headsNow()).toEqual([]);
  });
});

describe('the l quick picker (LAUN-101)', () => {
  const other = { id: 'team2', key: 'WEB', name: 'Web' };
  async function openQuick(issues: Issue[]) {
    const api = mockApi({
      ...baseRoutes(issues),
      'GET /api/v1/orgs/:org/labels': () => ({ json: { items: Object.values(L), nextCursor: null } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText(issues[0]!.title);
    await user.keyboard('jx');
    if (issues.length > 1) await user.keyboard('jx');
    await user.keyboard('l');
    await screen.findByRole('option', { name: 'bug' });
    return api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/labels'));
  }

  it('targets of one team ask for that team', async () => {
    const asked = await openQuick([makeIssue(1, { title: 'One' }), makeIssue(2, { title: 'Two' })]);
    expect(asked.some((c) => c.url.searchParams.get('team') === team.key)).toBe(true);
  });

  it('targets of two teams send no team param', async () => {
    const asked = await openQuick([
      makeIssue(1, { title: 'One' }),
      makeIssue(2, { title: 'Two', team: other }),
    ]);
    expect(asked.length).toBeGreaterThan(0);
    expect(asked.some((c) => c.url.searchParams.has('team'))).toBe(false);
  });
});
