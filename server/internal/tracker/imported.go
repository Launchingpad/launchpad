package tracker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/dbq"
)

// Imported Linear ids (T4b). The importer records, in
// import_mappings, which dev-ops row each Linear id became. Agents still
// pass Linear ids they learnt before the cutover, so wherever the API takes
// a team or a cycle by id it also takes the Linear id of an imported one.
// The lookup is by org: another org's mapping is not found, so a Linear id
// of another org's team is a 404 on a path and a 400 in a body or filter,
// exactly as a dev-ops id of another org's row is (decisions-T1 #25).
//
// The entity types are the importer's (internal/importer plan.go EntTeam,
// EntCycle); they are spelt out here because the importer imports this
// package, not the reverse.
const (
	importedTeam  = "team"
	importedCycle = "cycle"
)

// importedID is the dev-ops id a Linear id of entity maps to in org, when the
// row it names is in a team org.Access can see (T20 #14). The lookup is split
// per entity (issue, team, cycle): each variant joins the target table and adds
// the team predicate, so a Linear id of a hidden row is not found.
func importedID(ctx context.Context, q *dbq.Queries, org *auth.Org, entity, sourceID string) (uuid.UUID, bool, error) {
	all, ids := org.Access.Args()
	var id uuid.UUID
	var err error
	switch entity {
	case importedTeam:
		id, err = q.ResolveImportedTeamID(ctx, dbq.ResolveImportedTeamIDParams{OrgID: org.ID, SourceID: sourceID, AllTeams: all, TeamIds: ids})
	case importedCycle:
		id, err = q.ResolveImportedCycleID(ctx, dbq.ResolveImportedCycleIDParams{OrgID: org.ID, SourceID: sourceID, AllTeams: all, TeamIds: ids})
	default:
		return uuid.Nil, false, fmt.Errorf("imported %s lookup: no scoped variant", entity)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, false, nil
	case err != nil:
		return uuid.Nil, false, fmt.Errorf("imported %s lookup: %w", entity, err)
	}
	return id, true, nil
}

// resolveCycleID turns a cycle id or an imported Linear cycle id into the
// cycle id, within org and the teams org.Access can see. pgx.ErrNoRows when it
// names no such cycle.
func resolveCycleID(ctx context.Context, q *dbq.Queries, org *auth.Org, id uuid.UUID) (uuid.UUID, error) {
	all, ids := org.Access.Args()
	got, err := q.CycleExists(ctx, dbq.CycleExistsParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
	if !errors.Is(err, pgx.ErrNoRows) {
		return got, err
	}
	target, ok, merr := importedID(ctx, q, org, importedCycle, id.String())
	if merr != nil {
		return uuid.Nil, merr
	}
	if !ok {
		return uuid.Nil, pgx.ErrNoRows
	}
	// A mapping whose cycle was deleted names nothing, exactly as an
	// unknown id: 404 on a path, 400 in a body or in the cycle filter.
	return q.CycleExists(ctx, dbq.CycleExistsParams{OrgID: org.ID, ID: target, AllTeams: all, TeamIds: ids})
}

// cycleRef resolves an optional body cycleId. An id that names no cycle is
// passed through unchanged, so checkPlanningRefs answers its usual 400.
func cycleRef(ctx context.Context, q *dbq.Queries, org *auth.Org, id *uuid.UUID) (*uuid.UUID, error) {
	if id == nil {
		return nil, nil
	}
	got, err := resolveCycleID(ctx, q, org, *id)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, nil
	}
	if err != nil {
		return nil, err
	}
	return &got, nil
}

// teamFilterID is the team id a list filter's UUID means: the id itself, or
// the team an imported Linear id maps to. An id that names neither stays as
// it is (an empty page, as an unknown team key gives).
func teamFilterID(ctx context.Context, q *dbq.Queries, org *auth.Org, id uuid.UUID) (uuid.UUID, error) {
	all, ids := org.Access.Args()
	if _, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids}); !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	target, ok, err := importedID(ctx, q, org, importedTeam, id.String())
	if err != nil || !ok {
		return id, err
	}
	return target, nil
}

// cyclePathID resolves a {cycle} path id: a cycle id or an imported Linear
// cycle id. Neither is a 404.
func (s *Service) cyclePathID(ctx context.Context, org *auth.Org, id uuid.UUID) (uuid.UUID, error) {
	got, err := resolveCycleID(ctx, s.q, org, id)
	return got, notFound(err, "cycle")
}
