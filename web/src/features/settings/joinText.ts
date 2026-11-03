import { ApiError } from '../../api/errors';
import { waitText } from './tokenText';

export { waitText };

// Fixed texts for the invite, sign-up and platform admin calls. Server text never reaches the
// screen. The server's 409 messages (decisions-T12) are matched here only to choose a fixed sentence.

/** `403 signup_unavailable`: the sign-up mode does not allow this call. */
export function isModeRefusal(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403 && err.code === 'signup_unavailable';
}

const GENERIC = 'Something went wrong. Try again.';
const OFFLINE = 'Could not reach the server. Check your connection and try again.';

function common(err: unknown): string | null {
  if (!(err instanceof ApiError)) return GENERIC;
  if (err.status === 0) return OFFLINE;
  if (err.status === 401) return 'You are signed out. Sign in again.';
  if (err.status === 429) return `Too many requests. Try again in ${waitText(err.retryAfter)}.`;
  return null;
}

export function inviteCreateText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  const e = err as ApiError;
  if (isModeRefusal(e)) return 'Invites are closed. A platform admin must change the sign-up mode first.';
  if (e.status === 403) return 'You need admin access to invite a person.';
  if (e.status === 409) {
    // The webhook guard (decisions-T20 #65) names repositories; this fixed text names none.
    if (e.message.includes('repositor'))
      return 'Selected teams are not possible now: every connected repository needs a team first. Give each repository a team in the git settings, or invite with All teams.';
    if (e.message.includes('cannot be invited')) return 'That email cannot be invited.';
    if (e.message.includes('open invite')) return 'This email has an open invite. Revoke it first.';
    if (e.message.includes('member of this org')) return 'This person is a member already.';
    return 'That email cannot be invited.';
  }
  if (e.status === 400) {
    if (e.message.includes('unknown team'))
      return 'A team you picked does not exist any more. Reload the page and pick the teams again.';
    if (/team/i.test(e.message)) return 'The team choice was not accepted. Check the teams and try again.';
    return 'Enter a valid email address.';
  }
  return 'Could not create the invite. Try again.';
}

export function inviteRevokeText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  const e = err as ApiError;
  if (e.status === 409) return 'This invite was accepted already. It cannot be revoked.';
  if (e.status === 404) return 'This invite is gone already. Reload the list.';
  return 'Could not revoke the invite. Try again.';
}

export function approveText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  const e = err as ApiError;
  if (isModeRefusal(e))
    return 'Approval is off. A platform admin must set the sign-up mode to Approval before you can approve.';
  if (e.status === 403) return 'You need admin access to approve a request.';
  if (e.status === 404) return 'This request is gone already. Reload the list.';
  if (e.status === 409) {
    if (e.message.includes('not confirmed'))
      return 'The person did not confirm the email address yet. Wait for the code, or reject the request.';
    if (e.message.includes('username'))
      return 'This username is taken now. Reject the request and ask the person to ask again with another username.';
    if (e.message.includes('with this email'))
      return 'An account with this email exists already. Reject the request.';
    return 'This request cannot be approved now. Reload the list.';
  }
  return 'Could not approve the request. Try again.';
}

export function rejectText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  const e = err as ApiError;
  if (e.status === 403) return 'You need admin access to reject a request.';
  if (e.status === 404) return 'This request is gone already. Reload the list.';
  if (e.status === 409) return 'This request cannot be rejected now. Reload the list.';
  if (e.status === 400) return 'The reason is too long. Use 1000 characters or fewer.';
  return 'Could not reject the request. Try again.';
}

export function purgeText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  if ((err as ApiError).status === 403) return 'You need admin access to remove requests.';
  return 'Could not remove the requests. Try again.';
}

export function adminAddText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  const e = err as ApiError;
  if (e.status === 404) return 'No user has this username or email.';
  if (e.status === 400) return 'This account cannot be a platform admin. Use an active person, not an agent.';
  if (e.status === 403) return 'You need platform admin access to do this. Sign out and in again.';
  return 'Could not add the platform admin. Try again.';
}

export function adminRemoveText(err: unknown): string {
  const c = common(err);
  if (c) return c;
  const e = err as ApiError;
  if (e.status === 409) return 'You cannot remove the last platform admin. Add another platform admin first.';
  if (e.status === 404) return 'This person is not a platform admin. Reload the list.';
  if (e.status === 403) return 'You need platform admin access to do this. Sign out and in again.';
  return 'Could not remove the platform admin. Try again.';
}

/** Age of a date as short text: "just now", "5 minutes ago", "3 hours ago", "2 days ago". */
export function age(iso: string, now: number = Date.now()): string {
  const s = Math.max(0, Math.floor((now - Date.parse(iso)) / 1000));
  if (!Number.isFinite(s)) return '';
  if (s < 60) return 'just now';
  const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'} ago`;
  if (s < 3600) return plural(Math.floor(s / 60), 'minute');
  if (s < 86400) return plural(Math.floor(s / 3600), 'hour');
  return plural(Math.floor(s / 86400), 'day');
}

/** The Teams cell of the invites table and the note (decisions-T20 #75). An admin invite is always all. */
export function inviteAccessLabel(i: {
  role: string;
  teamAccess: 'all' | 'selected';
  teams: string[];
}): string {
  if (i.role === 'admin' || i.teamAccess === 'all') return 'All teams';
  return i.teams.length === 0 ? 'No team (its teams were deleted)' : i.teams.join(', ');
}
