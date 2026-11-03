import { useLayoutEffect, useState, type RefObject } from 'react';
import type { LabelRef } from '../../api/types';

/**
 * LAUN-233: the labels of a list row sit in one clipped line. These helpers find the pills that the clip cuts, so the
 * row can hide them and count them in "+N" (which is outside the clip and so always shows).
 */

/** The labels a row renders as pills; the rest are only counted. */
export const ROW_PILLS = 3;

type Span = { left: number; right: number };

/** For each pill: true when any part of it is outside the box (half a pixel of slack for rounding). */
export function clippedFlags(box: Span, pills: Span[]): boolean[] {
  return pills.map((p) => p.left < box.left - 0.5 || p.right > box.right + 0.5);
}

/** The labels a row does not show: those past the rendered pills, plus the rendered pills the clip cuts. */
export function hiddenLabels<T>(labels: T[], rendered: number, clipped: number): T[] {
  return labels.slice(Math.max(0, Math.min(rendered, labels.length) - clipped));
}

/** The hover text of the labels cell: the project and every label name. */
export function labelsTitle(
  project: string | undefined,
  labels: Pick<LabelRef, 'name'>[],
): string | undefined {
  const parts: string[] = [];
  if (project) parts.push(`Project: ${project}`);
  if (labels.length) parts.push(`Labels: ${labels.map((l) => l.name).join(', ')}`);
  return parts.length ? parts.join('\n') : undefined;
}

/*
 * One ResizeObserver for every row (LAUN-233 review 4). It watches each labels box AND each child of it (the chip and
 * the pills): a pill can change width while the box keeps its width (a rename, a late web font, user CSS). A
 * MutationObserver per box catches children that come or go (the project chip mounts when the projects arrive) and
 * text changes. A measure pass first READS every affected box, then WRITES: `data-clipped` only where it changes and
 * the count only where it changes, so a long list forces one layout per pass, not one per row. Hiding a pill sets
 * only its opacity, so it never changes a measured size (no observer loop).
 */
type Entry = {
  setCount: (n: number) => void;
  count: number;
  observed: Set<Element>;
  mo: MutationObserver | null;
};
const entries = new Map<Element, Entry>();
const ownerOf = new WeakMap<Element, Element>();
let ro: ResizeObserver | null = null;
let roCtor: unknown = null; // the ResizeObserver class `ro` was made from (tests swap it)
let fontsTarget: EventTarget | null = null; // the FontFaceSet we listen to (tests swap it)
let frame = 0;
const pending = new Set<Element>();

const pillsOf = (box: Element) => [...box.querySelectorAll<HTMLElement>(':scope > span')];

/** Reads every given box, then writes the changes. Exported for the tests. */
export function measureBoxes(boxes: Iterable<Element>): void {
  const reads: [Entry, HTMLElement[], boolean[]][] = [];
  for (const box of boxes) {
    const e = entries.get(box);
    if (!e || !box.isConnected) continue;
    const b = box.getBoundingClientRect();
    const pills = pillsOf(box);
    const flags =
      b.width > 0
        ? clippedFlags(
            b,
            pills.map((p) => p.getBoundingClientRect()),
          )
        : pills.map(() => false);
    reads.push([e, pills, flags]);
  }
  for (const [e, pills, flags] of reads) {
    pills.forEach((p, i) => {
      const has = p.hasAttribute('data-clipped');
      if (flags[i] && !has) p.setAttribute('data-clipped', '');
      else if (!flags[i] && has) p.removeAttribute('data-clipped');
    });
    const n = flags.filter(Boolean).length;
    if (n !== e.count) {
      e.count = n;
      e.setCount(n);
    }
  }
}

function schedule(box: Element) {
  pending.add(box);
  if (frame) return;
  frame = requestAnimationFrame(() => {
    frame = 0;
    const list = [...pending];
    pending.clear();
    measureBoxes(list);
  });
}

/** Observes the box's children that are new and forgets those that left. */
function syncChildren(box: Element, e: Entry) {
  for (const c of e.observed) {
    if (c.parentElement !== box) {
      ro?.unobserve?.(c);
      e.observed.delete(c);
    }
  }
  for (const c of box.children) {
    if (e.observed.has(c)) continue;
    e.observed.add(c);
    ownerOf.set(c, box);
    ro?.observe(c);
  }
}

/** A web font that arrives late changes pill widths: measure every mounted box again. */
function onFontsLoaded() {
  entries.forEach((_, box) => schedule(box));
}

function sharedObserver(): ResizeObserver {
  if (ro && roCtor !== ResizeObserver) resetLabelObserver();
  roCtor = ResizeObserver;
  ro ??= new ResizeObserver((list) => {
    const boxes = new Set<Element>();
    for (const en of list) {
      const box = entries.has(en.target) ? en.target : ownerOf.get(en.target);
      if (box) boxes.add(box);
    }
    measureBoxes(boxes);
  });
  const fonts = typeof document !== 'undefined' ? (document.fonts as EventTarget | undefined) : undefined;
  if (fonts?.addEventListener && fonts !== fontsTarget) {
    fontsTarget?.removeEventListener('loadingdone', onFontsLoaded);
    fontsTarget = fonts;
    fonts.addEventListener('loadingdone', onFontsLoaded);
  }
  return ro;
}

/** The number of boxes the shared observer knows (tests only: unmount must forget its box). */
export const knownLabelBoxes = (): number => entries.size;

/** Forgets the shared observer (tests only: each test installs its own fake ResizeObserver). */
export function resetLabelObserver(): void {
  ro?.disconnect?.();
  ro = null;
  entries.clear();
  pending.clear();
  if (frame) cancelAnimationFrame(frame);
  frame = 0;
}

/**
 * The number of pills the clip of the box cuts. A cut pill gets `data-clipped` (the CSS hides it, a screen reader
 * still reads it). Without ResizeObserver, or without layout (a box of no width), nothing counts as cut.
 */
export function useClippedPills(ref: RefObject<HTMLElement | null>): number {
  const [clipped, setClipped] = useState(0);
  useLayoutEffect(() => {
    const box = ref.current;
    if (!box || typeof ResizeObserver === 'undefined') return;
    const obs = sharedObserver();
    const e: Entry = { setCount: setClipped, count: 0, observed: new Set(), mo: null };
    entries.set(box, e);
    obs.observe(box); // the first notification measures the box before it is painted
    syncChildren(box, e);
    if (typeof MutationObserver !== 'undefined') {
      e.mo = new MutationObserver(() => {
        syncChildren(box, e);
        schedule(box);
      });
      e.mo.observe(box, { childList: true, subtree: true, characterData: true });
    }
    return () => {
      e.mo?.disconnect();
      // A test's fake observer may have no unobserve.
      obs.unobserve?.(box);
      e.observed.forEach((c) => obs.unobserve?.(c));
      entries.delete(box);
      pending.delete(box);
    };
  }, [ref]);
  return clipped;
}
