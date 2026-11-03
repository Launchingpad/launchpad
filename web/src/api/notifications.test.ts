import { QueryClient } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeEventSource } from '../test/fakes';
import { mockApi } from '../test/mockApi';
import { keys } from './keys';
import { counts, inFilter, InboxStream, type InboxState } from './notifications';

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

function make(extra: Partial<ConstructorParameters<typeof InboxStream>[0]> = {}) {
  const qc = new QueryClient();
  qc.setQueryData(keys.unread('orbit'), { count: 2 });
  qc.setQueryData(keys.inbox('orbit', 'unread'), { pages: [], pageParams: [] });
  const states: InboxState[] = [];
  const s = new InboxStream({
    org: 'orbit',
    qc,
    factory: (url, init) => new FakeEventSource(url, init),
    onState: (x) => states.push(x),
    random: () => 1,
    ...extra,
  });
  return { qc, s, states };
}
const stale = (qc: QueryClient) => qc.getQueryState(keys.inbox('orbit', 'unread'))?.isInvalidated;

describe('InboxStream (SSE)', () => {
  it('opens one cookie-credentialed stream for the inbox', () => {
    const { s } = make();
    s.start();
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.last!.url).toContain('/api/v1/orgs/orbit/notifications/stream');
    expect(FakeEventSource.last!.init).toEqual({ withCredentials: true });
    s.stop();
  });

  it('a count event sets the badge; a changed count refreshes the lists, the same count does not', () => {
    const { s, qc } = make();
    s.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit('count', { count: 2 }); // the first event after connect
    expect(qc.getQueryData(keys.unread('orbit'))).toEqual({ count: 2 });
    expect(stale(qc)).toBe(false);
    FakeEventSource.last!.emit('count', { count: 5 });
    expect(qc.getQueryData(keys.unread('orbit'))).toEqual({ count: 5 });
    expect(stale(qc)).toBe(true); // a read in another tab moves the count, not a row
    s.stop();
  });

  it('a notification event refreshes the lists once for a burst, and remembers its seq', () => {
    const { s, qc } = make();
    const spy = vi.spyOn(qc, 'invalidateQueries');
    s.start();
    for (let i = 1; i <= 5; i++)
      FakeEventSource.last!.emit('notification', { id: `n${i}`, kind: 'comment', inboxSeq: 10 + i });
    expect(spy).not.toHaveBeenCalled(); // batched
    vi.advanceTimersByTime(100);
    expect(spy).toHaveBeenCalledTimes(2); // lists and count, once
    expect(stale(qc)).toBe(true);
    expect(s.lastSeq).toBe(15);
    s.stop();
  });

  it('ignores a malformed frame', () => {
    const { s, qc } = make();
    s.start();
    FakeEventSource.last!.emitRaw('count', '{not json');
    FakeEventSource.last!.emitRaw('notification', 'nope');
    vi.advanceTimersByTime(200);
    expect(qc.getQueryData(keys.unread('orbit'))).toEqual({ count: 2 });
    expect(stale(qc)).toBe(false);
    s.stop();
  });

  it('reconnects with ?after= and refreshes the lists once it is back', async () => {
    const { s, qc, states } = make();
    s.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit('notification', { id: 'n', kind: 'mention', inboxSeq: 41 });
    vi.advanceTimersByTime(100);
    qc.setQueryData(keys.inbox('orbit', 'unread'), { pages: [], pageParams: [] }); // clean again
    FakeEventSource.last!.fail();
    expect(states).toContain('reconnecting');
    await vi.advanceTimersByTimeAsync(1100);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.last!.url).toContain('after=41');
    expect(stale(qc)).toBe(false);
    FakeEventSource.last!.open();
    expect(s.state).toBe('live');
    expect(stale(qc)).toBe(true);
    s.stop();
  });

  it('resets the backoff on the first count event, not on open', async () => {
    const { s } = make();
    s.start();
    FakeEventSource.last!.open();
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(1100);
    FakeEventSource.last!.open(); // opens, then drops again before any event
    expect(s.attempts).toBe(1);
    FakeEventSource.last!.fail();
    expect(s.attempts).toBe(2);
    await vi.advanceTimersByTimeAsync(2100);
    FakeEventSource.last!.open();
    FakeEventSource.last!.emit('count', { count: 2 });
    expect(s.attempts).toBe(0);
    s.stop();
  });

  it('stops when the credentials are gone', async () => {
    const onUnauthorized = vi.fn();
    const { s } = make({ probe: async () => false, onUnauthorized });
    s.start();
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(1100);
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(s.state).toBe('closed');
  });
});

