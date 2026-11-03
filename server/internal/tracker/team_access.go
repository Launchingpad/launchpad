package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// The team access setter (decisions-T20 #48 to #50, ticket 9):
// PUT /orgs/{org}/users/{user}/team-access, org admin scope. It is the first
// write that can make a member restricted (team_access 'selected').

// Team access values (org_members.team_access).
const (
	TeamAccessAll      = "all"
	TeamAccessSelected = "selected"
)

// Choices for the open issues assigned to a member in a team the member loses
// (#49, open point E).
const (
	AssignedUnassign = "unassign"
	AssignedKeep     = "keep"
)

// maxAccessTeams is the most teams one setter or agent body may name (#26).
const maxAccessTeams = 50

// TeamAccessInput is the PUT /users/{user}/team-access body. It replaces the
// whole set (#48).
type TeamAccessInput struct {
	// TeamAccess is "all" or "selected"; required.
	TeamAccess string `json:"teamAccess"`
	// Teams are keys or ids of teams of this org, for "selected" only. An
	// empty list is allowed (#5: an edit can take the last team away).
	Teams []string `json:"teams"`
	// AssignedIssues is "unassign" or "keep": what happens to the member's
	// open issues in the teams the member loses. Required when there are
	// such issues (#49), unless the call is a dry run.
	AssignedIssues string `json:"assignedIssues"`
}

// TeamIssueCount is a number of open issues in one team.
type TeamIssueCount struct {
	TeamID     uuid.UUID `json:"teamId"`
	OpenIssues int64     `json:"openIssues"`
}

// RemovedNotifyRows counts the notification rows the change deleted (#50).
type RemovedNotifyRows struct {
	Subscriptions int64 `json:"subscriptions"`
	Follows       int64 `json:"follows"`
	Settings      int64 `json:"settings"`
}

// TeamAccessResult is the setter's answer. It holds ids and counts only.
type TeamAccessResult struct {
	UserID     uuid.UUID `json:"userId"`
	TeamAccess string    `json:"teamAccess"`
	// TeamIDs is the member's set after the change (sorted); empty for "all".
	TeamIDs []uuid.UUID `json:"teamIds"`
	// DryRun: nothing was written.
	DryRun bool `json:"dryRun"`
	// Changed: the access, the team rows, an assignee or a notification row
	// changed (or would change, in a dry run).
	Changed      bool        `json:"changed"`
	AddedTeamIDs []uuid.UUID `json:"addedTeamIds"`
	LostTeamIDs  []uuid.UUID `json:"lostTeamIds"`
	// AssignedInLostTeams counts the member's open issues in each lost team,
	// before any unassign (#49).
	AssignedInLostTeams []TeamIssueCount `json:"assignedInLostTeams"`
	// Unassigned is how many open issues lost their assignee ("unassign").
	Unassigned int `json:"unassigned"`
	// HiddenAssigned counts the member's open issues, after the change, in
	// teams the member does not see (#46, #48): the target only.
	HiddenAssigned int64             `json:"hiddenAssigned"`
	Removed        RemovedNotifyRows `json:"removed"`
}

// errDryRun rolls back a dry run's transaction.
var errDryRun = errors.New("dry run")

func uuidCmp(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

// sortedIDs returns a sorted copy without duplicates.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, uuidCmp)
	return slices.Compact(out)
}

// minus returns the ids of a that are not in b (both sorted).
func minus(a, b []uuid.UUID) []uuid.UUID {
	out := []uuid.UUID{}
	for _, id := range a {
		if _, found := slices.BinarySearchFunc(b, id, uuidCmp); !found {
			out = append(out, id)
		}
	}
	return out
}

