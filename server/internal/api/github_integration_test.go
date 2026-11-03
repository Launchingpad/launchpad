package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/github"
	"devops/internal/platform"
	"devops/internal/tracker"
)

// LAUN-5 (T17): the GitHub webhook against a real database, driven by the
// fixtures in internal/github/testdata (hand-written, invented data, not
// recorded deliveries; see its README.md). The integration rows are inserted
// directly: there is no create route before LAUN-7.

const (
	ghSecret   = "ghwh-test-secret-0123456789abcdefABCDEF"
	ghSecret2  = "ghwh-second-secret-9876543210fedcbaFEDCBA"
	ghRepoKey  = "4242" // repository.id in every fixture
	ghBaseURL  = "https://github.com"
	ghDelivery = "72d3162e-cc78-11e3-81ab-4c9367dc0958"
)

func ghKeyring(t *testing.T, b byte) *platform.GitKeyring {
	t.Helper()
	kr, err := platform.NewGitKeyring(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// ghEnv is a test server with the git keyring set and the webhook limits
// given (zero: the defaults). srv is the Server, for the budget.
type ghEnv struct {
	*env
	srv2 *Server
	kr   *platform.GitKeyring
	rows int
}

func newGHEnv(t *testing.T, budget int64, deadline time.Duration, sealer bool, o envOpts) *ghEnv {
	t.Helper()
	g := &ghEnv{kr: ghKeyring(t, 1)}
	o.configure = func(_ *env, s *Server) {
		g.srv2 = s
		if sealer {
			s.svc.SetGitSealer(g.kr)
		}
		if budget > 0 {
			s.SetGitWebhookLimits(budget, deadline)
		}
	}
	g.env = newEnvWith(t, o)
	return g
}

type ghRow struct {
	org      uuid.UUID
	secret   string
	next     string // a pending secret
	enabled  bool
	sealWith *platform.GitKeyring // nil: the env's keyring
	noBlob   bool                 // webhook_secret_enc stays NULL
}

// ghInsert inserts a GitHub integration and maps repo ghRepoKey to it.
func (g *ghEnv) ghInsert(r ghRow) uuid.UUID {
	g.t.Helper()
	id := uuid.Must(uuid.NewV7())
	g.rows++
	base := ghBaseURL // one integration per (org, provider, base URL)
	if g.rows > 1 {
		base = "https://ghe" + strings.Repeat("x", g.rows) + ".example.com"
	}
	kr := r.sealWith
	if kr == nil {
		kr = g.kr
	}
	var blob, nextBlob, nextHash []byte
	if !r.noBlob {
		blob = kr.SealFor("webhook_secret_enc", id.String(), []byte(r.secret))
	}
	if r.next != "" {
		nextBlob = kr.SealFor("webhook_secret_next_enc", id.String(), []byte(r.next))
		h := sha256.Sum256([]byte(r.next))
		nextHash = h[:]
	}
	h := sha256.Sum256([]byte(r.secret))
	ctx := context.Background()
	if _, err := g.pool.Exec(ctx, `INSERT INTO git_integrations
		(id, org_id, base_url, webhook_secret_hash, enabled, provider, mode, webhook_secret_enc, webhook_secret_next_enc, webhook_secret_next_hash)
		VALUES ($1, $2, $3, $4, $5, 'github', 'webhook_only', $6, $7, $8)`,
		id, r.org, base, h[:], r.enabled, blob, nextBlob, nextHash); err != nil {
		g.t.Fatal(err)
	}
	if _, err := g.pool.Exec(ctx, `INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path) VALUES ($1, $2, $3, 'example-org/example-app')`,
		r.org, id, ghRepoKey); err != nil {
		g.t.Fatal(err)
	}
	return id
}

func (g *ghEnv) setup() uuid.UUID {
	return g.ghInsert(ghRow{org: g.orgID, secret: ghSecret, enabled: true})
}

func ghFixture(t *testing.T, name string, mut func(m map[string]any)) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "github", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ghNaming makes the PR name ident in its branch and title, with description desc.
func ghNaming(ident, desc string) func(map[string]any) {
	return func(m map[string]any) {
		pr := m["pull_request"].(map[string]any)
		pr["head"].(map[string]any)["ref"] = "chen/" + strings.ToLower(ident) + "-fix-login-redirect"
		pr["title"] = ident + ": Fix login redirect"
		pr["body"] = desc
	}
}

func (g *ghEnv) ghURL(org string) string {
	return g.env.srv.URL + "/api/v1/orgs/" + org + "/integrations/github/webhook"
}

// ghHook sends one delivery signed with secret (no signature header when secret
// is ""). headers are replacement key/value pairs; a value of "" removes it.
func (g *ghEnv) ghHook(org, secret, delivery string, body []byte, headers ...string) resp {
	g.t.Helper()
	req, err := http.NewRequest("POST", g.ghURL(org), bytes.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.HeaderEvent, github.EventPullRequest)
	if secret != "" {
		req.Header.Set(github.HeaderSignature, github.Sign([]byte(secret), body))
	}
	if delivery != "" {
		req.Header.Set(github.HeaderDelivery, delivery)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	return g.sendHook(req)
}

func (g *ghEnv) ghResult(r resp, status int) hookOutcome {
	g.t.Helper()
	return g.hookResult(r, status)
}

func githubSources(is tracker.Issue) int {
	n := 0
	for _, sc := range is.StateHistory {
		if sc.Source == "github" {
			n++
		}
	}
	return n
}

// AC: ready_for_review moves a branch-named issue to In Review; a closed,
// merged PR moves it to In Testing. The link, the history row and the event
// say github.
func TestGithubWebhook_ReadyThenMerge(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "Fix login redirect"})
	g.setState(is.Identifier, "Todo")

	r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_ready_for_review.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 1 || r.Linked[0] != is.Identifier || len(r.Moved) != 1 {
		t.Fatalf("ready result = %+v", r)
	}
	got := g.issueFull(is.Identifier)
	if got.State.Name != "In Review" {
		t.Fatalf("after ready_for_review: state %q, want In Review", got.State.Name)
	}
	last := got.StateHistory[len(got.StateHistory)-1]
	if last.Source != "github" || last.Actor != nil || last.FromStateName == nil || *last.FromStateName != "Todo" {
		t.Fatalf("history row = %+v, want Todo -> In Review from github with no actor", last)
	}
	prURL := "https://github.com/example-org/example-app/pull/17"
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want the PR link", got.Attachments)
	}
	a := got.Attachments[0]
	if a.Kind != "link" || a.URL == nil || *a.URL != prURL || a.Title != "PR #17: "+is.Identifier+": Fix login redirect" ||
		a.Subtitle != "example-org/example-app" || a.Creator != nil {
		t.Fatalf("link attachment = %+v", a)
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE org_id = $1 AND issue_id = $2 AND repo_key = $3 AND number = 17 AND state = 'opened' AND url = $4`,
		g.orgID, is.ID, ghRepoKey, prURL); n != 1 {
		t.Fatalf("mr_links rows = %d, want 1 opened", n)
	}
	if n := g.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'issue.updated' AND entity_id = $2 AND actor_id IS NULL
		AND payload->>'source' = 'github' AND payload->'mergeRequest'->>'number' = '17' AND payload->'mergeRequest'->>'repoKey' = $3`,
		g.orgID, is.ID, ghRepoKey); n != 1 {
		t.Fatalf("github issue.updated events = %d, want 1", n)
	}
	if n := g.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'attachment.created' AND payload->>'issueId' = $2 AND payload->>'source' = 'github'`,
		g.orgID, is.ID.String()); n != 1 {
		t.Fatalf("attachment.created events = %d, want 1", n)
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND event_type = 'pull_request' AND status = 'processed'
		AND event_uuid LIKE 'gh:%:r4242:pr17:ready'`, g.orgID); n != 1 {
		t.Fatalf("processed delivery rows = %d, want 1", n)
	}

	r = g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_closed_merged.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 || len(r.Linked) != 0 {
		t.Fatalf("merge result = %+v, want moved and nothing newly linked", r)
	}
	got = g.issueFull(is.Identifier)
	if got.State.Name != "In Testing" || githubSources(got) != 2 || len(got.Attachments) != 1 {
		t.Fatalf("after merge: %q, %d github history rows, %d attachments; want In Testing, 2, 1", got.State.Name, githubSources(got), len(got.Attachments))
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND state = 'merged'`, is.ID); n != 1 {
		t.Fatalf("merged mr_links = %d, want 1", n)
	}
}

func TestGithubWebhook_OpenedDraftAndClosed(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "draft then close"})
	r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_opened_draft.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if len(r.Linked) != 1 || len(r.Moved) != 0 {
		t.Fatalf("draft open = %+v, want linked and not moved", r)
	}
	if got := g.issueFull(is.Identifier); got.State.Name != "Backlog" {
		t.Fatalf("a draft PR moved the issue to %q", got.State.Name)
	}
	// Closed unmerged closes the link and moves nothing.
	r = g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_closed_unmerged.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 0 {
		t.Fatalf("close = %+v", r)
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND state = 'closed'`, is.ID); n != 1 {
		t.Fatalf("closed links = %d, want 1", n)
	}
	// An issue named only in the description is linked and keeps its state.
	other := g.createIssue(g.chen, map[string]any{"title": "described only"})
	r = g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_opened.json", func(m map[string]any) {
		pr := m["pull_request"].(map[string]any)
		pr["body"] = "Refs " + other.Identifier
		m["number"], pr["number"] = 18, 18
	})), http.StatusOK)
	if len(r.Linked) != 1 || len(r.Moved) != 0 {
		t.Fatalf("description-only = %+v", r)
	}
}

