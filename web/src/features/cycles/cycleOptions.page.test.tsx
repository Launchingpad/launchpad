import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Cycle } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { team } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { cycleDays, dayAfter } from './cycleRules';

// LAUN-172 (decisions-T27): the "..." options menu of the cycle page.

const day = 86400_000;
// A UTC midnight: with the team zone set to UTC below, a normal cycle boundary (the end is exclusive).
const iso = (offsetDays: number) =>
  new Date(Date.now() + offsetDays * day).toISOString().slice(0, 10) + 'T00:00:00.000Z';
const cy = (n: number, from: number, to: number, over: Partial<Cycle> = {}): Cycle => ({
  id: `cy${n}`,
  team: { id: 'team1', key: 'ORB', name: 'Orbit' },
  number: n,
  name: null,
  description: '',
  startsAt: iso(from),
  endsAt: iso(to),
  completedAt: null,
  createdAt: iso(from),
  updatedAt: iso(from),
  archivedAt: null,
  ...over,
});
const cycles = [
  cy(3, -2, 12),
  cy(4, 12, 26),
  cy(2, -16, -2, { completedAt: iso(-2) }),
  cy(1, -30, -16, { completedAt: iso(-16) }),
];
const on = { ...team, cyclesEnabled: true, cycleRollover: true, cycleTimezone: 'UTC' };

function server(routes = {}, opts: { role?: 'admin' | 'member' } = {}) {
  return joinServer({
    role: opts.role,
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([on]) }),
      'GET /api/v1/orgs/:org/cycles': () => ({ json: page(cycles) }),
      'GET /api/v1/orgs/:org/cycles/:cycle/snapshots': () => ({ json: page([]) }),
      ...routes,
    },
  });
}

async function openMenu(path: string, user = userEvent.setup()) {
  renderApp(path);
  await user.click(await screen.findByRole('button', { name: 'Cycle options' }));
  return { user, menu: await screen.findByRole('menu', { name: 'Cycle options' }) };
}

const item = (menu: HTMLElement, name: RegExp | string) => within(menu).getByRole('menuitem', { name });

