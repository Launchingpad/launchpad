package api

import (
	"bufio"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/events"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// The integration half of Test D (decisions-T20 #18, #53 to #56): the change
// feed and its stream, read by a restricted member, an unrestricted member
// and an admin. The unit half is TestEventsEmitScope (internal/events).

// hiddenKinds are the event types the test below makes in the hidden team
// through their routes. Each must be stored with team HID in its scope.
var hiddenKinds = []string{
	events.IssueCreated, events.IssueUpdated, events.IssueDeleted,
	events.CommentCreated, events.CommentUpdated, events.CommentDeleted,
	events.RelationCreated, events.RelationDeleted, events.AttachmentCreated,
	events.TeamCreated, events.TeamUpdated, events.TeamDeleted,
	events.ProjectCreated, events.ProjectUpdated, events.ProjectDeleted, events.ProjectUpdateCreated,
	events.MilestoneCreated, events.MilestoneUpdated, events.DocumentCreated, events.DocumentUpdated,
	events.LabelCreated, events.AgentGuidanceUpdated, events.CycleUpdated,
	events.ProductCreated, events.ProductUpdated, events.ProductTeamsChanged, events.ProductDeleted,
}

// uncoveredKinds may have a team scope but are not made by the test below,
// with the reason. A new event type with a team scope must go in one of the
// two lists, so it cannot be forgotten.
var uncoveredKinds = map[string]string{
	events.CycleCreated:     "made by the cycle job (MaintainCycles); its scope comes from emitAll with the team, as cycle.updated",
	events.CycleCompleted:   "made by the cycle job and POST /cycles/{cycle}/complete; same emitAll path as cycle.updated",
	events.CycleDeleted:     "DELETE /cycles/{cycle} needs a contiguous chain; same emitAll path as cycle.updated",
	events.ProjectUpdateDue: "made by the update schedule job; projectEventScope, as project_update.created",
	events.ChatServiceUpdated: "needs the chat test env (newChatEnv); the scope is the service's team, " +
		"shown by the emitChat code and checkScope (any only)",
}

// eventRow is one stored event with its scope.
type eventRow struct {
	seq   int64
	typ   string
	scope string
	teams []uuid.UUID
}

// visibleTo is the #55 rule for a restricted viewer, written out again in
// Go so the test does not trust the query it checks.
func (r eventRow) visibleTo(teams ...uuid.UUID) bool {
	switch r.scope {
	case events.ScopeKindOrg:
		return true
	case events.ScopeKindAny:
		return slices.ContainsFunc(r.teams, func(id uuid.UUID) bool { return slices.Contains(teams, id) })
	case events.ScopeKindAll:
		return len(r.teams) > 0 && !slices.ContainsFunc(r.teams, func(id uuid.UUID) bool { return !slices.Contains(teams, id) })
	}
	return false // legacy, admin
}

func (f *teamFixture) eventRows() []eventRow {
	rows, err := f.e.pool.Query(context.Background(), `SELECT seq, type, scope, team_ids FROM events WHERE org_id = $1 ORDER BY seq`, f.e.orgID)
	if err != nil {
		f.e.t.Fatal(err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var r eventRow
		if err := rows.Scan(&r.seq, &r.typ, &r.scope, &r.teams); err != nil {
			f.e.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.e.t.Fatal(err)
	}
	return out
}

// feedSeqs reads GET /events from seq 0 as token, following nextCursor.
func (f *teamFixture) feedSeqs(token string) []int64 {
	var out []int64
	after := "0"
	for range 50 {
		var p page[tracker.Event]
		f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/events?limit=500&after="+after), token, nil)).json(f.e.t, &p)
		for _, ev := range p.Items {
			out = append(out, ev.Seq)
		}
		if p.NextCursor == nil {
			return out
		}
		after = *p.NextCursor
	}
	f.e.t.Fatal("the feed does not end")
	return nil
}

// streamSeqs reads the stream from seq 0 up to a marker (readStream).
func (f *teamFixture) streamSeqs(token string) []int64 {
	res := f.readStream(token, f.orgPath("/events/stream"))
	if res.status != http.StatusOK {
		f.e.t.Fatalf("stream %d %s", res.status, res.body)
	}
	var out []int64
	for _, l := range strings.Split(string(res.body), "\n") {
		if v, ok := strings.CutPrefix(l, "id: "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				f.e.t.Fatal(err)
			}
			out = append(out, n)
		}
	}
	return out
}

// insertRawEvent stores an event with a scope Emit never writes (legacy, as
// before migration 0038) or cannot write for any kind yet (admin, ticket 9).
func (f *teamFixture) insertRawEvent(scope string) int64 {
	ctx := context.Background()
	seq, err := f.e.q.NextEventSeq(ctx, f.e.orgID)
	if err != nil {
		f.e.t.Fatal(err)
	}
	if scope == events.ScopeKindLegacy {
		testdb.MustExec(f.e.t, f.e.pool, `INSERT INTO events (org_id, seq, type, entity_type, entity_id, payload)
			VALUES ($1, $2, 'issue.updated', 'issue', uuidv7(), '{}')`, f.e.orgID, seq)
	} else {
		testdb.MustExec(f.e.t, f.e.pool, `INSERT INTO events (org_id, seq, type, entity_type, entity_id, payload, scope)
			VALUES ($1, $2, 'member.access_changed', 'org', $1, '{}', $3)`, f.e.orgID, seq, scope)
	}
	return seq
}

// makeEventKinds makes every kind of hiddenKinds in the hidden team HID
// through the routes, as the admin, then org-level and visible-team events.
// It returns the visible issue it makes last. Test D and the backfill round
// trip (TestEventsBackfill_RoundTrip) share it.
func (f *teamFixture) makeEventKinds(t *testing.T) tracker.Issue {
	t.Helper()
	e := f.e
	do := func(method, path string, body any, want int) resp {
		t.Helper()
		return e.must(want, e.do(method, f.orgPath(path), e.chen, body))
	}
	var id struct {
		ID uuid.UUID `json:"id"`
	}
	hid := f.hidden

	// Writes in the hidden team, as the admin, through the routes.
	do("PATCH", "/issues/"+hid["issue"], map[string]any{"title": "hidden issue renamed"}, http.StatusOK)
	second := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden two"})
	do("POST", "/issues/"+hid["issue"]+"/relations", map[string]any{"type": "related", "issue": second.Identifier}, http.StatusCreated)
	do("POST", "/issues/"+f.visible+"/relations", map[string]any{"type": "blocks", "issue": hid["issue"]}, http.StatusCreated).json(t, &id)
	cross := id.ID
	do("PATCH", "/comments/"+hid["comment"], map[string]any{"body": "hidden comment edited"}, http.StatusOK)
	do("POST", "/issues/"+hid["issue"]+"/attachments", map[string]any{"kind": "link", "url": "https://git.example.com/mr/7"}, http.StatusCreated)
	do("PATCH", "/teams/HID", map[string]any{"name": "Hidden team"}, http.StatusOK)
	do("PATCH", "/projects/"+hid["project"], map[string]any{"description": "hidden words"}, http.StatusOK)
	do("POST", "/projects/"+hid["project"]+"/updates", map[string]any{"health": "on_track"}, http.StatusCreated)
	do("PATCH", "/milestones/"+hid["milestone"], map[string]any{"name": "Hidden step two"}, http.StatusOK)
	do("PATCH", "/documents/"+hid["document"], map[string]any{"title": "Hidden doc two"}, http.StatusOK)
	do("POST", "/documents/"+hid["document"]+"/comments", map[string]any{"body": "hidden doc note"}, http.StatusCreated).json(t, &id)
	do("DELETE", "/comments/"+id.ID.String(), nil, http.StatusNoContent)
	do("POST", "/labels", map[string]any{"name": "hidden-label-two", "team": "HID"}, http.StatusCreated)
	do("PUT", "/agent-guidance", map[string]any{"text": "hidden team guidance", "team": "HID"}, http.StatusOK)
	do("PATCH", "/cycles/"+hid["cycle"], map[string]any{"name": "Hidden sprint"}, http.StatusOK)
	do("POST", "/products", map[string]any{"name": "Hidden product", "teams": []string{"HID"}}, http.StatusCreated).json(t, &id)
	do("PATCH", "/products/"+id.ID.String(), map[string]any{"name": "Hidden product two"}, http.StatusOK)
	do("POST", "/products", map[string]any{"name": "Hidden product three"}, http.StatusCreated).json(t, &id)
	do("PUT", "/products/"+id.ID.String()+"/teams/HID", nil, http.StatusNoContent)
	do("PATCH", "/products/"+id.ID.String(), map[string]any{"archived": true}, http.StatusOK)
	do("DELETE", "/products/"+id.ID.String(), nil, http.StatusNoContent)
	do("POST", "/projects", map[string]any{"name": "Hidden plan two", "teams": []string{"HID"}}, http.StatusCreated).json(t, &id)
	do("DELETE", "/projects/"+id.ID.String(), nil, http.StatusNoContent)
	do("DELETE", "/issues/"+second.Identifier, nil, http.StatusNoContent)
	do("POST", "/teams", map[string]any{"key": "GON", "name": "Gone"}, http.StatusCreated)
	do("DELETE", "/teams/GON", nil, http.StatusNoContent)
	// Org-level and visible-team events, which the restricted member reads.
	do("POST", "/project-statuses", map[string]any{"kind": "planned", "name": "Queued", "color": "#112233"}, http.StatusCreated)
	do("POST", "/labels", map[string]any{"name": "org-wide-label"}, http.StatusCreated)
	visibleNew := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible two"})
	do("DELETE", "/issues/"+f.visible+"/relations/"+cross.String(), nil, http.StatusNoContent)

	return visibleNew
}

// TestEventsEmitScope_RestrictedViewer is Test D: every kind made in the
// hidden team through its route is stored with the team in its scope, and a
// restricted member reads none of them from GET /events or the stream, while
// an unrestricted member and an admin read them all. A relation between a
// visible and a hidden issue is hidden until the member sees both teams.
// Legacy rows show to unrestricted viewers only; admin rows to admins only.
func TestEventsEmitScope_RestrictedViewer(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	// The fixture makes cross-team relations of its own (ticket 4: an edge
	// from the visible issue and a removed one from the child); only the
	// edges made below are counted.
	base := f.eventSeq()
	visibleNew := f.makeEventKinds(t)

	rows := f.eventRows()
	byScope := map[string]int{}
	hiddenSeq := map[int64]string{}
	var crossSeqs []int64
	stored, withHID := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		if slices.Contains(r.teams, f.hidID) {
			withHID[r.typ] = true
		}
		byScope[r.scope]++
		if r.scope == events.ScopeKindLegacy || r.scope == "" {
			t.Errorf("event %d %s was stored with scope %q: Emit must name one", r.seq, r.typ, r.scope)
		}
		if !r.visibleTo(f.orbID) {
			hiddenSeq[r.seq] = r.typ
			stored[r.typ] = true
		}
		// A relation between ORB and HID is all-of (#54). Other kinds may span
		// both teams too (the planning fixture of ticket 5): the shared
		// project's events are any-of, the mixed product's are all-of; this
		// check is about relations only.
		if strings.HasPrefix(r.typ, "relation.") && slices.Contains(r.teams, f.hidID) && slices.Contains(r.teams, f.orbID) {
			if r.scope != events.ScopeKindAll {
				t.Errorf("event %d %s spans ORB and HID with scope %q, want all", r.seq, r.typ, r.scope)
			}
			if r.seq > base {
				crossSeqs = append(crossSeqs, r.seq)
			}
		}
	}
	for _, k := range hiddenKinds {
		switch {
		case !stored[k]:
			t.Errorf("no %s event is hidden from the member of ORB", k)
		case k == events.TeamDeleted:
			// The deleted team is GON, not HID: hidden, as checked above.
		case !withHID[k]:
			t.Errorf("no %s event was stored with the hidden team HID in its scope", k)
		}
	}
	for k, rule := range events.KindScopes() {
		teamScoped := slices.Contains(rule, events.ScopeKindAny) || slices.Contains(rule, events.ScopeKindAll)
		_, skipped := uncoveredKinds[k]
		if teamScoped && !slices.Contains(hiddenKinds, k) && !skipped {
			t.Errorf("event kind %s can have a team scope but Test D neither makes it nor says why (hiddenKinds, uncoveredKinds)", k)
		}
	}
	if len(crossSeqs) != 2 {
		t.Fatalf("want the created and the deleted cross-team relation, got %v", crossSeqs)
	}
	if byScope[events.ScopeKindOrg] < 2 {
		t.Fatalf("want org-scoped events (status, label): %v", byScope)
	}

	legacy := f.insertRawEvent(events.ScopeKindLegacy)
	adminOnly := f.insertRawEvent(events.ScopeKindAdmin)

	for _, read := range []struct {
		name string
		seqs func(string) []int64
	}{{"GET /events", f.feedSeqs}, {"GET /events/stream", f.streamSeqs}} {
		t.Run(read.name, func(t *testing.T) {
			got := read.seqs(f.restricted)
			for _, s := range got {
				if typ, hidden := hiddenSeq[s]; hidden {
					t.Errorf("the restricted member read event %d %s of the hidden team", s, typ)
				}
				if s == legacy || s == adminOnly {
					t.Errorf("the restricted member read the %s row %d", map[bool]string{true: "legacy", false: "admin"}[s == legacy], s)
				}
			}
			// What the restricted member does read: the visible issue
			// events and the org events.
			for _, r := range f.eventRows() {
				if r.visibleTo(f.orbID) && !slices.Contains(got, r.seq) {
					t.Errorf("the restricted member does not read event %d %s (scope %s %v)", r.seq, r.typ, r.scope, r.teams)
				}
			}
			all := f.eventRows()
			member := read.seqs(f.member)
			admin := read.seqs(e.chen)
			for _, r := range all {
				if !slices.Contains(admin, r.seq) {
					t.Errorf("the admin does not read event %d %s (%s)", r.seq, r.typ, r.scope)
				}
				if (r.seq == adminOnly) == slices.Contains(member, r.seq) {
					t.Errorf("the all-teams member reads event %d %s (%s): %v, want %v", r.seq, r.typ, r.scope,
						slices.Contains(member, r.seq), r.seq != adminOnly)
				}
			}
		})
	}
	if t.Failed() {
		return
	}

	// The relation events are all-of: a member who sees both teams reads
	// them, and still no other hidden event appears to a member of ORB only.
	testdb.PutInTeams(t, e.pool, e.orgID, f.restrictedID, f.hidID)
	got := f.feedSeqs(f.restricted)
	for _, s := range crossSeqs {
		if !slices.Contains(got, s) {
			t.Errorf("a member of ORB and HID does not read the cross-team relation event %d", s)
		}
	}
	if slices.Contains(got, legacy) || slices.Contains(got, adminOnly) {
		t.Errorf("a restricted member of both teams reads a legacy or an admin row")
	}
	if !slices.Contains(got, latestSeqOf(f, visibleNew.ID)) {
		t.Errorf("the restricted member does not read the visible issue made last")
	}
}

// latestSeqOf is the seq of the newest event about an entity.
func latestSeqOf(f *teamFixture, entity uuid.UUID) int64 {
	var n int64
	if err := f.e.pool.QueryRow(context.Background(), `SELECT max(seq) FROM events WHERE org_id = $1 AND entity_id = $2`, f.e.orgID, entity).Scan(&n); err != nil {
		f.e.t.Fatal(err)
	}
	return n
}

// sseReader reads named events and comments from an SSE response.
type sseReader struct {
	t   *testing.T
	br  *bufio.Reader
	end func()
}

func (f *teamFixture) openStream(t *testing.T, token string) *sseReader {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, _ := http.NewRequestWithContext(ctx, "GET", f.e.srv.URL+f.orgPath("/events/stream"), nil)
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
	// The first block is the retry line and the connected comment.
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

// next returns the next event (comments are skipped), or ok false when the
// stream ended.
func (r *sseReader) next() (ev sseEvent, comments []string, ok bool) {
	r.t.Helper()
	for {
		line, err := r.br.ReadString('\n')
		if err != nil {
			return ev, comments, false
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, ": "):
			if line != ": ping" {
				comments = append(comments, line)
			}
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		case line == "" && ev.typ != "":
			return ev, comments, true
		}
	}
}

// TestEventsStream_AccessChange (#56): the stream re-reads the access at the
// start of every drain. With the poll set to an hour, only a NOTIFY wakes the
// stream: a team added sends one access event and then the new team's event;
// a team removed sends access and then no event of that team; a removed
// membership ends the stream. A second stream with the 250 ms poll gets the
// access event with no event to wake it.
func TestEventsStream_AccessChange(t *testing.T) {
	f := newTeamFixtureWith(t, envOpts{ssePoll: time.Hour})
	e := f.e
	s := f.openStream(t, f.restricted)
	defer s.end()

	testdb.PutInTeams(t, e.pool, e.orgID, f.restrictedID, f.hidID)
	now := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "now visible"})
	ev, _, ok := s.next()
	if !ok || ev.typ != "access" || ev.id != "" || ev.data != "{}" {
		t.Fatalf("first after the access change: %+v ok=%v, want an access event with no id and data {}", ev, ok)
	}
	ev, _, ok = s.next()
	if !ok || ev.typ != events.IssueCreated || !strings.Contains(ev.data, now.ID.String()) {
		t.Fatalf("after access: %+v ok=%v, want the new team's issue.created", ev, ok)
	}

	testdb.MustExec(t, e.pool, `DELETE FROM team_members WHERE org_id = $1 AND user_id = $2 AND team_id = $3`, e.orgID, f.restrictedID, f.hidID)
	hidden := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden again"})
	visible := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "still visible"})
	ev, _, ok = s.next()
	if !ok || ev.typ != "access" {
		t.Fatalf("after the removal: %+v ok=%v, want access", ev, ok)
	}
	ev, _, ok = s.next()
	if !ok || ev.typ != events.IssueCreated || !strings.Contains(ev.data, visible.ID.String()) || strings.Contains(ev.data, hidden.ID.String()) {
		t.Fatalf("after the removal: %+v ok=%v, want only the visible issue", ev, ok)
	}

	// The membership row is gone: the next drain ends the stream; it never
	// treats the member as restricted to no team.
	testdb.MustExec(t, e.pool, `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, e.orgID, f.restrictedID)
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "after the removal"})
	ev, comments, ok := s.next()
	if ok {
		t.Fatalf("the stream sent %+v after the membership was removed", ev)
	}
	if !slices.Contains(comments, ": credentials no longer valid") {
		t.Fatalf("the stream ended without the credentials comment: %v", comments)
	}

	// The poll path: a stream with the default poll and no event.
	g := newTeamFixture(t)
	p := g.openStream(t, g.restricted)
	defer p.end()
	testdb.PutInTeams(t, g.e.pool, g.e.orgID, g.restrictedID, g.hidID)
	ev, _, ok = p.next()
	if !ok || ev.typ != "access" {
		t.Fatalf("poll path: %+v ok=%v, want access on the poll tick", ev, ok)
	}
}

// TestEventsStream_AccessEventOnlyOnChange: no access event when nothing
// changed, for an unrestricted member and an admin.
func TestEventsStream_AccessEventOnlyOnChange(t *testing.T) {
	f := newTeamFixture(t)
	for who, tok := range map[string]string{"member": f.member, "admin": f.e.chen, "restricted": f.restricted} {
		s := f.openStream(t, tok)
		time.Sleep(600 * time.Millisecond) // two poll ticks
		is := f.e.createIssue(f.e.chen, map[string]any{"team": testdb.TeamKey, "title": "plain " + who})
		ev, _, ok := s.next()
		s.end()
		if !ok || ev.typ != events.IssueCreated || !strings.Contains(ev.data, is.ID.String()) {
			t.Errorf("%s: first event %+v ok=%v, want the issue (no access event)", who, ev, ok)
		}
	}
}

// TestEvents_HighWaterMark (#55, review of 6a): a reader's cursor moves past
// the rows it may not read. GET /events gives the seq it read up to as
// nextCursor when the page is not full, and following nextCursor never
// skips a visible event. The stream moves its own cursor (no id is sent), so
// a later drain does not read the hidden rows again.
func TestEvents_HighWaterMark(t *testing.T) {
	var mu sync.Mutex
	var afters []int64
	f := newTeamFixtureWith(t, envOpts{configure: func(_ *env, s *Server) {
		s.eventPageHook = func(after int64) {
			mu.Lock()
			afters = append(afters, after)
			mu.Unlock()
		}
	}})
	e := f.e
	feed := func(after int64, limit int) page[tracker.Event] {
		t.Helper()
		var p page[tracker.Event]
		e.must(http.StatusOK, e.do("GET", f.orgPath("/events?after="+strconv.FormatInt(after, 10)+"&limit="+strconv.Itoa(limit)), f.restricted, nil)).json(t, &p)
		return p
	}
	start := f.eventSeq()

	// Many hidden events, then nothing the member may read.
	for i := range 30 {
		e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden " + strconv.Itoa(i)})
	}
	top := f.eventSeq()
	p := feed(start, 100)
	if len(p.Items) != 0 || p.NextCursor == nil || *p.NextCursor != strconv.FormatInt(top, 10) {
		t.Fatalf("after hidden rows only: %d items, nextCursor %v, want none and %d", len(p.Items), p.NextCursor, top)
	}
	if p = feed(top, 100); len(p.Items) != 0 || p.NextCursor != nil {
		t.Fatalf("at the high-water mark: %d items, nextCursor %v, want the end", len(p.Items), p.NextCursor)
	}

	// Interleaved: following nextCursor from start with limit 1 returns
	// exactly the visible events, in order.
	var want []int64
	for i := range 3 {
		e.createIssue(e.chen, map[string]any{"team": "HID", "title": "between " + strconv.Itoa(i)})
		e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "seen " + strconv.Itoa(i)})
		want = append(want, f.eventSeq())
	}
	e.createIssue(e.chen, map[string]any{"team": "HID", "title": "tail"})
	var got []int64
	cur := start
	for range 200 {
		p := feed(cur, 1)
		for _, ev := range p.Items {
			got = append(got, ev.Seq)
		}
		if p.NextCursor == nil {
			break
		}
		n, err := strconv.ParseInt(*p.NextCursor, 10, 64)
		if err != nil || n <= cur {
			t.Fatalf("nextCursor %v does not move past %d", p.NextCursor, cur)
		}
		cur = n
	}
	if !slices.Equal(got, want) {
		t.Fatalf("following nextCursor read %v, want the visible events %v", got, want)
	}
	if cur != f.eventSeq() {
		t.Fatalf("the cursor ended at %d, want the newest seq %d (past the hidden tail)", cur, f.eventSeq())
	}
	// An unrestricted member: a page that is not full has no nextCursor,
	// as before.
	var mp page[tracker.Event]
	e.must(http.StatusOK, e.do("GET", f.orgPath("/events?after="+strconv.FormatInt(start, 10)+"&limit=500"), f.member, nil)).json(t, &mp)
	if mp.NextCursor != nil || len(mp.Items) != int(f.eventSeq()-start) {
		t.Fatalf("member: %d items, nextCursor %v", len(mp.Items), mp.NextCursor)
	}

	// The stream: after hidden events, a drain starts past them.
	s := f.openStream(t, f.restricted)
	defer s.end()
	for i := range 20 {
		e.createIssue(e.chen, map[string]any{"team": "HID", "title": "stream hidden " + strconv.Itoa(i)})
	}
	hiddenTop := f.eventSeq()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := afters[len(afters)-1]
		mu.Unlock()
		if n >= hiddenTop {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream's cursor stays at %d, below the hidden rows' top %d: every drain reads them again", n, hiddenTop)
		}
		time.Sleep(50 * time.Millisecond)
	}
	v := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "after the hidden rows"})
	ev, _, ok := s.next()
	if !ok || ev.typ != events.IssueCreated || !strings.Contains(ev.data, v.ID.String()) {
		t.Fatalf("the visible event after the hidden rows: %+v ok=%v", ev, ok)
	}
}

// TestEventsStream_RemovalWithinOnePage (#56, review of 6a): the stream
// re-reads the access before every page of a drain. A member who loses HID
// in the middle of a long drain gets the access event and no HID event after
// it, not even one emitted after the removal.
func TestEventsStream_RemovalWithinOnePage(t *testing.T) {
	var armed atomic.Bool
	var calls atomic.Int32
	var remove atomic.Pointer[func()]
	f := newTeamFixtureWith(t, envOpts{ssePoll: time.Hour, configure: func(_ *env, s *Server) {
		s.eventStreamPage = 2
		s.eventPageHook = func(int64) {
			if armed.Load() && calls.Add(1) == 2 {
				(*remove.Load())()
			}
		}
	}})
	e := f.e
	testdb.PutInTeams(t, e.pool, e.orgID, f.restrictedID, f.hidID)
	from := f.eventSeq()
	for i := range 6 {
		e.createIssue(e.chen, map[string]any{"team": "HID", "title": "drain " + strconv.Itoa(i)})
	}
	rm := func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM team_members WHERE org_id = $1 AND user_id = $2 AND team_id = $3`,
			e.orgID, f.restrictedID, f.hidID)
	}
	remove.Store(&rm)
	armed.Store(true)
	// The first drain reads the six HID events in pages of two; the hook
	// removes HID before the second page.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+f.orgPath("/events/stream"), nil)
	e.authorize(req, f.restricted)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(from, 10))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	s := &sseReader{t: t, br: bufio.NewReader(r.Body), end: cancel}
	var seen []string
	for {
		ev, _, ok := s.next()
		if !ok {
			t.Fatalf("stream ended; seen %v", seen)
		}
		seen = append(seen, ev.typ)
		if ev.typ == "access" {
			break
		}
		if len(seen) > 2 {
			t.Fatalf("more than one page of HID events before the access event: %v", seen)
		}
	}
	if !slices.Equal(seen, []string{events.IssueCreated, events.IssueCreated, "access"}) {
		t.Fatalf("want the first page (two HID events), then access; got %v", seen)
	}
	after := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "emitted after the removal"})
	marker := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "marker"})
	for {
		ev, _, ok := s.next()
		if !ok {
			t.Fatal("stream ended before the marker")
		}
		if strings.Contains(ev.data, after.ID.String()) || strings.Contains(ev.data, f.hidID.String()) {
			t.Fatalf("a HID event after the removal: %+v", ev)
		}
		if strings.Contains(ev.data, marker.ID.String()) {
			break
		}
	}
}