// An action that asks for nothing (a label) answers 200 ignored and writes
// nothing. A draft, edit or push (converted_to_draft, edited, synchronize) is a
// refresh since LAUN-83: it updates the link row, links nothing and moves
// nothing.
func TestGithubWebhook_IgnoredWriteNothing(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "ignored actions"})
	g.setState(is.Identifier, "Todo")
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_opened_draft.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	before := g.glSnapshot(is.Identifier)
	r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_labeled.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookIgnored || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("pr_labeled.json = %+v, want ignored", r)
	}
	if after := g.glSnapshot(is.Identifier); after != before {
		t.Fatalf("a label wrote something:\n before %+v\n after  %+v", before, after)
	}
	for _, fx := range []string{"pr_converted_to_draft.json", "pr_synchronize.json", "pr_edited.json"} {
		r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, fx, ghNaming(is.Identifier, ""))), http.StatusOK)
		if r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
			t.Fatalf("%s = %+v, want processed with nothing linked or moved", fx, r)
		}
		after := g.glSnapshot(is.Identifier)
		if after.mrLinks != before.mrLinks || after.attachments != before.attachments || after.stateChanges != before.stateChanges ||
			after.state != before.state || after.version != before.version {
			t.Fatalf("%s linked or moved something:\n before %+v\n after  %+v", fx, before, after)
		}
	}
}

