import { EmptyState } from '../../ui/States';

/**
 * decisions-T20 #73: a member who sees no team. The member cannot tell an empty org from no access, and that is
 * fine: one text for both. No "request access" control (#77): it would confirm that a hidden team exists.
 */
export const NO_TEAM_TITLE = 'You do not have access to any team yet';
export const NO_TEAM_TEXT = 'Ask an organisation admin.';

export function NoTeamAccess() {
  return (
    <EmptyState title={NO_TEAM_TITLE}>
      <p>{NO_TEAM_TEXT}</p>
    </EmptyState>
  );
}