// validateAccessKind checks a teamAccess value and its team list. empty is
// what an absent value means ("" for the setter, "all" for agents).
func validateAccessKind(kind string, teams []string) error {
	switch kind {
	case TeamAccessAll:
		if len(teams) > 0 {
			return httpx.Invalid(`teams may be set only with teamAccess "selected"`)
		}
	case TeamAccessSelected:
		if len(teams) > maxAccessTeams {
			return httpx.Invalid("at most %d teams", maxAccessTeams)
		}
	default:
		return httpx.Invalid(`teamAccess must be "all" or "selected"`)
	}
	return nil
}

// NewMemberAccess checks the team access of a new membership: an agent
// (#31), an invite (#26) or a sign-up approval (#29). An absent value is
// "all" (open point A); "selected" needs one to 50 teams (#5: inviting a
// person to nothing is a mistake); "all" takes no teams.
func NewMemberAccess(kind string, teams []string) (string, error) {
	if kind == "" {
		kind = TeamAccessAll
	}
	if err := validateAccessKind(kind, teams); err != nil {
		return "", err
	}
	if kind == TeamAccessSelected && len(teams) == 0 {
		return "", httpx.Invalid(`teamAccess "selected" needs at least one team`)
	}
	return kind, nil
}

// ResolveAccessTeams resolves keys or ids of teams of org for an org admin
// route (an invite, a sign-up approval). An unknown team or another org's is
// 400 "unknown team". The ids come back sorted, without duplicates.
func ResolveAccessTeams(ctx context.Context, q *dbq.Queries, org *auth.Org, refs []string) ([]uuid.UUID, error) {
	return resolveAccessTeams(ctx, q, org, refs)
}

// GrantNewMemberTeams makes the membership userID of org restricted to
// teams, in the transaction q runs in, right after the membership insert.
// The caller holds the webhook guard's lock and runs the guard after it
// (#65).
func GrantNewMemberTeams(ctx context.Context, q *dbq.Queries, orgID, userID uuid.UUID, teams []uuid.UUID) error {
	if err := q.SetMemberTeamAccess(ctx, dbq.SetMemberTeamAccessParams{OrgID: orgID, UserID: userID, TeamAccess: TeamAccessSelected}); err != nil {
		return fmt.Errorf("set team access: %w", err)
	}
	if _, err := q.InsertMemberTeams(ctx, dbq.InsertMemberTeamsParams{OrgID: orgID, UserID: userID, NewTeamIds: teams}); err != nil {
		return fmt.Errorf("insert member teams: %w", err)
	}
	return nil
}

// resolveAccessTeams resolves keys or ids of teams of the org for an org
// admin (who sees every team, #2). Archived teams count (#6).
func resolveAccessTeams(ctx context.Context, q *dbq.Queries, org *auth.Org, refs []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(refs))
	for _, r := range refs {
		t, err := resolveTeam(ctx, q, org, strings.TrimSpace(r))
		if err != nil {
			return nil, invalidRef(err, "unknown team %q", r)
		}
		out = append(out, t.ID)
	}
	return sortedIDs(out), nil
}

// memberRef reads the {user} path value: a user id or a username.
func memberRef(ref string) dbq.GetMemberForAccessParams {
	if id, err := uuid.Parse(ref); err == nil {
		return dbq.GetMemberForAccessParams{UserID: &id}
	}
	name := strings.ToLower(strings.TrimSpace(ref))
	return dbq.GetMemberForAccessParams{Username: &name}
}

