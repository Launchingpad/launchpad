package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Issue reads are hand-written pgx because the list is built from optional
// filters. Every value is a bind parameter; the only text spliced into the
// SQL is from fixed allowlists (the description column form and orderBy).

// issueSelect is the issue SELECT and its joins, with desc as the description
// column form. It binds the caller's access through arg, so the joins that
// name another row are scoped (T20 #11):
//   - the parent is joined only when the caller sees its team, so a hidden
//     parent gives parent null (#42);
//   - projectId is the project only when the caller sees it (#36, #40);
//   - milestoneId is the milestone only when the caller sees its project (#38,
//     #40).
//
// This is read-time redaction: the stored row is unchanged. The caller adds
// the WHERE, which must hold teamScope on i.team_id.
func issueSelect(org *auth.Org, arg argFunc, desc string) string {
	return fmt.Sprintf(`/* scope: team */
SELECT i.id, i.team_id, t.key, t.name, i.number, i.title, %s, i.priority, i.estimate,
       i.state_id, s.name, s.type, s.color,
       i.assignee_id, a.username, a.name,
       i.creator_id, c.username, c.name,
       p.id, pt.key, p.number, p.title,
       pj.id, ms.id, i.cycle_id, i.due_date, i.version,
       i.created_at, i.updated_at, i.started_at, i.completed_at, i.canceled_at, i.archived_at, i.sort_order
FROM issues i
JOIN teams t            ON t.org_id = i.org_id AND t.id = i.team_id
JOIN workflow_states s  ON s.org_id = i.org_id AND s.id = i.state_id
LEFT JOIN users a       ON a.id = i.assignee_id
LEFT JOIN users c       ON c.id = i.creator_id
LEFT JOIN issues p      ON p.org_id = i.org_id AND p.id = i.parent_id AND %s
LEFT JOIN teams pt      ON pt.org_id = p.org_id AND pt.id = p.team_id
LEFT JOIN projects pj   ON pj.org_id = i.org_id AND pj.id = i.project_id AND %s
LEFT JOIN milestones ms ON ms.org_id = i.org_id AND ms.id = i.milestone_id
	AND EXISTS (SELECT 1 FROM projects mp WHERE mp.org_id = ms.org_id AND mp.id = ms.project_id AND %s)
`, desc, teamScope(org, "p.team_id", arg), projectScope(org, "pj", arg), projectScope(org, "mp", arg))
}

// listDescriptionRunes is the description cut for list responses.
const listDescriptionRunes = 400

const descFull = "i.description"

// descList is the list form: one rune over the cut, so the reader can tell a
// cut happened.
var descList = fmt.Sprintf("left(i.description, %d)", listDescriptionRunes+1)

type issueRow struct {
	ID                                          uuid.UUID
	TeamID                                      uuid.UUID
	TeamKey, TeamName                           string
	Number                                      int32
	Title, Description                          string
	Priority                                    int16
	Estimate                                    *int32
	StateID                                     uuid.UUID
	StateName, StateType, StateColor            string
	AssigneeID                                  *uuid.UUID
	AssigneeUsername, AssigneeName              *string
	CreatorID                                   *uuid.UUID
	CreatorUsername, CreatorName                *string
	ParentID                                    *uuid.UUID
	ParentTeamKey                               *string
	ParentNumber                                *int32
	ParentTitle                                 *string
	ProjectID, MilestoneID, CycleID             *uuid.UUID
	DueDate                                     *time.Time
	Version                                     int32
	CreatedAt, UpdatedAt                        time.Time
	StartedAt, CompletedAt, CanceledAt, Archive *time.Time
	SortOrder                                   float64
}

func scanIssue(row pgx.Row) (issueRow, error) {
	var r issueRow
	err := row.Scan(&r.ID, &r.TeamID, &r.TeamKey, &r.TeamName, &r.Number, &r.Title, &r.Description, &r.Priority, &r.Estimate,
		&r.StateID, &r.StateName, &r.StateType, &r.StateColor,
		&r.AssigneeID, &r.AssigneeUsername, &r.AssigneeName,
		&r.CreatorID, &r.CreatorUsername, &r.CreatorName,
		&r.ParentID, &r.ParentTeamKey, &r.ParentNumber, &r.ParentTitle,
		&r.ProjectID, &r.MilestoneID, &r.CycleID, &r.DueDate, &r.Version,
		&r.CreatedAt, &r.UpdatedAt, &r.StartedAt, &r.CompletedAt, &r.CanceledAt, &r.Archive, &r.SortOrder)
	return r, err
}

