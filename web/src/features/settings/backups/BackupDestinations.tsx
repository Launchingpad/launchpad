import { useState } from 'react';
import { useBackupDestinations, useDestinationActions } from '../../../api/backups';
import type { BackupDestination, BackupDestinationType } from '../../../api/types';
import { fullDate, relativeTime } from '../../../lib/time';
import { Dialog } from '../../../ui/Dialog';
import { SkeletonRows } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { backupErrorText, codeText, RUN_STATUS, WEEKDAYS } from './backupText';
import { DestinationDialog } from './DestinationDialog';
import { TYPES } from './destinationTypes';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

function scheduleText(d: BackupDestination): string {
  const time = `${d.atUtc} UTC`;
  return d.schedule === 'weekly' && d.weekday !== null
    ? `Every ${WEEKDAYS[d.weekday] ?? 'week'} at ${time}`
    : `Every day at ${time}`;
}

function lastRunText(d: BackupDestination): string {
  const r = d.lastRun;
  if (!r) return 'No backup yet';
  const when = r.finishedAt ?? r.queuedAt;
  const base = `${RUN_STATUS[r.status]} ${relativeTime(when)}`;
  return r.status === 'failed' && r.errorCode ? `${base}. ${codeText(r.errorCode)}` : base;
}

/**
 * The destinations (decisions-T16 #44). Enable stays off until the signing public key exists and the admin ticked
 * that the private key and the signing public key are saved (`keysSaved`, UI only: the API checks the rest).
 */
