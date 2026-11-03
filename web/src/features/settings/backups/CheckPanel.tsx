import { useEffect, useId, useRef, useState } from 'react';
import { useBackupRun } from '../../../api/backups';
import type { BackupRun } from '../../../api/types';
import { backupErrorText, readSteps } from './backupText';
import { PollError, RunBox } from './StepList';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

/** What a probe found. */
export interface ProbeInfo {
  kind: string;
  fingerprint: string;
  chainVerified: boolean | null;
  failedCode: string;
}

export function probeInfo(run: BackupRun | undefined): ProbeInfo | null {
  if (!run || (run.status !== 'succeeded' && run.status !== 'failed' && run.status !== 'interrupted'))
    return null;
  const s = readSteps(run).find((x) => x.name === 'probe');
  if (!s || !s.ok)
    return {
      kind: '',
      fingerprint: '',
      chainVerified: null,
      failedCode: s?.code || run.errorCode || 'server_error',
    };
  return { kind: s.kind, fingerprint: s.fingerprint, chainVerified: s.chainVerified, failedCode: '' };
}

/**
 * The two-step check of decisions #36. Probe: a handshake with no credentials; it shows the certificate or the
 * host key. The admin confirms an untrusted fingerprint, or accepts an unencrypted connection, in words. Test: the
 * real sign-in and a small file. The page asks for these confirmations; the server judges the rest.
 */
