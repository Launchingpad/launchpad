import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../api/types';
import { baseRoutes, makeIssue, mockApi, statusRef } from '../../test/mockApi';
import { currentLocation, renderApp, renderAt } from '../../test/render';
import { ProjectChip } from './ProjectChip';
import { markDragged, resetDragged } from './cardLink';
import { emptyFilters, parseFilters, toApiQuery, writeFilters, activeFilterCount } from './filters';

// LAUN-179: the project name on a ticket, and Project as a filter.

const PID = '01a00000-0000-7000-8000-000000000001';
const proj = (id: string, name: string): Project =>
  ({
    id,
    name,
    description: '',
    content: '',
    contentTruncated: false,
    state: 'started',
    status: statusRef('started'),
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
    updateSchedule: { frequency: 'none', weekday: 5, hour: 9 },
    avatarUrl: null,
    emoji: null,
    issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
  }) as Project;

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});

function setup() {
  const inProject = makeIssue(1, { title: 'In a project', projectId: PID });
  const loose = makeIssue(2, { title: 'No project here', projectId: null });
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/projects': () => ({
      json: {
        items: [
          { ...proj(PID, 'Web rebuild'), emoji: '🛠️' },
          proj('01a00000-0000-7000-8000-000000000002', 'Mobile'),
        ],
        nextCursor: null,
      },
    }),
    'GET /api/v1/orgs/:org/issues': (r) => {
      const p = r.url.searchParams.get('project');
      const items = p === PID ? [inProject] : p === 'none' ? [loose] : [inProject, loose];
      return { json: { items, nextCursor: null } };
    },
  });
}

const issueCalls = (api: ReturnType<typeof mockApi>) =>
  api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues'));

describe('project filter state', () => {
  it('round-trips through the URL and the API query', () => {
    const f = { ...emptyFilters, project: PID };
    expect(writeFilters(f).get('project')).toBe(PID);
    expect(parseFilters(writeFilters(f)).project).toBe(PID);
    expect(toApiQuery(f).project).toBe(PID);
    expect(toApiQuery({ ...emptyFilters, project: 'none' }).project).toBe('none');
    expect(toApiQuery(emptyFilters)).not.toHaveProperty('project');
    expect(activeFilterCount(f)).toBe(1);
  });
});

describe('project name on the list', () => {
  it('shows the name as a link to the project, and nothing without a project', async () => {
    setup();
    renderApp('/orbit/issues');
    await screen.findByText('In a project');
    const link = await screen.findByRole('link', { name: 'Project: Web rebuild' });
    expect(link).toHaveAttribute('href', `/orbit/projects/${PID}`);
    const rows = screen.getAllByRole('listitem').filter((li) => li.hasAttribute('data-issue-id'));
    const loose = rows.find((r) => within(r).queryByText('No project here'))!;
    expect(within(loose).queryByRole('link', { name: 'Project: Web rebuild' })).toBeNull();
    expect(screen.getAllByRole('link', { name: 'Project: Web rebuild' })).toHaveLength(1);
  });
});

describe('Project filter chip', () => {
  it('sends project=<id> when a project is chosen', async () => {
    const api = setup();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('In a project');
    await user.click(screen.getByRole('button', { name: /^Project/ }));
    await user.click(await screen.findByRole('option', { name: /Web rebuild/ }));
    await waitFor(() => expect(issueCalls(api).at(-1)!.url.searchParams.get('project')).toBe(PID));
    expect(screen.getByRole('button', { name: /^Project/ })).toHaveTextContent('Project: Web rebuild');
    await waitFor(() => expect(screen.queryByText('No project here')).toBeNull());
  });

  it('shows the emoji of each project in the picker when it has no logo (LAUN-232)', async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('In a project');
    await user.click(screen.getByRole('button', { name: /^Project/ }));
    const opt = await screen.findByRole('option', { name: /Web rebuild/ });
    expect(opt.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'emoji');
    expect(opt).toHaveTextContent('🛠️');
    const plain = screen.getByRole('option', { name: /Mobile/ });
    expect(plain.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'letter');
  });

  it('offers No project, which sends project=none', async () => {
    const api = setup();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('In a project');
    await user.click(screen.getByRole('button', { name: /^Project/ }));
    await user.click(await screen.findByRole('option', { name: 'No project' }));
    await waitFor(() => expect(issueCalls(api).at(-1)!.url.searchParams.get('project')).toBe('none'));
    await screen.findByText('No project here');
  });

  it('reads project from the URL on load', async () => {
    const api = setup();
    renderApp(`/orbit/issues?project=${PID}`);
    await screen.findByText('In a project');
    expect(issueCalls(api)[0]!.url.searchParams.get('project')).toBe(PID);
  });
});

describe('project on the issue page', () => {
  it('shows the project name and a link to it', async () => {
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/projects': () => ({
        json: { items: [proj(PID, 'Web rebuild')], nextCursor: null },
      }),
      'GET /api/v1/orgs/:org/issues/:issue': () => ({ json: makeIssue(1, { projectId: PID }) }),
    });
    renderApp('/orbit/issue/ORB-1');
    const link = await screen.findByRole('link', { name: 'Open project' });
    expect(link).toHaveAttribute('href', `/orbit/projects/${PID}`);
    expect(screen.getByRole('button', { name: /Web rebuild/ })).toBeInTheDocument();
  });
});

