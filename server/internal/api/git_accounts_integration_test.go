package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/tracker"
)

// LAUN-199: connected accounts (docs/internal/decisions-T34-connected-accounts.md).
// A link is a claim, used for display attribution only.

const gaPath = O + "/me/git-accounts"

type gaFixture struct {
	*ghEnv
	integ    uuid.UUID
	ann, bob string // member tokens (read and write scope)
	annID    uuid.UUID
	bobID    uuid.UUID
}

func newGAFixture(t *testing.T) *gaFixture {
	t.Helper()
	g := newGHEnv(t, 0, 0, true, envOpts{})
	f := &gaFixture{ghEnv: g, integ: g.setup()}
	ann := g.newHuman("ann", "ann@example.com", "member")
	bob := g.newHuman("bob", "bob@example.com", "member")
	f.annID, f.bobID = ann.ID, bob.ID
	f.ann, _ = g.newToken(ann.ID, tokenOpts{scopes: []string{"read", "write"}})
	f.bob, _ = g.newToken(bob.ID, tokenOpts{scopes: []string{"read", "write"}})
	return f
}

func (f *gaFixture) put(token, handle string) resp {
	return f.do("PUT", gaPath+"/"+f.integ.String(), token, map[string]any{"handle": handle})
}

func (f *gaFixture) accounts(token string) tracker.GitAccounts {
	f.t.Helper()
	var a tracker.GitAccounts
	f.must(200, f.do("GET", gaPath, token, nil)).json(f.t, &a)
	return a
}

func (f *gaFixture) identities() tracker.GitIdentities {
	f.t.Helper()
	var a tracker.GitIdentities
	f.must(200, f.do("GET", O+"/git-identities", f.chen, nil)).json(f.t, &a)
	return a
}

