import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { fieldFromMessage, forgetSetupToken, validate } from './SetupPage';

const TOKEN = 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCde'; // 43 base64url characters
const SECRET = 'odt_first-admin-token-secret-value';
const ORGS = [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }];
const USER = {
  id: 'u1',
  username: 'chen',
  name: 'Chen',
  email: 'chen@example.com',
  isPlatformAdmin: true,
};
const unauth = { status: 401, json: { error: 'unauthenticated', code: 'unauthenticated' } };

function openAt(hash: string) {
  window.history.replaceState(null, '', `/setup${hash}`);
}

function server(over: Record<string, Handler> = {}, setupRequired = true) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => unauth,
    'GET /api/v1/setup': () => ({ json: { setupRequired } }),
    'POST /api/v1/setup': () => ({
      status: 201,
      json: { user: USER, orgs: ORGS, csrfToken: 'csrf-1', token: { token: { id: 't1' }, secret: SECRET } },
    }),
    ...over,
  });
}

async function fill(user: ReturnType<typeof userEvent.setup>, over: Record<string, string> = {}) {
  const v = {
    Username: 'Chen',
    Name: 'Chen Example',
    Email: 'chen@example.com',
    'Organisation name': 'Orbit',
    Slug: 'orbit',
    'Team name': 'Orbit team',
    'Team key': 'orb',
    ...over,
  };
  for (const [label, value] of Object.entries(v)) {
    const input = await screen.findByLabelText(label);
    await user.clear(input);
    if (value) await user.type(input, value);
  }
}

const submit = (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole('button', { name: 'Set up Launchpad' }));

beforeEach(() => forgetSetupToken());
afterEach(() => {
  forgetSetupToken();
  window.history.replaceState(null, '', '/');
});