// SetTeamAccess replaces the team access of the member userRef (an id or a
// username) of org (#48). Only an org admin calls it (the route's admin
// scope). In one transaction, under the team access lock of the webhook
// guard (#65, entry 1): it reads and locks the member, refuses an admin
// target (400, #2), counts the member's open issues in the teams the member
// loses and refuses with 409 when there are some and no choice was made
// (#49), writes the flag and the team rows, unassigns when asked, deletes the
// member's subscriptions, follows and team settings of the lost teams (#50),
// runs the webhook guard when the member ends restricted, and emits an
// issue.updated per unassigned issue and member.access_changed (admin scope)
// last. A dry run does the same and rolls back: nothing is written, and the
// missing choice is not refused (the counts are the answer). A call that
// changes nothing writes nothing and emits nothing.
func (s *Service) SetTeamAccess(ctx context.Context, org *auth.Org, p *auth.Principal, userRef string, in TeamAccessInput, dryRun bool) (TeamAccessResult, error) {
	if err := validateAccessKind(in.TeamAccess, in.Teams); err != nil {
		return TeamAccessResult{}, err
	}
	switch in.AssignedIssues {
	case "", AssignedUnassign, AssignedKeep:
	default:
		return TeamAccessResult{}, httpx.Invalid(`assignedIssues must be "unassign" or "keep"`)
	}
	var out TeamAccessResult
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		// The guard's lock first (#65): a concurrent setter, invite, agent or
		// repository map of this org waits until this one commits.
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		ref := memberRef(userRef)
		ref.OrgID = org.ID
		m, err := q.GetMemberForAccess(ctx, ref)
		if err != nil {
			return notFound(err, "user")
		}
		if m.Role == "admin" {
			return httpx.Invalid("an org admin always sees every team; team access applies to members only")
		}
		after, err := resolveAccessTeams(ctx, q, org, in.Teams)
		if err != nil {
			return err
		}
		refs, err := q.ListOrgTeamRefs(ctx, org.ID)
		if err != nil {
			return fmt.Errorf("teams: %w", err)
		}
		keys := make(map[uuid.UUID]string, len(refs))
		all := make([]uuid.UUID, 0, len(refs))
		for _, t := range refs {
			keys[t.ID] = t.Key
			all = append(all, t.ID)
		}
		all = sortedIDs(all)
		before := all
		if m.TeamAccess != TeamAccessAll {
			if before, err = q.ListMemberTeamIDs(ctx, dbq.ListMemberTeamIDsParams{OrgID: org.ID, UserID: m.UserID}); err != nil {
				return fmt.Errorf("member teams: %w", err)
			}
			before = sortedIDs(before)
		}
		visibleAfter := all
		if in.TeamAccess == TeamAccessSelected {
			visibleAfter = after
		} else {
			after = []uuid.UUID{}
		}
		lost := minus(before, visibleAfter)
		added := minus(visibleAfter, before)
		out = TeamAccessResult{UserID: m.UserID, TeamAccess: in.TeamAccess, TeamIDs: after, DryRun: dryRun,
			AddedTeamIDs: added, LostTeamIDs: lost, AssignedInLostTeams: []TeamIssueCount{}}

		// The member's open issues per team (#46, #49).
		counts, err := q.CountOpenAssignedIssuesByTeam(ctx, dbq.CountOpenAssignedIssuesByTeamParams{OrgID: org.ID, UserID: m.UserID})
		if err != nil {
			return fmt.Errorf("count assigned issues: %w", err)
		}
		var inLost int64
		for _, c := range counts {
			if slices.Contains(lost, c.TeamID) {
				out.AssignedInLostTeams = append(out.AssignedInLostTeams, TeamIssueCount{TeamID: c.TeamID, OpenIssues: c.OpenIssues})
				inLost += c.OpenIssues
			}
		}
		if inLost > 0 && in.AssignedIssues == "" && !dryRun {
			parts := make([]string, 0, len(out.AssignedInLostTeams))
			for _, c := range out.AssignedInLostTeams {
				parts = append(parts, fmt.Sprintf("%s: %d", keys[c.TeamID], c.OpenIssues))
			}
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
				`%d open issues assigned to this member are in teams the member would lose (%s); send assignedIssues "unassign" or "keep"`,
				inLost, strings.Join(parts, ", "))
		}

		// The flag and the team rows.
		changed := m.TeamAccess != in.TeamAccess
		if changed {
			if err := q.SetMemberTeamAccess(ctx, dbq.SetMemberTeamAccessParams{OrgID: org.ID, UserID: m.UserID, TeamAccess: in.TeamAccess}); err != nil {
				return fmt.Errorf("set team access: %w", err)
			}
		}
		gone, err := q.DeleteMemberTeamsExcept(ctx, dbq.DeleteMemberTeamsExceptParams{OrgID: org.ID, UserID: m.UserID, KeepTeamIds: after})
		if err != nil {
			return fmt.Errorf("delete team rows: %w", err)
		}
		made, err := q.InsertMemberTeams(ctx, dbq.InsertMemberTeamsParams{OrgID: org.ID, UserID: m.UserID, NewTeamIds: after})
		if err != nil {
			return fmt.Errorf("insert team rows: %w", err)
		}
		changed = changed || gone > 0 || made > 0

		// Unassign (#49): open issues only, in the lost teams only.
		var evs []events.Scoped
		if inLost > 0 && in.AssignedIssues == AssignedUnassign {
			rows, err := q.UnassignOpenIssuesInTeams(ctx, dbq.UnassignOpenIssuesInTeamsParams{OrgID: org.ID, UserID: m.UserID, LostTeamIds: lost})
			if err != nil {
				return fmt.Errorf("unassign: %w", err)
			}
			from, err := snapUser(ctx, q, &m.UserID)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if err := insertActivity(ctx, q, org.ID, r.ID, &p.UserID, "system", activityRow{ActAssignee, from, nil}); err != nil {
					return err
				}
				evs = append(evs, events.Event{Type: events.IssueUpdated, EntityType: "issue", EntityID: r.ID, ActorID: &p.UserID,
					Payload: map[string]any{"id": r.ID, "identifier": Identifier(keys[r.TeamID], r.Number),
						"changes": []string{"assignee"}, "version": r.Version, "teamId": r.TeamID,
						"assigneeId": nil, "fromAssigneeId": m.UserID, "reason": "access_removed"}}.In(events.ScopeTeams(r.TeamID)))
			}
			out.Unassigned = len(rows)
			changed = changed || len(rows) > 0
		}

		// Subscriptions, follows and team settings of the lost teams (#50).
		rm, err := RemoveLostTeamNotifyRows(ctx, q, org.ID, m.UserID, lost)
		if err != nil {
			return err
		}
		out.Removed = RemovedNotifyRows{Subscriptions: rm.Subscriptions, Follows: rm.Follows, Settings: rm.Settings}
		changed = changed || rm != (LostTeamNotifyRows{})
		out.Changed = changed

		// What stays assigned and hidden from the member, after the change.
		left, err := q.CountOpenAssignedIssuesByTeam(ctx, dbq.CountOpenAssignedIssuesByTeamParams{OrgID: org.ID, UserID: m.UserID})
		if err != nil {
			return fmt.Errorf("count assigned issues: %w", err)
		}
		for _, c := range left {
			if _, found := slices.BinarySearchFunc(visibleAfter, c.TeamID, uuidCmp); !found {
				out.HiddenAssigned += c.OpenIssues
			}
		}

		// The webhook guard (#65, entry 1): the member is restricted after
		// this write, so no enabled repository may lack a team.
		if in.TeamAccess == TeamAccessSelected {
			if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
				return err
			}
		}
		if dryRun {
			return errDryRun
		}
		if !changed {
			return nil
		}
		evs = append(evs, events.Event{Type: events.MemberAccessChanged, EntityType: "member", EntityID: m.UserID, ActorID: &p.UserID,
			Payload: map[string]any{"userId": m.UserID, "teamAccess": in.TeamAccess, "teamIds": after,
				"addedTeamIds": added, "lostTeamIds": lost, "unassigned": out.Unassigned}}.In(events.ScopeAdmin()))
		return events.EmitAll(ctx, q, org.ID, evs)
	})
	if errors.Is(err, errDryRun) {
		return out, nil
	}
	if err != nil {
		return TeamAccessResult{}, err
	}
	return out, nil
}
