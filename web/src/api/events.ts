import { useEffect, useState } from 'react';
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { apiBase } from '../env';
import { keys } from './keys';
import { usingBearer } from './http';
import { client, unwrap } from './client';
import { fetchSession } from './auth';
import type { ApiEvent, Team } from './types';
import { bumpAccessGeneration } from './accessGen';

export type ConnState = 'connecting' | 'live' | 'reconnecting' | 'closed';

export const EVENT_TYPES: ApiEvent['type'][] = [
  'issue.created',
  'issue.updated',
  'issue.deleted',
  'comment.created',
  'comment.updated',
  'comment.deleted',
  'relation.created',
  'relation.deleted',
  'attachment.created',
  'label.created',
  // Every other type the server emits (decisions-T27; events.test.ts keeps this list equal to the server's).
  'project.created',
  'project.updated',
  'project_update.created',
  'project.update_due',
  'milestone.created',
  'milestone.updated',
  'document.created',
  'document.updated',
  'cycle.created',
  'cycle.updated',
  'cycle.completed',
  'cycle.deleted',
  'team.created',
  'team.updated',
  'team.deleted',
  'project.deleted',
  'project_status.created',
  'project_status.updated',
  'project_status.deleted',
  'project_status.reordered',
  'project_label.created',
  'project_label.updated',
  'project_label.deleted',
  'product.created',
  'product.updated',
  'product.deleted',
  'product.teams_changed',
  'org.updated',
  'onboarding.step_changed',
  'agent_guidance.updated',
  'git_identity.removed',
  'slack.updated',
  'chat_service.updated',
  'push.updated',
  'member.access_changed',
];

/** The slice of EventSource the stream needs. Tests pass a fake. */
export interface EventSourceLike {
  readyState: number;
  onopen: ((ev: Event) => void) | null;
  onerror: ((ev: Event) => void) | null;
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void;
  close(): void;
}
export type EventSourceFactory = (url: string, init: { withCredentials: boolean }) => EventSourceLike;
const defaultFactory: EventSourceFactory = (url, init) =>
  new EventSource(url, init) as unknown as EventSourceLike;

/** A project answer carries live issue counts (LAUN-170): an issue change refreshes the lists and the details. */
export function refreshProjectCounts(qc: QueryClient, org: string): void {
  void qc.invalidateQueries({ queryKey: keys.projectsAll(org) });
  void qc.invalidateQueries({ queryKey: ['org', org, 'projects'] });
  void qc.invalidateQueries({ queryKey: keys.projectDetails(org) });
}

/**
 * Turn a change-feed event into cache invalidation. Events are small (ids and
 * field names), so the cache is refreshed by refetching what is on screen.
 */
