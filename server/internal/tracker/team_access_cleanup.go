package tracker

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"devops/internal/dbq"
)

// LostTeamNotifyRows counts what RemoveLostTeamNotifyRows deleted.
type LostTeamNotifyRows struct {
	Subscriptions int64 // issue_subscriptions on issues of the lost teams
	Follows       int64 // team_follows of the lost teams
	Settings      int64 // per-team notification_settings of the lost teams
}

// RemoveLostTeamNotifyRows is the cleanup of decisions-T20 #50: for one member
// and the teams the member loses, it deletes the member's issue subscriptions
// on issues of those teams, the follows of those teams and the per-team
// notification settings of those teams. Hard deletes; the global settings,
// the rows of other teams and the inbox rows stay (the inbox hides a row of a
// hidden team at read time, #51). It emits no event (per-user private rows).
//
// The access setter (ticket 9, admin scope) calls it with q on its
// transaction. It lives in tracker, not in notify, so that write path does not
// import internal/notify (AGENTS.md).
func RemoveLostTeamNotifyRows(ctx context.Context, q *dbq.Queries, orgID, userID uuid.UUID, lost []uuid.UUID) (LostTeamNotifyRows, error) {
	var out LostTeamNotifyRows
	if len(lost) == 0 {
		return out, nil
	}
	var err error
	if out.Subscriptions, err = q.DeleteMemberSubscriptionsInTeams(ctx, dbq.DeleteMemberSubscriptionsInTeamsParams{
		OrgID: orgID, UserID: userID, LostTeamIds: lost}); err != nil {
		return out, fmt.Errorf("delete subscriptions of lost teams: %w", err)
	}
	if out.Follows, err = q.DeleteMemberTeamFollowsInTeams(ctx, dbq.DeleteMemberTeamFollowsInTeamsParams{
		OrgID: orgID, UserID: userID, LostTeamIds: lost}); err != nil {
		return out, fmt.Errorf("delete follows of lost teams: %w", err)
	}
	if out.Settings, err = q.DeleteMemberTeamSettingsInTeams(ctx, dbq.DeleteMemberTeamSettingsInTeamsParams{
		OrgID: orgID, UserID: userID, LostTeamIds: lost}); err != nil {
		return out, fmt.Errorf("delete team settings of lost teams: %w", err)
	}
	return out, nil
}