func (f *gaFixture) events(typ string) int {
	return f.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = $2`, f.orgID, typ)
}

func TestGitAccounts_Auth(t *testing.T) {
	f := newGAFixture(t)
	id := f.integ.String()
	for _, c := range [][2]string{{"GET", gaPath}, {"PUT", gaPath + "/" + id}, {"DELETE", gaPath + "/" + id}, {"GET", O + "/git-identities"}, {"DELETE", O + "/git-identities/" + id}} {
		expectCode(t, f.do(c[0], c[1], "", map[string]any{"handle": "x"}), 401, "unauthenticated")
	}
	// An agent has no email and cannot log in: it has no account to connect.
	for _, c := range [][2]string{{"GET", gaPath}, {"PUT", gaPath + "/" + id}, {"DELETE", gaPath + "/" + id}} {
		expectCode(t, f.do(c[0], c[1], f.claude, map[string]any{"handle": "claude"}), 403, "forbidden")
	}
	if f.count(`SELECT count(*) FROM git_identities`) != 0 {
		t.Fatal("an agent wrote a link")
	}
	// A read-only token cannot write.
	ro, _ := f.newToken(f.annID, tokenOpts{scopes: []string{"read"}})
	f.must(200, f.do("GET", gaPath, ro, nil))
	expectCode(t, f.put(ro, "ann-dev"), 403, "insufficient_scope")
	expectCode(t, f.do("DELETE", gaPath+"/"+id, ro, nil), 403, "insufficient_scope")
	// The admin pair: a member role is refused even with the admin scope; a
	// write-only scope token of an admin is refused too.
	annAdminScope, _ := f.newToken(f.annID, tokenOpts{})
	expectCode(t, f.do("GET", O+"/git-identities", annAdminScope, nil), 403, "forbidden")
	expectCode(t, f.do("DELETE", O+"/git-identities/"+id, annAdminScope, nil), 403, "forbidden")
	expectCode(t, f.do("GET", O+"/git-identities", f.ann, nil), 403, "insufficient_scope")
	// The admin pair is closed to agents.
	expectCode(t, f.do("GET", O+"/git-identities", f.claude, nil), 403, "insufficient_scope")
}

func TestGitAccounts_LinkChangeUnlink(t *testing.T) {
	f := newGAFixture(t)
	a := f.accounts(f.ann)
	if len(a.Items) != 1 || a.Items[0].Handle != nil || a.Items[0].Provider != "github" || a.Items[0].IntegrationID != f.integ {
		t.Fatalf("fresh list %+v", a)
	}
	var one tracker.GitAccount
	f.must(200, f.put(f.ann, "  @Ann-Dev ")).json(t, &one)
	if one.Handle == nil || *one.Handle != "Ann-Dev" || one.LinkedAt == nil {
		t.Fatalf("link %+v", one)
	}
	// A replay and a change are 200; the other member sees no handle.
	f.must(200, f.put(f.ann, "Ann-Dev"))
	f.must(200, f.put(f.ann, "ann-dev2"))
	if h := f.accounts(f.ann).Items[0].Handle; h == nil || *h != "ann-dev2" {
		t.Fatalf("after change %v", h)
	}
	if h := f.accounts(f.bob).Items[0].Handle; h != nil {
		t.Fatalf("bob sees %v", *h)
	}
	if n := f.count(`SELECT count(*) FROM git_identities`); n != 1 {
		t.Fatalf("%d rows, want 1 (one handle per member per connection)", n)
	}
	// Unlink twice: 204 both times.
	f.must(204, f.do("DELETE", gaPath+"/"+f.integ.String(), f.ann, nil))
	f.must(204, f.do("DELETE", gaPath+"/"+f.integ.String(), f.ann, nil))
	if h := f.accounts(f.ann).Items[0].Handle; h != nil {
		t.Fatalf("after unlink %v", *h)
	}
	// A user-private row: no event at all.
	if n := f.count(`SELECT count(*) FROM events WHERE type LIKE 'git_identity.%'`); n != 0 {
		t.Fatalf("own link changes emitted %d events", n)
	}
	// A disabled connection with no link is not listed; with a link it is.
	f.must(200, f.put(f.ann, "ann-dev"))
	if _, err := f.pool.Exec(context.Background(), `UPDATE git_integrations SET enabled = false WHERE id = $1`, f.integ); err != nil {
		t.Fatal(err)
	}
	if len(f.accounts(f.ann).Items) != 1 || len(f.accounts(f.bob).Items) != 0 {
		t.Fatal("disabled connection listing wrong")
	}
}

func TestGitAccounts_Validation(t *testing.T) {
	f := newGAFixture(t)
	bad := []string{"", "  ", "@", "-ann", "a b", "ann!", "ann/dev", "a.b", strings.Repeat("a", 40), "<script>", "ann\u0000", "an\nn", "Ünï", "bot[bot]"}
	for _, h := range bad {
		expectCode(t, f.put(f.ann, h), 400, "validation_failed")
	}
	f.must(200, f.put(f.ann, strings.Repeat("a", 39)))
	expectCode(t, f.do("PUT", gaPath+"/"+f.integ.String(), f.ann, map[string]any{"handle": "x", "extra": 1}), 400, "bad_request")
	expectCode(t, f.do("PUT", gaPath+"/not-a-uuid", f.ann, map[string]any{"handle": "x"}), 404, "not_found")
	if n := f.count(`SELECT count(*) FROM git_identities WHERE handle = ''`); n != 0 {
		t.Fatal("empty handle stored")
	}
	// Provider rules: the CleanHandle table.
	cases := []struct {
		provider, in string
		ok           bool
	}{
		{"github", "a_b-c", true}, {"github", "a.b", false},
		{"gitlab", "a.b-c_d", true}, {"gitlab", "a b", false}, {"gitlab", strings.Repeat("a", 100), true}, {"gitlab", strings.Repeat("a", 101), false},
		{"bitbucket", "Ann Dev", true}, {"bitbucket", "557058:ab-12", true}, {"bitbucket", " ", false}, {"bitbucket", "a<b", false},
	}
	for _, c := range cases {
		if _, err := tracker.CleanHandle(c.provider, c.in); (err == nil) != c.ok {
			t.Errorf("CleanHandle(%s, %q) err=%v, want ok=%v", c.provider, c.in, err, c.ok)
		}
	}
}

// Duplicate and hijack: the handle has one holder; a second claim is a 409
// that does not say who holds it, and it changes nothing.
func TestGitAccounts_DuplicateAndHijack(t *testing.T) {
	f := newGAFixture(t)
	f.must(200, f.put(f.ann, "Example-Dev"))
	for _, h := range []string{"Example-Dev", "example-dev", "EXAMPLE-DEV", "@example-dev"} {
		r := f.put(f.bob, h)
		expectCode(t, r, 409, "conflict")
		s := string(r.body)
		if !strings.Contains(s, tracker.HandleTakenText) || strings.Contains(s, "ann") && strings.Contains(strings.ReplaceAll(s, tracker.HandleTakenText, ""), "ann") {
			t.Fatalf("409 body %s", s)
		}
	}
	if h := f.accounts(f.ann).Items[0].Handle; h == nil || *h != "Example-Dev" {
		t.Fatalf("ann lost the link: %v", h)
	}
	if f.accounts(f.bob).Items[0].Handle != nil {
		t.Fatal("bob got a handle")
	}
	// Bob's failed claim does not unlink his own earlier link (the upsert is atomic).
	f.must(200, f.put(f.bob, "bob-dev"))
	expectCode(t, f.put(f.bob, "example-dev"), 409, "conflict")
	if h := f.accounts(f.bob).Items[0].Handle; h == nil || *h != "bob-dev" {
		t.Fatalf("bob's link after a refused claim: %v", h)
	}
	// Only an org admin frees a handle.
	annAdminScope, _ := f.newToken(f.annID, tokenOpts{})
	var ids tracker.GitIdentities = f.identities()
	if len(ids.Items) != 2 {
		t.Fatalf("admin list %+v", ids)
	}
	var annLink tracker.GitIdentity
	for _, i := range ids.Items {
		if i.User.Username == "ann" {
			annLink = i
		}
	}
	expectCode(t, f.do("DELETE", O+"/git-identities/"+annLink.ID.String(), annAdminScope, nil), 403, "forbidden")
	expectCode(t, f.do("DELETE", O+"/git-identities/"+annLink.ID.String(), f.bob, nil), 403, "insufficient_scope")
	f.must(204, f.do("DELETE", O+"/git-identities/"+annLink.ID.String(), f.chen, nil))
	f.must(200, f.put(f.bob, "example-dev"))
	expectCode(t, f.do("DELETE", O+"/git-identities/"+annLink.ID.String(), f.chen, nil), 404, "not_found")
}

func TestGitAccounts_AdminUnlinkEmitsIDsOnly(t *testing.T) {
	f := newGAFixture(t)
	f.must(200, f.put(f.ann, "secret-handle-ann"))
	id := f.identities().Items[0]
	if id.Handle != "secret-handle-ann" || id.User.Username != "ann" {
		t.Fatalf("%+v", id)
	}
	f.must(204, f.do("DELETE", O+"/git-identities/"+id.ID.String(), f.chen, nil))
	if f.events("git_identity.removed") != 1 {
		t.Fatal("no event for an admin unlink")
	}
	var payload string
	if err := f.pool.QueryRow(context.Background(), `SELECT payload::text FROM events WHERE type = 'git_identity.removed'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "secret-handle") || !strings.Contains(payload, f.annID.String()) || !strings.Contains(payload, f.integ.String()) {
		t.Fatalf("payload %s", payload)
	}
	if h := f.accounts(f.ann).Items[0].Handle; h != nil {
		t.Fatalf("link survived %v", *h)
	}
}

