import { ApiError } from '../../api/errors';
import styles from '../../styles/ui.module.css';

/** A fixed sentence for a failed load. No server text reaches the screen. */
export function loadText(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 403) return 'You do not have access to this. Sign out and in again.';
  if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  return 'The server had a problem. Try again.';
}

/** Like the shared `ErrorState`, but with fixed text only. */
export function FixedError({
  error,
  onRetry,
  title,
}: {
  error: unknown;
  onRetry: () => void;
  title: string;
}) {
  return (
    <div className={styles.center} role="alert">
      <h2 style={{ color: 'var(--text)', fontSize: 16 }}>{title}</h2>
      <div>{loadText(error)}</div>
      <button type="button" className={styles.btn} onClick={onRetry}>
        Try again
      </button>
    </div>
  );
}
