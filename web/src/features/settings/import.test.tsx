import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { pollDelay } from '../../api/importer';
import { baseRoutes, members, mockApi, team, type Handler, type MockRequest } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { searchSettings, settingsPages } from './settingsPages';
import { stepsFor } from './importText';

// LAUN-10: the Linear import wizard (decisions-T17 #36 to #52).

const KEY = 'lin_api_SECRETKEY0123456789abcdefghijklmnop';
const SID = '01a00000-0000-7000-8000-000000000001';

function job(over: Record<string, unknown> = {}) {
  return {
    id: '01a00000-0000-7000-8000-0000000000j1',
    sessionId: SID,
    kind: 'export',
    state: 'succeeded',
    phase: '',
    done: 0,
    total: 0,
    report: null,
    rows: [],
    rowsTruncated: false,
    committed: false,
    errorCode: null,
    errorText: null,
    cancelRequested: false,
    failedFiles: 0,
    failedFileNames: [],
    createdAt: '2026-10-26T10:00:00Z',
    startedAt: '2026-10-26T10:00:01Z',
    finishedAt: '2026-10-26T10:05:00Z',
    ...over,
  };
}

function report(over: Record<string, unknown> = {}) {
  return {
    dryRun: true,
    counts: {
      issues: { export: 120, excluded: 2, create: 118, update: 0, skip: 0 },
      comments: { export: 300, excluded: 0, create: 300, update: 0, skip: 0 },
    },
    exclusionsTotal: 2,
    exclusions: [
      { entity: 'issues', sourceId: 'lin-1', reason: 'archived team' },
      { entity: 'issues', sourceId: 'lin-2', reason: 'archived team' },
    ],
    mergedLabels: 0,
    mergedRelations: 0,
    missingCycleNumbers: {},
    nextIssueNumbers: {},
    cadence: {},
    pendingUploads: 0,
    uploads: { uploaded: 0, present: 4, planned: 17 },
    ...over,
  };
}

function session(over: Record<string, unknown> = {}) {
  return {
    id: SID,
    state: 'new',
    createdBy: 'u1',
    exportCounts: {},
    hasUserMap: false,
    failedFiles: 0,
    failedFileNames: [],
    jobs: { export: null, dryRun: null, import: null, verify: null },
    createdAt: '2026-10-26T10:00:00Z',
    updatedAt: '2026-10-26T10:00:00Z',
    closedAt: null,
    ...over,
  };
}

const COUNTS = { issues: 120, comments: 300 };
const USERS = {
  users: [
    {
      id: 'lu1',
      name: 'Chen W',
      displayName: 'chen',
      email: 'chen@example.com',
      active: true,
      app: false,
      suggested: 'chen',
      mapped: null,
    },
    {
      id: 'lu2',
      name: 'Sam Smith',
      displayName: 'sam',
      email: 'sam@example.com',
      active: true,
      app: false,
      suggested: null,
      mapped: null,
    },
  ],
  members: [
    { username: 'chen', name: 'Chen', isAgent: false },
    { username: 'claude', name: 'Claude', isAgent: true },
  ],
  map: null,
};

function server(
  opts: { current?: unknown; role?: 'admin' | 'member'; routes?: Record<string, Handler> } = {},
) {
  const cur = opts.current === undefined ? session() : opts.current;
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'chen@example.com',
          isPlatformAdmin: false,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: opts.role ?? 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/import/sessions/current': () =>
      cur === null ? { status: 404, json: { error: 'no open session', code: 'not_found' } } : { json: cur },
    'GET /api/v1/orgs/:org/import/sessions/:s/users': () => ({ json: USERS }),
    ...opts.routes,
  });
}

const posts = (api: { calls: MockRequest[] }, end: string) =>
  api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith(end));
const open = (path = '/orbit/settings/import') => renderApp(path);
const stepHeading = (n: number) => screen.findByRole('heading', { name: new RegExp(`^Step ${n}:`) });

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('the page and its gate', () => {
  it('is in the settings list for admins, with keywords', () => {
    const page = settingsPages.find((p) => p.path === 'import');
    expect(page).toMatchObject({ label: 'Import from Linear', access: 'admin' });
    expect(page!.keywords.length).toBeGreaterThan(3);
    expect(searchSettings(settingsPages, 'api key').map((h) => h.page.path)).toContain('import');
    expect(searchSettings(settingsPages, 'dry run').map((h) => h.page.path)).toContain('import');
  });

  it('a member sees the notice and no import call is made', async () => {
    const api = server({ role: 'member' });
    open();
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(api.calls.some((c) => c.url.pathname.includes('/import/'))).toBe(false);
  });
});

