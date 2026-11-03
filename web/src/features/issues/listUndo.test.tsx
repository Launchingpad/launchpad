import { act, fireEvent, renderHook, screen, waitFor, within } from '@testing-library/react';
import { StrictMode, type ReactNode } from 'react';
import userEvent from '@testing-library/user-event';
import { onlineManager } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { keys } from '../../api/keys';
import { findIssueInCaches } from '../../api/queries';
import { dragHandle, handleOf, rowMiddle, stubListLayout } from '../../test/listLayout';
import { currentLocation, renderApp } from '../../test/render';
import { allStates, card, setServerStates, undoServer } from '../../test/undoServer';
import { resetDragged } from './cardLink';
import { listDragState, useListDrag } from './listDrag';

// LAUN-237: Cmd/Ctrl+Z on the issue LIST, the same hook as the board (useIssueUndo.tsx). jsdom's
// navigator.platform is not a Mac, so the key is Ctrl. test/listLayout.ts stacks the groups: Backlog head 0-30,
// row 30-66; Todo head 70-100, rows 100-136, 136-172.

const press = (o: KeyboardEventInit = {}, target: Element = document.body) =>
  fireEvent.keyDown(target, { key: 'z', ctrlKey: true, ...o });
const redo = () => press({ key: 'Z', shiftKey: true });
const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));
const row = (n: number) => document.querySelector(`[data-issue-id="i${n}"]`) as HTMLElement;
const group = (name: string) => document.querySelector(`section[data-group="${name}"]`) as HTMLElement | null;
const names = (g: string) =>
  group(g)
    ? within(group(g)!)
        .queryAllByRole('link')
        .map((a) => a.textContent)
    : [];

let restore: () => void;
beforeEach(() => {
  setServerStates(allStates);
  onlineManager.setOnline(true);
  localStorage.clear();
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubListLayout();
  resetDragged();
});
afterEach(() => restore());

// Backlog: 1 | Todo: 2 3
const start = () => [card(1, 'Backlog'), card(2, 'Todo'), card(3, 'Todo')];

/** Focus a row and pick an option in one of the S / P / A / L pickers. */
async function pick(
  user: ReturnType<typeof userEvent.setup>,
  n: number | null,
  key: string,
  dialogName: string,
  option: RegExp,
) {
  if (n !== null) fireEvent.mouseDown(row(n));
  await user.keyboard(key);
  const dialog = await screen.findByRole('dialog', { name: dialogName });
  await user.click(within(dialog).getByRole('option', { name: option }));
  if (screen.queryByRole('dialog')) await user.keyboard('{Escape}'); // the label picker stays open
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  await settle();
}

describe('list undo: each field action', () => {
  it('S (status): Ctrl+Z sends the old state and rank; the toast Redo moves it again', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 's', 'Change status', /Done/);
    expect(s.patches()).toEqual([{ state: 'Done', version: 1 }]);
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
    const toast = (await screen.findByText('Undone: moved ORB-1 to Backlog')).closest('[role="status"]')!;
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
    fireEvent.click(within(toast as HTMLElement).getByRole('button', { name: 'Redo' }));
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's4', sortOrder: 0, version: 3 });
    expect(await screen.findByText('Redone: moved ORB-1 to Done')).toBeInTheDocument();
  });

  it('P (priority): Ctrl+Z sets the old one back; Ctrl+Shift+Z and Ctrl+Y redo', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ priority: 0, version: 2 });
    expect(await screen.findByText('Undone: set ORB-1 priority to No priority')).toBeInTheDocument();
    redo();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ priority: 2, version: 3 });
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    press({ key: 'y' });
    await waitFor(() => expect(s.patches()).toHaveLength(5));
    expect(s.patches()[4]).toEqual({ priority: 2, version: 5 });
  });

  it('A (assignee): Ctrl+Z unassigns again, by username', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'a', 'Assign to', /Chen/);
    expect(s.patches()).toEqual([{ assignee: 'chen', version: 1 }]);
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ assignee: null, version: 2 });
    expect(await screen.findByText('Undone: unassigned ORB-1')).toBeInTheDocument();
    redo();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ assignee: 'chen', version: 3 });
    expect(await screen.findByText('Redone: assigned ORB-1 to Chen')).toBeInTheDocument();
  });

  it('L (labels): Ctrl+Z removes the added label, by id', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'l', 'Toggle label', /bug/);
    expect(s.patches()).toEqual([{ addLabels: ['bug'], version: 1 }]);
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ removeLabels: ['l1'], version: 2 });
    expect(await screen.findByText('Undone: removed bug from ORB-1')).toBeInTheDocument();
    expect(s.rows.find((r) => r.id === 'i1')!.labels).toEqual([]);
  });
});

