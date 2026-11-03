import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { baseRoutes, makeIssue, mockApi } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { STORAGE_PREFIX, storageKey } from './viewPrefs';

// LAUN-109: the layout and the sort come back when the person returns to the same place.

const stored = () => {
  const k = Object.keys(localStorage).find((x) => x.startsWith(STORAGE_PREFIX));
  return k ? (JSON.parse(localStorage.getItem(k)!) as Record<string, { view: string; sort: string }>) : {};
};
const sortSelect = () => screen.getByRole('combobox', { name: /sort by/i }) as HTMLSelectElement;
const boardBtn = () => screen.getByRole('button', { name: 'Board view' });
const listBtn = () => screen.getByRole('button', { name: 'List view' });

beforeEach(() => {
  localStorage.clear();
  document.cookie = 'odt_csrf=csrf-test; path=/';
  mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [makeIssue(1)], nextCursor: null } }),
  });
});
afterEach(() => vi.restoreAllMocks());

describe('the view is remembered per place (LAUN-109)', () => {
  it('a team board is still a board, with its sort, after the person leaves and comes back', async () => {
    const user = userEvent.setup();
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    await user.selectOptions(sortSelect(), 'priority');
    await waitFor(() => expect(stored()['team:orb']).toMatchObject({ view: 'board', sort: 'priority' }));
    first.unmount();

    renderApp('/orbit/team/ORB'); // no ?view in the address
    await screen.findByRole('button', { name: 'Board view' });
    expect(boardBtn()).toHaveAttribute('aria-pressed', 'true');
    expect(sortSelect().value).toBe('priority');
    expect(currentLocation.search).toBe(''); // nothing was written into the address
  });

  it('another place keeps its own choice', async () => {
    const user = userEvent.setup();
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    first.unmount();
    renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'List view' });
    expect(listBtn()).toHaveAttribute('aria-pressed', 'true');
    expect(sortSelect().value).toBe('updated');
  });

  it('an explicit address wins and does not change the stored choice', async () => {
    const user = userEvent.setup();
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    await waitFor(() => expect(stored()['team:orb']).toMatchObject({ view: 'board' }));
    first.unmount();

    const second = renderApp('/orbit/team/ORB?view=list&sort=created'); // a shared link
    await screen.findByRole('button', { name: 'List view' });
    expect(listBtn()).toHaveAttribute('aria-pressed', 'true');
    expect(sortSelect().value).toBe('created');
    expect(stored()['team:orb']).toMatchObject({ view: 'board' });
    second.unmount();

    renderApp('/orbit/team/ORB'); // the own choice is still there
    await screen.findByRole('button', { name: 'Board view' });
    expect(boardBtn()).toHaveAttribute('aria-pressed', 'true');
  });

  it('Reset view forgets the choice and shows the defaults; the button hides', async () => {
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull();
    await user.click(boardBtn());
    await user.click(await screen.findByRole('button', { name: 'Reset view' }));
    await waitFor(() => expect(listBtn()).toHaveAttribute('aria-pressed', 'true'));
    expect(sortSelect().value).toBe('updated');
    expect(stored()['team:orb']).toBeUndefined();
    expect(currentLocation.search).toBe('');
    expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull();
  });

  it('works when storage throws', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('denied');
    });
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    expect(boardBtn()).toHaveAttribute('aria-pressed', 'true');
    expect(currentLocation.search).toContain('view=board'); // the address still carries the choice
  });

  it('a stale stored value is ignored', async () => {
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    const k = Object.keys(localStorage)[0] ?? `${STORAGE_PREFIX}orbit:anon`;
    localStorage.setItem(k, JSON.stringify({ 'team:orb': { view: 'kanban', sort: 'x', at: 1 } }));
    window.dispatchEvent(new StorageEvent('storage', { key: null }));
    await waitFor(() => expect(listBtn()).toHaveAttribute('aria-pressed', 'true'));
    expect(sortSelect().value).toBe('updated');
  });

  it('changing only a filter stores nothing and shows no Reset view', async () => {
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.type(screen.getByRole('searchbox', { name: /search issues/i }), 'bug');
    await waitFor(() => expect(currentLocation.search).toContain('q=bug'));
    expect(Object.keys(stored())).toEqual([]);
    expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull();
  });

  it('a filter change on an explicit ?view=board link leaves the stored choice alone', async () => {
    const user = userEvent.setup();
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(listBtn());
    await user.selectOptions(sortSelect(), 'created');
    await waitFor(() => expect(stored()['team:orb']).toMatchObject({ view: 'list', sort: 'created' }));
    first.unmount();
    renderApp('/orbit/team/ORB?view=board');
    await screen.findByRole('button', { name: 'Board view' });
    await user.type(screen.getByRole('searchbox', { name: /search issues/i }), 'bug');
    await waitFor(() => expect(currentLocation.search).toContain('q=bug'));
    expect(stored()['team:orb']).toMatchObject({ view: 'list', sort: 'created' });
  });

  it('ignores entries stored for another org or another user', async () => {
    const other = JSON.stringify({ 'team:orb': { view: 'board', sort: 'priority', at: 1 } });
    for (const k of ['orbit:u2', 'other:u1', 'orbit:anon'])
      localStorage.setItem(`${STORAGE_PREFIX}${k}`, other);
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'List view' });
    expect(listBtn()).toHaveAttribute('aria-pressed', 'true');
    expect(sortSelect().value).toBe('updated');
    expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull();
  });

  // LAUN-142
  it('Manual from the board button does not stick to the list', async () => {
    const user = userEvent.setup();
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    await waitFor(() => expect(stored()['team:orb']).toMatchObject({ view: 'board', sort: 'manual' }));
    await user.click(listBtn());
    await waitFor(() => expect(listBtn()).toHaveAttribute('aria-pressed', 'true'));
    expect(sortSelect().value).toBe('updated');
    first.unmount();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'List view' });
    expect(sortSelect().value).toBe('updated');
  });

  it('a sort the person chose stays when the layout changes', async () => {
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.selectOptions(sortSelect(), 'priority');
    await user.click(boardBtn());
    await user.click(listBtn());
    await waitFor(() => expect(stored()['team:orb']).toMatchObject({ view: 'list', sort: 'priority' }));
  });

  it('board and back to the default list leaves no entry and no Reset view', async () => {
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    await user.click(listBtn());
    await waitFor(() => expect(listBtn()).toHaveAttribute('aria-pressed', 'true'));
    expect(stored()['team:orb']).toBeUndefined();
    expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull();
  });

  it('an old entry equal to the defaults shows no Reset view', async () => {
    const key = storageKey('orbit', 'u1'); // the key the page reads: the mock session user is u1
    // Control: a non-default entry under the same key shows the button, so the key is read.
    localStorage.setItem(key, JSON.stringify({ 'team:orb': { view: 'list', sort: 'priority', at: 1 } }));
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    expect(await screen.findByRole('button', { name: 'Reset view' })).toBeInTheDocument();
    first.unmount();
    localStorage.setItem(key, JSON.stringify({ 'team:orb': { view: 'list', sort: 'updated', at: 1 } }));
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull();
  });
});

