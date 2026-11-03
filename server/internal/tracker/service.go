// Package tracker is the domain layer: issues, comments, relations, labels,
// teams, workflow states, members, tokens and the change feed. Every method
// takes the org resolved by auth.RequireOrgMember, and every query filters by
// that org id, so a row from another org is "not found" (the composite
// (org_id, id) foreign keys make the same promise at the database level).
package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/storage"
)

// Service is the domain service. It is safe for concurrent use.
type Service struct {
	pool *pgxpool.Pool
	q    *dbq.Queries
	now  func() time.Time
	// maxCycles is the cycle job's catch-up cap per team and run (0: the
	// default).
	maxCycles int

	// Attachments: the object store (nil: file routes are off)
	// and the API origin download URLs are built on. Set before serving.
	store         storage.Store
	publicBaseURL string

	// The sealed git secrets (T17 #11, LAUN-5): the opener (nil: every sealed
	// secret reads as unreadable) and the cache of opened webhook secrets.
	gitSealer GitSealer
	gitCache  gitSecretCache
	// Access to git hosts for the config routes (T17, LAUN-7); nil: token
	// mode is off.
	gitNet *GitNet
	// The in-memory throttle of the deliveries ring's unbound rows (LAUN-87).
	gitDeliveryThrottle gitDeliveryThrottleState
	// backfillTimeout bounds one backfill (0: gitapi.RouteTimeout); tests shorten it.
	backfillTimeout time.Duration
}

// New returns a Service on pool.
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: dbq.New(pool), now: time.Now}
}

// Pool exposes the pool for health checks.
func (s *Service) Pool() *pgxpool.Pool { return s.pool }

// notFound maps pgx.ErrNoRows to a 404 for what; other errors are wrapped.
func notFound(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NotFound(what)
	}
	return fmt.Errorf("%s lookup: %w", what, err)
}

// invalidRef maps pgx.ErrNoRows to a 400 for a reference inside a body.
func invalidRef(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Invalid(format, args...)
	}
	return fmt.Errorf("resolve reference: %w", err)
}

// resolveIssueID turns "ORB-123", an issue UUID, or an imported Linear UUID
// into the issue id, within org and the teams org.Access can see (T20 #14).
// An issue of a hidden team is not found, in the same words as a missing one.
func resolveIssueID(ctx context.Context, q *dbq.Queries, org *auth.Org, ref string) (uuid.UUID, error) {
	all, ids := org.Access.Args()
	if key, n, ok := ParseIdentifier(ref); ok {
		id, err := q.ResolveIssueByNumber(ctx, dbq.ResolveIssueByNumberParams{OrgID: org.ID, Key: key, Number: n, AllTeams: all, TeamIds: ids})
		return id, notFound(err, "issue")
	}
	if id, err := uuid.Parse(ref); err == nil {
		got, err := q.IssueExists(ctx, dbq.IssueExistsParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("issue lookup: %w", err)
		}
	}
	// An id from Linear survives through import_mappings (T4); the scoped
	// variant checks the issue's team.
	got, err := q.ResolveImportedIssueID(ctx, dbq.ResolveImportedIssueIDParams{OrgID: org.ID, SourceID: ref, AllTeams: all, TeamIds: ids})
	return got, notFound(err, "issue")
}

// resolveTeam turns a team key, a team id or an imported Linear team id
// (import_mappings, T4b) into the team, within org and the teams org.Access
// can see (T20 #14).
func resolveTeam(ctx context.Context, q *dbq.Queries, org *auth.Org, ref string) (dbq.Team, error) {
	all, ids := org.Access.Args()
	if id, err := uuid.Parse(ref); err == nil {
		t, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
		if !errors.Is(err, pgx.ErrNoRows) {
			return t, err
		}
		// The mapping holds the canonical form; ref may be upper case or
		// braced ({...}), which uuid.Parse accepts.
		target, ok, merr := importedID(ctx, q, org, importedTeam, id.String())
		if merr != nil {
			return t, merr
		}
		if !ok {
			return t, err
		}
		return q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: target, AllTeams: all, TeamIds: ids})
	}
	return q.GetTeamByKey(ctx, dbq.GetTeamByKeyParams{OrgID: org.ID, Key: strings.ToUpper(ref), AllTeams: all, TeamIds: ids})
}