describe('the wizard reloads into the running step', () => {
  const failedImport = job({
    kind: 'import',
    state: 'failed',
    errorCode: 'import_failed',
    createdAt: '2026-10-26T11:00:00Z',
  });
  const cases: Array<[string, unknown, number]> = [
    ['no session (404)', null, 1],
    ['new', session(), 1],
    [
      'exporting',
      session({
        state: 'exporting',
        jobs: {
          export: job({ state: 'running', phase: 'issues', done: 5, total: 10 }),
          dryRun: null,
          import: null,
          verify: null,
        },
      }),
      1,
    ],
    [
      'exported',
      session({
        state: 'exported',
        exportCounts: COUNTS,
        jobs: { export: job(), dryRun: null, import: null, verify: null },
      }),
      2,
    ],
    ['user_map without a saved map', session({ state: 'user_map', exportCounts: COUNTS }), 2],
    ['user_map with a saved map', session({ state: 'user_map', exportCounts: COUNTS, hasUserMap: true }), 3],
    [
      'dry_run_ok',
      session({
        state: 'dry_run_ok',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: null,
          verify: null,
        },
      }),
      3,
    ],
    [
      'user_map with a dry run running',
      session({
        state: 'user_map',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', state: 'running', phase: 'issues', done: 1, total: 4 }),
          import: null,
          verify: null,
        },
      }),
      3,
    ],
    [
      'importing',
      session({
        state: 'importing',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: job({ kind: 'import', state: 'running', phase: 'issues', done: 2, total: 9 }),
          verify: null,
        },
      }),
      4,
    ],
    [
      'imported',
      session({
        state: 'imported',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: job({ kind: 'import', committed: true }),
          verify: null,
        },
      }),
      4,
    ],
    [
      'verified',
      session({
        state: 'verified',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: job({ kind: 'import', committed: true }),
          verify: job({ kind: 'verify' }),
        },
      }),
      5,
    ],
    [
      'failed, last job an import',
      session({
        state: 'failed',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: failedImport,
          verify: null,
        },
      }),
      4,
    ],
  ];

  it.each(cases)('%s opens step %i', async (_name, current, step) => {
    server({ current });
    open();
    expect(await stepHeading(step)).toBeInTheDocument();
  });

  it('shows a banner and progress while a job runs', async () => {
    server({
      current: session({
        state: 'exporting',
        jobs: {
          export: job({ state: 'running', phase: 'issues', done: 5, total: 10 }),
          dryRun: null,
          import: null,
          verify: null,
        },
      }),
    });
    open();
    expect(await screen.findByTestId('job-banner')).toHaveTextContent('An import job is running: export');
    expect(screen.getByText('issues: 5 of 10')).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: 'Exporting from Linear' })).toHaveAttribute('value', '5');
  });

  it('maps every state to a step range that never skips ahead', () => {
    expect(stepsFor(null)).toEqual({ current: 1, max: 1 });
    expect(stepsFor(session({ state: 'exported', exportCounts: COUNTS }) as never)).toEqual({
      current: 2,
      max: 2,
    });
    expect(stepsFor(session({ state: 'imported' }) as never)).toEqual({ current: 4, max: 5 });
  });
});

