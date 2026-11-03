import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type QueryClient,
} from '@tanstack/react-query';
import { useEffect } from 'react';
import { apiBase } from '../env';
import { client, unwrap } from './client';
import { errorMessage } from './errors';
import { fetchSession } from './auth';
import { usingBearer } from './http';
import { keys } from './keys';
import { useOrgSlug } from './org';
import type { InboxNotification, InboxNotificationList } from './types';
import { accessGeneration, bumpAccessGeneration } from './accessGen';
import type { EventSourceFactory } from './events';
import { useToast } from '../ui/Toast';

// The inbox of the signed-in person (decisions-T13 #27-#31, LAUN-37).

export type InboxFilter = 'unread' | 'all' | 'archived';
export const INBOX_FILTERS: InboxFilter[] = ['unread', 'all', 'archived'];
const PAGE = 30;

type InboxData = InfiniteData<InboxNotificationList>;
type CountData = { count: number };

/** Does the row show in this filter's list? */
export function inFilter(f: InboxFilter, n: Pick<InboxNotification, 'readAt' | 'archivedAt'>): boolean {
  if (f === 'archived') return n.archivedAt !== null;
  if (n.archivedAt !== null) return false;
  return f === 'all' || n.readAt === null;
}

/** Does the row count in the unread badge? */
export const counts = (n: Pick<InboxNotification, 'readAt' | 'archivedAt'>): boolean =>
  n.readAt === null && n.archivedAt === null;

export function filterQuery(f: InboxFilter): { state: 'unread' | 'all'; archived: boolean } {
  return { state: f === 'unread' ? 'unread' : 'all', archived: f === 'archived' };
}

export function useUnreadCount() {
  const org = useOrgSlug();
  return useQuery({
    queryKey: keys.unread(org),
    queryFn: () =>
      unwrap(client.GET('/api/v1/orgs/{org}/notifications/unread-count', { params: { path: { org } } })),
    staleTime: 30_000,
  });
}

export function useInbox(filter: InboxFilter) {
  const org = useOrgSlug();
  const q = filterQuery(filter);
  return useInfiniteQuery({
    queryKey: keys.inbox(org, filter),
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      unwrap(
        client.GET('/api/v1/orgs/{org}/notifications', {
          params: { path: { org }, query: { ...q, limit: PAGE, cursor: pageParam } },
        }),
      ),
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
}

/** The loaded rows of an inbox query, flat. */
export function flatRows(data: InboxData | undefined): InboxNotification[] {
  return data?.pages.flatMap((p) => p.items) ?? [];
}

function mapLists(
  qc: QueryClient,
  org: string,
  fn: (f: InboxFilter, rows: InboxNotification[]) => InboxNotification[],
) {
  for (const f of INBOX_FILTERS) {
    qc.setQueryData<InboxData>(keys.inbox(org, f), (old) =>
      old ? { ...old, pages: old.pages.map((p) => ({ ...p, items: fn(f, p.items) })) } : old,
    );
  }
}

function findRow(qc: QueryClient, org: string, id: string): InboxNotification | undefined {
  for (const f of INBOX_FILTERS) {
    const row = flatRows(qc.getQueryData<InboxData>(keys.inbox(org, f))).find((r) => r.id === id);
    if (row) return row;
  }
  return undefined;
}

/**
 * Mark one row read / unread or archive / unarchive it. The lists and the badge move at once and go back when
 * the server says no (the PATCH sets a state, so a repeat is harmless).
 */
export function useUpdateNotification() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const toast = useToast();
  return useMutation({
    mutationFn: ({ id, ...body }: { id: string; read?: boolean; archived?: boolean }) =>
      unwrap(
        client.PATCH('/api/v1/orgs/{org}/notifications/{id}', {
          params: { path: { org, id } },
          body,
        }),
      ),
    onMutate: async ({ id, read, archived }) => {
      await qc.cancelQueries({ queryKey: keys.inboxAll(org) });
      await qc.cancelQueries({ queryKey: keys.unread(org) });
      const snapshot = INBOX_FILTERS.map((f) => [f, qc.getQueryData<InboxData>(keys.inbox(org, f))] as const);
      const count = qc.getQueryData<CountData>(keys.unread(org));
      const before = findRow(qc, org, id);
      if (before) {
        const after: InboxNotification = {
          ...before,
          readAt:
            read === undefined ? before.readAt : read ? (before.readAt ?? new Date().toISOString()) : null,
          archivedAt:
            archived === undefined
              ? before.archivedAt
              : archived
                ? (before.archivedAt ?? new Date().toISOString())
                : null,
        };
        mapLists(qc, org, (f, rows) =>
          rows.flatMap((r) => (r.id !== id ? [r] : inFilter(f, after) ? [after] : [])),
        );
        if (count) {
          const delta = Number(counts(after)) - Number(counts(before));
          qc.setQueryData<CountData>(keys.unread(org), { count: Math.max(0, count.count + delta) });
        }
      }
      return { snapshot, count, gen: accessGeneration(qc, org) };
    },
    onError: (err, _v, ctx) => {
      // Not after an access reset of the inbox (T20 #76): the snapshot may hold rows of a team now hidden.
      if (ctx && ctx.gen === accessGeneration(qc, org)) {
        for (const [f, data] of ctx.snapshot) qc.setQueryData(keys.inbox(org, f), data);
        if (ctx.count) qc.setQueryData(keys.unread(org), ctx.count);
      }
      toast.show(errorMessage(err), { kind: 'error' });
    },
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: keys.unread(org) });
      void qc.invalidateQueries({ queryKey: keys.inboxAll(org) });
    },
  });
}

