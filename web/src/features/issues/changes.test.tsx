import { screen, waitFor, within } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import type { ChangeRequest, Issue } from '../../api/types';
import { invalidateForEvent } from '../../api/events';
import { keys } from '../../api/keys';
import { patchIssueInCaches } from '../../api/queries';
import { baseRoutes, makeIssue, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { repoWebUrl } from './changeText';

// LAUN-95: the Changes block, the MR and edit rows of the Activity, and the live refresh.

const emptyRel = { blocks: [], blockedBy: [], related: [], duplicateOf: [], duplicates: [] };
const cr = (over: Partial<ChangeRequest> = {}): ChangeRequest => ({
  id: 'cr1',
  integrationId: 'int-1',
  provider: 'gitlab',
  repoPath: 'example-org/example-app',
  number: 98,
  url: 'https://gitlab.example.com/example-org/launchpad/-/merge_requests/98',
  title: 'LAUN-1: the thing',
  author: 'chen',
  authorUser: null,
  state: 'open',
  changesRequested: false,
  checks: null,
  createdAt: '2026-10-02T00:00:00Z',
  updatedAt: '2026-10-02T00:00:00Z',
  ...over,
});
const act = (id: string, kind: string, over: Record<string, unknown> = {}) => ({
  id,
  kind,
  actor: { id: 'u1', username: 'chen', name: 'Chen' },
  source: 'api',
  from: null,
  to: null,
  createdAt: '2026-10-03T00:00:00Z',
  ...over,
});

function setup(
  issue: Issue,
  o: {
    changes?: ChangeRequest[];
    activity?: unknown[];
    relations?: Record<string, unknown>;
    routes?: Record<string, Handler>;
  } = {},
) {
  return mockApi({
    ...baseRoutes([issue]),
    'GET /api/v1/orgs/:org/issues/:issue': () => ({
      json: {
        ...issue,
        relations: { ...emptyRel, ...o.relations },
        stateHistory: [],
        attachments: [],
        ...(o.changes ? { changeRequests: o.changes } : {}),
      },
    }),
    'GET /api/v1/orgs/:org/issues/:issue/activity': () => ({
      json: { items: o.activity ?? [], nextCursor: null },
    }),
    ...o.routes,
  });
}

/** A timeline line by its whole text (the actor's name is a separate element). */
const line = (re: RegExp) =>
  screen.findByText((_c, el) => el?.tagName === 'P' && re.test(el.textContent ?? ''));
const lineNow = (re: RegExp) =>
  screen.getByText((_c, el) => el?.tagName === 'P' && re.test(el.textContent ?? ''));

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

describe('the Changes block', () => {
  it('asks for include=changeRequests', async () => {
    const api = setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('heading', { name: 'Activity' });
    const get = api.calls.find((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues/ORB-1'));
    expect(get?.url.searchParams.get('include')).toContain('changeRequests');
  });

  it('shows nothing, not even the heading, when the issue has no change request', async () => {
    setup(makeIssue(1));
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('heading', { name: 'Activity' });
    expect(screen.queryByRole('heading', { name: 'Changes' })).not.toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Changes' })).not.toBeInTheDocument();
  });

  it('shows nothing for an empty list too', async () => {
    setup(makeIssue(1), { changes: [] });
    renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('heading', { name: 'Activity' });
    expect(screen.queryByRole('heading', { name: 'Changes' })).not.toBeInTheDocument();
  });

  it('draws title, author, repository, state, the changes-requested flag and a safe link', async () => {
    setup(makeIssue(1), { changes: [cr({ changesRequested: true, state: 'open' })] });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    const link = within(block).getByRole('link', { name: /!98 LAUN-1: the thing/ });
    expect(link).toHaveAttribute(
      'href',
      'https://gitlab.example.com/example-org/launchpad/-/merge_requests/98',
    );
    expect(link).toHaveAttribute('rel', expect.stringContaining('noopener'));
    expect(within(block).getByText('Open')).toBeInTheDocument();
    expect(within(block).getByText('Changes requested')).toBeInTheDocument();
    expect(block).toHaveTextContent('example-org/example-app');
    expect(block).toHaveTextContent('by chen');
  });

  it('shows the linked member in place of the host login, and the login to a screen reader (LAUN-199)', async () => {
    setup(makeIssue(1), {
      changes: [
        cr({ author: 'chen-gh', authorUser: { id: 'u9', username: 'ann', name: 'Ann Lee' } }),
        cr({ id: 'cr2', number: 99, author: 'zed', authorUser: null }),
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    expect(block).toHaveTextContent('by Ann Lee');
    expect(block).toHaveTextContent('host login chen-gh');
    expect(block).toHaveTextContent('(linked, not verified)');
    expect(block.textContent).not.toMatch(/ {2}/);
    expect(block.querySelector('[title$="linked by the member, not verified"]')).not.toBeNull();
    expect(block).not.toHaveTextContent('by chen-gh');
    // No link: the plain host login, as before.
    expect(block).toHaveTextContent('by zed');
  });

  it('shows the checks badge only when checks is not null', async () => {
    setup(makeIssue(1), {
      changes: [
        cr({ id: 'a', checks: null }),
        cr({ id: 'b', number: 2, checks: 'failure' }),
        cr({ id: 'c', number: 3, checks: 'success' }),
        cr({ id: 'd', number: 4, checks: 'pending' }),
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    expect(within(block).getAllByText(/^Checks /)).toHaveLength(3);
    const rows = within(block).getAllByRole('listitem');
    expect(within(rows[0]!).queryByText(/Checks/)).not.toBeInTheDocument();
    expect(within(rows[1]!).getByText('Checks failed')).toBeInTheDocument();
    expect(within(rows[2]!).getByText('Checks passed')).toBeInTheDocument();
    expect(within(rows[3]!).getByText('Checks running')).toBeInTheDocument();
  });

  it('host text is plain text, and a non-http url is not a link', async () => {
    setup(makeIssue(1), {
      changes: [
        cr({
          title: '<img src=x onerror=alert(1)> **bold** [x](http://evil)',
          author: '<b>mallory</b>',
          repoPath: '<i>g/p</i>',
          url: 'javascript:alert(1)',
          provider: 'github',
          number: 7,
        }),
      ],
    });
    const { container } = renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    expect(block).toHaveTextContent('#7 <img src=x onerror=alert(1)> **bold** [x](http://evil)');
    expect(block).toHaveTextContent('by <b>mallory</b>');
    expect(container.querySelector('img')).toBeNull();
    expect(block.querySelector('b, i, strong')).toBeNull();
    // The one link left is the admin's link to the connection in Settings (LAUN-239): no host URL is a link.
    expect(
      within(block)
        .getAllByRole('link')
        .map((a) => a.getAttribute('href')),
    ).toEqual(['/orbit/settings/integrations/int-1']);
  });

  it('merged and draft requests say so', async () => {
    setup(makeIssue(1), {
      changes: [cr({ id: 'a', state: 'merged' }), cr({ id: 'b', number: 5, state: 'draft' })],
    });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    expect(within(block).getByText('Merged')).toBeInTheDocument();
    expect(within(block).getByText('Draft')).toBeInTheDocument();
  });

  it('an issue.updated event refreshes the block and the Activity', async () => {
    let changes = undefined as ChangeRequest[] | undefined;
    let rows: unknown[] = [];
    const issue = makeIssue(1);
    mockApi({
      ...baseRoutes([issue]),
      'GET /api/v1/orgs/:org/issues/:issue': () => ({
        json: {
          ...issue,
          relations: emptyRel,
          stateHistory: [],
          attachments: [],
          ...(changes ? { changeRequests: changes } : {}),
        },
      }),
      'GET /api/v1/orgs/:org/issues/:issue/activity': () => ({ json: { items: rows, nextCursor: null } }),
    });
    const { client } = renderApp('/orbit/issue/ORB-1');
    await screen.findByRole('heading', { name: 'Activity' });
    expect(screen.queryByRole('heading', { name: 'Changes' })).not.toBeInTheDocument();
    changes = [cr()];
    rows = [
      act('m1', 'mrLinked', {
        actor: null,
        source: 'gitlab',
        to: {
          provider: 'gitlab',
          repoPath: 'example-org/example-app',
          number: 98,
          url: 'https://x.test/98',
          title: 'T',
        },
      }),
    ];
    invalidateForEvent(client, 'orbit', { type: 'issue.updated' });
    expect(await screen.findByRole('region', { name: 'Changes' })).toBeInTheDocument();
    expect(await line(/GitLab linked merge request/)).toBeInTheDocument();
  });

  it('the activity query sits under the issueDetails key', () => {
    const k = keys.issueActivity('orbit', 'orb-1');
    expect(k.slice(0, 3)).toEqual(keys.issueDetails('orbit'));
    expect(k.slice(0, 4)).toEqual(keys.issue('orbit', 'ORB-1'));
  });
});

const asMember = () => ({
  'GET /api/v1/auth/session': () => ({
    json: {
      user: { id: 'u1', username: 'chen', name: 'Chen', email: 'chen@example.com', isPlatformAdmin: false },
      orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'member' }],
      csrfToken: 'csrf-test',
    },
  }),
});

describe('the repository and host links of a change request (LAUN-239)', () => {
  const hosts = [
    {
      provider: 'gitlab' as const,
      label: 'GitLab',
      repoPath: 'acme/team-docs',
      number: 451,
      url: 'https://gitlab.example.com/acme/team-docs/-/merge_requests/451',
      repo: 'https://gitlab.example.com/acme/team-docs',
    },
    {
      provider: 'github' as const,
      label: 'GitHub',
      repoPath: 'example-org/example-app',
      number: 17,
      url: 'https://github.com/example-org/example-app/pull/17',
      repo: 'https://github.com/example-org/example-app',
    },
    {
      provider: 'bitbucket' as const,
      label: 'Bitbucket',
      repoPath: 'example-org/example-app',
      number: 17,
      url: 'https://bitbucket.org/example-org/example-app/pull-requests/17',
      repo: 'https://bitbucket.org/example-org/example-app',
    },
  ];

  it.each(hosts)(
    '$label: the repository opens on the host, the host name opens its connection',
    async (h) => {
      setup(makeIssue(1), {
        changes: [
          cr({ provider: h.provider, repoPath: h.repoPath, number: h.number, url: h.url, state: 'merged' }),
        ],
      });
      renderApp('/orbit/issue/ORB-1');
      const block = await screen.findByRole('region', { name: 'Changes' });
      const repo = within(block).getByRole('link', {
        name: `Repository ${h.repoPath} on ${h.label}, opens in a new tab`,
      });
      expect(repo).toHaveAttribute('href', h.repo);
      expect(repo).toHaveAttribute('target', '_blank');
      expect(repo).toHaveAttribute('rel', 'noopener noreferrer');
      expect(repo).toHaveTextContent(h.repoPath);
      const host = within(block).getByRole('link', { name: `${h.label} integration settings` });
      expect(host).toHaveAttribute('href', '/orbit/settings/integrations/int-1');
      expect(host).not.toHaveAttribute('target');
      expect(host).toHaveTextContent(h.label);
      // The title link is as before.
      const ref = `${h.provider === 'gitlab' ? '!' : '#'}${h.number} LAUN-1`;
      expect(within(block).getByRole('link', { name: new RegExp(`^${ref}`) })).toHaveAttribute('href', h.url);
      expect(block).toHaveTextContent(`${h.repoPath} · by chen · ${h.label}`);
    },
  );

  it('a click on the host name opens the connection page in Settings', async () => {
    setup(makeIssue(1), {
      changes: [cr({ integrationId: 'int-7' })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration': () => ({
          status: 404,
          json: { error: 'not found', code: 'not_found' },
        }),
      },
    });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    within(block).getByRole('link', { name: 'GitLab integration settings' }).click();
    expect(await screen.findByText('This connection does not exist, or it was removed.')).toBeInTheDocument();
  });

  it('a member gets the host name as plain text, and still the repository link', async () => {
    setup(makeIssue(1), {
      changes: [cr({ url: 'https://gitlab.example.com/example-org/example-app/-/merge_requests/98' })],
      routes: asMember(),
    });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    expect(within(block).queryByRole('link', { name: /integration settings/ })).not.toBeInTheDocument();
    expect(block.querySelector('a[href*="/settings/"]')).toBeNull();
    expect(block).toHaveTextContent('GitLab');
    expect(
      within(block).getByRole('link', { name: /^Repository example-org\/example-app on GitLab/ }),
    ).toHaveAttribute('href', 'https://gitlab.example.com/example-org/example-app');
  });

  it('a hostile or unmatched request URL leaves the repository as text', async () => {
    const bad = [
      'javascript:alert(1)//gitlab.example.com/example-org/example-app/-/merge_requests/98',
      'data:text/html,/example-org/example-app/-/merge_requests/98',
      'ftp://gitlab.example.com/example-org/example-app/-/merge_requests/98',
      'https://user:pw@gitlab.example.com/example-org/example-app/-/merge_requests/98',
      'https://gitlab.example.com/other-org/other-app/-/merge_requests/98',
      'https://gitlab.example.com/example-org/example-app/-/merge_requests/99',
    ];
    setup(makeIssue(1), { changes: bad.map((url, i) => cr({ id: `c${i}`, url })) });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    expect(within(block).queryByRole('link', { name: /^Repository/ })).not.toBeInTheDocument();
    for (const a of within(block).getAllByRole('link')) {
      expect(a.getAttribute('href')).toMatch(/^(https:\/\/|\/orbit\/settings\/integrations\/)/);
    }
    expect(within(block).getAllByText(/example-org\/example-app/)).toHaveLength(bad.length);
  });

  it('a long one-word repository stays in its text run, which wraps (no new box)', async () => {
    const long = `example-org/${'a'.repeat(200)}`;
    setup(makeIssue(1), {
      changes: [cr({ repoPath: long, url: `https://gitlab.example.com/${long}/-/merge_requests/98` })],
    });
    renderApp('/orbit/issue/ORB-1');
    const block = await screen.findByRole('region', { name: 'Changes' });
    const repo = within(block).getByRole('link', { name: new RegExp(`^Repository ${long}`) });
    // A plain inline anchor in the .crText run (overflow-wrap: anywhere): no class, no box of its own.
    expect(repo.getAttribute('class')).toBeNull();
    expect(repo.parentElement?.className).toMatch(/crText/);
  });
});

describe('repoWebUrl', () => {
  it.each([
    [
      'gitlab',
      'https://gitlab.example.com/g/sub/p/-/merge_requests/3',
      'g/sub/p',
      'https://gitlab.example.com/g/sub/p',
    ],
    ['gitlab', 'https://example.com/gitlab/g/p/-/merge_requests/3', 'g/p', 'https://example.com/gitlab/g/p'],
    [
      'gitlab',
      'http://gitlab.internal.example.com/g/p/-/merge_requests/3/',
      'g/p',
      'http://gitlab.internal.example.com/g/p',
    ],
    ['github', 'https://github.example.com/O/R/pull/3', 'o/r', 'https://github.example.com/O/R'],
    ['bitbucket', 'https://bitbucket.org/w/r/pull-requests/3', 'w/r', 'https://bitbucket.org/w/r'],
    [
      'bitbucket',
      'https://bitbucket.example.test/projects/PROJ/repos/example-app/pull-requests/3',
      'PROJ/example-app',
      'https://bitbucket.example.test/projects/PROJ/repos/example-app',
    ],
  ])('%s %s', (provider, url, repoPath, want) => {
    expect(repoWebUrl(provider, url, 3, repoPath)).toBe(want);
  });

  it.each([
    ['javascript:alert(1)//x/g/p/-/merge_requests/3', 'gitlab'],
    ['JAVASCRIPT://x/g/p/-/merge_requests/3', 'gitlab'],
    ['data:text/html,/g/p/-/merge_requests/3', 'gitlab'],
    ['ftp://x.example.com/g/p/-/merge_requests/3', 'gitlab'],
    ['vbscript:x/g/p/-/merge_requests/3', 'gitlab'],
    ['//x.example.com/g/p/-/merge_requests/3', 'gitlab'],
    ['https://u@x.example.com/g/p/-/merge_requests/3', 'gitlab'],
    ['https://:pw@x.example.com/g/p/-/merge_requests/3', 'gitlab'],
    ['https://github.example.com/projects/g/repos/p/pull/3', 'github'],
    ['https://x.example.com/g/p/-/merge_requests/4', 'gitlab'],
    ['https://x.example.com/g/p/pull/3', 'gitlab'],
    ['https://x.example.com/g/p/-/merge_requests/3', 'gerrit'],
    ['https://x.example.com/evil/-/merge_requests/3', 'gitlab'],
    ['https://x.example.com/xg/p/-/merge_requests/3', 'gitlab'],
    ['not a url', 'gitlab'],
    ['', 'gitlab'],
  ])('%s (%s) gives no link', (url, provider) => {
    expect(repoWebUrl(provider, url, 3, 'g/p')).toBeNull();
  });

  it('no repository label or no number gives no link', () => {
    expect(repoWebUrl('gitlab', 'https://x.example.com/g/p/-/merge_requests/3', 3, '')).toBeNull();
    expect(repoWebUrl('gitlab', 'https://x.example.com/g/p/-/merge_requests/3', null, 'g/p')).toBeNull();
  });
});

describe('the Activity timeline: every kind the server sends', () => {
  const states = {
    title: act('t', 'title', { from: 'Old', to: 'New' }),
  };
  it('renders the MR rows with a link, the provider as the name and plain text', async () => {
    const to = (extra: object = {}) => ({
      provider: 'gitlab',
      repoPath: 'example-org/example-app',
      number: 98,
      url: 'https://gitlab.example.com/mr/98',
      title: '<b>Title</b>',
      ...extra,
    });
    setup(makeIssue(1), {
      activity: [
        act('m1', 'mrLinked', { actor: null, source: 'gitlab', to: to(), createdAt: '2026-10-03T00:00:00Z' }),
        act('m2', 'mrMerged', { actor: null, source: 'gitlab', to: to(), createdAt: '2026-10-04T00:00:00Z' }),
        act('m3', 'mrClosed', {
          actor: null,
          source: 'github',
          to: to({ provider: 'github', number: 5, url: 'javascript:x' }),
          createdAt: '2026-10-05T00:00:00Z',
        }),
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    const linked = await line(/GitLab linked merge request/);
    expect(within(linked.closest('li')!).getByRole('link', { name: '!98 <b>Title</b>' })).toHaveAttribute(
      'href',
      'https://gitlab.example.com/mr/98',
    );
    expect(lineNow(/GitLab marked merge request/)).toHaveTextContent('as merged');
    const closed = lineNow(/GitHub marked pull request/);
    expect(closed).toHaveTextContent('as closed');
    expect(within(closed).queryByRole('link')).not.toBeInTheDocument();
    expect(closed).toHaveTextContent('#5 <b>Title</b>');
  });

  it('renders the field edits in words', async () => {
    setup(makeIssue(1), {
      activity: [
        states.title,
        act('d', 'description'),
        act('as', 'assignee', { to: { id: 'u2', username: 'claude', name: 'Claude' } }),
        act('pr', 'priority', { from: 3, to: 1 }),
        act('es', 'estimate', { from: 2, to: null }),
        act('du', 'dueDate', { to: '2026-11-01' }),
        act('pa', 'parent', { to: { id: 'p', identifier: 'ORB-9', title: 'Parent' } }),
        act('pj', 'project', { from: { id: 'x', name: 'Alpha' }, to: { id: 'y', name: 'Beta' } }),
        act('mi', 'milestone', { to: { id: 'y', name: 'M1' } }),
        act('cy', 'cycle', { actor: null, source: 'system', to: { id: 'c', number: 8, name: null } }),
        act('rr', 'relationRemoved', {
          from: { type: 'blockedBy', issue: { id: 'z', identifier: 'ORB-4', title: 'Z' } },
        }),
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    await line(/changed the title from Old to New/);
    for (const re of [
      /Chen edited the description/,
      /Chen set the assignee to Claude/,
      /Chen changed the priority from Medium to Urgent/,
      /Chen removed the estimate \(2\)/,
      /Chen set the due date to 2026-11-01/,
      /Chen set the parent to ORB-9 Parent/,
      /Chen changed the project from Alpha to Beta/,
      /Chen set the milestone to M1/,
      /System set the cycle to Cycle 8/,
      /Chen removed the relation: blocked by ORB-4/,
    ])
      expect(lineNow(re)).toBeInTheDocument();
  });

  it('folds labels added together, and shows a kind it does not know', async () => {
    setup(makeIssue(1), {
      activity: [
        act('l1', 'labelAdded', { to: { id: 'a', name: 'bug' } }),
        act('l2', 'labelAdded', { to: { id: 'b', name: 'ui' } }),
        act('l3', 'labelRemoved', { from: { id: 'c', name: 'old' }, createdAt: '2026-10-03T01:00:00Z' }),
        act('x', 'somethingNew', { createdAt: '2026-10-03T02:00:00Z' }),
      ],
    });
    renderApp('/orbit/issue/ORB-1');
    expect(await line(/added 2 labels:/)).toHaveTextContent('bug, ui');
    expect(lineNow(/removed the label/)).toHaveTextContent('old');
    expect(lineNow(/Chen changed somethingNew/)).toBeInTheDocument();
  });

  it('shows who added a relation, and a relation with no known creator without a name', async () => {
    const issue = (id: string, n: number) => ({ id, identifier: `ORB-${n}`, title: `Other ${n}` });
    setup(makeIssue(1), {
      relations: {
        blocks: [
          {
            id: 'r1',
            type: 'blocks',
            issue: issue('i2', 2),
            createdAt: '2026-10-03T00:00:00Z',
            createdBy: { id: 'u2', username: 'claude', name: 'Claude' },
          },
        ],
        related: [
          {
            id: 'r2',
            type: 'related',
            issue: issue('i3', 3),
            createdAt: '2026-10-04T00:00:00Z',
            createdBy: null,
          },
        ],
      },
    });
    renderApp('/orbit/issue/ORB-1');
    const a = await line(/Claude marked this as blocking/);
    expect(within(a).getByRole('link', { name: 'ORB-2' })).toHaveAttribute('href', '/orbit/issue/ORB-2');
    const b = lineNow(/^Marked this as related to/);
    expect(within(b).getByRole('link', { name: 'ORB-3' })).toBeInTheDocument();
  });

  it('a failed activity read shows an error and still draws the rest', async () => {
    setup(makeIssue(1), {
      routes: {
        'GET /api/v1/orgs/:org/issues/:issue/activity': () => ({
          status: 500,
          json: { error: 'x', code: 'internal' },
        }),
      },
    });
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByText('Could not load the recorded changes')).toBeInTheDocument();
    expect(screen.getByText(/created the issue/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Activity' })).toBeInTheDocument());
  });
});

describe('activity paging, caches and the MR link (LAUN-95)', () => {
  it('shows an MR link once when the server also made the link attachment', async () => {
    const url = 'https://gitlab.example.com/mr/98';
    const issue = makeIssue(1);
    mockApi({
      ...baseRoutes([issue]),
      'GET /api/v1/orgs/:org/issues/:issue': () => ({
        json: {
          ...issue,
          relations: emptyRel,
          stateHistory: [],
          attachments: [
            {
              id: 'at1',
              issueId: 'i1',
              kind: 'link',
              title: 'MR !98: T',
              subtitle: '',
              url,
              filename: null,
              contentType: null,
              sizeBytes: null,
              downloadUrl: null,
              creator: null,
              createdAt: '2026-10-03T00:00:00Z',
            },
          ],
        },
      }),
      'GET /api/v1/orgs/:org/issues/:issue/activity': () => ({
        json: {
          items: [
            act('m1', 'mrLinked', {
              actor: null,
              source: 'gitlab',
              to: { provider: 'gitlab', repoPath: 'a/b', number: 98, url, title: 'T' },
            }),
          ],
          nextCursor: null,
        },
      }),
    });
    renderApp('/orbit/issue/ORB-1');
    await line(/GitLab linked merge request/);
    expect(screen.queryByText(/MR !98: T/)).not.toBeInTheDocument();
    expect(screen.getAllByRole('link', { name: /!98/ })).toHaveLength(1);
  });

  it('reads every page by cursor, oldest first', async () => {
    const seen: (string | null)[] = [];
    setup(makeIssue(1), {
      routes: {
        'GET /api/v1/orgs/:org/issues/:issue/activity': (r) => {
          const c = r.url.searchParams.get('cursor');
          seen.push(c);
          return c
            ? {
                json: {
                  items: [act('b', 'description', { createdAt: '2026-10-04T00:00:00Z' })],
                  nextCursor: null,
                },
              }
            : { json: { items: [act('a', 'title', { from: 'A', to: 'B' })], nextCursor: 'c2' } };
        },
      },
    });
    renderApp('/orbit/issue/ORB-1');
    await line(/edited the description/);
    expect(await line(/changed the title from A to B/)).toBeInTheDocument();
    expect(seen).toEqual([null, 'c2']);
    expect(screen.queryByText(/not all shown/)).not.toBeInTheDocument();
  });

  it('says so when the history is longer than the cap', async () => {
    setup(makeIssue(1), {
      routes: {
        'GET /api/v1/orgs/:org/issues/:issue/activity': () => ({
          json: { items: [act('a', 'description')], nextCursor: 'more' },
        }),
      },
    });
    renderApp('/orbit/issue/ORB-1');
    expect(await screen.findByText(/newest recorded changes are not all shown/)).toBeInTheDocument();
  });

  it('a patch of the issue caches leaves the activity array alone', async () => {
    setup(makeIssue(1), { activity: [act('a', 'description')] });
    const { client } = renderApp('/orbit/issue/ORB-1');
    await line(/edited the description/);
    const key = keys.issueActivity('orbit', 'ORB-1');
    const before = client.getQueryData(key);
    patchIssueInCaches(client, 'orbit', 'i1', { title: 'New' });
    expect(client.getQueryData(key)).toBe(before);
    expect(client.getQueryData<{ title: string }>(keys.issue('orbit', 'ORB-1'))?.title).toBe('New');
  });
});
