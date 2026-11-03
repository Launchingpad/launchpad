package api

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"

	"devops/internal/tracker"
)

// LAUN-133: a query that names a ticket finds it, first; title text still works.
func TestIssues_SearchByTicketNumber(t *testing.T) {
	e := newEnv(t)
	const O = "/api/v1/orgs/orbit"
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	mk := func(team, title string) {
		e.createIssue(e.chen, map[string]any{"title": title, "team": team})
	}
	mk("ORB", "Rollout of 3 regions") // ORB-1 (title has a 3)
	mk("WEB", "web one")              // WEB-1
	mk("WEB", "web two")              // WEB-2
	mk("ORB", "orb two")              // ORB-2
	mk("ORB", "orb three")            // ORB-3
	mk("WEB", "web three")            // WEB-3 (newest)
	// The title match is the most recently updated, so only the ranking puts the tickets first.
	e.must(200, e.do("PATCH", O+"/issues/ORB-1", e.chen, map[string]any{"priority": 2}))

	list := func(q string) ([]string, *string) {
		t.Helper()
		var p page[tracker.Issue]
		e.must(200, e.do("GET", O+"/issues?"+q, e.chen, nil)).json(t, &p)
		return identifiers(p.Items), p.NextCursor
	}
	q := func(s string) string { return "query=" + url.QueryEscape(s) }
	for _, tt := range []struct {
		name, query string
		want        []string
	}{
		{"full identifier", q("ORB-3"), []string{"ORB-3"}},
		{"lower case", q("web-3"), []string{"WEB-3"}},
		{"hash and space", q("  #Web-3 "), []string{"WEB-3"}},
		{"spaces round the dash", q("WEB - 3"), []string{"WEB-3"}},
		{"unknown number", q("ORB-99"), []string{}},
		{"bare number: any team, matches first, then the title", q("3"), []string{"WEB-3", "ORB-3", "ORB-1"}},
		{"hash number is the ticket number only", q("#3"), []string{"WEB-3", "ORB-3"}},
		{"bare number in the team in scope", q("3") + "&team=ORB", []string{"ORB-3", "ORB-1"}},
		{"bare number, other team", q("3") + "&team=WEB", []string{"WEB-3"}},
		{"title text", q("regions"), []string{"ORB-1"}},
		{"identifier of another team is out of scope", q("WEB-3") + "&team=ORB", []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := list(tt.query); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}

	// Pagination keeps the matches first and loses and repeats nothing.
	for _, order := range []string{"", "&orderBy=createdAt", "&orderBy=manual&team=ORB"} {
		want := []string{"WEB-3", "ORB-3", "ORB-1"}
		if order == "&orderBy=manual&team=ORB" {
			want = nil
		}
		var got []string
		cur := ""
		for i := 0; i < 5; i++ {
			qs := q("3") + order + "&limit=1"
			if cur != "" {
				qs += "&cursor=" + url.QueryEscape(cur)
			}
			ids, next := list(qs)
			got = append(got, ids...)
			if next == nil {
				break
			}
			cur = *next
		}
		if want == nil { // manual: the ticket match is first, the rest follows with no repeat
			if len(got) != 2 || got[0] != "ORB-3" || got[1] != "ORB-1" {
				t.Fatalf("manual paging: %v", got)
			}
			continue
		}
		if !slices.Equal(got, want) {
			t.Fatalf("paging %q: got %v, want %v", order, got, want)
		}
	}

	// Ties: five more title matches with one updated_at and one sort_order.
	// Walking with limit 1 gives the ticket first, then each other row once.
	for i := 0; i < 5; i++ {
		mk("ORB", fmt.Sprintf("tie %d about 3", i))
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE issues SET updated_at = '2026-01-01T00:00:00Z', sort_order = 5`); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"", "&orderBy=createdAt", "&orderBy=manual"} {
		var got []string
		cur := ""
		for i := 0; i < 20; i++ {
			qs := q("3") + "&team=ORB&limit=1" + order
			if cur != "" {
				qs += "&cursor=" + url.QueryEscape(cur)
			}
			ids, next := list(qs)
			got = append(got, ids...)
			if next == nil {
				break
			}
			cur = *next
		}
		all, _ := list(q("3") + "&team=ORB&limit=50" + order)
		if len(got) != 7 || got[0] != "ORB-3" || !slices.Equal(got, all) {
			t.Fatalf("tied paging %q: %v (one page: %v)", order, got, all)
		}
		seen := map[string]bool{}
		for _, id := range got {
			if seen[id] {
				t.Fatalf("tied paging %q repeats %s: %v", order, id, got)
			}
			seen[id] = true
		}
	}

	// Cross-org: another org's identifier or number returns nothing here.
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	var ac tracker.Issue
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"title": "acme secret", "team": "ACM"})).json(t, &ac)
	if ac.Identifier != "ACM-1" {
		t.Fatalf("acme issue %s", ac.Identifier)
	}
	for _, s := range []string{"ACM-1", "1"} {
		got, _ := list(q(s))
		if slices.Contains(got, "ACM-1") {
			t.Fatalf("orbit search %q returned acme's ticket: %v", s, got)
		}
	}
	if got, _ := list(q("ACM-1")); len(got) != 0 {
		t.Fatalf("ACM-1 in orbit: %v", got)
	}
}