export function invalidateForEvent(qc: QueryClient, org: string, ev: Pick<ApiEvent, 'type'>): void {
  const inv = (queryKey: readonly unknown[]) => void qc.invalidateQueries({ queryKey });
  switch (ev.type) {
    case 'issue.created':
    case 'issue.updated':
      inv(keys.issues(org));
      inv(keys.issueDetails(org));
      inv(keys.issueSubscriptions(org)); // the assignee is a reason (LAUN-38); a second tab or a colleague may change it
      refreshProjectCounts(qc, org);
      break;
    case 'issue.deleted': // LAUN-201: the row, its comments, relations and attachments are gone; sub-issues lose the parent
      inv(keys.issues(org));
      inv(keys.issueDetails(org)); // an open page of it answers 404 once and shows "This issue was deleted"
      inv(keys.issueSubscriptions(org));
      inv(keys.commentsAll(org));
      inv(keys.inboxAll(org)); // its notifications cascade away
      inv(keys.unread(org));
      inv(['org', org, 'cycle-progress']);
      refreshProjectCounts(qc, org);
      break;
    case 'comment.created':
    case 'comment.updated':
    case 'comment.deleted':
      inv(keys.commentsAll(org));
      // A commenter is a reason (LAUN-38). An edit changes none, but the extra read is cheap.
      if (ev.type !== 'comment.updated') inv(keys.issueSubscriptions(org));
      break;
    case 'relation.created':
    case 'relation.deleted':
    case 'attachment.created': // a link or file shows in the Activity timeline (LAUN-48); the server emits no attachment.deleted
      inv(keys.issueDetails(org));
      break;
    case 'label.created':
      inv(keys.labels(org));
      break;
    case 'team.deleted':
      // Its labels, cycles and project links go with it (cascade, LAUN-191).
      refreshProjectCounts(qc, org);
      inv(keys.labels(org));
      inv(['org', org, 'cycles']);
      inv(keys.teams(org));
      inv(keys.products(org));
      inv(keys.issueDetails(org));
      break;
    case 'team.created':
    case 'team.updated': // a team's name, key or cycle switch shows in the sidebar
      inv(keys.teams(org));
      // LAUN-99: product team lists and issue details hold a copy of the team name.
      inv(keys.products(org));
      inv(keys.issueDetails(org));
      // LAUN-244: a change of a team's git workflow rules is a team.updated too.
      inv(['org', org, 'git-workflow']);
      break;
    // LAUN-172: a cycle's name, dates, completion or delete (its dates and its neighbour's move together;
    // a completion or a job run also moves issues, but those send issue.updated).
    case 'cycle.created':
    case 'cycle.updated':
    case 'cycle.completed':
    case 'cycle.deleted':
      inv(['org', org, 'cycles']);
      inv(['org', org, 'cycle-progress']);
      break;
    case 'project.deleted': // the project's issues lose it (LAUN-191)
      refreshProjectCounts(qc, org);
      inv(keys.issues(org));
      inv(keys.issueDetails(org));
      inv(['org', org, 'milestones']); // its milestones go with it (no documents cache exists)
      break;
    case 'project_update.created': // another person posted an update (LAUN-196)
      inv(['org', org, 'project-updates']);
      break;
    case 'project_status.created': // LAUN-195: a status's name and colour show on every project
    case 'project_status.updated':
    case 'project_status.deleted':
    case 'project_status.reordered':
      inv(keys.projectStatuses(org));
      refreshProjectCounts(qc, org);
      break;
    // LAUN-194: project labels. A project answer carries its labels, and a label delete or rename changes them
    // without a project.updated, so the project queries are refreshed too.
    case 'project_label.created':
    case 'project_label.updated':
    case 'project_label.deleted':
      inv(keys.projectLabels(org));
      refreshProjectCounts(qc, org);
      break;
    case 'product.created':
    case 'product.updated': // also archive and restore
    case 'product.deleted':
    case 'product.teams_changed':
      // The product list gives the groups; Team.productIds (in the teams list) gives the members.
      inv(keys.products(org));
      inv(keys.teams(org));
      break;
    case 'org.updated': // the name, time zone or default team (LAUN-98); the sidebar reads the name from the session
      inv(keys.orgDetail(org));
      inv(keys.onboarding(org));
      inv(keys.session);
      break;
    case 'agent_guidance.updated': // another admin wrote or removed guidance (LAUN-200)
      inv(keys.agentGuidance(org));
      break;
    case 'git_identity.removed': // an admin removed a link (LAUN-199): my list, the admin list and the Changes blocks
      inv(keys.gitAccounts(org));
      inv(keys.gitIdentities(org));
      inv(keys.issueDetails(org));
      break;
    case 'slack.updated': // legacy (LAUN-198): old feed rows; nothing emits it since LAUN-204
    case 'chat_service.updated': // another admin added, changed or removed a chat service (LAUN-204)
      inv(keys.chat(org));
      break;
    case 'push.updated': // another admin set, changed or removed the push relay (LAUN-197)
      inv(keys.push(org));
      inv(keys.pushDevices(org)); // relayConfigured
      inv(keys.notificationSettings(org)); // the Push column shows only with a relay
      break;
    case 'onboarding.step_changed': // another admin skipped or undid a step
      inv(keys.onboarding(org));
      break;
    case 'member.access_changed': // admins only (LAUN-225, decisions-T20 #48): a member's team access shows in the list
      inv(keys.members(org));
      break;
  }
}

