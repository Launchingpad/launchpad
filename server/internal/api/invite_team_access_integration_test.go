package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/signup"
	"devops/internal/testdb"
)

// Ticket 10 of decisions-T20 (LAUN-226): invites and sign-up approvals carry
// team access (#26 to #30), the accept writes it in its transaction (#27),
// and an open 'selected' invite counts as restricted access for the webhook
// guard (#65, entries 2 and 3).

// inviteEnv is the fixture org with mail off (copied links), three extra
// teams with no issue (so a test can delete them) and a member rita.
type inviteEnv struct {
	*env
	rita    uuid.UUID
	ritaTok string
}

func newInviteEnv(t *testing.T, o envOpts) *inviteEnv {
	t.Helper()
	e := newEnvWith(t, o)
	e.smail.ready.Store(false)
	for _, k := range []string{"INVA", "INVB", "INVC"} {
		e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": k, "name": "Team " + k}))
	}
	ie := &inviteEnv{env: e}
	ie.rita = testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	ie.ritaTok, _ = e.newToken(ie.rita, tokenOpts{pin: &e.orgID})
	return ie
}

// invite posts an invite as chen.
func (e *inviteEnv) invite(body map[string]any) resp {
	e.t.Helper()
	return e.do("POST", "/api/v1/orgs/orbit/invites", e.chen, body)
}

// mustInvite wants 201 and returns the body.
func (e *inviteEnv) mustInvite(body map[string]any) signup.CreatedInvite {
	e.t.Helper()
	var ci signup.CreatedInvite
	e.must(http.StatusCreated, e.invite(body)).json(e.t, &ci)
	return ci
}

func (e *inviteEnv) accept(token, username string) resp {
	e.t.Helper()
	body := map[string]any{"token": token}
	if username != "" {
		body["name"], body["username"] = "Person "+username, username
	}
	return e.do("POST", "/api/v1/invites/accept", "", body)
}

// accessOf is the stored access of a user in orbit: "flag:KEY,KEY", or
// "none" when the user is not a member.
func (e *inviteEnv) accessOf(user uuid.UUID) string {
	e.t.Helper()
	var flag, rows string
	err := e.pool.QueryRow(context.Background(), `SELECT m.team_access,
		coalesce((SELECT string_agg(t.key, ',' ORDER BY t.key) FROM team_members tm JOIN teams t ON t.id = tm.team_id
		          WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id), '')
		FROM org_members m WHERE m.org_id = $1 AND m.user_id = $2`, e.orgID, user).Scan(&flag, &rows)
	if err != nil {
		return "none"
	}
	return flag + ":" + rows
}

func (e *inviteEnv) userID(username string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		e.t.Fatalf("user %s: %v", username, err)
	}
	return id
}

// inviteRows is every invite row and team row of the org, to show a refused
// write changed nothing.
func (e *inviteEnv) inviteRows() string {
	e.t.Helper()
	var a, b string
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY x.id), '')
		FROM org_invites x WHERE x.org_id = $1`, e.orgID).Scan(&a); err != nil {
		e.t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY x.invite_id, x.team_id), '')
		FROM org_invite_teams x WHERE x.org_id = $1`, e.orgID).Scan(&b); err != nil {
		e.t.Fatal(err)
	}
	return a + "|" + b
}

func (e *inviteEnv) inviteOpen(id uuid.UUID) bool {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM org_invites WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, id) == 1
}

func (e *inviteEnv) listInvites(token string) map[uuid.UUID]signup.Invite {
	e.t.Helper()
	var page struct {
		Items []signup.Invite `json:"items"`
	}
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/invites", token, nil)).json(e.t, &page)
	out := map[uuid.UUID]signup.Invite{}
	for _, iv := range page.Items {
		out[iv.ID] = iv
	}
	return out
}

func sel(teams ...string) map[string]any {
	return map[string]any{"teamAccess": "selected", "teams": teams}
}

