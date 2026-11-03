import { useEffect, useMemo, useRef, type PointerEvent as ReactPointerEvent } from 'react';
import type { Issue } from '../../api/types';
import { markDragged, setDragActive } from './cardLink';
import { columnAt, dropIndex, edgeScroll, placementAt, type Placement, type Slot } from './dropPosition';
import styles from './issues.module.css';

// LAUN-29: the pointer-driven board drag. Mouse, touch and pen go through pointer events.
//
// Nothing in a drag goes through React state: a card is moved by a style write, so a drag over a
// column of 200+ cards renders no card at all. While dragging:
//  - a copy of the card (the "clone") follows the pointer in a fixed layer above the board;
//  - the real card stays in its slot, hidden, and the cards under the gap slide with a CSS
//    transform (translateY), so the layout of the board never changes (no layout shift);
//  - one gray placeholder (<li data-gap>, absolutely positioned) marks the gap in the target column.
// On drop the clone lands on the placeholder, then ONE update is sent (state and place together)
// and the visuals stay until the optimistic update has rendered (`columnsChanged`).

const PRESS_PX = 4; // mouse and pen: this far before a press becomes a drag, so a click still works
const TOUCH_SLOP_PX = 8; // touch: moving more than this before the hold means the person scrolls
const TOUCH_HOLD_MS = 250; // touch: press and hold to pick a card up
const LAND_MS = 160;
const SETTLE_FALLBACK_MS = 500;

export interface DropRequest {
  issue: Issue;
  /** The destination column's key (the state name, or the key of an assignee, priority, label or project column). */
  column: string;
  placement: Placement;
  /** 0-based place in the column (the dragged card left out) and the column size with it. */
  index: number;
  count: number;
  /**
   * LAUN-171: Ctrl or Command was held when the pointer was released ("also change priority"), and the card the
   * dropped card landed next to (the one above it, else the one below; null in an empty column). Whether this
   * means anything is the board's call: it depends on the sort and on what the columns are.
   */
  alsoPriority: boolean;
  neighbourId: string | null;
  /**
   * LAUN-61: set when the dragged card is one of several selected cards. `issues` is the whole group in
   * board order; `base` is the place for each team (the first card of that team goes there, the rest
   * follow it). `issue` and `placement` are then the card you held and its own place.
   */
  group?: { issues: Issue[]; base: Record<string, Placement> };
}

export interface BoardDragOptions {
  getBoard: () => HTMLElement | null;
  onDrop: (r: DropRequest) => void;
  announce: (text: string) => void;
  /**
   * LAUN-167: false when the sort is not Manual. A drop where every held card is already in the target column
   * is then a cancel (no place to save), with its own announcement. Absent: true.
   */
  reorder?: () => boolean;
  /** LAUN-171: is the card in the column with this key? Absent: the column is its state (name). */
  isIn?: (issue: Issue, columnKey: string) => boolean;
  /**
   * LAUN-171: true when holding Ctrl or Command at the drop means something ("also change priority"). Then a drop in
   * the card's own column is not a cancel when the key is held: the priority can still change.
   */
  alsoPriority?: () => boolean;
  /** LAUN-171: what a screen reader hears when a drop in the card's own column is cancelled. */
  sameColumnSay?: (columnName: string) => string;
  /** LAUN-171: a drag started (true) or ended (false), for the hint over the board. */
  onActive?: (on: boolean) => void;
}

interface Item {
  id: string;
  teamId: string;
  el: HTMLElement;
  top: number;
  height: number;
  dy: number;
}
interface Col {
  key: string;
  section: HTMLElement;
  ul: HTMLElement;
  gapEl: HTMLElement | null;
  items: Item[];
  rowGap: number;
  srcIdx: number; // index of the dragged card in this column, -1 when it is not here
  slots: Slot[]; // the closed layout without the dragged card
}
type Phase = 'idle' | 'pressed' | 'dragging' | 'landing' | 'committed';

const reducedMotion = () =>
  typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// LAUN-236: the board's undo asks whether a drag is under way (the pointer still holds a card) or is landing
// (released, the drop not yet sent). Read from the machines, not from the body class, which stays a little
// longer.
const machines = new Set<BoardDrag>();
export function boardDragState(): 'held' | 'landing' | 'idle' {
  let landing = false;
  for (const m of machines) {
    if (m.phase === 'pressed' || m.phase === 'dragging') return 'held';
    if (m.phase === 'landing') landing = true;
  }
  return landing ? 'landing' : 'idle';
}

