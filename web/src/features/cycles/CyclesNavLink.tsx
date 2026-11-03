import { Link, useMatch } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useCycles } from '../../api/queries';
import type { Team } from '../../api/types';
import { IconCycle } from '../../ui/Icons';
import { sectionCycles } from './cycles';
import { useNow } from './useNow';
import styles from '../shell/shell.module.css';

/**
 * Sidebar item under a team (LAUN-17): "Cycles" (the list) with two children, "Current" and "Upcoming".
 * It shows only when the team has cycles on. The children open `/cycles/current` and `/cycles/upcoming`,
 * which redirect to the cycle (so the link is never stale). The cycles are read only while a cycle page of
 * THIS team is open, to light the right child; no other page asks for them.
 */
export function CyclesNavLink({
  team,
  inGroup,
}: {
  team: Pick<Team, 'key' | 'cyclesEnabled'>;
  /** The product group of this row (LAUN-64): a team in two groups has two of these, with two names. */
  inGroup?: string;
}) {
  const org = useOrgSlug();
  const onList = useMatch('/:org/team/:team/cycles');
  const onCycle = useMatch('/:org/team/:team/cycle/:number');
  const onJump = useMatch('/:org/team/:team/cycles/:which');
  const mine = (m: { params: { team?: string } } | null) =>
    !!m && m.params.team?.toLowerCase() === team.key.toLowerCase();
  const listHere = mine(onList);
  const cycleHere = mine(onCycle);
  const jumpHere = mine(onJump) ? onJump!.params.which : undefined;
  const now = useNow();
  const cycles = useCycles(team.key, team.cyclesEnabled && cycleHere);
  if (!team.cyclesEnabled) return null;

  let child: 'current' | 'upcoming' | undefined =
    jumpHere === 'current' || jumpHere === 'upcoming' ? jumpHere : undefined;
  if (cycleHere && cycles.data) {
    const n = Number(onCycle!.params.number);
    const sec = sectionCycles(cycles.data, now);
    if (sec.current.some((c) => c.number === n)) child = 'current';
    else if (sec.next[0]?.number === n) child = 'upcoming';
  }
  const parentActive = listHere || (cycleHere && !child);
  const link = (active: boolean) => `${styles.link} ${active ? styles.active : ''}`;
  const base = `/${org}/team/${team.key}/cycles`;
  return (
    <>
      <Link
        to={base}
        aria-current={parentActive ? 'page' : undefined}
        className={`${link(!!parentActive)} ${styles.sub}`}
      >
        <IconCycle /> Cycles
      </Link>
      <div
        className={styles.cycleChildren}
        role="group"
        aria-label={inGroup ? `${team.key} cycles in ${inGroup}` : `${team.key} cycles`}
      >
        <Link
          to={`${base}/current`}
          aria-current={child === 'current' ? 'page' : undefined}
          className={link(child === 'current')}
        >
          Current
        </Link>
        <Link
          to={`${base}/upcoming`}
          aria-current={child === 'upcoming' ? 'page' : undefined}
          className={link(child === 'upcoming')}
        >
          Upcoming
        </Link>
      </div>
    </>
  );
}
