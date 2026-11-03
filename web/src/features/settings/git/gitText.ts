import { ApiError } from '../../../api/errors';
import type {
  BackfillResult,
  HookRefreshResult,
  CreateGitIntegrationInput,
  GitIntegration,
  GitRepo,
} from '../../../api/types';

/**
 * Provider table (LAUN-9, decisions-T17 #46, #23). Bitbucket (LAUN-84) is one row for Cloud and Data Center:
 * the connect dialog, the list and the detail page read this table and nothing else.
 */
/** The providers the server accepts on create (CreateGitIntegrationInput). */
export type ProviderId = CreateGitIntegrationInput['provider'];

export interface ProviderInfo {
  id: ProviderId;
  label: string;
  /** The hosted address, prefilled. */
  hostedUrl: string;
  /** What "self-hosted" means for this provider, for the checkbox. */
  selfHostedLabel: string;
  /** The minimal token scopes (decisions-T17 #23), one line each. */
  scopes: readonly string[];
  /** One sentence on the token kind. */
  scopeNote: string;
  /**
   * What the token usually needs for each host call, in fixed words (LAUN-103). Used by a 403
   * (`provider_forbidden`) message. Guidance from documentation, unverified: it says "usually".
   */
  needs: { hook: string; list: string; check: string };
  /** The path of the host's token page, after the base URL. Empty: the host has no one page, so no link. */
  tokenPage: string;
  /** The exact header and event a webhook-only hook needs (decisions-T17 #47). */
  hookHelp: readonly string[];
}

export const PROVIDERS: readonly ProviderInfo[] = [
  {
    id: 'gitlab',
    label: 'GitLab',
    hostedUrl: 'https://gitlab.com',
    selfHostedLabel: 'My GitLab runs on my own server',
    scopes: [
      'Classic (legacy) personal token, or a project access token: scope api, and role Maintainer (or the user is a Maintainer)',
      'If your GitLab shows the new fine-grained form (Add resource permissions), usually: Resource access "Group and project", then read for Projects and Merge requests, and read and write for project hooks (named Webhooks or Integrations)',
    ],
    scopeNote:
      'The api scope is wider than Launchpad needs. It is the narrowest classic scope that can add project hooks: read_api cannot. The fine-grained permission names change between GitLab versions, so that line can be wrong. If you cannot find them, make a classic token.',
    needs: {
      hook: 'permission to manage project hooks (usually scope api and role Maintainer, or the fine-grained read and write on project hooks)',
      list: 'permission to list your projects (usually scope api, or the fine-grained read on Projects)',
      check:
        'permission to read your projects and merge requests (usually scope api, or the fine-grained read on Projects and Merge requests)',
    },
    tokenPage: '/-/user_settings/personal_access_tokens',
    hookHelp: [
      'Trigger: Merge request events',
      'Secret token: the secret from this page. GitLab sends it in the X-Gitlab-Token header.',
      'Enable SSL verification',
    ],
  },
  {
    id: 'github',
    label: 'GitHub',
    hostedUrl: 'https://github.com',
    selfHostedLabel: 'My GitHub is GitHub Enterprise Server',
    scopes: [
      'Fine-grained token (better): Metadata: read, and Webhooks: read and write, on the chosen repositories',
      'Classic token: repo and admin:repo_hook',
    ],
    scopeNote:
      'A fine-grained token is better: it reaches only the repositories you choose. To read pull requests for a backfill it usually also needs Pull requests: read. GitHub changes the form from time to time, so these names can be wrong.',
    needs: {
      hook: 'Webhooks: read and write on this repository (a classic token: admin:repo_hook)',
      list: 'access to the repositories, with Metadata: read (a classic token: repo)',
      check:
        'at least one permission (a classic token with no scope can do nothing; a fine-grained token needs Metadata: read)',
    },
    tokenPage: '/settings/personal-access-tokens/new',
    hookHelp: [
      'Content type: application/json',
      'Secret: the secret from this page. GitHub signs each delivery with it (X-Hub-Signature-256).',
      'Events: Pull requests',
      'Enable SSL verification',
    ],
  },
  {
    id: 'bitbucket',
    label: 'Bitbucket',
    hostedUrl: 'https://bitbucket.org',
    selfHostedLabel: 'My Bitbucket is Bitbucket Data Center',
    scopes: [
      'Bitbucket Cloud: a repository access token with Webhooks: read and write, Repositories: read and Pull requests: read',
      'Bitbucket Data Center: an HTTP access token with Repository admin on the chosen repositories',
    ],
    scopeNote:
      'Use a repository access token (Cloud) or an HTTP access token (Data Center). An app password needs a user name, so Launchpad does not take one. Bitbucket changes its token types and permission names, so this text can be wrong. Bitbucket has no one token page for every install, so there is no link.',
    needs: {
      hook: 'Webhooks: read and write (Cloud), or Repository admin (Data Center)',
      list: 'Repositories: read (Cloud), or read access to the repositories (Data Center)',
      check: 'Repositories: read and Pull requests: read (Cloud), or read access (Data Center)',
    },
    tokenPage: '',
    hookHelp: [
      'Secret: the secret from this page. Bitbucket signs each delivery with it (X-Hub-Signature).',
      'Events: Pull request created, updated, merged, declined and approved; Data Center also reviewer needs work. For build results: Cloud commit status created and updated.',
      'Bitbucket Data Center: the hook must have a secret. A version that cannot sign webhooks cannot be used with Launchpad.',
      'Enable SSL verification',
    ],
  },
];

