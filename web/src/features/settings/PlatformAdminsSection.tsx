import { SettingsCard, SettingsSection } from './SettingsParts';
import { useState, type FormEvent } from 'react';
import { useAddPlatformAdmin, usePlatformAdmins, useRemovePlatformAdmin } from '../../api/queries';
import type { PlatformAdmin } from '../../api/types';
import { useAuthedSession } from '../auth/session';
import { Dialog } from '../../ui/Dialog';
import { SkeletonRows } from '../../ui/States';
import { FixedError } from './FixedError';
import { adminAddText, adminRemoveText } from './joinText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

/**
 * Settings, Platform: the platform admins (decisions-T12 #23 and #24). The server refuses to remove
 * the last active one with a 409; the page shows that as a fixed sentence.
 */
export function PlatformAdminsSection({ onSelfRemoved }: { onSelfRemoved?: () => void } = {}) {
  const me = useAuthedSession().user.id;
  const admins = usePlatformAdmins();
  const add = useAddPlatformAdmin();
  const remove = useRemovePlatformAdmin();
  const [user, setUser] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [target, setTarget] = useState<PlatformAdmin | null>(null);
  const [removeError, setRemoveError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setNote(null);
    const v = user.trim();
    if (!v || v.length > 254) {
      setError('Enter a username or an email address.');
      return;
    }
    try {
      const r = await add.mutateAsync(v);
      setUser('');
      setNote(`${r.name} is a platform admin. The person must sign in again to use the platform pages.`);
    } catch (err) {
      setError(adminAddText(err));
    }
  }

  async function confirmRemove() {
    if (!target) return;
    setRemoveError(null);
    try {
      await remove.mutateAsync(target.id);
      setNote(`${target.name} is no longer a platform admin.`);
      setTarget(null);
      if (target.id === me) onSelfRemoved?.();
    } catch (err) {
      setRemoveError(adminRemoveText(err));
    }
  }

  return (
    <SettingsSection
      title="Platform admins"
      id="padmins-title"
      description={
        <>
          A platform admin can change these settings and add other platform admins. The install must keep at
          least one active platform admin, so you cannot remove the last one.
        </>
      }
    >
      {admins.isPending ? (
        <SkeletonRows n={2} />
      ) : admins.isError ? (
        <FixedError
          error={admins.error}
          onRetry={() => void admins.refetch()}
          title="Could not load the platform admins"
        />
      ) : (
        <SettingsCard>
          <table className={`${styles.table} ${styles.wide}`}>
            <caption className="sr-only">Platform admins</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Username</th>
                <th scope="col">Email</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {admins.data.map((a) => (
                <tr key={a.id}>
                  <td>
                    {a.name}
                    {a.deactivated && <span className={styles.tag}>Deactivated</span>}
                  </td>
                  <td>
                    <code>{a.username}</code>
                  </td>
                  <td>{a.email ?? '—'}</td>
                  <td>
                    <button
                      type="button"
                      className={`${ui.btn} ${ui.danger}`}
                      aria-label={`Remove ${a.name} as platform admin`}
                      onClick={() => {
                        setRemoveError(null);
                        setNote(null);
                        setTarget(a);
                      }}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </SettingsCard>
      )}
      <SettingsSection title="Add a platform admin" id="padmin-form-title">
        <SettingsCard padded>
          <form onSubmit={(e) => void submit(e)} noValidate aria-labelledby="padmin-form-title">
            <div className={ui.field}>
              <label htmlFor="padmin-user">Username or email</label>
              <input
                id="padmin-user"
                className={ui.input}
                value={user}
                onChange={(e) => setUser(e.target.value)}
                autoComplete="off"
                autoCapitalize="none"
                spellCheck={false}
                aria-invalid={!!error}
                aria-describedby={error ? 'padmin-error' : undefined}
              />
            </div>
            {error && (
              <p id="padmin-error" role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {error}
              </p>
            )}
            <div className={styles.actions}>
              <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={add.isPending}>
                {add.isPending ? 'Adding…' : 'Add platform admin'}
              </button>
            </div>
          </form>
        </SettingsCard>
      </SettingsSection>
      <p role="status" className={ui.hint} data-testid="padmin-note">
        {note}
      </p>
      {target && (
        <Dialog
          title="Remove this platform admin?"
          onClose={() => setTarget(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setTarget(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={remove.isPending}
                onClick={() => void confirmRemove()}
              >
                {remove.isPending ? 'Removing…' : 'Remove platform admin'}
              </button>
            </>
          }
        >
          <p>
            <strong>{target.name}</strong> will lose access to the platform pages at once. The person stays in
            the organisation.
          </p>
          {removeError && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {removeError}
            </p>
          )}
        </Dialog>
      )}
    </SettingsSection>
  );
}
