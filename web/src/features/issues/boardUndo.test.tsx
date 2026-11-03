import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { onlineManager } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { allStates, card, setServerStates, undoServer } from '../../test/undoServer';
import { currentLocation, renderApp, renderAt } from '../../test/render';
import { useIssueActions, type WriteRecorder } from './actions';
import { keys } from '../../api/keys';
import { findIssueInCaches } from '../../api/queries';
import { resetDragged } from './cardLink';

// LAUN-236: Cmd/Ctrl+Z on the board. jsdom's navigator.platform is not a Mac, so the key is Ctrl.

const server = undoServer;

const column = (name: string) => document.querySelector(`[data-column="${name}"]`) as HTMLElement;
const names = (col: string) =>
  within(column(col))
    .queryAllByRole('link')
    .map((a) => a.textContent);
const cardEl = (n: number) => document.querySelector(`[data-issue-id="i${n}"]`) as HTMLElement;
const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
function dragTo(n: number, grab: { x: number; y: number }, to: { x: number; y: number }) {
  fireEvent.pointerDown(cardEl(n), { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
  fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 10, clientY: grab.y + 10 });
  fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
  fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
}
const press = (o: KeyboardEventInit = {}, target: Element = document.body) =>
  fireEvent.keyDown(target, { key: 'z', ctrlKey: true, ...o });
const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));

let restore: () => void;
beforeEach(() => {
  setServerStates(allStates);
  onlineManager.setOnline(true);
  localStorage.clear();
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubBoardLayout();
  resetDragged();
});
afterEach(() => restore());

// Backlog: 1 | Todo: 2 3
const start = () => [card(1, 'Backlog'), card(2, 'Todo'), card(3, 'Todo')];

async function movedOne() {
  const s = server(start());
  renderApp('/orbit/issues?view=board');
  await screen.findByText('Card 1');
  dragTo(1, middle(0, 0), at(1, 1));
  await waitFor(() => expect(s.patches()).toHaveLength(1));
  await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3']));
  await settle();
  return s;
}

describe('board undo: a card move', () => {
  it('Ctrl+Z puts the card back: one PATCH with the old state, the old rank and the version', async () => {
    const s = await movedOne();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
    expect(await screen.findByText('Undone: moved ORB-1 to Backlog')).toBeInTheDocument();
  });

  it('Redo (the toast button, then Ctrl+Shift+Z after another undo) moves it again', async () => {
    const s = await movedOne();
    press();
    const toast = (await screen.findByText('Undone: moved ORB-1 to Backlog')).closest('[role="status"]')!;
    const redo = within(toast as HTMLElement).getByRole('button', { name: 'Redo' });
    redo.focus(); // the toast's button is a real button: reachable and usable by keyboard
    fireEvent.click(redo);
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's2', sortOrder: 2049, version: 3 });
    expect(await screen.findByText('Redone: moved ORB-1 to Todo')).toBeInTheDocument();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    press({ key: 'Z', shiftKey: true });
    await waitFor(() => expect(s.patches()).toHaveLength(5));
    expect(s.patches()[4]).toEqual({ state: 's2', sortOrder: 2049, version: 5 });
    press({ key: 'y' }); // Ctrl+Y redoes too (not on a Mac): nothing left
    expect(await screen.findByText('Nothing to redo')).toBeInTheDocument();
  });

  it('a double press makes ONE undo write', async () => {
    const s = await movedOne();
    press();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await settle();
    expect(s.patches()).toHaveLength(2);
  });

  it('nothing on the stack: says so and writes nothing', async () => {
    const s = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(0);
  });
});

