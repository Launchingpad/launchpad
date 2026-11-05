package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/move"
	"devops/internal/moveapi"
	"devops/internal/platform"
	"devops/internal/setup"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// LAUN-249: the move routes (docs/internal/decisions-T46-move-server.md).

func moveKeys(t *testing.T, settings, backup byte) move.Keys {
	t.Helper()
	sm, bm := bytes.Repeat([]byte{settings}, 32), bytes.Repeat([]byte{backup}, 32)
	var k move.Keys
	var err error
	if k.Settings, err = platform.NewKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Git, err = platform.NewGitKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Push, err = platform.NewPushKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Slack, err = platform.NewSlackKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Backup, err = platform.NewBackupKeyring(bm); err != nil {
		t.Fatal(err)
	}
	return k
}

// moveStore is a second bucket of the test silo, so a target never shares
// objects with a source.
func moveStore(t *testing.T, bucket string) *storage.S3 {
	t.Helper()
	storagetest.New(t) // skips without INTEGRATION=1
	cfg := storagetest.Config()
	cfg.Bucket = bucket
	st, err := storage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

type moveEnvOpts struct {
	empty      bool
	keys       byte
	store      move.ObjectStore
	now        func() time.Time
	logLevel   slog.Level
	workFactor int // scrypt log2 cost of the passphrase; 0: default
}

// newMoveEnv is a server with the move service wired.
func newMoveEnv(t *testing.T, o moveEnvOpts) *env {
	t.Helper()
	testdb.Require(t)
	if o.keys == 0 {
		o.keys = 0x11
	}
	if o.workFactor == 0 {
		o.workFactor = 10 // default scrypt costs 256 MiB and about a second per export and import
	}
	return newEnvWith(t, envOpts{empty: o.empty, logLevel: o.logLevel, configure: func(e *env, s *Server) {
		mo := moveapi.Options{Pool: e.pool, Keys: moveKeys(t, o.keys, o.keys+1), AppVersion: "test", Log: s.log, Now: o.now, ScryptWorkFactor: o.workFactor}
		if o.store != nil {
			mo.Store = o.store
		}
		s.Move = moveapi.New(mo)
	}})
}

// adminToken makes chen a platform admin and returns an unpinned admin token.
func (e *env) platformAdminToken() string {
	e.t.Helper()
	u := e.user("chen")
	if err := e.setPlatformAdmin(u.ID); err != nil {
		e.t.Fatal(err)
	}
	tok, _ := e.newToken(u.ID, tokenOpts{})
	return tok
}

func (e *env) setPlatformAdmin(id uuid.UUID) error {
	_, err := e.pool.Exec(context.Background(), `UPDATE users SET is_platform_admin = true WHERE id = $1`, id)
	return err
}

// startExport runs POST /platform/move/export and returns the body.
func (e *env) startExport(token string, body any) moveapi.StartResult {
	e.t.Helper()
	var res moveapi.StartResult
	e.must(http.StatusCreated, e.do("POST", "/api/v1/platform/move/export", token, body)).json(e.t, &res)
	return res
}

func (e *env) raw(method, path, token string, body io.Reader, headers ...string) (*http.Response, error) {
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		return nil, err
	}
	e.authorize(req, token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return http.DefaultClient.Do(req)
}

func readAll(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func errCodeOf(t *testing.T, status int, b []byte) string {
	t.Helper()
	return resp{status: status, body: b}.errCode(t)
}

// download fetches a ticket and returns the status and body.
func (e *env) moveDownload(token string, r moveapi.StartResult) (int, []byte) {
	e.t.Helper()
	res, err := e.raw("GET", r.DownloadPath, token, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return res.StatusCode, readAll(e.t, res)
}

// restore posts a raw bundle with the headers; it returns the response.
func (e *env) restore(body io.Reader, link, pass string, extra ...string) *http.Response {
	e.t.Helper()
	h := append([]string{"Content-Type", "application/octet-stream"}, extra...)
	if link != "" {
		h = append(h, moveapiHeaderLink, link)
	}
	if pass != "" {
		h = append(h, moveapiHeaderPass, pass)
	}
	res, err := e.raw("POST", "/api/v1/setup/restore", "", body, h...)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

const (
	moveapiHeaderLink = HeaderSetupToken
	moveapiHeaderPass = HeaderPassphrase
)

// TestMoveExport_Access: platform admin only; an agent, a member, a token
// pinned to an org, a read-only token and another org's admin are refused; no
// token is 401. The refusal is the same on the start and the download.
func TestMoveExport_Access(t *testing.T) {
	e := newMoveEnv(t, moveEnvOpts{})
	admin := e.platformAdminToken()
	good := e.startExport(admin, map[string]any{"includeAttachments": false})

	chen := e.user("chen")
	claude := e.user("claude")
	// An agent, even one flagged as a platform admin, is refused.
	if err := e.setPlatformAdmin(claude.ID); err != nil {
		t.Fatal(err)
	}
	agentTok, _ := e.newToken(claude.ID, tokenOpts{})
	member := e.newUser("mia")
	e.addMember(e.orgID, member.ID, "member")
	memberTok, _ := e.newToken(member.ID, tokenOpts{})
	// Cross-org: an org admin of ANOTHER org, not a platform admin.
	other := e.newOrg("other", "OTH")
	boss := e.newUser("boss")
	e.addMember(other, boss.ID, "admin")
	bossTok, _ := e.newToken(boss.ID, tokenOpts{})
	pinned, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID})
	readOnly, _ := e.newToken(chen.ID, tokenOpts{scopes: []string{"read"}})

	for name, tok := range map[string]string{
		"agent": agentTok, "member": memberTok, "other org admin": bossTok, "org-pinned": pinned, "read-only": readOnly,
	} {
		e.must(http.StatusForbidden, e.do("POST", "/api/v1/platform/move/export", tok, nil))
		if st, _ := e.moveDownload(tok, good); st != http.StatusForbidden {
			t.Errorf("%s: download status %d, want 403", name, st)
		}
	}
	e.must(http.StatusUnauthorized, e.do("POST", "/api/v1/platform/move/export", "", nil))
	if st, _ := e.moveDownload("", good); st != http.StatusUnauthorized {
		t.Errorf("no token: download status %d", st)
	}
	// The refused calls did not use the ticket.
	if st, body := e.moveDownload(admin, good); st != http.StatusOK || len(body) == 0 {
		t.Fatalf("the good ticket: status %d, %d bytes", st, len(body))
	}
}

// TestMoveExport_TicketLifecycle: single use, expiry, owner binding, a new
// start drops the older ticket, the Idempotency-Key header is refused.
func TestMoveExport_TicketLifecycle(t *testing.T) {
	var clock atomic.Pointer[time.Time]
	set := func(d time.Time) { clock.Store(&d) }
	set(time.Now())
	e := newMoveEnv(t, moveEnvOpts{now: func() time.Time { return *clock.Load() }})
	admin := e.platformAdminToken()

	r := e.startExport(admin, nil) // no body: the defaults (no store, so no attachments)
	if r.IncludeAttachments || !strings.HasPrefix(r.DownloadPath, moveapi.DownloadPrefix+r.Ticket) ||
		!strings.HasSuffix(r.Filename, moveapi.FileExt) || r.ExpiresAt.Sub(*clock.Load()) != moveapi.TicketTTL {
		t.Fatalf("start result %+v", r)
	}
	if len(r.Passphrase) != 39 || len(r.Ticket) != 43 {
		t.Fatalf("passphrase %d ticket %d chars", len(r.Passphrase), len(r.Ticket))
	}
	res, err := e.raw("GET", r.DownloadPath, admin, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, res)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(res.Header.Get("Content-Disposition"), r.Filename) || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", res.StatusCode, res.Header)
	}
	if bytes.Contains(body, []byte(r.Passphrase)) || bytes.Contains(body, []byte("migrationVersion")) {
		t.Fatal("the bundle is not encrypted")
	}
	// Single use.
	if st, b := e.moveDownload(admin, r); st != 404 || errCodeOf(t, st, b) != "not_found" {
		t.Fatalf("second download: %d %s", st, b)
	}
	// Expiry.
	r2 := e.startExport(admin, nil)
	set(clock.Load().Add(moveapi.TicketTTL))
	if st, _ := e.moveDownload(admin, r2); st != 404 {
		t.Fatalf("expired ticket: %d", st)
	}
	// Unknown and malformed tickets.
	r2.DownloadPath = moveapi.DownloadPrefix + strings.Repeat("A", 43)
	if st, _ := e.moveDownload(admin, r2); st != 404 {
		t.Fatalf("unknown ticket: %d", st)
	}
	r2.DownloadPath = moveapi.DownloadPrefix + "short"
	if st, _ := e.moveDownload(admin, r2); st != 404 {
		t.Fatalf("malformed ticket: %d", st)
	}
	// Another platform admin cannot use my ticket, and it stays mine.
	r3 := e.startExport(admin, nil)
	mia := e.newUser("mia")
	if err := e.setPlatformAdmin(mia.ID); err != nil {
		t.Fatal(err)
	}
	miaTok, _ := e.newToken(mia.ID, tokenOpts{})
	if st, _ := e.moveDownload(miaTok, r3); st != 404 {
		t.Fatalf("another admin's ticket: %d", st)
	}
	// A new start drops that user's older ticket only: mia's survives chen's start.
	miaTicket := e.startExport(miaTok, nil)
	r4 := e.startExport(admin, nil)
	if st, _ := e.moveDownload(miaTok, miaTicket); st != 200 {
		t.Fatalf("another admin's ticket was dropped by my start: %d", st)
	}
	if st, _ := e.moveDownload(admin, r3); st != 404 {
		t.Fatalf("older ticket after a new start: %d", st)
	}
	if st, _ := e.moveDownload(admin, r4); st != 200 {
		t.Fatalf("newest ticket: %d", st)
	}
	// Expired tickets (and their passphrases) are purged by the next start.
	r5 := e.startExport(admin, nil)
	set(clock.Load().Add(moveapi.TicketTTL))
	e.startExport(miaTok, nil)
	if st, _ := e.moveDownload(admin, r5); st != 404 {
		t.Fatalf("expired ticket after another start: %d", st)
	}
	// Idempotency-Key is refused; the attachments option without a store is 409.
	e.must(400, e.do("POST", "/api/v1/platform/move/export", admin, nil, "Idempotency-Key", "k1"))
	if c := e.must(409, e.do("POST", "/api/v1/platform/move/export", admin, map[string]any{"includeAttachments": true})).errCode(t); c != "move_blocked" {
		t.Fatalf("code %s", c)
	}
	// Nothing of this reached the idempotency table or the change feed.
	if n := e.countRows(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d idempotency rows", n)
	}
}

// TestMoveExport_OneAtATime: while another export holds the advisory lock the
// download is 409 export_in_progress and the ticket is NOT used.
func TestMoveExport_OneAtATime(t *testing.T) {
	e := newMoveEnv(t, moveEnvOpts{})
	admin := e.platformAdminToken()
	r := e.startExport(admin, nil)
	ctx := context.Background()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('devops-move-export'))`); err != nil {
		t.Fatal(err)
	}
	st, b := e.moveDownload(admin, r)
	if st != 409 || errCodeOf(t, st, b) != "export_in_progress" {
		t.Fatalf("busy: %d %s", st, b)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('devops-move-export'))`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if st, _ := e.moveDownload(admin, r); st != 200 {
		t.Fatalf("after the lock is free: %d", st)
	}
}

