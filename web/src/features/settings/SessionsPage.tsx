import { useRef, useState } from 'react';
import { ApiError } from '../../api/errors';
import { useMySessions, useRevokeMySession, useRevokeOtherSessions } from '../../api/queries';
import type { LoginSession } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, SkeletonRows } from '../../ui/States';
import { useClearSignedIn } from '../auth/session';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage } from './SettingsParts';
import { kindText, othersDoneText, revokeText, sessionsLoadText } from './sessionText';
import { day } from './tokenText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

// LAUN-160. Settings > Sessions, for every member: the person's own login sessions (LAUN-130, decisions-T6a #44
// to #52). The list holds no token value. Revoking the session in use is a log out: the app clears its state
// the way Sign out does, and the router shows the login page.

export function SessionsPage() {
  const sessions = useMySessions();
  const revoke = useRevokeMySession();
  const revokeOthers = useRevokeOtherSessions();
  const clearSignedIn = useClearSignedIn();
  const [target, setTarget] = useState<LoginSession | null>(null);
  const [confirmOthers, setConfirmOthers] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const statusRef = useRef<HTMLSpanElement>(null);

  const rows = sessions.data ?? [];
  const forbidden = sessions.error instanceof ApiError && sessions.error.status === 403;

  async function doRevoke(s: LoginSession) {
    setError(null);
    setNote(null);
    try {
      await revoke.mutateAsync(s.id);
      setTarget(null);
      if (s.current) {
        await clearSignedIn();
        return;
      }
      setNote(`Logged out the ${kindText(s).toLowerCase()} session from ${day(s.createdAt)}.`);
      // The row is gone after the refetch: move focus to the status line so a keyboard user keeps their place.
      statusRef.current?.focus();
    } catch (e) {
      // A 404 is a session that is gone already: the list was refetched, so say so and close the dialog.
      if (e instanceof ApiError && e.status === 404) setTarget(null);
      setError(revokeText(e));
    }
  }

  async function doRevokeOthers() {
    setError(null);
    setNote(null);
    try {
      const n = await revokeOthers.mutateAsync();
      setConfirmOthers(false);
      setNote(othersDoneText(n));
    } catch (e) {
      setError(revokeText(e));
    }
  }

  return (
    <SettingsPage
      title="Sessions"
      description="The places where you are logged in. The list shows no secret. Log out of any you do not know."
      wide
    >
      {sessions.isPending ? (
        <SkeletonRows n={3} />
      ) : forbidden ? (
        <EmptyState title="No sessions for this account">
          <p>{sessionsLoadText(sessions.error)}</p>
        </EmptyState>
      ) : sessions.isError ? (
        <FixedError
          error={sessions.error}
          onRetry={() => void sessions.refetch()}
          title="Could not load sessions"
        />
      ) : rows.length === 0 ? (
        <EmptyState title="No sessions">
          <p>You have no login sessions. You are using a token.</p>
        </EmptyState>
      ) : (
        <SettingsCard>
          <div className={styles.tableScroll}>
            <table className={`${styles.table} ${styles.wide}`}>
              <caption className="sr-only">Your login sessions</caption>
              <thead>
                <tr>
                  <th scope="col">Session</th>
                  <th scope="col">Created</th>
                  <th scope="col">Last used</th>
                  <th scope="col">Expires</th>
                  <th scope="col">
                    <span className="sr-only">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((s) => (
                  <tr key={s.id}>
                    <td>
                      {kindText(s)}
                      {s.current && <span className={styles.tag}>This device</span>}
                    </td>
                    <td>{day(s.createdAt)}</td>
                    <td>{day(s.lastUsedAt)}</td>
                    <td>{day(s.expiresAt)}</td>
                    <td>
                      <button
                        type="button"
                        className={`${ui.btn} ${ui.danger}`}
                        disabled={revoke.isPending}
                        aria-label={`${s.current ? 'Log out this device' : 'Revoke'}: ${kindText(s).toLowerCase()} session created ${day(s.createdAt)}, last used ${day(s.lastUsedAt)}`}
                        onClick={() => {
                          setError(null);
                          setNote(null);
                          if (s.current) setTarget(s);
                          else void doRevoke(s);
                        }}
                      >
                        {s.current ? 'Log out' : 'Revoke'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </SettingsCard>
      )}
      <p>
        <button
          type="button"
          className={`${ui.btn} ${ui.danger}`}
          disabled={sessions.isPending || sessions.isError}
          onClick={() => {
            setError(null);
            setNote(null);
            setConfirmOthers(true);
          }}
        >
          Log out everywhere else
        </button>
      </p>
      <span role="status" className={ui.hint} tabIndex={-1} ref={statusRef}>
        {note}
      </span>
      {error && !target && !confirmOthers && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}

      {target && (
        <Dialog
          title="Log out of this device?"
          onClose={() => setTarget(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setTarget(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={revoke.isPending}
                onClick={() => void doRevoke(target)}
              >
                {revoke.isPending ? 'Logging out…' : 'Log out'}
              </button>
            </>
          }
        >
          <p>This is the session you are using now. If you revoke it, you are logged out at once.</p>
          {error && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {error}
            </p>
          )}
        </Dialog>
      )}
      {confirmOthers && (
        <Dialog
          title="Log out everywhere else?"
          onClose={() => setConfirmOthers(false)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setConfirmOthers(false)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={revokeOthers.isPending}
                onClick={() => void doRevokeOthers()}
              >
                {revokeOthers.isPending ? 'Logging out…' : 'Log out everywhere else'}
              </button>
            </>
          }
        >
          <p>Every other session is logged out at once. This device stays logged in. You cannot undo this.</p>
          {error && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {error}
            </p>
          )}
        </Dialog>
      )}
    </SettingsPage>
  );
}
