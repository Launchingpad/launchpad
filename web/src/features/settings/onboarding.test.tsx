import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { FakeEventSource } from '../../test/fakes';
import type { Onboarding, OnboardingStep, PlatformSettings } from '../../api/types';
import { computeSteps, isOpen, resetPrefsMemory, stepPages, type StepInputs } from './onboarding';

const IDS: OnboardingStep['id'][] = [
  'organisation',
  'mail',
  'signups',
  'team',
  'agent',
  'import',
  'githost',
  'invite',
];
const sam = { id: 'u9', username: 'sam', name: 'Sam' };

/** The server's answer. `done` and `skipped` name step ids. */
function onb(
  done: string[] = ['organisation'],
  skipped: string[] = [],
  capabilities: Onboarding['capabilities'] = ['gitHosts'],
): Onboarding {
  const steps = IDS.map((id) => ({
    id,
    done: done.includes(id),
    skipped: skipped.includes(id),
    skippedBy: skipped.includes(id) ? sam : null,
    skippedAt: skipped.includes(id) ? '2026-10-27T00:00:00Z' : null,
  }));
  return { steps, capabilities, remaining: 0 };
}
const base: StepInputs = { data: onb(), isPlatformAdmin: false };
const status = (i: StepInputs, id: string) => computeSteps(i).find((s) => s.id === id)?.status;
const step = (i: StepInputs, id: string) => computeSteps(i).find((s) => s.id === id)!;

/** The server's answer with the optional products step listed (two or more teams, no product). */
const withProducts = (skipped = false): Onboarding => {
  const o = onb();
  o.steps.push({
    id: 'products',
    done: false,
    skipped,
    skippedBy: skipped ? sam : null,
    skippedAt: skipped ? '2026-10-27T00:00:00Z' : null,
  });
  return o;
};

describe('the optional products step (LAUN-67)', () => {
  it('shows only when the server lists it', () => {
    expect(computeSteps(base).some((s) => s.id === 'products')).toBe(false);
    expect(computeSteps({ data: undefined, isPlatformAdmin: false }).some((s) => s.id === 'products')).toBe(
      false,
    );
    const s = step({ ...base, data: withProducts() }, 'products');
    expect(s.status).toBe('todo');
    expect(s.page).toBe('products');
    expect(s.optional).toBe(true);
  });
  it('never counts as open, so it cannot block done', () => {
    const steps = computeSteps({ ...base, data: withProducts() });
    expect(steps.find((x) => x.id === 'products')).toBeDefined();
    expect(isOpen(steps.find((x) => x.id === 'products')!)).toBe(false);
    expect(steps.filter(isOpen).length).toBe(computeSteps(base).filter(isOpen).length);
  });
  it('a skip shows', () => {
    expect(status({ ...base, data: withProducts(true) }, 'products')).toBe('skipped');
  });
  it('the page leaves it out of the "N of M" count and links to Products', async () => {
    server({ state: withProducts() });
    renderApp('/orbit/settings/setup');
    expect(await screen.findByRole('link', { name: /Open Products/ })).toHaveAttribute(
      'href',
      '/orbit/settings/products',
    );
    expect(await screen.findByRole('status')).toHaveTextContent('1 of 8 steps done, 6 still to do');
  });
  it('the banner does not count it', async () => {
    server({ state: withProducts() });
    renderApp('/orbit/issues');
    expect(await screen.findByRole('region', { name: 'Get Launchpad ready' })).toHaveTextContent(
      '6 steps are still to do',
    );
  });
});

