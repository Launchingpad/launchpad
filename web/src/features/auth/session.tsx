import { useEffect } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { fetchSession, addUserChangedHandler, forgetSignedInUser, logout as apiLogout } from '../../api/auth';
import { keys } from '../../api/keys';
import { resetOwnWrites, resetThemeWrites } from '../../api/queries';
import { applyLook } from '../../lib/theme';
import { usingBearer } from '../../api/http';
import type { Session } from '../../api/types';
import { ErrorState, Spinner } from '../../ui/States';
import { useToast } from '../../ui/Toast';

export function useSession() {
  const qc = useQueryClient();
  useEffect(() => {
    // Another tab signed in as a different user: reload the session so this page shows who it is.
    // Every consumer adds its own handler and removes only that one, so one unmounting does not
    // switch off the others.
    return addUserChangedHandler(() => {
      resetOwnWrites(qc);
      resetThemeWrites(); // LAUN-235: the new person's theme comes from the server again
      void qc.invalidateQueries({ queryKey: keys.myPreferences });
      void qc.invalidateQueries({ queryKey: keys.session });
    });
  }, [qc]);
  return useQuery<Session | null>({
    queryKey: keys.session,
    queryFn: fetchSession,
    staleTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
  });
}

/** For components below RequireAuth, where a session is guaranteed. */
export function useAuthedSession(): Session {
  const q = useSession();
  if (!q.data) throw new Error('useAuthedSession outside RequireAuth');
  return q.data;
}

/**
 * Forget everything of the signed-in person and show the login page: the part of a log out after the server
 * has ended the session. Also used when the person revokes the session they are using (LAUN-160).
 */
export function useClearSignedIn() {
  const qc = useQueryClient();
  return async () => {
    forgetSignedInUser();
    resetOwnWrites(qc);
    // LAUN-235 (decisions-T42 #23): the theme is the person's; the login page and the next person start in Linear.
    applyLook('linear');
    resetThemeWrites();
    // Drop everything except the session entry: clear() would detach its observer.
    await qc.cancelQueries();
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.session[0] });
    qc.setQueryData(keys.session, null);
  };
}

export function useLogout() {
  const toast = useToast();
  const clear = useClearSignedIn();
  return async () => {
    if (!usingBearer()) {
      try {
        await apiLogout();
      } catch {
        // The server did not confirm, so its session may still be alive. Stay signed in and say so.
        toast.show('Could not sign out. The server did not confirm it. Try again.', { kind: 'error' });
        return;
      }
    }
    await clear();
  };
}

export function RequireAuth() {
  const q = useSession();
  const loc = useLocation();
  if (q.isPending) return <Spinner label="Checking your session" />;
  if (q.isError)
    return (
      <ErrorState error={q.error} onRetry={() => void q.refetch()} title="Could not check your session" />
    );
  if (!q.data) return <Navigate to="/login" replace state={{ from: loc.pathname + loc.search }} />;
  return <Outlet />;
}
