package tracker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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

// Projects and milestones (T3). A project's teams are project_teams rows;
// its lead must be a member of the org. Lists cut content to 400
// characters unless full=true, as the issue list cuts descriptions.

func validateProjectName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if l := utf8.RuneCountInString(n); l < 1 || l > 255 {
		return "", httpx.Invalid("name must be 1-255 characters")
	}
	return n, nil
}

func validateLongText(field, v string) error {
	if utf8.RuneCountInString(v) > maxDescriptionRunes {
		return httpx.Invalid("%s must be at most %d characters", field, maxDescriptionRunes)
	}
	return nil
}

// projectTimestamps sets started/completed/canceled from the new state:
//   - started: startedAt set if empty; completedAt and canceledAt cleared.
//   - paused: startedAt kept; completedAt and canceledAt cleared.
//   - completed: completedAt set (and startedAt if empty); canceledAt cleared.
//   - canceled: canceledAt set; completedAt cleared; startedAt kept.
//   - backlog / planned: all three cleared (the work has not started).
func projectTimestamps(state string, now time.Time, started, completed, canceled *time.Time) (*time.Time, *time.Time, *time.Time) {
	n := now
	switch state {
	case "started":
		if started == nil {
			started = &n
		}
		return started, nil, nil
	case "paused":
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

// resolveMember is "me" or the username of an active org member; anything
// else is a 400 naming the field.
func resolveMember(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, p *auth.Principal, ref, field string) (uuid.UUID, error) {
	if ref == "me" {
		return p.UserID, nil
	}
	m, err := q.GetOrgMemberByUsername(ctx, dbq.GetOrgMemberByUsernameParams{OrgID: orgID, Username: strings.ToLower(ref)})
	if err != nil {
		return uuid.Nil, invalidRef(err, "%s %q is not a member of this org", field, ref)
	}
	return m.ID, nil
}

// resolveTeams turns team keys or ids into ids. Every ref must name a team
// of this org.
func resolveTeams(ctx context.Context, q *dbq.Queries, org *auth.Org, refs []string) ([]uuid.UUID, error) {
	if len(refs) > 50 {
		return nil, httpx.Invalid("at most 50 teams per project")
	}
	out := make([]uuid.UUID, 0, len(refs))
	for _, r := range refs {
		t, err := resolveTeam(ctx, q, org, strings.TrimSpace(r))
		if err != nil {
			return nil, invalidRef(err, "unknown team %q", r)
		}
		if !slices.Contains(out, t.ID) {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

// checkDateOrder refuses a target before the start.
func checkDateOrder(start, target *time.Time) error {
	if start != nil && target != nil && target.Before(*start) {
		return httpx.Invalid("targetDate is before startDate")
	}
	return nil
}

// CreateProjectInput is the POST /projects body.
type CreateProjectInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	State       string   `json:"state"`    // a status name or a kind; default planned
	StatusID    string   `json:"statusId"` // a project status id (LAUN-195); wins over state
	Lead        string   `json:"lead"`     // username or "me"
	StartDate   string   `json:"startDate"`
	TargetDate  string   `json:"targetDate"`
	Teams       []string `json:"teams"` // keys or ids
	LabelIDs    []string `json:"labelIds"`
	Emoji       string   `json:"emoji"` // one emoji as the icon (LAUN-232); empty means none
}

// CreateProject creates a project and emits project.created.
func (s *Service) CreateProject(ctx context.Context, org *auth.Org, p *auth.Principal, in CreateProjectInput) (Project, error) {
	name, err := validateProjectName(in.Name)
	if err != nil {
		return Project{}, err
	}
	if err := validateLongText("description", in.Description); err != nil {
		return Project{}, err
	}
	if err := validateLongText("content", in.Content); err != nil {
		return Project{}, err
	}
	var start, target *time.Time
	if in.StartDate != "" {
		if start, err = parseDate("startDate", in.StartDate); err != nil {
			return Project{}, err
		}
	}
	if in.TargetDate != "" {
		if target, err = parseDate("targetDate", in.TargetDate); err != nil {
			return Project{}, err
		}
	}
	if err := checkDateOrder(start, target); err != nil {
		return Project{}, err
	}
	emoji, err := normalizeProjectEmoji(in.Emoji)
	if err != nil {
		return Project{}, err
	}
	var id uuid.UUID
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var lead *uuid.UUID
		if in.Lead != "" {
			l, err := resolveMember(ctx, q, org.ID, p, in.Lead, "lead")
			if err != nil {
				return err
			}
			lead = &l
		}
		teams, err := resolveTeams(ctx, q, org, in.Teams)
		if err != nil {
			return err
		}
		// A restricted member names one of their teams, or the project would
		// be org-wide (T20 #39).
		if !org.Access.All && len(teams) == 0 {
			return httpx.Invalid("teams must name at least one team")
		}
		st, err := resolveProjectStatus(ctx, q, org.ID, in.State, in.StatusID, "planned")
		if err != nil {
			return err
		}
		labels, err := resolveProjectLabelIDs(ctx, q, org.ID, in.LabelIDs)
		if err != nil {
			return err
		}
		started, completed, canceled := projectTimestamps(st.Kind, s.now(), nil, nil, nil)
		row, err := q.InsertProject(ctx, dbq.InsertProjectParams{OrgID: org.ID, Name: name, Description: in.Description,
			Content: in.Content, State: st.Kind, StatusID: st.ID, LeadID: lead, StartDate: start, TargetDate: target,
			StartedAt: started, CompletedAt: completed, CanceledAt: canceled, Emoji: emoji})
		if err != nil {
			return fmt.Errorf("insert project: %w", err)
		}
		id = row.ID
		for _, t := range teams {
			if err := q.InsertProjectTeam(ctx, dbq.InsertProjectTeamParams{OrgID: org.ID, ProjectID: id, TeamID: t}); err != nil {
				return fmt.Errorf("insert project team: %w", err)
			}
		}
		for _, l := range labels {
			if err := q.InsertProjectLabelAssignment(ctx, dbq.InsertProjectLabelAssignmentParams{OrgID: org.ID, ProjectID: id, LabelID: l}); err != nil {
				return fmt.Errorf("insert project label: %w", err)
			}
		}
		scope, err := projectEventScope(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectCreated, EntityType: "project", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "name": name}}, scope)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return s.getProject(ctx, org, id)
}

// UpdateProjectInput is the PATCH /projects/{id} body. Absent fields are
// left alone; null clears a nullable field. teams replaces the set.
type UpdateProjectInput struct {
	Version     *int32              `json:"version"`
	Name        httpx.Opt[string]   `json:"name"`
	Description httpx.Opt[string]   `json:"description"`
	Content     httpx.Opt[string]   `json:"content"`
	State       httpx.Opt[string]   `json:"state"`    // a status name or a kind
	StatusID    httpx.Opt[string]   `json:"statusId"` // a project status id; wins over state
	Lead        httpx.Opt[string]   `json:"lead"`
	StartDate   httpx.Opt[string]   `json:"startDate"`
	TargetDate  httpx.Opt[string]   `json:"targetDate"`
	Teams       httpx.Opt[[]string] `json:"teams"`
	// UpdateSchedule changes the update reminder schedule (LAUN-196); absent keeps it.
	UpdateSchedule *UpdateScheduleInput `json:"updateSchedule"`
	LabelIDs       httpx.Opt[[]string]  `json:"labelIds"`
	// Emoji is one emoji as the icon (LAUN-232); "" or null clears it.
	Emoji httpx.Opt[string] `json:"emoji"`
}

// UpdateProject applies a partial update under the row lock, with optional
// compare-and-swap on version. A PATCH that changes nothing writes nothing.
func (s *Service) UpdateProject(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in UpdateProjectInput) (Project, error) {
	scopeAll, scopeIDs := org.Access.Args()
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.LockProject(ctx, dbq.LockProjectParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "project")
		}
		if in.Version != nil && *in.Version != cur.Version {
			return httpx.Errorf(http.StatusConflict, httpx.CodeVersionConflict,
				"project is at version %d, not %d; re-read it and retry", cur.Version, *in.Version)
		}
		before, err := projectEventScope(ctx, q, org.ID, id) // the event goes to the teams before and after
		if err != nil {
			return err
		}
		next := dbq.UpdateProjectParams{OrgID: org.ID, ID: id, Name: cur.Name, Description: cur.Description,
			Content: cur.Content, State: cur.State, StatusID: cur.StatusID, LeadID: cur.LeadID, StartDate: cur.StartDate, TargetDate: cur.TargetDate,
			StartedAt: cur.StartedAt, CompletedAt: cur.CompletedAt, CanceledAt: cur.CanceledAt, Emoji: cur.Emoji}
		var changes []string
		if in.Name.Set {
			if in.Name.Null {
				return httpx.Invalid("name cannot be null")
			}
			n, err := validateProjectName(in.Name.V)
			if err != nil {
				return err
			}
			if n != cur.Name {
				next.Name, changes = n, append(changes, "name")
			}
		}
		for _, f := range []struct {
			name string
			opt  httpx.Opt[string]
			cur  string
			dst  *string
		}{{"description", in.Description, cur.Description, &next.Description}, {"content", in.Content, cur.Content, &next.Content}} {
			if !f.opt.Set {
				continue
			}
			v := ""
			if !f.opt.Null {
				v = f.opt.V
			}
			if err := validateLongText(f.name, v); err != nil {
				return err
			}
			if v != f.cur {
				*f.dst, changes = v, append(changes, f.name)
			}
		}
		// A bare kind word that is the project's own kind changes nothing (an old client or the MCP sends the
		// kind back): the project keeps its status, custom or not (decisions-T29 #4).
		sameKind := in.StatusID.V == "" && !in.StatusID.Set && strings.EqualFold(strings.TrimSpace(in.State.V), cur.State)
		if (in.State.Set || in.StatusID.Set) && !sameKind {
			if in.State.Null || in.StatusID.Null {
				return httpx.Invalid("state and statusId cannot be null")
			}
			st, err := resolveProjectStatus(ctx, q, org.ID, in.State.V, in.StatusID.V, "")
			if err != nil {
				return err
			}
			if st.ID != cur.StatusID {
				next.State, next.StatusID = st.Kind, st.ID
				if st.Kind != cur.State {
					next.StartedAt, next.CompletedAt, next.CanceledAt =
						projectTimestamps(st.Kind, s.now(), cur.StartedAt, cur.CompletedAt, cur.CanceledAt)
				}
				changes = append(changes, "status")
			}
		}
		if in.Lead.Set {
			var lead *uuid.UUID
			if !in.Lead.Null {
				l, err := resolveMember(ctx, q, org.ID, p, in.Lead.V, "lead")
				if err != nil {
					return err
				}
				lead = &l
			}
			if !eqPtr(lead, cur.LeadID) {
				next.LeadID, changes = lead, append(changes, "lead")
			}
		}
		for _, f := range []struct {
			name string
			opt  httpx.Opt[string]
			cur  *time.Time
			dst  **time.Time
		}{{"startDate", in.StartDate, cur.StartDate, &next.StartDate}, {"targetDate", in.TargetDate, cur.TargetDate, &next.TargetDate}} {
			if !f.opt.Set {
				continue
			}
			var d *time.Time
			if !f.opt.Null {
				if d, err = parseDate(f.name, f.opt.V); err != nil {
					return err
				}
			}
			if !eqDate(d, f.cur) {
				*f.dst, changes = d, append(changes, f.name)
			}
		}
		if err := checkDateOrder(next.StartDate, next.TargetDate); err != nil {
			return err
		}
		if in.Emoji.Set {
			var e *string
			if !in.Emoji.Null {
				if e, err = normalizeProjectEmoji(in.Emoji.V); err != nil {
					return err
				}
			}
			if !eqPtr(e, cur.Emoji) {
				next.Emoji, changes = e, append(changes, "emoji")
			}
		}
		teamsChanged := false
		if in.Teams.Set {
			want := []uuid.UUID{}
			if !in.Teams.Null {
				if want, err = resolveTeams(ctx, q, org, in.Teams.V); err != nil {
					return err
				}
			}
			if teamsChanged, err = replaceProjectTeams(ctx, q, org, id, want); err != nil {
				return err
			}
			if teamsChanged {
				changes = append(changes, "teams")
			}
		}
		labelsChanged := false
		if in.LabelIDs.Set {
			want := []uuid.UUID{}
			if !in.LabelIDs.Null {
				if want, err = resolveProjectLabelIDs(ctx, q, org.ID, in.LabelIDs.V); err != nil {
					return err
				}
			}
			if labelsChanged, err = replaceProjectLabels(ctx, q, org.ID, id, want); err != nil {
				return err
			}
			if labelsChanged {
				changes = append(changes, "labels")
			}
		}
		scheduleChanged := false
		if in.UpdateSchedule != nil {
			curSched := UpdateSchedule{Frequency: cur.UpdateFrequency, Weekday: int(cur.UpdateWeekday), Hour: int(cur.UpdateHour)}
			wantSched, err := curSched.apply(*in.UpdateSchedule)
			if err != nil {
				return err
			}
			if wantSched != curSched {
				start, err := s.scheduleStart(ctx, q, org.ID, wantSched)
				if err != nil {
					return err
				}
				if err := q.SetProjectUpdateSchedule(ctx, dbq.SetProjectUpdateScheduleParams{OrgID: org.ID, ID: id,
					UpdateFrequency: wantSched.Frequency, UpdateWeekday: int16(wantSched.Weekday), UpdateHour: int16(wantSched.Hour),
					UpdateLastDueAt: start}); err != nil {
					return fmt.Errorf("set update schedule: %w", err)
				}
				scheduleChanged = true
				changes = append(changes, "updateSchedule")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		var updated dbq.Project
		// A change of the team set, the labels or the schedule alone still bumps version and updated_at.
		if onlyTouch := len(changes) == btoi(teamsChanged)+btoi(labelsChanged)+btoi(scheduleChanged); onlyTouch {
			updated, err = q.TouchProject(ctx, dbq.TouchProjectParams{OrgID: org.ID, ID: id})
		} else {
			updated, err = q.UpdateProject(ctx, next)
		}
		if err != nil {
			return fmt.Errorf("update project: %w", err)
		}
		after, err := projectEventScope(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectUpdated, EntityType: "project", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "changes": changes, "version": updated.Version}},
			eitherScope(before, after))
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return s.getProject(ctx, org, id)
}

// DeleteProject removes a project (LAUN-191, decisions-T26-delete.md). Its
// issues and documents stay with a null project_id; its milestones and team
// links go (foreign keys). The avatar object is removed after the commit.
func (s *Service) DeleteProject(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) error {
	scopeAll, scopeIDs := org.Access.Args()
	var avatarKey *string
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.LockProject(ctx, dbq.LockProjectParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "project")
		}
		// Read before the delete: the project_teams rows go with the project.
		scope, err := projectEventScope(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		if err := q.DeleteProject(ctx, dbq.DeleteProjectParams{OrgID: org.ID, ID: id}); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		avatarKey = cur.AvatarKey
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectDeleted, EntityType: "project", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "name": cur.Name}}, scope) // no issue count: it would count hidden teams' issues (T20 #37)
		return err
	})
	if err != nil {
		return err
	}
	s.deleteOldAvatar(ctx, avatarOwner{org, &id}, avatarKey)
	return nil
}

