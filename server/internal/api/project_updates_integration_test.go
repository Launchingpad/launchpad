package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/notify"
	"devops/internal/tracker"
)

// LAUN-196: project updates, create / list / read.
func TestProjectUpdates_CreateListGet(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	base := O + "/projects/" + p.ID.String() + "/updates"

	var u1, u2 tracker.ProjectUpdate
	e.must(201, e.do("POST", base, e.chen, map[string]any{"health": "on_track", "body": "all **good**"})).json(t, &u1)
	e.must(201, e.do("POST", base, e.claude, map[string]any{"health": "at_risk"})).json(t, &u2)
	if u1.Health != "on_track" || u1.Body != "all **good**" || u1.ProjectID != p.ID || u1.Author == nil || u1.Author.Username != "chen" {
		t.Fatalf("u1 %+v", u1)
	}
	if u2.Author == nil || u2.Author.Username != "claude" || u2.Body != "" {
		t.Fatalf("u2 %+v", u2)
	}

	var pg page[tracker.ProjectUpdate]
	e.must(200, e.do("GET", base+"?limit=1", e.chen, nil)).json(t, &pg)
	if len(pg.Items) != 1 || pg.Items[0].ID != u2.ID || pg.NextCursor == nil {
		t.Fatalf("page 1 %+v", pg)
	}
	e.must(200, e.do("GET", base+"?limit=1&cursor="+*pg.NextCursor, e.chen, nil)).json(t, &pg)
	if len(pg.Items) != 1 || pg.Items[0].ID != u1.ID || pg.NextCursor != nil {
		t.Fatalf("page 2 %+v", pg)
	}
	var got tracker.ProjectUpdate
	e.must(200, e.do("GET", base+"/"+u1.ID.String(), e.chen, nil)).json(t, &got)
	if got.ID != u1.ID {
		t.Fatalf("get %+v", got)
	}

	// validation
	expectCode(t, e.do("POST", base, e.chen, map[string]any{"health": "great"}), 400, "validation_failed")
	expectCode(t, e.do("POST", base, e.chen, map[string]any{"body": "x"}), 400, "validation_failed")
	expectCode(t, e.do("POST", base, e.chen, map[string]any{"health": "on_track", "body": strings.Repeat("x", 50001)}), 400, "validation_failed")
	expectCode(t, e.do("POST", base, e.chen, map[string]any{"health": "on_track", "nope": 1}), 400, "bad_request")
	expectCode(t, e.do("GET", base+"?includeArchived=true", e.chen, nil), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/projects/"+uuid.NewString()+"/updates", e.chen, map[string]any{"health": "on_track"}), 404, "not_found")
	expectCode(t, e.do("GET", base+"/"+uuid.NewString(), e.chen, nil), 404, "not_found")

	// a read-only token may list but not post
	n, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	e.must(200, e.do("GET", base, n, nil))
	expectCode(t, e.do("POST", base, n, map[string]any{"health": "on_track"}), 403, "insufficient_scope")

	// the event holds ids and the health word, never the body
	if e.eventCount("project_update.created") != 2 {
		t.Fatal("event count")
	}
	if e.countRows(`SELECT count(*) FROM events WHERE type = 'project_update.created' AND payload::text LIKE '%good%'`) != 0 {
		t.Fatal("body leaked into the event")
	}
}

func TestProjectUpdates_Idempotency(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	base := O + "/projects/" + p.ID.String() + "/updates"
	body := map[string]any{"health": "off_track", "body": "late"}
	e.must(201, e.do("POST", base, e.chen, body, "Idempotency-Key", "k1"))
	e.must(201, e.do("POST", base, e.chen, body, "Idempotency-Key", "k1"))
	if n := e.countRows(`SELECT count(*) FROM project_updates`); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

func TestProjectUpdates_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	op := e.createProject(tok, map[string]any{"name": "orbit secret"})
	var ou tracker.ProjectUpdate
	e.must(201, e.do("POST", O+"/projects/"+op.ID.String()+"/updates", tok, map[string]any{"health": "on_track", "body": "secret"})).json(t, &ou)

	A := "/api/v1/orgs/acme"
	expectCode(t, e.do("GET", A+"/projects/"+op.ID.String()+"/updates", tok, nil), 404, "not_found")
	expectCode(t, e.do("POST", A+"/projects/"+op.ID.String()+"/updates", tok, map[string]any{"health": "on_track"}), 404, "not_found")
	expectCode(t, e.do("GET", A+"/projects/"+op.ID.String()+"/updates/"+ou.ID.String(), tok, nil), 404, "not_found")
	// an acme project cannot read an orbit update by id either
	var ap tracker.Project
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "acme"})).json(t, &ap)
	expectCode(t, e.do("GET", A+"/projects/"+ap.ID.String()+"/updates/"+ou.ID.String(), tok, nil), 404, "not_found")
	if e.countRows(`SELECT count(*) FROM project_updates WHERE org_id = $1`, acmeID) != 0 {
		t.Fatal("acme got a row")
	}
}

