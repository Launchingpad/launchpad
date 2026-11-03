import { useCallback, useEffect, useMemo, useRef } from 'react';
import { onlineManager, useQueryClient } from '@tanstack/react-query';
import { client, unwrap } from '../../api/client';
import { useOrgSlug } from '../../api/org';
import { findIssueInCaches, useAllStates, useMembers, useUpdateIssue, waitBounded } from '../../api/queries';
import { ApiError, errorMessage, isConflict } from '../../api/errors';
import type { Issue } from '../../api/types';
import { isTypingTarget, modalOpen } from '../../lib/hotkeys';
import { isApple } from '../../lib/platform';
import { priorityLabel } from '../../lib/priority';
import { useToast } from '../../ui/Toast';
import { useUi } from '../shell/ui-context';
import type { WriteRecorder } from './actions';
import { boardDragState } from './boardDrag';
import { listDragState } from './listDrag';
import {
  bodyOf,
  changeOf,
  createdChange,
  describeEntry,
  emptyStacks,
  record,
  settle,
  take,
  type ChangeOutcome,
  type Direction,
  type FieldSet,
  type UndoChange,
} from './undoStack';

// LAUN-236, LAUN-237: Cmd+Z (macOS) or Ctrl+Z undoes the last change made on the board or the list; Shift (or
// Ctrl+Y) redoes it. ONE hook serves both views. The rules: docs/internal/decisions-T43-board-undo.md.

/** One action in flight: its writes share one undo entry. */
interface Batch {
  gen: number;
  pending: number;
  open: boolean; // still in the task that started it: more writes may join
  changes: UndoChange[];
  settled: Promise<void>;
  resolve: () => void;
}

/** How long a press waits for the change before it to be saved (a drop, a picker). */
const SAVE_WAIT_MS = 10_000;

/**
 * The letter of the key. A Latin layout (QWERTY, AZERTY, QWERTZ) gives it in `key`, and that wins: on AZERTY
 * the key labelled Z is `code` KeyW. A non-Latin layout (Cyrillic, Greek, Hebrew...) gives another letter in
 * `key`; then the physical key (`code` KeyZ / KeyY) counts, as browsers and the OS do for Cmd+Z.
 */
function letterOf(e: KeyboardEvent): string {
  const k = e.key.toLowerCase();
  if (/^[a-z]$/.test(k)) return k;
  const m = /^Key([A-Z])$/.exec(e.code ?? '');
  return m ? m[1]!.toLowerCase() : k;
}

/** The key press asks for undo or redo, or is not ours. */
export function undoKey(e: KeyboardEvent, apple = isApple()): Direction | null {
  if (e.altKey) return null;
  const mod = apple ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey;
  if (!mod) return null;
  const k = letterOf(e);
  if (k === 'z') return e.shiftKey ? 'redo' : 'undo';
  if (k === 'y' && !apple && !e.shiftKey) return 'redo';
  return null;
}

