package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

const maxDescriptionRunes = 200_000

// CreateIssueInput is the POST /issues body.
type CreateIssueInput struct {
	Team        string     `json:"team"` // key or id
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"` // name or id; default: the team's default state
	Priority    *int       `json:"priority"`
	Estimate    *int32     `json:"estimate"`
	Assignee    string     `json:"assignee"` // username or "me"
	Labels      []string   `json:"labels"`   // names or ids
	Parent      string     `json:"parent"`   // identifier or id
	ProjectID   *uuid.UUID `json:"projectId"`
	MilestoneID *uuid.UUID `json:"milestoneId"`
	CycleID     *uuid.UUID `json:"cycleId"`
	DueDate     string     `json:"dueDate"` // YYYY-MM-DD
}

func validateTitle(t string) (string, error) {
	t = strings.TrimSpace(t)
	if n := utf8.RuneCountInString(t); n < 1 || n > 500 {
		return "", httpx.Invalid("title must be 1-500 characters")
	}
	return t, nil
}

func validateDescription(d string) error {
	if utf8.RuneCountInString(d) > maxDescriptionRunes {
		return httpx.Invalid("description must be at most %d characters", maxDescriptionRunes)
	}
	return nil
}

func validatePriority(p int) (int16, error) {
	if p < 0 || p > 4 {
		return 0, httpx.Invalid("priority must be 0 (none), 1 (urgent), 2 (high), 3 (medium) or 4 (low)")
	}
	return int16(p), nil
}

// maxEstimate bounds an issue estimate. Linear's largest scale
// (exponential) tops out at 64; 1000 leaves room for any points scheme
// while keeping every per-cycle sum far from overflow (docs/internal/decisions-T3.md).
const maxEstimate = 1000

func validateEstimate(e *int32) error {
	if e != nil && (*e < 0 || *e > maxEstimate) {
		return httpx.Invalid("estimate must be 0-%d", maxEstimate)
	}
	return nil
}

