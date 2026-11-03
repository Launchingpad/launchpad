package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/gitlab"
	"devops/internal/tracker"
)

// LAUN-87 (T17 #9): the deliveries ring, written by both webhook handlers and
// read by GET /integrations/{id}/deliveries and the lastDelivery summary.

type ringClock struct{ ns atomic.Int64 }

func (c *ringClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *ringClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// ringEnv is a GitHub-capable env whose service clock the test controls (the
// throttle of the unbound rows reads it).
func newRingEnv(t *testing.T) (*ghEnv, *ringClock) {
	t.Helper()
	g := newGHEnv(t, 0, 0, true, envOpts{logLevel: -4})
	c := &ringClock{}
	c.ns.Store(time.Now().UnixNano())
	g.srv2.svc.SetClock(c.now)
	return g, c
}

func (e *env) deliveries(token, integ string, status int) []tracker.GitDelivery {
	e.t.Helper()
	var page struct {
		Items      []tracker.GitDelivery `json:"items"`
		NextCursor *string               `json:"nextCursor"`
	}
	e.must(status, e.do("GET", "/api/v1/orgs/orbit/integrations/"+integ+"/deliveries", token, nil)).json(e.t, &page)
	return page.Items
}

func (e *env) integration(integ uuid.UUID) tracker.GitIntegration {
	e.t.Helper()
	var gi tracker.GitIntegration
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/integrations/"+integ.String(), e.chen, nil)).json(e.t, &gi)
	return gi
}

func TestDeliveries_GitLabRows(t *testing.T) {
	g, _ := newRingEnv(t)
	integ := g.glSetup("orbit", g.chen)
	if gi := g.integration(integ); gi.LastDelivery != nil {
		t.Fatalf("lastDelivery before any delivery = %+v, want null", gi.LastDelivery)
	}
	is := g.createIssue(g.chen, map[string]any{"title": "ring"})
	body := glFixture(t, "mr_open.json", naming(is.Identifier, "a secret description text"))
	uid := uuid.NewString()

	g.hookResult(g.hook("orbit", glSecret, uid, body), http.StatusOK)                  // processed
	g.hookResult(g.hook("orbit", glSecret, uid, body), http.StatusOK)                  // duplicate
	g.hook("orbit", glSecret, uuid.NewString(), body, gitlab.HeaderEvent, "Push Hook") // 202 ignored
	if r := g.hook("orbit", glSecret, "", body); r.status != http.StatusBadRequest {
		t.Fatalf("no uuid: %d", r.status)
	}
	if r := g.hook("orbit", glSecret, uuid.NewString(), []byte("{not json")); r.status != http.StatusBadRequest {
		t.Fatalf("bad json: %d", r.status)
	}

	got := g.deliveries(g.chen, integ.String(), http.StatusOK)
	var results []string
	for _, d := range got {
		results = append(results, fmt.Sprintf("%s/%d", d.Result, d.HTTPStatus))
		if d.IntegrationID == nil || *d.IntegrationID != integ || d.Provider != "gitlab" {
			t.Fatalf("row = %+v", d)
		}
	}
	// Newest first.
	want := "bad_request/400 bad_request/400 ignored/202 duplicate/200 processed/200"
	if strings.Join(results, " ") != want {
		t.Fatalf("results = %v, want %s", results, want)
	}
	if got[4].Event != gitlab.MergeRequestHook || got[4].RepoKey == nil || *got[4].RepoKey != "42" {
		t.Fatalf("processed row = %+v", got[4])
	}
	if got[0].ErrorCode == nil || *got[0].ErrorCode != "bad_request" {
		t.Fatalf("error code = %v", got[0].ErrorCode)
	}
	gi := g.integration(integ)
	if gi.LastDelivery == nil || gi.LastDelivery.Result != "bad_request" || gi.LastDelivery.HTTPStatus != 400 {
		t.Fatalf("lastDelivery = %+v", gi.LastDelivery)
	}
	var list struct {
		Items []tracker.GitIntegration `json:"items"`
	}
	g.must(http.StatusOK, g.do("GET", "/api/v1/orgs/orbit/integrations", g.chen, nil)).json(t, &list)
	if len(list.Items) != 1 || list.Items[0].LastDelivery == nil || list.Items[0].LastDelivery.Result != "bad_request" {
		t.Fatalf("list lastDelivery = %+v", list.Items)
	}

	// The table holds nothing of the payload, the secret or the caller.
	var dump string
	if err := g.pool.QueryRow(context.Background(), `SELECT string_agg(t::text, ' ') FROM git_webhook_deliveries t`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{glSecret, "a secret description", "Fix login redirect", is.Identifier, "chen/", "127.0.0.1", "Go-http-client"} {
		if strings.Contains(dump, bad) {
			t.Fatalf("ring holds %q: %s", bad, dump)
		}
	}
	if strings.Contains(g.logs.String(), glSecret) {
		t.Fatal("secret in the log")
	}
}

func TestDeliveries_GitHubRowsAndBadSignatureThrottle(t *testing.T) {
	g, clock := newRingEnv(t)
	integ := g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "ring"})
	opened := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))

	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), opened), http.StatusOK)
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "ping.json", nil), "X-GitHub-Event", "ping"), http.StatusOK)
	g.ghHook("orbit", ghSecret, uuid.NewString(), opened, "X-GitHub-Event", "push") // 202 ignored
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_fork_opened.json", ghNaming(is.Identifier, ""))), http.StatusOK)

	// Bad signatures: byte-identical 401, one row a minute.
	first := g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), opened)
	if first.status != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", first.status)
	}
	for i := 0; i < 20; i++ {
		r := g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), opened)
		if r.status != first.status || string(r.body) != string(first.body) {
			t.Fatalf("401 differs: %d %s vs %d %s", r.status, r.body, first.status, first.body)
		}
	}
	// A request with no signature header is refused the same way.
	if r := g.ghHook("orbit", "", uuid.NewString(), opened); r.status != http.StatusUnauthorized || string(r.body) != string(first.body) {
		t.Fatalf("no signature: %d %s", r.status, r.body)
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id IS NULL AND result = 'bad_signature'`); n != 1 {
		t.Fatalf("bad_signature rows = %d, want 1", n)
	}
	if d := g.srv2.svc.DroppedGitDeliveries(); d != 21 {
		t.Fatalf("dropped = %d, want 21", d)
	}
	clock.advance(61 * time.Second)
	g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), opened)
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id IS NULL AND result = 'bad_signature'`); n != 2 {
		t.Fatalf("bad_signature rows after a minute = %d, want 2", n)
	}
	// An unknown org and an org with no integration write nothing.
	g.ghHook("no-such-org", ghSecret, uuid.NewString(), opened)
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE org_id <> $1`, g.orgID); n != 0 {
		t.Fatalf("rows of other orgs = %d", n)
	}

	got := g.deliveries(g.chen, integ.String(), http.StatusOK)
	var seq []string
	for _, d := range got {
		seq = append(seq, d.Result+"/"+d.Event)
		if d.IntegrationID == nil && d.Result != "bad_signature" {
			t.Fatalf("unbound row %+v", d)
		}
	}
	if len(got) != 6 {
		t.Fatalf("rows = %d (%v), want 6", len(got), seq)
	}
	if got[len(got)-1].Result != "processed" || got[len(got)-1].RepoKey == nil || *got[len(got)-1].RepoKey != ghRepoKey {
		t.Fatalf("oldest row = %+v", got[len(got)-1])
	}
}

func TestDeliveries_RingKeeps100AndDropsOldest(t *testing.T) {
	g, clock := newRingEnv(t)
	integ := g.setup()
	svc := g.srv2.svc
	ctx := context.Background()
	rec := func(i int, id *uuid.UUID) tracker.GitDeliveryRecord {
		return tracker.GitDeliveryRecord{OrgID: g.orgID, IntegrationID: id, Provider: "github", Result: tracker.DeliveryProcessed,
			Event: "pull_request", RepoKey: fmt.Sprint(i), HTTPStatus: 200}
	}
	for i := 1; i <= 130; i++ {
		if err := svc.RecordGitDelivery(ctx, rec(i, &integ)); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id = $1`, integ); n != 100 {
		t.Fatalf("ring = %d rows, want 100", n)
	}
	var oldest string
	if err := g.pool.QueryRow(ctx, `SELECT repo_key FROM git_webhook_deliveries WHERE integration_id = $1 ORDER BY received_at, id LIMIT 1`, integ).Scan(&oldest); err != nil || oldest != "31" {
		t.Fatalf("oldest = %q (%v), want 31", oldest, err)
	}
	// Concurrent writers keep the bound.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.RecordGitDelivery(ctx, rec(1000+i, &integ)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id = $1`, integ); n != 100 {
		t.Fatalf("ring after concurrent writes = %d, want 100", n)
	}
	// The route returns the newest 100, newest first.
	if got := g.deliveries(g.chen, integ.String(), http.StatusOK); len(got) != 100 {
		t.Fatalf("route rows = %d", len(got))
	}
	// Unbound rows are trimmed per org and provider, and do not evict the
	// integration's rows.
	for i := 1; i <= 120; i++ {
		clock.advance(2 * time.Minute) // past the throttle
		if err := svc.RecordGitDelivery(ctx, rec(i, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id IS NULL`); n != 100 {
		t.Fatalf("unbound ring = %d, want 100", n)
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id = $1`, integ); n != 100 {
		t.Fatalf("integration ring = %d after unbound writes, want 100", n)
	}
	// An unbound burst inside one minute writes one row.
	before := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id IS NULL AND http_status = 413`)
	for i := 0; i < 50; i++ {
		r := rec(i, nil)
		r.Result, r.HTTPStatus = tracker.DeliveryError, 413
		_ = svc.RecordGitDelivery(ctx, r)
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_deliveries WHERE integration_id IS NULL AND http_status = 413`); n-before != 1 {
		t.Fatalf("burst wrote %d rows, want 1", n-before)
	}
}