/** The drag machine. One per board; `useBoardDrag` owns it. Exported for tests. */
export class BoardDrag {
  phase: Phase = 'idle';
  private o: BoardDragOptions;
  // the press
  private pointerId = -1;
  private pointerType = 'mouse';
  private startX = 0;
  private startY = 0;
  private x = 0;
  private y = 0;
  private card: HTMLElement | null = null;
  private issue: Issue | null = null;
  private group: Issue[] = []; // LAUN-61: the selected cards that go with it (this one included), or []
  private groupEls: HTMLElement[] = [];
  private holdTimer: ReturnType<typeof setTimeout> | undefined;
  // the drag
  private clone: HTMLElement | null = null;
  private offX = 0;
  private offY = 0;
  private w = 0;
  private h = 0;
  private src: Col | null = null;
  private cols = new Map<string, Col>();
  private target: { col: Col; t: number } | null = null;
  private raf = 0;
  private settleTimer: ReturnType<typeof setTimeout> | undefined;
  private landTimer: ReturnType<typeof setTimeout> | undefined;
  private mod = false; // Ctrl or Command at the release

  constructor(o: BoardDragOptions) {
    this.o = o;
    machines.add(this);
  }
  setOptions(o: BoardDragOptions) {
    this.o = o;
    machines.add(this);
  }

  // ---- the press -------------------------------------------------------------------------------

  press(e: ReactPointerEvent<HTMLElement>, issue: Issue, group: Issue[] = []) {
    if (this.phase !== 'idle') return;
    // A group moves with a mouse or a pen. On touch the selected cards do not drag: the page scrolls
    // and the bulk pickers (S, A, L, P) change them (LAUN-61).
    if (e.pointerType === 'touch' && group.length > 1) return;
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if ((e.target as Element).closest('input, button, select, textarea, label')) return;
    setDragActive(false); // a new press: any older drag is over
    this.phase = 'pressed';
    this.pointerId = e.pointerId;
    this.pointerType = e.pointerType;
    this.startX = this.x = e.clientX;
    this.startY = this.y = e.clientY;
    this.card = e.currentTarget;
    this.issue = issue;
    this.group = group.length > 1 ? group : [];
    window.addEventListener('pointermove', this.onMove);
    window.addEventListener('pointerup', this.onUp);
    window.addEventListener('pointercancel', this.onCancel);
    window.addEventListener('lostpointercapture', this.onLost);
    window.addEventListener('blur', this.onBlur);
    window.addEventListener('keydown', this.onKey, true);
    window.addEventListener('contextmenu', this.onContextMenu);
    window.addEventListener('touchmove', this.onTouchMove, { passive: false });
    if (e.pointerType === 'touch') {
      this.holdTimer = setTimeout(() => {
        if (this.phase === 'pressed') this.begin();
      }, TOUCH_HOLD_MS);
    }
  }

  private onMove = (e: PointerEvent) => {
    if (e.pointerId !== this.pointerId) return;
    this.x = e.clientX;
    this.y = e.clientY;
    if (this.phase === 'pressed') {
      const moved = Math.hypot(this.x - this.startX, this.y - this.startY);
      if (this.pointerType === 'touch') {
        if (moved > TOUCH_SLOP_PX) this.forget(); // the person scrolls: not a drag
      } else if (moved >= PRESS_PX) {
        this.begin();
      }
      return;
    }
    if (this.phase === 'dragging') this.update();
  };

  private onUp = (e: PointerEvent) => {
    if (e.pointerId !== this.pointerId) return;
    if (this.phase === 'pressed') {
      this.forget(); // a click: the card's own click handlers run
      return;
    }
    if (this.phase !== 'dragging') return;
    this.x = e.clientX;
    this.y = e.clientY;
    this.mod = e.metaKey || e.ctrlKey;
    this.update();
    this.drop();
  };

  private onCancel = (e: PointerEvent) => {
    if (e.pointerId !== this.pointerId) return;
    if (this.phase === 'pressed') this.forget();
    else if (this.phase === 'dragging') this.cancel(true);
  };

