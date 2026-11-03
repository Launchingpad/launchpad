import { useEffect, useState } from 'react';
import { Link, useLocation } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { flatRows, useInbox, useUnreadCount, type InboxFilter } from '../../api/notifications';
import { registerModal } from '../../lib/hotkeys';
import { Floating } from '../../ui/Floating';
import { IconBell } from '../../ui/Icons';
import { FilterTabs, MarkAllRead, UnreadBadge } from './InboxControls';
import { InboxList } from './InboxList';
import ui from '../../styles/ui.module.css';
import styles from './inbox.module.css';

function Popover({ anchor, onClose }: { anchor: HTMLElement; onClose: (restore: boolean) => void }) {
  const org = useOrgSlug();
  const [filter, setFilter] = useState<InboxFilter>('unread');
  const query = useInbox(filter);
  // The page's own j / k / e keys stay quiet while the popover is open; the popover has its own.
  useEffect(() => registerModal(), []);
  return (
    <Floating anchor={anchor} onClose={onClose} label="Inbox quick view" width={400}>
      <div className={styles.pop} role="dialog" aria-label="Inbox quick view" tabIndex={-1}>
        <div className={styles.popHead}>
          <strong>Inbox</strong>
          <FilterTabs value={filter} onChange={setFilter} filters={['unread', 'all']} />
          <span className={styles.grow} />
          <MarkAllRead rows={flatRows(query.data)} />
        </div>
        <div className={styles.popBody}>
          <InboxList query={query} filter={filter} keys="local" autoFocus onNavigate={() => onClose(false)} />
        </div>
        <div className={styles.popFoot}>
          <Link to={`/${org}/inbox`} className={ui.btn} tabIndex={0} onClick={() => onClose(false)}>
            Open inbox
          </Link>
        </div>
      </div>
    </Floating>
  );
}

/** The bell: the unread badge and the quick-triage popover. */
export function InboxBell() {
  const [anchor, setAnchor] = useState<HTMLButtonElement | null>(null);
  const [open, setOpen] = useState(false);
  const loc = useLocation();
  const unread = useUnreadCount().data?.count ?? 0;
  const label = unread > 0 ? `Inbox quick view, ${unread} unread` : 'Inbox quick view';
  useEffect(() => setOpen(false), [loc.pathname]);
  return (
    <>
      <button
        type="button"
        ref={setAnchor}
        className={`${ui.btn} ${ui.ghost} ${ui.icon} ${styles.bell}`}
        aria-label={label}
        aria-haspopup="dialog"
        aria-expanded={open}
        title="Inbox quick view"
        onClick={() => setOpen((v) => !v)}
      >
        <IconBell />
        <UnreadBadge className={styles.bellBadge} />
      </button>
      {open && anchor && (
        <Popover
          anchor={anchor}
          onClose={(restore) => {
            setOpen(false);
            if (restore) anchor.focus();
          }}
        />
      )}
    </>
  );
}
