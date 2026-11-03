import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fetchSession } from '../../api/auth';
import { secureFetch, setMemoryCsrf, setWritesBlocked } from '../../api/http';
import { baseRoutes, mockApi } from '../../test/mockApi';
import { useSession } from './session';
import { renderApp } from '../../test/render';
import { screen } from '@testing-library/react';

const ORGS = [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }];
const user = (id: string) => ({ id, username: id, name: id, email: `${id}@example.com` });

function Consumer() {
  useSession();
  return null;
}

afterEach(() => {
  setMemoryCsrf(undefined);
  setWritesBlocked(false);
});

describe('user-changed handler registration', () => {
  it('still reloads the session when one consumer unmounts and another stays mounted', async () => {
    let who = 'u1';
    const api = mockApi({
      ...baseRoutes(),
      'GET /api/v1/auth/session': () => ({ json: { user: user(who), orgs: ORGS, csrfToken: `csrf-${who}` } }),
      'POST /api/v1/orgs/:org/labels': () => ({
        status: 403,
        json: { error: 'csrf', code: 'csrf_failed' },
      }),
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(qc, 'invalidateQueries');
    const wrap = (n: number) => (
      <QueryClientProvider client={qc}>
        {Array.from({ length: n }, (_, i) => (
          <Consumer key={i} />
        ))}
      </QueryClientProvider>
    );
    const view = render(wrap(2));
    await waitFor(() => expect(api.calls.some((c) => c.url.pathname === '/api/v1/auth/session')).toBe(true));
    await fetchSession(); // this tab knows u1
    view.rerender(wrap(1)); // one consumer unmounts (like Comments leaving the page)

    who = 'u2'; // another tab signs in as someone else
    const res = await secureFetch(
      new Request('http://localhost:3000/api/v1/orgs/orbit/labels', {
        method: 'POST',
        body: '{}',
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    expect(res.status).toBe(403);
    expect(spy).toHaveBeenCalledWith({ queryKey: ['session'] });
  });
});

describe('isPlatformAdmin in the session', () => {
  it('reaches the session context from GET /auth/session', async () => {
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/auth/session': () => ({
        json: { user: { ...user('u1'), isPlatformAdmin: true }, orgs: ORGS, csrfToken: 'csrf-u1' },
      }),
    });
    let seen: boolean | undefined;
    function Probe() {
      seen = useSession().data?.user.isPlatformAdmin;
      return null;
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <Probe />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(seen).toBe(true));
  });

  it('is false for an ordinary user', async () => {
    mockApi(baseRoutes());
    renderApp('/orbit/issues');
    expect(await screen.findByRole('heading', { name: 'All issues' })).toBeInTheDocument();
    const s = await fetchSession();
    expect(s?.user.isPlatformAdmin).toBe(false);
  });
});
