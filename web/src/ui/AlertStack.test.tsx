import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AlertStack } from './AlertStack';

// jsdom has no layout: check the markup (each alert in its own row) and the CSS rule that stacks them (LAUN-150).
const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8');

describe('AlertStack', () => {
  it('renders two alerts as separate children of one stack, none hidden', () => {
    render(
      <AlertStack>
        <div role="status">Reconnecting…</div>
        <section aria-label="Get Launchpad ready">Finish setup</section>
        {false}
      </AlertStack>,
    );
    const stack = screen.getByTestId('alert-stack');
    expect(stack.children).toHaveLength(2);
    expect(screen.getByRole('status').parentElement).toBe(stack);
    expect(screen.getByLabelText('Get Launchpad ready').parentElement).toBe(stack);
    expect(screen.getByText('Reconnecting…')).toBeVisible();
    expect(screen.getByText('Finish setup')).toBeVisible();
  });

  it('stacks in a column with a gap, wraps long words and takes no space when empty', () => {
    const rule = /\.alertStack \{[^}]*\}/.exec(css)![0];
    expect(rule).toMatch(/flex-direction:\s*column/);
    expect(rule).toMatch(/gap:\s*8px/);
    expect(css).toMatch(/\.alertStack:empty \{[^}]*display:\s*none/);
    expect(css).toMatch(/\.alertStack > \* \{[^}]*overflow-wrap:\s*anywhere/);
  });
});
