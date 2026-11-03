package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/bitbucket"
	"devops/internal/platform"
	"devops/internal/tracker"
)

// LAUN-84 (T17 ticket 10): the Bitbucket webhook, Cloud and Data Center,
// against a real database, driven by the fixtures in internal/bitbucket/testdata
// (hand-written from the documented shapes with invented data, NOT recorded
// deliveries; see its README.md). Integration rows are inserted directly;
// the token-mode routes are in bitbucket_hosts_integration_test.go.

const (
	bbSecret    = "bbwh-test-secret-0123456789abcdefABCDEF"
	bbSecret2   = "bbwh-second-secret-9876543210fedcbaFEDCBA"
	bbWS        = "{11111111-2222-3333-4444-555555555555}"
	bbRepoUUID  = "{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	bbCloudKey  = bbWS + "/" + bbRepoUUID
	bbDCKey     = "4242"
	bbCloudPath = "example-org/example-app"
	bbDCPath    = "PROJ/example-app"
	bbHeadSHA   = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567"
	bbHeadSHA2  = "f0e1d2c3b4a5968778695a4b3c2d1e0f12345678"
)

type bbEnv struct {
	*ghEnv
	rows int
}

func newBBEnv(t *testing.T, o envOpts) *bbEnv {
	t.Helper()
	return &bbEnv{ghEnv: newGHEnv(t, 0, 0, true, o)}
}

type bbRow struct {
	org      uuid.UUID
	secret   string
	next     string // a pending secret
	enabled  bool
	key      string // repo key to map (default: the Cloud key)
	path     string
	sealWith *platform.GitKeyring
	noBlob   bool // webhook_secret_enc stays NULL: the integration cannot verify
}

