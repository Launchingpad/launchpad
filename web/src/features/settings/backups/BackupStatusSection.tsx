import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../../api/org';
import type { BackupStatus } from '../../../api/types';
import { fullDate, relativeTime } from '../../../lib/time';
import { SettingsCard, SettingsSection } from '../SettingsParts';
import { BANNERS, codeText, safeCode, sizeText } from './backupText';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

/** The banners of the status route (LAUN-69, LAUN-75) with the destinations they name. Every sentence is fixed. */
export function BackupBanners({ status }: { status: BackupStatus }) {
  const org = useOrgSlug();
  if (status.banners.length === 0) return null;
  return (
    <section aria-label="Backup warnings" className={styles.banners}>
      {status.banners.map((b) => {
        const def = BANNERS[b];
        if (!def) return null;
        return (
          <div
            key={b}
            role="status"
            data-banner={b}
            className={`${ui.banner} ${def.level === 'error' ? ui.errorBanner : ''}`}
          >
            <div>
              {def.text}
              {b === 'mail_off' && (
                <>
                  {' '}
                  <Link to={`/${org}/settings/platform`}>Open the Platform page</Link>.
                </>
              )}
              {b === 'backup_stale' && status.staleDestinations.length > 0 && (
                <ul className={styles.bannerList}>
                  {status.staleDestinations.map((d) => (
                    <li key={d.id}>
                      <strong>{d.name}</strong>:{' '}
                      {d.lastSuccessAt
                        ? `last good backup ${relativeTime(d.lastSuccessAt)}`
                        : 'no good backup yet'}
                      . Alert after {d.thresholdHours} hours.
                    </li>
                  ))}
                </ul>
              )}
              {b === 'files_not_backed_up' && status.filesNotBackedUp.length > 0 && (
                <ul className={styles.bannerList}>
                  {status.filesNotBackedUp.map((d) => (
                    <li key={d.id}>
                      <strong>{d.name}</strong>:{' '}
                      {d.lastBackupAt
                        ? `last files backup ${relativeTime(d.lastBackupAt)}`
                        : 'no files backup yet'}
                      .
                    </li>
                  ))}
                </ul>
              )}
              {b === 'backup_run_failed' && status.failedDestinations.length > 0 && (
                <ul className={styles.bannerList}>
                  {status.failedDestinations.map((d) => (
                    <li key={d.id}>
                      <strong>{d.name}</strong>: failed {relativeTime(d.finishedAt)}.{' '}
                      {codeText(d.errorCode) || 'The run failed.'}
                      {safeCode(d.errorCode) && <code className={styles.code}> {safeCode(d.errorCode)}</code>}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        );
      })}
      {status.destinationsUnreadable > 0 && (
        <div role="status" className={`${ui.banner} ${ui.errorBanner}`}>
          <div>
            {status.destinationsUnreadable === 1
              ? 'One destination has secrets that this server cannot open.'
              : `${status.destinationsUnreadable} destinations have secrets that this server cannot open.`}{' '}
            Edit each one and enter its secrets again.
          </div>
        </div>
      )}
    </section>
  );
}

export function BackupStatusSection({
  status,
  keysSaved,
  onKeysSaved,
}: {
  status: BackupStatus;
  keysSaved: boolean;
  onKeysSaved: (v: boolean) => void;
}) {
  const [copied, setCopied] = useState<'ok' | 'fail' | null>(null);
  const dump = status.newestDump;

  async function copy() {
    try {
      await navigator.clipboard.writeText(status.signingPublic);
      setCopied('ok');
    } catch {
      setCopied('fail');
    }
  }

  return (
    <>
      <SettingsSection title="Status" id="backup-status">
        <SettingsCard padded>
          <dl className={styles.facts}>
            <dt>Newest local dump</dt>
            <dd data-testid="newest-dump">
              {dump
                ? dump.status === 'ok'
                  ? `${relativeTime(dump.finishedAt ?? dump.startedAt)}, ${sizeText(dump.bytes)}${dump.fileRemoved ? ' (the local file was pruned)' : ''}`
                  : dump.status === 'running'
                    ? 'Running now'
                    : 'The newest dump failed'
                : 'No dump yet'}
              {dump && <span className={ui.hint}> {fullDate(dump.finishedAt ?? dump.startedAt)}</span>}
            </dd>
            <dt>Destinations</dt>
            <dd>
              {status.enabledDestinations} enabled of {status.destinations}
            </dd>
            <dt>Encryption keys</dt>
            <dd>{status.activeRecipients} active now</dd>
            <dt>Email for notices</dt>
            <dd>{status.mailReady ? 'Ready' : 'Not set up'}</dd>
          </dl>
        </SettingsCard>
      </SettingsSection>

      <SettingsSection
        title="Encryption"
        id="backup-encryption"
        description="Each backup is encrypted to your public keys (age) and signed by this install. Launchpad never holds a private key."
      >
        <SettingsCard padded>
          <h3 className={styles.formTitle}>Signing public key</h3>
          {status.signingPublic ? (
            <>
              <code className={styles.fp} data-testid="signing-public">
                {status.signingPublic}
              </code>
              <div className={styles.rowActions}>
                <button type="button" className={ui.btn} onClick={() => void copy()}>
                  Copy
                </button>
                <span role="status" className={ui.hint}>
                  {copied === 'ok'
                    ? 'Copied.'
                    : copied === 'fail'
                      ? 'Could not copy. Select the text and copy it.'
                      : ''}
                </span>
              </div>
            </>
          ) : (
            <p className={ui.hint}>
              The signing key does not exist yet. The off-site worker makes it when it first starts.
            </p>
          )}
          <p className={ui.hint}>
            Keep a copy of it away from this host. A restore uses it to prove that a backup is genuine. It is
            public: it is not a secret.
          </p>
          <h3 className={styles.formTitle}>How to make an encryption key</h3>
          <p>
            On a computer you trust, run <code>age-keygen</code>, or run{' '}
            <code>deploy/deploy.sh --backup-keygen</code> on the host (it needs a terminal and shows the
            private half once). Add the public half (it starts with <code>age1</code>) below. Keep the private
            half off this host.
          </p>
          <p className={`${ui.banner} ${ui.errorBanner}`} role="note">
            <span>
              <strong>Lose the private key and the backups cannot be read. Nobody can recover them.</strong>{' '}
              Also keep the credentials of each destination, and its address and folder, away from this host.
            </span>
          </p>
          <label className={styles.check1}>
            <input type="checkbox" checked={keysSaved} onChange={(e) => onKeysSaved(e.target.checked)} />
            <span>I have saved the private key and the signing public key.</span>
          </label>
          <p className={ui.hint}>
            This box only unlocks the Enable buttons on this page. The server checks the rest.
          </p>
        </SettingsCard>
      </SettingsSection>
    </>
  );
}
