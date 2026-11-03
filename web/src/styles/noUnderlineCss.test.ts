import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// LAUN-60: no underline on hover for card titles, identifiers and row links. jsdom has no
// layout; the real check is the headless-browser run in docs/internal/web-decisions.md.
const read = (f: string) => readFileSync(resolve(process.cwd(), f), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
function rule(css: string, sel: string): string {
  let out = '';
  for (const m of css.matchAll(/(?:^|\n)([^{}\n@][^{}]*)\{([^}]*)\}/g)) {
    if (m[1]!.split(',').some((x) => x.trim() === sel)) out += m[2]!;
  }
  return out;
}
const global = read('src/styles/global.css');
const issues = read('src/features/issues/issues.module.css');
const cycles = read('src/features/cycles/cycles.module.css');
const none = /text-decoration:\s*none/;

describe('no underline on hover (LAUN-60)', () => {
  it('only inline prose links (no class, or .textLink) underline on hover, with no focus rule', () => {
    const r = rule(global, 'a:not([class]):hover');
    expect(r).toMatch(/text-decoration:\s*underline/);
    expect(rule(global, 'a.textLink:hover')).toBe(r);
    const sels = [...global.matchAll(/([^{}]*)\{/g)].flatMap((m) => m[1]!.split(',').map((x) => x.trim()));
    expect(sels).not.toContain('a:hover');
    expect(sels).not.toContain('a:focus-visible');
    for (const m of global.matchAll(/([^{}]*)\{([^}]*)\}/g))
      if (/text-decoration:\s*underline/.test(m[2]!))
        expect(m[1]!.split(',').map((x) => x.trim())).toEqual(['a:not([class]):hover', 'a.textLink:hover']);
  });
  it('row, card and cycle links are not underlined on hover', () => {
    for (const sel of [
      '.row:hover .titleText',
      '.card:hover .titleText',
      '.row .ident:hover',
      '.card .ident:hover',
      '.title:hover',
      '.cardTitle:hover',
      '.relLink:hover',
    ])
      expect(rule(issues, sel), sel).toMatch(none);
    for (const sel of ['.title:hover', '.sub:hover']) expect(rule(cycles, sel), sel).toMatch(none);
  });
  it('no module css underlines anything on hover or focus', () => {
    const dir = resolve(process.cwd(), 'src');
    const files: string[] = [];
    const walk = (d: string) => {
      for (const e of readdirSync(d, { withFileTypes: true })) {
        const f = join(d, e.name);
        if (e.isDirectory()) walk(f);
        else if (e.name.endsWith('.module.css')) files.push(f);
      }
    };
    walk(dir);
    expect(files.length).toBeGreaterThan(5);
    for (const f of files) {
      const css = read(relative(process.cwd(), f));
      for (const m of css.matchAll(/([^{}]*)\{([^}]*)\}/g))
        if (/text-decoration:\s*underline/.test(m[2]!) && /:(hover|focus)/.test(m[1]!))
          throw new Error(`${f}: underline on ${m[1]!.trim()}`);
    }
  });
  it('the hover cue is a background and the focus ring stays', () => {
    expect(rule(issues, '.card:hover:not(.selected):not(.focused)')).toMatch(/background/);
    expect(rule(issues, '.row:hover:not(.selected):not(.focused)')).toMatch(/background/);
    expect(rule(global, ':focus-visible')).toMatch(/outline:\s*2px solid var\(--focus\)/);
  });
});
