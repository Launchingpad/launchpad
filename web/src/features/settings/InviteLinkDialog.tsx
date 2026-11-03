import { useEffect, useState } from 'react';
import type { CreatedInvite } from '../../api/types';
import { Dialog } from '../../ui/Dialog';
import styles from './settings.module.css';
import ui from '../../styles/ui.module.css';

/** A bare path (`/invite#...`, no PUBLIC_BASE_URL on the server) needs the address of this page in front. */
export function fullLink(url: string): string {
  return url.startsWith('/') ? `${window.location.origin}${url}` : url;
}

/**
 * The copy-once screen for an invite link (the same flow as the token dialog, decisions-T11 #5 and #6).
 * The link holds the invite token. It lives in the `link` prop, which the page keeps in component state:
 * never in web storage, a URL, a log, a cache or an error text. A modal dialog, so the rest of the app cannot
 * be reached until the admin confirms the copy.
 */
export function InviteLinkDialog({
  invite,
  link,
  onDone,
}: {
  invite: CreatedInvite['invite'];
  link: string;
  onDone: () => void;
}) {
  const [saved, setSaved] = useState(false);
  const [copyNote, setCopyNote] = useState<string | null>(null);
  const [nudge, setNudge] = useState(false);

  // The server never shows the link again, so warn before the page closes.
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener('beforeunload', warn);
    return () => window.removeEventListener('beforeunload', warn);
  }, []);

  async function copy() {
    try {
      await navigator.clipboard.writeText(link);
      setCopyNote('Copied.');
    } catch {
      setCopyNote('Could not copy. Select the link and copy it by hand.');
    }
  }

  // Escape, the close button and a click outside cannot close the dialog before the admin confirms.
  function close() {
    if (saved) onDone();
    else setNudge(true);
  }

  return (
    <Dialog
      title="Copy the invite link"
      width={560}
      onClose={close}
      footer={
        <button type="button" className={`${ui.btn} ${ui.primary}`} disabled={!saved} onClick={onDone}>
          Done
        </button>
      }
    >
      <p>
        Give this link to <strong>{invite.email}</strong>. The person joins as{' '}
        {invite.role === 'admin' ? 'an admin' : 'a member'}. The link works one time, until{' '}
        {new Date(invite.expiresAt).toISOString().slice(0, 10)}.
      </p>
      <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
        Copy this link now. It is not shown again.
      </p>
      <code className={styles.tokenBox} data-testid="invite-link">
        {link}
      </code>
      <div className={styles.actions}>
        <button type="button" className={ui.btn} data-autofocus onClick={() => void copy()}>
          Copy link
        </button>
        {copyNote && (
          <span role="status" className={ui.hint}>
            {copyNote}
          </span>
        )}
      </div>
      <p className={ui.hint}>
        The server cannot send email now, so it did not send this link. Send it to the person yourself. Anyone
        who has the link can use it. If you do not trust the link, revoke the invite.
      </p>
      <label className={styles.confirm}>
        <input
          type="checkbox"
          checked={saved}
          onChange={(e) => {
            setSaved(e.target.checked);
            setNudge(false);
          }}
        />
        <span>I copied the link.</span>
      </label>
      <p role="status" className={ui.hint}>
        {nudge ? 'Tick the box when you copied the link. Then you can close this window.' : ''}
      </p>
    </Dialog>
  );
}
