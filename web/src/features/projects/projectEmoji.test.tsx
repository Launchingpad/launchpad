import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { joinServer, page } from '../../test/join';
import { projectOf, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { Logo } from '../../ui/Logo';
import { COMMON_EMOJI, EMOJI_MAX_BYTES, looksLikeOneEmoji } from './ProjectEmojiField';

// LAUN-232 (decisions-T41): a project emoji. The icon order is logo, then emoji, then letter.

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

function server(start: Project, extra: Record<string, Handler> = {}) {
  const cur = { v: start };
  const api = joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page([cur.v]) }),
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: cur.v }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/projects/:project/updates': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      'PATCH /api/v1/orgs/:org/projects/:project': (r) => {
        const body = r.body as { emoji?: string | null };
        cur.v = { ...cur.v, emoji: body.emoji || null, version: cur.v.version + 1 };
        return { json: cur.v };
      },
      ...extra,
    },
  });
  return { api, cur };
}

const patches = (api: ReturnType<typeof joinServer>) => api.calls.filter((c) => c.method === 'PATCH');
const headerTile = () =>
  screen
    .getByRole('heading', { level: 1, name: 'Alpha launch' })
    .parentElement!.querySelector('[data-logo]')!;

describe('Logo order (LAUN-232)', () => {
  it('shows the emoji when there is no image, and the letter when there is neither', () => {
    const { container, rerender } = render(<Logo name="Alpha" emoji="🚀" />);
    const tile = container.querySelector('[data-logo]')!;
    expect(tile).toHaveAttribute('data-logo', 'emoji');
    expect(screen.getByRole('img', { name: 'Alpha' })).toHaveTextContent('🚀');
    rerender(<Logo name="Alpha" emoji={null} />);
    expect(container.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'letter');
    expect(screen.getByRole('img', { name: 'Alpha' })).toHaveTextContent('A');
    rerender(<Logo name="Alpha" emoji="  " />);
    expect(container.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'letter');
  });

  it('the image wins over the emoji; a failed image falls back to the emoji', async () => {
    const { container } = render(<Logo name="Alpha" src={URL1} emoji="🚀" />);
    const img = container.querySelector('img')!;
    expect(img).toHaveAttribute('src', URL1);
    expect(container.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'image');
    expect(container).not.toHaveTextContent('🚀');
    img.dispatchEvent(new Event('error'));
    await waitFor(() => expect(container.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'emoji'));
    expect(container).toHaveTextContent('🚀');
  });

  it('a decorative emoji is hidden from a screen reader', () => {
    const { container } = render(<Logo name="Alpha" emoji="🚀" decorative />);
    expect(screen.queryByRole('img')).toBeNull();
    expect(container.querySelector('[aria-hidden="true"]')).toHaveTextContent('🚀');
  });
});

describe('looksLikeOneEmoji', () => {
  it('takes one emoji, ZWJ and skin tone sequences included', () => {
    for (const v of [
      '🚀',
      '👩‍💻',
      '🧑🏽‍💻',
      '👍🏿',
      '🏳️‍🌈',
      '🇬🇧',
      '1️⃣',
      '❤️',
      '👨‍👩‍👧',
      '👨‍👩‍👧‍👦',
      '👩🏽‍❤️‍👨🏿',
      '🏴󠁧󠁢󠁥󠁮󠁧󠁿',
      '🏴󠁧󠁢󠁳󠁣󠁴󠁿',
      '🏴󠁧󠁢󠁷󠁬󠁳󠁿',
      '\u2194\uFE0F',
      '\u2139\uFE0F',
      '\u25FB\uFE0F',
      '\u25FE',
      '\u2934\uFE0F',
      '#\uFE0F\u20E3',
    ])
      expect(looksLikeOneEmoji(v), v).toBe(true);
  });
  it('refuses text, many emoji, control characters and more than 32 bytes', () => {
    for (const v of [
      '',
      'a',
      'ok',
      '🚀🎉',
      '🚀a',
      '🚀\u0007',
      '❤️‍❤️‍❤️‍❤️',
      '👩🏽‍❤️‍💋‍👨🏿',
      '\u2318',
      '\u2588',
      '\u2800',
      '\uFFFD',
      '\uFFFC',
      '\u24D0\u200D\u24D1',
    ])
      expect(looksLikeOneEmoji(v), v).toBe(false);
  });
  it('every emoji of the grid passes the check and the byte limit', () => {
    for (const c of COMMON_EMOJI) {
      expect(looksLikeOneEmoji(c.emoji), c.name).toBe(true);
      expect(new TextEncoder().encode(c.emoji).length).toBeLessThanOrEqual(EMOJI_MAX_BYTES);
    }
    expect(new Set(COMMON_EMOJI.map((c) => c.name)).size).toBe(COMMON_EMOJI.length);
  });
});