  // The browser took the pointer away (a dialog, a context menu, the window lost focus): cancel.
  private onLost = (e: PointerEvent) => {
    if (e.pointerId === this.pointerId && this.phase === 'dragging') this.cancel(false);
  };
  private onBlur = () => {
    if (this.phase === 'dragging') this.cancel(false);
    else if (this.phase === 'pressed') this.forget();
  };

  private onKey = (e: KeyboardEvent) => {
    if (e.key !== 'Escape') return;
    if (this.phase === 'dragging') {
      e.preventDefault();
      e.stopPropagation(); // the page's own Escape (clear the selection) stays out of it
      this.cancel(false);
    }
  };

  private onContextMenu = (e: Event) => {
    if (this.phase !== 'idle' && this.pointerType === 'touch') e.preventDefault(); // long-press menu
  };

  private onTouchMove = (e: TouchEvent) => {
    // Once a card is picked up, the finger moves the card and not the page.
    if (this.phase === 'dragging' && e.cancelable) e.preventDefault();
  };

  private onScroll = () => {
    if (this.phase === 'dragging') this.update();
  };

  private onSelectStart = (e: Event) => e.preventDefault();

  /** A press that did not become a drag: nothing to clean but the listeners. */
  private forget() {
    clearTimeout(this.holdTimer);
    this.removeListeners();
    this.phase = 'idle';
    this.card = null;
    this.issue = null;
    this.group = [];
  }

  private removeListeners() {
    window.removeEventListener('pointermove', this.onMove);
    window.removeEventListener('pointerup', this.onUp);
    window.removeEventListener('pointercancel', this.onCancel);
    window.removeEventListener('lostpointercapture', this.onLost);
    window.removeEventListener('blur', this.onBlur);
    window.removeEventListener('keydown', this.onKey, true);
    window.removeEventListener('contextmenu', this.onContextMenu);
    window.removeEventListener('touchmove', this.onTouchMove);
    window.removeEventListener('scroll', this.onScroll, true);
    document.removeEventListener('selectstart', this.onSelectStart);
  }

  // ---- the drag --------------------------------------------------------------------------------

  private begin() {
    clearTimeout(this.holdTimer);
    const card = this.card;
    const board = this.o.getBoard();
    const section = card?.closest<HTMLElement>('[data-column]');
    if (!card || !board || !section || !this.issue) return this.forget();
    this.phase = 'dragging';
    this.mod = false;
    this.o.onActive?.(true);
    setDragActive(true);
    window.getSelection?.()?.removeAllRanges();
    document.addEventListener('selectstart', this.onSelectStart);
    window.addEventListener('scroll', this.onScroll, true);
    try {
      card.setPointerCapture(this.pointerId);
    } catch {
      // The pointer is gone already, or the browser has no capture: the window listeners still work.
    }
    const r = card.getBoundingClientRect();
    this.offX = this.startX - r.left;
    this.offY = this.startY - r.top;
    this.w = r.width;
    this.h = card.offsetHeight || r.height;
    const clone = card.cloneNode(true) as HTMLElement;
    clone.removeAttribute('data-issue-id');
    clone.removeAttribute('data-team-id');
    clone.setAttribute('aria-hidden', 'true');
    clone.classList.add(styles.dragClone!);
    clone.querySelectorAll('a, input, button').forEach((n) => n.setAttribute('tabindex', '-1'));
    clone.style.width = `${r.width}px`;
    clone.style.height = `${r.height}px`;
    if (this.group.length > 1) {
      // The count badge, and the other selected cards dimmed in place (attributes: a React re-render keeps them).
      const badge = document.createElement('span');
      badge.className = styles.dragBadge!;
      badge.textContent = String(this.group.length);
      clone.appendChild(badge);
      const ids = new Set(this.group.map((g) => g.id));
      this.groupEls = Array.from(board.querySelectorAll<HTMLElement>('[data-issue-id]')).filter(
        (el) => ids.has(el.dataset.issueId!) && el !== card,
      );
      for (const el of this.groupEls) el.setAttribute('data-drag-group', '');
    }
    document.body.appendChild(clone);
    this.clone = clone;
    document.body.classList.add(styles.dragActive!);
    card.setAttribute('data-drag-source', ''); // not a class: React rewrites className on a re-render
    this.src = this.col(section);
    this.update();
    this.loop();
  }