describe('cycle options menu', () => {
  const FIVE = [
    'Edit name and description…',
    'Change dates…',
    'Mark complete…',
    'Delete cycle…',
    'Copy link',
  ];
  it('lists the five rows of the brief for an admin, all usable on the running cycle', async () => {
    server({}, { role: 'admin' });
    const { menu } = await openMenu('/orbit/team/ORB/cycle/3');
    const rows = within(menu).getAllByRole('menuitem');
    expect(rows.map((m) => m.textContent)).toEqual(FIVE);
    for (const m of rows) expect(m).not.toHaveAttribute('aria-disabled');
  });

  it('a member keeps all five rows, but Mark complete and Delete are disabled with the admin reason', async () => {
    const api = server({}, { role: 'member' });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    const rows = within(menu).getAllByRole('menuitem');
    expect(rows.map((m) => FIVE.find((f) => m.textContent!.startsWith(f)))).toEqual(FIVE);
    const reason = 'Only admins can complete or delete a cycle.';
    expect(item(menu, /Mark complete/)).toHaveAttribute('aria-disabled', 'true');
    expect(item(menu, /Mark complete/)).toHaveAccessibleDescription(reason);
    expect(item(menu, /Delete cycle/)).toHaveAccessibleDescription(reason);
    for (const n of [/Edit name/, /Change dates/, 'Copy link'])
      expect(item(menu, n)).not.toHaveAttribute('aria-disabled');
    await user.click(item(menu, /Mark complete/));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.method !== 'GET')).toHaveLength(0);
  });

  it('edits the name and the description with one PATCH of what changed', async () => {
    const api = server({
      'PATCH /api/v1/orgs/:org/cycles/:cycle': () => ({ json: { ...cycles[0]!, name: 'Sprint A' } }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Edit name and description/));
    const dialog = await screen.findByRole('dialog');
    const name = within(dialog).getByLabelText('Name');
    await user.type(name, '  Sprint A ');
    await user.type(within(dialog).getByLabelText('Description'), 'Ship it');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/cycles/cy3')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/cycles/cy3')[0]!.body).toEqual({
      name: 'Sprint A',
      description: 'Ship it',
    });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('clears a name with null and sends nothing when nothing changed', async () => {
    const named = [cy(3, -2, 12, { name: 'Old' }), cycles[1]!, cycles[2]!, cycles[3]!];
    const api = server({
      'GET /api/v1/orgs/:org/cycles': () => ({ json: page(named) }),
      'PATCH /api/v1/orgs/:org/cycles/:cycle': () => ({ json: named[0] }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Edit name and description/));
    let dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'PATCH', '/cycles/cy3')).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Cycle options' }));
    await user.click(
      item(await screen.findByRole('menu', { name: 'Cycle options' }), /Edit name and description/),
    );
    dialog = await screen.findByRole('dialog');
    await user.clear(within(dialog).getByLabelText('Name'));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/cycles/cy3')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/cycles/cy3')[0]!.body).toEqual({ name: null });
  });

  it('shows the server sentence when a save is refused', async () => {
    server({
      'PATCH /api/v1/orgs/:org/cycles/:cycle': () => ({
        status: 400,
        json: { error: 'name must be 1-255 characters, or null', code: 'validation_failed' },
      }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Edit name and description/));
    const dialog = await screen.findByRole('dialog');
    await user.type(within(dialog).getByLabelText('Name'), 'x');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'name must be 1-255 characters, or null',
    );
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  it('changes only the last day of the running cycle: the first day is locked', async () => {
    const api = server({ 'PATCH /api/v1/orgs/:org/cycles/:cycle': () => ({ json: cycles[0] }) });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Change dates/));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByLabelText('First day')).toBeDisabled();
    const days = cycleDays(cycles[0]!, on.cycleTimezone)!;
    expect(within(dialog).getByLabelText('Last day')).toHaveValue(days.last);
    const chosen = dayAfter(days.last); // one day later
    fireEvent.change(within(dialog).getByLabelText('Last day'), { target: { value: chosen } });
    await user.click(within(dialog).getByRole('button', { name: 'Save dates' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/cycles/cy3')).toHaveLength(1));
    // endsAt is the day AFTER the last day (exclusive), and no startsAt goes out.
    expect(callsTo(api, 'PATCH', '/cycles/cy3')[0]!.body).toEqual({ endsAt: dayAfter(chosen) });
  });

  it('a cycle with a mid-day end shows its end day as the last day and sends that day back as endsAt', async () => {
    const mid = [
      cy(3, -2, 12, { endsAt: iso(12).slice(0, 10) + 'T09:00:00.000Z' }),
      cy(4, 12, 26, { startsAt: iso(12).slice(0, 10) + 'T09:00:00.000Z' }),
    ];
    const api = server({
      'GET /api/v1/orgs/:org/cycles': () => ({ json: page(mid) }),
      'PATCH /api/v1/orgs/:org/cycles/:cycle': () => ({ json: mid[0] }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Change dates/));
    const dialog = await screen.findByRole('dialog');
    const last = within(dialog).getByLabelText('Last day');
    expect(last).toHaveValue(iso(12).slice(0, 10));
    const chosen = dayAfter(iso(12).slice(0, 10));
    fireEvent.change(last, { target: { value: chosen } });
    await user.click(within(dialog).getByRole('button', { name: 'Save dates' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/cycles/cy3')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/cycles/cy3')[0]!.body).toEqual({ endsAt: chosen });
  });

  it('sends a changed first day of an upcoming cycle, and shows the server message on a 409', async () => {
    const api = server({
      'PATCH /api/v1/orgs/:org/cycles/:cycle': () => ({
        status: 409,
        json: { error: 'the new start would leave the previous cycle (3) empty', code: 'cycle_conflict' },
      }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/4');
    await user.click(item(menu, /Change dates/));
    const dialog = await screen.findByRole('dialog');
    const first = within(dialog).getByLabelText('First day');
    expect(first).toBeEnabled();
    const days = cycleDays(cycles[1]!, on.cycleTimezone)!;
    const chosen = dayAfter(days.first);
    fireEvent.change(first, { target: { value: chosen } });
    await user.click(within(dialog).getByRole('button', { name: 'Save dates' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'the new start would leave the previous cycle (3) empty',
    );
    expect(callsTo(api, 'PATCH', '/cycles/cy4')[0]!.body).toEqual({ startsAt: chosen });
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  it('refuses a last day before the first day without a request', async () => {
    const api = server();
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/4');
    await user.click(item(menu, /Change dates/));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText('Last day'), { target: { value: '2000-01-01' } });
    await user.click(within(dialog).getByRole('button', { name: 'Save dates' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'The last day cannot be before the first day.',
    );
    expect(callsTo(api, 'PATCH', '/cycles/cy4')).toHaveLength(0);
  });

  it('completes the running cycle: one POST with an Idempotency-Key, then the dialog closes', async () => {
    const api = server({
      'POST /api/v1/orgs/:org/cycles/:cycle/complete': () => ({
        json: { ...cycles[0]!, completedAt: iso(0) },
      }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Mark complete/));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Issues that are not done or canceled move to Cycle 4.');
    await user.click(within(dialog).getByRole('button', { name: 'Complete cycle' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/cycles/cy3/complete')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/cycles/cy3/complete')[0]!.headers.get('Idempotency-Key')).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('says what a completion does when rollover is off, and shows a refusal from the server', async () => {
    server({
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([{ ...on, cycleRollover: false }]) }),
      'POST /api/v1/orgs/:org/cycles/:cycle/complete': () => ({
        status: 409,
        json: { error: 'cycle 3 has no next cycle', code: 'cycle_conflict' },
      }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Mark complete/));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('Its issues stay in this cycle');
    await user.click(within(dialog).getByRole('button', { name: 'Complete cycle' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('cycle 3 has no next cycle');
    expect(within(dialog).getByRole('button', { name: 'Complete cycle' })).toBeEnabled();
  });

  it('deletes a cycle and goes to the cycle list', async () => {
    const api = server({ 'DELETE /api/v1/orgs/:org/cycles/:cycle': () => ({ status: 204 }) });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Delete cycle/));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Delete cycle' }));
    await waitFor(() => expect(callsTo(api, 'DELETE', '/cycles/cy3')).toHaveLength(1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB/cycles'));
  });

  it('keeps the dialog and shows the server message when the cycle still has issues', async () => {
    server({
      'DELETE /api/v1/orgs/:org/cycles/:cycle': () => ({
        status: 409,
        json: {
          error: 'cycle 3 still has 2 issue(s); move them to another cycle first',
          code: 'cycle_in_use',
        },
      }),
    });
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, /Delete cycle/));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Delete cycle' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'cycle 3 still has 2 issue(s); move them to another cycle first',
    );
    expect(currentLocation.pathname).toBe('/orbit/team/ORB/cycle/3');
  });

  it('disables a row a rule forbids, with its reason, and does nothing when it is chosen', async () => {
    const api = server();
    // Cycle 2: complete (past). Dates, complete and delete are all forbidden.
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/2');
    const dates = item(menu, /Change dates/);
    expect(dates).toHaveAttribute('aria-disabled', 'true');
    expect(dates).toHaveAccessibleDescription('The dates of a past cycle cannot change.');
    expect(item(menu, /Mark complete/)).toHaveAccessibleDescription('This cycle is already complete.');
    expect(item(menu, /Delete cycle/)).toHaveAccessibleDescription('A completed cycle cannot be deleted.');
    await user.click(dates);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('menu', { name: 'Cycle options' })).toBeInTheDocument(); // stays open: the reason can be read
    // The name and the link stay available.
    expect(item(menu, /Edit name and description/)).not.toHaveAttribute('aria-disabled');
    expect(item(menu, 'Copy link')).not.toHaveAttribute('aria-disabled');
    expect(api.calls.filter((c) => c.method !== 'GET')).toHaveLength(0);
  });

  it('forbids completing an upcoming cycle and deleting the last cycle', async () => {
    server();
    const { menu } = await openMenu('/orbit/team/ORB/cycle/4');
    expect(item(menu, /Mark complete/)).toHaveAccessibleDescription(
      'Only the current cycle can be completed.',
    );
    expect(item(menu, /Delete cycle/)).toHaveAccessibleDescription('The last cycle cannot be deleted.');
    expect(item(menu, /Change dates/)).not.toHaveAttribute('aria-disabled');
  });

  it('copies the link to the cycle page', async () => {
    server();
    const { user, menu } = await openMenu('/orbit/team/ORB/cycle/3');
    await user.click(item(menu, 'Copy link'));
    expect(await navigator.clipboard.readText()).toBe(`${window.location.origin}/orbit/team/ORB/cycle/3`);
    expect(await screen.findByText('Link copied')).toBeInTheDocument();
  });
});