/**
 * Mark every unread row up to `before` read. `before` is the createdAt of the newest row the person has
 * loaded: the API requires it, and a retry marks the same rows (decisions-T13 #27).
 */
export function useReadAll() {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const toast = useToast();
  return useMutation({
    mutationFn: (before: string) =>
      unwrap(
        client.POST('/api/v1/orgs/{org}/notifications/read-all', {
          params: { path: { org } },
          body: { before },
        }),
      ),
    onMutate: async (before) => {
      await qc.cancelQueries({ queryKey: keys.inboxAll(org) });
      await qc.cancelQueries({ queryKey: keys.unread(org) });
      const snapshot = INBOX_FILTERS.map((f) => [f, qc.getQueryData<InboxData>(keys.inbox(org, f))] as const);
      const count = qc.getQueryData<CountData>(keys.unread(org));
      const t = Date.parse(before);
      const now = new Date().toISOString();
      let newer = 0;
      mapLists(qc, org, (f, rows) =>
        rows.flatMap((r) => {
          if (r.readAt !== null || r.archivedAt !== null || Date.parse(r.createdAt) > t) return [r];
          const after = { ...r, readAt: now };
          return inFilter(f, after) ? [after] : [];
        }),
      );
      // Rows newer than `before` (seen in another list) stay unread; the refetch below corrects the rest.
      for (const r of flatRows(qc.getQueryData<InboxData>(keys.inbox(org, 'all'))))
        if (counts(r) && Date.parse(r.createdAt) > t) newer++;
      if (count) qc.setQueryData<CountData>(keys.unread(org), { count: newer });
      return { snapshot, count, gen: accessGeneration(qc, org) };
    },
    onError: (err, _v, ctx) => {
      // Not after an access reset of the inbox (T20 #76): the snapshot may hold rows of a team now hidden.
      if (ctx && ctx.gen === accessGeneration(qc, org)) {
        for (const [f, data] of ctx.snapshot) qc.setQueryData(keys.inbox(org, f), data);
        if (ctx.count) qc.setQueryData(keys.unread(org), ctx.count);
      }
      toast.show(errorMessage(err), { kind: 'error' });
    },
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: keys.unread(org) });
      void qc.invalidateQueries({ queryKey: keys.inboxAll(org) });
    },
  });
}