// AC (#26, #30): an org admin makes an invite with access; the 201 body and
// the list name the team keys; "all" is the default; every bad body is 400
// and writes nothing; members, restricted members and agents get 403.
func TestInviteAccess_CreateAndRefusals(t *testing.T) {
	e := newInviteEnv(t, envOpts{})
	invb := e.teamID("INVB")
	ci := e.mustInvite(map[string]any{"email": "sel@example.com", "teamAccess": "selected", "teams": []string{"INVA", invb.String(), "INVA"}})
	if ci.Invite.TeamAccess != "selected" || strings.Join(ci.Invite.Teams, ",") != "INVA,INVB" || ci.Secret == "" {
		t.Fatalf("created %+v", ci.Invite)
	}
	all := e.mustInvite(map[string]any{"email": "all@example.com"})
	if all.Invite.TeamAccess != "all" || all.Invite.Teams == nil || len(all.Invite.Teams) != 0 {
		t.Fatalf("default invite %+v", all.Invite)
	}
	adm := e.mustInvite(map[string]any{"email": "adm@example.com", "role": "admin", "teamAccess": "all"})
	if adm.Invite.TeamAccess != "all" {
		t.Fatalf("admin invite %+v", adm.Invite)
	}
	list := e.listInvites(e.chen)
	if got := list[ci.Invite.ID]; got.TeamAccess != "selected" || strings.Join(got.Teams, ",") != "INVA,INVB" {
		t.Fatalf("listed %+v", got)
	}
	if got := list[all.Invite.ID]; got.TeamAccess != "all" || len(got.Teams) != 0 {
		t.Fatalf("listed %+v", got)
	}
	if n := e.count(`SELECT count(*) FROM org_invite_teams WHERE invite_id = $1`, ci.Invite.ID); n != 2 {
		t.Fatalf("%d team rows, want 2", n)
	}

	other := e.newOrg("elsewhere", "ELS")
	var els uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1`, other).Scan(&els); err != nil {
		t.Fatal(err)
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = "INVA"
	}
	before := e.inviteRows()
	for name, c := range map[string]struct {
		body map[string]any
		text string
	}{
		"selected with no teams":    {map[string]any{"email": "x1@example.com", "teamAccess": "selected"}, "at least one team"},
		"selected with empty teams": {map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{}}, "at least one team"},
		"all with teams":            {map[string]any{"email": "x1@example.com", "teamAccess": "all", "teams": []string{"INVA"}}, "teams may be set only"},
		"teams with no teamAccess":  {map[string]any{"email": "x1@example.com", "teams": []string{"INVA"}}, "teams may be set only"},
		"a bad teamAccess":          {map[string]any{"email": "x1@example.com", "teamAccess": "some"}, "teamAccess must be"},
		"51 teams":                  {map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": many}, "at most 50"},
		"an admin with teams":       {map[string]any{"email": "x1@example.com", "role": "admin", "teamAccess": "selected", "teams": []string{"INVA"}}, "admin invite"},
		"an unknown team":           {map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{"NOPE"}}, "unknown team"},
		"another org's team key":    {map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{"ELS"}}, "unknown team"},
		"another org's team id":     {map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{els.String()}}, "unknown team"},
		"a made-up team id":         {map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{uuid.NewString()}}, "unknown team"},
	} {
		r := e.invite(c.body)
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" || !strings.Contains(string(r.body), c.text) {
			t.Errorf("%s: %d %s, want 400 %q", name, r.status, r.body, c.text)
		}
	}
	// Another org's team and a made-up id answer alike (cross-org is 400 unknown).
	a := e.invite(map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{els.String()}})
	b := e.invite(map[string]any{"email": "x1@example.com", "teamAccess": "selected", "teams": []string{"00000000-0000-7000-8000-000000000000"}})
	if strings.ReplaceAll(string(a.body), els.String(), "X") != strings.ReplaceAll(string(b.body), "00000000-0000-7000-8000-000000000000", "X") {
		t.Errorf("another org's team and a made-up one differ: %s / %s", a.body, b.body)
	}

	// Only an org admin may make an invite (with access or without).
	claudeTok, _ := e.newToken(e.user("claude").ID, tokenOpts{pin: &e.orgID})
	mona := testdb.AddMember(t, e.pool, e.orgID, "mona", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, mona, e.teamID("INVA"))
	monaTok, _ := e.newToken(mona, tokenOpts{pin: &e.orgID})
	for who, tok := range map[string]string{"member": e.ritaTok, "restricted member": monaTok, "agent": claudeTok, "agent fixture token": e.claude} {
		for _, body := range []map[string]any{
			{"email": "y@example.com", "teamAccess": "selected", "teams": []string{"INVA"}},
			{"email": "y@example.com"},
		} {
			r := e.do("POST", "/api/v1/orgs/orbit/invites", tok, body)
			if code := r.errCode(t); r.status != http.StatusForbidden || (code != "forbidden" && code != "insufficient_scope") {
				t.Errorf("%s: %d %s, want 403", who, r.status, r.body)
			}
		}
		if r := e.do("GET", "/api/v1/orgs/orbit/invites", tok, nil); r.status != http.StatusForbidden {
			t.Errorf("%s lists invites: %d", who, r.status)
		}
	}
	// The invites of the refused calls above were never made; the restricted
	// member made above is not an invite row.
	if after := e.inviteRows(); after != before {
		t.Fatal("a refused invite wrote a row")
	}
}

// AC (#27, #28): the accept gives a new user exactly the invite's teams, in
// the accept's transaction; the accept body cannot widen it; an existing
// install-level user gets the invite's access; an existing member keeps
// theirs; an all invite and an admin invite give all.
func TestInviteAccess_AcceptAppliesExactly(t *testing.T) {
	e := newInviteEnv(t, envOpts{})
	ci := e.mustInvite(map[string]any{"email": "ann@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})

	// Widen attempts: the accept body has no access field (400, unknown
	// field), and the invite stays usable.
	for _, extra := range []map[string]any{{"teamAccess": "all"}, {"teams": []string{"ORB", "INVB"}}, {"role": "admin"}} {
		body := map[string]any{"token": ci.Secret, "name": "Ann", "username": "ann"}
		for k, v := range extra {
			body[k] = v
		}
		if r := e.do("POST", "/api/v1/invites/accept", "", body); r.status != http.StatusBadRequest {
			t.Fatalf("accept with %v: %d %s, want 400", extra, r.status, r.body)
		}
	}
	if !e.inviteOpen(ci.Invite.ID) || e.count(`SELECT count(*) FROM users WHERE username = 'ann'`) != 0 {
		t.Fatal("a refused accept used the invite or made a user")
	}

	r := e.must(http.StatusCreated, e.accept(ci.Secret, "ann"))
	ann := e.userID("ann")
	if got := e.accessOf(ann); got != "selected:INVA" {
		t.Fatalf("access after accept %q, want selected:INVA", got)
	}
	sess := e.asSession(r.cookies()[e.names.Session].Value)
	teams := e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/teams", sess, nil))
	if !strings.Contains(string(teams.body), `"INVA"`) || strings.Contains(string(teams.body), `"ORB"`) || strings.Contains(string(teams.body), `"INVB"`) {
		t.Fatalf("the new member's teams: %s", teams.body)
	}
	orbIssue := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "orb only"})
	if r := e.do("GET", "/api/v1/orgs/orbit/issues/"+orbIssue.Identifier, sess, nil); r.status != http.StatusNotFound {
		t.Fatalf("the new member reads an ORB issue: %d", r.status)
	}
	if got := e.listInvites(e.chen)[ci.Invite.ID]; got.Status != "accepted" || strings.Join(got.Teams, ",") != "INVA" {
		t.Fatalf("accepted invite %+v", got)
	}

	// An install-level user (a member of another org) joins with the
	// invite's access; no session.
	other := e.newOrg("other", "OTH")
	out := e.newUser("outsider")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET email = 'outsider@example.com' WHERE id = $1`, out.ID); err != nil {
		t.Fatal(err)
	}
	e.addMember(other, out.ID, "admin")
	oi := e.mustInvite(map[string]any{"email": "outsider@example.com", "teamAccess": "selected", "teams": []string{"INVB", "INVC"}})
	e.must(http.StatusOK, e.accept(oi.Secret, ""))
	if got := e.accessOf(out.ID); got != "selected:INVB,INVC" {
		t.Fatalf("joined access %q", got)
	}
	var otherFlag string
	if err := e.pool.QueryRow(context.Background(), `SELECT team_access FROM org_members WHERE org_id = $1 AND user_id = $2`, other, out.ID).Scan(&otherFlag); err != nil || otherFlag != "all" {
		t.Fatalf("the other org's membership changed: %q %v", otherFlag, err)
	}

	// An existing member keeps their access: the invite was made before the
	// person joined another way (create refuses a member's email).
	late := e.newUser("late")
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET email = 'late@example.com' WHERE id = $1`, late.ID); err != nil {
		t.Fatal(err)
	}
	li := e.mustInvite(map[string]any{"email": "late@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	e.addMember(e.orgID, late.ID, "member")
	e.must(http.StatusOK, e.accept(li.Secret, ""))
	if got := e.accessOf(late.ID); got != "all:" {
		t.Fatalf("an existing member's access changed to %q", got)
	}

	// An all invite and an admin invite give all.
	ai := e.mustInvite(map[string]any{"email": "allp@example.com"})
	e.must(http.StatusCreated, e.accept(ai.Secret, "allp"))
	if got := e.accessOf(e.userID("allp")); got != "all:" {
		t.Fatalf("all invite gave %q", got)
	}
	di := e.mustInvite(map[string]any{"email": "boss@example.com", "role": "admin"})
	e.must(http.StatusCreated, e.accept(di.Secret, "boss"))
	if got := e.accessOf(e.userID("boss")); got != "all:" {
		t.Fatalf("admin invite gave %q", got)
	}
}

// AC (#27): a team deleted between the invite and the accept is skipped;
// when every team is gone the accept is 409, writes nothing, names no team,
// and the invite stays open until an admin revokes it.
func TestInviteAccess_TeamsDeleted(t *testing.T) {
	e := newInviteEnv(t, envOpts{})
	some := e.mustInvite(map[string]any{"email": "some@example.com", "teamAccess": "selected", "teams": []string{"INVA", "INVB"}})
	gone := e.mustInvite(map[string]any{"email": "gone@example.com", "teamAccess": "selected", "teams": []string{"INVC"}})
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/teams/INVA", e.chen, nil))
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/teams/INVC", e.chen, nil))
	list := e.listInvites(e.chen)
	if got := list[some.Invite.ID]; got.TeamAccess != "selected" || strings.Join(got.Teams, ",") != "INVB" {
		t.Fatalf("listed after a delete %+v", got)
	}
	if got := list[gone.Invite.ID]; got.TeamAccess != "selected" || got.Teams == nil || len(got.Teams) != 0 || got.Status != "pending" {
		t.Fatalf("listed with every team gone %+v", got)
	}

	e.must(http.StatusCreated, e.accept(some.Secret, "some"))
	if got := e.accessOf(e.userID("some")); got != "selected:INVB" {
		t.Fatalf("access %q, want selected:INVB", got)
	}

	users := e.count(`SELECT count(*) FROM users`)
	members := e.count(`SELECT count(*) FROM org_members`)
	r := e.accept(gone.Secret, "gone")
	if r.status != http.StatusConflict || r.errCode(t) != "conflict" || strings.Contains(string(r.body), "INVC") {
		t.Fatalf("accept with every team gone: %d %s, want 409 naming no team", r.status, r.body)
	}
	if !e.inviteOpen(gone.Invite.ID) || e.count(`SELECT count(*) FROM users`) != users || e.count(`SELECT count(*) FROM org_members`) != members {
		t.Fatal("a refused accept used the invite or wrote a row")
	}
	// The same 409 again (the invite is still open), then the admin revokes it.
	if r2 := e.accept(gone.Secret, "gone"); r2.status != http.StatusConflict || string(r2.body) != string(r.body) {
		t.Fatalf("second try: %d %s", r2.status, r2.body)
	}
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+gone.Invite.ID.String(), e.chen, nil))
	if r := e.accept(gone.Secret, "gone"); r.status != http.StatusUnauthorized || r.errCode(t) != "invalid_invite" {
		t.Fatalf("a revoked invite: %d %s", r.status, r.body)
	}
}

// AC (#65, entry 2): a selected invite is refused while an enabled
// repository has no team (naming it, an admin route); an open selected
// invite is restricted access, so it blocks a repository with no team; at
// accept the guard runs again and answers without a repository name, and
// the invite stays usable. An all invite is never refused. A revoked or an
// expired selected invite no longer counts.
func TestInviteAccess_WebhookGuard(t *testing.T) {
	e := newInviteEnv(t, envOpts{})
	gi := e.glSetup("orbit", e.chen) // a repository with no team
	repo := e.repoPath(gi, e.repoID(gi))
	before := e.inviteRows()
	guardRefused(t, e.invite(map[string]any{"email": "g1@example.com", "teamAccess": "selected", "teams": []string{"INVA"}}), "example-org/example-app")
	if e.inviteRows() != before {
		t.Fatal("a refused invite wrote a row")
	}
	e.mustInvite(map[string]any{"email": "g0@example.com"}) // all: restricts nobody

	// A sign-up approval with selected is refused the same way (entry 3).
	e.setSignupMode("approval")
	e.smail.ready.Store(true)
	e.ask("sg@example.com", "sg")
	e.confirm("sg@example.com")
	e.smail.ready.Store(false)
	sid := e.signupID("sg@example.com")
	guardRefused(t, e.do("POST", "/api/v1/orgs/orbit/signups/"+sid+"/approve", e.chen, sel("INVA")), "example-org/example-app")
	if e.count(`SELECT count(*) FROM users WHERE username = 'sg'`) != 0 || e.count(`SELECT count(*) FROM signup_requests WHERE id = $1 AND status = 'pending'`, sid) != 1 {
		t.Fatal("a refused approval made the user or decided the request")
	}

	// With a team on the repository the invite passes.
	e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": "INVA"}))
	ci := e.mustInvite(map[string]any{"email": "g1@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	// The open invite blocks clearing the team (bypass test: no member is
	// restricted, only the invite).
	if n := e.count(`SELECT count(*) FROM org_members WHERE org_id = $1 AND team_access = 'selected'`, e.orgID); n != 0 {
		t.Fatalf("%d restricted members; the bypass test needs none", n)
	}
	guardRefused(t, e.do("PATCH", repo, e.chen, map[string]any{"teamId": nil}), "example-org/example-app")
	// A new repository with no team is refused too.
	guardRefused(t, e.do("POST", "/api/v1/orgs/orbit/integrations/"+gi.String()+"/repos", e.chen,
		map[string]any{"repoKey": "4242", "repoPath": "example-org/second"}), "")

	// The repository loses its team between create and accept (a direct
	// write: no route allows it now). The accept is 409, names no
	// repository, writes nothing; the invite stays usable.
	if _, err := e.pool.Exec(context.Background(), `UPDATE git_repo_map SET team_id = NULL WHERE org_id = $1`, e.orgID); err != nil {
		t.Fatal(err)
	}
	users := e.count(`SELECT count(*) FROM users`)
	r := e.accept(ci.Secret, "gone1")
	if r.status != http.StatusConflict || r.errCode(t) != "conflict" || strings.Contains(string(r.body), "example-org") || strings.Contains(string(r.body), "example-app") {
		t.Fatalf("accept under the guard: %d %s, want a 409 that names no repository", r.status, r.body)
	}
	if !e.inviteOpen(ci.Invite.ID) || e.count(`SELECT count(*) FROM users`) != users {
		t.Fatal("a refused accept used the invite or made a user")
	}
	// An all invite accepts whatever the repositories are.
	ai := e.mustInvite(map[string]any{"email": "g2@example.com"})
	e.must(http.StatusCreated, e.accept(ai.Secret, "gtwo"))
	// Once the repository has a team again, the same link works.
	e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": "INVA"}))
	e.must(http.StatusCreated, e.accept(ci.Secret, "gone1"))
	if got := e.accessOf(e.userID("gone1")); got != "selected:INVA" {
		t.Fatalf("access %q", got)
	}
	// The member now counts; make them all again so only invites count below.
	e.must(http.StatusOK, e.do("PUT", "/api/v1/orgs/orbit/users/gone1/team-access", e.chen, map[string]any{"teamAccess": "all"}))

	// A revoked selected invite no longer counts.
	rv := e.mustInvite(map[string]any{"email": "g3@example.com", "teamAccess": "selected", "teams": []string{"INVB"}})
	guardRefused(t, e.do("PATCH", repo, e.chen, map[string]any{"teamId": nil}), "example-org/example-app")
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+rv.Invite.ID.String(), e.chen, nil))
	e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": nil}))
	// An expired one neither (it can never be used).
	e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": "INVA"}))
	ex := e.mustInvite(map[string]any{"email": "g4@example.com", "teamAccess": "selected", "teams": []string{"INVB"}})
	guardRefused(t, e.do("PATCH", repo, e.chen, map[string]any{"teamId": nil}), "example-org/example-app")
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_invites SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE id = $1`, ex.Invite.ID); err != nil {
		t.Fatal(err)
	}
	e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": nil}))
}