// The attribution is display only: authorUser follows the link, per
// connection, and never a deactivated member.
func TestGitAccounts_AttributesChangeRequest(t *testing.T) {
	f := newGAFixture(t)
	is := f.createIssue(f.chen, map[string]any{"title": "attribution"})
	f.setState(is.Identifier, "Todo")
	f.ghSend("pr_opened.json", ghMut(is.Identifier, 1))
	if cr := f.oneCR(is.Identifier); cr.Author != "example-dev" || cr.AuthorUser != nil {
		t.Fatalf("no link: %+v", cr)
	}
	f.must(200, f.put(f.ann, "Example-Dev"))
	cr := f.oneCR(is.Identifier)
	if cr.AuthorUser == nil || cr.AuthorUser.Username != "ann" || cr.AuthorUser.Name == "" || cr.Author != "example-dev" {
		t.Fatalf("linked: %+v", cr)
	}
	// Reading the issue as another member shows the same: attribution is not a permission.
	if bobCR := f.changeRequestsAs(f.bob, is.Identifier); len(bobCR) != 1 || bobCR[0].AuthorUser == nil {
		t.Fatalf("bob view %+v", bobCR)
	}
	// A link on ANOTHER connection with the same handle attributes nothing here.
	other := f.otherConnection(f.orgID, "https://ghe.example.com")
	f.must(200, f.do("PUT", gaPath+"/"+other.String(), f.bob, map[string]any{"handle": "example-dev"}))
	if cr := f.oneCR(is.Identifier); cr.AuthorUser == nil || cr.AuthorUser.Username != "ann" {
		t.Fatalf("other connection changed attribution: %+v", cr)
	}
	// A deactivated member does not show.
	if _, err := f.pool.Exec(context.Background(), `UPDATE users SET deactivated_at = now() WHERE id = $1`, f.annID); err != nil {
		t.Fatal(err)
	}
	if cr := f.oneCR(is.Identifier); cr.AuthorUser != nil {
		t.Fatalf("deactivated member shown: %+v", cr)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE users SET deactivated_at = NULL WHERE id = $1`, f.annID); err != nil {
		t.Fatal(err)
	}
	// Unlink: back to the plain login.
	f.must(204, f.do("DELETE", gaPath+"/"+f.integ.String(), f.ann, nil))
	if cr := f.oneCR(is.Identifier); cr.AuthorUser != nil {
		t.Fatalf("after unlink: %+v", cr)
	}
}

func (f *gaFixture) changeRequestsAs(token, ident string) []tracker.ChangeRequest {
	f.t.Helper()
	var is tracker.Issue
	f.must(200, f.do("GET", O+"/issues/"+ident+"?include=changeRequests", token, nil)).json(f.t, &is)
	return is.ChangeRequests
}

// otherConnection inserts a second GitHub connection (another base URL).
func (f *gaFixture) otherConnection(org uuid.UUID, base string) uuid.UUID {
	f.t.Helper()
	id := uuid.Must(uuid.NewV7())
	h := make([]byte, 32)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO git_integrations
		(id, org_id, base_url, webhook_secret_hash, enabled, provider, mode) VALUES ($1, $2, $3, $4, true, 'github', 'webhook_only')`,
		id, org, base, h); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestGitAccounts_CrossOrg(t *testing.T) {
	f := newGAFixture(t)
	acmeID := f.newOrg("acme", "ACM")
	chen := f.user("chen")
	f.addMember(acmeID, chen.ID, "admin")
	tok, _ := f.newToken(chen.ID, tokenOpts{}) // unpinned
	acmeConn := f.otherConnection(acmeID, "https://github.com")
	const A = "/api/v1/orgs/acme"
	// Chen links in acme; the orbit side sees nothing of it.
	f.must(200, f.do("PUT", A+"/me/git-accounts/"+acmeConn.String(), tok, map[string]any{"handle": "chen-acme"}))
	if len(f.identities().Items) != 0 {
		t.Fatalf("acme link leaked into orbit: %+v", f.identities())
	}
	if len(f.accounts(tok).Items) != 1 || f.accounts(tok).Items[0].IntegrationID != f.integ {
		t.Fatalf("orbit list %+v", f.accounts(tok))
	}
	// A connection of the other org is 404 for every verb.
	expectCode(t, f.do("PUT", gaPath+"/"+acmeConn.String(), tok, map[string]any{"handle": "x"}), 404, "not_found")
	expectCode(t, f.do("DELETE", gaPath+"/"+acmeConn.String(), tok, nil), 404, "not_found")
	// An identity of the other org is 404 for the admin unlink in this org, and survives.
	var acmeIdent uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM git_identities WHERE org_id = $1`, acmeID).Scan(&acmeIdent); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.do("DELETE", O+"/git-identities/"+acmeIdent.String(), tok, nil), 404, "not_found")
	if n := f.count(`SELECT count(*) FROM git_identities WHERE org_id = $1`, acmeID); n != 1 {
		t.Fatalf("acme link removed from orbit: %d", n)
	}
	// The same handle in two orgs is two separate claims (the key is per connection).
	f.must(200, f.put(f.ann, "chen-acme"))
	// Composite FKs: a row cannot pair an org with another org's connection or non-member.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO git_identities (org_id, user_id, integration_id, handle, handle_key)
		VALUES ($1, $2, $3, 'x', 'x')`, f.orgID, f.annID, acmeConn); err == nil {
		t.Fatal("a cross-org connection reference was accepted")
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO git_identities (org_id, user_id, integration_id, handle, handle_key)
		VALUES ($1, $2, $3, 'y', 'y')`, acmeID, f.annID, acmeConn); err == nil {
		t.Fatal("a non-member of the org was accepted")
	}
}

// A member who leaves the org, or a connection that is deleted, takes the links with it.
func TestGitAccounts_Cascades(t *testing.T) {
	f := newGAFixture(t)
	f.must(200, f.put(f.ann, "ann-dev"))
	f.must(200, f.put(f.bob, "bob-dev"))
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, f.orgID, f.annID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM git_identities`); n != 1 {
		t.Fatalf("%d rows after a member left", n)
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM git_integrations WHERE id = $1`, f.integ); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM git_identities`); n != 0 {
		t.Fatalf("%d rows after the connection went", n)
	}
}

