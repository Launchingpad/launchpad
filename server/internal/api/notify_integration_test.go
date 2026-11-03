package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/notify"
)

// The preference, team-follow and issue-subscription routes (LAUN-32,
// docs/internal/decisions-T13-notifications.md #32).

const (
	nsPath  = "/api/v1/orgs/orbit/me/notification-settings"
	nsTeam  = nsPath + "/teams/ORB"
	folPath = "/api/v1/orgs/orbit/me/teams/ORB/follow"
)

func (e *env) settings(token string) notify.View {
	e.t.Helper()
	var v notify.View
	e.must(200, e.do("GET", nsPath, token, nil)).json(e.t, &v)
	return v
}

func viewEvent(t *testing.T, v notify.View, ev string) notify.EventView {
	t.Helper()
	for _, e := range v.Events {
		if e.Event == ev {
			return e
		}
	}
	t.Fatalf("no event %s in view", ev)
	return notify.EventView{}
}

func ptr(s string) *string { return &s }

func wantResolved(t *testing.T, what string, got notify.ResolvedChannel, value string, src notify.Source) {
	t.Helper()
	if got.Value != value || got.Source != src {
		t.Fatalf("%s = %+v, want %s from %s", what, got, value, src)
	}
}

// TestNotifySettings_Defaults: a user with no rows sees the built-in defaults
// (Chen, 2026-10-26), source default, follows nothing, and every event lists
// every team.
func TestNotifySettings_Defaults(t *testing.T) {
	e := newEnv(t)
	v := e.settings(e.chen)
	if !v.EmailEnabled || v.IncludeExcerpt || len(v.Follows) != 0 || v.Follows == nil || v.Mail.Available {
		t.Fatalf("view %+v", v)
	}
	want := map[string][2]string{"assigned": {"on", "on"}, "mention": {"on", "on"}, "comment": {"on", "on"},
		"state_change": {"off", "on"}, "new_issue": {"off", "on"}, "project_update_due": {"on", "on"}}
	if len(v.Events) != len(want) {
		t.Fatalf("events %d", len(v.Events))
	}
	for ev, w := range want {
		g := viewEvent(t, v, ev)
		wantResolved(t, ev+" email", g.Resolved.Email, w[0], notify.SourceDefault)
		wantResolved(t, ev+" inbox", g.Resolved.Inbox, w[1], notify.SourceDefault)
		if g.Default.Email != w[0] || g.Default.Inbox != w[1] || g.Global.Email != nil || g.Global.Inbox != nil {
			t.Fatalf("%s: %+v", ev, g)
		}
		if ev == "project_update_due" { // no team level (LAUN-196)
			if len(g.Teams) != 0 {
				t.Fatalf("%s teams %+v", ev, g.Teams)
			}
			continue
		}
		if len(g.Teams) != 1 || g.Teams[0].TeamKey != "ORB" {
			t.Fatalf("%s teams %+v", ev, g.Teams)
		}
		wantResolved(t, ev+" team email", g.Teams[0].Resolved.Email, w[0], notify.SourceDefault)
		if g.RequiresFollow != (ev == "new_issue") {
			t.Fatalf("%s requiresFollow %v", ev, g.RequiresFollow)
		}
	}
	// No row was written by the reads.
	if n := e.countRows(`SELECT count(*) FROM notification_settings`) + e.countRows(`SELECT count(*) FROM notification_user_settings`); n != 0 {
		t.Fatalf("GET wrote %d rows", n)
	}
}

