import { useEffect, type RefObject } from 'react';
import { markDragged, setDragActive } from './cardLink';

// LAUN-55: press on empty board space and drag: the board scrolls sideways with the pointer.
// Mouse and pen only (a finger already scrolls the board). It is separate from boardDrag.ts (the card
// drag, LAUN-29): a press that starts on a card is left to that drag, and the two never run together.

/** Pointer movement (px) before a press is a drag-scroll. Below it, it is a click. */
export const DRAG_SCROLL_THRESHOLD = 4;
/** A release faster than this (px per ms) gets a little momentum. */
export const MOMENTUM_MIN_SPEED = 0.3;
/** The momentum never carries further than this many px, and dies in about this many ms. */
export const MOMENTUM_MAX_PX = 400;
const MOMENTUM_DECAY_PER_MS = 0.006; // the natural rate: a flick of speed v coasts v / decay px
const VELOCITY_WINDOW_MS = 100;

// Anything that is not empty space: a card, a control, a link.
const INTERACTIVE = '[data-issue-id], a, button, input, select, textarea, label, summary, [contenteditable]';

/** True when the pointer is on a scrollbar of `el` or of an ancestor up to `board`. */
function onScrollbar(e: PointerEvent, board: HTMLElement): boolean {
  for (let el = e.target as HTMLElement | null; el; el = el.parentElement) {
    const r = el.getBoundingClientRect();
    const right = r.left + el.clientLeft + el.clientWidth;
    const bottom = r.top + el.clientTop + el.clientHeight;
    if (
      el.offsetWidth - el.clientWidth - 2 * el.clientLeft > 0 &&
      e.clientX >= right &&
      e.clientX < r.right
    ) {
      return true;
    }
    if (
      el.offsetHeight - el.clientHeight - 2 * el.clientTop > 0 &&
      e.clientY >= bottom &&
      e.clientY < r.bottom
    ) {
      return true;
    }
    if (el === board) break;
  }
  return false;
}

