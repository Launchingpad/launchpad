import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const read = (f: string) => readFileSync(resolve(process.cwd(), 'src/features/issues', f), 'utf8');
const rule = (css: string, sel: string) => {
  const m = css.match(new RegExp(`(?:^|\\n)${sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')} \\{([^}]*)\\}`));
  return m ? m[1] : '';
};

// LAUN-152: CSS in jsdom is checked as text.
describe('relations layout (LAUN-152)', () => {
  const issues = read('issues.module.css');
  const detail = read('detail.module.css');
  it('a relation row wraps, so its error never overlaps the link', () => {
    expect(rule(issues, '.relRow')).toMatch(/flex-wrap:\s*wrap/);
    expect(rule(issues, ".relRow > [role='alert']")).toMatch(/flex:\s*1 1 100%/);
  });
  it('the title link shrinks and truncates', () => {
    const r = rule(issues, '.relLink');
    expect(r).toMatch(/min-width:\s*0/);
    expect(r).toMatch(/text-overflow:\s*ellipsis/);
  });
  it('the add row does not pull the picker over the type select', () => {
    expect(rule(detail, '.relAdd .ghostBtn')).toMatch(/margin:\s*0;/);
  });
  it('children of the add row and the type select can shrink', () => {
    expect(rule(detail, '.relAdd > *')).toMatch(/min-width:\s*0/);
    expect(rule(detail, '.relType')).toMatch(/max-width:\s*100%/);
  });
  it('the icon and remove button keep their size; the alert wraps', () => {
    expect(rule(issues, '.relRow > svg,\n.relRow > button')).toMatch(/flex:\s*none/);
    const a = rule(issues, ".relRow > [role='alert']");
    expect(a).toMatch(/min-width:\s*0/);
    expect(a).toMatch(/overflow-wrap:\s*anywhere/);
  });
});
