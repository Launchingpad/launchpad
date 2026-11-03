import { useCallback, useEffect, useMemo } from 'react';
import { Link, NavLink, Outlet, useLocation, useMatch, useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { useOrgSlug } from '../../api/org';
import { useOrgEvents } from '../../api/events';
import { useInboxLive } from '../../api/notifications';
import { useProduct } from '../../api/products';
import { useOrgDetail, useOrgList, useProducts, useTeams } from '../../api/queries';
import { keys } from '../../api/keys';
import { useAuthedSession, useLogout } from '../auth/session';
import { useCanOpenSettings, useIsOrgAdmin } from '../settings/access';
import { useUi } from './ui-context';
import { useHotkeys } from '../../lib/hotkeys';
import { useTheme } from '../../lib/theme';
import { ThemeSync } from '../settings/ThemeSync';
import { presets } from '../issues/filters';
import { CreateIssueDialog } from '../issues/CreateIssueDialog';
import { CommandPalette } from '../palette/CommandPalette';
import { InboxBell } from '../inbox/InboxBell';
import { UnreadBadge } from '../inbox/InboxControls';
import inboxStyles from '../inbox/inbox.module.css';
import { ShortcutHelp } from './ShortcutHelp';
import { TeamNavItem } from './TeamNavItem';
import { TeamGroups } from './TeamGroups';
import { buildGroups } from './sidebarGroups';
import { AlertStack } from '../../ui/AlertStack';
import { OnboardingBanner } from '../settings/OnboardingBanner';
import { SettingsSidebar } from '../settings/SettingsSidebar';
import { Avatar } from '../../ui/Avatar';
import { Logo } from '../../ui/Logo';
import { Menu, type MenuItem } from '../../ui/Menu';
import {
  IconInbox,
  IconProject,
  IconMenu,
  IconMoon,
  IconPlus,
  IconSearch,
  IconStack,
  IconSun,
  IconUser,
} from '../../ui/Icons';
import ui from '../../styles/ui.module.css';
import styles from './shell.module.css';

export function Shell() {
  const org = useOrgSlug();
  const nav = useNavigate();
  const loc = useLocation();
  const qc = useQueryClient();
  const { user, orgs } = useAuthedSession();
  const logout = useLogout();
  const isAdmin = useIsOrgAdmin();
  const canOpenSettings = useCanOpenSettings();
  const teams = useTeams();
  const products = useProducts();
  // No product to show: the flat list, exactly as before products (T18 #7).
  const groups = useMemo(
    () => (teams.data && products.data ? buildGroups(teams.data, products.data) : null),
    [teams.data, products.data],
  );
  const { openCreate, setPaletteOpen, setHelpOpen, sidebarOpen, setSidebarOpen, paletteOpen } = useUi();
  const teamMatch = useMatch('/:org/team/:team/*');
  // The product page (LAUN-65): the dialog opens with that product's teams first (T18 #24).
  const productSlug = useMatch('/:org/product/:slug/*')?.params.slug?.toLowerCase();
  const productOnPage = useProduct(productSlug);
  const createHere = useCallback(
    () =>
      openCreate(
        teamMatch
          ? { team: teamMatch.params.team }
          : productSlug && productOnPage.data?.teams.length
            ? { firstTeams: productOnPage.data.teams.map((t) => t.key) }
            : {},
      ),
    [openCreate, teamMatch, productSlug, productOnPage.data],
  );
  // Settings replace the app sidebar with their own (LAUN-26).
  const inSettings = useMatch('/:org/settings/*') !== null;
  const { pref, cycle } = useTheme();

  const onUnauthorized = useCallback(() => qc.setQueryData(keys.session, null), [qc]);
  const conn = useOrgEvents(org, onUnauthorized);
  useInboxLive(org, onUnauthorized);

  useHotkeys({
    c: createHere,
    '?': () => setHelpOpen(true),
    'mod+k': () => setPaletteOpen(!paletteOpen),
  });

  // Close the mobile drawer after any navigation.
  useEffect(() => setSidebarOpen(false), [loc.pathname, loc.search, setSidebarOpen]);
  // Remember the org for the next visit (a preference, not a credential).
  useEffect(() => {
    try {
      localStorage.setItem('devops-last-org', org);
    } catch {
      /* storage blocked */
    }
  }, [org]);

  const current = orgs.find((o) => o.slug === org);
  // LAUN-22: the logos. The current org's comes from its own record; the list is read only when there is
  // another org to show in the switcher.
  const detail = useOrgDetail();
  const orgList = useOrgList(orgs.length > 1);
  const logoOf = (slug: string) =>
    slug === org ? detail.data?.avatarUrl : orgList.data?.find((o) => o.slug === slug)?.avatarUrl;
  const orgName = current?.name ?? org;
  const orgItems: MenuItem[] = [
    { label: 'Organisations', heading: true, onSelect: () => {} },
    ...orgs.map((o) => ({
      label: o.name,
      icon: <Logo name={o.name} src={logoOf(o.slug)} size={18} decorative />,
      current: o.slug === org,
      onSelect: () => nav(`/${o.slug}/issues`),
    })),
    ...(canOpenSettings ? [{ label: 'Settings', onSelect: () => nav(`/${org}/settings`) }] : []),
  ];
  const userItems: MenuItem[] = [
    { label: `Mode: ${pref}`, icon: pref === 'dark' ? <IconMoon /> : <IconSun />, onSelect: cycle },
    { label: 'Settings', onSelect: () => nav(`/${org}/settings`) },
    { label: 'Keyboard shortcuts', onSelect: () => setHelpOpen(true) },
    { label: 'Sign out', danger: true, onSelect: () => void logout() },
  ];
  const link = ({ isActive }: { isActive: boolean }) => `${styles.link} ${isActive ? styles.active : ''}`;

  return (
    <div className={styles.app}>
      <ThemeSync />
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      {sidebarOpen && (
        <div className={styles.backdrop} onClick={() => setSidebarOpen(false)} aria-hidden="true" />
      )}
      <aside className={`${styles.sidebar} ${sidebarOpen ? styles.open : ''}`} aria-label="Sidebar">
        {inSettings ? (
          <div className={styles.settingsSide}>
            <SettingsSidebar />
          </div>
        ) : (
          <>
            <Menu
              label="Switch organisation"
              items={orgItems}
              triggerClassName={styles.orgBtn}
              trigger={
                <>
                  <Logo name={orgName} src={logoOf(org)} decorative />
                  <span className={styles.grow}>{orgName}</span>
                </>
              }
            />
            <div className={styles.actions}>
              <button
                type="button"
                className={`${ui.btn} ${styles.grow}`}
                onClick={() => setPaletteOpen(true)}
              >
                <IconSearch /> Search <kbd style={{ marginLeft: 'auto' }}>⌘K</kbd>
              </button>
              <InboxBell />
              <button
                type="button"
                className={`${ui.btn} ${ui.primary} ${ui.icon}`}
                aria-label="Create issue"
                title="Create issue (c)"
                onClick={createHere}
              >
                <IconPlus />
              </button>
            </div>
            <nav aria-label="Primary" className={styles.nav}>
              <NavLink to={`/${org}/inbox`} className={link}>
                <IconInbox /> Inbox
                <UnreadBadge className={inboxStyles.navBadge} />
              </NavLink>
              <NavLink to={`/${org}/my-issues`} className={link}>
                <IconUser /> My issues
              </NavLink>
              <NavLink to={`/${org}/issues`} end className={link}>
                <IconStack /> All issues
              </NavLink>
              <NavLink to={`/${org}/projects`} className={link}>
                <IconProject /> Projects
              </NavLink>
              <div className={styles.section}>Teams</div>
              {groups ? (
                <TeamGroups groups={groups} link={link} />
              ) : (
                teams.data?.map((t) => <TeamNavItem key={t.id} team={t} link={link} />)
              )}
              <div className={styles.section}>Views</div>
              {presets.map((p) => (
                <Link
                  key={p.id}
                  to={`/${org}/issues?${new URLSearchParams(p.search).toString()}`}
                  className={styles.link}
                >
                  {p.label}
                </Link>
              ))}
            </nav>
          </>
        )}
        <div className={styles.userRow}>
          <Menu
            label="User menu"
            up
            items={userItems}
            triggerClassName={styles.orgBtn}
            trigger={
              <>
                <Avatar name={user.name} username={user.username} />
                <span className={styles.grow}>{user.name}</span>
                {isAdmin && <span className={styles.badge}>Admin</span>}
              </>
            }
          />
        </div>
      </aside>
      <div className={styles.main}>
        <header className={styles.topbar}>
          <button
            type="button"
            className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
            aria-label="Open menu"
            onClick={() => setSidebarOpen(true)}
          >
            <IconMenu />
          </button>
          <Logo name={orgName} src={logoOf(org)} size={20} decorative />
          <strong className={styles.grow}>{orgName}</strong>
          <button
            type="button"
            className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
            aria-label="Search"
            onClick={() => setPaletteOpen(true)}
          >
            <IconSearch />
          </button>
          <InboxBell />
          <button
            type="button"
            className={`${ui.btn} ${ui.primary} ${ui.icon}`}
            aria-label="Create issue"
            onClick={createHere}
          >
            <IconPlus />
          </button>
        </header>
        <AlertStack>
          {conn === 'reconnecting' && (
            <div className={styles.conn} role="status">
              Reconnecting…
            </div>
          )}
          {!inSettings && <OnboardingBanner />}
        </AlertStack>
        <main id="main" className={styles.content} tabIndex={-1}>
          <Outlet />
        </main>
      </div>
      <CreateIssueDialog />
      <CommandPalette />
      <ShortcutHelp />
    </div>
  );
}
