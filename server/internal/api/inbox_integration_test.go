package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/notify"
)

// The inbox routes (LAUN-35, docs/internal/decisions-T13-notifications.md
// #27-#31).

const inboxPath = "/api/v1/orgs/orbit/notifications"

// addNote inserts an inbox row for user on issue and returns its id.
func (e *env) addNote(user, issue uuid.UUID, kind string, inbox bool, at time.Time, data string) uuid.UUID {
	e.t.Helper()
	if data == "" {
		data = "{}"
	}
	var id uuid.UUID
	err := e.pool.QueryRow(context.Background(),
		`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, actor_id, data, dedupe_key, inbox, created_at)
		 VALUES ($1, $2, $3, $4, NULL, $5::jsonb, $6, $7, $8) RETURNING id`,
		e.orgID, user, kind, issue, data, uuid.NewString(), inbox, at).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) inbox(token, query string) notify.InboxPage {
	e.t.Helper()
	var p notify.InboxPage
	e.must(200, e.do("GET", inboxPath+query, token, nil)).json(e.t, &p)
	return p
}

func (e *env) unread(token string) int64 {
	e.t.Helper()
	var c notify.UnreadCount
	e.must(200, e.do("GET", inboxPath+"/unread-count", token, nil)).json(e.t, &c)
	return c.Count
}

func ids(p notify.InboxPage) []uuid.UUID {
	out := []uuid.UUID{}
	for _, it := range p.Items {
		out = append(out, it.ID)
	}
	return out
}

// TestInbox_OrderAndPaging: newest first by (created_at, id), a stable
// cursor across equal timestamps, email-only and archived rows left out.
func TestInbox_OrderAndPaging(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	var want []uuid.UUID
	// Five rows share one timestamp; two more are older and newer.
	for i := 0; i < 5; i++ {
		want = append(want, e.addNote(chen.ID, is.ID, "comment", true, base, ""))
	}
	older := e.addNote(chen.ID, is.ID, "assigned", true, base.Add(-time.Minute), "")
	newer := e.addNote(chen.ID, is.ID, "mention", true, base.Add(time.Minute), "")
	emailOnly := e.addNote(chen.ID, is.ID, "comment", false, base.Add(2*time.Minute), "")
	archived := e.addNote(chen.ID, is.ID, "comment", true, base.Add(3*time.Minute), "")
	e.must(200, e.do("PATCH", inboxPath+"/"+archived.String(), e.chen, map[string]any{"archived": true}))
	slices.SortFunc(want, func(a, b uuid.UUID) int { return -strings.Compare(a.String(), b.String()) })
	full := append(append([]uuid.UUID{newer}, want...), older)

	if got := ids(e.inbox(e.chen, "")); !slices.Equal(got, full) {
		t.Fatalf("order %v, want %v", got, full)
	}
	// Walk with a page size of 2: the same order, no repeat, no gap.
	var walked []uuid.UUID
	q := "?limit=2"
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		p := e.inbox(e.chen, q)
		walked = append(walked, ids(p)...)
		if p.NextCursor == nil {
			break
		}
		q = "?limit=2&cursor=" + *p.NextCursor
	}
	if !slices.Equal(walked, full) {
		t.Fatalf("walk %v, want %v", walked, full)
	}
	for _, id := range walked {
		if id == emailOnly || id == archived {
			t.Fatalf("list returned %s", id)
		}
	}
	// A full last page has no cursor; one row more has.
	if p := e.inbox(e.chen, "?limit=7"); p.NextCursor != nil || len(p.Items) != 7 {
		t.Fatalf("exact page: %d items, cursor %v", len(p.Items), p.NextCursor)
	}
	if p := e.inbox(e.chen, "?limit=6"); p.NextCursor == nil {
		t.Fatal("no cursor with a row left")
	}
	// archived=true lists only the archived row.
	if got := ids(e.inbox(e.chen, "?archived=true")); !slices.Equal(got, []uuid.UUID{archived}) {
		t.Fatalf("archived list %v", got)
	}
	// state=unread.
	e.must(200, e.do("PATCH", inboxPath+"/"+newer.String(), e.chen, map[string]any{"read": true}))
	if got := ids(e.inbox(e.chen, "?state=unread")); len(got) != 6 || slices.Contains(got, newer) {
		t.Fatalf("unread list %v", got)
	}
	if got := ids(e.inbox(e.chen, "?state=all")); len(got) != 7 {
		t.Fatalf("all list %v", got)
	}
	// Bad input.
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?state=maybe", "?archived=1", "?cursor=!!", "?cursor=e30"} {
		if r := e.do("GET", inboxPath+q, e.chen, nil); r.status != 400 {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
	}
	if n := e.unread(e.chen); n != 6 {
		t.Fatalf("unread count %d, want 6 (the email-only and the archived row do not count)", n)
	}
}

