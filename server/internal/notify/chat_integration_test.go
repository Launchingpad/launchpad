package notify_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/notify"
	"devops/internal/platform"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// The Slack channel (LAUN-198, decisions-T33), with the real database, the
// real consumer and sender, and a fake Slack: a TLS httptest server reached
// through the REAL netguard dialer (the test policy permits only 127.0.0.1).

const (
	secretToken = "SECRETWEBHOOKTOKEN987"
	hookURL     = "https://hooks.slack.com/services/T0001/B0001/" + secretToken
)

var slackKey = []byte("0123456789abcdef0123456789abcdef")

type chatHit struct {
	Path string
	Body string
}

// fakeChat is the server at the other end. status 0 means 200.
type fakeChat struct {
	*httptest.Server
	mu         sync.Mutex
	hits       []chatHit
	status     int
	retryAfter string
	redirect   string
	hangPath   string        // a request whose path holds this closes the connection (an unreachable host)
	delay      time.Duration // every other answer waits this long
	onHit      func()        // runs while the request is in flight, before the answer
}

func newFakeChat(t *testing.T) *fakeChat {
	t.Helper()
	f := &fakeChat{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, chatHit{Path: r.URL.Path, Body: string(b)})
		status, ra, red, hang, delay := f.status, f.retryAfter, f.redirect, f.hangPath, f.delay
		f.mu.Unlock()
		if hang != "" && strings.Contains(r.URL.Path, hang) {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
			return
		}
		f.mu.Lock()
		onHit := f.onHit
		f.mu.Unlock()
		if onHit != nil {
			onHit()
		}
		time.Sleep(delay)
		if red != "" {
			http.Redirect(w, r, red, http.StatusFound)
			return
		}
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeChat) set(status int, retryAfter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.retryAfter = status, retryAfter
}

func (f *fakeChat) got() []chatHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chatHit(nil), f.hits...)
}