// resolveState turns a state id or name into a state of teamID. A state id of
// a team the caller cannot see gives the same answer as an unknown id (T20
// #23): GetStateByID is scoped by the team join.
func resolveState(ctx context.Context, q *dbq.Queries, org *auth.Org, teamID uuid.UUID, ref string) (dbq.WorkflowState, error) {
	all, ids := org.Access.Args()
	if id, err := uuid.Parse(ref); err == nil {
		st, err := q.GetStateByID(ctx, dbq.GetStateByIDParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
		if err != nil {
			return st, invalidRef(err, "unknown state %q", ref)
		}
		if st.TeamID != teamID || st.ArchivedAt != nil {
			return st, httpx.Invalid("state %q does not belong to the issue's team", ref)
		}
		return st, nil
	}
	st, err := q.GetStateByTeamAndName(ctx, dbq.GetStateByTeamAndNameParams{OrgID: org.ID, TeamID: teamID, Name: ref})
	if err != nil {
		return st, invalidRef(err, "unknown state %q for this team", ref)
	}
	return st, nil
}

// resolveAssignee: "me" is the caller; otherwise a username of an org member.
func resolveAssignee(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, p *auth.Principal, ref string) (uuid.UUID, error) {
	if ref == "me" {
		return p.UserID, nil
	}
	m, err := q.GetOrgMemberByUsername(ctx, dbq.GetOrgMemberByUsernameParams{OrgID: orgID, Username: strings.ToLower(ref)})
	if err != nil {
		return uuid.Nil, invalidRef(err, "assignee %q is not a member of this org", ref)
	}
	return m.ID, nil
}

// resolveLabels turns label ids or names into ids. Every ref must resolve.
func resolveLabels(ctx context.Context, q *dbq.Queries, orgID, teamID uuid.UUID, refs []string) ([]uuid.UUID, error) {
	if len(refs) == 0 {
		return []uuid.UUID{}, nil
	}
	if len(refs) > 50 {
		return nil, httpx.Invalid("at most 50 labels per request")
	}
	ids := []uuid.UUID{}
	names := []string{}
	for _, r := range refs {
		if id, err := uuid.Parse(r); err == nil {
			ids = append(ids, id)
		} else {
			names = append(names, strings.ToLower(strings.TrimSpace(r)))
		}
	}
	found, err := q.FindLabels(ctx, dbq.FindLabelsParams{OrgID: orgID, TeamID: &teamID, Ids: ids, Names: names})
	if err != nil {
		return nil, fmt.Errorf("find labels: %w", err)
	}
	byID := map[uuid.UUID]bool{}
	byName := map[string]uuid.UUID{}
	for _, l := range found {
		byID[l.ID] = true
		// A name that is both an org label and a label of this team means the
		// team's (decisions-T21-team-labels #4).
		if k := strings.ToLower(l.Name); l.TeamID != nil || byName[k] == uuid.Nil {
			byName[k] = l.ID
		}
	}
	out := make([]uuid.UUID, 0, len(refs))
	seen := map[uuid.UUID]bool{}
	for _, r := range refs {
		var id uuid.UUID
		if pid, err := uuid.Parse(r); err == nil {
			if !byID[pid] {
				return nil, httpx.Invalid("unknown label %q", r)
			}
			id = pid
		} else {
			lid, ok := byName[strings.ToLower(strings.TrimSpace(r))]
			if !ok {
				return nil, httpx.Invalid("unknown label %q", r)
			}
			id = lid
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func parseDueDate(s string) (*time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, httpx.Invalid("dueDate must be YYYY-MM-DD")
	}
	return &t, nil
}

func formatDate(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

// planningRefs are the project, milestone and cycle an issue will hold
// after a write (nil: none), and which of them the write changes.
type planningRefs struct {
	Project, Milestone, Cycle                      *uuid.UUID
	ProjectChanged, MilestoneChanged, CycleChanged bool
}

// checkPlanningRefs validates an issue's planning references against the
// org (a reference to another org's row is a 400, decisions-T1 #25):
//   - the project must be a project of this org;
//   - the milestone must be a milestone of this org AND of the issue's
//     project, so a milestone needs its project set (a 400 tells the caller
//     to set or clear both; nothing is filled in silently);
//   - the cycle must be a cycle of this org AND of the issue's team.
//
// Only what the write changes is checked, except that a project change
// re-checks a kept milestone against the new project.
func checkPlanningRefs(ctx context.Context, q *dbq.Queries, org *auth.Org, teamID uuid.UUID, r planningRefs) error {
	all, ids := org.Access.Args()
	if r.ProjectChanged && r.Project != nil {
		if _, err := q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: *r.Project, AllTeams: all, TeamIds: ids}); err != nil {
			return invalidRef(err, "unknown projectId")
		}
	}
	if (r.MilestoneChanged || r.ProjectChanged) && r.Milestone != nil {
		pid, err := q.GetMilestoneProjectID(ctx, dbq.GetMilestoneProjectIDParams{OrgID: org.ID, ID: *r.Milestone, AllTeams: all, TeamIds: ids})
		if err != nil {
			return invalidRef(err, "unknown milestoneId")
		}
		if r.Project == nil || *r.Project != pid {
			return httpx.Invalid("milestoneId is a milestone of project %s; set projectId to it, or clear milestoneId", pid)
		}
	}
	if r.CycleChanged && r.Cycle != nil {
		tid, err := q.GetCycleTeamID(ctx, dbq.GetCycleTeamIDParams{OrgID: org.ID, ID: *r.Cycle, AllTeams: all, TeamIds: ids})
		if err != nil {
			return invalidRef(err, "unknown cycleId")
		}
		if tid != teamID {
			return httpx.Invalid("cycleId is a cycle of another team")
		}
	}
	return nil
}

// stateTimestamps sets started/completed/canceled from the new state's type:
//   - started: startedAt is set if it was empty.
//   - completed: completedAt = now (startedAt set if empty); canceledAt cleared.
//   - canceled: canceledAt = now; completedAt cleared.
//   - backlog / unstarted / triage: all three cleared (the work is not started).
func stateTimestamps(stateType string, now time.Time, started, completed, canceled *time.Time) (*time.Time, *time.Time, *time.Time) {
	n := now
	switch stateType {
	case "started":
		if started == nil {
			started = &n
		}
		return started, nil, nil
	case "completed":
		if started == nil {
			started = &n
		}
		if completed == nil {
			completed = &n
		}
		return started, completed, nil
	case "canceled":
		if canceled == nil {
			canceled = &n
		}
		return started, nil, canceled
	default:
		return nil, nil, nil
	}
}

// conflict is the generic 409.
func conflict(format string, args ...any) error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, format, args...)
}

func userRef(id *uuid.UUID, username, name *string) *UserRef {
	if id == nil || username == nil {
		return nil
	}
	n := ""
	if name != nil {
		n = *name
	}
	return &UserRef{ID: *id, Username: *username, Name: n}
}

// ResolveTeamID turns a team key, id or imported Linear id into the team id,
// within org. Another org's team is 404 not_found.
func (s *Service) ResolveTeamID(ctx context.Context, org *auth.Org, ref string) (uuid.UUID, error) {
	t, err := resolveTeam(ctx, s.q, org, ref)
	if err != nil {
		return uuid.Nil, notFound(err, "team")
	}
	return t.ID, nil
}

// ResolveIssueID turns an identifier, id or imported Linear id into the issue
// id, within org. Another org's issue is 404 not_found.
func (s *Service) ResolveIssueID(ctx context.Context, org *auth.Org, ref string) (uuid.UUID, error) {
	return resolveIssueID(ctx, s.q, org, ref)
}