  private col(section: HTMLElement): Col {
    const key = section.dataset.column ?? '';
    let c = this.cols.get(key);
    if (c) return c;
    const ul = section.querySelector<HTMLElement>('[data-cards]')!;
    const dragged = this.issue!.id;
    const items: Item[] = [];
    for (const li of Array.from(ul.children) as HTMLElement[]) {
      if (!li.dataset.issueId) continue;
      items.push({
        id: li.dataset.issueId,
        teamId: li.dataset.teamId ?? '',
        el: li,
        top: li.offsetTop,
        height: li.offsetHeight,
        dy: 0,
      });
    }
    const rowGap = parseFloat(getComputedStyle(ul).rowGap) || 6;
    const srcIdx = items.findIndex((i) => i.id === dragged);
    const closed = this.h + rowGap;
    const slots: Slot[] = [];
    items.forEach((i, k) => {
      if (k === srcIdx) return;
      slots.push({ id: i.id, top: i.top - (srcIdx >= 0 && k > srcIdx ? closed : 0), height: i.height });
    });
    c = {
      key,
      section,
      ul,
      gapEl: section.querySelector<HTMLElement>('[data-gap]'),
      items,
      rowGap,
      srcIdx,
      slots,
    };
    this.cols.set(key, c);
    return c;
  }

  /** The column under the pointer, or null outside the board. */
  private columnUnder(): HTMLElement | null {
    const board = this.o.getBoard();
    if (!board) return null;
    const b = board.getBoundingClientRect();
    if (this.y < b.top || this.y > b.bottom || this.x < b.left || this.x > b.right) return null;
    const secs = Array.from(board.querySelectorAll<HTMLElement>('section[data-column]'));
    // The gap between two columns belongs to the one on its left: a drop there still counts.
    const spans = secs.map((s, k) => {
      const r = s.getBoundingClientRect();
      const next = secs[k + 1]?.getBoundingClientRect();
      return {
        key: String(k),
        left: k === 0 ? Number.NEGATIVE_INFINITY : r.left,
        right: next ? next.left : Number.POSITIVE_INFINITY,
      };
    });
    const hit = columnAt(this.x, spans);
    return hit === null ? null : (secs[Number(hit)] ?? null);
  }

  /** Move the clone, find the target and apply the gap. Cheap: no React, a few style writes at most. */
  private update() {
    const clone = this.clone;
    const src = this.src;
    if (!clone || !src || !this.card?.isConnected) return;
    clone.style.transform = `translate3d(${this.x - this.offX}px, ${this.y - this.offY}px, 0)`;
    const section = this.columnUnder();
    if (!section) return this.apply(null);
    const col = this.col(section);
    const rect = col.ul.getBoundingClientRect();
    const centre = this.y - this.offY + this.h / 2 - rect.top + col.ul.scrollTop;
    this.apply({ col, t: dropIndex(centre, col.slots) });
  }

  private apply(target: { col: Col; t: number } | null) {
    const src = this.src!;
    const valid = target !== null;
    const tg = target ?? { col: src, t: Math.max(0, src.srcIdx) };
    const prev = this.target;
    if (prev && prev.col === tg.col && prev.t === tg.t && this.valid === valid) return;
    this.target = tg;
    this.valid = valid;
    for (const col of this.cols.values()) {
      const step = this.h + col.rowGap;
      col.items.forEach((it, k) => {
        // LAUN-171: a column with no gap promises no place (its order is not saved): the cards stay put.
        if (!col.gapEl) return;
        if (k === col.srcIdx) return;
        let dy = 0;
        if (col.srcIdx >= 0 && k > col.srcIdx) dy -= step;
        const rem = col.srcIdx >= 0 && k > col.srcIdx ? k - 1 : k;
        if (col === tg.col && rem >= tg.t) dy += step;
        if (it.dy !== dy) {
          it.dy = dy;
          it.el.style.transform = dy ? `translateY(${dy}px)` : '';
        }
      });
      const here = col === tg.col;
      col.section.classList.toggle(styles.dropTarget!, here && valid);
      if (col.gapEl) {
        if (here) {
          const s = col.slots;
          const top =
            tg.t < s.length
              ? s[tg.t]!.top
              : s.length
                ? s[s.length - 1]!.top + s[s.length - 1]!.height + col.rowGap
                : 0;
          col.gapEl.style.display = 'block';
          col.gapEl.style.top = `${top}px`;
          col.gapEl.style.height = `${this.h}px`;
        } else {
          col.gapEl.style.display = 'none';
        }
      }
    }
  }
  private valid = false;

