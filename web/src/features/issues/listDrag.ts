import { useEffect, useMemo, useRef, type PointerEvent as ReactPointerEvent } from 'react';
import type { Issue } from '../../api/types';
import { markDragged, setDragActive } from './cardLink';
import { edgeScroll, placementAt, type Placement, type Slot } from './dropPosition';
import { gapTop, listTarget, type ListGroupBox } from './listDropTarget';
import styles from './issues.module.css';

// LAUN-111: the pointer-driven drag of a list row, with sort Manual. Rules: web-decisions.md, LAUN-111.
//
// Like the board drag (boardDrag.ts) nothing in a drag goes through React state. Unlike it:
//  - the groups are STACKED, so the target comes from the pointer's y alone (listDropTarget.ts);
//  - the press starts on the row's HANDLE only (the rest of the row is a stretched link), and the
//    handle has `touch-action: none`, so a finger on the handle moves the row and a finger anywhere
//    else scrolls the page as always (no hold needed, no scroll hijack);
//  - rows do not slide: a copy of the row follows the pointer, the real row stays in its place,
//    dimmed, and ONE line marks where the row will land;
//  - only the LOADED rows are neighbours (infinite scroll): a drop names the loaded row above the
//    line (placeAfter), else the loaded row below it (placeBefore).

const PRESS_PX = 4;
const LAND_MS = 140;
const SETTLE_FALLBACK_MS = 500;

export interface ListDropRequest {
  issue: Issue;
  /** The destination group (state name). */
  column: string;
  placement: Placement;
  /** 0-based place in the group (the dragged row left out) and the group size with it. */
  index: number;
  count: number;
}

export interface ListDragOptions {
  /** The element that holds the groups (position: relative; rows are measured against its top). */
  getList: () => HTMLElement | null;
  /** The scrolling element around the list (edge auto-scroll, and the area a drop may land in). */
  getScroller: () => HTMLElement | null;
  onDrop: (r: ListDropRequest) => void;
  announce: (text: string) => void;
}

interface Box extends ListGroupBox {
  name: string;
  section: HTMLElement;
  rows: { id: string; teamId: string }[]; // every loaded row, the dragged one included
  srcIdx: number; // the dragged row's index in `rows`, -1 when it is not in this group
}
type Phase = 'idle' | 'pressed' | 'dragging' | 'landing' | 'committed';

const reducedMotion = () =>
  typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// LAUN-237: the undo (useIssueUndo.tsx) asks whether a row is held (pressed or dragged) or landing (released,
// the drop not yet sent), as on the board (boardDrag.ts `boardDragState`).
const machines = new Set<ListDrag>();
export function listDragState(): 'held' | 'landing' | 'idle' {
  let landing = false;
  for (const m of machines) {
    if (m.phase === 'pressed' || m.phase === 'dragging') return 'held';
    if (m.phase === 'landing') landing = true;
  }
  return landing ? 'landing' : 'idle';
}

export class ListDrag {
  phase: Phase = 'idle';
  private o: ListDragOptions;
  private pointerId = -1;
  private pointerType = 'mouse';
  private startX = 0;
  private startY = 0;
  private x = 0;
  private y = 0;
  private row: HTMLElement | null = null;
  private handle: HTMLElement | null = null;
  private issue: Issue | null = null;
  private clone: HTMLElement | null = null;
  private line: HTMLElement | null = null;
  private offX = 0;
  private offY = 0;
  private boxes: Box[] = [];
  private srcBox = -1;
  private target: { group: number; index: number } | null = null;
  private valid = false;
  private raf = 0;
  private settleTimer: ReturnType<typeof setTimeout> | undefined;
  private landTimer: ReturnType<typeof setTimeout> | undefined;

  constructor(o: ListDragOptions) {
    this.o = o;
    machines.add(this);
  }
  setOptions(o: ListDragOptions) {
    this.o = o;
    machines.add(this); // StrictMode: the effect runs again after a destroy
  }

  // ---- the press -------------------------------------------------------------------------------