export function BackupDestinations({
  supportedTypes,
  signingPublicSet,
  keysSaved,
}: {
  supportedTypes: BackupDestinationType[];
  signingPublicSet: boolean;
  keysSaved: boolean;
}) {
  const q = useBackupDestinations();
  const actions = useDestinationActions();
  const [dialog, setDialog] = useState<{ editId: string | null } | null>(null);
  const [deleting, setDeleting] = useState<BackupDestination | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const canEnable = signingPublicSet && keysSaved;

  async function act(d: BackupDestination, what: 'enable' | 'disable' | 'run') {
    setError(null);
    setNote(null);
    setBusyId(d.id);
    try {
      if (what === 'run') {
        await actions.runNow(d.id);
        setNote(`A backup of ${d.name} is queued. It starts when the next dump is ready.`);
      } else {
        await actions.update(d.id, { enabled: what === 'enable' });
        setNote(`${d.name} is ${what === 'enable' ? 'enabled' : 'disabled'}.`);
      }
    } catch (err) {
      setError(backupErrorText(err, what === 'run' ? 'run' : 'enable'));
    } finally {
      setBusyId(null);
    }
  }

  async function confirmDelete() {
    if (!deleting || deleteBusy) return;
    setError(null);
    setDeleteBusy(true);
    try {
      await actions.remove(deleting.id);
      setNote(`${deleting.name} is deleted. Its past runs stay in the history.`);
      setDeleting(null);
    } catch (err) {
      setError(backupErrorText(err, 'delete'));
      setDeleting(null);
    } finally {
      setDeleteBusy(false);
    }
  }

  return (
    <SettingsSection
      title="Destinations"
      id="backup-destinations"
      description={
        <>
          Where the encrypted copies go. A destination starts disabled. Private network addresses work only
          when the person who runs the install allows them (BACKUP_ALLOW_PRIVATE_CIDRS). After you clone this
          host or restore it onto another, run <code>deploy.sh --backup-resume</code> before backups run
          there.
        </>
      }
    >
      {q.isPending ? (
        <SkeletonRows n={2} />
      ) : q.isError ? (
        <FixedError
          error={q.error}
          onRetry={() => void q.refetch()}
          title="Could not load the destinations"
        />
      ) : (
        <>
          {q.data.length === 0 && (
            <SettingsCard padded>
              <p className={ui.hint}>No destination yet.</p>
            </SettingsCard>
          )}
          <ul className={styles.destList} aria-label="Destinations">
            {q.data.map((d) => (
              <li key={d.id} className={styles.dest}>
                <div className={styles.destMain}>
                  <div className={styles.destTitle}>
                    <strong>{d.name}</strong>
                    <span className={styles.tag}>{TYPES[d.type]?.label ?? d.type}</span>
                    <span className={`${styles.tag} ${d.enabled ? styles.tagOn : ''}`}>
                      {d.enabled ? 'Enabled' : 'Disabled'}
                    </span>
                    {d.secretsUnreadable && (
                      <span className={`${styles.tag} ${styles.tagBad}`}>Secrets unreadable</span>
                    )}
                    {!d.typeSupported && <span className={`${styles.tag} ${styles.tagBad}`}>No driver</span>}
                  </div>
                  <div className={ui.hint}>
                    {scheduleText(d)}. Keeps the newest {d.keepLast}.
                  </div>
                  <div
                    className={ui.hint}
                    title={d.lastRun ? fullDate(d.lastRun.finishedAt ?? d.lastRun.queuedAt) : undefined}
                  >
                    Last run: {lastRunText(d)}
                  </div>
                  {d.secretsUnreadable && (
                    <div className={styles.warnText}>
                      The server cannot open the stored secrets. Edit the destination and enter them again.
                    </div>
                  )}
                </div>
                <div className={styles.destActions}>
                  <button
                    type="button"
                    className={ui.btn}
                    aria-label={`Edit ${d.name}`}
                    onClick={() => setDialog({ editId: d.id })}
                  >
                    Edit
                  </button>
                  {d.enabled ? (
                    <>
                      <button
                        type="button"
                        className={ui.btn}
                        aria-label={`Back up ${d.name} now`}
                        disabled={busyId === d.id}
                        onClick={() => void act(d, 'run')}
                      >
                        Back up now
                      </button>
                      <button
                        type="button"
                        className={ui.btn}
                        aria-label={`Disable ${d.name}`}
                        disabled={busyId === d.id}
                        onClick={() => void act(d, 'disable')}
                      >
                        Disable
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      className={ui.btn}
                      aria-label={`Enable ${d.name}`}
                      disabled={busyId === d.id || !canEnable}
                      aria-describedby={!canEnable ? 'enable-gate' : undefined}
                      onClick={() => void act(d, 'enable')}
                    >
                      Enable
                    </button>
                  )}
                  <button
                    type="button"
                    className={`${ui.btn} ${ui.danger}`}
                    aria-label={`Delete ${d.name}`}
                    onClick={() => setDeleting(d)}
                  >
                    Delete
                  </button>
                </div>
              </li>
            ))}
          </ul>
          {!canEnable && (
            <p id="enable-gate" className={ui.hint} data-testid="enable-gate">
              {signingPublicSet
                ? 'To enable a destination, tick "I have saved the private key and the signing public key" above.'
                : 'To enable a destination, the signing public key must exist. The off-site worker makes it when it starts.'}
            </p>
          )}
          <div className={styles.rowActions}>
            <button
              type="button"
              className={`${ui.btn} ${ui.primary}`}
              disabled={supportedTypes.length === 0}
              onClick={() => setDialog({ editId: null })}
            >
              Add a destination
            </button>
            {supportedTypes.length === 0 && (
              <span className={ui.hint}>This server has no driver for any type.</span>
            )}
          </div>
        </>
      )}
      <p role="status" className={ui.hint} data-testid="dest-note">
        {note}
      </p>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      {dialog && q.data && (
        <DestinationDialog
          editId={dialog.editId}
          destinations={q.data}
          supportedTypes={supportedTypes}
          onClose={() => setDialog(null)}
          onSaved={(t) => {
            setNote(t);
            setDialog(null);
          }}
        />
      )}
      {deleting && (
        <Dialog
          title="Delete this destination?"
          onClose={() => setDeleting(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setDeleting(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={deleteBusy}
                onClick={() => void confirmDelete()}
              >
                {deleteBusy ? 'Deleting…' : 'Delete destination'}
              </button>
            </>
          }
        >
          <p>
            <strong>{deleting.name}</strong> and its stored secrets will be deleted. The copies already on the
            remote stay there; Launchpad will not prune them. Past runs stay in the history.
          </p>
        </Dialog>
      )}
    </SettingsSection>
  );
}
