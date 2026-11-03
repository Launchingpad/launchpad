import { useId, useState, type FormEvent } from 'react';
import { useSaveBackupSettings } from '../../../api/backups';
import type { BackupSettings } from '../../../api/types';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { backupErrorText } from './backupText';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

/** The notice settings (`PUT /platform/backups/settings` takes all three fields). */
export function BackupNotices({ settings, mailReady }: { settings: BackupSettings; mailReady: boolean }) {
  const save = useSaveBackupSettings();
  const [fail, setFail] = useState(settings.notifyOnFailure);
  const [ok, setOk] = useState(settings.notifyOnSuccess);
  const [hours, setHours] = useState(String(settings.staleAlertHours));
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const id = useId();
  const dirty =
    fail !== settings.notifyOnFailure ||
    ok !== settings.notifyOnSuccess ||
    hours.trim() !== String(settings.staleAlertHours);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSaved(false);
    const h = hours.trim();
    if (!/^\d{1,4}$/.test(h) || Number(h) > 8760) {
      setError('Enter the hours as a whole number from 0 to 8760. Use 0 to turn the stale alert off.');
      return;
    }
    setBusy(true);
    try {
      await save({ notifyOnFailure: fail, notifyOnSuccess: ok, staleAlertHours: Number(h) });
      setSaved(true);
    } catch (err) {
      setError(backupErrorText(err, 'settings'));
    } finally {
      setBusy(false);
    }
  }

  return (
    <SettingsSection
      title="Notices"
      id="backup-notices"
      description="Mail goes to every platform admin who has an email address."
    >
      <SettingsCard padded>
        <form onSubmit={(e) => void submit(e)} noValidate aria-label="Backup notices">
          {!mailReady && (
            <p className={ui.banner} role="note">
              Email is not set up, so none of these mails is sent now.
            </p>
          )}
          <label className={styles.check1}>
            <input
              type="checkbox"
              checked={fail}
              onChange={(e) => {
                setFail(e.target.checked);
                setSaved(false);
              }}
            />
            <span>Mail when a backup fails</span>
          </label>
          <label className={styles.check1}>
            <input
              type="checkbox"
              checked={ok}
              onChange={(e) => {
                setOk(e.target.checked);
                setSaved(false);
              }}
            />
            <span>Mail when a backup succeeds</span>
          </label>
          <div className={ui.field}>
            <label htmlFor={id}>Stale alert after (hours)</label>
            <input
              id={id}
              className={`${ui.input} ${styles.num}`}
              value={hours}
              inputMode="numeric"
              onChange={(e) => {
                setHours(e.target.value);
                setSaved(false);
              }}
              aria-describedby={`${id}-h`}
            />
            <span id={`${id}-h`} className={ui.hint}>
              0 to 8760. A destination with no good backup for this long gets a warning and a mail. The alert
              never comes sooner than twice the schedule interval. 0 turns it off.
            </span>
          </div>
          {error && (
            <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
              {error}
            </p>
          )}
          <div className={styles.rowActions}>
            <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy || !dirty}>
              {busy ? 'Saving…' : 'Save notices'}
            </button>
            {saved && !dirty && (
              <span role="status" className={ui.hint}>
                Notices saved.
              </span>
            )}
          </div>
        </form>
      </SettingsCard>
    </SettingsSection>
  );
}