describe('board undo: stale, hidden and network cases', () => {
  it('the card changed since (409): no overwrite, the entry is dropped', async () => {
    const s = server(start());
    const { client } = renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    // Someone else edits ORB-1, and this tab hears of it (the event stream refetches): the cache holds version 7.
    s.rows.find((r) => r.id === 'i1')!.version = 7;
    await act(() => client.invalidateQueries({ queryKey: keys.issues('orbit') }));
    await waitFor(() => expect(findIssueInCaches(client, 'orbit', 'i1')?.version).toBe(7));
    press();
    expect(await screen.findByText('This card changed since; undo skipped')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(2);
    expect(s.patches()[1]).toMatchObject({ version: 2 }); // the version this tab's move left, not the cache's 7
    expect(s.rows.find((r) => r.id === 'i1')!.state.name).toBe('Todo');
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(2);
  });

  it('the card is gone or hidden (404): skipped and dropped', async () => {
    const s = await movedOne();
    s.rows.splice(
      s.rows.findIndex((r) => r.id === 'i1'),
      1,
    );
    press();
    expect(await screen.findByText('ORB-1 is gone or hidden from you; undo skipped')).toBeInTheDocument();
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
  });

  it('a network error keeps the entry: the next press tries again', async () => {
    let fail = true;
    const s = server(start(), {
      patch: (_b, n) =>
        n === 1 && fail ? { status: 503, json: { error: 'down', code: 'unavailable' } } : undefined,
    });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    press();
    expect(
      await screen.findByText('Could not undo ORB-1: down. Press the keys again to retry.'),
    ).toBeInTheDocument();
    fail = false;
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
    expect(await screen.findByText('Undone: moved ORB-1 to Backlog')).toBeInTheDocument();
  });
});

describe('board undo: where the keys are not ours', () => {
  it('inside a text field the browser keeps its own undo', async () => {
    const s = await movedOne();
    // The board page has no text field of its own: any field (an input, a textarea, a contenteditable) counts.
    for (const html of [
      '<input type="text">',
      '<textarea></textarea>',
      '<div contenteditable="true"></div>',
    ]) {
      const host = document.createElement('div');
      host.innerHTML = html;
      document.body.appendChild(host);
      const field = host.firstElementChild as HTMLElement;
      field.focus();
      expect(press({}, field)).toBe(true); // not prevented: the field's own undo runs
      host.remove();
    }
    await settle();
    expect(s.patches()).toHaveLength(1);
  });

  it('while a dialog is open', async () => {
    const s = await movedOne();
    const user = userEvent.setup();
    await user.keyboard('?');
    await screen.findByRole('dialog', { name: 'Keyboard shortcuts' });
    press();
    await settle();
    expect(s.patches()).toHaveLength(1);
  });

  it('switching to the list view and back empties the stack', async () => {
    const s = await movedOne();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'List view' }));
    await user.click(screen.getByRole('button', { name: 'Board view' }));
    await screen.findByRole('group', { name: 'Board' });
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(1);
  });
});