// replaceProjectTeams makes the teams org sees match want (T20 #39). want
// holds visible teams only (resolveTeams is scoped). The rows of teams org
// cannot see are kept. A restricted member cannot set the teams of a project
// with no team (it is org-wide: only a caller who sees every team may), and
// cannot leave none of their own teams on it (it would vanish from them).
func replaceProjectTeams(ctx context.Context, q *dbq.Queries, org *auth.Org, projectID uuid.UUID, want []uuid.UUID) (bool, error) {
	orgID := org.ID
	have, err := q.ListProjectTeamRows(ctx, dbq.ListProjectTeamRowsParams{OrgID: orgID, ProjectID: projectID})
	if err != nil {
		return false, fmt.Errorf("project teams: %w", err)
	}
	if !org.Access.All {
		if len(have) == 0 {
			if len(want) == 0 {
				return false, nil
			}
			return false, httpx.Invalid("the teams of a project with no team can be set only by a member who sees every team")
		}
		if len(want) == 0 {
			return false, httpx.Invalid("teams must keep at least one of your teams")
		}
		// Only the visible rows take part in the replace.
		visible := have[:0:0]
		for _, h := range have {
			if org.Access.Can(h) {
				visible = append(visible, h)
			}
		}
		have = visible
	}
	changed := false
	for _, w := range want {
		if !slices.Contains(have, w) {
			if err := q.InsertProjectTeam(ctx, dbq.InsertProjectTeamParams{OrgID: orgID, ProjectID: projectID, TeamID: w}); err != nil {
				return false, fmt.Errorf("add project team: %w", err)
			}
			changed = true
		}
	}
	for _, h := range have {
		if !slices.Contains(want, h) {
			if err := q.DeleteProjectTeam(ctx, dbq.DeleteProjectTeamParams{OrgID: orgID, ProjectID: projectID, TeamID: h}); err != nil {
				return false, fmt.Errorf("remove project team: %w", err)
			}
			changed = true
		}
	}
	return changed, nil
}