describe('Project emoji picker (LAUN-232)', () => {
  it('a grid button sets the emoji; the header, the preview and the pressed state follow', async () => {
    const { api } = server(proj());
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    const section = await screen.findByRole('region', { name: 'Emoji' });
    expect(within(section).getByText(/No emoji is set/)).toBeInTheDocument();
    expect(within(section).queryByRole('button', { name: 'Remove emoji' })).toBeNull();
    await user.click(within(section).getByRole('button', { name: 'Rocket' }));
    expect(await within(section).findByText('Emoji saved.')).toBeInTheDocument();
    expect(patches(api).map((c) => c.body)).toEqual([{ emoji: '🚀' }]);
    await waitFor(() => expect(headerTile()).toHaveAttribute('data-logo', 'emoji'));
    expect(headerTile()).toHaveTextContent('🚀');
    expect(within(section).getByRole('button', { name: 'Rocket' })).toHaveAttribute('aria-pressed', 'true');
    expect(within(section).getByRole('button', { name: 'Star' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('pastes one emoji (a ZWJ sequence) and saves it', async () => {
    const { api } = server(proj());
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    const section = await screen.findByRole('region', { name: 'Emoji' });
    await user.type(within(section).getByLabelText('Or paste one emoji'), '🧑🏽‍💻');
    await user.click(within(section).getByRole('button', { name: 'Use emoji' }));
    expect(await within(section).findByText('Emoji saved.')).toBeInTheDocument();
    expect(patches(api).map((c) => c.body)).toEqual([{ emoji: '🧑🏽‍💻' }]);
    expect(within(section).getByLabelText('Or paste one emoji')).toHaveValue('');
  });

  it('refuses text or many emoji before any call', async () => {
    const { api } = server(proj());
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    const section = await screen.findByRole('region', { name: 'Emoji' });
    const input = within(section).getByLabelText('Or paste one emoji');
    await user.type(input, 'ab{Enter}');
    expect(await within(section).findByRole('alert')).toHaveTextContent('Enter exactly one emoji.');
    await user.clear(input);
    await user.type(input, '🚀🎉{Enter}');
    expect(within(section).getByRole('alert')).toHaveTextContent('Enter exactly one emoji.');
    expect(patches(api)).toHaveLength(0);
  });

  it('shows a fixed sentence when the server refuses', async () => {
    server(proj(), {
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({
        status: 400,
        json: { error: 'RAW', code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    const section = await screen.findByRole('region', { name: 'Emoji' });
    await user.click(within(section).getByRole('button', { name: 'Star' }));
    const alert = await within(section).findByRole('alert');
    expect(alert).toHaveTextContent('The server did not accept this emoji. Use one emoji.');
    expect(alert).not.toHaveTextContent('RAW');
  });

  it('removes the emoji with emoji null; the header shows the letter again', async () => {
    const { api } = server(proj({ emoji: '🎯' }));
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    const section = await screen.findByRole('region', { name: 'Emoji' });
    expect(within(section).getByText(/Current emoji/)).toHaveTextContent('🎯');
    await waitFor(() => expect(headerTile()).toHaveAttribute('data-logo', 'emoji'));
    await user.click(within(section).getByRole('button', { name: 'Remove emoji' }));
    expect(await within(section).findByText('Emoji removed.')).toBeInTheDocument();
    expect(patches(api).map((c) => c.body)).toEqual([{ emoji: null }]);
    await waitFor(() => expect(headerTile()).toHaveAttribute('data-logo', 'letter'));
  });

  it('the logo still shows first when both are set', async () => {
    server(proj({ emoji: '🎯', avatarUrl: URL1 }));
    renderApp(`/orbit/projects/${ID}`);
    await screen.findByRole('region', { name: 'Emoji' });
    expect(headerTile()).toHaveAttribute('data-logo', 'image');
  });

  it('the grid is one tab stop; arrow keys, Home and End move; Enter picks', async () => {
    const { api } = server(proj({ emoji: '⭐' }));
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID}`);
    const section = await screen.findByRole('region', { name: 'Emoji' });
    const grid = within(section).getByRole('group', { name: 'Common emoji' });
    const btns = within(grid).getAllByRole('button');
    expect(btns).toHaveLength(COMMON_EMOJI.length);
    // The pressed emoji (Star, the third) is the tab stop.
    expect(btns.filter((b) => b.tabIndex === 0)).toEqual([btns[2]]);
    btns[2]!.focus();
    await user.keyboard('{ArrowRight}');
    expect(btns[3]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(btns[11]).toHaveFocus();
    await user.keyboard('{ArrowUp}{ArrowLeft}');
    expect(btns[2]).toHaveFocus();
    await user.keyboard('{End}');
    expect(btns.at(-1)).toHaveFocus();
    await user.keyboard('{Home}');
    expect(btns[0]).toHaveFocus();
    expect(btns.filter((b) => b.tabIndex === 0)).toEqual([btns[0]]);
    await user.keyboard('{Enter}');
    await waitFor(() => expect(patches(api).map((c) => c.body)).toEqual([{ emoji: COMMON_EMOJI[0]!.emoji }]));
    // Tab leaves the grid for the text field.
    btns[0]!.focus();
    await user.tab();
    expect(within(section).getByLabelText('Or paste one emoji')).toHaveFocus();
  });

  it('the project list shows the emoji when there is no logo', async () => {
    server(proj({ emoji: '🌱' }));
    renderApp('/orbit/projects');
    const list = await screen.findByRole('table', { name: 'Projects' });
    const tile = list.querySelector('[data-logo]')!;
    expect(tile).toHaveAttribute('data-logo', 'emoji');
    expect(tile).toHaveTextContent('🌱');
    expect(tile.querySelector('[aria-hidden="true"]')).not.toBeNull();
  });
});