  /** `pointerdown` on a row's handle. */
  press(e: ReactPointerEvent<HTMLElement>, issue: Issue) {
    if (this.phase !== 'idle') return;
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const row = e.currentTarget.closest<HTMLElement>('[data-issue-id]');
    if (!row) return;
    setDragActive(false);
    this.phase = 'pressed';
    this.pointerId = e.pointerId;
    this.pointerType = e.pointerType;
    this.startX = this.x = e.clientX;
    this.startY = this.y = e.clientY;
    this.handle = e.currentTarget;
    this.row = row;
    this.issue = issue;
    window.addEventListener('pointermove', this.onMove);
    window.addEventListener('pointerup', this.onUp);
    window.addEventListener('pointercancel', this.onCancel);
    window.addEventListener('lostpointercapture', this.onLost);
    window.addEventListener('blur', this.onBlur);
    window.addEventListener('keydown', this.onKey, true);
    window.addEventListener('contextmenu', this.onContextMenu);
    window.addEventListener('touchmove', this.onTouchMove, { passive: false });
  }

  private onMove = (e: PointerEvent) => {
    if (e.pointerId !== this.pointerId) return;
    this.x = e.clientX;
    this.y = e.clientY;
    if (this.phase === 'pressed') {
      if (Math.hypot(this.x - this.startX, this.y - this.startY) >= PRESS_PX) this.begin();
      return;
    }
    if (this.phase === 'dragging') this.update();
  };

  private onUp = (e: PointerEvent) => {
    if (e.pointerId !== this.pointerId) return;
    if (this.phase === 'pressed') return this.forget(); // a tap on the handle: nothing
    if (this.phase !== 'dragging') return;
    this.x = e.clientX;
    this.y = e.clientY;
    this.update();
    this.drop();
  };

  private onCancel = (e: PointerEvent) => {
    if (e.pointerId !== this.pointerId) return;
    if (this.phase === 'pressed') this.forget();
    else if (this.phase === 'dragging') this.cancel(true);
  };
  private onLost = (e: PointerEvent) => {
    // A touch has an implicit capture on the element it landed on (an svg in the handle): that one's loss is no cancel.
    if (e.pointerId === this.pointerId && this.phase === 'dragging' && e.target === this.handle)
      this.cancel(false);
  };
  private onBlur = () => {
    if (this.phase === 'dragging') this.cancel(false);
    else if (this.phase === 'pressed') this.forget();
  };
  private onKey = (e: KeyboardEvent) => {
    if (e.key !== 'Escape' || this.phase !== 'dragging') return;
    e.preventDefault();
    e.stopPropagation(); // the page's own Escape (clear the selection) stays out of it
    this.cancel(false);
  };
  private onContextMenu = (e: Event) => {
    if (this.phase !== 'idle') e.preventDefault(); // a long press on the handle must not open a menu
  };
  private onTouchMove = (e: TouchEvent) => {
    if (this.phase === 'dragging' && e.cancelable) e.preventDefault();
  };
  private onScroll = () => {
    if (this.phase === 'dragging') this.update();
  };
  // LAUN-131: a resized window moves every row; measure again so the line and the drop follow.
  private onResize = () => {
    if (this.phase !== 'dragging') return;
    this.measure();
    this.key = '\0';
    this.update();
  };
  private onSelectStart = (e: Event) => e.preventDefault();

  private forget() {
    this.removeListeners();
    this.phase = 'idle';
    this.row = null;
    this.handle = null;
    this.issue = null;
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
    window.removeEventListener('resize', this.onResize);
    document.removeEventListener('selectstart', this.onSelectStart);
  }

  // ---- the drag --------------------------------------------------------------------------------

