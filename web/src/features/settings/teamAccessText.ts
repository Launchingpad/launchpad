import { ApiError, errorMessage } from '../../api/errors';
import type { Member, TeamAccessResult } from '../../api/types';

/** The Teams cell of the members table (decisions-T20 #74). An org admin always sees every team (#2). */
export function accessLabel(m: Member): string {
  if (m.role === 'admin' || m.teamAccess === 'all') return 'All teams';
  const keys = m.teams ?? [];
  return keys.length === 0 ? 'No team' : keys.join(', ');
}

/**
 * The text for a refused setter call. The 400 and 409 texts come from an admin-only route and hold team keys,
 * counts and repository names, never another person's data, so an admin may read them (cleaned by `toApiError`).
 */
export function accessErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return errorMessage(err);
  switch (err.status) {
    case 400:
      return `The server did not accept this access: ${sentence(err.message)}`;
    case 403:
      return 'Only an organisation admin can change team access.';
    case 404:
      return 'This person is no longer a member of this organisation. The member list is refreshed.';
    case 409:
      return `The access was not changed: ${sentence(err.message)} Check the changes again.`;
    default:
      return errorMessage(err);
  }
}

const sentence = (t: string) => (/[.!?…]$/.test(t) ? t : `${t}.`);

/** "1 open issue" or "3 open issues". */
export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

const assigned = (r: TeamAccessResult) => r.assignedInLostTeams.reduce((n, t) => n + t.openIssues, 0);

/** The short text a screen reader hears when the dry run answers. */
export function previewSummary(r: TeamAccessResult, noTeam: boolean): string {
  if (!r.changed) return 'Checked. Nothing changes.';
  const parts = [
    `${plural(r.addedTeamIds.length, 'team')} added`,
    `${plural(r.lostTeamIds.length, 'team')} removed`,
    `${plural(assigned(r), 'open issue')} assigned in the teams removed`,
  ];
  return `Checked: ${parts.join(', ')}.${noTeam ? ' This person will see no team.' : ''} Review, then save.`;
}

const sameSet = (a: string[], b: string[]) => a.length === b.length && a.every((x) => b.includes(x));

/** The page note after a save: what the server did, and whether it differs from the check the admin read. */
export function savedNote(
  name: string,
  checked: TeamAccessResult,
  done: TeamAccessResult,
  asked: 'unassign' | 'keep' | undefined,
): string {
  let text = `The team access of ${name} is saved.`;
  if (done.unassigned > 0) text += ` ${plural(done.unassigned, 'open issue')} unassigned.`;
  const { subscriptions, follows, settings } = done.removed;
  if (subscriptions + follows + settings > 0) {
    text += ` Removed: ${plural(subscriptions, 'issue subscription')}, ${plural(follows, 'team follow')}, ${plural(settings, 'team notification setting')}.`;
  }
  const differs =
    !sameSet(checked.addedTeamIds, done.addedTeamIds) ||
    !sameSet(checked.lostTeamIds, done.lostTeamIds) ||
    checked.removed.subscriptions !== subscriptions ||
    checked.removed.follows !== follows ||
    checked.removed.settings !== settings ||
    (asked === 'unassign' && done.unassigned !== assigned(checked));
  if (differs) text += ' The result differs from your check, because something changed in between.';
  return text;
}
