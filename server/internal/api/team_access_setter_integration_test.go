package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/events"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 9 of decisions-T20 (LAUN-225): the team access setter
// PUT /users/{user}/team-access (#48 to #50), the access fields of the member
// list (#33) and of POST /agents (#31). It is the first write that makes a
// member restricted.

// accessEnv is the fixture org with team HID next to ORB, a member rita
// (team_access 'all') with a token of every scope, and an issue in each team.
type accessEnv struct {
	*env
	rita             uuid.UUID
	ritaTok          string
	orb, hid         uuid.UUID
	hidIssue, orbIss tracker.Issue
}

func newAccessEnv(t *testing.T) *accessEnv {
	return newAccessEnvWith(t, envOpts{})
}

func newAccessEnvWith(t *testing.T, o envOpts) *accessEnv {
	t.Helper()
	e := newEnvWith(t, o)
	a := &accessEnv{env: e}
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "HID", "name": "Hidden"}))
	a.orb, a.hid = e.teamID(testdb.TeamKey), e.teamID("HID")
	a.rita = testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	a.ritaTok, _ = e.newToken(a.rita, tokenOpts{pin: &e.orgID})
	a.hidIssue = e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden plan"})
	a.orbIss = e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible plan"})
	return a
}

func accessPath(user string) string { return "/api/v1/orgs/orbit/users/" + user + "/team-access" }

// put calls the setter as token and returns the response.
func (a *accessEnv) put(token, user string, body any, query string) resp {
	a.t.Helper()
	return a.do("PUT", accessPath(user)+query, token, body)
}

// set calls the setter as the admin and wants 200.
func (a *accessEnv) set(user string, body any) tracker.TeamAccessResult {
	a.t.Helper()
	var res tracker.TeamAccessResult
	a.must(http.StatusOK, a.put(a.chen, user, body, "")).json(a.t, &res)
	return res
}

// accessState is the stored access of a member: the flag and the team rows.
func (a *accessEnv) accessState(user uuid.UUID) string {
	a.t.Helper()
	var flag, rows string
	if err := a.pool.QueryRow(context.Background(), `SELECT m.team_access,
		coalesce((SELECT string_agg(t.key, ',' ORDER BY t.key) FROM team_members tm JOIN teams t ON t.id = tm.team_id
		          WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id), '')
		FROM org_members m WHERE m.org_id = $1 AND m.user_id = $2`, a.orgID, user).Scan(&flag, &rows); err != nil {
		a.t.Fatal(err)
	}
	return flag + ":" + rows
}

// everything is a snapshot of the rows a refused or dry setter must not
// change.
func (a *accessEnv) everything() string {
	a.t.Helper()
	var s []string
	for _, tbl := range []string{"org_members", "team_members", "issue_subscriptions", "team_follows", "notification_settings"} {
		var out string
		if err := a.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY row_to_json(x)::text), '')
			FROM `+tbl+` x WHERE x.org_id = $1`, a.orgID).Scan(&out); err != nil {
			a.t.Fatal(err)
		}
		s = append(s, out)
	}
	var issues string
	if err := a.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(id::text || ':' || coalesce(assignee_id::text, '-') || ':' || version, ',' ORDER BY id), '')
		FROM issues WHERE org_id = $1`, a.orgID).Scan(&issues); err != nil {
		a.t.Fatal(err)
	}
	return strings.Join(append(s, issues), "|")
}

func idList(us ...uuid.UUID) []uuid.UUID { return us }

func sameIDs(got, want []uuid.UUID) bool {
	g, w := slices.Clone(got), slices.Clone(want)
	cmp := func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) }
	slices.SortFunc(g, cmp)
	slices.SortFunc(w, cmp)
	return slices.Equal(g, w)
}

