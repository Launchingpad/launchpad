import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it, vi } from 'vitest';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { applyLook, LOOK_KEY } from '../../lib/theme';
import { StatusIcon } from '../../ui/Icons';
import { saveFailedText, THEME_NAME } from './appearanceText';
import { searchSettings, settingsPages } from './settingsPages';

// LAUN-235 (decisions-T42-themes.md): Settings > Appearance, the theme sync and the first paint.

function server(stored = 'linear', routes: Record<string, Handler> = {}) {
  const state = { theme: stored };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/me/preferences': () => ({ json: { theme: state.theme } }),
    'PUT /api/v1/me/preferences': (r) => {
      state.theme = (r.body as { theme: string }).theme;
      return { json: { theme: state.theme } };
    },
    ...routes,
  });
  return { api, state };
}
const puts = (api: { calls: MockRequest[] }) =>
  api.calls.filter((c) => c.method === 'PUT' && c.url.pathname === '/api/v1/me/preferences');
const look = () => document.documentElement.getAttribute('data-look');

describe('settings registry', () => {
  it('Appearance is an Account page for every member and searchable by theme words', () => {
    const p = settingsPages.find((x) => x.path === 'appearance');
    expect(p).toMatchObject({ label: 'Appearance', group: 'Account', access: 'member' });
    expect(searchSettings(settingsPages, 'terminal').map((h) => h.page.path)).toContain('appearance');
    expect(searchSettings(settingsPages, 'default').map((h) => h.page.path)).toContain('appearance');
    expect(searchSettings(settingsPages, 'dark mode').map((h) => h.page.path)).toContain('appearance');
  });
});

