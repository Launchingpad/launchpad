import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { currentCsrf } from '../../api/http';
import { keys } from '../../api/keys';
import { callsTo, MARKER, webStorage } from '../../test/join';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { forgetInviteToken } from './InvitePage';

// the public invite page, /invite#<token> (decisions-T12 #16 to #22).

const TOKEN = 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCde'; // 43 base64url characters
const OTHER = 'ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210-_ZyXwV';
const ORGS = [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'member' }];
const USER = { id: 'u9', username: 'ann', name: 'Ann', email: 'ann@example.com', isPlatformAdmin: false };
const unauth = { status: 401, json: { error: 'unauthenticated', code: 'unauthenticated' } };

function openAt(hash: string) {
  window.history.replaceState(null, '', `/invite${hash}`);
}

function server(over: Record<string, Handler> = {}) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => unauth,
    'POST /api/v1/invites/accept': () => ({
      status: 201,
      json: {
        status: 'created',
        org: ORGS[0],
        session: { user: USER, orgs: ORGS, csrfToken: 'csrf-invite' },
      },
    }),
    ...over,
  });
}

async function fill(user: ReturnType<typeof userEvent.setup>, name = ' Ann Example ', username = 'Ann') {
  if (name) await user.type(await screen.findByLabelText('Name'), name);
  if (username) await user.type(screen.getByLabelText('Username'), username);
}
const join = (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole('button', { name: 'Join' }));

beforeEach(() => forgetInviteToken());
afterEach(() => {
  forgetInviteToken();
  window.history.replaceState(null, '', '/');
});

describe('invite page: the token', () => {
  it('reads the token from the hash and removes it from the URL at once', async () => {
    server();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await screen.findByRole('heading', { name: 'Join Launchpad' });
    expect(window.location.hash).toBe('');
    expect(window.location.href).not.toContain(TOKEN);
    expect(JSON.stringify(window.history.state)).not.toContain(TOKEN);
  });

  it('sends the token only in the POST body, and starts no other call with it', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await fill(user);
    await join(user);
    await screen.findByRole('heading', { name: 'All issues' });
    const post = callsTo(api, 'POST', '/invites/accept');
    expect(post).toHaveLength(1);
    expect(post[0]!.body).toEqual({ token: TOKEN, name: 'Ann Example', username: 'ann' });
    expect(post[0]!.headers.get('X-CSRF-Token')).toBeNull();
    const others = api.calls.filter((c) => c !== post[0]);
    expect(JSON.stringify(others.map((c) => [c.url.href, c.body, [...c.headers.entries()]]))).not.toContain(
      TOKEN,
    );
  });

  it('keeps the token out of storage, the URL and both caches', async () => {
    server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    const { client } = renderApp('/invite');
    await fill(user);
    await join(user);
    await screen.findByRole('heading', { name: 'All issues' });
    expect(webStorage()).not.toContain(TOKEN);
    expect(window.location.href).not.toContain(TOKEN);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(TOKEN);
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it('with no token it says so and shows no form', async () => {
    const api = server();
    openAt('');
    renderApp('/invite');
    expect(await screen.findByRole('heading', { name: 'You need an invite link' })).toHaveFocus();
    expect(screen.queryByLabelText('Name')).not.toBeInTheDocument();
    expect(callsTo(api, 'POST', '/invites/accept')).toHaveLength(0);
  });

  it('with a token of the wrong shape it asks for the whole link and sends nothing', async () => {
    const api = server();
    openAt('#short');
    renderApp('/invite');
    expect(
      await screen.findByRole('heading', { name: 'This invite link is not complete' }),
    ).toBeInTheDocument();
    expect(window.location.hash).toBe('');
    expect(callsTo(api, 'POST', '/invites/accept')).toHaveLength(0);
  });

  it('takes a new link opened in the same tab and removes it too', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await screen.findByLabelText('Name');
    window.history.replaceState(null, '', `/invite#${OTHER}`);
    window.dispatchEvent(new HashChangeEvent('hashchange'));
    await waitFor(() => expect(window.location.hash).toBe(''));
    await fill(user);
    await join(user);
    await screen.findByRole('heading', { name: 'All issues' });
    expect((callsTo(api, 'POST', '/invites/accept')[0]!.body as { token: string }).token).toBe(OTHER);
  });
});

