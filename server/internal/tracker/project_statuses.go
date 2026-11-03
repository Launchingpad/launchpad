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
	"github.com/jackc/pgx/v5/pgconn"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// Project statuses (LAUN-195, docs/internal/decisions-T29-project-statuses.md). A status has one of six fixed
// KINDS, a name, a colour and a place in the org's order. projects.state is the kind of the project's status.

// maxProjectStatuses bounds the statuses of one org.
const maxProjectStatuses = 60

var (
	// projectStates are the six kinds, in the order a project moves through them.
	projectStates = []string{"backlog", "planned", "started", "paused", "completed", "canceled"}
)

// ProjectStatus is one status as listed.
type ProjectStatus struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	Position  int32     `json:"position"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ProjectStatusRef is the status on a project.
type ProjectStatusRef struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind"`
	Name  string    `json:"name"`
	Color string    `json:"color"`
}

func toProjectStatus(r dbq.ProjectStatus) ProjectStatus {
	return ProjectStatus{ID: r.ID, Kind: r.Kind, Name: r.Name, Color: r.Color, Position: r.Position,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// validateStatusName trims the name and refuses one that is empty, too long, or another kind's word: the
// MCP and old clients send a kind as `state`, and a status named like a different kind would make that ambiguous.
func validateStatusName(n, kind string) (string, error) {
	n = strings.TrimSpace(n)
	if l := utf8.RuneCountInString(n); l < 1 || l > 50 {
		return "", httpx.Invalid("name must be 1-50 characters")
	}
	if strings.Contains(n, ",") {
		return "", httpx.Invalid("name cannot contain a comma")
	}
	if low := strings.ToLower(n); low != kind && slices.Contains(projectStates, low) {
		return "", httpx.Invalid("name %q is the name of the %s kind; choose another name", n, low)
	}
	return n, nil
}

func validateStatusColor(c string) error {
	if !colorRe.MatchString(c) {
		return httpx.Invalid("color must be a #rrggbb hex value")
	}
	return nil
}

func validProjectState(st string) bool { return slices.Contains(projectStates, st) }

func validateProjectState(st string) error {
	if !validProjectState(st) {
		return httpx.Invalid("state must be one of %s", strings.Join(projectStates, ", "))
	}
	return nil
}

// ListProjectStatuses lists the org's statuses in order.
func (s *Service) ListProjectStatuses(ctx context.Context, org *auth.Org) ([]ProjectStatus, error) {
	rows, err := s.q.ListProjectStatuses(ctx, org.ID)
	if err != nil {
		return nil, fmt.Errorf("list project statuses: %w", err)
	}
	out := make([]ProjectStatus, len(rows))
	for i, r := range rows {
		out[i] = toProjectStatus(r)
	}
	return out, nil
}

// CreateProjectStatusInput is the POST /project-statuses body.
type CreateProjectStatusInput struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// CreateProjectStatus adds a status at the end of the order and emits project_status.created.
func (s *Service) CreateProjectStatus(ctx context.Context, org *auth.Org, p *auth.Principal, in CreateProjectStatusInput) (ProjectStatus, error) {
	if err := validateProjectState(in.Kind); err != nil {
		return ProjectStatus{}, httpx.Invalid("kind must be one of %s", strings.Join(projectStates, ", "))
	}
	name, err := validateStatusName(in.Name, in.Kind)
	if err != nil {
		return ProjectStatus{}, err
	}
	if err := validateStatusColor(in.Color); err != nil {
		return ProjectStatus{}, err
	}
	var out dbq.ProjectStatus
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockProjectStatuses(ctx, org.ID); err != nil {
			return fmt.Errorf("lock project statuses: %w", err)
		}
		if n, err := q.CountProjectStatuses(ctx, org.ID); err != nil {
			return fmt.Errorf("count project statuses: %w", err)
		} else if n >= maxProjectStatuses {
			return httpx.Invalid("at most %d project statuses", maxProjectStatuses)
		}
		if err := statusNameFree(ctx, q, org.ID, name, uuid.Nil); err != nil {
			return err
		}
		if out, err = q.InsertProjectStatus(ctx, dbq.InsertProjectStatusParams{OrgID: org.ID, Kind: in.Kind, Name: name,
			Color: strings.ToLower(in.Color)}); err != nil {
			return fmt.Errorf("insert project status: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectStatusCreated, EntityType: "project_status", EntityID: out.ID,
			ActorID: &p.UserID, Payload: map[string]any{"id": out.ID, "kind": out.Kind, "name": name}}, events.ScopeOrg())
		return err
	})
	if err != nil {
		return ProjectStatus{}, err
	}
	return toProjectStatus(out), nil
}