export function providerInfo(id: string): ProviderInfo | undefined {
  return PROVIDERS.find((p) => p.id === id);
}
export function providerLabel(id: string): string {
  return providerInfo(id)?.label ?? id;
}

export function modeLabel(mode: GitIntegration['mode']): string {
  return mode === 'token' ? 'Token' : 'Webhook only';
}

export const STATUS_LABEL: Record<GitIntegration['status'], string> = {
  ok: 'Working',
  needs_attention: 'Needs attention',
  disabled: 'Disabled',
};

/** What each `attentionCode` means and what to do (decisions-T17 #11a, #47). Fixed text only. */
export const ATTENTION: Record<
  NonNullable<GitIntegration['attentionCode']>,
  { title: string; fix: string }
> = {
  secret_unreadable: {
    title: 'Launchpad cannot read the stored secret.',
    fix: 'The server key may have changed. Until you rotate the secret, this webhook answers 401 to the host. Rotate the secret below.',
  },
  token_rejected: {
    title: 'The host refused the token.',
    fix: 'The token may be revoked, expired or missing a scope. Replace the token, then press Check token.',
  },
  attention_unreachable: {
    title: 'Launchpad could not reach the host.',
    fix: 'Check the address and that the host is up. Then press Check token.',
  },
  hook_failed: {
    title: 'A hook could not be made or updated on the host.',
    fix: 'Look at the hook state of each repository below. Press Check token, then try again.',
  },
};

export function attentionText(code: string | null | undefined): { title: string; fix: string } | null {
  if (!code) return null;
  return (
    (ATTENTION as Record<string, { title: string; fix: string }>)[code] ?? {
      title: 'This connection needs attention.',
      fix: 'Press Check token. If that does not help, rotate the secret.',
    }
  );
}

/** The sentence of decisions-T17 #13: what disable does and does not do. */
export const DISABLE_SENTENCE =
  'Disable stops deliveries only. Hooks stay on the host until you unmap the repository. A host may turn off a hook that keeps getting 401.';

/** The hook state of a mapped repository, in words. Never host text: `hookError` is one of our short codes. */
export function hookText(repo: Pick<GitRepo, 'hookState' | 'hookError'>): string {
  if (repo.hookState === 'none') return 'You manage the hook';
  if (repo.hookState === 'active') return 'Hook working';
  switch (repo.hookError) {
    case 'not_updated':
      return 'Hook not on the new secret yet';
    case 'secret_discarded':
      return 'Hook holds a discarded secret';
    case 'provider_unauthorized':
      return 'Hook failed: the host refused the token';
    case 'provider_forbidden':
      return 'Hook failed: the token lacks a scope';
    case 'provider_unreachable':
      return 'Hook failed: host not reachable';
    case 'provider_rate_limited':
      return 'Hook failed: host rate limit';
    default:
      return 'Hook failed';
  }
}

