package mcp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/testdb"
)

// Team access through the MCP server (decisions-T20 #66, ticket 8): the tools
// call REST with the caller's token, so the server enforces. A token of a
// member who sees team ORB only gets nothing of team HID from any tool, and a
// hidden object answers as a made-up one, in the same words. Every tool of the
// catalogue has a case; the golden tool files do not change.

// hiddenTeam is the org's team HID with one object of every kind a tool names,
// made by the admin, and the marks no answer to the restricted token may hold.
type hiddenTeam struct {
	issue, comment, attachment, project, milestone, document, cycle, product string
	marks                                                                    []string
}

func makeHiddenTeam(l *liveAPI) hiddenTeam {
	l.t.Helper()
	var h hiddenTeam
	var team, issue, comment, att, project, milestone, doc, product struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Slug       string `json:"slug"`
	}
	l.rest("POST", "/teams", l.chen, map[string]any{"key": "HID", "name": "Hidden"}, &team)
	l.rest("POST", "/issues", l.chen, map[string]any{"team": "HID", "title": "hidden issue", "description": "hidden description"}, &issue)
	l.rest("POST", "/issues/"+issue.Identifier+"/comments", l.chen, map[string]any{"body": "hidden comment"}, &comment)
	l.rest("POST", "/issues/"+issue.Identifier+"/attachments", l.chen,
		map[string]any{"kind": "link", "url": "https://example.com/hidden-link", "title": "hidden link"}, &att)
	l.rest("POST", "/projects", l.chen, map[string]any{"name": "Hidden plan", "teams": []string{"HID"}}, &project)
	l.rest("POST", "/milestones", l.chen, map[string]any{"projectId": project.ID, "name": "Hidden step"}, &milestone)
	l.rest("POST", "/documents", l.chen, map[string]any{"projectId": project.ID, "title": "Hidden doc", "content": "hidden content"}, &doc)
	l.rest("POST", "/projects/"+project.ID+"/updates", l.chen, map[string]any{"health": "on_track", "body": "hidden update"}, nil)
	l.rest("POST", "/products", l.chen, map[string]any{"name": "Secret product", "slug": "secret-product", "teams": []string{"HID"}}, &product)
	var label struct {
		ID string `json:"id"`
	}
	l.rest("POST", "/labels", l.chen, map[string]any{"name": "hidden-label", "color": "#112233", "team": "HID"}, &label)
	// The hidden issue sits in the hidden project, milestone and label, so the
	// filters by them find it for the admin.
	l.rest("PATCH", "/issues/"+issue.Identifier, l.chen, map[string]any{"projectId": project.ID, "milestoneId": milestone.ID,
		"labels": []string{label.ID}}, nil)
	l.rest("PUT", "/agent-guidance", l.chen, map[string]any{"team": "HID", "text": "hidden guidance"}, nil)
	// Cycles are made by the cycle job; the test inserts one.
	if err := l.pool.QueryRow(context.Background(), `INSERT INTO cycles (org_id, team_id, number, starts_at, ends_at)
		SELECT org_id, id, 1, now(), now() + interval '7 days' FROM teams WHERE id = $1 RETURNING id::text`, team.ID).Scan(&h.cycle); err != nil {
		l.t.Fatal(err)
	}
	h.issue, h.comment, h.attachment, h.project = issue.Identifier, comment.ID, att.ID, project.ID
	h.milestone, h.document, h.product = milestone.ID, doc.ID, product.Slug
	h.marks = []string{"HID", team.ID, issue.ID, "hidden issue", "hidden description", comment.ID, "hidden comment",
		att.ID, "hidden link", "hidden-link", project.ID, "Hidden plan", milestone.ID, "Hidden step", doc.ID, "Hidden doc",
		"hidden content", "hidden update", product.ID, "Secret product", "secret-product", "hidden-label", "hidden guidance", h.cycle}
	return h
}

