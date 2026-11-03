import { productTeamsTitle } from '../../ui/teamKeys';
import { useEffect, useId, useState } from 'react';
import { Link, useMatch } from 'react-router-dom';
import type { Team } from '../../api/types';
import { useOrgSlug } from '../../api/org';
import { IconChevron } from '../../ui/Icons';
import { TeamNavItem } from './TeamNavItem';
import { holdsTeam, OTHER_ID, safeColor, startsOpen, type NavGroup } from './sidebarGroups';
import styles from './shell.module.css';

const PREFIX = 'devops-nav-group:';

/** One key per product id (a slug can change). "Other teams" is not a product, so it is kept per org. */
const storeKey = (g: NavGroup, org: string) => PREFIX + (g.id === OTHER_ID ? `${OTHER_ID}:${org}` : g.id);

function readStored(key: string): boolean | undefined {
  try {
    const v = localStorage.getItem(key);
    return v === '1' ? true : v === '0' ? false : undefined;
  } catch {
    return undefined; // storage blocked: the default rule applies
  }
}

function writeStored(key: string, open: boolean): void {
  try {
    localStorage.setItem(key, open ? '1' : '0');
  } catch {
    /* storage blocked: the choice lasts until the page closes */
  }
}

/**
 * The Teams part of the sidebar when the org has products (LAUN-64, T18 #22): one collapsible group per
 * product, then "Other teams". The caller makes sure there is at least one group.
 */
export function TeamGroups({
  groups,
  link,
}: {
  groups: NavGroup[];
  link: (s: { isActive: boolean }) => string;
}) {
  const org = useOrgSlug();
  const activeKey = useMatch('/:org/team/:team/*')?.params.team;
  // The person's choices this visit. They win over storage, so a click works when storage throws.
  const [chosen, setChosen] = useState<Record<string, boolean>>({});
  // Groups opened because the route went to a team inside them. Never written to storage.
  const [auto, setAuto] = useState<ReadonlySet<string>>(new Set());

  const holders = groups
    .filter((g) => holdsTeam(g, activeKey))
    .map((g) => g.id)
    .join(',');
  useEffect(() => {
    if (!holders) return;
    setAuto((prev) => new Set([...prev, ...holders.split(',')]));
  }, [activeKey, holders]);

  const isOpen = (g: NavGroup): boolean =>
    auto.has(g.id) || (chosen[g.id] ?? readStored(storeKey(g, org)) ?? startsOpen(groups, g, activeKey));

  const toggle = (g: NavGroup) => {
    const next = !isOpen(g);
    setAuto((prev) => {
      const s = new Set(prev);
      s.delete(g.id);
      return s;
    });
    setChosen((c) => ({ ...c, [g.id]: next }));
    writeStored(storeKey(g, org), next);
  };

  return (
    <div className={styles.groups}>
      {groups.map((g) => (
        <Group key={g.id} group={g} open={isOpen(g)} onToggle={() => toggle(g)} link={link} />
      ))}
    </div>
  );
}

function Group({
  group,
  open,
  onToggle,
  link,
}: {
  group: NavGroup;
  open: boolean;
  onToggle: () => void;
  link: (s: { isActive: boolean }) => string;
}) {
  const org = useOrgSlug();
  const uid = useId();
  const headId = `${uid}-head`;
  const bodyId = `${uid}-body`;
  const color = safeColor(group.color);
  return (
    <div className={styles.group}>
      <div className={styles.groupRow}>
        <button
          type="button"
          id={headId}
          className={styles.groupHead}
          aria-expanded={open}
          title={
            group.slug
              ? productTeamsTitle(
                  group.name,
                  group.teams.map((t) => t.key),
                )
              : undefined
          }
          aria-controls={open ? bodyId : undefined}
          onClick={onToggle}
        >
          <IconChevron className={`${styles.groupChevron} ${open ? '' : styles.groupClosed}`} />
          {(group.icon || color) && (
            <span className={styles.groupMark} aria-hidden="true">
              {color && <span className={styles.groupDot} style={{ background: color }} />}
              {group.icon}
            </span>
          )}
          <span className={styles.groupName}>{group.name}</span>
          <span className={styles.groupCount} aria-hidden="true">
            {group.teams.length}
          </span>
        </button>
        {/* A real product (not "Other teams") has its own page (LAUN-65). A link beside the button, never inside it. */}
        {group.slug && (
          <Link
            to={`/${org}/product/${group.slug}`}
            className={styles.groupOpen}
            aria-label={`Open product ${group.name}`}
            title={`Open ${group.name}`}
          >
            <span aria-hidden="true">›</span>
          </Link>
        )}
      </div>
      {/* Not rendered when closed (not hidden by CSS), so the keyboard never reaches these rows. */}
      {open && (
        <div id={bodyId} role="group" aria-labelledby={headId}>
          {group.teams.map((t: Team) => (
            // A team in two products is two rows: the key holds the group too.
            <TeamNavItem key={`${group.id}:${t.id}`} team={t} link={link} inGroup={group.name} />
          ))}
        </div>
      )}
    </div>
  );
}
