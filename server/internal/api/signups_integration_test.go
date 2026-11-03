package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/setup"
	"devops/internal/signup"
)

// sign-up requests, invites and platform admins, against the real
// router and Postgres (docs/internal/decisions-T12-signups-invites.md).

func (e *env) setSignupMode(mode string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE platform_settings SET signup_mode = $1`, mode); err != nil {
		e.t.Fatal(err)
	}
}

func signupBody(email, username string) map[string]any {
	return map[string]any{"email": email, "name": "Person " + username, "username": username, "note": "hello"}
}

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

// lastSignupCode is the newest confirm code mailed to "to" ("" if none).
func (e *env) lastSignupCode(to string) string {
	e.t.Helper()
	all := e.smail.all()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].to == to && strings.Contains(all[i].subject, "sign-up code") {
			return sixDigits.FindString(all[i].text)
		}
	}
	return ""
}

var inviteInMail = regexp.MustCompile(`/invite#([A-Za-z0-9_-]{43})`)

// mailedInviteToken is the token of the newest invite mailed to "to".
func (e *env) mailedInviteToken(to string) string {
	e.t.Helper()
	all := e.smail.all()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].to == to {
			if m := inviteInMail.FindStringSubmatch(all[i].text); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// mailsTo counts the emails to "to" whose subject holds sub.
func (e *env) mailsTo(to, sub string) int {
	n := 0
	for _, m := range e.smail.all() {
		if m.to == to && strings.Contains(m.subject, sub) {
			n++
		}
	}
	return n
}

func (e *env) ask(email, username string) {
	e.t.Helper()
	e.must(202, e.do("POST", "/api/v1/signups", "", signupBody(email, username)))
}

// confirm enters the newest code mailed to email.
func (e *env) confirm(email string) {
	e.t.Helper()
	code := e.lastSignupCode(email)
	if code == "" {
		e.t.Fatalf("no confirm code was mailed to %s", email)
	}
	e.must(204, e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": email, "code": code}))
}

// signupID is the newest request of email, read from the table (the list
// hides unconfirmed requests while mail works).
func (e *env) signupID(email string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM signup_requests WHERE email = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, email).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) listSignups(token, query string) []signup.Request {
	e.t.Helper()
	var page struct {
		Items []signup.Request `json:"items"`
	}
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/signups"+query, token, nil)).json(e.t, &page)
	return page.Items
}

func (e *env) onlySignup(token string) signup.Request {
	e.t.Helper()
	rs := e.listSignups(token, "?status=all")
	if len(rs) != 1 {
		e.t.Fatalf("%d sign-up requests, want 1: %+v", len(rs), rs)
	}
	return rs[0]
}

func (e *env) approve(id string) resp {
	e.t.Helper()
	return e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/approve", e.chen, nil)
}

// TestSignups_ModeGating: POST /signups and /signups/confirm work only in
// mode approval; invites work in invite and approval, never in closed.
func TestSignups_ModeGating(t *testing.T) {
	e := newEnv(t)
	n := 0
	invite := func() resp {
		n++
		return e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": fmt.Sprintf("new%d@example.com", n)})
	}
	accept := func() resp {
		return e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": strings.Repeat("A", 43), "name": "N", "username": "nobody"})
	}
	var refusal string
	for _, mode := range []string{"invite", "closed"} {
		e.setSignupMode(mode)
		for _, path := range []string{"/api/v1/signups", "/api/v1/signups/confirm"} {
			body := signupBody("a@example.com", "anna")
			if strings.HasSuffix(path, "confirm") {
				body = map[string]any{"email": "a@example.com", "code": "123456"}
			}
			r := e.do("POST", path, "", body)
			if r.status != 403 || r.errCode(t) != "signup_unavailable" {
				t.Fatalf("%s in mode %s: %d %s", path, mode, r.status, r.body)
			}
			if !strings.HasSuffix(path, "confirm") {
				if refusal == "" {
					refusal = string(r.body)
				} else if string(r.body) != refusal {
					t.Fatalf("the refusal differs between modes: %s vs %s", r.body, refusal)
				}
			}
		}
	}
	e.setSignupMode("closed")
	if r := invite(); r.status != 403 || r.errCode(t) != "signup_unavailable" {
		t.Fatalf("invite in closed: %d %s", r.status, r.body)
	}
	if r := accept(); r.status != 403 || r.errCode(t) != "signup_unavailable" {
		t.Fatalf("accept in closed: %d %s", r.status, r.body)
	}
	for _, mode := range []string{"invite", "approval"} {
		e.setSignupMode(mode)
		e.must(201, invite())
		if r := accept(); r.status != 401 || r.errCode(t) != "invalid_invite" {
			t.Fatalf("accept of a wrong token in %s: %d %s", mode, r.status, r.body)
		}
	}
	e.setSignupMode("approval")
	e.ask("a@example.com", "anna")
	e.confirm("a@example.com")
	id := e.signupID("a@example.com")
	e.setSignupMode("invite")
	if r := e.approve(id); r.status != 403 || r.errCode(t) != "signup_unavailable" {
		t.Fatalf("approve in mode invite: %d %s", r.status, r.body)
	}
}

// TestSignups_NoAccessBeforeApproval: a pending person has no user, no
// membership and no session, and cannot get or verify a login code.
func TestSignups_NoAccessBeforeApproval(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	users := e.countRows(`SELECT count(*) FROM users`)
	e.ask("Pending@Example.com", "pend")
	code := e.lastSignupCode("pending@example.com")
	e.confirm("pending@example.com")
	if n := e.countRows(`SELECT count(*) FROM users`); n != users {
		t.Fatalf("users %d, want %d: a sign-up request made a user", n, users)
	}
	if n := e.countRows(`SELECT count(*) FROM users WHERE username = 'pend' OR lower(email) = 'pending@example.com'`); n != 0 {
		t.Fatal("a user exists for the pending request")
	}
	if c := e.requestCode("pending@example.com"); c != "" {
		t.Fatal("a pending person got a login code")
	}
	r := e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": "pending@example.com", "code": code})
	if r.status != 401 || r.errCode(t) != "invalid_code" {
		t.Fatalf("verify with the confirm code: %d %s", r.status, r.body)
	}
	if _, ok := r.cookies()[e.names.Session]; ok {
		t.Fatal("verify set a session cookie")
	}
	sr := e.onlySignup(e.chen)
	if sr.Status != "pending" || !sr.EmailVerified || sr.Email != "pending@example.com" || sr.Username != "pend" || sr.Note != "hello" {
		t.Fatalf("request %+v", sr)
	}
}

// TestSignups_NoEnumeration: every POST /signups answer is the same, byte
// for byte, whether the email is new, a user, pending or blocked and
// whether the username is taken; each one takes at least the floor. What
// the mail shows: a taken username gets a confirm code like a new one (no
// signal about the username); an email that has an account gets a "you
// have an account" email, which only the owner of the address can read.
func TestSignups_NoEnumeration(t *testing.T) {
	const floor = 150 * time.Millisecond
	e := newEnvWith(t, envOpts{signup: signup.Options{Floor: floor}})
	e.setSignupMode("approval")
	e.ask("pending@example.com", "pend")
	e.confirm("pending@example.com")
	e.ask("blocked@example.com", "blk")
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+e.signupID("blocked@example.com")+"/reject", e.chen, map[string]any{"block": true}))
	rowsBefore := e.countRows(`SELECT count(*) FROM signup_requests`)
	mailsBefore := len(e.smail.all())

	cases := []struct{ name, email, username string }{
		{"a user's email", chenEmail, "fresh1"},
		{"a pending email", "pending@example.com", "fresh2"},
		{"a blocked email", "blocked@example.com", "fresh3"},
		{"a taken username", "other@example.com", "chen"},
		{"an agent's username", "other2@example.com", "claude"},
		{"new", "brand-new@example.com", "brandnew"},
	}
	var first resp
	for i, c := range cases {
		start := time.Now()
		r := e.do("POST", "/api/v1/signups", "", signupBody(c.email, c.username))
		if el := time.Since(start); el < floor {
			t.Fatalf("%s: answered in %v, under the %v floor", c.name, el, floor)
		}
		if r.status != 202 {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		if i == 0 {
			first = r
			continue
		}
		if string(r.body) != string(first.body) || headerShape(r.header) != headerShape(first.header) {
			t.Fatalf("%s: answer differs:\n%s %v\n%s %v", c.name, r.body, r.header, first.body, first.header)
		}
	}
	// The taken usernames are stored like the new address.
	if n := e.countRows(`SELECT count(*) FROM signup_requests`); n != rowsBefore+3 {
		t.Fatalf("rows %d, want %d (the two taken usernames and the new address)", n, rowsBefore+3)
	}
	got := map[string]string{}
	for _, m := range e.smail.all()[mailsBefore:] {
		got[m.to] += m.subject + ";"
	}
	want := map[string]string{
		chenEmail:               "Your Launchpad sign-up request;",
		"other@example.com":     "Your Launchpad sign-up code;",
		"other2@example.com":    "Your Launchpad sign-up code;",
		"brand-new@example.com": "Your Launchpad sign-up code;",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("mails %v\nwant  %v", got, want)
	}
	// The confirm route: an unknown email and a wrong code answer alike.
	a := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "nobody@example.com", "code": "000000"})
	b := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "brand-new@example.com", "code": wrongCode(e.lastSignupCode("brand-new@example.com"))})
	if a.status != 401 || string(a.body) != string(b.body) || a.status != b.status {
		t.Fatalf("confirm answers differ: %d %s / %d %s", a.status, a.body, b.status, b.body)
	}
	// The taken username shows at approval, as a 409.
	e.confirm("other@example.com")
	if r := e.approve(e.signupID("other@example.com")); r.status != 409 || !strings.Contains(string(r.body), "username") {
		t.Fatalf("approve a taken username: %d %s", r.status, r.body)
	}
}

