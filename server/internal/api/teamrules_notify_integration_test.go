package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/notify"
	"devops/internal/testdb"
)

// Ticket 7 of decisions-T20 (LAUN-223): notifications for a restricted
// member. The settings view, follows and team settings (#64), the issue
// subscription routes (#64), and the inbox, its count and its stream (#62).
// The samples join teamRuleSamples in init, as the planning samples do.

// notifyFixture holds the restricted member's own notification rows.
type notifyFixture struct {
	// visible is an unread inbox row of a visible (ORB) issue; hiddenIssue
	// one of the hidden issue; hiddenProject a reminder of the hidden project.
	visible, hiddenIssue, hiddenProject uuid.UUID
}

// addNotifyFixture gives the restricted member rows of every kind a later
// removal leaves behind (#50, #51): an inbox row of a hidden issue and of a
// hidden project, a follow, a team setting and a subscription of team HID.
// Nothing cleaned them, so every route must hide them by itself.
func (f *teamFixture) addNotifyFixture(t *testing.T) {
	t.Helper()
	e := f.e
	nf := &notifyFixture{}
	ins := func(kind, issue, key string) uuid.UUID {
		var id uuid.UUID
		if err := e.pool.QueryRow(context.Background(), `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, project_id, dedupe_key, inbox)
			VALUES ($1, $2, $3,
			        (SELECT i.id FROM issues i JOIN teams t ON t.id = i.team_id WHERE i.org_id = $1 AND t.key || '-' || i.number = $4),
			        NULL, $5, true) RETURNING id`,
			e.orgID, f.restrictedID, kind, issue, key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// The visible row first: the stream test reads it before the hidden ones.
	nf.visible = ins("comment", f.visible, "fx:visible")
	nf.hiddenIssue = ins("comment", f.hidden["issue"], "fx:hidden-issue")
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO notifications (org_id, recipient_id, kind, project_id, dedupe_key, inbox, data)
		VALUES ($1, $2, 'project_update_due', $3, 'fx:hidden-project', true, '{"dueAt":"2026-10-30T09:00:00Z"}') RETURNING id`,
		e.orgID, f.restrictedID, f.hidden["project"]).Scan(&nf.hiddenProject); err != nil {
		t.Fatal(err)
	}
	for _, team := range []uuid.UUID{f.orbID, f.hidID} {
		testdb.MustExec(t, e.pool, `INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, e.orgID, f.restrictedID, team)
		testdb.MustExec(t, e.pool, `INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'off')`,
			e.orgID, f.restrictedID, team)
	}
	testdb.MustExec(t, e.pool, `INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason)
		SELECT i.org_id, i.id, $2, 'subscribed', 'manual' FROM issues i JOIN teams t ON t.id = i.team_id
		WHERE i.org_id = $1 AND t.key || '-' || i.number = $3`, e.orgID, f.restrictedID, f.hidden["issue"])
	f.marks = append(f.marks, nf.hiddenIssue.String(), nf.hiddenProject.String())
	f.inbox = nf
}

// ownRows reads the restricted member's rows of a table with a user_id
// column: a refused write changes none.
func (f *teamFixture) ownRows(table string) func() string {
	return func() string {
		var out string
		if err := f.e.pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY row_to_json(x)::text), '')
			FROM %s x WHERE x.org_id = $1 AND x.user_id = $2`, table), f.e.orgID, f.restrictedID).Scan(&out); err != nil {
			f.e.t.Fatalf("own rows of %s: %v", table, err)
		}
		return out
	}
}

// hiddenInboxRows reads the two hidden inbox rows: read-all and a patch leave them.
func (f *teamFixture) hiddenInboxRows() string {
	return f.rowJSON("notifications", f.inbox.hiddenIssue.String())() + f.rowJSON("notifications", f.inbox.hiddenProject.String())()
}

// visibleUnread counts the restricted member's unread visible rows: the
// fixture has one (it may be read by a sample that ran before).
func (f *teamFixture) visibleUnread() int64 {
	var n int64
	if err := f.e.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE id = $1 AND read_at IS NULL AND archived_at IS NULL`,
		f.inbox.visible).Scan(&n); err != nil {
		f.e.t.Fatal(err)
	}
	return n
}