describe('Appearance page', () => {
  it('shows the stored theme as a radio group with a preview of each, and Default (linear) sets no attribute', async () => {
    server('linear');
    renderApp('/orbit/settings/appearance');
    const group = await screen.findByRole('radiogroup', { name: 'Theme' });
    const radios = within(group).getAllByRole('radio');
    expect(radios.map((r) => (r as HTMLInputElement).value)).toEqual(['linear', 'paper', 'terminal']);
    expect(within(group).getByRole('radio', { name: 'Default' })).toBeChecked();
    expect(within(group).getByRole('radio', { name: 'Paper' })).toHaveAccessibleDescription(/serif headings/);
    // A live preview per theme, hidden from assistive technology.
    for (const l of ['linear', 'paper', 'terminal']) {
      const pv = document.querySelector(`[data-preview='${l}']`);
      expect(pv).not.toBeNull();
      expect(pv).toHaveAttribute('aria-hidden', 'true');
    }
    expect(look()).toBeNull();
  });

  it('applies a choice at once, saves it, and remembers it for the first paint', async () => {
    const { api, state } = server('linear');
    renderApp('/orbit/settings/appearance');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: 'Paper' }));
    expect(look()).toBe('paper');
    expect(screen.getByRole('radio', { name: 'Paper' })).toBeChecked();
    await waitFor(() => expect(puts(api)).toHaveLength(1));
    expect(puts(api)[0]!.body).toEqual({ theme: 'paper' });
    expect(state.theme).toBe('paper');
    expect(localStorage.getItem(LOOK_KEY)).toBe('paper');
    // Arrow keys move inside the group (native radios) and each step applies.
    await user.keyboard('{ArrowRight}');
    expect(look()).toBe('terminal');
    await waitFor(() => expect(state.theme).toBe('terminal'));
    // Back to Default (linear): no attribute, no mirror.
    await user.click(screen.getByRole('radio', { name: 'Default' }));
    expect(look()).toBeNull();
    expect(localStorage.getItem(LOOK_KEY)).toBeNull();
    await waitFor(() => expect(state.theme).toBe('linear'));
  });

  it('puts the theme back and says so when the save fails', async () => {
    const { api } = server('paper', {
      'PUT /api/v1/me/preferences': () => ({ status: 500, json: { error: 'x', code: 'internal' } }),
    });
    renderApp('/orbit/settings/appearance');
    await waitFor(() => expect(look()).toBe('paper'));
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: 'Terminal' }));
    await waitFor(() => expect(puts(api)).toHaveLength(1));
    expect(
      await screen.findByText('Could not save the theme. Launchpad shows Paper again.', { selector: 'p' }),
    ).toBeVisible();
    expect(look()).toBe('paper');
    expect(screen.getByRole('radio', { name: 'Paper' })).toBeChecked();
    expect(localStorage.getItem(LOOK_KEY)).toBe('paper');
  });

  // LAUN-235 review: two quick choices. The writes run in click order and only the latest choice's answer changes
  // what is shown; a failure goes back to the last value the server accepted.
  function deferredServer(stored = 'linear') {
    const state = { theme: stored, bodies: [] as string[], pending: [] as ((ok: boolean) => void)[] };
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/me/preferences': () => ({ json: { theme: state.theme } }),
      'PUT /api/v1/me/preferences': (r) =>
        new Promise((resolve) => {
          const theme = (r.body as { theme: string }).theme;
          state.bodies.push(theme);
          state.pending.push((ok) => {
            if (ok) state.theme = theme;
            resolve(ok ? { json: { theme } } : { status: 500, json: { error: 'x', code: 'internal' } });
          });
        }),
    });
    return state;
  }

  it('a slow failure of an older choice does not undo the newer one', async () => {
    const state = deferredServer('linear');
    renderApp('/orbit/settings/appearance');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: 'Paper' }));
    await user.click(screen.getByRole('radio', { name: 'Terminal' }));
    expect(look()).toBe('terminal');
    // Serialised: only the first PUT is out until it ends.
    await waitFor(() => expect(state.bodies).toEqual(['paper']));
    act(() => state.pending.shift()!(false));
    await waitFor(() => expect(state.bodies).toEqual(['paper', 'terminal']));
    expect(look()).toBe('terminal');
    act(() => state.pending.shift()!(true));
    await waitFor(() => expect(state.theme).toBe('terminal'));
    await act(async () => {});
    expect(look()).toBe('terminal');
    expect(localStorage.getItem(LOOK_KEY)).toBe('terminal');
    expect(screen.queryByText(/Could not save the theme/)).toBeNull();
  });

  it('two successful choices end on the last one, in the server too', async () => {
    const state = deferredServer('linear');
    renderApp('/orbit/settings/appearance');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: 'Paper' }));
    await user.click(screen.getByRole('radio', { name: 'Terminal' }));
    await waitFor(() => expect(state.bodies).toEqual(['paper']));
    act(() => state.pending.shift()!(true));
    await waitFor(() => expect(state.bodies).toEqual(['paper', 'terminal']));
    // The older answer arrived while a newer choice was waiting: the page still shows the newer one.
    expect(look()).toBe('terminal');
    act(() => state.pending.shift()!(true));
    await waitFor(() => expect(state.theme).toBe('terminal'));
    await act(async () => {});
    expect(look()).toBe('terminal');
  });

  it('when the latest choice fails, the theme goes back to the last one the server accepted', async () => {
    const state = deferredServer('linear');
    renderApp('/orbit/settings/appearance');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: 'Paper' }));
    await user.click(screen.getByRole('radio', { name: 'Terminal' }));
    await waitFor(() => expect(state.bodies).toEqual(['paper']));
    act(() => state.pending.shift()!(true));
    await waitFor(() => expect(state.bodies).toEqual(['paper', 'terminal']));
    act(() => state.pending.shift()!(false));
    expect(
      await screen.findByText('Could not save the theme. Launchpad shows Paper again.', { selector: 'p' }),
    ).toBeVisible();
    expect(look()).toBe('paper');
    expect(state.theme).toBe('paper');
    expect(screen.getByRole('radio', { name: 'Paper' })).toBeChecked();
  });

  it('chooses the light or dark variant in this browser only', async () => {
    const { api } = server('linear');
    renderApp('/orbit/settings/appearance');
    const user = userEvent.setup();
    const group = await screen.findByRole('radiogroup', { name: 'Light or dark' });
    expect(within(group).getByRole('radio', { name: 'Match the system' })).toBeChecked();
    await user.click(within(group).getByRole('radio', { name: 'Dark' }));
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
    expect(localStorage.getItem('devops-theme')).toBe('dark');
    expect(puts(api)).toHaveLength(0);
  });
});

