import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout: this guards the CSS that keeps the cycle switcher on a phone screen (LAUN-17).
const css = readFileSync(resolve(process.cwd(), 'src/features/cycles/cycles.module.css'), 'utf8');

describe('cycle switcher on a phone', () => {
  const phone = css.slice(css.indexOf('@media (max-width: 480px)'));
  it('puts the panel 8px inside both screen edges and lifts the width cap', () => {
    expect(phone).toMatch(/\.switchPanel\s*\{[^}]*left:\s*8px[^}]*right:\s*8px/);
    expect(phone).toMatch(/max-width:\s*none/);
  });
  it('positions the panel against the header, not the trigger', () => {
    expect(phone).toMatch(/\.switchWrap\s*\{[^}]*position:\s*static/);
    expect(css).toMatch(/\.head\s*\{[^}]*position:\s*relative/);
  });
  it('hides the keyboard hints on narrow and touch screens', () => {
    expect(phone).toMatch(/\.switchPanel kbd\s*\{[^}]*display:\s*none/);
    expect(css).toMatch(/@media \(hover: none\)[^{]*\{[^}]*\.switchPanel kbd\s*\{[^}]*display:\s*none/);
  });
});

describe('cycle switcher width (LAUN-41)', () => {
  it('is wider than the default panel on a wide screen', () => {
    expect(css).toMatch(
      /\.head \.switchPanel\s*\{[^}]*min-width:\s*300px[^}]*max-width:\s*min\(400px, 92vw\)/,
    );
  });
});

describe('burn-up chart CSS (LAUN-14)', () => {
  it('colours the lines with theme tokens only, so light and dark both work', () => {
    const at = css.indexOf('Burn-up chart (LAUN-14)');
    expect(at).toBeGreaterThan(-1);
    const chart = css.slice(at);
    expect(chart).not.toContain('!important');
    expect(chart).not.toMatch(/#[0-9a-fA-F]{3,6}\b/);
    expect(chart).toMatch(/\.doneLine\s*\{[^}]*var\(--accent\)/);
    expect(chart).toMatch(/\.scopeLine\s*\{[^}]*var\(--muted\)/);
  });
  it('keeps the data table out of the layout but readable by a screen reader', () => {
    expect(css).toMatch(/\.srOnly\s*\{[^}]*position:\s*absolute[^}]*clip:/);
    expect(css).not.toMatch(/\.srOnly\s*\{[^}]*display:\s*none/);
  });
});

describe('cycle options menu placement (LAUN-172)', () => {
  const tsx = readFileSync(resolve(process.cwd(), 'src/features/cycles/CycleOptions.tsx'), 'utf8');
  it('opens left-aligned: the button is at the left of the header, a right-aligned panel ran off the screen', () => {
    expect(tsx).not.toMatch(/align=["']right["']/);
  });
  it('spans the header on a phone, like the switcher, and is wide enough for one-line reasons', () => {
    const phone = css.slice(css.indexOf('@media (max-width: 480px)', css.indexOf('.optionsWrap')));
    expect(phone).toMatch(/\.optionsWrap\s*\{[^}]*position:\s*static/);
    expect(phone).toMatch(/\.optionsPanel\s*\{[^}]*left:\s*8px[^}]*right:\s*8px/);
    expect(css).toMatch(/\.head \.optionsPanel\s*\{[^}]*min-width:\s*300px/);
  });
});