// client is the production client (netguard.NewClient) on a test dialer that
// permits 127.0.0.1, with the connection for hooks.slack.com:443 sent to the
// fake server. The dial still goes through netguard.Dialer.
func (f *fakeChat) client(t *testing.T) *http.Client {
	t.Helper()
	d := &netguard.Dialer{Policy: netguardtest.Loopback()}
	c := notify.NewChatClientFor(d)
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

// swo is a world with the Slack channel wired.
type swo struct {
	*sworld
	slack  *fakeChat
	keys   *platform.SlackKeyring
	svc2   *notify.ChatAdmin
	sender *notify.ChatSender
	slogs  *lockedBuf
	hooks  map[uuid.UUID]string // the URL putService last set per service
}

func newChatWorld(t *testing.T) *swo {
	t.Helper()
	sw := newSWorld(t)
	f := newFakeChat(t)
	keys, err := platform.NewSlackKeyring(slackKey)
	if err != nil {
		t.Fatal(err)
	}
	w := &swo{sworld: sw, slack: f, keys: keys, slogs: &lockedBuf{}, hooks: map[uuid.UUID]string{}}
	log := slog.New(slog.NewTextHandler(w.slogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	poster := notify.NewChatPoster(f.client(t))
	w.svc2 = notify.NewChatAdmin(sw.pool, log, keys, poster, testBase)
	w.sender = notify.NewChatSender(sw.pool, log, keys, poster, testBase)
	w.sender.SetClock(sw.clk.now)
	return w
}

// putService makes the service called name in the org, or changes it when it
// exists (the URL and the event list).
func (w *swo) putService(org uuid.UUID, kind, name, hook string, events ...string) notify.ChatView {
	w.t.Helper()
	if events == nil {
		events = []string{}
	}
	ctx := context.Background()
	list, err := w.svc2.List(ctx, org)
	if err != nil {
		w.t.Fatalf("List: %v", err)
	}
	for _, it := range list.Items {
		if it.Name == name {
			// Send the URL only when it changes: a new URL drops the waiting posts (LAUN-231).
			in := notify.ChatPatchInput{Events: events}
			if w.hooks[it.ID] != hook {
				in.WebhookURL = &hook
			}
			v, err := w.svc2.Patch(ctx, org, it.ID, w.user["chen"], in)
			if err != nil {
				w.t.Fatalf("Patch: %v", err)
			}
			w.hooks[it.ID] = hook
			return v
		}
	}
	v, err := w.svc2.Create(ctx, org, w.user["chen"], notify.ChatCreateInput{Name: name, TeamID: w.teamOf(org), Kind: kind, WebhookURL: hook, Events: events})
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	w.hooks[v.ID] = hook
	return v
}

// teamOf is the org's first team (the world has one: ORB).
func (w *swo) teamOf(org uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 ORDER BY created_at, id LIMIT 1`, org).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// putChannel is the one Slack service of an org, named "Slack".
func (w *swo) putChannel(org uuid.UUID, hook string, events ...string) {
	w.t.Helper()
	w.putService(org, notify.ChatSlack, "Slack", hook, events...)
}

// svcID is the id of the first service of an org.
func (w *swo) svcID(org uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM chat_services WHERE org_id = $1 ORDER BY created_at, id LIMIT 1`, org).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// consume runs the consumer and ages the new outbox rows to the fake clock.
func (w *swo) consume() {
	w.t.Helper()
	w.world.run()
	w.exec(`UPDATE chat_outbox SET created_at = $1, next_attempt_at = $1 WHERE created_at > $1`, w.clk.now())
}

func (w *swo) pass() notify.ChatResult {
	w.t.Helper()
	res, err := w.sender.RunOnce(context.Background())
	if err != nil {
		w.t.Fatalf("slack RunOnce: %v", err)
	}
	return res
}

type chatRow struct {
	Kind, State, Skip, LastErr string
	Attempts                   int32
	Next                       time.Time
}

func (w *swo) outRows() []chatRow {
	w.t.Helper()
	rs, err := w.pool.Query(context.Background(),
		`SELECT kind, state, coalesce(skip_reason, ''), coalesce(last_error, ''), attempts, next_attempt_at FROM chat_outbox ORDER BY created_at, id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rs.Close()
	var out []chatRow
	for rs.Next() {
		var r chatRow
		if err := rs.Scan(&r.Kind, &r.State, &r.Skip, &r.LastErr, &r.Attempts, &r.Next); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (w *swo) onlyRow() chatRow {
	w.t.Helper()
	r := w.outRows()
	if len(r) != 1 {
		w.t.Fatalf("chat_outbox = %+v, want one row", r)
	}
	return r[0]
}

func (w *swo) channelError() string {
	w.t.Helper()
	var s *string
	if err := w.pool.QueryRow(context.Background(), `SELECT last_error FROM chat_services WHERE org_id = $1 ORDER BY created_at, id LIMIT 1`, w.org.ID).Scan(&s); err != nil {
		w.t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func TestChat_PostsOneFixedSentenceAndLink(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice", Title: "TOPSECRETTITLE", Description: "TOPSECRETBODY"})
	w.consume()
	if r := w.onlyRow(); r.Kind != "assigned" || r.State != "pending" {
		t.Fatalf("row %+v", r)
	}
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	hits := w.slack.got()
	if len(hits) != 1 || hits[0].Path != "/services/T0001/B0001/"+secretToken {
		t.Fatalf("hits %+v", hits)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(hits[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	want := "[" + is.Identifier + "] Issue assigned: <" + testBase + "/orbit/issue/" + is.Identifier + "|open in Launchpad>"
	if body["text"] != want {
		t.Errorf("text = %q, want %q", body["text"], want)
	}
	// No title, no description, no person name in the post.
	for _, leak := range []string{"TOPSECRETTITLE", "TOPSECRETBODY", "alice", "chen"} {
		if strings.Contains(hits[0].Body, leak) {
			t.Errorf("the post holds %q: %s", leak, hits[0].Body)
		}
	}
	if r := w.onlyRow(); r.State != "sent" {
		t.Errorf("row %+v", r)
	}
	if res := w.pass(); res.Sent != 0 || len(w.slack.got()) != 1 {
		t.Errorf("a second pass posted again: %+v", res)
	}
	if e := w.channelError(); e != "" {
		t.Errorf("channel error %q", e)
	}
}

func TestChat_OnlySelectedKinds(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "comment")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := len(w.outRows()); n != 0 {
		t.Fatalf("an assignment made %d rows for a comment-only channel", n)
	}
	w.comment("bob", is.Identifier, "hello")
	w.consume()
	if r := w.onlyRow(); r.Kind != "comment" {
		t.Fatalf("row %+v", r)
	}
	// An empty list posts nothing at all.
	w.putChannel(w.org.ID, hookURL)
	w.comment("bob", is.Identifier, "hello again")
	w.consume()
	if n := len(w.outRows()); n != 1 {
		t.Fatalf("an empty list made rows: %d", n)
	}
}

// A comment on an issue with several watchers is ONE post, not one per person.
func TestChat_OnePostPerEventNotPerRecipient(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "comment", "state_change", "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.comment("bob", is.Identifier, "one")
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.comment("carol", is.Identifier, "two") // chen, alice and bob all watch
	w.consume()
	if r := w.onlyRow(); r.Kind != "comment" {
		t.Fatalf("row %+v", r)
	}
	// A replay of the same events makes no second row.
	w.exec(`UPDATE notification_cursors SET last_seq = 0`)
	w.exec(`DELETE FROM notifications`)
	w.consume()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE kind = 'comment'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// The earlier comment ("one") is replayed too, so two comment rows exist, never three.
	if n != 2 {
		t.Errorf("comment rows after replay = %d, want 2", n)
	}
}

// Agents get no notification (decisions-T13 #14), but a chat service does not
// need a recipient (decisions-T37, LAUN-208): an assignment to an agent and an
// agent's own comment both post. (Before LAUN-208 the first posted nothing.)
func TestChat_AgentsGetNoNotificationButTheChannelPosts(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned", "new_issue", "comment", "state_change", "mention")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "claude"})
	w.consume()
	rows := w.outRows()
	if len(rows) != 2 || rows[0].Kind == rows[1].Kind {
		t.Fatalf("an assignment to an agent made %+v, want assigned and new_issue", rows)
	}
	if got := w.got(); len(got) != 0 {
		t.Fatalf("the agent has notifications: %v", got)
	}
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.comment("claude", is.Identifier, "agent comment")
	w.consume()
	if r := w.onlyRow(); r.Kind != "comment" {
		t.Fatalf("row %+v", r)
	}
}

// The channel is an org setting: a person's own switches do not change it.
func TestChat_PersonalSettingsDoNotApply(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.exec(`INSERT INTO notification_user_settings (org_id, user_id, email_enabled) VALUES ($1, $2, false)`, w.org.ID, w.user["alice"].UserID)
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'assigned', 'off', 'off')`, w.org.ID, w.user["alice"].UserID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if r := w.onlyRow(); r.Kind != "assigned" {
		t.Fatalf("row %+v", r)
	}
	if rows := w.rows(w.org.ID); len(rows) != 0 {
		t.Fatalf("alice has notifications: %v", rows)
	}
}

func TestChat_NoChannelNoRows(t *testing.T) {
	w := newChatWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := len(w.outRows()); n != 0 {
		t.Fatalf("rows without a channel: %d", n)
	}
}

func TestChat_ProjectUpdateDue(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "project_update_due")
	p := w.project("Zeta", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	pid := p.ID
	// The project is linked to the service's team (decisions-T38 #8).
	w.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, pid, w.teamOf(w.org.ID))
	w.job(utc(2026, 10, 30, 9, 0))
	w.consume()
	if r := w.onlyRow(); r.Kind != "project_update_due" {
		t.Fatalf("row %+v", r)
	}
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	hits := w.slack.got()
	if len(hits) != 1 || !strings.Contains(hits[0].Body, "Project update due: <"+testBase+"/orbit/projects/"+pid.String()+"|open in Launchpad>") {
		t.Fatalf("hits %+v", hits)
	}
	if strings.Contains(hits[0].Body, "Zeta") {
		t.Error("the post holds the project name")
	}
}

func TestChat_Failures(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		retry     string
		wantState string
		wantErr   string
	}{
		{"server error retries", 500, "", "pending", "chat_unavailable"},
		{"rate limit retries", 429, "", "pending", "chat_rate_limited"},
		{"gone is final", 404, "", "dead", "chat_gone"},
		{"archived is final", 410, "", "dead", "chat_gone"},
		{"refused is final", 403, "", "dead", "chat_rejected"},
		{"bad payload is final", 400, "", "dead", "chat_rejected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newChatWorld(t)
			w.putChannel(w.org.ID, hookURL, "assigned")
			w.slack.set(c.status, c.retry)
			w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
			w.consume()
			w.pass()
			r := w.onlyRow()
			if r.State != c.wantState || r.LastErr != c.wantErr {
				t.Fatalf("row %+v, want %s/%s", r, c.wantState, c.wantErr)
			}
			if e := w.channelError(); e != c.wantErr {
				t.Errorf("channel error = %q, want %q", e, c.wantErr)
			}
			if c.wantState == "pending" {
				if r.Attempts != 1 || !r.Next.After(w.clk.now()) {
					t.Errorf("retry not scheduled: %+v", r)
				}
				// Not due yet: a pass at once posts nothing.
				before := len(w.slack.got())
				w.pass()
				if len(w.slack.got()) != before {
					t.Error("posted before the retry time")
				}
			}
		})
	}
}

// A 500 retries on the sender's schedule and the sixth failure is dead; a
// success clears the channel error.
func TestChat_RetryScheduleThenDead(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.slack.set(500, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	for i, d := range notify.RetryDelays {
		w.pass()
		r := w.onlyRow()
		if r.State != "pending" || int(r.Attempts) != i+1 {
			t.Fatalf("after failure %d: %+v", i+1, r)
		}
		if got := r.Next.Sub(w.clk.now()); got != d {
			t.Fatalf("delay after failure %d = %v, want %v", i+1, got, d)
		}
		w.clk.advance(d + time.Second)
	}
	w.pass()
	if r := w.onlyRow(); r.State != "dead" || r.Attempts != notify.MaxAttempts {
		t.Fatalf("final %+v", r)
	}
	// A new row after the webhook recovers is sent and clears the error.
	w.slack.set(200, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	w.pass()
	if e := w.channelError(); e != "" {
		t.Errorf("channel error after success = %q", e)
	}
}

func TestChat_RetryAfterIsHonoured(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.slack.set(429, "600")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	r := w.onlyRow()
	if got := r.Next.Sub(w.clk.now()); got != 10*time.Minute {
		t.Errorf("next attempt in %v, want 10m", got)
	}
}

// A redirect is never followed, so the second server is never called.
func TestChat_RedirectNotFollowed(t *testing.T) {
	w := newChatWorld(t)
	other := newFakeChat(t)
	w.slack.mu.Lock()
	w.slack.redirect = other.URL + "/steal"
	w.slack.mu.Unlock()
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	if r := w.onlyRow(); r.State != "dead" || r.LastErr != "chat_redirect" {
		t.Fatalf("row %+v", r)
	}
	if n := len(other.got()); n != 0 {
		t.Errorf("the redirect target was called %d times", n)
	}
}

// The production policy refuses every address it must (decisions-T33 #5),
// whatever the host name resolves to: the dial never happens.
func TestChat_DialGuardRefusesPrivateAndMetadata(t *testing.T) {
	ip := func(a string) []string { return []string{a} }
	for name, addr := range map[string]string{
		"loopback":        "127.0.0.1",
		"private 10":      "10.0.0.5",
		"private 192":     "192.168.1.1",
		"private 172":     "172.16.0.9",
		"cgnat":           "100.64.0.1",
		"link local":      "169.254.1.1",
		"metadata":        "169.254.169.254",
		"unspecified":     "0.0.0.0",
		"ipv6 loopback":   "::1",
		"ipv6 unique":     "fd00::1",
		"ipv4-mapped":     "::ffff:10.0.0.1",
		"nat64 to 10.x":   "64:ff9b::a00:1",
		"6to4 of 10.x":    "2002:a00:1::1",
		"multicast":       "224.0.0.1",
		"docs range":      "192.0.2.1",
		"mixed answer ok": "",
	} {
		t.Run(name, func(t *testing.T) {
			if addr == "" {
				return
			}
			res := &netguardtest.MapResolver{Hosts: map[string][]string{"hooks.slack.com": ip(addr)}}
			d := &netguard.Dialer{Policy: notify.ChatPolicy(nil), Resolver: res, Timeout: 2 * time.Second}
			poster := notify.NewChatPoster(notify.NewChatClientFor(d))
			err := poster.Post(context.Background(), notify.ChatSlack, hookURL, notify.ChatMessage{Event: notify.EventTest})
			var se *notify.ChatError
			if !asChatErr(err, &se) || se.Word != notify.ChatBlocked {
				t.Fatalf("Post = %v, want slack_blocked", err)
			}
			if strings.Contains(err.Error(), secretToken) {
				t.Error("the error holds the URL")
			}
		})
	}
	// One refused answer among good ones refuses the host.
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"hooks.slack.com": {"8.8.8.8", "10.0.0.5"}}}
	d := &netguard.Dialer{Policy: notify.ChatPolicy(nil), Resolver: res, Timeout: 2 * time.Second}
	err := notify.NewChatPoster(notify.NewChatClientFor(d)).Post(context.Background(), notify.ChatSlack, hookURL, notify.ChatMessage{Event: notify.EventTest})
	var se *notify.ChatError
	if !asChatErr(err, &se) || se.Word != notify.ChatBlocked {
		t.Fatalf("mixed answer: %v", err)
	}
}

func asChatErr(err error, target **notify.ChatError) bool {
	se, ok := err.(*notify.ChatError)
	if ok {
		*target = se
	}
	return ok
}

// The poster re-checks the host: a row or a call with another host never dials.
func TestChat_PosterRefusesOtherHosts(t *testing.T) {
	f := newFakeChat(t)
	poster := notify.NewChatPoster(f.client(t))
	for _, u := range []string{
		"https://evil.example.com/services/T0001/B0001/" + secretToken,
		"http://hooks.slack.com/services/T0001/B0001/" + secretToken,
		"https://hooks.slack.com:8443/services/T0001/B0001/" + secretToken,
		"https://hooks.slack.com/other/T0001/B0001/" + secretToken,
	} {
		err := poster.Post(context.Background(), notify.ChatSlack, u, notify.ChatMessage{Event: notify.EventTest})
		var se *notify.ChatError
		if !asChatErr(err, &se) || se.Word != notify.ChatBlocked {
			t.Errorf("%q: %v", u, err)
		}
	}
	if n := len(f.got()); n != 0 {
		t.Errorf("fake Slack was called %d times", n)
	}
	var nilPoster *notify.ChatPoster
	if err := nilPoster.Post(context.Background(), notify.ChatSlack, hookURL, notify.ChatMessage{Event: notify.EventTest}); err == nil {
		t.Error("a nil poster posted")
	}
}

func TestChat_RateCap(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	// SlackRateCap posts already went in the last hour.
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state, sent_at)
		SELECT $1, $5, 'assigned', $2, 'old:' || g, 'sent', $3 FROM generate_series(1, $4) g`,
		w.org.ID, is.ID, w.clk.now().Add(-time.Minute), notify.ChatRateCap, w.svcID(w.org.ID))
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	w.pass()
	var skipped int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE skip_reason = 'rate_cap'`).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("rate_cap rows = %d (%v)", skipped, err)
	}
	if n := len(w.slack.got()); n != 0 {
		t.Errorf("posted over the cap: %d", n)
	}
	// An hour later the window has moved on.
	w.clk.advance(notify.ChatRateWindow + time.Minute)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "carol"})
	w.consume()
	if res := w.pass(); res.Sent != 1 {
		t.Errorf("after the window: %+v", res)
	}
}