describe('ThemeSync', () => {
  it('shows the theme the server holds (chosen on another device) and mirrors it', async () => {
    server('terminal');
    renderApp('/orbit/issues');
    await waitFor(() => expect(look()).toBe('terminal'));
    expect(localStorage.getItem(LOOK_KEY)).toBe('terminal');
  });

  it('keeps the remembered look when the server refuses (an agent token gets 403)', async () => {
    applyLook('paper');
    const { api } = server('linear', {
      'GET /api/v1/me/preferences': () => ({ status: 403, json: { error: 'x', code: 'forbidden' } }),
    });
    renderApp('/orbit/issues');
    await waitFor(() =>
      expect(api.calls.some((c) => c.url.pathname === '/api/v1/me/preferences')).toBe(true),
    );
    await act(async () => {});
    expect(look()).toBe('paper');
  });
});

describe('StatusIcon per theme', () => {
  it('Linear draws the state colour; Paper a ring in the theme colour; Terminal a hidden bracket tag', () => {
    const { container, rerender } = render(<StatusIcon type="started" color="#f2c94c" name="In Review" />);
    expect(container.querySelector('svg circle')).toHaveAttribute('stroke', '#f2c94c');
    act(() => applyLook('paper'));
    rerender(<StatusIcon type="started" color="#f2c94c" name="In Review" />);
    expect(container.querySelector('svg')).toHaveAttribute('data-state-glyph', 'review');
    act(() => applyLook('terminal'));
    rerender(<StatusIcon type="started" color="#f2c94c" name="In Review" />);
    const tag = container.querySelector('[data-state-tag]')!;
    expect(tag.textContent).toBe('[REVW]');
    expect(tag).toHaveAttribute('aria-hidden', 'true');
    rerender(<StatusIcon type="started" color="#f2c94c" name="In Progress" />);
    expect(container.querySelector('[data-state-tag]')!.textContent).toBe('[WIP ]');
    rerender(<StatusIcon type="completed" color="#f2c94c" look="linear" />);
    expect(container.querySelector('[data-state-tag]')).toBeNull();
  });
});

describe('theme-init.js (before the first paint)', () => {
  const src = readFileSync(resolve(process.cwd(), 'public/theme-init.js'), 'utf8');
  const run = () => new Function(src)();
  it('sets the remembered look and variant, and ignores anything else', () => {
    localStorage.setItem(LOOK_KEY, 'terminal');
    localStorage.setItem('devops-theme', 'light');
    run();
    expect(look()).toBe('terminal');
    expect(document.documentElement).toHaveAttribute('data-theme', 'light');
    document.documentElement.removeAttribute('data-look');
    localStorage.setItem(LOOK_KEY, 'studio');
    run();
    expect(look()).toBeNull();
  });
  it('is loaded as a file from index.html, before the app (the CSP forbids inline scripts)', () => {
    const html = readFileSync(resolve(process.cwd(), 'index.html'), 'utf8');
    expect(html.indexOf('<script src="/theme-init.js"></script>')).toBeGreaterThan(0);
    expect(html.indexOf('/theme-init.js')).toBeLessThan(html.indexOf('/src/main.tsx'));
  });
});

