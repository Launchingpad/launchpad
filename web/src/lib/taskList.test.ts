import { describe, expect, it } from 'vitest';
import { isTaskMarkerAt, toggleTaskAt } from './taskList';

describe('toggleTaskAt', () => {
  it('flips the marker at the offset only', () => {
    expect(toggleTaskAt('- [ ] a\n- [ ] b', 10)).toBe('- [ ] a\n- [x] b');
    expect(toggleTaskAt('- [x] a', 2)).toBe('- [ ] a');
    expect(toggleTaskAt('- [X] a', 2)).toBe('- [ ] a');
  });
  it('returns null when no marker is there', () => {
    expect(toggleTaskAt('- [ ] a', 0)).toBeNull();
    expect(toggleTaskAt('- [y] a', 2)).toBeNull();
    expect(isTaskMarkerAt('- [ ] a', 2)).toBe(true);
  });
});
