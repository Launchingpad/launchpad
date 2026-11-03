package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Activity kinds recorded in issue_activity (decisions-T15-activity.md).
const (
	ActTitle          = "title"
	ActDescription    = "description"
	ActAssignee       = "assignee"
	ActPriority       = "priority"
	ActEstimate       = "estimate"
	ActDueDate        = "dueDate"
	ActParent         = "parent"
	ActProject        = "project"
	ActMilestone      = "milestone"
	ActCycle          = "cycle"
	ActLabelAdded     = "labelAdded"
	ActLabelRemoved   = "labelRemoved"
	ActRelationRemove = "relationRemoved"
	// LAUN-83 (decisions-T17 #20): written by the git webhooks and the
	// backfill, with source = the provider and no actor.
	ActMRLinked = "mrLinked"
	ActMRMerged = "mrMerged"
	ActMRClosed = "mrClosed"
)

// Activity is one recorded change of an issue. From and To are small JSON
// snapshots: null, a plain value, or an object with an id and a name made
// when the change was written. A description edit has neither.
type Activity struct {
	ID        uuid.UUID       `json:"id"`
	Kind      string          `json:"kind"`
	Actor     *UserRef        `json:"actor"`
	Source    string          `json:"source"` // api | gitlab | github | bitbucket | import | system
	From      json.RawMessage `json:"from"`
	To        json.RawMessage `json:"to"`
	CreatedAt time.Time       `json:"createdAt"`
}

// activityRow is a change to write.
type activityRow struct {
	kind     string
	from, to any
}

func jsonOrNull(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// insertActivity writes rows for one issue in the caller's transaction.
// A nil actor is a change with no user (the cycle job). It never holds a
// secret: snapshots are ids, names and plain field values only.
func insertActivity(ctx context.Context, q *dbq.Queries, orgID, issueID uuid.UUID, actor *uuid.UUID, source string, rows ...activityRow) error {
	for _, r := range rows {
		from, err := jsonOrNull(r.from)
		if err != nil {
			return fmt.Errorf("activity from: %w", err)
		}
		to, err := jsonOrNull(r.to)
		if err != nil {
			return fmt.Errorf("activity to: %w", err)
		}
		if err := q.InsertIssueActivity(ctx, dbq.InsertIssueActivityParams{
			OrgID: orgID, IssueID: issueID, ActorID: actor, Source: source, Kind: r.kind, FromValue: from, ToValue: to,
		}); err != nil {
			return fmt.Errorf("insert activity: %w", err)
		}
	}
	return nil
}

type namedRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type userSnap struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
}

type cycleSnap struct {
	ID     uuid.UUID `json:"id"`
	Number int32     `json:"number"`
	Name   *string   `json:"name"`
}

type issueSnap struct {
	ID         uuid.UUID `json:"id"`
	Identifier string    `json:"identifier"`
	Title      string    `json:"title"`
}

// snapUser, snapProject and the others return nil for a nil id, else the
// snapshot. A row that cannot be found gives just its id: the write
// already passed its own checks, so this is only a missing name.
func snapUser(ctx context.Context, q *dbq.Queries, id *uuid.UUID) (any, error) {
	if id == nil {
		return nil, nil
	}
	rows, err := q.GetUserRefs(ctx, []uuid.UUID{*id})
	if err != nil {
		return nil, fmt.Errorf("snapshot user: %w", err)
	}
	if len(rows) == 0 {
		return map[string]any{"id": *id}, nil
	}
	return userSnap{ID: rows[0].ID, Username: rows[0].Username, Name: rows[0].Name}, nil
}

func snapProject(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, id *uuid.UUID) (any, error) {
	if id == nil {
		return nil, nil
	}
	p, err := q.GetProjectSystem(ctx, dbq.GetProjectSystemParams{OrgID: orgID, ID: *id})
	if err != nil {
		return map[string]any{"id": *id}, nil //nolint:nilerr // see above
	}
	return namedRef{ID: p.ID, Name: p.Name}, nil
}

func snapMilestone(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, id *uuid.UUID) (any, error) {
	if id == nil {
		return nil, nil
	}
	m, err := q.GetMilestoneSystem(ctx, dbq.GetMilestoneSystemParams{OrgID: orgID, ID: *id})
	if err != nil {
		return map[string]any{"id": *id}, nil //nolint:nilerr // see above
	}
	return namedRef{ID: m.ID, Name: m.Name}, nil
}

func snapCycle(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, id *uuid.UUID) (any, error) {
	if id == nil {
		return nil, nil
	}
	c, err := q.GetCycleSystem(ctx, dbq.GetCycleSystemParams{OrgID: orgID, ID: *id})
	if err != nil {
		return map[string]any{"id": *id}, nil //nolint:nilerr // see above
	}
	return cycleSnap{ID: c.ID, Number: c.Number, Name: c.Name}, nil
}