// AC (#29): the approval takes an optional body. No body and {} are all;
// selected writes the teams in the approval's transaction; bad bodies are
// 400 and leave the request pending; a replay of an approved request
// changes nothing whatever its body.
func TestSignupApproveAccess(t *testing.T) {
	e := newInviteEnv(t, envOpts{})
	e.setSignupMode("approval")
	e.smail.ready.Store(true)
	for _, u := range []string{"pa", "pb", "pc", "pd"} {
		e.ask(u+"@example.com", u)
		e.confirm(u + "@example.com")
	}
	path := func(u string) string {
		return "/api/v1/orgs/orbit/signups/" + e.signupID(u+"@example.com") + "/approve"
	}

	other := e.newOrg("elsewhere", "ELS")
	var els uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1`, other).Scan(&els); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]any{
		"selected with no teams": {"teamAccess": "selected"},
		"all with teams":         {"teamAccess": "all", "teams": []string{"INVA"}},
		"a bad kind":             {"teamAccess": "some"},
		"an unknown team":        sel("NOPE"),
		"another org's team":     sel("ELS"),
		"another org's team id":  sel(els.String()),
	} {
		r := e.do("POST", path("pa"), e.chen, body)
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if r := e.do("POST", path("pa"), e.chen, map[string]any{"teamAccess": "selected", "teams": []string{"INVA"}, "role": "admin"}); r.status != http.StatusBadRequest {
		t.Errorf("an unknown field: %d", r.status)
	}
	if e.count(`SELECT count(*) FROM users WHERE username = 'pa'`) != 0 {
		t.Fatal("a refused approval made the user")
	}
	// Only an org admin approves.
	if r := e.do("POST", path("pa"), e.ritaTok, sel("INVA")); r.status != http.StatusForbidden {
		t.Fatalf("a member approves: %d", r.status)
	}

	var req signup.Request
	e.must(http.StatusOK, e.do("POST", path("pa"), e.chen, sel("INVA", "INVB"), "Idempotency-Key", "appr-pa")).json(t, &req)
	if req.Status != "approved" || e.accessOf(e.userID("pa")) != "selected:INVA,INVB" {
		t.Fatalf("approved %+v, access %q", req, e.accessOf(e.userID("pa")))
	}
	// A replay with another body (and another key) changes nothing.
	e.must(http.StatusOK, e.do("POST", path("pa"), e.chen, map[string]any{"teamAccess": "all"}))
	e.must(http.StatusOK, e.do("POST", path("pa"), e.chen, sel("INVC")))
	if got := e.accessOf(e.userID("pa")); got != "selected:INVA,INVB" {
		t.Fatalf("a replay changed the access to %q", got)
	}
	e.must(http.StatusOK, e.do("POST", path("pb"), e.chen, nil))
	e.must(http.StatusOK, e.do("POST", path("pc"), e.chen, map[string]any{}))
	e.must(http.StatusOK, e.do("POST", path("pd"), e.chen, map[string]any{"teamAccess": "all"}))
	for _, u := range []string{"pb", "pc", "pd"} {
		if got := e.accessOf(e.userID(u)); got != "all:" {
			t.Fatalf("%s: access %q, want all", u, got)
		}
	}
}

// AC (cross-org): another org's admin cannot list, make or revoke this
// org's invites (404), and cannot name this org's teams in their own (400).
func TestInviteAccess_CrossOrg(t *testing.T) {
	e := newInviteEnv(t, envOpts{})
	other := e.newOrg("elsewhere", "ELS")
	otto := testdb.AddMember(t, e.pool, other, "otto", "admin")
	ottoTok, _ := e.newToken(otto, tokenOpts{pin: &other})
	ottoFree, _ := e.newToken(otto, tokenOpts{})
	ci := e.mustInvite(map[string]any{"email": "co@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	before := e.inviteRows()
	for _, tok := range []string{ottoTok, ottoFree} {
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{"GET", "/api/v1/orgs/orbit/invites", nil},
			{"POST", "/api/v1/orgs/orbit/invites", map[string]any{"email": "z@example.com", "teamAccess": "selected", "teams": []string{"INVA"}}},
			{"DELETE", "/api/v1/orgs/orbit/invites/" + ci.Invite.ID.String(), nil},
		} {
			if r := e.do(c.method, c.path, tok, c.body); r.status != http.StatusNotFound {
				t.Errorf("%s %s: %d %s, want 404", c.method, c.path, r.status, r.body)
			}
		}
	}
	inva := e.teamID("INVA")
	for _, ref := range []string{"INVA", inva.String()} {
		r := e.do("POST", "/api/v1/orgs/elsewhere/invites", ottoFree, map[string]any{"email": "z@example.com", "teamAccess": "selected", "teams": []string{ref}})
		if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "unknown team") {
			t.Errorf("this org's team %s in another org's invite: %d %s", ref, r.status, r.body)
		}
	}
	// Its own revoke of this org's invite id is 404 too.
	if r := e.do("DELETE", "/api/v1/orgs/elsewhere/invites/"+ci.Invite.ID.String(), ottoFree, nil); r.status != http.StatusNotFound {
		t.Errorf("revoke another org's invite through its own org: %d", r.status)
	}
	if e.inviteRows() != before || !e.inviteOpen(ci.Invite.ID) {
		t.Fatal("a cross-org call changed this org's invites")
	}
	// An invite of another org with its own team: the accept restricts the
	// person there only.
	oi := e.do("POST", "/api/v1/orgs/elsewhere/invites", ottoFree, map[string]any{"email": "z@example.com", "teamAccess": "selected", "teams": []string{"ELS"}})
	var oci signup.CreatedInvite
	e.must(http.StatusCreated, oi).json(t, &oci)
	e.must(http.StatusCreated, e.accept(oci.Secret, "zed"))
	var flag string
	if err := e.pool.QueryRow(context.Background(), `SELECT team_access FROM org_members WHERE org_id = $1 AND user_id = $2`, other, e.userID("zed")).Scan(&flag); err != nil || flag != "selected" {
		t.Fatalf("zed in elsewhere: %q %v", flag, err)
	}
	if got := e.accessOf(e.userID("zed")); got != "none" {
		t.Fatalf("zed in orbit: %q", got)
	}
}

// AC (T12 #6, #20, decisions-T20 #10): the accept keeps its floor and one
// answer. A wrong token and a used, revoked or expired selected invite give
// the same 401 bytes; the 409s of a selected invite (teams gone, the
// guard) wait for the floor as well.
func TestInviteAccess_NoOracle(t *testing.T) {
	const floor = 150 * time.Millisecond
	e := newInviteEnv(t, envOpts{signup: signup.Options{Floor: floor}})
	used := e.mustInvite(map[string]any{"email": "o1@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	e.must(http.StatusCreated, e.accept(used.Secret, "oone"))
	revoked := e.mustInvite(map[string]any{"email": "o2@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/invites/"+revoked.Invite.ID.String(), e.chen, nil))
	expired := e.mustInvite(map[string]any{"email": "o3@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_invites SET created_at = now() - interval '8 days', expires_at = now() - interval '1 day' WHERE id = $1`, expired.Invite.ID); err != nil {
		t.Fatal(err)
	}
	gone := e.mustInvite(map[string]any{"email": "o4@example.com", "teamAccess": "selected", "teams": []string{"INVC"}})
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/teams/INVC", e.chen, nil))

	var first resp
	for i, c := range []struct{ name, token string }{
		{"a wrong token", strings.Repeat("C", 43)},
		{"a used selected invite", used.Secret},
		{"a revoked selected invite", revoked.Secret},
		{"an expired selected invite", expired.Secret},
	} {
		start := time.Now()
		r := e.accept(c.token, fmt.Sprintf("onew%d", i))
		if el := time.Since(start); el < floor {
			t.Fatalf("%s: answered in %v, under the floor", c.name, el)
		}
		if r.status != http.StatusUnauthorized {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		if i == 0 {
			first = r
			continue
		}
		if string(r.body) != string(first.body) || headerShape(r.header) != headerShape(first.header) {
			t.Fatalf("%s answers differently: %s / %s", c.name, r.body, first.body)
		}
	}
	start := time.Now()
	if r := e.accept(gone.Secret, "ogone"); r.status != http.StatusConflict {
		t.Fatalf("teams gone: %d %s", r.status, r.body)
	}
	if el := time.Since(start); el < floor {
		t.Fatalf("the teams-gone 409 answered in %v, under the floor", el)
	}
}