describe('computeSteps (LAUN-98)', () => {
  it('takes done from the server', () => {
    expect(status(base, 'organisation')).toBe('done');
    expect(status(base, 'team')).toBe('todo');
    expect(status({ ...base, data: onb(['organisation', 'team']) }, 'team')).toBe('done');
    expect(status(base, 'agent')).toBe('todo');
    expect(status(base, 'invite')).toBe('todo');
    expect(status({ ...base, data: onb(['organisation', 'invite', 'agent']) }, 'invite')).toBe('done');
  });
  it('a skip shows, but never hides a step that is really done', () => {
    expect(status({ ...base, data: onb(['organisation'], ['team']) }, 'team')).toBe('skipped');
    expect(step({ ...base, data: onb(['organisation'], ['team']) }, 'team').detail).toBe('Skipped by Sam.');
    expect(status({ ...base, data: onb(['organisation', 'team'], ['team']) }, 'team')).toBe('done');
  });
  it('mail and sign-up mode have no needsPlatformAdmin status: todo for an org admin, with no link', () => {
    for (const id of ['mail', 'signups']) {
      const s = step(base, id);
      expect(s.status).toBe('todo');
      expect(s.page).toBeUndefined();
      expect(s.detail).toContain('Ask a platform admin.');
    }
    // They can be skipped (todo) and the skip shows.
    expect(status({ ...base, data: onb(['organisation'], ['mail', 'signups']) }, 'mail')).toBe('skipped');
  });
  it('a platform admin gets the Platform link; sign-up mode asks for a review', () => {
    const pa = { ...base, isPlatformAdmin: true };
    expect(step(pa, 'mail')).toMatchObject({ status: 'todo', page: 'platform' });
    expect(step(pa, 'signups')).toMatchObject({ status: 'review', page: 'platform' });
    expect(status({ ...pa, data: onb(['organisation', 'mail', 'signups']) }, 'signups')).toBe('done');
    expect(status({ ...pa, data: onb(['organisation'], ['signups']) }, 'signups')).toBe('skipped');
  });
  it('import and git host follow the server capabilities, and need the page', () => {
    expect(status(base, 'import')).toBe('soon');
    expect(step(base, 'import').action).toBe('Coming soon');
    expect(step(base, 'import').page).toBeUndefined();
    expect(status(base, 'githost')).toBe('todo');
    expect(step(base, 'githost').page).toBe('integrations');
    expect(status({ ...base, data: onb(['organisation'], [], ['linearImport', 'gitHosts']) }, 'import')).toBe(
      'todo',
    );
    expect(step({ ...base, data: onb(['organisation'], [], ['linearImport']) }, 'import').page).toBe(
      'import',
    );
    expect(status({ ...base, data: onb(['organisation'], [], []) }, 'githost')).toBe('soon');
  });
  it('every page it links to is a real settings page', () => {
    const src = readFileSync(resolve(process.cwd(), 'src/features/settings/settingsPages.ts'), 'utf8');
    for (const p of Object.values(stepPages)) expect(src, String(p)).toContain(`path: '${p}'`);
  });
  it('loading and error', () => {
    expect(status({ ...base, data: undefined }, 'team')).toBe('loading');
    expect(status({ ...base, data: undefined, failed: true }, 'organisation')).toBe('error');
  });
});

function server(
  over: {
    role?: 'admin' | 'member';
    platform?: boolean;
    user?: string;
    state?: Onboarding;
    routes?: Record<string, Handler>;
  } = {},
) {
  const state = { current: over.state ?? onb() };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: over.user ?? 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'c@e.com',
          isPlatformAdmin: over.platform ?? false,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: over.role ?? 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: state.current }),
    'PUT /api/v1/orgs/:org/onboarding/skip': (r) => {
      const b = r.body as { step: string; skipped: boolean };
      state.current = {
        ...state.current,
        steps: state.current.steps.map((s) =>
          s.id === b.step
            ? {
                ...s,
                skipped: b.skipped,
                skippedBy: b.skipped ? sam : null,
                skippedAt: b.skipped ? 'x' : null,
              }
            : s,
        ),
      };
      return { json: state.current };
    },
    ...over.routes,
  });
  return { api, state };
}

const onboardingCalls = (api: ReturnType<typeof mockApi>) =>
  api.calls.filter((c) => /\/onboarding/.test(c.url.pathname));