export function CheckPanel({
  anchorLabel,
  probe,
  test,
  probeBlocked,
  testBlocked,
  onProbed,
  pinText = '',
  testResetKey = 0,
}: {
  anchorLabel: string;
  /** The pin / host key as it stands in the form. A hand edit that differs from the probed one drops the tick. */
  pinText?: string;
  /** Changes when a secret of the form is edited: the Test result shown belonged to the old secrets. */
  testResetKey?: number;
  probe: () => Promise<BackupRun>;
  /** `confirmed` is the fingerprint the admin accepted, if any. */
  test: (confirmed: string | undefined) => Promise<BackupRun>;
  /** A fixed reason why Probe cannot start, or null. */
  probeBlocked: string | null;
  /** A fixed reason why Test cannot start, or null. */
  testBlocked: string | null;
  onProbed?: () => void;
}) {
  const [probeId, setProbeId] = useState<string | null>(null);
  const [testId, setTestId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<'probe' | 'test' | null>(null);
  const [ackFp, setAckFp] = useState(false);
  const [ackPlain, setAckPlain] = useState(false);
  const [seenReset, setSeenReset] = useState(testResetKey);
  if (seenReset !== testResetKey) {
    // Render-time reset: the old Test result must not stay beside edited secrets. The probe is not affected.
    setSeenReset(testResetKey);
    setTestId(null);
    setError(null);
  }
  // A Test response that lands after a secret edit belongs to the old secrets: it is dropped (token compare).
  const testToken = useRef(0);
  useEffect(() => {
    testToken.current += 1;
  }, [testResetKey]);
  const probeRun = useBackupRun(probeId);
  const testRun = useBackupRun(testId);
  const ackId = useId();
  const info = probeInfo(probeRun.data);
  const needsFp =
    !!info &&
    !info.failedCode &&
    info.fingerprint !== '' &&
    (info.kind === 'ssh' || (info.kind === 'tls' && info.chainVerified === false));
  const isPlain = !!info && !info.failedCode && info.kind === 'plain';
  // The tick holds only while the pin box is empty or holds the probed fingerprint (a hand-edited pin is not the
  // one the admin compared).
  const pinNow = pinText.trim();
  const pinDiffers = needsFp && pinNow !== '' && pinNow !== info.fingerprint;
  const fpTicked = ackFp && !pinDiffers;
  const confirmed = needsFp && fpTicked ? info.fingerprint : undefined;
  const confirmMissing = (needsFp && !fpTicked) || (isPlain && !ackPlain);

  async function start(kind: 'probe' | 'test') {
    setError(null);
    setBusy(kind);
    const startedToken = testToken.current;
    try {
      if (kind === 'probe') {
        setAckFp(false);
        setAckPlain(false);
        setTestId(null);
        const run = await probe();
        setProbeId(run.id);
        onProbed?.();
      } else {
        const token = testToken.current;
        const run = await test(confirmed);
        if (token === testToken.current) setTestId(run.id);
      }
    } catch (err) {
      if (kind === 'probe' || startedToken === testToken.current) setError(backupErrorText(err, 'check'));
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className={styles.check}>
      <h3 className={styles.checkTitle}>Check the connection</h3>
      <ol className={styles.checkIntro}>
        <li>
          <strong>Probe</strong> looks at the server. It sends no password and no data.
        </li>
        <li>
          <strong>Test</strong> signs in, writes a small file, reads it back and deletes it.
        </li>
      </ol>
      <div className={styles.rowActions}>
        <button
          type="button"
          className={ui.btn}
          disabled={busy !== null || probeBlocked !== null}
          onClick={() => void start('probe')}
        >
          {busy === 'probe' ? 'Probing…' : 'Probe the server'}
        </button>
      </div>
      {probeBlocked && <p className={ui.hint}>{probeBlocked}</p>}
      <RunBox run={probeRun.data} label="Probe" />
      <PollError query={probeRun} />
      {info && !info.failedCode && info.kind === 'tls' && info.chainVerified === true && (
        <p className={styles.okText} data-testid="probe-trusted">
          The certificate is valid for this host. No pin is needed.
        </p>
      )}
      {needsFp && (
        <div
          className={`${ui.banner}`}
          role="group"
          aria-label="Confirm the fingerprint"
          data-testid="probe-untrusted"
        >
          <div>
            <p>
              <strong>
                {info.kind === 'ssh'
                  ? 'This is the SSH host key of the server.'
                  : 'The certificate of this server is not trusted by the system.'}
              </strong>{' '}
              Its {info.kind === 'ssh' ? 'host key' : 'key'} fingerprint is:
            </p>
            <code className={styles.fp} data-testid="probe-fingerprint">
              {info.fingerprint}
            </code>
            <p>
              Compare it with the fingerprint that the owner of the server gives you. If it is the same, tick
              the box. The page then saves it as the {anchorLabel}.
              {info.kind === 'tls' &&
                ' A pin replaces the check of the certificate chain and the host name. The expiry date is still checked.'}
            </p>
            <label className={styles.check1}>
              <input
                id={ackId}
                type="checkbox"
                checked={fpTicked}
                onChange={(e) => setAckFp(e.target.checked)}
              />
              <span>I compared this fingerprint and it is the right server.</span>
            </label>
            {pinDiffers && (
              <p role="status" className={ui.hint} data-testid="pin-differs">
                The {anchorLabel} box does not hold this fingerprint. Clear the box, or paste this fingerprint
                in it, to confirm.
              </p>
            )}
          </div>
        </div>
      )}
      {isPlain && (
        <div
          className={`${ui.banner} ${ui.errorBanner}`}
          role="group"
          aria-label="Not encrypted in transit"
          data-testid="probe-plain"
        >
          <div>
            <p>
              <strong>Not encrypted in transit.</strong> This address starts with http://. Anyone on the
              network path can read what is sent, and can replay a signed link for up to one hour. The backup
              file itself is still encrypted. Use https where the service has it.
            </p>
            <label className={styles.check1}>
              <input type="checkbox" checked={ackPlain} onChange={(e) => setAckPlain(e.target.checked)} />
              <span>I understand that this connection is not encrypted in transit.</span>
            </label>
          </div>
        </div>
      )}
      <div className={styles.rowActions}>
        <button
          type="button"
          className={ui.btn}
          disabled={busy !== null || testBlocked !== null || confirmMissing}
          onClick={() => void start('test')}
        >
          {busy === 'test' ? 'Starting…' : 'Test the connection'}
        </button>
      </div>
      {testBlocked && <p className={ui.hint}>{testBlocked}</p>}
      {confirmMissing && <p className={ui.hint}>Tick the box above before you test.</p>}
      <RunBox run={testRun.data} label="Test" />
      <PollError query={testRun} />
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
    </div>
  );
}