const projectSelectTmpl = `/* scope: project */
SELECT p.id, p.name, p.description, %s, p.state, ps.id, ps.name, ps.kind, ps.color, p.lead_id, u.username, u.name, p.start_date, p.target_date,
       p.version, p.created_at, p.updated_at, p.started_at, p.completed_at, p.canceled_at, p.archived_at,
       p.avatar_version, (p.avatar_key IS NOT NULL), p.update_frequency, p.update_weekday, p.update_hour, p.emoji
FROM projects p
LEFT JOIN users u ON u.id = p.lead_id
JOIN project_statuses ps ON ps.org_id = p.org_id AND ps.id = p.status_id
`

var (
	projectSelectFull = fmt.Sprintf(projectSelectTmpl, "p.content")
	projectSelectList = fmt.Sprintf(projectSelectTmpl, fmt.Sprintf("left(p.content, %d)", listDescriptionRunes+1))
)

func scanProject(row pgx.Row, truncate bool) (Project, error) {
	var pr Project
	var leadID *uuid.UUID
	var leadUser, leadName *string
	var start, target *time.Time
	err := row.Scan(&pr.ID, &pr.Name, &pr.Description, &pr.Content, &pr.State, &pr.Status.ID, &pr.Status.Name, &pr.Status.Kind, &pr.Status.Color, &leadID, &leadUser, &leadName,
		&start, &target, &pr.Version, &pr.CreatedAt, &pr.UpdatedAt, &pr.StartedAt, &pr.CompletedAt, &pr.CanceledAt, &pr.ArchivedAt,
		&pr.avatarVersion, &pr.hasAvatar, &pr.UpdateSchedule.Frequency, &pr.UpdateSchedule.Weekday, &pr.UpdateSchedule.Hour, &pr.Emoji)
	if err != nil {
		return pr, err
	}
	if truncate {
		pr.Content, pr.ContentTruncated = truncateRunes(pr.Content, listDescriptionRunes)
	}
	pr.Lead = userRef(leadID, leadUser, leadName)
	pr.StartDate, pr.TargetDate = formatDate(start), formatDate(target)
	pr.Teams = []TeamRef{}
	return pr, nil
}

