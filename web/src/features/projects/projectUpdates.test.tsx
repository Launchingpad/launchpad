import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { projectOf, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';

const ID = '01a00000-0000-7000-8000-000000000001';

const project = (over: Partial<Project> = {}): Project =>
  projectOf({
    labels: [],
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
    version: 1,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    startedAt: null,
    completedAt: null,
    canceledAt: null,
    archivedAt: null,
    updateSchedule: { frequency: 'none', weekday: 5, hour: 9 },
    avatarUrl: null,
    emoji: null,
    issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
    ...over,
  });

const upd = (id: string, health: string, body: string) => ({
  id,
  projectId: ID,
  author: { id: 'u1', username: 'chen', name: 'Chen' },
  health,
  body,
  createdAt: '2026-10-30T10:00:00Z',
});

function server(routes = {}, p: Project = project()) {
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: p }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      ...routes,
    },
  });
}

// LAUN-196: the Updates section of the project page.
describe('Project updates', () => {
  it('lists updates with the health as a word and the author', async () => {
    server({
      'GET /api/v1/orgs/:org/projects/:project/updates': () => ({
        json: page([upd('b', 'at_risk', 'Slipping a week'), upd('a', 'on_track', '')]),
      }),
    });
    renderApp(`/orbit/projects/${ID}`);
    const sec = await screen.findByRole('region', { name: 'Updates' });
    expect(await within(sec).findByText('At risk', { selector: 'span' })).toBeInTheDocument();
    expect(within(sec).getByText('On track', { selector: 'span' })).toBeInTheDocument();
    expect(within(sec).getByText('Slipping a week')).toBeInTheDocument();
  });

  it('shows an empty state', async () => {
    server({ 'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }) });
    renderApp(`/orbit/projects/${ID}`);
    expect(await screen.findByText('No updates yet.')).toBeInTheDocument();
  });

  it('posts the chosen health and the typed body, then clears the box and reloads', async () => {
    let list = [] as ReturnType<typeof upd>[];
    const api = server({
      'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page(list) }),
      'POST /api/v1/orgs/:org/projects/:project/updates': (r: MockRequest) => {
        const b = r.body as { health: string; body: string };
        list = [upd('n', b.health, b.body)];
        return { status: 201, json: list[0] };
      },
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Post an update' });
    await userEvent.selectOptions(within(form).getByLabelText('Health'), 'off_track');
    await userEvent.type(within(form).getByLabelText('Optional text'), 'Blocked on infra');
    await userEvent.click(within(form).getByRole('button', { name: 'Post update' }));
    expect(await screen.findByText('Off track', { selector: 'span' })).toBeInTheDocument();
    const posts = callsTo(api, 'POST', '/updates');
    expect(posts).toHaveLength(1);
    expect(posts[0]!.url.pathname).toBe(`/api/v1/orgs/orbit/projects/${ID}/updates`);
    expect(posts[0]!.body).toEqual({ health: 'off_track', body: 'Blocked on infra' });
    expect(within(form).getByLabelText('Optional text')).toHaveValue('');
  });

  it('keeps the text and shows an error when the post fails', async () => {
    server({
      'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }),
      'POST /api/v1/orgs/:org/projects/:project/updates': () => ({
        status: 403,
        json: { error: 'no', code: 'forbidden' },
      }),
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Post an update' });
    await userEvent.type(within(form).getByLabelText('Optional text'), 'keep me');
    await userEvent.click(within(form).getByRole('button', { name: 'Post update' }));
    expect(await within(form).findByRole('alert')).toBeInTheDocument();
    expect(within(form).getByLabelText('Optional text')).toHaveValue('keep me');
  });

  it('has no composer on an archived project', async () => {
    server(
      { 'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }) },
      project({ archivedAt: '2026-10-02T00:00:00Z' }),
    );
    renderApp(`/orbit/projects/${ID}`);
    await screen.findByText('No updates yet.');
    expect(screen.queryByRole('form', { name: 'Post an update' })).toBeNull();
  });
});

// LAUN-196 part B: the update reminder schedule on the project page.
describe('Update reminder schedule', () => {
  const updatesRoute = { 'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }) };

  it('sends the frequency, day and hour that were picked, and nothing else', async () => {
    const api = server({
      ...updatesRoute,
      'PATCH /api/v1/orgs/:org/projects/:project': (r: MockRequest) => ({
        json: project({
          updateSchedule: (r.body as { updateSchedule: Project['updateSchedule'] }).updateSchedule,
        }),
      }),
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Update reminders' });
    const save = within(form).getByRole('button', { name: 'Save reminders' });
    expect(save).toBeDisabled();
    expect(within(form).queryByLabelText('Day')).toBeNull(); // frequency none: no day, no time
    await userEvent.selectOptions(within(form).getByLabelText('Remind the lead'), 'biweekly');
    await userEvent.selectOptions(within(form).getByLabelText('Day'), '1');
    await userEvent.selectOptions(within(form).getByLabelText(/^Time/), '16');
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await waitFor(() => expect(callsTo(api, 'PATCH', `/projects/${ID}`)).toHaveLength(1));
    expect(callsTo(api, 'PATCH', `/projects/${ID}`)[0]!.body).toEqual({
      updateSchedule: { frequency: 'biweekly', weekday: 1, hour: 16 },
    });
  });

  it('shows the saved schedule and keeps Save off until something changes', async () => {
    server(updatesRoute, project({ updateSchedule: { frequency: 'weekly', weekday: 5, hour: 9 } }));
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Update reminders' });
    expect(within(form).getByLabelText('Remind the lead')).toHaveValue('weekly');
    expect(within(form).getByLabelText('Day')).toHaveValue('5');
    expect(within(form).getByLabelText(/^Time/)).toHaveValue('9');
    expect(within(form).getByRole('button', { name: 'Save reminders' })).toBeDisabled();
  });

  it('says so when the save fails', async () => {
    server({
      ...updatesRoute,
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({
        status: 400,
        json: { error: 'bad', code: 'validation_failed' },
      }),
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Update reminders' });
    await userEvent.selectOptions(within(form).getByLabelText('Remind the lead'), 'weekly');
    await userEvent.click(within(form).getByRole('button', { name: 'Save reminders' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('Could not save the reminder schedule.');
  });
});

// LAUN-196 review fixes: one post per attempt, honest wording, the server's sentence.
describe('Update composer (review fixes)', () => {
  const okList = { 'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }) };

  it('two synchronous submits make one POST, with an Idempotency-Key', async () => {
    let n = 0;
    const api = server({
      ...okList,
      'POST /api/v1/orgs/:org/projects/:project/updates': async () => {
        n += 1;
        await new Promise((r) => setTimeout(r, 30));
        return { status: 201, json: upd('n', 'on_track', '') };
      },
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Post an update' });
    fireEvent.submit(form);
    fireEvent.submit(form);
    await waitFor(() => expect(n).toBe(1));
    await new Promise((r) => setTimeout(r, 80));
    const posts = callsTo(api, 'POST', '/updates');
    expect(posts).toHaveLength(1);
    expect(posts[0]!.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
  });

  it('keeps the key for a retry of the same draft and makes a new one after a change or a success', async () => {
    let fail = true;
    const api = server({
      ...okList,
      'POST /api/v1/orgs/:org/projects/:project/updates': () =>
        fail
          ? { status: 500, json: { error: 'down', code: 'internal' } }
          : { status: 201, json: upd('n', 'on_track', 'same') },
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Post an update' });
    const send = () => userEvent.click(within(form).getByRole('button', { name: 'Post update' }));
    await userEvent.type(within(form).getByLabelText('Optional text'), 'same');
    await send();
    await waitFor(() => expect(callsTo(api, 'POST', '/updates')).toHaveLength(1));
    await within(form).findByRole('alert');
    fail = false;
    await send(); // same draft: same key
    await waitFor(() => expect(callsTo(api, 'POST', '/updates')).toHaveLength(2));
    const [a, b] = callsTo(api, 'POST', '/updates').map((c) => c.headers.get('Idempotency-Key'));
    expect(b).toBe(a);
    await waitFor(() => expect(within(form).getByLabelText('Optional text')).toHaveValue(''));
    await userEvent.type(within(form).getByLabelText('Optional text'), 'next');
    await send(); // a new draft after a success: a new key
    await waitFor(() => expect(callsTo(api, 'POST', '/updates')).toHaveLength(3));
    expect(callsTo(api, 'POST', '/updates')[2]!.headers.get('Idempotency-Key')).not.toBe(a);
  });

  it("shows the server's sentence for a 400 and says plain text and optional", async () => {
    server({
      ...okList,
      'POST /api/v1/orgs/:org/projects/:project/updates': () => ({
        status: 400,
        json: { error: 'body must be at most 50000 characters', code: 'validation_failed' },
      }),
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Post an update' });
    expect(within(form).getByPlaceholderText('Plain text for now')).toBeInTheDocument();
    await userEvent.click(within(form).getByRole('button', { name: 'Post update' })); // health-only is allowed
    expect(await within(form).findByRole('alert')).toHaveTextContent('body must be at most 50000 characters');
  });

  it('names the organisation time zone beside the hour', async () => {
    server({
      ...okList,
      'GET /api/v1/orgs/:org': () => ({
        json: {
          id: 'o1',
          slug: 'orbit',
          name: 'Orbit',
          role: 'admin',
          timeZone: 'Europe/London',
          defaultTeam: null,
          avatarUrl: null,
          createdAt: '',
        },
      }),
    });
    renderApp(`/orbit/projects/${ID}`);
    const form = await screen.findByRole('form', { name: 'Update reminders' });
    await userEvent.selectOptions(within(form).getByLabelText('Remind the lead'), 'weekly');
    expect(
      await within(form).findByLabelText('Time (organisation time zone: Europe/London)'),
    ).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Reminder schedule' })).toBeInTheDocument();
  });
});