  /** Edge auto-scroll: the board sideways, the target column up and down. */
  private loop = () => {
    if (this.phase !== 'dragging') return;
    const board = this.o.getBoard();
    if (board) {
      const b = board.getBoundingClientRect();
      let scrolled = false;
      const dx = edgeScroll(this.x, b.left, b.right);
      if (dx && this.y >= b.top && this.y <= b.bottom) {
        const before = board.scrollLeft;
        board.scrollLeft += dx;
        scrolled ||= board.scrollLeft !== before;
      }
      const ul = this.valid ? this.target?.col.ul : undefined;
      if (ul) {
        const u = ul.getBoundingClientRect();
        const dy = edgeScroll(this.y, u.top, u.bottom);
        if (dy) {
          const before = ul.scrollTop;
          ul.scrollTop += dy;
          scrolled ||= ul.scrollTop !== before;
        }
      }
      if (scrolled) this.update();
    }
    this.raf = requestAnimationFrame(this.loop);
  };

  // ---- the end ---------------------------------------------------------------------------------

  private drop() {
    setDragActive(false);
    markDragged(); // the click that follows the pointerup is not a click
    this.removeListeners();
    const tg = this.target;
    const issue = this.issue!;
    cancelAnimationFrame(this.raf);
    if (!this.valid || !tg) return this.cancel(true); // outside the board
    const col = tg.col;
    const same = this.group.length < 2 && col === this.src && tg.t === col.srcIdx;
    if (same) return this.cancel(true); // dropped where it was: nothing to send
    if (this.o.reorder && !this.o.reorder()) {
      const held = this.group.length > 1 ? this.group : [issue];
      const isIn = this.o.isIn ?? ((g: Issue, k: string) => g.state.name === k);
      if (held.every((g) => isIn(g, col.key)) && !(this.mod && this.o.alsoPriority?.()))
        return this.cancel(
          true,
          this.o.sameColumnSay?.(col.section.getAttribute('data-name') ?? col.key) ??
            `Reordering inside ${col.section.getAttribute('data-name') ?? col.key} needs the Manual sort`,
        );
    }
    const ids = new Set(this.group.map((g) => g.id));
    ids.add(issue.id);
    const rest = col.items.filter((i) => !ids.has(i.id));
    // `tg.t` counts the cards without the held one; the group's other cards are leaving too.
    const at = col.items
      .filter((i) => i.id !== issue.id)
      .slice(0, tg.t)
      .filter((i) => !ids.has(i.id)).length;
    let req: DropRequest;
    if (this.group.length > 1) {
      const group = this.group;
      const here = col.items.map((i) => i.id);
      const after = [
        ...rest.slice(0, at).map((i) => i.id),
        ...group.map((g) => g.id),
        ...rest.slice(at).map((i) => i.id),
      ];
      // Every card is already in this column in exactly this order: nothing to send.
      if (group.every((g) => here.includes(g.id)) && after.join() === here.join()) return this.cancel(true);
      const base: Record<string, Placement> = {};
      for (const g of group) base[g.team.id] ??= placementAt(rest, at, g.team.id);
      req = {
        issue,
        column: col.key,
        placement: base[issue.team.id]!,
        index: at,
        count: rest.length + group.length,
        alsoPriority: this.mod,
        neighbourId: (rest[at - 1] ?? rest[at])?.id ?? null,
        group: { issues: group, base },
      };
    } else {
      const placement = placementAt(rest, tg.t, issue.team.id);
      req = {
        issue,
        column: col.key,
        placement,
        index: tg.t,
        count: rest.length + 1,
        alsoPriority: this.mod,
        neighbourId: (rest[tg.t - 1] ?? rest[tg.t])?.id ?? null,
      };
    }
    this.phase = 'landing';
    this.landOn(col.gapEl, () => {
      this.phase = 'committed';
      this.o.onDrop(req);
      // Keep the picture until the optimistic update is on screen (columnsChanged), else the card
      // would flash back to its old place for a frame.
      this.settleTimer = setTimeout(() => this.finish(), SETTLE_FALLBACK_MS);
    });
  }