describe('board undo: a new stack for another team', () => {
  it('switching team empties the stack', async () => {
    const s = server(start());
    renderApp(['/orbit/team/WEB?view=board', '/orbit/team/ORB?view=board']);
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    act(() => currentLocation.go(-1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/WEB'));
    act(() => currentLocation.go(1));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/ORB'));
    await screen.findByRole('group', { name: 'Board' });
    press();
    expect(await screen.findByText('Nothing to undo')).toBeInTheDocument();
    expect(s.patches()).toHaveLength(1);
  });
});

describe('board undo: a field change and a create', () => {
  it('P on a card sets the priority; Ctrl+Z sets the old one back', async () => {
    const s = server(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    fireEvent.mouseDown(cardEl(1));
    await user.keyboard('p');
    const dialog = await screen.findByRole('dialog', { name: 'Set priority' });
    await user.click(within(dialog).getByRole('option', { name: /High/ }));
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    expect(s.patches()[0]).toMatchObject({ priority: 2 });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ priority: 0, version: 2 });
    expect(await screen.findByText('Undone: set ORB-1 priority to No priority')).toBeInTheDocument();
  });

  it('a create from a lane: Ctrl+Z cancels it (a member cannot delete) and says so', async () => {
    const s = server(start());
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB?view=board');
    await screen.findByText('Card 1');
    await user.click(within(column('Todo')).getByRole('button', { name: 'Create issue in Todo' }));
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
    press({ key: 'Z', shiftKey: true });
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    // Redo puts it back at the rank it was made with, not on top of the column.
    expect(s.patches()[1]).toEqual({ state: 's2', sortOrder: -1024, version: 2 });
  });

  it('a group drop is ONE undo step for every card', async () => {
    const s = server(start());
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    await user.keyboard('s');
    const dialog = await screen.findByRole('dialog', { name: 'Change status' });
    await user.click(within(dialog).getByRole('option', { name: /Done/ }));
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(
      s
        .patches()
        .slice(2)
        .map((b) => (b as { state: string }).state),
    ).toEqual(['s2', 's2']);
    expect(await screen.findByText('Undone: moved 2 issues to Todo')).toBeInTheDocument();
  });
});

describe('board undo: review follow-ups', () => {
  it('a card whose rank stays 0 (dropped in an empty column) goes back to its PLACE, not the top', async () => {
    const s = server([
      card(5, 'Backlog', { sortOrder: -3000 }),
      card(1, 'Backlog', { sortOrder: 0 }),
      card(6, 'Backlog', { sortOrder: 1000 }),
      card(2, 'Todo'),
    ]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    expect(names('Backlog')).toEqual(['Card 5', 'Card 1', 'Card 6']);
    dragTo(1, middle(0, 1), at(2, 0));
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    expect(s.patches()[0]).toEqual({ state: 'In Progress', version: 1 });
    expect(s.rows.find((r) => r.id === 'i1')!.sortOrder).toBe(0); // the same rank as before
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 0, version: 2 });
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 5', 'Card 1', 'Card 6']));
  });

  it("follows this tab's own later write: move, set priority, undo both (the second sends the chained version)", async () => {
    const s = await movedOne();
    const user = userEvent.setup();
    fireEvent.mouseDown(cardEl(1));
    await user.keyboard('p');
    const dialog = await screen.findByRole('dialog', { name: 'Set priority' });
    await user.click(within(dialog).getByRole('option', { name: /High/ }));
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ priority: 0, version: 3 });
    await screen.findByText('Undone: set ORB-1 priority to No priority');
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(s.patches()[3]).toEqual({ state: 's1', sortOrder: 1024, version: 4 });
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
  });

  it('a state renamed since: the undo sends the state id and still works', async () => {
    const s = await movedOne();
    setServerStates(allStates.map((x) => (x.id === 's1' ? { ...x, name: 'Icebox' } : x)));
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
    expect(await screen.findByText('Undone: moved ORB-1 to Backlog')).toBeInTheDocument();
    expect(s.rows.find((r) => r.id === 'i1')!.state).toMatchObject({ id: 's1', name: 'Icebox' });
  });

  it('offline: says so, writes nothing, keeps the entry; online again, the press works', async () => {
    const s = await movedOne();
    act(() => onlineManager.setOnline(false));
    press();
    expect(await screen.findByText(/You are offline; nothing was changed/)).toBeInTheDocument();
    await settle();
    expect(s.patches()).toHaveLength(1);
    act(() => onlineManager.setOnline(true));
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
  });

  it('a held key (repeat) on an empty stack shows no toast', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press({ repeat: true });
    press({ repeat: true });
    await settle();
    expect(screen.queryByText('Nothing to undo')).toBeNull();
  });

  it('a press right after the drop (the card still landing) undoes THAT drop', async () => {
    const s = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    press(); // no wait: the drop lands, then its write starts
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[0]).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
    expect(s.patches()[1]).toEqual({ state: 's1', sortOrder: 1024, version: 2 });
  });

  it('a press while a card is still held says so and writes nothing', async () => {
    const s = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    const grab = middle(0, 0);
    fireEvent.pointerDown(cardEl(1), { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
    fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 10, clientY: grab.y + 10 });
    press();
    expect(await screen.findByText('Drop the card first, then undo.')).toBeInTheDocument();
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    expect(s.patches()).toHaveLength(0);
  });

  it('a real group drag (placeMany) is one undo step: both cards go back to their places', async () => {
    const s = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    fireEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    dragTo(3, middle(1, 1), at(3, 0));
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await waitFor(() => expect(names('Done')).toEqual(['Card 2', 'Card 3']));
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(s.patches().slice(2)).toEqual([
      { state: 's2', sortOrder: 3072, version: 2 },
      { state: 's2', sortOrder: 2048, version: 2 },
    ]);
    expect(await screen.findByText('Undone: moved 2 issues to Todo')).toBeInTheDocument();
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 3']));
  });

  it('a multi-card undo where one card changed since: the other is undone, then only it is redone', async () => {
    const s = server(start());
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    await user.keyboard('s');
    const dialog = await screen.findByRole('dialog', { name: 'Change status' });
    await user.click(within(dialog).getByRole('option', { name: /Done/ }));
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    s.rows.find((r) => r.id === 'i2')!.version = 9; // someone else edits ORB-2
    await act(() => client.invalidateQueries({ queryKey: keys.issues('orbit') }));
    await waitFor(() => expect(findIssueInCaches(client, 'orbit', 'i2')?.version).toBe(9));
    press();
    expect(await screen.findByText('Undone: moved ORB-3 to Todo')).toBeInTheDocument();
    expect(await screen.findByText('ORB-2 changed since; undo skipped for it')).toBeInTheDocument();
    expect(s.rows.find((r) => r.id === 'i2')!.state.name).toBe('Done');
    expect(s.rows.find((r) => r.id === 'i3')!.state.name).toBe('Todo');
    const n = s.patches().length;
    press({ key: 'Z', shiftKey: true });
    await waitFor(() => expect(s.patches()).toHaveLength(n + 1));
    expect(s.patches()[n]).toMatchObject({ state: 's4' });
    expect(await screen.findByText('Redone: moved ORB-3 to Done')).toBeInTheDocument();
  });
});