// attachProjectTeams fills Teams for a batch with ONE query (no N+1): the
// teams org sees only (T20 #37).
func attachProjectTeams(ctx context.Context, q *dbq.Queries, org *auth.Org, ps []Project) error {
	if len(ps) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ps))
	idx := make(map[uuid.UUID]int, len(ps))
	for i, pr := range ps {
		ids[i], idx[pr.ID] = pr.ID, i
	}
	all, tids := org.Access.Args()
	rows, err := q.ListTeamsForProjects(ctx, dbq.ListTeamsForProjectsParams{OrgID: org.ID, ProjectIds: ids, AllTeams: all, TeamIds: tids})
	if err != nil {
		return fmt.Errorf("list project teams: %w", err)
	}
	for _, r := range rows {
		i := idx[r.ProjectID]
		ps[i].Teams = append(ps[i].Teams, TeamRef{ID: r.ID, Key: r.Key, Name: r.Name})
	}
	return nil
}

// attachProjectCounts fills IssueCounts for a batch with ONE grouped query
// (LAUN-170). Only issues of teams org sees count (T20 #37).
func attachProjectCounts(ctx context.Context, q *dbq.Queries, org *auth.Org, ps []Project) error {
	if len(ps) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ps))
	idx := make(map[uuid.UUID]int, len(ps))
	for i, pr := range ps {
		ids[i], idx[pr.ID] = pr.ID, i
	}
	all, tids := org.Access.Args()
	rows, err := q.ProjectIssueCounts(ctx, dbq.ProjectIssueCountsParams{OrgID: org.ID, ProjectIds: ids, AllTeams: all, TeamIds: tids})
	if err != nil {
		return fmt.Errorf("project issue counts: %w", err)
	}
	for _, r := range rows {
		if r.ProjectID == nil {
			continue
		}
		i, ok := idx[*r.ProjectID]
		if !ok {
			continue
		}
		ps[i].IssueCounts = IssueCounts{Total: r.Total, Scope: r.Total - r.Canceled, Done: r.Done,
			Started: r.Started, Canceled: r.Canceled}
	}
	return nil
}