// statusNameFree is a 409 when another status of the org has the name (any case).
func statusNameFree(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, name string, self uuid.UUID) error {
	other, err := q.GetProjectStatusByName(ctx, dbq.GetProjectStatusByNameParams{OrgID: orgID, Name: name})
	switch {
	case err == nil && other.ID != self:
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a project status named %q exists", other.Name)
	case err != nil && err != pgx.ErrNoRows:
		return fmt.Errorf("project status by name: %w", err)
	}
	return nil
}

// UpdateProjectStatusInput is the PATCH /project-statuses/{status} body. The kind never changes.
type UpdateProjectStatusInput struct {
	Name  httpx.Opt[string] `json:"name"`
	Color httpx.Opt[string] `json:"color"`
}

// UpdateProjectStatus renames or recolours a status. A PATCH that changes nothing writes nothing.
func (s *Service) UpdateProjectStatus(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in UpdateProjectStatusInput) (ProjectStatus, error) {
	var out dbq.ProjectStatus
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockProjectStatuses(ctx, org.ID); err != nil {
			return fmt.Errorf("lock project statuses: %w", err)
		}
		cur, err := q.GetProjectStatus(ctx, dbq.GetProjectStatusParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "project status")
		}
		out = cur
		name, color := cur.Name, cur.Color
		var changes []string
		if in.Name.Set {
			if in.Name.Null {
				return httpx.Invalid("name cannot be null")
			}
			n, err := validateStatusName(in.Name.V, cur.Kind)
			if err != nil {
				return err
			}
			if n != cur.Name {
				if err := statusNameFree(ctx, q, org.ID, n, id); err != nil {
					return err
				}
				name, changes = n, append(changes, "name")
			}
		}
		if in.Color.Set {
			if in.Color.Null {
				return httpx.Invalid("color cannot be null")
			}
			if err := validateStatusColor(in.Color.V); err != nil {
				return err
			}
			if c := strings.ToLower(in.Color.V); c != cur.Color {
				color, changes = c, append(changes, "color")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		if out, err = q.UpdateProjectStatus(ctx, dbq.UpdateProjectStatusParams{OrgID: org.ID, ID: id, Name: name, Color: color}); err != nil {
			return fmt.Errorf("update project status: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectStatusUpdated, EntityType: "project_status", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "changes": changes}}, events.ScopeOrg())
		return err
	})
	if err != nil {
		return ProjectStatus{}, err
	}
	return toProjectStatus(out), nil
}

// DeleteProjectStatus removes a status that no project uses and that is not the last of its kind.
func (s *Service) DeleteProjectStatus(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// The org lock first: a project write that moves to this status holds a KEY SHARE on the row through the
		// foreign key, and the count below must not miss it.
		if _, err := q.LockProjectStatuses(ctx, org.ID); err != nil {
			return fmt.Errorf("lock project statuses: %w", err)
		}
		cur, err := q.GetProjectStatus(ctx, dbq.GetProjectStatusParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "project status")
		}
		if n, err := q.CountProjectStatusesOfKind(ctx, dbq.CountProjectStatusesOfKindParams{OrgID: org.ID, Kind: cur.Kind}); err != nil {
			return fmt.Errorf("count statuses of kind: %w", err)
		} else if n <= 1 {
			return httpx.Errorf(http.StatusConflict, httpx.CodeStatusLastOfKind,
				"%q is the only %s status; every kind keeps at least one status", cur.Name, cur.Kind)
		}
		n, err := q.CountProjectsWithStatus(ctx, dbq.CountProjectsWithStatusParams{OrgID: org.ID, StatusID: id})
		if err != nil {
			return fmt.Errorf("count projects with status: %w", err)
		}
		if n > 0 {
			return httpx.Errorf(http.StatusConflict, httpx.CodeStatusInUse,
				"%d project(s) have the status %q; move them to another status first", n, cur.Name)
		}
		if err := q.DeleteProjectStatus(ctx, dbq.DeleteProjectStatusParams{OrgID: org.ID, ID: id}); err != nil {
			// Defence in depth: the count above holds under the org lock, but the key is the last word.
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == database.CodeForeignKeyViolation && pg.ConstraintName == "projects_status_fk" {
				return httpx.Errorf(http.StatusConflict, httpx.CodeStatusInUse,
					"projects have the status %q; move them to another status first", cur.Name)
			}
			return fmt.Errorf("delete project status: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectStatusDeleted, EntityType: "project_status", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "kind": cur.Kind, "name": cur.Name}}, events.ScopeOrg())
		return err
	})
}