// insert makes a Bitbucket integration and maps one repository to it.
func (b *bbEnv) insert(r bbRow) uuid.UUID {
	b.t.Helper()
	id := uuid.Must(uuid.NewV7())
	b.rows++
	base := "https://bitbucket.org"
	if b.rows > 1 {
		base = fmt.Sprintf("https://bb%d.example.test", b.rows)
	}
	if r.key == "" {
		r.key, r.path = bbCloudKey, bbCloudPath
	}
	kr := r.sealWith
	if kr == nil {
		kr = b.kr
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
	if _, err := b.pool.Exec(ctx, `INSERT INTO git_integrations
		(id, org_id, base_url, webhook_secret_hash, enabled, provider, mode, webhook_secret_enc, webhook_secret_next_enc, webhook_secret_next_hash)
		VALUES ($1, $2, $3, $4, $5, 'bitbucket', 'webhook_only', $6, $7, $8)`,
		id, r.org, base, h[:], r.enabled, blob, nextBlob, nextHash); err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.pool.Exec(ctx, `INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path) VALUES ($1, $2, $3, $4)`,
		r.org, id, r.key, r.path); err != nil {
		b.t.Fatal(err)
	}
	return id
}

// setup makes the org's integration with Cloud repo bbCloudKey and Data Center
// repo 4242 both mapped (one integration may hold both keys in a test).
func (b *bbEnv) setup() uuid.UUID {
	id := b.insert(bbRow{org: b.orgID, secret: bbSecret, enabled: true})
	if _, err := b.pool.Exec(context.Background(), `INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path) VALUES ($1, $2, $3, $4)`,
		b.orgID, id, bbDCKey, bbDCPath); err != nil {
		b.t.Fatal(err)
	}
	return id
}

func bbFixture(t *testing.T, name string, mut func(m map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "bitbucket", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
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

// bbNaming makes the pull request name ident in its branch and title (both flavours).
func bbNaming(ident, desc string) func(map[string]any) {
	return func(m map[string]any) {
		if pr, ok := m["pullrequest"].(map[string]any); ok {
			pr["title"] = ident + ": Fix login redirect"
			pr["description"] = desc
			pr["source"].(map[string]any)["branch"].(map[string]any)["name"] = "chen/" + strings.ToLower(ident) + "-fix"
		}
		if pr, ok := m["pullRequest"].(map[string]any); ok {
			pr["title"] = ident + ": Fix login redirect"
			pr["description"] = desc
			pr["fromRef"].(map[string]any)["displayId"] = "chen/" + strings.ToLower(ident) + "-fix"
		}
	}
}

func (b *bbEnv) url(org string) string {
	return b.env.srv.URL + "/api/v1/orgs/" + org + "/integrations/bitbucket/webhook"
}

// bbDeliveryHeader is the delivery header a flavour sends: Cloud X-Request-UUID,
// Data Center X-Request-Id.
func bbDeliveryHeader(event string) string {
	if strings.HasPrefix(event, "pr:") || event == bitbucket.EventDCBuildStatus || event == bitbucket.EventDCDiagnostics {
		return bitbucket.HeaderRequestID
	}
	return bitbucket.HeaderRequestUUID
}

// hook sends one delivery signed with secret (no signature header when secret is
// ""). headers are replacement key/value pairs; a value of "" removes the header.
func (b *bbEnv) hook(org, secret, event, delivery string, body []byte, headers ...string) resp {
	b.t.Helper()
	req, err := http.NewRequest("POST", b.url(org), bytes.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(bitbucket.HeaderEvent, event)
	if secret != "" {
		req.Header.Set(bitbucket.HeaderSignature, bitbucket.Sign([]byte(secret), body))
	}
	if delivery != "" {
		req.Header.Set(bbDeliveryHeader(event), delivery)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	return b.sendHook(req)
}

func (b *bbEnv) result(r resp, status int) hookOutcome {
	b.t.Helper()
	return b.hookResult(r, status)
}

// send delivers a fixture to org "orbit" under a new delivery id and expects 200.
func (b *bbEnv) send(event, fixture string, mut func(map[string]any)) hookOutcome {
	b.t.Helper()
	return b.result(b.hook("orbit", bbSecret, event, uuid.NewString(), bbFixture(b.t, fixture, mut)), http.StatusOK)
}

func bbSources(is tracker.Issue) int {
	n := 0
	for _, sc := range is.StateHistory {
		if sc.Source == "bitbucket" {
			n++
		}
	}
	return n
}

// withSHA changes the head commit of a pull request fixture.
func withSHA(sha string, then func(map[string]any)) func(map[string]any) {
	return func(m map[string]any) {
		if pr, ok := m["pullrequest"].(map[string]any); ok {
			pr["source"].(map[string]any)["commit"].(map[string]any)["hash"] = sha
		}
		if pr, ok := m["pullRequest"].(map[string]any); ok {
			pr["fromRef"].(map[string]any)["latestCommit"] = sha
		}
		if then != nil {
			then(m)
		}
	}
}

func (b *bbEnv) mrRow(issue uuid.UUID) (state, review, sha, key string, draft bool) {
	b.t.Helper()
	if err := b.pool.QueryRow(context.Background(), `SELECT state, review, head_sha, repo_key, draft FROM mr_links WHERE org_id = $1 AND issue_id = $2`,
		b.orgID, issue).Scan(&state, &review, &sha, &key, &draft); err != nil {
		b.t.Fatal(err)
	}
	return
}

// AC: Cloud created -> In Review (link, history, attachment and event say
// bitbucket, the repo key is the lower-case uuid pair), updated refreshes the
// head sha, approved and unapproved set the review, fulfilled -> In Testing.
func TestBitbucketWebhook_CloudLifecycle(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "Fix login redirect"})
	b.setState(is.Identifier, "Todo")
	name := bbNaming(is.Identifier, "")

	r := b.send(bitbucket.EventCloudCreated, "cloud_pr_created.json", name)
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 1 || r.Linked[0] != is.Identifier || len(r.Moved) != 1 {
		t.Fatalf("created = %+v", r)
	}
	got := b.issueFull(is.Identifier)
	if got.State.Name != "In Review" {
		t.Fatalf("after created: %q, want In Review", got.State.Name)
	}
	last := got.StateHistory[len(got.StateHistory)-1]
	if last.Source != "bitbucket" || last.Actor != nil || last.FromStateName == nil || *last.FromStateName != "Todo" {
		t.Fatalf("history row = %+v", last)
	}
	prURL := "https://bitbucket.org/example-org/example-app/pull-requests/17"
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %+v", got.Attachments)
	}
	a := got.Attachments[0]
	if a.Kind != "link" || a.URL == nil || *a.URL != prURL || a.Title != "PR #17: "+is.Identifier+": Fix login redirect" || a.Subtitle != bbCloudPath {
		t.Fatalf("attachment = %+v", a)
	}
	if st, review, sha, key, draft := b.mrRow(is.ID); st != "opened" || review != "none" || sha != bbHeadSHA || key != bbCloudKey || draft {
		t.Fatalf("mr row = %s %s %s %s %v", st, review, sha, key, draft)
	}
	if n := b.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'issue.updated' AND entity_id = $2 AND actor_id IS NULL
		AND payload->>'source' = 'bitbucket' AND payload->'mergeRequest'->>'number' = '17' AND payload->'mergeRequest'->>'repoKey' = $3`,
		b.orgID, is.ID, bbCloudKey); n != 1 {
		t.Fatalf("bitbucket issue.updated events = %d", n)
	}
	if n := b.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND event_type = 'pull_request' AND status = 'processed'
		AND event_uuid LIKE 'bb:%:r{11111111-2222-3333-4444-555555555555}_{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}:pr17:opened'`, b.orgID); n != 1 {
		t.Fatalf("delivery rows = %d", n)
	}

	// Approval and its removal; the ticket does not move.
	b.send(bitbucket.EventCloudApproved, "cloud_pr_approved.json", name)
	if cr := b.oneCR(is.Identifier); cr.State != tracker.CRApproved || cr.Provider != "bitbucket" {
		t.Fatalf("after approval: %+v", cr)
	}
	b.send(bitbucket.EventCloudUnapproved, "cloud_pr_unapproved.json", name)
	if cr := b.oneCR(is.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("after unapproval: %+v", cr)
	}
	if b.issueFull(is.Identifier).State.Name != "In Review" {
		t.Fatal("an approval moved the ticket")
	}
	// A push refreshes the link's head sha; it moves nothing.
	r = b.send(bitbucket.EventCloudUpdated, "cloud_pr_updated.json", name)
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("updated = %+v", r)
	}
	if _, _, sha, _, _ := b.mrRow(is.ID); sha != bbHeadSHA2 {
		t.Fatalf("head sha after update = %s", sha)
	}
	// Merged -> In Testing; the link is terminal.
	r = b.send(bitbucket.EventCloudFulfilled, "cloud_pr_fulfilled.json", name)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 || len(r.Linked) != 0 {
		t.Fatalf("fulfilled = %+v", r)
	}
	got = b.issueFull(is.Identifier)
	if got.State.Name != "In Testing" || bbSources(got) != 2 {
		t.Fatalf("after merge: %q, %d bitbucket history rows", got.State.Name, bbSources(got))
	}
	if st, _, _, _, _ := b.mrRow(is.ID); st != "merged" {
		t.Fatalf("link state = %s", st)
	}
}

