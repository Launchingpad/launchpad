import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import styles from './breadcrumbs.module.css';

// LAUN-157: the one breadcrumb trail (team > section > item). The last item is the current page.
export interface Crumb {
  key: string;
  label: string;
  /** Route of a link. The last item is never a link. */
  to?: string;
  /** Full text for a clipped label. Defaults to the label. */
  title?: string;
  /** The current item is the page heading (an h1). */
  heading?: boolean;
  /** Extra control after the current item, for example a switcher. */
  after?: ReactNode;
}

export function Breadcrumbs({ items, className }: { items: Crumb[]; className?: string }) {
  const last = items.length - 1;
  return (
    <nav aria-label="Breadcrumb" className={`${styles.nav} ${className ?? ''}`.trim()}>
      <ol className={styles.list}>
        {items.map((c, i) => {
          const current = i === last;
          // Phone: only the nearest ancestor and the current item stay; "…" stands for the rest.
          const hideSmall = i < last - 1;
          const gap = i === last - 1 && i > 0;
          const text = current ? (
            c.heading ? (
              <h1 className={`${styles.text} ${styles.current}`} title={c.title ?? c.label}>
                {c.label}
              </h1>
            ) : (
              <span className={`${styles.text} ${styles.current}`} title={c.title ?? c.label}>
                {c.label}
              </span>
            )
          ) : c.to ? (
            <Link to={c.to} className={`${styles.text} ${styles.link}`} title={c.title ?? c.label}>
              {c.label}
            </Link>
          ) : (
            <span className={styles.text} title={c.title ?? c.label}>
              {c.label}
            </span>
          );
          return (
            <li
              key={c.key}
              className={`${styles.item} ${current ? styles.last : ''} ${hideSmall ? styles.hideSmall : ''}`.trim()}
              aria-current={current ? 'page' : undefined}
            >
              {i > 0 && (
                <span className={styles.sep} aria-hidden="true">
                  {gap ? <span className={styles.gap}>&hellip; </span> : null}
                  &rsaquo;
                </span>
              )}
              {text}
              {current && c.after}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
