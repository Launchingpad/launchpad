package tracker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/events"
)

// The scope of an event (decisions-T20 #53, #54) is decided where the event
// is made, from the rows in the writing transaction. These helpers read them
// with System queries: the caller already resolved the object with its own
// scope, and the event is for every viewer, not only the caller.

// issueEventScope: the issue's team (issue, comment, attachment events).
func issueEventScope(ctx context.Context, q *dbq.Queries, orgID, issueID uuid.UUID) (events.Scope, error) {
	teams, err := q.ListIssueTeamIDsSystem(ctx, dbq.ListIssueTeamIDsSystemParams{OrgID: orgID, Ids: []uuid.UUID{issueID}})
	if err != nil {
		return events.Scope{}, fmt.Errorf("event scope: issue team: %w", err)
	}
	if len(teams) == 0 {
		return events.Scope{}, fmt.Errorf("event scope: issue %s has no team", issueID)
	}
	return events.ScopeTeams(teams...), nil
}

// relationEventScope: every team of both issues; a viewer must see all of
// them (#54).
func relationEventScope(ctx context.Context, q *dbq.Queries, orgID, a, b uuid.UUID) (events.Scope, error) {
	teams, err := q.ListIssueTeamIDsSystem(ctx, dbq.ListIssueTeamIDsSystemParams{OrgID: orgID, Ids: []uuid.UUID{a, b}})
	if err != nil {
		return events.Scope{}, fmt.Errorf("event scope: relation teams: %w", err)
	}
	if len(teams) == 0 {
		return events.Scope{}, fmt.Errorf("event scope: relation issues have no team")
	}
	return events.ScopeAllTeams(teams...), nil
}

// projectEventScope: the project's teams now (in the transaction); org when
// it has none (#36, #54).
func projectEventScope(ctx context.Context, q *dbq.Queries, orgID, projectID uuid.UUID) (events.Scope, error) {
	teams, err := q.ListProjectTeamIDs(ctx, dbq.ListProjectTeamIDsParams{OrgID: orgID, ProjectID: projectID})
	if err != nil {
		return events.Scope{}, fmt.Errorf("event scope: project teams: %w", err)
	}
	return events.ScopeProject(teams), nil
}

// documentEventScope: a document follows its project; a document with no
// project is org-level (#38, #54).
func documentEventScope(ctx context.Context, q *dbq.Queries, orgID, documentID uuid.UUID) (events.Scope, error) {
	projectID, err := q.GetDocumentProjectIDSystem(ctx, dbq.GetDocumentProjectIDSystemParams{OrgID: orgID, ID: documentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return events.Scope{}, fmt.Errorf("event scope: document %s not found", documentID)
	}
	if err != nil {
		return events.Scope{}, fmt.Errorf("event scope: document project: %w", err)
	}
	if projectID == nil {
		return events.ScopeOrg(), nil
	}
	return projectEventScope(ctx, q, orgID, *projectID)
}

// eitherScope is the scope of a change that may move an object between
// teams (a project's team set, a document's project): the readers before AND
// after the change, so a team that loses the object still hears of it. An
// org-level side makes it org-level.
func eitherScope(before, after events.Scope) events.Scope {
	if before.Kind() == events.ScopeKindOrg || after.Kind() == events.ScopeKindOrg {
		return events.ScopeOrg()
	}
	return events.ScopeTeams(append(before.TeamIDs(), after.TeamIDs()...)...)
}

// productEventScope: a product follows the project rule (#69). named says the
// payload lists the team ids: then only a viewer who sees every one of them
// reads it, so no hidden team is named (#9). No team: org-level.
func productEventScope(teams []uuid.UUID, named bool) events.Scope {
	switch {
	case len(teams) == 0:
		return events.ScopeOrg()
	case named:
		return events.ScopeAllTeams(teams...)
	}
	return events.ScopeTeams(teams...)
}

// commentEventScope: a comment follows its issue or its document.
func commentEventScope(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, issueID, documentID *uuid.UUID) (events.Scope, error) {
	switch {
	case issueID != nil:
		return issueEventScope(ctx, q, orgID, *issueID)
	case documentID != nil:
		return documentEventScope(ctx, q, orgID, *documentID)
	}
	return events.Scope{}, fmt.Errorf("event scope: a comment with no issue and no document")
}