/** The inbox and its count belong to the inbox stream, which reads them on its own `access` event (T20 #62). */
const inboxOwned = (k: readonly unknown[]) => k[2] === 'inbox' || k[2] === 'unread';

const upper = (v: unknown) => (typeof v === 'string' ? v.toUpperCase() : '');
/** The team key of an identifier (`HID-12` -> `HID`). */
const prefix = (v: unknown) => upper(v).slice(0, Math.max(0, upper(v).lastIndexOf('-')));

/** Queries that belong to one team: an issue by its identifier, or a team-keyed list. */
function teamOf(k: readonly unknown[]): string | undefined {
  switch (k[2]) {
    case 'issue':
    case 'comments':
    case 'issue-subscription':
      return prefix(k[3]) || undefined;
    case 'states':
    case 'cycles':
    case 'projects':
      return upper(k[3]) || undefined;
  }
  return undefined;
}
/**
 * Lists and details that mix rows of many teams, or hold a project that may belong to a lost team only: after a team
 * loss their cached rows may hold hidden ones, so they are emptied too. (Org-level kinds, such as the members, the
 * project statuses and the project labels, name no team row and stay.)
 */
const MIXED = new Set([
  'issues', // lists, the board, the search and the palette results
  'projects-all',
  'project', // a project's detail: it may be a project of the lost team only
  'project-updates',
  'milestones',
  'cycle-progress',
  'labels', // team labels
  'products',
]);

// ---- the last team set this module saw, per query client and org (not the teams query itself) ----

const seenTeams = new WeakMap<QueryClient, Map<string, Set<string>>>();
const teamSet = (list: Team[]) => new Set(list.map((t) => t.key.toUpperCase()));
const recorded = (qc: QueryClient, org: string) => seenTeams.get(qc)?.get(org);
function record(qc: QueryClient, org: string, set: Set<string>) {
  let m = seenTeams.get(qc);
  if (!m) seenTeams.set(qc, (m = new Map()));
  m.set(org, set);
}

const notInbox = (q: { queryKey: readonly unknown[] }) => !inboxOwned(q.queryKey);
const lostPredicate = (org: string, lost: Set<string>) => (q: { queryKey: readonly unknown[] }) => {
  if (!notInbox(q) || JSON.stringify(q.queryKey) === JSON.stringify(keys.teams(org))) return false;
  const team = teamOf(q.queryKey);
  return (team !== undefined && lost.has(team)) || MIXED.has(q.queryKey[2] as string);
};

/**
 * Compare a fresh team list with the last set recorded for this org and record it. On a loss, the queries of the
 * lost teams and the MIXED ones are dropped at once: not on screen, removed; on screen, reset (no data while they
 * load again). Idempotent: the same list a second time finds no loss. The first list only records.
 * Returns the lost and the gained keys (empty on a first record).
 */
export function applyTeamList(
  qc: QueryClient,
  org: string,
  list: Team[],
): { lost: Set<string>; gained: Set<string>; first: boolean } {
  const now = teamSet(list);
  const prev = recorded(qc, org);
  record(qc, org, now);
  if (!prev) return { lost: new Set(), gained: new Set(), first: true };
  const lost = new Set([...prev].filter((k) => !now.has(k)));
  const gained = new Set([...now].filter((k) => !prev.has(k)));
  if (lost.size) {
    bumpAccessGeneration(qc, org);
    const drop = lostPredicate(org, lost);
    qc.removeQueries({ queryKey: keys.org(org), type: 'inactive', predicate: drop });
    void qc.resetQueries({ queryKey: keys.org(org), predicate: drop });
  }
  return { lost, gained, first: false };
}

/**
 * Watch the team list of an org, whatever reads it (a focus refetch, a team event, a mount, this module): any
 * successful answer that has lost a team drops that team's data (applyTeamList). One watcher per client and org
 * (counted, so two users of it share one). Returns the stop function.
 */