// toIssue builds the wire object. gitBranchName uses the assignee's username,
// or the caller's when the issue is unassigned (docs/internal/decisions-T1.md).
func (r issueRow) toIssue(viewer string, truncate bool) Issue {
	desc, cut := r.Description, false
	if truncate {
		desc, cut = truncateRunes(r.Description, listDescriptionRunes)
	}
	branchUser := viewer
	if r.AssigneeUsername != nil {
		branchUser = *r.AssigneeUsername
	}
	is := Issue{
		ID: r.ID, Identifier: Identifier(r.TeamKey, r.Number), Number: r.Number,
		Title: r.Title, Description: desc, DescriptionTruncated: cut,
		Priority: r.Priority, PriorityLabel: PriorityLabel(r.Priority), Estimate: r.Estimate,
		Team:      TeamRef{ID: r.TeamID, Key: r.TeamKey, Name: r.TeamName},
		State:     StateRef{ID: r.StateID, Name: r.StateName, Type: r.StateType, Color: r.StateColor},
		Assignee:  userRef(r.AssigneeID, r.AssigneeUsername, r.AssigneeName),
		Creator:   userRef(r.CreatorID, r.CreatorUsername, r.CreatorName),
		Labels:    []LabelRef{},
		ProjectID: r.ProjectID, MilestoneID: r.MilestoneID, CycleID: r.CycleID,
		DueDate:       formatDate(r.DueDate),
		GitBranchName: GitBranchName(branchUser, r.TeamKey, r.Number, r.Title),
		Version:       r.Version, SortOrder: r.SortOrder,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, CanceledAt: r.CanceledAt, ArchivedAt: r.Archive,
	}
	if r.ParentID != nil && r.ParentTeamKey != nil && r.ParentNumber != nil {
		title := ""
		if r.ParentTitle != nil {
			title = *r.ParentTitle
		}
		is.Parent = &IssueRef{ID: *r.ParentID, Identifier: Identifier(*r.ParentTeamKey, *r.ParentNumber), Title: title}
	}
	return is
}

// attachLabels fills Labels for a batch of issues with ONE query (no N+1).
func attachLabels(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, issues []Issue) error {
	if len(issues) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(issues))
	idx := make(map[uuid.UUID]int, len(issues))
	for i, is := range issues {
		ids[i] = is.ID
		idx[is.ID] = i
	}
	rows, err := q.ListLabelsForIssues(ctx, dbq.ListLabelsForIssuesParams{OrgID: orgID, IssueIds: ids})
	if err != nil {
		return fmt.Errorf("list issue labels: %w", err)
	}
	for _, r := range rows {
		i := idx[r.IssueID]
		issues[i].Labels = append(issues[i].Labels, LabelRef{ID: r.ID, Name: r.Name, Color: r.Color})
	}
	return nil
}

// ListFilter is a parsed GET /issues query string.
type ListFilter struct {
	Assignee        string // username, "me", or "none"
	States          []string
	StateTypes      []string
	Team            string
	Product         string // product slug or id (T18 #20)
	Labels          []string
	Project         string
	Cycle           string // number, "active", or a cycle id (dev-ops or imported Linear)
	Parent          string
	Priority        *int16
	Query           string
	CreatedSince    *time.Time
	UpdatedSince    *time.Time
	OrderBy         string // createdAt | updatedAt | manual
	IncludeArchived bool
	Full            bool
	Limit           int
	Cursor          *cursor
}

var validStateTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true, "completed": true, "canceled": true}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// knownListParams guards against a misspelled filter silently matching all.
var knownListParams = map[string]bool{
	"assignee": true, "state": true, "stateType": true, "team": true, "label": true,
	"project": true, "cycle": true, "parent": true, "priority": true, "query": true,
	"createdAt": true, "updatedAt": true, "orderBy": true, "includeArchived": true,
	"full": true, "limit": true, "cursor": true, "sort": true, "product": true,
}