// headerShape is the set of response header names and values (Date aside).
func headerShape(h http.Header) string {
	var parts []string
	for k, v := range h {
		if k == "Date" {
			continue
		}
		parts = append(parts, k+"="+strings.Join(v, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// TestSignups_VerifyBeforeShow: while mail works, a request shows to
// admins and can be approved only after the person proves the email;
// without mail it shows as unverified and can be approved.
func TestSignups_VerifyBeforeShow(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.ask("v@example.com", "vee")
	if got := e.listSignups(e.chen, "?status=all"); len(got) != 0 {
		t.Fatalf("an unconfirmed request shows to admins: %+v", got)
	}
	r := e.approve(e.signupID("v@example.com"))
	if r.status != 409 || !strings.Contains(string(r.body), "not confirmed") {
		t.Fatalf("approve an unconfirmed request: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM users WHERE username = 'vee'`); n != 0 {
		t.Fatal("a refused approval made a user")
	}
	e.confirm("v@example.com")
	if sr := e.onlySignup(e.chen); !sr.EmailVerified {
		t.Fatalf("after the confirm: %+v", sr)
	}
	e.must(200, e.approve(e.signupID("v@example.com")))

	// No mail: shows unverified, and an admin may approve it.
	e.smail.ready.Store(false)
	e.ask("nomail@example.com", "nomail")
	var found *signup.Request
	for _, sr := range e.listSignups(e.chen, "") {
		if sr.Email == "nomail@example.com" {
			found = &sr
		}
	}
	if found == nil || found.EmailVerified {
		t.Fatalf("the no-mail request: %+v", found)
	}
	if !found.ExpiresAt.After(time.Now().Add(29 * 24 * time.Hour)) {
		t.Fatalf("a no-mail request expires at %v, want 30 days", found.ExpiresAt)
	}
	e.must(200, e.approve(found.ID.String()))
}

// TestSignups_ConfirmCode: the code proves the email once; five wrong
// tries lock it; a new request gives a new code but the wrong tries add
// up (at most 10 for the request); an expired code fails; with no working
// mail no code is issued.
func TestSignups_ConfirmCode(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.ask("c@example.com", "cee")
	code := e.lastSignupCode("c@example.com")
	wrong := func(email, c string) {
		t.Helper()
		r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": email, "code": wrongCode(c)})
		if r.status != 401 || r.errCode(t) != "invalid_code" {
			t.Fatalf("wrong code: %d %s", r.status, r.body)
		}
	}
	for range 5 {
		wrong("c@example.com", code)
	}
	if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "c@example.com", "code": code}); r.status != 401 {
		t.Fatalf("the right code after 5 wrong tries: %d %s (want a locked code)", r.status, r.body)
	}
	// A second request mails a fresh code that works, once.
	e.ask("c@example.com", "cee")
	code2 := e.lastSignupCode("c@example.com")
	e.must(204, e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "C@Example.com ", "code": code2}))
	if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "c@example.com", "code": code2}); r.status != 401 {
		t.Fatalf("a used code: %d", r.status)
	}
	if !e.onlySignup(e.chen).EmailVerified {
		t.Fatal("the email is not verified")
	}

	// The wrong tries of a request add up across its codes.
	e.ask("g@example.com", "gee")
	for range 5 {
		wrong("g@example.com", e.lastSignupCode("g@example.com"))
	}
	e.ask("g@example.com", "gee")
	for range 5 {
		wrong("g@example.com", e.lastSignupCode("g@example.com"))
	}
	before := e.mailsTo("g@example.com", "sign-up code")
	e.ask("g@example.com", "gee")
	if after := e.mailsTo("g@example.com", "sign-up code"); after != before {
		t.Fatal("a request with 10 wrong tries got another code")
	}
	if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "g@example.com", "code": e.lastSignupCode("g@example.com")}); r.status != 401 {
		t.Fatalf("a code after 10 wrong tries in all: %d", r.status)
	}
	if n := e.countRows(`SELECT code_attempts_total FROM signup_requests WHERE email = 'g@example.com'`); n != 10 {
		t.Fatalf("code_attempts_total %d, want 10", n)
	}

	// An expired code fails.
	e.ask("x@example.com", "eks")
	cx := e.lastSignupCode("x@example.com")
	if _, err := e.pool.Exec(context.Background(), `UPDATE signup_requests SET code_expires_at = now() - interval '1 second', created_at = created_at - interval '1 hour' WHERE email = 'x@example.com'`); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "x@example.com", "code": cx}); r.status != 401 {
		t.Fatalf("an expired code: %d", r.status)
	}

	// No mail: no code, and the request is unverified.
	e.smail.ready.Store(false)
	n := len(e.smail.all())
	e.ask("nomail@example.com", "nomail")
	if len(e.smail.all()) != n {
		t.Fatal("an email went out with mail off")
	}
	if c := e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'nomail@example.com' AND code_hash IS NULL AND email_verified_at IS NULL`); c != 1 {
		t.Fatal("the no-mail request is not stored unverified with no code")
	}
}

// TestSignups_Caps: while mail works, requests that wait for their code
// have their own cap (UnconfirmedCap) and never fill the admin queue
// (PendingCap); a confirm into a full queue is the cap's 429 and keeps
// the code; without mail the requests count against PendingCap. Every
// address gets the same 429 over a cap.
func TestSignups_Caps(t *testing.T) {
	e := newEnvWith(t, envOpts{signup: signup.Options{PendingCap: 1, UnconfirmedCap: 2}})
	e.setSignupMode("approval")
	e.ask("u1@example.com", "uone")
	e.ask("u2@example.com", "utwo")
	full := e.do("POST", "/api/v1/signups", "", signupBody("u3@example.com", "uthree"))
	user := e.do("POST", "/api/v1/signups", "", signupBody(chenEmail, "ufour"))
	for _, r := range []resp{full, user} {
		if r.status != 429 || r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
			t.Fatalf("over the unconfirmed cap: %d %s %v", r.status, r.body, r.header)
		}
	}
	if string(full.body) != string(user.body) {
		t.Fatalf("the cap answer differs: %s / %s", full.body, user.body)
	}
	// Two unconfirmed requests did not fill the admin queue (cap 1).
	e.confirm("u1@example.com")
	// The queue is full now: the second confirm is 429 and its code lives on.
	code2 := e.lastSignupCode("u2@example.com")
	r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "u2@example.com", "code": code2})
	if r.status != 429 || r.errCode(t) != "rate_limited" {
		t.Fatalf("confirm into a full queue: %d %s", r.status, r.body)
	}
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+e.signupID("u1@example.com")+"/reject", e.chen, map[string]any{}))
	e.must(204, e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "u2@example.com", "code": code2}))
	// A confirmed request freed its unconfirmed place.
	e.ask("u3@example.com", "uthree")

	// Without mail a request goes straight to the admin queue (cap 1, full).
	e.smail.ready.Store(false)
	if r := e.do("POST", "/api/v1/signups", "", signupBody("u5@example.com", "ufive")); r.status != 429 {
		t.Fatalf("no mail, admin queue full: %d %s", r.status, r.body)
	}
}

// TestSignups_Expiry: while mail works, an unconfirmed request expires
// after 24 hours; a confirmed one 30 days after it was made. Expiry is
// lazy (the next read or write), frees the address, drops the code and
// stops an approval.
func TestSignups_Expiry(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.ask("old@example.com", "oldie")
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE expires_at BETWEEN now() + interval '23 hours 59 minutes' AND now() + interval '24 hours 1 minute'`); n != 1 {
		t.Fatal("an unconfirmed request does not expire in 24 hours")
	}
	// A second request two hours later does not keep it alive.
	if _, err := e.pool.Exec(context.Background(), `UPDATE signup_requests SET created_at = created_at - interval '2 hours', expires_at = expires_at - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	e.ask("old@example.com", "oldie")
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE expires_at <= now() + interval '22 hours 1 minute'`); n != 1 {
		t.Fatal("asking again moved the expiry")
	}
	e.confirm("old@example.com")
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE expires_at BETWEEN created_at + interval '29 days 23 hours' AND created_at + interval '30 days 1 minute'`); n != 1 {
		t.Fatal("a confirmed request does not expire 30 days after it was made")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE signup_requests SET created_at = now() - interval '31 days', expires_at = now() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	if got := e.listSignups(e.chen, ""); len(got) != 0 {
		t.Fatalf("pending list after expiry: %+v", got)
	}
	exp := e.listSignups(e.chen, "?status=expired")
	if len(exp) != 1 || exp[0].Status != "expired" {
		t.Fatalf("expired list: %+v", exp)
	}
	if r := e.approve(exp[0].ID.String()); r.status != 409 {
		t.Fatalf("approve an expired request: %d %s", r.status, r.body)
	}
	// An unconfirmed one past 24 hours expires and loses its code.
	e.ask("new@example.com", "newbie")
	if _, err := e.pool.Exec(context.Background(), `UPDATE signup_requests SET created_at = now() - interval '25 hours', expires_at = now() - interval '1 hour' WHERE email = 'new@example.com'`); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "new@example.com", "code": e.lastSignupCode("new@example.com")}); r.status != 401 {
		t.Fatalf("confirm an expired request: %d", r.status)
	}
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE status = 'expired' AND code_hash IS NULL`); n != 2 {
		t.Fatalf("%d expired rows without a code, want 2", n)
	}
	// The address may ask again.
	e.ask("old@example.com", "oldie")
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'old@example.com' AND status = 'pending'`); n != 1 {
		t.Fatal("no new pending request after expiry")
	}
}

// TestSignups_ApproveIdempotentAndRace: eight approvals at once make ONE
// user and ONE membership, all answer 200 with the same request, one audit
// line and one email; the person can then log in.
func TestSignups_ApproveIdempotentAndRace(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug, publicBase: "https://launchpad.test"})
	e.setSignupMode("approval")
	e.ask("dana@example.com", "dana")
	e.confirm("dana@example.com")
	id := e.onlySignup(e.chen).ID
	// Widen the race: an open transaction holds an uncommitted user with the
	// same username, so every approval that gets past its read of the
	// request waits at its user insert. With the row lock only one gets
	// that far; without it, all eight do, and seven then fail.
	ctx := context.Background()
	hold, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `INSERT INTO users (username, name) VALUES ('dana', 'Holder')`); err != nil {
		t.Fatal(err)
	}
	const n = 8
	res := make([]resp, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			res[i], errs[i] = e.request("POST", "/api/v1/orgs/orbit/signups/"+id.String()+"/approve", e.chen, nil)
		})
	}
	close(start)
	time.Sleep(500 * time.Millisecond)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if res[i].status != 200 {
			t.Fatalf("approval %d: %d %s", i, res[i].status, res[i].body)
		}
	}
	if c := e.countRows(`SELECT count(*) FROM users WHERE username = 'dana' AND email = 'dana@example.com' AND NOT is_agent`); c != 1 {
		t.Fatalf("%d users made, want 1", c)
	}
	if c := e.countRows(`SELECT count(*) FROM org_members m JOIN users u ON u.id = m.user_id WHERE u.username = 'dana' AND m.role = 'member' AND m.org_id = $1`, e.orgID); c != 1 {
		t.Fatalf("%d memberships, want 1", c)
	}
	var sr signup.Request
	res[0].json(t, &sr)
	if sr.Status != "approved" || sr.UserID == nil || sr.DecidedBy == nil || *sr.DecidedBy != e.user("chen").ID || sr.DecidedAt == nil {
		t.Fatalf("approved request %+v", sr)
	}
	if c := strings.Count(e.logs.String(), "audit: sign-up approved"); c != 1 {
		t.Fatalf("%d audit lines, want 1", c)
	}
	var approvals int
	for _, m := range e.smail.all() {
		if m.to == "dana@example.com" && strings.Contains(m.subject, "account is ready") {
			approvals++
			if !strings.Contains(m.text, "https://launchpad.test/login") {
				t.Fatalf("approval text: %s", m.text)
			}
		}
	}
	if approvals != 1 {
		t.Fatalf("%d approval emails, want 1", approvals)
	}
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id.String()+"/approve", e.chen, nil, "Idempotency-Key", "appr-1"))
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id.String()+"/approve", e.chen, nil, "Idempotency-Key", "appr-1"))
	if c := e.countRows(`SELECT count(*) FROM users WHERE username = 'dana'`); c != 1 {
		t.Fatal("a repeat approval made a user")
	}
	if _, r := e.loginAs("dana@example.com"); r.status != 200 {
		t.Fatalf("login after approval: %d", r.status)
	}
	if r := e.do("POST", "/api/v1/orgs/orbit/signups/"+id.String()+"/reject", e.chen, map[string]any{}); r.status != 409 {
		t.Fatalf("reject an approved request: %d %s", r.status, r.body)
	}
}

// TestSignups_ApproveConflicts: a username taken after the request is 409
// and makes nothing.
func TestSignups_ApproveConflicts(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.ask("eve@example.com", "eve")
	e.confirm("eve@example.com")
	id := e.onlySignup(e.chen).ID
	e.newHuman("eve", "someone-else@example.com", "member")
	r := e.approve(id.String())
	if r.status != 409 || !strings.Contains(string(r.body), "username") {
		t.Fatalf("taken username: %d %s", r.status, r.body)
	}
	if e.onlySignup(e.chen).Status != "pending" {
		t.Fatal("a failed approval changed the request")
	}
}

// TestSignups_RejectAndBlock: a reject keeps the reason; a block stops the
// email from asking again; without a block it may.
func TestSignups_RejectAndBlock(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.ask("spam@example.com", "spam")
	id := e.signupID("spam@example.com")
	r := e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/reject", e.chen, map[string]any{"reason": "not known", "block": false}))
	var sr signup.Request
	r.json(t, &sr)
	if sr.Status != "rejected" || sr.RejectReason != "not known" || sr.Blocked {
		t.Fatalf("rejected %+v", sr)
	}
	e.ask("spam@example.com", "spam")
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'spam@example.com'`); n != 2 {
		t.Fatalf("rows %d, want 2", n)
	}
	id2 := e.signupID("spam@example.com")
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id2+"/reject", e.chen, map[string]any{"block": true}))
	before := e.mailsTo("spam@example.com", "")
	e.ask("spam@example.com", "spam")
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'spam@example.com'`); n != 2 {
		t.Fatalf("a blocked email stored a request (%d rows)", n)
	}
	if e.mailsTo("spam@example.com", "") != before {
		t.Fatal("a blocked email got an email")
	}
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/reject", e.chen, map[string]any{"block": true}))
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'spam@example.com' AND blocked AND reject_reason = 'not known'`); n != 1 {
		t.Fatal("the later block changed the reason or did not apply")
	}
	if r := e.approve(id); r.status != 409 {
		t.Fatalf("approve a rejected request: %d", r.status)
	}
	if r := e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/reject", e.chen, map[string]any{"reason": strings.Repeat("x", 1001)}); r.status != 400 {
		t.Fatalf("a long reason: %d", r.status)
	}
}

