package api

// Tests for the first review findings: idempotency and secrets, stream
// re-authorisation, comment moderation scope, reply cascades, relation races
// and team-scoped state references.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/idempotency"
	"devops/internal/tracker"
)

// openStream opens GET /orgs/{org}/events/stream. The returned func closes it.
func openStream(t *testing.T, e *env, org, token, lastEventID string, timeout time.Duration) (*bufio.Reader, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/v1/orgs/"+org+"/events/stream", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	e.authorize(req, token) // bearer, or the session cookies
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		cancel()
		t.Fatalf("stream status %d", res.StatusCode)
	}
	br := bufio.NewReader(res.Body)
	// Consume the preamble (retry + connected comment) so the server has
	// subscribed and read its starting seq before the caller acts.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			cancel()
			t.Fatalf("stream preamble: %v", err)
		}
		if strings.HasPrefix(line, ": connected") {
			if _, err := br.ReadString('\n'); err != nil { // the blank line
				cancel()
				t.Fatal(err)
			}
			break
		}
	}
	return br, func() { cancel(); _ = res.Body.Close() }
}

// nextEvent reads one complete event; err is the read error (timeout, EOF).
func nextEvent(br *bufio.Reader) (id, typ, data string, err error) {
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			return "", "", "", rerr
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && id != "":
			return id, typ, data, nil
		}
	}
}

// --- 1. idempotency never stores or leaks a token secret ---------------------

func TestIdempotency_TokenSecretsNeverStored(t *testing.T) {
	e := newEnv(t)
	// Fill the table with ordinary idempotent requests.
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, map[string]any{"team": "ORB", "title": "a"}, "Idempotency-Key", "i-1"))
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/labels", e.chen, map[string]any{"name": "x"}, "Idempotency-Key", "i-2"))

	// POST /tokens refuses the header, and works without it.
	r := e.must(400, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "t", "scopes": []string{"write"}}, "Idempotency-Key", "tok-1"))
	if r.errCode(t) != "bad_request" {
		t.Fatalf("code %s", r.errCode(t))
	}
	var made tracker.CreatedToken
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "t", "scopes": []string{"write"}})).json(t, &made)

	var rows, withSecret, tokenPaths int
	err := e.pool.QueryRow(context.Background(), `
		SELECT count(*),
		       count(*) FILTER (WHERE position('odt_' IN convert_from(coalesce(response_body, ''), 'UTF8')) > 0),
		       count(*) FILTER (WHERE path LIKE '%/tokens%')
		FROM idempotency_keys`).Scan(&rows, &withSecret, &tokenPaths)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 || withSecret != 0 || tokenPaths != 0 {
		t.Fatalf("idempotency_keys: %d rows, %d hold odt_, %d for /tokens", rows, withSecret, tokenPaths)
	}
}

// A read-scoped token of the SAME user must not get a write route's stored
// response by replaying its key: the scope check runs first.
func TestIdempotency_ReplayNeedsTheRouteScope(t *testing.T) {
	e := newEnv(t)
	chenRead, _ := e.newToken(e.user("chen").ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	body := map[string]any{"team": "ORB", "title": "secret plans"}
	first := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-r"))
	r := e.must(403, e.do("POST", "/api/v1/orgs/orbit/issues", chenRead, body, "Idempotency-Key", "k-r"))
	if r.errCode(t) != "insufficient_scope" || strings.Contains(string(r.body), "secret plans") || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("read token got %s %v", r.body, r.header)
	}
	// The writer still gets the replay.
	again := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-r"))
	if string(again.body) != string(first.body) {
		t.Fatal("replay changed")
	}
}

// --- 3a. an abandoned, uncommitted claim is taken over after the window ------

