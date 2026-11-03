package notify_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/notify"
	"devops/internal/platform"
	"devops/internal/tracker"
)

// Mobile push (LAUN-197, decisions-T35), with the real database, the real
// consumer and sender, and a fake relay: a TLS httptest server reached through
// the REAL netguard dialer (the test policy permits only 127.0.0.1).

const (
	relaySecret   = "RELAYBEARERSECRET-4711"
	relayURLToken = "RELAYURLTOKEN-9001"
	relayURL      = "https://relay.example.com/t/" + relayURLToken
	tokAlice1     = "APNSTOKEN-one-0001"
	tokAlice2     = "FCMTOKEN-two-0002"
	tokBob        = "APNSTOKEN-three-0003"
)

var pushKey = []byte("0123456789abcdef0123456789abcdef")

type relayHit struct {
	Path, Auth, Body, Type string
}

// fakeRelay is the relay at the other end. status 0 means 200. A token listed
// in invalid answers 410.
type fakeRelay struct {
	*httptest.Server
	mu         sync.Mutex
	hits       []relayHit
	status     int
	retryAfter string
	redirect   string
	invalid    map[string]bool
	onHit      func(token string) // called before the answer, outside the lock
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{invalid: map[string]bool{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.hits = append(f.hits, relayHit{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: string(b), Type: r.Header.Get("Content-Type")})
		status, ra, red, inv := f.status, f.retryAfter, f.redirect, f.invalid[body.Token]
		onHit := f.onHit
		f.mu.Unlock()
		if onHit != nil {
			onHit(body.Token)
		}
		if red != "" {
			http.Redirect(w, r, red, http.StatusFound)
			return
		}
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		if inv {
			status = 410
		}
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("relay says ok"))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRelay) set(status int, retryAfter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.retryAfter = status, retryAfter
}

func (f *fakeRelay) got() []relayHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relayHit(nil), f.hits...)
}

// client is the production client on a test dialer that permits 127.0.0.1,
// with the connection for relay.example.com sent to the fake server.
func (f *fakeRelay) client(t *testing.T) *http.Client {
	t.Helper()
	d := &netguard.Dialer{Policy: netguardtest.Loopback()}
	c := notify.NewPushClientFor(d)
	tr := c.Transport.(*http.Transport)
	addr := f.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "example.com"}
	return c
}

type pwo struct {
	*sworld
	relay  *fakeRelay
	keys   *platform.PushKeyring
	svc2   *notify.PushService
	sender *notify.PushSender
	plogs  *lockedBuf
}