// ParseListFilter validates the GET /issues query string.
func ParseListFilter(v url.Values, now time.Time) (ListFilter, error) {
	for k := range v {
		if !knownListParams[k] {
			return ListFilter{}, httpx.Invalid("unknown query parameter %q", k)
		}
	}
	f := ListFilter{
		Assignee: v.Get("assignee"), States: splitList(v.Get("state")), StateTypes: splitList(v.Get("stateType")),
		Team: v.Get("team"), Product: strings.TrimSpace(v.Get("product")), Labels: splitList(v.Get("label")), Project: v.Get("project"),
		Cycle: v.Get("cycle"), Parent: v.Get("parent"), Query: strings.TrimSpace(v.Get("query")),
		OrderBy: v.Get("orderBy"), Limit: 50,
	}
	for _, st := range f.StateTypes {
		if !validStateTypes[st] {
			return f, httpx.Invalid("unknown stateType %q", st)
		}
	}
	if p := v.Get("priority"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 4 {
			return f, httpx.Invalid("priority must be 0-4")
		}
		pr := int16(n)
		f.Priority = &pr
	}
	if c := f.Cycle; c != "" && c != "active" {
		if _, err := uuid.Parse(c); err != nil {
			if n, err := strconv.Atoi(c); err != nil || n < 1 {
				return f, httpx.Invalid("cycle must be a number, \"active\" or a cycle id")
			}
		}
	}
	for name, dst := range map[string]**time.Time{"createdAt": &f.CreatedSince, "updatedAt": &f.UpdatedSince} {
		if s := v.Get(name); s != "" {
			t, err := ParseTimeFilter(s, now)
			if err != nil {
				return f, httpx.Invalid("%s: %v", name, err)
			}
			*dst = &t
		}
	}
	// sort=manual is the same as orderBy=manual: the stored board order
	// (decisions-T14). No other value of sort exists.
	if sv := v.Get("sort"); sv != "" {
		if sv != "manual" {
			return f, httpx.Invalid("sort must be manual")
		}
		if f.OrderBy != "" && f.OrderBy != "manual" {
			return f, httpx.Invalid("sort=manual and orderBy=%s disagree", f.OrderBy)
		}
		f.OrderBy = "manual"
	}
	switch f.OrderBy {
	case "":
		f.OrderBy = "updatedAt"
	case "updatedAt", "createdAt", "manual":
	default:
		return f, httpx.Invalid("orderBy must be createdAt, updatedAt or manual")
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 250 {
			return f, httpx.Invalid("limit must be 1-250")
		}
		f.Limit = n
	}
	var err error
	if f.IncludeArchived, err = parseBool(v.Get("includeArchived")); err != nil {
		return f, httpx.Invalid("includeArchived must be true or false")
	}
	if f.Full, err = parseBool(v.Get("full")); err != nil {
		return f, httpx.Invalid("full must be true or false")
	}
	if c := v.Get("cursor"); c != "" {
		cur, err := decodeCursor(c)
		if err != nil {
			return f, httpx.Invalid("malformed cursor")
		}
		if cur.OrderBy != f.OrderBy {
			return f, httpx.Invalid("cursor was made for orderBy=%s", cur.OrderBy)
		}
		f.Cursor = &cur
	}
	return f, nil
}

func parseBool(s string) (bool, error) {
	if s == "" {
		return false, nil
	}
	return strconv.ParseBool(s)
}

