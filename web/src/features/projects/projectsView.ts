import type { Project } from '../../api/types';
import { isOverdue, KIND_ORDER } from './projects';

/** LAUN-165: the pure parts of the projects table: health, filters, sort and the remembered view. */

export type Health = 'off' | 'risk' | 'on' | 'none';

export const HEALTH_LABEL: Record<Health, string> = {
  off: 'Off track',
  risk: 'At risk',
  on: 'On track',
  none: 'No data',
};
const HEALTH_RANK: Record<Health, number> = { off: 0, risk: 1, on: 2, none: 3 };

/** `day` (YYYY-MM-DD) plus `n` days, without a time zone shift. */
export function addDays(day: string, n: number): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (!m) return day;
  const d = new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + n));
  return d.toISOString().slice(0, 10);
}

/**
 * The server stores no health, so the table derives one from state and dates only (decisions LAUN-165 #3):
 * a finished project has none; a past target is "Off track"; a paused project, or one not started with a target
 * within 14 days, is "At risk"; an open project with a target is "On track"; the rest have no data.
 */
export function healthOf(p: Pick<Project, 'state' | 'targetDate'>, today: string): Health {
  if (p.state === 'completed' || p.state === 'canceled') return 'none';
  if (isOverdue(p, today)) return 'off';
  if (p.state === 'paused') return 'risk';
  if (!p.targetDate) return 'none';
  if ((p.state === 'planned' || p.state === 'backlog') && p.targetDate <= addDays(today, 14)) return 'risk';
  return 'on';
}

export type ColumnId = 'labels' | 'health' | 'priority' | 'lead' | 'target' | 'status' | 'progress';
export const COLUMNS: { id: ColumnId; label: string }[] = [
  { id: 'labels', label: 'Labels' },
  { id: 'health', label: 'Health' },
  { id: 'priority', label: 'Priority' },
  { id: 'lead', label: 'Lead' },
  { id: 'target', label: 'Target date' },
  { id: 'status', label: 'Status' },
  { id: 'progress', label: 'Progress' },
];
/** Priority has no field on a project yet, so it starts hidden. */
export const DEFAULT_COLUMNS: ColumnId[] = ['labels', 'health', 'lead', 'target', 'status', 'progress'];

export type SortKey = 'name' | 'health' | 'lead' | 'target' | 'status';
export const SORT_LABEL: Record<SortKey, string> = {
  name: 'Name',
  health: 'Health',
  lead: 'Lead',
  target: 'Target date',
  status: 'Status',
};
export type SortDir = 'asc' | 'desc';

export interface ProjectFilters {
  q: string;
  /** Project status ids (LAUN-195). A stored id of a deleted status is dropped when the statuses load. */
  states: string[];
  /** A username, or `none` for no lead. */
  lead?: string;
  /** Team keys; a project matches when it has any of them. */
  teams: string[];
  /** A product slug. */
  product?: string;
  /**
   * Project label ids (LAUN-194); a project matches when it has any of them, or a label in a chosen group. The page
   * keeps them in the address (`?label=id,id`), not in the remembered view.
   */
  labels: string[];
}
export const emptyProjectFilters: ProjectFilters = { q: '', states: [], teams: [], labels: [] };

export function activeProjectFilters(f: ProjectFilters): number {
  return (
    (f.q.trim() ? 1 : 0) +
    (f.states.length ? 1 : 0) +
    (f.lead ? 1 : 0) +
    (f.teams.length ? 1 : 0) +
    (f.product ? 1 : 0) +
    (f.labels.length ? 1 : 0)
  );
}

export function filterProjects(
  list: Project[],
  f: ProjectFilters,
  productTeamKeys: Map<string, string[]>,
): Project[] {
  const q = f.q.trim().toLowerCase();
  const prodKeys = f.product
    ? new Set((productTeamKeys.get(f.product) ?? []).map((k) => k.toLowerCase()))
    : null;
  const teamSet = new Set(f.teams.map((k) => k.toLowerCase()));
  const labelSet = new Set(f.labels);
  return list.filter((p) => {
    if (q && !p.name.toLowerCase().includes(q)) return false;
    if (f.states.length && !f.states.includes(p.status.id)) return false;
    if (f.lead === 'none' ? p.lead !== null : f.lead ? p.lead?.username !== f.lead : false) return false;
    const keys = p.teams.map((t) => t.key.toLowerCase());
    if (teamSet.size && !keys.some((k) => teamSet.has(k))) return false;
    if (prodKeys && !keys.some((k) => prodKeys.has(k))) return false;
    if (
      labelSet.size &&
      !p.labels.some((l) => labelSet.has(l.id) || (l.parentId && labelSet.has(l.parentId)))
    )
      return false;
    return true;
  });
}