func TestIdempotency_AbandonedClaimIsTakenOver(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"team": "ORB", "title": "retry me"}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-a"))
	setClaim := func(age string) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), `UPDATE idempotency_keys
			SET status_code = NULL, response_body = NULL, committed_at = NULL, created_at = now() - $1::interval
			WHERE key = 'k-a'`, age); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh in-flight claim: 409, nothing runs.
	setClaim("1 second")
	if code := e.must(409, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-a")).errCode(t); code != "idempotency_in_progress" {
		t.Fatalf("code %s", code)
	}
	// Older than the 60s default: abandoned, so the retry runs.
	setClaim("2 minutes")
	r := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-a"))
	if r.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("abandoned claim was replayed, not re-run")
	}
	var is tracker.Issue
	r.json(t, &is)
	if is.Number != 2 {
		t.Fatalf("re-run made %s", is.Identifier)
	}
	// And it is stored again: the next retry replays.
	if e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-a")).header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("re-run response not stored")
	}
}

// failingComplete makes CompleteIdempotencyKey fail, as a DB blip right after
// the handler committed would.
type failingComplete struct{ idempotency.Store }

func (failingComplete) CompleteIdempotencyKey(context.Context, dbq.CompleteIdempotencyKeyParams) error {
	return errors.New("injected: complete failed")
}

// --- 3b. a committed request whose response was not stored never re-runs ---

func TestIdempotency_CompleteFailureNeverReRuns(t *testing.T) {
	e := newEnvWith(t, envOpts{idemStore: func(s idempotency.Store) idempotency.Store { return failingComplete{s} }})
	body := map[string]any{"team": "ORB", "title": "exactly once"}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-c"))

	var status *int32
	var committed *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT status_code, committed_at FROM idempotency_keys WHERE key = 'k-c'`).Scan(&status, &committed); err != nil {
		t.Fatalf("claim was released: %v", err)
	}
	if status != nil || committed == nil {
		t.Fatalf("claim: status=%v committed=%v", status, committed)
	}
	for _, age := range []string{"0 seconds", "10 minutes"} {
		if _, err := e.pool.Exec(context.Background(), `UPDATE idempotency_keys SET created_at = now() - $1::interval WHERE key = 'k-c'`, age); err != nil {
			t.Fatal(err)
		}
		r := e.must(409, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-c"))
		if code := r.errCode(t); code != "idempotency_outcome_unknown" {
			t.Fatalf("age %s: code %s", age, code)
		}
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM issues`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("issues = %d (want exactly 1) err %v", n, err)
	}
}

// --- 2. a stream ends when its credentials go away ---------------------------

func TestEventStream_EndsWhenCredentialsGoAway(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		revoke func(user dbq.User, tokenID uuid.UUID)
	}{
		{"token revoked through the API", func(_ dbq.User, id uuid.UUID) {
			e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/tokens/"+id.String(), e.chen, nil))
		}},
		{"token expired", func(_ dbq.User, id uuid.UUID) {
			mustExec(t, e, `UPDATE api_tokens SET expires_at = now() WHERE id = $1`, id)
		}},
		{"user deactivated", func(u dbq.User, _ uuid.UUID) {
			mustExec(t, e, `UPDATE users SET deactivated_at = now() WHERE id = $1`, u.ID)
		}},
		{"membership removed", func(u dbq.User, _ uuid.UUID) {
			mustExec(t, e, `DELETE FROM org_members WHERE user_id = $1`, u.ID)
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := e.newUser(fmt.Sprintf("viewer%d", i))
			e.addMember(e.orgID, u.ID, "member")
			tok, id := e.newToken(u.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
			br, closeFn := openStream(t, e, "orbit", tok, "", 10*time.Second)
			defer closeFn()
			start := time.Now()
			tt.revoke(u, id)
			// The server closes the stream on its next poll tick (250ms here).
			_, err := io.Copy(io.Discard, br)
			if err != nil || time.Since(start) > 5*time.Second {
				t.Fatalf("stream did not end cleanly: err=%v after %v", err, time.Since(start))
			}
		})
	}
}