describe('polling', () => {
  it('backs off after failures and stops at a ceiling', () => {
    expect(pollDelay(0)).toBe(2000);
    expect(pollDelay(1)).toBe(4000);
    expect(pollDelay(3)).toBe(16_000);
    expect(pollDelay(10)).toBe(30_000);
  });

  it('polls while a job runs and stops when the page goes away', async () => {
    const api = server({
      current: session({
        state: 'exporting',
        jobs: {
          export: job({ state: 'running', phase: 'issues', done: 1, total: 2 }),
          dryRun: null,
          import: null,
          verify: null,
        },
      }),
    });
    const { unmount } = open();
    const count = () => api.calls.filter((c) => c.url.pathname.endsWith('/sessions/current')).length;
    await screen.findByTestId('job-banner');
    await waitFor(() => expect(count()).toBeGreaterThanOrEqual(2), { timeout: 5000 });
    unmount();
    const after = count();
    await new Promise((r) => setTimeout(r, 2600));
    expect(count()).toBe(after);
  }, 15_000);

  it('does not poll when nothing runs', async () => {
    const api = server({ current: session({ state: 'exported', exportCounts: COUNTS }) });
    open();
    await stepHeading(2);
    await new Promise((r) => setTimeout(r, 2500));
    expect(api.calls.filter((c) => c.url.pathname.endsWith('/sessions/current'))).toHaveLength(1);
  }, 10_000);

  it('cancels the running job', async () => {
    const j = job({ state: 'running', phase: 'issues', done: 1, total: 2 });
    const api = server({
      current: session({ state: 'exporting', jobs: { export: j, dryRun: null, import: null, verify: null } }),
      routes: {
        'POST /api/v1/orgs/:org/import/jobs/:job/cancel': () => ({ json: { ...j, cancelRequested: true } }),
      },
    });
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(posts(api, '/cancel')).toHaveLength(1));
    expect(posts(api, '/cancel')[0]!.url.pathname).toContain(j.id);
  });
});

