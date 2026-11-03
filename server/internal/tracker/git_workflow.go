package tracker

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/scm"
)

// Git workflow rules (LAUN-244, docs/internal/decisions-T44-git-workflow.md).
// A team may replace the built-in move of a git event with its own: a target
// state of the team or "no move", and an optional set of states the issue must
// be in. A team with no rule for an event keeps the built-in move (decisions-T5
// #16, #17; decisions-T17 #16): opened, ready and reopened go to the team's
// review state, merged to its testing state, closed moves nothing, and every
// built-in move is forward only.

// The events a rule names. Each is a neutral scm action that the parsers of
// all three hosts know (Bitbucket sends no reopened and no ready).
const (
	GitEventOpened   = "opened"
	GitEventReady    = "ready"
	GitEventReopened = "reopened"
	GitEventMerged   = "merged"
	GitEventClosed   = "closed"
)

// GitWorkflowEvents is the fixed order of the rules in an answer.
var GitWorkflowEvents = []string{GitEventOpened, GitEventReady, GitEventReopened, GitEventMerged, GitEventClosed}

// GitRuleStateArchived flags a rule whose target state is archived: the rule
// does nothing until an admin picks another state.
const GitRuleStateArchived = "state_archived"

// GitRuleFromStateArchived flags a rule one of whose "only from" states is
// archived. The rule still works for its other states; a PUT must leave the
// archived one out.
const GitRuleFromStateArchived = "from_state_archived"

// maxGitRuleFromStates caps the "only from" set of one rule.
const maxGitRuleFromStates = 50

// GitWorkflowRule is one event of a team's git workflow. Custom is false for
// the built-in move; then StateID is the team's review or testing state (or
// null) and FromStateIDs is empty.
type GitWorkflowRule struct {
	Event  string `json:"event"`
	Custom bool   `json:"custom"`
	// StateID is the target state; null means no move.
	StateID *uuid.UUID `json:"stateId"`
	// FromStateIDs: empty means forward only (never out of a completed or
	// canceled state); else the issue moves only from one of these states.
	FromStateIDs []uuid.UUID `json:"fromStateIds"`
	// Problem is null; state_archived: the target is archived and the rule
	// does nothing; from_state_archived: an "only from" state is archived.
	Problem *string `json:"problem"`
}

// GitWorkflow is the GET and PUT /teams/{team}/git-workflow answer: one rule
// per event, in GitWorkflowEvents order.
type GitWorkflow struct {
	TeamID uuid.UUID         `json:"teamId"`
	Rules  []GitWorkflowRule `json:"rules"`
}

// GitWorkflowRuleInput is one custom rule in a PUT.
type GitWorkflowRuleInput struct {
	Event string `json:"event"`
	// StateID is required: a state id, or null for "no move".
	StateID      httpx.Opt[uuid.UUID] `json:"stateId"`
	FromStateIDs []uuid.UUID          `json:"fromStateIds"`
}

// GitWorkflowInput is the PUT body: the whole set of custom rules. An event
// not listed keeps its built-in move; an empty list resets every event.
type GitWorkflowInput struct {
	Rules *[]GitWorkflowRuleInput `json:"rules"`
}

// gitRule is a stored custom rule.
type gitRule struct {
	target *uuid.UUID
	from   []uuid.UUID
}

// gitRuleEvent is the rule event of a parsed event, or "" for an event no
// rule can move on. A draft that opens or reopens never moves (decisions-T5
// #16): it is not ready for review, under any rule.
func gitRuleEvent(ev scm.Event) string {
	switch ev.Action {
	case scm.ActionOpened:
		if !ev.Draft {
			return GitEventOpened
		}
	case scm.ActionReopened:
		if !ev.Draft {
			return GitEventReopened
		}
	case scm.ActionReady:
		return GitEventReady
	case scm.ActionMerged:
		return GitEventMerged
	case scm.ActionClosed:
		return GitEventClosed
	}
	return ""
}

// builtinTarget is the built-in move of a rule event: the team's review or
// testing anchor, or nil (closed, or a team with no anchor). It matches
// scm.Event.Plan's Move for every event gitRuleEvent names.
func builtinTarget(team dbq.Team, event string) *uuid.UUID {
	switch event {
	case GitEventOpened, GitEventReady, GitEventReopened:
		return team.ReviewStateID
	case GitEventMerged:
		return team.TestingStateID
	}
	return nil
}

// gitMove is what one event asks of one team's issues.
type gitMove struct {
	target *uuid.UUID  // nil: no move
	from   []uuid.UUID // custom only; empty: forward only
	custom bool        // a team rule: it moves only when the event changed the link
}

// resolveGitMove picks the team's rule for the event, else the built-in move.
func resolveGitMove(team dbq.Team, ev scm.Event, rules map[string]gitRule) gitMove {
	event := gitRuleEvent(ev)
	if event == "" {
		return gitMove{}
	}
	if r, ok := rules[event]; ok {
		return gitMove{target: r.target, from: r.from, custom: true}
	}
	return gitMove{target: builtinTarget(team, event)}
}

