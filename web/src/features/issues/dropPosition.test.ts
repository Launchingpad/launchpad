import { describe, expect, it } from 'vitest';
import { columnAt, dropIndex, edgeScroll, placementAt, type Slot } from './dropPosition';

// Three cards, 40 high every 46 (centres 20, 66, 112).
const slots: Slot[] = [
  { id: 'a', top: 0, height: 40 },
  { id: 'b', top: 46, height: 40 },
  { id: 'c', top: 92, height: 40 },
];

describe('dropIndex', () => {
  it('is 0 above the first centre and the card count below the last', () => {
    expect(dropIndex(-50, slots)).toBe(0);
    expect(dropIndex(5, slots)).toBe(0);
    expect(dropIndex(500, slots)).toBe(3);
  });
  it('counts the cards whose centre is above the point', () => {
    expect(dropIndex(21, slots)).toBe(1);
    expect(dropIndex(66, slots)).toBe(1); // exactly on a centre: not past it
    expect(dropIndex(67, slots)).toBe(2);
    expect(dropIndex(113, slots)).toBe(3);
  });
  it('is 0 for an empty column', () => {
    expect(dropIndex(100, [])).toBe(0);
  });
});

describe('placementAt', () => {
  const t1 = (id: string) => ({ id, teamId: 't1' });
  const t2 = (id: string) => ({ id, teamId: 't2' });
  it('names the card above as placeAfter, else the card below as placeBefore', () => {
    const cards = [t1('a'), t1('b'), t1('c')];
    expect(placementAt(cards, 0, 't1')).toEqual({ placeBefore: 'a' });
    expect(placementAt(cards, 1, 't1')).toEqual({ placeAfter: 'a' });
    expect(placementAt(cards, 2, 't1')).toEqual({ placeAfter: 'b' });
    expect(placementAt(cards, 3, 't1')).toEqual({ placeAfter: 'c' });
  });
  it('names nothing for an empty column (the server puts the card on top)', () => {
    expect(placementAt([], 0, 't1')).toEqual({});
  });
  it('skips cards of another team, because ranks only mean something inside one team', () => {
    const cards = [t2('x'), t1('a'), t2('y'), t2('z'), t1('b')];
    expect(placementAt(cards, 3, 't1')).toEqual({ placeAfter: 'a' }); // y and z are skipped
    expect(placementAt(cards, 1, 't1')).toEqual({ placeBefore: 'a' }); // nothing of t1 above
    expect(placementAt([t2('x'), t2('y')], 1, 't1')).toEqual({}); // no card of this team at all
  });
  it('never names more than one neighbour', () => {
    const p = placementAt([t1('a'), t1('b')], 1, 't1');
    expect(Object.keys(p)).toHaveLength(1);
  });
});

describe('edgeScroll', () => {
  it('is 0 in the middle and grows toward the edge', () => {
    expect(edgeScroll(500, 0, 1000)).toBe(0);
    expect(edgeScroll(10, 0, 1000)).toBeLessThan(0);
    expect(edgeScroll(990, 0, 1000)).toBeGreaterThan(0);
    expect(Math.abs(edgeScroll(1, 0, 1000))).toBeGreaterThan(Math.abs(edgeScroll(40, 0, 1000)));
  });
  it('stops at the full speed past the edge', () => {
    expect(edgeScroll(-500, 0, 1000)).toBe(-22);
    expect(edgeScroll(5000, 0, 1000)).toBe(22);
  });
  it('does not scroll a box too small for two zones from its middle', () => {
    expect(edgeScroll(50, 0, 100)).toBe(0);
  });
});

describe('columnAt', () => {
  const spans = [
    { key: 'a', left: Number.NEGATIVE_INFINITY, right: 300 },
    { key: 'b', left: 300, right: 600 },
  ];
  it('finds the column under x', () => {
    expect(columnAt(10, spans)).toBe('a');
    expect(columnAt(300, spans)).toBe('b');
    expect(columnAt(900, spans)).toBeNull();
  });
});
