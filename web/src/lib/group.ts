import type { Issue, StateType, WorkflowState } from '../api/types';
import { PRIORITIES, priorityRank } from './priority';
import type { GroupKey, SortKey } from '../features/issues/filters';

const TYPE_ORDER: Record<StateType, number> = {
  triage: 0,
  backlog: 1,
  unstarted: 2,
  started: 3,
  completed: 4,
  canceled: 5,
};

export interface Group {
  key: string;
  name: string;
  type: StateType;
  color: string;
  issues: Issue[];
  /** LAUN-171: what the column stands for. Absent means a state (the list, and the board by default). */
  kind?: GroupKey;
  /** The value a drop here sets: an assignee's username, a priority number. `null`: clear it. */
  value?: string | number | null;
  /** A label's color, for the column header. */
  tint?: string;
}

/** Group by status name, ordered by state type then board position. */
export function groupByState(issues: Issue[], states: WorkflowState[], sort: SortKey): Group[] {
  const pos = new Map<string, number>();
  for (const s of states) pos.set(s.name, Math.min(pos.get(s.name) ?? Infinity, s.position));
  const map = new Map<string, Group>();
  for (const i of issues) {
    const key = i.state.name;
    let g = map.get(key);
    if (!g) {
      g = { key, name: i.state.name, type: i.state.type, color: i.state.color, issues: [] };
      map.set(key, g);
    }
    g.issues.push(i);
  }
  const groups = [...map.values()].sort(
    (a, b) =>
      TYPE_ORDER[a.type] - TYPE_ORDER[b.type] ||
      (pos.get(a.name) ?? 0) - (pos.get(b.name) ?? 0) ||
      a.name.localeCompare(b.name),
  );
  if (sort === 'priority') for (const g of groups) g.issues = sortByPriority(g.issues);
  return groups;
}

export function sortByPriority(issues: Issue[]): Issue[] {
  return [...issues].sort((a, b) => priorityRank(a.priority) - priorityRank(b.priority));
}

/** Board columns: every known state name (so empty columns show), plus any seen only on issues. */
export function boardColumns(issues: Issue[], states: WorkflowState[], sort: SortKey): Group[] {
  const groups = groupByState(issues, states, sort);
  const have = new Set(groups.map((g) => g.name));
  for (const s of states) {
    if (!have.has(s.name)) {
      have.add(s.name);
      groups.push({ key: s.name, name: s.name, type: s.type, color: s.color, issues: [] });
    }
  }
  const pos = new Map<string, number>();
  for (const s of states) pos.set(s.name, Math.min(pos.get(s.name) ?? Infinity, s.position));
  return groups.sort(
    (a, b) =>
      TYPE_ORDER[a.type] - TYPE_ORDER[b.type] ||
      (pos.get(a.name) ?? 0) - (pos.get(b.name) ?? 0) ||
      a.name.localeCompare(b.name),
  );
}

/** The key of the column an issue is in, for each way to group the board (LAUN-171). */
export const NONE_KEY = '~none';
export function groupKeyOf(kind: GroupKey, i: Issue): string {
  switch (kind) {
    case 'assignee':
      return i.assignee?.username ?? NONE_KEY;
    case 'priority':
      return String(i.priority);
    case 'label':
      // A card with several labels shows once, under its first label (a card must have one column to be dragged).
      return i.labels[0]?.name.toLowerCase() ?? NONE_KEY;
    case 'project':
      return i.projectId ?? NONE_KEY;
    default:
      return i.state.name;
  }
}

export interface GroupSources {
  states: WorkflowState[];
  members: { username: string; name: string }[];
  labels: { name: string; color: string }[];
  projects: { id: string; name: string }[];
}

/** Board columns for any group-by. Names sort A to Z; the "none" column is last (priority: the fixed order). */
export function boardGroups(kind: GroupKey, issues: Issue[], src: GroupSources, sort: SortKey): Group[] {
  if (kind === 'state') return boardColumns(issues, src.states, sort);
  const cols = new Map<string, Group>();
  const add = (g: Omit<Group, 'type' | 'color' | 'issues'> & { color?: string }) => {
    if (!cols.has(g.key))
      cols.set(g.key, { type: 'unstarted', color: g.color ?? '#8a8f98', issues: [], kind, ...g });
  };
  if (kind === 'priority') {
    for (const p of PRIORITIES) add({ key: String(p.value), name: p.label, value: p.value });
  } else if (kind === 'assignee') {
    for (const m of [...src.members].sort((a, b) => a.name.localeCompare(b.name)))
      add({ key: m.username, name: m.name, value: m.username });
  } else if (kind === 'label') {
    for (const l of [...src.labels].sort((a, b) => a.name.localeCompare(b.name)))
      add({ key: l.name.toLowerCase(), name: l.name, value: l.name, tint: l.color });
  } else {
    for (const p of [...src.projects].sort((a, b) => a.name.localeCompare(b.name)))
      add({ key: p.id, name: p.name, value: p.id });
  }
  const none = { assignee: 'Unassigned', label: 'No label', project: 'No project' }[kind as string];
  if (none) add({ key: NONE_KEY, name: none, value: null });
  for (const i of issues) {
    const key = groupKeyOf(kind, i);
    // A value the lists do not hold (a deactivated member, an archived project): still a column, so no card is lost.
    if (!cols.has(key)) {
      if (kind === 'assignee') add({ key, name: i.assignee?.name ?? key, value: key });
      else if (kind === 'label') add({ key, name: i.labels[0]?.name ?? key, value: i.labels[0]?.name });
      else add({ key, name: 'Other project', value: key });
    }
    cols.get(key)!.issues.push(i);
  }
  const out = [...cols.values()];
  if (kind !== 'priority') {
    // Put the "none" column last: it was added after the named ones, unseen values come after it.
    out.sort((a, b) => Number(a.key === NONE_KEY) - Number(b.key === NONE_KEY));
  }
  if (sort === 'priority') for (const g of out) g.issues = sortByPriority(g.issues);
  return out;
}
