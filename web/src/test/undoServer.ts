import type { Issue, WorkflowState } from '../api/types';
import { baseRoutes, labels, makeIssue, members, mockApi, states, team } from './mockApi';

// LAUN-236, LAUN-237: the fake server of the undo tests (board and list). It keeps versions (409 on a stale
// one), manual ranks and the fields the board and the list write (state, rank, priority, assignee, labels).

export const canceled: WorkflowState = {
  id: 's9',
  name: 'Canceled',
  type: 'canceled',
  color: '#95a2b3',
  description: '',
  position: 9,
  teamId: 'team1',
};
export const allStates = [...states, canceled];

/** The server's states: a test may rename one (`setServerStates`). A ref is a name or an id, as the API takes. */
let serverStates: WorkflowState[] = allStates;
export const setServerStates = (s: WorkflowState[]) => (serverStates = s);
export const ref = (nameOrId: string) => {
  const s = serverStates.find((x) => x.id === nameOrId || x.name === nameOrId)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
export const card = (n: number, state: string, over: Partial<Issue> = {}) =>
  makeIssue(n, { state: ref(state), title: `Card ${n}`, sortOrder: n * 1024, ...over });

type Reply = { status: number; json: unknown } | undefined;

export function undoServer(
  initial: Issue[],
  opts: {
    patch?: (body: Record<string, unknown>, n: number, issue: string) => Reply | Promise<Reply>;
    /** Runs before each list read: a test can hold the refetches back. */
    list?: (n: number) => Promise<void> | void;
    /** The comments of each issue, by id or identifier (the undo of a create asks for them). */
    comments?: Record<string, unknown[]>;
    /** Runs before the reads of the undo of a create (comments, sub-issues): a reply here wins. */
    precheck?: (what: 'comments' | 'parent') => Reply;
  } = {},
) {
  const rows = initial.map((i) => ({ ...i }));
  let n = 0;
  let reads = 0;
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/teams': () => ({
      json: { items: [team, { ...team, id: 'team2', key: 'WEB', name: 'Web' }], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/teams/:team/states': () => ({ json: { items: serverStates, nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues/:issue/comments': (r) => {
      const bad = opts.precheck?.('comments');
      if (bad) return bad;
      const id = r.params.issue!;
      // As the real server: a deleted or hidden issue is 404.
      if (!rows.some((x) => x.id === id || x.identifier === id))
        return { status: 404, json: { error: 'issue not found', code: 'not_found' } };
      return { json: { items: opts.comments?.[id] ?? [], nextCursor: null } };
    },
    'GET /api/v1/orgs/:org/issues': async (r) => {
      // The sub-issues of one issue (the undo of a create asks): not a list read of the page.
      const parent = r.url.searchParams.get('parent');
      if (parent) {
        const bad = opts.precheck?.('parent');
        if (bad) return bad;
        // As the real server: an unknown (deleted or hidden) parent is 400, not 404.
        if (!rows.some((x) => x.id === parent || x.identifier === parent))
          return { status: 400, json: { error: `unknown parent ${parent}`, code: 'validation_failed' } };
        return {
          json: {
            items: rows.filter((x) => x.parent && (x.parent.id === parent || x.parent.identifier === parent)),
            nextCursor: null,
          },
        };
      }
      await opts.list?.(reads++);
      return { json: { items: [...rows].sort((a, b) => a.sortOrder - b.sortOrder), nextCursor: null } };
    },
    'POST /api/v1/orgs/:org/issues': (r) => {
      const body = r.body as { title: string; state?: string };
      const i = makeIssue(90, {
        title: body.title,
        identifier: 'ORB-90',
        state: ref(body.state ?? 'Backlog'),
        sortOrder: -1024,
      });
      rows.push(i);
      return { status: 201, json: i };
    },
    'PATCH /api/v1/orgs/:org/issues/:issue': async (r) => {
      const body = r.body as Record<string, unknown>;
      const bad = await opts.patch?.(body, n++, r.params.issue!);
      if (bad) return bad;
      const i = rows.find((x) => x.identifier === r.params.issue);
      if (!i) return { status: 404, json: { error: 'issue not found', code: 'not_found' } };
      if (body.version !== i.version)
        return { status: 409, json: { error: 'the issue changed', code: 'version_conflict' } };
      if (typeof body.state === 'string') {
        const to = ref(body.state);
        // As the real server (T14): a column change with no place goes on top of the new column; an empty
        // column's top is rank 0.
        if (to.id !== i.state.id && body.sortOrder === undefined && !body.placeAfter && !body.placeBefore) {
          const ranks = rows.filter((x) => x !== i && x.state.id === to.id).map((x) => x.sortOrder);
          i.sortOrder = ranks.length ? Math.min(...ranks) - 1024 : 0;
        }
        i.state = to;
      }
      if (typeof body.sortOrder === 'number') i.sortOrder = body.sortOrder;
      const nb = rows.find((x) => x.id === (body.placeAfter ?? body.placeBefore));
      if (nb) i.sortOrder = nb.sortOrder + (body.placeAfter ? 1 : -1);
      if (typeof body.priority === 'number') i.priority = body.priority;
      if (body.assignee !== undefined) {
        const m = members.find((x) => x.username === body.assignee);
        i.assignee = m ? { id: m.id, username: m.username, name: m.name } : null;
      }
      const label = (x: string) => labels.find((l) => l.id === x || l.name === x)!;
      for (const x of (body.addLabels as string[] | undefined) ?? []) {
        const l = label(x);
        if (!i.labels.some((y) => y.id === l.id))
          i.labels = [...i.labels, { id: l.id, name: l.name, color: l.color }];
      }
      for (const x of (body.removeLabels as string[] | undefined) ?? [])
        i.labels = i.labels.filter((y) => y.id !== label(x).id);
      i.version += 1;
      return { json: { ...i } };
    },
  });
  const patches = () => api.calls.filter((c) => c.method === 'PATCH').map((c) => c.body);
  return { api, rows, patches };
}