// TestSignups_AdminRoutesAuth: the org routes need the org admin role
// (admin scope), and a cookie write the CSRF header.
func TestSignups_AdminRoutesAuth(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.ask("q@example.com", "queue")
	e.confirm("q@example.com")
	id := e.signupID("q@example.com")
	member := e.newHuman("mem", "mem@example.com", "member")
	memTok, _ := e.newToken(member.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read", "write"}})
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/orgs/orbit/signups", nil},
		{"POST", "/api/v1/orgs/orbit/signups/" + id + "/approve", nil},
		{"POST", "/api/v1/orgs/orbit/signups/" + id + "/reject", map[string]any{}},
		{"GET", "/api/v1/orgs/orbit/invites", nil},
		{"POST", "/api/v1/orgs/orbit/invites", map[string]any{"email": "z@example.com"}},
		{"DELETE", "/api/v1/orgs/orbit/invites/" + id, nil},
	}
	for _, rt := range routes {
		if r := e.do(rt.method, rt.path, "", rt.body); r.status != 401 {
			t.Fatalf("%s %s with no credentials: %d", rt.method, rt.path, r.status)
		}
		if r := e.do(rt.method, rt.path, memTok, rt.body); r.status != 403 {
			t.Fatalf("%s %s as a member: %d %s", rt.method, rt.path, r.status, r.body)
		}
		if r := e.do(rt.method, rt.path, e.claude, rt.body); r.status != 403 {
			t.Fatalf("%s %s as the agent: %d", rt.method, rt.path, r.status)
		}
	}
	sess, _ := e.loginAs(chenEmail)
	r := e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/approve", sess, nil, "X-CSRF-Token", "wrong")
	if r.status != 403 || r.errCode(t) != "csrf_failed" {
		t.Fatalf("approve with a bad CSRF header: %d %s", r.status, r.body)
	}
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/approve", sess, nil))
	if r := e.do("GET", "/api/v1/orgs/orbit/signups?status=bogus", e.chen, nil); r.status != 400 {
		t.Fatalf("bad status filter: %d", r.status)
	}
	if r := e.do("GET", "/api/v1/orgs/orbit/signups?limit=0", e.chen, nil); r.status != 400 {
		t.Fatalf("bad limit: %d", r.status)
	}
	for _, path := range []string{"/api/v1/signups", "/api/v1/signups/confirm", "/api/v1/invites/accept"} {
		if r := e.do("POST", path, "", map[string]any{}, "Content-Type", "text/plain"); r.status != 415 {
			t.Fatalf("%s as text/plain: %d", path, r.status)
		}
		if r := e.do("POST", path, "", map[string]any{}, "Idempotency-Key", "k"); r.status != 400 {
			t.Fatalf("%s with Idempotency-Key: %d", path, r.status)
		}
	}
}