const STATE_ORDER = KIND_ORDER;
const byName = (a: Project, b: Project) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });

/**
 * Sort by a column. A missing value (no lead, no target date) is last in BOTH directions, and a tie falls back to
 * the name (always ascending), so the order is stable. `today` feeds the health sort.
 */
export function sortBy(list: Project[], key: SortKey, dir: SortDir, today: string): Project[] {
  const sign = dir === 'asc' ? 1 : -1;
  const missing = (p: Project): boolean =>
    (key === 'lead' && !p.lead) ||
    (key === 'target' && !p.targetDate) ||
    (key === 'health' && healthOf(p, today) === 'none');
  const cmp = (a: Project, b: Project): number => {
    switch (key) {
      case 'name':
        return byName(a, b);
      case 'status':
        return (
          STATE_ORDER.indexOf(a.state) - STATE_ORDER.indexOf(b.state) ||
          a.status.name.localeCompare(b.status.name, undefined, { sensitivity: 'base' })
        );
      case 'health':
        return HEALTH_RANK[healthOf(a, today)] - HEALTH_RANK[healthOf(b, today)];
      case 'lead':
        return (a.lead?.name ?? '').localeCompare(b.lead?.name ?? '', undefined, { sensitivity: 'base' });
      case 'target':
        return (a.targetDate ?? '').localeCompare(b.targetDate ?? '');
    }
  };
  return [...list].sort((a, b) => {
    const ma = missing(a);
    const mb = missing(b);
    if (ma !== mb) return ma ? 1 : -1;
    const c = ma ? 0 : cmp(a, b);
    return (c === 0 ? 0 : c * sign) || (key === 'name' ? sign * byName(a, b) : byName(a, b));
  });
}

/** The remembered view of the projects page (per browser). */
export interface ProjectView {
  sort: SortKey;
  dir: SortDir;
  columns: ColumnId[];
  filters: ProjectFilters;
}
export const VIEW_KEY = 'launchpad.projects.view.v1';
export const defaultView = (): ProjectView => ({
  sort: 'status',
  dir: 'asc',
  columns: [...DEFAULT_COLUMNS],
  filters: { ...emptyProjectFilters },
});

const isStr = (v: unknown): v is string => typeof v === 'string';
const strList = (v: unknown, max = 50): string[] => (Array.isArray(v) ? v.filter(isStr).slice(0, max) : []);

/** Read a stored view; anything wrong falls back field by field to the default. Never throws. */
export function parseView(raw: string | null): ProjectView {
  const d = defaultView();
  if (!raw) return d;
  try {
    const o: unknown = JSON.parse(raw);
    if (!o || typeof o !== 'object') return d;
    const v = o as Record<string, unknown>;
    const sorts = Object.keys(SORT_LABEL);
    const colIds = COLUMNS.map((c) => c.id) as string[];
    const f = (v.filters && typeof v.filters === 'object' ? v.filters : {}) as Record<string, unknown>;
    return {
      sort: isStr(v.sort) && sorts.includes(v.sort) ? (v.sort as SortKey) : d.sort,
      dir: v.dir === 'desc' ? 'desc' : 'asc',
      columns: Array.isArray(v.columns)
        ? (strList(v.columns).filter((c) => colIds.includes(c)) as ColumnId[])
        : d.columns,
      filters: {
        q: isStr(f.q) ? f.q.slice(0, 200) : '',
        states: strList(f.states),
        lead: isStr(f.lead) && f.lead ? f.lead : undefined,
        teams: strList(f.teams),
        product: isStr(f.product) && f.product ? f.product : undefined,
        labels: [], // not remembered: the address holds them
      },
    };
  } catch {
    return d;
  }
}

export function readView(): ProjectView {
  try {
    return parseView(localStorage.getItem(VIEW_KEY));
  } catch {
    return defaultView();
  }
}

export function writeView(v: ProjectView): void {
  try {
    // The search text is not remembered: a stale search hides rows on the next visit.
    localStorage.setItem(VIEW_KEY, JSON.stringify({ ...v, filters: { ...v.filters, q: '', labels: [] } }));
  } catch {
    /* not remembered */
  }
}