describe('step 1: the Linear key', () => {
  const webStorage = () => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });

  function field() {
    return screen.findByLabelText('Linear API key') as Promise<HTMLInputElement>;
  }

  async function submitKey(user: ReturnType<typeof userEvent.setup>) {
    await user.type(await field(), KEY);
    await user.click(screen.getByRole('button', { name: 'Start the export' }));
  }

  function expectKeyNowhere(
    client: {
      getQueryCache: () => { getAll: () => unknown[] };
      getMutationCache: () => { getAll: () => unknown[] };
    },
    logs: unknown[][],
  ) {
    expect(document.body.textContent).not.toContain(KEY);
    expect(document.body.innerHTML).not.toContain(KEY);
    expect(webStorage()).not.toContain(KEY);
    expect(currentLocation.pathname + currentLocation.search).not.toContain(KEY);
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => (q as { state: unknown }).state),
      ),
    ).not.toContain(KEY);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(JSON.stringify(logs)).not.toContain(KEY);
  }

  it('is a password field with autocomplete off', async () => {
    server();
    open();
    const input = await field();
    expect(input).toHaveAttribute('type', 'password');
    expect(input).toHaveAttribute('autocomplete', 'off');
  });

  it('sends the key once, with no Idempotency-Key, and clears the field', async () => {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/import/sessions': () => ({ status: 201, json: session() }),
        'POST /api/v1/orgs/:org/import/sessions/:s/export': () => ({
          status: 202,
          json: job({ state: 'queued' }),
        }),
      },
    });
    const logs: unknown[][] = [];
    for (const m of ['log', 'info', 'warn', 'error', 'debug'] as const)
      vi.spyOn(console, m).mockImplementation((...a) => void logs.push(a));
    const user = userEvent.setup();
    const { client } = open();
    await submitKey(user);
    await waitFor(() => expect(posts(api, '/export')).toHaveLength(1));
    const call = posts(api, '/export')[0]!;
    expect(call.body).toEqual({ linearApiKey: KEY });
    expect(call.headers.get('Idempotency-Key')).toBeNull();
    expect(call.url.search).toBe('');
    expect((await field()).value).toBe('');
    expectKeyNowhere(client, logs);
  });

  it('shows no key after a 401, a linear_unauthorized and a network error', async () => {
    const cases: Array<[string, Handler | 'network', string]> = [
      [
        '401',
        () => ({ status: 401, json: { error: `bad token ${KEY}`, code: 'unauthenticated' } }),
        'signed out',
      ],
      [
        '502 linear_unauthorized',
        () => ({ status: 502, json: { error: `Linear said ${KEY} is wrong`, code: 'linear_unauthorized' } }),
        'Linear did not accept that key',
      ],
      [
        '502 linear_unreachable',
        () => ({ status: 502, json: { error: KEY, code: 'linear_unreachable' } }),
        'Could not reach Linear',
      ],
      ['network', 'network', 'Could not reach the server'],
    ];
    for (const [name, handler, text] of cases) {
      const logs: unknown[][] = [];
      for (const m of ['log', 'info', 'warn', 'error', 'debug'] as const)
        vi.spyOn(console, m).mockImplementation((...a) => void logs.push(a));
      const api = server({
        current: session({ exportCounts: {} }),
        routes: {
          'POST /api/v1/orgs/:org/import/sessions/:s/export':
            handler === 'network' ? () => undefined : handler,
        },
      });
      if (handler === 'network') {
        const real = globalThis.fetch;
        vi.stubGlobal(
          'fetch',
          vi.fn(async (input: Request | string, init?: RequestInit) => {
            const req = input instanceof Request ? input : new Request(input, init);
            if (req.method === 'POST' && req.url.endsWith('/export'))
              throw new TypeError(`failed to fetch with ${KEY}`);
            return real(input, init);
          }),
        );
      }
      const user = userEvent.setup();
      const { client, unmount } = open();
      await submitKey(user);
      if (name === '401') {
        // A 401 signs the person out: the login page replaces the wizard.
        await waitFor(() => expect(screen.queryByLabelText('Linear API key')).not.toBeInTheDocument());
      } else {
        expect(await screen.findByRole('alert')).toHaveTextContent(text);
        expect((await field()).value).toBe('');
      }
      expect(api.calls.length).toBeGreaterThan(0);
      expectKeyNowhere(client, logs);
      unmount();
      vi.restoreAllMocks();
    }
  });

  it('refuses an empty key and a key with spaces inside without a request', async () => {
    const api = server();
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: 'Start the export' }));
    expect(await screen.findByText('Paste the Linear API key.')).toBeInTheDocument();
    await user.type(await field(), 'ab cd');
    await user.click(screen.getByRole('button', { name: 'Start the export' }));
    expect(await screen.findByText(/characters that a Linear key does not have/)).toBeInTheDocument();
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('says why the session cannot start (no file store)', async () => {
    server({
      current: null,
      routes: {
        'POST /api/v1/orgs/:org/import/sessions': () => ({
          status: 503,
          json: { error: 'x', code: 'attachments_unavailable' },
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await submitKey(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('File storage is off on this server');
  });

  it('shows the failed-file count and the first 20 names', async () => {
    const names = Array.from({ length: 25 }, (_, i) => ({
      name: `file-${String(i + 1).padStart(2, '0')}.png`,
      reason: 'not found',
    }));
    server({
      current: session({
        state: 'exported',
        exportCounts: COUNTS,
        failedFiles: 25,
        failedFileNames: names,
        jobs: { export: job(), dryRun: null, import: null, verify: null },
      }),
    });
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: /1\. Export from Linear/ }));
    const box = await screen.findByTestId('failed-files');
    expect(box).toHaveTextContent('25 files could not be downloaded from Linear.');
    const items = within(box).getAllByRole('listitem');
    expect(items).toHaveLength(20);
    expect(items[0]).toHaveTextContent('file-01.png');
    expect(items[19]).toHaveTextContent('file-20.png');
    expect(box).not.toHaveTextContent('file-21.png');
  });

  it('shows a failed export in plain words', async () => {
    server({
      current: session({
        state: 'failed',
        jobs: {
          export: job({ state: 'failed', errorCode: 'no_space', errorText: 'RAW-TEXT-MARKER' }),
          dryRun: null,
          import: null,
          verify: null,
        },
      }),
    });
    open();
    expect(await screen.findByRole('alert')).toHaveTextContent('too little free disk space');
    expect(document.body.textContent).not.toContain('RAW-TEXT-MARKER');
  });
});

describe('step 2: the user map', () => {
  const exported = session({
    state: 'exported',
    exportCounts: COUNTS,
    jobs: { export: job(), dryRun: null, import: null, verify: null },
  });

  it('starts from the suggestions and saves the map', async () => {
    const api = server({
      current: exported,
      routes: { 'PUT /api/v1/orgs/:org/import/sessions/:s/usermap': (r) => ({ json: r.body }) },
    });
    const user = userEvent.setup();
    open();
    const chen = await screen.findByLabelText('Member for chen');
    expect(chen).toHaveValue('chen');
    expect(screen.getByLabelText('Member for sam')).toHaveValue('');
    await user.selectOptions(screen.getByLabelText('Default member for everyone else'), 'claude');
    await user.click(screen.getByRole('button', { name: 'Save the map and continue' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(api.calls.find((c) => c.method === 'PUT')!.body).toEqual({
      default: 'claude',
      users: { lu1: 'chen' },
    });
  });

  it('asks for a member or a default before it saves', async () => {
    const api = server({ current: exported });
    const user = userEvent.setup();
    open();
    await screen.findByLabelText('Member for sam');
    await user.click(screen.getByRole('button', { name: 'Save the map and continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('1 person has no member');
    expect(api.calls.some((c) => c.method === 'PUT')).toBe(false);
  });

  it('explains a map the server refuses', async () => {
    server({
      current: exported,
      routes: {
        'PUT /api/v1/orgs/:org/import/sessions/:s/usermap': () => ({
          status: 400,
          json: { error: 'RAW-TEXT-MARKER', code: 'validation_failed' },
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await user.selectOptions(await screen.findByLabelText('Default member for everyone else'), 'claude');
    await user.click(screen.getByRole('button', { name: 'Save the map and continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Every name must be a member of this organisation',
    );
    expect(document.body.textContent).not.toContain('RAW-TEXT-MARKER');
  });

  it('Create agent calls the agent route and selects the new username', async () => {
    const api = server({
      current: exported,
      routes: {
        'POST /api/v1/orgs/:org/agents': (r) => ({
          status: 201,
          json: {
            ...members[0],
            id: 'u9',
            ...(r.body as object),
            isAgent: true,
            email: null,
            role: 'member',
            createdAt: '2026-10-26T00:00:00Z',
            deactivatedAt: null,
          },
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await screen.findByLabelText('Member for sam');
    const row = screen.getByLabelText('Member for sam').closest('tr')!;
    await user.click(within(row).getByRole('button', { name: 'Create agent' }));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('Username'), 'sam-bot');
    await user.click(within(dialog).getByRole('button', { name: 'Create agent' }));
    await waitFor(() => expect(screen.getByLabelText('Member for sam')).toHaveValue('sam-bot'));
    expect(api.calls.find((c) => c.url.pathname.endsWith('/agents'))!.body).toEqual({
      username: 'sam-bot',
      name: 'sam',
    });
  });
});

describe('step 3: the dry run', () => {
  const ready = (over: Record<string, unknown> = {}) =>
    session({
      state: 'user_map',
      exportCounts: COUNTS,
      hasUserMap: true,
      jobs: { export: job(), dryRun: null, import: null, verify: null },
      ...over,
    });

  it('starts the dry run without acceptFailedFiles when no file failed', async () => {
    const api = server({
      current: ready(),
      routes: {
        'POST /api/v1/orgs/:org/import/sessions/:s/dry-run': () => ({
          status: 202,
          json: job({ kind: 'dry_run', state: 'queued' }),
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: 'Run the dry run' }));
    await waitFor(() => expect(posts(api, '/dry-run')).toHaveLength(1));
    expect(posts(api, '/dry-run')[0]!.body).toEqual({});
  });

  it('sends acceptFailedFiles: true only after the tick', async () => {
    const api = server({
      current: ready({
        failedFiles: 2,
        failedFileNames: [
          { name: 'a.png', reason: 'gone' },
          { name: 'b.png', reason: 'gone' },
        ],
      }),
      routes: {
        'POST /api/v1/orgs/:org/import/sessions/:s/dry-run': () => ({
          status: 202,
          json: job({ kind: 'dry_run', state: 'queued' }),
        }),
      },
    });
    const user = userEvent.setup();
    open();
    const button = await screen.findByRole('button', { name: 'Run the dry run' });
    expect(button).toBeDisabled();
    expect(screen.getByText('a.png')).toBeInTheDocument();
    await user.click(screen.getByRole('checkbox', { name: /I read the list of files that failed/ }));
    expect(button).toBeEnabled();
    await user.click(button);
    await waitFor(() => expect(posts(api, '/dry-run')).toHaveLength(1));
    expect(posts(api, '/dry-run')[0]!.body).toEqual({ acceptFailedFiles: true });
  });

  it('shows the counts, the exclusions and Uploads.Planned', async () => {
    server({
      current: session({
        state: 'dry_run_ok',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: null,
          verify: null,
        },
      }),
    });
    open();
    const box = await screen.findByTestId('dry-run-report');
    expect(within(box).getByRole('row', { name: /issues 120 2 118 0 0/ })).toBeInTheDocument();
    expect(screen.getByTestId('uploads-planned')).toHaveTextContent('A real run would upload 17 files.');
    expect(box).toHaveTextContent('2 records are left out');
    expect(box).toHaveTextContent('Nothing was written');
  });

  it('marks a report out of date after a map change', async () => {
    server({
      current: ready({
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: null,
          verify: null,
        },
      }),
    });
    open();
    expect(await screen.findByText(/This report is out of date/)).toBeInTheDocument();
  });

  it('says a 409 job_active in plain words', async () => {
    server({
      current: ready(),
      routes: {
        'POST /api/v1/orgs/:org/import/sessions/:s/dry-run': () => ({
          status: 409,
          json: { error: 'RAW', code: 'job_active' },
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: 'Run the dry run' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Another import job is running');
  });
});

describe('step 4: import and verify', () => {
  const dryOk = (over: Record<string, unknown> = {}, jobs: Record<string, unknown> = {}) =>
    session({
      state: 'dry_run_ok',
      exportCounts: COUNTS,
      hasUserMap: true,
      jobs: {
        export: job(),
        dryRun: job({ kind: 'dry_run', report: report() }),
        import: null,
        verify: null,
        ...jobs,
      },
      ...over,
    });

  it('shows the warnings and sends confirmOrg after the slug is typed', async () => {
    const api = server({
      current: dryOk(
        { state: 'failed' },
        {
          import: job({
            kind: 'import',
            state: 'canceled',
            errorCode: 'canceled',
            createdAt: '2026-10-26T11:00:00Z',
          }),
        },
      ),
      routes: {
        'POST /api/v1/orgs/:org/import/sessions/:s/run': () => ({
          status: 202,
          json: job({ kind: 'import', state: 'queued' }),
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await stepHeading(4);
    expect(screen.getByRole('note')).toHaveTextContent('A stopped import keeps no rows');
    expect(screen.getByRole('note')).toHaveTextContent(
      'Edits you made to imported rows in Launchpad are lost',
    );
    const start = screen.getByRole('button', { name: 'Start the import' });
    expect(start).toBeDisabled();
    await user.type(screen.getByLabelText(/Type the organisation slug/), 'orbi');
    expect(start).toBeDisabled();
    await user.type(screen.getByLabelText(/Type the organisation slug/), 't');
    expect(start).toBeEnabled();
    await user.click(start);
    await waitFor(() => expect(posts(api, '/run')).toHaveLength(1));
    expect(posts(api, '/run')[0]!.body).toEqual({ confirmOrg: 'orbit' });
  });

  it('explains dry_run_required from the server', async () => {
    server({
      current: dryOk(
        { state: 'failed' },
        {
          import: job({
            kind: 'import',
            state: 'failed',
            errorCode: 'import_failed',
            createdAt: '2026-10-26T11:00:00Z',
          }),
        },
      ),
      routes: {
        'POST /api/v1/orgs/:org/import/sessions/:s/run': () => ({
          status: 409,
          json: { error: 'x', code: 'dry_run_required' },
        }),
      },
    });
    const user = userEvent.setup();
    open();
    await stepHeading(4);
    await user.type(screen.getByLabelText(/Type the organisation slug/), 'orbit');
    await user.click(screen.getByRole('button', { name: 'Start the import' }));
    expect(await screen.findAllByRole('alert')).not.toHaveLength(0);
    expect(document.body.textContent).toContain('Run the dry run again first');
  });

  const mismatch = (committed: boolean) =>
    dryOk(
      { state: 'failed' },
      {
        import: job({
          kind: 'import',
          state: 'failed',
          errorCode: 'verify_mismatch',
          committed,
          createdAt: '2026-10-26T11:00:00Z',
          rows: [
            { entity: 'issues', expected: 120, actual: 119, rows: ['issue lin-9'] },
            { entity: 'comments', expected: 300, actual: 300 },
          ],
        }),
      },
    );

  it('says nothing was written for committed: false', async () => {
    server({ current: mismatch(false) });
    open();
    await stepHeading(4);
    expect(await screen.findByRole('alert')).toHaveTextContent('nothing was written');
    expect(document.body.textContent).not.toContain('The rows were written');
    const rows = screen.getByTestId('verify-rows');
    expect(within(rows).getByRole('row', { name: /issues 120 119 Differs/ })).toBeInTheDocument();
    expect(within(rows).getByRole('row', { name: /comments 300 300 Matches/ })).toBeInTheDocument();
  });

  it('says the rows were written for committed: true', async () => {
    server({ current: mismatch(true) });
    open();
    await stepHeading(4);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'The rows were written, but the check found a difference. Run verify again, or run the import again',
    );
    expect(screen.getByRole('button', { name: 'Run verify' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Run the import again' })).toBeInTheDocument();
  });

  it('runs verify and lists the rows', async () => {
    const verifyJob = job({ kind: 'verify', rows: [{ entity: 'issues', expected: 5, actual: 5 }] });
    const api = server({
      current: session({
        state: 'imported',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: job({ kind: 'import', committed: true }),
          verify: verifyJob,
        },
      }),
      routes: {
        'POST /api/v1/orgs/:org/import/sessions/:s/verify': () => ({ status: 202, json: verifyJob }),
      },
    });
    const user = userEvent.setup();
    open();
    await stepHeading(4);
    expect(screen.getByText('Verify passed: every count matches.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Run verify again' }));
    await waitFor(() => expect(posts(api, '/verify')).toHaveLength(1));
  });
});

describe('step 5 and the cycles guard', () => {
  it('shows the cycles switches after verify', async () => {
    server({
      current: session({
        state: 'verified',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: job({ kind: 'import', committed: true }),
          verify: job({ kind: 'verify' }),
        },
      }),
    });
    open();
    await stepHeading(5);
    expect(screen.getByRole('switch', { name: 'Cycles for ORB' })).toBeInTheDocument();
    expect(screen.getByText(/while an import session is in the export/)).toBeInTheDocument();
  });

  it('the Teams switch says "An import is running" on import_in_progress', async () => {
    server({
      routes: {
        'PATCH /api/v1/orgs/:org/teams/:team': () => ({
          status: 409,
          json: { error: 'RAW', code: 'import_in_progress' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('switch', { name: 'Cycles for ORB' }));
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('ORB: An import is running.');
    expect(screen.getByRole('switch', { name: 'Cycles for ORB' })).toHaveAttribute('aria-checked', 'false');
    expect(team.cyclesEnabled).toBe(false);
  });
});

describe('closing the import', () => {
  const exported = () =>
    session({
      state: 'exported',
      exportCounts: COUNTS,
      jobs: { export: job(), dryRun: null, import: null, verify: null },
    });
  const dels = (api: { calls: MockRequest[] }) => api.calls.filter((c) => c.method === 'DELETE');

  it('focuses Keep it; Escape and Keep it send no DELETE', async () => {
    const api = server({ current: exported() });
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: 'Close this import' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('button', { name: 'Keep it' })).toHaveFocus();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Close this import' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Keep it' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(dels(api)).toHaveLength(0);
  });

  it('Close the import sends DELETE for the session', async () => {
    const api = server({
      current: exported(),
      routes: { 'DELETE /api/v1/orgs/:org/import/sessions/:s': () => ({ status: 204 }) },
    });
    const user = userEvent.setup();
    open();
    await user.click(await screen.findByRole('button', { name: 'Close this import' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Close the import' }),
    );
    await waitFor(() => expect(dels(api)).toHaveLength(1));
    expect(dels(api)[0]!.url.pathname).toBe(`/api/v1/orgs/orbit/import/sessions/${SID}`);
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});

describe('stepper after the import ran', () => {
  it('turns steps 2 and 3 off with a reason', async () => {
    server({
      current: session({
        state: 'imported',
        exportCounts: COUNTS,
        hasUserMap: true,
        jobs: {
          export: job(),
          dryRun: job({ kind: 'dry_run', report: report() }),
          import: job({ kind: 'import', committed: true }),
          verify: null,
        },
      }),
    });
    open();
    await stepHeading(4);
    for (const n of [2, 3]) {
      const b = screen.getByRole('button', { name: new RegExp(`^${n}\\.`) });
      expect(b).toBeDisabled();
      expect(b).toHaveAccessibleDescription(/does not allow a new user map or dry run/);
    }
    expect(screen.getByRole('button', { name: /^5\./ })).toBeEnabled();
  });
});