// createInvite makes an invite as chen and returns the 201 body.
func (e *env) createInvite(email, role string) signup.CreatedInvite {
	e.t.Helper()
	body := map[string]any{"email": email}
	if role != "" {
		body["role"] = role
	}
	var ci signup.CreatedInvite
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, body)).json(e.t, &ci)
	return ci
}

// TestInvites_EmailedLinkHidesToken: while mail works the 201 body holds
// no token at all (no secret, no url): only the address gets the link.
// Accepting it makes an account whose email counts as proved.
func TestInvites_EmailedLinkHidesToken(t *testing.T) {
	e := newEnvWith(t, envOpts{publicBase: "https://launchpad.test"})
	r := e.must(201, e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": "mailed@example.com"}))
	var raw map[string]json.RawMessage
	r.json(t, &raw)
	if _, ok := raw["secret"]; ok {
		t.Fatalf("an emailed invite returned its secret: %s", r.body)
	}
	if _, ok := raw["url"]; ok {
		t.Fatalf("an emailed invite returned its url: %s", r.body)
	}
	var ci signup.CreatedInvite
	r.json(t, &ci)
	if !ci.Emailed || !ci.Invite.Emailed {
		t.Fatalf("created %s", r.body)
	}
	tok := e.mailedInviteToken("mailed@example.com")
	if tok == "" || strings.Contains(string(r.body), tok) {
		t.Fatal("no token in the email, or the token is in the body")
	}
	e.must(201, e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": tok, "name": "Mailed", "username": "mailed"}))
	if n := e.countRows(`SELECT count(*) FROM users WHERE username = 'mailed' AND NOT email_unproven`); n != 1 {
		t.Fatal("an account from an emailed link is marked email_unproven")
	}
}

// TestInvites_CopiedLinkSessionDiesAtFirstLogin: without mail the admin
// gets the link; an account made with it is email_unproven, and its first
// email-code login revokes every session and token it had (also the ones
// it minted), so whoever used the copied link keeps nothing.
func TestInvites_CopiedLinkSessionDiesAtFirstLogin(t *testing.T) {
	e := newEnv(t)
	e.smail.ready.Store(false)
	ci := e.createInvite("victim@example.com", "admin")
	if ci.Secret == "" || ci.Emailed {
		t.Fatalf("a copied invite: %+v", ci)
	}
	r := e.must(201, e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "Victim", "username": "victim"}))
	held := e.asSession(r.cookies()[e.names.Session].Value)
	if n := e.countRows(`SELECT count(*) FROM users WHERE username = 'victim' AND email_unproven`); n != 1 {
		t.Fatal("an account from a copied link is not email_unproven")
	}
	// The holder mints a token with the session, and one for an agent.
	var tok struct {
		Secret string `json:"secret"`
	}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", held, map[string]any{"name": "kept", "scopes": []string{"read"}, "expiresInDays": 30})).json(t, &tok)
	var agentTok struct {
		Secret string `json:"secret"`
	}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", held, map[string]any{"name": "agent", "scopes": []string{"read"}, "user": "claude", "expiresInDays": 30})).json(t, &agentTok)
	e.must(200, e.do("GET", "/api/v1/me", tok.Secret, nil))
	// The owner of the address logs in with an email code.
	own, _ := e.loginAs("victim@example.com")
	for name, s := range map[string]string{"the held session": held, "its token": tok.Secret, "its agent token": agentTok.Secret} {
		if r := e.do("GET", "/api/v1/me", s, nil); r.status != 401 {
			t.Fatalf("%s still works after the owner's first login: %d", name, r.status)
		}
	}
	e.must(200, e.do("GET", "/api/v1/me", own, nil))
	if n := e.countRows(`SELECT count(*) FROM users WHERE username = 'victim' AND NOT email_unproven`); n != 1 {
		t.Fatal("the flag is not cleared")
	}
	// A second login revokes nothing.
	e.loginAs("victim@example.com")
	e.must(200, e.do("GET", "/api/v1/me", own, nil))
}

