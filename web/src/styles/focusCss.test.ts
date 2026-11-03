import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout, so these pin the CSS behind LAUN-57 (no blue ring on text fields). The real
// check is the headless-browser one in docs/internal/web-decisions.md ("Text fields: no blue ring").
const root = resolve(process.cwd(), 'src');
const read = (p: string) => readFileSync(join(root, p), 'utf8');
// Prettier may re-quote and wrap a long selector; compare without that.
const global = read('styles/global.css')
  .replace(/"/g, "'")
  .replace(/\s+/g, ' ')
  .replace(/\(\s+/g, '(')
  .replace(/\s+\)/g, ')');
const tokens = read('styles/tokens.css');

const cssFiles = (dir: string): string[] =>
  readdirSync(dir).flatMap((n) => {
    const p = join(dir, n);
    return statSync(p).isDirectory() ? cssFiles(p) : p.endsWith('.css') ? [p] : [];
  });

/** [selector, body] of each flat rule (comments removed). */
const rules = (css: string): [string, string][] =>
  [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => [
    m[1]!.trim(),
    m[2]!,
  ]);

describe('text fields have no blue ring (LAUN-57)', () => {
  it('the global rule removes outline and box-shadow on every text field kind', () => {
    const r = rules(global).find(
      ([sel, body]) =>
        sel
          .split(',')
          .map((x) => x.trim())
          .includes('textarea:focus-visible') && /outline:\s*none/.test(body),
    );
    expect(r).toBeTruthy();
    const [sel, body] = r!;
    expect(sel).toMatch(/^input:not\(\[type='checkbox'\]\)/);
    const parts = sel.split(',').map((x) => x.trim());
    expect(parts).toContain('select:focus-visible');
    expect(parts.some((x) => x.startsWith('[contenteditable]') && x.endsWith(':focus-visible'))).toBe(true);
    expect(body).toMatch(/box-shadow:\s*none/);
  });
  it('the global rule leaves checkbox, radio, range and buttons their ring', () => {
    const sel = rules(global).find(([s]) => s.includes('textarea:focus-visible'))![0];
    for (const t of ['checkbox', 'radio', 'range', 'button', 'submit']) {
      expect(sel).toContain(`:not([type='${t}'])`);
    }
    const base = rules(global).find(([s]) => s === ':focus-visible');
    expect(base?.[1]).toMatch(/outline:\s*2px solid var\(--focus\)/);
  });
  it('the quiet state is neutral, and invalid fields turn red', () => {
    expect(global).toMatch(/border-color:\s*var\(--field-focus-border\)/);
    expect(global).toMatch(/\[aria-invalid='true'\]:focus-visible[\s\S]*?border-color:\s*var\(--danger\)/);
    expect((tokens.match(/--field-focus-border:/g) ?? []).length).toBe(3); // light, dark media, dark attr
  });
  it('forced colors: input, textarea, select and contenteditable each get an !important outline', () => {
    // !important, because the text-field rule above has a far more specific selector chain.
    const m = global.match(/@media \(forced-colors: active\) \{([^{}]*)\{([^{}]*)\}/);
    expect(m).toBeTruthy();
    const parts = m![1]!.split(',').map((x) => x.trim());
    for (const el of ['input', 'textarea', 'select', '[contenteditable]']) {
      expect(parts).toContain(`${el}:focus-visible`);
    }
    expect(m![2]).toMatch(/outline:\s*2px solid Highlight\s*!important/);
  });
  it('contenteditable gets a quiet neutral cue, not only the caret', () => {
    const r = rules(global).find(
      ([sel, body]) =>
        sel.startsWith('[contenteditable]') &&
        sel.endsWith(':focus-visible') &&
        /--field-focus-border/.test(body),
    );
    expect(r).toBeTruthy();
    expect(r![1]).not.toMatch(/--focus\b|--accent/);
  });
  it('the click-to-edit description region keeps the global keyboard ring', () => {
    const detail = readFileSync(join(root, 'features/issues/detail.module.css'), 'utf8');
    const descRules = rules(detail).filter(([sel]) => /(^|[\s,])\.desc\b/.test(sel));
    expect(descRules.length).toBeGreaterThan(0); // the scan must see .desc rules, or it proves nothing
    for (const [, body] of descRules) {
      expect(body).not.toMatch(/outline/);
      expect(body).not.toMatch(/box-shadow:\s*none/);
    }
    // The ring comes from the global rule, which is present and names no .desc.
    const base = rules(global).find(([sel]) => sel === ':focus-visible');
    expect(base?.[1]).toMatch(/outline:\s*2px solid var\(--focus\)/);
    expect(global).not.toMatch(/\.desc\b/);
  });
  it('no other rule draws a blue ring, shadow or border on a field or its wrapper', () => {
    // Real non-text controls that keep a ring (exact selectors, so a new class name cannot hide).
    const ringAllowed = new Set([
      ".panel [role='menuitem']:focus-visible",
      '.remove:focus-visible',
      '.add:focus-visible',
      '.title:focus-visible::after,\n.cardTitle:focus-visible::after',
    ]);
    const hexBlue = (body: string) =>
      [...body.matchAll(/#([0-9a-f]{3}|[0-9a-f]{6})\b/gi)].some((m) => {
        const h = m[1]!.length === 3 ? [...m[1]!].map((c) => c + c).join('') : m[1]!;
        const [r, g, b] = [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16));
        return b! > r! + 40 && b! >= g!;
      });
    const bad: string[] = [];
    for (const f of cssFiles(root)) {
      if (f.endsWith('global.css') || f.endsWith('tokens.css')) continue;
      for (const [sel, body] of rules(readFileSync(f, 'utf8'))) {
        if (!/:focus(-visible|-within)?(?![\w-])/.test(sel)) continue;
        if (ringAllowed.has(sel)) continue;
        const ring = /(?:^|[;\s])outline:\s*(?!\s|none\b|0\b)[^;]+|box-shadow:\s*(?!\s|none\b)[^;]+/.test(
          body,
        );
        const blueBorder =
          /(?:border[\w-]*|outline-color|box-shadow|outline):[^;]*(?:var\(--(?:focus|accent|accent-solid)\)|\bblue\b)/.test(
            body,
          );
        if (ring || blueBorder || hexBlue(body)) bad.push(`${f}: ${sel}`);
      }
    }
    expect(bad).toEqual([]);
  });
});
