package database

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"devops/internal/httpx"
)

func TestRetryable(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		err := Retryable(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: code}))
		var api *httpx.Error
		if !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable || api.Code != httpx.CodeOverloaded || api.RetryAfter < 1 {
			t.Fatalf("%s: %#v", code, err)
		}
	}
	// A foreign key on a team means the team was deleted under the write.
	fk := Retryable(&pgconn.PgError{Code: CodeForeignKeyViolation, ConstraintName: "labels_team_fk"})
	var gone *GoneError
	var api *httpx.Error
	if !errors.As(fk, &gone) || gone.Ref != "team" || !errors.As(fk, &api) || api.Status != http.StatusBadRequest || api.Code != httpx.CodeValidationFailed {
		t.Fatalf("team fk: %#v", fk)
	}
	for c, ref := range map[string]string{
		"issues_project_fk": "project", "milestones_project_fk": "project", "documents_project_fk": "project",
		"project_teams_project_fk": "project", "issues_milestone_fk": "milestone", "issues_cycle_fk": "cycle", "projects_status_fk": "project status",
		"comments_issue_fk": "issue", "attachments_issue_fk": "issue", "issue_labels_issue_fk": "issue", "issue_relations_issue_fk": "issue",
		"issue_relations_related_fk": "issue", "issues_parent_fk": "issue",
	} {
		err := Retryable(&pgconn.PgError{Code: CodeForeignKeyViolation, ConstraintName: c})
		var g *GoneError
		if !errors.As(err, &g) || g.Ref != ref {
			t.Fatalf("%s: %#v", c, err)
		}
	}
	// Anything else passes through unchanged.
	for _, in := range []error{
		errors.New("boom"),
		&pgconn.PgError{Code: CodeForeignKeyViolation, ConstraintName: "issues_state_fk"},
		&pgconn.PgError{Code: CodeUniqueViolation},
	} {
		if got := Retryable(in); got != in {
			t.Fatalf("%v changed to %v", in, got)
		}
	}
}