// restoreOK runs a restore and returns the decoded 201 body.
func (e *env) restoreOK(body io.Reader, link, pass string, extra ...string) (moveapi.RestoreResponse, *http.Response) {
	e.t.Helper()
	res := e.restore(body, link, pass, extra...)
	b := readAll(e.t, res)
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("restore: %d %s", res.StatusCode, b)
	}
	var out moveapi.RestoreResponse
	resp{body: b}.json(e.t, &out)
	return out, res
}

// exportBundle makes a bundle through the HTTP routes.
func exportBundle(t *testing.T, e *env, admin string, attachments bool) (moveapi.StartResult, []byte) {
	t.Helper()
	r := e.startExport(admin, map[string]any{"includeAttachments": attachments})
	st, b := e.moveDownload(admin, r)
	if st != 200 {
		t.Fatalf("download: %d %s", st, b)
	}
	return r, b
}

// TestMoveRestore_RoundTrip: export on one server, restore on an empty one
// (other keys, other bucket): the session and the token work, the data is
// there, a second restore is 409, and no secret is in the logs or events.
func TestMoveRestore_RoundTrip(t *testing.T) {
	srcStore := moveStore(t, "devops-test")
	src := newMoveEnv(t, moveEnvOpts{store: srcStore, logLevel: slog.LevelDebug})
	admin := src.platformAdminToken()
	is := src.createIssue(src.chen, map[string]any{"title": "carry me over", "description": "body"})
	srcEvents := src.countRows(`SELECT count(*) FROM events`)
	r, bundle := exportBundle(t, src, admin, true)
	if n := src.countRows(`SELECT count(*) FROM events`); n != srcEvents {
		t.Fatalf("export emitted events: %d -> %d", srcEvents, n)
	}

	dst := newMoveEnv(t, moveEnvOpts{empty: true, keys: 0x33, store: moveStore(t, "devops-test-move-in"), logLevel: slog.LevelDebug})
	link := dst.mintLink()
	// A multipart restore with the passphrase as the first part.
	var mp bytes.Buffer
	w := multipart.NewWriter(&mp)
	_ = w.WriteField("passphrase", strings.ToLower(r.Passphrase))
	fw, _ := w.CreateFormFile("file", "x.lpmove")
	_, _ = fw.Write(bundle)
	_ = w.Close()
	out, res := dst.restoreOK(&mp, link, "", "Content-Type", w.FormDataContentType())

	if !out.User.IsPlatformAdmin || out.User.Username != "chen" || len(out.Orgs) == 0 || out.Summary.Rows == 0 || !out.Summary.AttachmentsIncluded {
		t.Fatalf("response %+v", out)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", res.Header.Get("Cache-Control"))
	}
	var sess *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == dst.names.Session {
			sess = c
		}
	}
	if sess == nil || sess.Value == "" {
		t.Fatal("no session cookie")
	}
	if cc := cookieByName(res, dst.names.CSRF); cc == nil || cc.Value != out.CSRFToken {
		t.Fatalf("csrf cookie %v", cc)
	}
	// The token and the session both work; the token has no expiry and read, write, admin.
	tok := out.Token.Secret
	if !auth.WellFormed(tok) || out.Token.Token.ExpiresAt != nil || len(out.Token.Token.Scopes) != 3 {
		t.Fatalf("token %+v", out.Token.Token)
	}
	var me tracker.Me
	dst.must(200, dst.do("GET", "/api/v1/me", tok, nil)).json(t, &me)
	if !me.IsPlatformAdmin || me.PinnedOrgID == nil {
		t.Fatalf("me %+v", me)
	}
	var got tracker.Issue
	dst.must(200, dst.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier, tok, nil)).json(t, &got)
	if got.Title != "carry me over" {
		t.Fatalf("issue %+v", got)
	}
	dst.must(200, dst.do("GET", "/api/v1/auth/session", dst.asSession(sess.Value), nil))
	if dst.setupRequired() {
		t.Fatal("setup still required")
	}
	// The link is gone: a second restore with it is 401; with a fresh live
	// link on a used install it is 409 setup_complete.
	res2 := dst.restore(bytes.NewReader(bundle), link, r.Passphrase)
	if b := readAll(t, res2); res2.StatusCode != 401 || errCodeOf(t, 401, b) != "invalid_setup_token" {
		t.Fatalf("used link: %d %s", res2.StatusCode, b)
	}
	live := "L" + strings.Repeat("x", 41) + "y"
	dst.insertLink(live)
	res3 := dst.restore(bytes.NewReader(bundle), live, r.Passphrase)
	if b := readAll(t, res3); res3.StatusCode != 409 || errCodeOf(t, 409, b) != "setup_complete" {
		t.Fatalf("complete: %d %s", res3.StatusCode, b)
	}

	// No passphrase, ticket, link, token or session in logs, events or idempotency rows.
	secrets := []string{r.Passphrase, strings.ReplaceAll(r.Passphrase, "-", ""), r.Ticket, link, tok, sess.Value, out.CSRFToken}
	for name, text := range map[string]string{"source logs": src.logs.String(), "target logs": dst.logs.String(),
		"source events": src.dump(`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`),
		"target events": dst.dump(`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`),
		"idempotency":   src.dump(`SELECT coalesce(string_agg(key || path || convert_from(coalesce(response_body, ''::bytea), 'UTF8'), ' '), '') FROM idempotency_keys`) + dst.dump(`SELECT coalesce(string_agg(key, ' '), '') FROM idempotency_keys`),
	} {
		for _, s := range secrets {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("%s hold a secret (%d chars)", name, len(s))
			}
		}
	}
	if !strings.Contains(src.logs.String(), "{ticket}") {
		t.Error("the ticket path is not hidden in the access log")
	}
}