describe('list undo: manual order (sort Manual)', () => {
  it('a handle drag to another group: Ctrl+Z puts the row back in its group, at its rank', async () => {
    const s = undoServer(start());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 1');
    dragHandle('i1', { x: 100, y: 150 }); // Todo, after Card 2
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    expect(s.patches()[0]).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3']));
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
    expect(await screen.findByText('Undone: moved ORB-1 to Backlog')).toBeInTheDocument();
  });

  it('a drag inside a group: the toast says "in its group"', async () => {
    const s = undoServer(start());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 3');
    dragHandle('i3', { x: 100, y: 105 }); // above Card 2
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    expect(s.patches()[0]).toEqual({ placeBefore: 'i2', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2']));
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ sortOrder: 3072, version: 2 });
    expect(await screen.findByText('Undone: moved ORB-3 in its group')).toBeInTheDocument();
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 3']));
  });

  it('Up on the handle (the keyboard move) is undone too, with focus still on the handle', async () => {
    const s = undoServer(start());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 3');
    handleOf('i3').focus();
    fireEvent.keyDown(handleOf('i3'), { key: 'ArrowUp' });
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2']));
    await settle();
    press({}, document.activeElement ?? document.body);
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ sortOrder: 3072, version: 2 });
  });

  it('a press right after the release (the row still landing) undoes THAT drop', async () => {
    const s = undoServer(start());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 1');
    dragHandle('i1', { x: 100, y: 150 });
    press(); // no wait
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[0]).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
  });

  it('a press while a row is still held says so and writes nothing', async () => {
    const s = undoServer(start());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 1');
    dragHandle('i1', { x: 100, y: 150 }, { release: false });
    press();
    expect(await screen.findByText('Drop the issue first, then undo.')).toBeInTheDocument();
    fireEvent.keyDown(window, { key: 'Escape' });
    fireEvent.pointerUp(window, {
      pointerId: 1,
      pointerType: 'mouse',
      clientX: 15,
      clientY: rowMiddle('i1').y,
    });
    await settle();
    expect(s.patches()).toHaveLength(0);
  });
});

describe('list undo: create from the list', () => {
  it('C, then Ctrl+Z cancels the new issue (never a delete) and says so; redo puts it back at its rank', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByText('Card 1');
    await user.keyboard('c');
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(dialog).getByLabelText('Title'), 'Oops');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    expect(s.patches()[0]).toEqual({ state: 's9', version: 1 });
    expect(
      await screen.findByText(
        'Undone: moved ORB-90 to Canceled (undo cancels a new issue; it does not delete it)',
      ),
    ).toBeInTheDocument();
    expect(s.api.calls.some((c) => c.method === 'DELETE')).toBe(false);
    redo();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: -1024, version: 2 });
  });
});

describe('list undo: a change on a selection is ONE step', () => {
  it('S on two selected rows: one press undoes both', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    await pick(user, null, 's', 'Change status', /Done/);
    expect(s.patches()).toHaveLength(2);
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(s.patches().slice(2)).toEqual(
      expect.arrayContaining([
        { state: 's2', sortOrder: 2048, version: 2 },
        { state: 's2', sortOrder: 3072, version: 2 },
      ]),
    );
    expect(await screen.findByText('Undone: moved 2 issues to Todo')).toBeInTheDocument();
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });

  it('a partial failure: the step holds only the rows that were written', async () => {
    let fail = true;
    const s = undoServer(start(), {
      patch: (_b, _n, issue) =>
        issue === 'ORB-3' && fail
          ? { status: 400, json: { error: 'no', code: 'validation_failed' } }
          : undefined,
    });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    await pick(user, null, 'p', 'Set priority', /High/);
    fail = false;
    expect(s.patches()).toHaveLength(2);
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ priority: 0, version: 2 });
    expect(await screen.findByText('Undone: set ORB-2 priority to No priority')).toBeInTheDocument();
    await settle();
    expect(s.patches()).toHaveLength(3);
  });

  it('an undo where one row changed since: the other is undone, the stale one is skipped', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    await pick(user, null, 'p', 'Set priority', /High/);
    s.rows.find((r) => r.id === 'i2')!.version = 9; // someone else edits ORB-2
    await act(() => client.invalidateQueries({ queryKey: keys.issues('orbit') }));
    await waitFor(() => expect(findIssueInCaches(client, 'orbit', 'i2')?.version).toBe(9));
    press();
    expect(await screen.findByText('Undone: set ORB-3 priority to No priority')).toBeInTheDocument();
    expect(await screen.findByText('ORB-2 changed since; undo skipped for it')).toBeInTheDocument();
    expect(s.rows.find((r) => r.id === 'i2')!.priority).toBe(2);
  });
});