// restrict makes username a member who sees only team key.
func (l *liveAPI) restrict(username, key string) {
	l.t.Helper()
	var org, user, team uuid.UUID
	if err := l.pool.QueryRow(context.Background(), `SELECT o.id, u.id, t.id FROM orgs o JOIN teams t ON t.org_id = o.id, users u
		WHERE o.slug = 'orbit' AND t.key = $1 AND u.username = $2`, key, username).Scan(&org, &user, &team); err != nil {
		l.t.Fatal(err)
	}
	testdb.MakeRestricted(l.t, l.pool, org, user, team)
}

// pair is one tool call with a hidden reference and the same call with a
// made-up one. refs are the hidden and made-up strings replaced with <ref>
// before the two answers are compared.
type pair struct {
	tool         string
	hidden, made map[string]any
	refs         [][2]string
}

// sameAnswer runs both calls of p: the answers must be equal once the echoed
// references are replaced, and neither may hold a mark.
func sameAnswer(t *testing.T, s *session, p pair, marks []string) {
	t.Helper()
	h, m := s.callTool(p.tool, p.hidden), s.callTool(p.tool, p.made)
	ht, mt := h.Text, m.Text
	for _, r := range p.refs {
		ht = strings.ReplaceAll(ht, r[0], "<ref>")
		mt = strings.ReplaceAll(mt, r[1], "<ref>")
	}
	if h.IsError != m.IsError || ht != mt {
		t.Errorf("%s: a hidden object answers differently from a made-up one:\nhidden  %v %s\nmade-up %v %s", p.tool, h.IsError, ht, m.IsError, mt)
	}
	noMarks(t, p.tool+" (hidden)", ht, marks)
}

// noMarks fails when text holds a string of team HID.
func noMarks(t *testing.T, what, text string, marks []string) {
	t.Helper()
	for _, mk := range marks {
		if strings.Contains(text, mk) {
			t.Errorf("%s: the restricted token's answer holds %q of the hidden team: %s", what, mk, text)
		}
	}
}