// CreateIssue creates an issue. The number comes from issue_counters inside
// the same transaction, the initial state is written to the history, and an
// issue.created event is appended — all or nothing.
func (s *Service) CreateIssue(ctx context.Context, org *auth.Org, p *auth.Principal, in CreateIssueInput) (Issue, error) {
	scopeAll, scopeIDs := org.Access.Args()
	title, err := validateTitle(in.Title)
	if err != nil {
		return Issue{}, err
	}
	if err := validateDescription(in.Description); err != nil {
		return Issue{}, err
	}
	if strings.TrimSpace(in.Team) == "" {
		return Issue{}, httpx.Invalid("team is required")
	}
	priority := int16(0)
	if in.Priority != nil {
		if priority, err = validatePriority(*in.Priority); err != nil {
			return Issue{}, err
		}
	}
	if err := validateEstimate(in.Estimate); err != nil {
		return Issue{}, err
	}
	var due *time.Time
	if in.DueDate != "" {
		if due, err = parseDueDate(in.DueDate); err != nil {
			return Issue{}, err
		}
	}

	var id uuid.UUID
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		team, err := resolveTeam(ctx, q, org, in.Team)
		if err != nil {
			return invalidRef(err, "unknown team %q", in.Team)
		}
		var state dbq.WorkflowState
		switch {
		case in.State != "":
			state, err = resolveState(ctx, q, org, team.ID, in.State)
		case team.DefaultStateID != nil:
			state, err = q.GetStateByID(ctx, dbq.GetStateByIDParams{OrgID: org.ID, ID: *team.DefaultStateID, AllTeams: scopeAll, TeamIds: scopeIDs})
		default:
			state, err = q.FirstStateOfType(ctx, dbq.FirstStateOfTypeParams{OrgID: org.ID, TeamID: team.ID, Type: "backlog"})
		}
		if err != nil {
			return invalidRef(err, "team %s has no default state", team.Key)
		}
		// The (team_id, state_id) foreign keys already refuse a state of
		// another team; this guard gives a clear 400 instead of a 500.
		if state.TeamID != team.ID {
			return httpx.Invalid("state %q does not belong to team %s", state.Name, team.Key)
		}
		var assignee *uuid.UUID
		if in.Assignee != "" {
			a, err := resolveAssignee(ctx, q, org.ID, p, in.Assignee)
			if err != nil {
				return err
			}
			assignee = &a
		}
		var parent *uuid.UUID
		if in.Parent != "" {
			pid, err := resolveIssueID(ctx, q, org, in.Parent)
			if err != nil {
				return asInvalid(err, "unknown parent %q", in.Parent)
			}
			parent = &pid
		}
		if in.CycleID, err = cycleRef(ctx, q, org, in.CycleID); err != nil {
			return err
		}
		if err := checkPlanningRefs(ctx, q, org, team.ID, planningRefs{
			Project: in.ProjectID, Milestone: in.MilestoneID, Cycle: in.CycleID,
			ProjectChanged: true, MilestoneChanged: true, CycleChanged: true,
		}); err != nil {
			return err
		}
		labelIDs, err := resolveLabels(ctx, q, org.ID, team.ID, in.Labels)
		if err != nil {
			return err
		}
		// A new issue goes to the top of its column (decisions-T14). The team
		// lock makes two creations in one column get different ranks. It
		// costs no extra serialisation: NextIssueNumber below already
		// row-locks the team's counter, so creates in a team are sequential
		// anyway. Order: rank lock, then counter row; no other path takes
		// them the other way round.
		if err := rankLock(ctx, tx, team.ID); err != nil {
			return err
		}
		rank, err := topRank(ctx, q, org.ID, team.ID, state.ID)
		if err != nil {
			return err
		}
		number, err := q.NextIssueNumber(ctx, dbq.NextIssueNumberParams{OrgID: org.ID, TeamID: team.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			// The team was deleted between the lookup and the lock (LAUN-191):
			// a body reference to a missing row is 400, as at the lookup.
			return httpx.Invalid("unknown team %q", in.Team)
		}
		if err != nil {
			return fmt.Errorf("next issue number: %w", err)
		}
		started, completed, canceled := stateTimestamps(state.Type, s.now(), nil, nil, nil)
		row, err := q.InsertIssue(ctx, dbq.InsertIssueParams{
			OrgID: org.ID, TeamID: team.ID, Number: number, Title: title, Description: in.Description,
			StateID: state.ID, Priority: priority, Estimate: in.Estimate, AssigneeID: assignee,
			CreatorID: &p.UserID, ParentID: parent, ProjectID: in.ProjectID, MilestoneID: in.MilestoneID,
			CycleID: in.CycleID, DueDate: due, StartedAt: started, CompletedAt: completed, CanceledAt: canceled,
			SortOrder: rank,
		})
		if err != nil {
			return fmt.Errorf("insert issue: %w", err)
		}
		id = row.ID
		for _, lid := range labelIDs {
			if err := q.InsertIssueLabel(ctx, dbq.InsertIssueLabelParams{OrgID: org.ID, IssueID: id, LabelID: lid}); err != nil {
				return fmt.Errorf("insert issue label: %w", err)
			}
		}
		if err := q.InsertStateChange(ctx, dbq.InsertStateChangeParams{
			OrgID: org.ID, IssueID: id, ToStateID: state.ID, ActorID: &p.UserID, Source: "api",
		}); err != nil {
			return fmt.Errorf("insert state change: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.IssueCreated, EntityType: "issue", EntityID: id, ActorID: &p.UserID,
			Payload: map[string]any{"id": id, "identifier": Identifier(team.Key, number), "stateId": state.ID, "sortOrder": rank,
				// For the notification consumer (LAUN-33): the issue as it was made.
				"teamId": team.ID, "assigneeId": assignee},
		}, events.ScopeTeams(team.ID))
		return err
	})
	if err != nil {
		return Issue{}, err
	}
	return s.getIssueByID(ctx, org, p, id, Includes{})
}

// asInvalid turns a 404 from a nested reference into a 400: the request
// path was fine, a value inside the body was not.
func asInvalid(err error, format string, args ...any) error {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return httpx.Invalid(format, args...)
	}
	return err
}