// AC: a restricted member really loses access at once, through the setter.
// rita ('all') reads team HID; the admin sets her to ORB only; her next
// requests answer HID as unknown, her lists and feed leave it out, her open
// stream sends `access`; the admin's event is admin-only and holds ids and
// counts only; set back to all, she reads HID again. An all member and the
// admin are unchanged throughout.
func TestTeamAccessSetter_RoundTrip(t *testing.T) {
	a := newAccessEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	e := a.env
	issuePath := "/api/v1/orgs/orbit/issues/" + a.hidIssue.Identifier
	e.must(http.StatusOK, e.do("GET", issuePath, a.ritaTok, nil))
	f := &teamFixture{e: e}
	s := f.openStream(t, a.ritaTok)
	defer s.end()

	res := a.set("rita", map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}})
	if !res.Changed || res.DryRun || res.UserID != a.rita || res.TeamAccess != "selected" ||
		!sameIDs(res.TeamIDs, idList(a.orb)) || !sameIDs(res.LostTeamIDs, idList(a.hid)) || len(res.AddedTeamIDs) != 0 {
		t.Fatalf("setter answer %+v", res)
	}
	if got := a.accessState(a.rita); got != "selected:"+testdb.TeamKey {
		t.Fatalf("stored access %q", got)
	}

	// The stream: access first, then only visible events.
	ev, _, ok := s.next()
	if !ok || ev.typ != "access" {
		t.Fatalf("stream after the change: %+v ok=%v, want access", ev, ok)
	}
	hidden := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "after the change"})
	visible := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible after"})
	ev, _, ok = s.next()
	if !ok || ev.typ != events.IssueCreated || !strings.Contains(ev.data, visible.ID.String()) || strings.Contains(ev.data, hidden.ID.String()) {
		t.Fatalf("stream: %+v ok=%v, want only the visible issue", ev, ok)
	}

	// The next requests.
	missing := e.do("GET", "/api/v1/orgs/orbit/issues/HID-999", a.ritaTok, nil)
	got := e.do("GET", issuePath, a.ritaTok, nil)
	if got.status != http.StatusNotFound || string(got.body) != strings.ReplaceAll(string(missing.body), "HID-999", a.hidIssue.Identifier) {
		t.Fatalf("hidden issue: %d %s; missing: %d %s", got.status, got.body, missing.status, missing.body)
	}
	var list page[tracker.Issue]
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/issues?limit=100", a.ritaTok, nil)).json(t, &list)
	for _, is := range list.Items {
		if is.Team.Key == "HID" {
			t.Fatalf("the list holds %s", is.Identifier)
		}
	}
	var teams page[tracker.Team]
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/teams", a.ritaTok, nil)).json(t, &teams)
	if len(teams.Items) != 1 || teams.Items[0].Key != testdb.TeamKey {
		t.Fatalf("teams: %+v", teams.Items)
	}
	var feed page[tracker.Event]
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/events?limit=500&after=0", a.ritaTok, nil)).json(t, &feed)
	for _, ev := range feed.Items {
		if ev.EntityID == hidden.ID || ev.Type == events.MemberAccessChanged {
			t.Fatalf("rita's feed holds %s of %s", ev.Type, ev.EntityID)
		}
	}

	// The event: admin only, ids and counts only.
	var scope, payload string
	var actor uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT scope, payload::text, actor_id FROM events
		WHERE org_id = $1 AND type = $2 AND entity_id = $3`, e.orgID, events.MemberAccessChanged, a.rita).Scan(&scope, &payload, &actor); err != nil {
		t.Fatal(err)
	}
	if scope != "admin" || actor != e.user("chen").ID {
		t.Fatalf("member.access_changed scope %q actor %s", scope, actor)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range p {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"addedTeamIds", "lostTeamIds", "teamAccess", "teamIds", "unassigned", "userId"}; !slices.Equal(keys, want) {
		t.Fatalf("payload keys %v, want %v", keys, want)
	}
	for _, name := range []string{"rita", "Hidden", testdb.TeamKey, "HID"} {
		if strings.Contains(payload, `"`+name+`"`) {
			t.Fatalf("payload names %q: %s", name, payload)
		}
	}
	claudeAll, _ := e.newToken(e.user("claude").ID, tokenOpts{pin: &e.orgID})
	for who, tok := range map[string]string{"admin": e.chen, "all member": claudeAll} {
		var fd page[tracker.Event]
		e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/events?limit=500&after=0", tok, nil)).json(t, &fd)
		seen := slices.ContainsFunc(fd.Items, func(ev tracker.Event) bool { return ev.Type == events.MemberAccessChanged })
		if seen != (who == "admin") {
			t.Errorf("%s reads member.access_changed: %v", who, seen)
		}
		// Admins and all members still read team HID.
		e.must(http.StatusOK, e.do("GET", issuePath, tok, nil))
	}
	// The audit line: ids and counts, no name of a team.
	logs := e.logs.String()
	if strings.Count(logs, "audit: team access changed") != 1 || !strings.Contains(logs, "user_id="+a.rita.String()) {
		t.Fatalf("audit line missing or repeated:\n%s", logs)
	}

	// Back to all.
	back := a.set("rita", map[string]any{"teamAccess": "all"})
	if !back.Changed || back.TeamAccess != "all" || len(back.TeamIDs) != 0 || !sameIDs(back.AddedTeamIDs, idList(a.hid)) || len(back.LostTeamIDs) != 0 {
		t.Fatalf("back to all: %+v", back)
	}
	if got := a.accessState(a.rita); got != "all:" {
		t.Fatalf("stored access %q", got)
	}
	e.must(http.StatusOK, e.do("GET", issuePath, a.ritaTok, nil))
	ev, _, ok = s.next()
	if !ok || ev.typ != "access" {
		t.Fatalf("stream after the way back: %+v ok=%v, want access", ev, ok)
	}

	// No team at all is allowed on an edit (#5).
	none := a.set("rita", map[string]any{"teamAccess": "selected", "teams": []string{}})
	if !none.Changed || len(none.TeamIDs) != 0 || !sameIDs(none.LostTeamIDs, idList(a.orb, a.hid)) {
		t.Fatalf("no team: %+v", none)
	}
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/teams", a.ritaTok, nil)).json(t, &teams)
	if len(teams.Items) != 0 {
		t.Fatalf("a member with no team lists %+v", teams.Items)
	}
}

// AC: the member list shows each member's access to admins only (#33).
func TestTeamAccessSetter_MemberFields(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	a.set("rita", map[string]any{"teamAccess": "selected", "teams": []string{"hid", a.orb.String()}})
	members := func(tok string) map[string]tracker.Member {
		var p page[tracker.Member]
		e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/users", tok, nil)).json(t, &p)
		out := map[string]tracker.Member{}
		for _, m := range p.Items {
			out[m.Username] = m
		}
		return out
	}
	admin := members(e.chen)
	if m := admin["rita"]; m.TeamAccess == nil || *m.TeamAccess != "selected" || !slices.Equal(m.Teams, []string{"HID", testdb.TeamKey}) {
		t.Fatalf("admin sees rita as %v %v", m.TeamAccess, m.Teams)
	}
	for _, name := range []string{"chen", "claude"} {
		if m := admin[name]; m.TeamAccess == nil || *m.TeamAccess != "all" || m.Teams == nil || len(m.Teams) != 0 {
			t.Fatalf("admin sees %s as %v %v", name, m.TeamAccess, m.Teams)
		}
	}
	// An admin whose stored flag says selected is still all (#2).
	testdb.MustExec(t, e.pool, `UPDATE org_members SET team_access = 'selected' WHERE org_id = $1 AND user_id = $2`, e.orgID, e.user("chen").ID)
	if m := members(e.chen)["chen"]; *m.TeamAccess != "all" {
		t.Fatalf("an admin with the flag selected shows %v", *m.TeamAccess)
	}
	testdb.MustExec(t, e.pool, `UPDATE org_members SET team_access = 'all' WHERE org_id = $1 AND user_id = $2`, e.orgID, e.user("chen").ID)
	// Any other caller: null, for every member, the caller included.
	r := e.do("GET", "/api/v1/orgs/orbit/users", a.ritaTok, nil)
	e.must(http.StatusOK, r)
	if strings.Contains(string(r.body), `"selected"`) || strings.Contains(string(r.body), `"HID"`) {
		t.Fatalf("a member reads access data: %s", r.body)
	}
	for name, m := range members(a.ritaTok) {
		if m.TeamAccess != nil || m.Teams != nil {
			t.Fatalf("rita sees %s as %v %v", name, m.TeamAccess, m.Teams)
		}
	}
}

// AC: refusals. Only an org admin calls it (403 for a member, a restricted
// member and an agent, whatever their token scopes); an admin target (the
// caller itself included: the last admin cannot demote itself) is 400 and
// changes nothing; bad bodies are 400; an unknown user is 404; no JSON is
// 415.
func TestTeamAccessSetter_Refusals(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	before := a.everything()
	seq := latestSeq(t, e)

	claudeTok, _ := e.newToken(e.user("claude").ID, tokenOpts{pin: &e.orgID}) // an agent, every scope
	mona := testdb.AddMember(t, e.pool, e.orgID, "mona", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, mona, a.orb)
	monaTok, _ := e.newToken(mona, tokenOpts{pin: &e.orgID})
	before = a.everything()
	sel := map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}}
	for who, tok := range map[string]string{"member": a.ritaTok, "restricted member": monaTok, "agent": claudeTok, "agent fixture token": e.claude} {
		// A made-up user too: a non-admin gets 403 whether or not the user exists.
		for _, target := range []string{"rita", "mona", "chen", "claude", uuid.NewString(), "nobody"} {
			r := a.put(tok, target, sel, "")
			if code := r.errCode(t); r.status != http.StatusForbidden || (code != "forbidden" && code != "insufficient_scope") {
				t.Errorf("%s on %s: %d %s, want 403", who, target, r.status, r.body)
			}
		}
	}

	// Admin targets: the caller itself (the only admin) and another admin.
	ada := testdb.AddMember(t, e.pool, e.orgID, "ada", "admin")
	before = a.everything()
	for _, target := range []string{"chen", e.user("chen").ID.String(), "ada", ada.String()} {
		for _, body := range []any{sel, map[string]any{"teamAccess": "all"}} {
			r := a.put(e.chen, target, body, "")
			if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" || !strings.Contains(string(r.body), "admin") {
				t.Errorf("admin target %s: %d %s, want 400", target, r.status, r.body)
			}
		}
	}
	var role, flag string
	if err := e.pool.QueryRow(context.Background(), `SELECT role, team_access FROM org_members WHERE org_id = $1 AND user_id = $2`,
		e.orgID, e.user("chen").ID).Scan(&role, &flag); err != nil || role != "admin" || flag != "all" {
		t.Fatalf("the last admin is now %s %s (%v)", role, flag, err)
	}

	// Validation.
	for name, c := range map[string]struct {
		body  any
		query string
	}{
		"no teamAccess":           {map[string]any{"teams": []string{testdb.TeamKey}}, ""},
		"unknown teamAccess":      {map[string]any{"teamAccess": "some"}, ""},
		"teams with all":          {map[string]any{"teamAccess": "all", "teams": []string{testdb.TeamKey}}, ""},
		"unknown team":            {map[string]any{"teamAccess": "selected", "teams": []string{"NOPE"}}, ""},
		"made-up team id":         {map[string]any{"teamAccess": "selected", "teams": []string{uuid.NewString()}}, ""},
		"bad assignedIssues":      {map[string]any{"teamAccess": "selected", "teams": []string{}, "assignedIssues": "drop"}, ""},
		"bad dryRun":              {sel, "?dryRun=maybe"},
		"unknown field":           {map[string]any{"teamAccess": "all", "role": "admin"}, ""},
		"too many teams":          {map[string]any{"teamAccess": "selected", "teams": slices.Repeat([]string{testdb.TeamKey}, 51)}, ""},
		"teams of the wrong type": {map[string]any{"teamAccess": "selected", "teams": "ORB"}, ""},
	} {
		r := a.put(e.chen, "rita", c.body, c.query)
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	for _, target := range []string{"nobody", uuid.NewString(), "Rita-"} {
		r := a.put(e.chen, target, sel, "")
		if r.status != http.StatusNotFound || r.errCode(t) != "not_found" {
			t.Errorf("unknown user %s: %d %s, want 404", target, r.status, r.body)
		}
	}
	// A user of the install who is not a member of this org is unknown here.
	e.newUser("stranger")
	if r := a.put(e.chen, "stranger", sel, ""); r.status != http.StatusNotFound {
		t.Errorf("a non-member: %d %s, want 404", r.status, r.body)
	}
	if r := e.do("PUT", accessPath("rita"), e.chen, nil); r.status != http.StatusUnsupportedMediaType {
		t.Errorf("no body: %d %s, want 415", r.status, r.body)
	}
	if a.everything() != before {
		t.Fatal("a refused call changed a row")
	}
	if latestSeq(t, e) != seq {
		t.Fatal("a refused call emitted an event")
	}
	// The username form is case-insensitive, as usernames are lower case.
	if r := a.set("RITA", map[string]any{"teamAccess": "all"}); r.Changed {
		t.Fatalf("all to all changed: %+v", r)
	}
}

// AC: another org's admin gets 404 for a user of this org, and another org's
// team in the body is 400 unknown team (AGENTS.md cross-org rule).
func TestTeamAccessSetter_CrossOrg(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	other := e.newOrg("elsewhere", "ELS")
	otto := testdb.AddMember(t, e.pool, other, "otto", "admin")
	ottoTok, _ := e.newToken(otto, tokenOpts{pin: &other})
	ottoFree, _ := e.newToken(otto, tokenOpts{})
	before, seq := a.everything(), latestSeq(t, e)
	sel := map[string]any{"teamAccess": "selected", "teams": []string{"ELS"}}
	for name, path := range map[string]string{
		"this org's path, pinned token":   accessPath("rita"),
		"this org's path, unpinned token": accessPath("rita"),
		"own org, a user of this org":     "/api/v1/orgs/elsewhere/users/rita/team-access",
		"own org, a user id of this org":  "/api/v1/orgs/elsewhere/users/" + a.rita.String() + "/team-access",
	} {
		for _, tok := range []string{ottoTok, ottoFree} {
			r := e.do("PUT", path, tok, sel)
			if r.status != http.StatusNotFound || r.errCode(t) != "not_found" {
				t.Errorf("%s: %d %s, want 404", name, r.status, r.body)
			}
		}
	}
	var els uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = 'ELS'`, other).Scan(&els); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"ELS", els.String()} {
		r := a.put(e.chen, "rita", map[string]any{"teamAccess": "selected", "teams": []string{ref}}, "")
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" || !strings.Contains(string(r.body), "unknown team") {
			t.Errorf("another org's team %s: %d %s, want 400 unknown team", ref, r.status, r.body)
		}
	}
	if a.everything() != before || latestSeq(t, e) != seq {
		t.Fatal("a cross-org call changed something")
	}
}

