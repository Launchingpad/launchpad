import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../api/types';
import { joinServer, page } from '../../test/join';
import { projectOf } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { VIEW_KEY } from './projectsView';

const proj = (n: number, name: string, over: Partial<Project> = {}): Project =>
  projectOf({
    id: `01a00000-0000-7000-8000-00000000000${n}`,
    name,
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
    createdAt: '',
    updatedAt: '',
    startedAt: null,
    completedAt: null,
    canceledAt: null,
    archivedAt: null,
    avatarUrl: null,
    issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
    ...over,
  }) as Project;

const list = [
  proj(1, 'Alpha', { targetDate: '2020-01-01', lead: { id: 'u1', username: 'chen', name: 'Chen' } }),
  proj(2, 'Beta', { state: 'planned', targetDate: '2099-01-01' }),
  proj(3, 'Gamma', { state: 'completed', teams: [{ id: 't2', key: 'IOS', name: 'iOS' }] }),
];
const product = {
  id: 'p1',
  slug: 'apps',
  name: 'Apps',
  description: '',
  icon: null,
  color: null,
  version: 1,
  teamKeys: ['IOS'],
  createdAt: '',
  updatedAt: '',
  archivedAt: null,
};

function server(products: unknown[] = []) {
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page(list) }),
      'GET /api/v1/orgs/:org/products': () => ({ json: page(products) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
    },
  });
}
const nameOf = (r: HTMLElement) => within(r).getAllByRole('link')[0]!.lastElementChild!.textContent;
const rowNames = () =>
  screen
    .getAllByRole('row')
    .filter((r) => within(r).queryAllByRole('link').length > 0)
    .map(nameOf);
const bar = () => within(screen.getByRole('toolbar', { name: 'Filters' }));

beforeEach(() => localStorage.clear());

