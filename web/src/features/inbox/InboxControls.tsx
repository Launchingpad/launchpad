import { counts, INBOX_FILTERS, useReadAll, useUnreadCount, type InboxFilter } from '../../api/notifications';
import type { InboxNotification } from '../../api/types';
import { badgeText, FILTER_LABEL } from './inboxModel';
import ui from '../../styles/ui.module.css';
import styles from './inbox.module.css';

export function FilterTabs({
  value,
  onChange,
  filters = INBOX_FILTERS,
}: {
  value: InboxFilter;
  onChange: (f: InboxFilter) => void;
  filters?: InboxFilter[];
}) {
  return (
    <div className={styles.tabs} role="group" aria-label="Show">
      {filters.map((f) => (
        <button
          key={f}
          type="button"
          className={`${styles.tab} ${f === value ? styles.tabOn : ''}`}
          aria-pressed={f === value}
          onClick={() => onChange(f)}
        >
          {FILTER_LABEL[f]}
        </button>
      ))}
    </div>
  );
}

/** Mark read everything up to the newest loaded row. The API needs `before` (decisions-T13 #27). */
export function MarkAllRead({ rows }: { rows: InboxNotification[] }) {
  const readAll = useReadAll();
  const unread = rows.some(counts);
  const newest = rows[0];
  return (
    <button
      type="button"
      className={`${ui.btn} ${styles.markAll}`}
      disabled={!newest || !unread || readAll.isPending}
      onClick={() => newest && readAll.mutate(newest.createdAt)}
    >
      Mark all as read
    </button>
  );
}

/** The unread number, or nothing at 0. The words are for screen readers; the digits are for eyes. */
export function UnreadBadge({ className }: { className?: string }) {
  const q = useUnreadCount();
  const n = q.data?.count ?? 0;
  if (n <= 0) return null;
  return (
    <span className={`${styles.badge} ${className ?? ''}`} data-testid="unread-badge">
      <span aria-hidden="true">{badgeText(n)}</span>
      <span className="sr-only">{n} unread</span>
    </span>
  );
}