// gitRuleCache loads each team's rules once per event.
type gitRuleCache map[uuid.UUID]map[string]gitRule

func (c gitRuleCache) get(ctx context.Context, q *dbq.Queries, orgID, teamID uuid.UUID) (map[string]gitRule, error) {
	if r, ok := c[teamID]; ok {
		return r, nil
	}
	r, err := loadGitRules(ctx, q, orgID, teamID)
	if err != nil {
		return nil, err
	}
	c[teamID] = r
	return r, nil
}

func loadGitRules(ctx context.Context, q *dbq.Queries, orgID, teamID uuid.UUID) (map[string]gitRule, error) {
	rows, err := q.ListTeamGitRules(ctx, dbq.ListTeamGitRulesParams{OrgID: orgID, TeamID: teamID})
	if err != nil {
		return nil, fmt.Errorf("list git rules: %w", err)
	}
	out := make(map[string]gitRule, len(rows))
	for _, r := range rows {
		var from []uuid.UUID
		if len(r.FromStateIds) > 0 {
			from = r.FromStateIds
		}
		out[r.Event] = gitRule{target: r.TargetStateID, from: from}
	}
	return out, nil
}

// mayMoveTo reports whether an issue in state from may move to state to under
// mv. The built-in rule and a custom rule with no from set move forward only
// (isForwardMove: never out of a completed or canceled state). A custom rule
// with a from set moves only from one of its states, in any direction. Nothing
// moves into an archived state or to where it is.
func mayMoveTo(mv gitMove, from, to dbq.WorkflowState) bool {
	if to.ArchivedAt != nil || from.ID == to.ID {
		return false
	}
	if len(mv.from) == 0 {
		return isForwardMove(from, to)
	}
	return slices.Contains(mv.from, from.ID)
}

// --- the admin routes ------------------------------------------------------

// GetGitWorkflow answers a team's rules (any member who sees the team; the
// route refuses agents). A hidden team is 404 (T20 #14).
func (s *Service) GetGitWorkflow(ctx context.Context, org *auth.Org, teamRef string) (GitWorkflow, error) {
	t, err := resolveTeam(ctx, s.q, org, teamRef)
	if err != nil {
		return GitWorkflow{}, notFound(err, "team")
	}
	return gitWorkflowOf(ctx, s.q, org.ID, t)
}

func gitWorkflowOf(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, t dbq.Team) (GitWorkflow, error) {
	rules, err := loadGitRules(ctx, q, orgID, t.ID)
	if err != nil {
		return GitWorkflow{}, err
	}
	states, err := q.ListTeamStatesWithArchived(ctx, dbq.ListTeamStatesWithArchivedParams{OrgID: orgID, TeamID: t.ID})
	if err != nil {
		return GitWorkflow{}, fmt.Errorf("list states: %w", err)
	}
	archived := map[uuid.UUID]bool{}
	for _, st := range states {
		if st.ArchivedAt != nil {
			archived[st.ID] = true
		}
	}
	out := GitWorkflow{TeamID: t.ID, Rules: make([]GitWorkflowRule, 0, len(GitWorkflowEvents))}
	for _, e := range GitWorkflowEvents {
		rule := GitWorkflowRule{Event: e, FromStateIDs: []uuid.UUID{}}
		if r, ok := rules[e]; ok {
			rule.Custom, rule.StateID = true, r.target
			if r.from != nil {
				rule.FromStateIDs = r.from
			}
		} else {
			rule.StateID = builtinTarget(t, e)
		}
		switch {
		case rule.StateID != nil && archived[*rule.StateID]:
			p := GitRuleStateArchived
			rule.Problem = &p
		case slices.ContainsFunc(rule.FromStateIDs, func(id uuid.UUID) bool { return archived[id] }):
			p := GitRuleFromStateArchived
			rule.Problem = &p
		}
		out.Rules = append(out.Rules, rule)
	}
	return out, nil
}