func snapIssue(ctx context.Context, s *Service, q *dbq.Queries, orgID uuid.UUID, id *uuid.UUID) (any, error) {
	if id == nil {
		return nil, nil
	}
	rows, err := q.ListIssueRefsSystem(ctx, dbq.ListIssueRefsSystemParams{OrgID: orgID, Ids: []uuid.UUID{*id}})
	if err != nil {
		return nil, fmt.Errorf("snapshot issue: %w", err)
	}
	if len(rows) == 0 {
		return map[string]any{"id": *id}, nil
	}
	return issueSnap{ID: rows[0].ID, Identifier: Identifier(rows[0].TeamKey, rows[0].Number), Title: rows[0].Title}, nil
}

// labelSnaps names labels by id, in the order given.
func labelSnaps(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, ids []uuid.UUID) ([]namedRef, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.GetLabelsByIDs(ctx, dbq.GetLabelsByIDsParams{OrgID: orgID, Ids: ids})
	if err != nil {
		return nil, fmt.Errorf("snapshot labels: %w", err)
	}
	names := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		names[r.ID] = r.Name
	}
	out := make([]namedRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, namedRef{ID: id, Name: names[id]})
	}
	return out, nil
}

// ActivityPage is one page of GET /issues/{issue}/activity.
type ActivityPage = httpx.Page[Activity]

// ListActivity returns an issue's recorded activity, oldest first, cursor
// paged. State changes, comments and attachments are not in it: they have
// their own reads (decisions-T15).
func (s *Service) ListActivity(ctx context.Context, org *auth.Org, issueRef string, lq listQuery) (ActivityPage, error) {
	id, err := resolveIssueID(ctx, s.q, org, issueRef)
	if err != nil {
		return ActivityPage{}, err
	}
	arg := dbq.ListIssueActivityParams{OrgID: org.ID, IssueID: id, MaxRows: int32(lq.Limit + 1)}
	if lq.Cursor != nil {
		arg.AfterAt, arg.AfterID = &lq.Cursor.At, &lq.Cursor.ID
	}
	rows, err := s.q.ListIssueActivity(ctx, arg)
	if err != nil {
		return ActivityPage{}, fmt.Errorf("list activity: %w", err)
	}
	page := ActivityPage{Items: []Activity{}}
	for i, r := range rows {
		if i == lq.Limit {
			c := encodeCursor(cursor{OrderBy: "activity", At: rows[i-1].CreatedAt, ID: rows[i-1].ID})
			page.NextCursor = &c
			break
		}
		page.Items = append(page.Items, Activity{
			ID: r.ID, Kind: r.Kind, Actor: userRef(r.ActorID, r.ActorUsername, r.ActorName),
			Source: r.Source, From: r.FromValue, To: r.ToValue, CreatedAt: r.CreatedAt,
		})
	}
	if err := s.redactActivity(ctx, org, page.Items); err != nil {
		return ActivityPage{}, err
	}
	return page, nil
}

// snapRef is what redaction reads from a stored snapshot: the id of the
// object it names. A relationRemoved row nests the issue under "issue".
type snapRef struct {
	ID    *uuid.UUID `json:"id"`
	Issue *struct {
		ID *uuid.UUID `json:"id"`
	} `json:"issue"`
}

// snapID is the id a stored snapshot names, or nil (null, a plain value, or
// JSON this code did not write).
func snapID(kind string, raw json.RawMessage) *uuid.UUID {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var r snapRef
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	if kind == ActRelationRemove {
		if r.Issue == nil {
			return nil
		}
		return r.Issue.ID
	}
	return r.ID
}

// hide says whether a non-null snapshot must be redacted.
func hide(kind string, raw json.RawMessage, visible map[uuid.UUID]bool) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	id := snapID(kind, raw)
	return id == nil || !visible[*id]
}

