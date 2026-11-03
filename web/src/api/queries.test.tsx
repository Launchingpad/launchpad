import { waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { baseRoutes, makeIssue, mockApi } from '../test/mockApi';
import { renderAt } from '../test/render';
import type { Issue } from './types';
import { fetchSession } from './auth';
import { client, unwrap } from './client';
import { errorMessage } from './errors';
import { SessionChangedError, setMemoryCsrf, setWritesBlocked } from './http';
import { isNoopPatch, resetOwnWrites, useUpdateIssue, waitBounded } from './queries';

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

let who = 'u1';

type Reply = { status?: number; json?: unknown };

/** A server that enforces the version, with gates so a test can hold a PATCH until it says so. */
function setup(initial: { version: number }, opts: { jumpOnFirst?: number } = {}) {
  const issue: Issue = makeIssue(1, { version: initial.version });
  const gates: (() => void)[] = [];
  let n = 0;
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: who, username: who, name: who, email: `${who}@example.com` },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
        csrfToken: `csrf-${who}`,
      },
    }),
    'PATCH /api/v1/orgs/:org/issues/:issue': async (r): Promise<Reply> => {
      const idx = n++;
      const body = r.body as { version: number; [k: string]: unknown };
      if (hold.has(idx)) await new Promise<void>((res) => (gates[idx] = res));
      if (body.version !== issue.version)
        return { status: 409, json: { error: 'version conflict', code: 'version_conflict' } };
      if (opts.jumpOnFirst && idx === 0)
        issue.version = opts.jumpOnFirst; // a foreign write got in first
      else issue.version += 1;
      return { json: { ...issue } };
    },
  });
  const hold = new Set<number>();
  return { issue, api, hold, release: (i: number) => gates[i]?.() };
}

let update!: ReturnType<typeof useUpdateIssue>;
function Harness() {
  update = useUpdateIssue();
  return null;
}
const mount = () => renderAt(<Harness />, { path: '/orbit/issues' });
const patches = (api: { calls: { method: string; body: unknown }[] }) =>
  api.calls.filter((c) => c.method === 'PATCH').map((c) => (c.body as { version: number }).version);

