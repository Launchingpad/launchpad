package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 4 of decisions-T20 (LAUN-219): the children of an issue for a
// restricted member: attachments and their download, relations (#41),
// activity snapshots (#43), the reply parent text (#23), and cross-org. The
// fixture is Test B's (teamFixture): ORB visible, HID hidden.

// presignSpy counts the presigned GETs, the Heads and the Deletes the server
// asks the store for.
type presignSpy struct {
	storage.Store
	gets, heads, deletes atomic.Int32
}

func (s *presignSpy) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.heads.Add(1)
	return s.Store.Head(ctx, key)
}

func (s *presignSpy) Delete(ctx context.Context, key string) error {
	s.deletes.Add(1)
	return s.Store.Delete(ctx, key)
}

func (s *presignSpy) PresignGet(key, contentType, disposition string, ttl time.Duration) string {
	s.gets.Add(1)
	return s.Store.PresignGet(key, contentType, disposition, ttl)
}

// fileRow inserts a file attachment row on an issue (no object: a download
// only signs a URL).
func fileRow(t *testing.T, e *env, issueID uuid.UUID, title string) uuid.UUID {
	t.Helper()
	key, fname, ctype, size := "org/"+e.orgID.String()+"/2026/"+uuid.NewString()+"/f.txt", "f.txt", "text/plain", int64(1)
	a, err := e.q.InsertAttachment(context.Background(), dbq.InsertAttachmentParams{OrgID: e.orgID, IssueID: issueID, Kind: "file", Title: title,
		ObjectKey: &key, Filename: &fname, ContentType: &ctype, SizeBytes: &size})
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// TestTeamAccess_AttachmentDownload (ticket 4 AC): the download of a hidden
// attachment is the 404 of a missing one and the store signs nothing; the
// same for another org's admin. A visible attachment still downloads, and
// the admin and the all-teams member download the hidden one.
func TestTeamAccess_AttachmentDownload(t *testing.T) {
	spy := &presignSpy{Store: storagetest.New(t)}
	f := newTeamFixtureWith(t, envOpts{store: spy})
	e := f.e
	hid := f.hidden["attachment"]
	made := uuid.NewString()
	for _, suffix := range []string{"", "/download"} {
		got := e.do("GET", f.orgPath("/attachments/"+hid+suffix), f.restricted, nil)
		want := e.do("GET", f.orgPath("/attachments/"+made+suffix), f.restricted, nil)
		gb := bytes.ReplaceAll(got.body, []byte(hid), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(made), []byte("<ref>"))
		if got.status != http.StatusNotFound || want.status != http.StatusNotFound || !bytes.Equal(gb, wb) ||
			got.header.Get("Location") != "" || got.header.Get("Cache-Control") != want.header.Get("Cache-Control") {
			t.Errorf("restricted %q: hidden %d %s %v, missing %d %s %v", suffix, got.status, gb, got.header, want.status, wb, want.header)
		}
		// Another org's admin, by this org's path and by their own org's path.
		for _, p := range []string{f.orgPath("/attachments/" + hid + suffix), "/api/v1/orgs/elsewhere/attachments/" + hid + suffix} {
			if res := e.do("GET", p, f.outsider, nil); res.status != http.StatusNotFound || res.errCode(t) != "not_found" {
				t.Errorf("outsider %s: %d %s, want 404", p, res.status, res.body)
			}
		}
	}
	if n := spy.gets.Load(); n != 0 {
		t.Fatalf("the store signed %d URLs for refused downloads, want 0", n)
	}
	// A visible attachment downloads for the restricted member.
	vis := fileRow(t, e, f.issueAs(t, e.chen, f.visible).ID, "visible file")
	if res := e.download(f.orgPath("/attachments/"+vis.String()+"/download"), f.restricted); res.StatusCode != http.StatusFound {
		t.Errorf("restricted visible download: %d", res.StatusCode)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		if res := e.download(f.orgPath("/attachments/"+hid+"/download"), tok); res.StatusCode != http.StatusFound {
			t.Errorf("%s hidden download: %d", who, res.StatusCode)
		}
		if res := e.do("GET", f.orgPath("/attachments/"+hid), tok, nil); res.status != http.StatusOK {
			t.Errorf("%s hidden attachment: %d %s", who, res.status, res.body)
		}
	}
	if n := spy.gets.Load(); n != 3 {
		t.Errorf("the store signed %d URLs, want 3 (one visible, admin, member)", n)
	}
}

// relationIDs lists every relation id of an issue's relations as tok reads them.
func (f *teamFixture) relationIDs(t *testing.T, tok, ref string) []string {
	t.Helper()
	var rel tracker.Relations
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/issues/"+ref+"/relations"), tok, nil)).json(t, &rel)
	var out []string
	for _, group := range [][]tracker.Relation{rel.Blocks, rel.BlockedBy, rel.Related, rel.DuplicateOf, rel.Duplicates} {
		for _, r := range group {
			out = append(out, r.ID.String())
		}
	}
	var is tracker.Issue
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/issues/"+ref+"?include=relations"), tok, nil)).json(t, &is)
	n := 0
	for _, group := range [][]tracker.Relation{is.Relations.Blocks, is.Relations.BlockedBy, is.Relations.Related, is.Relations.DuplicateOf, is.Relations.Duplicates} {
		n += len(group)
	}
	if n != len(out) {
		t.Errorf("%s: include=relations has %d edges, /relations %d", ref, n, len(out))
	}
	return out
}

// TestTeamAccess_Relations (ticket 4 AC, #41): an edge to a hidden issue is
// absent from the visible issue's view, the hidden issue's view is 404, and a
// restricted member can neither make nor remove one. Edges between visible
// issues work as before; the admin and the all-teams member see the edge.
func TestTeamAccess_Relations(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	rel := f.hidden["relation"]
	if got := f.relationIDs(t, f.restricted, f.visible); len(got) != 0 {
		t.Errorf("restricted reads the visible issue's edges %v, want none", got)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		if got := f.relationIDs(t, tok, f.visible); !strings.Contains(strings.Join(got, ","), rel) {
			t.Errorf("%s reads %v, want the edge %s", who, got, rel)
		}
		if got := f.relationIDs(t, tok, f.hidden["issue"]); !strings.Contains(strings.Join(got, ","), rel) {
			t.Errorf("%s reads the hidden side %v, want the edge %s", who, got, rel)
		}
	}
	e.must(http.StatusNotFound, e.do("GET", f.orgPath("/issues/"+f.hidden["issue"]+"/relations"), f.restricted, nil))
	before := f.rows("issue_relations")()
	e.must(http.StatusBadRequest, e.do("POST", f.orgPath("/issues/"+f.visible+"/relations"), f.restricted,
		map[string]any{"type": "blocks", "issue": f.hidden["issue"]}))
	e.must(http.StatusNotFound, e.do("POST", f.orgPath("/issues/"+f.hidden["issue"]+"/relations"), f.restricted,
		map[string]any{"type": "blocks", "issue": f.visible}))
	e.must(http.StatusNotFound, e.do("DELETE", f.orgPath("/issues/"+f.visible+"/relations/"+rel), f.restricted, nil))
	e.must(http.StatusNotFound, e.do("DELETE", f.orgPath("/issues/"+f.hidden["issue"]+"/relations/"+rel), f.restricted, nil))
	if after := f.rows("issue_relations")(); after != before {
		t.Fatalf("a refused relation write changed the rows:\nbefore %s\nafter  %s", before, after)
	}
	// Two visible issues: the restricted member makes and removes an edge.
	var own tracker.Relation
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+f.visible+"/relations"), f.restricted,
		map[string]any{"type": "blocks", "issue": f.child})).json(t, &own)
	if got := f.relationIDs(t, f.restricted, f.child); strings.Join(got, ",") != own.ID.String() {
		t.Errorf("restricted reads the child's edges %v, want only %s", got, own.ID)
	}
	e.must(http.StatusNoContent, e.do("DELETE", f.orgPath("/issues/"+f.child+"/relations/"+own.ID.String()), f.restricted, nil))
	// The admin removes the hidden edge.
	e.must(http.StatusNoContent, e.do("DELETE", f.orgPath("/issues/"+f.visible+"/relations/"+rel), e.chen, nil))
}