// likePattern escapes LIKE metacharacters so user text matches literally.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// ListIssues returns one keyset page.
func (s *Service) ListIssues(ctx context.Context, org *auth.Org, p *auth.Principal, f ListFilter) (httpx.Page[Issue], error) {
	args := []any{org.ID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"i.org_id = $1", teamScope(org, "i.team_id", arg)}
	if !f.IncludeArchived {
		where = append(where, "i.archived_at IS NULL")
	}
	switch strings.ToLower(f.Assignee) {
	case "":
	case "me":
		where = append(where, "i.assignee_id = "+arg(p.UserID))
	case "none", "null":
		where = append(where, "i.assignee_id IS NULL")
	default:
		where = append(where, "a.username = "+arg(strings.ToLower(f.Assignee)))
	}
	if len(f.States) > 0 {
		ids := []uuid.UUID{}
		names := []string{}
		for _, st := range f.States {
			if id, err := uuid.Parse(st); err == nil {
				ids = append(ids, id)
			} else {
				names = append(names, strings.ToLower(st))
			}
		}
		where = append(where, fmt.Sprintf("(i.state_id = ANY(%s::uuid[]) OR lower(s.name) = ANY(%s::text[]))", arg(ids), arg(names)))
	}
	if len(f.StateTypes) > 0 {
		where = append(where, "s.type = ANY("+arg(f.StateTypes)+"::text[])")
	}
	if f.Team != "" {
		if id, err := uuid.Parse(f.Team); err == nil {
			if id, err = teamFilterID(ctx, s.q, org, id); err != nil {
				return httpx.Page[Issue]{}, err
			}
			where = append(where, "i.team_id = "+arg(id))
		} else {
			where = append(where, "t.key = "+arg(strings.ToUpper(f.Team)))
		}
	}
	if f.Product != "" {
		// The union of ALL joined teams, archived ones included (T18 #20).
		// An unknown product (or one of another org) matches nothing (#9).
		pr, err := resolveProduct(ctx, s.q, org, f.Product)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			where = append(where, "FALSE")
		case err != nil:
			return httpx.Page[Issue]{}, fmt.Errorf("resolve product filter: %w", err)
		default:
			where = append(where, "/* scope: team */ i.team_id IN (SELECT pt.team_id FROM product_teams pt WHERE pt.org_id = $1 AND pt.product_id = "+arg(pr.ID)+")")
		}
	}
	if len(f.Labels) > 0 {
		lower := make([]string, len(f.Labels))
		for i, l := range f.Labels {
			lower[i] = strings.ToLower(l)
		}
		// A team label of a hidden team never matches (T20 #8).
		where = append(where, `/* scope: team */ EXISTS (SELECT 1 FROM issue_labels il
			JOIN labels l ON l.org_id = il.org_id AND l.id = il.label_id
			WHERE il.org_id = i.org_id AND il.issue_id = i.id AND lower(l.name) = ANY(`+arg(lower)+`::text[])
			AND (l.team_id IS NULL OR `+teamScope(org, "l.team_id", arg)+`))`)
	}
	if f.Project != "" {
		if strings.EqualFold(f.Project, "none") {
			// LAUN-179: the issues that are in no project (a project named "none" is found by its id).
			// A project the caller cannot see counts as none, so the filter
			// agrees with the redacted projectId (T20 #40).
			where = append(where, `/* scope: project */ (i.project_id IS NULL OR NOT EXISTS (SELECT 1 FROM projects pn
				WHERE pn.org_id = i.org_id AND pn.id = i.project_id AND `+projectScope(org, "pn", arg)+`))`)
		} else {
			// Only a project the caller sees matches (T20 #40): a hidden
			// project's id or name is the empty list of an unknown one, so
			// the filter cannot confirm a hidden project's name.
			var cond string
			if id, err := uuid.Parse(f.Project); err == nil {
				cond = "pr.id = " + arg(id)
			} else {
				cond = "lower(pr.name) = " + arg(strings.ToLower(f.Project))
			}
			where = append(where, `/* scope: project */ EXISTS (SELECT 1 FROM projects pr WHERE pr.org_id = i.org_id AND pr.id = i.project_id
				AND `+cond+` AND `+projectScope(org, "pr", arg)+`)`)
		}
	}
	if f.Cycle != "" {
		cond := "cy.starts_at <= now() AND cy.ends_at > now() AND cy.completed_at IS NULL"
		if id, err := uuid.Parse(f.Cycle); err == nil {
			cid, err := resolveCycleID(ctx, s.q, org, id)
			if err != nil {
				// A filter value is a reference: unknown is a 400.
				return httpx.Page[Issue]{}, invalidRef(err, "unknown cycle %q", f.Cycle)
			}
			cond = "cy.id = " + arg(cid)
		} else if f.Cycle != "active" {
			n, _ := strconv.Atoi(f.Cycle) // validated by ParseListFilter
			cond = "cy.number = " + arg(int32(n))
		}
		where = append(where, "/* scope: team */ EXISTS (SELECT 1 FROM cycles cy WHERE cy.org_id = i.org_id AND cy.id = i.cycle_id AND "+cond+
			" AND "+teamScope(org, "cy.team_id", arg)+")")
	}
	if f.Parent != "" {
		pid, err := resolveIssueID(ctx, s.q, org, f.Parent)
		if err != nil {
			// A filter value is not the request path: unknown is a 400.
			return httpx.Page[Issue]{}, asInvalid(err, "unknown parent %q", f.Parent)
		}
		where = append(where, "i.parent_id = "+arg(pid))
	}
	if f.Priority != nil {
		where = append(where, "i.priority = "+arg(*f.Priority))
	}
	matchExpr := "" // set when the query names a ticket: those rows sort first
	refKey, refNum, isRef := "", int32(0), false
	if f.Query != "" {
		cond := fmt.Sprintf("(i.title ILIKE %s OR i.description ILIKE %[1]s", arg(likePattern(f.Query)))
		if key, n, ok := ParseTicketRef(f.Query); ok {
			// LAUN-133: "LAUN-113" or "#laun-113" names one ticket; "113" and
			// "#113" name that number in every team the other filters allow
			// (a team filter is the team in scope). The ticket comes first.
			refKey, refNum, isRef = key, n, true
			if key != "" {
				matchExpr = fmt.Sprintf("(t.key = %s AND i.number = %s)", arg(key), arg(n))
			} else {
				matchExpr = fmt.Sprintf("(i.number = %s)", arg(n))
			}
			cond += " OR " + matchExpr
		}
		where = append(where, cond+")")
	}
	if f.CreatedSince != nil {
		where = append(where, "i.created_at >= "+arg(*f.CreatedSince))
	}
	if f.UpdatedSince != nil {
		where = append(where, "i.updated_at >= "+arg(*f.UpdatedSince))
	}
	col, dir, cmp := "i.updated_at", "DESC", "<"
	switch f.OrderBy {
	case "createdAt":
		col = "i.created_at"
	case "manual":
		// Ascending rank; ranks are per column, so a list over several
		// columns interleaves them by number. id breaks ties (uuidv7 is not
		// monotonic, but it is a total order, which a keyset needs).
		col, dir, cmp = "i.sort_order", "ASC", ">"
	}
	if f.Cursor != nil {
		var after string
		if f.OrderBy == "manual" {
			if f.Cursor.Rank == nil {
				return httpx.Page[Issue]{}, httpx.Invalid("malformed cursor")
			}
			after = fmt.Sprintf("(%s, i.id) > (%s::float8, %s)", col, arg(*f.Cursor.Rank), arg(f.Cursor.ID))
		} else {
			after = fmt.Sprintf("(%s, i.id) %s (%s, %s)", col, cmp, arg(f.Cursor.At), arg(f.Cursor.ID))
		}
		switch {
		case matchExpr == "":
		case f.Cursor.Match:
			// The last row was a ticket match: more matches, then every other row.
			after = fmt.Sprintf("(NOT %s OR %s)", matchExpr, after)
		default:
			after = fmt.Sprintf("(NOT %s AND %s)", matchExpr, after)
		}
		where = append(where, after)
	}
	desc := descList
	if f.Full {
		desc = descFull
	}
	sel := issueSelect(org, arg, desc)
	order := fmt.Sprintf("%s %s, i.id %[2]s", col, dir)
	if matchExpr != "" {
		order = matchExpr + " DESC, " + order
	}
	sql := sel + " WHERE " + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY %s LIMIT %s", order, arg(f.Limit+1))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.Page[Issue]{}, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()
	items := make([]Issue, 0, f.Limit+1)
	raw := make([]issueRow, 0, f.Limit+1)
	for rows.Next() {
		r, err := scanIssue(rows)
		if err != nil {
			return httpx.Page[Issue]{}, fmt.Errorf("scan issue: %w", err)
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return httpx.Page[Issue]{}, fmt.Errorf("iterate issues: %w", err)
	}
	page := httpx.Page[Issue]{}
	if len(raw) > f.Limit {
		raw = raw[:f.Limit]
		last := raw[len(raw)-1]
		cur := cursor{OrderBy: f.OrderBy, At: last.UpdatedAt, ID: last.ID}
		switch f.OrderBy {
		case "createdAt":
			cur.At = last.CreatedAt
		case "manual":
			rank := last.SortOrder
			cur.Rank = &rank
		}
		cur.Match = isRef && last.Number == refNum && (refKey == "" || last.TeamKey == refKey)
		c := encodeCursor(cur)
		page.NextCursor = &c
	}
	for _, r := range raw {
		items = append(items, r.toIssue(p.Username, !f.Full))
	}
	if err := attachLabels(ctx, s.q, org.ID, items); err != nil {
		return httpx.Page[Issue]{}, err
	}
	page.Items = items
	return page, nil
}