func mustExec(t *testing.T, e *env, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// --- 6. LISTEN/NOTIFY alone delivers the event --------------------------------

// The poll is set to ten minutes, so within the 5-second budget only the
// NOTIFY wake-up can deliver the event. With the listener off it must fail.
func TestEventStream_NotifyAloneDelivers(t *testing.T) {
	e := newEnvWith(t, envOpts{ssePoll: 10 * time.Minute})
	br, closeFn := openStream(t, e, "orbit", e.claude, "", 5*time.Second)
	defer closeFn()
	is := e.createIssue(e.chen, map[string]any{"title": "pushed, not polled"})
	_, typ, data, err := nextEvent(br)
	if err != nil {
		t.Fatalf("no event within 5s with polling off (is LISTEN/NOTIFY broken?): %v", err)
	}
	if typ != "issue.created" || !strings.Contains(data, is.ID.String()) {
		t.Fatalf("got %s %s", typ, data)
	}
}

// --- 4 and 9. comment moderation scope and reply cascade ---------------------

func TestComments_ModerationScopeAndCascade(t *testing.T) {
	e := newEnv(t)
	chenWrite, _ := e.newToken(e.user("chen").ID, tokenOpts{pin: &e.orgID, scopes: []string{"write"}})
	e.createIssue(e.chen, map[string]any{"title": "thread"})
	var c1, c2, c3 tracker.Comment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.claude, map[string]any{"body": "root"})).json(t, &c1)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.claude, map[string]any{"body": "reply", "parentId": c1.ID})).json(t, &c2)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.chen, map[string]any{"body": "nested", "parentId": c2.ID})).json(t, &c3)

	// chen is an org admin, but a write-scoped token cannot moderate.
	if code := e.must(403, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+c1.ID.String(), chenWrite, nil)).errCode(t); code != "forbidden" {
		t.Fatalf("code %s", code)
	}
	// His own comment needs only write.
	var mine tracker.Comment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", chenWrite, map[string]any{"body": "mine"})).json(t, &mine)
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+mine.ID.String(), chenWrite, nil))

	var before tracker.Event
	var evs page[tracker.Event]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/events?after=0", e.chen, nil)).json(t, &evs)
	before = evs.Items[len(evs.Items)-1]

	// Admin scope deletes the root; both replies cascade, each with an event.
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+c1.ID.String(), e.chen, nil))
	e.must(200, e.do("GET", fmt.Sprintf("/api/v1/orgs/orbit/events?after=%d", before.Seq), e.chen, nil)).json(t, &evs)
	got := map[uuid.UUID]string{}
	for _, ev := range evs.Items {
		if ev.Type == "comment.deleted" {
			got[ev.EntityID] = string(ev.Payload)
		}
	}
	if len(got) != 3 || got[c1.ID] == "" || got[c2.ID] == "" || got[c3.ID] == "" {
		t.Fatalf("comment.deleted events: %v", got)
	}
	if !strings.Contains(got[c1.ID], c2.ID.String()) || !strings.Contains(got[c1.ID], c3.ID.String()) || !strings.Contains(got[c3.ID], `"cascadedFrom"`) {
		t.Fatalf("payloads: %v", got)
	}
	var cs page[tracker.Comment]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.chen, nil)).json(t, &cs)
	if len(cs.Items) != 0 {
		t.Fatalf("%d comments survived", len(cs.Items))
	}
}

// --- 8. relation creates race in both directions -----------------------------

