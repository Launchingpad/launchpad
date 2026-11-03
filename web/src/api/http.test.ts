import { describe, expect, it, vi } from 'vitest';
import { CsrfMissingError, createSecureFetch, currentCsrf, readCookie, setMemoryCsrf } from './http';

function setup(opts: { csrf?: string; bearer?: string } = {}) {
  const fetchImpl = vi.fn(async (_r: Request) => new Response('{}', { status: 200 }));
  const secure = createSecureFetch({
    fetchImpl: fetchImpl as unknown as typeof fetch,
    getCsrf: () => opts.csrf,
    getBearer: () => opts.bearer,
  });
  return { fetchImpl, secure };
}
const req = (method: string, path = '/api/v1/orgs/orbit/issues') =>
  new Request(`http://localhost${path}`, {
    method,
    body: method === 'GET' ? undefined : '{"a":1}',
    headers: { 'Content-Type': 'application/json' },
  });

describe('secure fetch (CSRF)', () => {
  it.each(['POST', 'PATCH', 'PUT', 'DELETE'])('never sends a %s without a CSRF token', async (method) => {
    const { fetchImpl, secure } = setup({ csrf: undefined });
    await expect(secure(req(method))).rejects.toBeInstanceOf(CsrfMissingError);
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it.each(['POST', 'PATCH', 'PUT', 'DELETE'])('adds X-CSRF-Token to every %s', async (method) => {
    const { fetchImpl, secure } = setup({ csrf: 'tok123' });
    await secure(req(method));
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const sent = fetchImpl.mock.calls[0]![0];
    expect(sent.headers.get('X-CSRF-Token')).toBe('tok123');
    expect(sent.credentials).toBe('include');
  });

  it('sends the body unchanged', async () => {
    const { fetchImpl, secure } = setup({ csrf: 't' });
    await secure(req('POST'));
    expect(await fetchImpl.mock.calls[0]![0].text()).toBe('{"a":1}');
  });

  it('GET needs no token and sends none', async () => {
    const { fetchImpl, secure } = setup({ csrf: undefined });
    await secure(req('GET'));
    expect(fetchImpl.mock.calls[0]![0].headers.has('X-CSRF-Token')).toBe(false);
  });

  it('the two pre-login routes are exempt; logout is not', async () => {
    const a = setup({ csrf: undefined });
    await a.secure(req('POST', '/api/v1/auth/otp'));
    await a.secure(req('POST', '/api/v1/auth/otp/verify'));
    expect(a.fetchImpl).toHaveBeenCalledTimes(2);
    const b = setup({ csrf: undefined });
    await expect(b.secure(req('POST', '/api/v1/auth/logout'))).rejects.toBeInstanceOf(CsrfMissingError);
    expect(b.fetchImpl).not.toHaveBeenCalled();
  });

  it('the three public sign-up routes are exempt; the admin routes next to them are not', async () => {
    const a = setup({ csrf: undefined });
    await a.secure(req('POST', '/api/v1/signups'));
    await a.secure(req('POST', '/api/v1/signups/confirm'));
    await a.secure(req('POST', '/api/v1/invites/accept'));
    expect(a.fetchImpl).toHaveBeenCalledTimes(3);
    for (const path of [
      '/api/v1/orgs/orbit/signups/unconfirmed',
      '/api/v1/orgs/orbit/signups/x/approve',
      '/api/v1/orgs/orbit/invites',
      '/api/v1/platform/admins',
    ]) {
      const b = setup({ csrf: undefined });
      await expect(b.secure(req('POST', path))).rejects.toBeInstanceOf(CsrfMissingError);
      expect(b.fetchImpl).not.toHaveBeenCalled();
    }
  });

  it('a bearer token skips CSRF and is sent as Authorization', async () => {
    const { fetchImpl, secure } = setup({ bearer: 'odt_x' });
    await secure(req('POST'));
    const sent = fetchImpl.mock.calls[0]![0];
    expect(sent.headers.get('Authorization')).toBe('Bearer odt_x');
    expect(sent.headers.has('X-CSRF-Token')).toBe(false);
  });
});

describe('readCookie', () => {
  it('reads and decodes one cookie among several', () => {
    expect(readCookie('odt_csrf', 'a=1; odt_csrf=abc%2Fdef; b=2')).toBe('abc/def');
    expect(readCookie('odt_csrf', 'a=1')).toBeUndefined();
    expect(readCookie('odt_csrf', 'odt_csrf=')).toBeUndefined();
  });
});

describe('currentCsrf', () => {
  const clear = (name: string) => {
    document.cookie = `${name}=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/`;
  };
  it('prefers the token from the server reply over any cookie', () => {
    document.cookie = 'odt_csrf=from-cookie; path=/';
    setMemoryCsrf('from-reply');
    expect(currentCsrf()).toBe('from-reply');
    setMemoryCsrf(undefined);
    expect(currentCsrf()).toBe('from-cookie');
    clear('odt_csrf');
  });
  it('reads the bare local cookie name, and has nothing when there is none', () => {
    setMemoryCsrf(undefined);
    expect(currentCsrf()).toBeUndefined();
    document.cookie = 'odt_csrf=local-name; path=/';
    expect(currentCsrf()).toBe('local-name');
    clear('odt_csrf');
  });
});

describe('csrf_failed retry', () => {
  const csrf403 = () =>
    new Response(JSON.stringify({ error: 'csrf', code: 'csrf_failed' }), {
      status: 403,
      headers: { 'Content-Type': 'application/json' },
    });
  const post = () =>
    new Request('http://api.test/api/v1/orgs/orbit/labels', {
      method: 'POST',
      body: JSON.stringify({ name: 'x' }),
      headers: { 'Content-Type': 'application/json' },
    });

  it('reloads the token once after 403 csrf_failed, then sends the same body again with the new token', async () => {
    let token = 'stale';
    const seen: { csrf: string | null; body: string }[] = [];
    const impl = vi.fn(async (r: Request) => {
      seen.push({ csrf: r.headers.get('X-CSRF-Token'), body: await r.text() });
      return seen.length === 1 ? csrf403() : new Response('{}', { status: 201 });
    });
    const refreshCsrf = vi.fn(async () => {
      token = 'fresh';
      return true;
    });
    const f = createSecureFetch({
      fetchImpl: impl as never,
      getBearer: () => undefined,
      getCsrf: () => token,
      refreshCsrf,
    });
    const res = await f(post());
    expect(res.status).toBe(201);
    expect(refreshCsrf).toHaveBeenCalledTimes(1);
    expect(seen).toEqual([
      { csrf: 'stale', body: '{"name":"x"}' },
      { csrf: 'fresh', body: '{"name":"x"}' },
    ]);
  });

  it('does not retry when the refresher says no (signed out, or a different user): the 403 is returned', async () => {
    const impl = vi.fn(async () => csrf403());
    for (const answer of [false, undefined]) {
      const f = createSecureFetch({
        fetchImpl: impl as never,
        getBearer: () => undefined,
        getCsrf: () => 'stale',
        refreshCsrf: async () => answer as never,
      });
      expect((await f(post())).status).toBe(403);
    }
    expect(impl).toHaveBeenCalledTimes(2); // one send per fetch, no second send
  });

  it('retries only once: a second csrf_failed is returned to the caller', async () => {
    const impl = vi.fn(async () => csrf403());
    const refreshCsrf = vi.fn(async () => true);
    const f = createSecureFetch({
      fetchImpl: impl as never,
      getBearer: () => undefined,
      getCsrf: () => 'x',
      refreshCsrf,
    });
    expect((await f(post())).status).toBe(403);
    expect(impl).toHaveBeenCalledTimes(2);
    expect(refreshCsrf).toHaveBeenCalledTimes(1);
  });

  it('does not retry other 403 codes', async () => {
    const impl = vi.fn(async () => new Response(JSON.stringify({ code: 'forbidden' }), { status: 403 }));
    const refreshCsrf = vi.fn(async () => true);
    const f = createSecureFetch({
      fetchImpl: impl as never,
      getBearer: () => undefined,
      getCsrf: () => 'x',
      refreshCsrf,
    });
    expect((await f(post())).status).toBe(403);
    expect(impl).toHaveBeenCalledTimes(1);
    expect(refreshCsrf).not.toHaveBeenCalled();
  });
});

describe('__Host-odt_csrf cookie name', () => {
  it('readCookie finds the __Host- cookie among others', () => {
    expect(readCookie('__Host-odt_csrf', 'a=1; __Host-odt_session=s; __Host-odt_csrf=tok%2B1')).toBe('tok+1');
  });
  it('currentCsrf reads the __Host- name from document.cookie when nothing is in memory', () => {
    setMemoryCsrf(undefined);
    // jsdom (http://localhost) may refuse a Secure __Host- cookie, so supply the cookie string directly.
    const original = Object.getOwnPropertyDescriptor(Document.prototype, 'cookie')!;
    Object.defineProperty(document, 'cookie', {
      configurable: true,
      get: () => '__Host-odt_session=s; __Host-odt_csrf=host-token',
    });
    try {
      expect(currentCsrf()).toBe('host-token');
    } finally {
      delete (document as unknown as Record<string, unknown>).cookie;
      Object.defineProperty(Document.prototype, 'cookie', original);
    }
  });
});
