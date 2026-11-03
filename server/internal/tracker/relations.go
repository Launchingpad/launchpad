package tracker

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// RelationInput is the POST .../relations body. Type is one of blocks,
// blockedBy, related, duplicateOf, duplicates. blockedBy and duplicates are
// stored as the reverse row, so the database only holds blocks, related and
// duplicate_of.
type RelationInput struct {
	Type  string `json:"type"`
	Issue string `json:"issue"` // identifier or id of the other issue
}

// relationsFor reads every edge touching issueID, with refs for the other side.
func (s *Service) relationsFor(ctx context.Context, org *auth.Org, issueID uuid.UUID) (Relations, error) {
	out := Relations{Blocks: []Relation{}, BlockedBy: []Relation{}, Related: []Relation{}, DuplicateOf: []Relation{}, Duplicates: []Relation{}}
	scopeAll, scopeIDs := org.Access.Args()
	rows, err := s.q.ListRelationsForIssue(ctx, dbq.ListRelationsForIssueParams{OrgID: org.ID, IssueID: issueID, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return out, fmt.Errorf("list relations: %w", err)
	}
	others := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		others = append(others, otherSide(r, issueID))
	}
	refs, err := s.issueRefs(ctx, org, others)
	if err != nil {
		return out, err
	}
	creators, err := s.relationCreators(ctx, rows)
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		if _, ok := refs[otherSide(r, issueID)]; !ok {
			continue // the other issue is not visible (T20 #41)
		}
		outgoing := r.IssueID == issueID
		rel := Relation{ID: r.ID, Issue: refs[otherSide(r, issueID)], CreatedAt: r.CreatedAt, CreatedBy: creators[r.ID]}
		switch {
		case r.Type == "blocks" && outgoing:
			rel.Type = "blocks"
			out.Blocks = append(out.Blocks, rel)
		case r.Type == "blocks":
			rel.Type = "blockedBy"
			out.BlockedBy = append(out.BlockedBy, rel)
		case r.Type == "related":
			rel.Type = "related"
			out.Related = append(out.Related, rel)
		case r.Type == "duplicate_of" && outgoing:
			rel.Type = "duplicateOf"
			out.DuplicateOf = append(out.DuplicateOf, rel)
		case r.Type == "duplicate_of":
			rel.Type = "duplicates"
			out.Duplicates = append(out.Duplicates, rel)
		}
	}
	return out, nil
}

func otherSide(r dbq.IssueRelation, issueID uuid.UUID) uuid.UUID {
	if r.IssueID == issueID {
		return r.RelatedIssueID
	}
	return r.IssueID
}

func (s *Service) issueRefs(ctx context.Context, org *auth.Org, ids []uuid.UUID) (map[uuid.UUID]IssueRef, error) {
	scopeAll, scopeIDs := org.Access.Args()
	out := make(map[uuid.UUID]IssueRef, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.ListIssueRefs(ctx, dbq.ListIssueRefsParams{OrgID: org.ID, Ids: ids, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return nil, fmt.Errorf("issue refs: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = IssueRef{ID: r.ID, Identifier: Identifier(r.TeamKey, r.Number), Title: r.Title, StateName: r.StateName, StateType: r.StateType}
	}
	return out, nil
}

// ListRelations returns an issue's relations grouped by direction.
func (s *Service) ListRelations(ctx context.Context, org *auth.Org, issueRef string) (Relations, error) {
	id, err := resolveIssueID(ctx, s.q, org, issueRef)
	if err != nil {
		return Relations{}, err
	}
	return s.relationsFor(ctx, org, id)
}

// CreateRelation adds an edge and returns it as seen from issueRef.
func (s *Service) CreateRelation(ctx context.Context, org *auth.Org, p *auth.Principal, issueRef string, in RelationInput) (Relation, error) {
	var dbType string
	reverse := false
	switch in.Type {
	case "blocks":
		dbType = "blocks"
	case "blockedBy":
		dbType, reverse = "blocks", true
	case "related":
		dbType = "related"
	case "duplicateOf":
		dbType = "duplicate_of"
	case "duplicates":
		dbType, reverse = "duplicate_of", true
	default:
		return Relation{}, httpx.Invalid("type must be blocks, blockedBy, related, duplicateOf or duplicates")
	}
	if in.Issue == "" {
		return Relation{}, httpx.Invalid("issue is required")
	}
	var row dbq.IssueRelation
	var self, other uuid.UUID
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		if self, err = resolveIssueID(ctx, q, org, issueRef); err != nil {
			return err
		}
		if other, err = resolveIssueID(ctx, q, org, in.Issue); err != nil {
			return asInvalid(err, "unknown issue %q", in.Issue)
		}
		if self == other {
			return httpx.Invalid("an issue cannot relate to itself")
		}
		from, to := self, other
		if reverse {
			from, to = other, self
		}
		for _, pair := range [][2]uuid.UUID{{from, to}, {to, from}} {
			found, err := q.RelationExists(ctx, dbq.RelationExistsParams{OrgID: org.ID, IssueID: pair[0], RelatedIssueID: pair[1], Type: dbType})
			if err != nil {
				return fmt.Errorf("relation exists: %w", err)
			}
			if found {
				if pair[0] == from {
					return conflict("that relation already exists")
				}
				return conflict("the reverse relation already exists")
			}
		}
		row, err = q.InsertRelation(ctx, dbq.InsertRelationParams{OrgID: org.ID, Type: dbType, IssueID: from, RelatedIssueID: to, CreatedBy: &p.UserID})
		if err != nil {
			// The pair index makes this race-free: two concurrent creates of
			// the same pair (either direction) cannot both insert.
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("that relation, or its reverse, already exists")
			}
			return fmt.Errorf("insert relation: %w", err)
		}
		scope, err := relationEventScope(ctx, q, org.ID, from, to)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.RelationCreated, EntityType: "relation", EntityID: row.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": row.ID, "type": dbType, "issueId": from, "relatedIssueId": to},
		}, scope)
		return err
	})
	if err != nil {
		return Relation{}, err
	}
	refs, err := s.issueRefs(ctx, org, []uuid.UUID{other})
	if err != nil {
		return Relation{}, err
	}
	return Relation{ID: row.ID, Type: in.Type, Issue: refs[other], CreatedAt: row.CreatedAt,
		CreatedBy: &UserRef{ID: p.UserID, Username: p.Username, Name: p.Name}}, nil
}