// Includes is the parsed ?include= list for GET /issues/{id}.
type Includes struct {
	Relations, Attachments, StateHistory, Comments, ChangeRequests bool
}

// ParseIncludes validates include=relations,attachments,stateHistory,comments.
func ParseIncludes(v string) (Includes, error) {
	var inc Includes
	for _, part := range splitList(v) {
		switch part {
		case "relations":
			inc.Relations = true
		case "attachments":
			inc.Attachments = true
		case "stateHistory":
			inc.StateHistory = true
		case "comments":
			inc.Comments = true
		case "changeRequests":
			inc.ChangeRequests = true
		default:
			return inc, httpx.Invalid("unknown include %q (want relations, attachments, stateHistory, comments, changeRequests)", part)
		}
	}
	return inc, nil
}

// GetIssue reads one issue by identifier or id, with the requested includes.
func (s *Service) GetIssue(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, inc Includes) (Issue, error) {
	id, err := resolveIssueID(ctx, s.q, org, ref)
	if err != nil {
		return Issue{}, err
	}
	return s.getIssueByID(ctx, org, p, id, inc)
}

func (s *Service) getIssueByID(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, inc Includes) (Issue, error) {
	orgID := org.ID
	args := []any{orgID, id}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	sel := issueSelect(org, arg, descFull)
	r, err := scanIssue(s.pool.QueryRow(ctx, sel+" WHERE i.org_id = $1 AND i.id = $2 AND "+teamScope(org, "i.team_id", arg), args...))
	if err != nil {
		return Issue{}, notFound(err, "issue")
	}
	items := []Issue{r.toIssue(p.Username, false)}
	if err := attachLabels(ctx, s.q, orgID, items); err != nil {
		return Issue{}, err
	}
	is := items[0]
	if inc.Relations {
		rel, err := s.relationsFor(ctx, org, id)
		if err != nil {
			return Issue{}, err
		}
		is.Relations = &rel
	}
	if inc.StateHistory {
		rows, err := s.q.ListStateChanges(ctx, dbq.ListStateChangesParams{OrgID: orgID, IssueID: id})
		if err != nil {
			return Issue{}, fmt.Errorf("list state changes: %w", err)
		}
		is.StateHistory = make([]StateChange, 0, len(rows))
		for _, r := range rows {
			is.StateHistory = append(is.StateHistory, StateChange{
				ID: r.ID, FromStateID: r.FromStateID, FromStateName: r.FromStateName,
				ToStateID: r.ToStateID, ToStateName: r.ToStateName,
				Actor: userRef(r.ActorID, r.ActorUsername, r.ActorName), Source: r.Source, CreatedAt: r.CreatedAt,
			})
		}
	}
	if inc.Comments {
		cs, err := s.listComments(ctx, s.q, orgID, id)
		if err != nil {
			return Issue{}, err
		}
		is.Comments = cs
	}
	if inc.Attachments {
		as, err := s.listAttachments(ctx, org.Slug, orgID, id)
		if err != nil {
			return Issue{}, err
		}
		is.Attachments = as
	}
	if inc.ChangeRequests {
		crs, err := s.listChangeRequests(ctx, orgID, id)
		if err != nil {
			return Issue{}, err
		}
		if len(crs) > 0 { // an issue with no MR has no key at all
			is.ChangeRequests = crs
		}
	}
	return is, nil
}
