package tracker

import (
	"context"
	"fmt"
	"slices"
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

// Project labels (LAUN-194, decisions-T28-project-labels.md): org-wide labels
// for projects, with one level of groups (parent_id).

const maxProjectLabels = 50 // on one project

func validateProjectLabelName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if l := utf8.RuneCountInString(n); l < 1 || l > 100 {
		return "", httpx.Invalid("name must be 1-100 characters")
	}
	return n, nil
}

func validateProjectLabelColor(c string) error {
	if !colorRe.MatchString(c) {
		return httpx.Invalid("color must be #RRGGBB")
	}
	return nil
}

func toProjectLabel(l dbq.ProjectLabel, n int) ProjectLabel {
	return ProjectLabel{ID: l.ID, Name: l.Name, Color: l.Color, ParentID: l.ParentID, ProjectCount: n, CreatedAt: l.CreatedAt}
}

// ListProjectLabels lists the org's project labels, by name, with their project
// counts. A count holds only the projects the caller sees (T20 #36).
func (s *Service) ListProjectLabels(ctx context.Context, org *auth.Org) ([]ProjectLabel, error) {
	all, ids := org.Access.Args()
	rows, err := s.q.ListProjectLabels(ctx, dbq.ListProjectLabelsParams{OrgID: org.ID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return nil, fmt.Errorf("list project labels: %w", err)
	}
	out := make([]ProjectLabel, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProjectLabel(dbq.ProjectLabel{ID: r.ID, Name: r.Name, Color: r.Color, ParentID: r.ParentID,
			CreatedAt: r.CreatedAt}, int(r.ProjectCount)))
	}
	return out, nil
}

// checkLabelParent validates a parent for label id (uuid.Nil on create): it
// must be a label of this org (400 otherwise), not the label itself, itself
// top-level, and the label must have no children (groups are one level).
func checkLabelParent(ctx context.Context, q *dbq.Queries, orgID, id, parent uuid.UUID) error {
	if parent == id && id != uuid.Nil {
		return httpx.Invalid("a label cannot be its own group")
	}
	par, err := q.LockProjectLabel(ctx, dbq.LockProjectLabelParams{OrgID: orgID, ID: parent})
	if err != nil {
		return invalidRef(err, "unknown parentId")
	}
	if par.ParentID != nil {
		return httpx.Invalid("a group cannot itself be in a group")
	}
	if id != uuid.Nil {
		n, err := q.CountProjectLabelChildren(ctx, dbq.CountProjectLabelChildrenParams{OrgID: orgID, ParentID: &id})
		if err != nil {
			return fmt.Errorf("count label children: %w", err)
		}
		if n > 0 {
			return httpx.Invalid("a group cannot be put in another group; move its labels out first")
		}
	}
	return nil
}

// ProjectLabelInput is the POST /project-labels body.
type ProjectLabelInput struct {
	Name     string  `json:"name"`
	Color    string  `json:"color"`
	ParentID *string `json:"parentId"`
}

func parseLabelParent(ref string) (uuid.UUID, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return uuid.Nil, httpx.Invalid("parentId must be a UUID")
	}
	return id, nil
}

// CreateProjectLabel adds a label. Names are unique ignoring case (409 conflict).
func (s *Service) CreateProjectLabel(ctx context.Context, org *auth.Org, p *auth.Principal, in ProjectLabelInput) (ProjectLabel, error) {
	name, err := validateProjectLabelName(in.Name)
	if err != nil {
		return ProjectLabel{}, err
	}
	color := in.Color
	if color == "" {
		color = "#95a2b3"
	}
	if err := validateProjectLabelColor(color); err != nil {
		return ProjectLabel{}, err
	}
	var parent *uuid.UUID
	if in.ParentID != nil {
		id, err := parseLabelParent(*in.ParentID)
		if err != nil {
			return ProjectLabel{}, err
		}
		parent = &id
	}
	var l dbq.ProjectLabel
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if parent != nil {
			if err := checkLabelParent(ctx, q, org.ID, uuid.Nil, *parent); err != nil {
				return err
			}
		}
		var err error
		if l, err = q.InsertProjectLabel(ctx, dbq.InsertProjectLabelParams{OrgID: org.ID, Name: name, Color: color, ParentID: parent}); err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("a project label named %q already exists", name)
			}
			return fmt.Errorf("insert project label: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectLabelCreated, EntityType: "project_label", EntityID: l.ID,
			ActorID: &p.UserID, Payload: map[string]any{"id": l.ID, "name": l.Name}}, events.ScopeOrg())
		return err
	})
	if err != nil {
		return ProjectLabel{}, err
	}
	return toProjectLabel(l, 0), nil
}