// AC (T12 #14, #25; AGENTS.md): the selected flows put no secret in a log
// line, an event, an idempotency row or a stored row, emit no change-feed
// event, and their audit lines name the access kind.
func TestInviteAccess_NoSecretsNoEvents(t *testing.T) {
	e := newInviteEnv(t, envOpts{logLevel: slog.LevelDebug})
	seq := latestSeq(t, e.env)
	ci := e.mustInvite(map[string]any{"email": "q1@example.com", "teamAccess": "selected", "teams": []string{"INVA"}})
	r := e.must(http.StatusCreated, e.accept(ci.Secret, "qone"))
	sess := r.cookies()[e.names.Session].Value
	e.smail.ready.Store(true)
	e.mustInvite(map[string]any{"email": "q2@example.com", "teamAccess": "selected", "teams": []string{"INVB"}})
	mailed := e.mailedInviteToken("q2@example.com")
	e.must(http.StatusCreated, e.accept(mailed, "qtwo"))
	if got := e.accessOf(e.userID("qtwo")); got != "selected:INVB" {
		t.Fatalf("emailed invite gave %q", got)
	}
	e.setSignupMode("approval")
	e.ask("q3@example.com", "qthree")
	code := e.lastSignupCode("q3@example.com")
	e.confirm("q3@example.com")
	e.must(http.StatusOK, e.do("POST", "/api/v1/orgs/orbit/signups/"+e.signupID("q3@example.com")+"/approve", e.chen, sel("INVC"), "Idempotency-Key", "sec-appr-sel"))
	if got := latestSeq(t, e.env); got != seq {
		t.Fatalf("the invite and approval flows emitted events (seq %d -> %d)", seq, got)
	}
	logs := e.logs.String()
	for _, want := range []string{"audit: invite created", "audit: invite accepted", "audit: sign-up approved", "team_access=selected"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("the log has no %q; the check would be vacuous", want)
		}
	}
	var idem, rows string
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(key || ' ' || path || ' ' || coalesce(convert_from(response_body, 'UTF8'), ''), E'\n'), '') FROM idempotency_keys`).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT (SELECT coalesce(string_agg(row_to_json(s)::text, E'\n'), '') FROM signup_requests s)
		|| (SELECT coalesce(string_agg(row_to_json(i)::text, E'\n'), '') FROM org_invites i)
		|| (SELECT coalesce(string_agg(row_to_json(t)::text, E'\n'), '') FROM org_invite_teams t)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idem, "sec-appr-sel") {
		t.Fatal("the approval's idempotency row is missing; the check would be vacuous")
	}
	for _, sec := range []string{ci.Secret, mailed, sess, code} {
		if sec == "" {
			t.Fatal("a secret is empty; the check would be vacuous")
		}
		for where, s := range map[string]string{"the log": logs, "an idempotency row": idem, "a stored row": rows} {
			if containsSecret(s, sec) {
				t.Fatalf("%s holds a secret (%d characters)", where, len(sec))
			}
		}
	}
}
