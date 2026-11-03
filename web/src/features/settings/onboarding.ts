import { useCallback, useEffect, useState } from 'react';
import { ApiError } from '../../api/errors';
import { useOrgSlug } from '../../api/org';
import { useConfirmSignupMode, useOnboardingState, useSetOnboardingSkip } from '../../api/queries';
import type { Onboarding, OnboardingStep } from '../../api/types';
import { useAuthedSession } from '../auth/session';
import { useIsPlatformAdmin } from './access';

/**
 * The first-run checklist "Get Launchpad ready" (LAUN-25, LAUN-98). The server works out `done` and keeps the
 * skips for the whole org (`GET /orgs/{org}/onboarding`, `PUT .../onboarding/skip`; decisions-T17-onboarding.md).
 * This browser keeps one thing only: whether the banner was dismissed.
 */
export type StepId = OnboardingStep['id'];
export type Capability = Onboarding['capabilities'][number];

/**
 * The settings page that does a step, by path after `/:org/settings/`. A step with no page here shows "Coming
 * soon" even when the server can do it. Remove the entry's page only if a page goes away.
 */
export const stepPages: Record<StepId, string | undefined> = {
  organisation: 'organisation',
  mail: 'platform',
  signups: 'platform',
  team: 'teams',
  agent: 'mcp',
  import: 'import',
  githost: 'integrations',
  invite: 'members',
  products: 'products',
};

/** The server capability a step needs. A step not listed needs none. */
export const stepCapability: Partial<Record<StepId, Capability>> = {
  import: 'linearImport',
  githost: 'gitHosts',
};

export type StepStatus = 'done' | 'todo' | 'review' | 'skipped' | 'soon' | 'loading' | 'error';

export interface Step {
  id: StepId;
  title: string;
  help: string;
  /** Path after `/:org/settings/`; absent for a step with no page the person may open. */
  page?: string;
  action: string;
  status: StepStatus;
  /** An optional step (LAUN-67): never counts as open and never blocks "done". */
  optional?: boolean;
  /** Why the step is in this state: one short line. */
  detail?: string;
}

export interface StepInputs {
  /** `undefined`: not loaded yet. */
  data: Onboarding | undefined;
  /** The read failed and there is no data to show. */
  failed?: boolean;
  isPlatformAdmin: boolean;
}

interface Meta {
  title: string;
  help: string;
  action: string;
  /** Needs a platform admin to open its page. */
  platform?: boolean;
  done: string;
  todo: string;
}

const META: Record<StepId, Meta> = {
  organisation: {
    title: 'Organisation',
    help: 'Setup made your organisation. Check its name and set its time zone.',
    action: 'Open Organisation',
    done: 'Made by setup.',
    todo: 'Made by setup.',
  },
  mail: {
    title: 'Mail',
    help: 'Login codes and invites go out by email. Choose a provider and send a test.',
    action: 'Open Platform',
    platform: true,
    done: 'Mail goes out.',
    todo: 'Mail does not go out yet.',
  },
  signups: {
    title: 'Sign-up mode',
    help: 'Choose who may join: invites only, sign-up requests with approval, or nobody.',
    action: 'Open Platform',
    platform: true,
    done: 'A platform admin chose the sign-up mode.',
    todo: 'Nobody chose the sign-up mode yet. Invites only is the safe default.',
  },
  team: {
    title: 'Team and cycles',
    help: 'Setup made your first team. Add more teams and turn cycles on where you want them.',
    action: 'Open Teams',
    done: 'Cycles are on for a team.',
    todo: 'No team has cycles on.',
  },
  agent: {
    title: 'Agent and MCP token',
    help: 'An agent user, such as Claude, with a token lets a program use Launchpad through MCP.',
    action: 'Open MCP',
    done: 'An agent has a token.',
    todo: 'No agent has a working token yet.',
  },
  import: {
    title: 'Import from Linear',
    help: 'Copy your issues, projects and cycles from Linear.',
    action: 'Open Import',
    done: 'Something was imported.',
    todo: 'Nothing was imported.',
  },
  githost: {
    title: 'Git host',
    help: 'Connect GitLab, GitHub or Bitbucket so merge requests move issues.',
    action: 'Open Integrations',
    done: 'A git host is connected.',
    todo: 'No git host is connected.',
  },
  invite: {
    title: 'Invite your team',
    help: 'Send invites so other people can sign in.',
    action: 'Open Members',
    done: 'Another person is in.',
    todo: 'You are the only person.',
  },
  products: {
    title: 'Group your teams into products',
    help: 'Optional. A product groups teams in the sidebar and filters the issue list. You can skip this.',
    action: 'Open Products',
    done: 'A product exists.',
    todo: 'You have two or more teams and no product.',
  },
};

/** Steps that are optional (LAUN-67; decisions-T18-products.md #32). The server lists them only while they apply. */
export const optionalSteps: readonly StepId[] = ['products'];

const ORDER: StepId[] = [
  'organisation',
  'mail',
  'signups',
  'team',
  'agent',
  'import',
  'githost',
  'invite',
  'products',
];