// SetGitWorkflow replaces a team's custom rules (org admin; the route refuses
// agents). Every state must be a non-archived state of the team (400
// validation_failed otherwise, also for a state of another team or org). A
// change emits team.updated with changes ["gitWorkflow"] as the last write; a
// PUT of the same rules changes nothing and emits nothing.
func (s *Service) SetGitWorkflow(ctx context.Context, org *auth.Org, p *auth.Principal, teamRef string, in GitWorkflowInput) (GitWorkflow, error) {
	if in.Rules == nil {
		return GitWorkflow{}, httpx.Invalid("rules is required; send an empty list to use the built-in moves")
	}
	if len(*in.Rules) > len(GitWorkflowEvents) {
		return GitWorkflow{}, httpx.Invalid("rules has at most %d entries, one per event", len(GitWorkflowEvents))
	}
	var out GitWorkflow
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		t, err := resolveTeam(ctx, q, org, teamRef)
		if err != nil {
			return notFound(err, "team")
		}
		// The team row lock serialises two PUTs of one team.
		if _, err := q.LockTeam(ctx, dbq.LockTeamParams{OrgID: org.ID, ID: t.ID}); err != nil {
			return notFound(err, "team")
		}
		states, err := q.ListTeamStatesWithArchived(ctx, dbq.ListTeamStatesWithArchivedParams{OrgID: org.ID, TeamID: t.ID})
		if err != nil {
			return fmt.Errorf("list states: %w", err)
		}
		live := map[uuid.UUID]bool{}
		for _, st := range states {
			if st.ArchivedAt == nil {
				live[st.ID] = true
			}
		}
		next, err := validateGitRules(*in.Rules, live)
		if err != nil {
			return err
		}
		cur, err := loadGitRules(ctx, q, org.ID, t.ID)
		if err != nil {
			return err
		}
		if sameGitRules(cur, next) {
			out, err = gitWorkflowOf(ctx, q, org.ID, t)
			return err
		}
		if err := q.DeleteTeamGitRules(ctx, dbq.DeleteTeamGitRulesParams{OrgID: org.ID, TeamID: t.ID}); err != nil {
			return fmt.Errorf("delete git rules: %w", err)
		}
		for _, e := range GitWorkflowEvents {
			r, ok := next[e]
			if !ok {
				continue
			}
			if err := q.InsertTeamGitRule(ctx, dbq.InsertTeamGitRuleParams{OrgID: org.ID, TeamID: t.ID, Event: e, TargetStateID: r.target}); err != nil {
				return fmt.Errorf("insert git rule: %w", err)
			}
			for _, f := range r.from {
				if err := q.InsertTeamGitRuleFromState(ctx, dbq.InsertTeamGitRuleFromStateParams{OrgID: org.ID, TeamID: t.ID, Event: e, StateID: f}); err != nil {
					return fmt.Errorf("insert git rule from state: %w", err)
				}
			}
		}
		if out, err = gitWorkflowOf(ctx, q, org.ID, t); err != nil {
			return err
		}
		// Ids and a fixed word only; the event is scoped to the team (T20 #54).
		return emitAll(ctx, q, org.ID, events.ScopeTeams(t.ID), []events.Event{{
			Type: events.TeamUpdated, EntityType: "team", EntityID: t.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": t.ID, "key": t.Key, "changes": []string{"gitWorkflow"}},
		}})
	})
	if err != nil {
		return GitWorkflow{}, err
	}
	return out, nil
}

// validateGitRules checks a PUT's rules against the team's live states and
// returns them as stored rules (from sets sorted, duplicates refused).
func validateGitRules(in []GitWorkflowRuleInput, live map[uuid.UUID]bool) (map[string]gitRule, error) {
	out := make(map[string]gitRule, len(in))
	for _, r := range in {
		if !slices.Contains(GitWorkflowEvents, r.Event) {
			return nil, httpx.Invalid("event must be one of opened, ready, reopened, merged, closed")
		}
		if _, dup := out[r.Event]; dup {
			return nil, httpx.Invalid("each event may appear once")
		}
		if !r.StateID.Set {
			return nil, httpx.Invalid("stateId is required in each rule; null means no move")
		}
		var target *uuid.UUID
		if !r.StateID.Null {
			id := r.StateID.V
			target = &id
		}
		if target != nil && !live[*target] {
			return nil, httpx.Invalid("stateId must be a state of this team that is not archived")
		}
		if target == nil && len(r.FromStateIDs) > 0 {
			return nil, httpx.Invalid("a rule with no move takes no fromStateIds")
		}
		if len(r.FromStateIDs) > maxGitRuleFromStates {
			return nil, httpx.Invalid("fromStateIds has at most %d states", maxGitRuleFromStates)
		}
		var from []uuid.UUID
		for _, f := range r.FromStateIDs {
			if !live[f] {
				return nil, httpx.Invalid("fromStateIds must be states of this team that are not archived")
			}
			if slices.Contains(from, f) {
				return nil, httpx.Invalid("fromStateIds must not repeat a state")
			}
			from = append(from, f)
		}
		slices.SortFunc(from, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		out[r.Event] = gitRule{target: target, from: from}
	}
	return out, nil
}

// sameGitRules compares two rule sets (from sets in the same sorted order:
// the query sorts by state id, validateGitRules sorts too).
func sameGitRules(a, b map[string]gitRule) bool {
	if len(a) != len(b) {
		return false
	}
	for e, ra := range a {
		rb, ok := b[e]
		if !ok {
			return false
		}
		if (ra.target == nil) != (rb.target == nil) || (ra.target != nil && *ra.target != *rb.target) {
			return false
		}
		if !slices.Equal(ra.from, rb.from) {
			return false
		}
	}
	return true
}