// UpdateIssueInput is the PATCH /issues/{id} body. Absent fields are left
// alone; null clears a nullable field.
type UpdateIssueInput struct {
	Version      *int32               `json:"version"` // optional compare-and-swap
	Title        httpx.Opt[string]    `json:"title"`
	Description  httpx.Opt[string]    `json:"description"`
	State        httpx.Opt[string]    `json:"state"`
	Priority     httpx.Opt[int]       `json:"priority"`
	Estimate     httpx.Opt[int32]     `json:"estimate"`
	Assignee     httpx.Opt[string]    `json:"assignee"`
	Labels       httpx.Opt[[]string]  `json:"labels"` // replaces the set
	AddLabels    []string             `json:"addLabels"`
	RemoveLabels []string             `json:"removeLabels"`
	Parent       httpx.Opt[string]    `json:"parent"`
	ProjectID    httpx.Opt[uuid.UUID] `json:"projectId"`
	MilestoneID  httpx.Opt[uuid.UUID] `json:"milestoneId"`
	CycleID      httpx.Opt[uuid.UUID] `json:"cycleId"`
	DueDate      httpx.Opt[string]    `json:"dueDate"`

	// Placement in the manual order (decisions-T14). At most one of the
	// three. With `state`, it is the position in the NEW column.
	SortOrder   *float64 `json:"sortOrder"`
	PlaceAfter  string   `json:"placeAfter"`  // issue id or identifier, same team and destination state
	PlaceBefore string   `json:"placeBefore"` // same
}