export function waitText(seconds: number | undefined): string {
  if (!seconds || seconds <= 1) return 'a moment';
  if (seconds < 90) return `${seconds} seconds`;
  return `${Math.ceil(seconds / 60)} minutes`;
}

export type GitContext =
  'connect' | 'token' | 'rotate' | 'repos' | 'list' | 'check' | 'hook' | 'backfill' | 'remove' | 'other';

/**
 * The 403 sentence (`provider_forbidden`, LAUN-103). The call (ctx) and the provider choose the fixed words:
 * the host's own words never come back. 'repos' is a map or an unmap, and 'rotate' pushes secrets to the hooks (both are hook writes), 'list' the repository
 * list, 'hook' a hook refresh, 'backfill' a read of merge requests. The "usually" is on purpose: the names are
 * from documentation and unverified (decisions-T17 "LAUN-103").
 */
export function forbiddenText(ctx: GitContext, provider?: string): string {
  const info = provider ? providerInfo(provider) : undefined;
  const base = 'The host says the token is not allowed to do this.';
  if (!info)
    return `${base} The token does not have the scopes it needs. See the list of scopes, make a new token and try again.`;
  const [kind, action]: ['hook' | 'list' | 'check', string] =
    ctx === 'repos' || ctx === 'hook' || ctx === 'rotate'
      ? ['hook', 'change hooks']
      : ctx === 'list'
        ? ['list', 'list repositories']
        : ctx === 'backfill'
          ? ['check', 'read merge requests']
          : ['check', 'read'];
  return `${base} The token cannot ${action} on ${info.label}. It usually needs ${info.needs[kind]}. Add that to the token, or make a new token, and try again.`;
}

/**
 * Plain words for a failed call. SERVER TEXT NEVER REACHES THE SCREEN: only the status and the `code` choose
 * a fixed sentence, so a token or a host's words cannot come back in an error (decisions-T17 #51, AGENTS.md
 * "A provider error ... never the token, a URL or the host's words"). A network failure carries fixed client text.
 */
export function gitErrorText(err: unknown, ctx: GitContext = 'other', provider?: string): string {
  if (!(err instanceof ApiError)) return 'Something went wrong. Try again.';
  switch (err.code) {
    case 'provider_unreachable':
      return 'Launchpad could not reach the host. Check the web address and that the host is up.';
    case 'provider_unauthorized':
      return 'The host refused the token. Check that it is correct and has not expired.';
    case 'provider_forbidden':
      return forbiddenText(ctx, provider);
    case 'provider_rate_limited':
      return `The host is limiting requests. Try again in ${waitText(err.retryAfter)}.`;
    case 'provider_error':
      return 'The host answered with an error. Try again.';
    case 'integration_limit':
      return 'This provider already has 8 connections turned on. Turn one off first.';
    case 'encryption_unavailable':
      return 'This server has no SETTINGS_ENCRYPTION_KEY, so it cannot keep a token or a secret. Ask the person who runs the install to set it.';
    case 'public_url_required':
      return 'Token mode needs PUBLIC_BASE_URL set to an https address, so the host can call Launchpad. Ask the person who runs the install, or use webhook only.';
    case 'integration_exists':
      return 'A connection to this host already exists for this provider. Open the existing connection, or remove it and connect again.';
    case 'secret_in_use':
      return 'Another connection of this provider that is turned on already uses this webhook secret. Use a different secret, or let Launchpad make a new one.';
    case 'secret_hash_only':
      return 'This connection keeps its webhook secret in a form that cannot be sent to the host, so it cannot use a token yet. Rotate the webhook secret first, and put the new secret in the hook on the host.';
    case 'repos_mapped':
      return ctx === 'remove'
        ? 'This connection still has mapped repositories. Unmap them first, then remove the connection.'
        : 'The web address cannot change while a repository is mapped. Unmap the repositories first.';
    case 'integration_changed':
      return 'This connection changed while you were saving. Close this window, look at the connection again and then try once more.';
    case 'rate_limited':
      return `Too many tries. Wait ${waitText(err.retryAfter)} and try again.`;
    case 'insufficient_scope':
    case 'forbidden':
      return 'You cannot do this. Only an organisation admin can change git connections.';
    case 'not_found':
      return 'That item was not found. It may have been removed.';
  }
  if (err.status === 0) return err.message; // fixed client text: could not reach the server
  if (err.status === 400)
    return ctx === 'repos'
      ? 'The server did not accept this repository. Check the id and the path.'
      : 'The server did not accept this. Check the fields.';
  if (ctx === 'hook' || ctx === 'backfill') {
    // The two calls of LAUN-95 can answer 429 and 502/503 with no code we know; other contexts keep their text.
    if (err.status === 429) return `Too many tries. Wait ${waitText(err.retryAfter)} and try again.`;
    if (err.status === 502 || err.status === 503) return 'The host answered with an error. Try again.';
  }
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 409) {
    if (ctx === 'repos')
      return 'This repository is already mapped, or the connection changed. Check the list of repositories and try again.';
    if (ctx === 'hook')
      return 'Launchpad cannot update this hook now. A secret rotation may be waiting (commit or discard it first), or this connection has no usable token.';
    if (ctx === 'backfill')
      return 'Launchpad cannot read this repository now. A secret rotation may be waiting, or this connection has no token.';
    if (ctx === 'check') return 'This connection has no token to check. Use token mode.';
    if (ctx === 'rotate')
      return 'There is no new secret to commit, or the hooks are not all updated. Look at the state of the hooks.';
    return 'The server refused this because of the current state of the connection. Look at the connection and try again.';
  }
  if (err.status >= 500) return 'The server had a problem. Try again.';
  return 'Something went wrong. Try again.';
}

