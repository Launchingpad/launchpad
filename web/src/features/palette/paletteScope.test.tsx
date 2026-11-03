import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, makeIssue, mockApi } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-114: from inside a team the palette searches that team; a ticket number and the
// "All teams" button search every team.
function setup() {
  return mockApi({
    ...baseRoutes([makeIssue(1)]),
    'GET /api/v1/orgs/:org/issues': (r) => {
      const q = r.url.searchParams.get('query');
      const team = r.url.searchParams.get('team');
      if (q === 'zzz' && team) return { json: { items: [], nextCursor: null } };
      if (q === 'zzz')
        return { json: { items: [makeIssue(9, { title: 'zzz elsewhere' })], nextCursor: null } };
      return { json: { items: [makeIssue(1)], nextCursor: null } };
    },
  });
}

/** The `team` param of every search call (those with a `query`) made for this text. */
const teamsFor = (calls: { url: URL }[], text: string) =>
  calls.filter((c) => c.url.searchParams.get('query') === text).map((c) => c.url.searchParams.get('team'));

async function openPalette(path: string) {
  const user = userEvent.setup();
  renderApp(path);
  await screen.findByText('Issue 1');
  await user.keyboard('{Meta>}k{/Meta}');
  const box = await screen.findByRole('combobox', { name: 'Search issues and commands' });
  return { user, box };
}

describe('palette search scope (LAUN-114)', () => {
  it('from a team page it sends the team and says so', async () => {
    const api = setup();
    const { user, box } = await openPalette('/orbit/team/ORB');
    expect(screen.getByText(/Searching ORB only/)).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveAttribute('aria-live', 'polite');
    expect(screen.getByRole('status')).toHaveTextContent(/Searching ORB only/);
    await user.type(box, 'zzz');
    await waitFor(() => expect(teamsFor(api.calls, 'zzz')).toContain('ORB'));
    expect(teamsFor(api.calls, 'zzz')).not.toContain(null);
  });

  it('from an issue page it uses the team of the identifier', async () => {
    const api = setup();
    const { user, box } = await openPalette('/orbit/issue/ORB-1');
    await user.type(box, 'zzz');
    await waitFor(() => expect(teamsFor(api.calls, 'zzz')).toContain('ORB'));
  });

  it('with no team context it searches every team and shows no toggle', async () => {
    const api = setup();
    const { user, box } = await openPalette('/orbit/issues');
    expect(screen.queryByRole('button', { name: 'All teams' })).toBeNull();
    await user.type(box, 'zzz');
    await waitFor(() => expect(teamsFor(api.calls, 'zzz').length).toBeGreaterThan(0));
    expect(teamsFor(api.calls, 'zzz')).toEqual([null]);
  });

  it('a ticket number is searched in every team even when scoped', async () => {
    const api = setup();
    const { user, box } = await openPalette('/orbit/team/ORB');
    await user.type(box, '113');
    await waitFor(() => expect(teamsFor(api.calls, '113').length).toBeGreaterThan(0));
    expect(teamsFor(api.calls, '113')).toEqual([null]);
    expect(screen.getByText(/ticket numbers search all teams/)).toBeInTheDocument();
  });

  it('the All teams button widens the search and Only ORB narrows it again', async () => {
    const api = setup();
    const { user, box } = await openPalette('/orbit/team/ORB');
    const toggle = screen.getByRole('button', { name: 'All teams' });
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    await user.click(toggle);
    expect(screen.getByText(/Searching all teams/)).toBeInTheDocument();
    // LAUN-114 review: a stable label with aria-pressed, never a changing label as well.
    expect(screen.getByRole('button', { name: 'All teams' })).toHaveAttribute('aria-pressed', 'true');
    expect(box).toHaveFocus();
    await user.type(box, 'zzz');
    expect(await screen.findByRole('option', { name: /zzz elsewhere/ })).toBeInTheDocument();
    expect(teamsFor(api.calls, 'zzz')).toEqual([null]);
    await user.click(screen.getByRole('button', { name: 'All teams' }));
    expect(screen.getByText(/Searching ORB only/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('option', { name: /zzz elsewhere/ })).toBeNull());
  });

  it('the empty state offers Search all teams', async () => {
    setup();
    const { user, box } = await openPalette('/orbit/team/ORB');
    await user.type(box, 'zzz');
    await waitFor(() => expect(screen.getByText(/No results/)).toHaveTextContent('in ORB'));
    await user.click(screen.getByRole('button', { name: 'Search all teams' }));
    expect(box).toHaveFocus();
    expect(await screen.findByRole('option', { name: /zzz elsewhere/ })).toBeInTheDocument();
  });
});
