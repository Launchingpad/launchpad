import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useIsOrgAdmin, useIsPlatformAdmin } from './access';
import { NoAccess } from './SettingsParts';
import styles from './settings.module.css';

/**
 * `/:org/settings`. The sidebar of the settings area is `SettingsSidebar`, drawn by the shell in place of
 * the app sidebar. The server is the judge of every write; this gate only keeps a person who cannot
 * change anything from seeing pages that look like they can.
 */
export function SettingsLayout() {
  const isAdmin = useIsOrgAdmin();
  const isPlatformAdmin = useIsPlatformAdmin();
  const path = useLocation().pathname.replace(/\/+$/, '');
  const platformOnlyOk = path.endsWith('/settings/platform');
  // LAUN-36: every member opens their own notification settings, and (LAUN-160) their own sessions (and, LAUN-229, their own webhooks). Nothing else is open to a plain member.
  // The bare address only redirects, so it may render too.
  if (
    path.endsWith('/settings/notifications') ||
    path.endsWith('/settings/sessions') ||
    path.endsWith('/settings/connected-accounts') ||
    path.endsWith('/settings/mobile-devices') ||
    path.endsWith('/settings/personal-webhooks') ||
    path.endsWith('/settings')
  )
    return (
      <div className={styles.layout}>
        <Outlet />
      </div>
    );
  const denied = (
    <NoAccess title="You need admin access." hint="Ask an organisation admin to change your role." />
  );
  if (!isAdmin && !isPlatformAdmin) return <div className={styles.layout}>{denied}</div>;
  return (
    // A platform admin who is not an org admin sees the Platform page only.
    <div className={styles.layout}>{isAdmin || platformOnlyOk ? <Outlet /> : denied}</div>
  );
}

/** `/:org/settings`: the first page this person may open. */
export function SettingsIndex() {
  const org = useOrgSlug();
  const isAdmin = useIsOrgAdmin();
  const isPlatformAdmin = useIsPlatformAdmin();
  const to = isAdmin ? 'organisation' : isPlatformAdmin ? 'platform' : 'notifications';
  return <Navigate to={`/${org}/settings/${to}`} replace />;
}
