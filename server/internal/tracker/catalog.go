package tracker

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// DefaultState is one of the nine states every new team gets. The types are
// Linear's: Duplicate is a canceled-type state, as in Linear.
type DefaultState struct {
	Name, Type, Color string
}

// DefaultStates is the default workflow, in board order.
var DefaultStates = []DefaultState{
	{"Triage", "triage", "#bec2c8"},
	{"Backlog", "backlog", "#bec2c8"},
	{"Todo", "unstarted", "#e2e2e2"},
	{"In Progress", "started", "#f2c94c"},
	{"In Review", "started", "#0f783c"},
	{"In Testing", "started", "#26b5ce"},
	{"Done", "completed", "#5e6ad2"},
	{"Canceled", "canceled", "#95a2b3"},
	{"Duplicate", "canceled", "#95a2b3"},
}

// Anchor states: new issues start in Backlog, GitLab (T5) moves an opened MR
// to In Review and a merged MR to In Testing.
const (
	defaultStateName = "Backlog"
	reviewStateName  = "In Review"
	testingStateName = "In Testing"
)

var teamKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// orgSlugRe is the orgs_slug_format CHECK of 0001, checked before the insert
// so a bad slug is a 400 and not a constraint error.
var orgSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`)

// ValidateTeam trims and checks a team key and name: the rules of POST
// /orgs/{org}/teams, shared with first-run setup.
func ValidateTeam(key, name string) (string, string, error) {
	key = strings.TrimSpace(key)
	if !teamKeyRe.MatchString(key) {
		return "", "", httpx.Invalid("key must be 1-10 characters, A-Z and 0-9, starting with a letter")
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return "", "", httpx.Invalid("name must be 1-200 characters")
	}
	return key, name, nil
}

// ValidateOrg trims and checks an org slug and name (the orgs CHECKs).
func ValidateOrg(slug, name string) (string, string, error) {
	slug = strings.TrimSpace(slug)
	if !orgSlugRe.MatchString(slug) {
		return "", "", httpx.Invalid("org slug must be 2-40 characters, a-z, 0-9 and -, starting and ending with a letter or digit")
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return "", "", httpx.Invalid("org name must be 1-200 characters")
	}
	return slug, name, nil
}

// DefaultLabels are the labels every new org gets (first-run setup; the
// importer adopts them by name).
var DefaultLabels = []struct{ Name, Color string }{
	{"Bug", "#eb5757"},
	{"Improvement", "#4ea7fc"},
	{"Feature", "#bb87fc"},
}

// InsertDefaultLabelsTx adds DefaultLabels to org inside the caller's
// transaction. A label of that name that exists already is kept.
func InsertDefaultLabelsTx(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) error {
	for _, l := range DefaultLabels {
		if err := q.InsertLabelIfAbsent(ctx, dbq.InsertLabelIfAbsentParams{OrgID: orgID, Name: l.Name, Color: l.Color}); err != nil {
			return fmt.Errorf("insert label %s: %w", l.Name, err)
		}
	}
	return nil
}

// CreateTeamTx creates a team with the default workflow, its anchor states
// and its issue counter, inside the caller's transaction. First-run setup
// uses it too, so the first team and an API-made team are identical.
func CreateTeamTx(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, key, name, description string) (dbq.Team, error) {
	team, err := q.InsertTeam(ctx, dbq.InsertTeamParams{OrgID: orgID, Key: key, Name: name, Description: description})
	if err != nil {
		if database.IsPgCode(err, database.CodeUniqueViolation) {
			return team, conflict("team key %s is already used in this org", key)
		}
		return team, fmt.Errorf("insert team: %w", err)
	}
	anchors := map[string]*uuid.UUID{}
	for i, st := range DefaultStates {
		row, err := q.InsertWorkflowState(ctx, dbq.InsertWorkflowStateParams{
			OrgID: orgID, TeamID: team.ID, Name: st.Name, Type: st.Type, Color: st.Color, Position: float64(i),
		})
		if err != nil {
			return team, fmt.Errorf("insert state %s: %w", st.Name, err)
		}
		id := row.ID
		anchors[st.Name] = &id
	}
	if err := q.SetTeamAnchorStates(ctx, dbq.SetTeamAnchorStatesParams{
		OrgID: orgID, ID: team.ID, DefaultStateID: anchors[defaultStateName],
		ReviewStateID: anchors[reviewStateName], TestingStateID: anchors[testingStateName],
	}); err != nil {
		return team, fmt.Errorf("set anchor states: %w", err)
	}
	if err := q.InsertIssueCounter(ctx, dbq.InsertIssueCounterParams{TeamID: team.ID, OrgID: orgID}); err != nil {
		return team, fmt.Errorf("insert issue counter: %w", err)
	}
	return q.GetTeamByIDSystem(ctx, dbq.GetTeamByIDSystemParams{OrgID: orgID, ID: team.ID})
}

func toTeam(t dbq.Team) Team {
	return Team{ID: t.ID, Key: t.Key, Name: t.Name, Description: t.Description,
		DefaultStateID: t.DefaultStateID, ReviewStateID: t.ReviewStateID, TestingStateID: t.TestingStateID,
		CyclesEnabled: t.CyclesEnabled, CycleLengthDays: t.CycleLengthDays, CycleStartWeekday: t.CycleStartWeekday,
		CycleStartTime: formatClock(t.CycleStartTime), CycleTimezone: t.CycleTimezone, CycleRollover: t.CycleRollover,
		ProductIDs: []uuid.UUID{}, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

// ListTeams lists the org's teams that the caller sees (T20 #9).
func (s *Service) ListTeams(ctx context.Context, org *auth.Org) ([]Team, error) {
	all, ids := org.Access.Args()
	rows, err := s.q.ListTeams(ctx, dbq.ListTeamsParams{OrgID: org.ID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	out := make([]Team, 0, len(rows))
	for _, t := range rows {
		out = append(out, toTeam(t))
	}
	if err := attachProductIDs(ctx, s.q, org.ID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTeam reads a team by key or id.
func (s *Service) GetTeam(ctx context.Context, org *auth.Org, ref string) (Team, error) {
	t, err := resolveTeam(ctx, s.q, org, ref)
	if err != nil {
		return Team{}, notFound(err, "team")
	}
	out := []Team{toTeam(t)}
	if err := attachProductIDs(ctx, s.q, org.ID, out); err != nil {
		return Team{}, err
	}
	return out[0], nil
}

// TeamInput is the POST /teams body.
type TeamInput struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CreateTeam makes a team with the default workflow and emits team.created.
func (s *Service) CreateTeam(ctx context.Context, org *auth.Org, p *auth.Principal, in TeamInput) (Team, error) {
	key, name, err := ValidateTeam(in.Key, in.Name)
	if err != nil {
		return Team{}, err
	}
	var team dbq.Team
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		if team, err = CreateTeamTx(ctx, q, org.ID, key, name, in.Description); err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.TeamCreated, EntityType: "team", EntityID: team.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": team.ID, "key": team.Key},
		}, events.ScopeTeams(team.ID))
		return err
	})
	if err != nil {
		return Team{}, err
	}
	return toTeam(team), nil
}

// ListStates lists a team's workflow states in board order.
func (s *Service) ListStates(ctx context.Context, org *auth.Org, teamRef string) ([]WorkflowState, error) {
	t, err := resolveTeam(ctx, s.q, org, teamRef)
	if err != nil {
		return nil, notFound(err, "team")
	}
	rows, err := s.q.ListTeamStates(ctx, dbq.ListTeamStatesParams{OrgID: org.ID, TeamID: t.ID})
	if err != nil {
		return nil, fmt.Errorf("list states: %w", err)
	}
	out := make([]WorkflowState, 0, len(rows))
	for _, r := range rows {
		out = append(out, WorkflowState{ID: r.ID, Name: r.Name, Type: r.Type, Color: r.Color,
			Description: r.Description, Position: r.Position, TeamID: r.TeamID})
	}
	return out, nil
}

func toLabel(l dbq.Label, teamKey *string) Label {
	return Label{ID: l.ID, Name: l.Name, Color: l.Color, Description: l.Description, ParentID: l.ParentID,
		TeamID: l.TeamID, TeamKey: teamKey, CreatedAt: l.CreatedAt}
}

// ListLabels lists the org's labels with their issue counts. teamRef (a team
// key or id, or empty) narrows it: the org labels plus that team's labels,
// counting only that team's issues. An unknown team is 404.
//
// org.Access limits the team labels listed and the issues counted to the
// teams the caller sees (decisions-T20 #8, #37); an empty set sees no team.
func (s *Service) ListLabels(ctx context.Context, org *auth.Org, teamRef string) ([]Label, error) {
	return s.listLabels(ctx, org, teamRef)
}

func (s *Service) listLabels(ctx context.Context, org *auth.Org, teamRef string) ([]Label, error) {
	all, ids := org.Access.Args()
	arg := dbq.ListLabelsWithCountsParams{OrgID: org.ID, AllTeams: all, TeamIds: ids}
	if teamRef = strings.TrimSpace(teamRef); teamRef != "" {
		t, err := resolveTeam(ctx, s.q, org, teamRef)
		if err != nil {
			return nil, notFound(err, "team")
		}
		arg.TeamID = &t.ID
	}
	rows, err := s.q.ListLabelsWithCounts(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	out := make([]Label, 0, len(rows))
	for _, r := range rows {
		l := toLabel(dbq.Label{ID: r.ID, OrgID: r.OrgID, Name: r.Name, Color: r.Color, Description: r.Description,
			ParentID: r.ParentID, CreatedAt: r.CreatedAt, ArchivedAt: r.ArchivedAt, TeamID: r.TeamID}, r.TeamKey)
		l.IssueCount = r.IssueCount
		out = append(out, l)
	}
	return out, nil
}

// LabelInput is the POST /labels body. Team (a key or id) makes a team label.
type LabelInput struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Team        string `json:"team"`
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// CreateLabel adds a label. Names are unique ignoring case within a scope:
// among the org labels, and among one team's labels. A team label may share
// a name with an org label (decisions-T21-team-labels #3).
func (s *Service) CreateLabel(ctx context.Context, org *auth.Org, p *auth.Principal, in LabelInput) (Label, error) {
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return Label{}, httpx.Invalid("name must be 1-100 characters")
	}
	if _, err := uuid.Parse(name); err == nil {
		return Label{}, httpx.Invalid("a label name cannot be a UUID")
	}
	if strings.Contains(name, ",") {
		return Label{}, httpx.Invalid("a label name cannot contain a comma")
	}
	color := in.Color
	if color == "" {
		color = "#95a2b3"
	}
	if !colorRe.MatchString(color) {
		return Label{}, httpx.Invalid("color must be #RRGGBB")
	}
	var l dbq.Label
	var teamKey *string
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var teamID *uuid.UUID
		if ref := strings.TrimSpace(in.Team); ref != "" {
			t, err := resolveTeam(ctx, q, org, ref)
			if err != nil {
				return invalidRef(err, "unknown team %q", ref)
			}
			teamID, teamKey = &t.ID, &t.Key
		}
		var err error
		l, err = q.InsertLabel(ctx, dbq.InsertLabelParams{OrgID: org.ID, Name: name, Color: color, Description: in.Description, TeamID: teamID})
		if err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("a label named %q already exists", name)
			}
			return fmt.Errorf("insert label: %w", err)
		}
		payload := map[string]any{"id": l.ID, "name": l.Name}
		scope := events.ScopeOrg() // an org label is org-wide (#8)
		if teamID != nil {
			payload["teamId"] = *teamID
			scope = events.ScopeTeams(*teamID) // a team label follows its team
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.LabelCreated, EntityType: "label", EntityID: l.ID, ActorID: &p.UserID,
			Payload: payload,
		}, scope)
		return err
	})
	if err != nil {
		return Label{}, err
	}
	return toLabel(l, teamKey), nil
}

// ListMembers lists the org's members.
//
// Emails are login identifiers (T6a), so only an org admin sees them; any
// other caller sees its own and null for everyone else (decisions-T6a #20,
// closing T1 decision 35).
func (s *Service) ListMembers(ctx context.Context, org *auth.Org, p *auth.Principal) ([]Member, error) {
	rows, err := s.q.ListOrgMembers(ctx, org.ID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	// Team access is admin data (decisions-T20 #33): a restricted member never
	// learns who else is restricted. The team rows are read for an admin only.
	isAdmin := org.Role == "admin"
	teamKeys := map[uuid.UUID][]string{}
	if isAdmin {
		keys, err := s.q.ListOrgMemberTeamKeys(ctx, org.ID)
		if err != nil {
			return nil, fmt.Errorf("list member teams: %w", err)
		}
		for _, k := range keys {
			teamKeys[k.UserID] = append(teamKeys[k.UserID], k.Key)
		}
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		email := r.Email
		if !isAdmin && r.ID != p.UserID {
			email = nil
		}
		m := Member{ID: r.ID, Username: r.Username, Name: r.Name, Email: email,
			IsAgent: r.IsAgent, Role: r.Role, CreatedAt: r.CreatedAt, DeactivatedAt: r.DeactivatedAt}
		if isAdmin {
			m.TeamAccess, m.Teams = memberAccess(r.Role, r.TeamAccess, teamKeys[r.ID])
		}
		out = append(out, m)
	}
	return out, nil
}

// memberAccess is the access an admin sees for one member (#33): an org admin
// is "all" whatever the flag (#2); "all" has no team keys; any other flag is
// "selected" with its keys (fail closed, as auth.NewTeamAccess).
func memberAccess(role, flag string, keys []string) (*string, []string) {
	if role == "admin" || flag == TeamAccessAll {
		a := TeamAccessAll
		return &a, []string{}
	}
	a := TeamAccessSelected
	if keys == nil {
		keys = []string{}
	}
	return &a, keys
}

// ListOrgs lists the orgs the caller belongs to. A pinned token sees only
// its org.
func (s *Service) ListOrgs(ctx context.Context, p *auth.Principal) ([]Org, error) {
	rows, err := s.q.ListOrgsForUser(ctx, p.UserID)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	out := make([]Org, 0, len(rows))
	for _, r := range rows {
		if p.PinnedOrgID != nil && *p.PinnedOrgID != r.ID {
			continue
		}
		out = append(out, Org{ID: r.ID, Slug: r.Slug, Name: r.Name, Role: r.Role, CreatedAt: r.CreatedAt,
			TimeZone: r.TimeZone, DefaultTeam: r.DefaultTeamKey,
			AvatarURL: s.avatarURL(r.Slug, "", r.HasAvatar, r.AvatarVersion)})
	}
	return out, nil
}

// MeFrom describes the caller.
func MeFrom(p *auth.Principal) Me {
	scopes := make([]string, len(p.Scopes))
	for i, sc := range p.Scopes {
		scopes[i] = string(sc)
	}
	return Me{ID: p.UserID, Username: p.Username, Name: p.Name, DisplayName: p.Name, Email: p.Email, IsAgent: p.IsAgent,
		IsPlatformAdmin: p.IsPlatformAdmin, TokenID: p.TokenID, TokenKind: p.TokenKind, Scopes: scopes, PinnedOrgID: p.PinnedOrgID}
}
