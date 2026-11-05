import { useEffect, useId, useRef, useState } from 'react';
import { apiBase } from '../../env';
import { startMoveExport } from '../../api/queries';
import type { components } from '../../api/schema';
import { useIsPlatformAdmin } from './access';
import { isDownloadPath, moveExportError } from './moveText';
import { Dialog } from '../../ui/Dialog';
import { NoAccess, SettingsCard, SettingsPage, SettingsSection } from './SettingsParts';
import styles from './move.module.css';
import ui from '../../styles/ui.module.css';

type Started = components['schemas']['MoveExportStarted'];

/**
 * Settings, Platform, Move server (decisions-T46, LAUN-250). The passphrase of an export is shown ONCE. It lives in
 * the state of `ExportResult` only: not in the query cache (a plain function makes the call), not in storage, not
 * in a URL, not in a log. Leaving the page unmounts it and the value is gone.
 */
export function MoveServerPage() {
  const platformAdmin = useIsPlatformAdmin();
  if (!platformAdmin) {
    return (
      <NoAccess
        title="You need platform admin access."
        hint="Only the person who runs this install can open this page."
      />
    );
  }
  return <MoveServerView />;
}

function startDownload(path: string, filename: string): boolean {
  // Only the export route of this server, one ticket segment. The ticket in the path is single use.
  if (!isDownloadPath(path)) return false;
  const a = document.createElement('a');
  a.href = `${apiBase()}${path}`;
  a.download = filename;
  a.rel = 'noopener';
  a.style.display = 'none';
  document.body.appendChild(a);
  a.click();
  a.remove();
  return true;
}

function MoveServerView() {
  const attachId = useId();
  const [attach, setAttach] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [started, setStarted] = useState<Started | null>(null);
  const [made, setMade] = useState(false);

  const inFlight = useRef(false);
  async function make() {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError(null);
    try {
      const r = await startMoveExport(attach);
      setStarted(r);
      setMade(true);
    } catch (err) {
      setError(moveExportError(err));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  }

  return (
    <SettingsPage
      title="Move server"
      description="Make one encrypted file with all data of this install, to load it on a new server."
      wide
    >
      {started && <ExportResult started={started} onDone={() => setStarted(null)} />}
      {made && <NextSteps />}
      {!started && (
        <SettingsSection title="Make a move file">
          <SettingsCard padded>
            <p className={ui.hint}>
              The file holds every organisation, user, issue and setting of this install, with its secrets. A
              passphrase protects it. The passphrase is shown one time, after you make the file.
            </p>
            <div className={styles.check}>
              <input
                id={attachId}
                type="checkbox"
                checked={attach}
                onChange={(e) => setAttach(e.target.checked)}
              />
              <label htmlFor={attachId}>Include attachments</label>
            </div>
            <span className={ui.hint}>
              Attachments can make the file much bigger. Without them, files on issues are not moved.
            </span>
            {error && (
              <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
                {error}
              </p>
            )}
            <div className={styles.actions}>
              <button
                type="button"
                className={`${ui.btn} ${ui.primary}`}
                disabled={busy}
                onClick={() => void make()}
              >
                {busy ? 'Making the file…' : 'Make move file'}
              </button>
            </div>
          </SettingsCard>
        </SettingsSection>
      )}
    </SettingsPage>
  );
}

function ExportResult({ started, onDone }: { started: Started; onDone: () => void }) {
  const [saved, setSaved] = useState(false);
  const [nudge, setNudge] = useState(false);
  const [copyNote, setCopyNote] = useState<string | null>(null);
  const [downloadNote, setDownloadNote] = useState<string | null>(null);
  const { passphrase, downloadPath, filename } = started;

  // Start the download once, when the passphrase appears. The ticket works one time.
  const began = useRef(false);
  useEffect(() => {
    if (began.current) return;
    began.current = true;
    if (!startDownload(downloadPath, filename))
      setDownloadNote('The download did not start. Close this window and make a new file.');
  }, [downloadPath, filename]);

  // The server never shows the passphrase again, so warn before the page closes.
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(passphrase);
      setCopyNote('Copied.');
    } catch {
      setCopyNote('Could not copy. Select the passphrase and copy it by hand.');
    }
  }

  // The copy-once dialog (web-decisions, API tokens #5): Escape, the close button and a click outside cannot close
  // it before the person confirms they saved the passphrase.
  function close() {
    if (saved) onDone();
    else setNudge(true);
  }

  return (
    <Dialog
      title="Save your passphrase"
      width={560}
      onClose={close}
      footer={
        <button type="button" className={`${ui.btn} ${ui.primary}`} disabled={!saved} onClick={onDone}>
          Done
        </button>
      }
    >
      <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
        Save this passphrase now. It is shown one time and cannot be found again. Keep it apart from the file.
      </p>
      <code className={styles.secretBox} data-testid="move-passphrase">
        {passphrase}
      </code>
      <div className={styles.actions}>
        <button type="button" className={ui.btn} data-autofocus onClick={() => void copy()}>
          Copy passphrase
        </button>
        {copyNote && (
          <span role="status" className={ui.hint}>
            {copyNote}
          </span>
        )}
      </div>
      <p className={ui.hint} role="status">
        {downloadNote ??
          `The download of ${filename} starts now. The link works one time and ends in 5 minutes. If the download fails, close this window and make a new file.`}
      </p>
      <label className={styles.check}>
        <input
          type="checkbox"
          checked={saved}
          onChange={(e) => {
            setSaved(e.target.checked);
            setNudge(false);
          }}
        />
        <span>I saved the passphrase in a safe place.</span>
      </label>
      <p role="status" className={ui.hint}>
        {nudge ? 'Tick the box when you saved the passphrase. Then you can close this window.' : ''}
      </p>
    </Dialog>
  );
}

function NextSteps() {
  return (
    <SettingsSection title="Next steps">
      <SettingsCard padded>
        <ol className={styles.steps}>
          <li>
            Install the same release on the new server. The file loads only into the same release as the one
            that made it.
          </li>
          <li>Open the setup link of the new server and choose “Restore from a move file”.</li>
          <li>Point your DNS or proxy at the new server.</li>
          <li>
            Update your git webhooks, MCP clients and mobile clients to the new address. Check the settings of
            each.
          </li>
          <li>
            Stop using this server. Anything written here after the export is NOT in the file and is lost when
            you switch.
          </li>
        </ol>
      </SettingsCard>
    </SettingsSection>
  );
}
