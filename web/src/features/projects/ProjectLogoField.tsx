import { useRef, useState, type ChangeEvent } from 'react';
import { ApiError } from '../../api/errors';
import type { Project } from '../../api/types';
import { Logo } from '../../ui/Logo';
import { LOGO_MAX_BYTES, LOGO_TYPES } from '../settings/OrgLogoField';
import { useClearProjectAvatar, useSetProjectAvatar } from './queries';
import ui from '../../styles/ui.module.css';
import styles from './projects.module.css';

const RULE = 'Use a PNG, JPEG, GIF or WebP image of at most 2 MiB.';

/** A fixed sentence for a failed upload or removal. No server text reaches the screen. */
export function projectLogoErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not upload the image. Try again.';
  switch (err.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return `The server did not accept this image. ${RULE}`;
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return 'You need write access to change the logo.';
    case 404:
      return 'The server cannot find the project. Reload the page and try again.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    case 503:
      return 'This server has no file storage, so it cannot keep a logo.';
    default:
      return 'Could not change the logo. Try again.';
  }
}

/**
 * The Logo section of the project page (LAUN-162): preview, upload or replace, remove. The web app has no
 * scope check for other writes, so the field shows to everyone; a refusal is a fixed sentence.
 */
export function ProjectLogoField({ project }: { project: Project }) {
  // Keyed by id: the status and error text of one project never show on another.
  return <LogoField key={project.id} project={project} />;
}

function LogoField({ project }: { project: Project }) {
  const set = useSetProjectAvatar(project.id);
  const clear = useClearProjectAvatar(project.id);
  const input = useRef<HTMLInputElement>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [status, setStatus] = useState('');
  const busy = set.isPending || clear.isPending;
  const has = !!project.avatarUrl;

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
      setProblem(projectLogoErrorText(err));
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
      setProblem(projectLogoErrorText(err));
    }
  }

  return (
    <section aria-labelledby="logo-h" className={styles.logoSection}>
      <h2 id="logo-h" className={styles.h2}>
        Logo
      </h2>
      <div className={styles.logoRow}>
        <Logo name={project.name} src={project.avatarUrl} emoji={project.emoji} size={48} />
        <div className={styles.logoBody}>
          <div className={styles.logoActions}>
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
                className={ui.btn}
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
          <span className={styles.muted}>{RULE}</span>
          {problem && (
            <p role="alert" className={styles.logoProblem}>
              {problem}
            </p>
          )}
          <p role="status" className={styles.logoOk}>
            {status}
          </p>
        </div>
      </div>
    </section>
  );
}
