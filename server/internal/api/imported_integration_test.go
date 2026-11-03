package api

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/tracker"
)

// TestImportedLinearIDs (T4b): a Linear team or cycle id that the
// importer mapped resolves wherever the API takes a team or a cycle, within
// its own org only. Another org's Linear id is a 404 on a path, a 400 in a
// body or a cycle filter, and an empty page in a team filter, exactly as
// another org's dev-ops id is.
func TestImportedLinearIDs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})

	cycleOf := func(orgID uuid.UUID, key string) (uuid.UUID, uuid.UUID) {
		team, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: orgID, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Hour)
		c, err := e.q.InsertCycle(ctx, dbq.InsertCycleParams{OrgID: orgID, TeamID: team.ID, Number: 7,
			StartsAt: start, EndsAt: start.Add(14 * 24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return team.ID, c.ID
	}
	mapID := func(orgID uuid.UUID, ent, src string, target uuid.UUID) {
		if _, err := e.q.ImportUpsertMapping(ctx, dbq.ImportUpsertMappingParams{OrgID: orgID, EntityType: ent,
			SourceID: src, TargetID: target}); err != nil {
			t.Fatal(err)
		}
	}
	orbTeam, orbCycle := cycleOf(e.orgID, "ORB")
	acmeTeam, acmeCycle := cycleOf(acmeID, "ACM")
	lTeam, lCycle := uuid.NewString(), uuid.NewString()         // orbit's, from Linear
	lAcmeTeam, lAcmeCycle := uuid.NewString(), uuid.NewString() // acme's, from Linear
	mapID(e.orgID, "team", lTeam, orbTeam)
	mapID(e.orgID, "cycle", lCycle, orbCycle)
	mapID(acmeID, "team", lAcmeTeam, acmeTeam)
	mapID(acmeID, "cycle", lAcmeCycle, acmeCycle)

	O := "/api/v1/orgs/orbit"
	t.Run("a Linear team id names the team", func(t *testing.T) {
		var team tracker.Team
		e.must(200, e.do("GET", O+"/teams/"+lTeam, tok, nil)).json(t, &team)
		if team.ID != orbTeam {
			t.Fatalf("team %+v", team)
		}
		e.must(200, e.do("GET", O+"/teams/"+lTeam+"/states", tok, nil))
		// uuid.Parse accepts upper case and braces; the mapping is looked up
		// in its canonical form.
		for _, ref := range []string{strings.ToUpper(lTeam), url.PathEscape("{" + lTeam + "}")} {
			var tm tracker.Team
			e.must(200, e.do("GET", O+"/teams/"+ref, tok, nil)).json(t, &tm)
			if tm.ID != orbTeam {
				t.Fatalf("team %s = %+v", ref, tm)
			}
		}
		var p page[tracker.Cycle]
		e.must(200, e.do("GET", O+"/cycles?team="+lTeam, tok, nil)).json(t, &p)
		if len(p.Items) != 1 || p.Items[0].ID != orbCycle {
			t.Fatalf("cycles?team = %+v", p.Items)
		}
		// Projects: a Linear team id in the create and update bodies, and in
		// the list filter.
		var pr struct {
			ID    uuid.UUID `json:"id"`
			Teams []struct {
				ID uuid.UUID `json:"id"`
			} `json:"teams"`
		}
		e.must(201, e.do("POST", O+"/projects", tok, map[string]any{"name": "Cutover", "teams": []string{lTeam}})).json(t, &pr)
		if len(pr.Teams) != 1 || pr.Teams[0].ID != orbTeam {
			t.Fatalf("project teams %+v", pr.Teams)
		}
		e.must(200, e.do("PATCH", O+"/projects/"+pr.ID.String(), tok, map[string]any{"teams": []string{strings.ToUpper(lTeam)}})).json(t, &pr)
		if len(pr.Teams) != 1 || pr.Teams[0].ID != orbTeam {
			t.Fatalf("patched project teams %+v", pr.Teams)
		}
		var pp page[map[string]any]
		e.must(200, e.do("GET", O+"/projects?team="+lTeam, tok, nil)).json(t, &pp)
		if len(pp.Items) != 1 || pp.Items[0]["id"] != pr.ID.String() {
			t.Fatalf("projects?team=<linear id> = %+v", pp.Items)
		}
		expect400 := e.do("POST", O+"/projects", tok, map[string]any{"name": "x", "teams": []string{lAcmeTeam}})
		if expect400.status != 400 {
			t.Fatalf("another org's Linear team in project teams: %d", expect400.status)
		}
	})
	t.Run("a Linear cycle id names the cycle", func(t *testing.T) {
		var c tracker.Cycle
		e.must(200, e.do("GET", O+"/cycles/"+lCycle, tok, nil)).json(t, &c)
		if c.ID != orbCycle {
			t.Fatalf("cycle %+v", c)
		}
		e.must(200, e.do("GET", O+"/cycles/"+lCycle+"/snapshots", tok, nil))
		e.must(200, e.do("PATCH", O+"/cycles/"+lCycle, tok, map[string]any{"name": "Seven"}))
	})
	var is tracker.Issue
	t.Run("in an issue body and the issue filters", func(t *testing.T) {
		e.must(201, e.do("POST", O+"/issues", tok, map[string]any{"team": lTeam, "title": "x", "cycleId": lCycle})).json(t, &is)
		if is.Team.ID != orbTeam || is.CycleID == nil || *is.CycleID != orbCycle {
			t.Fatalf("created %+v", is)
		}
		var patched tracker.Issue
		e.must(200, e.do("PATCH", O+"/issues/"+is.Identifier, tok, map[string]any{"cycleId": nil}))
		e.must(200, e.do("PATCH", O+"/issues/"+is.Identifier, tok, map[string]any{"cycleId": lCycle})).json(t, &patched)
		if patched.CycleID == nil || *patched.CycleID != orbCycle {
			t.Fatalf("patched cycleId %v", patched.CycleID)
		}
		for _, q := range []string{"?cycle=" + lCycle, "?cycle=" + orbCycle.String(), "?team=" + lTeam} {
			var p page[tracker.Issue]
			e.must(200, e.do("GET", O+"/issues"+q, tok, nil)).json(t, &p)
			if len(p.Items) != 1 || p.Items[0].ID != is.ID {
				t.Fatalf("issues%s = %d items", q, len(p.Items))
			}
		}
	})
	t.Run("another org's Linear ids do not resolve", func(t *testing.T) {
		expect := func(status int, method, path string, body any) {
			t.Helper()
			r := e.do(method, path, tok, body)
			if r.status != status {
				t.Fatalf("%s %s: %d, want %d: %s", method, path, r.status, status, r.body)
			}
			r.errCode(t)
		}
		expect(404, "GET", O+"/teams/"+lAcmeTeam, nil)
		expect(404, "GET", O+"/cycles/"+lAcmeCycle, nil)
		expect(404, "GET", O+"/cycles/"+lAcmeCycle+"/snapshots", nil)
		expect(404, "PATCH", O+"/cycles/"+lAcmeCycle, map[string]any{"name": "x"})
		expect(404, "GET", "/api/v1/orgs/acme/cycles/"+lCycle, nil)
		expect(400, "POST", O+"/issues", map[string]any{"team": lAcmeTeam, "title": "x"})
		expect(400, "POST", O+"/issues", map[string]any{"team": "ORB", "title": "x", "cycleId": lAcmeCycle})
		expect(400, "PATCH", O+"/issues/"+is.Identifier, map[string]any{"cycleId": lAcmeCycle})
		expect(400, "GET", O+"/issues?cycle="+lAcmeCycle, nil)
		expect(404, "GET", O+"/cycles/"+uuid.NewString(), nil)
		// A mapping whose cycle was deleted names nothing.
		gone := uuid.NewString()
		mapID(e.orgID, "cycle", gone, uuid.New())
		expect(404, "GET", O+"/cycles/"+gone, nil)
		expect(400, "GET", O+"/issues?cycle="+gone, nil)
		expect(400, "POST", O+"/issues", map[string]any{"team": "ORB", "title": "x", "cycleId": gone})
		for _, path := range []string{O + "/issues?team=" + lAcmeTeam, O + "/cycles?team=" + lAcmeTeam} {
			var p page[map[string]any]
			e.must(200, e.do("GET", path, tok, nil)).json(t, &p)
			if len(p.Items) != 0 {
				t.Fatalf("%s lists %d items from acme", path, len(p.Items))
			}
		}
	})
}