// TestInvites_CreateAcceptSingleUse: (copied links, mail off) the token is
// in the 201 body once and never in a list; accepting makes the user, the
// membership with the invite's role and a working session; the token works
// once; wrong, malformed, revoked and expired tokens get one fixed answer.
func TestInvites_CreateAcceptSingleUse(t *testing.T) {
	e := newEnvWith(t, envOpts{publicBase: "https://launchpad.test"})
	e.smail.ready.Store(false)
	ci := e.createInvite("New.Admin@Example.com", "admin")
	if ci.Secret == "" || ci.URL != "https://launchpad.test/invite#"+ci.Secret || ci.Invite.Role != "admin" ||
		ci.Invite.Status != "pending" || ci.Invite.Email != "new.admin@example.com" || ci.Emailed {
		t.Fatalf("created invite %+v", ci)
	}
	if !ci.Invite.ExpiresAt.After(time.Now().Add(7*24*time.Hour-time.Minute)) || ci.Invite.ExpiresAt.After(time.Now().Add(7*24*time.Hour+time.Minute)) {
		t.Fatalf("expires %v, want 7 days", ci.Invite.ExpiresAt)
	}
	if len(e.smail.all()) != 0 {
		t.Fatal("an email went out with mail off")
	}
	list := e.must(200, e.do("GET", "/api/v1/orgs/orbit/invites", e.chen, nil))
	if strings.Contains(string(list.body), ci.Secret) || strings.Contains(string(list.body), "token") {
		t.Fatalf("the list holds the token: %s", list.body)
	}
	wrong := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": strings.Repeat("B", 43), "name": "New", "username": "newadmin"})
	malformed := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": "short", "name": "New", "username": "newadmin"})
	for _, r := range []resp{wrong, malformed} {
		if r.status != 401 || r.errCode(t) != "invalid_invite" {
			t.Fatalf("bad token: %d %s", r.status, r.body)
		}
	}
	if r := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret}); r.status != 400 {
		t.Fatalf("accept with no username: %d %s", r.status, r.body)
	}
	r := e.must(201, e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "New Admin", "username": "NewAdmin"}))
	var out signup.AcceptResponse
	r.json(t, &out)
	if out.Status != "created" || out.Session == nil || out.Session.User.Username != "newadmin" || out.Org.Role != "admin" || out.Session.CSRFToken == "" {
		t.Fatalf("accept body %s", r.body)
	}
	c, ok := r.cookies()[e.names.Session]
	if !ok || c.Value == "" || !c.HttpOnly || !c.Secure {
		t.Fatalf("no session cookie: %v", r.header)
	}
	sess := e.asSession(c.Value)
	me := e.must(200, e.do("GET", "/api/v1/me", sess, nil))
	if !strings.Contains(string(me.body), `"newadmin"`) {
		t.Fatalf("me %s", me.body)
	}
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/invites", sess, nil))
	if n := e.countRows(`SELECT count(*) FROM org_invites WHERE accepted_at IS NOT NULL AND accepted_by = (SELECT id FROM users WHERE username = 'newadmin')`); n != 1 {
		t.Fatal("the invite does not record who accepted it")
	}
	again := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "X", "username": "other"})
	if again.status != 401 || string(again.body) != string(wrong.body) {
		t.Fatalf("a used token: %d %s (want the wrong-token answer %s)", again.status, again.body, wrong.body)
	}
	rv := e.createInvite("rev@example.com", "")
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+rv.Invite.ID.String(), e.chen, nil))
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+rv.Invite.ID.String(), e.chen, nil))
	if r := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": rv.Secret, "name": "R", "username": "rev"}); string(r.body) != string(wrong.body) || r.status != 401 {
		t.Fatalf("a revoked token: %d %s", r.status, r.body)
	}
	ex := e.createInvite("exp@example.com", "")
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_invites SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE id = $1`, ex.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ex.Secret, "name": "E", "username": "exp"}); string(r.body) != string(wrong.body) || r.status != 401 {
		t.Fatalf("an expired token: %d %s", r.status, r.body)
	}
	// An admin who revokes an invite after its expiry sees "revoked".
	late := e.createInvite("late@example.com", "")
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_invites SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE id = $1`, late.Invite.ID); err != nil {
		t.Fatal(err)
	}
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+late.Invite.ID.String(), e.chen, nil))
	ex2 := e.createInvite("exp@example.com", "")
	var page struct {
		Items []signup.Invite `json:"items"`
	}
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/invites", e.chen, nil)).json(t, &page)
	got := map[string]string{}
	for _, iv := range page.Items {
		got[iv.ID.String()] = iv.Status
	}
	if got[ci.Invite.ID.String()] != "accepted" || got[rv.Invite.ID.String()] != "revoked" || got[ex.Invite.ID.String()] != "expired" ||
		got[ex2.Invite.ID.String()] != "pending" || got[late.Invite.ID.String()] != "revoked" {
		t.Fatalf("statuses %v", got)
	}
	if r := e.do("DELETE", "/api/v1/orgs/orbit/invites/"+ci.Invite.ID.String(), e.chen, nil); r.status != 409 {
		t.Fatalf("revoke an accepted invite: %d", r.status)
	}
}

// TestInvites_ConcurrentAccept: eight accepts of one token: exactly one
// 201 and one user.
func TestInvites_ConcurrentAccept(t *testing.T) {
	e := newEnv(t)
	e.smail.ready.Store(false)
	ci := e.createInvite("race@example.com", "")
	const n = 8
	statuses := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			r, err := e.request("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "Racer", "username": "racer" + string(rune('a'+i))})
			statuses[i], errs[i] = r.status, err
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch statuses[i] {
		case 201:
			ok++
		case 401:
		default:
			t.Fatalf("accept %d: %d", i, statuses[i])
		}
	}
	if ok != 1 {
		t.Fatalf("%d successes, want 1 (%v)", ok, statuses)
	}
	if c := e.countRows(`SELECT count(*) FROM users WHERE email = 'race@example.com'`); c != 1 {
		t.Fatalf("%d users, want 1", c)
	}
}

// TestInvites_EmailTakenDuringAccept: an email that becomes a user's while
// the accept runs is 409 conflict (not 500), and the invite stays usable.
func TestInvites_EmailTakenDuringAccept(t *testing.T) {
	e := newEnv(t)
	e.smail.ready.Store(false)
	ci := e.createInvite("taken@example.com", "")
	ctx := context.Background()
	hold, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `INSERT INTO users (username, name, email) VALUES ('holder', 'Holder', 'taken@example.com')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan resp, 1)
	go func() {
		r, _ := e.request("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "T", "username": "taker"})
		done <- r
	}()
	time.Sleep(400 * time.Millisecond)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.status != 409 || r.errCode(t) != "conflict" {
		t.Fatalf("accept while the email became a user's: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM org_invites WHERE accepted_at IS NULL AND id = $1`, ci.Invite.ID); n != 1 {
		t.Fatal("the failed accept used up the invite")
	}
}