func newPushWorld(t *testing.T) *pwo {
	t.Helper()
	sw := newSWorld(t)
	f := newFakeRelay(t)
	keys, err := platform.NewPushKeyring(pushKey)
	if err != nil {
		t.Fatal(err)
	}
	w := &pwo{sworld: sw, relay: f, keys: keys, plogs: &lockedBuf{}}
	log := slog.New(slog.NewTextHandler(w.plogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	poster := notify.NewPushPoster(f.client(t))
	w.svc2 = notify.NewPushService(sw.pool, log, keys, poster)
	w.svc2.SetClock(sw.clk.now)
	w.sender = notify.NewPushSender(sw.pool, log, keys, poster)
	w.sender.SetClock(sw.clk.now)
	return w
}

func (w *pwo) putRelay(org uuid.UUID) {
	w.t.Helper()
	u, s := relayURL, relaySecret
	if _, err := w.svc2.Put(context.Background(), org, w.user["chen"], notify.PushInput{RelayURL: &u, RelaySecret: &s}); err != nil {
		w.t.Fatalf("Put: %v", err)
	}
}

func (w *pwo) device(org uuid.UUID, user, token string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if _, err := w.svc2.PutDevice(context.Background(), org, w.user[user].UserID, id,
		notify.DeviceInput{Platform: "ios", Token: token, AppVersion: "1.0", Label: user + " phone"}); err != nil {
		w.t.Fatalf("PutDevice: %v", err)
	}
	return id
}

// pushOn turns the push channel on for the global "assigned" (or other) event of a user.
func (w *pwo) pushOn(user string, events ...string) {
	w.t.Helper()
	if len(events) == 0 {
		events = []string{"assigned"}
	}
	for _, ev := range events {
		w.exec(`INSERT INTO notification_settings (org_id, user_id, event, push) VALUES ($1, $2, $3, 'on')
			ON CONFLICT (org_id, user_id, event) WHERE team_id IS NULL DO UPDATE SET push = 'on'`, w.org.ID, w.user[user].UserID, ev)
	}
}

func (w *pwo) consume() notify.Result {
	w.t.Helper()
	res := w.sworld.run()
	w.exec(`UPDATE push_outbox SET created_at = $1, next_attempt_at = $1 WHERE created_at > $1`, w.clk.now())
	return res
}

func (w *pwo) pass() notify.PushResult {
	w.t.Helper()
	res, err := w.sender.RunOnce(context.Background())
	if err != nil {
		w.t.Fatalf("push RunOnce: %v", err)
	}
	return res
}

type pushRow struct {
	ID                         uuid.UUID
	User, State, Skip, LastErr string
	Attempts                   int32
	Next                       time.Time
}

func (w *pwo) outRows() []pushRow {
	w.t.Helper()
	rs, err := w.pool.Query(context.Background(),
		`SELECT o.id, u.username, o.state, coalesce(o.skip_reason, ''), coalesce(o.last_error, ''), o.attempts, o.next_attempt_at
		 FROM push_outbox o JOIN push_devices d ON d.id = o.device_id JOIN users u ON u.id = d.user_id
		 ORDER BY o.created_at, o.id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rs.Close()
	var out []pushRow
	for rs.Next() {
		var r pushRow
		if err := rs.Scan(&r.ID, &r.User, &r.State, &r.Skip, &r.LastErr, &r.Attempts, &r.Next); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (w *pwo) onlyRow() pushRow {
	w.t.Helper()
	r := w.outRows()
	if len(r) != 1 {
		w.t.Fatalf("push_outbox = %+v, want one row", r)
	}
	return r[0]
}

func (w *pwo) relayError() string {
	w.t.Helper()
	var s *string
	if err := w.pool.QueryRow(context.Background(), `SELECT last_error FROM push_relays WHERE org_id = $1`, w.org.ID).Scan(&s); err != nil {
		w.t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (w *pwo) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// ready is a world with a relay, alice's device and alice's push on.
func readyPushWorld(t *testing.T) (*pwo, uuid.UUID) {
	w := newPushWorld(t)
	w.putRelay(w.org.ID)
	d := w.device(w.org.ID, "alice", tokAlice1)
	w.pushOn("alice")
	return w, d
}

// The relay gets a bare wake: a token, a platform, an opaque id, the word
// "wake", and nothing of the issue.
func TestPush_SendsABareWake(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice", Title: "TOPSECRETTITLE", Description: "TOPSECRETBODY"})
	if res := w.consume(); res.Pushes != 1 {
		t.Fatalf("consume %+v", res)
	}
	row := w.onlyRow()
	if row.State != "pending" || row.User != "alice" {
		t.Fatalf("row %+v", row)
	}
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	hits := w.relay.got()
	if len(hits) != 1 {
		t.Fatalf("hits %+v", hits)
	}
	h := hits[0]
	if h.Path != "/t/"+relayURLToken+"/v1/wake" || h.Auth != "Bearer "+relaySecret || h.Type != "application/json" {
		t.Errorf("request %+v", h)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(h.Body), &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if strings.Join(keys, ",") != "id,kind,platform,token" {
		t.Errorf("body keys %v", keys)
	}
	if body["kind"] != "wake" || body["platform"] != "ios" || body["token"] != tokAlice1 || body["id"] != row.ID.String() {
		t.Errorf("body %v", body)
	}
	for _, leak := range []string{"TOPSECRETTITLE", "TOPSECRETBODY", "alice", "chen", "ORB-", "example.test"} {
		if strings.Contains(h.Body, leak) {
			t.Errorf("the push holds %q: %s", leak, h.Body)
		}
	}
	if r := w.onlyRow(); r.State != "sent" {
		t.Errorf("row %+v", r)
	}
	if res := w.pass(); res.Sent != 0 || len(w.relay.got()) != 1 {
		t.Errorf("a second pass sent again: %+v", res)
	}
	if e := w.relayError(); e != "" {
		t.Errorf("relay error %q", e)
	}
}

// Push is off by default, needs a relay, a device and a person who turned it on.
func TestPush_OffByDefaultAndNeedsRelayAndDevice(t *testing.T) {
	w := newPushWorld(t)
	// A relay and a device, but push is off (the default).
	w.putRelay(w.org.ID)
	w.device(w.org.ID, "alice", tokAlice1)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := len(w.outRows()); n != 0 {
		t.Fatalf("a default setting made %d push rows", n)
	}
	// Push on and a relay, but bob has no device.
	w.pushOn("alice")
	w.pushOn("bob")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	if n := len(w.outRows()); n != 0 {
		t.Fatalf("a person with no device got %d rows", n)
	}
	// No relay: the consumer makes nothing, even with a device and push on.
	if err := w.svc2.Delete(context.Background(), w.org.ID, w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	if res := w.consume(); res.Pushes != 0 {
		t.Fatalf("consume %+v", res)
	}
	if n := len(w.outRows()); n != 0 {
		t.Fatalf("no relay but %d push rows", n)
	}
	// With the relay back, the next event makes a row.
	w.putRelay(w.org.ID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := len(w.outRows()); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// The actor is not pushed, and an agent gets nothing (decisions-T13 #14).
func TestPush_ActorAndAgentsGetNothing(t *testing.T) {
	w := newPushWorld(t)
	w.putRelay(w.org.ID)
	w.device(w.org.ID, "chen", "TOKEN-c-")
	w.pushOn("chen", "assigned", "comment")
	// An agent member with a device and push on.
	w.exec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, w.org.ID, w.user["claude"].UserID)
	w.exec(`INSERT INTO push_devices (id, org_id, user_id, platform, token_enc, token_hash, last_seen_at)
		VALUES (gen_random_uuid(), $1, $2, 'ios', decode(repeat('00', 40), 'hex'), sha256('agent-token'::bytea), now())`, w.org.ID, w.user["claude"].UserID)
	w.pushOn("claude", "assigned", "comment")
	// chen assigns to himself, and to the agent.
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "chen"})
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "claude"})
	w.consume()
	if rows := w.outRows(); len(rows) != 0 {
		t.Fatalf("actor or agent was pushed: %+v", rows)
	}
}

// A person with every other channel off but push on gets a push, and the
// notification is not in the inbox.
func TestPush_OnlyChannelMakesAHiddenNotification(t *testing.T) {
	w := newPushWorld(t)
	w.putRelay(w.org.ID)
	w.device(w.org.ID, "alice", tokAlice1)
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox, push) VALUES ($1, $2, 'assigned', 'off', 'off', 'on')`,
		w.org.ID, w.user["alice"].UserID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := w.count(`SELECT count(*) FROM notifications WHERE recipient_id = $1 AND NOT inbox`, w.user["alice"].UserID); n != 1 {
		t.Fatalf("hidden notifications = %d", n)
	}
	if n := len(w.outRows()); n != 1 {
		t.Fatalf("push rows = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM notification_emails`); n != 0 {
		t.Fatalf("email rows = %d", n)
	}
}

// Push on, everything else off, relay set, but no device: nothing is stored at all
// (no hidden notification either).
func TestPush_NoDeviceMakesNoHiddenNotification(t *testing.T) {
	w := newPushWorld(t)
	w.putRelay(w.org.ID)
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox, push) VALUES ($1, $2, 'assigned', 'off', 'off', 'on')`,
		w.org.ID, w.user["alice"].UserID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := w.count(`SELECT count(*) FROM notifications WHERE recipient_id = $1`, w.user["alice"].UserID); n != 0 {
		t.Fatalf("notifications = %d, want 0", n)
	}
	if n := len(w.outRows()); n != 0 {
		t.Fatalf("push rows = %d", n)
	}
}

// One row per device; a replay of the feed makes no second row.
func TestPush_OneRowPerDeviceAndReplayIsQuiet(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.device(w.org.ID, "alice", tokAlice2)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := len(w.outRows()); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	w.exec(`UPDATE notification_cursors SET last_seq = 0`)
	w.consume()
	if n := len(w.outRows()); n != 2 {
		t.Fatalf("rows after replay = %d, want 2", n)
	}
	if res := w.pass(); res.Sent != 2 {
		t.Fatalf("pass %+v", res)
	}
	ids := map[string]bool{}
	for _, h := range w.relay.got() {
		var b struct{ ID, Token string }
		_ = json.Unmarshal([]byte(h.Body), &b)
		ids[b.ID] = true
	}
	if len(ids) != 2 {
		t.Errorf("ids %v", ids)
	}
}

func TestPush_Failures(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantState string
		wantErr   string
	}{
		{"server error retries", 500, "pending", "push_unavailable"},
		{"rate limit retries", 429, "pending", "push_rate_limited"},
		{"bad bearer is final", 401, "dead", "push_unauthorized"},
		{"forbidden is final", 403, "dead", "push_unauthorized"},
		{"bad request is final", 400, "dead", "push_rejected"},
		{"not found is final", 404, "dead", "push_rejected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, _ := readyPushWorld(t)
			w.relay.set(c.status, "")
			w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
			w.consume()
			w.pass()
			r := w.onlyRow()
			if r.State != c.wantState || r.LastErr != c.wantErr {
				t.Fatalf("row %+v, want %s/%s", r, c.wantState, c.wantErr)
			}
			if e := w.relayError(); e != c.wantErr {
				t.Errorf("relay error = %q, want %q", e, c.wantErr)
			}
			if c.wantState == "pending" {
				if r.Attempts != 1 || !r.Next.After(w.clk.now()) {
					t.Errorf("retry not scheduled: %+v", r)
				}
				before := len(w.relay.got())
				w.pass()
				if len(w.relay.got()) != before {
					t.Error("sent before the retry time")
				}
			}
		})
	}
}

// 1m, 5m, 30m, 2h, 12h, then dead on the sixth failure.
func TestPush_RetryScheduleThenDead(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.relay.set(500, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}
	for i, d := range want {
		before := w.clk.now()
		w.pass()
		r := w.onlyRow()
		if r.State != "pending" || int(r.Attempts) != i+1 || r.Next.Sub(before) != d {
			t.Fatalf("after failure %d: %+v (next in %v, want %v)", i+1, r, r.Next.Sub(before), d)
		}
		w.clk.advance(d + time.Second)
		w.exec(`UPDATE push_outbox SET created_at = $1`, w.clk.now()) // keep the 24 h expiry out of the way
	}
	w.pass()
	if r := w.onlyRow(); r.State != "dead" || r.Attempts != 6 || r.LastErr != "push_unavailable" {
		t.Fatalf("final row %+v", r)
	}
	// A success later clears the relay error.
	w.relay.set(200, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	if e := w.relayError(); e != "" {
		t.Errorf("relay error after a success = %q", e)
	}
}

func TestPush_RetryAfterIsHonouredAndBounded(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.relay.set(429, "3600")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	before := w.clk.now()
	w.pass()
	if r := w.onlyRow(); r.Next.Sub(before) != time.Hour {
		t.Fatalf("next in %v, want 1h", r.Next.Sub(before))
	}
	// A huge value is bounded before it is multiplied: no overflow, one hour at most.
	w.exec(`UPDATE push_outbox SET state = 'pending', attempts = 0, next_attempt_at = $1`, w.clk.now())
	w.relay.set(429, "9223372036854775807")
	before = w.clk.now()
	w.pass()
	if r := w.onlyRow(); r.Next.Sub(before) != time.Hour {
		t.Fatalf("huge Retry-After: next in %v, want 1h", r.Next.Sub(before))
	}
}

// A 410 for a token the app has refreshed since the wake left must not delete the
// device that now holds the new token (the sender and the test push).
func TestPush_InvalidTokenKeepsARefreshedDevice(t *testing.T) {
	w, d1 := readyPushWorld(t)
	w.relay.invalid[tokAlice1] = true
	w.relay.onHit = func(tok string) {
		if tok == tokAlice1 {
			if _, err := w.svc2.PutDevice(context.Background(), w.org.ID, w.user["alice"].UserID, d1, notify.DeviceInput{Platform: "ios", Token: "REFRESHED-TOKEN-1"}); err != nil {
				t.Errorf("refresh: %v", err)
			}
		}
	}
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	res := w.pass()
	if res.Invalid != 0 {
		t.Fatalf("pass %+v: a refreshed device counted as invalid", res)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND token_hash = sha256('REFRESHED-TOKEN-1'::bytea)`, d1); n != 1 {
		t.Fatal("the refreshed device was deleted")
	}
	if rows := w.outRows(); len(rows) != 1 || rows[0].State != "skipped" {
		t.Fatalf("rows %+v, want one skipped", rows)
	}
}

func TestPush_TestPushKeepsARefreshedDevice(t *testing.T) {
	w := newPushWorld(t)
	w.putRelay(w.org.ID)
	d := w.device(w.org.ID, "chen", tokAlice1)
	w.relay.invalid[tokAlice1] = true
	w.relay.onHit = func(tok string) {
		if tok == tokAlice1 {
			if _, err := w.svc2.PutDevice(context.Background(), w.org.ID, w.user["chen"].UserID, d, notify.DeviceInput{Platform: "ios", Token: "REFRESHED-TOKEN-2"}); err != nil {
				t.Errorf("refresh: %v", err)
			}
		}
	}
	_, _ = w.svc2.Test(context.Background(), w.org.ID, w.user["chen"])
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND token_hash = sha256('REFRESHED-TOKEN-2'::bytea)`, d); n != 1 {
		t.Fatal("the refreshed device was deleted by the test push")
	}
}

// The relay says the token is dead: the device and its rows are deleted.
func TestPush_InvalidTokenDeletesTheDevice(t *testing.T) {
	w, d1 := readyPushWorld(t)
	d2 := w.device(w.org.ID, "alice", tokAlice2)
	w.relay.invalid[tokAlice1] = true
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	res := w.pass()
	if res.Invalid != 1 || res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, d1); n != 0 {
		t.Error("the invalid device is still there")
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, d2); n != 1 {
		t.Error("the good device was removed")
	}
	if rows := w.outRows(); len(rows) != 1 || rows[0].State != "sent" {
		t.Fatalf("rows %+v", rows)
	}
	// An invalid token is not a relay fault.
	if e := w.relayError(); e != "" {
		t.Errorf("relay error %q", e)
	}
}

func TestPush_RedirectNotFollowed(t *testing.T) {
	w, _ := readyPushWorld(t)
	other := newFakeRelay(t)
	w.relay.mu.Lock()
	w.relay.redirect = other.URL + "/elsewhere"
	w.relay.mu.Unlock()
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	if r := w.onlyRow(); r.State != "dead" || r.LastErr != "push_redirect" {
		t.Fatalf("row %+v", r)
	}
	if n := len(other.got()); n != 0 {
		t.Fatalf("the redirect was followed (%d hits)", n)
	}
}

func asPushErr(err error, target **notify.PushError) bool {
	pe, ok := err.(*notify.PushError)
	if ok {
		*target = pe
	}
	return ok
}

// The dial guard refuses private, loopback, link-local and metadata addresses
// on the resolved address (the relay host is entered by the admin).
func TestPush_DialGuardRefusesPrivateAndMetadata(t *testing.T) {
	for name, addr := range map[string]string{
		"loopback": "127.0.0.1", "private 10": "10.0.0.5", "private 192": "192.168.1.1", "private 172": "172.16.0.9",
		"cgnat": "100.64.0.1", "link local": "169.254.1.1", "metadata": "169.254.169.254", "unspecified": "0.0.0.0",
		"ipv6 loopback": "::1", "ipv6 unique": "fd00::1", "ipv4-mapped": "::ffff:10.0.0.1", "nat64 to 10.x": "64:ff9b::a00:1",
	} {
		t.Run(name, func(t *testing.T) {
			res := &netguardtest.MapResolver{Hosts: map[string][]string{"relay.example.com": {addr}}}
			d := &netguard.Dialer{Policy: notify.PushPolicy(nil), Resolver: res, Timeout: 2 * time.Second}
			err := notify.NewPushPoster(notify.NewPushClientFor(d)).Wake(context.Background(), relayURL, relaySecret, notify.PushWake{Token: tokAlice1, Platform: "ios", ID: "x"})
			var pe *notify.PushError
			if !asPushErr(err, &pe) || pe.Word != notify.PushBlocked {
				t.Fatalf("Wake = %v, want push_blocked", err)
			}
			for _, s := range []string{relayURLToken, relaySecret, tokAlice1, "relay.example.com"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("the error holds %q", s)
				}
			}
		})
	}
	// One refused answer among good ones refuses the host.
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"relay.example.com": {"8.8.8.8", "10.0.0.5"}}}
	d := &netguard.Dialer{Policy: notify.PushPolicy(nil), Resolver: res, Timeout: 2 * time.Second}
	err := notify.NewPushPoster(notify.NewPushClientFor(d)).Wake(context.Background(), relayURL, relaySecret, notify.PushWake{Token: "t", Platform: "ios", ID: "x"})
	var pe *notify.PushError
	if !asPushErr(err, &pe) || pe.Word != notify.PushBlocked {
		t.Fatalf("mixed answer: %v", err)
	}
}

// The poster checks the URL again: an http URL, or one with user info, never dials.
func TestPush_PosterRefusesBadURLs(t *testing.T) {
	w := newPushWorld(t)
	poster := notify.NewPushPoster(w.relay.client(t))
	for _, u := range []string{"http://relay.example.com/", "https://u:p@relay.example.com/", "ftp://relay.example.com", "", "https://relay.example.com/?x=1"} {
		err := poster.Wake(context.Background(), u, relaySecret, notify.PushWake{Token: "t", Platform: "ios", ID: "x"})
		var pe *notify.PushError
		if !asPushErr(err, &pe) || pe.Word != notify.PushBlocked {
			t.Errorf("Wake(%q) = %v", u, err)
		}
	}
	if n := len(w.relay.got()); n != 0 {
		t.Errorf("a bad URL reached the relay (%d)", n)
	}
	if err := notify.NewPushPoster(nil).Wake(context.Background(), relayURL, relaySecret, notify.PushWake{}); err == nil {
		t.Error("a poster with no client sent")
	}
}

// bulk makes n hidden notifications for alice (copies of her latest one) and
// returns their ids. Used to make many outbox rows without many events.
func (w *pwo) bulk(prefix string, n int) []uuid.UUID {
	w.t.Helper()
	rs, err := w.pool.Query(context.Background(), `
		INSERT INTO notifications (org_id, recipient_id, kind, issue_id, data, dedupe_key, inbox)
		SELECT b.org_id, b.recipient_id, b.kind, b.issue_id, b.data, $1::text || g, false
		FROM (SELECT * FROM notifications ORDER BY created_at DESC LIMIT 1) b, generate_series(1, $2::int) g
		RETURNING id`, prefix, n)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rs.Close()
	var ids []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			w.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (w *pwo) firstDevice() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM push_devices ORDER BY created_at, id LIMIT 1`).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func TestPush_RateCap(t *testing.T) {
	w, dev := readyPushWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM push_outbox`)
	// PushRateCap pushes already went in the last hour.
	old := w.bulk("old:", notify.PushRateCap)
	w.exec(`INSERT INTO push_outbox (org_id, notification_id, device_id, state, sent_at)
		SELECT $1, n, $2, 'sent', $3 FROM unnest($4::uuid[]) n`, w.org.ID, dev, w.clk.now().Add(-time.Minute), old)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	if n := w.count(`SELECT count(*) FROM push_outbox WHERE skip_reason = 'rate_cap'`); n != 1 {
		t.Fatalf("rate_cap rows = %d", n)
	}
	if n := len(w.relay.got()); n != 0 {
		t.Errorf("sent over the cap: %d", n)
	}
	// The admin sees the count.
	v, err := w.svc2.Get(context.Background(), w.org.ID)
	if err != nil || v.RateCappedCount != 1 || v.DeviceCount != 1 {
		t.Errorf("view %+v %v", v, err)
	}
	// An hour later the window has moved on.
	w.clk.advance(notify.PushRateWindow + time.Minute)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if res := w.pass(); res.Sent != 1 {
		t.Errorf("after the window: %+v", res)
	}
}

func TestPush_PerPassLimitAndFairAcrossOrgs(t *testing.T) {
	w, dev := readyPushWorld(t)
	ctx := context.Background()
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM push_outbox`)
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "alice")
	w.putRelay(org2.ID)
	dev2 := w.device(org2.ID, "alice", "TOKEN-org2")
	ids := w.bulk("a:", 60)
	w.exec(`INSERT INTO push_outbox (org_id, notification_id, device_id, created_at, next_attempt_at)
		SELECT $1, n, $2, $3, $3 FROM unnest($4::uuid[]) n`, w.org.ID, dev, w.clk.now().Add(-time.Minute), ids)
	// One notification of org 2, newer than all of org 1's.
	var n2 uuid.UUID
	if err := w.pool.QueryRow(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, data, dedupe_key, inbox)
		SELECT $1, $2, 'assigned', i.id, '{}', 'o2', false FROM issues i WHERE i.org_id = $1 LIMIT 1 RETURNING id`, org2.ID, w.user["alice"].UserID).Scan(&n2); err != nil {
		// org 2 has no issue: make one
		o2 := &auth.Org{ID: org2.ID, Slug: "other", Name: "Other", Role: "admin", Access: auth.TeamAccess{All: true}}
		is, cerr := w.svc.CreateIssue(ctx, o2, w.user["chen"], tracker.CreateIssueInput{Team: "OTH", Title: "t"})
		if cerr != nil {
			t.Fatal(cerr)
		}
		if err := w.pool.QueryRow(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, data, dedupe_key, inbox)
			VALUES ($1, $2, 'assigned', $3, '{}', 'o2', false) RETURNING id`, org2.ID, w.user["alice"].UserID, is.ID).Scan(&n2); err != nil {
			t.Fatal(err)
		}
	}
	w.exec(`INSERT INTO push_outbox (org_id, notification_id, device_id, created_at, next_attempt_at) VALUES ($1, $2, $3, $4, $4)`,
		org2.ID, n2, dev2, w.clk.now().Add(-time.Second))
	res := w.pass()
	if res.Sent != notify.PushPerOrgPerPass+1 {
		t.Fatalf("first pass sent %d, want %d", res.Sent, notify.PushPerOrgPerPass+1)
	}
	if n := w.count(`SELECT count(*) FROM push_outbox WHERE org_id = $1 AND state = 'sent'`, org2.ID); n != 1 {
		t.Fatalf("org 2 pushes in the first pass = %d, want 1", n)
	}
	// The relay of org 1 never got org 2's device token.
	if n := w.count(`SELECT count(*) FROM push_outbox WHERE org_id = $1 AND state = 'sent'`, w.org.ID); n != notify.PushPerOrgPerPass {
		t.Fatalf("org 1 sent = %d", n)
	}
}