  private begin() {
    const row = this.row;
    const list = this.o.getList();
    if (!row || !list || !this.issue) return this.forget();
    this.phase = 'dragging';
    setDragActive(true);
    window.getSelection?.()?.removeAllRanges();
    document.addEventListener('selectstart', this.onSelectStart);
    window.addEventListener('scroll', this.onScroll, true);
    window.addEventListener('resize', this.onResize);
    try {
      if (this.pointerType !== 'touch') this.handle?.setPointerCapture(this.pointerId);
    } catch {
      // The pointer is gone, or the browser has no capture: the window listeners still work.
    }
    const r = row.getBoundingClientRect();
    this.offX = this.startX - r.left;
    this.offY = this.startY - r.top;
    const clone = row.cloneNode(true) as HTMLElement;
    clone.removeAttribute('data-issue-id');
    clone.removeAttribute('data-team-id');
    clone.setAttribute('aria-hidden', 'true');
    clone.classList.add(styles.dragClone!, styles.rowClone!);
    clone.querySelectorAll('a, input, button').forEach((n) => n.setAttribute('tabindex', '-1'));
    clone.style.width = `${r.width}px`;
    clone.style.height = `${r.height}px`;
    document.body.appendChild(clone);
    this.clone = clone;
    const line = document.createElement('div');
    line.className = styles.listGap!;
    line.setAttribute('aria-hidden', 'true');
    line.style.display = 'none';
    list.appendChild(line);
    this.line = line;
    document.body.classList.add(styles.dragActive!);
    row.setAttribute('data-drag-source', '');
    this.measure();
    this.update();
    this.loop();
  }

  /** The groups and their loaded rows, in the list's coordinates. Again after the data changed. */
  private measure() {
    const list = this.o.getList();
    if (!list) return;
    const base = list.getBoundingClientRect().top;
    const dragged = this.issue!.id;
    this.boxes = [];
    this.srcBox = -1;
    for (const section of Array.from(list.querySelectorAll<HTMLElement>('section[data-group]'))) {
      const sr = section.getBoundingClientRect();
      const rows: Box['rows'] = [];
      const slots: Slot[] = [];
      let srcIdx = -1;
      for (const li of Array.from(section.querySelectorAll<HTMLElement>('li[data-issue-id]'))) {
        const id = li.dataset.issueId!;
        if (id === dragged) srcIdx = rows.length;
        rows.push({ id, teamId: li.dataset.teamId ?? '' });
        if (id === dragged) continue;
        const lr = li.getBoundingClientRect();
        slots.push({ id, top: lr.top - base, height: lr.height });
      }
      if (srcIdx >= 0) this.srcBox = this.boxes.length;
      this.boxes.push({
        key: section.dataset.group ?? '',
        name: section.dataset.group ?? '',
        top: sr.top - base,
        bottom: sr.bottom - base,
        slots,
        section,
        rows,
        srcIdx,
      });
    }
  }

  private inside(): boolean {
    const sc = this.o.getScroller();
    if (!sc) return false;
    const b = sc.getBoundingClientRect();
    return this.x >= b.left && this.x <= b.right && this.y >= b.top && this.y <= b.bottom;
  }

  private update() {
    const clone = this.clone;
    const list = this.o.getList();
    if (!clone || !list || !this.row?.isConnected) return;
    clone.style.transform = `translate3d(${this.x - this.offX}px, ${this.y - this.offY}px, 0)`;
    const t = this.inside() ? listTarget(this.y - list.getBoundingClientRect().top, this.boxes) : null;
    this.apply(t);
  }

  private apply(t: { group: number; index: number } | null) {
    const key = t ? `${t.group}:${t.index}` : '';
    if (key === this.key) return;
    this.key = key;
    this.target = t;
    this.valid = t !== null;
    this.boxes.forEach((b, k) => b.section.toggleAttribute('data-drop-target', t !== null && k === t.group));
    const line = this.line;
    if (!line) return;
    if (!t) {
      line.style.display = 'none';
      return;
    }
    const box = this.boxes[t.group]!;
    line.style.display = 'block';
    line.style.top = `${gapTop(box, t.index, box.top)}px`;
  }
  private key = '';

  /** Edge auto-scroll: the list's scroller, up and down. */
  private loop = () => {
    if (this.phase !== 'dragging') return;
    const sc = this.o.getScroller();
    if (sc && this.inside()) {
      const b = sc.getBoundingClientRect();
      const dy = edgeScroll(this.y, b.top, b.bottom);
      if (dy) {
        const before = sc.scrollTop;
        sc.scrollTop += dy;
        if (sc.scrollTop !== before) this.update();
      }
    }
    this.raf = requestAnimationFrame(this.loop);
  };

  // ---- the end ---------------------------------------------------------------------------------

