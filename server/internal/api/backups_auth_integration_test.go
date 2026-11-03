//go:build backuptest

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/auth"
)

type bkRoute struct {
	method, path string
	body         any
	write        bool
}

// bkRoutes is every backups route with a body that would succeed or fail
// harmlessly. The ids are random: the gate answers before any lookup.
func (bk *bkEnv) bkRoutes() []bkRoute {
	id, fp := uuid.NewString(), strings.Repeat("a", 64)
	return []bkRoute{
		{"GET", bkp + "/settings", nil, false},
		{"PUT", bkp + "/settings", map[string]any{"notifyOnFailure": true, "notifyOnSuccess": true, "staleAlertHours": 12}, true},
		{"GET", bkp + "/status", nil, false},
		{"GET", bkp + "/recipients", nil, false},
		{"POST", bkp + "/recipients", map[string]any{"publicKey": bk.newKey()}, true},
		{"DELETE", bkp + "/recipients/" + fp, nil, true},
		{"POST", bkp + "/recipients/" + fp + "/cancel", nil, true},
		{"GET", bkp + "/destinations", nil, false},
		{"POST", bkp + "/destinations", map[string]any{"name": "gate", "type": "s3", "config": cfgFor("gate")}, true},
		{"POST", bkp + "/destinations/probe", map[string]any{"type": "s3", "config": cfgFor("gate")}, true},
		{"POST", bkp + "/destinations/test", map[string]any{"type": "s3", "config": cfgFor("gate")}, true},
		{"GET", bkp + "/destinations/" + id, nil, false},
		{"PATCH", bkp + "/destinations/" + id, map[string]any{"name": "x"}, true},
		{"DELETE", bkp + "/destinations/" + id, nil, true},
		{"POST", bkp + "/destinations/" + id + "/probe", nil, true},
		{"POST", bkp + "/destinations/" + id + "/test", nil, true},
		{"POST", bkp + "/destinations/" + id + "/exchange", map[string]any{"code": "c"}, true},
		{"POST", bkp + "/destinations/" + id + "/run", nil, true},
		{"GET", bkp + "/runs", nil, false},
		{"GET", bkp + "/runs/" + id, nil, false},
		{"GET", bkp + "/changes", nil, false},
	}
}

// TestBackups_Auth is the platform-admin gate matrix (decisions-T16 #43) on
// every route: no credentials, a bad token, a member, an org admin who is not a
// platform admin, a platform admin's org-pinned token, a platform admin's token
// without the admin scope, an agent, and a session. Nothing is written by a
// refused request.
func TestBackups_Auth(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	member := bk.newHuman("ann", "ann@example.com", "member")
	memberTok, _ := bk.newToken(member.ID, tokenOpts{pin: &bk.orgID, scopes: []string{"read", "write"}})
	orgAdmin := bk.newHuman("bob", "bob@example.com", "admin")
	orgAdminUnpinned, _ := bk.newToken(orgAdmin.ID, tokenOpts{})
	orgAdminPinned, _ := bk.newToken(orgAdmin.ID, tokenOpts{pin: &bk.orgID})
	readWrite, _ := bk.newToken(bk.chen.ID, tokenOpts{scopes: []string{"read", "write"}})
	denied := []struct {
		name, token string
		status      int
		code        string
	}{
		{"no credentials", "", 401, "unauthenticated"},
		{"bad token", "odt_" + strings.Repeat("x", 43), 401, "invalid_token"},
		{"member", memberTok, 403, "forbidden"},
		{"org admin, unpinned admin PAT, not a platform admin", orgAdminUnpinned, 403, "forbidden"},
		{"org admin, pinned admin PAT", orgAdminPinned, 403, "forbidden"},
		{"platform admin, org-pinned fixture token", bk.env.chen, 403, "forbidden"},
		{"platform admin, PAT without the admin scope", readWrite, 403, "forbidden"},
		{"agent", bk.claude, 403, "forbidden"},
	}
	routes := bk.bkRoutes()
	for _, rt := range routes {
		for _, tt := range denied {
			t.Run(rt.method+" "+rt.path[len(bkp):]+" "+tt.name, func(t *testing.T) {
				r := bk.do(rt.method, rt.path, tt.token, rt.body)
				if r.status != tt.status || r.errCode(t) != tt.code {
					t.Fatalf("%d %s, want %d %s", r.status, r.body, tt.status, tt.code)
				}
			})
		}
	}
	// A refused request wrote nothing.
	for _, table := range []string{"backup_change_log", "backup_destinations", "backup_runs", "backup_requests"} {
		if n := bk.countRows(table); n != 0 {
			t.Errorf("%s holds %d rows after refused requests", table, n)
		}
	}
	if l := bk.recipients(); len(l.Items) != 0 {
		t.Errorf("a refused request added a recipient: %+v", l)
	}
	// A platform admin's unpinned admin token and session pass the gate.
	for _, rt := range routes {
		for name, tok := range map[string]string{"token": bk.admin, "session": bk.session, "second admin": bk.admin2} {
			r := bk.do(rt.method, rt.path, tok, rt.body)
			if r.status == 401 || r.status == 403 && r.errCode(t) == "forbidden" {
				t.Errorf("%s %s as a platform admin (%s): %d %s", rt.method, rt.path, name, r.status, r.body)
			}
		}
	}
}

// TestBackups_Auth_CSRF: a cookie write needs the CSRF header.
func TestBackups_Auth_CSRF(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	body := map[string]any{"notifyOnFailure": true, "notifyOnSuccess": false, "staleAlertHours": 48}
	r := bk.do("PUT", bkp+"/settings", bk.session, body, auth.CSRFHeader, "wrong")
	if r.status != 403 || r.errCode(t) != "csrf_failed" {
		t.Fatalf("PUT with a bad CSRF header: %d %s", r.status, r.body)
	}
	bk.must(200, bk.do("PUT", bkp+"/settings", bk.session, body))
}

// TestBackups_NoIdempotencyKey: every write route refuses an Idempotency-Key
// and stores no idempotency row (the bodies hold secrets).
func TestBackups_NoIdempotencyKey(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	for _, rt := range bk.bkRoutes() {
		if !rt.write {
			continue
		}
		r := bk.do(rt.method, rt.path, bk.admin, rt.body, "Idempotency-Key", "k-"+rt.method)
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "Idempotency-Key") {
			t.Errorf("%s %s with an Idempotency-Key: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	if n := bk.countRows("idempotency_keys"); n != 0 {
		t.Fatalf("%d idempotency rows after write requests to the backup routes", n)
	}
	if n := bk.countRows("backup_change_log"); n != 0 {
		t.Fatalf("a refused request wrote %d change-log rows", n)
	}
}

// TestBackups_JSONOnly: a write with another content type is 415, and a
// malformed body is 400 that does not echo the body.
func TestBackups_JSONOnly(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	for _, rt := range bk.bkRoutes() {
		if !rt.write || rt.body == nil {
			continue
		}
		req, _ := http.NewRequest(rt.method, bk.srv.URL+rt.path, strings.NewReader("a=b"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		bk.authorize(req, bk.admin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 415 {
			t.Errorf("%s %s as a form: %d", rt.method, rt.path, res.StatusCode)
		}
	}
	r := bk.do("POST", bkp+"/destinations", bk.admin, nil)
	if r.status != 400 && r.status != 415 {
		t.Errorf("no body: %d %s", r.status, r.body)
	}
	_ = context.Background()
}
