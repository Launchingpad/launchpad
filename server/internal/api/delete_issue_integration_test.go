package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/tracker"
)

// LAUN-201 (docs/internal/decisions-T32-delete-issues.md): DELETE /issues/{issue}.

// tablesWithIssueRows counts every table that has an issue_id or related_issue_id column.
func (e *env) tablesWithIssueRows(id uuid.UUID) map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
SELECT table_name, column_name FROM information_schema.columns
WHERE table_schema = 'public' AND column_name IN ('issue_id', 'related_issue_id') AND table_name <> 'issues'`)
	if err != nil {
		e.t.Fatal(err)
	}
	type tc struct{ t, c string }
	var cols []tc
	for rows.Next() {
		var x tc
		if err := rows.Scan(&x.t, &x.c); err != nil {
			e.t.Fatal(err)
		}
		cols = append(cols, x)
	}
	rows.Close()
	out := map[string]int{}
	for _, x := range cols {
		out[x.t+"."+x.c] = e.countRows(`SELECT count(*) FROM `+x.t+` WHERE `+x.c+` = $1`, id)
	}
	return out
}

func TestDeleteIssue(t *testing.T) {
	store := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: store, publicBase: "https://tracker.example.com"})
	const base = O + "/issues/"
	parent := e.createIssue(e.chen, map[string]any{"title": "parent secret title", "description": "secret body", "labels": []string{"Bug"}})
	child := e.createIssue(e.chen, map[string]any{"title": "child", "parent": parent.Identifier})
	other := e.createIssue(e.chen, map[string]any{"title": "other"})
	// Something on every side of the parent.
	e.must(201, e.do("POST", base+parent.Identifier+"/comments", e.chen, map[string]any{"body": "a comment"}))
	e.must(201, e.do("POST", base+parent.Identifier+"/relations", e.chen, map[string]any{"type": "blocks", "issue": other.Identifier}))
	e.must(201, e.do("POST", base+other.Identifier+"/relations", e.chen, map[string]any{"type": "related", "issue": parent.Identifier}))
	e.must(201, e.do("POST", base+parent.Identifier+"/attachments", e.chen, map[string]any{"kind": "link", "url": "https://example.com/x"}))
	up := e.prepare(e.chen, "shot.png", "image/png", len(pngBytes))
	if code := putUpload(t, up, pngBytes); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	e.must(201, e.do("POST", base+parent.Identifier+"/attachments", e.chen, map[string]any{"kind": "file", "uploadKey": up.UploadKey, "filename": "shot.png"}))
	e.must(200, e.do("PATCH", base+parent.Identifier, e.chen, map[string]any{"title": "renamed", "state": "In Progress"}))
	e.must(200, e.do("PUT", base+parent.Identifier+"/subscription", e.chen, map[string]any{"subscribed": true}))
	e.must(200, e.do("PUT", base+child.Identifier+"/subscription", e.chen, map[string]any{"subscribed": true}))
	if _, err := store.Head(context.Background(), up.UploadKey); err != nil {
		t.Fatalf("object missing before delete: %v", err)
	}
	// A row each in the git and notification tables, which no API call in this test makes.
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO notifications (org_id, recipient_id, issue_id, kind, dedupe_key, inbox)
		VALUES ($1, $2, $3, 'assigned', 'test-dedupe', true)`, e.orgID, e.user("chen").ID, parent.ID); err != nil {
		t.Fatal(err)
	}

	before := e.tablesWithIssueRows(parent.ID)
	for _, tbl := range []string{"comments.issue_id", "attachments.issue_id", "issue_relations.issue_id", "issue_relations.related_issue_id",
		"issue_activity.issue_id", "issue_labels.issue_id", "issue_state_changes.issue_id", "issue_subscriptions.issue_id", "notifications.issue_id"} {
		if before[tbl] == 0 {
			t.Fatalf("test setup left %s empty (%v)", tbl, before)
		}
	}

	// Scope: write (the agent) and read cannot delete; nothing changes.
	expectCode(t, e.do("DELETE", base+parent.Identifier, e.claude, nil), 403, "insufficient_scope")
	readTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	expectCode(t, e.do("DELETE", base+parent.Identifier, readTok, nil), 403, "insufficient_scope")
	writeTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"write"}})
	expectCode(t, e.do("DELETE", base+parent.Identifier, writeTok, nil), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", base+"ORB-9999", e.chen, nil), 404, "not_found")
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1`, parent.ID); n != 1 || e.eventCount("issue.deleted") != 0 {
		t.Fatal("a refused delete changed something")
	}

	e.must(204, e.do("DELETE", base+parent.Identifier, e.chen, nil))
	expectCode(t, e.do("GET", base+parent.Identifier, e.chen, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", base+parent.Identifier, e.chen, nil), 404, "not_found") // a repeat

	// Everything that pointed at it is gone, in every table that has an issue column.
	for tbl, n := range e.tablesWithIssueRows(parent.ID) {
		if n != 0 {
			t.Errorf("%s still has %d row(s) for the deleted issue", tbl, n)
		}
	}
	// The child stays, without a parent; the other issue stays.
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1 AND parent_id IS NULL`, child.ID); n != 1 {
		t.Fatal("sub-issue was deleted or kept its parent")
	}
	var got tracker.Issue
	e.must(200, e.do("GET", base+child.Identifier, e.chen, nil)).json(t, &got)
	if got.Parent != nil {
		t.Fatalf("child parent = %+v", got.Parent)
	}
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1`, other.ID); n != 1 {
		t.Fatal("another issue was deleted")
	}
	if n := e.countRows(`SELECT count(*) FROM issue_subscriptions WHERE issue_id = $1`, child.ID); n != 1 {
		t.Fatal("the child's subscription went")
	}
	// The attachment object is removed after the commit.
	if _, err := store.Head(context.Background(), up.UploadKey); err != storage.ErrNotFound {
		t.Fatalf("object after delete: err = %v, want ErrNotFound", err)
	}

	// One event, ids and the identifier only, and the LAST of the transaction.
	if e.eventCount("issue.deleted") != 1 {
		t.Fatal("want one issue.deleted")
	}
	if n := e.countRows(`SELECT count(*) FROM events WHERE type = 'issue.deleted' AND entity_type = 'issue' AND entity_id = $1
		AND payload->>'identifier' = $2 AND NOT payload ? 'detachedChildren' AND NOT payload ? 'parentId'`, parent.ID, parent.Identifier); n != 1 {
		t.Fatal("event payload lacks the identifier, or holds the child count or the parent (T20 #42, LAUN-221)")
	}
	var payload string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload::text FROM events WHERE type = 'issue.deleted'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret", "renamed", "title", "description"} {
		if strings.Contains(payload, leak) {
			t.Fatalf("payload leaks %q: %s", leak, payload)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM events WHERE seq > (SELECT seq FROM events WHERE type = 'issue.deleted')`); n != 0 {
		t.Fatal("issue.deleted is not the last event")
	}

	// The number is not reused.
	next := e.createIssue(e.chen, map[string]any{"title": "after"})
	if next.Number <= other.Number {
		t.Fatalf("number %d reused after delete (other = %d)", next.Number, other.Number)
	}
}