  private drop() {
    setDragActive(false);
    markDragged();
    this.removeListeners();
    cancelAnimationFrame(this.raf);
    const tg = this.target;
    const issue = this.issue!;
    if (!this.valid || !tg) return this.cancel(true, 'outside');
    const box = this.boxes[tg.group]!;
    if (tg.group === this.srcBox && tg.index === box.srcIdx) return this.cancel(true, 'same'); // the same place
    const rest = box.rows.filter((r) => r.id !== issue.id);
    const placement = placementAt(rest, tg.index, issue.team.id);
    const req: ListDropRequest = {
      issue,
      column: box.name,
      placement,
      index: tg.index,
      count: rest.length + 1,
    };
    this.phase = 'landing';
    this.landOn(() => {
      this.phase = 'committed';
      this.o.onDrop(req);
      // Keep the picture until the optimistic update is on screen (rowsChanged), else the row would
      // flash back to its old place for a frame.
      this.settleTimer = setTimeout(() => this.finish(), SETTLE_FALLBACK_MS);
    });
  }

  /**
   * End a drag that moves nothing. `why`: Escape, a lost pointer or a blur is a cancel ("canceled"); a drop on
   * the row's own place or outside the list is not (LAUN-131): the person finished the drag.
   */
  private cancel(released: boolean, why: 'cancel' | 'same' | 'outside' = 'cancel') {
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
    if (this.issue) {
      const id = this.issue.identifier;
      this.o.announce(
        why === 'same'
          ? `${id} stays in its place`
          : why === 'outside'
            ? `${id} was not moved: dropped outside the list`
            : `Move of ${id} canceled`,
      );
    }
    this.apply(null);
    this.phase = 'landing';
    this.landOn(() => this.finish(), this.row);
  }

  /** The copy lands on the line (a drop) or on the row it came from (a cancel). */
  private landOn(then: () => void, home: HTMLElement | null = null) {
    const clone = this.clone;
    const list = this.o.getList();
    const tg = this.target;
    if (!clone || !list) return then();
    let top: number;
    if (home?.isConnected) top = home.getBoundingClientRect().top;
    else if (tg && this.boxes[tg.group]) {
      const box = this.boxes[tg.group]!;
      top = list.getBoundingClientRect().top + gapTop(box, tg.index, box.top);
    } else return then();
    const left = home?.isConnected
      ? home.getBoundingClientRect().left
      : this.row?.getBoundingClientRect().left;
    const to = `translate3d(${left ?? 0}px, ${top}px, 0)`;
    if (reducedMotion()) {
      clone.style.transform = to;
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
    clone.style.transform = to;
    clone.addEventListener('transitionend', end);
    this.landTimer = setTimeout(end, LAND_MS + 80);
  }

  /** The data changed under a drag (an event, a refetch, more rows) or the drop's update has rendered. */
  rowsChanged() {
    if (this.phase === 'committed') return this.finish();
    if (this.phase !== 'dragging') return;
    if (!this.row?.isConnected) return this.cancel(false);
    this.measure();
    this.key = '\0'; // measured again: draw the line again
    this.update();
  }

  private finish() {
    clearTimeout(this.settleTimer);
    clearTimeout(this.landTimer);
    cancelAnimationFrame(this.raf);
    this.removeListeners();
    for (const b of this.boxes) b.section.removeAttribute('data-drop-target');
    this.clone?.remove();
    this.line?.remove();
    this.row?.removeAttribute('data-drag-source');
    document.body.classList.remove(styles.dragActive!);
    this.clone = null;
    this.line = null;
    this.row = null;
    this.handle = null;
    this.issue = null;
    this.target = null;
    this.valid = false;
    this.key = '';
    this.boxes = [];
    this.phase = 'idle';
  }

  destroy() {
    if (this.phase !== 'idle') this.finish();
    machines.delete(this);
  }
}

export function useListDrag(opts: ListDragOptions) {
  const drag = useMemo(() => new ListDrag(opts), []); // eslint-disable-line react-hooks/exhaustive-deps
  const latest = useRef(opts);
  latest.current = opts;
  useEffect(() => {
    drag.setOptions({
      getList: () => latest.current.getList(),
      getScroller: () => latest.current.getScroller(),
      onDrop: (r) => latest.current.onDrop(r),
      announce: (t) => latest.current.announce(t),
    });
    return () => drag.destroy();
  }, [drag]);
  return drag;
}