describe('board undo: one card moved twice before a list refetch lands', () => {
  // The drop moves the card in the cache, but its cached rank stays the old one until a refetch lands. Here no
  // refetch lands at all, so the second change must take its "before" rank from the first write's answer.
  const never = new Promise<void>(() => {});

  it('the undo of the second move sends the rank the FIRST move gave (not the one from before it)', async () => {
    let hold = false;
    const s = server(start(), { list: () => (hold ? never : undefined) });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    hold = true;
    dragTo(1, middle(0, 0), at(1, 1)); // between ORB-2 (2048) and ORB-3 (3072): the server gives 2049
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3']));
    await settle();
    expect(s.rows.find((r) => r.id === 'i1')!.sortOrder).toBe(2049);
    dragTo(1, middle(1, 1), at(3, 0)); // to Done
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    expect(s.patches()[1]).toMatchObject({ state: 'Done', version: 2 });
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's2', sortOrder: 2049, version: 3 });
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(s.patches()[3]).toEqual({ state: 's1', sortOrder: 1024, version: 4 });
  });

  it('the same when the second drop comes while the first write is still in flight', async () => {
    let hold = false;
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    const s = server(start(), {
      list: () => (hold ? never : undefined),
      patch: async (_b, n) => {
        if (n === 0) await gate;
        return undefined;
      },
    });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    hold = true;
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3']));
    await settle();
    dragTo(1, middle(1, 1), at(3, 0)); // the first write has no answer yet
    await settle();
    act(() => open());
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await settle();
    expect(s.rows.find((r) => r.id === 'i1')!.state.name).toBe('Done');
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's2', sortOrder: 2049, version: 3 });
  });
});