// TestInbox_ItemShape: actor, issue and data come from one query; no text.
func TestInbox_ItemShape(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "Shape me", "description": "secret words"})
	var from, to uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT min(id::text)::uuid, max(id::text)::uuid FROM workflow_states WHERE org_id = $1`, e.orgID).Scan(&from, &to); err != nil {
		t.Fatal(err)
	}
	cid := uuid.New()
	id := e.addNote(chen.ID, is.ID, "state_change", true, time.Now(),
		`{"fromStateId":"`+from.String()+`","toStateId":"`+to.String()+`","commentId":"`+cid.String()+`"}`)
	if _, err := e.pool.Exec(context.Background(), `UPDATE notifications SET actor_id = $1 WHERE id = $2`, e.user("claude").ID, id); err != nil {
		t.Fatal(err)
	}
	var fromName, toName string
	_ = e.pool.QueryRow(context.Background(), `SELECT name FROM workflow_states WHERE id = $1`, from).Scan(&fromName)
	_ = e.pool.QueryRow(context.Background(), `SELECT name FROM workflow_states WHERE id = $1`, to).Scan(&toName)
	p := e.inbox(e.chen, "")
	if len(p.Items) != 1 {
		t.Fatalf("items %d", len(p.Items))
	}
	it := p.Items[0]
	if it.Kind != "state_change" || it.Actor == nil || it.Actor.Username != "claude" || it.Issue.ID != is.ID ||
		it.Issue.Identifier != is.Identifier || it.Issue.Title != "Shape me" || it.Issue.TeamKey != "ORB" || it.Issue.StateName == "" ||
		it.ReadAt != nil || it.ArchivedAt != nil || it.Data.CommentID == nil || *it.Data.CommentID != cid ||
		it.Data.FromStateName == nil || *it.Data.FromStateName != fromName || it.Data.ToStateName == nil || *it.Data.ToStateName != toName {
		t.Fatalf("item %+v", it)
	}
	r := e.do("GET", inboxPath, e.chen, nil)
	if strings.Contains(string(r.body), "secret words") {
		t.Fatalf("the description is in the list: %s", r.body)
	}
	// A system move has no actor: null.
	e.addNote(chen.ID, is.ID, "assigned", true, time.Now().Add(time.Second), "")
	if first := e.inbox(e.chen, "").Items[0]; first.Actor != nil || !strings.Contains(string(e.do("GET", inboxPath, e.chen, nil).body), `"actor":null`) {
		t.Fatalf("actor %+v", first.Actor)
	}
}

// TestInbox_PatchSetsState: read and archived both ways; a repeat keeps the
// first time; an empty or unknown body is 400.
func TestInbox_PatchSetsState(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	id := e.addNote(chen.ID, is.ID, "comment", true, time.Now(), "")
	path := inboxPath + "/" + id.String()
	patch := func(body map[string]any) notify.InboxItem {
		t.Helper()
		var it notify.InboxItem
		e.must(200, e.do("PATCH", path, e.chen, body)).json(t, &it)
		return it
	}
	first := patch(map[string]any{"read": true})
	if first.ReadAt == nil || e.unread(e.chen) != 0 {
		t.Fatalf("read: %+v", first)
	}
	time.Sleep(5 * time.Millisecond)
	again := patch(map[string]any{"read": true})
	if again.ReadAt == nil || !again.ReadAt.Equal(*first.ReadAt) {
		t.Fatalf("a repeat of read:true moved readAt: %v then %v", first.ReadAt, again.ReadAt)
	}
	if un := patch(map[string]any{"read": false}); un.ReadAt != nil || e.unread(e.chen) != 1 {
		t.Fatalf("read:false: %+v", un)
	}
	if un := patch(map[string]any{"read": false}); un.ReadAt != nil {
		t.Fatalf("repeat of read:false: %+v", un)
	}
	// Archive hides the row and its unread count; read stays as it was.
	if ar := patch(map[string]any{"archived": true}); ar.ArchivedAt == nil || ar.ReadAt != nil {
		t.Fatalf("archive: %+v", ar)
	}
	if len(e.inbox(e.chen, "").Items) != 0 || e.unread(e.chen) != 0 {
		t.Fatal("an archived row is in the live list or the count")
	}
	arFirst := patch(map[string]any{"archived": true})
	if both := patch(map[string]any{"archived": false, "read": true}); both.ArchivedAt != nil || both.ReadAt == nil || arFirst.ArchivedAt == nil {
		t.Fatalf("both: %+v", both)
	}
	if len(e.inbox(e.chen, "").Items) != 1 {
		t.Fatal("an unarchived row is not back")
	}
	for _, body := range []any{map[string]any{}, map[string]any{"seen": true}, map[string]any{"read": "yes"}} {
		if r := e.do("PATCH", path, e.chen, body); r.status != 400 {
			t.Fatalf("%v: %d %s", body, r.status, r.body)
		}
	}
	if r := e.do("PATCH", path, e.chen, nil); r.status != 400 {
		t.Fatalf("no body: %d", r.status)
	}
	// An id that is not a uuid, or that nobody has, is 404.
	e.must(404, e.do("PATCH", inboxPath+"/not-a-uuid", e.chen, map[string]any{"read": true}))
	e.must(404, e.do("PATCH", inboxPath+"/"+uuid.NewString(), e.chen, map[string]any{"read": true}))
}

// TestInbox_ReadAll: before is required; a retry never marks a newer row.
func TestInbox_ReadAll(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	for i := 0; i < 3; i++ {
		e.addNote(chen.ID, is.ID, "comment", true, t0.Add(time.Duration(i)*time.Second), "")
	}
	emailOnly := e.addNote(chen.ID, is.ID, "comment", false, t0, "")
	cut := t0.Add(2 * time.Second) // the newest of the three: a row at the cut-off is marked
	for _, body := range []any{map[string]any{}, nil, map[string]any{"before": "yesterday"}, map[string]any{"before": nil}} {
		if r := e.do("POST", inboxPath+"/read-all", e.chen, body); r.status != 400 {
			t.Fatalf("%v: %d %s", body, r.status, r.body)
		}
	}
	if e.unread(e.chen) != 3 {
		t.Fatal("a refused read-all changed rows")
	}
	// Nothing older than the cut-off yet matches a cut-off before every row.
	var res notify.ReadAllResult
	e.must(200, e.do("POST", inboxPath+"/read-all", e.chen, map[string]any{"before": t0.Add(-time.Second)})).json(t, &res)
	if res.Marked != 0 || e.unread(e.chen) != 3 {
		t.Fatalf("early cut-off marked %d", res.Marked)
	}
	// A row arrives after the client loaded its list.
	late := e.addNote(chen.ID, is.ID, "comment", true, cut.Add(time.Second), "")
	e.must(200, e.do("POST", inboxPath+"/read-all", e.chen, map[string]any{"before": cut})).json(t, &res)
	if res.Marked != 3 || e.unread(e.chen) != 1 {
		t.Fatalf("marked %d, unread %d", res.Marked, e.unread(e.chen))
	}
	// A retry marks nothing and never the late row.
	e.must(200, e.do("POST", inboxPath+"/read-all", e.chen, map[string]any{"before": cut})).json(t, &res)
	if res.Marked != 0 || e.unread(e.chen) != 1 {
		t.Fatalf("retry marked %d", res.Marked)
	}
	var readAt *time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT read_at FROM notifications WHERE id = $1`, late).Scan(&readAt)
	if readAt != nil {
		t.Fatal("the late row was marked")
	}
	_ = e.pool.QueryRow(context.Background(), `SELECT read_at FROM notifications WHERE id = $1`, emailOnly).Scan(&readAt)
	if readAt != nil {
		t.Fatal("an email-only row was marked")
	}
}

