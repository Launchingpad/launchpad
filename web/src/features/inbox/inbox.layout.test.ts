import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout: this guards the touch targets of the inbox (40px on a phone and a coarse pointer).
const css = readFileSync(resolve(process.cwd(), 'src/features/inbox/inbox.module.css'), 'utf8');
const touch = css.slice(css.indexOf('@media (max-width: 760px), (pointer: coarse)'));

describe('inbox touch targets', () => {
  it('uses the phone and coarse-pointer query', () => {
    expect(css).toContain('@media (max-width: 760px), (pointer: coarse)');
  });
  it.each([
    ['.tabs .tab', 'min-height'],
    ['.rowActions button', 'min-width'],
    ['.rowActions button', 'min-height'],
    ['.inlineError button', 'min-height'],
    ['.more button', 'min-height'],
    ['button.bell', 'width'],
    ['button.bell', 'min-height'],
    ['button.markAll', 'min-height'],
  ])('%s has %s of at least 40px', (sel, prop) => {
    const block = new RegExp(`${sel.replace(/\./g, '\\.')}\\s*\\{([^}]*)\\}`).exec(touch)?.[1] ?? '';
    const m = new RegExp(`(?:^|[\\s;])${prop}:\\s*(\\d+)px`).exec(block);
    expect(Number(m?.[1])).toBeGreaterThanOrEqual(40);
  });
  it('shows the time again on a focused row without hover', () => {
    expect(css).toMatch(/\.row:focus-within \.time,\s*\.focused \.time \{\s*visibility: visible/);
  });
});

describe('the two bells', () => {
  const shell = readFileSync(resolve(process.cwd(), 'src/features/shell/shell.module.css'), 'utf8');
  it('shows the top bar bell only on a phone, where the sidebar is a closed drawer', () => {
    expect(shell).toMatch(/\.topbar\s*\{[^}]*display:\s*none/);
    const phone = shell.slice(shell.indexOf('@media (max-width: 760px)'));
    expect(phone).toMatch(/\.topbar\s*\{[^}]*display:\s*flex/);
    expect(phone).toMatch(/\.sidebar\s*\{[^}]*transform:\s*translateX\(-102%\)/);
  });
});
