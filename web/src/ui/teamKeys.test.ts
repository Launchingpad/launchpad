import { describe, expect, it } from 'vitest';
import { productTeamsTitle, teamKeysShort } from './teamKeys';

describe('teamKeysShort', () => {
  it('says No teams for none, lists a few, and counts the rest', () => {
    expect(teamKeysShort([])).toBe('No teams');
    expect(teamKeysShort(['A'])).toBe('A');
    expect(teamKeysShort(['A', 'B', 'C'])).toBe('A, B, C');
    expect(teamKeysShort(['A', 'B', 'C', 'D', 'E'])).toBe('A, B, C +2');
    expect(teamKeysShort(['A', 'B', 'C'], 2)).toBe('A, B +1');
  });
});

describe('productTeamsTitle', () => {
  it('names every key', () => {
    expect(productTeamsTitle('Alpha', [])).toBe('Alpha: no teams');
    expect(productTeamsTitle('Alpha', ['A'])).toBe('Alpha: team A');
    expect(productTeamsTitle('Alpha', ['A', 'B', 'C', 'D'])).toBe('Alpha: teams A, B, C, D');
  });
});

// jsdom has no layout: guard the source of "a long team list cannot widen a phone" (LAUN-100).
describe('picker hint css', () => {
  it('lets the team-key hint shrink and cut', async () => {
    const { readFileSync } = await import('node:fs');
    const css = readFileSync('src/styles/ui.module.css', 'utf8');
    const rule = /\.optionHint\s*\{([^}]*)\}/.exec(css)?.[1] ?? '';
    expect(rule).toMatch(/min-width:\s*0/);
    expect(rule).toMatch(/text-overflow:\s*ellipsis/);
    expect(rule).toMatch(/overflow:\s*hidden/);
  });
});