const watchers = new WeakMap<QueryClient, Map<string, { n: number; off: () => void }>>();
export function watchTeamList(qc: QueryClient, org: string): () => void {
  let m = watchers.get(qc);
  if (!m) watchers.set(qc, (m = new Map()));
  const have = m.get(org);
  if (have) have.n += 1;
  else {
    const key = JSON.stringify(keys.teams(org));
    const cached = qc.getQueryData<Team[]>(keys.teams(org));
    if (cached && !recorded(qc, org)) record(qc, org, teamSet(cached));
    const off = qc.getQueryCache().subscribe((e) => {
      if (e.type !== 'updated' || e.action.type !== 'success') return;
      if (JSON.stringify(e.query.queryKey) !== key) return;
      const list = e.query.state.data as Team[] | undefined;
      // Out of the cache's notify loop: the drop changes other queries.
      if (list) void Promise.resolve().then(() => applyTeamList(qc, org, list));
    });
    m.set(org, { n: 1, off });
  }
  let done = false;
  return () => {
    if (done) return;
    done = true;
    const w = m.get(org);
    if (!w) return;
    w.n -= 1;
    if (w.n === 0) {
      w.off();
      m.delete(org);
    }
  };
}

/**
 * decisions-T20 #56, #76: the caller's role or team access changed (an `access` event), or may have changed (a
 * reconnect after a gap, see OrgEvents). The session and the team list are read again; the cache then follows what
 * really changed, compared with the last team set this module recorded (not the cached list, which a focus refetch
 * may have updated already), so a page whose data stays valid keeps its state (a comment draft, an open edit):
 *
 * - A team lost (now or by an earlier read of the list, see watchTeamList): that team's data and the MIXED queries
 *   were dropped by applyTeamList; everything else refetches in place.
 * - Only gains, or a role change (an `access` event): everything else refetches in place. With `onlyIfChanged`
 *   (a reconnect) and nothing changed since the last record, nothing more is read.
 * - The team list cannot be read, or was never read: reset everything (the safe side); on a reconnect, nothing.
 *
 * The inbox and the count are left to the inbox stream, which reads them on its own `access` event. It touches no
 * stream, so it cannot start a reconnect loop.
 */
export async function refreshForAccess(
  qc: QueryClient,
  org: string,
  opts: { onlyIfChanged?: boolean } = {},
): Promise<void> {
  void qc.invalidateQueries({ queryKey: keys.session });
  void qc.invalidateQueries({ queryKey: keys.orgList });
  const teamsKey = keys.teams(org);
  const cached = qc.getQueryData<Team[]>(teamsKey);
  if (cached && !recorded(qc, org)) record(qc, org, teamSet(cached));
  const before = recorded(qc, org);
  let after: Team[] | undefined;
  if (before) {
    await qc.refetchQueries({ queryKey: teamsKey, exact: true });
    const st = qc.getQueryState<Team[]>(teamsKey);
    after = st?.status === 'success' && st.fetchStatus === 'idle' && st.error === null ? st.data : undefined;
  }
  // A reconnect that cannot read the list changes nothing: the stream is likely down again, and its next reconnect
  // checks again (an open issue of a lost team still shows Not found on its own, IssueDetailPage).
  if ((!before || !after) && opts.onlyIfChanged) return;
  if (!before || !after) {
    bumpAccessGeneration(qc, org);
    qc.removeQueries({ queryKey: keys.org(org), type: 'inactive', predicate: notInbox });
    await qc.resetQueries({ queryKey: keys.org(org), predicate: notInbox });
    return;
  }
  // `before` is the set from before this read; a watcher may have applied the new list already (then this finds no
  // loss and drops nothing twice).
  const now = teamSet(after);
  const lost = new Set([...before].filter((k) => !now.has(k)));
  const gained = [...now].some((k) => !before.has(k));
  applyTeamList(qc, org, after);
  if (opts.onlyIfChanged && lost.size === 0 && !gained) return;
  const drop = lostPredicate(org, lost);
  await qc.invalidateQueries({
    queryKey: keys.org(org),
    predicate: (q) =>
      notInbox(q) && JSON.stringify(q.queryKey) !== JSON.stringify(teamsKey) && !(lost.size && drop(q)),
  });
}