// unreadCountIs checks GET /notifications/unread-count: the count of visible
// rows only, so the bell matches the list (#62).
func (f *teamFixture) unreadCountIs(body []byte) string {
	var c notify.UnreadCount
	if err := json.Unmarshal(body, &c); err != nil {
		return err.Error()
	}
	if want := f.visibleUnread(); c.Count != want {
		return fmt.Sprintf("unread count = %d, want %d (visible rows only)", c.Count, want)
	}
	return ""
}

// settingsSeen checks the settings view: its teams and follows are ORB only.
func (f *teamFixture) settingsSeen(body []byte) string {
	var v notify.View
	if err := json.Unmarshal(body, &v); err != nil {
		return err.Error()
	}
	if len(v.Follows) != 1 || v.Follows[0] != f.orbID {
		return fmt.Sprintf("follows = %v, want ORB only", v.Follows)
	}
	for _, ev := range v.Events {
		for _, te := range ev.Teams {
			if te.TeamID != f.orbID {
				return fmt.Sprintf("event %s lists team %s, want ORB only", ev.Event, te.TeamKey)
			}
		}
	}
	return ""
}

var notifySamples = map[string]func(f *teamFixture) teamSample{
	"GET /me/notification-settings": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/me/notification-settings"), Check: f.settingsSeen}
	},
	"PUT /me/notification-settings": func(f *teamFixture) teamSample {
		return teamSample{Method: "PUT", Path: f.orgPath("/me/notification-settings"), Body: map[string]any{"includeExcerpt": false},
			Check: f.settingsSeen}
	},
	"PUT /me/notification-settings/teams/{team}": func(f *teamFixture) teamSample {
		return f.path("PUT", "/me/notification-settings/teams/HID", "/me/notification-settings/teams/ZZZ", "HID", "ZZZ",
			map[string]any{"events": map[string]any{"comment": map[string]any{"email": "on"}}}, f.ownRows("notification_settings"))
	},
	"PUT /me/teams/{team}/follow": func(f *teamFixture) teamSample {
		return f.path("PUT", "/me/teams/HID/follow", "/me/teams/ZZZ/follow", "HID", "ZZZ", nil, f.ownRows("team_follows"))
	},
	"DELETE /me/teams/{team}/follow": func(f *teamFixture) teamSample {
		return f.path("DELETE", "/me/teams/HID/follow", "/me/teams/ZZZ/follow", "HID", "ZZZ", nil, f.ownRows("team_follows"))
	},
	"GET /issues/{issue}/subscription": func(f *teamFixture) teamSample {
		return f.issuePath("GET", "/subscription", "", nil, nil)
	},
	"PUT /issues/{issue}/subscription": func(f *teamFixture) teamSample {
		return f.issuePath("PUT", "/subscription", "", map[string]any{"subscribed": false}, f.ownRows("issue_subscriptions"))
	},
	"DELETE /issues/{issue}/subscription": func(f *teamFixture) teamSample {
		return f.issuePath("DELETE", "/subscription", "", nil, f.ownRows("issue_subscriptions"))
	},
	"GET /notifications": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/notifications?limit=100"), Check: func(b []byte) string {
			if !bytes.Contains(b, []byte(f.inbox.visible.String())) {
				return "the inbox does not hold the visible row: " + string(b)
			}
			return ""
		}, Also: []teamSample{{Method: "GET", Path: f.orgPath("/notifications?state=unread&limit=100")}}}
	},
	"GET /notifications/unread-count": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/notifications/unread-count"), Check: f.unreadCountIs}
	},
	"GET /notifications/stream": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/notifications/stream"), Stream: true}
	},
	"POST /notifications/read-all": func(f *teamFixture) teamSample {
		// before is after every row of the fixture: every visible row is marked.
		return teamSample{Method: "POST", Path: f.orgPath("/notifications/read-all"),
			Body: map[string]any{"before": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, Snapshot: f.hiddenInboxRows,
			Check: func(b []byte) string {
				if f.visibleUnread() != 0 {
					return "read-all left the visible row unread: " + string(b)
				}
				return ""
			}}
	},
	// A hidden row by id is the 404 of a made-up id, and stays unread (#62).
	"PATCH /notifications/{id}": func(f *teamFixture) teamSample {
		one := func(id uuid.UUID) teamSample {
			made := uuid.NewString()
			return f.path("PATCH", "/notifications/"+id.String(), "/notifications/"+made, id.String(), made,
				map[string]any{"read": true}, f.hiddenInboxRows)
		}
		s := one(f.inbox.hiddenIssue)
		s.Also = []teamSample{one(f.inbox.hiddenProject)}
		return s
	},
}