// AC (#48, #49): a removal that leaves open issues assigned to the member in
// a lost team is 409 with the count per team, without a choice; a dry run
// answers the counts and writes nothing; keep leaves them; unassign clears
// only the open ones of the lost teams, with system activity and one
// issue.updated each (scoped to the issue's team).
func TestTeamAccessSetter_AssignedIssues(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	open1 := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "open one", "assignee": "rita"})
	open2 := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "open two", "assignee": "rita"})
	done := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "done", "assignee": "rita", "state": "Done"})
	canceled := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "canceled", "assignee": "rita", "state": "Canceled"})
	orbOpen := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "orb", "assignee": "rita"})
	sel := map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}}
	before, seq := a.everything(), latestSeq(t, e)

	r := a.put(e.chen, "rita", sel, "")
	if r.status != http.StatusConflict || r.errCode(t) != "conflict" || !strings.Contains(string(r.body), "HID: 2") ||
		!strings.Contains(string(r.body), "assignedIssues") {
		t.Fatalf("no choice: %d %s, want 409 with HID: 2", r.status, r.body)
	}
	if a.everything() != before || latestSeq(t, e) != seq {
		t.Fatal("the 409 changed something")
	}

	// Dry runs: the counts, nothing written.
	var dry tracker.TeamAccessResult
	e.must(http.StatusOK, a.put(e.chen, "rita", sel, "?dryRun=true")).json(t, &dry)
	if !dry.DryRun || !dry.Changed || dry.Unassigned != 0 || dry.HiddenAssigned != 2 ||
		len(dry.AssignedInLostTeams) != 1 || dry.AssignedInLostTeams[0] != (tracker.TeamIssueCount{TeamID: a.hid, OpenIssues: 2}) {
		t.Fatalf("dry run: %+v", dry)
	}
	sel["assignedIssues"] = "unassign"
	e.must(http.StatusOK, a.put(e.chen, "rita", sel, "?dryRun=true")).json(t, &dry)
	if !dry.DryRun || dry.Unassigned != 2 || dry.HiddenAssigned != 0 {
		t.Fatalf("dry run with unassign: %+v", dry)
	}
	if a.everything() != before || latestSeq(t, e) != seq {
		t.Fatal("a dry run changed something")
	}

	// keep.
	sel["assignedIssues"] = "keep"
	kept := a.set("rita", sel)
	if kept.Unassigned != 0 || kept.HiddenAssigned != 2 || len(kept.AssignedInLostTeams) != 1 {
		t.Fatalf("keep: %+v", kept)
	}
	if n := e.count(`SELECT count(*) FROM issues WHERE org_id = $1 AND assignee_id = $2`, e.orgID, a.rita); n != 5 {
		t.Fatalf("keep left %d assigned issues, want 5", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND seq > $2 AND type = $3`, e.orgID, seq, events.IssueUpdated); n != 0 {
		t.Fatalf("keep emitted %d issue.updated", n)
	}

	// Back to all (nothing lost), then unassign.
	a.set("rita", map[string]any{"teamAccess": "all"})
	seq = latestSeq(t, e)
	sel["assignedIssues"] = "unassign"
	un := a.set("rita", sel)
	if un.Unassigned != 2 || un.HiddenAssigned != 0 {
		t.Fatalf("unassign: %+v", un)
	}
	for _, c := range []struct {
		is       tracker.Issue
		assigned bool
	}{{open1, false}, {open2, false}, {done, true}, {canceled, true}, {orbOpen, true}} {
		var n int
		n = e.count(`SELECT count(*) FROM issues WHERE id = $1 AND assignee_id = $2`, c.is.ID, a.rita)
		if (n == 1) != c.assigned {
			t.Errorf("%s assigned = %v, want %v", c.is.Identifier, n == 1, c.assigned)
		}
	}
	for _, is := range []tracker.Issue{open1, open2} {
		var source, kind string
		var actor uuid.UUID
		var to *string
		if err := e.pool.QueryRow(context.Background(), `SELECT source, kind, actor_id, "to"::text FROM issue_activity
			WHERE issue_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, is.ID).Scan(&source, &kind, &actor, &to); err != nil {
			t.Fatal(err)
		}
		if source != "system" || kind != tracker.ActAssignee || actor != e.user("chen").ID || to != nil {
			t.Errorf("%s activity %s %s %s %v", is.Identifier, source, kind, actor, to)
		}
		var scope string
		var teamIDs []uuid.UUID
		var payload string
		if err := e.pool.QueryRow(context.Background(), `SELECT scope, team_ids, payload::text FROM events
			WHERE org_id = $1 AND seq > $2 AND type = $3 AND entity_id = $4`, e.orgID, seq, events.IssueUpdated, is.ID).Scan(&scope, &teamIDs, &payload); err != nil {
			t.Fatalf("%s: no issue.updated: %v", is.Identifier, err)
		}
		if scope != "any" || !sameIDs(teamIDs, idList(a.hid)) || !strings.Contains(payload, `"access_removed"`) || strings.Contains(payload, "open ") {
			t.Errorf("%s event %s %v %s", is.Identifier, scope, teamIDs, payload)
		}
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND seq > $2 AND type = $3`, e.orgID, seq, events.IssueUpdated); n != 2 {
		t.Fatalf("unassign emitted %d issue.updated, want 2", n)
	}
	// member.access_changed is the last event.
	var last string
	if err := e.pool.QueryRow(context.Background(), `SELECT type FROM events WHERE org_id = $1 ORDER BY seq DESC LIMIT 1`, e.orgID).Scan(&last); err != nil || last != events.MemberAccessChanged {
		t.Fatalf("last event %q (%v)", last, err)
	}
	// An admin reads the issues as unassigned; rita no longer reads them.
	if got := e.issueFull(open1.Identifier); got.Assignee != nil {
		t.Fatalf("assignee still %+v", got.Assignee)
	}
	if r := e.do("GET", "/api/v1/orgs/orbit/issues/"+open1.Identifier, a.ritaTok, nil); r.status != http.StatusNotFound {
		t.Fatalf("rita reads %s: %d", open1.Identifier, r.status)
	}
}

// AC (#50): the member's subscriptions, follows and per-team settings of the
// lost teams are gone, the others stay, the inbox rows stay; another member's
// rows are untouched.
func TestTeamAccessSetter_NotifyCleanup(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	mona := testdb.AddMember(t, e.pool, e.orgID, "mona", "member")
	for _, u := range []uuid.UUID{a.rita, mona} {
		for _, is := range []uuid.UUID{a.hidIssue.ID, a.orbIss.ID} {
			testdb.MustExec(t, e.pool, `INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'subscribed', 'manual')`, e.orgID, is, u)
			testdb.MustExec(t, e.pool, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, $4, true)`,
				e.orgID, u, is, "k:"+is.String())
		}
		for _, team := range []uuid.UUID{a.orb, a.hid} {
			testdb.MustExec(t, e.pool, `INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, e.orgID, u, team)
			testdb.MustExec(t, e.pool, `INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'off')`, e.orgID, u, team)
		}
	}
	res := a.set("rita", map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}})
	if res.Removed != (tracker.RemovedNotifyRows{Subscriptions: 1, Follows: 1, Settings: 1}) {
		t.Fatalf("removed %+v", res.Removed)
	}
	for _, c := range []struct {
		name string
		sql  string
		args []any
		want int
	}{
		{"rita's HID subscription", `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1 AND issue_id = $2`, []any{a.rita, a.hidIssue.ID}, 0},
		{"rita's ORB subscription", `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1 AND issue_id = $2`, []any{a.rita, a.orbIss.ID}, 1},
		{"rita's HID follow", `SELECT count(*) FROM team_follows WHERE user_id = $1 AND team_id = $2`, []any{a.rita, a.hid}, 0},
		{"rita's ORB follow", `SELECT count(*) FROM team_follows WHERE user_id = $1 AND team_id = $2`, []any{a.rita, a.orb}, 1},
		{"rita's HID setting", `SELECT count(*) FROM notification_settings WHERE user_id = $1 AND team_id = $2`, []any{a.rita, a.hid}, 0},
		{"rita's ORB setting", `SELECT count(*) FROM notification_settings WHERE user_id = $1 AND team_id = $2`, []any{a.rita, a.orb}, 1},
		{"rita's inbox rows (kept, #51)", `SELECT count(*) FROM notifications WHERE recipient_id = $1`, []any{a.rita}, 2},
		{"mona's rows", `SELECT (SELECT count(*) FROM issue_subscriptions WHERE user_id = $1) + (SELECT count(*) FROM team_follows WHERE user_id = $1)
			+ (SELECT count(*) FROM notification_settings WHERE user_id = $1)`, []any{mona}, 6},
	} {
		if n := e.count(c.sql, c.args...); n != c.want {
			t.Errorf("%s = %d, want %d", c.name, n, c.want)
		}
	}
	// The inbox hides the kept HID row: one row, unread count 1.
	var inbox page[map[string]any]
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/notifications", a.ritaTok, nil)).json(t, &inbox)
	if len(inbox.Items) != 1 {
		t.Fatalf("rita's inbox has %d rows, want 1", len(inbox.Items))
	}
}

// AC: replace is idempotent. The same PUT again (with or without an
// Idempotency-Key, which a PUT ignores) answers changed false, writes
// nothing, emits nothing and logs no second audit line.
func TestTeamAccessSetter_Idempotent(t *testing.T) {
	a := newAccessEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	e := a.env
	sel := map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}}
	var first tracker.TeamAccessResult
	e.must(http.StatusOK, e.do("PUT", accessPath("rita"), e.chen, sel, "Idempotency-Key", "same-key")).json(t, &first)
	if !first.Changed {
		t.Fatalf("first: %+v", first)
	}
	before, seq := a.everything(), latestSeq(t, e)
	for _, hdr := range [][]string{{"Idempotency-Key", "same-key"}, {"Idempotency-Key", "other-key"}, nil} {
		var again tracker.TeamAccessResult
		r := e.do("PUT", accessPath("rita"), e.chen, sel, hdr...)
		e.must(http.StatusOK, r).json(t, &again)
		if r.header.Get("Idempotent-Replayed") != "" {
			t.Fatalf("a PUT was replayed from the store")
		}
		if again.Changed || !sameIDs(again.TeamIDs, first.TeamIDs) || len(again.LostTeamIDs) != 0 || len(again.AddedTeamIDs) != 0 {
			t.Fatalf("repeat: %+v", again)
		}
	}
	if a.everything() != before || latestSeq(t, e) != seq {
		t.Fatal("a repeat changed something")
	}
	if n := e.count(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d idempotency rows for a PUT", n)
	}
	if n := strings.Count(e.logs.String(), "audit: team access changed"); n != 1 {
		t.Fatalf("%d audit lines, want 1", n)
	}
}

// AC (#65, entry 1): while an enabled integration has a repository with no
// team, the setter refuses to make or keep a selected member (409 naming the
// repository, a dry run too) and changes nothing; "all" is allowed; once the
// repository has a team, or its integration is disabled, the setter works.
// The same for POST /agents with selected (entry 4).
func TestTeamAccessSetter_WebhookGuard(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	gi := e.glSetup("orbit", e.chen) // a repository with no team
	sel := map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}}
	before, seq := a.everything(), latestSeq(t, e)
	guardRefused(t, a.put(e.chen, "rita", sel, ""), "example-org/example-app")
	guardRefused(t, a.put(e.chen, "rita", sel, "?dryRun=true"), "example-org/example-app")
	agentsBefore := e.count(`SELECT count(*) FROM org_members WHERE org_id = $1`, e.orgID)
	guardRefused(t, e.do("POST", "/api/v1/orgs/orbit/agents", e.chen,
		map[string]any{"username": "bot-sel", "name": "Bot", "teamAccess": "selected", "teams": []string{testdb.TeamKey}}), "example-org/example-app")
	if e.count(`SELECT count(*) FROM org_members WHERE org_id = $1`, e.orgID) != agentsBefore || e.count(`SELECT count(*) FROM users WHERE username = 'bot-sel'`) != 0 {
		t.Fatal("a refused agent was made")
	}
	if a.everything() != before || latestSeq(t, e) != seq {
		t.Fatal("a refused setter changed something")
	}
	// all is never refused: it restricts nobody.
	if r := a.set("rita", map[string]any{"teamAccess": "all"}); r.Changed {
		t.Fatalf("all to all: %+v", r)
	}
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/agents", e.chen, map[string]any{"username": "bot-all", "name": "Bot"}))

	// Disabled: the map with no team matches nothing, so the setter passes;
	// enabling it is then refused (ticket 8).
	e.must(http.StatusOK, e.do("PATCH", "/api/v1/orgs/orbit/integrations/"+gi.String(), e.chen, map[string]any{"enabled": false}))
	a.set("rita", sel)
	guardRefused(t, e.do("PATCH", "/api/v1/orgs/orbit/integrations/"+gi.String(), e.chen, map[string]any{"enabled": true}), "example-org/example-app")

	// With a team on the repository, everything passes.
	e.must(http.StatusOK, e.do("PATCH", e.repoPath(gi, e.repoID(gi)), e.chen, map[string]any{"teamId": testdb.TeamKey}))
	e.must(http.StatusOK, e.do("PATCH", "/api/v1/orgs/orbit/integrations/"+gi.String(), e.chen, map[string]any{"enabled": true}))
	a.set("rita", map[string]any{"teamAccess": "selected", "teams": []string{"HID"}})
	// And now a repository with no team is refused (the guard sees rita).
	guardRefused(t, e.do("PATCH", e.repoPath(gi, e.repoID(gi)), e.chen, map[string]any{"teamId": nil}), "example-org/example-app")
}

// AC: two setters at once run one after the other (the guard's advisory
// lock and the member row lock): the stored set is exactly one of the two,
// never a mix. And a setter racing a repository map with no team: exactly one
// of the two passes, so restricted access and a repository with no team never
// both exist (the race ticket 8 could not test).
func TestTeamAccessSetter_Concurrent(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	put := func(teams ...string) func() (resp, error) {
		return func() (resp, error) {
			return e.request("PUT", accessPath("rita"), e.chen, map[string]any{"teamAccess": "selected", "teams": teams})
		}
	}
	for i := range 8 {
		ra, rb := e.raceOnce(i, put(testdb.TeamKey), put("HID"))
		if ra.status != http.StatusOK || rb.status != http.StatusOK {
			t.Fatalf("round %d: %d %s / %d %s", i, ra.status, ra.body, rb.status, rb.body)
		}
		if got := a.accessState(a.rita); got != "selected:"+testdb.TeamKey && got != "selected:HID" {
			t.Fatalf("round %d: stored %q, want one set", i, got)
		}
		a.set("rita", map[string]any{"teamAccess": "all"})
	}

	gi := e.glSetup("orbit", e.chen)
	repo := e.repoPath(gi, e.repoID(gi))
	e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": testdb.TeamKey}))
	clear := func() (resp, error) { return e.request("PATCH", repo, e.chen, map[string]any{"teamId": nil}) }
	for i := range 8 {
		rs, rc := e.raceOnce(i, put(testdb.TeamKey), clear)
		ok := 0
		for _, r := range []resp{rs, rc} {
			switch r.status {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
			default:
				t.Fatalf("round %d: %d %s", i, r.status, r.body)
			}
		}
		restricted := e.count(`SELECT count(*) FROM org_members WHERE org_id = $1 AND team_access = 'selected'`, e.orgID)
		noTeam := e.count(`SELECT count(*) FROM git_repo_map WHERE org_id = $1 AND team_id IS NULL`, e.orgID)
		if ok != 1 || (restricted > 0 && noTeam > 0) {
			t.Fatalf("round %d: %d passed; restricted %d, repositories with no team %d", i, ok, restricted, noTeam)
		}
		// Reset: rita all, the repository mapped.
		a.set("rita", map[string]any{"teamAccess": "all"})
		e.must(http.StatusOK, e.do("PATCH", repo, e.chen, map[string]any{"teamId": testdb.TeamKey}))
	}
}

// AC (#31): POST /agents takes teamAccess and teams. A selected agent sees
// only its teams; the answer shows the access; bad bodies are 400.
func TestAgents_TeamAccess(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	var m tracker.Member
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/agents", e.chen,
		map[string]any{"username": "ci-bot", "name": "CI", "teamAccess": "selected", "teams": []string{"orb"}})).json(t, &m)
	if m.TeamAccess == nil || *m.TeamAccess != "selected" || !slices.Equal(m.Teams, []string{testdb.TeamKey}) {
		t.Fatalf("agent %v %v", m.TeamAccess, m.Teams)
	}
	if got := a.accessState(m.ID); got != "selected:"+testdb.TeamKey {
		t.Fatalf("stored %q", got)
	}
	exp := time.Now().Add(24 * time.Hour)
	tok, _ := e.newToken(m.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read", "write"}, expires: &exp})
	if r := e.do("GET", "/api/v1/orgs/orbit/issues/"+a.hidIssue.Identifier, tok, nil); r.status != http.StatusNotFound {
		t.Fatalf("the selected agent reads HID: %d", r.status)
	}
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/issues/"+a.orbIss.Identifier, tok, nil))

	var plain tracker.Member
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/agents", e.chen, map[string]any{"username": "plain-bot", "name": "Plain"})).json(t, &plain)
	if plain.TeamAccess == nil || *plain.TeamAccess != "all" || len(plain.Teams) != 0 || a.accessState(plain.ID) != "all:" {
		t.Fatalf("default agent %v %v", plain.TeamAccess, plain.Teams)
	}
	for name, body := range map[string]map[string]any{
		"selected with no team":  {"teamAccess": "selected", "teams": []string{}},
		"selected, teams absent": {"teamAccess": "selected"},
		"teams with all":         {"teamAccess": "all", "teams": []string{testdb.TeamKey}},
		"unknown kind":           {"teamAccess": "some"},
		"unknown team":           {"teamAccess": "selected", "teams": []string{"NOPE"}},
	} {
		body["username"], body["name"] = "bad-bot", "Bad"
		r := e.do("POST", "/api/v1/orgs/orbit/agents", e.chen, body)
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if e.count(`SELECT count(*) FROM users WHERE username = 'bad-bot'`) != 0 {
		t.Fatal("a refused agent was made")
	}
	// The setter works on an agent too (#31: an agent is a normal member).
	if r := a.set("ci-bot", map[string]any{"teamAccess": "selected", "teams": []string{"HID"}}); !sameIDs(r.TeamIDs, idList(a.hid)) {
		t.Fatalf("agent setter %+v", r)
	}
}

// LAUN-225 review: a member.access_changed row that is legacy again (after a
// migration down past 0038 and up) stays admin data through GET /events:
// hidden from an all-teams member before and after the backfill; the admin
// reads it; the backfill places it as admin.
func TestTeamAccessSetter_LegacyEventStaysAdmin(t *testing.T) {
	a := newAccessEnv(t)
	e := a.env
	a.set("rita", map[string]any{"teamAccess": "selected", "teams": []string{testdb.TeamKey}})
	testdb.MustExec(t, e.pool, `UPDATE events SET scope = 'legacy', team_ids = '{}' WHERE org_id = $1 AND type = $2`, e.orgID, events.MemberAccessChanged)
	claudeAll, _ := e.newToken(e.user("claude").ID, tokenOpts{pin: &e.orgID})
	reads := func(tok string) bool {
		var fd page[tracker.Event]
		e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/events?limit=500&after=0", tok, nil)).json(t, &fd)
		return slices.ContainsFunc(fd.Items, func(ev tracker.Event) bool { return ev.Type == events.MemberAccessChanged })
	}
	check := func(when string) {
		t.Helper()
		if !reads(e.chen) {
			t.Errorf("%s: the admin does not read the row", when)
		}
		if reads(claudeAll) {
			t.Errorf("%s: an all-teams member reads the admin row", when)
		}
		if reads(a.ritaTok) {
			t.Errorf("%s: the restricted member reads the admin row", when)
		}
	}
	check("legacy, before the backfill")
	if _, locked, err := events.Backfill(context.Background(), e.pool, events.BackfillOptions{}); err != nil || !locked {
		t.Fatalf("backfill: locked %v, err %v", locked, err)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = $2 AND scope <> 'admin'`, e.orgID, events.MemberAccessChanged); n != 0 {
		t.Fatalf("%d rows not placed as admin", n)
	}
	check("after the backfill")
}
