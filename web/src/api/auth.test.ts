import { afterEach, describe, expect, it, vi } from 'vitest';
import { addUserChangedHandler, fetchSession, logout, verifyOtp } from './auth';
import { currentCsrf, secureFetch, setMemoryCsrf, setWritesBlocked } from './http';
import { mockApi } from '../test/mockApi';

const ORGS = [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }];
const user = (id: string) => ({ id, username: id, name: id, email: `${id}@example.com` });
const csrf403 = { status: 403, json: { error: 'csrf', code: 'csrf_failed' } };
const write = () =>
  secureFetch(
    new Request('http://localhost:3000/api/v1/orgs/orbit/labels', {
      method: 'POST',
      body: JSON.stringify({ name: 'x' }),
      headers: { 'Content-Type': 'application/json' },
    }),
  );

afterEach(() => {
  setMemoryCsrf(undefined);
  setWritesBlocked(false);
  vi.unstubAllGlobals();
});

describe('logout and the CSRF token', () => {
  const login = async (api: ReturnType<typeof mockApi>) => {
    void api;
    await verifyOtp('a@example.com', '000000');
  };
  const base = {
    'POST /api/v1/auth/otp/verify': () => ({ json: { user: user('u1'), orgs: ORGS, csrfToken: 'csrf-1' } }),
    'POST /api/v1/orgs/:org/labels': () => ({ status: 201, json: {} }),
  };

  it('keeps the token after a 500, so the next write still carries it', async () => {
    const api = mockApi({
      ...base,
      'POST /api/v1/auth/logout': () => ({ status: 500, json: { error: 'x', code: 'internal_error' } }),
    });
    await login(api);
    await expect(logout()).rejects.toThrow();
    expect(currentCsrf()).toBe('csrf-1');
    expect((await write()).status).toBe(201);
    expect(api.calls.at(-1)!.headers.get('X-CSRF-Token')).toBe('csrf-1');
  });

  it('keeps the token after a network error', async () => {
    const api = mockApi(base);
    await login(api);
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('network down');
      }),
    );
    await expect(logout()).rejects.toThrow();
    expect(currentCsrf()).toBe('csrf-1');
  });

  it('drops the token after 204 and after 401 (already signed out)', async () => {
    for (const status of [204, 401]) {
      const api = mockApi({ ...base, 'POST /api/v1/auth/logout': () => ({ status, json: {} }) });
      await login(api);
      await logout();
      expect(currentCsrf()).toBeUndefined();
    }
  });
});