func init() {
	for k, v := range notifySamples {
		if _, dup := teamRuleSamples[k]; dup {
			panic("notify sample " + k + " is in teamRuleSamples twice")
		}
		teamRuleSamples[k] = v
	}
}

// TestNotifySamples_Harness shows the ticket 7 samples can fail: with the
// restricted member made an 'all' member, the inbox, the count, the stream
// and the settings view hold team B, and the Checks fail.
func TestNotifySamples_Harness(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	testdb.MustExec(t, e.pool, `UPDATE org_members SET team_access = 'all' WHERE org_id = $1 AND user_id = $2`, e.orgID, f.restrictedID)
	for _, k := range []string{"GET /notifications", "GET /me/notification-settings", "GET /notifications/stream"} {
		s := notifySamples[k](f)
		var res resp
		if s.Stream {
			res = f.readStream(f.restricted, s.Path)
		} else {
			res = e.do("GET", s.Path, f.restricted, nil)
		}
		held := false
		for _, m := range f.marks {
			held = held || bytes.Contains(res.body, []byte(m))
		}
		if res.status != http.StatusOK || !held {
			t.Errorf("%s: an all-teams caller does not see team B: %d %s", k, res.status, res.body)
		}
	}
	for _, k := range []string{"GET /me/notification-settings", "GET /notifications/unread-count"} {
		s := notifySamples[k](f)
		if p := s.Check(e.do("GET", s.Path, f.restricted, nil).body); p == "" {
			t.Errorf("%s: the Check passes for an all-teams caller, so it cannot fail", k)
		}
	}
	// The same routes for the admin and an all-teams member work as before.
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		for _, p := range []string{"/notifications", "/notifications/unread-count", "/me/notification-settings",
			"/issues/" + f.hidden["issue"] + "/subscription"} {
			if res := e.do("GET", f.orgPath(p), tok, nil); res.status != http.StatusOK {
				t.Errorf("%s GET %s: %d %s", who, p, res.status, res.body)
			}
		}
		if res := e.do("PUT", f.orgPath("/me/teams/HID/follow"), tok, nil); res.status != http.StatusNoContent {
			t.Errorf("%s follows HID: %d %s", who, res.status, res.body)
		}
	}
}

// openInboxStream opens GET /notifications/stream from inbox seq 0 and reads
// past the opening block.
func (f *teamFixture) openInboxStream(t *testing.T, token string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", f.e.srv.URL+f.orgPath("/notifications/stream?after=0"), nil)
	f.e.authorize(req, token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream %d", res.StatusCode)
	}
	r := &sseReader{t: t, br: bufio.NewReader(res.Body), end: func() { cancel(); _ = res.Body.Close() }}
	for {
		line, err := r.br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			return r
		}
	}
}

