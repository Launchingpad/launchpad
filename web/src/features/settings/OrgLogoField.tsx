import { useRef, useState, type ChangeEvent } from 'react';
import { ApiError } from '../../api/errors';
import { useClearOrgAvatar, useSetOrgAvatar } from '../../api/queries';
import type { Org } from '../../api/types';
import { Logo } from '../../ui/Logo';
import { SettingsCard } from './SettingsParts';
import styles from './organisation.module.css';
import ui from '../../styles/ui.module.css';

/** The types and size the server accepts (decisions-T24 #8). SVG is not one of them. */
export const LOGO_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const LOGO_MAX_BYTES = 2 * 1024 * 1024;

const RULE = 'Use a PNG, JPEG, GIF or WebP image of at most 2 MiB.';

/** A fixed sentence for a failed upload or removal. No server text reaches the screen. */
export function logoErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not upload the image. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return `The server did not accept this image. ${RULE}`;
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You need admin access to change the logo.';
    case 404:
      return 'The server cannot find the organisation. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    case 503:
      return 'This server has no file storage, so it cannot keep a logo.';
    default:
      return 'Could not change the logo. Try again.';
  }
}

/** Settings, Organisation, Logo (LAUN-22): preview, upload or replace, remove. */
export function OrgLogoField({ org }: { org: Org }) {
  const set = useSetOrgAvatar();
  const clear = useClearOrgAvatar();
  const input = useRef<HTMLInputElement>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [status, setStatus] = useState('');
  const busy = set.isPending || clear.isPending;
  const has = !!org.avatarUrl;

  async function pick(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    e.target.value = ''; // the same file can be chosen again
    if (!file || busy) return;
    setStatus('');
    if (!LOGO_TYPES.includes(file.type) || file.size < 1 || file.size > LOGO_MAX_BYTES) {
      setProblem(RULE);
      return;
    }
    setProblem(null);
    try {
      await set.mutateAsync(file);
      setStatus('Logo saved.');
    } catch (err) {
      setProblem(logoErrorText(err));
    }
  }

  async function remove() {
    if (busy) return;
    setProblem(null);
    setStatus('');
    try {
      await clear.mutateAsync();
      setStatus('Logo removed.');
    } catch (err) {
      setProblem(logoErrorText(err));
    }
  }

  return (
    <SettingsCard padded>
      <div className={styles.logoRow}>
        <Logo name={org.name} src={org.avatarUrl} size={64} />
        <div className={styles.logoBody}>
          <div className={styles.actions}>
            <button
              type="button"
              className={`${ui.btn} ${ui.primary}`}
              aria-disabled={busy || undefined}
              onClick={() => {
                if (!busy) input.current?.click();
              }}
            >
              {set.isPending ? 'Uploading' : has ? 'Replace logo' : 'Upload logo'}
            </button>
            {has && (
              <button
                type="button"
                className={`${ui.btn}`}
                aria-disabled={busy || undefined}
                onClick={() => void remove()}
              >
                {clear.isPending ? 'Removing' : 'Remove logo'}
              </button>
            )}
          </div>
          <input
            ref={input}
            type="file"
            hidden
            accept={LOGO_TYPES.join(',')}
            aria-label="Logo file"
            data-testid="logo-file"
            onChange={(e) => void pick(e)}
          />
          <span className={styles.hint}>{RULE}</span>
          {problem && (
            <p role="alert" className={styles.problem}>
              {problem}
            </p>
          )}
          <p role="status" className={styles.ok}>
            {status}
          </p>
        </div>
      </div>
    </SettingsCard>
  );
}