func (s *Service) getProject(ctx context.Context, org *auth.Org, id uuid.UUID) (Project, error) {
	orgID := org.ID
	args := []any{orgID, id}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	pr, err := scanProject(s.pool.QueryRow(ctx, projectSelectFull+" WHERE p.org_id = $1 AND p.id = $2 AND "+projectScope(org, "p", arg), args...), false)
	if err != nil {
		return Project{}, notFound(err, "project")
	}
	items := []Project{pr}
	if err := attachProjectTeams(ctx, s.q, org, items); err != nil {
		return Project{}, err
	}
	if err := attachProjectLabels(ctx, s.q, orgID, items); err != nil {
		return Project{}, err
	}
	if err := attachProjectCounts(ctx, s.q, org, items); err != nil {
		return Project{}, err
	}
	s.setProjectAvatarURLs(org.Slug, items)
	return items[0], nil
}

// GetProject reads one project.
func (s *Service) GetProject(ctx context.Context, org *auth.Org, id uuid.UUID) (Project, error) {
	return s.getProject(ctx, org, id)
}

// ProjectFilter is a parsed GET /projects query.
type ProjectFilter struct {
	listQuery
	States []string
	Lead   string // username, "me" or "none"
	Team   string // key or id
	Label  string // a project label id or name; a group matches its labels too
	Query  string
	Full   bool
}

