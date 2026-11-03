package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
)

func TestGenerateToken(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if !WellFormed(tok.Plaintext) {
			t.Fatalf("not well formed: %q", tok.Plaintext)
		}
		if tok.Prefix != tok.Plaintext[:12] || len(tok.Hash) != 32 || string(tok.Hash) != string(HashToken(tok.Plaintext)) {
			t.Fatalf("prefix/hash wrong for %q", tok.Plaintext)
		}
		if seen[tok.Plaintext] {
			t.Fatal("duplicate token")
		}
		seen[tok.Plaintext] = true
	}
}

func TestWellFormed(t *testing.T) {
	good := "odt_" + "abcdefghijABCDEFGHIJ0123456789abcdefghijABC"
	tests := []struct {
		in   string
		want bool
	}{
		{good, true},
		{good[:len(good)-1], false},
		{good + "x", false},
		{"pat_" + good[4:], false},
		{good[:10] + "-" + good[11:], false},
		{"", false},
	}
	for _, tt := range tests {
		if got := WellFormed(tt.in); got != tt.want {
			t.Errorf("WellFormed(%q) = %v", tt.in, got)
		}
	}
}

func TestParseScopesAndHasScope(t *testing.T) {
	if _, err := ParseScopes(nil); err == nil {
		t.Fatal("empty scopes accepted")
	}
	if _, err := ParseScopes([]string{"read", "root"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	got, err := ParseScopes([]string{"read", "read", "write"})
	if err != nil || len(got) != 2 {
		t.Fatalf("dedupe: %v %v", got, err)
	}
	tests := []struct {
		have []Scope
		need Scope
		want bool
	}{
		{[]Scope{ScopeRead}, ScopeRead, true},
		{[]Scope{ScopeRead}, ScopeWrite, false},
		{[]Scope{ScopeWrite}, ScopeRead, true},
		{[]Scope{ScopeWrite}, ScopeAdmin, false},
		{[]Scope{ScopeAdmin}, ScopeWrite, true},
	}
	for _, tt := range tests {
		p := &Principal{Scopes: tt.have}
		if got := p.HasScope(tt.need); got != tt.want {
			t.Errorf("%v has %s = %v", tt.have, tt.need, got)
		}
	}
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	tokens     map[string]dbq.GetTokenByHashRow // key: string(hash)
	orgs       map[string]dbq.Org
	members    map[[2]uuid.UUID]string
	teamAccess map[[2]uuid.UUID]string // flag; absent is 'all'
	teamIDs    map[[2]uuid.UUID][]uuid.UUID
	teamReads  int
	fail       bool
}

func (f *fakeStore) GetTokenByHash(_ context.Context, h []byte) (dbq.GetTokenByHashRow, error) {
	if f.fail {
		return dbq.GetTokenByHashRow{}, errors.New("db down")
	}
	row, ok := f.tokens[string(h)]
	if !ok {
		return row, pgx.ErrNoRows
	}
	return row, nil
}
func (f *fakeStore) TouchTokenLastUsed(context.Context, uuid.UUID) error { return nil }
func (f *fakeStore) GetOrgBySlug(_ context.Context, slug string) (dbq.Org, error) {
	o, ok := f.orgs[slug]
	if !ok {
		return o, pgx.ErrNoRows
	}
	return o, nil
}
func (f *fakeStore) GetMembership(_ context.Context, a dbq.GetMembershipParams) (dbq.GetMembershipRow, error) {
	k := [2]uuid.UUID{a.OrgID, a.UserID}
	r, ok := f.members[k]
	if !ok {
		return dbq.GetMembershipRow{}, pgx.ErrNoRows
	}
	flag, set := f.teamAccess[k]
	if !set {
		flag = "all"
	}
	return dbq.GetMembershipRow{Role: r, TeamAccess: flag}, nil
}
func (f *fakeStore) ListMemberTeamIDs(_ context.Context, a dbq.ListMemberTeamIDsParams) ([]uuid.UUID, error) {
	f.teamReads++
	return f.teamIDs[[2]uuid.UUID{a.OrgID, a.UserID}], nil
}

func TestAuthenticate(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	userID := uuid.New()
	mk := func() string { tok, _ := GenerateToken(); return tok.Plaintext }
	ok, revoked, expired, unexpired, session, native, deactivated := mk(), mk(), mk(), mk(), mk(), mk(), mk()
	row := func(mod func(*dbq.GetTokenByHashRow)) dbq.GetTokenByHashRow {
		r := dbq.GetTokenByHashRow{ID: uuid.New(), UserID: userID, Kind: KindPAT, Scopes: []string{"read"}, Username: "chen"}
		if mod != nil {
			mod(&r)
		}
		return r
	}
	store := &fakeStore{tokens: map[string]dbq.GetTokenByHashRow{
		string(HashToken(ok)):          row(nil),
		string(HashToken(revoked)):     row(func(r *dbq.GetTokenByHashRow) { r.RevokedAt = &past }),
		string(HashToken(expired)):     row(func(r *dbq.GetTokenByHashRow) { r.ExpiresAt = &past }),
		string(HashToken(unexpired)):   row(func(r *dbq.GetTokenByHashRow) { r.ExpiresAt = &future }),
		string(HashToken(session)):     row(func(r *dbq.GetTokenByHashRow) { r.Kind = KindSession }),
		string(HashToken(native)):      row(func(r *dbq.GetTokenByHashRow) { r.Kind = KindNative }),
		string(HashToken(deactivated)): row(func(r *dbq.GetTokenByHashRow) { r.DeactivatedAt = &past }),
	}}
	a := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	a.now = func() time.Time { return now }
	var seen *Principal
	h := a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name   string
		header string
		status int
		code   string
	}{
		{"none", "", 401, "unauthenticated"},
		{"basic", "Basic abc", 401, "invalid_token"},
		{"bearer lowercase scheme", "bearer " + ok, 204, ""},
		{"malformed", "Bearer odt_x", 401, "invalid_token"},
		{"unknown", "Bearer " + mk(), 401, "invalid_token"},
		{"ok", "Bearer " + ok, 204, ""},
		{"revoked", "Bearer " + revoked, 401, "token_revoked"},
		{"expired", "Bearer " + expired, 401, "token_expired"},
		{"not yet expired", "Bearer " + unexpired, 204, ""},
		{"native token as bearer (native app, LAUN-122)", "Bearer " + native, 204, ""},
		{"web session value as bearer is refused like an unknown token (LAUN-129)", "Bearer " + session, 401, "invalid_token"},
		{"deactivated user", "Bearer " + deactivated, 401, "invalid_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			req := httptest.NewRequest("GET", "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tt.status, rec.Body)
			}
			if tt.code == "" {
				if seen == nil || seen.UserID != userID {
					t.Fatal("principal not set")
				}
				return
			}
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["code"] != tt.code || body["error"] == "" {
				t.Fatalf("body %v, want code %s", body, tt.code)
			}
		})
	}
	t.Run("db error is a 500 without detail", func(t *testing.T) {
		store.fail = true
		defer func() { store.fail = false }()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer "+ok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 500 || strings.Contains(rec.Body.String(), "db down") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})
}

func TestRequireOrgMemberAndScope(t *testing.T) {
	orbit, acme := uuid.New(), uuid.New()
	member, outsider := uuid.New(), uuid.New()
	store := &fakeStore{
		orgs: map[string]dbq.Org{"orbit": {ID: orbit, Slug: "orbit"}, "acme": {ID: acme, Slug: "acme"}},
		members: map[[2]uuid.UUID]string{
			{orbit, member}: "member",
			{acme, member}:  "admin",
		},
	}
	a := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	route := func(scope Scope) http.Handler {
		r := chi.NewRouter()
		r.Route("/orgs/{org}", func(r chi.Router) {
			r.Use(a.RequireOrgMember)
			r.With(RequireScope(scope)).Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
		})
		return r
	}
	tests := []struct {
		name   string
		p      *Principal
		org    string
		scope  Scope
		status int
		code   string
	}{
		{"member reads", &Principal{UserID: member, Scopes: []Scope{ScopeRead}}, "orbit", ScopeRead, 204, ""},
		{"unknown org", &Principal{UserID: member, Scopes: []Scope{ScopeRead}}, "nope", ScopeRead, 404, "not_found"},
		{"non-member", &Principal{UserID: outsider, Scopes: []Scope{ScopeAdmin}}, "orbit", ScopeRead, 404, "not_found"},
		{"pinned elsewhere", &Principal{UserID: member, Scopes: []Scope{ScopeRead}, PinnedOrgID: &acme}, "orbit", ScopeRead, 404, "not_found"},
		{"pinned here", &Principal{UserID: member, Scopes: []Scope{ScopeRead}, PinnedOrgID: &orbit}, "orbit", ScopeRead, 204, ""},
		{"read cannot write", &Principal{UserID: member, Scopes: []Scope{ScopeRead}}, "orbit", ScopeWrite, 403, "insufficient_scope"},
		{"admin scope, member role", &Principal{UserID: member, Scopes: []Scope{ScopeAdmin}}, "orbit", ScopeAdmin, 403, "forbidden"},
		{"admin scope, admin role", &Principal{UserID: member, Scopes: []Scope{ScopeAdmin}}, "acme", ScopeAdmin, 204, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/orgs/"+tt.org+"/x", nil)
			req = req.WithContext(WithPrincipal(req.Context(), tt.p))
			rec := httptest.NewRecorder()
			route(tt.scope).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tt.status, rec.Body)
			}
			if tt.code != "" {
				var body map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if body["code"] != tt.code {
					t.Fatalf("code %q, want %q", body["code"], tt.code)
				}
			}
		})
	}
}

