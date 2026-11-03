import { ApiError } from '../../api/errors';

// Fixed sentences for the project dialog (LAUN-166). No server text reaches the screen.
export const NAME_RULE = 'Enter 1 to 255 characters.';
export const TEXT_RULE = 'Enter at most 200000 characters.';
export const TEAMS_RULE = 'Choose at least one team.';
export const DATE_RULE = 'Use a date like 2026-11-30.';
export const ORDER_RULE = 'The target date cannot be before the start date.';
export const LEAD_RULE = 'This person is not a member of the org. Choose another lead.';
export const TEAM_GONE = 'A team in the list does not exist any more. Close this window and open it again.';
export const TEAMS_MAX = 'A project can have at most 50 teams.';
export const MAYBE_CREATED =
  'The project may have been created. Check the project list before you try again.';
export const STILL_RUNNING = 'The first try is still running. Check the project list before you try again.';
export const STALE = 'Someone else changed this project. Close this window and open it again.';

export type ProjectField = 'name' | 'description' | 'lead' | 'startDate' | 'targetDate' | 'teams';

/** What a failed save means. `field` puts the sentence under that field; no field means a banner. */
export function saveProjectError(
  err: unknown,
  verb: 'create' | 'save',
): { field?: ProjectField; text: string } {
  if (!(err instanceof ApiError)) return { text: `Could not ${verb} the project. Try again.` };
  if (err.code === 'idempotency_outcome_unknown' || err.code === 'idempotency_key_reused')
    return { text: MAYBE_CREATED };
  if (err.code === 'idempotency_in_progress') return { text: STILL_RUNNING };
  if (err.status === 409 && err.code === 'version_conflict') return { text: STALE };
  if (err.status === 400) {
    const m = err.message.toLowerCase();
    if (m.startsWith('name')) return { field: 'name', text: NAME_RULE };
    if (m.startsWith('description')) return { field: 'description', text: TEXT_RULE };
    if (m.startsWith('targetdate is before')) return { field: 'targetDate', text: ORDER_RULE };
    if (m.startsWith('startdate')) return { field: 'startDate', text: DATE_RULE };
    if (m.startsWith('targetdate')) return { field: 'targetDate', text: DATE_RULE };
    if (m.startsWith('at most 50 teams')) return { field: 'teams', text: TEAMS_MAX };
    if (m.startsWith('unknown team')) return { field: 'teams', text: TEAM_GONE };
    // Label problems (LAUN-194): the server says which; show its sentence in the banner.
    if (m.includes('label')) return { text: err.message };
    if (m.startsWith('lead') || m.startsWith('unknown lead')) return { field: 'lead', text: LEAD_RULE };
  }
  if (err.status === 403) return { text: `You need write access to ${verb} a project.` };
  if (err.status === 404) return { text: 'This project is gone. Close this window and reload the page.' };
  if (err.status === 0) return { text: 'Could not reach the server. Check your connection and try again.' };
  if (err.status === 401) return { text: 'You are signed out. Sign in again.' };
  if (err.status === 429) return { text: 'Too many requests. Wait a moment and try again.' };
  return { text: `Could not ${verb} the project. Try again.` };
}
