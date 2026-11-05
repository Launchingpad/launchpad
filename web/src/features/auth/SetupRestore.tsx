import { useEffect, useRef, useState, type FormEvent } from 'react';
import { commitRestoreSession, restoreFromMoveFile, type RestoreResult } from '../../api/auth';
import { ApiError } from '../../api/errors';
import { isUnknownOutcome, moveRestoreError, RESTORE_UNKNOWN } from '../settings/moveText';
import styles from './setup.module.css';
import ui from '../../styles/ui.module.css';

/**
 * Setup, option two: restore from a move file (decisions-T46, LAUN-250). The setup link token comes from the parent
 * (it stays in the URL fragment, then in memory). The passphrase is typed here, sent in a header, and kept only in
 * this component's state: never in a URL, storage, a query or a log. The file streams from disk (XHR `send(File)`).
 */
export function SetupRestore({
  token,
  onRestored,
  onLinkProblem,
  onBusy,
}: {
  token: string;
  onRestored: (r: RestoreResult) => void;
  /** The link is wrong or the install has a user: the parent shows its own page. */
  onLinkProblem: (kind: 'invalid' | 'complete') => void;
  /** True from the start of an upload until it ends: the parent keeps the page in place meanwhile. */
  onBusy: (busy: boolean) => void;
}) {
  const [file, setFile] = useState<File | null>(null);
  const [passphrase, setPassphrase] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState(0);
  const abort = useRef<AbortController | null>(null);
  const cancelled = useRef(false);

  const mounted = useRef(true);
  // Once the whole body is sent the server may finish the restore (and use up the link): from then on the request
  // is NOT aborted, so the answer, and the only copy of the first token, still reaches this page.
  const sent = useRef(false);
  const inFlight = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      if (!sent.current) abort.current?.abort();
    };
  }, []);

  // The 100% text says "do not close this page": ask the browser to confirm while a restore runs.
  useEffect(() => {
    if (!busy) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, [busy]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (inFlight.current) return;
    if (!file || passphrase.trim() === '') {
      setError('Choose the move file and enter the passphrase.');
      return;
    }
    setError(null);
    inFlight.current = true;
    sent.current = false;
    setBusy(true);
    onBusy(true);
    setProgress(0);
    cancelled.current = false;
    const ctl = new AbortController();
    abort.current = ctl;
    try {
      const r = await restoreFromMoveFile({
        token,
        passphrase: passphrase.trim(),
        file,
        onProgress: setProgress,
        onSent: () => {
          sent.current = true;
        },
        signal: ctl.signal,
      });
      // Only a page that is still here shows the token, so only it may start the session.
      if (mounted.current) {
        setPassphrase('');
        commitRestoreSession(r);
        onRestored(r);
      }
    } catch (err) {
      if (!mounted.current) return;
      if (cancelled.current && !sent.current) {
        setError('The upload was cancelled. Nothing was loaded.');
      } else if (err instanceof ApiError && err.status === 401 && err.code === 'invalid_setup_token') {
        onLinkProblem('invalid');
      } else if (err instanceof ApiError && err.status === 409 && err.code === 'setup_complete') {
        onLinkProblem('complete');
      } else if (sent.current && isUnknownOutcome(err)) {
        setError(RESTORE_UNKNOWN);
      } else {
        setError(moveRestoreError(err));
      }
    } finally {
      abort.current = null;
      inFlight.current = false;
      if (mounted.current) setBusy(false);
      onBusy(false);
    }
  }

  const percent = Math.round(progress * 100);
  return (
    <form onSubmit={(e) => void submit(e)} noValidate>
      <p className={ui.hint}>
        Load a move file from another Launchpad server. This server must run the same release. Everything in
        the file replaces a new setup: you do not make a user, an organisation or a team.
      </p>
      <div className={ui.field}>
        <label htmlFor="restore-file">Move file</label>
        <input
          id="restore-file"
          type="file"
          accept=".lpmove,application/octet-stream"
          disabled={busy}
          onChange={(e) => setFile(e.target.files?.[0] ?? null)}
        />
      </div>
      <div className={ui.field}>
        <label htmlFor="restore-passphrase">Passphrase</label>
        <input
          id="restore-passphrase"
          className={ui.input}
          type="password"
          value={passphrase}
          onChange={(e) => setPassphrase(e.target.value)}
          disabled={busy}
          autoComplete="new-password"
          autoCapitalize="none"
          spellCheck={false}
        />
        <span className={ui.hint}>Eight groups of four characters. Case and spaces do not matter.</span>
      </div>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
      {busy && (
        <div className={styles.progressWrap}>
          <progress max={100} value={percent} aria-label="Upload progress" className={styles.progress} />
          <span role="status" className={ui.hint}>
            {percent < 100
              ? `Uploading: ${percent}%`
              : 'Upload done. The server is loading the data. This can take a few minutes. Do not close this page.'}
          </span>
        </div>
      )}
      <div className={styles.actions}>
        <button
          type="submit"
          className={`${ui.btn} ${ui.primary}`}
          disabled={busy}
          style={{ flex: 1, justifyContent: 'center' }}
        >
          {busy ? 'Restoring…' : 'Restore'}
        </button>
        {busy && percent < 100 && (
          <button
            type="button"
            className={ui.btn}
            onClick={() => {
              cancelled.current = true;
              abort.current?.abort();
            }}
          >
            Cancel
          </button>
        )}
      </div>
    </form>
  );
}