/**
 * Which form field a 400 is about. The server's message is READ for field names and never shown.
 * (The same approach as the token form.)
 */
export function badField(err: unknown): 'baseUrl' | 'apiBaseUrl' | 'secret' | 'token' | 'name' | null {
  if (!(err instanceof ApiError) || err.status !== 400) return null;
  const m = err.message;
  if (m.includes('apiBaseUrl')) return 'apiBaseUrl';
  if (m.includes('baseUrl')) return 'baseUrl';
  if (m.includes('secret')) return 'secret';
  if (m.includes('token')) return 'token';
  if (m.includes('name')) return 'name';
  return null;
}

export const FIELD_TEXT = {
  baseUrl:
    'The server did not accept this address. It must be an https address of a host Launchpad may call. A private or internal address needs an operator setting (GIT_ALLOW_PRIVATE_PROVIDERS).',
  apiBaseUrl: 'The server did not accept the API address. It follows the same rules as the web address.',
  secret: 'The server did not accept this secret. Use 32 to 256 characters with no space.',
  token: 'The server did not accept this token.',
  name: 'The server did not accept this name.',
} as const;

/** Client check before the call; the server stays the judge. */
export function checkBaseUrl(raw: string): string | null {
  const v = raw.trim();
  if (!v) return 'Enter the web address of the host.';
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    return 'Enter a full address, for example https://git.example.com.';
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return 'The address must start with https://.';
  if (u.username || u.password) return 'Remove the user name and password from the address.';
  if (u.search || u.hash) return 'Remove the query and the # part from the address.';
  return null;
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

/** The backfill answer in plain words (LAUN-95). */
export function backfillText(r: Pick<BackfillResult, 'seen' | 'linked' | 'moved' | 'truncated'>): string {
  const parts = [
    r.seen === 0
      ? 'No open merge or pull request was found.'
      : `Read ${plural(r.seen, 'open request', 'open requests')}.`,
    `${plural(r.linked, 'ticket got', 'tickets got')} a new link.`,
    `${plural(r.moved, 'ticket moved', 'tickets moved')} forward.`,
  ];
  if (r.truncated) parts.push('More open requests were left. Run it again after the oldest ones close.');
  return parts.join(' ');
}

/** The answer of a hook refresh, one short sentence per state (LAUN-95). */
export function refreshText(state: HookRefreshResult['hookState']): string {
  if (state === 'active') return 'The hook is up to date and working.';
  if (state === 'failed') return 'The hook could not be updated. Look at its state in the table.';
  return 'Launchpad does not manage this hook.';
}