export function useDragScroll(ref: RefObject<HTMLElement | null>, activeClass: string) {
  useEffect(() => {
    const board = ref.current;
    if (!board) return;
    let phase: 'idle' | 'pressed' | 'dragging' = 'idle';
    let pointerId = -1;
    let startX = 0;
    let startScroll = 0;
    let samples: Array<{ t: number; x: number }> = [];
    let raf = 0;

    const stopMomentum = () => {
      if (raf) cancelAnimationFrame(raf);
      raf = 0;
    };
    const reduced = () =>
      typeof window.matchMedia === 'function' &&
      window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    const onSelect = (e: Event) => e.preventDefault();
    const onUpClear = () => {
      window.removeEventListener('pointerup', onUpClear, true);
      window.removeEventListener('pointercancel', onUpClear, true);
      setDragActive(false);
    };

    function listen(on: boolean) {
      const f = on ? window.addEventListener.bind(window) : window.removeEventListener.bind(window);
      f('pointermove', onMove);
      f('pointerup', onUp);
      f('pointercancel', onCancel);
      f('lostpointercapture', onCancel);
      f('blur', onCancel);
      f('keydown', onKey as EventListener, true);
      (on ? document.addEventListener : document.removeEventListener).call(document, 'selectstart', onSelect);
    }

    function finish(released: boolean) {
      const was = phase;
      phase = 'idle';
      listen(false);
      board!.classList.remove(activeClass);
      try {
        board!.releasePointerCapture(pointerId);
      } catch {
        // already released
      }
      if (was !== 'dragging') return;
      markDragged(); // the click that follows the release is not a click
      if (released) {
        setDragActive(false);
      } else {
        // Escape, blur or a cancel with the button still down: the click comes at its release.
        window.addEventListener('pointerup', onUpClear, true);
        window.addEventListener('pointercancel', onUpClear, true);
      }
    }

    function onMove(e: PointerEvent) {
      if (e.pointerId !== pointerId) return;
      if (phase === 'pressed') {
        if (Math.abs(e.clientX - startX) < DRAG_SCROLL_THRESHOLD) return;
        phase = 'dragging';
        setDragActive(true);
        board!.classList.add(activeClass);
        window.getSelection?.()?.removeAllRanges();
        try {
          board!.setPointerCapture(pointerId);
        } catch {
          // no capture: the window listeners still work
        }
        // The scroll follows from where the press began, so the first pixels are not lost.
      }
      e.preventDefault();
      board!.scrollLeft = startScroll - (e.clientX - startX);
      samples.push({ t: e.timeStamp, x: e.clientX });
      while (samples.length > 2 && e.timeStamp - samples[0]!.t > VELOCITY_WINDOW_MS) samples.shift();
    }

    function onUp(e: PointerEvent) {
      if (e.pointerId !== pointerId) return;
      const wasDragging = phase === 'dragging';
      let v = 0; // px per ms, the board's direction
      if (wasDragging && samples.length >= 2) {
        const a = samples[0]!;
        const b = samples[samples.length - 1]!;
        // A pointer that stopped before the release has no momentum.
        if (e.timeStamp - b.t <= 50 && b.t > a.t) v = -(b.x - a.x) / (b.t - a.t);
      }
      finish(true);
      if (wasDragging && !reduced() && Math.abs(v) >= MOMENTUM_MIN_SPEED) coast(v);
    }

    function coast(v0: number) {
      // Distance still to go is v0/decay; cap it, so a fast flick stays small.
      const total = Math.min(Math.abs(v0) / MOMENTUM_DECAY_PER_MS, MOMENTUM_MAX_PX);
      const dir = Math.sign(v0);
      const rate = Math.abs(v0) / total; // x(t) = total * (1 - e^(-rate t)): it starts at speed v0
      const from = board!.scrollLeft;
      let t0 = -1;
      let last = from;
      const step = (now: number) => {
        // Someone else scrolled the board (a wheel, the scrollbar, a script): stop, do not fight.
        if (Math.abs(board!.scrollLeft - last) > 1) return void (raf = 0);
        if (t0 < 0) t0 = now;
        const t = now - t0;
        const k = 1 - Math.exp(-t * rate);
        const want = from + dir * total * k;
        board!.scrollLeft = want;
        last = board!.scrollLeft;
        // At an edge the board cannot move to `want`: done.
        raf = k < 0.995 && t < 1500 && Math.abs(last - want) <= 1 ? requestAnimationFrame(step) : 0;
      };
      raf = requestAnimationFrame(step);
    }

    function onCancel(e: Event) {
      if ('pointerId' in e && (e as PointerEvent).pointerId !== pointerId) return;
      finish(false);
    }

    function onKey(e: KeyboardEvent) {
      if (e.key !== 'Escape') return;
      if (phase === 'dragging') e.stopPropagation(); // the page's own Escape stays out of it
      finish(false);
    }

    function onDown(e: PointerEvent) {
      if (e.pointerType === 'touch') return; // native scrolling
      if (e.pointerType === 'mouse' && e.button !== 0) return;
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      if (e.defaultPrevented) return;
      stopMomentum(); // a press stops a coasting board
      if (phase !== 'idle') finish(false);
      const t = e.target as Element;
      if (!board!.contains(t) || t.closest(INTERACTIVE)) return;
      if (onScrollbar(e, board!)) return;
      phase = 'pressed';
      pointerId = e.pointerId;
      startX = e.clientX;
      startScroll = board!.scrollLeft;
      samples = [{ t: e.timeStamp, x: e.clientX }];
      listen(true);
    }

    board.addEventListener('pointerdown', onDown);
    return () => {
      board.removeEventListener('pointerdown', onDown);
      stopMomentum();
      if (phase !== 'idle') finish(false);
      onUpClear();
    };
  }, [ref, activeClass]);
}
