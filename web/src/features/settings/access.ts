import { useOrgSlug } from '../../api/org';
import { useAuthedSession } from '../auth/session';

/** The person's role in the current org, from the session. `undefined` when the org is not in it. */
export function useOrgRole(): 'admin' | 'member' | undefined {
  const org = useOrgSlug();
  return useAuthedSession().orgs.find((o) => o.slug === org)?.role;
}

export function useIsOrgAdmin(): boolean {
  return useOrgRole() === 'admin';
}

export function useIsPlatformAdmin(): boolean {
  return useAuthedSession().user.isPlatformAdmin;
}

/**
 * Who may open Settings. An org admin, or a platform admin. A platform admin who is only a
 * member of the org still reaches Platform: the server judges that route by the platform flag and the admin
 * scope of the session, not by the org role (a web session of a platform admin always has the admin scope).
 */
export function useCanOpenSettings(): boolean {
  const orgAdmin = useIsOrgAdmin();
  const platformAdmin = useIsPlatformAdmin();
  return orgAdmin || platformAdmin;
}
