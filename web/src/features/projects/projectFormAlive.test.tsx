import { StrictMode } from 'react';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setMemoryCsrf } from '../../api/http';
import { keys } from '../../api/keys';
import type { Project } from '../../api/types';
import { joinServer, page } from '../../test/join';
import { renderAt, testClient } from '../../test/render';
import { ProjectFormDialog } from './ProjectFormDialog';

// LAUN-177: the dialog's `alive` ref (no callback after it is gone, also under StrictMode) and the IME Enter rule.

const created = {
  id: '01a00000-0000-7000-8000-0000000000aa',
  name: 'Roadmap',
  description: '',
  content: '',
  contentTruncated: false,
  state: 'planned',
  lead: null,
  startDate: null,
  targetDate: null,
  teams: [{ id: 't-ORB', key: 'ORB', name: 'Orbit' }],
  version: 1,
  createdAt: '',
  updatedAt: '',
  startedAt: null,
  completedAt: null,
  canceledAt: null,
  archivedAt: null,
  avatarUrl: null,
  issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
} as Project;

function gate() {
  let release: () => void = () => {};
  const p = new Promise<void>((r) => (release = r));
  return { p, release: () => release() };
}

function setup(opts: { project?: Promise<void>; milestone?: Promise<void> } = {}, strict = false) {
  const api = joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page([]) }),
      'POST /api/v1/orgs/:org/projects': async () => {
        await opts.project;
        return { status: 201, json: created };
      },
      'POST /api/v1/orgs/:org/milestones': async () => {
        await opts.milestone;
        return { status: 201, json: { id: 'm1' } };
      },
    },
  });
  setMemoryCsrf('csrf-test');
  const client = testClient();
  client.setQueryData(keys.session, {
    user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@example.com', isPlatformAdmin: false },
    orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
    csrfToken: 'csrf-test',
  });
  const onClose = vi.fn();
  const onSaved = vi.fn();
  const el = <ProjectFormDialog onClose={onClose} onSaved={onSaved} />;
  const r = renderAt(strict ? <StrictMode>{el}</StrictMode> : el, { client });
  return { api, onClose, onSaved, ...r };
}

async function fill(withMilestone = false) {
  const dlg = await screen.findByRole('dialog');
  await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
  await userEvent.click(within(dlg).getByRole('button', { name: /^Teams/ }));
  await userEvent.click(await within(dlg).findByRole('option', { name: /ORB/ }));
  await userEvent.keyboard('{Escape}');
  if (withMilestone) {
    await userEvent.click(within(dlg).getByRole('button', { name: 'Add milestone' }));
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' }), 'Alpha');
  }
  return dlg;
}

let errors: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  errors = vi.spyOn(console, 'error').mockImplementation(() => {});
});
afterEach(() => errors.mockRestore());

describe('the alive ref', () => {
  it('still calls onSaved and onClose once under StrictMode (effects run twice)', async () => {
    const { onClose, onSaved } = setup({}, true);
    const dlg = await fill(true);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(onSaved).toHaveBeenCalledTimes(1);
    expect(onSaved.mock.calls[0]![0]).toMatchObject({ id: created.id });
  });

  it('calls nothing and warns about nothing when the dialog is gone during the project create', async () => {
    const g = gate();
    const { onClose, onSaved, unmount, api } = setup({ project: g.p });
    const dlg = await fill();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'POST')).toBe(true));
    unmount();
    g.release();
    await new Promise((r) => setTimeout(r, 50));
    expect(onSaved).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(errors).not.toHaveBeenCalled();
  });

  it('calls nothing and warns about nothing when the dialog is gone during a milestone', async () => {
    const g = gate();
    const { onClose, onSaved, unmount, api } = setup({ milestone: g.p });
    const dlg = await fill(true);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(api.calls.some((c) => c.url.pathname.includes('/milestones'))).toBe(true));
    unmount();
    g.release();
    await new Promise((r) => setTimeout(r, 50));
    expect(onSaved).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(errors).not.toHaveBeenCalled();
  });
});

describe('Enter while an input method is composing', () => {
  it('does not leave the name field or add a milestone row on keyCode 229', async () => {
    setup();
    const dlg = await fill(true);
    const name = within(dlg).getByRole('textbox', { name: 'Name' });
    name.focus();
    fireEvent.keyDown(name, { key: 'Enter', keyCode: 229 });
    expect(name).toHaveFocus();
    const ms = within(dlg).getByRole('textbox', { name: 'Milestone 1 name' });
    fireEvent.keyDown(ms, { key: 'Enter', keyCode: 229 });
    expect(within(dlg).queryByRole('textbox', { name: 'Milestone 2 name' })).toBeNull();
    fireEvent.keyDown(ms, { key: 'Enter', keyCode: 13 });
    expect(await within(dlg).findByRole('textbox', { name: 'Milestone 2 name' })).toBeInTheDocument();
  });
});

describe('while saving, what is shown is what was sent', () => {
  it('ignores typing, pills and panels', async () => {
    const g = gate();
    const { api } = setup({ project: g.p });
    const dlg = await fill();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await within(dlg).findByRole('button', { name: 'Saving…' });
    const name = within(dlg).getByRole('textbox', { name: 'Name' });
    const desc = within(dlg).getByRole('textbox', { name: 'Description' });
    expect(name).toHaveAttribute('readonly');
    expect(desc).toHaveAttribute('readonly');
    await userEvent.type(name, 'Z');
    await userEvent.type(desc, 'Z');
    expect(name).toHaveValue('Roadmap');
    expect(desc).toHaveValue('');
    for (const pill of [/^Status/, /^Lead/, /^Start date/, /^Target date/, /^Teams/]) {
      await userEvent.click(within(dlg).getByRole('button', { name: pill }));
      expect(within(dlg).getByRole('button', { name: pill })).toHaveAttribute('aria-expanded', 'false');
    }
    expect(within(dlg).queryByRole('listbox')).toBeNull();
    expect(within(dlg).getByRole('button', { name: 'Close' })).toHaveAttribute('aria-disabled', 'true');
    expect(within(dlg).getByRole('button', { name: 'Cancel' })).toHaveAttribute('aria-disabled', 'true');
    g.release();
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1));
    expect(api.calls.find((c) => c.method === 'POST')!.body).toMatchObject({
      name: 'Roadmap',
      teams: ['ORB'],
    });
  });

  it('marks the name as a one-line multiline-false field', async () => {
    setup();
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByRole('textbox', { name: 'Name' })).toHaveAttribute('aria-multiline', 'false');
  });
});