// UpdateIssue applies a partial update. The issue row is locked FOR UPDATE,
// so a concurrent PATCH waits; with `version` the caller gets 409
// version_conflict instead of overwriting a change it has not seen. A state
// change writes issue_state_changes, and every change appends one
// issue.updated event, in the same transaction.
func (s *Service) UpdateIssue(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, in UpdateIssueInput) (Issue, error) {
	scopeAll, scopeIDs := org.Access.Args()
	if in.Labels.Set && (len(in.AddLabels) > 0 || len(in.RemoveLabels) > 0) {
		return Issue{}, httpx.Invalid("use labels, or addLabels/removeLabels, not both")
	}
	// placement() (rank.go) rejects more than one of sortOrder, placeAfter
	// and placeBefore, and a bad sortOrder, with 400 validation_failed
	// (TestPlacementValidation, TestManualOrder_PlacementIsRefused).
	place, err := in.placement()
	if err != nil {
		return Issue{}, err
	}
	var id uuid.UUID
	var rebalanced bool // the destination column was renumbered (decisions-T14 #9)
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		rebalanced = false
		if id, err = resolveIssueID(ctx, q, org, ref); err != nil {
			return err
		}
		if in.Parent.Set && !in.Parent.Null {
			// Two concurrent PATCHes (A.parent=B, B.parent=A) could each pass
			// the cycle check against the other's old row. One org-wide lock
			// for re-parenting makes the checks sequential, and READ
			// COMMITTED then lets the second see the first's commit. It is
			// taken BEFORE the issue row lock: the parent FK check share-locks
			// the other issue's row, so the opposite order deadlocks.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:issue-parents:' || $1::text, 0))`, org.ID); err != nil {
				return fmt.Errorf("parent lock: %w", err)
			}
		}
		if place.set() || in.State.Set {
			// A placement or a column change reads and writes ranks of the
			// whole column: one team lock, BEFORE the issue row lock.
			// Lock order of this function, always: parent lock (above, only
			// when re-parenting), then the rank lock, then the issue row
			// (decisions-T14 #10). Both advisory locks come before any row.
			var teamID uuid.UUID
			if err := tx.QueryRow(ctx, `/* scope: resolved */ SELECT team_id FROM issues WHERE org_id = $1 AND id = $2`, org.ID, id).Scan(&teamID); err != nil {
				return notFound(err, "issue")
			}
			if err := rankLock(ctx, tx, teamID); err != nil {
				return err
			}
		}
		cur, err := q.LockIssue(ctx, dbq.LockIssueParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "issue")
		}
		if in.Version != nil && *in.Version != cur.Version {
			return httpx.Errorf(http.StatusConflict, httpx.CodeVersionConflict,
				"issue is at version %d, not %d; re-read it and retry", cur.Version, *in.Version)
		}
		next := dbq.UpdateIssueParams{
			OrgID: org.ID, ID: id, Title: cur.Title, Description: cur.Description, StateID: cur.StateID,
			Priority: cur.Priority, Estimate: cur.Estimate, AssigneeID: cur.AssigneeID, ParentID: cur.ParentID,
			ProjectID: cur.ProjectID, MilestoneID: cur.MilestoneID, CycleID: cur.CycleID, DueDate: cur.DueDate,
			StartedAt: cur.StartedAt, CompletedAt: cur.CompletedAt, CanceledAt: cur.CanceledAt,
			SortOrder: cur.SortOrder,
		}
		var changes []string
		changed := func(field string) { changes = append(changes, field) }

		if in.Title.Set {
			if in.Title.Null {
				return httpx.Invalid("title cannot be null")
			}
			t, err := validateTitle(in.Title.V)
			if err != nil {
				return err
			}
			if t != cur.Title {
				next.Title = t
				changed("title")
			}
		}
		if in.Description.Set {
			d := ""
			if !in.Description.Null {
				d = in.Description.V
			}
			if err := validateDescription(d); err != nil {
				return err
			}
			if d != cur.Description {
				next.Description = d
				changed("description")
			}
		}
		var fromState, toState *dbq.WorkflowState
		if in.State.Set {
			if in.State.Null {
				return httpx.Invalid("state cannot be null")
			}
			st, err := resolveState(ctx, q, org, cur.TeamID, in.State.V)
			if err != nil {
				return err
			}
			if st.ID != cur.StateID {
				prev, err := q.GetStateByID(ctx, dbq.GetStateByIDParams{OrgID: org.ID, ID: cur.StateID, AllTeams: scopeAll, TeamIds: scopeIDs})
				if err != nil {
					return fmt.Errorf("current state: %w", err)
				}
				fromState, toState = &prev, &st
				next.StateID = st.ID
				next.StartedAt, next.CompletedAt, next.CanceledAt =
					stateTimestamps(st.Type, s.now(), cur.StartedAt, cur.CompletedAt, cur.CanceledAt)
				changed("state")
			}
		}
		if in.Priority.Set {
			if in.Priority.Null {
				return httpx.Invalid("priority cannot be null; use 0 for no priority")
			}
			pr, err := validatePriority(in.Priority.V)
			if err != nil {
				return err
			}
			if pr != cur.Priority {
				next.Priority = pr
				changed("priority")
			}
		}
		if in.Estimate.Set {
			var e *int32
			if !in.Estimate.Null {
				v := in.Estimate.V
				e = &v
			}
			if err := validateEstimate(e); err != nil {
				return err
			}
			if !eqPtr(e, cur.Estimate) {
				next.Estimate = e
				changed("estimate")
			}
		}
		if in.Assignee.Set {
			var a *uuid.UUID
			if !in.Assignee.Null {
				aid, err := resolveAssignee(ctx, q, org.ID, p, in.Assignee.V)
				if err != nil {
					return err
				}
				a = &aid
			}
			if !eqPtr(a, cur.AssigneeID) {
				next.AssigneeID = a
				changed("assignee")
			}
		}
		if in.Parent.Set {
			var par *uuid.UUID
			if !in.Parent.Null {
				pid, err := resolveIssueID(ctx, q, org, in.Parent.V)
				if err != nil {
					return asInvalid(err, "unknown parent %q", in.Parent.V)
				}
				if err := checkNoParentCycle(ctx, tx, org, id, pid, in.Parent.V); err != nil {
					return err
				}
				par = &pid
			}
			if !eqPtr(par, cur.ParentID) {
				next.ParentID = par
				changed("parent")
			}
		}
		// forced[field]: the body names the issue's current project or
		// milestone, which the caller cannot see (T20 #40). It is judged as a
		// new value, so checkPlanningRefs refuses it in its usual order, with the
		// usual text and the same statements as a made-up id: a guess of a
		// hidden id is never confirmed.
		forced := map[string]bool{}
		for _, f := range []struct {
			name string
			opt  httpx.Opt[uuid.UUID]
			dst  **uuid.UUID
			cur  *uuid.UUID
		}{
			{"projectId", in.ProjectID, &next.ProjectID, cur.ProjectID},
			{"milestoneId", in.MilestoneID, &next.MilestoneID, cur.MilestoneID},
			{"cycleId", in.CycleID, &next.CycleID, cur.CycleID},
		} {
			if !f.opt.Set {
				continue
			}
			var v *uuid.UUID
			if !f.opt.Null {
				x := f.opt.V
				v = &x
			}
			// A current project or milestone the caller cannot see reads as
			// null (T20 #40): a null leaves it in place.
			if f.name != "cycleId" && f.cur != nil {
				hidden, err := hiddenPlanningRef(ctx, q, org, f.name, *f.cur)
				if err != nil {
					return err
				}
				if hidden && v == nil {
					continue
				}
				if hidden && *v == *f.cur {
					forced[f.name] = true
				}
			}
			if f.name == "cycleId" {
				// An imported Linear cycle id names its dev-ops cycle.
				if v, err = cycleRef(ctx, q, org, v); err != nil {
					return err
				}
			}
			if !eqPtr(v, f.cur) {
				*f.dst = v
				changed(f.name)
			}
		}
		projectChanged := !eqPtr(next.ProjectID, cur.ProjectID) || forced["projectId"]
		milestoneChanged := !eqPtr(next.MilestoneID, cur.MilestoneID) || forced["milestoneId"]
		// A hidden project replaced by a visible one takes its hidden milestone
		// with it: the caller cannot see or clear that milestone, and it
		// belongs to the project that left (T20 #40). A forced project fails
		// below, so the clear never commits for it.
		if projectChanged && cur.MilestoneID != nil && !milestoneChanged {
			hidden, err := hiddenPlanningRef(ctx, q, org, "milestoneId", *cur.MilestoneID)
			if err != nil {
				return err
			}
			if hidden {
				next.MilestoneID = nil
				changed("milestoneId")
				milestoneChanged = true
			}
		}
		if err := checkPlanningRefs(ctx, q, org, cur.TeamID, planningRefs{
			Project: next.ProjectID, Milestone: next.MilestoneID, Cycle: next.CycleID,
			ProjectChanged:   projectChanged,
			MilestoneChanged: milestoneChanged,
			CycleChanged:     !eqPtr(next.CycleID, cur.CycleID),
		}); err != nil {
			return err
		}
		if in.DueDate.Set {
			var d *time.Time
			if !in.DueDate.Null {
				t, err := parseDueDate(in.DueDate.V)
				if err != nil {
					return err
				}
				d = t
			}
			if !eqDate(d, cur.DueDate) {
				next.DueDate = d
				changed("dueDate")
			}
		}
		// Rank. A placement sets it in the destination column (the new one
		// when the state changes). A state change alone puts the issue on
		// top of the new column. Neither: the rank stays.
		if place.set() || toState != nil {
			dest := next.StateID
			var rank float64
			switch {
			case place.set():
				if rank, rebalanced, err = placeRank(ctx, q, org, cur, dest, place); err != nil {
					return err
				}
			default:
				if rank, err = topRank(ctx, q, org.ID, cur.TeamID, dest); err != nil {
					return err
				}
			}
			// A rebalance wrote other rows, so it always counts as a change of
			// this issue, even when its own rank came out the same: the row
			// is written, the version moves and the event is emitted.
			if rank != cur.SortOrder || rebalanced {
				next.SortOrder = rank
				changed("sortOrder")
			}
		}
		labelsAdded, labelsRemoved, err := applyLabelChanges(ctx, q, org.ID, cur.TeamID, id, in)
		if err != nil {
			return err
		}
		if len(labelsAdded)+len(labelsRemoved) > 0 {
			changed("labels")
		}
		if len(changes) == 0 {
			return nil // nothing to write; no version bump, no event
		}
		updated, err := q.UpdateIssue(ctx, next)
		if err != nil {
			return fmt.Errorf("update issue: %w", err)
		}
		acts, err := s.fieldActivity(ctx, q, org.ID, cur, next, labelsAdded, labelsRemoved)
		if err != nil {
			return err
		}
		if err := insertActivity(ctx, q, org.ID, id, &p.UserID, "api", acts...); err != nil {
			return err
		}
		payload := map[string]any{"id": id, "changes": changes, "version": updated.Version}
		if slices.Contains(changes, "sortOrder") {
			// With stateId a client can place the card without a refetch.
			payload["sortOrder"], payload["stateId"] = next.SortOrder, next.StateID
		}
		if rebalanced {
			// Other issues of the column got new ranks too and have no event
			// of their own: a reader refetches the column.
			payload["rebalanced"] = true
		}
		if toState != nil {
			if err := q.InsertStateChange(ctx, dbq.InsertStateChangeParams{
				OrgID: org.ID, IssueID: id, FromStateID: &fromState.ID, ToStateID: toState.ID,
				ActorID: &p.UserID, Source: "api",
			}); err != nil {
				return fmt.Errorf("insert state change: %w", err)
			}
			payload["fromStateId"], payload["toStateId"] = fromState.ID, toState.ID
		}
		team, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: cur.TeamID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return fmt.Errorf("issue team: %w", err)
		}
		payload["identifier"] = Identifier(team.Key, cur.Number)
		// For the notification consumer (LAUN-33): the team and the assignee as
		// they are after this change, and the one before it when it changed.
		payload["teamId"], payload["assigneeId"] = cur.TeamID, next.AssigneeID
		if slices.Contains(changes, "assignee") {
			payload["fromAssigneeId"] = cur.AssigneeID
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.IssueUpdated, EntityType: "issue", EntityID: id, ActorID: &p.UserID, Payload: payload,
		}, events.ScopeTeams(cur.TeamID))
		return err
	})
	if err != nil {
		return Issue{}, err
	}
	is, err := s.getIssueByID(ctx, org, p, id, Includes{})
	is.Rebalanced = rebalanced
	return is, err
}