// TestNotifySettings_Resolution: team row, global row, default; null clears
// an override; the master switch wins; absent keeps a column.
func TestNotifySettings_Resolution(t *testing.T) {
	e := newEnv(t)
	// Global: comment email off, state_change email on.
	e.must(200, e.do("PUT", nsPath, e.chen, map[string]any{"events": map[string]any{
		"comment": map[string]any{"email": "off"}, "state_change": map[string]any{"email": "on", "inbox": "off"}}}))
	v := e.settings(e.chen)
	c := viewEvent(t, v, "comment")
	wantResolved(t, "comment email", c.Resolved.Email, "off", notify.SourceGlobal)
	wantResolved(t, "comment inbox", c.Resolved.Inbox, "on", notify.SourceDefault)
	if c.Global.Email == nil || *c.Global.Email != "off" || c.Global.Inbox != nil {
		t.Fatalf("global %+v", c.Global)
	}
	// Team override beats global, and only for that column.
	var tv notify.View
	e.must(200, e.do("PUT", nsTeam, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"email": "on"}}})).json(t, &tv)
	c = viewEvent(t, tv, "comment")
	wantResolved(t, "global comment email", c.Resolved.Email, "off", notify.SourceGlobal)
	wantResolved(t, "team comment email", c.Teams[0].Resolved.Email, "on", notify.SourceTeam)
	wantResolved(t, "team comment inbox", c.Teams[0].Resolved.Inbox, "on", notify.SourceDefault)
	// A later PUT that does not name the column keeps it (read-merge).
	e.must(200, e.do("PUT", nsTeam, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"inbox": "off"}}}))
	c = viewEvent(t, e.settings(e.chen), "comment")
	wantResolved(t, "kept team email", c.Teams[0].Resolved.Email, "on", notify.SourceTeam)
	wantResolved(t, "team inbox", c.Teams[0].Resolved.Inbox, "off", notify.SourceTeam)
	// null clears the team override: the global value shows again.
	e.must(200, e.do("PUT", nsTeam, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"email": nil, "inbox": nil}}}))
	c = viewEvent(t, e.settings(e.chen), "comment")
	wantResolved(t, "cleared team email", c.Teams[0].Resolved.Email, "off", notify.SourceGlobal)
	wantResolved(t, "cleared team inbox", c.Teams[0].Resolved.Inbox, "on", notify.SourceDefault)
	if n := e.countRows(`SELECT count(*) FROM notification_settings WHERE team_id IS NOT NULL`); n != 0 {
		t.Fatalf("an all-NULL team row stayed: %d", n)
	}
	// null clears the global value: default again; the row goes.
	e.must(200, e.do("PUT", nsPath, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"email": nil}}}))
	c = viewEvent(t, e.settings(e.chen), "comment")
	wantResolved(t, "cleared global", c.Resolved.Email, "on", notify.SourceDefault)
	if n := e.countRows(`SELECT count(*) FROM notification_settings WHERE event = 'comment'`); n != 0 {
		t.Fatalf("an all-NULL global row stayed: %d", n)
	}
	// The master switch wins over a team "on" and keeps the inbox.
	e.must(200, e.do("PUT", nsTeam, e.chen, map[string]any{"events": map[string]any{"assigned": map[string]any{"email": "on"}}}))
	e.must(200, e.do("PUT", nsPath, e.chen, map[string]any{"emailEnabled": false, "includeExcerpt": true}))
	v = e.settings(e.chen)
	a := viewEvent(t, v, "assigned")
	if v.EmailEnabled || !v.IncludeExcerpt {
		t.Fatalf("switches %+v", v)
	}
	wantResolved(t, "assigned email under master off", a.Teams[0].Resolved.Email, "off", notify.SourceTeam)
	wantResolved(t, "assigned inbox under master off", a.Teams[0].Resolved.Inbox, "on", notify.SourceDefault)
	// An empty PUT keeps the switches; null resets them.
	e.must(200, e.do("PUT", nsPath, e.chen, map[string]any{}))
	if v = e.settings(e.chen); v.EmailEnabled || !v.IncludeExcerpt {
		t.Fatalf("empty PUT changed switches: %+v", v)
	}
	e.must(200, e.do("PUT", nsPath, e.chen, map[string]any{"emailEnabled": nil, "includeExcerpt": nil}))
	if v = e.settings(e.chen); !v.EmailEnabled || v.IncludeExcerpt {
		t.Fatalf("null did not reset: %+v", v)
	}
	// The settings are per user: claude's human colleague sees defaults.
	other := e.newUser("dana")
	e.addMember(e.orgID, other.ID, "member")
	otok, _ := e.newToken(other.ID, tokenOpts{pin: &e.orgID})
	wantResolved(t, "dana", viewEvent(t, e.settings(otok), "assigned").Teams[0].Resolved.Email, "on", notify.SourceDefault)
}