// A burst is spread over passes, not sent at once.
func TestChat_PerPassLimit(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	for i := 0; i < notify.ChatPerOrgPerPass+3; i++ {
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	}
	w.consume()
	if res := w.pass(); res.Sent != notify.ChatPerOrgPerPass {
		t.Fatalf("first pass %+v", res)
	}
	if res := w.pass(); res.Sent != 3 {
		t.Fatalf("second pass %+v", res)
	}
}

func TestChat_SkipsWhenChannelChanges(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned", "comment")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.comment("bob", is.Identifier, "x")
	w.consume()
	if n := len(w.outRows()); n != 2 {
		t.Fatalf("rows %d", n)
	}
	// The admin takes "comment" off the list before the sender runs.
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.pass()
	rows := map[string]string{}
	for _, r := range w.outRows() {
		rows[r.Kind] = r.State + "/" + r.Skip
	}
	if rows["assigned"] != "sent/" || rows["comment"] != "skipped/not_selected" {
		t.Errorf("rows %v", rows)
	}
	// The admin removes the channel: what waits is deleted, nothing posts.
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	if err := w.svc2.Delete(context.Background(), w.org.ID, w.svcID(w.org.ID), w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	before := len(w.slack.got())
	w.pass()
	if len(w.slack.got()) != before {
		t.Error("posted after the channel was removed")
	}
	var waiting int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE state IN ('pending','sending')`).Scan(&waiting); err != nil || waiting != 0 {
		t.Errorf("waiting rows after the delete = %d (%v)", waiting, err)
	}
}

func TestChat_NoBaseURLSkips(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	poster := notify.NewChatPoster(w.slack.client(t))
	w.sender = notify.NewChatSender(w.pool, slog.New(slog.NewTextHandler(w.slogs, nil)), w.keys, poster, "")
	w.sender.SetClock(w.clk.now)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "no_base_url" {
		t.Fatalf("row %+v", r)
	}
}

func TestChat_ExpiredAfter24Hours(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.slack.set(500, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.pass()
	w.clk.advance(notify.ChatRowTTL + time.Minute)
	w.slack.set(200, "")
	w.pass()
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "expired" {
		t.Fatalf("row %+v", r)
	}
}

// A key change makes the URL unreadable: nothing is posted, the channel says so.
func TestChat_UnreadableSecret(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	other, _ := platform.NewSlackKeyring([]byte("ffffffffffffffffffffffffffffffff"))
	poster := notify.NewChatPoster(w.slack.client(t))
	w.sender = notify.NewChatSender(w.pool, slog.New(slog.NewTextHandler(w.slogs, nil)), other, poster, testBase)
	w.sender.SetClock(w.clk.now)
	w.pass()
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "unreadable" {
		t.Fatalf("row %+v", r)
	}
	if e := w.channelError(); e != "chat_unreadable" {
		t.Errorf("channel error %q", e)
	}
	if n := len(w.slack.got()); n != 0 {
		t.Errorf("posted with an unreadable secret: %d", n)
	}
	svc := notify.NewChatAdmin(w.pool, slog.New(slog.NewTextHandler(w.slogs, nil)), other, poster, testBase)
	l, err := svc.List(context.Background(), w.org.ID)
	if err != nil || len(l.Items) != 1 || !l.Items[0].SecretUnreadable {
		t.Errorf("list %+v %v", l, err)
	}
}

// Two orgs: an event posts to its own org's webhook only (decisions-T33 #11).
func TestChat_CrossOrg(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "alice")
	w.addMember(org2.ID, "chen")
	hook2 := "https://hooks.slack.com/services/T0002/B0002/OTHERORGTOKEN"
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.putChannel(org2.ID, hook2, "assigned")
	org := &auth.Org{ID: org2.ID, Slug: "other", Name: "Other", Role: "admin", Access: auth.TeamAccess{All: true}}
	if _, err := w.svc.CreateIssue(ctx, org, w.user["chen"], tracker.CreateIssueInput{Team: "OTH", Title: "t", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	w.consume()
	w.pass()
	hits := w.slack.got()
	if len(hits) != 1 || hits[0].Path != "/services/T0002/B0002/OTHERORGTOKEN" || !strings.Contains(hits[0].Body, "/other/issue/OTH-1") {
		t.Fatalf("hits %+v", hits)
	}
	// Org 1's list knows nothing of org 2's service, and a row of org 2 can
	// never be moved to org 1's webhook (the blob is bound to the org).
	var blob []byte
	if err := w.pool.QueryRow(ctx, `SELECT webhook_enc FROM chat_services WHERE org_id = $1`, org2.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if _, err := w.keys.OpenFor(platform.SlackColWebhook, w.org.ID.String(), blob); err == nil {
		t.Error("org 2's blob opens as org 1's")
	}
	// Deleting org 1's channel leaves org 2's.
	if err := w.svc2.Delete(ctx, w.org.ID, w.svcID(w.org.ID), w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	if l, _ := w.svc2.List(ctx, org2.ID); len(l.Items) != 1 {
		t.Error("org 2's service was removed with org 1's")
	}
}

// The secret is nowhere but the sealed blob (decisions-T33 #3): not in the
// log, an event, an outbox row, an error word, or the row in clear.
func TestChat_SecretNeverStoredOrLogged(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	w.putChannel(w.org.ID, hookURL, "assigned")
	for _, status := range []int{500, 404, 200} {
		w.slack.set(status, "")
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
		w.consume()
		w.pass()
	}
	// A failing test message, and one from a dead connection.
	sid := w.svcID(w.org.ID)
	w.slack.set(502, "")
	if _, err := w.svc2.Test(ctx, w.org.ID, sid); err == nil {
		t.Fatal("test message should fail")
	} else if strings.Contains(err.Error(), secretToken) {
		t.Error("the test error holds the URL")
	}
	w.slack.Close()
	if _, err := w.svc2.Test(ctx, w.org.ID, sid); err == nil {
		t.Fatal("test message to a closed server should fail")
	} else if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "hooks.slack.com") {
		t.Errorf("the error leaks: %v", err)
	}
	for name, text := range map[string]string{
		"sender log":   w.slogs.String(),
		"consumer log": w.logs.String(),
		"mail log":     w.slog.String(),
	} {
		if strings.Contains(text, secretToken) || strings.Contains(text, "hooks.slack.com/services") {
			t.Errorf("%s holds the webhook: %s", name, text)
		}
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(chat_outbox::text, ' '), '') FROM chat_outbox`,
		`SELECT coalesce(string_agg(last_error, ' '), '') FROM chat_services`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM idempotency_keys t`,
	} {
		var text string
		if err := w.pool.QueryRow(ctx, q).Scan(&text); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if strings.Contains(text, secretToken) || strings.Contains(text, "hooks.slack.com") {
			t.Errorf("stored in clear (%s)", q)
		}
	}
	var blob []byte
	if err := w.pool.QueryRow(ctx, `SELECT webhook_enc FROM chat_services`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(secretToken)) || bytes.Contains(blob, []byte("hooks.slack.com")) {
		t.Error("the sealed column holds the URL in clear")
	}
	l, _ := w.svc2.List(ctx, w.org.ID)
	b, _ := json.Marshal(l)
	if strings.Contains(string(b), secretToken) || strings.Contains(string(b), "services") {
		t.Errorf("list holds the URL: %s", b)
	}
	if testdb.OrgSlug == "" {
		t.Fatal("unreachable")
	}
}

func TestChat_CreatePatchRules(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	chen := w.user["chen"]
	tm := w.teamOf(w.org.ID)
	create := func(in notify.ChatCreateInput) (notify.ChatView, error) {
		return w.svc2.Create(ctx, w.org.ID, chen, in)
	}
	good := notify.ChatCreateInput{Name: "Eng", TeamID: tm, Kind: "slack", WebhookURL: hookURL, Events: []string{}}
	for name, in := range map[string]notify.ChatCreateInput{
		"empty name":       {Name: " ", TeamID: tm, Kind: "slack", WebhookURL: hookURL},
		"long name":        {Name: strings.Repeat("n", 41), TeamID: tm, Kind: "slack", WebhookURL: hookURL},
		"control name":     {Name: "a\nb", TeamID: tm, Kind: "slack", WebhookURL: hookURL},
		"bad kind":         {Name: "x", TeamID: tm, Kind: "teams", WebhookURL: hookURL},
		"no kind":          {Name: "x", TeamID: tm, WebhookURL: hookURL},
		"no url":           {Name: "x", TeamID: tm, Kind: "slack"},
		"bad host":         {Name: "x", TeamID: tm, Kind: "slack", WebhookURL: "https://evil.example.com/services/T/B/x"},
		"slack as discord": {Name: "x", TeamID: tm, Kind: "discord", WebhookURL: hookURL},
		"private custom":   {Name: "x", TeamID: tm, Kind: "custom", WebhookURL: "https://10.0.0.5/hook"},
		"bad event":        {Name: "x", TeamID: tm, Kind: "slack", WebhookURL: hookURL, Events: []string{"bogus"}},
	} {
		if _, err := create(in); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), secretToken) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
	v, err := create(good)
	if err != nil || !v.Enabled || v.Kind != "slack" || v.Host != "hooks.slack.com" || v.SecretUnreadable {
		t.Fatalf("create: %+v %v", v, err)
	}
	// The name is unique per org, ignoring case.
	dup := good
	dup.Name = "eng"
	if _, err := create(dup); err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("duplicate name: %v", err)
	}
	// Events are stored in the canonical order, without duplicates; the URL stays.
	ev := []string{"comment", "assigned", "comment"}
	p, err := w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{Events: ev})
	if err != nil || strings.Join(p.Events, ",") != "assigned,comment" || p.SecretUnreadable {
		t.Fatalf("patch events: %+v %v", p, err)
	}
	// A patch with an empty list clears it; an absent list keeps it.
	if p, _ = w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{Events: []string{}}); len(p.Events) != 0 {
		t.Errorf("empty list kept %v", p.Events)
	}
	p, _ = w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{Events: []string{"comment"}})
	if p, _ = w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{Name: strp("Renamed")}); p.Name != "Renamed" || strings.Join(p.Events, ",") != "comment" {
		t.Errorf("rename: %+v", p)
	}
	// A URL of another kind is refused for a Slack service (the kind never changes).
	if _, err := w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{WebhookURL: strp(goodDiscord)}); err == nil {
		t.Error("a Discord URL replaced a Slack one")
	}
	if _, err := w.svc2.Patch(ctx, w.org.ID, uuid.New(), chen, notify.ChatPatchInput{Enabled: ptrBool(false)}); err == nil {
		t.Error("patched a service that does not exist")
	}
	// Replacing the URL clears the status; the host follows the new URL (custom).
	c, err := create(notify.ChatCreateInput{Name: "Fluxer", TeamID: tm, Kind: "custom", WebhookURL: "https://chat.example.com/hooks/AAA", Events: []string{"assigned"}})
	if err != nil || c.Host != "chat.example.com" {
		t.Fatalf("custom: %+v %v", c, err)
	}
	w.exec(`UPDATE chat_services SET last_error = 'chat_gone' WHERE id = $1`, c.ID)
	c, err = w.svc2.Patch(ctx, w.org.ID, c.ID, chen, notify.ChatPatchInput{WebhookURL: strp("https://other.example.org/in/BBB")})
	if err != nil || c.Host != "other.example.org" || c.LastError != nil {
		t.Fatalf("replace url: %+v %v", c, err)
	}
	// Without a key nothing can be stored or opened.
	nokeys := notify.NewChatAdmin(w.pool, slog.New(slog.NewTextHandler(w.slogs, nil)), nil, nil, testBase)
	if _, err := nokeys.Create(ctx, w.org.ID, chen, notify.ChatCreateInput{Name: "k", TeamID: tm, Kind: "slack", WebhookURL: hookURL}); err == nil {
		t.Error("stored a URL without a key")
	}
	if l, _ := nokeys.List(ctx, w.org.ID); l.Available || !l.Items[0].SecretUnreadable {
		t.Errorf("no-key list %+v", l)
	}
	// The admin's writes emit an event without the URL, the name or the host.
	var n int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'chat_service.updated'`).Scan(&n); err != nil || n < 5 {
		t.Errorf("chat_service.updated events = %d (%v)", n, err)
	}
	var payloads string
	_ = w.pool.QueryRow(ctx, `SELECT string_agg(payload::text, ' ') FROM events WHERE type = 'chat_service.updated'`).Scan(&payloads)
	for _, leak := range []string{"Renamed", "Fluxer", "example", "hooks.slack", secretToken} {
		if strings.Contains(payloads, leak) {
			t.Errorf("event payload holds %q: %s", leak, payloads)
		}
	}
	// Delete twice: the second emits nothing.
	if err := w.svc2.Delete(ctx, w.org.ID, v.ID, chen); err != nil {
		t.Fatal(err)
	}
	var after int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'chat_service.updated'`).Scan(&after)
	if err := w.svc2.Delete(ctx, w.org.ID, v.ID, chen); err != nil {
		t.Fatal(err)
	}
	var again int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'chat_service.updated'`).Scan(&again)
	if again != after {
		t.Errorf("a second delete emitted")
	}
}

