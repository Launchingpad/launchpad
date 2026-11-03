import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeEventSource } from '../test/fakes';
import {
  EVENT_TYPES,
  OrgEvents,
  invalidateForEvent,
  refreshForAccess,
  watchTeamList,
  type ConnState,
} from './events';
import { accessGeneration, restoreSnapshot } from './accessGen';
import { keys } from './keys';
import { mockApi } from '../test/mockApi';

const ev = (seq: number, type: string, extra: object = {}) => ({
  seq,
  type,
  entityType: 'issue',
  entityId: 'x',
  actor: null,
  payload: {},
  createdAt: '2026-10-24T00:00:00Z',
  ...extra,
});

function make(extra: Partial<ConstructorParameters<typeof OrgEvents>[0]> = {}) {
  const qc = new QueryClient();
  const spy = vi.spyOn(qc, 'invalidateQueries');
  const states: ConnState[] = [];
  const events = new OrgEvents({
    org: 'orbit',
    qc,
    factory: (url, init) => new FakeEventSource(url, init),
    onState: (s) => states.push(s),
    random: () => 1,
    ...extra,
  });
  return { qc, spy, states, events };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('OrgEvents (SSE)', () => {
  it('opens one cookie-credentialed stream for the org', () => {
    const { events } = make();
    events.start();
    expect(FakeEventSource.instances).toHaveLength(1);
    const src = FakeEventSource.last!;
    expect(src.url).toContain('/api/v1/orgs/orbit/events/stream');
    expect(src.url).not.toContain('after=');
    expect(src.init).toEqual({ withCredentials: true });
    events.stop();
  });

  it('turns issue events into cache invalidation (another tab updates without refresh)', () => {
    const { events, spy } = make();
    events.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit('issue.updated', ev(5, 'issue.updated'));
    expect(spy).not.toHaveBeenCalled(); // batched
    vi.advanceTimersByTime(100);
    const calledKeys = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(calledKeys).toContain(JSON.stringify(keys.issues('orbit')));
    expect(calledKeys).toContain(JSON.stringify(keys.issueDetails('orbit')));
    events.stop();
  });

  it('batches a burst of events into one invalidation per kind', () => {
    const { events, spy } = make();
    events.start();
    for (let i = 1; i <= 20; i++) FakeEventSource.last!.emit('issue.updated', ev(i, 'issue.updated'));
    vi.advanceTimersByTime(100);
    expect(spy).toHaveBeenCalledTimes(6); // lists + details + subscription + project lists, team lists and details (LAUN-170), once
    events.stop();
  });

  it('refetches data on screen for real: a cached list becomes stale', async () => {
    const qc = new QueryClient();
    qc.setQueryData(keys.issueList('orbit', { orderBy: 'updatedAt' }), { pages: [], pageParams: [] });
    qc.setQueryData(keys.comments('orbit', 'ORB-1'), []);
    invalidateForEvent(qc, 'orbit', { type: 'issue.created' });
    invalidateForEvent(qc, 'orbit', { type: 'comment.created' });
    expect(qc.getQueryState(keys.issueList('orbit', { orderBy: 'updatedAt' }))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.comments('orbit', 'ORB-1'))?.isInvalidated).toBe(true);
  });

  it('refreshes the issue subscription on issue and comment events (LAUN-38)', () => {
    for (const type of ['issue.created', 'issue.updated', 'comment.created', 'comment.deleted'] as const) {
      const qc = new QueryClient();
      qc.setQueryData(keys.issueSubscription('orbit', 'ORB-1'), { state: 'default', reasons: [] });
      qc.setQueryData(keys.issueSubscription('other', 'ORB-1'), { state: 'default', reasons: [] });
      invalidateForEvent(qc, 'orbit', { type });
      expect(qc.getQueryState(keys.issueSubscription('orbit', 'ORB-1'))?.isInvalidated, type).toBe(true);
      expect(qc.getQueryState(keys.issueSubscription('other', 'ORB-1'))?.isInvalidated, type).toBe(false);
    }
  });

  it('issue events refresh the project lists and details, which carry issue counts (LAUN-170)', () => {
    for (const type of ['issue.created', 'issue.updated'] as const) {
      const qc = new QueryClient();
      const ks = [keys.projectsAll('orbit'), keys.projects('orbit', 'ORB'), keys.project('orbit', 'p1')];
      const other = keys.projectsAll('other');
      for (const k of [...ks, other]) qc.setQueryData(k, {});
      invalidateForEvent(qc, 'orbit', { type });
      for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, `${type} ${k.join('/')}`).toBe(true);
      expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
    }
  });

  it('team.deleted refreshes the project, label and cycle caches too (LAUN-191)', () => {
    const qc = new QueryClient();
    const ks = [
      keys.projectsAll('orbit'),
      keys.project('orbit', 'p1'),
      keys.labels('orbit'),
      keys.cycles('orbit', 'ORB'),
      keys.teams('orbit'),
    ];
    for (const k of ks) qc.setQueryData(k, {});
    invalidateForEvent(qc, 'orbit', { type: 'team.deleted' });
    for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, k.join('/')).toBe(true);
  });

  it('EVENT_TYPES lists every event type the server can emit (a type missing here never reaches the page)', () => {
    // EventSource calls only listeners registered by event NAME, so a missing name means a silent, stale page.
    const src = readFileSync(resolve(process.cwd(), '../server/internal/events/events.go'), 'utf8');
    const server = [...src.matchAll(/=\s*"([a-z_]+\.[a-z_]+)"/g)].map((m) => m[1]!);
    expect(server.length).toBeGreaterThan(20);
    expect([...EVENT_TYPES].sort()).toEqual([...new Set(server)].sort());
  });

  it('project_update.created refreshes the project updates lists only (LAUN-196)', () => {
    const qc = new QueryClient();
    const k = keys.projectUpdates('orbit', 'p1');
    qc.setQueryData(k, []);
    const other = keys.labels('orbit');
    qc.setQueryData(other, {});
    invalidateForEvent(qc, 'orbit', { type: 'project_update.created' });
    expect(qc.getQueryState(k)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
  });

  it('cycle events refresh the cycle lists and the burn-up rows (LAUN-172)', () => {
    for (const type of ['cycle.created', 'cycle.updated', 'cycle.completed', 'cycle.deleted'] as const) {
      const qc = new QueryClient();
      const ks = [keys.cycles('orbit', 'ORB'), ['org', 'orbit', 'cycle-progress', 'c1'] as const];
      for (const k of ks) qc.setQueryData(k, {});
      const other = keys.labels('orbit');
      qc.setQueryData(other, {});
      invalidateForEvent(qc, 'orbit', { type });
      for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, `${type} ${k.join('/')}`).toBe(true);
      expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
    }
  });

  it('project_status events refresh the status list and every project list (LAUN-195)', () => {
    for (const type of [
      'project_status.created',
      'project_status.updated',
      'project_status.deleted',
      'project_status.reordered',
    ] as const) {
      const qc = new QueryClient();
      const ks = [keys.projectStatuses('orbit'), keys.projectsAll('orbit'), keys.projects('orbit', 'ORB')];
      for (const k of ks) qc.setQueryData(k, {});
      const other = keys.labels('orbit');
      qc.setQueryData(other, {});
      invalidateForEvent(qc, 'orbit', { type });
      for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, `${type} ${k.join('/')}`).toBe(true);
      expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
    }
  });

  it('every project_label event refreshes the label list and the projects that carry labels (LAUN-194)', () => {
    for (const type of ['project_label.created', 'project_label.updated', 'project_label.deleted'] as const) {
      const qc = new QueryClient();
      const ks = [keys.projectLabels('orbit'), keys.projectsAll('orbit'), keys.project('orbit', 'p1')];
      const other = keys.projectLabels('other');
      for (const k of [...ks, other]) qc.setQueryData(k, {});
      invalidateForEvent(qc, 'orbit', { type });
      for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, `${type} ${k.join('/')}`).toBe(true);
      expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
      expect(EVENT_TYPES).toContain(type);
    }
  });

  it('project.deleted refreshes the project lists and details and the issue lists (LAUN-191)', () => {
    const qc = new QueryClient();
    const ks = [
      keys.projectsAll('orbit'),
      keys.projects('orbit', 'ORB'),
      keys.project('orbit', 'p1'),
      keys.issues('orbit'),
      keys.issueDetails('orbit'),
      keys.milestones('orbit', 'p1'),
    ];
    const other = keys.issues('other');
    for (const k of [...ks, other]) qc.setQueryData(k, {});
    invalidateForEvent(qc, 'orbit', { type: 'project.deleted' });
    for (const k of ks) expect(qc.getQueryState(k)?.isInvalidated, k.join('/')).toBe(true);
    expect(qc.getQueryState(other)?.isInvalidated).toBe(false);
    expect(EVENT_TYPES).toContain('project.deleted');
  });

  it('refreshes an open issue when an attachment is added (LAUN-48)', () => {
    const qc = new QueryClient();
    qc.setQueryData(keys.issue('orbit', 'ORB-1'), { identifier: 'ORB-1' });
    invalidateForEvent(qc, 'orbit', { type: 'attachment.created' });
    expect(qc.getQueryState(keys.issue('orbit', 'ORB-1'))?.isInvalidated).toBe(true);
    expect(EVENT_TYPES).toContain('attachment.created');
  });

  it('team.updated and every product event refetch the teams list; product events also the product list (LAUN-64)', () => {
    for (const type of [
      'team.updated',
      'product.created',
      'product.updated',
      'product.deleted',
      'product.teams_changed',
    ] as const) {
      const qc = new QueryClient();
      qc.setQueryData(keys.teams('orbit'), []);
      qc.setQueryData(keys.products('orbit'), []);
      qc.setQueryData(keys.teams('other'), []);
      qc.setQueryData(keys.products('other'), []);
      qc.setQueryData(keys.labels('orbit'), []);
      invalidateForEvent(qc, 'orbit', { type });
      expect(qc.getQueryState(keys.teams('orbit'))?.isInvalidated, `${type} teams`).toBe(true);
      expect(qc.getQueryState(keys.products('orbit'))?.isInvalidated, `${type} products`).toBe(true);
      // Another org and an unrelated list stay as they are.
      expect(qc.getQueryState(keys.teams('other'))?.isInvalidated, type).toBe(false);
      expect(qc.getQueryState(keys.products('other'))?.isInvalidated, type).toBe(false);
      expect(qc.getQueryState(keys.labels('orbit'))?.isInvalidated, type).toBe(false);
      expect(EVENT_TYPES, type).toContain(type);
    }
  });

  it('team.updated also refreshes product team names and issue details (LAUN-99)', () => {
    const qc = new QueryClient();
    qc.setQueryData(keys.products('orbit'), []);
    qc.setQueryData(keys.issue('orbit', 'ORB-1'), {});
    qc.setQueryData(keys.products('other'), []);
    invalidateForEvent(qc, 'orbit', { type: 'team.updated' });
    expect(qc.getQueryState(keys.products('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.issue('orbit', 'ORB-1'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.products('other'))?.isInvalidated).toBe(false);
  });

  it('agent_guidance.updated refreshes the guidance of that org only (LAUN-200)', () => {
    expect(EVENT_TYPES).toContain('agent_guidance.updated');
    const qc = new QueryClient();
    qc.setQueryData(keys.agentGuidance('orbit'), {});
    qc.setQueryData(keys.agentGuidance('other'), {});
    invalidateForEvent(qc, 'orbit', { type: 'agent_guidance.updated' });
    expect(qc.getQueryState(keys.agentGuidance('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.agentGuidance('other'))?.isInvalidated).toBe(false);
  });

  it('chat_service.updated refreshes the chat services of that org only (LAUN-204)', () => {
    expect(EVENT_TYPES).toContain('chat_service.updated');
    const qc = new QueryClient();
    qc.setQueryData(keys.chat('orbit'), {});
    qc.setQueryData(keys.chat('other'), {});
    invalidateForEvent(qc, 'orbit', { type: 'chat_service.updated' });
    expect(qc.getQueryState(keys.chat('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.chat('other'))?.isInvalidated).toBe(false);
  });

  it('an old slack.updated row refreshes the chat services too', () => {
    const qc = new QueryClient();
    qc.setQueryData(keys.chat('orbit'), {});
    invalidateForEvent(qc, 'orbit', { type: 'slack.updated' });
    expect(qc.getQueryState(keys.chat('orbit'))?.isInvalidated).toBe(true);
  });

  it('push.updated refreshes the relay, the devices and the settings of that org only (LAUN-197)', () => {
    expect(EVENT_TYPES).toContain('push.updated');
    const qc = new QueryClient();
    for (const o of ['orbit', 'other']) {
      qc.setQueryData(keys.push(o), {});
      qc.setQueryData(keys.pushDevices(o), {});
      qc.setQueryData(keys.notificationSettings(o), {});
    }
    invalidateForEvent(qc, 'orbit', { type: 'push.updated' });
    expect(qc.getQueryState(keys.push('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.pushDevices('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.notificationSettings('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.push('other'))?.isInvalidated).toBe(false);
  });

  it('member.access_changed refreshes the member list of that org only (LAUN-225)', () => {
    expect(EVENT_TYPES).toContain('member.access_changed');
    const qc = new QueryClient();
    for (const o of ['orbit', 'other']) qc.setQueryData(keys.members(o), {});
    invalidateForEvent(qc, 'orbit', { type: 'member.access_changed' });
    expect(qc.getQueryState(keys.members('orbit'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.members('other'))?.isInvalidated).toBe(false);
  });

  it('org.updated and onboarding.step_changed refresh the checklist; org.updated also the org and the session (LAUN-98)', () => {
    for (const type of ['org.updated', 'onboarding.step_changed'] as const) {
      expect(EVENT_TYPES, type).toContain(type);
      const qc = new QueryClient();
      qc.setQueryData(keys.onboarding('orbit'), {});
      qc.setQueryData(keys.orgDetail('orbit'), {});
      qc.setQueryData(keys.session, {});
      qc.setQueryData(keys.onboarding('other'), {});
      qc.setQueryData(keys.teams('orbit'), []);
      invalidateForEvent(qc, 'orbit', { type });
      expect(qc.getQueryState(keys.onboarding('orbit'))?.isInvalidated, `${type} onboarding`).toBe(true);
      expect(qc.getQueryState(keys.orgDetail('orbit'))?.isInvalidated, `${type} org`).toBe(
        type === 'org.updated',
      );
      expect(qc.getQueryState(keys.session)?.isInvalidated, `${type} session`).toBe(type === 'org.updated');
      expect(qc.getQueryState(keys.onboarding('other'))?.isInvalidated, `${type} other org`).toBe(false);
      expect(qc.getQueryState(keys.teams('orbit'))?.isInvalidated, `${type} teams`).toBe(false);
    }
  });

  it('an onboarding event from the stream reaches the cache (the stream listens for the named kind)', () => {
    const { events, spy } = make();
    events.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit(
      'onboarding.step_changed',
      ev(11, 'onboarding.step_changed', { entityType: 'org' }),
    );
    vi.advanceTimersByTime(100);
    const called = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(called).toContain(JSON.stringify(keys.onboarding('orbit')));
    events.stop();
  });

  it('a product event from the stream reaches the cache (the stream listens for the named kind)', () => {
    const { events, spy } = make();
    events.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit(
      'product.teams_changed',
      ev(9, 'product.teams_changed', { entityType: 'product' }),
    );
    vi.advanceTimersByTime(100);
    const called = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(called).toContain(JSON.stringify(keys.products('orbit')));
    expect(called).toContain(JSON.stringify(keys.teams('orbit')));
    events.stop();
  });

  it('never touches another org', () => {
    const qc = new QueryClient();
    qc.setQueryData(keys.issueList('other', {}), { pages: [], pageParams: [] });
    invalidateForEvent(qc, 'orbit', { type: 'issue.updated' });
    expect(qc.getQueryState(keys.issueList('other', {}))?.isInvalidated).toBe(false);
  });

  it('shows reconnecting, backs off, and resumes after the last seq it saw', async () => {
    const { events, states } = make();
    events.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit('issue.updated', ev(41, 'issue.updated'));
    FakeEventSource.last!.fail();
    expect(states).toContain('reconnecting');
    expect(FakeEventSource.instances[0]!.closed).toBe(true);
    await vi.advanceTimersByTimeAsync(1100);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.last!.url).toContain('after=41');
    // fail again: the delay grows
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(1100);
    expect(FakeEventSource.instances).toHaveLength(2); // 2s delay not yet over
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(3);
    FakeEventSource.last!.open();
    expect(events.state).toBe('live');
    // The backoff resets once the stream stays open (or sends a message), not on open.
    await vi.advanceTimersByTimeAsync(10_000);
    expect(events.attempts).toBe(0);
    events.stop();
  });

  it('caps the backoff delay', () => {
    const { events } = make();
    events.attempts = 50;
    expect(events.nextDelay()).toBeLessThanOrEqual(30_000);
  });

  it('stops retrying when the credentials are gone', async () => {
    const onUnauthorized = vi.fn();
    const { events } = make({ probe: async () => false, onUnauthorized });
    events.start();
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(1100);
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(events.state).toBe('closed');
  });

  it('ignores a malformed frame', () => {
    const { events, spy } = make();
    events.start();
    FakeEventSource.last!.emitRaw('issue.updated', '{not json');
    vi.advanceTimersByTime(200);
    expect(spy).not.toHaveBeenCalled();
    events.stop();
  });

  it('stop closes the stream and cancels pending reconnects', async () => {
    const { events } = make();
    events.start();
    FakeEventSource.last!.fail();
    events.stop();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});

describe('OrgEvents (polling fallback for bearer tokens)', () => {
  it('learns the newest seq first, then applies only new events', async () => {
    let call = 0;
    const api = mockApi({
      'GET /api/v1/orgs/:org/events': (r) => {
        call++;
        const after = Number(r.url.searchParams.get('after'));
        if (after === 0)
          return { json: { items: [ev(1, 'issue.created'), ev(2, 'issue.updated')], nextCursor: null } };
        if (after === 2) return { json: { items: [ev(3, 'issue.updated')], nextCursor: null } };
        return { json: { items: [], nextCursor: null } };
      },
    });
    const { events, spy } = make({ mode: 'poll', pollMs: 1000 });
    events.start();
    await vi.advanceTimersByTimeAsync(10);
    expect(events.lastSeq).toBe(2);
    await vi.advanceTimersByTimeAsync(200);
    expect(spy).not.toHaveBeenCalled(); // history is not replayed
    await vi.advanceTimersByTimeAsync(1100);
    await vi.advanceTimersByTimeAsync(200);
    expect(events.lastSeq).toBe(3);
    expect(spy).toHaveBeenCalled();
    expect(api.calls.every((c) => c.url.pathname.endsWith('/events'))).toBe(true);
    expect(call).toBeGreaterThanOrEqual(2);
    events.stop();
  });
});

describe('MR state events (LAUN-95)', () => {
  it('issue.updated refreshes the recorded activity of an open issue', () => {
    const qc = new QueryClient();
    qc.setQueryData(keys.issueActivity('orbit', 'ORB-1'), []);
    qc.setQueryData(keys.issue('orbit', 'ORB-1'), { identifier: 'ORB-1' });
    invalidateForEvent(qc, 'orbit', { type: 'issue.updated' });
    expect(qc.getQueryState(keys.issueActivity('orbit', 'ORB-1'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.issue('orbit', 'ORB-1'))?.isInvalidated).toBe(true);
  });
});

/**
 * A query client with queries on screen whose refetch takes `delay` ms (an instant mock would hide a reset: the data
 * would come back before anyone looked). `seen` records every data value each query held.
 */
function world(teamKeys: string[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = { teams: teamKeys, failTeams: false };
  const seen = new Map<string, unknown[]>();
  qc.getQueryCache().subscribe((e) => {
    if (e.type !== 'updated') return;
    const k = JSON.stringify(e.query.queryKey);
    if (!seen.has(k)) seen.set(k, []);
    seen.get(k)!.push(e.query.state.data);
  });
  const fetched: string[] = [];
  const offs: (() => void)[] = [];
  const watch = (queryKey: readonly unknown[], data: () => unknown, delay = 50) =>
    offs.push(
      new QueryObserver(qc, {
        queryKey,
        queryFn: () => {
          fetched.push(JSON.stringify(queryKey));
          return new Promise((resolve, reject) =>
            setTimeout(() => {
              if (server.failTeams && JSON.stringify(queryKey) === JSON.stringify(keys.teams('orbit')))
                reject(new Error('down'));
              else resolve(data());
            }, delay),
          );
        },
        staleTime: Infinity,
      }).subscribe(() => {}),
    );
  watch(keys.teams('orbit'), () => server.teams.map((key) => ({ key })));
  const everEmpty = (k: readonly unknown[]) =>
    (seen.get(JSON.stringify(k)) ?? []).some((d) => d === undefined);
  return { qc, server, seen, fetched, watch, everEmpty, off: () => offs.forEach((o) => o()) };
}

describe('refreshForAccess (decisions-T20 #56, #76)', () => {
  it('no team lost: everything on screen refetches in place, nothing goes empty, the inbox is left alone', async () => {
    const w = world(['ORB']);
    w.watch(keys.issue('orbit', 'ORB-1'), () => ({ identifier: 'ORB-1' }));
    w.watch(keys.issues('orbit'), () => []);
    w.watch(keys.unread('orbit'), () => ({ count: 0 }));
    await vi.advanceTimersByTimeAsync(100);
    w.seen.clear();
    w.fetched.length = 0;
    const spy = vi.spyOn(w.qc, 'invalidateQueries');
    const done = refreshForAccess(w.qc, 'orbit');
    await vi.advanceTimersByTimeAsync(200);
    await done;
    expect(w.everEmpty(keys.issue('orbit', 'ORB-1'))).toBe(false);
    expect(w.everEmpty(keys.issues('orbit'))).toBe(false);
    expect(w.fetched).toEqual(
      expect.arrayContaining(
        [keys.teams('orbit'), keys.issue('orbit', 'ORB-1'), keys.issues('orbit')].map((k) =>
          JSON.stringify(k),
        ),
      ),
    );
    expect(w.fetched).not.toContain(JSON.stringify(keys.unread('orbit')));
    const called = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(called).toEqual(
      expect.arrayContaining([keys.session, keys.orgList].map((k) => JSON.stringify(k))),
    );
    w.off();
  });

  it('a team lost: its data and the mixed lists go at once (never shown during the refetch); the rest stays', async () => {
    const w = world(['ORB', 'HID']);
    w.watch(keys.issue('orbit', 'ORB-1'), () => ({ identifier: 'ORB-1' }));
    w.watch(keys.issue('orbit', 'HID-1'), () => ({ identifier: 'HID-1' }));
    w.watch(keys.issueActivity('orbit', 'HID-1'), () => []);
    w.watch(keys.issues('orbit'), () => []);
    w.watch(keys.cycles('orbit', 'HID'), () => []);
    await vi.advanceTimersByTimeAsync(100);
    w.qc.setQueryData(keys.issue('orbit', 'HID-2'), { identifier: 'HID-2' }); // not on screen
    w.seen.clear();
    w.server.teams = ['ORB'];
    const done = refreshForAccess(w.qc, 'orbit');
    await vi.advanceTimersByTimeAsync(60); // the team list is back; the other refetches still run
    for (const k of [
      keys.issue('orbit', 'HID-1'),
      keys.issueActivity('orbit', 'HID-1'),
      keys.issues('orbit'),
      keys.cycles('orbit', 'HID'),
    ])
      expect(w.qc.getQueryData(k), JSON.stringify(k)).toBeUndefined();
    expect(w.qc.getQueryState(keys.issue('orbit', 'HID-2'))).toBeUndefined();
    expect(w.qc.getQueryData(keys.issue('orbit', 'ORB-1'))).toEqual({ identifier: 'ORB-1' });
    await vi.advanceTimersByTimeAsync(200);
    await done;
    expect(w.everEmpty(keys.issue('orbit', 'ORB-1'))).toBe(false);
    w.off();
  });

  it.each([
    [
      'the team list was never read',
      (w: ReturnType<typeof world>) => w.qc.removeQueries({ queryKey: keys.teams('orbit') }),
    ],
    ['the team list cannot be read', (w: ReturnType<typeof world>) => (w.server.failTeams = true)],
  ])('%s: everything is reset (the safe side)', async (_name, setup) => {
    const w = world(['ORB']);
    w.watch(keys.issue('orbit', 'ORB-1'), () => ({ identifier: 'ORB-1' }));
    await vi.advanceTimersByTimeAsync(100);
    setup(w);
    const done = refreshForAccess(w.qc, 'orbit');
    await vi.advanceTimersByTimeAsync(w.server.failTeams ? 60 : 1); // before the issue's own refetch (50 ms) ends
    expect(w.qc.getQueryData(keys.issue('orbit', 'ORB-1'))).toBeUndefined();
    await vi.advanceTimersByTimeAsync(200);
    await done;
    w.off();
  });

  it('onlyIfChanged (a reconnect): the same teams read nothing more', async () => {
    const w = world(['ORB']);
    w.watch(keys.issue('orbit', 'ORB-1'), () => ({ identifier: 'ORB-1' }));
    await vi.advanceTimersByTimeAsync(100);
    w.fetched.length = 0;
    const done = refreshForAccess(w.qc, 'orbit', { onlyIfChanged: true });
    await vi.advanceTimersByTimeAsync(200);
    await done;
    expect(w.fetched).toEqual([JSON.stringify(keys.teams('orbit'))]);
    w.off();
  });

  it('onlyIfChanged with a team gained refreshes the rest in place', async () => {
    const w = world(['ORB']);
    w.watch(keys.issues('orbit'), () => []);
    await vi.advanceTimersByTimeAsync(100);
    w.fetched.length = 0;
    w.seen.clear();
    w.server.teams = ['ORB', 'NEW'];
    const done = refreshForAccess(w.qc, 'orbit', { onlyIfChanged: true });
    await vi.advanceTimersByTimeAsync(200);
    await done;
    expect(w.fetched).toContain(JSON.stringify(keys.issues('orbit')));
    expect(w.everEmpty(keys.issues('orbit'))).toBe(false);
    w.off();
  });
});

/** How often the team list was read again (the first step of every access refresh). */
const teamChecks = (refetch: { mock: { calls: unknown[][] } }) =>
  refetch.mock.calls.filter(
    (c) =>
      JSON.stringify((c[0] as { queryKey?: unknown } | undefined)?.queryKey) ===
      JSON.stringify(keys.teams('orbit')),
  ).length;

describe('the access event on the stream', () => {
  it('refreshes once for a burst, keeps its cursor and its connection', async () => {
    const { events, qc } = make();
    const refetch = vi.spyOn(qc, 'refetchQueries');
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }]);
    events.start();
    const src = FakeEventSource.last!;
    src.open();
    src.emit('issue.updated', ev(7, 'issue.updated'));
    for (let i = 0; i < 5; i++) src.emit('access', {});
    await vi.advanceTimersByTimeAsync(100);
    expect(teamChecks(refetch)).toBe(1);
    expect(events.lastSeq).toBe(7); // no id, not a feed row
    expect(FakeEventSource.instances).toHaveLength(1); // no reconnect, so no loop
    expect(src.closed).toBe(false);
    expect(events.state).toBe('live');
    src.emit('access', {});
    await vi.advanceTimersByTimeAsync(100);
    expect(teamChecks(refetch)).toBe(2);
    events.stop();
  });

  it('a feed event after an access event is handled as usual', async () => {
    const { events, spy } = make();
    events.start();
    FakeEventSource.last!.emit('access', {});
    await vi.advanceTimersByTimeAsync(100);
    spy.mockClear();
    FakeEventSource.last!.emit('label.created', ev(8, 'label.created'));
    vi.advanceTimersByTime(100);
    const called = spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown }).queryKey));
    expect(called).toEqual([JSON.stringify(keys.labels('orbit'))]);
    expect(events.lastSeq).toBe(8);
    events.stop();
  });
});

describe('a reconnect after an error (decisions-T20 #56)', () => {
  it('checks the team list when the new stream opens (a change in the gap sends no access), not on the first open', async () => {
    const { events, qc } = make();
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }]);
    const refetch = vi.spyOn(qc, 'refetchQueries');
    events.start();
    FakeEventSource.last!.open();
    await vi.advanceTimersByTimeAsync(100);
    expect(teamChecks(refetch)).toBe(0);
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(2100);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(teamChecks(refetch)).toBe(0); // not before the new stream is open
    FakeEventSource.last!.open();
    await vi.advanceTimersByTimeAsync(10);
    expect(teamChecks(refetch)).toBe(1);
    FakeEventSource.last!.open(); // a second open of the same connection does nothing more
    await vi.advanceTimersByTimeAsync(10);
    expect(teamChecks(refetch)).toBe(1);
    events.stop();
  });

  it('a flapping stream (open, fail at once, again) keeps backing off', async () => {
    const { events } = make();
    events.start();
    FakeEventSource.last!.open();
    const delays: number[] = [];
    let t = Date.now();
    for (let i = 0; i < 6; i++) {
      FakeEventSource.last!.fail();
      const n = FakeEventSource.instances.length;
      while (FakeEventSource.instances.length === n) await vi.advanceTimersByTimeAsync(100);
      delays.push(Date.now() - t);
      FakeEventSource.last!.open(); // opens, and fails at the top of the loop
      t = Date.now();
    }
    // An open is no proof the stream works: the delay keeps growing.
    expect(delays[5]!).toBeGreaterThan(delays[0]! * 4);
    events.stop();
  });

  it('a fast flapping stream (backoff capped at 1 s) reads the team list at most once per 30 s window', async () => {
    const { events, qc } = make({ maxDelayMs: 1000 });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }]);
    const refetch = vi.spyOn(qc, 'refetchQueries');
    events.start();
    FakeEventSource.last!.open();
    for (let i = 0; i < 20; i++) {
      FakeEventSource.last!.fail();
      const n = FakeEventSource.instances.length;
      while (FakeEventSource.instances.length === n) await vi.advanceTimersByTimeAsync(100);
      FakeEventSource.last!.open();
    }
    expect(FakeEventSource.instances.length).toBe(21); // 20 reconnects in about 20 s
    // One check at once, one at the end of the window for all the others.
    expect(teamChecks(refetch)).toBe(1);
    await vi.advanceTimersByTimeAsync(31_000);
    expect(teamChecks(refetch)).toBe(2);
    events.stop();
  });

  it('the backoff resets once the stream has stayed open, or on a message', async () => {
    const { events } = make();
    events.start();
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(1100);
    FakeEventSource.last!.open();
    expect(events.attempts).toBe(1);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(events.attempts).toBe(0);
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(1100);
    FakeEventSource.last!.open();
    expect(events.attempts).toBe(1);
    FakeEventSource.last!.emit('label.created', ev(3, 'label.created'));
    expect(events.attempts).toBe(0);
    events.stop();
  });
});

describe('the team list watcher (final review: a loss found by any read of the list)', () => {
  it('a focus-style refetch of the team list drops the lost team at once; a later access refresh drops nothing twice', async () => {
    const w = world(['ORB', 'HID']);
    const stop = watchTeamList(w.qc, 'orbit');
    w.watch(keys.issue('orbit', 'ORB-1'), () => ({ identifier: 'ORB-1' }));
    w.watch(keys.issue('orbit', 'HID-1'), () => ({ identifier: 'HID-1' }));
    w.watch(keys.project('orbit', 'p1'), () => ({ id: 'p1' }));
    w.watch(keys.projectUpdates('orbit', 'p1'), () => []);
    await vi.advanceTimersByTimeAsync(100);
    w.qc.setQueryData(keys.issue('orbit', 'HID-2'), { identifier: 'HID-2' }); // not on screen (Back, the palette)
    w.qc.setQueryData(keys.comments('orbit', 'HID-2'), []);
    w.qc.setQueryData(keys.issue('other', 'HID-2'), { identifier: 'HID-2' }); // another org: never touched
    w.seen.clear();
    w.server.teams = ['ORB'];
    // Not this module: a window focus refetch, a team event, a mount.
    void w.qc.refetchQueries({ queryKey: keys.teams('orbit') });
    await vi.advanceTimersByTimeAsync(55);
    expect(w.qc.getQueryState(keys.issue('orbit', 'HID-2'))).toBeUndefined();
    expect(w.qc.getQueryState(keys.comments('orbit', 'HID-2'))).toBeUndefined();
    expect(w.qc.getQueryData(keys.issue('other', 'HID-2'))).toEqual({ identifier: 'HID-2' });
    for (const k of [
      keys.issue('orbit', 'HID-1'),
      keys.project('orbit', 'p1'),
      keys.projectUpdates('orbit', 'p1'),
    ])
      expect(w.qc.getQueryData(k), JSON.stringify(k)).toBeUndefined();
    expect(w.qc.getQueryData(keys.issue('orbit', 'ORB-1'))).toEqual({ identifier: 'ORB-1' });
    await vi.advanceTimersByTimeAsync(200);
    // The stream reconnects later: the check finds the loss handled, so nothing is emptied again.
    const reset = vi.spyOn(w.qc, 'resetQueries');
    w.seen.clear();
    const done = refreshForAccess(w.qc, 'orbit', { onlyIfChanged: true });
    await vi.advanceTimersByTimeAsync(200);
    await done;
    expect(reset).not.toHaveBeenCalled();
    expect(w.everEmpty(keys.issue('orbit', 'ORB-1'))).toBe(false);
    stop();
    w.off();
  });

  it('the first list only records; a loss and a gain together drop only the lost team', async () => {
    const qc = new QueryClient();
    const stop = watchTeamList(qc, 'orbit');
    qc.setQueryData(keys.issue('orbit', 'HID-1'), { identifier: 'HID-1' });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }, { key: 'HID' }]); // the first list
    await Promise.resolve();
    expect(qc.getQueryData(keys.issue('orbit', 'HID-1'))).toEqual({ identifier: 'HID-1' });
    qc.setQueryData(keys.issue('orbit', 'ORB-1'), { identifier: 'ORB-1' });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }, { key: 'NEW' }]);
    await Promise.resolve();
    expect(qc.getQueryState(keys.issue('orbit', 'HID-1'))).toBeUndefined();
    expect(qc.getQueryData(keys.issue('orbit', 'ORB-1'))).toEqual({ identifier: 'ORB-1' });
    stop();
  });

  it('one watcher per client and org; it stops with the last user', async () => {
    const qc = new QueryClient();
    const subscribe = vi.spyOn(qc.getQueryCache(), 'subscribe');
    const a = watchTeamList(qc, 'orbit');
    const b = watchTeamList(qc, 'orbit');
    expect(subscribe).toHaveBeenCalledTimes(1);
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }, { key: 'HID' }]);
    await Promise.resolve();
    a();
    a(); // twice is once
    qc.setQueryData(keys.issue('orbit', 'HID-1'), { identifier: 'HID-1' });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }]);
    await Promise.resolve();
    expect(qc.getQueryState(keys.issue('orbit', 'HID-1'))).toBeUndefined(); // b still watches
    b();
    qc.setQueryData(keys.issue('orbit', 'ORB-2'), { identifier: 'ORB-2' });
    qc.setQueryData(keys.teams('orbit'), []);
    await Promise.resolve();
    expect(qc.getQueryData(keys.issue('orbit', 'ORB-2'))).toEqual({ identifier: 'ORB-2' }); // nobody watches
  });

  it('the stream watches from start to stop', async () => {
    const { events, qc } = make({ mode: 'poll' });
    mockApi({ 'GET /api/v1/orgs/:org/events': () => ({ json: { items: [], nextCursor: null } }) });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }, { key: 'HID' }]);
    events.start(); // poll mode (a bearer token) is covered too
    qc.setQueryData(keys.issue('orbit', 'HID-1'), { identifier: 'HID-1' });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }]);
    await Promise.resolve();
    expect(qc.getQueryState(keys.issue('orbit', 'HID-1'))).toBeUndefined();
    events.stop();
    qc.setQueryData(keys.issue('orbit', 'ORB-1'), { identifier: 'ORB-1' });
    qc.setQueryData(keys.teams('orbit'), []);
    await Promise.resolve();
    expect(qc.getQueryData(keys.issue('orbit', 'ORB-1'))).toEqual({ identifier: 'ORB-1' });
  });

  it('a snapshot taken before a drop is not put back after it', async () => {
    const qc = new QueryClient();
    const stop = watchTeamList(qc, 'orbit');
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }, { key: 'HID' }]);
    await Promise.resolve();
    const list = keys.issueList('orbit', {});
    qc.setQueryData(list, { pages: [{ items: [{ identifier: 'HID-1' }] }], pageParams: [] });
    const gen = accessGeneration(qc, 'orbit');
    const snap = qc.getQueriesData({ queryKey: keys.issues('orbit') });
    qc.setQueryData(keys.teams('orbit'), [{ key: 'ORB' }]); // HID lost while the write runs
    await Promise.resolve();
    expect(restoreSnapshot(qc, 'orbit', gen, snap)).toBe(false);
    expect(qc.getQueryData(list)).toBeUndefined();
    // With no drop in between, a snapshot is put back as before.
    const gen2 = accessGeneration(qc, 'orbit');
    expect(restoreSnapshot(qc, 'orbit', gen2, [[list, { pages: [], pageParams: [] }]])).toBe(true);
    stop();
  });
});
