import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-200: Settings, Agent guidance. The requests are the contract: PUT {team, text}, DELETE ?team=.

interface Entry {
  team: string | null;
  text: string;
  updatedAt: string;
}

function server(items: Entry[], routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'admin') {
  const cur = { items };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
    'GET /api/v1/orgs/:org/agent-guidance': () => ({ json: { items: cur.items } }),
    'PUT /api/v1/orgs/:org/agent-guidance': (r) => {
      const b = r.body as { team: string | null; text: string };
      const e = { team: b.team, text: b.text, updatedAt: new Date().toISOString() };
      cur.items = [...cur.items.filter((i) => i.team !== b.team), e];
      return { json: e };
    },
    'DELETE /api/v1/orgs/:org/agent-guidance': (r) => {
      cur.items = cur.items.filter((i) => i.team !== (r.url.searchParams.get('team') ?? null));
      return { status: 204 };
    },
    ...routes,
  });
  return api;
}

const orgEntry: Entry = { team: null, text: 'Be brief.', updatedAt: '2026-10-31T10:00:00Z' };

describe('Agent guidance page (LAUN-200)', () => {
  it('shows the saved org text and an empty editor per team', async () => {
    server([orgEntry]);
    renderApp('/orbit/settings/agent-guidance');
    expect(await screen.findByRole('heading', { name: 'Agent guidance' })).toBeInTheDocument();
    expect(await screen.findByLabelText('Organisation guidance')).toHaveValue('Be brief.');
    expect(screen.getByLabelText(`${team.key} guidance`)).toHaveValue('');
    expect(screen.getByText('Markdown. 9 of 8192 bytes.')).toBeInTheDocument();
  });

  it('saves the org text with PUT {team: null, text} (trimmed)', async () => {
    const api = server([]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    const box = await screen.findByLabelText('Organisation guidance');
    await user.type(box, '  Use ticket keys.  ');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    const put = api.calls.find((c) => c.method === 'PUT')!;
    expect(put.url.pathname).toBe('/api/v1/orgs/orbit/agent-guidance');
    expect(put.body).toEqual({ team: null, text: 'Use ticket keys.' });
  });

  it('saves a team text with the team key', async () => {
    const api = server([]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    await user.type(await screen.findByLabelText(`${team.key} guidance`), 'Run make test.');
    await user.click(
      screen.getByRole('form', { name: `${team.key} guidance form` }).querySelector('button[type=submit]')!,
    );
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PUT')?.body).toEqual({
        team: team.key,
        text: 'Run make test.',
      }),
    );
  });

  it('Remove sends DELETE with ?team= for a team and none for the org', async () => {
    const api = server([orgEntry, { team: team.key, text: 'T', updatedAt: '2026-10-31T10:00:00Z' }]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    await screen.findByLabelText('Organisation guidance');
    const removes = screen.getAllByRole('button', { name: 'Remove' });
    await user.click(removes[1]!);
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(1));
    expect(api.calls.find((c) => c.method === 'DELETE')!.url.search).toBe(`?team=${team.key}`);
    await user.click(screen.getAllByRole('button', { name: 'Remove' })[0]!);
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(2));
    expect(api.calls.filter((c) => c.method === 'DELETE')[1]!.url.search).toBe('');
  });

  it('refuses empty, unchanged and over-size text in the page, with no request', async () => {
    const api = server([orgEntry]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    const box = await screen.findByLabelText('Organisation guidance');
    const save = screen.getAllByRole('button', { name: 'Save' })[0]!;
    await user.click(save);
    expect(await screen.findByRole('alert')).toHaveTextContent('Nothing changed.');
    await user.clear(box);
    await user.click(save);
    expect(await screen.findByRole('alert')).toHaveTextContent('Write some text, or use Remove.');
    // 2731 euro signs are 8193 bytes (3 bytes each), though only 2731 characters.
    await user.click(box);
    await user.paste('€'.repeat(2731));
    await user.click(save);
    expect(await screen.findByRole('alert')).toHaveTextContent('over 8192 bytes');
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
  });

  it('a 400 shows a fixed sentence, never the server text', async () => {
    server([], {
      'PUT /api/v1/orgs/:org/agent-guidance': () => ({
        status: 400,
        json: { error: 'raw server words', code: 'x' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    await user.type(await screen.findByLabelText('Organisation guidance'), 'x');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('The server did not accept this text.');
    expect(alert).not.toHaveTextContent('raw server words');
  });

  it('a failed Remove says it could not remove; a failed Save says save', async () => {
    server([orgEntry], {
      'DELETE /api/v1/orgs/:org/agent-guidance': () => ({ status: 500, json: { error: 'raw', code: 'x' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    await screen.findByLabelText('Organisation guidance');
    await user.click(screen.getAllByRole('button', { name: 'Remove' })[0]!);
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not remove the guidance. Try again.');
  });

  it('the 400 sentence names only what the server refuses', async () => {
    server([], {
      'PUT /api/v1/orgs/:org/agent-guidance': () => ({ status: 400, json: { error: 'raw', code: 'x' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    await user.type(await screen.findByLabelText('Organisation guidance'), 'x');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    const a = await screen.findByRole('alert');
    expect(a).toHaveTextContent('with no bidirectional or zero-width characters.');
    expect(a).not.toHaveTextContent('control,');
  });

  it('editing the text clears a failed-save alert', async () => {
    server([], {
      'PUT /api/v1/orgs/:org/agent-guidance': () => ({ status: 500, json: { error: 'raw', code: 'x' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    const box = await screen.findByLabelText('Organisation guidance');
    await user.type(box, 'x');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not save the guidance.');
    await user.type(box, 'y');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('only one card shows Saved. at a time', async () => {
    server([]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/agent-guidance');
    const org = await screen.findByLabelText('Organisation guidance');
    await user.type(org, 'a');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    // Starting to edit another card clears the first card's status.
    await user.type(screen.getByLabelText(`${team.key} guidance`), 'b');
    await waitFor(() => expect(screen.queryByText('Saved.')).not.toBeInTheDocument());
    await user.click(screen.getAllByRole('button', { name: 'Save' })[1]!);
    expect(await screen.findAllByText('Saved.')).toHaveLength(1);
  });

  it('a member cannot open the page', async () => {
    server([], {}, 'member');
    renderApp('/orbit/settings/agent-guidance');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(screen.queryByLabelText('Organisation guidance')).not.toBeInTheDocument();
  });
});