describe('own writes and the version a save sends', () => {
  it('a draft save waits for an own PATCH that is still in flight, then sends the version it produced', async () => {
    const s = setup({ version: 3 });
    s.hold.add(0);
    mount();
    const quick = update.mutateAsync({ issue: s.issue, patch: { state: 'In Progress' } });
    await waitFor(() => expect(patches(s.api)).toEqual([3])); // the status PATCH is sent and held
    const draft = update.mutateAsync({ issue: s.issue, patch: { description: 'd' }, baseVersion: 3 });
    await new Promise((r) => setTimeout(r, 30));
    expect(patches(s.api)).toEqual([3]); // the draft has NOT gone out yet
    s.release(0);
    await quick;
    await draft;
    expect(patches(s.api)).toEqual([3, 4]); // no false 409
  });

  it('a title save started while another own PATCH is pending waits for it too', async () => {
    const s = setup({ version: 3 });
    s.hold.add(0);
    mount();
    const first = update.mutateAsync({ issue: s.issue, patch: { description: 'x' }, baseVersion: 3 });
    await waitFor(() => expect(patches(s.api)).toEqual([3]));
    const title = update.mutateAsync({ issue: s.issue, patch: { title: 't' }, baseVersion: 3 });
    await new Promise((r) => setTimeout(r, 30));
    s.release(0);
    await first;
    await title;
    expect(patches(s.api)).toEqual([3, 4]);
  });

  it('a one-click edit also waits for the in-flight own write and follows it', async () => {
    const s = setup({ version: 3 });
    s.hold.add(0);
    mount();
    const title = update.mutateAsync({ issue: s.issue, patch: { title: 't' }, baseVersion: 3 });
    await waitFor(() => expect(patches(s.api)).toEqual([3]));
    const status = update.mutateAsync({ issue: { ...s.issue, version: 3 }, patch: { state: 'Todo' } });
    await new Promise((r) => setTimeout(r, 30));
    s.release(0);
    await title;
    await status;
    expect(patches(s.api)).toEqual([3, 4]);
  });

  it('a reply that jumps by more than one is a foreign write: nothing is recorded, so the draft gets 409', async () => {
    const s = setup({ version: 3 }, { jumpOnFirst: 5 });
    mount();
    await update.mutateAsync({ issue: s.issue, patch: { state: 'Todo' } }); // sent at 3, replies 5
    await expect(
      update.mutateAsync({ issue: s.issue, patch: { description: 'd' }, baseVersion: 3 }),
    ).rejects.toMatchObject({ status: 409 });
    expect(patches(s.api)).toEqual([3, 3]);
  });

  it('forgetting own writes (logout, user change) makes an old draft base stand as it is', async () => {
    const s = setup({ version: 3 });
    const { client } = mount();
    await update.mutateAsync({ issue: s.issue, patch: { state: 'Todo' } }); // 3 -> 4
    resetOwnWrites(client);
    await expect(
      update.mutateAsync({ issue: s.issue, patch: { description: 'd' }, baseVersion: 3 }),
    ).rejects.toMatchObject({ status: 409 });
  });

  it('three writes to one issue go out one at a time in the order they were made: 3, 4, 5, no false 409', async () => {
    const s = setup({ version: 3 });
    s.hold.add(0);
    mount();
    const a = update.mutateAsync({ issue: s.issue, patch: { state: 'Todo' } });
    await waitFor(() => expect(patches(s.api)).toEqual([3]));
    const b = update.mutateAsync({ issue: s.issue, patch: { description: 'b' }, baseVersion: 3 });
    const c = update.mutateAsync({ issue: s.issue, patch: { priority: 2 } });
    await new Promise((r) => setTimeout(r, 30));
    expect(patches(s.api)).toEqual([3]); // B and C are queued behind A
    s.release(0);
    await Promise.all([a, b, c]);
    expect(patches(s.api)).toEqual([3, 4, 5]);
  });

  it('a write queued behind another is not sent as a different user', async () => {
    who = 'u1';
    const s = setup({ version: 3 });
    await fetchSession(); // this tab is u1
    s.hold.add(0);
    mount();
    const a = update.mutateAsync({ issue: s.issue, patch: { state: 'Todo' } });
    await waitFor(() => expect(patches(s.api)).toEqual([3]));
    const draft = update.mutateAsync({ issue: s.issue, patch: { description: 'd' }, baseVersion: 3 });
    const settled = draft.then(
      () => 'sent',
      (e: unknown) => e,
    );
    who = 'u2';
    await fetchSession(); // the session is now u2
    s.release(0);
    await a;
    expect(await settled).toBeInstanceOf(SessionChangedError);
    expect(patches(s.api)).toEqual([3]); // the draft was never sent
  });

  it('a patch that changes nothing is not sent, and records no step', async () => {
    const s = setup({ version: 3 });
    mount();
    const same = { ...s.issue };
    const out = await update.mutateAsync({ issue: s.issue, patch: { state: s.issue.state.name } });
    expect(out.version).toBe(3);
    expect(patches(s.api)).toEqual([]);
    expect(isNoopPatch({ priority: s.issue.priority }, same)).toBe(true);
    expect(isNoopPatch({ assignee: null }, { ...same, assignee: null })).toBe(true);
    expect(isNoopPatch({ removeLabels: ['nope'] }, same)).toBe(true);
    expect(isNoopPatch({ addLabels: ['nope'] }, same)).toBe(false);
    expect(isNoopPatch({ title: 'x' }, same)).toBe(false);
    // The next real write still sends the cached version.
    await update.mutateAsync({ issue: s.issue, patch: { state: 'Todo' } });
    expect(patches(s.api)).toEqual([3]);
  });

  it('the wait is bounded: a stalled write does not hold the next one for ever', async () => {
    const t = Date.now();
    expect(await waitBounded(new Promise(() => {}), 40)).toBe(false);
    expect(Date.now() - t).toBeLessThan(2000);
    expect(await waitBounded(Promise.resolve(), 40)).toBe(true);
    expect(await waitBounded(Promise.reject(new Error('x')), 40)).toBe(true);
  });
});

describe('a blocked session reaches the person as a real message', () => {
  it('issue, comment and relation writes through the client pass SessionChangedError through', async () => {
    mockApi({ ...baseRoutes() });
    setMemoryCsrf('csrf-1');
    setWritesBlocked(true);
    try {
      const calls = [
        () =>
          unwrap(
            client.PATCH('/api/v1/orgs/{org}/issues/{issue}', {
              params: { path: { org: 'orbit', issue: 'ORB-1' } },
              body: { title: 'x', version: 1 },
            }),
          ),
        () =>
          unwrap(
            client.POST('/api/v1/orgs/{org}/issues/{issue}/comments', {
              params: { path: { org: 'orbit', issue: 'ORB-1' }, header: { 'Idempotency-Key': 'k' } },
              body: { body: 'hi' },
            }),
          ),
        () =>
          unwrap(
            client.POST('/api/v1/orgs/{org}/issues/{issue}/relations', {
              params: { path: { org: 'orbit', issue: 'ORB-1' } },
              body: { type: 'related', issue: 'ORB-2' } as never,
            }),
          ),
      ];
      for (const call of calls) {
        const err = await call().catch((e: unknown) => e);
        expect(err).toBeInstanceOf(SessionChangedError);
        expect(errorMessage(err)).toMatch(/Another user signed in/);
      }
    } finally {
      setWritesBlocked(false);
      setMemoryCsrf(undefined);
    }
  });
});