func cookieByName(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (e *env) dump(sql string) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), sql).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// insertLink stores token as a live setup link (what a boot would log).
func (e *env) insertLink(token string) {
	e.t.Helper()
	sum := sha256.Sum256([]byte(token))
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO setup_tokens (token_hash, expires_at) VALUES ($1, now() + interval '1 hour')`, sum[:]); err != nil {
		e.t.Fatal(err)
	}
}

// TestMoveRestore_SetupLinkFirst: a wrong, missing, expired, replaced or used
// link is 401 invalid_setup_token BEFORE the body is read (the request body is
// a pipe that never delivers a byte), and nothing else answers first.
func TestMoveRestore_SetupLinkFirst(t *testing.T) {
	e := newMoveEnv(t, moveEnvOpts{empty: true})
	good := e.mintLink()
	older := good
	good = e.mintLink() // closes the older link
	expired := "E" + strings.Repeat("e", 42)
	e.insertLink(expired)
	if _, err := e.pool.Exec(context.Background(), `UPDATE setup_tokens SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 minute' WHERE token_hash = $1`,
		func() []byte { s := sha256.Sum256([]byte(expired)); return s[:] }()); err != nil {
		t.Fatal(err)
	}
	for name, link := range map[string]string{"missing": "", "malformed": "nope", "wrong": strings.Repeat("A", 43),
		"replaced": older, "expired": expired} {
		pr, pw := io.Pipe() // never written: a read would block the request
		done := make(chan *http.Response, 1)
		go func() {
			h := []string{"Content-Type", "application/octet-stream", moveapiHeaderPass, "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA"}
			if link != "" {
				h = append(h, moveapiHeaderLink, link)
			}
			res, err := e.raw("POST", "/api/v1/setup/restore", "", pr, h...)
			if err != nil {
				t.Error(err)
			}
			done <- res
		}()
		select {
		case res := <-done:
			b := readAll(t, res)
			if res == nil || res.StatusCode != 401 || errCodeOf(t, 401, b) != "invalid_setup_token" {
				t.Errorf("%s: %d %s", name, res.StatusCode, b)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: no answer before the body was read", name)
		}
		_ = pw.Close()
	}
	// The passphrase never comes from the query string; no passphrase is 400
	// and leaves the link usable. Idempotency-Key is refused.
	res := e.restore(bytes.NewReader([]byte("x")), good, "")
	if b := readAll(t, res); res.StatusCode != 400 || errCodeOf(t, 400, b) != "validation_failed" {
		t.Fatalf("no passphrase: %d %s", res.StatusCode, b)
	}
	res, err := e.raw("POST", "/api/v1/setup/restore?passphrase=AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA", "", bytes.NewReader([]byte("x")),
		moveapiHeaderLink, good)
	if err != nil {
		t.Fatal(err)
	}
	if b := readAll(t, res); res.StatusCode != 400 {
		t.Fatalf("query passphrase: %d %s", res.StatusCode, b)
	}
	res = e.restore(bytes.NewReader([]byte("x")), good, "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA", "Idempotency-Key", "k")
	if b := readAll(t, res); res.StatusCode != 400 {
		t.Fatalf("idempotency key: %d %s", res.StatusCode, b)
	}
	// Not a bundle / wrong passphrase: 400, nothing restored, the link still works.
	res = e.restore(bytes.NewReader(bytes.Repeat([]byte("junk"), 100)), good, "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA")
	if b := readAll(t, res); res.StatusCode != 400 || !strings.Contains("bad_passphrase bundle_invalid", errCodeOf(t, 400, b)) {
		t.Fatalf("junk: %d %s", res.StatusCode, b)
	}
	if !e.setupRequired() {
		t.Fatal("a failed restore left the install out of setup state")
	}
	if err := setup.CheckLink(context.Background(), e.pool, good); err != nil {
		t.Fatalf("the link was used up by a failed restore: %v", err)
	}
}

