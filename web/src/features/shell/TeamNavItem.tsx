import { NavLink, useNavigate } from 'react-router-dom';
import type { Team } from '../../api/types';
import { useOrgSlug } from '../../api/org';
import { useIsOrgAdmin } from '../settings/access';
import { CyclesNavLink } from '../cycles/CyclesNavLink';
import { Menu } from '../../ui/Menu';
import { IconMore } from '../../ui/Icons';
import styles from './shell.module.css';

/** A team in the sidebar, with a `...` menu for an org admin (LAUN-15). */
export function TeamNavItem({
  team,
  link,
  inGroup,
}: {
  team: Pick<Team, 'key' | 'name' | 'cyclesEnabled'>;
  link: (s: { isActive: boolean }) => string;
  /** The product group this row is in. It names the row's menu and cycles group, so two rows of one team differ. */
  inGroup?: string;
}) {
  const org = useOrgSlug();
  const nav = useNavigate();
  // Only an org admin can open the Teams settings page (a platform admin who is not an org admin sees
  // the Platform page only), so only an org admin gets the menu.
  const isAdmin = useIsOrgAdmin();
  return (
    <>
      <div className={`${styles.teamRow} ${isAdmin ? styles.teamRowMenu : ''}`}>
        <NavLink to={`/${org}/team/${team.key}`} end className={link}>
          <span className={styles.key}>{team.key}</span> <span className={styles.teamName}>{team.name}</span>
        </NavLink>
        {isAdmin && (
          <div className={styles.teamMore}>
            <Menu
              label={inGroup ? `${team.name} team options in ${inGroup}` : `${team.name} team options`}
              trigger={<IconMore />}
              hideChevron
              align="right"
              triggerClassName={styles.teamMoreBtn}
              items={[{ label: 'Team settings', onSelect: () => nav(`/${org}/settings/teams`) }]}
            />
          </div>
        )}
      </div>
      <CyclesNavLink team={team} inGroup={inGroup} />
    </>
  );
}