/** Something else owns the keys now: a field (its own text undo), a dialog, a menu, a picker. */
export function undoBlocked(e: KeyboardEvent): boolean {
  if (isTypingTarget(e.target) || isTypingTarget(document.activeElement)) return true;
  const el = e.target instanceof Element ? e.target : null;
  if (el?.closest('[contenteditable]:not([contenteditable="false"])')) return true;
  if (modalOpen()) return true;
  if (e.target instanceof HTMLElement && e.target.closest('[data-no-hotkeys]')) return true;
  return !!document.querySelector('[role="dialog"], [aria-modal="true"], [role="menu"], [role="listbox"]');
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/** A drag on either view: held (the pointer still holds a card or a row), landing, or idle. */
function dragState(): 'held' | 'landing' | 'idle' {
  const b = boardDragState();
  const l = listDragState();
  if (b === 'held' || l === 'held') return 'held';
  return b === 'landing' || l === 'landing' ? 'landing' : 'idle';
}

export type UndoView = 'board' | 'list';

/** The words of each view: what a row is, and what holds the rows. */
const WORDS: Record<UndoView, { thing: string; place: string }> = {
  board: { thing: 'card', place: 'column' },
  list: { thing: 'issue', place: 'group' },
};

type Ranks = Map<string, { version: number; sortOrder: number }>;

/**
 * Keeps the rank of the newest version of the issue seen. Only a NEWER version replaces it: a write that sent
 * nothing (a no-op patch) answers with the cached issue, whose version is current but whose rank may be stale.
 */
function noteRank(ranks: Ranks, i: Issue) {
  const k = ranks.get(i.id);
  if (!k || i.version > k.version) ranks.set(i.id, { version: i.version, sortOrder: i.sortOrder });
}

/** A server text as a sentence: it ends with a period (or other stop) before more words follow it. */
export const asSentence = (s: string) => {
  const t = s.trim();
  return /[.!?]$/.test(t) ? t : `${t}.`;
};

function outcomeOf(e: unknown): 'stale' | 'gone' | 'keep' | 'refused' {
  if (isConflict(e) || (e instanceof ApiError && e.status === 409)) return 'stale';
  if (e instanceof ApiError && e.status === 404) return 'gone';
  if (!(e instanceof ApiError) || e.status === 0 || e.status === 401 || e.status === 429 || e.status >= 500)
    return 'keep';
  return 'refused';
}

/**
 * The undo of the issue board and list (one hook for both, LAUN-237). `enabled` false: nothing is recorded and
 * the keys are left alone. A change of `scope` or `view` (another team, product, cycle or project page, the
 * other view) empties the stacks; so does leaving the page (the hook unmounts).
 */
export function useIssueUndo({
  enabled = true,
  scope,
  view,
}: {
  enabled?: boolean;
  scope: string;
  view: UndoView;
}) {
  const qc = useQueryClient();
  const org = useOrgSlug();
  const update = useUpdateIssue();
  const toast = useToast();
  const { states } = useAllStates();
  const members = useMembers();
  const { onIssueCreated } = useUi();

  const stacks = useRef(emptyStacks());
  const gen = useRef(0); // a scope change: writes still in flight from before do not record
  const nextId = useRef(1);
  const current = useRef<Batch | null>(null);
  const inFlight = useRef(new Set<Batch>());
  const busy = useRef(false);
  const recorded = useRef(0); // counts new actions: one made while an undo runs starts a new branch
  // The rank the server gave each card on its last write from here. A drop moves the card in the cache but
  // leaves its `sortOrder` as it was until a list refetch lands (and the next write cancels that refetch), so
  // the cached issue can hold a rank that is no longer true. A change records this rank as its "before" when
  // it is at least as new as the issue the page showed (same or higher version).
  const ranks = useRef<Ranks>(new Map());
  const live = useRef({ enabled, states, members: members.data ?? [], words: WORDS[view] });
  live.current = { enabled, states, members: members.data ?? [], words: WORDS[view] };

  useEffect(() => {
    stacks.current = emptyStacks();
    ranks.current.clear();
    gen.current += 1;
  }, [scope, view, enabled]);

  // Leaving the page: the stacks go, and a toast's Undo or Redo button (the toast outlives the page) does nothing.
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    const known = ranks.current; // the same Map for the life of the hook
    return () => {
      mounted.current = false;
      stacks.current = emptyStacks();
      known.clear();
      gen.current += 1;
    };
  }, []);

  const commit = useCallback((b: Batch) => {
    inFlight.current.delete(b);
    if (b.gen === gen.current && b.changes.length) {
      stacks.current = record(stacks.current, { id: nextId.current++, changes: b.changes });
      recorded.current += 1;
    }
    b.resolve();
  }, []);

  const recorder = useMemo<WriteRecorder>(
    () => ({
      begin() {
        if (!live.current.enabled) return null;
        let b = current.current;
        if (!b) {
          let resolve!: () => void;
          const settled = new Promise<void>((r) => (resolve = r));
          const fresh: Batch = { gen: gen.current, pending: 0, open: true, changes: [], settled, resolve };
          b = fresh;
          current.current = fresh;
          inFlight.current.add(fresh);
          // Every write started in this same task (a picker on a selection, Promise.all) joins this batch.
          queueMicrotask(() => {
            fresh.open = false;
            if (current.current === fresh) current.current = null;
            if (fresh.pending === 0) commit(fresh);
          });
        }
        b.pending += 1;
        return b;
      },
      applied(token, before, after, patch) {
        const known = ranks.current.get(before.id);
        const seen =
          known && known.version >= before.version ? { ...before, sortOrder: known.sortOrder } : before;
        noteRank(ranks.current, after);
        const c = changeOf(seen, after, patch);
        if (c) (token as Batch).changes.push(c);
      },
      end(token) {
        const b = token as Batch;
        b.pending -= 1;
        if (b.pending === 0 && !b.open) commit(b);
      },
    }),
    [commit],
  );

  // A create from the New issue dialog while the board or the list is on screen (the lane "+", the C key, the
  // New issue button).
  useEffect(() => {
    if (!enabled) return;
    return onIssueCreated((issue) => {
      stacks.current = record(stacks.current, { id: nextId.current++, changes: [createdChange(issue)] });
      recorded.current += 1;
    });
  }, [enabled, onIssueCreated]);

  const names = useMemo(
    () => ({
      priority: priorityLabel,
      member: (u: string) => live.current.members.find((m) => m.username === u)?.name ?? u,
      get place() {
        return live.current.words.place;
      },
    }),
    [],
  );

  /** The canceled state of a team (the first by position), for the undo of a create. */
  const canceledOf = (teamId: string) =>
    live.current.states
      .filter((s) => s.teamId === teamId && s.type === 'canceled')
      .sort((a, b) => a.position - b.position)[0];

  /**
   * A new issue that has comments or sub-issues now is not canceled by an undo: neither raises its version, so
   * the compare-and-swap would not see them (T43 #7b). Asked of the server at the time of the undo.
   */
  const inUse = async (c: UndoChange): Promise<boolean> => {
    // One after the other, comments first: for a deleted or hidden issue that read is a 404 ("gone"), whereas the
    // sub-issue list answers 400 "unknown parent". Read in parallel, whichever failed first would pick the toast.
    const comments = await unwrap(
      client.GET('/api/v1/orgs/{org}/issues/{issue}/comments', {
        params: { path: { org, issue: c.issueId } },
      }),
    );
    if (comments.items.length > 0) return true;
    try {
      const children = await unwrap(
        client.GET('/api/v1/orgs/{org}/issues', {
          params: { path: { org }, query: { parent: c.issueId, limit: 1 } },
        }),
      );
      return children.items.length > 0;
    } catch (e) {
      // The parent became unknown between the two reads (deleted, or its team hidden): the same as a 404.
      if (e instanceof ApiError && e.status === 400 && /unknown parent/i.test(e.message))
        throw new ApiError(404, 'not_found', e.message);
      throw e;
    }
  };

  /** One write of an undo or a redo, with the compare-and-swap on the version the change saw last. */
  const write = async (c: UndoChange, f: FieldSet): Promise<Issue> => {
    const cached = findIssueInCaches(qc, org, c.issueId);
    const issue: Issue = cached ?? ({ id: c.issueId, identifier: c.identifier, version: c.version } as Issue);
    const optimistic: Partial<Issue> = {};
    if (f.state !== undefined) {
      const st = live.current.states.find((s) => s.teamId === c.teamId && s.id === f.state);
      if (st) optimistic.state = { id: st.id, name: st.name, type: st.type, color: st.color };
    }
    if (f.priority !== undefined) {
      optimistic.priority = f.priority;
      optimistic.priorityLabel = priorityLabel(f.priority) as Issue['priorityLabel'];
    }
    if (f.assignee !== undefined) {
      const m = live.current.members.find((x) => x.username === f.assignee);
      if (f.assignee === null) optimistic.assignee = null;
      else if (m) optimistic.assignee = { id: m.id, username: m.username, name: m.name };
    }
    if (f.sortOrder !== undefined) optimistic.sortOrder = f.sortOrder;
    return update.mutateAsync({
      issue,
      patch: bodyOf(f),
      baseVersion: c.version,
      optimistic: Object.keys(optimistic).length ? optimistic : undefined,
      touch: f.state !== undefined || f.priority !== undefined,
    });
  };

  const run = useCallback(
    // `only`: a toast's button acts on ITS entry, and only while that entry is still the top of the stack.
    async (dir: Direction, repeat = false, only?: number) => {
      if (!mounted.current) return; // a toast's button after the page was left
      if (busy.current) return; // one undo or redo at a time: a second press while one runs does nothing
      if (dragState() === 'held') {
        if (!repeat) toast.show(`Drop the ${live.current.words.thing} first, then ${dir}.`);
        return;
      }
      busy.current = true;
      try {
        // Offline the writes would wait (TanStack pauses them) and the press would seem dead: say so, keep all.
        if (!onlineManager.isOnline()) {
          if (!repeat)
            toast.show(
              `You are offline; nothing was changed. Press again when you are connected to ${dir}.`,
              {
                kind: 'error',
              },
            );
          return;
        }
        // A drop that has just been released lands for a moment before its write starts: wait for it.
        for (let t = 0; dragState() === 'landing' && t < 40; t++) await sleep(25);
        // A change still being saved is not on the stack yet: wait for it, so the press undoes IT.
        const deadline = Date.now() + SAVE_WAIT_MS;
        while (inFlight.current.size && Date.now() < deadline) {
          const all = Promise.all([...inFlight.current].map((b) => b.settled));
          await waitBounded(all, Math.max(1, deadline - Date.now()));
        }
        if (inFlight.current.size) {
          toast.show(`The last change is still being saved; press again to ${dir}.`, { kind: 'error' });
          return;
        }
        const [entry, rest] = take(stacks.current, dir);
        if (only !== undefined && entry?.id !== only) {
          toast.show(`That change is no longer the last one; nothing was changed. Use the keys to ${dir}.`);
          return;
        }
        if (!entry) {
          if (!repeat) toast.show(dir === 'undo' ? 'Nothing to undo' : 'Nothing to redo');
          return;
        }
        stacks.current = rest;
        const before = recorded.current;
        const startGen = gen.current;
        const order = dir === 'undo' ? [...entry.changes].reverse() : entry.changes;
        const outcomes = new Map<UndoChange, ChangeOutcome>();
        const said: { f: FieldSet; id: string }[] = [];
        const problems: string[] = [];
        let kept = false;
        for (const c of order) {
          if (kept) {
            outcomes.set(c, { kind: 'keep' }); // the network is gone: the rest stays for the next press
            continue;
          }
          let f = dir === 'undo' ? c.undo : c.redo;
          if (c.created && dir === 'undo') {
            const st = canceledOf(c.teamId);
            if (!st) {
              outcomes.set(c, { kind: 'drop' });
              problems.push(`${c.identifier} cannot be undone: its team has no canceled state`);
              continue;
            }
            f = { state: st.id, words: { state: st.name } };
          }
          try {
            if (c.created && dir === 'undo' && (await inUse(c))) {
              outcomes.set(c, { kind: 'drop' });
              problems.push(`${c.identifier} has comments or sub-issues now; undo skipped`);
              continue;
            }
            const after = await write(c, f);
            noteRank(ranks.current, after);
            outcomes.set(c, { kind: 'done', version: after.version });
            said.push({ f, id: c.identifier });
          } catch (e) {
            const why = outcomeOf(e);
            if (why === 'keep') {
              kept = true;
              outcomes.set(c, { kind: 'keep' });
              problems.push(
                `Could not ${dir} ${c.identifier}: ${asSentence(errorMessage(e))} Press the keys again to retry.`,
              );
            } else {
              outcomes.set(c, { kind: 'drop' });
              problems.push(
                why === 'stale'
                  ? entry.changes.length === 1
                    ? `This ${live.current.words.thing} changed since; ${dir} skipped`
                    : `${c.identifier} changed since; ${dir} skipped for it`
                  : why === 'gone'
                    ? `${c.identifier} is gone or hidden from you; ${dir} skipped`
                    : `${c.identifier}: ${dir} refused. ${errorMessage(e)}`,
              );
            }
          }
        }
        // Another team or view meanwhile: its stacks are new, this entry does not belong in them. New actions
        // made while this ran stay on top (`newer`, see `settle`).
        if (gen.current === startGen)
          stacks.current = settle(
            stacks.current,
            dir,
            entry,
            entry.changes.map((c) => outcomes.get(c) ?? { kind: 'keep' }),
            undefined,
            recorded.current - before,
            nextId.current++,
          );
        if (said.length) {
          const created =
            dir === 'undo' && entry.changes.some((c) => c.created && outcomes.get(c)?.kind === 'done');
          const text = `${dir === 'undo' ? 'Undone' : 'Redone'}: ${describeEntry(said, names)}${
            created ? ' (undo cancels a new issue; it does not delete it)' : ''
          }`;
          const back: Direction = dir === 'undo' ? 'redo' : 'undo';
          toast.show(text, {
            ms: 8000,
            action: (
              <button type="button" onClick={() => void runRef.current(back, false, entry.id)}>
                {back === 'redo' ? 'Redo' : 'Undo'}
              </button>
            ),
          });
        }
        for (const p of problems) toast.show(p, { kind: 'error', ms: 8000 });
      } finally {
        busy.current = false;
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [qc, org, update.mutateAsync, toast, names],
  );
  const runRef = useRef(run);
  runRef.current = run;

  useEffect(() => {
    if (!enabled) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.isComposing) return;
      const dir = undoKey(e);
      if (!dir || undoBlocked(e)) return;
      e.preventDefault();
      void runRef.current(dir, e.repeat);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [enabled]);

  return { recorder, undo: () => runRef.current('undo'), redo: () => runRef.current('redo') };
}