describe('Projects table (LAUN-165)', () => {
  it('has a header row with the columns and real table semantics', async () => {
    server();
    renderApp('/orbit/projects');
    const table = await screen.findByRole('table', { name: 'Projects' });
    const heads = within(table)
      .getAllByRole('columnheader')
      .map((h) => h.textContent?.replace(/[↑↓]/g, ''));
    expect(heads).toEqual(['Name', 'Labels', 'Health', 'Lead', 'Target date', 'Status', 'Progress']);
    expect(
      within(table)
        .getAllByRole('rowheader')
        .map((h) => nameOf(h)),
    ).toEqual(['Alpha', 'Beta', 'Gamma']);
    // Alpha is overdue, so its health is Off track; Gamma is done and has none.
    const alpha = within(table).getAllByRole('row')[1]!;
    expect(within(alpha).getByText('Off track')).toBeInTheDocument();
  });

  it('sorts by a header click, flips on a second click and sets aria-sort', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    const nameHead = screen.getByRole('columnheader', { name: /Name/ });
    expect(screen.getByRole('columnheader', { name: /Status/ })).toHaveAttribute('aria-sort', 'ascending');
    expect(nameHead).not.toHaveAttribute('aria-sort');
    await userEvent.click(within(nameHead).getByRole('button'));
    expect(rowNames()).toEqual(['Alpha', 'Beta', 'Gamma']);
    expect(nameHead).toHaveAttribute('aria-sort', 'ascending');
    await userEvent.click(within(nameHead).getByRole('button'));
    expect(rowNames()).toEqual(['Gamma', 'Beta', 'Alpha']);
    expect(nameHead).toHaveAttribute('aria-sort', 'descending');
    expect(screen.getByRole('columnheader', { name: /Status/ })).not.toHaveAttribute('aria-sort');
  });

  it('is operable from the keyboard', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    const btn = within(screen.getByRole('columnheader', { name: /Target date/ })).getByRole('button');
    btn.focus();
    await userEvent.keyboard('{Enter}');
    expect(screen.getByRole('columnheader', { name: /Target date/ })).toHaveAttribute(
      'aria-sort',
      'ascending',
    );
    expect(rowNames()).toEqual(['Alpha', 'Beta', 'Gamma']);
    await userEvent.keyboard(' ');
    expect(screen.getByRole('columnheader', { name: /Target date/ })).toHaveAttribute(
      'aria-sort',
      'descending',
    );
    expect(rowNames()).toEqual(['Beta', 'Alpha', 'Gamma']);
  });

  it('filters by status and lead, shows the count, and clears', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(bar().getByRole('button', { name: 'Status' }));
    await userEvent.click(await screen.findByRole('option', { name: /Planned/ }));
    expect(rowNames()).toEqual(['Beta']);
    expect(screen.getByText('1 of 3')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Clear \(1\)/ }));
    expect(rowNames()).toHaveLength(3);
    await userEvent.click(bar().getByRole('button', { name: 'Lead' }));
    await userEvent.click(await screen.findByRole('option', { name: /Chen/ }));
    expect(rowNames()).toEqual(['Alpha']);
  });

  it('filters by team and by product', async () => {
    server([product]);
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(screen.getByRole('button', { name: 'Product' }));
    await userEvent.click(await screen.findByRole('option', { name: /Apps/ }));
    await waitFor(() => expect(rowNames()).toEqual(['Gamma']));
    await userEvent.click(screen.getByRole('button', { name: /Clear/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Team' }));
    await userEvent.click(await screen.findByRole('option', { name: /Orbit/ }));
    expect(rowNames().sort()).toEqual(['Alpha', 'Beta']);
  });

  it('says so when no project matches, and Clear filters brings them back', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.type(screen.getByRole('searchbox', { name: 'Search projects' }), 'zzz');
    expect(await screen.findByText('No projects match')).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    expect(await screen.findByRole('table', { name: 'Projects' })).toBeInTheDocument();
  });

  it('shows and hides columns from Display, Priority starts hidden', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    expect(screen.queryByRole('columnheader', { name: 'Priority' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Display' }));
    await userEvent.click(await screen.findByRole('option', { name: 'Priority' }));
    expect(screen.getByRole('columnheader', { name: 'Priority' })).toBeInTheDocument();
    await userEvent.click(
      within(screen.getByRole('listbox', { name: 'Display columns' })).getByRole('option', { name: 'Lead' }),
    );
    expect(screen.queryByRole('columnheader', { name: 'Lead' })).toBeNull();
    // Priority sits between Health and Target date whatever order it was switched on in.
    expect(screen.getAllByRole('columnheader').map((h) => h.textContent?.replace(/[↑↓]/g, ''))).toEqual([
      'Name',
      'Labels',
      'Health',
      'Priority',
      'Target date',
      'Status',
      'Progress',
    ]);
  });

  it('remembers sort, columns and filters, and starts from them', async () => {
    server();
    const { unmount } = renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(within(screen.getByRole('columnheader', { name: /Name/ })).getByRole('button'));
    await userEvent.click(bar().getByRole('button', { name: 'Status' }));
    await userEvent.click(await screen.findByRole('option', { name: /Planned/ }));
    const stored = JSON.parse(localStorage.getItem(VIEW_KEY)!);
    expect(stored).toMatchObject({ sort: 'name', dir: 'asc', filters: { states: ['ps-planned'] } });
    unmount();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    expect(rowNames()).toEqual(['Beta']);
    expect(screen.getByRole('columnheader', { name: /Name/ })).toHaveAttribute('aria-sort', 'ascending');
  });

  it('works when localStorage throws', async () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    try {
      server();
      renderApp('/orbit/projects');
      await screen.findByRole('table', { name: 'Projects' });
      await userEvent.click(within(screen.getByRole('columnheader', { name: /Name/ })).getByRole('button'));
      expect(rowNames()).toEqual(['Alpha', 'Beta', 'Gamma']);
    } finally {
      get.mockRestore();
      set.mockRestore();
    }
  });

  it('keeps New project and the group toggle working with the filter bar', async () => {
    server([product]);
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    expect(screen.getByRole('checkbox', { name: 'Group by product' })).toBeChecked();
    await userEvent.click(screen.getByRole('button', { name: 'New project' }));
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
  });

  it('marks a group header as a rowgroup heading and names the tbody by it', async () => {
    server([product]);
    renderApp('/orbit/projects');
    const table = await screen.findByRole('table', { name: 'Projects' });
    const head = within(table).getByRole('rowheader', { name: /Apps/ });
    expect(head).toHaveAttribute('scope', 'rowgroup');
    expect(head).toHaveAttribute('colspan');
    const group = within(table).getByRole('rowgroup', { name: /Apps/ });
    expect(within(group).getByRole('heading', { level: 2 })).toHaveTextContent('Apps');
    expect(nameOf(within(group).getAllByRole('row')[1]!)).toBe('Gamma');
  });

  it('Clear resets the typed search at once, so the debounce cannot bring it back', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    const box = screen.getByRole('searchbox', { name: 'Search projects' });
    await userEvent.click(bar().getByRole('button', { name: 'Lead' }));
    await userEvent.click(await screen.findByRole('option', { name: /Chen/ }));
    await userEvent.type(box, 'zz');
    await userEvent.click(bar().getByRole('button', { name: /Clear/ }));
    expect(box).toHaveValue('');
    await new Promise((r) => setTimeout(r, 400));
    expect(box).toHaveValue('');
    expect(rowNames()).toHaveLength(3);
  });

  it('Clear (2) counts lead and status together', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(bar().getByRole('button', { name: 'Lead' }));
    await userEvent.click(await screen.findByRole('option', { name: /Chen/ }));
    await userEvent.click(bar().getByRole('button', { name: /Lead: Chen/ }));
    await userEvent.click(bar().getByRole('button', { name: 'Status' }));
    await userEvent.click(await screen.findByRole('option', { name: /In progress/ }));
    expect(bar().getByRole('button', { name: 'Clear (2)' })).toBeInTheDocument();
  });

  it('labels every cell for a reader once the header row is hidden', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    const row = screen.getAllByRole('row')[1]!;
    const text = row.textContent ?? '';
    for (const l of ['Health:', 'Lead:', 'Status:', 'Progress:', 'Target date']) expect(text).toContain(l);
  });

  it('shows No priority in a hidden-by-default Priority column once switched on', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(screen.getByRole('button', { name: 'Display' }));
    await userEvent.click(await screen.findByRole('option', { name: 'Priority' }));
    expect(within(screen.getAllByRole('row')[1]!).getByText('No priority')).toBeInTheDocument();
  });

  it('matches a two-team project on either team and on a product holding either', async () => {
    const two = proj(4, 'Both', {
      teams: [
        { id: 'team1', key: 'ORB', name: 'Orbit' },
        { id: 't2', key: 'IOS', name: 'iOS' },
      ],
    });
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/projects': () => ({ json: page([...list, two]) }),
        'GET /api/v1/orgs/:org/products': () => ({ json: page([product]) }),
        'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      },
    });
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(bar().getByRole('button', { name: 'Product' }));
    await userEvent.click(await screen.findByRole('option', { name: /Apps/ }));
    await waitFor(() => expect(rowNames().sort()).toEqual(['Both', 'Gamma']));
    await userEvent.click(bar().getByRole('button', { name: /Clear/ }));
    await userEvent.click(bar().getByRole('button', { name: 'Team' }));
    await userEvent.click(await screen.findByRole('option', { name: /Orbit/ }));
    expect(rowNames().sort()).toEqual(['Alpha', 'Beta', 'Both']);
  });
});