func TestDeliveries_RingWriteFailureNeverChangesTheAnswer(t *testing.T) {
	g, _ := newRingEnv(t)
	g.setup()
	ctx := context.Background()
	is := g.createIssue(g.chen, map[string]any{"title": "ring"})
	opened := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	if _, err := g.pool.Exec(ctx, `ALTER TABLE git_webhook_deliveries RENAME TO git_webhook_deliveries_off`); err != nil {
		t.Fatal(err)
	}
	r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), opened), http.StatusOK)
	if r.Status != tracker.WebhookProcessed {
		t.Fatalf("result = %+v", r)
	}
	if r := g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), opened); r.status != http.StatusUnauthorized {
		t.Fatalf("bad signature with the ring down: %d", r.status)
	}
	if !strings.Contains(g.logs.String(), "delivery_write_failed") {
		t.Fatal("a failed ring write is not logged")
	}
	if _, err := g.pool.Exec(ctx, `ALTER TABLE git_webhook_deliveries_off RENAME TO git_webhook_deliveries`); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveries_RouteAccessAndCrossOrg(t *testing.T) {
	g, _ := newRingEnv(t)
	integ := g.setup()
	id := integ.String()
	path := "/api/v1/orgs/orbit/integrations/" + id + "/deliveries"

	g.must(http.StatusUnauthorized, g.do("GET", path, "", nil))
	// A read token of a plain member: 403 (the route needs the admin scope and role).
	m := g.newHuman("ringmember", "ringmember@example.com", "member")
	tok, _ := g.newToken(m.ID, tokenOpts{pin: &g.orgID, scopes: []string{"read"}})
	g.must(http.StatusForbidden, g.do("GET", path, tok, nil))
	g.must(http.StatusNotFound, g.do("GET", "/api/v1/orgs/orbit/integrations/"+uuid.NewString()+"/deliveries", g.chen, nil))
	g.must(http.StatusNotFound, g.do("GET", "/api/v1/orgs/orbit/integrations/not-a-uuid/deliveries", g.chen, nil))

	// Another org's admin cannot read this org's ring, by either org path.
	org2 := g.newOrg("acme", "ACME")
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	if err := g.srv2.svc.RecordGitDelivery(context.Background(), tracker.GitDeliveryRecord{
		OrgID: g.orgID, IntegrationID: &integ, Provider: "github", Result: tracker.DeliveryProcessed, HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	g.must(http.StatusNotFound, g.do("GET", "/api/v1/orgs/acme/integrations/"+id+"/deliveries", tok2, nil))
	g.must(http.StatusNotFound, g.do("GET", path, tok2, nil))
	// And the other org's list shows no lastDelivery of ours.
	var list struct {
		Items []tracker.GitIntegration `json:"items"`
	}
	g.must(http.StatusOK, g.do("GET", "/api/v1/orgs/acme/integrations", tok2, nil)).json(t, &list)
	if len(list.Items) != 0 {
		t.Fatalf("acme sees %d integrations", len(list.Items))
	}
}
