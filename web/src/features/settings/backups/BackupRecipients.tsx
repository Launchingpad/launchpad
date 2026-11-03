import { useRef, useState, type FormEvent } from 'react';
import { useBackupRecipients, useRecipientActions } from '../../../api/backups';
import type { BackupRecipient } from '../../../api/types';
import { fullDate } from '../../../lib/time';
import { Dialog } from '../../../ui/Dialog';
import { SkeletonRows } from '../../../ui/States';
import { FixedError } from '../FixedError';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { backupErrorText } from './backupText';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

function stateText(r: BackupRecipient): string {
  if (r.state === 'pending') return `Pending: held until ${fullDate(r.effectiveAt)}`;
  if (r.state === 'removing') return `Removing: active until ${fullDate(r.removeAt)}`;
  return 'Active';
}

/**
 * The age recipients (decisions-T16 #20). A new key waits 24 hours before backups are encrypted to it; a removal
 * is also scheduled 24 hours ahead. The fingerprint is the full sha256 of the key text; the page shows its first 12
 * characters and the whole value on demand.
 */
export function BackupRecipients() {
  const q = useBackupRecipients();
  const actions = useRecipientActions();
  const [key, setKey] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState<BackupRecipient | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const cancelling = useRef(new Set<string>());
  const [cancelBusy, setCancelBusy] = useState<string | null>(null);

  async function add(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setNote(null);
    const k = key.trim();
    if (!/^age1[0-9a-z]{50,70}$/.test(k)) {
      setError('Enter an age public key. It starts with "age1" and has no spaces.');
      return;
    }
    setBusy(true);
    try {
      const r = await actions.add(k);
      setKey('');
      setNote(
        r.state === 'pending'
          ? `Key ${r.shortFingerprint} is added. Backups are encrypted to it from ${fullDate(r.effectiveAt)}.`
          : `Key ${r.shortFingerprint} is added and active.`,
      );
    } catch (err) {
      setError(backupErrorText(err, 'recipient'));
    } finally {
      setBusy(false);
    }
  }

  async function cancel(r: BackupRecipient) {
    if (cancelling.current.has(r.fingerprint)) return; // one request for two clicks
    cancelling.current.add(r.fingerprint);
    setCancelBusy(r.fingerprint);
    setError(null);
    setNote(null);
    try {
      await actions.cancel(r.fingerprint);
      setNote(`The pending change of key ${r.shortFingerprint} is cancelled.`);
    } catch (err) {
      setError(backupErrorText(err, 'recipientCancel'));
    } finally {
      cancelling.current.delete(r.fingerprint);
      setCancelBusy(null);
    }
  }

  async function confirmRemove() {
    if (!removing || removeBusy) return;
    setRemoveBusy(true);
    setError(null);
    setNote(null);
    const r = removing;
    try {
      await actions.remove(r.fingerprint);
      setNote(`Key ${r.shortFingerprint} will be removed in 24 hours. Another platform admin can cancel it.`);
    } catch (err) {
      setError(backupErrorText(err, 'recipientRemove'));
    } finally {
      setRemoveBusy(false);
      setRemoving(null);
    }
  }

  return (
    <SettingsSection
      title="Encryption keys (recipients)"
      id="backup-recipients"
      description="Backups are encrypted to every active key. To stop a stolen session from swapping keys unseen, a new key is held for 24 hours and a removal waits 24 hours. Every change is mailed to the platform admins."
    >
      {q.isPending ? (
        <SkeletonRows n={2} />
      ) : q.isError ? (
        <FixedError error={q.error} onRetry={() => void q.refetch()} title="Could not load the keys" />
      ) : (
        <>
          <SettingsCard>
            {q.data.items.length === 0 ? (
              <p className={`${ui.hint} ${styles.pad}`}>No key yet. Add one to make backups possible.</p>
            ) : (
              <ul className={styles.recipList} aria-label="Encryption keys">
                {q.data.items.map((r) => (
                  <li key={r.fingerprint} className={styles.recip}>
                    <div className={styles.destMain}>
                      <div className={styles.destTitle}>
                        <code>{r.shortFingerprint}</code>
                        <span className={`${styles.tag} ${r.state === 'active' ? styles.tagOn : ''}`}>
                          {r.state === 'pending' ? 'Pending' : r.state === 'removing' ? 'Removing' : 'Active'}
                        </span>
                      </div>
                      <div className={ui.hint}>{stateText(r)}</div>
                      <details>
                        <summary>Show the key and its fingerprint</summary>
                        <code className={styles.fp}>{r.publicKey}</code>
                        <div className={ui.hint}>Fingerprint (sha256):</div>
                        <code className={styles.fp}>{r.fingerprint}</code>
                      </details>
                    </div>
                    <div className={styles.destActions}>
                      {r.state === 'active' && (
                        <button
                          type="button"
                          className={`${ui.btn} ${ui.danger}`}
                          aria-label={`Remove key ${r.shortFingerprint}`}
                          onClick={() => setRemoving(r)}
                        >
                          Remove
                        </button>
                      )}
                      {r.state === 'pending' && (
                        <button
                          type="button"
                          className={ui.btn}
                          aria-label={`Cancel the add of key ${r.shortFingerprint}`}
                          aria-disabled={cancelBusy === r.fingerprint || undefined}
                          onClick={() => void cancel(r)}
                        >
                          Cancel the add
                        </button>
                      )}
                      {r.state === 'removing' && (
                        <button
                          type="button"
                          className={ui.btn}
                          aria-label={`Cancel the removal of key ${r.shortFingerprint}`}
                          aria-disabled={cancelBusy === r.fingerprint || undefined}
                          onClick={() => void cancel(r)}
                        >
                          Cancel the removal
                        </button>
                      )}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </SettingsCard>
          <SettingsCard padded>
            <form onSubmit={(e) => void add(e)} noValidate aria-label="Add an encryption key">
              <div className={ui.field}>
                <label htmlFor="backup-recipient-key">Public key (age1…)</label>
                <input
                  id="backup-recipient-key"
                  className={ui.input}
                  value={key}
                  onChange={(e) => setKey(e.target.value)}
                  autoComplete="off"
                  autoCapitalize="none"
                  spellCheck={false}
                  aria-describedby="backup-recipient-hint"
                />
                <span id="backup-recipient-hint" className={ui.hint}>
                  {q.data.everHadRecipient
                    ? 'This key is held for 24 hours before backups use it.'
                    : 'The first key is active at once.'}{' '}
                  At most {q.data.limit} keys.
                </span>
              </div>
              <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
                {busy ? 'Adding…' : 'Add key'}
              </button>
            </form>
          </SettingsCard>
        </>
      )}
      <p role="status" className={ui.hint} data-testid="recipient-note">
        {note}
      </p>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      {removing && (
        <Dialog
          title="Remove this key?"
          onClose={() => setRemoving(null)}
          footer={
            <>
              <button type="button" className={ui.btn} onClick={() => setRemoving(null)}>
                Cancel
              </button>
              <button
                type="button"
                className={`${ui.btn} ${ui.danger}`}
                disabled={removeBusy}
                onClick={() => void confirmRemove()}
              >
                {removeBusy ? 'Scheduling…' : 'Schedule the removal'}
              </button>
            </>
          }
        >
          <p>
            Key <code>{removing.shortFingerprint}</code> stays active for 24 hours, then backups stop using
            it. Another platform admin can cancel the removal in that time. While a destination is enabled, at
            least one other key must stay active.
          </p>
          <p className={ui.hint}>
            For a removal at once, run{' '}
            <code>server backup-recipient-remove --fingerprint {removing.fingerprint}</code> on the host.
          </p>
        </Dialog>
      )}
    </SettingsSection>
  );
}
