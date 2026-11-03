import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, ME, mockApi } from '../../test/mockApi';
import { renderApp } from '../../test/render';

const ORGS = [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }];
const USER = { id: 'u1', username: 'chen', name: 'Chen', email: 'chen@example.com' };
const unauth = { status: 401, json: { error: 'unauthenticated', code: 'unauthenticated' } };

/** A fake server that remembers the session across "page loads", like a real cookie would. */
function server(opts: { otpStatus?: number; retryAfter?: string; logoutStatus?: number } = {}) {
  const state = { loggedIn: false };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/me': () => (state.loggedIn ? { json: ME } : unauth),
    'GET /api/v1/auth/session': () =>
      state.loggedIn ? { json: { user: USER, orgs: ORGS, csrfToken: 'csrf-1' } } : unauth,
    'POST /api/v1/auth/otp': () => ({
      status: opts.otpStatus ?? 202,
      headers: opts.retryAfter ? { 'Retry-After': opts.retryAfter } : undefined,
      json: opts.otpStatus === 429 ? { error: 'slow down', code: 'rate_limited' } : undefined,
    }),
    'POST /api/v1/auth/otp/verify': (r) => {
      if ((r.body as { code: string }).code === '000000') {
        state.loggedIn = true;
        document.cookie = 'odt_csrf=csrf-1; path=/'; // the browser stores the Set-Cookie
        return {
          json: {
            user: { id: 'u1', username: 'chen', name: 'Chen', email: 'chen@example.com' },
            orgs: ORGS,
            csrfToken: 'csrf-1',
          },
        };
      }
      return { status: 401, json: { error: 'code expired for chen@example.com', code: 'invalid_token' } };
    },
    'POST /api/v1/auth/logout': (r) => {
      if (opts.logoutStatus)
        return { status: opts.logoutStatus, json: { error: 'boom', code: 'internal_error' } };
      if (r.headers.get('X-CSRF-Token') !== 'csrf-1')
        return { status: 403, json: { error: 'csrf', code: 'forbidden' } };
      state.loggedIn = false;
      document.cookie = 'odt_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/';
      return { status: 204 };
    },
  });
  return { state, api };
}

async function loginWith(user: ReturnType<typeof userEvent.setup>, email: string) {
  await user.type(await screen.findByLabelText('Email'), email);
  await user.click(screen.getByRole('button', { name: 'Email me a code' }));
}

describe('login (mocked server)', () => {
  it('email -> code -> in; reload keeps the session; logout ends it', async () => {
    const { state, api } = server();
    const user = userEvent.setup();
    const first = renderApp('/orbit/issues');

    // Signed out: sent to the login screen.
    expect(await screen.findByRole('heading', { name: 'Sign in to Launchpad 🚀' })).toBeInTheDocument();
    await loginWith(user, 'chen@example.com');
    const code = await screen.findByLabelText('6-digit code');
    await user.type(code, '000000');
    expect(await screen.findByRole('heading', { name: 'All issues' })).toBeInTheDocument();
    expect(state.loggedIn).toBe(true);

    // "Reload": a brand-new app and query cache. The server-side cookie session is what survives.
    first.unmount();
    renderApp('/orbit/issues');
    expect(await screen.findByRole('heading', { name: 'All issues' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Sign in to Launchpad 🚀' })).not.toBeInTheDocument();

    // Logout.
    await user.click(screen.getByRole('button', { name: 'User menu' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in to Launchpad 🚀' })).toBeInTheDocument();
    const logout = api.calls.find((c) => c.url.pathname === '/api/v1/auth/logout')!;
    expect(logout.headers.get('X-CSRF-Token')).toBe('csrf-1');
    expect(state.loggedIn).toBe(false);
  });

  it('a failed logout (500) shows a message, keeps the person signed in, and keeps the CSRF token', async () => {
    const { state, api } = server({ logoutStatus: 500 });
    const user = userEvent.setup();
    renderApp('/');
    await loginWith(user, 'chen@example.com');
    await user.type(await screen.findByLabelText('6-digit code'), '000000');
    expect(await screen.findByRole('heading', { name: 'All issues' })).toBeInTheDocument();
    document.cookie = 'odt_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'; // the memory token is the only source

    await user.click(screen.getByRole('button', { name: 'User menu' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out' }));
    expect(await screen.findByText(/Could not sign out/)).toBeInTheDocument();
    expect(state.loggedIn).toBe(true);
    expect(screen.getByRole('heading', { name: 'All issues' })).toBeInTheDocument();

    // A write after the failed logout must still carry the token.
    await user.click(screen.getByRole('button', { name: 'User menu' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out' }));
    await waitFor(() =>
      expect(api.calls.filter((c) => c.url.pathname === '/api/v1/auth/logout')).toHaveLength(2),
    );
    const second = api.calls.filter((c) => c.url.pathname === '/api/v1/auth/logout')[1]!;
    expect(second.headers.get('X-CSRF-Token')).toBe('csrf-1');
  });

  it('a wrong code shows one generic message that does not echo server detail', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/');
    await loginWith(user, 'chen@example.com');
    await user.type(await screen.findByLabelText('6-digit code'), '111111');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('That code is not valid or has expired.');
    expect(alert).not.toHaveTextContent('chen@example.com');
    expect(alert).not.toHaveTextContent('invalid_token');
  });

  it('gives the same answer for any address (no account enumeration)', async () => {
    server();
    const user = userEvent.setup();
    const texts: string[] = [];
    for (const email of ['known@example.com', 'nobody@example.com']) {
      const { unmount } = renderApp('/');
      await loginWith(user, email);
      await screen.findByLabelText('6-digit code');
      texts.push(document.body.textContent!.replace(email, ''));
      unmount();
    }
    expect(texts[0]).toBe(texts[1]);
    expect(texts[0]).toContain('If that address can sign in');
  });

  it('rejects a malformed address before any request', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/');
    await loginWith(user, 'not-an-email');
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter a valid email address.');
    expect(api.calls.some((c) => c.url.pathname === '/api/v1/auth/otp')).toBe(false);
  });

  it('shows a rate-limit message with the wait', async () => {
    server({ otpStatus: 429, retryAfter: '120' });
    const user = userEvent.setup();
    renderApp('/');
    await loginWith(user, 'chen@example.com');
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 2 minutes.');
  });

  it('never stores a token in web storage', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/');
    await loginWith(user, 'chen@example.com');
    await user.type(await screen.findByLabelText('6-digit code'), '000000');
    await screen.findByRole('heading', { name: 'All issues' });
    const dump = JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });
    expect(dump).not.toMatch(/csrf-1|odt_|session/i);
    await waitFor(() =>
      expect(within(document.body).getByRole('button', { name: 'User menu' })).toBeInTheDocument(),
    );
  });
});
