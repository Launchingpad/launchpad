import { useEffect, useState } from 'react';
import { apiBase } from '../../env';
import { useOrgSlug } from '../../api/org';
import { useTeams } from '../../api/queries';
import type { CreatedToken } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import { accessText, day } from './tokenText';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

/**
 * The copy-once screen (the same flow as the first-run setup page). The secret lives in the `issued` prop,
 * which the page keeps in component state: never in web storage, a URL, a log or an error text. It is a
 * modal dialog, so the rest of the app cannot be reached until the person confirms they saved the token.
 */
export function TokenSecretDialog({ issued, onDone }: { issued: CreatedToken; onDone: () => void }) {
  const org = useOrgSlug();
  const teams = useTeams();
  const [saved, setSaved] = useState(false);
  const [copyNote, setCopyNote] = useState<string | null>(null);
  const [nudge, setNudge] = useState(false);
  const { token, secret } = issued;

  // The server never shows the secret again, so warn before the page closes.
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret);
      setCopyNote('Copied.');
    } catch {
      setCopyNote('Could not copy. Select the token and copy it by hand.');
    }
  }

  // Escape, the close button and a click outside cannot close the dialog before the person confirms.
  function close() {
    if (saved) onDone();
    else setNudge(true);
  }

  return (
    <Dialog
      title="Copy your token"
      width={560}
      onClose={close}
      footer={
        <button type="button" className={`${ui.btn} ${ui.primary}`} disabled={!saved} onClick={onDone}>
          Done
        </button>
      }
    >
      <p>
        This token is for {token.user.name} (<code>{token.user.username}</code>). Access:{' '}
        {accessText(token.scopes).toLowerCase()}. It expires on {day(token.expiresAt)}.
      </p>
      <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
        Copy this token now. It is not shown again.
      </p>
      <code className={styles.tokenBox} data-testid="new-token">
        {secret}
      </code>
      <div className={styles.actions}>
        <button type="button" className={ui.btn} data-autofocus onClick={() => void copy()}>
          Copy token
        </button>
        {copyNote && (
          <span role="status" className={ui.hint}>
            {copyNote}
          </span>
        )}
      </div>
      <label className={styles.confirm}>
        <input
          type="checkbox"
          checked={saved}
          onChange={(e) => {
            setSaved(e.target.checked);
            setNudge(false);
          }}
        />
        <span>I saved the token in a safe place.</span>
      </label>
      <p role="status" className={ui.hint}>
        {nudge ? 'Tick the box when you saved the token. Then you can close this window.' : ''}
      </p>

      <h3 className={styles.subtitle}>MCP setup</h3>
      <p className={ui.hint}>
        To use this token with the MCP server, set these environment variables. Paste the token where the text
        says so.
      </p>
      <pre className={styles.envBox} aria-label="MCP environment variables">
        <code>
          {`LAUNCHPAD_URL=${apiBase()}\n`}
          {'LAUNCHPAD_TOKEN=<paste the token>\n'}
          {`LAUNCHPAD_ORG=${org}\n`}
          {`LAUNCHPAD_TEAM=${teams.data?.[0]?.key ?? '<your team key>'}`}
        </code>
      </pre>
    </Dialog>
  );
}