// TestMoveRestore_Refusals: a wrong passphrase, a damaged bundle and a bundle
// with no admin leave the install in setup state with the link usable.
func TestMoveRestore_Refusals(t *testing.T) {
	src := newMoveEnv(t, moveEnvOpts{})
	admin := src.platformAdminToken()
	r, bundle := exportBundle(t, src, admin, false)
	dst := newMoveEnv(t, moveEnvOpts{empty: true, keys: 0x33})
	link := dst.mintLink()

	res := dst.restore(bytes.NewReader(bundle), link, "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA")
	if b := readAll(t, res); res.StatusCode != 400 || errCodeOf(t, 400, b) != "bad_passphrase" {
		t.Fatalf("wrong passphrase: %d %s", res.StatusCode, b)
	}
	res = dst.restore(bytes.NewReader(bundle[:len(bundle)-40]), link, r.Passphrase)
	if b := readAll(t, res); res.StatusCode != 400 || errCodeOf(t, 400, b) != "bundle_invalid" {
		t.Fatalf("cut bundle: %d %s", res.StatusCode, b)
	}
	// A bundle with no active platform admin who is an org admin.
	if _, err := src.pool.Exec(context.Background(), `UPDATE users SET is_platform_admin = false`); err != nil {
		t.Fatal(err)
	}
	pass, _ := move.GeneratePassphrase()
	var buf bytes.Buffer
	if _, err := move.Export(context.Background(), &buf, move.ExportOptions{Pool: src.pool, Keys: moveKeys(t, 0x11, 0x12), Passphrase: pass, ScryptWorkFactor: 10}); err != nil {
		t.Fatal(err)
	}
	res = dst.restore(&buf, link, pass)
	if b := readAll(t, res); res.StatusCode != 409 || errCodeOf(t, 409, b) != "move_blocked" {
		t.Fatalf("no admin: %d %s", res.StatusCode, b)
	}
	if !dst.setupRequired() {
		t.Fatal("install left setup state")
	}
	if n := dst.countRows(`SELECT count(*) FROM users`) + dst.countRows(`SELECT count(*) FROM orgs`); n != 0 {
		t.Fatalf("%d rows left behind", n)
	}
	// The link still works: the good bundle restores.
	dst.restoreOK(bytes.NewReader(bundle), link, r.Passphrase)
}