// TestInvites_ExistingUser: an invite to an address that is a user already
// adds the membership and starts NO session; duplicates are refused; an
// agent's or a deactivated user's email cannot be invited.
func TestInvites_ExistingUser(t *testing.T) {
	e := newEnv(t)
	e.smail.ready.Store(false)
	other := e.newOrg("other", "OTH")
	u := e.newUser("outsider")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET email = 'outsider@example.com' WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	e.addMember(other, u.ID, "admin")
	ci := e.createInvite("outsider@example.com", "")
	if r := e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": "outsider@example.com"}); r.status != 409 {
		t.Fatalf("a second open invite: %d", r.status)
	}
	if r := e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": chenEmail}); r.status != 409 {
		t.Fatalf("an invite for a member: %d", r.status)
	}
	if r := e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": "x@example.com", "role": "owner"}); r.status != 400 {
		t.Fatalf("a bad role: %d", r.status)
	}
	r := e.must(200, e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret}))
	var out signup.AcceptResponse
	r.json(t, &out)
	if out.Status != "joined" || out.Session != nil || out.Org.Slug != "orbit" || out.Org.Role != "member" {
		t.Fatalf("joined body %s", r.body)
	}
	if _, ok := r.cookies()[e.names.Session]; ok {
		t.Fatal("an invite for an existing user started a session")
	}
	if strings.Contains(string(r.body), "outsider") || strings.Contains(string(r.body), "other") {
		t.Fatalf("the joined body tells about the user: %s", r.body)
	}
	if c := e.countRows(`SELECT count(*) FROM org_members WHERE user_id = $1`, u.ID); c != 2 {
		t.Fatalf("memberships %d, want 2", c)
	}
	d := e.newUser("gone")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET email = 'gone@example.com' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	dci := e.createInvite("gone@example.com", "")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET deactivated_at = now() WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": dci.Secret}); r.status != 401 || r.errCode(t) != "invalid_invite" {
		t.Fatalf("deactivated: %d %s", r.status, r.body)
	}
	// Agent and deactivated emails get one fixed text that names neither.
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET email = 'agent@example.com' WHERE username = 'claude'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM org_members WHERE org_id = $1 AND user_id = (SELECT id FROM users WHERE username = 'claude')`, e.orgID); err != nil {
		t.Fatal(err)
	}
	agent := e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": "agent@example.com"})
	if agent.status != 409 || strings.Contains(string(agent.body), "deactivated") || !strings.Contains(string(agent.body), "cannot be invited") {
		t.Fatalf("an agent's email: %d %s", agent.status, agent.body)
	}
}

// TestSignups_ClientLimits: the per-client limit and the proxies
// (decisions-T12 #9). Behind a trusted proxy each client is limited on its
// own and one client cannot lock out another; a forged X-Real-IP or
// X-Forwarded-For from a peer that is not a trusted proxy is ignored; with
// no trusted proxy configured, a request through a proxy gets no
// per-client key (so one shared address never locks out everyone) and a
// direct one does; a client that its own limit refuses does not use up the
// global bucket.
func TestSignups_ClientLimits(t *testing.T) {
	loop := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	n := 0
	ask := func(e *env, headers ...string) int {
		n++
		r := e.do("POST", "/api/v1/signups", "", signupBody(fmt.Sprintf("c%d@example.com", n), fmt.Sprintf("cl%d", n)), headers...)
		return r.status
	}
	t.Run("trusted proxy: clients apart", func(t *testing.T) {
		e := newEnvWith(t, envOpts{trustedProxies: loop, signup: signup.Options{SignupPerClientPerHour: 2}})
		e.setSignupMode("approval")
		for i := range 2 {
			if s := ask(e, "X-Forwarded-For", "203.0.113.1"); s != 202 {
				t.Fatalf("client A request %d: %d", i, s)
			}
		}
		if s := ask(e, "X-Forwarded-For", "203.0.113.1"); s != 429 {
			t.Fatalf("client A over its limit: %d", s)
		}
		// Another client behind the same proxy is not locked out.
		for _, hdr := range [][]string{{"X-Forwarded-For", "198.51.100.7, 203.0.113.9"}, {"X-Real-IP", "198.51.100.8"}} {
			if s := ask(e, hdr...); s != 202 {
				t.Fatalf("another client (%v): %d", hdr, s)
			}
		}
		// A spoofed left part of the chain does not change the key: the
		// right-most untrusted hop is the client.
		if s := ask(e, "X-Forwarded-For", "192.0.2.55, 203.0.113.1"); s != 429 {
			t.Fatalf("client A with a spoofed chain: %d", s)
		}
	})
	t.Run("untrusted peer: forged headers ignored", func(t *testing.T) {
		e := newEnvWith(t, envOpts{trustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, signup: signup.Options{SignupPerClientPerHour: 2}})
		e.setSignupMode("approval")
		for i, ip := range []string{"203.0.113.1", "203.0.113.2"} {
			if s := ask(e, "X-Real-IP", ip, "X-Forwarded-For", ip); s != 202 {
				t.Fatalf("request %d: %d", i, s)
			}
		}
		if s := ask(e, "X-Real-IP", "203.0.113.3", "X-Forwarded-For", "203.0.113.3"); s != 429 {
			t.Fatalf("a forged header escaped the peer's limit: %d", s)
		}
	})
	t.Run("no trusted proxy: a shared address is not limited per client", func(t *testing.T) {
		e := newEnvWith(t, envOpts{signup: signup.Options{SignupPerClientPerHour: 2}})
		e.setSignupMode("approval")
		for i := range 5 {
			if s := ask(e, "X-Forwarded-For", "203.0.113.1", "X-Real-IP", "172.18.0.1"); s != 202 {
				t.Fatalf("proxied request %d: %d (one shared address locked out everyone)", i, s)
			}
		}
		for i := range 2 {
			if s := ask(e); s != 202 {
				t.Fatalf("direct request %d: %d", i, s)
			}
		}
		if s := ask(e); s != 429 {
			t.Fatalf("a direct client over its limit: %d", s)
		}
	})
	acc := func(e *env, headers ...string) int {
		r := e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": strings.Repeat("D", 43), "name": "N", "username": "nn"}, headers...)
		return r.status
	}
	t.Run("accept, no trusted proxy: proxied callers are not limited per client", func(t *testing.T) {
		e := newEnvWith(t, envOpts{signup: signup.Options{AcceptPerClientPerHour: 2}})
		for i := range 6 {
			if s := acc(e, "X-Forwarded-For", "203.0.113.1", "X-Real-IP", "172.18.0.1"); s != 401 {
				t.Fatalf("proxied accept %d: %d (one shared address locked out every invite)", i, s)
			}
		}
		for range 2 {
			acc(e)
		}
		if s := acc(e); s != 429 {
			t.Fatalf("a direct caller over its accept limit: %d", s)
		}
	})
	t.Run("accept, trusted proxy: clients apart, a refused one does not drain the global bucket", func(t *testing.T) {
		e := newEnvWith(t, envOpts{trustedProxies: loop, signup: signup.Options{AcceptPerClientPerHour: 1, AcceptPerMinute: 1, AcceptBurst: 2}})
		if s := acc(e, "X-Forwarded-For", "203.0.113.1"); s != 401 {
			t.Fatalf("first: %d", s)
		}
		for range 5 {
			if s := acc(e, "X-Forwarded-For", "203.0.113.1"); s != 429 {
				t.Fatalf("client A over its limit: %d", s)
			}
		}
		if s := acc(e, "X-Forwarded-For", "198.51.100.7"); s != 401 {
			t.Fatalf("client B after client A was refused: %d (A drained the global bucket)", s)
		}
	})
	t.Run("confirm: per email", func(t *testing.T) {
		e := newEnvWith(t, envOpts{signup: signup.Options{ConfirmPerEmailPerHour: 2}})
		e.setSignupMode("approval")
		e.ask("victim@example.com", "victim")
		c := e.lastSignupCode("victim@example.com")
		for range 2 {
			e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "victim@example.com", "code": wrongCode(c)})
		}
		r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "victim@example.com", "code": c})
		if r.status != 429 || r.header.Get("Retry-After") == "" {
			t.Fatalf("a third confirm for one email in an hour: %d %s", r.status, r.body)
		}
		if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "other@example.com", "code": "123456"}); r.status != 401 {
			t.Fatalf("another email: %d", r.status)
		}
		if n := e.countRows(`SELECT code_attempts_total FROM signup_requests WHERE email = 'victim@example.com'`); n != 2 {
			t.Fatalf("attempts %d: the refused confirm counted", n)
		}
	})
	t.Run("a refused client does not drain the global bucket", func(t *testing.T) {
		e := newEnvWith(t, envOpts{trustedProxies: loop, signup: signup.Options{SignupPerClientPerHour: 1, SignupPerMinute: 1, SignupBurst: 2}})
		e.setSignupMode("approval")
		if s := ask(e, "X-Forwarded-For", "203.0.113.1"); s != 202 {
			t.Fatalf("first: %d", s)
		}
		for range 5 {
			if s := ask(e, "X-Forwarded-For", "203.0.113.1"); s != 429 {
				t.Fatalf("refused client: %d", s)
			}
		}
		if s := ask(e, "X-Forwarded-For", "198.51.100.7"); s != 202 {
			t.Fatalf("another client after a refused one: %d (the refusals used up the global bucket)", s)
		}
	})
}

// TestSignups_RateLimits: the per-email limit and the global accept bucket
// answer 429 with Retry-After.
func TestSignups_RateLimits(t *testing.T) {
	e := newEnvWith(t, envOpts{signup: signup.Options{AcceptPerMinute: 1, AcceptBurst: 1, SignupPerEmailPerHour: 1}})
	e.setSignupMode("approval")
	e.ask("r1@example.com", "rone")
	r := e.do("POST", "/api/v1/signups", "", signupBody("r1@example.com", "rone"))
	if r.status != 429 || r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("per-email: %d %s", r.status, r.body)
	}
	e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": strings.Repeat("C", 43), "name": "N", "username": "nn"})
	r = e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": strings.Repeat("C", 43), "name": "N", "username": "nn"})
	if r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("accept bucket: %d %s", r.status, r.body)
	}
}

// TestPlatformAdmins: list, add (by username or email), remove; the last
// active platform admin cannot be removed, also by two removals at once;
// only a platform admin's unpinned admin token or session gets in.
func TestPlatformAdmins(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	ctx := context.Background()
	if _, err := setup.GrantPlatformAdmin(ctx, e.pool, "chen"); err != nil {
		t.Fatal(err)
	}
	chen := e.user("chen")
	admin, _ := e.newToken(chen.ID, tokenOpts{})
	ann := e.newHuman("ann", "ann@example.com", "member")
	annTok, _ := e.newToken(ann.ID, tokenOpts{scopes: []string{"read", "write", "admin"}})

	for _, tok := range []string{"", annTok, e.chen /* org-pinned */} {
		if r := e.do("GET", "/api/v1/platform/admins", tok, nil); r.status != 401 && r.status != 403 {
			t.Fatalf("GET as a non platform admin: %d", r.status)
		}
		if r := e.do("POST", "/api/v1/platform/admins", tok, map[string]any{"user": "ann"}); r.status != 401 && r.status != 403 {
			t.Fatalf("POST as a non platform admin: %d", r.status)
		}
	}
	var page struct {
		Items []setup.PlatformAdmin `json:"items"`
	}
	e.must(200, e.do("GET", "/api/v1/platform/admins", admin, nil)).json(t, &page)
	if len(page.Items) != 1 || page.Items[0].Username != "chen" {
		t.Fatalf("admins %+v", page.Items)
	}
	if r := e.do("DELETE", "/api/v1/platform/admins/"+chen.ID.String(), admin, nil); r.status != 409 {
		t.Fatalf("remove the last admin: %d %s", r.status, r.body)
	}
	e.must(201, e.do("POST", "/api/v1/platform/admins", admin, map[string]any{"user": "ANN@example.com"}))
	e.must(200, e.do("POST", "/api/v1/platform/admins", admin, map[string]any{"user": "ann"}))
	if r := e.do("POST", "/api/v1/platform/admins", admin, map[string]any{"user": "claude"}); r.status != 400 {
		t.Fatalf("an agent: %d", r.status)
	}
	if r := e.do("POST", "/api/v1/platform/admins", admin, map[string]any{"user": "nobody"}); r.status != 404 {
		t.Fatalf("an unknown user: %d", r.status)
	}
	annAdmin, _ := e.newToken(ann.ID, tokenOpts{})
	var wg sync.WaitGroup
	res := make([]resp, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		res[0], errs[0] = e.request("DELETE", "/api/v1/platform/admins/"+ann.ID.String(), admin, nil)
	})
	wg.Go(func() {
		<-start
		res[1], errs[1] = e.request("DELETE", "/api/v1/platform/admins/"+chen.ID.String(), annAdmin, nil)
	})
	close(start)
	wg.Wait()
	ok := 0
	for i := range 2 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch res[i].status {
		case 204:
			ok++
		case 409, 403: // 403: the loser lost its own flag first
		default:
			t.Fatalf("removal %d: %d %s", i, res[i].status, res[i].body)
		}
	}
	if ok != 1 {
		t.Fatalf("%d removals succeeded, want 1", ok)
	}
	if n := e.countRows(`SELECT count(*) FROM users WHERE is_platform_admin`); n != 1 {
		t.Fatalf("%d platform admins left, want 1", n)
	}
	if !strings.Contains(e.logs.String(), "audit: platform admin removed") || !strings.Contains(e.logs.String(), "audit: platform admin added") {
		t.Fatal("no audit lines")
	}
	var leftID string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM users WHERE is_platform_admin`).Scan(&leftID); err != nil {
		t.Fatal(err)
	}
	leftTok := admin
	if leftID == ann.ID.String() {
		leftTok = annAdmin
	}
	gone := e.newUser("gone-admin")
	if _, err := e.pool.Exec(ctx, `UPDATE users SET is_platform_admin = true, deactivated_at = now() WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do("DELETE", "/api/v1/platform/admins/"+leftID, leftTok, nil); r.status != 409 {
		t.Fatalf("remove the last ACTIVE admin beside a deactivated one: %d %s", r.status, r.body)
	}
	e.must(204, e.do("DELETE", "/api/v1/platform/admins/"+gone.ID.String(), leftTok, nil))
	if r := e.do("DELETE", "/api/v1/platform/admins/"+gone.ID.String(), leftTok, nil); r.status != 404 {
		t.Fatalf("remove a non admin: %d", r.status)
	}
}

// containsSecret: s holds sec as a whole word (no letter or digit next to
// it), so a 6-digit code never matches inside a longer number or a hex id.
func containsSecret(s, sec string) bool {
	return regexp.MustCompile(`(^|[^0-9A-Za-z])` + regexp.QuoteMeta(sec) + `([^0-9A-Za-z]|$)`).MatchString(s)
}

// TestSignups_NoSecretsInLogsEventsOrIdempotency: confirm codes, invite
// tokens and sessions are in no log line, event, idempotency row or stored
// row (only their hashes are stored).
func TestSignups_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	e.setSignupMode("approval")
	e.ask("s1@example.com", "sone")
	code := e.lastSignupCode("s1@example.com")
	e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "s1@example.com", "code": wrongCode(code)})
	e.confirm("s1@example.com")
	id := e.onlySignup(e.chen).ID.String()
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+id+"/approve", e.chen, nil, "Idempotency-Key", "sec-appr"))
	// An emailed invite (the token only in the email) and a copied one.
	e.createInvite("s5@example.com", "")
	mailed := e.mailedInviteToken("s5@example.com")
	e.smail.ready.Store(false)
	ci := e.createInvite("s2@example.com", "")
	e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, map[string]any{"email": "s3@example.com"}, "Idempotency-Key", "sec-inv") // refused
	r := e.must(201, e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "S2", "username": "stwo"}))
	sess := r.cookies()[e.names.Session].Value
	e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": ci.Secret, "name": "S2", "username": "stwo"})
	e.must(201, e.do("POST", "/api/v1/invites/accept", "", map[string]any{"token": mailed, "name": "S5", "username": "sfive"}))
	ci2 := e.createInvite("s4@example.com", "")
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+ci2.Invite.ID.String(), e.chen, nil))
	e.createIssue(e.chen, map[string]any{"title": "x"}) // an event, so the event check is not vacuous

	secrets := []string{code, ci.Secret, ci2.Secret, mailed, sess}
	logs := e.logs.String()
	if !strings.Contains(logs, "audit: invite created") || !strings.Contains(logs, "/api/v1/signups") {
		t.Fatalf("the log capture saw no sign-up lines; the check would be vacuous:\n%s", logs)
	}
	var events, idem, rows string
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(type || ' ' || payload::text, E'\n'), '') FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(key || ' ' || path || ' ' || coalesce(convert_from(response_body, 'UTF8'), ''), E'\n'), '') FROM idempotency_keys`).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT (SELECT coalesce(string_agg(row_to_json(s)::text, E'\n'), '') FROM signup_requests s) || (SELECT coalesce(string_agg(row_to_json(i)::text, E'\n'), '') FROM org_invites i)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if events == "" || !strings.Contains(idem, "sec-appr") {
		t.Fatalf("expected rows are missing; the check would be vacuous:\nevents %s\nidem %s", events, idem)
	}
	if strings.Contains(idem, "sec-inv") {
		t.Fatal("the refused Idempotency-Key was stored")
	}
	for _, sec := range secrets {
		if sec == "" {
			t.Fatal("a secret is empty; the check would be vacuous")
		}
		for where, s := range map[string]string{"the log": logs, "an event": events, "an idempotency row": idem, "a stored row": rows} {
			if containsSecret(s, sec) {
				t.Fatalf("%s holds a secret (%d characters)", where, len(sec))
			}
		}
	}
}