// TestNotifySettings_Validation: an unknown event, channel or value is 400
// validation_failed (or bad_request for a wrong JSON type) and writes nothing,
// even when another event in the body is valid.
func TestNotifySettings_Validation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, code string
		body       any
	}{
		{"unknown event", "validation_failed", map[string]any{"events": map[string]any{"digest": map[string]any{"email": "on"}}}},
		{"unknown value", "validation_failed", map[string]any{"events": map[string]any{"assigned": map[string]any{"email": "maybe"}}}},
		{"unknown channel", "validation_failed", map[string]any{"events": map[string]any{"assigned": map[string]any{"sms": "on"}}}},
		{"unknown field", "bad_request", map[string]any{"digest": true}},
		{"valid beside invalid", "validation_failed", map[string]any{"emailEnabled": false, "events": map[string]any{
			"assigned": map[string]any{"email": "off"}, "nope": map[string]any{"email": "off"}}}},
	}
	for _, c := range cases {
		r := e.do("PUT", nsPath, e.chen, c.body)
		if r.status != 400 || r.errCode(t) != c.code {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		// The team route takes the same events (it has no top-level fields).
		if m := c.body.(map[string]any); m["events"] != nil {
			r := e.do("PUT", nsTeam, e.chen, map[string]any{"events": m["events"]})
			if r.status != 400 || r.errCode(t) != c.code {
				t.Fatalf("%s (team): %d %s", c.name, r.status, r.body)
			}
		}
	}
	if n := e.countRows(`SELECT count(*) FROM notification_settings`) + e.countRows(`SELECT count(*) FROM notification_user_settings`); n != 0 {
		t.Fatalf("a refused PUT wrote %d rows", n)
	}
	// An unknown team is 404.
	e.must(404, e.do("PUT", nsPath+"/teams/NOPE", e.chen, map[string]any{"events": map[string]any{}}))
}

// TestNotifyFollow: follow and unfollow are idempotent 204s; the key, the id
// and any case work; the view lists the follow.
func TestNotifyFollow(t *testing.T) {
	e := newEnv(t)
	var team struct{ ID uuid.UUID }
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/ORB", e.chen, nil)).json(t, &team)
	for i := 0; i < 2; i++ {
		e.must(204, e.do("PUT", folPath, e.chen, nil))
	}
	e.must(204, e.do("PUT", "/api/v1/orgs/orbit/me/teams/"+team.ID.String()+"/follow", e.chen, nil))
	if n := e.countRows(`SELECT count(*) FROM team_follows`); n != 1 {
		t.Fatalf("follow rows %d", n)
	}
	if v := e.settings(e.chen); len(v.Follows) != 1 || v.Follows[0] != team.ID {
		t.Fatalf("follows %v", v.Follows)
	}
	for i := 0; i < 2; i++ {
		e.must(204, e.do("DELETE", folPath, e.chen, nil))
	}
	if v := e.settings(e.chen); len(v.Follows) != 0 {
		t.Fatalf("follows %v", v.Follows)
	}
	e.must(404, e.do("PUT", "/api/v1/orgs/orbit/me/teams/NOPE/follow", e.chen, nil)).errCode(t)
	e.must(404, e.do("DELETE", "/api/v1/orgs/orbit/me/teams/NOPE/follow", e.chen, nil)).errCode(t)
}

