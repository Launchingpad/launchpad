import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { ApiError } from '../../api/errors';
import { usePushDevices, useRemovePushDevice } from '../../api/queries';
import type { PushDevice } from '../../api/types';
import { useOrgSlug } from '../../api/org';
import { Dialog } from '../../ui/Dialog';
import { EmptyState, SkeletonRows } from '../../ui/States';
import org_ from './organisation.module.css';
import { FixedError } from './FixedError';
import { SettingsCard, SettingsPage } from './SettingsParts';
import { deviceRemoveText, devicesLoadText, platformLabel } from './pushText';
import styles from './push.module.css';
import ui from '../../styles/ui.module.css';

function when(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

/**
 * Settings, Mobile devices (LAUN-197, decisions-T35): the phones this person registered by signing in to the
 * mobile app. The page lists them and removes one. It never shows a token (the server never returns one).
 */
export function MobileDevicesPage() {
  const devices = usePushDevices();
  const remove = useRemovePushDevice();
  const org = useOrgSlug();
  const [error, setError] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<PushDevice | null>(null);
  const listRef = useRef<HTMLUListElement>(null);
  // Synchronous guard: two clicks in one tick would both pass an isPending check.
  const flying = useRef(false);
  // After a removal focus goes to the next row's button, else the previous, else the page heading.
  const pendingFocus = useRef<{ removed: string; next: string | null } | null>(null);
  const items = devices.data?.items;

  useEffect(() => {
    const p = pendingFocus.current;
    // Wait until the confirm dialog has unmounted: while it is open the page behind is inert, and its
    // unmount would take focus back to the opener. This effect runs after that cleanup.
    if (confirm || !p || !items || items.some((d) => d.id === p.removed)) return;
    pendingFocus.current = null;
    const btn = p.next ? listRef.current?.querySelector<HTMLElement>(`[data-device="${p.next}"]`) : null;
    if (btn) btn.focus();
    else document.querySelector<HTMLElement>('h1')?.focus();
  }, [items, confirm]);

  async function onRemove(d: PushDevice) {
    if (flying.current) return;
    flying.current = true;
    setError(null);
    const rows = items ?? [];
    const i = rows.findIndex((r) => r.id === d.id);
    pendingFocus.current = { removed: d.id, next: (rows[i + 1] ?? rows[i - 1])?.id ?? null };
    try {
      await remove.mutateAsync(d.id);
      setConfirm(null);
    } catch (err) {
      // A device that is already gone (404) is the state the person wanted.
      setConfirm(null);
      if (!(err instanceof ApiError && err.status === 404)) {
        pendingFocus.current = null;
        setError(deviceRemoveText(err));
      }
    } finally {
      flying.current = false;
    }
  }

  const forbidden = devices.error instanceof ApiError && devices.error.status === 403;

  return (
    <SettingsPage
      title="Mobile devices"
      description="The phones where you are signed in to the Launchpad mobile app. A device gets a wake only when your administrator has turned on mobile push and you have turned Push on for an event. Remove a device you no longer use."
    >
      {devices.isPending ? (
        <SkeletonRows n={2} />
      ) : forbidden ? (
        <EmptyState title="No mobile devices for this account">
          <p>{devicesLoadText(devices.error)}</p>
        </EmptyState>
      ) : devices.isError ? (
        <FixedError
          error={devices.error}
          onRetry={() => void devices.refetch()}
          title="Could not load your devices"
        />
      ) : (
        <>
          {!devices.data.relayConfigured && (
            <p role="note" className={styles.warn}>
              Your administrator has not turned on mobile push, so no device gets a wake yet.
            </p>
          )}
          {devices.data.relayConfigured && (
            <p className={styles.note}>
              Choose which events wake your devices under{' '}
              <Link to={`/${org}/settings/notifications`}>Notifications</Link>. Push is off until you turn it
              on.
            </p>
          )}
          {error && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {error}
            </p>
          )}
          {devices.data.items.length === 0 ? (
            <EmptyState title="No devices yet">
              <p>Sign in to the Launchpad mobile app and it appears here.</p>
            </EmptyState>
          ) : (
            <SettingsCard>
              <ul className={styles.list} ref={listRef} aria-label="Registered devices">
                {devices.data.items.map((d) => {
                  const name = d.label || 'Unnamed device';
                  return (
                    <li key={d.id} className={styles.device}>
                      <div className={styles.deviceText}>
                        <span className={styles.deviceName}>{name}</span>
                        <span className={styles.deviceMeta}>
                          {platformLabel(d.platform)}
                          {d.appVersion ? `, app ${d.appVersion}` : ''}. Last seen {when(d.lastSeenAt)}.
                        </span>
                      </div>
                      <button
                        type="button"
                        data-device={d.id}
                        className={`${ui.btn} ${ui.danger}`}
                        aria-label={`Remove ${name}`}
                        onClick={() => setConfirm(d)}
                      >
                        Remove
                      </button>
                    </li>
                  );
                })}
              </ul>
            </SettingsCard>
          )}
        </>
      )}
      {confirm && (
        <Dialog title="Remove this device?" width={460} onClose={() => !remove.isPending && setConfirm(null)}>
          <p>
            {confirm.label || 'Unnamed device'} will get no more wakes. Sign in to the mobile app on it to
            register it again.
          </p>
          <div className={org_.actions} style={{ justifyContent: 'flex-end', marginTop: 12 }}>
            <button
              type="button"
              className={`${ui.btn} ${ui.ghost}`}
              data-autofocus
              onClick={() => !remove.isPending && setConfirm(null)}
            >
              Cancel
            </button>
            <button
              type="button"
              className={`${ui.btn} ${ui.danger}`}
              aria-disabled={remove.isPending || undefined}
              onClick={() => void onRemove(confirm)}
            >
              {remove.isPending ? 'Removing…' : 'Remove device'}
            </button>
          </div>
        </Dialog>
      )}
    </SettingsPage>
  );
}