// TestSignups_ApprovedUnprovenEmail: a request approved while mail was off
// (its email never proved) makes an email_unproven account; its first
// email-code login revokes every token and session it had before. A
// confirmed request makes an ordinary account.
func TestSignups_ApprovedUnprovenEmail(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.smail.ready.Store(false)
	e.ask("unproven@example.com", "unproven")
	e.must(200, e.approve(e.signupID("unproven@example.com")))
	u := e.user("unproven")
	if !u.EmailUnproven {
		t.Fatal("an account approved without a proved email is not email_unproven")
	}
	earlier, _ := e.newToken(u.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	e.must(200, e.do("GET", "/api/v1/me", earlier, nil))
	e.smail.ready.Store(true)
	own, _ := e.loginAs("unproven@example.com")
	if r := e.do("GET", "/api/v1/me", earlier, nil); r.status != 401 {
		t.Fatalf("a token from before the first login still works: %d", r.status)
	}
	e.must(200, e.do("GET", "/api/v1/me", own, nil))
	if e.user("unproven").EmailUnproven {
		t.Fatal("the flag is not cleared")
	}
	e.ask("proved@example.com", "proved")
	e.confirm("proved@example.com")
	e.must(200, e.approve(e.signupID("proved@example.com")))
	if e.user("proved").EmailUnproven {
		t.Fatal("an account from a confirmed request is email_unproven")
	}
}

// TestSignups_MailOffFloodThenMailOn: 200 requests made while mail is off
// fill the admin queue. When mail works again they stay visible (no code,
// emailVerified false), so an admin can reject or approve them; new
// requests go to the unconfirmed cap and are not blocked; a confirm into
// the full queue is 429 until an admin frees a place.
func TestSignups_MailOffFloodThenMailOn(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.smail.ready.Store(false)
	for i := range signup.DefaultPendingCap {
		e.ask(fmt.Sprintf("flood%d@example.com", i), fmt.Sprintf("flood%d", i))
	}
	if r := e.do("POST", "/api/v1/signups", "", signupBody("late@example.com", "late")); r.status != 429 {
		t.Fatalf("request 201 with mail off: %d", r.status)
	}
	e.smail.ready.Store(true)
	shown := e.listSignups(e.chen, "?limit=500")
	if len(shown) != signup.DefaultPendingCap {
		t.Fatalf("%d requests show after mail came back, want %d", len(shown), signup.DefaultPendingCap)
	}
	for _, sr := range shown[:2] {
		if sr.EmailVerified {
			t.Fatalf("a no-mail request shows as verified: %+v", sr)
		}
	}
	e.ask("late@example.com", "late") // the unconfirmed cap, not the full queue
	code := e.lastSignupCode("late@example.com")
	r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "late@example.com", "code": code})
	if r.status != 429 {
		t.Fatalf("confirm into a full queue: %d %s", r.status, r.body)
	}
	e.must(200, e.do("POST", "/api/v1/orgs/orbit/signups/"+shown[0].ID.String()+"/reject", e.chen, map[string]any{"block": true}))
	e.must(200, e.approve(shown[1].ID.String()))
	e.must(204, e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "late@example.com", "code": code}))
}