func TestPush_SkipsWhenRelayChangesAndDeleteDropsWaitingRows(t *testing.T) {
	w, _ := readyPushWorld(t)
	ctx := context.Background()
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	// The admin removes the relay: what waits is deleted, nothing is sent, devices stay.
	if err := w.svc2.Delete(ctx, w.org.ID, w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM push_outbox WHERE state IN ('pending','sending')`); n != 0 {
		t.Fatalf("waiting rows after the delete = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM push_devices`); n != 1 {
		t.Fatalf("devices after the delete = %d", n)
	}
	w.pass()
	if n := len(w.relay.got()); n != 0 {
		t.Error("sent after the relay was removed")
	}
	// A row that raced the delete is skipped, never sent.
	w.putRelay(w.org.ID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM push_relays`)
	if res := w.pass(); res.Sent != 0 || res.Skipped != 1 {
		t.Fatalf("pass %+v", res)
	}
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "disabled" {
		t.Fatalf("row %+v", r)
	}
}

func TestPush_ReadNotificationIsSkipped(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`UPDATE notifications SET read_at = now()`)
	w.pass()
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "read" {
		t.Fatalf("row %+v", r)
	}
	if n := len(w.relay.got()); n != 0 {
		t.Errorf("sent for a read notification: %d", n)
	}
}

func TestPush_ExpiredAfter24Hours(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.relay.set(500, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	w.clk.advance(notify.PushRowTTL + time.Minute)
	w.relay.set(200, "")
	w.pass()
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "expired" {
		t.Fatalf("row %+v", r)
	}
}

// A key change makes the blobs unreadable: nothing is sent, the relay says so.
func TestPush_UnreadableSecret(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	other, _ := platform.NewPushKeyring([]byte("ffffffffffffffffffffffffffffffff"))
	poster := notify.NewPushPoster(w.relay.client(t))
	w.sender = notify.NewPushSender(w.pool, slog.New(slog.NewTextHandler(w.plogs, nil)), other, poster)
	w.sender.SetClock(w.clk.now)
	w.pass()
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "unreadable" {
		t.Fatalf("row %+v", r)
	}
	if e := w.relayError(); e != "push_unreadable" {
		t.Errorf("relay error %q", e)
	}
	if n := len(w.relay.got()); n != 0 {
		t.Errorf("sent with an unreadable secret: %d", n)
	}
	svc := notify.NewPushService(w.pool, slog.New(slog.NewTextHandler(w.plogs, nil)), other, poster)
	v, err := svc.Get(context.Background(), w.org.ID)
	if err != nil || !v.SecretUnreadable || !v.Configured {
		t.Errorf("view %+v %v", v, err)
	}
}

// A row the claim cannot take (it left pending between the list and the claim)
// is never sent.
func TestPush_UnclaimedRowIsNotSent(t *testing.T) {
	w, _ := readyPushWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`CREATE FUNCTION push_no_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
	w.exec(`CREATE TRIGGER push_no_claim BEFORE UPDATE ON push_outbox FOR EACH ROW WHEN (NEW.state = 'sending') EXECUTE FUNCTION push_no_claim()`)
	if res := w.pass(); res.Sent != 0 {
		t.Fatalf("pass %+v", res)
	}
	if n := len(w.relay.got()); n != 0 {
		t.Fatalf("an unclaimed row was sent (%d)", n)
	}
	if r := w.onlyRow(); r.State != "pending" {
		t.Fatalf("row %+v", r)
	}
}

// The sender deletes a device not seen for 90 days, and old terminal rows.
func TestPush_SenderPrunesStaleDevices(t *testing.T) {
	w, _ := readyPushWorld(t)
	stale := w.device(w.org.ID, "bob", tokBob)
	w.exec(`UPDATE push_devices SET last_seen_at = $2 WHERE id = $1`, stale, w.clk.now().Add(-notify.PushDeviceRetention-time.Hour))
	res := w.pass()
	if res.PrunedDev != 1 {
		t.Fatalf("pass %+v", res)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, stale); n != 0 {
		t.Error("the stale device is still there")
	}
	if n := w.count(`SELECT count(*) FROM push_devices`); n != 1 {
		t.Errorf("devices = %d, want alice's", n)
	}
}

func TestPush_CrossOrg(t *testing.T) {
	w, dev := readyPushWorld(t)
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "alice")
	w.addMember(org2.ID, "bob")
	// The id is chosen by the client and is unique per org: org 2 registering the same id is
	// no denial. It gets its own row, and org 1's device is untouched.
	if _, err = w.svc2.PutDevice(ctx, org2.ID, w.user["alice"].UserID, dev, notify.DeviceInput{Platform: "android", Token: "OTHERORG-TOKEN"}); err != nil {
		t.Fatalf("PutDevice with an id that exists in another org = %v, want ok", err)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, dev); n != 2 {
		t.Fatalf("rows with that id = %d, want 2 (one per org)", n)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND org_id = $2 AND platform = 'ios'`, dev, w.org.ID); n != 1 {
		t.Fatal("org 1's device was changed")
	}
	// Org 2's delete removes only its own row.
	if err := w.svc2.DeleteDevice(ctx, org2.ID, w.user["alice"].UserID, dev); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND org_id = $2`, dev, w.org.ID); n != 1 {
		t.Fatal("deleted another org's device")
	}
	// Nor another member's in the same org.
	_, err = w.svc2.PutDevice(ctx, w.org.ID, w.user["bob"].UserID, dev, notify.DeviceInput{Platform: "ios", Token: "STOLEN"})
	if he, ok := err.(*httpx.Error); !ok || he.Status != 404 {
		t.Fatalf("PutDevice over another member's id = %v, want 404", err)
	}
	if err := w.svc2.DeleteDevice(ctx, w.org.ID, w.user["bob"].UserID, dev); err == nil {
		t.Fatal("deleted another member's device")
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND user_id = $2`, dev, w.user["alice"].UserID); n != 1 {
		t.Fatal("alice's device changed")
	}
	// Lists are per org and per member.
	l, _ := w.svc2.ListDevices(ctx, org2.ID, w.user["alice"].UserID)
	if len(l.Items) != 0 || l.RelayConfigured {
		t.Errorf("org 2 list %+v", l)
	}
	// A relay blob and a token blob of org 1 do not open as org 2's.
	var urlBlob, tokBlob []byte
	if err := w.pool.QueryRow(ctx, `SELECT url_enc FROM push_relays WHERE org_id = $1`, w.org.ID).Scan(&urlBlob); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT token_enc FROM push_devices WHERE id = $1`, dev).Scan(&tokBlob); err != nil {
		t.Fatal(err)
	}
	if _, err := w.keys.OpenFor(platform.PushColRelayURL, org2.ID.String(), urlBlob); err == nil {
		t.Error("org 1's relay blob opens as org 2's")
	}
	if _, err := w.keys.OpenFor(platform.PushColRelaySecret, w.org.ID.String(), urlBlob); err == nil {
		t.Error("the URL blob opens as the secret column")
	}
	if _, err := w.keys.OpenFor(platform.PushColDeviceToken, org2.ID.String()+"."+dev.String(), tokBlob); err == nil {
		t.Error("a token blob opens in another org")
	}
	if _, err := w.keys.OpenFor(platform.PushColDeviceToken, w.org.ID.String()+"."+uuid.NewString(), tokBlob); err == nil {
		t.Error("a token blob opens for another device id")
	}
	if tok, err := w.keys.OpenFor(platform.PushColDeviceToken, w.org.ID.String()+"."+dev.String(), tokBlob); err != nil || string(tok) != tokAlice1 {
		t.Errorf("own blob: %q %v", tok, err)
	}
	// A blob sealed with another keyring (Slack) never opens as push.
	sk, _ := platform.NewSlackKeyring(pushKey)
	if _, err := w.keys.OpenFor("webhook_enc", w.org.ID.String(), sk.SealFor("webhook_enc", w.org.ID.String(), []byte("x"))); err == nil {
		t.Error("a Slack blob opens as a push blob")
	}
	// Org 2's own relay: removing org 1's leaves it.
	u, sec := "https://relay.example.com/two", "SECRET-TWO-00"
	if _, err := w.svc2.Put(ctx, org2.ID, w.user["chen"], notify.PushInput{RelayURL: &u, RelaySecret: &sec}); err != nil {
		t.Fatal(err)
	}
	if err := w.svc2.Delete(ctx, w.org.ID, w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	if v, _ := w.svc2.Get(ctx, org2.ID); !v.Configured {
		t.Error("org 2's relay was removed with org 1's")
	}
}

func TestPush_DeviceRules(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	alice, bob := w.user["alice"].UserID, w.user["bob"].UserID
	put := func(user uuid.UUID, id uuid.UUID, in notify.DeviceInput) (notify.DeviceView, error) {
		return w.svc2.PutDevice(ctx, w.org.ID, user, id, in)
	}
	good := notify.DeviceInput{Platform: "android", Token: "TOKEN-A", AppVersion: "2.1.0", Label: "Pixel"}
	// Validation: nothing echoes the token.
	for name, in := range map[string]notify.DeviceInput{
		"platform": {Platform: "web", Token: "TOKEN-X"}, "empty token": {Platform: "ios"},
		"long token": {Platform: "ios", Token: strings.Repeat("a", notify.MaxPushToken+1)},
		"space":      {Platform: "ios", Token: "TOKEN X"}, "non ascii": {Platform: "ios", Token: "TOKEN-é"},
		"long label": {Platform: "ios", Token: "T", Label: strings.Repeat("x", 81)},
		"control":    {Platform: "ios", Token: "T", Label: "a\nb"},
		"long ver":   {Platform: "ios", Token: "T", AppVersion: strings.Repeat("1", 65)},
	} {
		_, err := put(alice, uuid.New(), in)
		he, ok := err.(*httpx.Error)
		if !ok || he.Status != 400 {
			t.Errorf("%s: %v, want 400", name, err)
			continue
		}
		if in.Token != "" && strings.Contains(err.Error(), in.Token) && len(in.Token) > 3 {
			t.Errorf("%s: the error repeats the token", name)
		}
	}
	if _, err := put(alice, uuid.New(), notify.DeviceInput{Platform: "ios", Token: strings.Repeat("a", notify.MaxPushToken)}); err != nil {
		t.Fatalf("a token of 4096 characters: %v", err)
	}
	w.exec(`DELETE FROM push_devices`)
	// Register, refresh (lastSeenAt moves, the id stays), and the view holds no token.
	id := uuid.New()
	v1, err := put(alice, id, good)
	if err != nil || v1.Platform != "android" || v1.Label != "Pixel" {
		t.Fatalf("register %+v %v", v1, err)
	}
	w.clk.advance(time.Hour)
	good.Label = "Pixel 9"
	v2, err := put(alice, id, good)
	if err != nil || !v2.LastSeenAt.After(v1.LastSeenAt) || v2.Label != "Pixel 9" || !v2.CreatedAt.Equal(v1.CreatedAt) {
		t.Fatalf("refresh %+v %v", v2, err)
	}
	if b, _ := json.Marshal(v2); strings.Contains(string(b), "TOKEN-A") {
		t.Errorf("the view holds the token: %s", b)
	}
	// One row per token: a second id with the same token takes it over (a phone changed hands).
	id2 := uuid.New()
	if _, err := put(bob, id2, good); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, id); n != 0 {
		t.Error("the old row for the token is still there")
	}
	if l, _ := w.svc2.ListDevices(ctx, w.org.ID, alice); len(l.Items) != 0 {
		t.Errorf("alice still lists %d", len(l.Items))
	}
	if l, _ := w.svc2.ListDevices(ctx, w.org.ID, bob); len(l.Items) != 1 || l.MaxDevices != notify.PushMaxDevices {
		t.Errorf("bob list %+v", l)
	}
	// The cap: PushMaxDevices per member; a refresh at the cap is fine, one more is 409.
	for i := 0; i < notify.PushMaxDevices; i++ {
		if _, err := put(alice, uuid.New(), notify.DeviceInput{Platform: "ios", Token: "CAP-" + uuid.NewString()}); err != nil {
			t.Fatalf("device %d: %v", i, err)
		}
	}
	_, err = put(alice, uuid.New(), notify.DeviceInput{Platform: "ios", Token: "ONE-TOO-MANY"})
	if he, ok := err.(*httpx.Error); !ok || he.Status != 409 {
		t.Fatalf("over the cap: %v, want 409", err)
	}
	var someone uuid.UUID
	if err := w.pool.QueryRow(ctx, `SELECT id FROM push_devices WHERE user_id = $1 LIMIT 1`, alice).Scan(&someone); err != nil {
		t.Fatal(err)
	}
	if _, err := put(alice, someone, notify.DeviceInput{Platform: "ios", Token: "REFRESHED-AT-CAP"}); err != nil {
		t.Fatalf("refresh at the cap: %v", err)
	}
	// Bob's cap is his own.
	if _, err := put(bob, uuid.New(), notify.DeviceInput{Platform: "ios", Token: "BOB-2"}); err != nil {
		t.Fatal(err)
	}
	// Retention on write: a device not seen for 90 days goes at the owner's next registration,
	// which also frees a place at the cap.
	w.exec(`UPDATE push_devices SET last_seen_at = $2 WHERE id = $1`, someone, w.clk.now().Add(-notify.PushDeviceRetention-time.Hour))
	if _, err := put(alice, uuid.New(), notify.DeviceInput{Platform: "ios", Token: "AFTER-RETENTION"}); err != nil {
		t.Fatalf("after the stale device went: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, someone); n != 0 {
		t.Error("the stale device survived a registration")
	}
	// Delete: own device, then a repeat is 404.
	if err := w.svc2.DeleteDevice(ctx, w.org.ID, bob, id2); err != nil {
		t.Fatal(err)
	}
	if err := w.svc2.DeleteDevice(ctx, w.org.ID, bob, id2); err == nil {
		t.Error("a repeat delete answered success")
	}
	// No SETTINGS_ENCRYPTION_KEY: a device cannot be stored.
	svc := notify.NewPushService(w.pool, slog.New(slog.NewTextHandler(w.plogs, nil)), nil, nil)
	_, err = svc.PutDevice(ctx, w.org.ID, alice, uuid.New(), good)
	if he, ok := err.(*httpx.Error); !ok || he.Status != 409 || he.Code != httpx.CodeEncryptionUnavailable {
		t.Errorf("no key: %v", err)
	}
}

func TestPush_PutRelayRules(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	chen := w.user["chen"]
	str := func(s string) *string { return &s }
	bad := func(in notify.PushInput) {
		t.Helper()
		_, err := w.svc2.Put(ctx, w.org.ID, chen, in)
		he, ok := err.(*httpx.Error)
		if !ok || he.Status != 400 {
			t.Errorf("Put(%+v) = %v, want 400", in, err)
			return
		}
		for _, p := range []*string{in.RelayURL, in.RelaySecret} {
			if p != nil && len(*p) > 3 && strings.Contains(err.Error(), *p) {
				t.Errorf("the error repeats the value: %v", err)
			}
		}
	}
	for _, u := range []string{"http://relay.example.com/", "ftp://x", "https://u:p@relay.example.com/", "https://relay.example.com/?k=1",
		"https://relay.example.com/#f", "https://", "relay.example.com", "", "https://relay.example.com/a b", "https://relay.example.com/" + strings.Repeat("a", 600)} {
		bad(notify.PushInput{RelayURL: str(u), RelaySecret: str("SECRETSECRET")})
	}
	// An IP literal in a refused range, or an internal name, is refused at save.
	for _, u := range []string{"https://127.0.0.1/", "https://10.1.2.3:8443/x", "https://192.168.0.5", "https://172.16.0.1", "https://169.254.169.254/latest",
		"https://100.64.0.1", "https://0.0.0.0", "https://[::1]/", "https://[fd00::1]/", "https://[::ffff:10.0.0.1]/", "https://[fe80::1]/"} {
		bad(notify.PushInput{RelayURL: str(u), RelaySecret: str("SECRETSECRET")})
	}
	for _, s := range []string{"", "has space", "ünïcode", strings.Repeat("s", 513)} {
		bad(notify.PushInput{RelayURL: str(relayURL), RelaySecret: str(s)})
	}
	bad(notify.PushInput{})                        // nothing
	bad(notify.PushInput{RelayURL: str(relayURL)}) // a new relay needs both
	bad(notify.PushInput{RelaySecret: str("SECRETSECRET")})
	if n := w.count(`SELECT count(*) FROM push_relays`); n != 0 {
		t.Fatal("a refused request stored a relay")
	}
	// Events: set, partial change, delete, repeat delete.
	w.putRelay(w.org.ID)
	if v, _ := w.svc2.Get(ctx, w.org.ID); !v.Configured || v.Host == nil || *v.Host != "relay.example.com" || !v.Available {
		t.Fatalf("view %+v", v)
	}
	if _, err := w.svc2.Put(ctx, w.org.ID, chen, notify.PushInput{RelaySecret: str("NEWSECRET-123")}); err != nil {
		t.Fatalf("secret only: %v", err)
	}
	// A new host without a new secret is refused: the kept secret must not go to a host
	// the admin has just named.
	bad(notify.PushInput{RelayURL: str("https://Other.Example.com:8443/x")})
	if v, _ := w.svc2.Get(ctx, w.org.ID); *v.Host != "relay.example.com" {
		t.Fatalf("a refused change moved the host to %q", *v.Host)
	}
	// The same host (another path or port) keeps the secret.
	if _, err := w.svc2.Put(ctx, w.org.ID, chen, notify.PushInput{RelayURL: str("https://Relay.Example.com:8443/v2")}); err != nil {
		t.Fatalf("url only, same host: %v", err)
	}
	if _, err := w.svc2.Put(ctx, w.org.ID, chen, notify.PushInput{RelayURL: str("https://Other.Example.com:8443/x"), RelaySecret: str("OTHERSECRET-1")}); err != nil {
		t.Fatalf("new host with a new secret: %v", err)
	}
	if v, _ := w.svc2.Get(ctx, w.org.ID); *v.Host != "other.example.com" || v.SecretUnreadable {
		t.Fatalf("view after a partial change %+v", v)
	}
	if n := w.count(`SELECT count(*) FROM events WHERE type = 'push.updated'`); n != 4 {
		t.Errorf("push.updated events = %d, want 4", n)
	}
	if err := w.svc2.Delete(ctx, w.org.ID, chen); err != nil {
		t.Fatal(err)
	}
	if err := w.svc2.Delete(ctx, w.org.ID, chen); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM events WHERE type = 'push.updated'`); n != 5 {
		t.Errorf("push.updated events = %d, want 5 (the repeat delete emits none)", n)
	}
	// A device registration emits nothing.
	w.device(w.org.ID, "alice", tokAlice1)
	if n := w.count(`SELECT count(*) FROM events WHERE type LIKE 'push%'`); n != 5 {
		t.Errorf("a device registration emitted an event (%d)", n)
	}
	// No key: the relay cannot be stored.
	svc := notify.NewPushService(w.pool, slog.New(slog.NewTextHandler(w.plogs, nil)), nil, nil)
	_, err := svc.Put(ctx, w.org.ID, chen, notify.PushInput{RelayURL: str(relayURL), RelaySecret: str("SECRETSECRET")})
	if he, ok := err.(*httpx.Error); !ok || he.Status != 409 || he.Code != httpx.CodeEncryptionUnavailable {
		t.Errorf("no key: %v", err)
	}
	if v, _ := svc.Get(ctx, w.org.ID); v.Available {
		t.Error("available without a key")
	}
}

func TestPush_TestPushNeedsARelay(t *testing.T) {
	w := newPushWorld(t)
	_, err := w.svc2.Test(context.Background(), w.org.ID, w.user["chen"])
	if he, ok := err.(*httpx.Error); !ok || he.Status != 404 {
		t.Fatalf("no relay: %v", err)
	}
}

func TestPush_TestPush(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	chen := w.user["chen"]
	w.putRelay(w.org.ID)
	// No device of the caller (alice has one, chen has none).
	w.device(w.org.ID, "alice", tokAlice1)
	if _, err := w.svc2.Test(ctx, w.org.ID, chen); err == nil {
		t.Fatal("test with no device succeeded")
	} else if he, ok := err.(*httpx.Error); !ok || he.Status != 409 {
		t.Fatalf("no device: %v", err)
	}
	if n := len(w.relay.got()); n != 0 {
		t.Fatalf("another member's device got a test push (%d)", n)
	}
	// The caller's own devices only; a dead token is removed.
	w.device(w.org.ID, "chen", "TOKEN-c-1")
	dead := w.device(w.org.ID, "chen", "TOKEN-c-dead")
	w.relay.invalid["TOKEN-c-dead"] = true
	res, err := w.svc2.Test(ctx, w.org.ID, chen)
	if err != nil || res.Sent != 1 || res.Removed != 1 {
		t.Fatalf("test %+v %v", res, err)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE id = $1`, dead); n != 0 {
		t.Error("the dead device is still there")
	}
	for _, h := range w.relay.got() {
		if strings.Contains(h.Body, tokAlice1) {
			t.Error("alice's token was used")
		}
	}
	// A failing relay: 502 push_send_failed with the word and nothing else.
	w.relay.set(401, "")
	_, err = w.svc2.Test(ctx, w.org.ID, chen)
	he, ok := err.(*httpx.Error)
	if !ok || he.Status != 502 || he.Code != httpx.CodePushSendFailed || !strings.Contains(he.Error(), "push_unauthorized") {
		t.Fatalf("failing relay: %v", err)
	}
	for _, s := range []string{relaySecret, relayURLToken, "TOKEN-c-", "relay.example.com"} {
		if strings.Contains(he.Error(), s) {
			t.Errorf("the error holds %q", s)
		}
	}
	if e := w.relayError(); e != "push_unauthorized" {
		t.Errorf("relay error %q", e)
	}
}

