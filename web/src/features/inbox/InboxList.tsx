import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { Link, useLocation, useNavigate } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { flatRows, useUpdateNotification, type useInbox, type InboxFilter } from '../../api/notifications';
import type { InboxNotification } from '../../api/types';
import { nextPageStep, useEntryMemory, useScrollRestore } from '../../lib/backNav';
import { relativeTime, fullDate } from '../../lib/time';
import { isTypingTarget, useHotkeys } from '../../lib/hotkeys';
import { Avatar } from '../../ui/Avatar';
import { IconArchive, IconCheck, IconDot } from '../../ui/Icons';
import { ErrorState, SkeletonRows } from '../../ui/States';
import { EMPTY, summary, target } from './inboxModel';
import ui from '../../styles/ui.module.css';
import styles from './inbox.module.css';

type Query = ReturnType<typeof useInbox>;

function Row({
  n,
  org,
  focused,
  onOpen,
  onAux,
  onFocus,
  onRead,
  onArchive,
}: {
  n: InboxNotification;
  org: string;
  focused: boolean;
  onOpen: (n: InboxNotification) => void;
  onAux: (n: InboxNotification) => void;
  onFocus: (id: string) => void;
  onRead: (n: InboxNotification) => void;
  onArchive: (n: InboxNotification) => void;
}) {
  const unread = n.readAt === null;
  const archived = n.archivedAt !== null;
  const dest = target(n, org);
  return (
    <li
      className={`${styles.row} ${focused ? styles.focused : ''} ${unread ? styles.unread : ''}`}
      data-notification-id={n.id}
      data-focused={focused ? 'true' : undefined}
      onMouseDown={(e) => {
        // The popover stops the browser from moving focus on a press (Floating), so do it here: the pressed control,
        // else the row link. The li's onFocus then highlights the row, and highlight and DOM focus agree.
        const t = (e.target as HTMLElement).closest<HTMLElement>('button,a[href]');
        (t ?? e.currentTarget.querySelector<HTMLElement>('a[href]'))?.focus({ preventScroll: true });
      }}
      // Tab onto a link or button of this row makes it the highlighted row: highlight and DOM focus are one thing.
      onFocus={() => onFocus(n.id)}
    >
      <Link
        to={dest.to}
        className={styles.main}
        // tabIndex 0 on purpose: the popover's Tab handling counts only controls it can see in a list.
        tabIndex={0}
        onClick={() => onOpen(n)}
        onAuxClick={(e) => e.button === 1 && onAux(n)}
      >
        <span className={styles.dot} aria-hidden="true" />
        {unread && <span className="sr-only">Unread. </span>}
        {n.actor ? (
          <Avatar name={n.actor.name || n.actor.username} username={n.actor.username} />
        ) : (
          <span className={styles.noActor} aria-hidden="true" />
        )}
        <span className={styles.body}>
          <span className={styles.line}>
            <span className={styles.ident}>{dest.ident}</span>
            <span className={styles.title}>{dest.title}</span>
          </span>
          <span className={styles.sub}>{summary(n)}</span>
        </span>
        <time className={styles.time} dateTime={n.createdAt} title={fullDate(n.createdAt)}>
          {relativeTime(n.createdAt)}
        </time>
      </Link>
      <span className={styles.rowActions}>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
          aria-label={`${unread ? 'Mark as read' : 'Mark as unread'}: ${dest.ident}`}
          title={unread ? 'Mark as read (u)' : 'Mark as unread (u)'}
          onClick={() => onRead(n)}
        >
          {unread ? <IconCheck /> : <IconDot />}
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
          aria-label={`${archived ? 'Unarchive' : 'Archive'}: ${dest.ident}`}
          title={archived ? 'Unarchive (e)' : 'Archive (e)'}
          onClick={() => onArchive(n)}
        >
          <IconArchive />
        </button>
      </span>
    </li>
  );
}

/**
 * The rows of one inbox filter, with the empty, error and loading states and the keyboard:
 * j / k move, Enter opens (and marks read), e archives (unarchives in Archived), u toggles read.
 * `keys="global"` listens on the document (the page); `"local"` listens inside the list (the popover).
 */
