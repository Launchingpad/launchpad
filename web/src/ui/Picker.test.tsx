import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { PickerList } from './Picker';

const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8');
const options = [
  { id: 'a', label: 'Alpha' },
  { id: 'b', label: 'Beta' },
];

describe('Picker selected option (LAUN-118)', () => {
  it('marks the chosen option with a tick and aria-selected, and gives it no active class', () => {
    const { container } = render(
      <PickerList options={options} selected={['b']} onSelect={() => {}} ariaLabel="Pick" />,
    );
    const beta = screen.getByRole('option', { name: 'Beta' });
    const alpha = screen.getByRole('option', { name: 'Alpha' });
    expect(beta).toHaveAttribute('aria-selected', 'true');
    expect(beta.querySelector('svg')).not.toBeNull();
    expect(alpha.querySelector('svg')).toBeNull();
    // the keyboard-active row (the first) keeps its highlight; the chosen one does not get one
    expect(alpha.className).toMatch(/optionActive/);
    expect(beta.className).not.toMatch(/optionActive/);
    expect(container.querySelectorAll('[aria-selected="true"]')).toHaveLength(1);
  });
  it('marks a chosen row in a multi list with the checked box, aria-selected and no active class', () => {
    render(<PickerList options={options} selected={['b']} onSelect={() => {}} ariaLabel="Pick" multi />);
    expect(screen.getByRole('listbox')).toHaveAttribute('aria-multiselectable', 'true');
    const beta = screen.getByRole('option', { name: 'Beta' });
    const alpha = screen.getByRole('option', { name: 'Alpha' });
    expect(beta).toHaveAttribute('aria-selected', 'true');
    expect(beta.querySelector('[class*="checkOn"] svg')).not.toBeNull();
    expect(alpha.querySelector('svg')).toBeNull();
    expect(alpha).toHaveAttribute('aria-selected', 'false');
    expect(beta.className).not.toMatch(/optionActive/);
  });
  it('CSS: no rule on aria-selected sets a background; hover and active keep theirs', () => {
    const bad = [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(
      (m) => /aria-selected/.test(m[1] ?? '') && /background/.test(m[2] ?? ''),
    );
    expect(bad.map((m) => (m[1] ?? '').trim())).toEqual([]);
    expect(css).toMatch(/\.option:hover\s*\{[^}]*background/);
    expect(css).toMatch(/\.optionActive\s*\{[^}]*background/);
  });
});