// AC: a redelivered GUID is duplicate and changes nothing.
func TestGithubWebhook_Redelivery(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "redeliver"})
	open := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, open), http.StatusOK)
	g.setState(is.Identifier, "In Progress") // a person moves it back
	before := g.glSnapshot(is.Identifier)
	r := g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, open), http.StatusOK)
	if r.Status != tracker.WebhookDuplicate || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("redelivery = %+v, want duplicate", r)
	}
	if after := g.glSnapshot(is.Identifier); after != before || after.state != "In Progress" {
		t.Fatalf("a redelivery changed something: %+v -> %+v", before, after)
	}
	// The same GUID with another action is another event.
	r = g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, ghFixture(t, "pr_closed_merged.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 {
		t.Fatalf("merge under the same GUID = %+v", r)
	}
}

// AC (open point 16): a fork PR, or one whose head repo is null, is 200 ignored
// and writes nothing, also on merge.
func TestGithubWebhook_ForkPRsIgnored(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "fork"})
	before := g.glSnapshot(is.Identifier)
	for _, fx := range []string{"pr_fork_opened.json", "pr_fork_closed_merged.json", "pr_deleted_fork_opened.json"} {
		r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, fx, ghNaming(is.Identifier, ""))), http.StatusOK)
		if r.Status != tracker.WebhookIgnored || len(r.Linked) != 0 || len(r.Moved) != 0 {
			t.Fatalf("%s = %+v, want ignored", fx, r)
		}
	}
	if after := g.glSnapshot(is.Identifier); after != before {
		t.Fatalf("a fork PR wrote something:\n before %+v\n after  %+v", before, after)
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE org_id = $1`, g.orgID); n != 0 {
		t.Fatalf("mr_links = %d", n)
	}
}

func TestGithubWebhook_EventTypesAndUnmapped(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{logLevel: slog.LevelDebug})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "types"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, "zebra-private-description"))

	if r := g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, ghFixture(t, "ping.json", nil), github.HeaderEvent, "ping"), http.StatusOK); r.Status != tracker.WebhookIgnored {
		t.Fatalf("ping = %+v", r)
	}
	// A ping needs no delivery header; a push is 202 ignored.
	if r := g.ghResult(g.ghHook("orbit", ghSecret, "", body, github.HeaderEvent, "push"), http.StatusAccepted); r.Status != tracker.WebhookIgnored {
		t.Fatalf("push = %+v", r)
	}
	// An unmapped repository is 202; nothing is kept; a resend after mapping applies.
	other := ghFixture(t, "pr_opened.json", func(m map[string]any) {
		ghNaming(is.Identifier, "zebra-private-description")(m)
		m["repository"].(map[string]any)["id"] = 999
		m["pull_request"].(map[string]any)["head"].(map[string]any)["repo"].(map[string]any)["id"] = 999
		m["pull_request"].(map[string]any)["base"].(map[string]any)["repo"].(map[string]any)["id"] = 999
	})
	if r := g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, other), http.StatusAccepted); r.Status != tracker.WebhookUnmapped {
		t.Fatalf("unmapped = %+v", r)
	}
	logs := g.logs.String()
	if !strings.Contains(logs, "unmapped repository") || !strings.Contains(logs, "repo_key=999") {
		t.Fatalf("no unmapped log line with the repo id:\n%s", logs)
	}
	for _, s := range []string{"zebra-private", "example-app", "example-dev", "pull/17", ghSecret} {
		if strings.Contains(logs, s) {
			t.Fatalf("content %q is in the log", s)
		}
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("event rows = %d, want 0 for ping, push and unmapped", n)
	}
}

func TestGithubWebhook_BadRequests(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "bad"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	// Signed, but wrong: 400, nothing kept.
	for name, r := range map[string]resp{
		"no delivery header": g.ghHook("orbit", ghSecret, "", body),
		"long delivery":      g.ghHook("orbit", ghSecret, strings.Repeat("a", 101), body),
		"space in delivery":  g.ghHook("orbit", ghSecret, "a b", body),
		"not json":           g.ghHook("orbit", ghSecret, ghDelivery, []byte("{nope")),
		"no action":          g.ghHook("orbit", ghSecret, ghDelivery, []byte(`{"number":1,"repository":{"id":4242}}`)),
		"empty body":         g.ghHook("orbit", ghSecret, ghDelivery, nil),
	} {
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("400s kept %d event rows", n)
	}
}

// AC: a bad signature, a missing header and an unknown org give the same 401,
// before the body is parsed. Disabled, other-org and no-integration cases too.
func TestGithubWebhook_Auth(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{logLevel: slog.LevelDebug})
	is := g.createIssue(g.chen, map[string]any{"title": "auth"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))

	var want []byte
	unauth := func(name string, r resp) {
		t.Helper()
		if r.status != http.StatusUnauthorized || r.errCode(t) != "unauthenticated" {
			t.Fatalf("%s: %d %s, want 401 unauthenticated", name, r.status, r.body)
		}
		if want == nil {
			want = r.body
		} else if !bytes.Equal(want, r.body) {
			t.Fatalf("%s: body %s differs from %s", name, r.body, want)
		}
	}
	unauth("no integration", g.ghHook("orbit", ghSecret, uuid.NewString(), body))
	id := g.setup()
	unauth("unknown org", g.ghHook("no-such-org", ghSecret, uuid.NewString(), body))
	unauth("wrong secret", g.ghHook("orbit", ghSecret+"x", uuid.NewString(), body))
	unauth("no signature", g.ghHook("orbit", "", uuid.NewString(), body))
	unauth("sha1 form", g.ghHook("orbit", ghSecret, uuid.NewString(), body, github.HeaderSignature, "sha1="+strings.Repeat("a", 64)))
	unauth("short signature", g.ghHook("orbit", ghSecret, uuid.NewString(), body, github.HeaderSignature, "sha256=abcd"))
	unauth("signature of another body", g.ghHook("orbit", ghSecret, uuid.NewString(), body,
		github.HeaderSignature, github.Sign([]byte(ghSecret), append([]byte(" "), body...))))
	unauth("garbage body", g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), []byte("{not json")))
	unauth("no delivery", g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", "", body))
	unauth("push event", g.ghHook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), body, github.HeaderEvent, "push"))
	unauth("bearer token instead", g.ghHook("orbit", "", uuid.NewString(), body, "Authorization", "Bearer "+g.chen))
	unauth("the GitLab token header", g.ghHook("orbit", "", uuid.NewString(), body, "X-Gitlab-Token", ghSecret))
	unauth("idempotency key", g.ghHook("orbit", "bad", uuid.NewString(), body, "Idempotency-Key", "k1"))
	if n := g.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("refused deliveries left %d event rows", n)
	}
	if got := g.issueFull(is.Identifier); got.State.Name != "Backlog" {
		t.Fatalf("a refused delivery moved the issue to %q", got.State.Name)
	}

	// Cross-org: another org's integration (same repo key) and its secret.
	org2 := g.newOrg("acme", "ACME")
	g.ghInsert(ghRow{org: org2, secret: ghSecret2, enabled: true})
	unauth("org 2's secret at org 1", g.ghHook("orbit", ghSecret2, uuid.NewString(), body))
	unauth("org 1's secret at org 2", g.ghHook("acme", ghSecret, uuid.NewString(), body))
	// A delivery for org 2 naming org 1's issue links nothing (team keys are per org).
	r := g.ghResult(g.ghHook("acme", ghSecret2, uuid.NewString(), body), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("cross-org delivery = %+v, want nothing linked", r)
	}
	if got := g.issueFull(is.Identifier); got.State.Name != "Backlog" || len(got.Attachments) != 0 {
		t.Fatalf("org 2's delivery changed org 1's issue: %q", got.State.Name)
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE org_id = $1`, g.orgID); n != 0 {
		t.Fatalf("org 1 mr_links = %d", n)
	}

	// Disabled: refused.
	if _, err := g.pool.Exec(context.Background(), `UPDATE git_integrations SET enabled = false, updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	unauth("disabled integration", g.ghHook("orbit", ghSecret, uuid.NewString(), body))
}

// A GitLab integration is never a candidate for a GitHub signature, and the
// other way round.
func TestGithubWebhook_ProviderSeparation(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	is := g.createIssue(g.chen, map[string]any{"title": "providers"})
	g.glSetup("orbit", g.chen) // a GitLab integration with glSecret
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	if r := g.ghHook("orbit", glSecret, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("a GitLab secret signed a GitHub delivery: %d", r.status)
	}
	g.setup()
	if r := g.hook("orbit", ghSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))); r.status != http.StatusUnauthorized {
		t.Fatalf("a GitHub secret passed the GitLab check: %d", r.status)
	}
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), body), http.StatusOK)
}

// Rotation (T17 #11): the current and the pending secret both verify; a change
// of the row (updated_at) is seen at once through the cache.
func TestGithubWebhook_PendingSecretAndCache(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	id := g.ghInsert(ghRow{org: g.orgID, secret: ghSecret, next: ghSecret2, enabled: true})
	is := g.createIssue(g.chen, map[string]any{"title": "rotation"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	for _, s := range []string{ghSecret, ghSecret2, ghSecret} { // the third one is served from the cache
		if r := g.ghHook("orbit", s, uuid.NewString(), body); r.status != http.StatusOK {
			t.Fatalf("secret %q: %d %s", s[:6], r.status, r.body)
		}
	}
	// Commit: the pending secret becomes the current one (re-sealed under the
	// current column's AAD), updated_at moves.
	if _, err := g.pool.Exec(context.Background(), `UPDATE git_integrations SET webhook_secret_enc = $2, webhook_secret_next_enc = NULL,
		webhook_secret_next_hash = NULL, updated_at = now() + interval '1 second' WHERE id = $1`,
		id, g.kr.SealFor("webhook_secret_enc", id.String(), []byte(ghSecret2))); err != nil {
		t.Fatal(err)
	}
	if r := g.ghHook("orbit", ghSecret, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("the old secret still verifies after the swap: %d", r.status)
	}
	if r := g.ghHook("orbit", ghSecret2, uuid.NewString(), body); r.status != http.StatusOK {
		t.Fatalf("the new secret: %d", r.status)
	}
	// ForgetGitSecrets drops the entry without a row change.
	g.srv2.svc.ForgetGitSecrets(id)
	if r := g.ghHook("orbit", ghSecret2, uuid.NewString(), body); r.status != http.StatusOK {
		t.Fatalf("after forget: %d", r.status)
	}
}

// AC: unreadable sealed values give 401 and a log line with only our code;
// one unreadable integration never stops the others.
func TestGithubWebhook_UnreadableSecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sealer bool
		row    ghRow
	}{
		{"another key", true, ghRow{sealWith: ghKeyring(t, 2)}},
		{"no key on the server", false, ghRow{}},
		{"null blob", true, ghRow{noBlob: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGHEnv(t, 0, 0, tc.sealer, envOpts{logLevel: slog.LevelDebug})
			row := tc.row
			row.org, row.secret, row.enabled = g.orgID, ghSecret, true
			bad := g.ghInsert(row)
			is := g.createIssue(g.chen, map[string]any{"title": "unreadable"})
			body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
			sig := github.Sign([]byte(ghSecret), body)
			r := g.ghHook("orbit", ghSecret, uuid.NewString(), body)
			if r.status != http.StatusUnauthorized || r.errCode(t) != "unauthenticated" {
				t.Fatalf("unreadable: %d %s, want the plain 401", r.status, r.body)
			}
			logs := g.logs.String()
			if !strings.Contains(logs, "code=secret_unreadable") || !strings.Contains(logs, "integration_id="+bad.String()) {
				t.Fatalf("no secret_unreadable log line:\n%s", logs)
			}
			for _, s := range []string{ghSecret, strings.TrimPrefix(sig, "sha256="), "decrypt", "cipher", "message authentication"} {
				if strings.Contains(logs, s) {
					t.Fatalf("%q is in the log", s)
				}
			}
			// A second, readable integration in the org still works.
			if tc.sealer {
				g.ghInsert(ghRow{org: g.orgID, secret: ghSecret2, enabled: true})
				if r := g.ghHook("orbit", ghSecret2, uuid.NewString(), body); r.status != http.StatusOK {
					t.Fatalf("the readable integration: %d %s", r.status, r.body)
				}
				if r := g.ghHook("orbit", ghSecret, uuid.NewString(), body); r.status != http.StatusUnauthorized {
					t.Fatalf("the unreadable one verified: %d", r.status)
				}
			}
		})
	}
}

// AC: no Content-Length is 411; above 2 MiB is 413; both only for an org that
// has an enabled integration (an unknown org gets 401 first); a malformed
// signature header is 401 before the length is looked at.
func TestGithubWebhook_Length(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "length"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))

	// Chunked: no Content-Length.
	req, _ := http.NewRequest("POST", g.ghURL("orbit"), io.NopCloser(bytes.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set(github.HeaderSignature, github.Sign([]byte(ghSecret), body))
	req.Header.Set(github.HeaderEvent, github.EventPullRequest)
	req.Header.Set(github.HeaderDelivery, ghDelivery)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusLengthRequired || !strings.Contains(string(b), `"length_required"`) {
		t.Fatalf("chunked: %d %s, want 411 length_required", res.StatusCode, b)
	}

	// Content-Length above 2 MiB: 413 with the body never sent.
	big := raw(t, g.env.srv.Listener.Addr().String(), "POST /api/v1/orgs/orbit/integrations/github/webhook HTTP/1.1\r\nHost: x\r\n"+
		"X-Hub-Signature-256: sha256="+strings.Repeat("ab", 32)+"\r\nContent-Length: 2097153\r\n\r\n")
	if !strings.HasPrefix(big, "HTTP/1.1 413") || !strings.Contains(big, `"payload_too_large"`) {
		t.Fatalf("2 MiB + 1: %s", big)
	}
	// The same for an unknown org or a malformed signature: 401, no 413 oracle.
	for name, req := range map[string]string{
		"unknown org":   "POST /api/v1/orgs/nobody/integrations/github/webhook HTTP/1.1\r\nHost: x\r\nX-Hub-Signature-256: sha256=" + strings.Repeat("ab", 32) + "\r\nContent-Length: 2097153\r\n\r\n",
		"no signature":  "POST /api/v1/orgs/orbit/integrations/github/webhook HTTP/1.1\r\nHost: x\r\nContent-Length: 2097153\r\n\r\n",
		"bad signature": "POST /api/v1/orgs/orbit/integrations/github/webhook HTTP/1.1\r\nHost: x\r\nX-Hub-Signature-256: nope\r\nContent-Length: 2097153\r\n\r\n",
	} {
		if out := raw(t, g.env.srv.Listener.Addr().String(), req); !strings.HasPrefix(out, "HTTP/1.1 401") {
			t.Fatalf("%s: %s, want 401", name, out)
		}
	}
	// Exactly 2 MiB passes the size gate (it is read and verified; the payload is not JSON: 400).
	exact := bytes.Repeat([]byte(" "), 2<<20)
	if r := g.ghHook("orbit", ghSecret, ghDelivery, exact); r.status != http.StatusBadRequest {
		t.Fatalf("2 MiB: %d %s, want 400 (read, verified, not a payload)", r.status, r.body)
	}
	if g.srv2.gitBudget.inUse() != 0 {
		t.Fatalf("budget in use after the requests: %d", g.srv2.gitBudget.inUse())
	}
}

// raw sends request text and returns the whole response up to the close (or
// two seconds).
func raw(t *testing.T, addr, request string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		out.Write(buf[:n])
		if err != nil || bytes.Contains(out.Bytes(), []byte("\r\n\r\n")) && n < len(buf) {
			break
		}
	}
	return out.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// AC: a slow reader is closed at the deadline and its bytes are freed.
func TestGithubWebhook_SlowReaderClosedAtDeadline(t *testing.T) {
	g := newGHEnv(t, 1<<20, 300*time.Millisecond, true, envOpts{})
	g.setup()
	addr := g.env.srv.Listener.Addr().String()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	hdr := "POST /api/v1/orgs/orbit/integrations/github/webhook HTTP/1.1\r\nHost: x\r\nX-Hub-Signature-256: sha256=" +
		strings.Repeat("ab", 32) + "\r\nContent-Length: 5000\r\n\r\n{\"action\""
	start := time.Now()
	if _, err := c.Write([]byte(hdr)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the budget to be taken", func() bool { return g.srv2.gitBudget.inUse() == 5000 })
	// The sender stalls. The server answers and closes at the deadline.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	out, _ := io.ReadAll(c)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the slow reader was held %v, deadline 300ms", took)
	}
	if !strings.HasPrefix(string(out), "HTTP/1.1 400") {
		t.Fatalf("slow reader got %q, want a 400 and a close", out)
	}
	waitFor(t, "the bytes to be freed", func() bool { return g.srv2.gitBudget.inUse() == 0 })
}

// AC: the in-flight budget returns 503 and Retry-After, and frees up when the
// holder goes.
func TestGithubWebhook_BudgetOverloaded(t *testing.T) {
	g := newGHEnv(t, 10_000, 5*time.Second, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "budget"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	if len(body) < 1000 {
		t.Fatalf("fixture is only %d bytes", len(body))
	}
	holder, err := net.Dial("tcp", g.env.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Write([]byte("POST /api/v1/orgs/orbit/integrations/github/webhook HTTP/1.1\r\nHost: x\r\nX-Hub-Signature-256: sha256=" +
		strings.Repeat("ab", 32) + "\r\nContent-Length: 9500\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the holder's bytes", func() bool { return g.srv2.gitBudget.inUse() == 9500 })

	r := g.ghHook("orbit", ghSecret, uuid.NewString(), body)
	if r.status != http.StatusServiceUnavailable || r.errCode(t) != "overloaded" || r.header.Get("Retry-After") != "5" {
		t.Fatalf("over budget: %d %s Retry-After=%q, want 503 overloaded with 5", r.status, r.body, r.header.Get("Retry-After"))
	}
	// An unknown org is still 401 while the budget is spent: 503 only for an org with an integration.
	if r := g.ghHook("nobody", ghSecret, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("unknown org over budget: %d", r.status)
	}
	_ = holder.Close()
	waitFor(t, "the holder to be freed", func() bool { return g.srv2.gitBudget.inUse() == 0 })
	if r := g.ghHook("orbit", ghSecret, uuid.NewString(), body); r.status != http.StatusOK {
		t.Fatalf("after the holder left: %d %s", r.status, r.body)
	}
}

// AC: no secret, signature or payload text in logs, events or idempotency rows.
func TestGithubWebhook_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{logLevel: slog.LevelDebug})
	g.ghInsert(ghRow{org: g.orgID, secret: ghSecret, next: ghSecret2, enabled: true})
	bad := g.ghInsert(ghRow{org: g.orgID, secret: "third-secret-third-secret-third-secret", sealWith: ghKeyring(t, 9), enabled: true})
	_ = bad
	is := g.createIssue(g.chen, map[string]any{"title": "secrets"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, "zebra-private-description"))
	sigs := []string{github.Sign([]byte(ghSecret), body), github.Sign([]byte("wrong-wrong-wrong-wrong-wrong-wrong!"), body)}
	g.ghHook("orbit", ghSecret, uuid.NewString(), body, "Idempotency-Key", "idem-1")
	g.ghHook("orbit", ghSecret2, uuid.NewString(), body)
	g.ghHook("orbit", "wrong-wrong-wrong-wrong-wrong-wrong!", uuid.NewString(), body)
	g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_closed_merged.json", ghNaming(is.Identifier, "")))

	logs := g.logs.String()
	secrets := []string{ghSecret, ghSecret2, "third-secret-third-secret-third-secret", "wrong-wrong-wrong-wrong-wrong-wrong!"}
	for _, s := range append(secrets, strings.TrimPrefix(sigs[0], "sha256="), strings.TrimPrefix(sigs[1], "sha256="), "zebra-private") {
		if strings.Contains(logs, s) {
			t.Fatalf("%q is in the server log", s)
		}
		if n := g.count(`SELECT count(*) FROM events WHERE payload::text LIKE '%' || $1 || '%'`, s); n != 0 {
			t.Fatalf("%q is in an event payload", s)
		}
		if n := g.count(`SELECT count(*) FROM git_webhook_events WHERE payload::text LIKE '%' || $1 || '%' OR event_uuid LIKE '%' || $1 || '%'`, s); n != 0 {
			t.Fatalf("%q is in a delivery row", s)
		}
	}
	if n := g.count(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("idempotency rows = %d; the webhook must never store one", n)
	}
	// The sealed columns do not hold a plaintext secret.
	for _, s := range secrets {
		if n := g.count(`SELECT count(*) FROM git_integrations WHERE position($1::bytea in webhook_secret_enc) > 0 OR position($1::bytea in webhook_secret_next_enc) > 0`, []byte(s)); n != 0 {
			t.Fatalf("%q is in a sealed column in the clear", s)
		}
	}
}

// Hardening: a connection that cannot take a read deadline is refused (503),
// not read without one; the budget is freed.
func TestGithubWebhook_NoReadDeadlineFailsClosed(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{logLevel: slog.LevelDebug})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "no deadline"})
	body := ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))
	req := httptest.NewRequest("POST", "/api/v1/orgs/orbit/integrations/github/webhook", bytes.NewReader(body))
	req.Header.Set(github.HeaderSignature, github.Sign([]byte(ghSecret), body))
	req.Header.Set(github.HeaderEvent, github.EventPullRequest)
	req.Header.Set(github.HeaderDelivery, ghDelivery)
	rec := httptest.NewRecorder() // a ResponseRecorder has no SetReadDeadline
	g.srv2.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d %s, want 503", rec.Code, rec.Body)
	}
	if g.srv2.gitBudget.inUse() != 0 {
		t.Fatal("budget not freed")
	}
	if n := g.count(`SELECT count(*) FROM mr_links`); n != 0 {
		t.Fatalf("the refused body was processed: %d links", n)
	}
}

// Hardening: secret_unreadable is logged once per integration per minute.
func TestGithubWebhook_UnreadableWarnRateLimited(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{logLevel: slog.LevelDebug})
	var nowNs atomic.Int64
	nowNs.Store(time.Now().UnixNano())
	g.srv2.gitWarnNow = func() time.Time { return time.Unix(0, nowNs.Load()) }
	g.ghInsert(ghRow{org: g.orgID, secret: ghSecret, sealWith: ghKeyring(t, 2), enabled: true})
	body := ghFixture(t, "pr_opened.json", nil)
	count := func() int { return strings.Count(g.logs.String(), "code=secret_unreadable") }
	for range 3 {
		if r := g.ghHook("orbit", ghSecret, uuid.NewString(), body); r.status != http.StatusUnauthorized {
			t.Fatalf("status %d", r.status)
		}
	}
	if count() != 1 {
		t.Fatalf("%d log lines in a minute, want 1", count())
	}
	nowNs.Add(int64(61 * time.Second))
	g.ghHook("orbit", ghSecret, uuid.NewString(), body)
	if count() != 2 {
		t.Fatalf("%d log lines after a minute, want 2", count())
	}
}
