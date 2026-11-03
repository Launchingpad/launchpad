import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { joinServer, page } from '../../test/join';
import { projectOf, type Handler } from '../../test/mockApi';
import { QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router-dom';
import { OrgProvider } from '../../api/org';
import { renderApp, testClient } from '../../test/render';
import { ProjectLogoField } from './ProjectLogoField';

const ID = '01a00000-0000-7000-8000-000000000001';
const URL1 = '/api/v1/orgs/orbit/projects/' + ID + '/avatar?v=1';

const proj = (over: Partial<Project> = {}): Project =>
  projectOf({
    id: ID,
    name: 'Alpha launch',
    description: '',
    content: '',
    contentTruncated: false,
    state: 'started',
    lead: null,
    startDate: null,
    targetDate: null,
    teams: [{ id: 'team1', key: 'ORB', name: 'Orbit' }],
    labels: [],
    version: 1,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    startedAt: null,
    completedAt: null,
    canceledAt: null,
    archivedAt: null,
    avatarUrl: null,
    emoji: null,
    issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
    ...over,
  });

const png = () => new File([new Uint8Array([137, 80, 78, 71])], 'logo.png', { type: 'image/png' });

function server(start: Project, extra: Record<string, Handler> = {}) {
  const cur = { v: start };
  const api = joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page([cur.v]) }),
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: cur.v }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      'POST /api/v1/orgs/:org/projects/:project/avatar/prepare': () => ({
        json: {
          uploadKey: 'org/o1/avatars/project/p/k1',
          uploadUrl: 'https://silo.example.com/put',
          method: 'PUT',
          headers: { 'Content-Type': 'image/png', 'x-amz-meta': 'a' },
          contentType: 'image/png',
          sizeBytes: 4,
          expiresAt: '2026-01-01T00:15:00Z',
        },
      }),
      'PUT /api/v1/orgs/:org/projects/:project/avatar': () => {
        cur.v = { ...cur.v, avatarUrl: URL1, version: 2 };
        return { json: cur.v };
      },
      'DELETE /api/v1/orgs/:org/projects/:project/avatar': () => {
        cur.v = { ...cur.v, avatarUrl: null };
        return { json: cur.v };
      },
      ...extra,
    },
  });
  const store: { headers?: unknown; calls: number } = { calls: 0 };
  const orig = api.fn.getMockImplementation()!;
  api.fn.mockImplementation(async (input: Request | string, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url === 'https://silo.example.com/put') {
      store.calls++;
      store.headers = init?.headers;
      return new Response(null, { status: 200 });
    }
    return orig(input, init);
  });
  return { api, store };
}