describe('project chip behaviour', () => {
  it('a click on the chip goes to the project, not the issue', async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await user.click(await screen.findByRole('link', { name: 'Project: Web rebuild' }));
    expect(currentLocation.pathname).toBe(`/orbit/projects/${PID}`);
  });

  it('a click that ends a drag does not navigate', async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const chip = await screen.findByRole('link', { name: 'Project: Web rebuild' });
    markDragged();
    await user.click(chip);
    resetDragged();
    expect(currentLocation.pathname).toBe('/orbit/issues');
  });

  it('a board card shows the chip', async () => {
    setup();
    renderApp('/orbit/issues?view=board');
    expect(await screen.findByRole('link', { name: 'Project: Web rebuild' })).toHaveAttribute(
      'href',
      `/orbit/projects/${PID}`,
    );
  });

  it('an unknown project id in the URL shows the raw id on the chip and still filters', async () => {
    const api = setup();
    renderApp('/orbit/issues?project=gone-id');
    expect(await screen.findByRole('button', { name: 'Project: gone-id' })).toBeInTheDocument();
    expect(issueCalls(api)[0]!.url.searchParams.get('project')).toBe('gone-id');
  });

  it('Clear filters in the empty state clears the project too', async () => {
    const api = mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/projects': () => ({
        json: { items: [proj(PID, 'Web rebuild')], nextCursor: null },
      }),
      'GET /api/v1/orgs/:org/issues': (r) => ({
        json: { items: r.url.searchParams.get('project') ? [] : [makeIssue(1)], nextCursor: null },
      }),
    });
    const user = userEvent.setup();
    renderApp(`/orbit/issues?project=${PID}`);
    await user.click(await screen.findByRole('button', { name: 'Clear filters' }));
    await screen.findByText('Issue 1');
    expect(issueCalls(api).at(-1)!.url.searchParams.has('project')).toBe(false);
    expect(currentLocation.search).not.toContain('project=');
  });

  it('the project page has no Project filter chip and ignores ?project=', async () => {
    const api = mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/projects': () => ({
        json: { items: [proj(PID, 'Web rebuild')], nextCursor: null },
      }),
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: proj(PID, 'Web rebuild') }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: { items: [], nextCursor: null } }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [makeIssue(1)], nextCursor: null } }),
    });
    renderApp(`/orbit/projects/${PID}?project=other-id`);
    await screen.findByText('Issue 1');
    expect(screen.queryByRole('button', { name: /^Project/ })).toBeNull();
    // The stripped ?project= does not count as an active filter: no Clear button.
    expect(screen.queryByRole('button', { name: /^Clear/ })).toBeNull();
    expect(issueCalls(api).every((c) => c.url.searchParams.get('project') === PID)).toBe(true);
  });
});

describe('project chip: the shared click guard', () => {
  it('does not reach a parent click handler (stopPropagation)', async () => {
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/projects': () => ({
        json: { items: [proj(PID, 'Web rebuild')], nextCursor: null },
      }),
    });
    const seen = vi.fn();
    renderAt(
      <div onClick={seen}>
        <ProjectChip org="orbit" projectId={PID} selecting={false} onToggle={() => {}} />
      </div>,
    );
    await userEvent.setup().click(await screen.findByRole('link', { name: 'Project: Web rebuild' }));
    expect(seen).not.toHaveBeenCalled();
  });

  it('a click that ends a text selection inside the row does not navigate', async () => {
    setup();
    renderApp('/orbit/issues');
    const chip = await screen.findByRole('link', { name: 'Project: Web rebuild' });
    const row = chip.closest('li')!;
    const sel = window.getSelection()!;
    const range = document.createRange();
    range.selectNodeContents(row.querySelector('[class*="titleText"]')!);
    sel.removeAllRanges();
    sel.addRange(range);
    fireEvent.click(chip, { detail: 1 }); // user-event's mousedown would clear the selection
    sel.removeAllRanges();
    expect(currentLocation.pathname).toBe('/orbit/issues');
  });

  it('on a phone in select mode a tap toggles the row instead of navigating', async () => {
    setup();
    vi.stubGlobal('matchMedia', (q: string) => ({
      matches: q.includes('max-width: 760px'),
      media: q,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      onchange: null,
      dispatchEvent: () => false,
    }));
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('In a project');
    await user.click(screen.getByRole('button', { name: 'Select' }));
    const chip = await screen.findByRole('link', { name: 'Project: Web rebuild' });
    const box = within(chip.closest('li')!).getByRole('checkbox');
    expect(box).not.toBeChecked();
    await user.click(chip);
    expect(currentLocation.pathname).toBe('/orbit/issues');
    expect(box).toBeChecked();
    vi.unstubAllGlobals();
  });
});
