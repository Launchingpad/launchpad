import type { ReactNode } from 'react';
import { errorMessage } from '../api/errors';
import styles from '../styles/ui.module.css';

export function Spinner({ label = 'Loading' }: { label?: string }) {
  return (
    <div className={styles.center} role="status">
      <div className={styles.spinner} />
      <span className="sr-only">{label}</span>
    </div>
  );
}

export function SkeletonRows({ n = 8 }: { n?: number }) {
  return (
    <div role="status" aria-label="Loading" aria-busy="true">
      {Array.from({ length: n }, (_, i) => (
        <div key={i} className={styles.skeleton} />
      ))}
    </div>
  );
}

export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className={styles.center}>
      <h2 style={{ color: 'var(--text)', fontSize: 16 }}>{title}</h2>
      {children && <div>{children}</div>}
    </div>
  );
}

export function ErrorState({
  error,
  onRetry,
  title = 'Could not load this',
}: {
  error: unknown;
  onRetry?: () => void;
  title?: string;
}) {
  return (
    <div className={styles.center} role="alert">
      <h2 style={{ color: 'var(--text)', fontSize: 16 }}>{title}</h2>
      <div>{errorMessage(error)}</div>
      {onRetry && (
        <button type="button" className={styles.btn} onClick={onRetry}>
          Try again
        </button>
      )}
    </div>
  );
}