// storedActivity reads an issue's stored activity rows as text.
func (f *teamFixture) storedActivity(t *testing.T, ref string) string {
	t.Helper()
	var out string
	if err := f.e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(a.kind || ' ' || coalesce(a."from"::text, '-') || ' ' || coalesce(a."to"::text, '-'), E'\n' ORDER BY a.created_at, a.id), '')
		FROM issue_activity a WHERE a.org_id = $1 AND a.issue_id = $2`, f.e.orgID, f.issueAs(t, f.e.chen, ref).ID).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// activityAs reads an issue's activity as tok, keyed by kind (the last row of each kind).
func (f *teamFixture) activityAs(t *testing.T, tok, ref string) map[string]tracker.Activity {
	t.Helper()
	var page struct {
		Items []tracker.Activity `json:"items"`
	}
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/issues/"+ref+"/activity?limit=100"), tok, nil)).json(t, &page)
	out := map[string]tracker.Activity{}
	for _, a := range page.Items {
		out[a.Kind] = a
	}
	return out
}

// TestTeamAccess_ActivityRedaction (ticket 4 AC, #43): a snapshot that names a
// hidden issue, project, milestone or cycle, or an object that no longer
// exists, is null for the restricted viewer; the stored rows do not change;
// the admin and the all-teams member read them whole; a snapshot of a
// visible object stays.
func TestTeamAccess_ActivityRedaction(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	child := f.issueAs(t, e.chen, f.child)
	// By hand: a cycle row naming the hidden cycle (the API never sets a
	// cycle of another team), and a project row naming a project that is gone.
	testdb.MustExec(t, e.pool, `INSERT INTO issue_activity (org_id, issue_id, source, kind, "from", "to")
		VALUES ($1, $2, 'system', 'cycle', NULL, jsonb_build_object('id', $3::uuid, 'number', 1, 'name', 'Hidden sprint')),
		       ($1, $2, 'system', 'project', NULL, jsonb_build_object('id', $4::uuid, 'name', 'Gone plan'))`,
		e.orgID, child.ID, f.hidden["cycle"], uuid.NewString())
	stored := f.storedActivity(t, f.child)
	for _, want := range []string{f.hidden["issue"], "Hidden plan", "Hidden step", "Hidden sprint", "Gone plan"} {
		if !strings.Contains(stored, want) {
			t.Fatalf("the stored activity lacks %q: %s", want, stored)
		}
	}
	res := e.must(http.StatusOK, e.do("GET", f.orgPath("/issues/"+f.child+"/activity?limit=100"), f.restricted, nil))
	for _, m := range append(f.marks, "Hidden sprint", "Gone plan") {
		if bytes.Contains(res.body, []byte(m)) {
			t.Errorf("the restricted member's activity holds %q: %s", m, res.body)
		}
	}
	if p := f.activityRedacted(res.body); p != "" {
		t.Error(p)
	}
	var page struct {
		Items []tracker.Activity `json:"items"`
	}
	res.json(t, &page)
	kinds := map[string]int{}
	for _, a := range page.Items {
		kinds[a.Kind]++
	}
	if kinds[tracker.ActCycle] != 1 || kinds[tracker.ActProject] != 2 {
		t.Errorf("the redacted rows are still listed: kinds %v", kinds)
	}
	if after := f.storedActivity(t, f.child); after != stored {
		t.Errorf("a read changed the stored activity:\nbefore %s\nafter  %s", stored, after)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		body := string(e.must(http.StatusOK, e.do("GET", f.orgPath("/issues/"+f.child+"/activity?limit=100"), tok, nil)).body)
		for _, want := range []string{f.hidden["issue"], "hidden issue", "Hidden plan", "Hidden step", "Hidden sprint", "Gone plan"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s reads the activity without %q: %s", who, want, body)
			}
		}
	}
	// A new parent that the viewer sees: its snapshot stays, the old one is null.
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.child), e.chen, map[string]any{"parent": f.visible}))
	got := f.activityAs(t, f.restricted, f.child)[tracker.ActParent]
	if string(got.From) != "null" || !strings.Contains(string(got.To), f.visible) {
		t.Errorf("parent row for the restricted member: from %s to %s, want null and %s", got.From, got.To, f.visible)
	}
	var to struct {
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal(got.To, &to); err != nil || to.Identifier != f.visible {
		t.Errorf("parent to = %s, want the visible issue", got.To)
	}
}

// TestTeamAccess_ReplyParentText (#23): for a restricted member a reply whose
// parent is a visible comment of another issue is "unknown parentId", the
// answer for a made-up id; a caller who sees every team keeps the plain text;
// a reply on the same issue works.
func TestTeamAccess_ReplyParentText(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	var other, own struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+f.child+"/comments"), e.chen, map[string]any{"body": "on the child"})).json(t, &other)
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+f.visible+"/comments"), e.chen, map[string]any{"body": "on the visible"})).json(t, &own)
	made := uuid.NewString()
	path := f.orgPath("/issues/" + f.visible + "/comments")
	got := e.do("POST", path, f.restricted, map[string]any{"body": "r", "parentId": other.ID})
	want := e.do("POST", path, f.restricted, map[string]any{"body": "r", "parentId": made})
	if got.status != http.StatusBadRequest || !bytes.Equal(got.body, want.body) || !strings.Contains(string(got.body), "unknown parentId") {
		t.Errorf("restricted: other issue's comment %d %s, made-up %d %s", got.status, got.body, want.status, want.body)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		res := e.do("POST", path, tok, map[string]any{"body": "r", "parentId": other.ID})
		if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), "parentId is a comment on another issue") {
			t.Errorf("%s: %d %s, want the plain text", who, res.status, res.body)
		}
	}
	e.must(http.StatusCreated, e.do("POST", path, f.restricted, map[string]any{"body": "r", "parentId": own.ID}))
}

// TestTeamAccess_ChildrenCrossOrg: another org's attachment, comment,
// relation and issue, named in this org's routes of ticket 4, answer as
// unknown ones (404 in a path, 400 in a body), byte for byte.
func TestTeamAccess_ChildrenCrossOrg(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	far := func(p string) string { return "/api/v1/orgs/elsewhere" + p }
	var a, b tracker.Issue
	e.must(http.StatusCreated, e.do("POST", far("/issues"), f.outsider, map[string]any{"team": "ELS", "title": "far a"})).json(t, &a)
	e.must(http.StatusCreated, e.do("POST", far("/issues"), f.outsider, map[string]any{"team": "ELS", "title": "far b"})).json(t, &b)
	var com, att, rel struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", far("/issues/"+a.Identifier+"/comments"), f.outsider, map[string]any{"body": "far"})).json(t, &com)
	e.must(http.StatusCreated, e.do("POST", far("/issues/"+a.Identifier+"/attachments"), f.outsider,
		map[string]any{"kind": "link", "url": "https://example.com/far"})).json(t, &att)
	e.must(http.StatusCreated, e.do("POST", far("/issues/"+a.Identifier+"/relations"), f.outsider,
		map[string]any{"type": "related", "issue": b.Identifier})).json(t, &rel)
	made := uuid.NewString()
	same := func(name, method, p1, p2 string, b1, b2 any, r1 string) {
		t.Helper()
		got := e.do(method, f.orgPath(p1), e.chen, b1)
		want := e.do(method, f.orgPath(p2), e.chen, b2)
		gb := bytes.ReplaceAll(got.body, []byte(r1), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(made), []byte("<ref>"))
		if got.status != want.status || got.status < 400 || got.status >= 500 || !bytes.Equal(gb, wb) {
			t.Errorf("%s: other org %d %s; unknown %d %s", name, got.status, gb, want.status, wb)
		}
	}
	same("attachment", "GET", "/attachments/"+att.ID.String(), "/attachments/"+made, nil, nil, att.ID.String())
	same("download", "GET", "/attachments/"+att.ID.String()+"/download", "/attachments/"+made+"/download", nil, nil, att.ID.String())
	same("comment patch", "PATCH", "/comments/"+com.ID.String(), "/comments/"+made, map[string]any{"body": "x"}, map[string]any{"body": "x"}, com.ID.String())
	same("comment delete", "DELETE", "/comments/"+com.ID.String(), "/comments/"+made, nil, nil, com.ID.String())
	same("reply parentId", "POST", "/issues/"+f.visible+"/comments", "/issues/"+f.visible+"/comments",
		map[string]any{"body": "x", "parentId": com.ID}, map[string]any{"body": "x", "parentId": made}, com.ID.String())
	same("relation issue", "POST", "/issues/"+f.visible+"/relations", "/issues/"+f.visible+"/relations",
		map[string]any{"type": "related", "issue": a.ID}, map[string]any{"type": "related", "issue": made}, a.ID.String())
	same("relation delete", "DELETE", "/issues/"+f.visible+"/relations/"+rel.ID.String(), "/issues/"+f.visible+"/relations/"+made, nil, nil, rel.ID.String())
	same("issue by id", "GET", "/issues/"+a.ID.String()+"?include=relations,attachments,comments", "/issues/"+made+"?include=relations,attachments,comments", nil, nil, a.ID.String())
	same("activity", "GET", "/issues/"+a.ID.String()+"/activity", "/issues/"+made+"/activity", nil, nil, a.ID.String())
}

// TestTeamAccess_AttachHeldKey (LAUN-219 review): an upload already attached
// to a hidden issue, attached again by a restricted member to a visible
// issue, is the 409 of an upload attached to another visible issue, byte for
// byte; the store sees no Head and no Delete, and the object stays.
func TestTeamAccess_AttachHeldKey(t *testing.T) {
	spy := &presignSpy{Store: storagetest.New(t)}
	f := newTeamFixtureWith(t, envOpts{store: spy})
	e := f.e
	upload := func(target string) string {
		up := e.prepare(e.chen, "a.png", "image/png", len(pngBytes))
		if st := putUpload(t, up, pngBytes); st != http.StatusOK {
			t.Fatalf("put upload: %d", st)
		}
		e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+target+"/attachments"), e.chen,
			map[string]any{"kind": "file", "uploadKey": up.UploadKey}))
		return up.UploadKey
	}
	hiddenKey, visibleKey := upload(f.hidden["issue"]), upload(f.child)
	spy.heads.Store(0)
	spy.deletes.Store(0)
	before := f.rows("attachments")()
	path := f.orgPath("/issues/" + f.visible + "/attachments")
	got := e.do("POST", path, f.restricted, map[string]any{"kind": "file", "uploadKey": hiddenKey})
	want := e.do("POST", path, f.restricted, map[string]any{"kind": "file", "uploadKey": visibleKey})
	if got.status != http.StatusConflict || want.status != http.StatusConflict || !bytes.Equal(got.body, want.body) {
		t.Errorf("hidden-issue key %d %s; visible-issue key %d %s", got.status, got.body, want.status, want.body)
	}
	if h, d := spy.heads.Load(), spy.deletes.Load(); h != 0 || d != 0 {
		t.Errorf("the store saw %d Heads and %d Deletes, want none", h, d)
	}
	if after := f.rows("attachments")(); after != before {
		t.Errorf("a refused attach changed the rows")
	}
	if _, err := spy.Store.Head(context.Background(), hiddenKey); err != nil {
		t.Errorf("the hidden issue's object is gone: %v", err)
	}
	// A key made from a hidden attachment's id with another filename is not
	// held (only the exact key is): it answers as a key with a made-up id,
	// byte for byte, with one Head each and no Delete (#10, no oracle).
	parts := strings.Split(hiddenKey, "/") // org/{org}/{yyyy}/{id}/{name}
	if len(parts) != 5 {
		t.Fatalf("upload key %q", hiddenKey)
	}
	keyOf := func(id string) string { return strings.Join([]string{parts[0], parts[1], parts[2], id, "x.png"}, "/") }
	for _, id := range []string{parts[3], f.hidden["attachment"]} {
		made := uuid.Must(uuid.NewV7()).String() // prepare mints v7 ids
		spy.heads.Store(0)
		spy.deletes.Store(0)
		got := e.do("POST", path, f.restricted, map[string]any{"kind": "file", "uploadKey": keyOf(id)})
		want := e.do("POST", path, f.restricted, map[string]any{"kind": "file", "uploadKey": keyOf(made)})
		gb := bytes.ReplaceAll(got.body, []byte(id), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(made), []byte("<ref>"))
		if got.status != http.StatusBadRequest || got.status != want.status || !bytes.Equal(gb, wb) {
			t.Errorf("key from hidden id %s: %d %s; made-up id: %d %s", id, got.status, gb, want.status, wb)
		}
		if h, d := spy.heads.Load(), spy.deletes.Load(); h != 2 || d != 0 {
			t.Errorf("key from hidden id %s: %d Heads and %d Deletes, want 2 and 0", id, h, d)
		}
	}
	if after := f.rows("attachments")(); after != before {
		t.Errorf("a refused attach changed the rows")
	}
}

// TestTeamAccess_QueryPredicates pins two predicates that a second check in
// Go would hide: ListRelationsForIssue alone drops an edge to a hidden issue
// (#41), and GetComment alone finds no comment of a hidden issue or a hidden
// document for a restricted caller (#23). With all teams both find the rows.
func TestTeamAccess_QueryPredicates(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	ctx := context.Background()
	orb := f.teamID(t, testdb.TeamKey)
	restricted := []uuid.UUID{orb}
	visible := f.issueAs(t, e.chen, f.visible)
	rel := uuid.MustParse(f.hidden["relation"])
	for _, all := range []bool{false, true} {
		rows, err := e.q.ListRelationsForIssue(ctx, dbq.ListRelationsForIssueParams{OrgID: e.orgID, IssueID: visible.ID, AllTeams: all, TeamIds: restricted})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			found = found || r.ID == rel
		}
		if found != all {
			t.Errorf("ListRelationsForIssue all=%v: the edge to the hidden issue found=%v", all, found)
		}
	}
	var own struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+f.visible+"/comments"), e.chen, map[string]any{"body": "seen"})).json(t, &own)
	for _, c := range []struct {
		name string
		id   uuid.UUID
		seen bool // found for the restricted caller
	}{
		{"hidden issue comment", uuid.MustParse(f.hidden["comment"]), false},
		{"hidden document comment", uuid.MustParse(f.hidden["docComment"]), false},
		{"visible issue comment", own.ID, true},
	} {
		for _, all := range []bool{false, true} {
			_, err := e.q.GetComment(ctx, dbq.GetCommentParams{OrgID: e.orgID, ID: c.id, AllTeams: all, TeamIds: restricted})
			want := all || c.seen
			if (err == nil) != want || (err != nil && !errors.Is(err, pgx.ErrNoRows)) {
				t.Errorf("GetComment %s all=%v: err %v, want found=%v", c.name, all, err, want)
			}
		}
	}
}