// ParseProjectFilter validates the GET /projects query string.
func ParseProjectFilter(v url.Values) (ProjectFilter, error) {
	lq, err := parseListQuery(v, "projects", "state", "lead", "team", "label", "query", "full")
	if err != nil {
		return ProjectFilter{}, err
	}
	f := ProjectFilter{listQuery: lq, States: splitList(v.Get("state")), Lead: v.Get("lead"),
		Team: strings.TrimSpace(v.Get("team")), Label: strings.TrimSpace(v.Get("label")), Query: strings.TrimSpace(v.Get("query"))}
	if f.Full, err = parseBool(v.Get("full")); err != nil {
		return f, httpx.Invalid("full must be true or false")
	}
	return f, nil
}

// resolveStateFilter splits a GET /projects `state` list into kinds and status ids. An unknown item is a 400.
func (s *Service) resolveStateFilter(ctx context.Context, orgID uuid.UUID, items []string) ([]string, []uuid.UUID, error) {
	kinds, ids := []string{}, []uuid.UUID{}
	for _, it := range items {
		if low := strings.ToLower(it); validProjectState(low) {
			kinds = append(kinds, low)
			continue
		}
		if id, err := uuid.Parse(it); err == nil {
			st, err := s.q.GetProjectStatus(ctx, dbq.GetProjectStatusParams{OrgID: orgID, ID: id})
			if err != nil {
				return nil, nil, invalidRef(err, "unknown project status %q", it)
			}
			ids = append(ids, st.ID)
			continue
		}
		st, err := s.q.GetProjectStatusByName(ctx, dbq.GetProjectStatusByNameParams{OrgID: orgID, Name: it})
		if err != nil {
			return nil, nil, invalidRef(err, "state %q is not a kind (%s) or a project status of this org", it, strings.Join(projectStates, ", "))
		}
		ids = append(ids, st.ID)
	}
	return kinds, ids, nil
}