// A link cannot be made on a turned-off connection: a fixed 409, nothing stored.
func TestGitAccounts_DisabledConnectionRefusesLink(t *testing.T) {
	f := newGAFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE git_integrations SET enabled = false WHERE id = $1`, f.integ); err != nil {
		t.Fatal(err)
	}
	r := f.put(f.ann, "ann-dev")
	expectCode(t, r, 409, "conflict")
	// The web client matches the prefix "this connection is turned off" (connectedAccountsText.ts).
	if !strings.HasPrefix(tracker.ConnectionOffText, "this connection is turned off") || !strings.Contains(string(r.body), tracker.ConnectionOffText) {
		t.Fatalf("body %s", r.body)
	}
	if n := f.count(`SELECT count(*) FROM git_identities`); n != 0 {
		t.Fatalf("%d rows stored", n)
	}
	// Removing is still allowed.
	f.must(204, f.do("DELETE", gaPath+"/"+f.integ.String(), f.ann, nil))
}

// Only an ASCII login is matched: lower() folds U+212A (Kelvin sign) to "k", so
// a login that only looks like a handle must not attribute.
func TestGitAccounts_AttributionIsASCIIOnly(t *testing.T) {
	f := newGAFixture(t)
	is := f.createIssue(f.chen, map[string]any{"title": "kelvin"})
	f.setState(is.Identifier, "Todo")
	f.ghSend("pr_opened.json", ghMut(is.Identifier, 1))
	f.must(200, f.put(f.ann, "kate"))
	setAuthor := func(a string) {
		if _, err := f.pool.Exec(context.Background(), `UPDATE mr_links SET author = $1 WHERE org_id = $2`, a, f.orgID); err != nil {
			f.t.Fatal(err)
		}
	}
	setAuthor("Kate")
	if cr := f.oneCR(is.Identifier); cr.AuthorUser == nil {
		t.Fatal("ASCII login not attributed")
	}
	setAuthor("\u212Aate")
	if cr := f.oneCR(is.Identifier); cr.AuthorUser != nil {
		t.Fatalf("Kelvin-sign login attributed: %+v", cr.AuthorUser)
	}
}