// The cap is ChatMaxServices per org, and it holds under concurrent creates.
func TestChat_CapOfTenServices(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	tm := w.teamOf(w.org.ID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, full int
	for i := 0; i < notify.ChatMaxServices+5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := w.svc2.Create(ctx, w.org.ID, w.user["chen"], notify.ChatCreateInput{
				Name: fmt.Sprintf("svc %d", i), TeamID: tm, Kind: "custom", WebhookURL: fmt.Sprintf("https://chat.example.com/h/%d", i)})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case strings.Contains(err.Error(), "at most"):
				full++
			default:
				t.Errorf("create %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != notify.ChatMaxServices || full != 5 {
		t.Fatalf("created %d, refused %d; want %d and 5", ok, full, notify.ChatMaxServices)
	}
	var n int
	_ = w.pool.QueryRow(ctx, `SELECT count(*) FROM chat_services`).Scan(&n)
	if n != notify.ChatMaxServices {
		t.Fatalf("%d rows", n)
	}
	// Another org is not counted.
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "chen")
	w.exec(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'OTH', 'Other')`, org2.ID)
	if _, err := w.svc2.Create(ctx, org2.ID, w.user["chen"], notify.ChatCreateInput{Name: "x", TeamID: w.teamOf(org2.ID), Kind: "slack", WebhookURL: hookURL}); err != nil {
		t.Errorf("org 2 is full too: %v", err)
	}
}

func ptrBool(b bool) *bool { return &b }

func strp(s string) *string { return &s }

// A huge Retry-After must not overflow time.Duration into a negative wait.
func TestChat_RetryAfterCapAndOverflow(t *testing.T) {
	f := newFakeChat(t)
	poster := notify.NewChatPoster(f.client(t))
	for in, want := range map[string]time.Duration{
		"120": 2 * time.Minute, "3600": time.Hour, "3601": time.Hour, "86400": time.Hour,
		"9223372036854775807": time.Hour, "18446744073": time.Hour, "9223372037": time.Hour,
		"0": 0, "-5": 0, "abc": 0, "": 0, "99999999999999999999": time.Hour,
		"1.5": 1500 * time.Millisecond, "0.2": 200 * time.Millisecond, "NaN": 0, "Inf": time.Hour,
	} {
		f.set(429, in)
		err := poster.Post(context.Background(), notify.ChatSlack, hookURL, notify.ChatMessage{Event: notify.EventTest})
		var se *notify.ChatError
		if !asChatErr(err, &se) || se.Word != notify.ChatRateLimited {
			t.Fatalf("%q: %v", in, err)
		}
		if se.RetryAfter != want {
			t.Errorf("Retry-After %q = %v, want %v", in, se.RetryAfter, want)
		}
	}
}

// One org's burst must not starve another org: the oldest 200 rows install-wide
// are all org A's, yet org B's single (newer) row goes out in the first pass.
func TestChat_FairAcrossOrgs(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "alice")
	w.addMember(org2.ID, "chen")
	hook2 := "https://hooks.slack.com/services/T0002/B0002/OTHERORGTOKEN"
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.putChannel(org2.ID, hook2, "assigned")
	isA := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	o2 := &auth.Org{ID: org2.ID, Slug: "other", Name: "Other", Role: "admin", Access: auth.TeamAccess{All: true}}
	isB, err := w.svc.CreateIssue(ctx, o2, w.user["chen"], tracker.CreateIssueInput{Team: "OTH", Title: "t", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	// 250 older rows for org A, then one newer row for org B.
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, created_at, next_attempt_at)
		SELECT $1, $4, 'assigned', $2, 'burst:' || g, $3::timestamptz - make_interval(secs => 1000 - g), $3
		FROM generate_series(1, 250) g`, w.org.ID, isA.ID, w.clk.now().Add(-time.Minute), w.svcID(w.org.ID))
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, created_at, next_attempt_at)
		VALUES ($1, $4, 'assigned', $2, 'b1', $3, $3)`, org2.ID, isB.ID, w.clk.now().Add(-time.Second), w.svcID(org2.ID))
	res := w.pass()
	if res.Sent != notify.ChatPerOrgPerPass+1 {
		t.Fatalf("first pass sent %d, want %d", res.Sent, notify.ChatPerOrgPerPass+1)
	}
	var b int
	for _, h := range w.slack.got() {
		if strings.Contains(h.Path, "OTHERORGTOKEN") {
			b++
		}
	}
	if b != 1 {
		t.Fatalf("org B posts in the first pass = %d, want 1", b)
	}
}

// DELETE takes the waiting posts with it: a webhook set later gets none of them.
func TestChat_DeleteDropsWaitingPosts(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	w.putChannel(w.org.ID, hookURL, "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	// One sent row (stays), one retrying row and one sending row (go).
	sid := w.svcID(w.org.ID)
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state, sent_at) VALUES ($1, $3, 'assigned', $2, 's1', 'sent', now())`, w.org.ID, is.ID, sid)
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state, attempts, last_error) VALUES ($1, $3, 'assigned', $2, 'r1', 'pending', 2, 'chat_unavailable')`, w.org.ID, is.ID, sid)
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state) VALUES ($1, $3, 'assigned', $2, 'g1', 'sending')`, w.org.ID, is.ID, sid)
	if err := w.svc2.Delete(ctx, w.org.ID, w.svcID(w.org.ID), w.user["chen"]); err != nil {
		t.Fatal(err)
	}
	// The posts of a removed service go with it (the outbox rows cascade), sent rows too.
	var all int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM chat_outbox`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if all != 0 {
		t.Fatalf("%d outbox rows left after the delete", all)
	}
	// A new webhook is set within the day: nothing old is posted to it.
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.pass()
	if n := len(w.slack.got()); n != 0 {
		t.Fatalf("the new webhook got %d old posts", n)
	}
}

// The admin can see that the cap dropped posts.
func TestChat_RateCapIsCounted(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	w.putChannel(w.org.ID, hookURL, "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state, sent_at)
		SELECT $1, $5, 'assigned', $2, 'old:' || g, 'sent', $3 FROM generate_series(1, $4) g`,
		w.org.ID, is.ID, w.clk.now().Add(-time.Minute), notify.ChatRateCap, w.svcID(w.org.ID))
	for i := 0; i < 2; i++ {
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	}
	w.consume()
	w.pass()
	w.svc2.SetClock(w.clk.now)
	v, err := w.svc2.Get(ctx, w.org.ID, w.svcID(w.org.ID))
	if err != nil || v.RateCappedCount != 2 || v.LastRateCappedAt == nil {
		t.Fatalf("view %+v %v", v, err)
	}
	// After a day the count is gone.
	w.clk.advance(25 * time.Hour)
	if v, _ := w.svc2.Get(ctx, w.org.ID, w.svcID(w.org.ID)); v.RateCappedCount != 0 || v.LastRateCappedAt != nil {
		t.Fatalf("stale count %+v", v)
	}
}

