import type { ChangeRequest } from '../../api/types';

/** Words for the Changes block and the MR rows of the Activity (LAUN-95). Plain text only. */

export function providerLabel(provider: string): string {
  return provider === 'gitlab'
    ? 'GitLab'
    : provider === 'github'
      ? 'GitHub'
      : provider === 'bitbucket'
        ? 'Bitbucket'
        : 'The host';
}

/** GitLab says "merge request", the others "pull request". */
export function requestNoun(provider: string): string {
  return provider === 'gitlab' ? 'merge request' : 'pull request';
}

/** `!12` on GitLab, `#12` elsewhere; empty when the number is unknown. */
export function requestRef(provider: string, number: number | null): string {
  if (number === null || !Number.isFinite(number)) return requestNoun(provider);
  return `${provider === 'gitlab' ? '!' : '#'}${number}`;
}

/** The link target of a host URL: http(s) only. The server checks it; the web checks again. */
export function safeHttpUrl(url: string | null | undefined): string | null {
  return url && /^https?:\/\//i.test(url) ? url : null;
}

/** The path tail after the repository in a request's web URL, per host (LAUN-239). */
const REQUEST_TAIL: Record<string, string> = {
  gitlab: '/-/merge_requests/', // GitLab.com and self-managed: <base>/<group>/<project>/-/merge_requests/<iid>
  github: '/pull/', // github.com and Enterprise Server: <base>/<owner>/<repo>/pull/<n>
  bitbucket: '/pull-requests/', // Cloud: <base>/<ws>/<repo>/...; Data Center: <base>/projects/<K>/repos/<r>/...
};

/**
 * The repository's web page, cut from the request's own web URL (LAUN-239): the same scheme and host as the
 * title link, the path up to the host's request tail. The path must end in the repository label shown
 * (`group/project`, or `projects/KEY/repos/slug` for a Bitbucket Data Center `KEY/slug`), so the link goes
 * where its text says. Null (the repository stays text) when the URL is not http(s), carries user info, or
 * does not match.
 */
export function repoWebUrl(
  provider: string,
  url: string | null | undefined,
  number: number | null,
  repoPath: string,
): string | null {
  const tail = REQUEST_TAIL[provider];
  if (!tail || !url || !repoPath || number === null || !Number.isSafeInteger(number)) return null;
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  if ((u.protocol !== 'https:' && u.protocol !== 'http:') || u.username || u.password || !u.hostname)
    return null;
  const path = u.pathname.replace(/\/+$/, '');
  const end = `${tail}${number}`;
  if (!path.endsWith(end)) return null;
  const repo = path.slice(0, -end.length);
  let shown: string;
  try {
    shown = decodeURIComponent(repo).toLowerCase();
  } catch {
    return null;
  }
  const label = repoPath.toLowerCase();
  const parts = label.split('/');
  const ok =
    shown.endsWith(`/${label}`) ||
    (provider === 'bitbucket' &&
      parts.length === 2 &&
      shown.endsWith(`/projects/${parts[0]}/repos/${parts[1]}`));
  return ok ? `${u.origin}${repo}` : null;
}

export const STATE_TEXT: Record<ChangeRequest['state'], string> = {
  draft: 'Draft',
  open: 'Open',
  approved: 'Approved',
  merged: 'Merged',
  closed: 'Closed',
};

export const CHECKS_TEXT: Record<NonNullable<ChangeRequest['checks']>, string> = {
  pending: 'Checks running',
  success: 'Checks passed',
  failure: 'Checks failed',
};
