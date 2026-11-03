import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout, so these pin the CSS rules of LAUN-120 (the toned issue page and its centred column).
const read = (p: string) => readFileSync(resolve(process.cwd(), p), 'utf8').replace(/\/\*[\s\S]*?\*\//g, ''); // comments out: a comment must not satisfy a rule
const detail = read('src/features/issues/detail.module.css');
const tokens = read('src/styles/tokens.css');
const rule = (css: string, sel: string) =>
  css.match(new RegExp(`(?:^|\\n)${sel.replace('.', '\\.')}\\s*\\{([^}]*)\\}`))?.[1] ?? '';

describe('issue page tone and column (LAUN-120)', () => {
  it('--bg-page and --bg-card are defined in light, dark (OS) and dark (manual)', () => {
    for (const name of ['bg-page', 'bg-card']) {
      expect(tokens.match(new RegExp(`--${name}:`, 'g'))?.length, name).toBe(3);
    }
  });
  it('the scroller has the page tone (LAUN-202) and generous side padding', () => {
    const r = rule(detail, '.detail');
    expect(r).toMatch(/background:\s*var\(--bg-page\)/);
    expect(r).toMatch(/padding:\s*\d+px 48px/);
    expect(r).toMatch(/overflow:\s*auto/);
  });
  it('bar, title and body share one centred column of at most 860px', () => {
    const r = rule(detail, '.bar,\n.head,\n.main');
    const w = Number(/max-width:\s*(\d+)px/.exec(r)?.[1]);
    expect(w).toBeGreaterThanOrEqual(720);
    expect(w).toBeLessThanOrEqual(860);
    expect(r).toMatch(/justify-self:\s*center/);
  });
  it('input areas are cards on --bg-card', () => {
    expect(rule(detail, '.inputCard')).toMatch(/background:\s*var\(--bg-card\)/);
  });
  it('on a phone the side padding is 16px', () => {
    const phone = detail.slice(detail.indexOf('@media (max-width: 900px)'));
    expect(phone).toMatch(/padding:\s*\d+px 16px \d+px/);
  });
  it('only the centre column is a card on the tone (LAUN-202), and the phone has no card behind', () => {
    const r = rule(detail, '.detail::before');
    expect(r).toMatch(/background:\s*var\(--bg-card\)/);
    expect(r).toMatch(/grid-area:\s*1 \/ 1 \/ 4 \/ 2/);
    const phone = detail.slice(detail.indexOf('@media (max-width: 900px)'));
    expect(phone).toMatch(/\.detail::before\s*\{\s*display:\s*none/);
    expect(phone).toMatch(/\.detail\s*\{[^}]*background:\s*var\(--bg-card\)/);
  });
  it('the card is inert, bordered and 24px wider than the column on each side, 8px above and 32px below (LAUN-202)', () => {
    const r = rule(detail, '.detail::before');
    expect(r).toMatch(/pointer-events:\s*none/);
    expect(r).toMatch(/border:\s*1px solid var\(--border\)/);
    expect(r).toMatch(/width:\s*calc\(100% \+ 48px\)/);
    expect(r).toMatch(/max-width:\s*848px/);
    expect(r).toMatch(/margin:\s*-12px 0 -32px/);
  });
  it('inside the page --surface is the card-aware token, which differs from --bg-card (LAUN-202)', () => {
    const r = rule(detail, '.detail');
    expect(r).toMatch(/--surface:\s*var\(--card-surface\)/);
    expect(r).toMatch(/--surface-hover:\s*var\(--card-surface-hover\)/);
    const blocks = tokens.split(/\n(?=@media|:root\[)/);
    for (const b of blocks.filter((x) => /--bg-card:/.test(x))) {
      const get = (n: string) => b.match(new RegExp(`--${n}:\\s*(#[0-9a-f]{6})`))?.[1];
      expect(get('card-surface'), b.slice(0, 40)).toBeTruthy();
      expect(get('card-surface')).not.toBe(get('bg-card'));
    }
  });
  it('the Properties column hovers on --page-hover, the phone card on --surface-hover', () => {
    expect(rule(detail, '.ghostBtn:hover')).toMatch(/var\(\s*--page-hover/);
    expect(detail.slice(detail.indexOf('@media (max-width: 900px)'))).toMatch(
      /\.ghostBtn:hover\s*\{\s*background:\s*var\(--surface-hover\)/,
    );
  });
  it('a rule that masks the card behind the timeline uses --bg-card, the colour of the centre card (LAUN-202)', () => {
    expect(rule(detail, '.event .gutter')).toMatch(/background:\s*var\(--bg-card\)/);
  });
  it('no rule in the detail sheet paints --surface or --bg, except hovers', () => {
    const plain = detail.replace(/@media[^{]*\{[\s\S]*?\n\}\n/g, '');
    const bad = [...plain.matchAll(/([^{}]+)\{([^}]*)\}/g)].filter(
      (m) =>
        /background:\s*var\(--(bg|surface)\)/.test(m[2]!) ||
        (/background:\s*var\(--bg-page\)/.test(m[2]!) && !/^\s*\.detail\s*$/.test(m[1]!)),
    );
    expect(bad.map((m) => m[1]!.trim())).toEqual([]);
  });
  it('--shadow-card is defined in all three blocks', () => {
    expect(tokens.match(/--shadow-card:/g)?.length).toBe(3);
  });
});