describe('InboxStream access event (decisions-T20 #56, #62)', () => {
  it('refetches the lists and the count once per burst, keeps the cursor and the connection', () => {
    const { s, qc } = make();
    s.start();
    const src = FakeEventSource.last!;
    src.emit('notification', { id: 'n1', kind: 'comment', inboxSeq: 12 });
    vi.advanceTimersByTime(100);
    qc.setQueryData(keys.inbox('orbit', 'unread'), { pages: [], pageParams: [] });
    qc.setQueryData(keys.unread('orbit'), { count: 2 });
    const reset = vi.spyOn(qc, 'resetQueries');
    const inv = vi.spyOn(qc, 'invalidateQueries');
    for (let i = 0; i < 4; i++) src.emit('access', {});
    vi.advanceTimersByTime(100);
    // One burst, one read of each. The older rows of a team that is now visible are below the cursor: only a
    // refetch shows them; the rows of a team that is now hidden go at once.
    expect(reset).toHaveBeenCalledTimes(1);
    expect(reset).toHaveBeenCalledWith({ queryKey: keys.inboxAll('orbit') });
    expect(qc.getQueryData(keys.inbox('orbit', 'unread'))).toBeUndefined();
    expect(inv).toHaveBeenCalledTimes(1);
    expect(qc.getQueryState(keys.unread('orbit'))?.isInvalidated).toBe(true);
    expect(s.lastSeq).toBe(12);
    expect(FakeEventSource.instances).toHaveLength(1); // no reconnect, so no loop
    expect(src.closed).toBe(false);
    expect(s.state).toBe('live');
    s.stop();
  });
});

describe('unread count polling fallback (60 s)', () => {
  const countRoute = (n: () => number) => ({
    'GET /api/v1/orgs/:org/notifications/unread-count': () => ({ json: { count: n() } }),
  });

  it('polls every 60 seconds with no stream at all (a bearer token)', async () => {
    const api = mockApi(countRoute(() => 9));
    const { s, qc } = make({ mode: 'poll' });
    s.start();
    expect(FakeEventSource.instances).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(59_000);
    expect(api.calls).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1_500);
    expect(api.calls).toHaveLength(1);
    expect(qc.getQueryData(keys.unread('orbit'))).toEqual({ count: 9 });
    expect(stale(qc)).toBe(true); // the count moved, so the lists are old
    await vi.advanceTimersByTimeAsync(60_000);
    expect(api.calls).toHaveLength(2);
    s.stop();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(api.calls).toHaveLength(2);
  });

  it('polls while the stream is down, and not while it is live', async () => {
    const api = mockApi(countRoute(() => 3));
    const { s } = make();
    s.start();
    FakeEventSource.last!.open();
    await vi.advanceTimersByTimeAsync(130_000);
    expect(api.calls).toHaveLength(0);
    FakeEventSource.last!.fail();
    await vi.advanceTimersByTimeAsync(60_500);
    expect(api.calls.length).toBeGreaterThanOrEqual(1);
    s.stop();
  });

  it('a failed poll is not fatal', async () => {
    mockApi({ 'GET /api/v1/orgs/:org/notifications/unread-count': () => ({ status: 500, json: {} }) });
    const { s, qc } = make({ mode: 'poll' });
    s.start();
    await vi.advanceTimersByTimeAsync(61_000);
    expect(qc.getQueryData(keys.unread('orbit'))).toEqual({ count: 2 });
    s.stop();
  });
});

it('knows which list a row belongs to', () => {
  const unread = { readAt: null, archivedAt: null };
  const read = { readAt: 'x', archivedAt: null };
  const archived = { readAt: null, archivedAt: 'x' };
  expect([inFilter('unread', unread), inFilter('unread', read), inFilter('unread', archived)]).toEqual([
    true,
    false,
    false,
  ]);
  expect([inFilter('all', unread), inFilter('all', read), inFilter('all', archived)]).toEqual([
    true,
    true,
    false,
  ]);
  expect([inFilter('archived', unread), inFilter('archived', archived)]).toEqual([false, true]);
  expect([counts(unread), counts(read), counts(archived)]).toEqual([true, false, false]);
});
