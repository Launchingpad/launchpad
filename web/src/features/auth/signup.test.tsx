import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, MARKER, webStorage } from '../../test/join';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// the public sign-up page (decisions-T12 #4 to #11).

const unauth = { status: 401, json: { error: 'unauthenticated', code: 'unauthenticated' } };
const CODE = '246801';

function server(over: Record<string, Handler> = {}) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => unauth,
    'POST /api/v1/signups': () => ({ status: 202 }),
    'POST /api/v1/signups/confirm': () => ({ status: 204 }),
    ...over,
  });
}

async function fill(user: ReturnType<typeof userEvent.setup>, over: Record<string, string> = {}) {
  const v = {
    Email: 'Ann@Example.com ',
    Name: ' Ann Example ',
    Username: 'Ann',
    'Note (optional)': '',
    ...over,
  };
  for (const [label, value] of Object.entries(v)) {
    const input = await screen.findByLabelText(label);
    await user.clear(input);
    if (value) await user.type(input, value);
  }
}
const send = (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole('button', { name: 'Send request' }));

describe('sign-up page: request', () => {
  it('works signed out, with labelled fields, and needs no CSRF token', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/signup');
    expect(await screen.findByRole('heading', { name: 'Ask to join Launchpad' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: /Sign in/ })).not.toBeInTheDocument();
    for (const l of ['Email', 'Name', 'Username', 'Note (optional)'])
      expect(screen.getByLabelText(l)).toBeInTheDocument();
    await fill(user);
    await send(user);
    await screen.findByRole('heading', { name: 'Check your email' });
    const post = callsTo(api, 'POST', '/api/v1/signups')[0]!;
    expect(post.headers.get('X-CSRF-Token')).toBeNull();
    expect(post.headers.get('Idempotency-Key')).toBeNull();
    expect(post.headers.get('Content-Type')).toContain('application/json');
  });

  it('sends the trimmed fields, a lower-case username, and no note when it is empty', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/signup');
    await fill(user);
    await send(user);
    await screen.findByRole('heading', { name: 'Check your email' });
    expect(callsTo(api, 'POST', '/api/v1/signups')[0]!.body).toEqual({
      email: 'Ann@Example.com',
      name: 'Ann Example',
      username: 'ann',
    });
  });

  it('sends the note when there is one', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/signup');
    await fill(user, { 'Note (optional)': ' I work on iOS ' });
    await send(user);
    await screen.findByRole('heading', { name: 'Check your email' });
    expect(callsTo(api, 'POST', '/api/v1/signups')[0]!.body).toMatchObject({ note: 'I work on iOS' });
  });

  it('shows the same "check your email" step for every address: it never says who exists', async () => {
    const texts: string[] = [];
    for (const email of ['new@example.com', 'has-account@example.com', 'blocked@example.com']) {
      server(); // the server answers 202 {} for all of them
      const user = userEvent.setup();
      const { unmount } = renderApp('/signup');
      await fill(user, { Email: email });
      await send(user);
      await screen.findByRole('heading', { name: 'Check your email' });
      texts.push(document.body.textContent!.split(email).join('<address>'));
      unmount();
    }
    expect(texts[1]).toBe(texts[0]);
    expect(texts[2]).toBe(texts[0]);
    expect(texts[0]).not.toMatch(/already|exists|taken|account (exists|found)|blocked/i);
  });

  it('checks the fields before it sends anything, and moves focus to the first bad one', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/signup');
    await fill(user, { Email: 'nope', Name: '', Username: '1bad' });
    await send(user);
    expect(await screen.findByText('Enter a valid email address.')).toBeInTheDocument();
    expect(screen.getByText('Enter 1 to 200 characters.')).toBeInTheDocument();
    expect(screen.getByText(/Use 1 to 39 characters: a-z/)).toBeInTheDocument();
    expect(screen.getByLabelText('Email')).toHaveFocus();
    expect(screen.getByLabelText('Email')).toHaveAttribute('aria-invalid', 'true');
    expect(callsTo(api, 'POST', '/api/v1/signups')).toHaveLength(0);
  });

  it('refuses a note over 1000 characters', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/signup');
    await fill(user);
    const note = screen.getByLabelText('Note (optional)');
    await user.click(note);
    await user.paste('x'.repeat(1001));
    await send(user);
    expect(await screen.findByText('Use 1000 characters or fewer, as plain text.')).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/api/v1/signups')).toHaveLength(0);
  });

  it('403 signup_unavailable shows a plain notice and no form', async () => {
    server({
      'POST /api/v1/signups': () => ({ status: 403, json: { error: MARKER, code: 'signup_unavailable' } }),
    });
    const user = userEvent.setup();
    renderApp('/signup');
    await fill(user);
    await send(user);
    expect(await screen.findByRole('heading', { name: 'Sign-up is not open' })).toHaveFocus();
    expect(screen.getByText(/Ask an admin for an invite/)).toBeInTheDocument();
    expect(screen.queryByLabelText('Email')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to the login page' })).toHaveAttribute('href', '/login');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it.each([
    [429, 'Too many requests. Try again in 60 seconds.', { 'Retry-After': '60' }],
    [400, 'The server did not accept the request. Check each field and try again.', undefined],
    [415, 'Could not send the request. Try again.', undefined],
    [500, 'Could not send the request. Try again.', undefined],
  ])('maps a %i to fixed text and keeps the form', async (status, text, headers) => {
    server({ 'POST /api/v1/signups': () => ({ status, json: { error: MARKER, code: 'x' }, headers }) });
    const user = userEvent.setup();
    renderApp('/signup');
    await fill(user);
    await send(user);
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByLabelText('Email')).toHaveValue('Ann@Example.com');
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('sign-up page: confirm the email', () => {
  async function toCodeStep(over: Record<string, Handler> = {}) {
    const api = server(over);
    const user = userEvent.setup();
    const { client } = renderApp('/signup');
    await fill(user);
    await send(user);
    await screen.findByRole('heading', { name: 'Check your email' });
    return { api, user, client };
  }

  it('puts focus in the code box and keeps only digits', async () => {
    const { user } = await toCodeStep();
    const box = screen.getByLabelText('6-digit code');
    expect(box).toHaveFocus();
    await user.type(box, '12ab34567890');
    expect(box).toHaveValue('123456');
  });

  it('confirms with the address and the code, then says an admin must approve', async () => {
    const { api, user } = await toCodeStep();
    await user.type(screen.getByLabelText('6-digit code'), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(await screen.findByRole('heading', { name: 'Your email is confirmed' })).toHaveFocus();
    expect(callsTo(api, 'POST', '/signups/confirm')[0]!.body).toEqual({
      email: 'Ann@Example.com',
      code: CODE,
    });
    expect(callsTo(api, 'POST', '/signups/confirm')[0]!.headers.get('X-CSRF-Token')).toBeNull();
    expect(screen.getByText(/An admin must now approve your request/)).toBeInTheDocument();
    // It does not log anyone in.
    expect(callsTo(api, 'GET', '/auth/session').length).toBeLessThanOrEqual(1);
  });

  it('asks for 6 digits before it sends anything', async () => {
    const { api, user } = await toCodeStep();
    await user.type(screen.getByLabelText('6-digit code'), '123');
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter the 6 digits from the email.');
    expect(callsTo(api, 'POST', '/signups/confirm')).toHaveLength(0);
  });

  it('a wrong code gives one fixed text and clears the box', async () => {
    const { user } = await toCodeStep({
      'POST /api/v1/signups/confirm': () => ({ status: 401, json: { error: MARKER, code: 'invalid_code' } }),
    });
    await user.type(screen.getByLabelText('6-digit code'), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'That code is not valid or has expired. Check it, or send a new code.',
    );
    expect(screen.getByLabelText('6-digit code')).toHaveValue('');
    expect(screen.getByLabelText('6-digit code')).toHaveFocus();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('429 shows the wait, 403 shows the closed notice, 500 shows a fixed text', async () => {
    let status = 429;
    const { user } = await toCodeStep({
      'POST /api/v1/signups/confirm': () => ({
        status,
        json: { error: MARKER, code: status === 403 ? 'signup_unavailable' : 'x' },
        headers: status === 429 ? { 'Retry-After': '3600' } : undefined,
      }),
    });
    const box = () => screen.getByLabelText('6-digit code');
    await user.type(box(), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many tries. Try again in 60 minutes.');
    status = 500;
    await user.type(box(), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent('Could not send the request. Try again.'),
    );
    status = 403;
    await user.type(box(), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    expect(await screen.findByRole('heading', { name: 'Sign-up is not open' })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('"Send a new code" sends the same request again and says nothing about the address', async () => {
    const { api, user } = await toCodeStep();
    await user.click(screen.getByRole('button', { name: 'Send a new code' }));
    expect(
      await screen.findByText('If the server can send email, a new code is on its way.'),
    ).toBeInTheDocument();
    const posts = callsTo(api, 'POST', '/api/v1/signups');
    expect(posts).toHaveLength(2);
    expect(posts[1]!.body).toEqual(posts[0]!.body);
  });

  it('"Use another email address" goes back to the form with the values and one fixed line', async () => {
    const { user } = await toCodeStep();
    expect(screen.queryByRole('button', { name: 'Change my details' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Use another email address' }));
    expect(await screen.findByLabelText('Name')).toHaveValue(' Ann Example ');
    expect(screen.getByTestId('signup-went-back')).toHaveTextContent(
      'A new name, username or note for the same email address is not used.',
    );
    expect(screen.getByTestId('signup-went-back')).toHaveTextContent(
      'Only a different email address makes a new request.',
    );
  });

  it('shows no such line on the first visit to the form', async () => {
    server();
    renderApp('/signup');
    await screen.findByRole('heading', { name: 'Ask to join Launchpad' });
    expect(screen.queryByTestId('signup-went-back')).not.toBeInTheDocument();
  });

  it('keeps the code out of storage, the URL, the caches and every other request', async () => {
    const { api, user, client } = await toCodeStep();
    await user.type(screen.getByLabelText('6-digit code'), CODE);
    await user.click(screen.getByRole('button', { name: 'Confirm email' }));
    await screen.findByRole('heading', { name: 'Your email is confirmed' });
    expect(webStorage()).not.toContain(CODE);
    expect(window.location.href).not.toContain(CODE);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(CODE);
    expect(document.body.textContent).not.toContain(CODE);
    const others = api.calls.filter((c) => !c.url.pathname.endsWith('/signups/confirm'));
    expect(JSON.stringify(others.map((c) => [c.url.href, c.body]))).not.toContain(CODE);
  });
});

describe('login page link', () => {
  it('links to the sign-up page (the mode is not readable while signed out)', async () => {
    server();
    renderApp('/login');
    const link = await screen.findByRole('link', { name: 'Ask to join' });
    expect(link).toHaveAttribute('href', '/signup');
    await userEvent.setup().click(link);
    expect(await screen.findByRole('heading', { name: 'Ask to join Launchpad' })).toBeInTheDocument();
  });
});