// AC: the same moves on Data Center, with needs_work as changes requested.
func TestBitbucketWebhook_DataCenterLifecycle(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "dc"})
	b.setState(is.Identifier, "Todo")
	name := bbNaming(is.Identifier, "")

	// A draft links and does not move.
	r := b.send(bitbucket.EventDCOpened, "dc_pr_opened_draft.json", name)
	if len(r.Linked) != 1 || len(r.Moved) != 0 {
		t.Fatalf("draft opened = %+v", r)
	}
	if st, _, _, key, draft := b.mrRow(is.ID); st != "opened" || key != bbDCKey || !draft {
		t.Fatalf("draft row: %s %s %v", st, key, draft)
	}
	if b.issueFull(is.Identifier).State.Name != "Todo" {
		t.Fatal("a draft moved the ticket")
	}
	// Opened again, no draft field: not a draft -> In Review (the link is the same row).
	r = b.send(bitbucket.EventDCOpened, "dc_pr_opened.json", name)
	if len(r.Moved) != 1 {
		t.Fatalf("opened = %+v", r)
	}
	if got := b.issueFull(is.Identifier); got.State.Name != "In Review" || got.StateHistory[len(got.StateHistory)-1].Source != "bitbucket" {
		t.Fatalf("after opened: %q", got.State.Name)
	}
	// Reviewers: needs work, then approved. Times move forward in the fixtures' updatedDate.
	b.send(bitbucket.EventDCNeedsWork, "dc_pr_reviewer_needs_work.json", name)
	if cr := b.oneCR(is.Identifier); !cr.ChangesRequested || cr.State != tracker.CRopen {
		t.Fatalf("needs_work: %+v", cr)
	}
	b.send(bitbucket.EventDCApproved, "dc_pr_reviewer_approved.json", func(m map[string]any) {
		name(m)
		m["pullRequest"].(map[string]any)["updatedDate"] = float64(1793009900000)
	})
	if cr := b.oneCR(is.Identifier); cr.State != tracker.CRApproved || cr.ChangesRequested {
		t.Fatalf("approved: %+v", cr)
	}
	// Source branch updated: head sha refreshes.
	b.send(bitbucket.EventDCFromRef, "dc_pr_from_ref_updated.json", name)
	if _, _, sha, _, _ := b.mrRow(is.ID); sha != bbHeadSHA2 {
		t.Fatalf("head sha = %s", sha)
	}
	r = b.send(bitbucket.EventDCMerged, "dc_pr_merged.json", name)
	if len(r.Moved) != 1 {
		t.Fatalf("merged = %+v", r)
	}
	if b.issueFull(is.Identifier).State.Name != "In Testing" {
		t.Fatal("merge did not move the ticket to In Testing")
	}
}

// AC: declined / rejected closes the link and moves nothing, on both flavours.
func TestBitbucketWebhook_DeclinedClosesTheLink(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	for _, c := range []struct{ open, openFx, closeEv, closeFx string }{
		{bitbucket.EventCloudCreated, "cloud_pr_created.json", bitbucket.EventCloudRejected, "cloud_pr_rejected.json"},
		{bitbucket.EventDCOpened, "dc_pr_opened.json", bitbucket.EventDCDeclined, "dc_pr_declined.json"},
	} {
		is := b.createIssue(b.chen, map[string]any{"title": "declined " + c.closeEv})
		b.setState(is.Identifier, "Todo")
		b.send(c.open, c.openFx, bbNaming(is.Identifier, ""))
		r := b.send(c.closeEv, c.closeFx, bbNaming(is.Identifier, ""))
		if r.Status != tracker.WebhookProcessed || len(r.Moved) != 0 {
			t.Fatalf("%s = %+v", c.closeEv, r)
		}
		if st, _, _, _, _ := b.mrRow(is.ID); st != "closed" {
			t.Fatalf("%s: link state %s", c.closeEv, st)
		}
		if b.issueFull(is.Identifier).State.Name != "In Review" {
			t.Fatalf("%s: the ticket must stay where the open put it", c.closeEv)
		}
		// Reset the shared PR number for the next flavour's open: the rows are per repo key, not shared.
	}
}

