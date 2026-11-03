import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { createElement } from 'react';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { renderAt } from '../../test/render';
import { team } from '../../test/mockApi';
import { CloseDialog } from './ImportPage';
import { TeamCyclesControl } from './TeamCycles';

// jsdom has no layout: this guards the touch targets of the import wizard (40px on a phone and a coarse pointer).
const css = readFileSync(resolve(process.cwd(), 'src/features/settings/import.module.css'), 'utf8');
const query = '@media (max-width: 760px), (pointer: coarse)';
const touch = css.slice(css.indexOf('{', css.indexOf(query)) + 1);

/** The declarations of the rule whose selector list contains `sel`. */
function declarations(sel: string): string {
  for (const m of touch.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
    if (m[1]!.split(',').some((s) => s.trim() === sel)) return m[2]!;
  }
  return '';
}

describe('import wizard touch targets', () => {
  it('uses the phone and coarse-pointer query', () => {
    expect(css).toContain(query);
  });
  it.each([
    '.stepBtn',
    '.mapSelect',
    '.page button',
    '.page select',
    ".page input:not([type='checkbox'])",
    '.page summary',
    ".page label:has(input[type='checkbox'])",
    ".page [role='switch']",
    '.dialogForm button',
    '.dialogForm input',
  ])('%s has min-height of at least 40px', (sel) => {
    const m = /min-height:\s*(\d+)px/.exec(declarations(sel));
    expect(Number(m?.[1])).toBeGreaterThanOrEqual(40);
  });
});

// The dialogs are drawn in a portal, so the page class does not reach them: each carries its own class.
describe('the dialogs get the 40px rule', () => {
  it('the Close dialog buttons sit inside .dialogForm', () => {
    renderAt(createElement(CloseDialog, { id: 'x', onDone: vi.fn() }));
    for (const name of ['Keep it', 'Close the import'])
      expect(screen.getByRole('button', { name }).closest('[class*="dialogForm"]')).not.toBeNull();
  });

  it('the Turn on cycles dialog buttons sit inside the cycles dialog body class', async () => {
    renderAt(createElement(TeamCyclesControl, { team, canEdit: true, onError: vi.fn() }));
    await userEvent.setup().click(screen.getByRole('switch', { name: 'Cycles for ORB' }));
    for (const name of ['Cancel', 'Turn on'])
      expect(screen.getByRole('button', { name }).closest('[class*="dialogBody"]')).not.toBeNull();
  });

  it('the cycles dialog css has the 40px rule', () => {
    const c = readFileSync(resolve(process.cwd(), 'src/features/settings/cyclesDialog.module.css'), 'utf8');
    expect(c).toContain(query);
    expect(/\.dialogBody button\s*\{[^}]*min-height:\s*40px/.test(c)).toBe(true);
  });
});