describe('list undo: stale, hidden, network and offline', () => {
  async function prioritySet(opts: Parameters<typeof undoServer>[1] = {}) {
    const s = undoServer(start(), opts);
    const user = userEvent.setup();
    const r = renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    return { s, ...r };
  }

  it('409: "This issue changed since", no overwrite, the entry goes', async () => {
    const { s, client } = await prioritySet();
    s.rows.find((r) => r.id === 'i1')!.version = 7;
    await act(() => client.invalidateQueries({ queryKey: keys.issues('orbit') }));
    await waitFor(() => expect(findIssueInCaches(client, 'orbit', 'i1')?.version).toBe(7));
    press();
    expect(await screen.findByText('This issue changed since; undo skipped')).toBeInTheDocument();
    expect(s.patches()[1]).toMatchObject({ version: 2 });
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });

  it('404: skipped and dropped', async () => {
    const { s } = await prioritySet();
    s.rows.splice(
      s.rows.findIndex((r) => r.id === 'i1'),
      1,
    );
    press();
    expect(await screen.findByText('ORB-1 is gone or hidden from you; undo skipped')).toBeInTheDocument();
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });

  it('a network error keeps the entry; offline writes nothing; the next press works', async () => {
    let fail = true;
    const { s } = await prioritySet({
      patch: (_b, n) =>
        n === 1 && fail ? { status: 503, json: { error: 'down', code: 'unavailable' } } : undefined,
    });
    press();
    expect(
      await screen.findByText('Could not undo ORB-1: down. Press the keys again to retry.'),
    ).toBeInTheDocument();
    fail = false;
    act(() => onlineManager.setOnline(false));
    press();
    expect(await screen.findByText(/You are offline; nothing was changed/)).toBeInTheDocument();
    await settle();
    expect(s.patches()).toHaveLength(2);
    act(() => onlineManager.setOnline(true));
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ priority: 0, version: 2 });
  });
});

