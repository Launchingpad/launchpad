import { vi } from 'vitest';
import type { Issue, Comment, Team, WorkflowState, Label, Member, ProjectStatus } from '../api/types';

export interface MockRequest {
  url: URL;
  method: string;
  body: unknown;
  headers: Headers;
  params: Record<string, string>;
}
export interface MockReply {
  status?: number;
  json?: unknown;
  headers?: Record<string, string>;
}
export type Handler = (req: MockRequest) => MockReply | Promise<MockReply> | undefined;

function compile(pattern: string): { re: RegExp; names: string[] } {
  const names: string[] = [];
  const src = pattern.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/:([a-zA-Z]+)/g, (_m, n: string) => {
    names.push(n);
    return '([^/]+)';
  });
  return { re: new RegExp(`^${src}$`), names };
}

/** Replace global fetch with a tiny router. Keys look like `GET /api/v1/orgs/:org/issues`. */
export function mockApi(routes: Record<string, Handler>) {
  const compiled = Object.entries(routes).map(([k, h]) => {
    const [method, path] = k.split(' ') as [string, string];
    return { method, ...compile(path!), h };
  });
  const calls: MockRequest[] = [];
  const fn = vi.fn(async (input: Request | string, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(input, init);
    const url = new URL(req.url);
    const text = req.method === 'GET' || req.method === 'HEAD' ? '' : await req.text();
    let body: unknown = undefined;
    try {
      body = text ? JSON.parse(text) : undefined;
    } catch {
      body = text;
    }
    for (const c of compiled) {
      if (c.method !== req.method) continue;
      const m = c.re.exec(url.pathname);
      if (!m) continue;
      const params = Object.fromEntries(c.names.map((n, i) => [n, decodeURIComponent(m[i + 1]!)]));
      const mr: MockRequest = { url, method: req.method, body, headers: req.headers, params };
      calls.push(mr);
      const reply = (await c.h(mr)) ?? {};
      const status = reply.status ?? 200;
      const noBody = status === 204 || (status === 202 && reply.json === undefined);
      return new Response(noBody ? null : JSON.stringify(reply.json ?? {}), {
        status,
        headers: { 'Content-Type': 'application/json', ...reply.headers },
      });
    }
    calls.push({ url, method: req.method, body, headers: req.headers, params: {} });
    return new Response(
      JSON.stringify({ error: `no mock for ${req.method} ${url.pathname}`, code: 'not_found' }),
      {
        status: 404,
        headers: { 'Content-Type': 'application/json' },
      },
    );
  });
  vi.stubGlobal('fetch', fn);
  return { calls, fn };
}

// ---------- fixtures ----------
export const ME = {
  id: 'u1',
  username: 'chen',
  name: 'Chen',
  isAgent: false,
  isPlatformAdmin: false,
  tokenId: 't',
  tokenKind: 'session',
  scopes: ['read', 'write'],
  pinnedOrgId: null,
};
export const team: Team = {
  id: 'team1',
  key: 'ORB',
  name: 'Orbit',
  description: '',
  defaultStateId: 's1',
  reviewStateId: null,
  testingStateId: null,
  cyclesEnabled: false,
  cycleLengthDays: 14,
  cycleStartWeekday: 0,
  cycleStartTime: '00:00',
  cycleTimezone: 'Europe/London',
  cycleRollover: true,
  productIds: [],
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};
/** The six default project statuses every org starts with (LAUN-195). Ids are `ps-<kind>`. */
const DEFAULT_STATUS: [ProjectStatus['kind'], string, string][] = [
  ['backlog', 'Backlog', '#bec2c8'],
  ['planned', 'Planned', '#e2e2e2'],
  ['started', 'In progress', '#f2c94c'],
  ['paused', 'Paused', '#95a2b3'],
  ['completed', 'Completed', '#5e6ad2'],
  ['canceled', 'Canceled', '#95a2b3'],
];
export const projectStatuses: ProjectStatus[] = DEFAULT_STATUS.map(([kind, name, color], position) => ({
  id: `ps-${kind}`,
  kind,
  name,
  color,
  position,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
}));
/** The `status` of a project in `kind`, for test fixtures. */
export const statusRef = (kind: ProjectStatus['kind']) => {
  const s = projectStatuses.find((x) => x.kind === kind)!;
  return { id: s.id, kind: s.kind, name: s.name, color: s.color };
};
export const states: WorkflowState[] = [
  {
    id: 's1',
    name: 'Backlog',
    type: 'backlog',
    color: '#bec2c8',
    description: '',
    position: 0,
    teamId: 'team1',
  },
  {
    id: 's2',
    name: 'Todo',
    type: 'unstarted',
    color: '#e2e2e2',
    description: '',
    position: 1,
    teamId: 'team1',
  },
  {
    id: 's3',
    name: 'In Progress',
    type: 'started',
    color: '#f2c94c',
    description: '',
    position: 2,
    teamId: 'team1',
  },
  {
    id: 's4',
    name: 'Done',
    type: 'completed',
    color: '#5e6ad2',
    description: '',
    position: 3,
    teamId: 'team1',
  },
];
export const labels: Label[] = [
  {
    id: 'l1',
    name: 'bug',
    color: '#d6481f',
    description: '',
    parentId: null,
    teamId: null,
    teamKey: null,
    issueCount: 0,
    createdAt: '2026-01-01T00:00:00Z',
  },
];
export const members: Member[] = [
  {
    id: 'u1',
    username: 'chen',
    name: 'Chen',
    email: null,
    isAgent: false,
    role: 'admin',
    createdAt: '2026-01-01T00:00:00Z',
    deactivatedAt: null,
    teamAccess: 'all',
    teams: [],
  },
  {
    id: 'u2',
    username: 'claude',
    name: 'Claude',
    email: null,
    isAgent: true,
    role: 'member',
    createdAt: '2026-01-01T00:00:00Z',
    deactivatedAt: null,
    teamAccess: 'all',
    teams: [],
  },
];