// TestSignups_PurgeUnconfirmed: the admin's way out of a flood deletes only
// the requests that wait for their code.
func TestSignups_PurgeUnconfirmed(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	e.setSignupMode("approval")
	for i := range 3 {
		e.ask(fmt.Sprintf("w%d@example.com", i), fmt.Sprintf("wait%d", i))
	}
	e.ask("ok@example.com", "okay")
	e.confirm("ok@example.com")
	e.smail.ready.Store(false)
	e.ask("nomail@example.com", "nomail")
	e.smail.ready.Store(true)
	member := e.newHuman("mem", "mem@example.com", "member")
	memTok, _ := e.newToken(member.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read", "write"}})
	if r := e.do("DELETE", "/api/v1/orgs/orbit/signups/unconfirmed", memTok, nil); r.status != 403 {
		t.Fatalf("purge as a member: %d", r.status)
	}
	var out signup.PurgeResult
	e.must(200, e.do("DELETE", "/api/v1/orgs/orbit/signups/unconfirmed", e.chen, nil)).json(t, &out)
	if out.Deleted != 3 {
		t.Fatalf("deleted %d, want 3", out.Deleted)
	}
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE email IN ('ok@example.com', 'nomail@example.com')`); n != 2 {
		t.Fatal("the purge took a confirmed or a no-mail request")
	}
	if !strings.Contains(e.logs.String(), "audit: unconfirmed sign-up requests purged") {
		t.Fatal("no audit line")
	}
}

// TestSignups_KeyTableFlood: a flood of random emails cannot fill the
// per-email tables (decisions-T12 #9). The global bucket refuses most of
// the flood, and a refused NEW key is not recorded, so once the bucket
// refills a real new address gets through, on both routes.
func TestSignups_KeyTableFlood(t *testing.T) {
	clk := newClock()
	const table = 50
	e := newEnvWith(t, envOpts{signup: signup.Options{Now: clk.Now, KeyTableSize: table,
		SignupPerMinute: 60, SignupBurst: 20, ConfirmPerMinute: 60, ConfirmBurst: 20,
		SignupPerEmailPerHour: 3, ConfirmPerEmailPerHour: 5}})
	e.setSignupMode("approval")
	refused := 0
	for i := range 4 * table {
		r := e.do("POST", "/api/v1/signups", "", signupBody(fmt.Sprintf("flood%d@example.com", i), fmt.Sprintf("fl%d", i)))
		if r.status == 429 {
			refused++
		}
	}
	if refused < 2*table {
		t.Fatalf("only %d of %d flood requests were refused; the check would be vacuous", refused, 4*table)
	}
	for i := range 4 * table {
		e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": fmt.Sprintf("cflood%d@example.com", i), "code": "123456"})
	}
	clk.Advance(time.Minute) // the global buckets refill; the per-key windows do not
	if r := e.do("POST", "/api/v1/signups", "", signupBody("real@example.com", "real")); r.status != 202 {
		t.Fatalf("a real new address after the flood: %d %s (the email table was full)", r.status, r.body)
	}
	if r := e.do("POST", "/api/v1/signups/confirm", "", map[string]any{"email": "real2@example.com", "code": "123456"}); r.status != 401 {
		t.Fatalf("a real new confirm after the flood: %d %s (the email table was full)", r.status, r.body)
	}
}

// TestSignups_NoMailRowAskedAgainWhenMailWorks: a code-less request made
// while mail was off, asked again once mail works, gets a code and becomes
// an ordinary unconfirmed request: hidden from GET /signups, refused by
// approve, in the 1000 bucket, living at most 24 hours, and the purge
// deletes it.
func TestSignups_NoMailRowAskedAgainWhenMailWorks(t *testing.T) {
	e := newEnv(t)
	e.setSignupMode("approval")
	e.smail.ready.Store(false)
	e.ask("again@example.com", "again")
	if got := e.listSignups(e.chen, ""); len(got) != 1 {
		t.Fatalf("the no-mail request does not show: %+v", got)
	}
	e.smail.ready.Store(true)
	e.ask("again@example.com", "again")
	if e.lastSignupCode("again@example.com") == "" {
		t.Fatal("no code was mailed when the person asked again")
	}
	if got := e.listSignups(e.chen, "?status=all"); len(got) != 0 {
		t.Fatalf("the request still shows after it got a code: %+v", got)
	}
	id := e.signupID("again@example.com")
	if r := e.approve(id); r.status != 409 {
		t.Fatalf("approve: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'again@example.com' AND status = 'pending'
		AND email_verified_at IS NULL AND code_hash IS NOT NULL AND expires_at <= now() + interval '24 hours 1 minute'`); n != 1 {
		t.Fatal("the request is not in the unconfirmed bucket with a 24-hour life")
	}
	var out signup.PurgeResult
	e.must(200, e.do("DELETE", "/api/v1/orgs/orbit/signups/unconfirmed", e.chen, nil)).json(t, &out)
	if out.Deleted != 1 || e.countRows(`SELECT count(*) FROM signup_requests WHERE email = 'again@example.com'`) != 0 {
		t.Fatalf("purge deleted %d", out.Deleted)
	}
}