// AC (open point 3): a payload with no draft field is not a draft; with
// "draft": true it is. Both flavours.
func TestBitbucketWebhook_DraftField(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	for _, c := range []struct {
		event, fixture string
		moved          int
	}{
		{bitbucket.EventCloudCreated, "cloud_pr_created.json", 1},
		{bitbucket.EventCloudCreated, "cloud_pr_created_draft.json", 0},
		{bitbucket.EventDCOpened, "dc_pr_opened.json", 1},
		{bitbucket.EventDCOpened, "dc_pr_opened_draft.json", 0},
	} {
		is := b.createIssue(b.chen, map[string]any{"title": c.fixture})
		b.setState(is.Identifier, "Todo")
		r := b.send(c.event, c.fixture, func(m map[string]any) {
			bbNaming(is.Identifier, "")(m)
			// a new pull request number per case, so the links are separate rows
			if pr, ok := m["pullrequest"].(map[string]any); ok {
				pr["id"] = 100 + is.Number
			}
			if pr, ok := m["pullRequest"].(map[string]any); ok {
				pr["id"] = 100 + is.Number
			}
		})
		if len(r.Linked) != 1 || len(r.Moved) != c.moved {
			t.Fatalf("%s: %+v, want %d moved", c.fixture, r, c.moved)
		}
	}
}

// AC (open point 16): a fork pull request (or a deleted fork) is 200 ignored
// and writes nothing, also when merged.
func TestBitbucketWebhook_ForkPRsIgnored(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "fork"})
	before := b.glSnapshot(is.Identifier)
	for _, c := range []struct{ event, fixture string }{
		{bitbucket.EventCloudCreated, "cloud_pr_fork_created.json"},
		{bitbucket.EventCloudFulfilled, "cloud_pr_deleted_fork_created.json"},
		{bitbucket.EventDCOpened, "dc_pr_fork_opened.json"},
		{bitbucket.EventDCMerged, "dc_pr_deleted_fork_opened.json"},
	} {
		r := b.send(c.event, c.fixture, bbNaming(is.Identifier, ""))
		if r.Status != tracker.WebhookIgnored || len(r.Linked) != 0 || len(r.Moved) != 0 {
			t.Fatalf("%s = %+v, want ignored", c.fixture, r)
		}
	}
	if after := b.glSnapshot(is.Identifier); after != before {
		t.Fatalf("a fork pull request wrote something:\n before %+v\n after  %+v", before, after)
	}
	if n := b.count(`SELECT count(*) FROM mr_links WHERE org_id = $1`, b.orgID); n != 0 {
		t.Fatalf("mr_links = %d", n)
	}
}

// AC: a delivery seen before is duplicate and changes nothing, on both flavours.
func TestBitbucketWebhook_Redelivery(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "redeliver"})
	for _, c := range []struct{ event, fixture string }{
		{bitbucket.EventCloudCreated, "cloud_pr_created.json"},
		{bitbucket.EventDCOpened, "dc_pr_opened.json"},
	} {
		is = b.createIssue(b.chen, map[string]any{"title": "redeliver " + c.event})
		body := bbFixture(t, c.fixture, bbNaming(is.Identifier, ""))
		d := "req-" + uuid.NewString()
		b.result(b.hook("orbit", bbSecret, c.event, d, body), http.StatusOK)
		b.setState(is.Identifier, "In Progress") // a person moves it back
		before := b.glSnapshot(is.Identifier)
		r := b.result(b.hook("orbit", bbSecret, c.event, d, body), http.StatusOK)
		if r.Status != tracker.WebhookDuplicate || len(r.Linked) != 0 || len(r.Moved) != 0 {
			t.Fatalf("%s redelivery = %+v", c.event, r)
		}
		if after := b.glSnapshot(is.Identifier); after != before || after.state != "In Progress" {
			t.Fatalf("a redelivery changed something: %+v -> %+v", before, after)
		}
	}
}