export interface OrgEventsOptions {
  org: string;
  qc: QueryClient;
  factory?: EventSourceFactory;
  mode?: 'sse' | 'poll';
  /** Returns false when the credentials are gone, so the stream stops instead of retrying. */
  probe?: () => Promise<boolean>;
  onState?: (s: ConnState) => void;
  onUnauthorized?: () => void;
  baseDelayMs?: number;
  maxDelayMs?: number;
  batchMs?: number;
  pollMs?: number;
  /** The stream must stay open this long before the backoff resets (or send a message). */
  stableMs?: number;
  /** At most one team-list check after a reconnect in this time. */
  gapCheckMs?: number;
  random?: () => number;
}

export class OrgEvents {
  lastSeq: number | undefined;
  state: ConnState = 'connecting';
  attempts = 0;
  private src: EventSourceLike | undefined;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private batch: ApiEvent['type'][] = [];
  private access = false;
  private resync = false;
  private stableTimer: ReturnType<typeof setTimeout> | undefined;
  private gapTimer: ReturnType<typeof setTimeout> | undefined;
  private lastGapCheck = -Infinity;
  private unwatch: (() => void) | undefined;
  private batchTimer: ReturnType<typeof setTimeout> | undefined;
  private stopped = false;
  private o: Required<
    Pick<
      OrgEventsOptions,
      'baseDelayMs' | 'maxDelayMs' | 'batchMs' | 'pollMs' | 'stableMs' | 'gapCheckMs' | 'random'
    >
  > &
    OrgEventsOptions;

  constructor(opts: OrgEventsOptions) {
    this.o = {
      baseDelayMs: 1000,
      maxDelayMs: 30_000,
      batchMs: 80,
      pollMs: 4000,
      stableMs: 10_000,
      gapCheckMs: 30_000,
      random: Math.random,
      ...opts,
    };
  }

  start(): void {
    this.stopped = false;
    // A focus refetch, a team event or a mount may read the team list before this stream says anything: a loss
    // found by any of them drops that team's data (T20 #76).
    this.unwatch ??= watchTeamList(this.o.qc, this.o.org);
    if (this.o.mode === 'poll') void this.poll(true);
    else this.connect();
  }

  stop(): void {
    this.stopped = true;
    this.unwatch?.();
    this.unwatch = undefined;
    this.src?.close();
    this.src = undefined;
    clearTimeout(this.timer);
    clearTimeout(this.batchTimer);
    clearTimeout(this.stableTimer);
    clearTimeout(this.gapTimer);
    this.gapTimer = undefined;
    this.set('closed');
  }

  private set(s: ConnState) {
    if (this.state === s) return;
    this.state = s;
    this.o.onState?.(s);
  }

  url(): string {
    const after = this.lastSeq !== undefined ? `?after=${this.lastSeq}` : '';
    return `${apiBase()}/api/v1/orgs/${encodeURIComponent(this.o.org)}/events/stream${after}`;
  }

  private connect(): void {
    if (this.stopped) return;
    // EventSource cannot set Last-Event-ID itself, so a resume uses `?after=`,
    // which the server treats the same way.
    const src = (this.o.factory ?? defaultFactory)(this.url(), { withCredentials: true });
    this.src = src;
    src.onopen = () => {
      this.set('live');
      // The backoff resets on the first real message, or once the stream has stayed open for a while: a stream
      // that opens and fails at once keeps backing off.
      clearTimeout(this.stableTimer);
      this.stableTimer = setTimeout(() => (this.attempts = 0), this.o.stableMs);
      // A reconnect after an error: the server compares access with what it read when THIS connection opened, so
      // a change made while the stream was down sends no `access`. Check the team list (T20 #56).
      if (this.resync) {
        this.resync = false;
        this.gapCheck();
      }
    };
    const onMsg = (ev: MessageEvent) => {
      this.attempts = 0;
      this.set('live');
      this.handle(ev.data as string, ev.lastEventId);
    };
    for (const t of EVENT_TYPES) src.addEventListener(t, onMsg);
    src.addEventListener('message', onMsg);
    // T20 #56: not a feed row (no seq, not stored), so the cursor does not move.
    src.addEventListener('access', () => {
      this.attempts = 0;
      this.set('live');
      this.queueAccess();
    });
    src.onerror = () => {
      src.close();
      clearTimeout(this.stableTimer);
      if (this.src === src) this.src = undefined;
      this.scheduleReconnect();
    };
  }