// TestMoveRestore_LinkOnUsedInstall: a live link on an install that has a
// user is 409 setup_complete (a malformed one is still 401).
func TestMoveRestore_LinkOnUsedInstall(t *testing.T) {
	e := newMoveEnv(t, moveEnvOpts{})
	live := "L" + strings.Repeat("x", 41) + "y"
	e.insertLink(live)
	res := e.restore(bytes.NewReader([]byte("x")), live, "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA")
	if b := readAll(t, res); res.StatusCode != 409 || errCodeOf(t, 409, b) != "setup_complete" {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
	res = e.restore(bytes.NewReader([]byte("x")), "bad", "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA")
	if b := readAll(t, res); res.StatusCode != 401 {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

// patternReader makes n bytes of a repeating, non-compressible-enough
// pattern without holding them, and hashes what it hands out.
type patternReader struct {
	n, i int64
	h    interface{ Write([]byte) (int, error) }
	seed uint64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.i >= p.n {
		return 0, io.EOF
	}
	k := int64(len(b))
	if rem := p.n - p.i; k > rem {
		k = rem
	}
	for j := int64(0); j < k; j++ {
		p.seed = p.seed*6364136223846793005 + 1442695040888963407
		b[j] = byte(p.seed >> 56)
	}
	p.i += k
	_, _ = p.h.Write(b[:k])
	return int(k), nil
}

// TestMoveRestore_LargeStreamedRoundTrip: a 96 MiB attachment moves from one
// server to another through the HTTP routes, the download body piped straight
// into the restore request (no file, no buffer in the test either). The heap
// of the process (both servers, the client, the store client) must stay far
// below the object size.
func TestMoveRestore_LargeStreamedRoundTrip(t *testing.T) {
	const size = 96 << 20
	srcStore := moveStore(t, "devops-test")
	src := newMoveEnv(t, moveEnvOpts{store: srcStore, workFactor: 10}) // a low scrypt cost: its 256 MiB would hide the stream
	admin := src.platformAdminToken()
	is := src.createIssue(src.chen, map[string]any{"title": "big"})
	id := uuid.New()
	key := tracker.AttachmentObjectKey(src.orgID, 2026, id, "big.bin")
	h := sha256.New()
	pr := &patternReader{n: size, h: h, seed: 42}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := srcStore.Put(ctx, key, "application/octet-stream", size, pr); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(h.Sum(nil))
	if _, err := src.pool.Exec(ctx, `INSERT INTO attachments (id, org_id, issue_id, kind, title, object_key, filename, content_type, size_bytes, creator_id)
		VALUES ($1, $2, (SELECT id FROM issues WHERE org_id = $2 AND number = $5), 'file', 'big', $3, 'big.bin', 'application/octet-stream', $4, (SELECT id FROM users WHERE username = 'chen'))`,
		id, src.orgID, key, int64(size), is.Number); err != nil {
		t.Fatal(err)
	}
	dstStore := moveStore(t, "devops-test-move-in")
	dst := newMoveEnv(t, moveEnvOpts{empty: true, keys: 0x33, store: dstStore})
	link := dst.mintLink()

	r := src.startExport(admin, map[string]any{"includeAttachments": true})
	runtime.GC()
	old := debug.SetGCPercent(10)
	defer debug.SetGCPercent(old)
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
		}
	})
	dl, err := src.raw("GET", r.DownloadPath, admin, nil)
	if err != nil || dl.StatusCode != 200 {
		t.Fatalf("download: %v %v", err, dl)
	}
	res := dst.restore(dl.Body, link, r.Passphrase)
	b := readAll(t, res)
	_ = dl.Body.Close()
	close(stop)
	wg.Wait()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("restore: %d %s", res.StatusCode, b)
	}
	if grow := int64(peak.Load()) - int64(base.HeapAlloc); grow > 40<<20 {
		t.Fatalf("heap grew by %d MiB while moving a %d MiB file: the stream is buffered", grow>>20, size>>20)
	} else {
		t.Logf("heap grew by %d MiB for a %d MiB file", grow>>20, size>>20)
	}
	// The object arrived whole.
	rc, info, err := dstStore.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	h2 := sha256.New()
	if n, err := io.Copy(h2, rc); err != nil || n != size || info.Size != size {
		t.Fatalf("copied %d bytes, err %v", n, err)
	}
	if got := hex.EncodeToString(h2.Sum(nil)); got != want {
		t.Fatalf("object checksum %s, want %s", got, want)
	}
}