// DeleteRelation removes an edge that touches issueRef. The other issue of the
// edge must be visible too (T20 #41): an edge to a hidden issue is the 404 of
// an unknown relation, and nothing is written.
func (s *Service) DeleteRelation(ctx context.Context, org *auth.Org, p *auth.Principal, issueRef string, relationID uuid.UUID) error {
	scopeAll, scopeIDs := org.Access.Args()
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		issueID, err := resolveIssueID(ctx, q, org, issueRef)
		if err != nil {
			return err
		}
		row, err := q.DeleteRelation(ctx, dbq.DeleteRelationParams{OrgID: org.ID, ID: relationID, IssueID: issueID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "relation")
		}
		// The row is gone; keep who removed it, and what it joined, per
		// issue (decisions-T15). Both issues get a row.
		if err := recordRelationRemoved(ctx, s, q, org.ID, p, row); err != nil {
			return err
		}
		scope, err := relationEventScope(ctx, q, org.ID, row.IssueID, row.RelatedIssueID)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.RelationDeleted, EntityType: "relation", EntityID: row.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": row.ID, "type": row.Type, "issueId": row.IssueID, "relatedIssueId": row.RelatedIssueID},
		}, scope)
		return err
	})
}

// relationSnap is the from value of a relationRemoved row: the type as the
// row's own issue sees it, and the other issue.
type relationSnap struct {
	Type  string `json:"type"` // blocks | blockedBy | related | duplicateOf | duplicates
	Issue any    `json:"issue"`
}

func recordRelationRemoved(ctx context.Context, s *Service, q *dbq.Queries, orgID uuid.UUID, p *auth.Principal, row dbq.IssueRelation) error {
	// type as seen from the first issue (issue_id) and from the second.
	first, second := row.Type, row.Type
	switch row.Type {
	case "blocks":
		first, second = "blocks", "blockedBy"
	case "duplicate_of":
		first, second = "duplicateOf", "duplicates"
	}
	for _, side := range []struct {
		issue, other uuid.UUID
		typ          string
	}{{row.IssueID, row.RelatedIssueID, first}, {row.RelatedIssueID, row.IssueID, second}} {
		snap, err := snapIssue(ctx, s, q, orgID, &side.other)
		if err != nil {
			return err
		}
		if err := insertActivity(ctx, q, orgID, side.issue, &p.UserID, "api",
			activityRow{kind: ActRelationRemove, from: relationSnap{Type: side.typ, Issue: snap}}); err != nil {
			return err
		}
	}
	return nil
}

// relationCreators maps a relation id to the user who added it. A relation
// with no creator (imported, or the user was removed) has no entry.
func (s *Service) relationCreators(ctx context.Context, rows []dbq.IssueRelation) (map[uuid.UUID]*UserRef, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r.CreatedBy != nil {
			ids = append(ids, *r.CreatedBy)
		}
	}
	out := make(map[uuid.UUID]*UserRef, len(rows))
	if len(ids) == 0 {
		return out, nil
	}
	users, err := s.q.GetUserRefs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("relation creators: %w", err)
	}
	byID := make(map[uuid.UUID]*UserRef, len(users))
	for _, u := range users {
		byID[u.ID] = &UserRef{ID: u.ID, Username: u.Username, Name: u.Name}
	}
	for _, r := range rows {
		if r.CreatedBy != nil {
			out[r.ID] = byID[*r.CreatedBy]
		}
	}
	return out, nil
}
