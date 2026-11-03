import { ApiError } from '../../api/errors';
import type { GitAccount } from '../../api/types';

// LAUN-199. Fixed sentences for Settings > Connected accounts. No server text reaches the screen, so a
// refusal never carries a name or a handle of someone else.

export function hostLabel(provider: string): string {
  return provider === 'github'
    ? 'GitHub'
    : provider === 'gitlab'
      ? 'GitLab'
      : provider === 'bitbucket'
        ? 'Bitbucket'
        : 'Git host';
}

/** The row title: the connection name; the host when the name already says it. */
export function connectionTitle(a: Pick<GitAccount, 'provider' | 'name' | 'host'>): string {
  return a.name && a.name !== a.host
    ? `${a.name} (${hostLabel(a.provider)})`
    : `${hostLabel(a.provider)}, ${a.host}`;
}

export function handleHint(provider: string): string {
  switch (provider) {
    case 'github':
      return 'Your GitHub username: letters, digits, "-" and "_", up to 39 characters.';
    case 'gitlab':
      return 'Your GitLab username: letters, digits, ".", "-" and "_", up to 100 characters.';
    default:
      return 'Your Bitbucket nickname or account id, up to 100 characters.';
  }
}

export function accountsLoadText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 401) return 'You are signed out. Sign in again.';
    if (err.status === 403)
      return 'Connected accounts are for people. An agent has no account on a git host.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return 'Could not load your connected accounts. Try again.';
}

/** The start of the server's 409 sentence for a turned-off connection (tracker.ConnectionOffText). */
export const CONNECTION_OFF_PREFIX = 'this connection is turned off';

/** The fixed text for a failed save or removal. A 409 never says who holds the handle. */
export function accountSaveText(err: unknown, provider = ''): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 400) return invalidHandleText(provider);
    if (err.status === 401) return 'You are signed out. Sign in again.';
    if (err.status === 403) return 'You cannot change connected accounts with this account.';
    if (err.status === 404) return 'This connection is gone. The list is updated.';
    if (err.status === 409 && err.message.startsWith(CONNECTION_OFF_PREFIX))
      return 'This connection is turned off. Ask an org admin to turn it on.';
    if (err.status === 409)
      return 'That handle is already linked on this connection. Check it, or ask an org admin if it is yours.';
    if (err.status === 429) return 'Too many requests. Wait a moment and try again.';
  }
  return 'Could not save the handle. Try again.';
}

export function identityRemoveText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 0) return 'Could not reach the server. Check your connection and try again.';
    if (err.status === 403) return 'Only an org admin can remove another member’s link.';
    if (err.status === 404) return 'That link is already gone. The list is updated.';
  }
  return 'Could not remove the link. Try again.';
}

/** The 400 sentence names the rule of the host; it matches CleanHandle on the server. */
export function invalidHandleText(provider: string): string {
  switch (provider) {
    case 'github':
      return 'That is not a valid GitHub username. Use 1 to 39 letters, digits, - or _, starting with a letter or digit.';
    case 'gitlab':
      return 'That is not a valid GitLab username. Use 1 to 100 characters: letters, digits, . - or _, not starting with . or -.';
    case 'bitbucket':
      return 'That is not a valid Bitbucket handle. Use 1 to 100 characters: letters, digits, space, . : @ - or _, starting with a letter, digit or _.';
    default:
      return 'That is not a valid handle for this host. Check it and try again.';
  }
}