func TestRelations_ConcurrentPairCreates(t *testing.T) {
	e := newEnv(t)
	e.createIssue(e.chen, map[string]any{"title": "A"})
	e.createIssue(e.chen, map[string]any{"title": "B"})
	for _, typ := range []string{"related", "blocks", "duplicateOf"} {
		t.Run(typ, func(t *testing.T) {
			if _, err := e.pool.Exec(context.Background(), `DELETE FROM issue_relations`); err != nil {
				t.Fatal(err)
			}
			const n = 16
			statuses := make(chan int, n)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range n {
				from, to := "ORB-1", "ORB-2"
				if i%2 == 1 {
					from, to = to, from // half the racers try the reverse direction
				}
				wg.Go(func() {
					<-start
					r := e.doNoFatal("POST", "/api/v1/orgs/orbit/issues/"+from+"/relations", e.chen, map[string]any{"type": typ, "issue": to})
					statuses <- r.status
				})
			}
			close(start)
			wg.Wait()
			close(statuses)
			counts := map[int]int{}
			for st := range statuses {
				counts[st]++
			}
			if counts[201] != 1 || counts[409] != n-1 {
				t.Fatalf("statuses %v, want one 201 and %d 409", counts, n-1)
			}
			var rows int
			if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_relations`).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("rows %d err %v", rows, err)
			}
		})
	}
}

// --- 11. state references are team-scoped in the database --------------------

func TestStates_AreTeamScoped(t *testing.T) {
	e := newEnv(t)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	var webStates page[tracker.WorkflowState]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/WEB/states", e.chen, nil)).json(t, &webStates)
	webTodo := webStates.Items[1].ID
	is := e.createIssue(e.chen, map[string]any{"title": "orb issue"})

	for name, sql := range map[string]string{
		"team default state": `UPDATE teams SET default_state_id = $1 WHERE key = 'ORB'`,
		"team review state":  `UPDATE teams SET review_state_id = $1 WHERE key = 'ORB'`,
		"issue state":        `UPDATE issues SET state_id = $1 WHERE id = '` + is.ID.String() + `'`,
	} {
		if _, err := e.pool.Exec(context.Background(), sql, webTodo); err == nil || !strings.Contains(err.Error(), "23503") {
			t.Fatalf("%s: another team's state was accepted (err %v)", name, err)
		}
	}
	// Through the API: a state id of another team is a 400.
	e.must(400, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-1", e.chen, map[string]any{"state": webTodo.String()}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "state": webTodo.String()}))
}

// --- round 2 -----------------------------------------------------------------

// deletedAfter returns the ids of comment.deleted events after seq.
func deletedAfter(t *testing.T, e *env, seq int64) map[uuid.UUID]bool {
	t.Helper()
	var evs page[tracker.Event]
	e.must(200, e.do("GET", fmt.Sprintf("/api/v1/orgs/orbit/events?after=%d&limit=500", seq), e.chen, nil)).json(t, &evs)
	out := map[uuid.UUID]bool{}
	for _, ev := range evs.Items {
		if ev.Type == "comment.deleted" {
			out[ev.EntityID] = true
		}
	}
	return out
}

func lastSeq(t *testing.T, e *env) int64 {
	t.Helper()
	var seq int64
	if err := e.pool.QueryRow(context.Background(), `SELECT COALESCE(MAX(seq), 0) FROM events WHERE org_id = $1`, e.orgID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func postComment(t *testing.T, e *env, token string, parent *uuid.UUID, body string) tracker.Comment {
	t.Helper()
	in := map[string]any{"body": body}
	if parent != nil {
		in["parentId"] = *parent
	}
	var c tracker.Comment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", token, in)).json(t, &c)
	return c
}

func commentExists(t *testing.T, e *env, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM comments WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// Blocker round 2: deleting your own root must not cascade away someone
// else's reply. claude's fixture token is read+write, not a moderator.
func TestComments_DeleteNeedsTheWholeSubtree(t *testing.T) {
	e := newEnv(t)
	e.createIssue(e.chen, map[string]any{"title": "thread"})

	t.Run("own root with another user's reply is refused", func(t *testing.T) {
		x := postComment(t, e, e.claude, nil, "claude root")
		y := postComment(t, e, e.chen, &x.ID, "chen reply")
		seq := lastSeq(t, e)
		if code := e.must(403, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+x.ID.String(), e.claude, nil)).errCode(t); code != "forbidden" {
			t.Fatalf("code %s", code)
		}
		if !commentExists(t, e, x.ID) || !commentExists(t, e, y.ID) {
			t.Fatal("a refused delete removed comments")
		}
		if got := deletedAfter(t, e, seq); len(got) != 0 {
			t.Fatalf("refused delete emitted events: %v", got)
		}
		// A moderator (org admin + admin scope) may delete the whole thread.
		seq = lastSeq(t, e)
		e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+x.ID.String(), e.chen, nil))
		if got := deletedAfter(t, e, seq); len(got) != 2 || !got[x.ID] || !got[y.ID] {
			t.Fatalf("moderator delete events: %v", got)
		}
	})
	t.Run("own root with only own replies is allowed", func(t *testing.T) {
		r := postComment(t, e, e.claude, nil, "root")
		r1 := postComment(t, e, e.claude, &r.ID, "reply")
		r2 := postComment(t, e, e.claude, &r1.ID, "nested reply")
		seq := lastSeq(t, e)
		e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+r.ID.String(), e.claude, nil))
		got := deletedAfter(t, e, seq)
		if len(got) != 3 || !got[r.ID] || !got[r1.ID] || !got[r2.ID] {
			t.Fatalf("events %v", got)
		}
	})
	t.Run("a deep foreign reply also blocks", func(t *testing.T) {
		r := postComment(t, e, e.claude, nil, "root")
		r1 := postComment(t, e, e.claude, &r.ID, "own reply")
		postComment(t, e, e.chen, &r1.ID, "foreign, two levels down")
		e.must(403, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+r.ID.String(), e.claude, nil))
	})
}

// A reply racing a delete of its tree: never a 500, and never a reply that
// is cascaded away without its own comment.deleted event.
func TestComments_ReplyDeleteRace(t *testing.T) {
	e := newEnv(t)
	e.createIssue(e.chen, map[string]any{"title": "race"})
	outcomes := map[int]int{}
	for i := range 25 {
		root := postComment(t, e, e.chen, nil, fmt.Sprintf("root %d", i))
		child := postComment(t, e, e.chen, &root.ID, "child")
		seq := lastSeq(t, e)
		var del, rep resp
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			del = e.doNoFatal("DELETE", "/api/v1/orgs/orbit/comments/"+root.ID.String(), e.chen, nil)
		})
		wg.Go(func() {
			<-start
			rep = e.doNoFatal("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.chen, map[string]any{"body": "late", "parentId": child.ID})
		})
		close(start)
		wg.Wait()
		outcomes[rep.status]++
		if del.status != 204 {
			t.Fatalf("iteration %d: delete %d %s", i, del.status, del.body)
		}
		if rep.status != 201 && rep.status != 400 {
			t.Fatalf("iteration %d: reply %d %s", i, rep.status, rep.body)
		}
		got := deletedAfter(t, e, seq)
		if rep.status == 201 {
			var c tracker.Comment
			rep.json(t, &c)
			if commentExists(t, e, c.ID) || !got[c.ID] {
				t.Fatalf("iteration %d: reply %s cascaded without an event (ghost)", i, c.ID)
			}
		}
		if !got[root.ID] || !got[child.ID] {
			t.Fatalf("iteration %d: events %v", i, got)
		}
	}
	t.Logf("reply outcomes over 25 races: %v", outcomes)
}

// SHOULD round 2: a slow first request whose claim was taken over must roll
// back at commit, so the create happens exactly once.
func TestIdempotency_TakenOverClaimRollsBack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chen := e.user("chen")
	org := &auth.Org{ID: e.orgID, Slug: "orbit", Name: "Orbit", Role: "admin", Access: auth.TeamAccess{All: true}}
	p := &auth.Principal{UserID: chen.ID, Username: "chen", Name: "Chen", Scopes: []auth.Scope{auth.ScopeAdmin}, PinnedOrgID: &e.orgID}
	svc := tracker.New(e.pool)
	claim := func() uuid.UUID {
		t.Helper()
		id, err := e.q.ClaimIdempotencyKey(ctx, dbq.ClaimIdempotencyKeyParams{
			UserID: chen.ID, Key: "k-slow", Method: "POST", Path: "/api/v1/orgs/orbit/issues",
			RequestHash: make([]byte, 32), ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := claim() // the slow first request
	mustExec(t, e, `UPDATE idempotency_keys SET created_at = now() - interval '2 minutes' WHERE id = $1`, old)
	n, err := e.q.TakeOverAbandonedIdempotencyKey(ctx, dbq.TakeOverAbandonedIdempotencyKeyParams{ID: old, StaleSeconds: 60})
	if err != nil || n != 1 {
		t.Fatalf("takeover n=%d err=%v", n, err)
	}
	retry := claim() // the retry that took over
	in := tracker.CreateIssueInput{Team: "ORB", Title: "once"}
	if _, err := svc.CreateIssue(database.WithIdempotencyClaim(ctx, retry), org, p, in); err != nil {
		t.Fatalf("retry create: %v", err)
	}
	// The slow first request now reaches its commit.
	_, err = svc.CreateIssue(database.WithIdempotencyClaim(ctx, old), org, p, in)
	if !errors.Is(err, database.ErrIdempotencyClaimLost) {
		t.Fatalf("stale claim committed: err=%v", err)
	}
	var issues int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM issues`).Scan(&issues); err != nil || issues != 1 {
		t.Fatalf("issues = %d, want 1 (err %v)", issues, err)
	}
}

// The stale window is configurable, and measured on the database clock.
func TestIdempotency_StaleWindowIsConfigurable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stale  time.Duration
		status int
	}{
		{"default 60s: a 3s-old claim is still in flight", 0, 409},
		{"1s window: a 3s-old claim is abandoned", time.Second, 201},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnvWith(t, envOpts{idemOpts: idempotency.Options{StaleAfter: tt.stale}})
			body := map[string]any{"team": "ORB", "title": "w"}
			e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-w"))
			mustExec(t, e, `UPDATE idempotency_keys SET status_code = NULL, response_body = NULL, committed_at = NULL,
				created_at = now() - interval '3 seconds' WHERE key = 'k-w'`)
			r := e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-w")
			if r.status != tt.status {
				t.Fatalf("status %d, want %d: %s", r.status, tt.status, r.body)
			}
		})
	}
}