// ReorderProjectStatusesInput is the PUT /project-statuses/order body: every status id of the org, once, in the new order.
type ReorderProjectStatusesInput struct {
	IDs []uuid.UUID `json:"ids"`
}

// ReorderProjectStatuses sets the order of all the org's statuses. The ids must be exactly the org's set (a
// missing, repeated or foreign id is a 400, so a stale client cannot drop a status from the order).
func (s *Service) ReorderProjectStatuses(ctx context.Context, org *auth.Org, p *auth.Principal, in ReorderProjectStatusesInput) ([]ProjectStatus, error) {
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockProjectStatuses(ctx, org.ID); err != nil {
			return fmt.Errorf("lock project statuses: %w", err)
		}
		rows, err := q.ListProjectStatuses(ctx, org.ID)
		if err != nil {
			return fmt.Errorf("list project statuses: %w", err)
		}
		have := make(map[uuid.UUID]int32, len(rows))
		for _, r := range rows {
			have[r.ID] = r.Position
		}
		seen := make(map[uuid.UUID]bool, len(in.IDs))
		for _, id := range in.IDs {
			if _, ok := have[id]; !ok || seen[id] {
				return httpx.Invalid("ids must list each status of this org exactly once")
			}
			seen[id] = true
		}
		if len(in.IDs) != len(rows) {
			return httpx.Invalid("ids must list each status of this org exactly once")
		}
		moved := 0
		for i, id := range in.IDs {
			if have[id] != int32(i) {
				moved++
			}
			if err := q.SetProjectStatusPosition(ctx, dbq.SetProjectStatusPositionParams{OrgID: org.ID, ID: id, Position: int32(i)}); err != nil {
				return fmt.Errorf("set project status position: %w", err)
			}
		}
		if moved == 0 {
			return nil
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectStatusesReordered, EntityType: "project_status", EntityID: in.IDs[0],
			ActorID: &p.UserID, Payload: map[string]any{"ids": in.IDs}}, events.ScopeOrg())
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.ListProjectStatuses(ctx, org)
}

// resolveProjectStatus picks the status a project write names. statusID (a project-status id of this org) wins;
// else state is a status NAME (any case), then a KIND (the first status of that kind in the order), so the MCP and
// old clients that send a kind keep working. Neither given: the first status of defKind (when set). A reference
// to another org's status is a 400, never a 404 (decisions: a body reference).
func resolveProjectStatus(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, state, statusID, defKind string) (dbq.ProjectStatus, error) {
	st, err := pickProjectStatus(ctx, q, orgID, state, statusID, defKind)
	if err != nil {
		return st, err
	}
	// Hold the row until the project write commits, so a status delete cannot slip in between.
	if st, err = q.ShareLockProjectStatus(ctx, dbq.ShareLockProjectStatusParams{OrgID: orgID, ID: st.ID}); err != nil {
		return st, invalidRef(err, "the project status no longer exists")
	}
	return st, nil
}

func pickProjectStatus(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, state, statusID, defKind string) (dbq.ProjectStatus, error) {
	if statusID != "" {
		id, err := uuid.Parse(statusID)
		if err != nil {
			return dbq.ProjectStatus{}, httpx.Invalid("statusId must be a project status id")
		}
		st, err := q.GetProjectStatus(ctx, dbq.GetProjectStatusParams{OrgID: orgID, ID: id})
		if err != nil {
			return st, invalidRef(err, "unknown statusId")
		}
		if state != "" && !strings.EqualFold(state, st.Kind) && !strings.EqualFold(state, st.Name) {
			return st, httpx.Invalid("state %q does not match the status %q", state, st.Name)
		}
		return st, nil
	}
	state = strings.TrimSpace(state)
	if state == "" {
		state = defKind
	}
	st, err := q.GetProjectStatusByName(ctx, dbq.GetProjectStatusByNameParams{OrgID: orgID, Name: state})
	if err == nil {
		return st, nil
	}
	if err != pgx.ErrNoRows {
		return st, fmt.Errorf("project status by name: %w", err)
	}
	kind := strings.ToLower(state)
	if !validProjectState(kind) {
		return st, httpx.Invalid("state must be a project status name or one of %s", strings.Join(projectStates, ", "))
	}
	if st, err = q.FirstProjectStatusOfKind(ctx, dbq.FirstProjectStatusOfKindParams{OrgID: orgID, Kind: kind}); err != nil {
		return st, fmt.Errorf("first project status of kind: %w", err)
	}
	return st, nil
}