// TestInboxStream_AccessChange (#56, #62): the inbox stream re-reads the
// access before every page of every drain, with the access event pattern of
// the change feed. With the poll at an hour only a NOTIFY wakes it.
func TestInboxStream_AccessChange(t *testing.T) {
	f := newTeamFixtureWith(t, envOpts{ssePoll: time.Hour})
	e := f.e
	s := f.openInboxStream(t, f.restricted)
	defer s.end()
	want := func(typ, has string) sseEvent {
		t.Helper()
		ev, _, ok := s.next()
		if !ok || ev.typ != typ || !strings.Contains(ev.data, has) {
			t.Fatalf("next = %+v ok=%v, want %s holding %q", ev, ok, typ, has)
		}
		return ev
	}
	// The first drain: the visible row only, and a count of one.
	want("notification", f.inbox.visible.String())
	want("count", `{"count":1}`)

	// Team HID is added; any event of the org wakes the stream: access first,
	// then the hidden rows that are now visible, and the new count.
	testdb.PutInTeams(t, e.pool, e.orgID, f.restrictedID, f.hidID)
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "wake one"})
	if ev := want("access", "{}"); ev.id != "" {
		t.Fatalf("the access event has an id: %+v", ev)
	}
	want("notification", f.inbox.hiddenIssue.String())
	want("notification", f.inbox.hiddenProject.String())
	want("count", `{"count":3}`)

	// HID is removed; a new row of the hidden issue is never sent, and the
	// count drops to the visible row.
	testdb.MustExec(t, e.pool, `DELETE FROM team_members WHERE org_id = $1 AND user_id = $2 AND team_id = $3`, e.orgID, f.restrictedID, f.hidID)
	var late uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox)
		SELECT org_id, recipient_id, 'comment', issue_id, 'fx:late', true FROM notifications WHERE id = $1 RETURNING id`, f.inbox.hiddenIssue).Scan(&late); err != nil {
		t.Fatal(err)
	}
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "wake two"})
	want("access", "{}")
	ev := want("count", `{"count":1}`)
	if strings.Contains(ev.data, late.String()) {
		t.Fatalf("the hidden row was sent: %+v", ev)
	}
	// A visible row still arrives after the removal.
	var vis uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox)
		SELECT org_id, recipient_id, 'comment', issue_id, 'fx:late-visible', true FROM notifications WHERE id = $1 RETURNING id`, f.inbox.visible).Scan(&vis); err != nil {
		t.Fatal(err)
	}
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "wake three"})
	want("notification", vis.String())

	// The membership row is gone: the stream ends; it never reads the member
	// as restricted to no team.
	testdb.MustExec(t, e.pool, `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, e.orgID, f.restrictedID)
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "wake four"})
	for {
		ev, comments, ok := s.next()
		if !ok {
			found := false
			for _, c := range comments {
				found = found || c == ": credentials no longer valid"
			}
			if !found {
				t.Fatalf("the stream ended without the credentials comment: %v", comments)
			}
			break
		}
		if ev.typ == "notification" {
			t.Fatalf("the stream sent %+v after the membership was removed", ev)
		}
	}
}

// TestInboxStream_AccessEventOnlyOnChange: no access event when nothing
// changed: after two poll ticks a mark-read sends the count only.
func TestInboxStream_AccessEventOnlyOnChange(t *testing.T) {
	f := newTeamFixture(t)
	s := f.openInboxStream(t, f.restricted)
	defer s.end()
	for i := 0; i < 2; i++ {
		ev, _, ok := s.next()
		if !ok || ev.typ == "access" {
			t.Fatalf("first drain: %+v ok=%v", ev, ok)
		}
	}
	time.Sleep(600 * time.Millisecond) // two poll ticks
	testdb.MustExec(t, f.e.pool, `UPDATE notifications SET read_at = now() WHERE id = $1`, f.inbox.visible)
	f.e.createIssue(f.e.chen, map[string]any{"team": testdb.TeamKey, "title": "wake"})
	ev, _, ok := s.next()
	if !ok || ev.typ != "count" || ev.data != `{"count":0}` {
		t.Fatalf("after a mark-read: %+v ok=%v, want the count and no access event", ev, ok)
	}
}

// TestNotifyRoutes_CrossOrg: another org's team and issue in the ticket 7
// paths answer as unknown ones, byte for byte (the cross-org rule).
func TestNotifyRoutes_CrossOrg(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	var other struct {
		Identifier string `json:"identifier"`
	}
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/elsewhere/issues", f.outsider, map[string]any{"team": "ELS", "title": "other org"})).json(t, &other)
	otherIssue := other.Identifier
	for _, c := range []struct{ method, path, made string }{
		{"PUT", "/me/teams/ELS/follow", "/me/teams/ZZZ/follow"},
		{"PUT", "/me/notification-settings/teams/ELS", "/me/notification-settings/teams/ZZZ"},
		{"GET", "/issues/" + otherIssue + "/subscription", "/issues/ELS-999/subscription"},
		{"PUT", "/issues/" + otherIssue + "/subscription", "/issues/ELS-999/subscription"},
	} {
		var body any
		if strings.HasSuffix(c.path, "/subscription") && c.method == "PUT" {
			body = map[string]any{"subscribed": true}
		}
		if strings.Contains(c.path, "notification-settings") {
			body = map[string]any{"events": map[string]any{"comment": map[string]any{"email": "on"}}}
		}
		got := e.do(c.method, f.orgPath(c.path), f.restricted, body)
		want := e.do(c.method, f.orgPath(c.made), f.restricted, body)
		gb := strings.NewReplacer("ELS-1", "<ref>", "ELS", "<ref>").Replace(string(got.body))
		wb := strings.NewReplacer("ELS-999", "<ref>", "ZZZ", "<ref>").Replace(string(want.body))
		if got.status != http.StatusNotFound || gb != wb {
			t.Errorf("%s %s: %d %s, want the 404 of %s: %s", c.method, c.path, got.status, gb, c.made, wb)
		}
	}
}
