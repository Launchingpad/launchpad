import type { Product, Team } from '../../api/types';

/** The id of the last group, "Other teams". It is not a product, so it has no product id. */
export const OTHER_ID = 'other';

/** Above this many team rows (every group open), groups without the active team start collapsed (T18 #22). */
export const AUTO_COLLAPSE_ROWS = 12;

export interface NavGroup {
  /** The product id, or `OTHER_ID`. The collapse state is stored under it. */
  id: string;
  name: string;
  /** The product slug (its page is `/:org/product/:slug`); undefined for "Other teams". */
  slug?: string;
  icon: string | null;
  color: string | null;
  teams: Team[];
}

/**
 * The sidebar groups (T18 #7, #8, #22), or null when the sidebar stays the flat list.
 * A team shows under every non-archived product it is in; a team in none shows under "Other teams".
 * A product with no team to show has no group. With no group at all the list is flat, as before products.
 */
export function buildGroups(teams: Team[], products: Product[]): NavGroup[] | null {
  const live = products.filter((p) => p.archivedAt === null);
  const groups: NavGroup[] = [];
  for (const p of live) {
    const members = teams.filter((t) => t.productIds.includes(p.id));
    if (members.length)
      groups.push({ id: p.id, name: p.name, slug: p.slug, icon: p.icon, color: p.color, teams: members });
  }
  if (!groups.length) return null;
  const grouped = new Set(groups.map((g) => g.id));
  const rest = teams.filter((t) => !t.productIds.some((id) => grouped.has(id)));
  if (rest.length) groups.push({ id: OTHER_ID, name: 'Other teams', icon: null, color: null, teams: rest });
  return groups;
}

export const holdsTeam = (g: NavGroup, teamKey: string | undefined): boolean =>
  !!teamKey && g.teams.some((t) => t.key.toLowerCase() === teamKey.toLowerCase());

/** Whether a group is open when the person has not chosen (T18 #22, the 12-row rule). */
export function startsOpen(groups: NavGroup[], g: NavGroup, activeTeamKey: string | undefined): boolean {
  const rows = groups.reduce((n, x) => n + x.teams.length, 0);
  return rows <= AUTO_COLLAPSE_ROWS || holdsTeam(g, activeTeamKey);
}

/** A colour from the server is `#rrggbb`; anything else is not used in a style. */
export const safeColor = (c: string | null): string | undefined =>
  c && /^#[0-9a-fA-F]{6}$/.test(c) ? c : undefined;
