export type SortKey = 'updated' | 'created' | 'priority' | 'manual';
export type ViewKey = 'list' | 'board';
/** LAUN-171: what the board's columns are made of. Not a filter: it is part of the view, like the sort. */
export type GroupKey = 'state' | 'assignee' | 'priority' | 'label' | 'project';
export const GROUP_KEYS: readonly GroupKey[] = ['state', 'assignee', 'priority', 'label', 'project'];
export const GROUP_NAMES: Record<GroupKey, string> = {
  state: 'Status',
  assignee: 'Assignee',
  priority: 'Priority',
  label: 'Label',
  project: 'Project',
};

export interface IssueFilters {
  team?: string;
  /** A product slug (LAUN-64): the issues of all its teams. */
  product?: string;
  state: string[];
  assignee?: string;
  /** A project id, or `none` for the issues in no project (LAUN-179). One value, like assignee. */
  project?: string;
  label: string[];
  priority?: number;
  q: string;
  sort: SortKey;
  view: ViewKey;
  /** LAUN-171: the board's column source. Default `state`. */
  group: GroupKey;
}

export const emptyFilters: IssueFilters = {
  state: [],
  label: [],
  q: '',
  sort: 'updated',
  view: 'list',
  group: 'state',
};

/** The sort a view starts with (LAUN-110): the board is the manual order, the list shows the newest first. */
export const defaultSort = (view: ViewKey): SortKey => (view === 'board' ? 'manual' : 'updated');

const list = (v: string | null): string[] =>
  v
    ? v
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
    : [];

export function parseFilters(sp: URLSearchParams): IssueFilters {
  const p = sp.get('priority');
  const pr = p === null || p === '' ? undefined : Number(p);
  const sort = sp.get('sort');
  const view: ViewKey = sp.get('view') === 'board' ? 'board' : 'list';
  return {
    team: sp.get('team') || undefined,
    product: sp.get('product') || undefined,
    state: list(sp.get('state')),
    assignee: sp.get('assignee') || undefined,
    project: sp.get('project') || undefined,
    label: list(sp.get('label')),
    priority: pr !== undefined && Number.isInteger(pr) && pr >= 0 && pr <= 4 ? pr : undefined,
    q: sp.get('q') ?? '',
    sort:
      sort === 'created' || sort === 'priority' || sort === 'manual' || sort === 'updated'
        ? sort
        : defaultSort(view),
    view,
    group: GROUP_KEYS.find((g) => g === sp.get('group')) ?? 'state',
  };
}

export function writeFilters(f: IssueFilters): URLSearchParams {
  const sp = new URLSearchParams();
  if (f.team) sp.set('team', f.team);
  if (f.product) sp.set('product', f.product);
  if (f.state.length) sp.set('state', f.state.join(','));
  if (f.assignee) sp.set('assignee', f.assignee);
  if (f.project) sp.set('project', f.project);
  if (f.label.length) sp.set('label', f.label.join(','));
  if (f.priority !== undefined) sp.set('priority', String(f.priority));
  if (f.q) sp.set('q', f.q);
  if (f.sort !== defaultSort(f.view)) sp.set('sort', f.sort);
  if (f.view !== 'list') sp.set('view', f.view);
  if (f.group !== 'state') sp.set('group', f.group);
  return sp;
}

/** Only parameters the server knows: it answers 400 to an unknown one. */
export interface IssueApiQuery {
  team?: string;
  /** A product slug or id; combines with `team` as AND (LAUN-62). */
  product?: string;
  state?: string;
  assignee?: string;
  label?: string;
  priority?: number;
  query?: string;
  /** `manual` is the stored board order (LAUN-51); the board asks for it with sort Manual (its default). */
  orderBy?: 'updatedAt' | 'createdAt' | 'manual';
  parent?: string;
  /** A cycle number (with `team`), `active`, or a cycle id. */
  cycle?: string;
  /** A project id (LAUN-119): the project page's issues. */
  project?: string;
}

export function toApiQuery(f: IssueFilters, extra: Partial<IssueApiQuery> = {}): IssueApiQuery {
  const q: IssueApiQuery = {};
  if (f.team) q.team = f.team;
  if (f.product) q.product = f.product;
  if (f.state.length) q.state = f.state.join(',');
  if (f.assignee) q.assignee = f.assignee;
  if (f.project) q.project = f.project;
  if (f.label.length) q.label = f.label.join(',');
  if (f.priority !== undefined) q.priority = f.priority;
  if (f.q.trim()) q.query = f.q.trim();
  // LAUN-110: the board follows the sort like the list does. Only "manual" lets a card be dragged.
  // (`priority` is sorted in the page, the server has no such order: it sends the newest first.)
  q.orderBy = f.sort === 'manual' ? 'manual' : f.sort === 'created' ? 'createdAt' : 'updatedAt';
  return { ...q, ...extra };
}

export function activeFilterCount(f: IssueFilters): number {
  return (
    (f.product ? 1 : 0) +
    (f.state.length ? 1 : 0) +
    (f.assignee ? 1 : 0) +
    (f.project ? 1 : 0) +
    (f.label.length ? 1 : 0) +
    (f.priority !== undefined ? 1 : 0) +
    (f.q ? 1 : 0)
  );
}

/** Client-side presets (saved filters). Real saved views arrive with a server table. */
export const presets: { id: string; label: string; search: string }[] = [
  { id: 'active', label: 'Active', search: 'state=In Progress,In Review,In Testing' },
  { id: 'backlog', label: 'Backlog', search: 'state=Backlog,Todo' },
  { id: 'urgent', label: 'Urgent', search: 'priority=1' },
];