export function InboxList({
  query,
  filter,
  keys,
  onNavigate,
  className,
  autoFocus,
}: {
  query: Query;
  filter: InboxFilter;
  keys: 'global' | 'local';
  /** The popover closes itself when a row is opened. */
  onNavigate?: () => void;
  className?: string;
  /** Take focus when shown, so the keys work at once (the popover). */
  autoFocus?: boolean;
}) {
  const org = useOrgSlug();
  const nav = useNavigate();
  const update = useUpdateNotification();
  const rows = useMemo(() => flatRows(query.data), [query.data]);
  // LAUN-90: on the page, Back puts the selected row and the scroll offset back (per history entry).
  const page = keys === 'global';
  const [savedFocus, saveFocus] = useEntryMemory('inbox-row');
  const [focusId, setFocusId] = useState<string | undefined>(page ? savedFocus : undefined);
  useEffect(() => {
    if (page) saveFocus(focusId);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- saveFocus is a new function each render
  }, [page, focusId]);
  const lastIdx = useRef(0);
  const root = useRef<HTMLDivElement>(null);
  const { hasNextPage, isFetchingNextPage, fetchNextPage, isFetchNextPageError } = query;
  const pagesLoaded = query.data?.pages.length ?? 0;
  const entry = useLocation().key;
  const asked = useRef('');
  useScrollRestore('inbox', root, page, () => {
    // A failed page stops the loading (LAUN-88 shows "Could not load more" with Try again).
    const step = nextPageStep({
      hasNextPage: !!hasNextPage,
      failed: isFetchNextPageError,
      busy: isFetchingNextPage,
      pages: pagesLoaded,
      asked: asked.current,
      entry,
    });
    if (step.fetch) {
      asked.current = `${entry}|${pagesLoaded}`;
      void fetchNextPage();
    }
    return step.wait;
  });
  const byKey = useRef(false);
  const inside = useRef(false);
  const focusedOnce = useRef(false);
  const fallback = useRef<HTMLElement | null>(null);

  // The focused row. When it leaves the list (archived, read in the Unread list) the next one takes its place.
  const at = rows.findIndex((r) => r.id === focusId);
  const idx = rows.length === 0 ? -1 : at >= 0 ? at : Math.min(lastIdx.current, rows.length - 1);
  const focused = idx >= 0 ? rows[idx] : undefined;
  const hasRows = rows.length > 0;
  useEffect(() => {
    // Put focus on a real control, never on the list box: Floating's Tab handling only counts controls, and a
    // focus on the box would make the first Tab close the popover. Rows first; else the first control of the panel.
    if (!autoFocus || focusedOnce.current) return;
    const el = root.current;
    if (!el) return;
    const link = el.querySelector<HTMLElement>('a[href]');
    if (link) {
      // Rows came. Take focus from the stand-in control (below) too, but never from where the person went.
      const a = document.activeElement;
      if (!fallback.current || a === fallback.current || a === document.body) link.focus();
      focusedOnce.current = true;
      return;
    }
    // Loading or empty: a control of the panel has the focus at once, so Tab and Esc work.
    if (fallback.current) return;
    const btn = el.closest('[data-floating]')?.querySelector<HTMLElement>('button');
    if (btn) {
      fallback.current = btn;
      btn.focus();
    }
  }, [autoFocus, hasRows]);
  useEffect(() => {
    if (idx >= 0) lastIdx.current = idx;
  }, [idx]);
  useEffect(() => {
    // The focused control left with its row (archived, read in Unread): keep focus in the list, or the keys die.
    const el = root.current;
    if (!el || !inside.current || el.contains(document.activeElement)) return;
    const links = [...el.querySelectorAll<HTMLElement>('a[href]')];
    (
      links[Math.min(lastIdx.current, links.length - 1)] ??
      el.closest('[data-floating]')?.querySelector<HTMLElement>('button')
    )?.focus();
  }, [rows]);
  useEffect(() => {
    // Only a key moves the view; a new top row from the stream must not pull a scrolled-down reader up.
    if (!focused || !byKey.current) return;
    byKey.current = false;
    const li = root.current?.querySelector<HTMLElement>(`[data-notification-id="${focused.id}"]`);
    // j / k move the DOM focus too, so Enter, e and u act on the row that is highlighted.
    li?.querySelector<HTMLElement>('a[href]')?.focus();
    li?.scrollIntoView({ block: 'nearest' });
  }, [focused]);

  const open = (n: InboxNotification) => {
    if (n.readAt === null) update.mutate({ id: n.id, read: true });
    onNavigate?.();
  };
  const toggleRead = (n: InboxNotification) => update.mutate({ id: n.id, read: n.readAt === null });
  const archive = (n: InboxNotification) => update.mutate({ id: n.id, archived: n.archivedAt === null });
  const move = (d: number) => {
    if (rows.length === 0) return;
    const next = rows[Math.max(0, Math.min(rows.length - 1, (idx < 0 ? 0 : idx) + d))]!.id;
    // Same row (at an end): no focus move is coming, so a later refetch must not steal the focus.
    byKey.current = next !== focused?.id;
    setFocusId(next);
  };
  const handlers = {
    j: () => move(1),
    k: () => move(-1),
    Enter: () => {
      if (!focused) return;
      open(focused);
      nav(target(focused, org).to);
    },
    e: () => focused && archive(focused),
    u: () => focused && toggleRead(focused),
  };
  useHotkeys(handlers, keys === 'global');
  const onKeyDown = (ev: KeyboardEvent) => {
    if (keys !== 'local' || ev.metaKey || ev.ctrlKey || ev.altKey || isTypingTarget(ev.target)) return;
    // Enter on a focused link or button belongs to that control.
    if (ev.key === 'Enter' && ev.target instanceof HTMLElement && ['BUTTON', 'A'].includes(ev.target.tagName))
      return;
    const h = (handlers as Record<string, (() => void) | undefined>)[ev.key];
    if (h) {
      ev.preventDefault();
      h();
    }
  };

  // A failed refetch or Load more leaves query.data filled and sets isError: keep the rows, say so in one line.
  // The full error state is for a first load that has no data (LAUN-88).
  const failedMore = query.isError && query.isFetchNextPageError;
  const inlineError =
    query.isError && query.data ? (
      <p className={styles.inlineError} role="alert">
        <span>{failedMore ? 'Could not load more. ' : 'Could not refresh. '}</span>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost}`}
          disabled={query.isFetching}
          onClick={() => void (failedMore ? query.fetchNextPage() : query.refetch())}
        >
          Try again
        </button>
      </p>
    ) : null;

  let content;
  if (query.isPending) content = <SkeletonRows n={keys === 'local' ? 4 : 8} />;
  else if (query.isError && !query.data)
    content = <ErrorState error={query.error} onRetry={() => void query.refetch()} />;
  else if (rows.length === 0) {
    const e = EMPTY[filter];
    content = (
      <>
        {inlineError}
        <div className={styles.empty}>
          <strong>{e.title}</strong>
          <span>{e.text}</span>
        </div>
      </>
    );
  } else {
    content = (
      <>
        {inlineError}
        <ul className={styles.list} aria-label="Notifications">
          {rows.map((n) => (
            <Row
              key={n.id}
              n={n}
              org={org}
              focused={focused?.id === n.id}
              onOpen={open}
              onAux={(n) => n.readAt === null && update.mutate({ id: n.id, read: true })}
              onFocus={setFocusId}
              onRead={toggleRead}
              onArchive={archive}
            />
          ))}
        </ul>
        {query.hasNextPage && (
          <div className={styles.more}>
            <button
              type="button"
              className={ui.btn}
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {query.isFetchingNextPage ? 'Loading…' : 'Load more'}
            </button>
          </div>
        )}
      </>
    );
  }
  return (
    <div
      ref={root}
      className={`${className ?? ''} ${styles.listBox}`}
      onKeyDown={onKeyDown}
      onFocus={() => (inside.current = true)}
      onBlur={(e) => {
        if (e.relatedTarget) {
          if (!e.currentTarget.contains(e.relatedTarget as Node)) inside.current = false;
          return;
        }
        // Focus went nowhere (a click on blank space). A control that left with its row is not that: it is gone.
        const gone = e.target as HTMLElement;
        setTimeout(() => {
          if (gone.isConnected && !root.current?.contains(document.activeElement)) inside.current = false;
        }, 0);
      }}
    >
      {content}
    </div>
  );
}
