import { useState } from 'react';
import { Link, NavLink } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { IconChevron, IconSearch } from '../../ui/Icons';
import { useIsOrgAdmin, useIsPlatformAdmin } from './access';
import { searchSettings, settingsPages, type SettingsGroup } from './settingsPages';
import styles from './settingsSidebar.module.css';

const groups: SettingsGroup[] = ['Account', 'Administration', 'Platform'];

/**
 * The settings sidebar. It replaces the app sidebar while a settings page is open (LAUN-26): Back to app,
 * a search box, then the pages the person may open, in groups. On a phone the shell shows it in the same
 * drawer as the app sidebar, so it is the page list and a tap opens a page.
 */
export function SettingsSidebar() {
  const org = useOrgSlug();
  const isAdmin = useIsOrgAdmin();
  const isPlatformAdmin = useIsPlatformAdmin();
  const [query, setQuery] = useState('');
  const pages = settingsPages.filter((p) =>
    p.access === 'member' ? true : p.access === 'admin' ? isAdmin : isPlatformAdmin,
  );
  const hits = searchSettings(pages, query);
  const searching = query.trim() !== '';
  const link = ({ isActive }: { isActive: boolean }) => `${styles.link} ${isActive ? styles.active : ''}`;

  return (
    <>
      <Link to={`/${org}/issues`} className={styles.back}>
        <IconChevron className={styles.backIcon} /> Back to app
      </Link>
      {pages.length > 0 && (
        <>
          <div className={styles.search}>
            <IconSearch />
            <input
              type="search"
              aria-label="Search settings"
              placeholder="Search settings"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape' && query) {
                  e.stopPropagation();
                  setQuery('');
                }
              }}
            />
          </div>
          <nav aria-label="Settings" className={styles.nav}>
            {searching ? (
              hits.length === 0 ? (
                <p role="status" className={styles.none}>
                  No settings match.
                </p>
              ) : (
                <>
                  <p role="status" className="sr-only">
                    {hits.length === 1 ? '1 page found' : `${hits.length} pages found`}
                  </p>
                  {hits.map(({ page, via }) => (
                    <NavLink key={page.path} to={`/${org}/settings/${page.path}`} className={link}>
                      <span>{page.label}</span>
                      {via && <span className={styles.via}>{via}</span>}
                    </NavLink>
                  ))}
                </>
              )
            ) : (
              groups.map((g) => {
                const inGroup = pages.filter((p) => p.group === g);
                if (inGroup.length === 0) return null;
                return (
                  <div key={g} role="group" aria-label={g} className={styles.group}>
                    <div className={styles.heading} aria-hidden="true">
                      {g}
                    </div>
                    {inGroup.map((p) => (
                      <NavLink key={p.path} to={`/${org}/settings/${p.path}`} className={link}>
                        {p.label}
                      </NavLink>
                    ))}
                  </div>
                );
              })
            )}
          </nav>
        </>
      )}
    </>
  );
}
