import type { Project } from '../../api/types';

export type ProjectState = Project['state'];

/** Order of the list: live work first, finished last. */
const ORDER: ProjectState[] = ['started', 'planned', 'backlog', 'paused', 'completed', 'canceled'];

export function sortProjects(list: Project[]): Project[] {
  return [...list].sort(
    (a, b) =>
      ORDER.indexOf(a.state) - ORDER.indexOf(b.state) ||
      a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }),
  );
}

/** A date-only value (`2026-11-03`) as "3 Nov 2026" without a time zone shift. */
export function formatDay(day: string | null | undefined): string {
  if (!day) return '';
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (!m) return day;
  const d = new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3])));
  return d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });
}

/** True when the target day is before `today` (a `YYYY-MM-DD` string) and the project is still open. */
export function isOverdue(p: Pick<Project, 'targetDate' | 'state'>, today: string): boolean {
  if (!p.targetDate) return false;
  if (p.state === 'completed' || p.state === 'canceled') return false;
  return p.targetDate < today;
}

export const todayString = (now: number = Date.now()): string => {
  const d = new Date(now);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
};

export function pct(done: number, scope: number): number {
  return scope <= 0 ? 0 : Math.min(100, Math.round((done / scope) * 100));
}

/** A product as grouping needs it: its name, slug and the keys of its teams. */
export interface GroupProduct {
  slug: string;
  name: string;
  teamKeys: string[];
}

export interface ProjectGroup {
  /** The product slug, or `''` for the "No product" bucket. */
  id: string;
  name: string;
  projects: Project[];
}

export const NO_PRODUCT = 'No product';

/**
 * LAUN-166: group projects by product. A team may be in several products and a project may span teams, so the rule
 * is: a project goes under the ONE product that holds the MOST of its teams (a tie goes to the first by name, then
 * slug). A project none of whose teams is in a product goes under "No product", last. A project appears once. The
 * order inside a group is the order of `list` (the caller sorts first). A product with no project is left out.
 */
export function groupByProduct(list: Project[], products: GroupProduct[]): ProjectGroup[] {
  const ordered = [...products].sort(
    (a, b) =>
      a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) || a.slug.localeCompare(b.slug),
  );
  const buckets = new Map<string, Project[]>();
  const none: Project[] = [];
  for (const p of list) {
    const keys = new Set(p.teams.map((t) => t.key));
    let best: GroupProduct | null = null;
    let bestN = 0;
    for (const pr of ordered) {
      const n = pr.teamKeys.filter((k) => keys.has(k)).length;
      if (n > bestN) {
        best = pr;
        bestN = n;
      }
    }
    if (!best) none.push(p);
    else buckets.set(best.slug, [...(buckets.get(best.slug) ?? []), p]);
  }
  const out: ProjectGroup[] = ordered
    .filter((pr) => buckets.has(pr.slug))
    .map((pr) => ({ id: pr.slug, name: pr.name, projects: buckets.get(pr.slug)! }));
  if (none.length > 0) out.push({ id: '', name: NO_PRODUCT, projects: none });
  return out;
}

/** Kind order of the list and the sort: live work first, finished last. */
export const KIND_ORDER = ORDER;