  /**
   * After a gap: read the team list again and refresh only when it changed. At most once per `gapCheckMs`; a check
   * asked for inside the window runs once at its end, so a flapping stream cannot cause a refetch storm.
   */
  private gapCheck(): void {
    if (this.gapTimer) return;
    const run = () => {
      this.gapTimer = undefined;
      if (this.stopped) return;
      this.lastGapCheck = Date.now();
      void refreshForAccess(this.o.qc, this.o.org, { onlyIfChanged: true });
    };
    const wait = this.lastGapCheck + this.o.gapCheckMs - Date.now();
    if (wait <= 0) run();
    else this.gapTimer = setTimeout(run, wait);
  }

  private handle(data: string, lastEventId?: string): void {
    let ev: ApiEvent | undefined;
    try {
      ev = JSON.parse(data) as ApiEvent;
    } catch {
      return; // a malformed frame is ignored, never fatal
    }
    const seq = typeof ev.seq === 'number' ? ev.seq : Number(lastEventId);
    if (Number.isFinite(seq)) this.lastSeq = Math.max(this.lastSeq ?? 0, seq);
    this.queue(ev.type);
  }

  private queue(type: ApiEvent['type'] | null): void {
    if (type) this.batch.push(type);
    if (this.batchTimer) return;
    this.batchTimer = setTimeout(() => {
      this.batchTimer = undefined;
      const types = new Set(this.batch);
      this.batch = [];
      if (this.access) {
        // It refetches everything of the org: the per-type refreshes are inside it.
        this.access = false;
        void refreshForAccess(this.o.qc, this.o.org);
        return;
      }
      for (const t of types) invalidateForEvent(this.o.qc, this.o.org, { type: t });
    }, this.o.batchMs);
  }

  /** One refresh per batch window, however many `access` events arrive in it. */
  private queueAccess(): void {
    this.access = true;
    this.queue(null);
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

  // ---- polling fallback (bearer tokens: EventSource cannot send them) ----

  private async poll(first: boolean): Promise<void> {
    if (this.stopped) return;
    try {
      let after = this.lastSeq ?? 0;
      for (let page = 0; page < 50; page++) {
        const r = await unwrap(
          client.GET('/api/v1/orgs/{org}/events', {
            params: { path: { org: this.o.org }, query: { after, limit: 500 } },
          }),
        );
        for (const ev of r.items) {
          this.lastSeq = Math.max(this.lastSeq ?? 0, ev.seq);
          if (!first) this.queue(ev.type);
        }
        if (!r.nextCursor) break;
        after = Number(r.nextCursor);
        if (!Number.isFinite(after)) break;
        // The server may read past the last item (events this member cannot
        // see, T20 #55); its cursor is the place to resume from.
        this.lastSeq = Math.max(this.lastSeq ?? 0, after);
      }
      this.attempts = 0;
      this.set('live');
    } catch {
      this.set('reconnecting');
      this.attempts += 1;
    }
    if (this.stopped) return;
    this.timer = setTimeout(() => void this.poll(false), this.attempts ? this.nextDelay() : this.o.pollMs);
  }
}

/** One live connection per org, for as long as the shell is mounted. */
export function useOrgEvents(org: string, onUnauthorized: () => void): ConnState {
  const qc = useQueryClient();
  const [state, setState] = useState<ConnState>('connecting');
  useEffect(() => {
    setState('connecting');
    const ev = new OrgEvents({
      org,
      qc,
      mode: usingBearer() ? 'poll' : 'sse',
      probe: async () => (await fetchSession()) !== null,
      onState: setState,
      onUnauthorized,
    });
    ev.start();
    return () => ev.stop();
  }, [org, qc, onUnauthorized]);
  return state;
}