func TestProjectUpdates_ProjectDeleteCascades(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	e.must(201, e.do("POST", O+"/projects/"+p.ID.String()+"/updates", e.chen, map[string]any{"health": "on_track"}))
	e.must(204, e.do("DELETE", O+"/projects/"+p.ID.String(), e.chen, nil))
	if e.countRows(`SELECT count(*) FROM project_updates`) != 0 {
		t.Fatal("update outlived its project")
	}
}

// LAUN-196 part B: the update schedule is a field of the project.
func TestProjectUpdateSchedule_PatchAndRead(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	if p.UpdateSchedule.Frequency != "none" || p.UpdateSchedule.Weekday != 5 || p.UpdateSchedule.Hour != 9 {
		t.Fatalf("default %+v", p.UpdateSchedule)
	}
	var got tracker.Project
	e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{
		"updateSchedule": map[string]any{"frequency": "biweekly", "weekday": 1, "hour": 16}})).json(t, &got)
	if got.UpdateSchedule != (tracker.UpdateSchedule{Frequency: "biweekly", Weekday: 1, Hour: 16}) || got.Version != p.Version+1 {
		t.Fatalf("patched %+v v%d", got.UpdateSchedule, got.Version)
	}
	// A partial change keeps the rest; the same value changes nothing (no version bump).
	e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"updateSchedule": map[string]any{"hour": 8}})).json(t, &got)
	if got.UpdateSchedule != (tracker.UpdateSchedule{Frequency: "biweekly", Weekday: 1, Hour: 8}) {
		t.Fatalf("partial %+v", got.UpdateSchedule)
	}
	v := got.Version
	e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"updateSchedule": map[string]any{"hour": 8}})).json(t, &got)
	if got.Version != v {
		t.Fatalf("a no-op PATCH bumped the version: %d -> %d", v, got.Version)
	}
	// A schedule and a name together: one version bump.
	e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"name": "P2", "updateSchedule": map[string]any{"frequency": "monthly"}})).json(t, &got)
	if got.Version != v+1 || got.Name != "P2" || got.UpdateSchedule.Frequency != "monthly" {
		t.Fatalf("name + schedule %+v", got)
	}
	for _, bad := range []map[string]any{{"frequency": "daily"}, {"weekday": 7}, {"weekday": -1}, {"hour": 24}, {"nope": 1}} {
		e.must(400, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"updateSchedule": bad}))
	}
	// List and get carry it too.
	var pg page[tracker.Project]
	e.must(200, e.do("GET", O+"/projects", e.chen, nil)).json(t, &pg)
	if len(pg.Items) != 1 || pg.Items[0].UpdateSchedule.Frequency != "monthly" {
		t.Fatalf("list %+v", pg.Items)
	}
	// A read token cannot change it.
	n, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	expectCode(t, e.do("PATCH", O+"/projects/"+p.ID.String(), n, map[string]any{"updateSchedule": map[string]any{"frequency": "none"}}), 403, "insufficient_scope")
}

func TestProjectUpdateSchedule_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	op := e.createProject(tok, map[string]any{"name": "orbit"})
	A := "/api/v1/orgs/acme"
	expectCode(t, e.do("PATCH", A+"/projects/"+op.ID.String(), tok, map[string]any{"updateSchedule": map[string]any{"frequency": "weekly"}}), 404, "not_found")
	var got tracker.Project
	e.must(200, e.do("GET", O+"/projects/"+op.ID.String(), tok, nil)).json(t, &got)
	if got.UpdateSchedule.Frequency != "none" {
		t.Fatalf("another org changed the schedule: %+v", got.UpdateSchedule)
	}
}

// The project reminder is a notification setting like the others, and an agent token is refused.
func TestProjectUpdateDue_SettingsRoute(t *testing.T) {
	e := newEnv(t)
	v := e.settings(e.chen)
	ev := viewEvent(t, v, "project_update_due")
	wantResolved(t, "email", ev.Resolved.Email, "on", notify.SourceDefault)
	wantResolved(t, "inbox", ev.Resolved.Inbox, "on", notify.SourceDefault)
	if len(ev.Teams) != 0 || ev.RequiresFollow {
		t.Fatalf("event %+v", ev)
	}
	e.must(200, e.do("PUT", nsPath, e.chen, map[string]any{"events": map[string]any{"project_update_due": map[string]any{"email": "off"}}}))
	wantResolved(t, "email", viewEvent(t, e.settings(e.chen), "project_update_due").Resolved.Email, "off", notify.SourceGlobal)
	expectCode(t, e.do("PUT", nsTeam, e.chen, map[string]any{"events": map[string]any{"project_update_due": map[string]any{"email": "on"}}}), 400, "validation_failed")
	expectCode(t, e.do("GET", nsPath, e.claude, nil), 403, "forbidden")
}
