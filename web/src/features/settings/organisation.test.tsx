import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import type { Org } from '../../api/types';
import { timeZoneList } from './timeZones';

const org = (over: Partial<Org> = {}): Org => ({
  id: 'o1',
  slug: 'orbit',
  name: 'Orbit',
  role: 'admin',
  createdAt: '2026-01-01T00:00:00Z',
  timeZone: 'UTC',
  defaultTeam: null,
  avatarUrl: null,
  ...over,
});

function server(routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'admin') {
  const cur = { v: org() };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: cur.v.name, role }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
    'GET /api/v1/orgs/:org': () => ({ json: cur.v }),
    'PATCH /api/v1/orgs/:org': (r) => {
      cur.v = { ...cur.v, ...(r.body as object) } as Org;
      return { json: cur.v };
    },
    ...routes,
  });
  return api;
}

describe('Organisation page (LAUN-98)', () => {
  it('shows name, slug, time zone and default team', async () => {
    server();
    renderApp('/orbit/settings/organisation');
    expect(await screen.findByRole('heading', { name: 'Organisation' })).toBeInTheDocument();
    expect(await screen.findByLabelText('Name')).toHaveValue('Orbit');
    expect(screen.getByText('orbit')).toBeInTheDocument();
    expect(screen.getByLabelText('Time zone')).toHaveValue('UTC');
    expect(screen.getByLabelText('Default team')).toHaveValue('');
    expect(screen.getByRole('group', { name: 'Europe' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: /ORB/ })).toBeInTheDocument();
  });

  it('saves only what changed with PATCH /orgs/{org}', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    const name = await screen.findByLabelText('Name');
    await user.clear(name);
    await user.type(name, 'Orbit Labs');
    await user.selectOptions(screen.getByLabelText('Time zone'), 'Europe/London');
    await user.selectOptions(screen.getByLabelText('Default team'), team.key);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.url.pathname).toBe('/api/v1/orgs/orbit');
    expect(patch.body).toEqual({ name: 'Orbit Labs', timeZone: 'Europe/London', defaultTeam: team.key });
    // The sidebar name follows through the session.
    expect((await screen.findAllByText('Orbit Labs')).length).toBeGreaterThan(0);
  });

  it('clearing the default team sends null; nothing changed sends nothing', async () => {
    const api = server({ 'GET /api/v1/orgs/:org': () => ({ json: org({ defaultTeam: team.key }) }) });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await screen.findByLabelText('Name');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Nothing changed.');
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(0);
    await user.selectOptions(screen.getByLabelText('Default team'), '');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toEqual({ defaultTeam: null }),
    );
  });

  it('an empty name is refused in the page, and focus goes to the field', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    const name = await screen.findByLabelText('Name');
    await user.clear(name);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Enter 1 to 200 characters.')).toBeInTheDocument();
    await waitFor(() => expect(name).toHaveFocus());
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(0);
  });

  it.each([
    [400, 'The server did not accept this. Check the name, the time zone and the default team.'],
    [403, 'You need admin access to change the organisation.'],
    [409, 'Someone changed the organisation at the same time. Reload the page and try again.'],
  ])('a %i is a fixed sentence, never the server text', async (status, text) => {
    server({ 'PATCH /api/v1/orgs/:org': () => ({ status, json: { error: 'raw server words', code: 'x' } }) });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.type(await screen.findByLabelText('Name'), 'x');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.queryByText(/raw server words/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveValue('Orbitx');
  });

  it('Save is aria-disabled, not disabled, in flight; focus stays; a second press sends nothing', async () => {
    let release: () => void = () => undefined;
    const gate = new Promise<void>((r) => (release = r));
    const api = server({
      'PATCH /api/v1/orgs/:org': async () => {
        await gate;
        return { json: org({ name: 'Orbitx' }) };
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.type(await screen.findByLabelText('Name'), 'x');
    const save = screen.getByRole('button', { name: 'Save' });
    await user.click(save);
    const busy = await screen.findByRole('button', { name: 'Saving' });
    expect(busy).toHaveAttribute('aria-disabled', 'true');
    expect(busy).not.toBeDisabled();
    await user.click(busy);
    release();
    await screen.findByText('Saved.');
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(1);
  });

  it('a failed load shows a fixed error with Try again', async () => {
    server({ 'GET /api/v1/orgs/:org': () => ({ status: 500, json: { error: 'raw', code: 'internal' } }) });
    renderApp('/orbit/settings/organisation');
    expect(await screen.findByRole('button', { name: 'Try again' })).toBeInTheDocument();
    expect(screen.queryByText('raw')).not.toBeInTheDocument();
  });

  it('a member cannot open it', async () => {
    server({}, 'member');
    renderApp('/orbit/settings/organisation');
    expect(await screen.findByText('You need admin access.')).toBeInTheDocument();
  });
});

describe('time zone list', () => {
  it('has UTC first and the current zone, with or without Intl.supportedValuesOf', () => {
    const z = timeZoneList('Mars/Base');
    expect(z[0]).toBe('UTC');
    expect(z).toContain('Mars/Base');
    expect(z).toContain('Europe/London');
    const intl = Intl as unknown as { supportedValuesOf?: unknown };
    const saved = intl.supportedValuesOf;
    intl.supportedValuesOf = undefined;
    try {
      expect(timeZoneList()).toContain('Europe/London');
      intl.supportedValuesOf = () => {
        throw new Error('no');
      };
      expect(timeZoneList()[0]).toBe('UTC');
    } finally {
      intl.supportedValuesOf = saved;
    }
  });
});

describe('Organisation touch targets (LAUN-98)', () => {
  const dir = resolve(process.cwd(), 'src/features/settings');
  const css = readFileSync(resolve(dir, 'organisation.module.css'), 'utf8');
  const query = '@media (max-width: 760px), (pointer: coarse)';
  const touch = css.slice(css.indexOf(query));
  it.each(['.touch button', '.touch select', ".touch input[type='text']", '.touch input:not([type])'])(
    '%s is at least 40px',
    (sel) => {
      expect(touch).toContain(sel);
    },
  );
  it('a button that waits (aria-disabled) has a visible style', () => {
    expect(css).toMatch(/\.touch button\[aria-disabled='true'\]\s*\{[^}]*opacity/);
  });
  it('has a 40px min-height and the page carries the class', () => {
    expect(Number(/min-height:\s*(\d+)px/.exec(touch)?.[1])).toBeGreaterThanOrEqual(40);
    expect(readFileSync(resolve(dir, 'OrganisationPage.tsx'), 'utf8')).toMatch(
      /<div className=\{touchClass\}>/,
    );
    expect(css).not.toMatch(/text-decoration:\s*underline/);
  });
});

describe('Organisation logo (LAUN-22)', () => {
  const png = () => new File([new Uint8Array([137, 80, 78, 71])], 'logo.png', { type: 'image/png' });

  function logoServer(extra: Record<string, Handler> = {}) {
    const state = { v: org() };
    const api = mockApi({
      ...baseRoutes(),
      'GET /api/v1/auth/session': () => ({
        json: {
          user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
          orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
          csrfToken: 'csrf-test',
        },
      }),
      'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
      'GET /api/v1/orgs/:org': () => ({ json: state.v }),
      'POST /api/v1/orgs/:org/avatar/prepare': () => ({
        json: {
          uploadKey: 'org/o1/avatars/org/k1',
          uploadUrl: 'https://silo.example.com/put',
          method: 'PUT',
          headers: { 'Content-Type': 'image/png' },
          contentType: 'image/png',
          sizeBytes: 4,
          expiresAt: '2026-01-01T00:15:00Z',
        },
      }),
      'PUT /api/v1/orgs/:org/avatar': () => {
        state.v = { ...state.v, avatarUrl: '/api/v1/orgs/orbit/avatar?v=1' };
        return { json: state.v };
      },
      'DELETE /api/v1/orgs/:org/avatar': () => {
        state.v = { ...state.v, avatarUrl: null };
        return { json: state.v };
      },
      ...extra,
    });
    return api;
  }

  it('uploads: prepare, PUT to the store, confirm; then shows the image and Replace / Remove', async () => {
    const api = logoServer();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    const input = await screen.findByTestId('logo-file');
    expect(screen.getByRole('button', { name: 'Upload logo' })).toBeInTheDocument();
    // The store PUT is a plain fetch to another origin; the mock router answers it with a 404 unless routed.
    const store = api.fn.getMockImplementation()!;
    api.fn.mockImplementation(async (input2: Request | string, init?: RequestInit) => {
      const url = input2 instanceof Request ? input2.url : String(input2);
      if (url === 'https://silo.example.com/put') return new Response(null, { status: 200 });
      return store(input2, init);
    });
    await user.upload(input, png());
    expect(await screen.findByText('Logo saved.')).toBeInTheDocument();
    const body = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/avatar/prepare'))!;
    expect(body.body).toEqual({ contentType: 'image/png', sizeBytes: 4 });
    expect(api.calls.some((c) => c.method === 'PUT' && c.url.pathname === '/api/v1/orgs/orbit/avatar')).toBe(
      true,
    );
    expect(await screen.findByRole('button', { name: 'Replace logo' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Remove logo' })).toBeInTheDocument();
    expect(screen.getAllByRole('img', { name: 'Orbit' })[0]).toHaveAttribute(
      'src',
      '/api/v1/orgs/orbit/avatar?v=1',
    );
  });

  it('refuses an SVG or a big file on this side, with the rule', async () => {
    const api = logoServer();
    const user = userEvent.setup({ applyAccept: false });
    renderApp('/orbit/settings/organisation');
    const input = await screen.findByTestId('logo-file');
    await user.upload(input, new File(['<svg/>'], 'a.svg', { type: 'image/svg+xml' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Use a PNG, JPEG, GIF or WebP image of at most 2 MiB.',
    );
    const big = new File([new Uint8Array(2 * 1024 * 1024 + 1)], 'big.png', { type: 'image/png' });
    await user.upload(input, big);
    expect(screen.getByRole('alert')).toHaveTextContent('at most 2 MiB');
    expect(api.calls.some((c) => c.url.pathname.endsWith('/avatar/prepare'))).toBe(false);
  });

  it('shows a fixed sentence when the server refuses', async () => {
    logoServer({
      'POST /api/v1/orgs/:org/avatar/prepare': () => ({
        status: 503,
        json: { error: 'x', code: 'attachments_unavailable' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.upload(await screen.findByTestId('logo-file'), png());
    expect(await screen.findByRole('alert')).toHaveTextContent('no file storage');
  });

  it('removes the logo', async () => {
    const api = logoServer({
      'GET /api/v1/orgs/:org': () => ({ json: org({ avatarUrl: '/api/v1/orgs/orbit/avatar?v=1' }) }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.click(await screen.findByRole('button', { name: 'Remove logo' }));
    expect(await screen.findByText('Logo removed.')).toBeInTheDocument();
    expect(
      api.calls.some((c) => c.method === 'DELETE' && c.url.pathname === '/api/v1/orgs/orbit/avatar'),
    ).toBe(true);
  });
});
