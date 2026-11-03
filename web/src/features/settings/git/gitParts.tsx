import { useEffect, useState, type ReactNode } from 'react';
import { Dialog } from '../../../ui/Dialog';
import type { GitIntegration } from '../../../api/types';
import { STATUS_LABEL } from './gitText';
import styles from './git.module.css';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

/**
 * A field for a token or a secret. Write-only: it never shows a stored value. `new-password` and the
 * ignore hints keep browsers and password managers from filling or saving it; it has no `name`.
 */
export function SecretInput({
  id,
  value,
  onChange,
  describedBy,
  invalid,
  placeholder,
  autoFocus,
}: {
  id: string;
  value: string;
  onChange: (v: string) => void;
  describedBy?: string;
  invalid?: boolean;
  placeholder?: string;
  autoFocus?: boolean;
}) {
  return (
    <input
      id={id}
      type="password"
      className={ui.input}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      autoComplete="new-password"
      autoCapitalize="off"
      autoCorrect="off"
      spellCheck={false}
      data-lpignore="true"
      data-1p-ignore=""
      data-form-type="other"
      aria-invalid={invalid || undefined}
      aria-describedby={describedBy}
      placeholder={placeholder}
      data-autofocus={autoFocus ? '' : undefined}
    />
  );
}

/** Class for the page and every git dialog: the 40px touch rules of git.module.css. */
export const touchClass = styles.touch;

export function StatusChip({ integration }: { integration: Pick<GitIntegration, 'status'> }) {
  const cls =
    integration.status === 'ok'
      ? styles.chipOk
      : integration.status === 'needs_attention'
        ? styles.chipWarn
        : styles.chipOff;
  return <span className={`${styles.chip} ${cls}`}>{STATUS_LABEL[integration.status]}</span>;
}

/**
 * The copy-once screen for a webhook secret (the pattern of the token dialog). The secret is the
 * `secret` prop, which the caller keeps in component state: never in web storage, a URL, a log, a cache or an
 * error text. It is a modal, and cannot be closed before the admin confirms the copy.
 */
export function SecretOnceDialog({
  title,
  secret,
  children,
  confirmLabel = 'I saved the secret in a safe place.',
  onDone,
}: {
  title: string;
  secret: string;
  children?: ReactNode;
  confirmLabel?: string;
  onDone: () => void;
}) {
  const [saved, setSaved] = useState(false);
  const [copyNote, setCopyNote] = useState<string | null>(null);
  const [nudge, setNudge] = useState(false);

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
      setCopyNote('Could not copy. Select the secret and copy it by hand.');
    }
  }
  function close() {
    if (saved) onDone();
    else setNudge(true);
  }

  return (
    <Dialog
      className={touchClass}
      title={title}
      width={560}
      onClose={close}
      footer={
        <button type="button" className={`${ui.btn} ${ui.primary}`} disabled={!saved} onClick={onDone}>
          Done
        </button>
      }
    >
      <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
        Copy this secret now. It is not shown again.
      </p>
      <code className={settings.tokenBox} data-testid="new-git-secret">
        {secret}
      </code>
      <div className={settings.actions}>
        <button type="button" className={ui.btn} data-autofocus onClick={() => void copy()}>
          Copy secret
        </button>
        <span role="status" className={ui.hint}>
          {copyNote}
        </span>
      </div>
      {children}
      <label className={settings.confirm}>
        <input
          type="checkbox"
          checked={saved}
          onChange={(e) => {
            setSaved(e.target.checked);
            setNudge(false);
          }}
        />
        <span>{confirmLabel}</span>
      </label>
      <p role="status" className={ui.hint}>
        {nudge ? 'Tick the box when you saved the secret. Then you can close this window.' : ''}
      </p>
    </Dialog>
  );
}

/** The URL for the host's webhook form. A bare path means the server has no PUBLIC_BASE_URL (decisions-T17 #25). */
export function WebhookUrl({ url }: { url: string }) {
  const [note, setNote] = useState<string | null>(null);
  const isPath = url.startsWith('/');
  const full = isPath ? `${window.location.origin}${url}` : url;
  async function copy() {
    try {
      await navigator.clipboard.writeText(full);
      setNote('Copied.');
    } catch {
      setNote('Could not copy. Select the address and copy it by hand.');
    }
  }
  return (
    <>
      <code className={settings.tokenBox} data-testid="webhook-url">
        {full}
      </code>
      <div className={settings.actions}>
        <button type="button" className={ui.btn} onClick={() => void copy()}>
          Copy address
        </button>
        <span role="status" className={ui.hint}>
          {note}
        </span>
      </div>
      {isPath && (
        <p className={ui.hint}>
          The server has no PUBLIC_BASE_URL, so this address is built from the address of this page. The host
          can only use it when it can reach this page at that address. Set PUBLIC_BASE_URL so the full address
          can be shown.
        </p>
      )}
    </>
  );
}