describe('list undo: where the keys are not ours, and when the stack empties', () => {
  it('in the filter field the browser keeps its own undo; with a dialog open nothing happens', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    const field = document.createElement('input');
    document.body.appendChild(field);
    field.focus();
    expect(press({}, field)).toBe(true); // not prevented
    field.remove();
    await user.keyboard('?');
    await screen.findByRole('dialog', { name: 'Keyboard shortcuts' });
    press();
    await settle();
    expect(s.patches()).toHaveLength(1);
  });

  it('switching to the board and back empties the stack', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    await user.click(screen.getByRole('button', { name: 'Board view' }));
    await screen.findByRole('group', { name: 'Board' });
    await user.click(screen.getByRole('button', { name: 'List view' }));
    await waitFor(() => expect(screen.queryByRole('group', { name: 'Board' })).toBeNull());
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(1);
  });

  it('another team empties the stack', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp(['/orbit/team/WEB', '/orbit/team/ORB']);
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    act(() => currentLocation.go(-1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/WEB'));
    act(() => currentLocation.go(1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
    await screen.findByText('Card 1');
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(1);
  });

  it('the shortcut help names the list', async () => {
    undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await user.keyboard('?');
    const help = await screen.findByRole('dialog', { name: 'Keyboard shortcuts' });
    expect(within(help).getByText(/On the board or the list: undo your last change/)).toBeInTheDocument();
    expect(within(help).getByText(/On the board or the list: redo/)).toBeInTheDocument();
  });
});

describe('list undo: review follow-ups', () => {
  it('All issues -> My issues (the same page instance) empties the stack', async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp(['/orbit/my-issues', '/orbit/issues']);
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    act(() => currentLocation.go(-1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/my-issues'));
    await screen.findByText('Card 1');
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    act(() => currentLocation.go(1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issues'));
    await screen.findByText('Card 1');
    press();
    await waitFor(() => expect(screen.getAllByText('Nothing to undo')).toHaveLength(2));
    expect(s.patches()).toHaveLength(1);
  });

  it("an older toast's Redo does not redo another change: it says so and writes nothing", async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    await pick(user, 3, 'p', 'Set priority', /Urgent/);
    press(); // undo ORB-3: its toast's Redo is for ORB-3
    const older = (await screen.findByText('Undone: set ORB-3 priority to No priority')).closest(
      '[role="status"]',
    ) as HTMLElement;
    press(); // undo ORB-1: now the top of the redo stack
    const newer = (await screen.findByText('Undone: set ORB-1 priority to No priority')).closest(
      '[role="status"]',
    ) as HTMLElement;
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    fireEvent.click(within(older).getByRole('button', { name: 'Redo' }));
    expect(
      await screen.findByText(/That change is no longer the last one; nothing was changed/),
    ).toBeInTheDocument();
    await settle();
    expect(s.patches()).toHaveLength(4);
    fireEvent.click(within(newer).getByRole('button', { name: 'Redo' }));
    await waitFor(() => expect(s.patches()).toHaveLength(5));
    expect(s.patches()[4]).toEqual({ priority: 2, version: 3 });
  });

  it("a toast's Redo after leaving the page writes nothing", async () => {
    const s = undoServer(start());
    const user = userEvent.setup();
    renderApp(['/orbit/issue/ORB-1', '/orbit/issues']);
    await screen.findByText('Card 1');
    await pick(user, 1, 'p', 'Set priority', /High/);
    press();
    const toast = (await screen.findByText('Undone: set ORB-1 priority to No priority')).closest(
      '[role="status"]',
    ) as HTMLElement;
    act(() => currentLocation.go(-1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-1'));
    await waitFor(() => expect(document.querySelector('[data-list]')).toBeNull());
    fireEvent.click(within(toast).getByRole('button', { name: 'Redo' }));
    await settle();
    expect(s.patches()).toHaveLength(2);
  });

  async function created() {
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByText('Card 1');
    await user.keyboard('c');
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(dialog).getByLabelText('Title'), 'Oops');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
  }

  it('the undo of a create skips an issue that has a comment now', async () => {
    const comments: Record<string, unknown[]> = {};
    const s = undoServer(start(), { comments });
    await created();
    comments.i90 = [{ id: 'c1' }];
    press();
    expect(
      await screen.findByText('ORB-90 has comments or sub-issues now; undo skipped'),
    ).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });

  it('the undo of a create skips an issue that has a sub-issue now', async () => {
    const s = undoServer(start());
    await created();
    s.rows.push(card(7, 'Todo', { parent: { id: 'i90', identifier: 'ORB-90', title: 'Oops' } as never }));
    press();
    expect(
      await screen.findByText('ORB-90 has comments or sub-issues now; undo skipped'),
    ).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
  });
});

describe('list undo: the reads before the undo of a create', () => {
  async function createOops() {
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB');
    await screen.findByText('Card 1');
    await user.keyboard('c');
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(dialog).getByLabelText('Title'), 'Oops');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
  }

  it('the new issue is gone (comments 404, sub-issues 400 "unknown parent"): "gone or hidden", dropped', async () => {
    const s = undoServer(start());
    await createOops();
    s.rows.splice(
      s.rows.findIndex((r) => r.id === 'i90'),
      1,
    );
    press();
    expect(await screen.findByText('ORB-90 is gone or hidden from you; undo skipped')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });

  it('the sub-issue read alone says "unknown parent" (400): also "gone or hidden", not "refused"', async () => {
    const s = undoServer(start(), {
      precheck: (what) =>
        what === 'parent'
          ? { status: 400, json: { error: 'unknown parent i90', code: 'validation_failed' } }
          : undefined,
    });
    await createOops();
    press();
    expect(await screen.findByText('ORB-90 is gone or hidden from you; undo skipped')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
  });

  it('a network error on the reads keeps the entry: the next press cancels the issue', async () => {
    let fail = true;
    const s = undoServer(start(), {
      precheck: (what) =>
        what === 'comments' && fail
          ? { status: 503, json: { error: 'down', code: 'unavailable' } }
          : undefined,
    });
    await createOops();
    press();
    expect(
      await screen.findByText('Could not undo ORB-90: down. Press the keys again to retry.'),
    ).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
    fail = false;
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    expect(s.patches()[0]).toEqual({ state: 's9', version: 1 });
  });

  it('a 403 on the reads: refused with the server text, dropped', async () => {
    const s = undoServer(start(), {
      precheck: (what) =>
        what === 'comments' ? { status: 403, json: { error: 'not allowed', code: 'forbidden' } } : undefined,
    });
    await createOops();
    press();
    expect(await screen.findByText('ORB-90: undo refused. not allowed')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });
});

describe('list undo: the list drag machine', () => {
  it('a handle only pressed (no move yet) counts as held: nothing written, the toast says so', async () => {
    const s = undoServer(start());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 1');
    const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
    const y = rowMiddle('i1').y;
    fireEvent.pointerDown(handleOf('i1'), { ...ptr, button: 0, clientX: 15, clientY: y });
    press();
    expect(await screen.findByText('Drop the issue first, then undo.')).toBeInTheDocument();
    fireEvent.pointerUp(window, { ...ptr, clientX: 15, clientY: y });
    await settle();
    expect(s.patches()).toHaveLength(0);
  });

  it('under StrictMode (the effect runs twice) the machine is still known to the undo', () => {
    const wrapper = ({ children }: { children: ReactNode }) => <StrictMode>{children}</StrictMode>;
    const opts = { getList: () => null, getScroller: () => null, onDrop: () => {}, announce: () => {} };
    const { result, unmount } = renderHook(() => useListDrag(opts), { wrapper });
    result.current.phase = 'pressed';
    expect(listDragState()).toBe('held');
    result.current.phase = 'landing';
    expect(listDragState()).toBe('landing');
    result.current.phase = 'idle';
    unmount();
    expect(listDragState()).toBe('idle');
  });
});