// UpdateProjectLabelInput is the PATCH /project-labels/{id} body. parentId null leaves a group.
type UpdateProjectLabelInput struct {
	Name     httpx.Opt[string] `json:"name"`
	Color    httpx.Opt[string] `json:"color"`
	ParentID httpx.Opt[string] `json:"parentId"`
}

// UpdateProjectLabel renames, recolours or regroups a label under its row lock.
func (s *Service) UpdateProjectLabel(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in UpdateProjectLabelInput) (ProjectLabel, error) {
	var out dbq.ProjectLabel
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Lock the label and its new group together, in id order (decisions-T28 #10).
		lockIDs := []uuid.UUID{id}
		if in.ParentID.Set && !in.ParentID.Null {
			if pid, err := uuid.Parse(in.ParentID.V); err == nil {
				lockIDs = append(lockIDs, pid)
			}
		}
		if _, err := q.LockProjectLabelsForUpdate(ctx, dbq.LockProjectLabelsForUpdateParams{OrgID: org.ID, Ids: lockIDs}); err != nil {
			return fmt.Errorf("lock project labels: %w", err)
		}
		cur, err := q.LockProjectLabel(ctx, dbq.LockProjectLabelParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "project label")
		}
		out = cur
		next := dbq.UpdateProjectLabelParams{OrgID: org.ID, ID: id, Name: cur.Name, Color: cur.Color, ParentID: cur.ParentID}
		var changes []string
		if in.Name.Set {
			if in.Name.Null {
				return httpx.Invalid("name cannot be null")
			}
			n, err := validateProjectLabelName(in.Name.V)
			if err != nil {
				return err
			}
			if n != cur.Name {
				next.Name, changes = n, append(changes, "name")
			}
		}
		if in.Color.Set {
			if in.Color.Null {
				return httpx.Invalid("color cannot be null")
			}
			if err := validateProjectLabelColor(in.Color.V); err != nil {
				return err
			}
			if in.Color.V != cur.Color {
				next.Color, changes = in.Color.V, append(changes, "color")
			}
		}
		if in.ParentID.Set {
			var parent *uuid.UUID
			if !in.ParentID.Null {
				pid, err := parseLabelParent(in.ParentID.V)
				if err != nil {
					return err
				}
				if err := checkLabelParent(ctx, q, org.ID, id, pid); err != nil {
					return err
				}
				parent = &pid
			}
			if !eqPtr(parent, cur.ParentID) {
				next.ParentID, changes = parent, append(changes, "parentId")
			}
		}
		if len(changes) > 0 {
			if out, err = q.UpdateProjectLabel(ctx, next); err != nil {
				if database.IsPgCode(err, database.CodeUniqueViolation) {
					return conflict("a project label named %q already exists", next.Name)
				}
				return fmt.Errorf("update project label: %w", err)
			}
			_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectLabelUpdated, EntityType: "project_label", EntityID: id,
				ActorID: &p.UserID, Payload: map[string]any{"id": id, "changes": changes}}, events.ScopeOrg())
		}
		return err
	})
	if err != nil {
		return ProjectLabel{}, err
	}
	return s.projectLabel(ctx, org, out)
}

