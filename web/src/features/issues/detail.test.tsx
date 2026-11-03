import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Issue } from '../../api/types';
import { baseRoutes, makeComment, makeIssue, mockApi, states, team } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';

const done = states[3]!;
const emptyRel = { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] };

function setup(
  issue: Issue,
  extra: {
    children?: Issue[];
    history?: unknown[];
    attachments?: unknown[];
    comments?: unknown[];
    cyclesOn?: boolean;
    related?: Record<string, unknown>;
    projects?: unknown[];
    routes?: Parameters<typeof mockApi>[0];
  } = {},
) {
  const kids = extra.children ?? [];
  let cur = issue; // a PATCH with removeLabels changes what the next GET returns
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/teams': () => ({
      json: { items: [{ ...team, cyclesEnabled: !!extra.cyclesOn }], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/issues': (r) => ({
      json: {
        items: r.url.searchParams.get('parent') === 'ORB-1' ? kids : [],
        nextCursor: null,
      },
    }),
    'GET /api/v1/orgs/:org/issues/:issue': () => ({
      json: {
        ...cur,
        relations: { ...emptyRel, ...extra.related },
        stateHistory: extra.history ?? [],
        attachments: extra.attachments ?? [],
      },
    }),
    'GET /api/v1/orgs/:org/issues/:issue/comments': () => ({
      json: { items: extra.comments ?? [], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/projects': () => ({
      json: { items: extra.projects ?? [{ id: 'p1', name: 'Launch', teams: [] }], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/projects/:project': () => ({
      json: { id: 'p9', name: 'Old archived', teams: [] },
    }),
    'GET /api/v1/orgs/:org/cycles': () => ({
      json: {
        items: [
          {
            id: 'c8',
            number: 8,
            name: null,
            team: issue.team,
            startsAt: '2020-01-01T00:00:00Z',
            endsAt: '2099-01-01T00:00:00Z',
            completedAt: null,
          },
        ],
        nextCursor: null,
      },
    }),
    'GET /api/v1/orgs/:org/cycles/:cycle/snapshots': () => ({ json: { items: [], nextCursor: null } }),
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => {
      const b = r.body as { removeLabels?: string[] };
      cur = {
        ...cur,
        labels: cur.labels.filter((l) => !b.removeLabels?.includes(l.name)),
        version: cur.version + 1,
      };
      return { json: cur };
    },
    ...extra.routes,
  });
  return api;
}

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe('issue detail layout (LAUN-21)', () => {
  it('has no Description heading, no Edit button and no old caption rows', async () => {
    setup(makeIssue(1, { description: 'Some text' }));
    renderApp('/orbit/issue/ORB-1');
    await screen.findByText('Some text');
    expect(screen.queryByRole('heading', { name: 'Description' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument();
    for (const gone of ['Team', 'Created', 'Updated', 'Branch']) {
      expect(screen.queryByText(gone, { selector: 'dt' })).not.toBeInTheDocument();
    }
    expect(screen.getByRole('heading', { name: 'Properties' })).toBeInTheDocument();
    for (const h of ['Labels', 'Project', 'Related', 'Activity'])
      expect(screen.getByRole('heading', { name: h })).toBeInTheDocument();
  });

  it('edits the description in place from the keyboard, and Escape drops the draft', async () => {
    const api = setup(makeIssue(1, { description: 'Some text' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const view = await screen.findByRole('group', { name: /Description/ });
    view.focus();
    await user.keyboard('{Enter}');
    const box = await screen.findByLabelText('Description (markdown)');
    await user.type(box, ' more');
    await user.keyboard('{Escape}');
    expect(screen.queryByLabelText('Description (markdown)')).not.toBeInTheDocument();
    expect(screen.getByText('Some text')).toBeInTheDocument();
    expect(api.calls.some((c) => c.method === 'PATCH')).toBe(false);
  });

  it('copies the link, the id and the branch name from the toolbar', async () => {
    setup(makeIssue(1));
    const user = userEvent.setup(); // installs its own clipboard; replace it after
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    renderApp('/orbit/issue/ORB-1');
    const bar = await screen.findByRole('group', { name: 'Issue actions' });
    await user.click(within(bar).getByRole('button', { name: 'Copy link' }));
    await user.click(within(bar).getByRole('button', { name: 'Copy ID' }));
    await user.click(within(bar).getByRole('button', { name: 'Copy branch name' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(3));
    expect(writeText.mock.calls[0]![0]).toContain('/orbit/issue/ORB-1');
    expect(writeText.mock.calls[1]![0]).toBe('ORB-1');
    expect(writeText.mock.calls[2]![0]).toBe('chen/orb-1-issue');
  });

  it('shows "Sub-issue of" with a done/total pill when there is a parent', async () => {
    const child = (n: number, st: typeof done) =>
      makeIssue(n, {
        parent: { id: 'i1', identifier: 'ORB-1', title: 'Parent title' },
        state: { id: st.id, name: st.name, type: st.type, color: st.color },
      });
    setup(child(2, done), { children: [child(2, done), child(3, states[0]!)] });
    renderApp('/orbit/issue/ORB-2');
    const link = await screen.findByRole('link', { name: 'ORB-1 Parent title' });
    expect(link).toHaveAttribute('href', '/orbit/issue/ORB-1');
    expect(await screen.findByTitle('1 of 2 sub-issues done')).toHaveTextContent('1/2');
  });

  it('lists sub-issues only when there are some; "+ Add sub-issues" is always there', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByRole('button', { name: '+ Add sub-issues' })).toBeInTheDocument();
    expect(screen.queryByText('No sub-issues.')).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: /Sub-issues/ })).not.toBeInTheDocument();
  });

  it('shows sub-issues as a compact list', async () => {
    setup(makeIssue(1), { children: [makeIssue(5, { title: 'Child five' })] });
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByRole('link', { name: /Child five/ })).toBeInTheDocument();
  });

  it('puts creation, state changes and comments in one Activity stream, comment box last', async () => {
    setup(makeIssue(1, { createdAt: '2026-10-01T00:00:00Z' }), {
      history: [
        {
          id: 'h0',
          fromStateId: null,
          fromStateName: null,
          toStateId: 's1',
          toStateName: 'Backlog',
          actor: null,
          source: 'api',
          createdAt: '2026-10-01T00:00:00Z',
        },
        {
          id: 'h1',
          fromStateId: 's1',
          fromStateName: 'Backlog',
          toStateId: 's3',
          toStateName: 'In Progress',
          actor: { id: 'u1', username: 'chen', name: 'Chen' },
          source: 'api',
          createdAt: '2026-10-03T00:00:00Z',
        },
      ],
      comments: [makeComment('c1', { body: 'Middle comment', createdAt: '2026-10-02T00:00:00Z' })],
    });
    renderApp('/orbit/issue/ORB-1');
    await screen.findByText('Middle comment');
    const act = screen.getByRole('heading', { name: 'Activity' }).closest('section')!;
    const items = within(act)
      .getAllByRole('listitem')
      .map((li) => li.textContent ?? '');
    expect(items[0]).toContain('created the issue');
    expect(items[1]).toContain('Middle comment');
    expect(items[2]).toContain('moved from');
    expect(screen.queryByRole('heading', { name: /Comments/ })).not.toBeInTheDocument();
    const box = within(act).getByLabelText('Leave a comment');
    expect(items.length).toBe(3);
    expect(
      box.compareDocumentPosition(within(act).getByRole('list')) & Node.DOCUMENT_POSITION_PRECEDING,
    ).toBeTruthy();
  });

  it('shows links and files from the issue as timeline items, folded, with state icons and exact times', async () => {
    const att = (id: string, minute: number) => ({
      id,
      issueId: 'i1',
      kind: 'link',
      title: `MR ${id}`,
      subtitle: '',
      url: `https://gitlab.example/mr/${id}`,
      filename: null,
      contentType: null,
      sizeBytes: null,
      downloadUrl: null,
      creator: { id: 'u1', username: 'chen', name: 'Chen' },
      createdAt: `2026-10-02T00:0${minute}:00Z`,
    });
    setup(makeIssue(1, { createdAt: '2026-10-01T00:00:00Z' }), {
      history: [
        {
          id: 'h1',
          fromStateId: 's1',
          fromStateName: 'Backlog',
          toStateId: 's2',
          toStateName: 'Todo',
          actor: { id: 'u1', username: 'chen', name: 'Chen' },
          source: 'gitlab',
          createdAt: '2026-10-03T00:00:00Z',
        },
      ],
      attachments: [att('1', 1), att('2', 2)],
    });
    renderApp('/orbit/issue/ORB-1');
    const act = (await screen.findByRole('heading', { name: 'Activity' })).closest('section')!;
    const items = await waitFor(() => {
      const li = within(act).getAllByRole('listitem');
      expect(li).toHaveLength(3);
      return li;
    });
    expect(items[1]).toHaveTextContent('Chen linked 2 links: MR 1, MR 2');
    expect(within(items[1]!).getByRole('link', { name: 'MR 2' })).toHaveAttribute(
      'href',
      'https://gitlab.example/mr/2',
    );
    expect(items[2]).toHaveTextContent('moved from Backlog to Todo via GitLab');
    const time = items[2]!.querySelector('time')!;
    expect(time).toHaveAttribute('title');
    expect(time.getAttribute('title')).not.toBe('');
  });

  it('shows an attached image once, in the attachments area and not again in the Activity row (LAUN-105, LAUN-115)', async () => {
    const img = {
      id: 'f1',
      issueId: 'i1',
      kind: 'file',
      title: 'shot.png',
      subtitle: '',
      url: null,
      filename: 'shot.png',
      contentType: 'image/png',
      sizeBytes: 10,
      downloadUrl: '/api/v1/orgs/orbit/attachments/f1/download',
      creator: { id: 'u1', username: 'chen', name: 'Chen' },
      createdAt: '2026-10-02T00:00:00Z',
    };
    setup(makeIssue(1, { createdAt: '2026-10-01T00:00:00Z' }), { attachments: [img] });
    renderApp('/orbit/issue/ORB-1');
    const act = (await screen.findByRole('heading', { name: 'Activity' })).closest('section')!;
    const area = await screen.findByRole('region', { name: 'Attachments' });
    expect(within(area).getByRole('img', { name: 'shot.png' })).toHaveAttribute(
      'src',
      '/api/v1/orgs/orbit/attachments/f1/download',
    );
    expect(within(act).getByText(/shot\.png/)).toBeInTheDocument();
    expect(within(act).queryByRole('img')).not.toBeInTheDocument();
    expect(screen.getAllByRole('img', { name: 'shot.png' })).toHaveLength(1);
  });

  it('names no one for links without a creator and caps a long folded list', async () => {
    const att = (n: number) => ({
      id: `a${n}`,
      issueId: 'i1',
      kind: 'link',
      title: `T${n}`,
      subtitle: '',
      url: `https://x.example/${n}`,
      filename: null,
      contentType: null,
      sizeBytes: null,
      downloadUrl: null,
      creator: null,
      createdAt: `2026-10-02T00:00:${String(10 + n).padStart(2, '0')}Z`,
    });
    setup(makeIssue(1, { createdAt: '2026-10-01T00:00:00Z' }), { attachments: [1, 2, 3, 4, 5].map(att) });
    renderApp('/orbit/issue/ORB-1');
    const li = await screen.findByText(/Linked 5 links/);
    expect(li).toHaveTextContent('Linked 5 links: T1, T2, T3 +2 more');
    expect(li).not.toHaveTextContent('Someone');
  });

  it('puts a reply row in a comment card that opens the reply box', async () => {
    setup(makeIssue(1), { comments: [makeComment('c1', { body: 'Root' })] });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Leave a reply to Chen' }));
    expect(await screen.findByLabelText('Write a reply')).toHaveFocus();
    expect(screen.queryByRole('button', { name: 'Leave a reply to Chen' })).not.toBeInTheDocument();
  });

  it('sets the project and the cycle from the properties column', async () => {
    const api = setup(makeIssue(1), { cyclesOn: true });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: /Add to project/ }));
    await user.click(await screen.findByRole('option', { name: 'Launch' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    expect(api.calls.find((c) => c.method === 'PATCH')!.body).toMatchObject({ projectId: 'p1' });
    await user.click(await screen.findByRole('button', { name: /Add to cycle/ }));
    await user.click(await screen.findByRole('option', { name: 'Cycle 8' }));
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(2));
    expect(api.calls.filter((c) => c.method === 'PATCH')[1]!.body).toMatchObject({ cycleId: 'c8' });
  });

  it('shows no Cycle row when the team has cycles off', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('button', { name: /Add to project/ });
    expect(screen.queryByRole('button', { name: /Add to cycle/ })).not.toBeInTheDocument();
  });

  it('folds relations into Related, with remove', async () => {
    setup(makeIssue(1), {
      related: {
        blocks: [
          {
            id: 'r1',
            type: 'blocks',
            issue: { id: 'i9', identifier: 'ORB-9', title: 'Other', stateType: 'started' },
          },
        ],
      },
    });
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByRole('link', { name: /ORB-9 Other/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Remove relation to ORB-9' })).toBeInTheDocument();
    expect(screen.getByLabelText('Relation type')).toBeInTheDocument();
  });

  it('keeps replies nested (list inside the comment, thread class on the stream)', async () => {
    setup(makeIssue(1), {
      comments: [
        makeComment('c1', { body: 'Root', createdAt: '2026-10-02T00:00:00Z' }),
        makeComment('c2', { body: 'Reply', parentId: 'c1', createdAt: '2026-10-02T01:00:00Z' }),
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    const reply = await screen.findByText('Reply', { selector: 'p' });
    const root = screen.getByText('Root', { selector: 'p' }).closest('li')!;
    const nested = reply.closest('ul')!;
    expect(root.contains(nested)).toBe(true);
    // The stream list and the reply list are both `thread` lists, so `.thread .thread` styles the reply.
    const stream = root.parentElement!;
    expect(stream.tagName).toBe('OL');
    expect(stream.className).toMatch(/thread/);
    expect(nested.className).toMatch(/thread/);
  });

  it('does not start an edit on a selection, a link or a click that follows one', async () => {
    setup(makeIssue(1, { description: 'Plain text and [a link](https://example.com/x)' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const link = await screen.findByRole('link', { name: 'a link' });
    await user.click(link);
    expect(screen.queryByLabelText('Description (markdown)')).not.toBeInTheDocument();
    const text = screen.getByText(/Plain text and/);
    const range = document.createRange();
    range.selectNodeContents(text);
    window.getSelection()!.removeAllRanges();
    window.getSelection()!.addRange(range);
    fireEvent.click(text); // (userEvent would clear the selection on mousedown)
    expect(screen.queryByLabelText('Description (markdown)')).not.toBeInTheDocument();
    window.getSelection()!.removeAllRanges();
    await user.click(text);
    expect(await screen.findByLabelText('Description (markdown)')).toBeInTheDocument();
  });

  it('returns focus to the description text when the editor closes', async () => {
    setup(makeIssue(1, { description: 'Some text' }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByText('Some text'));
    await screen.findByLabelText('Description (markdown)');
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.getByRole('group', { name: /Description/ })).toHaveFocus());
  });

  it('names the pickers for a screen reader and has one Properties landmark', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByRole('button', { name: 'Status: Backlog' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Assignee: Assign' })).toBeInTheDocument();
    expect(screen.getAllByRole('complementary', { name: 'Properties' })).toHaveLength(1);
    expect(screen.queryByRole('region', { name: 'Properties' })).not.toBeInTheDocument();
  });

  it('changing the project of an issue with a milestone clears the milestone; "No project" and "No cycle" clear', async () => {
    const api = setup(makeIssue(1, { projectId: 'p1', milestoneId: 'm1', cycleId: 'c8' }), {
      cyclesOn: true,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: /Project: Launch/ }));
    await user.click(await screen.findByRole('option', { name: 'No project' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    const patches = () => api.calls.filter((c) => c.method === 'PATCH');
    expect(patches()[0]!.body).toMatchObject({ projectId: null, milestoneId: null });
    await user.click(await screen.findByRole('button', { name: /Cycle: Cycle 8/ }));
    await user.click(await screen.findByRole('option', { name: 'No cycle' }));
    await waitFor(() => expect(patches()).toHaveLength(2));
    expect(patches()[1]!.body).toMatchObject({ cycleId: null });
  });

  it('shows the project logo in the Project chip and its picker (LAUN-162), empty alt beside the name', async () => {
    const logo = '/api/v1/orgs/orbit/projects/p1/avatar?v=3';
    setup(makeIssue(1, { projectId: 'p1' }), {
      projects: [{ id: 'p1', name: 'Launch', teams: [], avatarUrl: logo }],
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const trigger = await screen.findByRole('button', { name: /Project: Launch/ });
    const chipImg = trigger.querySelector('img')!;
    expect(chipImg).toHaveAttribute('src', logo);
    expect(chipImg).toHaveAttribute('alt', '');
    await user.click(trigger);
    const opt = await screen.findByRole('option', { name: 'Launch' });
    expect(opt.querySelector('img')).toHaveAttribute('src', logo);
    expect(opt.querySelector('img')).toHaveAttribute('alt', '');
  });

  it('shows the project emoji in the Project chip and its picker when there is no logo (LAUN-232)', async () => {
    setup(makeIssue(1, { projectId: 'p1' }), {
      projects: [{ id: 'p1', name: 'Launch', teams: [], avatarUrl: null, emoji: '🚀' }],
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const trigger = await screen.findByRole('button', { name: /Project: Launch/ });
    expect(trigger.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'emoji');
    expect(trigger).toHaveTextContent('🚀');
    await user.click(trigger);
    const opt = await screen.findByRole('option', { name: 'Launch' });
    expect(opt.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'emoji');
    expect(opt).toHaveTextContent('🚀');
  });

  it('names an archived project that is not in the list', async () => {
    setup(makeIssue(1, { projectId: 'p9' }));
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByRole('button', { name: /Project: Old archived/ })).toBeInTheDocument();
  });

  it('does not count a canceled sub-issue in the pill', async () => {
    const st = (n: number) => {
      const s = { ...states[3]!, type: 'canceled' as const, name: 'Canceled' };
      return makeIssue(n, {
        parent: { id: 'i1', identifier: 'ORB-1', title: 'P' },
        state: { id: s.id, name: s.name, type: s.type, color: s.color },
      });
    };
    setup(st(2), { children: [st(2), makeIssue(3)] });
    renderApp('/orbit/issue/ORB-2');
    expect(await screen.findByTitle('0 of 2 sub-issues done')).toHaveTextContent('0/2');
  });

  it('says where a state change came from, and shows the update time on the Created line', async () => {
    setup(makeIssue(1, { updatedAt: '2026-10-05T00:00:00Z' }), {
      history: [
        {
          id: 'h1',
          fromStateId: 's1',
          fromStateName: 'Backlog',
          toStateId: 's3',
          toStateName: 'In Progress',
          actor: null,
          source: 'gitlab',
          createdAt: '2026-10-03T00:00:00Z',
        },
        {
          id: 'h2',
          fromStateId: 's3',
          fromStateName: 'In Progress',
          toStateId: 's4',
          toStateName: 'Done',
          actor: null,
          source: 'import',
          createdAt: '2026-10-04T00:00:00Z',
        },
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByText('GitLab', { selector: 'strong' })).toBeInTheDocument();
    expect(screen.getByText('Import', { selector: 'strong' })).toBeInTheDocument();
    expect(document.querySelector('[title^="Updated "]')).not.toBeNull();
  });

  it('shows an error toast when copying fails or there is no clipboard', async () => {
    setup(makeIssue(1));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const bar = await screen.findByRole('group', { name: 'Issue actions' });
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText: vi.fn().mockRejectedValue(new Error('no')) },
      configurable: true,
    });
    await user.click(within(bar).getByRole('button', { name: 'Copy ID' }));
    expect(await screen.findByText('Could not copy')).toBeInTheDocument();
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    await user.click(within(bar).getByRole('button', { name: 'Copy link' }));
    await waitFor(() => expect(screen.getAllByText('Could not copy').length).toBeGreaterThan(0));
  });
});

describe('labels like Linear (LAUN-46)', () => {
  const bug = { id: 'l1', name: 'bug', color: '#d6481f' };

  it('shows only "Add label" when the issue has no labels', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    const add = await screen.findByRole('button', { name: 'Add label' });
    expect(add).toHaveAttribute('aria-haspopup', 'listbox');
    expect(add).toHaveTextContent('Add label');
    expect(screen.queryByRole('list', { name: 'Labels' })).toBeInTheDocument();
  });

  it('shows pills with a name, then a quiet "+" at the end of the row', async () => {
    setup(makeIssue(1, { labels: [bug, { id: 'l2', name: 'ios', color: '#2255cc' }] }));
    renderApp('/orbit/issue/ORB-1');
    const list = await screen.findByRole('list', { name: 'Labels' });
    const items = within(list).getAllByRole('listitem');
    expect(items).toHaveLength(3);
    expect(items[0]).toHaveTextContent('bug');
    expect(items[1]).toHaveTextContent('ios');
    expect(within(items[2]!).getByRole('button', { name: 'Add label' })).not.toHaveTextContent('Add label');
    expect(screen.getByRole('button', { name: 'Remove label bug' })).toBeInTheDocument();
  });

  it('adds a label from the picker with the keyboard', async () => {
    const api = setup(makeIssue(1));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const add = await screen.findByRole('button', { name: 'Add label' });
    add.focus();
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('combobox', { name: 'Labels' })).toBeInTheDocument();
    await user.keyboard('{Enter}');
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ addLabels: ['bug'] }),
    );
    // multi: the list stays open
    expect(screen.getByRole('listbox', { name: 'Labels' })).toBeInTheDocument();
  });

  it('removes a label with its x button, by mouse and by keyboard', async () => {
    const api = setup(makeIssue(1, { labels: [bug, { id: 'l2', name: 'ios', color: '#2255cc' }] }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Remove label bug' }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ removeLabels: ['bug'] }),
    );
    expect(screen.queryByRole('listbox', { name: 'Labels' })).not.toBeInTheDocument();
    api.calls.length = 0;
    (await screen.findByRole('button', { name: 'Remove label ios' })).focus();
    await user.keyboard('{Enter}');
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ removeLabels: ['ios'] }),
    );
  });

  it('a press on a pill opens the picker, and the picker removes a chosen label', async () => {
    const api = setup(makeIssue(1, { labels: [bug] }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const list = await screen.findByRole('list', { name: 'Labels' });
    await user.click(within(list).getByText('bug'));
    const opt = await screen.findByRole('option', { name: 'bug' });
    expect(opt).toHaveAttribute('aria-selected', 'true');
    await user.click(opt);
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({ removeLabels: ['bug'] }),
    );
  });

  it('moves focus to the next pill\'s x after a removal, the previous when last, "+" when none', async () => {
    const ios = { id: 'l2', name: 'ios', color: '#2255cc' };
    const kmp = { id: 'l3', name: 'kmp', color: '#22cc55' };
    setup(makeIssue(1, { labels: [bug, ios, kmp] }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Remove label ios' }));
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Remove label ios' })).not.toBeInTheDocument(),
    );
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Remove label kmp' }));
    await user.click(screen.getByRole('button', { name: 'Remove label kmp' }));
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Remove label kmp' })).not.toBeInTheDocument(),
    );
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Remove label bug' }));
    screen.getByRole('button', { name: 'Remove label bug' }).focus();
    await user.keyboard('{Enter}');
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Remove label bug' })).not.toBeInTheDocument(),
    );
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Add label' }));
  });

  it('a press on a pill while the picker is open closes it', async () => {
    setup(makeIssue(1, { labels: [bug] }));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const list = await screen.findByRole('list', { name: 'Labels' });
    await user.click(within(list).getByText('bug'));
    expect(await screen.findByRole('listbox', { name: 'Labels' })).toBeInTheDocument();
    await user.click(within(list).getByText('bug'));
    await waitFor(() => expect(screen.queryByRole('listbox', { name: 'Labels' })).not.toBeInTheDocument());
  });

  it('can make a new label from the picker', async () => {
    const api = setup(makeIssue(1));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await user.click(await screen.findByRole('button', { name: 'Add label' }));
    await user.type(await screen.findByRole('combobox', { name: 'Labels' }), 'fresh');
    await user.click(screen.getByRole('option', { name: 'Create “fresh”' }));
    await waitFor(() =>
      expect(api.calls.some((c) => c.method === 'POST' && c.url.pathname.endsWith('/labels'))).toBe(true),
    );
  });
});

describe('breadcrumb trail (LAUN-157)', () => {
  it('shows team > identifier, the issue as the current page', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' });
    expect(within(nav).getByRole('link', { name: team.name })).toHaveAttribute('href', '/orbit/team/ORB');
    const cur = within(nav).getByText('ORB-1');
    expect(cur.closest('li')).toHaveAttribute('aria-current', 'page');
    expect(within(nav).getAllByRole('listitem')).toHaveLength(2);
  });
  it('puts the parent between team and issue as a link', async () => {
    setup(makeIssue(2, { parent: { id: 'i1', identifier: 'ORB-1', title: 'Parent title' } }));
    renderApp('/orbit/issue/ORB-2');
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' });
    expect(within(nav).getByRole('link', { name: 'ORB-1' })).toHaveAttribute('href', '/orbit/issue/ORB-1');
    expect(within(nav).getAllByRole('listitem')).toHaveLength(3);
  });
});

describe('Back button (LAUN-89)', () => {
  it('goes back in history to the view the issue was opened from', async () => {
    setup(makeIssue(1));
    const user = userEvent.setup();
    // A list entry, then the issue: the issue entry has a router key (it was pushed).
    renderApp(['/orbit/my-issues?status=todo', '/orbit/issue/ORB-1']);
    await screen.findByRole('group', { name: 'Issue actions' });
    await user.click(screen.getByRole('button', { name: 'Back' }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/my-issues'));
    expect(currentLocation.search).toBe('?status=todo');
  });

  it('goes to the team issue list when the page was opened with no history', async () => {
    setup(makeIssue(1));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('group', { name: 'Issue actions' });
    await user.click(screen.getByRole('button', { name: 'Back' }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
  });

  it('is a real button with a name, ahead of the team link', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    const back = await screen.findByRole('button', { name: 'Back' });
    expect(back).toHaveAttribute('type', 'button');
    const teamLink = screen.getByRole('link', { name: team.name });
    expect(back.compareDocumentPosition(teamLink) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('treats the first entry as no history after the login redirect (random key, idx 0)', async () => {
    setup(makeIssue(1));
    const user = userEvent.setup();
    // react-router keeps idx in window.history.state; the login replace left idx 0 under a random key.
    window.history.replaceState({ idx: 0 }, '');
    try {
      renderApp(['/orbit/my-issues', '/orbit/issue/ORB-1']);
      await screen.findByRole('group', { name: 'Issue actions' });
      await user.click(screen.getByRole('button', { name: 'Back' }));
      await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
    } finally {
      window.history.replaceState(null, '');
    }
  });

  it('Escape and Back share the same fallback', async () => {
    setup(makeIssue(1));
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('group', { name: 'Issue actions' });
    await user.keyboard('{Escape}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
  });
});