// applyLabelChanges handles labels (replace) or addLabels/removeLabels.
func applyLabelChanges(ctx context.Context, q *dbq.Queries, orgID, teamID, issueID uuid.UUID, in UpdateIssueInput) (added, removed []uuid.UUID, err error) {
	if !in.Labels.Set && len(in.AddLabels) == 0 && len(in.RemoveLabels) == 0 {
		return nil, nil, nil
	}
	current, err := q.ListIssueLabelIDs(ctx, dbq.ListIssueLabelIDsParams{OrgID: orgID, IssueID: issueID})
	if err != nil {
		return nil, nil, fmt.Errorf("current labels: %w", err)
	}
	var add, remove []uuid.UUID
	if in.Labels.Set {
		want := []uuid.UUID{}
		if !in.Labels.Null {
			if want, err = resolveLabels(ctx, q, orgID, teamID, in.Labels.V); err != nil {
				return nil, nil, err
			}
		}
		for _, w := range want {
			if !slices.Contains(current, w) {
				add = append(add, w)
			}
		}
		for _, c := range current {
			if !slices.Contains(want, c) {
				remove = append(remove, c)
			}
		}
	} else {
		adds, err := resolveLabels(ctx, q, orgID, teamID, in.AddLabels)
		if err != nil {
			return nil, nil, err
		}
		rems, err := resolveRemovals(ctx, q, orgID, teamID, current, in.RemoveLabels)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range adds {
			if !slices.Contains(current, a) && !slices.Contains(rems, a) {
				add = append(add, a)
			}
		}
		for _, r := range rems {
			if slices.Contains(current, r) {
				remove = append(remove, r)
			}
		}
	}
	for _, a := range add {
		if err := q.InsertIssueLabel(ctx, dbq.InsertIssueLabelParams{OrgID: orgID, IssueID: issueID, LabelID: a}); err != nil {
			return nil, nil, fmt.Errorf("add label: %w", err)
		}
	}
	for _, r := range remove {
		if err := q.DeleteIssueLabel(ctx, dbq.DeleteIssueLabelParams{OrgID: orgID, IssueID: issueID, LabelID: r}); err != nil {
			return nil, nil, fmt.Errorf("remove label: %w", err)
		}
	}
	return add, remove, nil
}