// redactActivity applies T20 #43 to one page. A snapshot names another issue
// (parent, relationRemoved), a project, a milestone or a cycle; it is stored
// whole when written (the System lookups). At read time the ids of the page
// are looked up with the caller's scope, one query per kind, and a snapshot
// whose object the caller cannot see becomes null. The row is still returned:
// the edit is a fact of the caller's own issue. A snapshot whose object no
// longer exists is also null for such a caller (it fails closed: its team is
// unknown). A caller who sees every team gets the stored rows as they are,
// so admins and all-teams members read what they read before.
func (s *Service) redactActivity(ctx context.Context, org *auth.Org, items []Activity) error {
	if org.Access.All {
		return nil
	}
	scopeAll, scopeIDs := org.Access.Args()
	ids := map[string][]uuid.UUID{}
	kindOf := func(k string) string {
		switch k {
		case ActParent, ActRelationRemove:
			return "issue"
		case ActProject, ActMilestone, ActCycle:
			return k
		}
		return ""
	}
	for _, a := range items {
		k := kindOf(a.Kind)
		if k == "" {
			continue
		}
		for _, raw := range []json.RawMessage{a.From, a.To} {
			if id := snapID(a.Kind, raw); id != nil {
				ids[k] = append(ids[k], *id)
			}
		}
	}
	visible := map[uuid.UUID]bool{}
	mark := func(found []uuid.UUID, err error) error {
		if err != nil {
			return fmt.Errorf("activity redaction: %w", err)
		}
		for _, id := range found {
			visible[id] = true
		}
		return nil
	}
	if len(ids["issue"]) > 0 {
		rows, err := s.q.ListIssueRefs(ctx, dbq.ListIssueRefsParams{OrgID: org.ID, Ids: ids["issue"], AllTeams: scopeAll, TeamIds: scopeIDs})
		found := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			found = append(found, r.ID)
		}
		if err := mark(found, err); err != nil {
			return err
		}
	}
	if len(ids[ActProject]) > 0 {
		if err := mark(s.q.ListVisibleProjectIDs(ctx, dbq.ListVisibleProjectIDsParams{OrgID: org.ID, Ids: ids[ActProject], AllTeams: scopeAll, TeamIds: scopeIDs})); err != nil {
			return err
		}
	}
	if len(ids[ActMilestone]) > 0 {
		if err := mark(s.q.ListVisibleMilestoneIDs(ctx, dbq.ListVisibleMilestoneIDsParams{OrgID: org.ID, Ids: ids[ActMilestone], AllTeams: scopeAll, TeamIds: scopeIDs})); err != nil {
			return err
		}
	}
	if len(ids[ActCycle]) > 0 {
		if err := mark(s.q.ListVisibleCycleIDs(ctx, dbq.ListVisibleCycleIDsParams{OrgID: org.ID, Ids: ids[ActCycle], AllTeams: scopeAll, TeamIds: scopeIDs})); err != nil {
			return err
		}
	}
	null := json.RawMessage("null")
	for i := range items {
		a := &items[i]
		if kindOf(a.Kind) == "" {
			continue
		}
		// A snapshot with no id this code can read is null too (fail closed).
		if hide(a.Kind, a.From, visible) {
			a.From = null
		}
		if hide(a.Kind, a.To, visible) {
			a.To = null
		}
	}
	return nil
}

// ParseActivityQuery reads limit and cursor of the activity list.
func ParseActivityQuery(v url.Values) (listQuery, error) {
	return parseListQuery(v, "activity")
}

// fieldActivity turns an UpdateIssue into activity rows: one per changed
// field and one per label added or removed. State and sortOrder are not
// here (state has issue_state_changes; rank is board order, not history).
func (s *Service) fieldActivity(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, cur dbq.Issue, next dbq.UpdateIssueParams,
	labelsAdded, labelsRemoved []uuid.UUID) ([]activityRow, error) {
	var rows []activityRow
	if next.Title != cur.Title {
		rows = append(rows, activityRow{ActTitle, cur.Title, next.Title})
	}
	if next.Description != cur.Description {
		rows = append(rows, activityRow{kind: ActDescription})
	}
	if next.Priority != cur.Priority {
		rows = append(rows, activityRow{ActPriority, cur.Priority, next.Priority})
	}
	if !eqPtr(next.Estimate, cur.Estimate) {
		rows = append(rows, activityRow{ActEstimate, ptrVal(cur.Estimate), ptrVal(next.Estimate)})
	}
	if !eqDate(next.DueDate, cur.DueDate) {
		rows = append(rows, activityRow{ActDueDate, dateVal(cur.DueDate), dateVal(next.DueDate)})
	}
	type snapFn func(*uuid.UUID) (any, error)
	for _, f := range []struct {
		kind     string
		from, to *uuid.UUID
		snap     snapFn
	}{
		{ActAssignee, cur.AssigneeID, next.AssigneeID, func(id *uuid.UUID) (any, error) { return snapUser(ctx, q, id) }},
		{ActParent, cur.ParentID, next.ParentID, func(id *uuid.UUID) (any, error) { return snapIssue(ctx, s, q, orgID, id) }},
		{ActProject, cur.ProjectID, next.ProjectID, func(id *uuid.UUID) (any, error) { return snapProject(ctx, q, orgID, id) }},
		{ActMilestone, cur.MilestoneID, next.MilestoneID, func(id *uuid.UUID) (any, error) { return snapMilestone(ctx, q, orgID, id) }},
		{ActCycle, cur.CycleID, next.CycleID, func(id *uuid.UUID) (any, error) { return snapCycle(ctx, q, orgID, id) }},
	} {
		if eqPtr(f.from, f.to) {
			continue
		}
		from, err := f.snap(f.from)
		if err != nil {
			return nil, err
		}
		to, err := f.snap(f.to)
		if err != nil {
			return nil, err
		}
		rows = append(rows, activityRow{f.kind, from, to})
	}
	for _, l := range []struct {
		kind string
		ids  []uuid.UUID
	}{{ActLabelAdded, labelsAdded}, {ActLabelRemoved, labelsRemoved}} {
		snaps, err := labelSnaps(ctx, q, orgID, l.ids)
		if err != nil {
			return nil, err
		}
		for _, sn := range snaps {
			if l.kind == ActLabelAdded {
				rows = append(rows, activityRow{kind: l.kind, to: sn})
			} else {
				rows = append(rows, activityRow{kind: l.kind, from: sn})
			}
		}
	}
	return rows, nil
}

// ptrVal gives nil for a nil pointer (an untyped nil, so it stores as SQL
// NULL), else the value.
func ptrVal[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func dateVal(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}