describe('setup page', () => {
  it('without a token it says where to find the link and shows no form', async () => {
    const api = server();
    openAt('');
    renderApp('/setup');
    expect(await screen.findByRole('heading', { name: 'You need a setup link' })).toBeInTheDocument();
    expect(screen.getByText(/deploy\.sh --setup-link/)).toBeInTheDocument();
    expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('works signed out: no redirect to the login page', async () => {
    server();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    expect(await screen.findByRole('heading', { name: /Set up Launchpad/ })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: /Sign in/ })).not.toBeInTheDocument();
  });

  it('reads the token from the hash, removes it from the URL, and sends it in the body', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await screen.findByLabelText('Username');
    expect(window.location.hash).toBe('');
    expect(window.location.href).not.toContain(TOKEN);
    await fill(user);
    await submit(user);
    await screen.findByTestId('first-token');
    const post = api.calls.find((c) => c.method === 'POST')!;
    expect(post.body).toEqual({
      token: TOKEN,
      user: { username: 'chen', name: 'Chen Example', email: 'chen@example.com' },
      org: { slug: 'orbit', name: 'Orbit' },
      team: { key: 'ORB', name: 'Orbit team' },
    });
    expect(post.headers.get('X-CSRF-Token')).toBeNull(); // no session yet: the route is CSRF-exempt
  });

  it('shows a link that is cut short as incomplete and sends nothing', async () => {
    const api = server();
    openAt('#short');
    renderApp('/setup');
    expect(
      await screen.findByRole('heading', { name: 'This setup link is not complete' }),
    ).toBeInTheDocument();
    expect(window.location.hash).toBe('');
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('checks every field before it sends', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user, { Username: '1bad', Email: 'nope', Slug: 'A', 'Team key': '1' });
    await submit(user);
    expect(
      await screen.findByText(/Start with a letter\./, { selector: '#setup-username-error' }),
    ).toBeInTheDocument();
    expect(screen.getByText('Enter a valid email address.')).toBeInTheDocument();
    expect(document.getElementById('setup-slug-error')).toBeInTheDocument();
    expect(document.getElementById('setup-key-error')).toBeInTheDocument();
    expect(screen.getByLabelText('Username')).toHaveFocus();
    expect(screen.getByLabelText('Team key')).toHaveAccessibleDescription(
      /Start with a letter\..*starts every issue number/,
    );
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('validate matches the server rules', () => {
    const ok = {
      username: 'a',
      name: 'N',
      email: 'a@b.co',
      orgName: 'O',
      slug: 'ab',
      teamName: 'T',
      key: 'A',
    };
    expect(validate(ok)).toEqual({});
    expect(validate({ ...ok, slug: 'a' }).slug).toBeDefined(); // 2 to 40
    expect(validate({ ...ok, slug: '-ab' }).slug).toBeDefined();
    expect(validate({ ...ok, slug: 'ab-' }).slug).toBeDefined();
    expect(validate({ ...ok, slug: 'a'.repeat(41) }).slug).toBeDefined();
    expect(validate({ ...ok, username: 'a'.repeat(40) }).username).toBeDefined();
    expect(validate({ ...ok, key: 'A'.repeat(11) }).key).toBeDefined();
    expect(validate({ ...ok, name: 'x'.repeat(201) }).name).toBeDefined();
    expect(validate({ ...ok, email: `${'a'.repeat(250)}@b.co` }).email).toBeDefined();
  });

  it('maps a 400 message to its field, and shows it', async () => {
    expect(fieldFromMessage('user.email must be a valid email address')).toBe('email');
    expect(fieldFromMessage('org slug must be 2-40 characters')).toBe('slug');
    expect(fieldFromMessage('team.key must be 1-10 characters')).toBe('key');
    expect(fieldFromMessage('something else')).toBeUndefined();

    server({
      'POST /api/v1/setup': () => ({
        status: 400,
        json: { error: 'user.email must be a valid email address', code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    expect(await screen.findByText('user.email must be a valid email address')).toBeInTheDocument();
    expect(screen.getByLabelText('Email')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByLabelText('Email')).toHaveFocus();
    // The link was not used up: the form stays.
    expect(screen.getByLabelText('Username')).toBeInTheDocument();
  });

  it('401 invalid_setup_token shows the invalid-link state with the way to get a new one', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 401,
        json: { error: 'invalid setup link', code: 'invalid_setup_token' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    const heading = await screen.findByRole('heading', { name: 'This setup link does not work' });
    expect(heading).toHaveFocus();
    expect(screen.getByText(/deploy\.sh --setup-link/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it('409 setup_complete links to the login page', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 409,
        json: { error: 'setup is complete', code: 'setup_complete' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    expect(await screen.findByRole('heading', { name: 'Setup is already done' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to the login page' })).toHaveAttribute('href', '/login');
  });

  it('shows the done state at once when GET /setup says setup is not required', async () => {
    server({}, false);
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    expect(await screen.findByRole('heading', { name: 'Setup is already done' })).toBeInTheDocument();
    expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
  });

  it('409 conflict (slug in use) is a field error', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 409,
        json: { error: 'an org with slug "orbit" exists already', code: 'conflict' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    expect(await screen.findByText(/exists already\. Use another slug/)).toBeInTheDocument();
    expect(screen.getByLabelText('Slug')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByLabelText('Slug')).toHaveFocus();
  });

  it('429 shows the wait and keeps the form', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 429,
        headers: { 'Retry-After': '120' },
        json: { error: 'slow down', code: 'rate_limited' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 2 minutes.');
    expect(screen.getByLabelText('Username')).toBeInTheDocument();
  });

  it('success shows the token once, needs a confirmation, then goes to the app', async () => {
    const api = server({
      'GET /api/v1/auth/session': () => ({ json: { user: USER, orgs: ORGS, csrfToken: 'csrf-1' } }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);

    expect(await screen.findByTestId('first-token')).toHaveTextContent(SECRET);
    expect(screen.getByRole('alert')).toHaveTextContent('Save this token now. It is not shown again.');
    const next = screen.getByRole('button', { name: 'Continue to Launchpad' });
    expect(next).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Copy token' }));
    expect(await navigator.clipboard.readText()).toBe(SECRET);

    // Not in web storage, not in the URL, in no request after the POST.
    const dump = JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });
    expect(dump).not.toContain(SECRET);
    expect(dump).not.toContain(TOKEN);
    expect(window.location.href).not.toContain(TOKEN);

    await user.click(screen.getByLabelText('I saved the token in a safe place.'));
    expect(next).toBeEnabled();
    await user.click(next);

    expect(await screen.findByRole('heading', { name: 'Get Launchpad ready' })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(SECRET);
    const later = api.calls.slice(api.calls.findIndex((c) => c.method === 'POST') + 1);
    expect(JSON.stringify(later.map((c) => [c.url.href, c.body, [...c.headers.entries()]]))).not.toContain(
      SECRET,
    );
    expect(later.some((c) => c.headers.get('Authorization'))).toBe(false);
  });

  it('shows fixed text for a server error and never the link token', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 500,
        json: { error: `boom ${TOKEN}`, code: 'internal_error' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Setup failed. Try again.');
    expect(alert).not.toHaveTextContent(TOKEN);
    expect(document.body.textContent).not.toContain(TOKEN);
    const dump = JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });
    expect(dump).not.toContain(TOKEN);
  });

  it('cuts the link token out of a 400 text that has no field', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 400,
        json: { error: `bad request ${TOKEN} here`, code: 'bad_request' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('bad request');
    expect(alert).not.toHaveTextContent(TOKEN);
  });

  it('takes a new link opened in the same tab (fragment change only)', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt('');
    renderApp('/setup');
    expect(await screen.findByRole('heading', { name: 'You need a setup link' })).toBeInTheDocument();
    window.location.hash = `#${TOKEN}`;
    expect(await screen.findByLabelText('Username')).toBeInTheDocument();
    expect(window.location.hash).toBe('');
    await fill(user);
    await submit(user);
    await screen.findByTestId('first-token');
    expect(api.calls.find((c) => c.method === 'POST')!.body).toMatchObject({ token: TOKEN });
  });

  it('after a 401 a new link in the same tab brings the form back', async () => {
    const NEW = 'ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210_-ZyXwV';
    let n = 0;
    const api = server({
      'POST /api/v1/setup': () => {
        n++;
        return n === 1
          ? { status: 401, json: { error: 'invalid', code: 'invalid_setup_token' } }
          : {
              status: 201,
              json: {
                user: USER,
                orgs: ORGS,
                csrfToken: 'csrf-1',
                token: { token: { id: 't1' }, secret: SECRET },
              },
            };
      },
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    await screen.findByRole('heading', { name: 'This setup link does not work' });
    window.location.hash = `#${NEW}`;
    await fill(user);
    expect(window.location.hash).toBe('');
    await submit(user);
    await screen.findByTestId('first-token');
    const posts = api.calls.filter((c) => c.method === 'POST');
    expect(posts[1]!.body).toMatchObject({ token: NEW });
  });
});

describe('setup page details', () => {
  it('says 1 second for a one second wait', async () => {
    server({
      'POST /api/v1/setup': () => ({
        status: 429,
        headers: { 'Retry-After': '1' },
        json: { error: 'slow', code: 'rate_limited' },
      }),
    });
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user);
    await submit(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('Try again in 1 second.');
  });

  it('lower-cases the slug as the person types and sends it lower-case', async () => {
    const api = server();
    const user = userEvent.setup();
    openAt(`#${TOKEN}`);
    renderApp('/setup');
    await fill(user, { Slug: 'ORB' });
    expect(screen.getByLabelText('Slug')).toHaveValue('orb');
    await submit(user);
    await screen.findByTestId('first-token');
    const post = api.calls.find((c) => c.method === 'POST')!;
    expect((post.body as { org: { slug: string } }).org.slug).toBe('orb');
  });
});