describe('LAUN-235 review follow-ups', () => {
  it('sign-out forgets the theme: the login page and the next load are Default', async () => {
    const state = { signedOut: false };
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/me/preferences': () => ({ json: { theme: 'terminal' } }),
      'GET /api/v1/auth/session': () =>
        state.signedOut
          ? { status: 401, json: { error: 'x', code: 'unauthenticated' } }
          : {
              json: {
                user: {
                  id: 'u1',
                  username: 'chen',
                  name: 'Chen',
                  email: 'c@example.com',
                  isPlatformAdmin: false,
                },
                orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
                csrfToken: 'csrf-test',
              },
            },
      'POST /api/v1/auth/logout': () => {
        state.signedOut = true;
        return { status: 204 };
      },
    });
    renderApp('/orbit/issues');
    await waitFor(() => expect(look()).toBe('terminal'));
    expect(localStorage.getItem(LOOK_KEY)).toBe('terminal');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'User menu' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in to Launchpad 🚀' })).toBeInTheDocument();
    expect(look()).toBeNull();
    expect(localStorage.getItem(LOOK_KEY)).toBeNull();
    // The next load: theme-init.js finds no copy, so the first paint is Default.
    new Function(readFileSync(resolve(process.cwd(), 'public/theme-init.js'), 'utf8'))();
    expect(look()).toBeNull();
  });

  it('another tab that changes the theme or the variant is followed here', async () => {
    server('linear');
    renderApp('/orbit/settings/appearance');
    const group = await screen.findByRole('radiogroup', { name: 'Theme' });
    act(() => {
      localStorage.setItem(LOOK_KEY, 'paper');
      window.dispatchEvent(new StorageEvent('storage', { key: LOOK_KEY, newValue: 'paper' }));
    });
    expect(look()).toBe('paper');
    expect(within(group).getByRole('radio', { name: 'Paper' })).toBeChecked();
    act(() => {
      localStorage.setItem('devops-theme', 'dark');
      window.dispatchEvent(new StorageEvent('storage', { key: 'devops-theme', newValue: 'dark' }));
    });
    expect(screen.getByRole('radio', { name: 'Dark' })).toBeChecked();
    await waitFor(() => expect(document.documentElement).toHaveAttribute('data-theme', 'dark'));
    act(() => {
      localStorage.removeItem(LOOK_KEY);
      window.dispatchEvent(new StorageEvent('storage', { key: LOOK_KEY, newValue: null }));
    });
    expect(look()).toBeNull();
  });

  it('a variant shows even when storage is full (setItem throws, getItem works)', async () => {
    server('linear');
    renderApp('/orbit/settings/appearance');
    const user = userEvent.setup();
    const group = await screen.findByRole('radiogroup', { name: 'Light or dark' });
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('full', 'QuotaExceededError');
    });
    try {
      await user.click(within(group).getByRole('radio', { name: 'Dark' }));
      expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
      expect(within(group).getByRole('radio', { name: 'Dark' })).toBeChecked();
    } finally {
      spy.mockRestore();
    }
  });

  it('the command palette offers the themes and the light or dark modes', async () => {
    const { state } = server('linear');
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'All issues' });
    await user.keyboard('{Meta>}k{/Meta}');
    const box = await screen.findByRole('combobox', { name: 'Search issues and commands' });
    await user.type(box, 'mode');
    expect(await screen.findByRole('option', { name: /Dark mode/ })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: /Match system mode/ })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: /Dark theme/ })).toBeNull();
    await user.clear(box);
    await user.type(box, 'theme: paper');
    await user.click(await screen.findByRole('option', { name: /Theme: Paper/ }));
    expect(look()).toBe('paper');
    await waitFor(() => expect(state.theme).toBe('paper'));
  });

  // LAUN-245: the look stored as `linear` is shown as "Default".
  it('the command palette names the linear theme "Default"', async () => {
    const { state } = server('paper');
    renderApp('/orbit/issues');
    const user = userEvent.setup();
    await screen.findByRole('heading', { name: 'All issues' });
    await user.keyboard('{Meta>}k{/Meta}');
    const box = await screen.findByRole('combobox', { name: 'Search issues and commands' });
    await user.type(box, 'theme: default');
    await user.click(await screen.findByRole('option', { name: /Theme: Default/ }));
    expect(screen.queryByRole('option', { name: /Theme: Linear/ })).toBeNull();
    expect(look()).toBeNull();
    await waitFor(() => expect(state.theme).toBe('linear'));
  });
});

describe('the visible name of the linear theme (LAUN-245)', () => {
  it('is "Default", and the save-failed text uses it', () => {
    expect(THEME_NAME.linear).toBe('Default');
    expect(saveFailedText('linear')).toBe('Could not save the theme. Launchpad shows Default again.');
  });
});
