package tracker

import (
	"context"
	"fmt"
	"net/url"
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

// Project updates (LAUN-196, decisions-T30): a status post on a project with a
// health value. v1 is create, list and read. No edit, no delete.

// ProjectUpdateHealth values.
var projectUpdateHealths = []string{"on_track", "at_risk", "off_track"}

const maxProjectUpdateBodyRunes = 50_000

// ProjectUpdate is one posted update.
type ProjectUpdate struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"projectId"`
	Author    *UserRef  `json:"author"`
	Health    string    `json:"health"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreateProjectUpdateInput is the POST /projects/{project}/updates body.
type CreateProjectUpdateInput struct {
	Health string `json:"health"`
	Body   string `json:"body"`
}

const projectUpdateSelect = `/* scope: resolved */ SELECT pu.id, pu.project_id, pu.health, pu.body, pu.created_at, u.id, u.username, u.name
	FROM project_updates pu LEFT JOIN users u ON u.id = pu.author_id `

func scanProjectUpdate(row pgx.Row) (ProjectUpdate, error) {
	var pu ProjectUpdate
	var uid *uuid.UUID
	var un, nm *string
	if err := row.Scan(&pu.ID, &pu.ProjectID, &pu.Health, &pu.Body, &pu.CreatedAt, &uid, &un, &nm); err != nil {
		return ProjectUpdate{}, err
	}
	pu.Author = userRef(uid, un, nm)
	return pu, nil
}

// CreateProjectUpdate posts an update on a project of this org. The event is
// the last statement of the transaction.
func (s *Service) CreateProjectUpdate(ctx context.Context, org *auth.Org, p *auth.Principal, projectID uuid.UUID, in CreateProjectUpdateInput) (ProjectUpdate, error) {
	scopeAll, scopeIDs := org.Access.Args()
	if !slices.Contains(projectUpdateHealths, in.Health) {
		return ProjectUpdate{}, httpx.Invalid("health must be one of %s", strings.Join(projectUpdateHealths, ", "))
	}
	if utf8.RuneCountInString(in.Body) > maxProjectUpdateBodyRunes {
		return ProjectUpdate{}, httpx.Invalid("body must be at most %d characters", maxProjectUpdateBodyRunes)
	}
	var id uuid.UUID
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: projectID, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
			return notFound(err, "project")
		}
		row, err := q.InsertProjectUpdate(ctx, dbq.InsertProjectUpdateParams{OrgID: org.ID, ProjectID: projectID,
			AuthorID: &p.UserID, Health: in.Health, Body: in.Body})
		if err != nil {
			return fmt.Errorf("insert project update: %w", err)
		}
		id = row.ID
		scope, err := projectEventScope(ctx, q, org.ID, projectID)
		if err != nil {
			return err
		}
		// ids and the health word only: no body in the feed.
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectUpdateCreated, EntityType: "project_update", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "projectId": projectID, "health": in.Health}}, scope)
		return err
	})
	if err != nil {
		return ProjectUpdate{}, err
	}
	return scanProjectUpdate(s.pool.QueryRow(ctx, projectUpdateSelect+"WHERE pu.org_id = $1 AND pu.id = $2", org.ID, id))
}

// GetProjectUpdate reads one update of a project.
func (s *Service) GetProjectUpdate(ctx context.Context, org *auth.Org, projectID, id uuid.UUID) (ProjectUpdate, error) {
	// The project is checked first, as in create and list: a hidden project is
	// the 404 of an unknown one (T20 #36).
	all, ids := org.Access.Args()
	if _, err := s.q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: projectID, AllTeams: all, TeamIds: ids}); err != nil {
		return ProjectUpdate{}, notFound(err, "project")
	}
	pu, err := scanProjectUpdate(s.pool.QueryRow(ctx,
		projectUpdateSelect+"WHERE pu.org_id = $1 AND pu.project_id = $2 AND pu.id = $3", org.ID, projectID, id))
	return pu, notFound(err, "project update")
}

// ProjectUpdateFilter is a parsed GET /projects/{project}/updates query.
type ProjectUpdateFilter struct{ listQuery }

// ParseProjectUpdateFilter validates the query string (limit and cursor).
func ParseProjectUpdateFilter(v url.Values) (ProjectUpdateFilter, error) {
	if v.Has("includeArchived") {
		return ProjectUpdateFilter{}, httpx.Invalid("unknown query parameter %q", "includeArchived")
	}
	lq, err := parseListQuery(v, "project_updates")
	return ProjectUpdateFilter{lq}, err
}

// ListProjectUpdates returns one keyset page, newest first. An unknown project
// (or one of another org) is 404.
func (s *Service) ListProjectUpdates(ctx context.Context, org *auth.Org, projectID uuid.UUID, f ProjectUpdateFilter) (httpx.Page[ProjectUpdate], error) {
	scopeAll, scopeIDs := org.Access.Args()
	if _, err := s.q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: projectID, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
		return httpx.Page[ProjectUpdate]{}, notFound(err, "project")
	}
	args := []any{org.ID, projectID}
	where := "pu.org_id = $1 AND pu.project_id = $2"
	if f.Cursor != nil {
		args = append(args, f.Cursor.At, f.Cursor.ID)
		where += " AND (pu.created_at, pu.id) < ($3, $4)"
	}
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, projectUpdateSelect+"WHERE "+where+
		fmt.Sprintf(" ORDER BY pu.created_at DESC, pu.id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return httpx.Page[ProjectUpdate]{}, fmt.Errorf("list project updates: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProjectUpdate, error) { return scanProjectUpdate(r) })
	if err != nil {
		return httpx.Page[ProjectUpdate]{}, fmt.Errorf("scan project updates: %w", err)
	}
	return pageOf(items, f.Limit, "project_updates", func(u ProjectUpdate) (time.Time, uuid.UUID) { return u.CreatedAt, u.ID }), nil
}