// A row the claim cannot take (it left pending between the list and the claim,
// as a DELETE purge does) is never sent. A trigger makes the claim UPDATE touch
// no row, which is what the purge looks like to the claim.
func TestChat_UnclaimedRowIsNotSent(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`CREATE FUNCTION chat_no_claim() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
	w.exec(`CREATE TRIGGER chat_no_claim BEFORE UPDATE ON chat_outbox FOR EACH ROW WHEN (NEW.state = 'sending') EXECUTE FUNCTION chat_no_claim()`)
	if res := w.pass(); res.Sent != 0 {
		t.Fatalf("pass %+v", res)
	}
	if n := len(w.slack.got()); n != 0 {
		t.Fatalf("an unclaimed row was posted (%d)", n)
	}
	if r := w.onlyRow(); r.State != "pending" {
		t.Fatalf("row %+v", r)
	}
}

// A row whose service is off is skipped, never posted (sendRow re-reads the
// service). The flag is flipped in SQL so the row stays: a switch that raced
// the list.
func TestChat_RowOfDisabledServiceIsSkipped(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`UPDATE chat_services SET enabled = false`)
	if res := w.pass(); res.Sent != 0 || res.Skipped != 1 {
		t.Fatalf("pass %+v", res)
	}
	if n := len(w.slack.got()); n != 0 {
		t.Fatalf("posted for a disabled service: %d", n)
	}
	if r := w.onlyRow(); r.State != "skipped" || r.Skip != "disabled" {
		t.Fatalf("row %+v", r)
	}
}

// netguardMap is a client whose resolver maps names to the given addresses and
// whose dialer has the production policy, so a private answer is refused.
type netguardMap struct{ Hosts map[string][]string }

func (m *netguardMap) client() *http.Client {
	d := &netguard.Dialer{Policy: notify.ChatPolicy(nil), Resolver: &netguardtest.MapResolver{Hosts: m.Hosts}, Timeout: 2 * time.Second}
	return notify.NewChatClientFor(d)
}

// One service that never answers must not cost the pass one timeout per row:
// the rest of ITS rows are released untouched, and another service of the same
// org still gets its posts in the same pass.
func TestChat_UnreachableServiceIsSkippedForThePass(t *testing.T) {
	w := newChatWorld(t)
	deadHook := "https://hooks.slack.com/services/T0009/B0009/DEADTOKEN"
	w.putService(w.org.ID, notify.ChatSlack, "Dead", deadHook, "assigned")
	w.putService(w.org.ID, notify.ChatSlack, "Live", hookURL, "assigned")
	w.slack.mu.Lock()
	w.slack.hangPath = "DEADTOKEN"
	w.slack.mu.Unlock()
	for i := 0; i < 3; i++ {
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	}
	w.consume()
	res := w.pass()
	if res.Retried != 1 || res.Released != 2 || res.Sent != 3 {
		t.Fatalf("pass = %+v, want retried 1, released 2, sent 3", res)
	}
	var attempts, pending int
	if err := w.pool.QueryRow(context.Background(), `SELECT coalesce(sum(attempts),0), count(*) FILTER (WHERE state = 'pending')
		FROM chat_outbox o JOIN chat_services s ON s.id = o.service_id AND s.org_id = o.org_id WHERE s.name = 'Dead'`).Scan(&attempts, &pending); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || pending != 3 {
		t.Fatalf("dead service: attempts %d (want 1), pending %d (want 3)", attempts, pending)
	}
	// The released rows stay due: the next pass tries one row again, no more.
	w.clk.advance(2 * time.Minute)
	if res := w.pass(); res.Retried != 1 || res.Released != 2 {
		t.Fatalf("next pass = %+v", res)
	}
}

// The pass has a wall-clock budget. Past it, the claimed rows go back to
// pending with no attempt used, and the next pass sends them.
func TestChat_PassBudgetReleasesTheRest(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.slack.mu.Lock()
	w.slack.delay = 300 * time.Millisecond
	w.slack.mu.Unlock()
	for i := 0; i < 4; i++ {
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	}
	w.consume()
	w.sender.SetBudget(100 * time.Millisecond)
	res := w.pass()
	if res.Sent != 1 || res.Released != 3 {
		t.Fatalf("pass = %+v, want sent 1, released 3", res)
	}
	for _, r := range w.outRows() {
		if r.State == "pending" && r.Attempts != 0 {
			t.Fatalf("a released row used an attempt: %+v", r)
		}
	}
	w.sender.SetBudget(time.Minute)
	w.slack.mu.Lock()
	w.slack.delay = 0
	w.slack.mu.Unlock()
	if res := w.pass(); res.Sent != 3 || res.Released != 0 {
		t.Fatalf("next pass = %+v", res)
	}
}

// A service switched off while the post is in flight drops its row; that is
// not a sent post and the pass must not count it.
func TestChat_SentIsNotCountedWhenTheRowIsGone(t *testing.T) {
	w := newChatWorld(t)
	v := w.putChannel2(w.org.ID, hookURL, "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.slack.onHit = func() {
		off := false
		if _, err := w.svc2.Patch(context.Background(), w.org.ID, v.ID, w.user["chen"], notify.ChatPatchInput{Enabled: &off}); err != nil {
			t.Errorf("Patch: %v", err)
		}
	}
	res := w.pass()
	if res.Sent != 0 {
		t.Fatalf("pass = %+v, a dropped row was counted as sent", res)
	}
}

func (w *swo) putChannel2(org uuid.UUID, hook string, events ...string) notify.ChatView {
	w.t.Helper()
	return w.putService(org, notify.ChatSlack, "Slack", hook, events...)
}

// A dead service with many old rows must not fill the org's claim slots: the
// healthy service of the SAME org is sent in the first pass and in every pass
// after it.
func TestChat_DeadServiceDoesNotStarveItsOrgsOtherServices(t *testing.T) {
	w := newChatWorld(t)
	deadHook := "https://hooks.slack.com/services/T0009/B0009/DEADTOKEN"
	dead := w.putService(w.org.ID, notify.ChatSlack, "Dead", deadHook, "assigned")
	live := w.putService(w.org.ID, notify.ChatSlack, "Live", hookURL, "assigned")
	w.slack.mu.Lock()
	w.slack.hangPath = "DEADTOKEN"
	w.slack.mu.Unlock()
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	old := w.clk.now().Add(-time.Hour)
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, created_at, next_attempt_at)
		SELECT $1, $4, 'assigned', $2, 'dead:' || g, $3::timestamptz + make_interval(secs => g), $3
		FROM generate_series(1, 15) g`, w.org.ID, is.ID, old, dead.ID)
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, created_at, next_attempt_at)
		VALUES ($1, $4, 'assigned', $2, 'live:1', $3, $3)`, w.org.ID, is.ID, w.clk.now().Add(-time.Minute), live.ID)
	for pass := 1; pass <= 3; pass++ {
		w.pass()
		if pass == 1 {
			var sent int
			if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE service_id = $1 AND state = 'sent'`, live.ID).Scan(&sent); err != nil || sent != 1 {
				t.Fatalf("pass 1: healthy row sent = %d (%v), want 1", sent, err)
			}
		}
		// A new healthy row, newer than every dead row, is also sent in this pass.
		w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, created_at, next_attempt_at)
			VALUES ($1, $4, 'assigned', $2, $5, $3, $3)`, w.org.ID, is.ID, w.clk.now(), live.ID, fmt.Sprintf("live:p%d", pass))
		w.clk.advance(2 * time.Minute)
		w.exec(`UPDATE chat_outbox SET next_attempt_at = LEAST(next_attempt_at, $1) WHERE state = 'pending'`, w.clk.now())
	}
	w.pass()
	var left int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE service_id = $1 AND state <> 'sent'`, live.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("healthy rows not sent = %d (%v)", left, err)
	}
	var burned int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE service_id = $1 AND attempts > 4`, dead.ID).Scan(&burned); err != nil || burned != 0 {
		t.Fatalf("dead rows burned attempts: %d (%v)", burned, err)
	}
}

// A 429 (or any Retry-After) trips the service for the pass too: the rest of
// its rows are released with no attempt used.
func TestChat_RateLimitedServiceIsSkippedForThePass(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "assigned")
	w.slack.set(429, "30")
	for i := 0; i < 3; i++ {
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	}
	w.consume()
	res := w.pass()
	if res.Retried != 1 || res.Released != 2 || res.Sent != 0 {
		t.Fatalf("pass = %+v, want retried 1, released 2", res)
	}
	if n := len(w.slack.got()); n != 1 {
		t.Fatalf("posts made = %d, want 1", n)
	}
	for _, r := range w.outRows() {
		if r.State == "pending" && r.LastErr == "" && r.Attempts != 0 {
			t.Fatalf("a released row used an attempt: %+v", r)
		}
	}
}