// TestInbox_Access: agents get 403 on every route, a read token cannot
// write, nobody reads or changes another user's or org's row, and an
// email-only row is 404.
func TestInbox_Access(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	mine := e.addNote(chen.ID, is.ID, "comment", true, time.Now(), "")
	emailOnly := e.addNote(chen.ID, is.ID, "comment", false, time.Now(), "")
	one := inboxPath + "/" + mine.String()

	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", inboxPath, nil},
		{"GET", inboxPath + "/unread-count", nil},
		{"GET", inboxPath + "/stream", nil},
		{"PATCH", one, map[string]any{"read": true}},
		{"POST", inboxPath + "/read-all", map[string]any{"before": time.Now()}},
	}
	for _, rt := range routes {
		r := e.do(rt.method, rt.path, e.claude, rt.body)
		if r.status != 403 || r.errCode(t) != "forbidden" {
			t.Fatalf("agent %s %s: %d %s", rt.method, rt.path, r.status, r.body)
		}
		if r := e.do(rt.method, rt.path, "", rt.body); r.status != 401 {
			t.Fatalf("no token %s %s: %d", rt.method, rt.path, r.status)
		}
	}
	// A read-only token reads and cannot write.
	ro, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	e.must(200, e.do("GET", inboxPath, ro, nil))
	for _, rt := range routes[3:] {
		if r := e.do(rt.method, rt.path, ro, rt.body); r.status != 403 || r.errCode(t) != "insufficient_scope" {
			t.Fatalf("read token %s %s: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
	// Another member of the same org: an empty inbox, 404 on chen's id, and
	// read-all changes none of chen's rows. An admin is no exception (chen
	// is the admin and dana a plain member, so test both ways).
	dana := e.newHuman("dana", "dana@example.com", "member")
	dt, _ := e.newToken(dana.ID, tokenOpts{pin: &e.orgID})
	if len(e.inbox(dt, "").Items) != 0 || e.unread(dt) != 0 {
		t.Fatal("dana sees chen's rows")
	}
	e.must(404, e.do("PATCH", one, dt, map[string]any{"read": true}))
	e.must(200, e.do("POST", inboxPath+"/read-all", dt, map[string]any{"before": time.Now().Add(time.Hour)}))
	danaNote := e.addNote(dana.ID, is.ID, "comment", true, time.Now(), "")
	e.must(404, e.do("PATCH", inboxPath+"/"+danaNote.String(), e.chen, map[string]any{"read": true}))
	if e.unread(e.chen) != 1 {
		t.Fatal("another user's read-all changed chen's rows")
	}
	// An email-only row is never reachable.
	e.must(404, e.do("PATCH", inboxPath+"/"+emailOnly.String(), e.chen, map[string]any{"read": true}))
	if n := e.countRows(`SELECT count(*) FROM notifications WHERE id = $1 AND read_at IS NOT NULL`, emailOnly); n != 0 {
		t.Fatal("an email-only row was changed")
	}
}

// TestInbox_CrossOrg: chen is in orbit and acme. A row of orbit is not in
// acme's list or count and is 404 by acme's path; a token pinned to orbit
// gets 404 on acme.
func TestInbox_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "member")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	orb := e.createIssue(tok, map[string]any{"title": "orbit issue"})
	var acmeIssue struct{ ID uuid.UUID }
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"team": "ACM", "title": "acme issue"})).json(t, &acmeIssue)
	orbNote := e.addNote(chen.ID, orb.ID, "comment", true, time.Now(), "")
	var acmeNote uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, 'k', true) RETURNING id`,
		acmeID, chen.ID, acmeIssue.ID).Scan(&acmeNote); err != nil {
		t.Fatal(err)
	}
	var p notify.InboxPage
	e.must(200, e.do("GET", "/api/v1/orgs/acme/notifications", tok, nil)).json(t, &p)
	if got := ids(p); !slices.Equal(got, []uuid.UUID{acmeNote}) {
		t.Fatalf("acme list %v", got)
	}
	if got := ids(e.inbox(tok, "")); !slices.Equal(got, []uuid.UUID{orbNote}) {
		t.Fatalf("orbit list %v", got)
	}
	e.must(404, e.do("PATCH", "/api/v1/orgs/acme/notifications/"+orbNote.String(), tok, map[string]any{"read": true}))
	e.must(404, e.do("PATCH", inboxPath+"/"+acmeNote.String(), tok, map[string]any{"read": true}))
	// read-all in acme leaves orbit's row unread.
	e.must(200, e.do("POST", "/api/v1/orgs/acme/notifications/read-all", tok, map[string]any{"before": time.Now().Add(time.Hour)}))
	if e.unread(tok) != 1 {
		t.Fatal("acme's read-all changed an orbit row")
	}
	// A token pinned to orbit has no access to acme.
	for _, rt := range []struct{ m, p string }{{"GET", "/notifications"}, {"GET", "/notifications/unread-count"}, {"GET", "/notifications/stream"}} {
		e.must(404, e.do(rt.m, "/api/v1/orgs/acme"+rt.p, e.chen, nil))
	}
}

// sseEvent is one event of a stream.
type sseEvent struct{ id, typ, data string }

// openInboxStream opens the stream and returns a channel of its events (a
// closed channel when the server ends it) and a close function.
func (e *env) openInboxStream(token, lastID string) (<-chan sseEvent, func()) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+inboxPath+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("X-Accel-Buffering") != "no" {
		cancel()
		e.t.Fatalf("stream response %d %v", res.StatusCode, res.Header)
	}
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		br := bufio.NewReader(res.Body)
		var ev sseEvent
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.typ != "":
				out <- ev
				ev = sseEvent{}
			}
		}
	}()
	return out, func() { cancel(); _ = res.Body.Close() }
}

func (e *env) nextEvent(ch <-chan sseEvent) sseEvent {
	e.t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			e.t.Fatal("stream ended")
		}
		return ev
	case <-time.After(10 * time.Second):
		e.t.Fatal("no stream event in 10 seconds")
	}
	return sseEvent{}
}

func (e *env) noEvent(ch <-chan sseEvent, d time.Duration) {
	e.t.Helper()
	select {
	case ev, ok := <-ch:
		e.t.Fatalf("unexpected stream event %+v (open %v)", ev, ok)
	case <-time.After(d):
	}
}

func (e *env) wantCount(ch <-chan sseEvent, n int64) {
	e.t.Helper()
	ev := e.nextEvent(ch)
	var c notify.UnreadCount
	if ev.typ != "count" || ev.id != "" || json.Unmarshal([]byte(ev.data), &c) != nil || c.Count != n {
		e.t.Fatalf("event %+v, want count %d", ev, n)
	}
}

// wakeFromAnotherServer makes the NOTIFY that another server's consumer
// sends after its commit.
func (e *env) wakeFromAnotherServer() {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `SELECT pg_notify('devops_events', $1)`, e.orgID.String()); err != nil {
		e.t.Fatal(err)
	}
}

// TestInbox_Stream: a new row arrives through NOTIFY (the poll is an hour
// away, so only the wake-up can deliver it); email-only and archived rows are
// never sent; a mark-read in another connection gives a count event.
func TestInbox_Stream(t *testing.T) {
	e := newEnvWith(t, envOpts{ssePoll: time.Hour})
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	e.addNote(chen.ID, is.ID, "comment", true, time.Now().Add(-time.Minute), "") // before the stream: not sent
	ch, closeFn := e.openInboxStream(e.chen, "")
	defer closeFn()
	e.wantCount(ch, 1) // the count is sent once at the start

	// A row from another "server": insert, then NOTIFY.
	hidden := e.addNote(chen.ID, is.ID, "comment", false, time.Now(), "")
	e.wakeFromAnotherServer()
	e.noEvent(ch, 400*time.Millisecond) // an email-only row: no event, no count change
	id := e.addNote(chen.ID, is.ID, "mention", true, time.Now(), "")
	e.wakeFromAnotherServer()
	ev := e.nextEvent(ch)
	var row notify.StreamRow
	if ev.typ != "notification" || json.Unmarshal([]byte(ev.data), &row) != nil || row.ID != id || row.Kind != "mention" || ev.id == "" || ev.id == "0" {
		t.Fatalf("event %+v", ev)
	}
	e.wantCount(ch, 2)

	// A mark-read in another connection (a second tab) gives a count event.
	e.must(200, e.do("PATCH", inboxPath+"/"+id.String(), e.chen, map[string]any{"read": true}))
	e.wantCount(ch, 1)
	// Unread again, then archive: each moves the count only.
	e.must(200, e.do("PATCH", inboxPath+"/"+id.String(), e.chen, map[string]any{"read": false}))
	e.wantCount(ch, 2)
	e.must(200, e.do("PATCH", inboxPath+"/"+id.String(), e.chen, map[string]any{"archived": true}))
	e.wantCount(ch, 1)
	e.must(200, e.do("POST", inboxPath+"/read-all", e.chen, map[string]any{"before": time.Now().Add(time.Hour)}))
	e.wantCount(ch, 0)
	// A change that leaves the count as it was sends nothing.
	e.must(200, e.do("PATCH", inboxPath+"/"+id.String(), e.chen, map[string]any{"archived": true}))
	e.noEvent(ch, 400*time.Millisecond)
	_ = hidden

	// A row another user gets is not sent at all.
	dana := e.newHuman("dana", "dana@example.com", "member")
	e.addNote(dana.ID, is.ID, "comment", true, time.Now(), "")
	e.wakeFromAnotherServer()
	e.noEvent(ch, 300*time.Millisecond)
}

// TestInbox_StreamResume: Last-Event-ID replays the unarchived rows after it.
func TestInbox_StreamResume(t *testing.T) {
	e := newEnvWith(t, envOpts{ssePoll: time.Hour})
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	a := e.addNote(chen.ID, is.ID, "comment", true, time.Now(), "")
	b := e.addNote(chen.ID, is.ID, "assigned", true, time.Now(), "")
	ch, closeFn := e.openInboxStream(e.chen, "0")
	defer closeFn()
	var got []uuid.UUID
	for len(got) < 2 {
		ev := e.nextEvent(ch)
		if ev.typ == "count" {
			continue
		}
		var row notify.StreamRow
		_ = json.Unmarshal([]byte(ev.data), &row)
		got = append(got, row.ID)
	}
	if !slices.Equal(got, []uuid.UUID{a, b}) {
		t.Fatalf("resume gave %v, want %v", got, []uuid.UUID{a, b})
	}
	if r := e.do("GET", inboxPath+"/stream", e.chen, nil, "Last-Event-ID", "abc"); r.status != 400 {
		t.Fatalf("bad Last-Event-ID: %d", r.status)
	}
}

// TestInbox_StreamPolls: without any NOTIFY the poll still delivers a row.
func TestInbox_StreamPolls(t *testing.T) {
	e := newEnvWith(t, envOpts{ssePoll: 100 * time.Millisecond, noListener: true})
	chen := e.user("chen")
	is := e.createIssue(e.chen, map[string]any{"title": "x"})
	ch, closeFn := e.openInboxStream(e.chen, "")
	defer closeFn()
	e.wantCount(ch, 0)
	id := e.addNote(chen.ID, is.ID, "comment", true, time.Now(), "")
	ev := e.nextEvent(ch)
	var row notify.StreamRow
	if ev.typ != "notification" || json.Unmarshal([]byte(ev.data), &row) != nil || row.ID != id {
		t.Fatalf("event %+v", ev)
	}
	e.wantCount(ch, 1)
}

// TestInbox_StreamEnds: the stream ends when the membership goes, and a
// closed stream leaves no broker subscription.
func TestInbox_StreamEnds(t *testing.T) {
	e := newEnvWith(t, envOpts{ssePoll: 100 * time.Millisecond})
	dana := e.newHuman("dana", "dana@example.com", "member")
	dt, _ := e.newToken(dana.ID, tokenOpts{pin: &e.orgID})
	ch, closeFn := e.openInboxStream(dt, "")
	defer closeFn()
	e.wantCount(ch, 0)
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, e.orgID, dana.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an event after the membership went")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end")
	}
	closeFn()
	deadline := time.Now().Add(2 * time.Second)
	for e.broker.Subscribers() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.broker.Subscribers(); n != 0 {
		t.Fatalf("%d stream subscriptions leaked", n)
	}
}