/** Pure: the steps with their status, from the server's answer. */
export function computeSteps(i: StepInputs): Step[] {
  const caps = i.data?.capabilities ?? [];
  return ORDER.flatMap((id): Step[] => {
    const server = i.data?.steps.find((s) => s.id === id);
    // An optional step shows only when the server lists it (two or more teams, no product).
    if (optionalSteps.includes(id) && !server) return [];
    return [one(id, server)];
  });

  function one(id: StepId, server: Onboarding['steps'][number] | undefined): Step {
    const m = META[id];
    const need = stepCapability[id];
    const page = stepPages[id];
    // Needs the platform pages? Only a platform admin may open them.
    const reachable = page !== undefined && !(m.platform && !i.isPlatformAdmin);
    const base = {
      id,
      title: m.title,
      help: m.help,
      action: m.action,
      page: reachable ? page : undefined,
      optional: optionalSteps.includes(id) || undefined,
    };
    if (!i.data) return { ...base, status: i.failed ? 'error' : 'loading' };
    // The server can do it and the web has the page, or the step is "Coming soon".
    if (page === undefined || (need && !caps.includes(need))) {
      return { ...base, page: undefined, action: 'Coming soon', status: 'soon' };
    }
    const done = server?.done === true;
    // A skip shows, but never hides a step that is really done.
    if (!done && server?.skipped) {
      const who = server.skippedBy?.name;
      return { ...base, status: 'skipped', detail: who ? `Skipped by ${who}.` : 'Skipped.' };
    }
    if (done) return { ...base, status: 'done', detail: m.done };
    if (m.platform && !i.isPlatformAdmin) {
      return { ...base, status: 'todo', detail: `${m.todo} Ask a platform admin.` };
    }
    return { ...base, status: id === 'signups' ? 'review' : 'todo', detail: m.todo };
  }
}

/** A step still waiting for the person. */
export const isOpen = (s: Step): boolean => !s.optional && (s.status === 'todo' || s.status === 'review');

/** A fixed sentence for a failed skip or confirm. No server text reaches the screen. */
export function onboardingErrorText(err: unknown, what: 'skip' | 'confirm'): string {
  const e = err instanceof ApiError ? err : undefined;
  if (!e) return 'Something went wrong. Try again.';
  switch (e.status) {
    case 0:
      return 'Could not reach the server. Check your connection and try again.';
    case 400:
      return 'The server did not accept this. Reload the page and try again.';
    case 401:
      return 'You are signed out. Sign in again.';
    case 403:
      return what === 'confirm' ? 'Only a platform admin can do this.' : 'You need admin access to do this.';
    case 409:
      return 'The server cannot store this now. Ask the person who runs the install.';
    case 429:
      return 'Too many requests. Wait a moment and try again.';
    default:
      return what === 'confirm'
        ? 'Could not save the sign-up mode. Try again.'
        : 'Could not save the skip. Try again.';
  }
}

// ---------- the one choice stored in this browser ----------

interface Prefs {
  dismissed: boolean;
}
const key = (org: string, user: string) => `launchpad-onboarding:${org}:${user}`;

// Kept when storage is blocked, so the banner still goes away until the page closes.
const memory = new Map<string, Prefs>();

/** For tests. */
export const resetPrefsMemory = () => memory.clear();

function read(org: string, user: string): Prefs {
  const kept = memory.get(key(org, user));
  if (kept) return kept;
  try {
    const v = JSON.parse(localStorage.getItem(key(org, user)) ?? 'null') as Partial<Prefs> | null;
    return { dismissed: v?.dismissed === true };
  } catch {
    return { dismissed: false };
  }
}

/** The banner choice. Skip and the sign-up mode are org state on the server (LAUN-98), not here. */
export function usePrefs() {
  const org = useOrgSlug();
  const user = useAuthedSession().user.id;
  const [prefs, setPrefs] = useState<Prefs>(() => read(org, user));
  useEffect(() => setPrefs(read(org, user)), [org, user]);
  const save = useCallback(
    (next: Prefs) => {
      setPrefs(next);
      memory.delete(key(org, user));
      try {
        // Only `dismissed`. An older build also wrote `skipped` and `reviewed`; they are dropped on the next save.
        localStorage.setItem(key(org, user), JSON.stringify(next));
      } catch {
        memory.set(key(org, user), next); // storage blocked: the choice lasts until the page closes
      }
      window.dispatchEvent(new Event('launchpad-onboarding'));
    },
    [org, user],
  );
  // Another component (the banner and the page) changed it.
  useEffect(() => {
    const on = () => setPrefs(read(org, user));
    window.addEventListener('launchpad-onboarding', on);
    return () => window.removeEventListener('launchpad-onboarding', on);
  }, [org, user]);
  return { ...prefs, setDismissed: (dismissed: boolean) => save({ dismissed }) };
}

/**
 * The steps for the current org and person. Call it only for an org admin (mount the component only for
 * one): the onboarding route is admin only. It makes ONE request (plus the skip and confirm writes).
 */
export function useOnboarding() {
  const isPlatformAdmin = useIsPlatformAdmin();
  const prefs = usePrefs();
  const q = useOnboardingState();
  const skip = useSetOnboardingSkip();
  const confirm = useConfirmSignupMode();
  const [error, setError] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const steps = computeSteps({ data: q.data, failed: q.isError, isPlatformAdmin });
  const retry = () => void q.refetch();
  const setSkipped = (step: StepId, skipped: boolean) => {
    // One call at a time. The button is aria-disabled, not disabled, so focus stays on it; a second press is ignored.
    if (skip.isPending) return;
    setError(null);
    skip.mutate({ step, skipped }, { onError: (e) => setError(onboardingErrorText(e, 'skip')) });
  };
  const confirmSignups = async (): Promise<boolean> => {
    if (confirming) return false;
    setError(null);
    setConfirming(true);
    try {
      await confirm();
      return true;
    } catch (e) {
      setError(onboardingErrorText(e, 'confirm'));
      return false;
    } finally {
      setConfirming(false);
    }
  };
  return {
    steps,
    prefs,
    retry,
    setSkipped,
    confirmSignups,
    busy: skip.isPending || confirming,
    error,
    loaded: steps.every((s) => s.status !== 'loading'),
  };
}