// ---------- the live stream ----------

const defaultFactory: EventSourceFactory = (url, init) =>
  new EventSource(url, init) as unknown as ReturnType<EventSourceFactory>;

export type InboxState = 'connecting' | 'live' | 'reconnecting' | 'closed';

export interface InboxStreamOptions {
  org: string;
  qc: QueryClient;
  factory?: EventSourceFactory;
  /** `poll`: no stream at all (a bearer token cannot be sent by EventSource). */
  mode?: 'sse' | 'poll';
  probe?: () => Promise<boolean>;
  onState?: (s: InboxState) => void;
  onUnauthorized?: () => void;
  baseDelayMs?: number;
  maxDelayMs?: number;
  batchMs?: number;
  /** The fallback: the unread count is read this often while the stream is not open. */
  pollMs?: number;
  random?: () => number;
}

/**
 * The inbox stream (`GET /notifications/stream`). It is a hint, the tables are the truth: `count` sets the
 * badge, `notification` refreshes the lists. While the stream is not open (or never will be) the unread
 * count is polled every 60 seconds (decisions-T13 #31).
 */
export class InboxStream {
  lastSeq: number | undefined;
  state: InboxState = 'connecting';
  attempts = 0;
  private src: ReturnType<EventSourceFactory> | undefined;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private pollTimer: ReturnType<typeof setInterval> | undefined;
  private batchTimer: ReturnType<typeof setTimeout> | undefined;
  private accessTimer: ReturnType<typeof setTimeout> | undefined;
  private stopped = false;
  private resync = false;
  private o: Required<
    Pick<InboxStreamOptions, 'baseDelayMs' | 'maxDelayMs' | 'batchMs' | 'pollMs' | 'random'>
  > &
    InboxStreamOptions;

  constructor(opts: InboxStreamOptions) {
    this.o = {
      baseDelayMs: 1000,
      maxDelayMs: 30_000,
      batchMs: 80,
      pollMs: 60_000,
      random: Math.random,
      ...opts,
    };
  }

  start(): void {
    this.stopped = false;
    this.pollTimer = setInterval(() => {
      if (this.state !== 'live') void this.refreshCount();
    }, this.o.pollMs);
    if (this.o.mode !== 'poll') this.connect();
  }

  stop(): void {
    this.stopped = true;
    this.src?.close();
    this.src = undefined;
    clearTimeout(this.timer);
    clearTimeout(this.batchTimer);
    clearTimeout(this.accessTimer);
    clearInterval(this.pollTimer);
    this.set('closed');
  }

  url(): string {
    const after = this.lastSeq !== undefined ? `?after=${this.lastSeq}` : '';
    return `${apiBase()}/api/v1/orgs/${encodeURIComponent(this.o.org)}/notifications/stream${after}`;
  }

  private set(s: InboxState) {
    if (this.state === s) return;
    this.state = s;
    this.o.onState?.(s);
  }

  private lists() {
    void this.o.qc.invalidateQueries({ queryKey: keys.inboxAll(this.o.org) });
  }

  /** Read the count; when it moved, the lists are out of date too. */
  async refreshCount(): Promise<void> {
    try {
      const r = await unwrap(
        client.GET('/api/v1/orgs/{org}/notifications/unread-count', {
          params: { path: { org: this.o.org } },
        }),
      );
      if (!this.stopped) this.setCount(r.count);
    } catch {
      /* the next tick tries again */
    }
  }

  private setCount(count: number): void {
    const key = keys.unread(this.o.org);
    const old = this.o.qc.getQueryData<CountData>(key);
    this.o.qc.setQueryData<CountData>(key, { count });
    if (old && old.count !== count) this.lists();
  }

