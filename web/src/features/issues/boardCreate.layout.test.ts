import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// LAUN-173: jsdom has no hover or cascade, so the CSS of the lane "+" is read as rules and compared by specificity.
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/issues.module.css'), 'utf8').replace(
  /\/\*[\s\S]*?\*\//g,
  '',
);

type Rule = { sel: string; body: string; media: string };
/** Flat rules (one @media level) as selector, declarations and the media prelude ('' = none). */
function rules(text: string, media = ''): Rule[] {
  const out: Rule[] = [];
  let i = 0;
  while (i < text.length) {
    const open = text.indexOf('{', i);
    if (open < 0) break;
    const head = text.slice(i, open).trim();
    let depth = 1;
    let j = open + 1;
    while (j < text.length && depth) {
      if (text[j] === '{') depth++;
      else if (text[j] === '}') depth--;
      j++;
    }
    const inner = text.slice(open + 1, j - 1);
    if (head.startsWith('@media')) out.push(...rules(inner, head.replace(/\s+/g, ' ')));
    else
      for (const sel of head.split(','))
        out.push({ sel: sel.trim().replace(/\s+/g, ' '), body: inner, media });
    i = j;
  }
  return out;
}
const all = rules(css);
const mentions = (r: Rule) => r.sel.includes('colAdd');
/** [ids, classes and pseudo-classes, elements] */
function spec(sel: string): [number, number, number] {
  const ids = (sel.match(/#[\w-]+/g) ?? []).length;
  const cls = (sel.match(/\.[\w-]+|:(?!:)[\w-]+/g) ?? []).length;
  const el = (sel.replace(/\.[\w-]+|:+[\w-]+|#[\w-]+/g, ' ').match(/(^|[\s>+~])[a-z][\w-]*/gi) ?? []).length;
  return [ids, cls, el];
}
const wins = (a: string, b: string) => {
  const x = spec(a);
  const y = spec(b);
  for (let k = 0; k < 3; k++) if (x[k] !== y[k]) return x[k]! > y[k]!;
  return false;
};
const decl = (r: Rule, prop: string, value: string) =>
  new RegExp(`(^|;|\\s)${prop}\\s*:\\s*${value}\\s*(;|$)`, 'i').test(r.body.trim());
const base = all.find((r) => r.sel === '.colAdd' && !r.media)!;
const reveals = all.filter((r) => mentions(r) && !r.media && decl(r, 'opacity', '1'));
const drag = all.filter((r) => mentions(r) && /dragActive/.test(r.sel));
const touch = all.find((r) => r.sel === '.colAdd' && /hover: ?none/.test(r.media))!;

describe('the lane "+" CSS (LAUN-173)', () => {
  it('is hidden at rest but stays clickable and in the layout', () => {
    expect(decl(base, 'opacity', '0')).toBe(true);
    expect(base.body).not.toMatch(/pointer-events\s*:\s*none/i);
    expect(base.body).not.toMatch(/display\s*:\s*none/i);
    expect(base.body).not.toMatch(/visibility\s*:\s*hidden/i);
  });
  it('is revealed by lane hover, by focus inside the lane and by its own focus', () => {
    const sels = reveals.map((r) => r.sel);
    expect(sels).toEqual(
      expect.arrayContaining([
        '.column:hover .colAdd',
        '.column:focus-within .colAdd',
        '.colAdd:focus-visible',
      ]),
    );
  });
  it('stays visible, and 40px high, where there is no hover or on a phone', () => {
    expect(touch).toBeTruthy();
    expect(decl(touch, 'opacity', '1')).toBe(true);
    expect(decl(touch, 'min-height', '40px')).toBe(true);
  });
  it('is hidden and inert while a card is dragged, and that beats every reveal', () => {
    expect(drag.length).toBeGreaterThan(0);
    for (const r of drag) {
      expect(decl(r, 'opacity', '0')).toBe(true);
      expect(decl(r, 'pointer-events', 'none')).toBe(true);
      for (const v of [...reveals, touch]) expect(wins(r.sel, v.sel), `${r.sel} vs ${v.sel}`).toBe(true);
    }
  });
});
