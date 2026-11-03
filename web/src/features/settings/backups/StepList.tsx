import { useEffect, useState } from 'react';
import { isFinalRun } from '../../../api/backups';
import type { BackupRun } from '../../../api/types';
import { backupErrorText, codeText, readSteps, RUN_STATUS, stepLabel } from './backupText';
import styles from './backups.module.css';
import ui from '../../../styles/ui.module.css';

/** After this long without a start, the page says that the off-site worker may not run. */
const STUCK_MS = 45_000;

/**
 * A probe, test or exchange run: its status and, for a test, each step with its result. Every sentence is fixed
 * (backupText.ts); a code of the server maps to a sentence of ours and never shows a remote's words.
 */
export function RunBox({ run, label }: { run: BackupRun | undefined; label: string }) {
  const [stuck, setStuck] = useState(false);
  const id = run?.id;
  const waiting = run ? run.status === 'queued' || run.status === 'waiting_dump' : false;
  useEffect(() => {
    setStuck(false);
    if (!id || !waiting) return;
    const t = setTimeout(() => setStuck(true), STUCK_MS);
    return () => clearTimeout(t);
  }, [id, waiting]);
  if (!run) return null;
  const steps = readSteps(run);
  const final = isFinalRun(run);
  return (
    <div className={styles.runBox} data-testid={`run-${label}`}>
      <p role="status" aria-live="polite" className={styles.runHead}>
        <strong>{label}:</strong> {RUN_STATUS[run.status]}
        {!final && '…'}
      </p>
      {stuck && (
        <p className={`${ui.banner}`} role="status">
          The off-site worker has not started this yet. Check that the offsite service runs.
        </p>
      )}
      {steps.length > 0 && (
        <ol className={styles.steps} aria-label={`${label} steps`}>
          {steps.map((s, i) => (
            <li key={`${s.name}-${i}`} className={s.ok ? styles.stepOk : styles.stepFail}>
              <span className={styles.stepName}>{stepLabel(s.name)}</span>{' '}
              <span className={styles.stepState}>{s.ok ? 'OK' : 'Failed'}</span>
              {!s.ok && s.code && <span className={styles.stepCode}>{codeText(s.code)}</span>}
            </li>
          ))}
        </ol>
      )}
      {final && run.status !== 'succeeded' && steps.length === 0 && run.errorCode && (
        <p className={styles.stepCode}>{codeText(run.errorCode)}</p>
      )}
      {final && run.status === 'succeeded' && label !== 'Probe' && (
        <p className={styles.okText}>It worked.</p>
      )}
    </div>
  );
}

/** A failed read of a run. After five failures in a row the page has stopped asking, and says so. */
export function PollError({ query }: { query: { isError: boolean; error: unknown; gaveUp: boolean } }) {
  if (!query.isError) return null;
  return (
    <p className={`${ui.banner} ${ui.errorBanner}`} role="alert">
      {query.gaveUp
        ? 'The page stopped asking for the result. Check the run history below.'
        : backupErrorText(query.error, 'check')}
    </p>
  );
}