describe('invite page: accept', () => {
  it('a new person lands signed in, and the CSRF token is kept for the next write', async () => {
    server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    const { client } = renderApp('/invite');
    await fill(user);
    await join(user);
    await screen.findByRole('heading', { name: 'All issues' });
    expect(client.getQueryData(keys.session)).toEqual({ user: USER, orgs: ORGS });
    expect(currentCsrf()).toBe('csrf-invite');
  });

  it('a person with an account leaves both boxes empty: only the token is sent; no session starts', async () => {
    const api = server({
      'POST /api/v1/invites/accept': () => ({ status: 200, json: { status: 'joined', org: ORGS[0] } }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    const { client } = renderApp('/invite');
    await screen.findByLabelText('Name');
    await join(user);
    expect(await screen.findByRole('heading', { name: 'You joined' })).toHaveFocus();
    expect(callsTo(api, 'POST', '/invites/accept')[0]!.body).toEqual({ token: TOKEN });
    expect(screen.getByText(/You are now a member of Orbit/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to the login page' })).toHaveAttribute('href', '/login');
    expect(client.getQueryData(keys.session)).toBeUndefined();
    expect(currentCsrf()).toBeUndefined();
  });

  it('needs both name and username when one is filled, and sends nothing before that', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await fill(user, '', 'ann');
    await join(user);
    expect(await screen.findByText('Enter 1 to 200 characters.')).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveFocus();
    await user.type(screen.getByLabelText('Name'), 'Ann');
    await user.clear(screen.getByLabelText('Username'));
    await user.type(screen.getByLabelText('Username'), '9x');
    await join(user);
    expect(await screen.findByText(/Use 1 to 39 characters: a-z/)).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/invites/accept')).toHaveLength(0);
  });
});

describe('invite page: errors', () => {
  it('401 invalid_invite is one generic text, and the token is gone', async () => {
    const api = server({
      'POST /api/v1/invites/accept': () => ({ status: 401, json: { error: MARKER, code: 'invalid_invite' } }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await fill(user);
    await join(user);
    expect(await screen.findByRole('heading', { name: 'This invite link does not work' })).toHaveFocus();
    expect(screen.getByText(/wrong, expired, used or revoked/)).toBeInTheDocument();
    expect(screen.queryByLabelText('Name')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(callsTo(api, 'POST', '/invites/accept')).toHaveLength(1);
  });

  it('400 keeps the form and the token: a retry with both fields works', async () => {
    let n = 0;
    const api = server({
      'POST /api/v1/invites/accept': () =>
        n++ === 0
          ? { status: 400, json: { error: MARKER, code: 'validation_failed' } }
          : {
              status: 201,
              json: { status: 'created', org: ORGS[0], session: { user: USER, orgs: ORGS, csrfToken: 'c' } },
            },
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await screen.findByLabelText('Name');
    await join(user); // an empty form for a new person: the server says 400
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Enter your name and a username. A new account needs both.',
    );
    expect(document.body.textContent).not.toContain(MARKER);
    await fill(user);
    await join(user);
    await screen.findByRole('heading', { name: 'All issues' });
    const posts = callsTo(api, 'POST', '/invites/accept');
    expect((posts[1]!.body as { token: string }).token).toBe(TOKEN);
  });

  it('409 is an error on the username field and keeps the token', async () => {
    server({
      'POST /api/v1/invites/accept': () => ({
        status: 409,
        json: { error: 'a user with username "ann" exists already; choose another', code: 'conflict' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await fill(user);
    await join(user);
    expect(
      await screen.findByText(
        'This username is taken, or your account changed. Choose another username and try again.',
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Username')).toHaveFocus();
    expect(screen.getByLabelText('Username')).toHaveAttribute('aria-invalid', 'true');
    expect(document.body.textContent).not.toContain('exists already;');
  });

  it('403 signup_unavailable shows a plain notice', async () => {
    server({
      'POST /api/v1/invites/accept': () => ({
        status: 403,
        json: { error: MARKER, code: 'signup_unavailable' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await fill(user);
    await join(user);
    expect(await screen.findByRole('heading', { name: 'Invites are closed' })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it.each([
    [429, 'Too many tries. Try again in 45 seconds.', { 'Retry-After': '45' }],
    [500, 'Could not accept the invite. Try again.', undefined],
  ])('%i shows fixed text and keeps the form', async (status, text, headers) => {
    server({
      'POST /api/v1/invites/accept': () => ({ status, json: { error: MARKER, code: 'x' }, headers }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/invite');
    await fill(user);
    await join(user);
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByLabelText('Name')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    expect(document.body.textContent).not.toContain(TOKEN);
  });
});
