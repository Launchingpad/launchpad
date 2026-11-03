import { useState, type FormEvent } from 'react';
import { useReplaceGitToken } from '../../../api/gitIntegrations';
import type { GitIntegration } from '../../../api/types';
import { ApiError } from '../../../api/errors';
import { Dialog } from '../../../ui/Dialog';
import { FIELD_TEXT, badField, checkBaseUrl, gitErrorText, providerInfo } from './gitText';
import { SecretInput, touchClass } from './gitParts';
import settings from '../settings.module.css';
import ui from '../../../styles/ui.module.css';

/** Replace the token, or add one to a webhook-only connection (which then becomes token mode). */
export function ReplaceTokenDialog({
  integration,
  onClose,
  onDone,
  onRotate,
}: {
  integration: GitIntegration;
  onClose: () => void;
  /** Called after a good save. The token is already gone from this component. */
  onDone: () => void;
  /** The way forward when the webhook secret is a hash only: close this and start the rotation. */
  onRotate?: () => void;
}) {
  const replace = useReplaceGitToken();
  const info = providerInfo(integration.provider);
  const adding = integration.mode === 'webhook_only';
  const [token, setToken] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [hashOnly, setHashOnly] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setFieldError(null);
    setHashOnly(false);
    if (token.trim() === '') {
      setFieldError('Enter the token.');
      return;
    }
    const t = token;
    setBusy(true);
    try {
      await replace(integration.id, t, adding ? 'token' : undefined);
      setToken('');
      onDone();
    } catch (err) {
      if (badField(err) === 'token') setFieldError(FIELD_TEXT.token);
      else {
        setHashOnly(err instanceof ApiError && err.code === 'secret_hash_only' && !!onRotate);
        setError(gitErrorText(err, 'token', integration.provider));
      }
    } finally {
      setToken('');
      setBusy(false);
    }
  }

  return (
    <Dialog
      className={touchClass}
      title={adding ? 'Add a token' : 'Replace the token'}
      width={520}
      onClose={onClose}
    >
      <form
        onSubmit={(e) => void submit(e)}
        noValidate
        aria-label={adding ? 'Add a token' : 'Replace the token'}
      >
        {info && (
          <>
            <p>The token needs only these permissions. Use the line that fits the token form you see:</p>
            <ul>
              {info.scopes.map((s) => (
                <li key={s}>{s}</li>
              ))}
            </ul>
            <p className={ui.hint}>{info.scopeNote}</p>
            {info.tokenPage !== '' &&
              checkBaseUrl(integration.baseUrl) === null &&
              integration.baseUrl.startsWith('https://') && (
                <p>
                  <a
                    href={`${integration.baseUrl.trim().replace(/\/+$/, '')}${info.tokenPage}`}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    Make a token on {info.label} (opens in a new tab)
                  </a>
                </p>
              )}
          </>
        )}
        <p className={ui.hint}>
          {adding
            ? 'With a token, Launchpad makes the hooks itself. The connection changes to token mode.'
            : 'The old token stops working for Launchpad when you save. Launchpad then checks the new one.'}
        </p>
        <div className={ui.field}>
          <label htmlFor="git-new-token">New token</label>
          <SecretInput
            id="git-new-token"
            value={token}
            onChange={setToken}
            invalid={!!fieldError}
            describedBy={fieldError ? 'git-new-token-error' : undefined}
            autoFocus
          />
          {fieldError && (
            <span id="git-new-token-error" className={settings.fieldError} aria-live="polite">
              {fieldError}
            </span>
          )}
        </div>
        {error && (
          <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
            {error}
          </p>
        )}
        {hashOnly && (
          <>
            <p className={ui.hint}>
              The host's hook must get the new secret too. Launchpad shows it once, then you press Commit on
              the connection page. After that, add the token.
            </p>
            <div className={settings.actions}>
              <button type="button" className={`${ui.btn} ${ui.primary}`} onClick={onRotate}>
                Rotate the webhook secret first
              </button>
            </div>
          </>
        )}
        <div className={settings.actions}>
          <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={busy}>
            {busy ? 'Saving…' : 'Save token'}
          </button>
          <button type="button" className={ui.btn} onClick={onClose} disabled={busy}>
            Cancel
          </button>
        </div>
      </form>
    </Dialog>
  );
}