// The relay URL, the relay secret and the device tokens are nowhere but in the
// sealed columns: not in a log, an event, an outbox row, an idempotency row, an
// error word, a view, or the row in clear.
func TestPush_SecretsNeverStoredOrLogged(t *testing.T) {
	w, _ := readyPushWorld(t)
	ctx := context.Background()
	secrets := []string{relaySecret, relayURLToken, tokAlice1, "relay.example.com/t"}
	for _, status := range []int{500, 401, 200} {
		w.relay.set(status, "")
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
		w.consume()
		w.pass()
	}
	w.device(w.org.ID, "chen", "TOKEN-c-secret")
	secrets = append(secrets, "TOKEN-c-secret")
	w.relay.set(502, "")
	if _, err := w.svc2.Test(ctx, w.org.ID, w.user["chen"]); err == nil {
		t.Fatal("test push should fail")
	} else {
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("the test error holds %q", s)
			}
		}
	}
	w.relay.Close()
	if _, err := w.svc2.Test(ctx, w.org.ID, w.user["chen"]); err == nil {
		t.Fatal("test push to a closed relay should fail")
	} else {
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("the error holds %q", s)
			}
		}
	}
	texts := map[string]string{"sender log": w.plogs.String(), "consumer log": w.logs.String(), "mail log": w.slog.String()}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(push_outbox::text, ' '), '') FROM push_outbox`,
		`SELECT coalesce(string_agg(coalesce(last_error, '') || coalesce(host, ''), ' '), '') FROM push_relays`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM idempotency_keys t`,
		`SELECT coalesce(string_agg(data::text, ' '), '') FROM notifications`,
		`SELECT coalesce(string_agg(d.id::text || d.platform || d.app_version || d.label, ' '), '') FROM push_devices d`,
	} {
		var text string
		if err := w.pool.QueryRow(ctx, q).Scan(&text); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		texts[q] = text
	}
	for name, text := range texts {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s holds %q", name, s)
			}
		}
	}
	var urlBlob, secBlob, tokBlob []byte
	if err := w.pool.QueryRow(ctx, `SELECT url_enc, secret_enc FROM push_relays`).Scan(&urlBlob, &secBlob); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT token_enc FROM push_devices ORDER BY created_at LIMIT 1`).Scan(&tokBlob); err != nil {
		t.Fatal(err)
	}
	for _, blob := range [][]byte{urlBlob, secBlob, tokBlob} {
		for _, s := range secrets {
			if bytes.Contains(blob, []byte(s)) {
				t.Errorf("a sealed column holds %q in clear", s)
			}
		}
	}
	var hash []byte
	if err := w.pool.QueryRow(ctx, `SELECT token_hash FROM push_devices ORDER BY created_at LIMIT 1`).Scan(&hash); err != nil || len(hash) != 32 || bytes.Contains(hash, []byte(tokAlice1)) {
		t.Errorf("token hash %d bytes %v", len(hash), err)
	}
	v, _ := w.svc2.Get(ctx, w.org.ID)
	l, _ := w.svc2.ListDevices(ctx, w.org.ID, w.user["alice"].UserID)
	for _, x := range []any{v, l} {
		b, _ := json.Marshal(x)
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("a view holds %q: %s", s, b)
			}
		}
	}
}

// The notification settings carry the push channel; the page knows from the
// view whether to show it.
func TestPush_NotificationSettingsView(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	svc := notify.New(w.pool)
	alice := w.user["alice"].UserID
	v, err := svc.View(ctx, w.org, alice)
	if err != nil || v.Push.Available {
		t.Fatalf("view %+v %v", v, err)
	}
	for _, e := range v.Events {
		if e.Default.Push != "off" || e.Resolved.Push.Value != "off" || e.Global.Push != nil {
			t.Errorf("%s: push default %+v", e.Event, e)
		}
	}
	w.putRelay(w.org.ID)
	on := httpx.Opt[string]{Set: true, V: "on"}
	v, err = svc.UpdateGlobal(ctx, w.org, alice, notify.SettingsInput{Events: map[string]notify.EventInput{"comment": {Push: on}}})
	if err != nil || !v.Push.Available {
		t.Fatalf("update %+v %v", v, err)
	}
	for _, e := range v.Events {
		want := "off"
		if e.Event == "comment" {
			want = "on"
		}
		if e.Resolved.Push.Value != want {
			t.Errorf("%s: push = %s, want %s", e.Event, e.Resolved.Push.Value, want)
		}
	}
	// A team level, then clearing both levels removes the row.
	tid := w.teamID()
	if _, err := svc.UpdateTeam(ctx, w.org, alice, tid, notify.TeamSettingsInput{Events: map[string]notify.EventInput{"comment": {Push: httpx.Opt[string]{Set: true, V: "off"}}}}); err != nil {
		t.Fatal(err)
	}
	null := httpx.Opt[string]{Set: true, Null: true}
	if _, err := svc.UpdateTeam(ctx, w.org, alice, tid, notify.TeamSettingsInput{Events: map[string]notify.EventInput{"comment": {Push: null}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateGlobal(ctx, w.org, alice, notify.SettingsInput{Events: map[string]notify.EventInput{"comment": {Push: null}}}); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM notification_settings WHERE user_id = $1`, alice); n != 0 {
		t.Errorf("rows left after clearing push: %d", n)
	}
	// A bad value is a 400.
	_, err = svc.UpdateGlobal(ctx, w.org, alice, notify.SettingsInput{Events: map[string]notify.EventInput{"comment": {Push: httpx.Opt[string]{Set: true, V: "maybe"}}}})
	if he, ok := err.(*httpx.Error); !ok || he.Status != 400 {
		t.Errorf("bad value: %v", err)
	}
	// Removing the relay turns Available off again, and the stored choice stays.
	if _, err := svc.UpdateGlobal(ctx, w.org, alice, notify.SettingsInput{Events: map[string]notify.EventInput{"comment": {Push: on}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.svc2.Delete(ctx, w.org.ID, w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.View(ctx, w.org, alice); v.Push.Available {
		t.Error("available after the relay was removed")
	}
}

// The project update reminder (LAUN-196) reaches the lead's devices too, as a
// bare wake with no project name.
func TestPush_ProjectUpdateDue(t *testing.T) {
	w := newPushWorld(t)
	w.putRelay(w.org.ID)
	w.device(w.org.ID, "alice", tokAlice1)
	w.pushOn("alice", "project_update_due")
	w.project("TOPSECRETPROJECT", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	if r := w.job(utc(2026, 10, 30, 9, 0)); r.Reminded != 1 {
		t.Fatalf("job %+v", r)
	}
	if res := w.consume(); res.Pushes != 1 {
		t.Fatalf("consume %+v", res)
	}
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	if h := w.relay.got(); len(h) != 1 || strings.Contains(h[0].Body, "TOPSECRETPROJECT") {
		t.Fatalf("hits %+v", h)
	}
}

// Two members register the same phone at the same moment: no 500, one row remains.
func TestPush_SameTokenRaceIsSerialised(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		tok := fmt.Sprintf("RACE-TOKEN-%02d", i)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, u := range []string{"alice", "bob"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := w.svc2.PutDevice(ctx, w.org.ID, w.user[u].UserID, uuid.New(), notify.DeviceInput{Platform: "ios", Token: tok})
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		if n := w.count(`SELECT count(*) FROM push_devices WHERE token_hash = sha256($1::bytea)`, tok); n != 1 {
			t.Fatalf("round %d: rows for one token = %d", i, n)
		}
		w.exec(`DELETE FROM push_devices`)
	}
}

// hasDevice tells the page whether the caller has a device in this org: the Push
// radios need the relay AND a device.
func TestPush_SettingsViewHasDevice(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	svc := notify.New(w.pool)
	alice := w.user["alice"].UserID
	w.putRelay(w.org.ID)
	if v, _ := svc.View(ctx, w.org, alice); !v.Push.Available || v.Push.HasDevice {
		t.Fatalf("relay without a device: %+v", v.Push)
	}
	w.device(w.org.ID, "alice", tokAlice1)
	if v, _ := svc.View(ctx, w.org, alice); !v.Push.HasDevice {
		t.Fatalf("with a device: %+v", v.Push)
	}
	if v, _ := svc.View(ctx, w.org, w.user["bob"].UserID); v.Push.HasDevice {
		t.Fatal("bob has alice's device")
	}
}

// The token lock is real: while another transaction holds the (org, token) lock,
// a registration of that token waits, then completes.
func TestPush_TokenRegistrationWaitsForTheTokenLock(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	const tok = "LOCKED-TOKEN-1"
	sum := sha256.Sum256([]byte(tok))
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:push-token:' || $1::text || ':' || $2::text, 0))`, w.org.ID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.svc2.PutDevice(ctx, w.org.ID, w.user["alice"].UserID, uuid.New(), notify.DeviceInput{Platform: "ios", Token: tok})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("PutDevice finished while the token lock was held (%v)", err)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PutDevice did not finish after the lock was released")
	}
}

// A member at the cap who registers their OWN token again under a new id takes the old
// row over: the count stays 10, no 409. A new token at the cap is still 409.
func TestPush_CapAllowsTakeoverOfOwnToken(t *testing.T) {
	w := newPushWorld(t)
	ctx := context.Background()
	alice := w.user["alice"].UserID
	for i := 0; i < notify.PushMaxDevices; i++ {
		w.device(w.org.ID, "alice", fmt.Sprintf("CAP-TOKEN-%02d", i))
	}
	if _, err := w.svc2.PutDevice(ctx, w.org.ID, alice, uuid.New(), notify.DeviceInput{Platform: "ios", Token: "CAP-TOKEN-03"}); err != nil {
		t.Fatalf("own token under a new id at the cap: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM push_devices WHERE user_id = $1`, alice); n != notify.PushMaxDevices {
		t.Fatalf("devices = %d, want %d", n, notify.PushMaxDevices)
	}
	_, err := w.svc2.PutDevice(ctx, w.org.ID, alice, uuid.New(), notify.DeviceInput{Platform: "ios", Token: "CAP-TOKEN-NEW"})
	if he, ok := err.(*httpx.Error); !ok || he.Status != 409 {
		t.Fatalf("a new token at the cap = %v, want 409", err)
	}
}