// checkNoParentCycle refuses a parent that is the issue itself or one of its
// descendants (walking the new parent's ancestors, bounded at depth 100).
//
// The walk may pass through issues of teams the caller cannot see. So a
// restricted caller is told only "unknown parent", the text of a parent that
// does not exist (T20 #23); the text names no hidden issue and does not say
// a chain exists. A caller who sees every team keeps the plain text.
func checkNoParentCycle(ctx context.Context, tx pgx.Tx, org *auth.Org, issueID, parentID uuid.UUID, ref string) error {
	orgID := org.ID
	if issueID == parentID {
		return httpx.Invalid("an issue cannot be its own parent")
	}
	var cycle bool
	err := tx.QueryRow(ctx, `/* scope: resolved */
		WITH RECURSIVE anc(id, parent_id, depth) AS (
			SELECT id, parent_id, 1 FROM issues WHERE org_id = $1 AND id = $2
			UNION ALL
			SELECT i.id, i.parent_id, a.depth + 1
			FROM issues i JOIN anc a ON i.org_id = $1 AND i.id = a.parent_id
			WHERE a.depth < 100
		)
		SELECT EXISTS (SELECT 1 FROM anc WHERE id = $3)`, orgID, parentID, issueID).Scan(&cycle)
	if err != nil {
		return fmt.Errorf("parent cycle check: %w", err)
	}
	if cycle {
		if !org.Access.All {
			return httpx.Invalid("unknown parent %q", ref)
		}
		return httpx.Invalid("that parent would make a cycle")
	}
	return nil
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqDate(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// resolveRemovals resolves removeLabels. A name that matches a label the issue
// already carries (org or team scope) means that label; anything else resolves
// as for an add (LAUN-54: "bug" removes the org "bug" even when the team has
// its own "bug").
func resolveRemovals(ctx context.Context, q *dbq.Queries, orgID, teamID uuid.UUID, current []uuid.UUID, refs []string) ([]uuid.UUID, error) {
	if len(refs) == 0 || len(current) == 0 {
		return resolveLabels(ctx, q, orgID, teamID, refs)
	}
	carried, err := q.GetLabelsByIDs(ctx, dbq.GetLabelsByIDsParams{OrgID: orgID, Ids: current})
	if err != nil {
		return nil, fmt.Errorf("carried labels: %w", err)
	}
	var rest []string
	var ids []uuid.UUID
	for _, r := range refs {
		hit := false
		if _, perr := uuid.Parse(r); perr != nil {
			for _, c := range carried {
				if strings.EqualFold(c.Name, strings.TrimSpace(r)) {
					ids, hit = append(ids, c.ID), true
				}
			}
		}
		if !hit {
			rest = append(rest, r)
		}
	}
	more, err := resolveLabels(ctx, q, orgID, teamID, rest)
	if err != nil {
		return nil, err
	}
	return append(ids, more...), nil
}

// hiddenPlanningRef tells whether an issue's current project ("projectId") or
// milestone ("milestoneId") is one org cannot see (T20 #40). A milestone is
// judged by its project. A caller who sees every team sees both.
func hiddenPlanningRef(ctx context.Context, q *dbq.Queries, org *auth.Org, field string, id uuid.UUID) (bool, error) {
	if org.Access.All {
		return false, nil
	}
	all, ids := org.Access.Args()
	var err error
	if field == "projectId" {
		_, err = q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
	} else {
		_, err = q.GetMilestoneProjectID(ctx, dbq.GetMilestoneProjectIDParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s lookup: %w", field, err)
	}
	return false, nil
}