func (s *Service) projectLabel(ctx context.Context, org *auth.Org, l dbq.ProjectLabel) (ProjectLabel, error) {
	all, err := s.ListProjectLabels(ctx, org)
	if err != nil {
		return ProjectLabel{}, err
	}
	for _, x := range all {
		if x.ID == l.ID {
			return x, nil
		}
	}
	return toProjectLabel(l, 0), nil
}

// DeleteProjectLabel removes a label. Its assignments go (foreign key); the
// labels of a deleted group stay, with no group.
func (s *Service) DeleteProjectLabel(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.LockProjectLabel(ctx, dbq.LockProjectLabelParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "project label")
		}
		if err := q.DeleteProjectLabel(ctx, dbq.DeleteProjectLabelParams{OrgID: org.ID, ID: id}); err != nil {
			return fmt.Errorf("delete project label: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.ProjectLabelDeleted, EntityType: "project_label", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "name": cur.Name}}, events.ScopeOrg())
		return err
	})
}

// resolveProjectLabelIDs parses label ids from a project body. Every id must
// name a label of this org (400 otherwise); the rows are share-locked so a
// concurrent label delete waits for this transaction.
func resolveProjectLabelIDs(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, refs []string) ([]uuid.UUID, error) {
	if len(refs) > maxProjectLabels {
		return nil, httpx.Invalid("at most %d labels per project", maxProjectLabels)
	}
	out := make([]uuid.UUID, 0, len(refs))
	for _, r := range refs {
		id, err := uuid.Parse(strings.TrimSpace(r))
		if err != nil {
			return nil, httpx.Invalid("labelIds must be UUIDs: %q", r)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	found, err := q.LockProjectLabelsByID(ctx, dbq.LockProjectLabelsByIDParams{OrgID: orgID, Ids: out})
	if err != nil {
		return nil, fmt.Errorf("lock project labels: %w", err)
	}
	if len(found) != len(out) {
		return nil, httpx.Invalid("unknown project label in labelIds")
	}
	return out, nil
}

// replaceProjectLabels makes the project's labels exactly want; true if it changed.
func replaceProjectLabels(ctx context.Context, q *dbq.Queries, orgID, projectID uuid.UUID, want []uuid.UUID) (bool, error) {
	have, err := q.ListProjectLabelIDs(ctx, dbq.ListProjectLabelIDsParams{OrgID: orgID, ProjectID: projectID})
	if err != nil {
		return false, fmt.Errorf("project labels: %w", err)
	}
	changed := false
	for _, w := range want {
		if !slices.Contains(have, w) {
			if err := q.InsertProjectLabelAssignment(ctx, dbq.InsertProjectLabelAssignmentParams{OrgID: orgID, ProjectID: projectID, LabelID: w}); err != nil {
				return false, fmt.Errorf("add project label: %w", err)
			}
			changed = true
		}
	}
	for _, h := range have {
		if !slices.Contains(want, h) {
			if err := q.DeleteProjectLabelAssignment(ctx, dbq.DeleteProjectLabelAssignmentParams{OrgID: orgID, ProjectID: projectID, LabelID: h}); err != nil {
				return false, fmt.Errorf("remove project label: %w", err)
			}
			changed = true
		}
	}
	return changed, nil
}

// attachProjectLabels fills Labels for a batch with ONE query (no N+1).
func attachProjectLabels(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, ps []Project) error {
	for i := range ps {
		ps[i].Labels = []ProjectLabelRef{}
	}
	if len(ps) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ps))
	idx := make(map[uuid.UUID]int, len(ps))
	for i, pr := range ps {
		ids[i], idx[pr.ID] = pr.ID, i
	}
	rows, err := q.ListLabelsForProjects(ctx, dbq.ListLabelsForProjectsParams{OrgID: orgID, ProjectIds: ids})
	if err != nil {
		return fmt.Errorf("list project labels: %w", err)
	}
	for _, r := range rows {
		i := idx[r.ProjectID]
		ps[i].Labels = append(ps[i].Labels, ProjectLabelRef{ID: r.ID, Name: r.Name, Color: r.Color, ParentID: r.ParentID})
	}
	return nil
}