describe('the view follows the person to another device (LAUN-141)', () => {
  it('a board chosen elsewhere shows here, and a choice made here is sent', async () => {
    const user = userEvent.setup();
    const puts: { url: string; body: unknown }[] = [];
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [makeIssue(1)], nextCursor: null } }),
      'GET /api/v1/orgs/:org/me/views': () => ({
        json: { items: [{ place: 'team:orb', view: 'board', sort: 'priority', at: Date.now() }] },
      }),
      'PUT /api/v1/orgs/:org/me/views/:place': (req) => {
        puts.push({ url: req.url.pathname, body: req.body });
        return { json: {} };
      },
    });
    renderApp('/orbit/team/ORB'); // this device has nothing stored
    await waitFor(() => expect(boardBtn()).toHaveAttribute('aria-pressed', 'true'));
    expect(sortSelect().value).toBe('priority');
    expect(stored()['team:orb']).toMatchObject({ view: 'board', sort: 'priority' });

    await user.click(listBtn());
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]!.url).toBe('/api/v1/orgs/orbit/me/views/team%3Aorb');
    expect(puts[0]!.body).toMatchObject({ view: 'list', sort: 'priority' }); // a chosen sort stays (LAUN-142)
  });

  it('works with the server unreachable: the local choice still applies', async () => {
    const user = userEvent.setup();
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [makeIssue(1)], nextCursor: null } }),
      'GET /api/v1/orgs/:org/me/views': () => ({ status: 503, json: { error: 'x', code: 'internal' } }),
      'PUT /api/v1/orgs/:org/me/views/:place': () => ({
        status: 503,
        json: { error: 'x', code: 'internal' },
      }),
    });
    const first = renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await user.click(boardBtn());
    await waitFor(() => expect(stored()['team:orb']).toMatchObject({ view: 'board' }));
    first.unmount();
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    expect(boardBtn()).toHaveAttribute('aria-pressed', 'true');
  });
});

describe('first load with a full store (LAUN-141)', () => {
  it('sends what only this device has even when it cannot write the merge back', async () => {
    const puts: string[] = [];
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({ 'team:orb': { view: 'board', sort: 'manual', at: 5 } }),
    );
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [makeIssue(1)], nextCursor: null } }),
      'PUT /api/v1/orgs/:org/me/views/:place': (req) => {
        puts.push(req.url.pathname);
        return { json: {} };
      },
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('full', 'QuotaExceededError');
    });
    renderApp('/orbit/team/ORB');
    await waitFor(() => expect(puts).toEqual(['/api/v1/orgs/orbit/me/views/team%3Aorb']));
  });
});

describe('Reset view and stale hidden names (LAUN-163)', () => {
  it('a hidden name that is no state shows no Reset view and no "(changed)"', async () => {
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({
        'team:orb': { view: 'list', sort: 'updated', hideEmpty: false, hidden: ['Gone'], at: 1 },
      }),
    );
    renderApp('/orbit/team/ORB');
    await screen.findByRole('button', { name: 'Board view' });
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Reset view' })).toBeNull());
  });

  it('a hidden name that is a state keeps Reset view', async () => {
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({
        'team:orb': { view: 'list', sort: 'updated', hideEmpty: false, hidden: ['Done'], at: 1 },
      }),
    );
    renderApp('/orbit/team/ORB');
    expect(await screen.findByRole('button', { name: 'Reset view' })).toBeInTheDocument();
  });
});