export function makeIssue(n: number, over: Partial<Issue> = {}): Issue {
  const st = states[0]!;
  return {
    id: `i${n}`,
    identifier: `ORB-${n}`,
    number: n,
    title: `Issue ${n}`,
    description: `Description ${n}`,
    descriptionTruncated: false,
    priority: 0,
    priorityLabel: 'No priority',
    estimate: null,
    team: { id: 'team1', key: 'ORB', name: 'Orbit' },
    state: { id: st.id, name: st.name, type: st.type, color: st.color },
    assignee: null,
    creator: { id: 'u1', username: 'chen', name: 'Chen' },
    labels: [],
    parent: null,
    projectId: null,
    milestoneId: null,
    cycleId: null,
    dueDate: null,
    sortOrder: n * 1024,
    gitBranchName: `chen/orb-${n}-issue`,
    version: 1,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-02T00:00:00Z',
    startedAt: null,
    completedAt: null,
    canceledAt: null,
    archivedAt: null,
    ...over,
  };
}

export function makeComment(id: string, over: Partial<Comment> = {}): Comment {
  return {
    id,
    issueId: 'i1',
    documentId: null,
    parentId: null,
    body: `Comment ${id}`,
    author: { id: 'u1', username: 'chen', name: 'Chen' },
    createdAt: '2026-10-02T00:00:00Z',
    updatedAt: '2026-10-02T00:00:00Z',
    editedAt: null,
    ...over,
  };
}

const page = <T>(items: T[]) => ({ items, nextCursor: null });

/** The reference-data routes every screen needs, for one org. */
export function baseRoutes(issues: Issue[] = []): Record<string, Handler> {
  return {
    'GET /api/v1/me': () => ({ json: ME }),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: ME.id,
          username: ME.username,
          name: ME.name,
          email: 'chen@example.com',
          isPlatformAdmin: false,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs': () => ({
      json: page([
        { id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin', createdAt: '2026-01-01T00:00:00Z' },
      ]),
    }),
    'GET /api/v1/orgs/:org/me/views': () => ({ json: { items: [] } }),
    'GET /api/v1/me/preferences': () => ({ json: { theme: 'linear' } }),
    'GET /api/v1/orgs/:org/teams': () => ({ json: page([team]) }),
    'GET /api/v1/orgs/:org/teams/:team/states': () => ({ json: page(states) }),
    'GET /api/v1/orgs/:org/labels': () => ({ json: page(labels) }),
    'GET /api/v1/orgs/:org/project-statuses': () => ({ json: page(projectStatuses) }),
    'GET /api/v1/orgs/:org/users': () => ({ json: page(members) }),
    'GET /api/v1/orgs/:org/issues': () => ({ json: page(issues) }),
    'GET /api/v1/orgs/:org/issues/:issue': (r) => {
      const found = issues.find((i) => i.identifier === r.params.issue);
      return found ? { json: found } : { status: 404, json: { error: 'issue not found', code: 'not_found' } };
    },
    'GET /api/v1/orgs/:org/issues/:issue/comments': () => ({ json: page([]) }),
    'GET /api/v1/orgs/:org/issues/:issue/activity': () => ({ json: page([]) }),
  };
}

/** Give a project fixture the `status` of its `state` (LAUN-195), unless it already has one. */
export function projectOf<T extends { state: ProjectStatus['kind'] }>(
  p: T,
): T & {
  status: ReturnType<typeof statusRef>;
  updateSchedule: { frequency: 'none'; weekday: number; hour: number };
} {
  return { status: statusRef(p.state), updateSchedule: { frequency: 'none', weekday: 5, hour: 9 }, ...p };
}