// ListProjects returns one keyset page, most recently updated first.
func (s *Service) ListProjects(ctx context.Context, org *auth.Org, p *auth.Principal, f ProjectFilter) (httpx.Page[Project], error) {
	args := []any{org.ID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"p.org_id = $1", projectScope(org, "p", arg)}
	if !f.IncludeArchived {
		where = append(where, "p.archived_at IS NULL")
	}
	if len(f.States) > 0 {
		// Each item is a kind, a status name or a status id (LAUN-195); a kind matches every status of it.
		kinds, ids, err := s.resolveStateFilter(ctx, org.ID, f.States)
		if err != nil {
			return httpx.Page[Project]{}, err
		}
		where = append(where, "(p.state = ANY("+arg(kinds)+"::text[]) OR p.status_id = ANY("+arg(ids)+"::uuid[]))")
	}
	switch strings.ToLower(f.Lead) {
	case "":
	case "me":
		where = append(where, "p.lead_id = "+arg(p.UserID))
	case "none", "null":
		where = append(where, "p.lead_id IS NULL")
	default:
		where = append(where, "u.username = "+arg(strings.ToLower(f.Lead)))
	}
	if f.Team != "" {
		// Each branch adds its own argument: an argument added and then not
		// used is a parameter Postgres cannot type (that was a 500 for
		// ?team=<uuid> before T4b).
		var cond string
		if id, err := uuid.Parse(f.Team); err == nil {
			if id, err = teamFilterID(ctx, s.q, org, id); err != nil {
				return httpx.Page[Project]{}, err
			}
			cond = "t.id = " + arg(id)
		} else {
			cond = "t.key = " + arg(strings.ToUpper(f.Team))
		}
		// Only a team row the caller sees matches: ?team=<hidden> must not
		// find a visible project through its hidden team (T20 #9, #37).
		where = append(where, `/* scope: project */ EXISTS (SELECT 1 FROM project_teams pt JOIN teams t ON t.org_id = pt.org_id AND t.id = pt.team_id
			WHERE pt.org_id = p.org_id AND pt.project_id = p.id AND `+cond+` AND `+teamScope(org, "pt.team_id", arg)+`)`)
	}
	if f.Label != "" {
		// A label's id or its name (ignoring case). A group also matches the
		// labels in it. An unknown label matches no project.
		var cond string
		if id, err := uuid.Parse(f.Label); err == nil {
			ph := arg(id)
			cond = "(l.id = " + ph + " OR l.parent_id = " + ph + ")"
		} else {
			ph := arg(strings.ToLower(f.Label))
			cond = "(lower(l.name) = " + ph + " /* scope: project */ OR l.parent_id IN (SELECT g.id FROM project_labels g WHERE g.org_id = p.org_id AND lower(g.name) = " + ph + "))"
		}
		where = append(where, `/* scope: project */ EXISTS (SELECT 1 FROM project_label_assignments pa JOIN project_labels l ON l.org_id = pa.org_id AND l.id = pa.label_id
			WHERE pa.org_id = p.org_id AND pa.project_id = p.id AND `+cond+`)`)
	}
	if f.Query != "" {
		where = append(where, "p.name ILIKE "+arg(likePattern(f.Query)))
	}
	if f.Cursor != nil {
		where = append(where, fmt.Sprintf("(p.updated_at, p.id) < (%s, %s)", arg(f.Cursor.At), arg(f.Cursor.ID)))
	}
	sel := projectSelectList
	if f.Full {
		sel = projectSelectFull
	}
	sql := sel + " WHERE " + strings.Join(where, " AND ") + " ORDER BY p.updated_at DESC, p.id DESC LIMIT " + arg(f.Limit+1)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.Page[Project]{}, fmt.Errorf("list projects: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) { return scanProject(r, !f.Full) })
	if err != nil {
		return httpx.Page[Project]{}, fmt.Errorf("scan projects: %w", err)
	}
	page := pageOf(items, f.Limit, "projects", func(pr Project) (time.Time, uuid.UUID) { return pr.UpdatedAt, pr.ID })
	if err := attachProjectTeams(ctx, s.q, org, page.Items); err != nil {
		return httpx.Page[Project]{}, err
	}
	if err := attachProjectLabels(ctx, s.q, org.ID, page.Items); err != nil {
		return httpx.Page[Project]{}, err
	}
	if err := attachProjectCounts(ctx, s.q, org, page.Items); err != nil {
		return httpx.Page[Project]{}, err
	}
	s.setProjectAvatarURLs(org.Slug, page.Items)
	return page, nil
}

// --- milestones ----------------------------------------------------------------

func toMilestone(m dbq.Milestone) Milestone {
	return Milestone{ID: m.ID, ProjectID: m.ProjectID, Name: m.Name, Description: m.Description,
		TargetDate: formatDate(m.TargetDate), SortOrder: m.SortOrder, CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt, ArchivedAt: m.ArchivedAt}
}

// CreateMilestoneInput is the POST /milestones body.
type CreateMilestoneInput struct {
	ProjectID   *uuid.UUID `json:"projectId"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	TargetDate  string     `json:"targetDate"`
	SortOrder   *float64   `json:"sortOrder"`
}

// CreateMilestone adds a milestone to a project of this org.
func (s *Service) CreateMilestone(ctx context.Context, org *auth.Org, p *auth.Principal, in CreateMilestoneInput) (Milestone, error) {
	scopeAll, scopeIDs := org.Access.Args()
	if in.ProjectID == nil {
		return Milestone{}, httpx.Invalid("projectId is required")
	}
	name, err := validateProjectName(in.Name)
	if err != nil {
		return Milestone{}, err
	}
	if err := validateLongText("description", in.Description); err != nil {
		return Milestone{}, err
	}
	var target *time.Time
	if in.TargetDate != "" {
		if target, err = parseDate("targetDate", in.TargetDate); err != nil {
			return Milestone{}, err
		}
	}
	order := 0.0
	if in.SortOrder != nil {
		order = *in.SortOrder
	}
	var out dbq.Milestone
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: *in.ProjectID, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
			return invalidRef(err, "unknown projectId")
		}
		var err error
		if out, err = q.InsertMilestone(ctx, dbq.InsertMilestoneParams{OrgID: org.ID, ProjectID: *in.ProjectID, Name: name,
			Description: in.Description, TargetDate: target, SortOrder: order}); err != nil {
			return fmt.Errorf("insert milestone: %w", err)
		}
		scope, err := projectEventScope(ctx, q, org.ID, out.ProjectID)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.MilestoneCreated, EntityType: "milestone", EntityID: out.ID,
			ActorID: &p.UserID, Payload: map[string]any{"id": out.ID, "projectId": out.ProjectID, "name": name}}, scope)
		return err
	})
	if err != nil {
		return Milestone{}, err
	}
	return toMilestone(out), nil
}

// UpdateMilestoneInput is the PATCH /milestones/{id} body. A milestone
// cannot move to another project (its issues are in the old one).
type UpdateMilestoneInput struct {
	Name        httpx.Opt[string]  `json:"name"`
	Description httpx.Opt[string]  `json:"description"`
	TargetDate  httpx.Opt[string]  `json:"targetDate"`
	SortOrder   httpx.Opt[float64] `json:"sortOrder"`
}

// UpdateMilestone applies a partial update under the row lock.
func (s *Service) UpdateMilestone(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in UpdateMilestoneInput) (Milestone, error) {
	scopeAll, scopeIDs := org.Access.Args()
	var out dbq.Milestone
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.LockMilestone(ctx, dbq.LockMilestoneParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "milestone")
		}
		out = cur
		next := dbq.UpdateMilestoneParams{OrgID: org.ID, ID: id, Name: cur.Name, Description: cur.Description,
			TargetDate: cur.TargetDate, SortOrder: cur.SortOrder}
		var changes []string
		if in.Name.Set {
			if in.Name.Null {
				return httpx.Invalid("name cannot be null")
			}
			n, err := validateProjectName(in.Name.V)
			if err != nil {
				return err
			}
			if n != cur.Name {
				next.Name, changes = n, append(changes, "name")
			}
		}
		if in.Description.Set {
			d := ""
			if !in.Description.Null {
				d = in.Description.V
			}
			if err := validateLongText("description", d); err != nil {
				return err
			}
			if d != cur.Description {
				next.Description, changes = d, append(changes, "description")
			}
		}
		if in.TargetDate.Set {
			var d *time.Time
			if !in.TargetDate.Null {
				if d, err = parseDate("targetDate", in.TargetDate.V); err != nil {
					return err
				}
			}
			if !eqDate(d, cur.TargetDate) {
				next.TargetDate, changes = d, append(changes, "targetDate")
			}
		}
		if in.SortOrder.Set {
			if in.SortOrder.Null {
				return httpx.Invalid("sortOrder cannot be null")
			}
			if in.SortOrder.V != cur.SortOrder {
				next.SortOrder, changes = in.SortOrder.V, append(changes, "sortOrder")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		if out, err = q.UpdateMilestone(ctx, next); err != nil {
			return fmt.Errorf("update milestone: %w", err)
		}
		scope, err := projectEventScope(ctx, q, org.ID, cur.ProjectID)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.MilestoneUpdated, EntityType: "milestone", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "projectId": cur.ProjectID, "changes": changes}}, scope)
		return err
	})
	if err != nil {
		return Milestone{}, err
	}
	return toMilestone(out), nil
}

// GetMilestone reads one milestone.
func (s *Service) GetMilestone(ctx context.Context, org *auth.Org, id uuid.UUID) (Milestone, error) {
	scopeAll, scopeIDs := org.Access.Args()
	m, err := s.q.GetMilestone(ctx, dbq.GetMilestoneParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return Milestone{}, notFound(err, "milestone")
	}
	return toMilestone(m), nil
}

// MilestoneFilter is a parsed GET /milestones query.
type MilestoneFilter struct {
	listQuery
	Project *uuid.UUID
}

// ParseMilestoneFilter validates the GET /milestones query string.
func ParseMilestoneFilter(v url.Values) (MilestoneFilter, error) {
	lq, err := parseListQuery(v, "milestones", "project")
	if err != nil {
		return MilestoneFilter{}, err
	}
	pid, err := parseUUIDParam(v, "project")
	return MilestoneFilter{listQuery: lq, Project: pid}, err
}

// ListMilestones returns one keyset page, most recently updated first. A
// project filter naming no project of this org is a 400.
func (s *Service) ListMilestones(ctx context.Context, org *auth.Org, f MilestoneFilter) (httpx.Page[Milestone], error) {
	scopeAll, scopeIDs := org.Access.Args()
	args := []any{org.ID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"org_id = $1", "/* scope: project */ EXISTS (SELECT 1 FROM projects mp WHERE mp.org_id = milestones.org_id AND mp.id = milestones.project_id AND " + projectScope(org, "mp", arg) + ")"}
	if !f.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	if f.Project != nil {
		if _, err := s.q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: *f.Project, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
			return httpx.Page[Milestone]{}, invalidRef(err, "unknown project")
		}
		where = append(where, "project_id = "+arg(*f.Project))
	}
	if f.Cursor != nil {
		where = append(where, fmt.Sprintf("(updated_at, id) < (%s, %s)", arg(f.Cursor.At), arg(f.Cursor.ID)))
	}
	sql := `/* scope: project */ SELECT id, org_id, project_id, name, description, target_date, sort_order, created_at, updated_at, archived_at
		FROM milestones WHERE ` + strings.Join(where, " AND ") + " ORDER BY updated_at DESC, id DESC LIMIT " + arg(f.Limit+1)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.Page[Milestone]{}, fmt.Errorf("list milestones: %w", err)
	}
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dbq.Milestone])
	if err != nil {
		return httpx.Page[Milestone]{}, fmt.Errorf("scan milestones: %w", err)
	}
	items := make([]Milestone, len(ms))
	for i, m := range ms {
		items[i] = toMilestone(m)
	}
	return pageOf(items, f.Limit, "milestones", func(m Milestone) (time.Time, uuid.UUID) { return m.UpdatedAt, m.ID }), nil
}

// parseDate reads a YYYY-MM-DD body field.
func parseDate(field, s string) (*time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, httpx.Invalid("%s must be YYYY-MM-DD", field)
	}
	return &t, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