// TestIntegration_RestrictedToken runs every tool of the full profile with
// the token of a member who sees ORB only: the lists hold nothing of HID, a
// hidden object is the made-up one's answer, and no write lands.
func TestIntegration_RestrictedToken(t *testing.T) {
	l := newLiveAPI(t)
	h := makeHiddenTeam(l)
	var visible struct {
		Identifier string `json:"identifier"`
	}
	l.rest("POST", "/issues", l.chen, map[string]any{"team": "ORB", "title": "visible issue"}, &visible)
	l.restrict("claude", "ORB")
	dir := filepath.Join(t.TempDir(), "dl")
	s := l.sessionWith(l.claude, ProfileFull, func(c *ClientConfig) { c.DownloadDir, c.AllowInsecureHTTP = dir, true })

	madeID, madeID2 := uuid.NewString(), uuid.NewString()
	team := [][2]string{{"HID", "ZZZ"}}
	issue := [][2]string{{h.issue, "HID-999"}}
	pairs := []pair{
		{"get_issue", map[string]any{"id": h.issue}, map[string]any{"id": "HID-999"}, issue},
		{"list_issues", map[string]any{"team": "HID"}, map[string]any{"team": "ZZZ"}, team},
		{"list_issues", map[string]any{"project": "Hidden plan"}, map[string]any{"project": "Made plan"}, [][2]string{{"Hidden plan", "Made plan"}}},
		{"list_issues", map[string]any{"project": h.project}, map[string]any{"project": madeID}, [][2]string{{h.project, madeID}}},
		{"list_issues", map[string]any{"parentId": h.issue}, map[string]any{"parentId": "HID-999"}, issue},
		{"list_issues", map[string]any{"cycle": h.cycle}, map[string]any{"cycle": madeID}, [][2]string{{h.cycle, madeID}}},
		{"list_issues", map[string]any{"label": "hidden-label"}, map[string]any{"label": "made-label"}, [][2]string{{"hidden-label", "made-label"}}},
		{"list_issues", map[string]any{"product": h.product}, map[string]any{"product": "made-product"}, [][2]string{{h.product, "made-product"}}},
		{"list_issues", map[string]any{"query": h.issue}, map[string]any{"query": "HID-999"}, issue},
		{"save_issue", map[string]any{"id": h.issue, "title": "changed"}, map[string]any{"id": "HID-999", "title": "changed"}, issue},
		{"save_issue", map[string]any{"title": "probe", "team": "HID"}, map[string]any{"title": "probe", "team": "ZZZ"}, team},
		{"save_issue", map[string]any{"id": visible.Identifier, "parentId": h.issue}, map[string]any{"id": visible.Identifier, "parentId": "HID-999"}, issue},
		{"save_issue", map[string]any{"id": visible.Identifier, "blocks": []string{h.issue}}, map[string]any{"id": visible.Identifier, "blocks": []string{"HID-999"}}, issue},
		{"save_issue", map[string]any{"id": visible.Identifier, "project": "Hidden plan"}, map[string]any{"id": visible.Identifier, "project": "Made plan"}, [][2]string{{"Hidden plan", "Made plan"}}},
		{"save_issue", map[string]any{"id": visible.Identifier, "cycle": h.cycle}, map[string]any{"id": visible.Identifier, "cycle": madeID}, [][2]string{{h.cycle, madeID}}},
		{"list_issue_statuses", map[string]any{"team": "HID"}, map[string]any{"team": "ZZZ"}, team},
		{"get_guidance", map[string]any{"team": "HID"}, map[string]any{"team": "ZZZ"}, team},
		{"list_issue_labels", map[string]any{"team": "HID"}, map[string]any{"team": "ZZZ"}, team},
		{"list_cycles", map[string]any{"teamId": "HID"}, map[string]any{"teamId": "ZZZ"}, team},
		{"list_milestones", map[string]any{"project": "Hidden plan"}, map[string]any{"project": "Made plan"}, [][2]string{{"Hidden plan", "Made plan"}}},
		{"list_milestones", map[string]any{"project": h.project}, map[string]any{"project": madeID}, [][2]string{{h.project, madeID}}},
		{"save_comment", map[string]any{"issueId": h.issue, "body": "probe"}, map[string]any{"issueId": "HID-999", "body": "probe"}, issue},
		{"save_comment", map[string]any{"id": h.comment, "body": "probe"}, map[string]any{"id": madeID, "body": "probe"}, [][2]string{{h.comment, madeID}}},
		{"save_comment", map[string]any{"documentId": h.document, "body": "probe"}, map[string]any{"documentId": madeID, "body": "probe"}, [][2]string{{h.document, madeID}}},
		{"save_comment", map[string]any{"issueId": visible.Identifier, "parentId": h.comment, "body": "probe"},
			map[string]any{"issueId": visible.Identifier, "parentId": madeID, "body": "probe"}, [][2]string{{h.comment, madeID}}},
		{"list_comments", map[string]any{"issueId": h.issue}, map[string]any{"issueId": "HID-999"}, issue},
		{"list_comments", map[string]any{"documentId": h.document}, map[string]any{"documentId": madeID}, [][2]string{{h.document, madeID}}},
		{"get_document", map[string]any{"id": h.document}, map[string]any{"id": madeID}, [][2]string{{h.document, madeID}}},
		{"list_documents", map[string]any{"project": "Hidden plan"}, map[string]any{"project": "Made plan"}, [][2]string{{"Hidden plan", "Made plan"}}},
		{"save_document", map[string]any{"id": h.document, "content": "probe"}, map[string]any{"id": madeID, "content": "probe"}, [][2]string{{h.document, madeID}}},
		{"save_document", map[string]any{"title": "probe", "project": "Hidden plan"}, map[string]any{"title": "probe", "project": "Made plan"}, [][2]string{{"Hidden plan", "Made plan"}}},
		{"list_projects", map[string]any{"team": "HID"}, map[string]any{"team": "ZZZ"}, team},
		{"get_project", map[string]any{"query": "Hidden plan"}, map[string]any{"query": "Made plan"}, [][2]string{{"Hidden plan", "Made plan"}}},
		{"get_project", map[string]any{"query": h.project}, map[string]any{"query": madeID}, [][2]string{{h.project, madeID}}},
		{"save_project", map[string]any{"id": h.project, "name": "probe"}, map[string]any{"id": madeID, "name": "probe"}, [][2]string{{h.project, madeID}}},
		{"save_project", map[string]any{"name": "probe", "teams": []string{"HID"}}, map[string]any{"name": "probe", "teams": []string{"ZZZ"}}, team},
		{"list_project_updates", map[string]any{"project": h.project}, map[string]any{"project": madeID}, [][2]string{{h.project, madeID}}},
		{"save_project_update", map[string]any{"project": h.project, "health": "on_track"}, map[string]any{"project": madeID, "health": "on_track"}, [][2]string{{h.project, madeID}}},
		{"prepare_attachment_upload", map[string]any{"issue": h.issue, "filename": "a.png", "contentType": "image/png", "size": 4},
			map[string]any{"issue": "HID-999", "filename": "a.png", "contentType": "image/png", "size": 4}, issue},
		{"create_attachment_from_upload", map[string]any{"issue": h.issue, "assetUrl": "org/x/2026/y/a.png"},
			map[string]any{"issue": "HID-999", "assetUrl": "org/x/2026/y/a.png"}, issue},
		{"download_attachment", map[string]any{"issue": h.issue, "attachment": h.attachment},
			map[string]any{"issue": "HID-999", "attachment": madeID2}, [][2]string{{h.issue, "HID-999"}, {h.attachment, madeID2}}},
		{"download_attachment", map[string]any{"issue": visible.Identifier, "attachment": h.attachment},
			map[string]any{"issue": visible.Identifier, "attachment": madeID2}, [][2]string{{h.attachment, madeID2}}},
	}
	// Lists with no reference: nothing of team HID.
	lists := map[string]map[string]any{
		"list_issues":       {"limit": 250, "includeArchived": true},
		"list_teams":        {},
		"get_guidance":      {},
		"list_products":     {"includeArchived": true},
		"list_issue_labels": {},
		"list_users":        {},
		"list_cycles":       {},
		"list_documents":    {},
		"list_projects":     {"includeArchived": true},
	}

	covered := map[string]bool{}
	before := l.counts()
	for _, p := range pairs {
		covered[p.tool] = true
		sameAnswer(t, s, p, h.marks)
	}
	names := make([]string, 0, len(lists))
	for n := range lists {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		covered[n] = true
		r := s.callTool(n, lists[n])
		if r.IsError {
			t.Errorf("%s: %s", n, r.Text)
		}
		noMarks(t, n, r.Text, h.marks)
	}
	if after := l.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a refused call changed the database: %v -> %v", before, after)
	}
	for _, tl := range catalogue() {
		if !covered[tl.Name] {
			t.Errorf("tool %s has no restricted-token case", tl.Name)
		}
	}

	// The cases are real: the admin's token reads team HID through every list,
	// and every read pair answers the hidden call differently from the
	// made-up one (so the comparison above can fail).
	admin := l.sessionWith(l.chen, ProfileFull, func(c *ClientConfig) { c.DownloadDir, c.AllowInsecureHTTP = dir, true })
	// get_guidance and list_cycles read the default team (ORB) and list_users
	// the org-wide roster (#7): those hold nothing of HID for anyone.
	for _, n := range names {
		if n == "get_guidance" || n == "list_cycles" || n == "list_users" {
			continue
		}
		r := admin.callTool(n, lists[n])
		found := false
		for _, mk := range h.marks {
			found = found || strings.Contains(r.Text, mk)
		}
		if r.IsError || !found {
			t.Errorf("%s as the admin shows nothing of team HID: %s", n, r.Text)
		}
	}
	readOnly := map[string]bool{}
	for _, tl := range catalogue() {
		readOnly[tl.Name] = tl.ReadOnly
	}
	for _, p := range pairs {
		if !readOnly[p.tool] {
			continue
		}
		hr, mr := admin.callTool(p.tool, p.hidden), admin.callTool(p.tool, p.made)
		ht, mt := hr.Text, mr.Text
		for _, r := range p.refs {
			ht, mt = strings.ReplaceAll(ht, r[0], "<ref>"), strings.ReplaceAll(mt, r[1], "<ref>")
		}
		if hr.IsError == mr.IsError && ht == mt {
			t.Errorf("%s %v: the admin gets the same answer for the hidden and the made-up object, so the case proves nothing: %s", p.tool, p.hidden, ht)
		}
	}

	// LAUNCHPAD_TEAM set to a team the owner cannot see answers as an unknown
	// team: the default team is resolved through the visible list (#9).
	def := func(team string) *session {
		c, err := NewClient(ClientConfig{BaseURL: l.url, Token: l.claude, Org: "orbit"})
		if err != nil {
			t.Fatal(err)
		}
		tools, err := NewTools(c, team, ProfileFull)
		if err != nil {
			t.Fatal(err)
		}
		logs := &bytes.Buffer{}
		return &session{t: t, tools: tools, logs: logs, srv: NewServer(tools, slog.New(slog.NewTextHandler(logs, nil)), "test")}
	}
	hid, zzz := def("HID"), def("ZZZ")
	for _, call := range []struct {
		tool string
		args map[string]any
	}{{"save_issue", map[string]any{"title": "default team"}}, {"get_guidance", map[string]any{}}, {"list_cycles", map[string]any{}}} {
		a, b := hid.callTool(call.tool, call.args), zzz.callTool(call.tool, call.args)
		at, bt := strings.ReplaceAll(a.Text, "HID", "<ref>"), strings.ReplaceAll(b.Text, "ZZZ", "<ref>")
		if a.IsError != b.IsError || at != bt || !a.IsError {
			t.Errorf("%s with a hidden default team: %v %s, made-up: %v %s", call.tool, a.IsError, at, b.IsError, bt)
		}
	}
	if after := l.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a default-team call changed the database: %v -> %v", before, after)
	}
}

// The reviewer profile with a restricted token: its three tools answer a
// hidden object as a made-up one.
func TestIntegration_RestrictedReviewer(t *testing.T) {
	l := newLiveAPI(t)
	h := makeHiddenTeam(l)
	l.restrict("claude", "ORB")
	rev := l.session(l.mint("read"), ProfileReviewer)
	madeID := uuid.NewString()
	for _, p := range []pair{
		{"get_issue", map[string]any{"id": h.issue, "includeComments": true}, map[string]any{"id": "HID-999", "includeComments": true}, [][2]string{{h.issue, "HID-999"}}},
		{"list_comments", map[string]any{"issueId": h.issue}, map[string]any{"issueId": "HID-999"}, [][2]string{{h.issue, "HID-999"}}},
		{"list_comments", map[string]any{"documentId": h.document}, map[string]any{"documentId": madeID}, [][2]string{{h.document, madeID}}},
		{"get_document", map[string]any{"id": h.document}, map[string]any{"id": madeID}, [][2]string{{h.document, madeID}}},
	} {
		sameAnswer(t, rev, p, h.marks)
	}
	if got := len(reviewerTools); got != 3 {
		t.Fatalf("reviewer tools = %d, want 3: every one has a case above", got)
	}
}