// TestRequirePlatformAdmin: only a platform admin's unpinned token with the
// admin scope passes. The flag comes from the user row (GetTokenByHash), so
// the test goes through Authenticate.
func TestRequirePlatformAdmin(t *testing.T) {
	org := uuid.New()
	mk := func() string { tok, _ := GenerateToken(); return tok.Plaintext }
	type tc struct {
		name   string
		row    dbq.GetTokenByHashRow
		status int
	}
	all := []string{"read", "write", "admin"}
	cases := []tc{
		{"member (no flag, read/write)", dbq.GetTokenByHashRow{Scopes: []string{"read", "write"}}, 403},
		{"org admin (admin scope, no flag)", dbq.GetTokenByHashRow{Scopes: all}, 403},
		{"org admin pinned (no flag)", dbq.GetTokenByHashRow{Scopes: all, OrgID: &org}, 403},
		{"platform admin, unpinned, admin scope", dbq.GetTokenByHashRow{Scopes: all, IsPlatformAdmin: true}, 204},
		{"platform admin, org-pinned PAT", dbq.GetTokenByHashRow{Scopes: all, IsPlatformAdmin: true, OrgID: &org}, 403},
		{"platform admin, no admin scope", dbq.GetTokenByHashRow{Scopes: []string{"read", "write"}, IsPlatformAdmin: true}, 403},
		{"platform admin session without admin scope", dbq.GetTokenByHashRow{Kind: KindSession, Scopes: []string{"read", "write"}, IsPlatformAdmin: true}, 403},
		{"platform admin session with admin scope", dbq.GetTokenByHashRow{Kind: KindSession, Scopes: all, IsPlatformAdmin: true}, 204},
	}
	store := &fakeStore{tokens: map[string]dbq.GetTokenByHashRow{}}
	secrets := make([]string, len(cases))
	for i, c := range cases {
		secrets[i] = mk()
		c.row.ID, c.row.UserID, c.row.Username = uuid.New(), uuid.New(), "u"
		if c.row.Kind == "" {
			c.row.Kind = KindPAT
		}
		store.tokens[string(HashToken(secrets[i]))] = c.row
	}
	a := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	a.UseCookieNames(LocalCookieNames)
	h := a.Authenticate(RequirePlatformAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if c.row.Kind == KindSession {
				req.AddCookie(&http.Cookie{Name: LocalCookieNames.Session, Value: secrets[i]})
			} else {
				req.Header.Set("Authorization", "Bearer "+secrets[i])
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, c.status, rec.Body)
			}
			if c.status == 403 {
				var body map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if body["code"] != "forbidden" || body["error"] == "" {
					t.Fatalf("body %v", body)
				}
			}
		})
	}
	t.Run("no principal is 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		RequirePlatformAdmin(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 401 {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

func TestNewTeamAccess(t *testing.T) {
	a, b, c := uuid.MustParse("00000000-0000-7000-8000-000000000002"),
		uuid.MustParse("00000000-0000-7000-8000-000000000001"), uuid.New()
	if got := NewTeamAccess("member", "all", nil); !got.All || len(got.IDs) != 0 {
		t.Fatalf("all: %+v", got)
	}
	if got := NewTeamAccess("admin", "selected", []uuid.UUID{a}); !got.All || len(got.IDs) != 0 {
		t.Fatalf("admin is All whatever the flag: %+v", got)
	}
	for _, flag := range []string{"", "Selected", "future"} {
		if got := NewTeamAccess("member", flag, []uuid.UUID{a}); got.All || len(got.IDs) != 1 {
			t.Fatalf("flag %q must fail closed: %+v", flag, got)
		}
		if !NeedsTeamRows("member", flag) || NeedsTeamRows("admin", flag) {
			t.Fatalf("NeedsTeamRows %q", flag)
		}
	}
	if NeedsTeamRows("member", "all") || !NeedsTeamRows("member", "selected") {
		t.Fatal("NeedsTeamRows all/selected")
	}
	in := []uuid.UUID{a, b}
	got := NewTeamAccess("member", "selected", in)
	if got.All || len(got.IDs) != 2 || got.IDs[0] != b || got.IDs[1] != a {
		t.Fatalf("selected, sorted: %+v", got)
	}
	if in[0] != a {
		t.Fatal("input slice was reordered")
	}
	if !got.Can(a) || !got.Can(b) || got.Can(c) {
		t.Fatal("Can")
	}
	if all, ids := got.Args(); all || len(ids) != 2 {
		t.Fatalf("Args %v %v", all, ids)
	}
	if all, ids := (TeamAccess{All: true}).Args(); !all || ids == nil || len(ids) != 0 {
		t.Fatalf("Args all %v %v", all, ids)
	}
	if e := NewTeamAccess("member", "selected", nil); !e.Empty() || e.Can(a) {
		t.Fatalf("empty: %+v", e)
	}
	if (TeamAccess{All: true}).Empty() || !(TeamAccess{All: true}).Can(c) {
		t.Fatal("All is not empty and sees any team")
	}
}

func TestRequireOrgMemberFillsAccess(t *testing.T) {
	orbit := uuid.New()
	t1, t2 := uuid.MustParse("00000000-0000-7000-8000-000000000009"), uuid.MustParse("00000000-0000-7000-8000-000000000003")
	all, sel, adm := uuid.New(), uuid.New(), uuid.New()
	k := func(u uuid.UUID) [2]uuid.UUID { return [2]uuid.UUID{orbit, u} }
	store := &fakeStore{
		orgs:       map[string]dbq.Org{"orbit": {ID: orbit, Slug: "orbit"}},
		members:    map[[2]uuid.UUID]string{k(all): "member", k(sel): "member", k(adm): "admin"},
		teamAccess: map[[2]uuid.UUID]string{k(sel): "selected", k(adm): "selected"},
		teamIDs:    map[[2]uuid.UUID][]uuid.UUID{k(sel): {t1, t2}, k(adm): {t1}},
	}
	a := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	var got *Org
	h := chi.NewRouter()
	h.Route("/orgs/{org}", func(r chi.Router) {
		r.Use(a.RequireOrgMember)
		r.Get("/x", func(w http.ResponseWriter, r *http.Request) { got, _ = OrgFrom(r.Context()); w.WriteHeader(204) })
	})
	call := func(u uuid.UUID) *Org {
		got = nil
		req := httptest.NewRequest("GET", "/orgs/orbit/x", nil)
		req = req.WithContext(WithPrincipal(req.Context(), &Principal{UserID: u, Scopes: []Scope{ScopeRead}}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 204 || got == nil {
			t.Fatalf("status %d", rec.Code)
		}
		return got
	}
	if o := call(all); !o.Access.All || store.teamReads != 0 {
		t.Fatalf("all member: %+v reads %d", o.Access, store.teamReads)
	}
	if o := call(adm); !o.Access.All || len(o.Access.IDs) != 0 || store.teamReads != 0 {
		t.Fatalf("admin: %+v reads %d", o.Access, store.teamReads)
	}
	o := call(sel)
	if o.Access.All || len(o.Access.IDs) != 2 || o.Access.IDs[0] != t2 || o.Access.IDs[1] != t1 || store.teamReads != 1 {
		t.Fatalf("selected: %+v reads %d", o.Access, store.teamReads)
	}
}
