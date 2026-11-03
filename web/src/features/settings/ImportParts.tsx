import { useState } from 'react';
import type { ImportJob, ImportSession } from '../../api/types';
import { cleanServerText } from '../../api/errors';
import ui from '../../styles/ui.module.css';
import styles from './import.module.css';
import settings from './settings.module.css';
import { importErrorText, jobErrorText, mismatchText, progressText } from './importText';
import { useImportActions } from '../../api/importer';

/** A call that can fail with a sentence. Shared by the steps. */
export function useAct() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  async function act(fn: () => Promise<unknown>, text: (e: unknown) => string = importErrorText) {
    setBusy(true);
    setError(null);
    try {
      await fn();
      return true;
    } catch (e) {
      setError(text(e));
      return false;
    } finally {
      setBusy(false);
    }
  }
  return { busy, error, act, clear: () => setError(null) };
}

export function ErrorLine({ text }: { text: string | null }) {
  if (!text) return null;
  return (
    <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
      {text}
    </p>
  );
}

/** The running job: phase, a bar, and Cancel. */
export function JobProgress({ job, label }: { job: ImportJob; label: string }) {
  const actions = useImportActions();
  const { act, error } = useAct();
  const known = job.total > 0;
  return (
    <div className={styles.block}>
      <p className={styles.okText}>{label}</p>
      <progress
        className={styles.progress}
        max={known ? job.total : undefined}
        value={known ? Math.min(job.done, job.total) : undefined}
        aria-label={label}
      />
      <p role="status" className={ui.hint}>
        {progressText(job)}
      </p>
      <div className={settings.actions}>
        <button
          type="button"
          className={ui.btn}
          disabled={job.cancelRequested}
          onClick={() => void act(() => actions.cancel(job.id))}
        >
          {job.cancelRequested ? 'Canceling...' : 'Cancel'}
        </button>
        <span className={ui.hint}>
          {job.kind === 'export'
            ? 'A canceled export keeps the files it made.'
            : 'A canceled job keeps no rows.'}
        </span>
      </div>
      <ErrorLine text={error} />
    </div>
  );
}

/** The result of a finished job that did not succeed, or the progress of a running one. */
export function JobFailure({ job }: { job: ImportJob }) {
  if (job.state !== 'failed' && job.state !== 'canceled') return null;
  const mismatch = job.errorCode === 'verify_mismatch';
  return (
    <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
      {mismatch ? mismatchText(job) : jobErrorText(job)}
    </p>
  );
}

/** Step 1 and step 3: how many files could not be downloaded, and the first 20 names. */
export function FailedFiles({ session }: { session: ImportSession }) {
  if (session.failedFiles <= 0) return null;
  const shown = session.failedFileNames.slice(0, 20);
  return (
    <div className={styles.block} data-testid="failed-files">
      <p role="status" className={styles.okText}>
        {session.failedFiles} {session.failedFiles === 1 ? 'file' : 'files'} could not be downloaded from
        Linear.
      </p>
      <p className={ui.hint}>
        Text that points at these files keeps its Linear link, so the verify step reports it later. The first{' '}
        {shown.length} {shown.length === 1 ? 'name is' : 'names are'} listed.
      </p>
      <ul className={styles.names}>
        {shown.map((f, i) => (
          <li key={`${f.name}-${i}`}>
            <span className={styles.mono}>{cleanServerText(f.name) || 'unnamed file'}</span>
            {cleanServerText(f.reason) ? `: ${cleanServerText(f.reason)}` : ''}
          </li>
        ))}
      </ul>
    </div>
  );
}

export function ExportSummary({ session }: { session: ImportSession }) {
  const entries = Object.entries(session.exportCounts).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return null;
  return (
    <div className={styles.block}>
      <p className={styles.okText}>The export is on the server.</p>
      <dl className={settings.facts}>
        {entries.map(([name, n]) => (
          <div key={name} style={{ display: 'contents' }}>
            <dt>{name.replace(/_/g, ' ')}</dt>
            <dd>{n}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