// Every foreign key onto issues must cascade or set null, or a future table
// would make the delete answer 500.
func TestDeleteIssue_EveryForeignKeyCanGo(t *testing.T) {
	e := newEnv(t)
	rows, err := e.pool.Query(context.Background(), `
SELECT conrelid::regclass::text, conname, confdeltype FROM pg_constraint
WHERE contype = 'f' AND confrelid = 'public.issues'::regclass AND confdeltype NOT IN ('c', 'n')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, name, rule string
		if err := rows.Scan(&tbl, &name, &rule); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s.%s has ON DELETE rule %q: an issue delete would fail", tbl, name, rule)
	}
}

func TestDeleteIssue_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	var ai tracker.Issue
	e.must(201, e.do("POST", A+"/issues", tok, map[string]any{"team": "ACM", "title": "acme issue"})).json(t, &ai)
	oi := e.createIssue(tok, map[string]any{"title": "orbit issue"})

	expectCode(t, e.do("DELETE", O+"/issues/"+ai.ID.String(), tok, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", O+"/issues/"+ai.Identifier, tok, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", A+"/issues/"+oi.ID.String(), tok, nil), 404, "not_found")
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id IN ($1, $2)`, ai.ID, oi.ID); n != 2 {
		t.Fatal("a cross-org delete removed an issue")
	}
	// Each org deletes its own.
	e.must(204, e.do("DELETE", A+"/issues/"+ai.ID.String(), tok, nil))
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1`, oi.ID); n != 1 {
		t.Fatal("the other org's issue went")
	}
}

// A comment, a label or a re-parent that loses a race with the delete answers
// 400 validation_failed, never 500 (database.goneFKs).
func TestDeleteIssue_LostRaceIsClean(t *testing.T) {
	e := newEnv(t)
	del := `DELETE FROM issues WHERE org_id = $1 AND id = $2`
	a := e.createIssue(e.chen, map[string]any{"title": "a"})
	r := e.lostRace(del, []any{e.orgID, a.ID}, "POST", O+"/issues/"+a.ID.String()+"/comments", map[string]any{"body": "late"})
	if r.status != 400 || r.errCode(t) != "validation_failed" {
		t.Fatalf("comment on a deleted issue: %d %s", r.status, r.body)
	}
	b := e.createIssue(e.chen, map[string]any{"title": "b"})
	c := e.createIssue(e.chen, map[string]any{"title": "c"})
	r = e.lostRace(del, []any{e.orgID, b.ID}, "PATCH", O+"/issues/"+c.ID.String(), map[string]any{"parent": b.Identifier})
	if r.status >= 500 {
		t.Fatalf("re-parent under a deleted issue: %d %s", r.status, r.body)
	}
}

func actOf(t *testing.T, e *env, ident string) []tracker.Activity {
	t.Helper()
	return e.activity(O + "/issues/" + ident + "/activity").Items
}

// The deleted issue's title must not stay in other issues' history (parent
// change and removed relation, both sides), but the lines still read: id and
// identifier stay. Mutant: remove RedactIssueSnapshots from DeleteIssue.
func TestDeleteIssue_RedactsTitleInOtherHistories(t *testing.T) {
	e := newEnv(t)
	const base = O + "/issues/"
	p := e.createIssue(e.chen, map[string]any{"title": "SECRETPARENT"})
	c := e.createIssue(e.chen, map[string]any{"title": "child"})
	x := e.createIssue(e.chen, map[string]any{"title": "other"})
	e.must(200, e.do("PATCH", base+c.Identifier, e.chen, map[string]any{"parent": p.Identifier})) // to = P
	e.must(200, e.do("PATCH", base+c.Identifier, e.chen, map[string]any{"parent": x.Identifier})) // from = P
	var rel tracker.Relation
	e.must(201, e.do("POST", base+x.Identifier+"/relations", e.chen, map[string]any{"type": "blocks", "issue": p.Identifier})).json(t, &rel)
	e.must(204, e.do("DELETE", base+x.Identifier+"/relations/"+rel.ID.String(), e.chen, nil)) // relationRemoved on x and on p
	e.must(201, e.do("POST", base+c.Identifier+"/relations", e.chen, map[string]any{"type": "related", "issue": p.Identifier}))
	// A second relation removed from the other side's issue id, so P appears as the "other" in x's and c's history.
	e.must(200, e.do("PATCH", base+c.Identifier, e.chen, map[string]any{"parent": nil}))

	has := func(items []tracker.Activity) (withTitle, withID int) {
		for _, a := range items {
			for _, raw := range []json.RawMessage{a.From, a.To} {
				s := string(raw)
				if strings.Contains(s, p.ID.String()) {
					withID++
				}
				if strings.Contains(s, "SECRETPARENT") {
					withTitle++
				}
			}
		}
		return
	}
	for _, id := range []string{c.Identifier, x.Identifier} {
		if wt, wi := has(actOf(t, e, id)); wt == 0 || wi == 0 {
			t.Fatalf("setup: %s history has no snapshot of P (title %d, id %d)", id, wt, wi)
		}
	}

	e.must(204, e.do("DELETE", base+p.Identifier, e.chen, nil))

	for _, id := range []string{c.Identifier, x.Identifier} {
		items := actOf(t, e, id)
		wt, wi := has(items)
		if wt != 0 {
			t.Fatalf("%s history still holds the deleted title", id)
		}
		if wi == 0 {
			t.Fatalf("%s history lost the id of the deleted issue", id)
		}
		for _, a := range items {
			if strings.Contains(string(a.From)+string(a.To), p.ID.String()) && !strings.Contains(string(a.From)+string(a.To), p.Identifier) {
				t.Fatalf("snapshot lost its identifier: %s %s", a.From, a.To)
			}
		}
	}
	// The event rows do not hold it either.
	if n := e.countRows(`SELECT count(*) FROM events WHERE payload::text LIKE '%SECRETPARENT%' AND type = 'issue.deleted'`); n != 0 {
		t.Fatal("event holds the title")
	}
}

// events.Emit must be the last write of DeleteIssue's transaction. A probe in
// the database turns any later write into an error: a trigger on events marks
// the transaction, and triggers on the tables DeleteIssue writes raise once the
// mark is set. Mutant: move Emit above RedactIssueSnapshots or DeleteIssue and
// the delete answers 500.
func TestDeleteIssue_EmitIsTheLastWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const base = O + "/issues/"
	p := e.createIssue(e.chen, map[string]any{"title": "p"})
	c := e.createIssue(e.chen, map[string]any{"title": "c"})
	e.must(200, e.do("PATCH", base+c.Identifier, e.chen, map[string]any{"parent": p.Identifier}))
	e.must(201, e.do("POST", base+p.Identifier+"/comments", e.chen, map[string]any{"body": "x"}))
	if _, err := e.pool.Exec(ctx, `INSERT INTO import_mappings (org_id, entity_type, source_id, target_id) VALUES ($1, 'issue', 'lin-1', $2)`, e.orgID, p.ID); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE FUNCTION probe_mark() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM set_config('probe.emitted', '1', true); RETURN NEW; END $$`,
		`CREATE FUNCTION probe_check() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF current_setting('probe.emitted', true) = '1' THEN RAISE EXCEPTION 'write after events.Emit'; END IF;
			RETURN COALESCE(NEW, OLD); END $$`,
		`CREATE TRIGGER probe_mark AFTER INSERT ON events FOR EACH ROW EXECUTE FUNCTION probe_mark()`,
		`CREATE TRIGGER probe_issues BEFORE DELETE ON issues FOR EACH ROW EXECUTE FUNCTION probe_check()`,
		`CREATE TRIGGER probe_activity BEFORE UPDATE ON issue_activity FOR EACH ROW EXECUTE FUNCTION probe_check()`,
		`CREATE TRIGGER probe_maps BEFORE DELETE ON import_mappings FOR EACH ROW EXECUTE FUNCTION probe_check()`,
		`CREATE TRIGGER probe_comments BEFORE DELETE ON comments FOR EACH ROW EXECUTE FUNCTION probe_check()`,
	} {
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	e.must(204, e.do("DELETE", base+p.Identifier, e.chen, nil))
	if e.eventCount("issue.deleted") != 1 {
		t.Fatal("want one issue.deleted")
	}
}