// TestNotifySubscription: the implicit reasons, subscribe, mute, delete.
func TestNotifySubscription(t *testing.T) {
	e := newEnv(t)
	dana := e.newUser("dana")
	e.addMember(e.orgID, dana.ID, "member")
	dtok, _ := e.newToken(dana.ID, tokenOpts{pin: &e.orgID})
	// chen creates and is the creator; dana has no relation yet.
	is := e.createIssue(e.chen, map[string]any{"title": "watched"})
	sub := func(tok string) notify.Subscription {
		var s notify.Subscription
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/subscription", tok, nil)).json(t, &s)
		return s
	}
	eq := func(what string, got notify.Subscription, state string, reasons ...string) {
		t.Helper()
		if got.State != state || len(got.Reasons) != len(reasons) {
			t.Fatalf("%s: %+v, want %s %v", what, got, state, reasons)
		}
		for i, r := range reasons {
			if got.Reasons[i] != r {
				t.Fatalf("%s: %+v, want %s %v", what, got, state, reasons)
			}
		}
	}
	eq("creator", sub(e.chen), "default", "creator")
	eq("stranger", sub(dtok), "default")
	// A comment makes dana a commenter.
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/comments", dtok, map[string]any{"body": "hi"}))
	eq("commenter", sub(dtok), "default", "commenter")
	// Assignee.
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/"+is.Identifier, e.chen, map[string]any{"assignee": "dana"}))
	eq("assignee", sub(dtok), "default", "assignee", "commenter")

	path := "/api/v1/orgs/orbit/issues/" + is.Identifier + "/subscription"
	// Subscribe twice: same answer, one row.
	for i := 0; i < 2; i++ {
		var s notify.Subscription
		e.must(200, e.do("PUT", path, dtok, map[string]any{"subscribed": true})).json(t, &s)
		eq("subscribed", s, "subscribed", "assignee", "commenter", "manual")
	}
	// Mute wins over the implicit reasons.
	var s notify.Subscription
	e.must(200, e.do("PUT", path, dtok, map[string]any{"subscribed": false})).json(t, &s)
	eq("muted", s, "muted", "assignee", "commenter")
	if n := e.countRows(`SELECT count(*) FROM issue_subscriptions WHERE user_id = $1`, dana.ID); n != 1 {
		t.Fatalf("rows %d", n)
	}
	// Delete twice: back to the implicit rules.
	for i := 0; i < 2; i++ {
		e.must(204, e.do("DELETE", path, dtok, nil))
	}
	eq("after delete", sub(dtok), "default", "assignee", "commenter")
	// Body checks.
	e.must(400, e.do("PUT", path, dtok, map[string]any{})).errCode(t)
	e.must(400, e.do("PUT", path, dtok, map[string]any{"subscribed": "yes"}))
	e.must(404, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-999/subscription", dtok, nil)).errCode(t)
	e.must(404, e.do("PUT", "/api/v1/orgs/orbit/issues/ORB-999/subscription", dtok, map[string]any{"subscribed": true}))
	e.must(404, e.do("DELETE", "/api/v1/orgs/orbit/issues/ORB-999/subscription", dtok, nil))
}

// TestNotify_AgentsAndScopes: an agent token gets 403 forbidden on every
// route (also with every scope); a read token cannot write.
func TestNotify_AgentsAndScopes(t *testing.T) {
	e := newEnv(t)
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	sp := "/api/v1/orgs/orbit/issues/" + is.Identifier + "/subscription"
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", nsPath, nil},
		{"PUT", nsPath, map[string]any{}},
		{"PUT", nsTeam, map[string]any{}},
		{"PUT", folPath, nil},
		{"DELETE", folPath, nil},
		{"GET", sp, nil},
		{"PUT", sp, map[string]any{"subscribed": true}},
		{"DELETE", sp, nil},
	}
	for _, rt := range routes {
		r := e.do(rt.method, rt.path, e.claude, rt.body)
		if r.status != 403 || r.errCode(t) != "forbidden" {
			t.Fatalf("agent %s %s: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM team_follows`) + e.countRows(`SELECT count(*) FROM issue_subscriptions`) +
		e.countRows(`SELECT count(*) FROM notification_settings`); n != 0 {
		t.Fatalf("an agent wrote %d rows", n)
	}
	chen := e.user("chen")
	ro, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	for _, rt := range routes {
		want := 403
		if rt.method == "GET" {
			want = 200
		}
		if r := e.do(rt.method, rt.path, ro, rt.body); r.status != want {
			t.Fatalf("read token %s %s: %d %s", rt.method, rt.path, r.status, r.body)
		} else if want == 403 && r.errCode(t) != "insufficient_scope" {
			t.Fatalf("read token %s %s: %s", rt.method, rt.path, r.body)
		}
	}
	// No token at all.
	e.must(401, e.do("GET", nsPath, "", nil))
}

// TestNotify_CrossOrg: chen is a member of orbit and acme with an unpinned
// token. Another org's team or issue in a path is 404; the org in the path
// scopes the settings (acme's view lists acme's team only, and orbit's rows
// do not show); a token pinned to orbit gets 404 on acme.
func TestNotify_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "member")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	orb := e.createIssue(tok, map[string]any{"title": "orbit issue"})
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"team": "ACM", "title": "acme issue"}))
	var orbTeam struct{ ID uuid.UUID }
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/ORB", tok, nil)).json(t, &orbTeam)

	A, O := "/api/v1/orgs/acme", "/api/v1/orgs/orbit"
	// Orbit's team and issue through acme's paths: 404, nothing written.
	for _, rt := range []struct{ method, path string }{
		{"PUT", A + "/me/teams/ORB/follow"}, {"DELETE", A + "/me/teams/ORB/follow"},
		{"PUT", A + "/me/teams/" + orbTeam.ID.String() + "/follow"},
		{"PUT", A + "/me/notification-settings/teams/ORB"},
		{"PUT", A + "/me/notification-settings/teams/" + orbTeam.ID.String()},
		{"GET", A + "/issues/" + orb.ID.String() + "/subscription"},
		{"GET", A + "/issues/ORB-1/subscription"},
		{"PUT", A + "/issues/" + orb.ID.String() + "/subscription"},
		{"DELETE", A + "/issues/" + orb.ID.String() + "/subscription"},
	} {
		var body any
		switch {
		case rt.method != "PUT", strings.HasSuffix(rt.path, "/follow"):
		case strings.HasSuffix(rt.path, "/subscription"):
			body = map[string]any{"subscribed": true}
		default:
			body = map[string]any{"events": map[string]any{}}
		}
		r := e.do(rt.method, rt.path, tok, body)
		if r.status != 404 || r.errCode(t) != "not_found" {
			t.Fatalf("%s %s: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM team_follows`) + e.countRows(`SELECT count(*) FROM issue_subscriptions`) +
		e.countRows(`SELECT count(*) FROM notification_settings`); n != 0 {
		t.Fatalf("a cross-org call wrote %d rows", n)
	}
	// Settings and follows made in orbit do not show in acme.
	e.must(204, e.do("PUT", O+"/me/teams/ORB/follow", tok, nil))
	e.must(200, e.do("PUT", O+"/me/notification-settings", tok, map[string]any{"emailEnabled": false,
		"events": map[string]any{"comment": map[string]any{"email": "off"}}}))
	var av notify.View
	e.must(200, e.do("GET", A+"/me/notification-settings", tok, nil)).json(t, &av)
	c := viewEvent(t, av, "comment")
	if !av.EmailEnabled || len(av.Follows) != 0 || len(c.Teams) != 1 || c.Teams[0].TeamKey != "ACM" {
		t.Fatalf("acme view %+v", av)
	}
	wantResolved(t, "acme comment email", c.Resolved.Email, "on", notify.SourceDefault)
	// A subscription to an acme issue is acme's alone.
	e.must(200, e.do("PUT", A+"/issues/ACM-1/subscription", tok, map[string]any{"subscribed": false}))
	var s notify.Subscription
	e.must(200, e.do("GET", O+"/issues/"+orb.Identifier+"/subscription", tok, nil)).json(t, &s)
	if s.State != "default" {
		t.Fatalf("orbit issue state %s", s.State)
	}
	// A token pinned to orbit does not reach acme at all.
	for _, rt := range []struct{ method, path string }{
		{"GET", A + "/me/notification-settings"}, {"PUT", A + "/me/teams/ACM/follow"}, {"GET", A + "/issues/ACM-1/subscription"},
	} {
		e.must(404, e.do(rt.method, rt.path, e.chen, nil)).errCode(t)
	}
	// An org chen is not in.
	e.newOrg("other", "OTH")
	e.must(404, e.do("GET", "/api/v1/orgs/other/me/notification-settings", tok, nil))
}

// TestNotify_WebSessionAndIdempotency: a cookie session works with the CSRF
// header (and not without it); a repeated PUT with a key is replay-safe.
func TestNotify_WebSessionAndIdempotency(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	e.must(403, e.do("PUT", folPath, session, nil, "X-CSRF-Token", ""))
	e.must(204, e.do("PUT", folPath, session, nil))
	e.must(200, e.do("GET", nsPath, session, nil))

	// The PUT is replay-safe by itself: a retry, with or without an
	// Idempotency-Key (the middleware acts on POST only), gives the same
	// answer and the same rows.
	body := map[string]any{"events": map[string]any{"comment": map[string]any{"email": "off"}}}
	r1 := e.must(200, e.do("PUT", nsPath, e.chen, body, "Idempotency-Key", "ns-1"))
	r2 := e.must(200, e.do("PUT", nsPath, e.chen, body, "Idempotency-Key", "ns-1"))
	if string(r1.body) != string(r2.body) {
		t.Fatalf("replay differs:\n%s\n%s", r1.body, r2.body)
	}
	if n := e.countRows(`SELECT count(*) FROM notification_settings`); n != 1 {
		t.Fatalf("rows after a replay: %d", n)
	}
}
