import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout, so these guard the CSS rules behind LAUN-40 (two scrollbars). The real check
// is in docs/internal/web-decisions.md ("One scroll area"): the page scroll height must equal the window.
const read = (p: string) => readFileSync(resolve(process.cwd(), p), 'utf8').replace(/\/\*[\s\S]*?\*\//g, ''); // comments out: a comment must not satisfy a rule
const global = read('src/styles/global.css');
const shell = read('src/features/shell/shell.module.css');
const rule = (css: string, sel: string) =>
  css.match(new RegExp(`(?:^|\\n)${sel.replace('.', '\\.')}\\s*\\{([^}]*)\\}`))?.[1] ?? '';

describe('one scroll area (LAUN-40)', () => {
  it('.sr-only is not position:absolute (it would stretch the page scroll height)', () => {
    const r = rule(global, '.sr-only');
    expect(r).toMatch(/position:\s*fixed/);
    expect(r).not.toMatch(/position:\s*absolute/);
  });
  it('the shell is exactly the window tall and clips', () => {
    const r = rule(shell, '.app');
    expect(r).toMatch(/height:\s*100dvh/);
    expect(r).toMatch(/overflow:\s*hidden/);
  });
  it('.main and .content can shrink, and .content is the fallback scroller', () => {
    expect(rule(shell, '.main')).toMatch(/min-height:\s*0/);
    expect(rule(shell, '.content')).toMatch(/min-height:\s*0/);
    expect(rule(shell, '.content')).toMatch(/overflow-y:\s*auto/);
  });
});

// LAUN-181: the sidebar and the main area are rounded panels on the page tone, inset by a gap; on a phone they go edge to edge.
describe('shell cards (LAUN-181)', () => {
  const phone = shell.slice(shell.indexOf('@media (max-width: 760px) {'));
  it('the shell has the page tone and an 8px inset and gap', () => {
    const r = rule(shell, '.app');
    expect(r).toMatch(/background:\s*var\(--bg-page\)/);
    expect(r).toMatch(/padding:\s*8px/);
    expect(r).toMatch(/gap:\s*8px/);
  });
  it('the sidebar and the main area are rounded with the radius token; the main card clips and is --bg', () => {
    expect(rule(shell, '.sidebar')).toMatch(/border-radius:\s*var\(--radius\)/);
    const m = rule(shell, '.main');
    expect(m).toMatch(/border-radius:\s*var\(--radius\)/);
    expect(m).toMatch(/background:\s*var\(--bg\)/);
    expect(m).toMatch(/overflow:\s*hidden/);
  });
  it('on a phone the inset, the gap and the corners go', () => {
    expect(phone).toMatch(/\.app\s*\{[^}]*padding:\s*0/);
    expect(phone).toMatch(/\.main\s*\{[^}]*border-radius:\s*0/);
    expect(phone).toMatch(/\.sidebar\s*\{[^}]*border-radius:\s*0/);
  });
  it('the settings content has no own background: it sits on the --bg card', () => {
    const settings = read('src/features/settings/settings.module.css');
    expect(rule(settings, '.layout')).toMatch(/overflow-y:\s*auto/);
    expect(rule(settings, '.layout')).not.toMatch(/background/);
  });
});