describe('board undo: the remembered rank, more cases', () => {
  const never = new Promise<void>(() => {});

  it("a newer edit by someone else wins over the rank remembered from this tab's write", async () => {
    const s = server(start());
    const { client } = renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1)); // rank 2049, version 2: remembered
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    // Someone else moves ORB-1 to the bottom of Todo (version 3, rank 5000), and this tab hears of it.
    const row = s.rows.find((r) => r.id === 'i1')!;
    row.version = 3;
    row.sortOrder = 5000;
    await act(() => client.invalidateQueries({ queryKey: keys.issues('orbit') }));
    await waitFor(() => expect(findIssueInCaches(client, 'orbit', 'i1')?.version).toBe(3));
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 1']));
    dragTo(1, middle(1, 2), at(3, 0)); // to Done
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's2', sortOrder: 5000, version: 4 });
  });

  it('a third move of one card with no refetch: each undo sends the rank the move before it gave', async () => {
    let hold = false;
    const s = server(start(), { list: () => (hold ? never : undefined) });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    hold = true;
    dragTo(1, middle(0, 0), at(1, 1)); // Todo, rank 2049
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    dragTo(1, middle(1, 1), at(3, 0)); // Done (empty column): rank 0
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await settle();
    expect(s.rows.find((r) => r.id === 'i1')!.sortOrder).toBe(0);
    dragTo(1, middle(3, 0), at(2, 0)); // In Progress
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toMatchObject({ state: 'In Progress', version: 3 });
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(s.patches()[3]).toEqual({ state: 's4', sortOrder: 0, version: 4 });
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(5));
    expect(s.patches()[4]).toEqual({ state: 's2', sortOrder: 2049, version: 5 });
  });

  it('a no-op write after a drop (unassign an unassigned card) does not replace the remembered rank', async () => {
    let hold = false;
    const s = server(start(), { list: () => (hold ? never : undefined) });
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    hold = true;
    dragTo(1, middle(0, 0), at(1, 1)); // Todo, rank 2049
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    fireEvent.mouseDown(cardEl(1));
    // Unassign a card with no assignee: the write sends nothing and answers with the cached issue, whose
    // version is current but whose rank is still the one from before the drop.
    await user.keyboard('a');
    const dialog = await screen.findByRole('dialog', { name: 'Assign to' });
    await user.click(within(dialog).getByRole('option', { name: /Unassigned/ }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    expect(s.patches()).toHaveLength(1); // nothing was sent
    dragTo(1, middle(1, 1), at(3, 0)); // Done
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    expect(s.patches()[2]).toEqual({ state: 's2', sortOrder: 2049, version: 3 });
  });
});

describe('board undo: a new action while an undo runs', () => {
  it('a kept (failed) undo goes BELOW the new action: the next press undoes the new one', async () => {
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    const s = server(start(), {
      patch: async (_b, n) => {
        if (n !== 1) return undefined;
        await gate; // the undo's write is slow, then fails
        return { status: 503, json: { error: 'down', code: 'unavailable' } };
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(s.patches()).toHaveLength(1));
    await settle();
    press(); // undo of the move: its write hangs
    await waitFor(() => expect(s.patches()).toHaveLength(2));
    fireEvent.mouseDown(cardEl(3));
    await user.keyboard('p');
    const dialog = await screen.findByRole('dialog', { name: 'Set priority' });
    await user.click(within(dialog).getByRole('option', { name: /High/ }));
    await waitFor(() => expect(s.patches()).toHaveLength(3));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await settle();
    act(() => open());
    expect(await screen.findByText(/Could not undo ORB-1/)).toBeInTheDocument();
    await settle();
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(4));
    expect(s.patches()[3]).toEqual({ priority: 0, version: 2 }); // ORB-3's priority, the newest action
    press();
    await waitFor(() => expect(s.patches()).toHaveLength(5));
    expect(s.patches()[4]).toEqual({ state: 's1', sortOrder: 1024, version: 2 }); // then the kept move
  });
});

describe('placeMany ends its undo batch whatever happens', () => {
  it('a throw inside it still calls end, so the undo does not wait for ever', async () => {
    server(start());
    const recorder: WriteRecorder = { begin: vi.fn(() => ({})), applied: vi.fn(), end: vi.fn() };
    let actions!: ReturnType<typeof useIssueActions>;
    function Harness() {
      actions = useIssueActions(undefined, recorder);
      return null;
    }
    renderAt(<Harness />, { path: '/orbit/issues' });
    const boom = new Proxy(
      {},
      {
        get: () => {
          throw new Error('boom');
        },
      },
    );
    await expect(actions.placeMany([card(2, 'Todo')], 'Done', boom)).rejects.toThrow('boom');
    expect(recorder.begin).toHaveBeenCalledTimes(1);
    expect(recorder.end).toHaveBeenCalledTimes(1);
  });
});
