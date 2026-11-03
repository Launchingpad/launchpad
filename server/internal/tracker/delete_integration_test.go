package tracker_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/tracker"
)

// LAUN-191: DeleteTeam, DeleteProject and CountTeamIssues never cross an org.
func TestDelete_CrossOrg(t *testing.T) {
	e := newCycEnv(t) // org orbit with team ORB
	ctx := context.Background()
	other, err := e.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, e.q, other.ID, "ACM", "Acme", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, e.q, other.ID, "ACN", "Acme 2", ""); err != nil {
		t.Fatal(err)
	}
	acme := &auth.Org{ID: other.ID, Slug: "acme", Name: "Acme", Role: "admin", Access: auth.TeamAccess{All: true}}
	acmeTeam, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: other.ID, Key: "ACN"})
	if err != nil {
		t.Fatal(err)
	}
	// ACN is empty and not the default: from its own org it could be deleted.
	proj, err := e.svc.CreateProject(ctx, acme, e.chen, tracker.CreateProjectInput{Name: "acme project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateIssue(ctx, acme, e.chen, tracker.CreateIssueInput{Team: "ACM", Title: "acme issue"}); err != nil {
		t.Fatal(err)
	}
	acmeTeamM, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: other.ID, Key: "ACM"})
	if err != nil {
		t.Fatal(err)
	}

	notFound := func(name string, err error) {
		t.Helper()
		var api *httpx.Error
		if !errors.As(err, &api) || api.Status != http.StatusNotFound {
			t.Fatalf("%s: %v, want 404", name, err)
		}
	}
	// Called with the ORBIT org, every acme id and key is "not found".
	notFound("team by key", e.svc.DeleteTeam(ctx, e.org, e.chen, "ACN"))
	notFound("team by id", e.svc.DeleteTeam(ctx, e.org, e.chen, acmeTeam.ID.String()))
	notFound("project", e.svc.DeleteProject(ctx, e.org, e.chen, proj.ID))

	count := func(sql string, args ...any) int {
		var n int
		if err := e.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(`SELECT count(*) FROM teams WHERE org_id = $1`, other.ID) != 2 ||
		count(`SELECT count(*) FROM projects WHERE id = $1`, proj.ID) != 1 {
		t.Fatal("a delete crossed the org")
	}
	// The issue count is per org too: acme's issue is not visible from orbit.
	n, err := e.q.CountTeamIssues(ctx, dbq.CountTeamIssuesParams{OrgID: e.org.ID, TeamID: acmeTeamM.ID})
	if err != nil || n != 0 {
		t.Fatalf("CountTeamIssues across orgs: %d %v", n, err)
	}
	n, err = e.q.CountTeamIssues(ctx, dbq.CountTeamIssuesParams{OrgID: other.ID, TeamID: acmeTeamM.ID})
	if err != nil || n != 1 {
		t.Fatalf("CountTeamIssues in its own org: %d %v", n, err)
	}
}