describe('checklist page (LAUN-98)', () => {
  beforeEach(() => {
    localStorage.clear();
    resetPrefsMemory();
  });

  it('lists the steps with statuses and links to the real pages', async () => {
    server();
    renderApp('/orbit/settings/setup');
    expect(await screen.findByRole('heading', { name: 'Get Launchpad ready' })).toHaveFocus();
    const items = await screen.findAllByRole('listitem');
    expect(items).toHaveLength(8);
    expect(within(items[0]!).getByText('Done')).toBeInTheDocument();
    // Mail: todo, no link for an org admin who is not a platform admin.
    expect(within(items[1]!).getByText('Not started')).toBeInTheDocument();
    expect(within(items[1]!).queryByRole('link')).not.toBeInTheDocument();
    expect(within(items[1]!).getByText(/Ask a platform admin/)).toBeInTheDocument();
    expect(within(items[5]!).getByText('Coming soon')).toBeInTheDocument();
    expect(within(items[5]!).queryByRole('link')).not.toBeInTheDocument();
    expect(within(items[6]!).getByRole('link', { name: /Open Integrations/ })).toHaveAttribute(
      'href',
      '/orbit/settings/integrations',
    );
    expect(within(items[3]!).getByRole('link', { name: /Open Teams/ })).toHaveAttribute(
      'href',
      '/orbit/settings/teams',
    );
    expect(await screen.findByRole('status')).toHaveTextContent('1 of 8 steps done, 6 still to do');
  });

  it('reads the server state with one request and no member, token, platform or team request', async () => {
    const { api } = server();
    renderApp('/orbit/settings/setup');
    await screen.findAllByRole('listitem');
    await screen.findByText('Done');
    await new Promise((r) => setTimeout(r, 50));
    expect(onboardingCalls(api).filter((c) => c.method === 'GET')).toHaveLength(1);
    expect(api.calls.filter((c) => /\/tokens|\/platform\//.test(c.url.pathname))).toEqual([]);
  });

  it('skip and undo call PUT .../onboarding/skip, and keep nothing but "dismissed" in this browser', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const skip = await screen.findByRole('button', { name: 'Skip Invite your team' });
    await user.click(skip);
    const undo = await screen.findByRole('button', { name: 'Undo skip of Invite your team' });
    // One button for both, so focus stays where it was.
    expect(undo).toBe(skip);
    expect(undo).toHaveFocus();
    const put = api.calls.filter((c) => c.method === 'PUT');
    expect(put[0]!.body).toEqual({ step: 'invite', skipped: true });
    expect(put[0]!.url.pathname).toBe('/api/v1/orgs/orbit/onboarding/skip');
    expect(screen.getByText('Skipped by Sam.')).toBeInTheDocument();
    expect(localStorage.getItem('launchpad-onboarding:orbit:u1')).toBeNull();
    await user.click(undo);
    expect(await screen.findByRole('button', { name: 'Skip Invite your team' })).toBeInTheDocument();
    expect(put).toBeDefined();
    expect(api.calls.filter((c) => c.method === 'PUT')[1]!.body).toEqual({ step: 'invite', skipped: false });
  });

  it('a skip made by another admin shows, with their name', async () => {
    server({ state: onb(['organisation'], ['agent']) });
    renderApp('/orbit/settings/setup');
    const item = (await screen.findAllByRole('listitem'))[4]!;
    expect(await within(item).findByText('Skipped')).toBeInTheDocument();
    expect(within(item).getByText('Skipped by Sam.')).toBeInTheDocument();
    expect(
      within(item).getByRole('button', { name: 'Undo skip of Agent and MCP token' }),
    ).toBeInTheDocument();
  });

  it('old skipped and reviewed values in localStorage are ignored', async () => {
    localStorage.setItem(
      'launchpad-onboarding:orbit:u1',
      JSON.stringify({ skipped: ['team'], reviewed: ['signups'], dismissed: false }),
    );
    server();
    renderApp('/orbit/settings/setup');
    const item = (await screen.findAllByRole('listitem'))[3]!;
    expect(await within(item).findByText('Not started')).toBeInTheDocument();
  });

  it('a failed skip says so in a fixed sentence and keeps the step', async () => {
    server({
      routes: {
        'PUT /api/v1/orgs/:org/onboarding/skip': () => ({
          status: 403,
          json: { error: 'secret server words', code: 'forbidden' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    await user.click(await screen.findByRole('button', { name: 'Skip Invite your team' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('You need admin access to do this.');
    expect(screen.queryByText(/secret server words/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Skip Invite your team' })).toBeInTheDocument();
  });

  it('the skip button is aria-disabled, not disabled, while a call is in flight; a second press sends nothing', async () => {
    let release: () => void = () => undefined;
    const gate = new Promise<void>((r) => (release = r));
    const { api, state } = server({
      routes: {
        'PUT /api/v1/orgs/:org/onboarding/skip': async () => {
          await gate;
          return { json: state.current };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const skip = await screen.findByRole('button', { name: 'Skip Invite your team' });
    await user.click(skip);
    await waitFor(() => expect(skip).toHaveAttribute('aria-disabled', 'true'));
    expect(skip).not.toBeDisabled();
    expect(skip).toHaveFocus();
    await user.click(skip);
    release();
    await waitFor(() => expect(skip).not.toHaveAttribute('aria-disabled'));
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
  });

  it('a platform admin marks sign-up mode reviewed: PUT /platform/settings with signupModeConfirmed, no secret', async () => {
    let platformBody: unknown;
    const { state } = server({
      platform: true,
      routes: {
        'GET /api/v1/platform/settings': () => ({ json: settings() }),
        'PUT /api/v1/platform/settings': (r) => {
          platformBody = r.body;
          return { json: settings({ signupModeChosen: true }) };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const items = await screen.findAllByRole('listitem');
    expect(within(items[2]!).getByText('Review')).toBeInTheDocument();
    expect(within(items[1]!).getByRole('link', { name: /Open Platform/ })).toHaveAttribute(
      'href',
      '/orbit/settings/platform',
    );
    // The server counts the step done after the PUT.
    state.current = onb(['organisation', 'signups']);
    await user.click(within(items[2]!).getByRole('button', { name: /reviewed/ }));
    expect(await within(items[2]!).findByText('Done')).toBeInTheDocument();
    expect(platformBody).toMatchObject({
      signupMode: 'invite',
      signupModeConfirmed: true,
      mailProvider: 'postmark',
    });
    expect(Object.keys(platformBody as object)).not.toContain('postmarkToken');
    expect(Object.keys(platformBody as object)).not.toContain('smtpPassword');
    // Focus moves to the step title when the button goes.
    await waitFor(() => expect(within(items[2]!).getByRole('heading')).toHaveFocus());
  });

  it('a 400 on Mark reviewed shows a fixed sentence', async () => {
    server({
      platform: true,
      routes: {
        'GET /api/v1/platform/settings': () => ({ json: settings() }),
        'PUT /api/v1/platform/settings': () => ({
          status: 400,
          json: { error: 'raw words', code: 'validation_failed' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const items = await screen.findAllByRole('listitem');
    await user.click(await within(items[2]!).findByRole('button', { name: /reviewed/ }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'The server did not accept this. Reload the page and try again.',
    );
    expect(screen.queryByText(/raw words/)).not.toBeInTheDocument();
  });

  it('an org admin who is not a platform admin has no Mark reviewed button, and can skip the step', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const items = await screen.findAllByRole('listitem');
    await within(items[2]!).findByText('Not started');
    expect(within(items[2]!).queryByRole('button', { name: /reviewed/ })).not.toBeInTheDocument();
    expect(api.calls.filter((c) => /\/platform\//.test(c.url.pathname))).toEqual([]);
    await user.click(within(items[2]!).getByRole('button', { name: 'Skip Sign-up mode' }));
    expect(await within(items[2]!).findByText('Skipped')).toBeInTheDocument();
  });

  it('a failed Mark reviewed shows a fixed sentence', async () => {
    server({
      platform: true,
      routes: {
        'GET /api/v1/platform/settings': () => ({ json: settings() }),
        'PUT /api/v1/platform/settings': () => ({
          status: 403,
          json: { error: 'raw words', code: 'forbidden' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const items = await screen.findAllByRole('listitem');
    await user.click(await within(items[2]!).findByRole('button', { name: /reviewed/ }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Only a platform admin can do this.');
    expect(screen.queryByText(/raw words/)).not.toBeInTheDocument();
    expect(within(items[2]!).getByText('Review')).toBeInTheDocument();
  });

  it('a capability from the server turns "Coming soon" into a step with a link', async () => {
    server({ state: onb(['organisation'], [], ['linearImport', 'gitHosts']) });
    renderApp('/orbit/settings/setup');
    const items = await screen.findAllByRole('listitem');
    await within(items[5]!).findByText('Not started');
    expect(within(items[5]!).getByRole('link', { name: /Open Import/ })).toHaveAttribute(
      'href',
      '/orbit/settings/import',
    );
  });

  it('a failed read shows an error with Retry', async () => {
    let fail = true;
    const { state } = server({
      routes: {
        'GET /api/v1/orgs/:org/onboarding': () =>
          fail ? { status: 500, json: { error: 'x', code: 'internal' } } : { json: state.current },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/setup');
    const retry = (await screen.findAllByRole('button', { name: /^Retry / }))[0]!;
    expect(screen.getAllByText('Could not load').length).toBeGreaterThan(0);
    fail = false;
    await user.click(retry);
    expect(
      await screen.findByText('1 of 8 steps done, 6 still to do.', { exact: false }),
    ).toBeInTheDocument();
  });

  it('a member cannot open the checklist page', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings/setup');
    expect(await screen.findByText('You need admin access.')).toBeInTheDocument();
  });

  it('the checklist page is in the settings search', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.type(await screen.findByRole('searchbox', { name: 'Search settings' }), 'checklist');
    expect(await screen.findByRole('link', { name: /Get started/ })).toBeInTheDocument();
  });
});

const settings = (over: Partial<PlatformSettings> = {}): PlatformSettings => ({
  mailProvider: 'postmark',
  postmarkFrom: 'a@b.c',
  postmarkMessageStream: '',
  postmarkTokenSet: true,
  smtpHost: '',
  smtpPort: 587,
  smtpSecurity: 'starttls',
  smtpUsername: '',
  smtpPasswordSet: false,
  smtpFromAddress: '',
  smtpFromName: '',
  signupMode: 'invite',
  signupModeChosen: false,
  activeMail: { transport: 'postmark', source: 'settings' },
  loginAvailable: true,
  encryptionKeySet: true,
  secretsUnreadable: false,
  updatedAt: '2026-01-01T00:00:00Z',
  updatedBy: 'u1',
  ...over,
});

describe('banner (LAUN-98)', () => {
  beforeEach(() => {
    localStorage.clear();
    resetPrefsMemory();
  });

  it('the reconnect bar and this banner are separate rows of one alert stack (LAUN-150)', async () => {
    server();
    renderApp('/orbit/issues');
    const banner = await screen.findByRole('region', { name: 'Get Launchpad ready' });
    await waitFor(() => expect(FakeEventSource.lastEvents).toBeDefined());
    act(() => {
      FakeEventSource.lastEvents!.open();
      FakeEventSource.lastEvents!.fail();
    });
    const bar = await screen.findByText('Reconnecting…');
    const stack = screen.getByTestId('alert-stack');
    expect(bar.parentElement).toBe(stack);
    expect(banner.parentElement).toBe(stack);
    expect(bar).toBeVisible();
    expect(banner).toBeVisible();
  });

  it('shows to an org admin with the open count, can be dismissed, and stores only "dismissed"', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const banner = await screen.findByRole('region', { name: 'Get Launchpad ready' });
    expect(banner).toHaveTextContent('6 steps are still to do');
    expect(within(banner).getByRole('link', { name: 'Open the checklist' })).toHaveAttribute(
      'href',
      '/orbit/settings/setup',
    );
    await user.click(within(banner).getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region', { name: 'Get Launchpad ready' })).not.toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem('launchpad-onboarding:orbit:u1')!)).toEqual({ dismissed: true });
    // The banner makes the one onboarding read and no member, token or platform read.
    expect(onboardingCalls(api).filter((c) => c.method === 'GET')).toHaveLength(1);
    expect(api.calls.filter((c) => /\/tokens|\/platform\//.test(c.url.pathname))).toEqual([]);
  });

  it('a skip by any admin takes the step out of the count; nothing open means no banner', async () => {
    server({
      state: onb(['organisation', 'team', 'agent', 'invite'], ['mail', 'signups', 'import', 'githost']),
    });
    renderApp('/orbit/issues');
    await screen.findByRole('heading', { name: 'All issues' });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole('region', { name: 'Get Launchpad ready' })).not.toBeInTheDocument();
  });

  it('counts one open step with the singular text', async () => {
    server({
      state: onb(['organisation', 'team', 'agent', 'invite'], ['mail', 'signups', 'import']),
    });
    renderApp('/orbit/issues');
    expect(await screen.findByRole('region', { name: 'Get Launchpad ready' })).toHaveTextContent(
      '1 step is still to do',
    );
  });

  it('a member sees no banner and makes no onboarding request', async () => {
    const { api } = server({ role: 'member' });
    renderApp('/orbit/issues');
    await screen.findByRole('heading', { name: 'All issues' });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole('region', { name: 'Get Launchpad ready' })).not.toBeInTheDocument();
    expect(onboardingCalls(api)).toEqual([]);
  });

  it('a platform admin who is only a member makes no onboarding or platform request', async () => {
    const { api } = server({ role: 'member', platform: true });
    renderApp('/orbit/issues');
    await screen.findByRole('heading', { name: 'All issues' });
    await new Promise((r) => setTimeout(r, 50));
    expect(onboardingCalls(api)).toEqual([]);
    expect(api.calls.filter((c) => /\/platform\//.test(c.url.pathname))).toEqual([]);
  });

  it('the banner is not drawn on a settings page', async () => {
    server();
    renderApp('/orbit/settings/organisation');
    await screen.findByRole('heading', { name: 'Organisation' });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole('region', { name: 'Get Launchpad ready' })).not.toBeInTheDocument();
  });

  it('dismiss still works when the browser blocks storage', async () => {
    server();
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    try {
      const user = userEvent.setup();
      renderApp('/orbit/issues');
      const banner = await screen.findByRole('region', { name: 'Get Launchpad ready' });
      await user.click(within(banner).getByRole('button', { name: 'Dismiss' }));
      expect(screen.queryByRole('region', { name: 'Get Launchpad ready' })).not.toBeInTheDocument();
    } finally {
      set.mockRestore();
    }
  });

  it('dismiss is not shared between two users or two orgs', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const banner = await screen.findByRole('region', { name: 'Get Launchpad ready' });
    await user.click(within(banner).getByRole('button', { name: 'Dismiss' }));
    expect(localStorage.getItem('launchpad-onboarding:orbit:u1')).toContain('dismissed');
    expect(localStorage.getItem('launchpad-onboarding:orbit:u2')).toBeNull();
    expect(localStorage.getItem('launchpad-onboarding:other:u1')).toBeNull();
  });
});

describe('touch targets and no hover underline (LAUN-98)', () => {
  const dir = resolve(process.cwd(), 'src/features/settings');
  const css = readFileSync(resolve(dir, 'onboarding.module.css'), 'utf8');
  const query = '@media (max-width: 760px), (pointer: coarse)';
  const touch = css.slice(css.indexOf(query));
  it('buttons and links are at least 40px on a phone and a coarse pointer', () => {
    expect(css).toContain(query);
    expect(Number(/min-height:\s*(\d+)px/.exec(touch)?.[1])).toBeGreaterThanOrEqual(40);
    expect(touch).toMatch(/\.touch a,\s*\.touch button/);
  });
  it('the page body and the banner carry the touch class', () => {
    expect(readFileSync(resolve(dir, 'SetupChecklistPage.tsx'), 'utf8')).toMatch(
      /className=\{styles\.touch\}/,
    );
    expect(readFileSync(resolve(dir, 'OnboardingBanner.tsx'), 'utf8')).toContain('styles.touch');
  });
  it('a button that waits (aria-disabled) has a visible style', () => {
    expect(css).toMatch(/\.touch button\[aria-disabled='true'\]\s*\{[^}]*opacity/);
  });
  it('nothing underlines on hover', () => {
    expect(css).not.toMatch(/text-decoration:\s*underline/);
    expect(css).toMatch(/\.touch a,\s*\.touch button\s*\{\s*text-decoration:\s*none/);
  });
});