  /**
   * Escape, an outside drop, the same spot, pointercancel, lost capture or blur: the card goes back to
   * its slot and a screen reader hears it. `released`: the pointer is already up. If not, the click
   * guard stays on until it is (Escape, then a release much later, must not open the issue).
   */
  private cancel(released: boolean, say?: string) {
    this.removeListeners();
    cancelAnimationFrame(this.raf);
    if (released) {
      setDragActive(false);
      markDragged();
    } else {
      const up = () => {
        window.removeEventListener('pointerup', up, true);
        window.removeEventListener('pointercancel', up, true);
        window.removeEventListener('pointerdown', up, true);
        setDragActive(false);
        markDragged();
      };
      window.addEventListener('pointerup', up, true);
      window.addEventListener('pointercancel', up, true);
      window.addEventListener('pointerdown', up, true);
    }
    if (this.issue)
      this.o.announce(
        say ??
          (this.group.length > 1
            ? `Move of ${this.group.length} issues canceled`
            : `Move of ${this.issue.identifier} canceled`),
      );
    this.apply(null);
    this.phase = 'landing';
    this.landOn(this.src?.gapEl ?? null, () => this.finish());
  }

  private landOn(gap: HTMLElement | null, then: () => void) {
    const clone = this.clone;
    if (!clone || !gap) return then();
    const r = gap.getBoundingClientRect();
    if (reducedMotion()) {
      clone.style.transform = `translate3d(${r.left}px, ${r.top}px, 0)`;
      return then();
    }
    let done = false;
    const end = () => {
      if (done) return;
      done = true;
      clone.removeEventListener('transitionend', end);
      clearTimeout(this.landTimer);
      then();
    };
    clone.style.transition = `transform ${LAND_MS}ms ease, box-shadow ${LAND_MS}ms ease`;
    clone.classList.add(styles.dragLanding!);
    clone.style.transform = `translate3d(${r.left}px, ${r.top}px, 0)`;
    clone.addEventListener('transitionend', end);
    this.landTimer = setTimeout(end, LAND_MS + 80);
  }

  /** The data changed under a drag (an event, a refetch) or the drop's update has rendered. */
  columnsChanged() {
    if (this.phase === 'committed') return this.finish();
    if (this.phase !== 'dragging') return;
    if (!this.card?.isConnected) return this.cancel(false);
    // Measure again from the new layout.
    this.reset();
    this.cols.clear();
    const section = this.card.closest<HTMLElement>('[data-column]');
    if (!section) return this.cancel(false);
    this.src = this.col(section);
    this.target = null;
    this.update();
  }

  private reset() {
    for (const col of this.cols.values()) {
      for (const it of col.items) it.el.style.transform = '';
      col.section.classList.remove(styles.dropTarget!);
      if (col.gapEl) col.gapEl.style.display = 'none';
    }
  }

  private finish() {
    clearTimeout(this.settleTimer);
    clearTimeout(this.landTimer);
    cancelAnimationFrame(this.raf);
    this.removeListeners();
    this.reset();
    this.clone?.remove();
    this.card?.removeAttribute('data-drag-source');
    for (const el of this.groupEls) el.removeAttribute('data-drag-group');
    this.groupEls = [];
    this.group = [];
    document.body.classList.remove(styles.dragActive!);
    this.o.onActive?.(false);
    this.clone = null;
    this.card = null;
    this.issue = null;
    this.src = null;
    this.target = null;
    this.valid = false;
    this.cols.clear();
    this.phase = 'idle';
  }

  destroy() {
    clearTimeout(this.holdTimer);
    if (this.phase !== 'idle') this.finish();
    machines.delete(this);
  }
}

export function useBoardDrag(opts: BoardDragOptions) {
  const drag = useMemo(() => new BoardDrag(opts), []); // eslint-disable-line react-hooks/exhaustive-deps
  const latest = useRef(opts);
  latest.current = opts;
  useEffect(() => {
    drag.setOptions({
      getBoard: () => latest.current.getBoard(),
      onDrop: (r) => latest.current.onDrop(r),
      announce: (t) => latest.current.announce(t),
      reorder: () => latest.current.reorder?.() ?? true,
      isIn: (i, k) => latest.current.isIn?.(i, k) ?? i.state.name === k,
      alsoPriority: () => latest.current.alsoPriority?.() ?? false,
      sameColumnSay: (n) =>
        latest.current.sameColumnSay?.(n) ?? `Reordering inside ${n} needs the Manual sort`,
      onActive: (on) => latest.current.onActive?.(on),
    });
    return () => drag.destroy();
  }, [drag]);
  return drag;
}