// AC: the signature check with each secret kind; cross-org; no secret.
func TestBitbucketWebhook_Auth(t *testing.T) {
	b := newBBEnv(t, envOpts{logLevel: slog.LevelDebug})
	is := b.createIssue(b.chen, map[string]any{"title": "auth"})
	body := bbFixture(t, "cloud_pr_created.json", bbNaming(is.Identifier, ""))
	ev := bitbucket.EventCloudCreated

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
	unauth("no integration", b.hook("orbit", bbSecret, ev, uuid.NewString(), body))
	id := b.setup()
	unauth("unknown org", b.hook("no-such-org", bbSecret, ev, uuid.NewString(), body))
	unauth("wrong secret", b.hook("orbit", bbSecret+"x", ev, uuid.NewString(), body))
	unauth("no signature (a hook without a secret)", b.hook("orbit", "", ev, uuid.NewString(), body))
	unauth("sha1 form", b.hook("orbit", bbSecret, ev, uuid.NewString(), body, bitbucket.HeaderSignature, "sha1="+strings.Repeat("a", 64)))
	unauth("short signature", b.hook("orbit", bbSecret, ev, uuid.NewString(), body, bitbucket.HeaderSignature, "sha256=abcd"))
	unauth("signature of another body", b.hook("orbit", bbSecret, ev, uuid.NewString(), body,
		bitbucket.HeaderSignature, bitbucket.Sign([]byte(bbSecret), append([]byte(" "), body...))))
	unauth("garbage body", b.hook("orbit", "wrong-secret-wrong-secret-wrong", ev, uuid.NewString(), []byte("{not json")))
	unauth("no delivery header", b.hook("orbit", "wrong-secret-wrong-secret-wrong", ev, "", body))
	unauth("another event", b.hook("orbit", "wrong-secret-wrong-secret-wrong", "repo:push", uuid.NewString(), body))
	unauth("bearer token instead", b.hook("orbit", "", ev, uuid.NewString(), body, "Authorization", "Bearer "+b.chen))
	unauth("the GitHub header", b.hook("orbit", "", ev, uuid.NewString(), body, "X-Hub-Signature-256", bitbucket.Sign([]byte(bbSecret), body)))
	unauth("idempotency key", b.hook("orbit", "bad", ev, uuid.NewString(), body, "Idempotency-Key", "k1"))
	if n := b.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("refused deliveries left %d event rows", n)
	}
	if got := b.issueFull(is.Identifier); got.State.Name != "Backlog" {
		t.Fatalf("a refused delivery moved the issue to %q", got.State.Name)
	}

	// Cross-org: another org's integration (same repo key) and its secret.
	org2 := b.newOrg("acme", "ACME")
	b.insert(bbRow{org: org2, secret: bbSecret2, enabled: true})
	unauth("org 2's secret at org 1", b.hook("orbit", bbSecret2, ev, uuid.NewString(), body))
	unauth("org 1's secret at org 2", b.hook("acme", bbSecret, ev, uuid.NewString(), body))
	r := b.result(b.hook("acme", bbSecret2, ev, uuid.NewString(), body), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("cross-org delivery = %+v, want nothing linked", r)
	}
	if got := b.issueFull(is.Identifier); got.State.Name != "Backlog" || len(got.Attachments) != 0 {
		t.Fatalf("org 2's delivery changed org 1's issue: %q", got.State.Name)
	}
	if n := b.count(`SELECT count(*) FROM mr_links WHERE org_id = $1`, b.orgID); n != 0 {
		t.Fatalf("org 1 mr_links = %d", n)
	}
	if n := b.count(`SELECT count(*) FROM mr_links WHERE org_id = $1 AND repo_key = $2`, org2, bbCloudKey); n != 0 {
		t.Fatalf("org 2 linked nothing, but has %d rows", n)
	}

	// Disabled: refused.
	if _, err := b.pool.Exec(context.Background(), `UPDATE git_integrations SET enabled = false, updated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	unauth("disabled integration", b.hook("orbit", bbSecret, ev, uuid.NewString(), body))
}

// A GitLab or GitHub integration is never a candidate for a Bitbucket
// signature, and the other way round.
func TestBitbucketWebhook_ProviderSeparation(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	is := b.createIssue(b.chen, map[string]any{"title": "providers"})
	b.ghEnv.setup() // a GitHub integration with ghSecret
	body := bbFixture(t, "cloud_pr_created.json", bbNaming(is.Identifier, ""))
	if r := b.hook("orbit", ghSecret, bitbucket.EventCloudCreated, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("a GitHub secret signed a Bitbucket delivery: %d", r.status)
	}
	b.setup()
	if r := b.ghHook("orbit", bbSecret, uuid.NewString(), ghFixture(t, "pr_opened.json", ghNaming(is.Identifier, ""))); r.status != http.StatusUnauthorized {
		t.Fatalf("a Bitbucket secret passed the GitHub check: %d", r.status)
	}
	b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, uuid.NewString(), body), http.StatusOK)
}

// AC: each secret kind verifies (current, pending during a rotation), a sealed
// secret that cannot be opened (no secret) is a 401 and logs our code only.
func TestBitbucketWebhook_SecretKinds(t *testing.T) {
	b := newBBEnv(t, envOpts{logLevel: slog.LevelDebug})
	is := b.createIssue(b.chen, map[string]any{"title": "kinds"})
	body := bbFixture(t, "dc_pr_opened.json", bbNaming(is.Identifier, ""))
	ev := bitbucket.EventDCOpened
	id := b.insert(bbRow{org: b.orgID, secret: bbSecret, next: bbSecret2, enabled: true, key: bbDCKey, path: bbDCPath})
	for name, sec := range map[string]string{"current": bbSecret, "pending": bbSecret2} {
		if r := b.hook("orbit", sec, ev, uuid.NewString(), body); r.status != http.StatusOK {
			t.Fatalf("%s secret: %d %s", name, r.status, r.body)
		}
	}
	if r := b.hook("orbit", bbSecret2+"x", ev, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("a third secret: %d", r.status)
	}

	// An integration sealed under another key cannot be read: 401, the code is logged, never the secret.
	org2 := b.newOrg("acme", "ACME")
	b.insert(bbRow{org: org2, secret: bbSecret, enabled: true, sealWith: ghKeyring(t, 9)})
	if r := b.hook("acme", bbSecret, ev, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("unreadable secret: %d", r.status)
	}
	// An integration with no stored secret at all (a Data Center that cannot sign) never verifies.
	org3 := b.newOrg("nosec", "NOS")
	b.insert(bbRow{org: org3, secret: bbSecret, enabled: true, noBlob: true})
	if r := b.hook("nosec", bbSecret, ev, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("no secret: %d", r.status)
	}
	if r := b.hook("nosec", "", ev, uuid.NewString(), body); r.status != http.StatusUnauthorized {
		t.Fatalf("no secret, no signature: %d", r.status)
	}
	logs := b.logs.String()
	if !strings.Contains(logs, "secret_unreadable") {
		t.Fatalf("no secret_unreadable log:\n%s", logs)
	}
	for _, s := range []string{bbSecret, bbSecret2, "Fix login redirect", "example-app", "example-dev"} {
		if strings.Contains(logs, s) {
			t.Fatalf("%q is in the log", s)
		}
	}
	_ = id
}

// Event headers: a Data Center test ping is 200 ignored, another event is 202,
// a missing delivery header or a bad payload is 400, an unmapped repository is 202.
func TestBitbucketWebhook_EventTypesAndBadRequests(t *testing.T) {
	b := newBBEnv(t, envOpts{logLevel: slog.LevelDebug})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "types"})
	body := bbFixture(t, "cloud_pr_created.json", bbNaming(is.Identifier, "zebra-private-description"))

	if r := b.result(b.hook("orbit", bbSecret, bitbucket.EventDCDiagnostics, "", bbFixture(t, "dc_ping.json", nil)), http.StatusOK); r.Status != tracker.WebhookIgnored {
		t.Fatalf("diagnostics:ping = %+v", r)
	}
	for _, ev := range []string{"repo:push", "pullrequest:comment_created", "pr:comment:added", "pr:deleted", ""} {
		if r := b.result(b.hook("orbit", bbSecret, ev, "", body), http.StatusAccepted); r.Status != tracker.WebhookIgnored {
			t.Fatalf("%q = %+v", ev, r)
		}
	}
	// Unmapped: 202, nothing kept, a log line with the repo key and no content.
	other := bbFixture(t, "cloud_pr_created.json", func(m map[string]any) {
		bbNaming(is.Identifier, "zebra-private-description")(m)
		m["repository"].(map[string]any)["uuid"] = "{bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee}"
	})
	if r := b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "d-unmapped", other), http.StatusAccepted); r.Status != tracker.WebhookUnmapped {
		t.Fatalf("unmapped = %+v", r)
	}
	logs := b.logs.String()
	if !strings.Contains(logs, "bitbucket webhook from an unmapped repository") || !strings.Contains(logs, "{bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee}") {
		t.Fatalf("no unmapped log line with the repo key:\n%s", logs)
	}
	for _, s := range []string{"zebra-private", "example-app", "example-dev", "pull-requests/17", bbSecret} {
		if strings.Contains(logs, s) {
			t.Fatalf("content %q is in the log", s)
		}
	}
	// Signed, but wrong: 400, nothing kept.
	for name, r := range map[string]resp{
		"no delivery header":  b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "", body),
		"dc, no delivery":     b.hook("orbit", bbSecret, bitbucket.EventDCOpened, "", bbFixture(t, "dc_pr_opened.json", nil)),
		"long delivery":       b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, strings.Repeat("a", 101), body),
		"space in delivery":   b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "a b", body),
		"not json":            b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "d1", []byte("{nope")),
		"no workspace uuid":   b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "d2", []byte(`{"pullrequest":{"id":1},"repository":{"uuid":"{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"}}`)),
		"dc without a repo":   b.hook("orbit", bbSecret, bitbucket.EventDCOpened, "d3", []byte(`{"pullRequest":{"id":1}}`)),
		"empty body":          b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "d4", nil),
		"status, no sha":      b.hook("orbit", bbSecret, bitbucket.EventCloudStatusNew, "d5", []byte(`{"repository":{"uuid":"{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}","workspace":{"uuid":"{11111111-2222-3333-4444-555555555555}"}},"commit_status":{"state":"FAILED"}}`)),
		"dc build, no commit": b.hook("orbit", bbSecret, bitbucket.EventDCBuildStatus, "d6", []byte(`{"repository":{"id":4242},"buildStatus":{"state":"FAILED"}}`)),
	} {
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if n := b.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("ping, ignored, unmapped and 400s kept %d event rows", n)
	}
}

// An action that asks for nothing (a Data Center event we read but that maps to
// no action) writes nothing.
func TestBitbucketWebhook_NoopWritesNothing(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "noop"})
	before := b.glSnapshot(is.Identifier)
	// A status event whose state the tracker does not know leaves no action.
	r := b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudStatusNew, "d-unknown", bbFixture(t, "cloud_status_unknown_state.json", nil)), http.StatusOK)
	if r.Status != tracker.WebhookIgnored {
		t.Fatalf("unknown state = %+v", r)
	}
	if after := b.glSnapshot(is.Identifier); after != before {
		t.Fatalf("wrote something: %+v -> %+v", before, after)
	}
}

// AC: the ring is written for each outcome (processed, ignored, duplicate, bad
// signature), as for the other providers; it holds no payload, secret or caller.
func TestBitbucketWebhook_DeliveriesRing(t *testing.T) {
	g, _ := newRingEnv(t)
	b := &bbEnv{ghEnv: g}
	integ := b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "ring"})
	body := bbFixture(t, "cloud_pr_created.json", bbNaming(is.Identifier, "a secret description text"))

	b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "ring-1", body), http.StatusOK)                                                                    // processed
	b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "ring-1", body), http.StatusOK)                                                                    // duplicate
	b.result(b.hook("orbit", bbSecret, "repo:push", "ring-2", body), http.StatusAccepted)                                                                              // 202 ignored
	b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "ring-3", []byte("{no")), http.StatusBadRequest)                                                   // bad request
	b.result(b.hook("orbit", bbSecret+"x", bitbucket.EventCloudCreated, "ring-4", body), http.StatusUnauthorized)                                                      // bad signature, no integration bound
	b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudCreated, "ring-5", bbFixture(t, "cloud_pr_fork_created.json", bbNaming(is.Identifier, ""))), http.StatusOK) // fork: ignored

	got := b.deliveries(b.chen, integ.String(), http.StatusOK)
	var results []string
	for _, d := range got {
		results = append(results, fmt.Sprintf("%s/%d", d.Result, d.HTTPStatus))
		if d.Provider != "bitbucket" || (d.IntegrationID == nil && d.Result != "bad_signature") || (d.IntegrationID != nil && *d.IntegrationID != integ) {
			t.Fatalf("row = %+v", d)
		}
	}
	want := "ignored/200 bad_signature/401 bad_request/400 ignored/202 duplicate/200 processed/200"
	if strings.Join(results, " ") != want {
		t.Fatalf("bound results = %v, want %s", results, want)
	}
	if got[5].Event != bitbucket.EventCloudCreated || got[5].RepoKey == nil || *got[5].RepoKey != bbCloudKey {
		t.Fatalf("processed row = %+v", got[5])
	}
	// The bad signature has no integration to bind to: its row is org-wide.
	if n := b.count(`SELECT count(*) FROM git_webhook_deliveries WHERE org_id = $1 AND provider = 'bitbucket' AND integration_id IS NULL AND result = 'bad_signature'`, b.orgID); n != 1 {
		t.Fatalf("unbound bad-signature rows = %d, want 1", n)
	}
	var dump string
	if err := b.pool.QueryRow(context.Background(), `SELECT string_agg(t::text, ' ') FROM git_webhook_deliveries t`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{bbSecret, "a secret description", "Fix login redirect", is.Identifier, "chen/", "127.0.0.1", "Go-http-client", "ring-1", "example-dev"} {
		if strings.Contains(dump, bad) {
			t.Fatalf("ring holds %q: %s", bad, dump)
		}
	}
	if strings.Contains(b.logs.String(), bbSecret) {
		t.Fatal("secret in the log")
	}
}

// Check events (decisions-T17 #19): a commit status sets the check of the links
// whose head sha it names; failure beats pending beats success; an older
// result of the same build key is dropped; a sha no link has keeps nothing.
func TestBitbucketWebhook_Checks(t *testing.T) {
	for _, flavour := range []struct {
		name    string
		open    string
		openFx  string
		stat    string
		pending string
		ok      string
		failed  string
	}{
		{"cloud", bitbucket.EventCloudCreated, "cloud_pr_created.json", bitbucket.EventCloudStatusUpd, "cloud_status_inprogress.json", "cloud_status_successful.json", "cloud_status_failed.json"},
		{"dc", bitbucket.EventDCOpened, "dc_pr_opened.json", bitbucket.EventDCBuildStatus, "dc_build_inprogress.json", "dc_build_successful.json", "dc_build_failed.json"},
	} {
		t.Run(flavour.name, func(t *testing.T) {
			b := newBBEnv(t, envOpts{})
			b.setup()
			is := b.createIssue(b.chen, map[string]any{"title": "checks"})
			b.setState(is.Identifier, "Todo")
			b.send(flavour.open, flavour.openFx, bbNaming(is.Identifier, ""))
			if c := checksOf(t, b.env, is.Identifier); c != "null" {
				t.Fatalf("checks before any event = %s", c)
			}
			state0 := b.issueFull(is.Identifier).State.Name
			ev0 := b.mrEvents(is.ID)

			// key renames the build: the fixtures share the key ci-main.
			rename := func(key string) func(map[string]any) {
				return func(m map[string]any) {
					if cs, ok := m["commit_status"].(map[string]any); ok {
						cs["key"] = key
					}
					if bs, ok := m["buildStatus"].(map[string]any); ok {
						bs["key"] = key
					}
				}
			}
			r := b.send(flavour.stat, flavour.pending, nil)
			if r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
				t.Fatalf("pending = %+v", r)
			}
			if c := checksOf(t, b.env, is.Identifier); c != "pending" || b.mrEvents(is.ID) != ev0+1 {
				t.Fatalf("after in-progress: %s, %d events", c, b.mrEvents(is.ID)-ev0)
			}
			b.send(flavour.stat, flavour.ok, nil) // same key, later: success replaces pending
			if c := checksOf(t, b.env, is.Identifier); c != "success" {
				t.Fatalf("after success: %s", c)
			}
			b.send(flavour.stat, flavour.failed, rename("ci-lint")) // another build fails
			if c := checksOf(t, b.env, is.Identifier); c != "failure" {
				t.Fatalf("one failing build: %s", c)
			}
			// An older result of the failing build is dropped.
			before := b.count(`SELECT count(*) FROM events WHERE org_id = $1`, b.orgID)
			r = b.send(flavour.stat, flavour.ok, func(m map[string]any) {
				rename("ci-lint")(m)
				if cs, ok := m["commit_status"].(map[string]any); ok {
					cs["updated_on"] = "2026-10-26T09:00:00.000000+00:00"
				}
				if bs, ok := m["buildStatus"].(map[string]any); ok {
					bs["dateAdded"] = float64(1793000000000)
				}
			})
			// The answer is processed whatever the check did (decisions-T20 #65).
			if r.Status != tracker.WebhookProcessed || checksOf(t, b.env, is.Identifier) != "failure" ||
				b.count(`SELECT count(*) FROM events WHERE org_id = $1`, b.orgID) != before {
				t.Fatalf("an older result: %+v, checks %s", r, checksOf(t, b.env, is.Identifier))
			}
			if b.issueFull(is.Identifier).State.Name != state0 {
				t.Fatal("a check event moved the ticket")
			}
			// A sha no link has: 200 processed (the answer does not say whether a
			// link exists, decisions-T20 #65), no dedupe row, no event.
			rows := b.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, b.orgID)
			evs := b.count(`SELECT count(*) FROM events WHERE org_id = $1`, b.orgID)
			r = b.send(flavour.stat, flavour.ok, func(m map[string]any) {
				if cs, ok := m["commit_status"].(map[string]any); ok {
					cs["commit"] = map[string]any{"hash": "ffffffffffffffffffffffffffffffffffffffff"}
				}
				if _, ok := m["buildStatus"]; ok {
					m["commit"] = "ffffffffffffffffffffffffffffffffffffffff"
				}
			})
			if r.Status != tracker.WebhookProcessed ||
				b.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, b.orgID) != rows ||
				b.count(`SELECT count(*) FROM events WHERE org_id = $1`, b.orgID) != evs {
				t.Fatalf("unknown sha = %+v", r)
			}
			// A new head sha (a push) resets the checks.
			if flavour.name == "cloud" {
				b.send(bitbucket.EventCloudUpdated, "cloud_pr_updated.json", bbNaming(is.Identifier, ""))
			} else {
				b.send(bitbucket.EventDCFromRef, "dc_pr_from_ref_updated.json", bbNaming(is.Identifier, ""))
			}
			if c := checksOf(t, b.env, is.Identifier); c != "null" && c != "none" {
				t.Fatalf("after a push checks = %s", c)
			}
		})
	}
}

// The delivery key of a check event has the sha form; a repeat is a duplicate.
func TestBitbucketWebhook_CheckKeyAndRepeat(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	is := b.createIssue(b.chen, map[string]any{"title": "check key"})
	b.send(bitbucket.EventCloudCreated, "cloud_pr_created.json", bbNaming(is.Identifier, ""))
	body := bbFixture(t, "cloud_status_inprogress.json", nil)
	b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudStatusNew, "chk-1", body), http.StatusOK)
	if n := b.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND event_type = 'checks'
		AND event_uuid = 'bb:chk-1:r{11111111-2222-3333-4444-555555555555}_{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}:sha0a1b2c3d4e5f:checks'`, b.orgID); n != 1 {
		t.Fatalf("check delivery rows = %d", n)
	}
	// A repeated check answers processed (decisions-T20 #65: a check with no
	// link keeps no key, so its repeat is not a duplicate); the ring says duplicate.
	if r := b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudStatusNew, "chk-1", body), http.StatusOK); r.Status != tracker.WebhookProcessed ||
		b.lastRingResult() != tracker.WebhookDuplicate {
		t.Fatalf("repeat = %+v, ring %s", r, b.lastRingResult())
	}
	// An unmapped repository: 202, nothing kept.
	other := bbFixture(t, "cloud_status_inprogress.json", func(m map[string]any) {
		m["repository"].(map[string]any)["uuid"] = "{bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee}"
	})
	if r := b.result(b.hook("orbit", bbSecret, bitbucket.EventCloudStatusNew, "chk-2", other), http.StatusAccepted); r.Status != tracker.WebhookUnmapped {
		t.Fatalf("unmapped check = %+v", r)
	}
}