// NIT round 2: an admin may mint tokens for themselves or an agent only.
func TestTokens_NoImpersonation(t *testing.T) {
	e := newEnv(t)
	dana := e.newUser("dana")
	e.addMember(e.orgID, dana.ID, "member")
	r := e.must(403, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "x", "scopes": []string{"read"}, "user": "dana"}))
	if r.errCode(t) != "forbidden" || strings.Contains(string(r.body), "odt_") {
		t.Fatalf("human impersonation: %s", r.body)
	}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "bot", "scopes": []string{"read"}, "user": "claude", "expiresInDays": 30}))
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "self", "scopes": []string{"read"}, "user": "chen"}))
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_tokens WHERE user_id = $1`, dana.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("dana has %d tokens", n)
	}
}

// NIT round 2: concurrent A.parent=B and B.parent=A must not both succeed.
func TestIssues_ConcurrentReparentNoCycle(t *testing.T) {
	e := newEnv(t)
	a := e.createIssue(e.chen, map[string]any{"title": "A"})
	b := e.createIssue(e.chen, map[string]any{"title": "B"})
	both := 0
	for i := range 30 {
		mustExec(t, e, `UPDATE issues SET parent_id = NULL`)
		var ra, rb resp
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			ra = e.doNoFatal("PATCH", "/api/v1/orgs/orbit/issues/ORB-1", e.chen, map[string]any{"parent": "ORB-2"})
		})
		wg.Go(func() {
			<-start
			rb = e.doNoFatal("PATCH", "/api/v1/orgs/orbit/issues/ORB-2", e.chen, map[string]any{"parent": "ORB-1"})
		})
		close(start)
		wg.Wait()
		if ra.status == 200 && rb.status == 200 {
			both++
		}
		if (ra.status != 200 && ra.status != 400) || (rb.status != 200 && rb.status != 400) {
			t.Fatalf("iteration %d: statuses %d %d", i, ra.status, rb.status)
		}
		var cycles int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM issues x JOIN issues y ON x.parent_id = y.id AND y.parent_id = x.id`).Scan(&cycles); err != nil {
			t.Fatal(err)
		}
		if cycles != 0 {
			t.Fatalf("iteration %d: a parent cycle was committed (%s <-> %s)", i, a.Identifier, b.Identifier)
		}
	}
	if both != 0 {
		t.Fatalf("both PATCHes succeeded %d times", both)
	}
}