describe('csrf_failed retry and who is signed in', () => {
  function setup(sessionReply: () => { status?: number; json?: unknown }) {
    let posts = 0;
    const api = mockApi({
      'GET /api/v1/auth/session': sessionReply,
      'POST /api/v1/orgs/:org/labels': () => (++posts === 1 ? csrf403 : { status: 201, json: {} }),
    });
    return api;
  }
  const signedIn = (id: string, token: string) => () => ({
    json: { user: user(id), orgs: ORGS, csrfToken: token },
  });

  it('retries with the new token when the same user is still signed in', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    const api = setup(() => reply());
    await fetchSession(); // this tab now knows u1
    reply = signedIn('u1', 'csrf-2');
    const res = await write();
    expect(res.status).toBe(201);
    const posts = api.calls.filter((c) => c.method === 'POST');
    expect(posts.map((p) => p.headers.get('X-CSRF-Token'))).toEqual(['csrf-1', 'csrf-2']);
  });

  it('does not retry when a different user is signed in, and tells the page to reload the session', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    const api = setup(() => reply());
    await fetchSession();
    const changed = vi.fn();
    const remove = addUserChangedHandler(changed);
    reply = signedIn('u2', 'csrf-9');
    const res = await write();
    expect(res.status).toBe(403);
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1);
    expect(changed).toHaveBeenCalledTimes(1);
    remove();
  });

  it('a refused retry installs nothing: the next write is blocked and never carries the other user token', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    const api = setup(() => reply());
    await fetchSession();
    reply = signedIn('u2', 'csrf-9');
    expect((await write()).status).toBe(403); // retry refused: another user
    expect(currentCsrf()).toBe('csrf-1'); // the other user token was NOT installed
    await expect(write()).rejects.toThrow(/signed-in user changed/); // blocked, nothing is sent
    const sent = api.calls.filter((c) => c.method === 'POST').map((c) => c.headers.get('X-CSRF-Token'));
    expect(sent).toEqual(['csrf-1']);
    expect(sent).not.toContain('csrf-9');
    // The page reloads the session as the new user: writes work again, as that user.
    await fetchSession();
    expect(currentCsrf()).toBe('csrf-9');
    expect((await write()).status).toBe(201);
    expect(
      api.calls
        .filter((c) => c.method === 'POST')
        .at(-1)!
        .headers.get('X-CSRF-Token'),
    ).toBe('csrf-9');
  });

  it('two writes that both get 403 while another user is signed in: one session reload, neither is sent as the other user', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    let posts = 0;
    const api = mockApi({
      'GET /api/v1/auth/session': () => reply(),
      'POST /api/v1/orgs/:org/labels': () => (++posts <= 2 ? csrf403 : { status: 201, json: {} }),
    });
    await fetchSession();
    reply = signedIn('u2', 'csrf-9');
    const [a, b] = await Promise.all([write(), write()]);
    expect([a.status, b.status]).toEqual([403, 403]);
    expect(api.calls.filter((c) => c.url.pathname === '/api/v1/auth/session')).toHaveLength(2); // 1 initial + 1 shared
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(2); // no retries
    expect(currentCsrf()).toBe('csrf-1');
    await expect(write()).rejects.toThrow();
  });

  it('does not retry when the session is gone (401): the 403 is returned', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    const api = setup(() => reply());
    await fetchSession();
    reply = () => ({ status: 401, json: { error: 'no', code: 'unauthenticated' } });
    const res = await write();
    expect(res.status).toBe(403);
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1);
  });

  it('a delayed 403 that arrives after the session became another user is NOT replayed as that user', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    let release!: () => void;
    const api = mockApi({
      'GET /api/v1/auth/session': () => reply(),
      'POST /api/v1/orgs/:org/labels': async () => {
        await new Promise<void>((r) => (release = r)); // the reply is held
        return csrf403;
      },
    });
    await fetchSession(); // u1, token csrf-1
    const pending = write(); // sent as u1
    await vi.waitFor(() => expect(api.calls.some((c) => c.method === 'POST')).toBe(true));
    reply = signedIn('u2', 'csrf-2');
    await fetchSession(); // meanwhile the session reloads as u2
    expect(currentCsrf()).toBe('csrf-2');
    release();
    const res = await pending;
    expect(res.status).toBe(403); // returned as it is
    const posts = api.calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(1); // no replay as u2
    expect(api.calls.filter((c) => c.url.pathname === '/api/v1/auth/session')).toHaveLength(2); // no extra reload
  });

  it('signing out works while writes are blocked, and ends the block', async () => {
    let reply: () => { status?: number; json?: unknown } = signedIn('u1', 'csrf-1');
    const api = mockApi({
      'GET /api/v1/auth/session': () => reply(),
      'POST /api/v1/orgs/:org/labels': () => csrf403,
      'POST /api/v1/auth/logout': () => ({ status: 204 }),
    });
    await fetchSession();
    reply = signedIn('u2', 'csrf-9');
    expect((await write()).status).toBe(403); // refused retry: writes are blocked now
    await expect(write()).rejects.toThrow(/signed-in user changed/);
    await logout();
    const out = api.calls.find((c) => c.url.pathname === '/api/v1/auth/logout')!;
    expect(out).toBeTruthy();
    expect(out.headers.get('X-CSRF-Token')).toBe('csrf-9'); // matches the cookie session that is signed out
    expect(currentCsrf()).toBeUndefined();
  });
});

// LAUN-145 (decisions-T22 #15 a): the web app logs in in web mode only. A
// "native" client value would get a bearer token in the body instead of the
// HttpOnly cookie.
describe('web login never asks for the native mode', () => {
  it('sends only email and code to /auth/otp/verify', async () => {
    const api = mockApi({
      'POST /api/v1/auth/otp/verify': () => ({ json: { user: user('u1'), orgs: ORGS, csrfToken: 'csrf-1' } }),
    });
    await verifyOtp('a@example.com', '000000');
    expect(api.calls).toHaveLength(1);
    expect(api.calls[0]!.body).toEqual({ email: 'a@example.com', code: '000000' });
  });

  it('has no client:"native" in any source file', () => {
    const files = import.meta.glob('../**/*.{ts,tsx}', {
      query: '?raw',
      import: 'default',
      eager: true,
    }) as Record<string, string>;
    const names = Object.keys(files).filter((f) => !/\.test\.|schema\.d\.ts$/.test(f));
    expect(names.length).toBeGreaterThan(20); // the glob found the sources
    for (const f of names) {
      expect(files[f], f).not.toMatch(/client['"]?\s*:\s*['"]native['"]/);
    }
  });
});
