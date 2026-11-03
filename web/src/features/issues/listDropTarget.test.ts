import { describe, expect, it } from 'vitest';
import { gapTop, listTarget, stepPlacement, type ListGroupBox } from './listDropTarget';

// Two groups of 36 px rows. A: head 0-30, rows a1 30-66, a2 66-102 (bottom 102, 4 px margin).
// B: head 106-136, rows b1 136-172, b2 172-208, b3 208-244 (bottom 244).
const A: ListGroupBox = {
  key: 'A',
  top: 0,
  bottom: 102,
  slots: [
    { id: 'a1', top: 30, height: 36 },
    { id: 'a2', top: 66, height: 36 },
  ],
};
const B: ListGroupBox = {
  key: 'B',
  top: 106,
  bottom: 244,
  slots: [
    { id: 'b1', top: 136, height: 36 },
    { id: 'b2', top: 172, height: 36 },
    { id: 'b3', top: 208, height: 36 },
  ],
};

describe('listTarget', () => {
  it('counts the rows whose centre is above the pointer, inside a group', () => {
    expect(listTarget(60, [A, B])).toEqual({ group: 0, index: 1 }); // below a1's centre (48)
    expect(listTarget(40, [A, B])).toEqual({ group: 0, index: 0 });
    expect(listTarget(90, [A, B])).toEqual({ group: 0, index: 2 });
  });
  it('a group head and the space above the first row are the top of that group', () => {
    expect(listTarget(10, [A, B])).toEqual({ group: 0, index: 0 });
    expect(listTarget(120, [A, B])).toEqual({ group: 1, index: 0 });
    expect(listTarget(-50, [A, B])).toEqual({ group: 0, index: 0 });
  });
  it('the space between two groups goes with the lower one; below the last group is its end', () => {
    expect(listTarget(104, [A, B])).toEqual({ group: 1, index: 0 }); // the margin 102-106 belongs to B (A ended)
    expect(listTarget(101, [A, B])).toEqual({ group: 0, index: 2 });
    expect(listTarget(999, [A, B])).toEqual({ group: 1, index: 3 });
  });
  it('crosses into the next group by y', () => {
    expect(listTarget(190, [A, B])).toEqual({ group: 1, index: 1 }); // 172+18=190: not above yet
    expect(listTarget(191, [A, B])).toEqual({ group: 1, index: 2 });
  });
  it('no groups: nothing', () => {
    expect(listTarget(10, [])).toBeNull();
  });
});

describe('gapTop', () => {
  it('is the top of the row below the drop, else the bottom of the last row', () => {
    expect(gapTop(B, 0, 0)).toBe(136);
    expect(gapTop(B, 2, 0)).toBe(208);
    expect(gapTop(B, 3, 0)).toBe(244);
  });
  it('uses the fallback when the group has no other row', () => {
    expect(gapTop({ key: 'C', top: 0, bottom: 66, slots: [] }, 0, 30)).toBe(30);
  });
});

describe('stepPlacement', () => {
  const rows = [
    { id: 'a', teamId: 't1' },
    { id: 'b', teamId: 't1' },
    { id: 'c', teamId: 't1' },
  ];
  it('moves one place up and down', () => {
    expect(stepPlacement(rows, 'b', -1)).toEqual({ placement: { placeBefore: 'a' }, index: 0 });
    expect(stepPlacement(rows, 'b', 1)).toEqual({ placement: { placeAfter: 'c' }, index: 2 });
  });
  it('stops at the edge of the loaded rows', () => {
    expect(stepPlacement(rows, 'a', -1)).toBeNull();
    expect(stepPlacement(rows, 'c', 1)).toBeNull();
    expect(stepPlacement(rows, 'zzz', 1)).toBeNull();
  });
  it('steps over a row of another team', () => {
    const mixed = [
      { id: 'a', teamId: 't1' },
      { id: 'x', teamId: 't2' },
      { id: 'b', teamId: 't1' },
    ];
    expect(stepPlacement(mixed, 'b', -1)).toEqual({ placement: { placeBefore: 'a' }, index: 0 });
    expect(stepPlacement(mixed, 'a', 1)).toEqual({ placement: { placeAfter: 'b' }, index: 2 });
  });
  it('a lone row of its team has nothing to name', () => {
    const lone = [
      { id: 'x', teamId: 't2' },
      { id: 'a', teamId: 't1' },
    ];
    expect(stepPlacement(lone, 'a', -1)).toBeNull();
  });
});