describe('Project logo (LAUN-162)', () => {
  it('uploads: prepare, PUT with the given headers, confirm; the page header and field update at once', async () => {
    const { api, store } = server(proj());
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    await screen.findByRole('heading', { name: 'Alpha launch' });
    expect(screen.getByRole('button', { name: 'Upload logo' })).toBeInTheDocument();
    await user.upload(await screen.findByTestId('logo-file'), png());
    expect(await screen.findByText('Logo saved.')).toBeInTheDocument();
    const prep = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/avatar/prepare'))!;
    expect(prep.url.pathname).toBe(`/api/v1/orgs/orbit/projects/${ID}/avatar/prepare`);
    expect(prep.body).toEqual({ contentType: 'image/png', sizeBytes: 4 });
    expect(store.headers).toEqual({ 'Content-Type': 'image/png', 'x-amz-meta': 'a' });
    const confirm = api.calls.find((c) => c.method === 'PUT' && c.url.pathname.endsWith('/avatar'))!;
    expect(confirm.url.pathname).toBe(`/api/v1/orgs/orbit/projects/${ID}/avatar`);
    expect(confirm.body).toEqual({ uploadKey: 'org/o1/avatars/project/p/k1' });
    expect(await screen.findByRole('button', { name: 'Replace logo' })).toBeInTheDocument();
    // The settings preview names the project; the header logo beside the h1 is decorative.
    const imgs = document.querySelectorAll('img');
    expect(imgs).toHaveLength(2);
    const alts = [...imgs].map((i) => i.getAttribute('alt'));
    expect(alts.sort()).toEqual(['', 'Alpha launch']);
    for (const i of imgs) expect(i).toHaveAttribute('src', URL1);
  });

  it('refuses an SVG or a big file before any call', async () => {
    const { api } = server(proj());
    const user = userEvent.setup({ applyAccept: false });
    renderApp(`/orbit/projects/${ID}`);
    const input = await screen.findByTestId('logo-file');
    await user.upload(input, new File(['<svg/>'], 'a.svg', { type: 'image/svg+xml' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Use a PNG, JPEG, GIF or WebP image of at most 2 MiB.',
    );
    await user.upload(
      input,
      new File([new Uint8Array(2 * 1024 * 1024 + 1)], 'big.png', { type: 'image/png' }),
    );
    expect(screen.getByRole('alert')).toHaveTextContent('at most 2 MiB');
    expect(api.calls.some((c) => c.url.pathname.endsWith('/avatar/prepare'))).toBe(false);
  });

  it('accepts a file of exactly 2 MiB', async () => {
    const { api } = server(proj());
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    await user.upload(
      await screen.findByTestId('logo-file'),
      new File([new Uint8Array(2 * 1024 * 1024)], 'ok.png', { type: 'image/png' }),
    );
    await waitFor(() => expect(api.calls.some((c) => c.url.pathname.endsWith('/avatar/prepare'))).toBe(true));
  });

  it('shows a fixed sentence when the server refuses', async () => {
    server(proj(), {
      'POST /api/v1/orgs/:org/projects/:project/avatar/prepare': () => ({
        status: 403,
        json: { error: 'RAW', code: 'forbidden' },
      }),
    });
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    await user.upload(await screen.findByTestId('logo-file'), png());
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('You need write access to change the logo.');
    expect(alert).not.toHaveTextContent('RAW');
  });

  it('removes the logo with DELETE on the project route', async () => {
    const { api } = server(proj({ avatarUrl: URL1 }));
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    await user.click(await screen.findByRole('button', { name: 'Remove logo' }));
    expect(await screen.findByText('Logo removed.')).toBeInTheDocument();
    const del = api.calls.find((c) => c.method === 'DELETE')!;
    expect(del.url.pathname).toBe(`/api/v1/orgs/orbit/projects/${ID}/avatar`);
    expect(await screen.findByRole('button', { name: 'Upload logo' })).toBeInTheDocument();
    expect(document.querySelectorAll('img')).toHaveLength(0);
  });

  it('shows the letter tile with no logo, and when the image fails to load', async () => {
    server(proj({ avatarUrl: URL1 }));
    renderApp(`/orbit/projects/${ID}`);
    await screen.findByRole('heading', { name: 'Alpha launch' });
    const imgs = await waitFor(() => {
      const l = document.querySelectorAll('img');
      expect(l.length).toBeGreaterThan(0);
      return l;
    });
    for (const i of [...imgs]) i.dispatchEvent(new Event('error'));
    await waitFor(() => expect(document.querySelectorAll('img')).toHaveLength(0));
    expect(document.querySelectorAll('[data-logo="letter"]').length).toBeGreaterThan(0);
  });

  it('the list shows each logo with an empty alt and refreshes after a change on the page', async () => {
    server(proj({ avatarUrl: URL1 }));
    renderApp('/orbit/projects');
    const list = await screen.findByRole('table', { name: 'Projects' });
    const img = within(list).getByRole('presentation', { hidden: true });
    expect(img).toHaveAttribute('src', URL1);
    expect(img).toHaveAttribute('alt', '');
  });

  it('after a change the list and the detail caches carry the new logo without a reload', async () => {
    const { api } = server(proj());
    const user = userEvent.setup();
    renderApp('/orbit/projects');
    const list = await screen.findByRole('table', { name: 'Projects' });
    expect(within(list).queryByRole('presentation', { hidden: true })).toBeNull();
    await user.click(within(list).getByRole('link', { name: 'Alpha launch' }));
    await user.upload(await screen.findByTestId('logo-file'), png());
    await screen.findByText('Logo saved.');
    await user.click(
      within(screen.getByRole('navigation', { name: 'Breadcrumb' })).getByRole('link', { name: 'Projects' }),
    );
    const list2 = await screen.findByRole('table', { name: 'Projects' });
    await waitFor(() =>
      expect(within(list2).getByRole('presentation', { hidden: true })).toHaveAttribute('src', URL1),
    );
    expect(api.calls.length).toBeGreaterThan(0);
  });

  it('clears the saved status when the project changes', async () => {
    const { api } = server(proj());
    const user = userEvent.setup();
    // Load a page of the app once, so the session and CSRF value are in place; then drop it.
    renderApp(`/orbit/projects/${ID}`).unmount();
    await waitFor(() => expect(api.calls.some((c) => c.url.pathname.endsWith('/auth/session'))).toBe(true));
    const client = testClient();
    const wrap = (p: Project) => (
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/orbit/x']}>
          <OrgProvider slug="orbit">
            <ProjectLogoField project={p} />
          </OrgProvider>
        </MemoryRouter>
      </QueryClientProvider>
    );
    const { rerender } = render(wrap(proj()));
    await user.upload(await screen.findByTestId('logo-file'), png());
    expect(await screen.findByText('Logo saved.')).toBeInTheDocument();
    rerender(wrap(proj({ id: '01a00000-0000-7000-8000-000000000002', name: 'Beta' })));
    expect(screen.queryByText('Logo saved.')).toBeNull();
  });
});
