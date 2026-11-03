package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// eventTypeConsts reads the event type constants of events.go: every string
// constant but Channel and the ScopeKind values.
func eventTypeConsts(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name == "Channel" || strings.HasPrefix(n.Name, "ScopeKind") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[n.Name] = v
			}
		}
	}
	if len(out) < 40 {
		t.Fatalf("found only %d event type constants; the parse is wrong", len(out))
	}
	return out
}

// TestEventsEmitScope is the unit half of Test D (decisions-T20 #18, #53):
// every event type has a scope rule, Emit refuses an event without a valid
// scope, and the scope constructors keep their promises. The integration
// half (TestEventsEmitScope_RestrictedViewer, internal/api) emits the kinds
// through their routes and reads the feed as a restricted member.
func TestEventsEmitScope(t *testing.T) {
	consts := eventTypeConsts(t)
	types := map[string]bool{}
	for name, typ := range consts {
		types[typ] = true
		if _, ok := kindScopes[typ]; !ok {
			t.Errorf("event type %s (%q) has no scope rule in kindScopes: decide who may read it (T20 #54)", name, typ)
		}
	}
	for typ, kinds := range kindScopes {
		if !types[typ] {
			t.Errorf("kindScopes has %q, which is no event type constant", typ)
		}
		if len(kinds) == 0 {
			t.Errorf("kindScopes[%q] allows no scope", typ)
		}
		for _, k := range kinds {
			if !slices.Contains([]string{ScopeKindAny, ScopeKindAll, ScopeKindOrg, ScopeKindAdmin}, k) {
				t.Errorf("kindScopes[%q] allows %q, which Emit never writes", typ, k)
			}
		}
	}

	team, other := uuid.New(), uuid.New()
	for _, c := range []struct {
		name, typ string
		scope     Scope
		want      string // part of the error; empty: accepted
	}{
		{"zero scope", IssueCreated, Scope{}, "has no scope"},
		{"unknown type", "issue.exploded", ScopeTeams(team), "has no scope rule"},
		{"kind not allowed", IssueCreated, ScopeOrg(), `may not have scope "org"`},
		{"admin not allowed", IssueUpdated, ScopeAdmin(), `may not have scope "admin"`},
		{"any with no team", IssueCreated, ScopeTeams(), "needs a team"},
		{"any with only the nil id", IssueCreated, ScopeTeams(uuid.Nil), "needs a team"},
		{"all with no team", RelationCreated, ScopeAllTeams(), "needs a team"},
		{"relation is all-of", RelationCreated, ScopeTeams(team), `may not have scope "any"`},
		{"issue ok", IssueCreated, ScopeTeams(team), ""},
		{"relation ok", RelationDeleted, ScopeAllTeams(team, other), ""},
		{"org label ok", LabelCreated, ScopeOrg(), ""},
		{"project with no team is org", ProjectCreated, ScopeProject(nil), ""},
	} {
		err := checkScope(c.typ, c.scope)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}

	if s := ScopeProject(nil); s.Kind() != ScopeKindOrg || len(s.TeamIDs()) != 0 {
		t.Errorf("ScopeProject(nil) = %+v, want org", s)
	}
	if s := ScopeProject([]uuid.UUID{team}); s.Kind() != ScopeKindAny || !slices.Equal(s.TeamIDs(), []uuid.UUID{team}) {
		t.Errorf("ScopeProject(team) = %+v", s)
	}
	s := ScopeAllTeams(other, team, other, uuid.Nil)
	if got := s.TeamIDs(); len(got) != 2 || !slices.Contains(got, team) || !slices.Contains(got, other) {
		t.Errorf("ScopeAllTeams does not drop duplicates and the nil id: %v", got)
	}
	if got := ScopeOrg().TeamIDs(); got == nil || len(got) != 0 {
		t.Errorf("an org scope's team list must be empty and not nil (it binds as '{}'): %#v", got)
	}
}