  private connect(): void {
    if (this.stopped) return;
    const src = (this.o.factory ?? defaultFactory)(this.url(), { withCredentials: true });
    this.src = src;
    const live = () => {
      // The backoff resets on the first `count` event (the server's proof that the stream works), not on
      // `open`: a stream that opens and drops again at once must keep backing off.
      this.set('live');
      if (this.resync) {
        this.resync = false;
        this.lists(); // rows may have come or gone while the stream was down
        void this.o.qc.invalidateQueries({ queryKey: keys.unread(this.o.org) });
      }
    };
    src.onopen = live;
    src.addEventListener('count', (ev) => {
      this.attempts = 0;
      live();
      try {
        const d = JSON.parse(ev.data as string) as { count?: unknown };
        if (typeof d.count === 'number') this.setCount(d.count);
      } catch {
        /* a malformed frame is ignored */
      }
    });
    src.addEventListener('notification', (ev) => {
      live();
      try {
        const d = JSON.parse(ev.data as string) as { inboxSeq?: unknown };
        const seq = typeof d.inboxSeq === 'number' ? d.inboxSeq : Number(ev.lastEventId);
        if (Number.isFinite(seq)) this.lastSeq = Math.max(this.lastSeq ?? 0, seq);
      } catch {
        return;
      }
      if (this.batchTimer) return;
      this.batchTimer = setTimeout(() => {
        this.batchTimer = undefined;
        this.lists();
        void this.o.qc.invalidateQueries({ queryKey: keys.unread(this.o.org) });
      }, this.o.batchMs);
    });
    // decisions-T20 #56, #62: the caller's role or team access changed (data `{}`, no id). The stream sends only rows
    // after its cursor, so the older rows of a team that is now visible never come: read the lists and the count
    // again. The cursor stays; the connection stays (no reconnect, so no loop).
    src.addEventListener('access', () => {
      live(); // the backoff resets on `count` only (above)
      // One burst, one read of each. The rows of a team that is now hidden go at once (reset, not invalidate).
      if (this.accessTimer) return;
      this.accessTimer = setTimeout(() => {
        this.accessTimer = undefined;
        bumpAccessGeneration(this.o.qc, this.o.org);
        void this.o.qc.resetQueries({ queryKey: keys.inboxAll(this.o.org) });
        void this.o.qc.invalidateQueries({ queryKey: keys.unread(this.o.org) });
      }, this.o.batchMs);
    });
    src.onerror = () => {
      src.close();
      if (this.src === src) this.src = undefined;
      this.scheduleReconnect();
    };
  }

  nextDelay(): number {
    const exp = Math.min(this.o.maxDelayMs, this.o.baseDelayMs * 2 ** Math.min(this.attempts, 10));
    return Math.round(exp / 2 + this.o.random() * (exp / 2));
  }

  private scheduleReconnect(): void {
    if (this.stopped) return;
    this.set('reconnecting');
    this.resync = true;
    const delay = this.nextDelay();
    this.attempts += 1;
    this.timer = setTimeout(async () => {
      if (this.stopped) return;
      if (this.o.probe) {
        let ok = true;
        try {
          ok = await this.o.probe();
        } catch {
          ok = true; // network down: keep retrying
        }
        if (this.stopped) return;
        if (!ok) {
          this.stop();
          this.o.onUnauthorized?.();
          return;
        }
      }
      this.connect();
    }, delay);
  }
}

/** One inbox stream per org, for as long as the shell is mounted. */
export function useInboxLive(org: string, onUnauthorized: () => void): void {
  const qc = useQueryClient();
  useEffect(() => {
    const s = new InboxStream({
      org,
      qc,
      mode: usingBearer() ? 'poll' : 'sse',
      probe: async () => (await fetchSession()) !== null,
      onUnauthorized,
    });
    s.start();
    return () => s.stop();
  }, [org, qc, onUnauthorized]);
}
