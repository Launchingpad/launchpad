package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"devops/api"
)

// orgPrefix is where the RequireOrgMember group is mounted.
const orgPrefix = "/api/v1/orgs/{org}"

// walkRoutes lists "METHOD /full/pattern" of a router (trailing slash cut, as
// routesOf does).
func walkRoutes(r chi.Routes) ([]string, error) {
	var out []string
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		out = append(out, method+" "+route)
		return nil
	})
	sort.Strings(out)
	return out, err
}

// teamRuleKey is the teamRules key of a full route, and false when the route is
// not under /orgs/{org}.
func teamRuleKey(route string) (string, bool) {
	method, p, _ := strings.Cut(route, " ")
	if p != orgPrefix && !strings.HasPrefix(p, orgPrefix+"/") {
		return "", false
	}
	rel := strings.TrimPrefix(p, orgPrefix)
	if rel == "" {
		rel = "/"
	}
	return method + " " + rel, true
}

// ruleProblems is Test A: every route has exactly one rule, every rule has a
// route, and every rule is well formed.
func ruleProblems(routes []string, rules, outside map[string]teamRule) []string {
	var out []string
	usedRule, usedOutside := map[string]bool{}, map[string]bool{}
	for _, r := range routes {
		key, inOrg := teamRuleKey(r)
		_, isOutside := outside[r]
		_, isRule := rules[key]
		switch {
		case isOutside && inOrg && isRule:
			out = append(out, fmt.Sprintf("route %s has a rule in teamRules AND in outsideRules: keep one", r))
		case isOutside:
			usedOutside[r] = true
		case inOrg && isRule:
			usedRule[key] = true
		case inOrg:
			out = append(out, fmt.Sprintf("route %s has no team rule: add %q to teamRules in internal/api/teamrules.go (decisions-T20 #17)", r, key))
		default:
			out = append(out, fmt.Sprintf("route %s is outside /orgs/{org} and has no entry in outsideRules (decisions-T20 #17)", r))
		}
	}
	for k := range rules {
		if !usedRule[k] {
			out = append(out, fmt.Sprintf("teamRules has %q, which the router does not serve", k))
		}
	}
	for k, r := range outside {
		if !usedOutside[k] {
			out = append(out, fmt.Sprintf("outsideRules has %q, which the router does not serve", k))
		}
		if _, inOrg := teamRuleKey(k); inOrg && !publicOrgRoutes[k] {
			out = append(out, fmt.Sprintf("outsideRules has %q under /orgs/{org}: only the three unauthenticated webhooks may be there; put it in teamRules", k))
		}
		out = append(out, ruleShapeProblems(k, r)...)
	}
	for k, r := range rules {
		out = append(out, ruleShapeProblems(k, r)...)
	}
	sort.Strings(out)
	return out
}

// publicOrgRoutes are the only routes under /orgs/{org} outside the
// RequireOrgMember group: the public webhooks (AGENTS.md).
var publicOrgRoutes = map[string]bool{
	"POST /api/v1/orgs/{org}/integrations/gitlab/webhook":    true,
	"POST /api/v1/orgs/{org}/integrations/github/webhook":    true,
	"POST /api/v1/orgs/{org}/integrations/bitbucket/webhook": true,
}

// ruleShapeProblems checks the fields one kind needs.
func ruleShapeProblems(key string, r teamRule) []string {
	var out []string
	bad := func(format string, a ...any) { out = append(out, key+": "+fmt.Sprintf(format, a...)) }
	if r.Ticket == "" {
		bad("no Ticket: name the build ticket that enforces it")
	}
	switch r.Kind {
	case ruleList, ruleFeed:
		if r.Resource == "" {
			bad("%s rule has no Resource", r.Kind)
		}
	case rulePath:
		if r.Resource == "" || r.Param == "" {
			bad("path rule needs Resource and Param")
		} else if !strings.Contains(key, "{"+r.Param+"}") {
			bad("path rule names {%s}, which the pattern does not have", r.Param)
		}
	case ruleBody, ruleFilter:
		if r.Resource == "" || r.Param == "" {
			bad("%s rule needs Resource and Param", r.Kind)
		}
	case ruleOwn, ruleAdmin, ruleNone:
		if strings.TrimSpace(r.Reason) == "" {
			bad("%s rule has no Reason", r.Kind)
		}
	default:
		bad("unknown rule kind %q", r.Kind)
	}
	return out
}

// TestTeamRulesCoverRouter is Test A of decisions-T20 #18: a new route cannot
// merge without a team rule.
func TestTeamRulesCoverRouter(t *testing.T) {
	routes := routesOf(t)
	if len(routes) < 100 {
		t.Fatalf("walked only %d routes: the walk is broken", len(routes))
	}
	for _, p := range ruleProblems(routes, teamRules, outsideRules) {
		t.Error(p)
	}
}

// TestTeamRulesCoverRouter_FailsOnNewRoute shows Test A fails when a route is
// added without a rule, and when a rule names no route (a throwaway router).
func TestTeamRulesCoverRouter_FailsOnNewRoute(t *testing.T) {
	h := func(http.ResponseWriter, *http.Request) {}
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/healthz2", h)
		r.Route("/orgs/{org}", func(r chi.Router) {
			r.Get("/teams", h)
			r.Get("/widgets", h) // new, put in outsideRules by mistake
			r.Get("/gizmos", h)  // new, no rule
		})
	})
	routes, err := walkRoutes(r)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]teamRule{
		"GET /teams":   listRule("team", "3"),
		"GET /gadgets": listRule("gadget", "3"), // no route
	}
	outside := map[string]teamRule{"GET /api/v1/orgs/{org}/widgets": noneRule("sneaked out")}
	got := strings.Join(ruleProblems(routes, rules, outside), "\n")
	for _, want := range []string{
		`outsideRules has "GET /api/v1/orgs/{org}/widgets" under /orgs/{org}: only the three unauthenticated webhooks`,
		`route GET /api/v1/orgs/{org}/gizmos has no team rule`,
		`route GET /api/v1/healthz2 is outside /orgs/{org} and has no entry in outsideRules`,
		`teamRules has "GET /gadgets", which the router does not serve`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems do not say %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "GET /api/v1/orgs/{org}/teams") {
		t.Errorf("a route with a rule was reported:\n%s", got)
	}
	bad := ruleProblems(nil, map[string]teamRule{
		"GET /x/{thing}": pathRule("thing", "other", "3"),
		"GET /y":         {Kind: ruleNone, Ticket: "8"},
		"GET /z":         {Kind: "maybe", Ticket: "8"},
	}, nil)
	got = strings.Join(bad, "\n")
	for _, want := range []string{"names {other}", "none rule has no Reason", `unknown rule kind "maybe"`} {
		if !strings.Contains(got, want) {
			t.Errorf("shape problems do not say %q; got:\n%s", want, got)
		}
	}
}

// accessFieldNames are request fields that would set a member's team access
// (#48, #26, #29, #31), under any spelling the API might use.
var accessFieldNames = map[string]bool{"teamAccess": true, "team_access": true, "access": true, "restricted": true}

// memberRequestRe: the operations about a membership; there a "teams" list
// also sets access (an invite, an agent, an approval, a member, the accept).
var memberRequestRe = regexp.MustCompile(`/(?:invites|agents|signups|users|members)(?:/|$)`)

// teamAccessWriters lists the operations of an OpenAPI document whose request
// (a body schema, by $ref and nested, or a parameter) has an access field, or
// a "teams" field on a membership operation. A response field is not a writer.
func teamAccessWriters(doc []byte) ([]string, error) {
	var root map[string]any
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	refs := func(ref string) any {
		var n any = root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := n.(map[string]any)
			if !ok {
				return nil
			}
			n = m[part]
		}
		return n
	}
	var has func(n any, member bool, seen map[string]bool) bool
	has = func(n any, member bool, seen map[string]bool) bool {
		switch v := n.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				if seen[ref] {
					return false
				}
				seen[ref] = true
				return has(refs(ref), member, seen)
			}
			if props, ok := v["properties"].(map[string]any); ok {
				for p := range props {
					if accessFieldNames[p] || (member && p == "teams") {
						return true
					}
				}
			}
			if name, _ := v["name"].(string); accessFieldNames[name] {
				if _, isParam := v["in"]; isParam {
					return true
				}
			}
			for k, c := range v {
				if k == "responses" || k == "example" || k == "examples" {
					continue
				}
				if has(c, member, seen) {
					return true
				}
			}
		case []any:
			for _, c := range v {
				if has(c, member, seen) {
					return true
				}
			}
		}
		return false
	}
	var out []string
	paths, _ := root["paths"].(map[string]any)
	for p, ops := range paths {
		opm, _ := ops.(map[string]any)
		for m, op := range opm {
			o, ok := op.(map[string]any)
			if !ok || !httpMethods[m] {
				continue
			}
			req := map[string]any{"requestBody": o["requestBody"], "parameters": o["parameters"], "shared": opm["parameters"]}
			if has(req, memberRequestRe.MatchString(p), map[string]bool{}) {
				out = append(out, strings.ToUpper(m)+" "+p)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

var (
	teamAccessColRe = regexp.MustCompile(`(?i)\bteam_access\b`)
	writeStmtRe     = regexp.MustCompile(`(?i)\b(?:insert|update)\b`)
	teamMembersIns  = regexp.MustCompile(`(?i)\binsert\s+into\s+(?:public\.)?"?team_members\b`)
	accessJSONTagRe = regexp.MustCompile("json:\"(?:teamAccess|team_access|restricted)[,\"]")
)

// sqlAccessWriters lists the non-test SQL (sqlc queries, which are the dbq
// methods, and Go literals) that writes org_members.team_access or inserts
// team_members rows.
func sqlAccessWriters(units []sqlUnit) []string {
	var out []string
	for _, u := range units {
		code, _ := sqlScan(u.Text)
		if (teamAccessColRe.MatchString(code) && writeStmtRe.MatchString(code)) || teamMembersIns.MatchString(code) {
			out = append(out, fmt.Sprintf("%s (%s)", u.Where, u.Name))
		}
	}
	sort.Strings(out)
	return out
}

// goAccessFields lists production Go struct fields with an access JSON name
// (a request body that could set team access).
//
// It also lists struct fields NAMED TeamAccess or Restricted with no JSON
// name: encoding/json matches "teamAccess" to such a field by its Go name.
func goAccessFields(gofiles map[string][]byte) []string {
	var out []string
	for where, b := range gofiles {
		for _, m := range accessJSONTagRe.FindAllIndex(b, -1) {
			out = append(out, fmt.Sprintf("%s:%d", where, 1+strings.Count(string(b[:m[0]]), "\n")))
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, where, b, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: parse: %v", where, err))
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fl, ok := n.(*ast.Field)
			if !ok {
				return true
			}
			jsonName := ""
			if fl.Tag != nil {
				tag, _ := strconv.Unquote(fl.Tag.Value)
				jsonName, _, _ = strings.Cut(reflect.StructTag(tag).Get("json"), ",")
			}
			for _, id := range fl.Names {
				if (id.Name == "TeamAccess" || id.Name == "Restricted") && jsonName == "" {
					out = append(out, fmt.Sprintf("%s:%d", where, fset.Position(id.Pos()).Line))
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// allRules is every rule, inside and outside /orgs/{org}.
func allRules() map[string]teamRule {
	out := map[string]teamRule{}
	for k, r := range teamRules {
		out[k] = r
	}
	for k, r := range outsideRules {
		out[k] = r
	}
	return out
}

// selectedWriterProblems is the guard of #19: while any rule is not
// Enforced, nothing may write team access.
func selectedWriterProblems(writers []string, rules map[string]teamRule) []string {
	if len(writers) == 0 {
		return nil
	}
	var open []string
	for k, r := range rules {
		if !r.Enforced {
			open = append(open, k)
		}
	}
	if len(open) == 0 {
		return nil
	}
	sort.Strings(open)
	return []string{fmt.Sprintf("%s can write team access while %d team rules are not Enforced (first: %s); "+
		"no restricted member may be made before every route is enforced (decisions-T20 #19)",
		strings.Join(writers, ", "), len(open), open[0])}
}

// TestNoSelectedWriterWhileUnenforced is the guard of decisions-T20 #19. It
// reads the spec, the SQL and the Go request structs, not a hand-set flag.
func TestNoSelectedWriterWhileUnenforced(t *testing.T) {
	writers, err := teamAccessWriters(openapi.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	src := loadScopeSources(t)
	writers = append(writers, sqlAccessWriters(src.units)...)
	writers = append(writers, goAccessFields(src.gofiles)...)
	for _, p := range selectedWriterProblems(writers, allRules()) {
		t.Error(p)
	}
}

// TestTeamRules_AllEnforced: since ticket 8 (decisions-T20) every rule, inside
// and outside /orgs/{org}, is Enforced, so Test B runs them all and the guard
// of #19 lets ticket 9 add the setter. A new route must be added Enforced,
// with its Test B samples (a path, body, list, filter, feed or own rule), or
// as admin or none with a reason.
func TestTeamRules_AllEnforced(t *testing.T) {
	keys := make([]string, 0)
	for k := range allRules() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if r := allRules()[k]; !r.Enforced {
			t.Errorf("rule %s (%s, ticket %s) is not Enforced", k, r.Kind, r.Ticket)
		}
	}
}

// TestNoSelectedWriterWhileUnenforced_Fails shows the guard fails on a spec
// with an access request field (in a body by $ref, as a parameter, and a
// "teams" list on a member route), on SQL that writes team_access or
// team_members, and on a Go struct with an access JSON field; and passes once
// every rule is Enforced.
func TestNoSelectedWriterWhileUnenforced_Fails(t *testing.T) {
	doc := []byte(`
openapi: 3.0.3
paths:
  /orgs/{org}/users/{user}/team-access:
    put:
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/TeamAccessInput'}
      responses: {'200': {description: ok}}
  /orgs/{org}/agents:
    post:
      parameters:
        - {name: restricted, in: query, schema: {type: boolean}}
      responses: {'201': {description: ok}}
  /orgs/{org}/invites:
    post:
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {email: {type: string}, teams: {type: array}}}
      responses: {'201': {description: ok}}
  /orgs/{org}/projects:
    post:
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {teams: {type: array}}}
      responses: {'201': {description: ok}}
  /orgs/{org}/users:
    get:
      responses:
        '200':
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Member'}
components:
  schemas:
    TeamAccessInput:
      type: object
      properties:
        access: {$ref: '#/components/schemas/Inner'}
    Inner:
      type: object
      properties:
        team_access: {type: string}
    Member:
      type: object
      properties:
        teamAccess: {type: string}
`)
	writers, err := teamAccessWriters(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /orgs/{org}/agents", "POST /orgs/{org}/invites", "PUT /orgs/{org}/users/{user}/team-access"}
	if !slices.Equal(writers, want) {
		t.Fatalf("writers = %v, want %v (a response field and a project's teams are not writers)", writers, want)
	}
	units := parseQueryFile("db/queries/x.sql", `-- name: SetAccess :exec
UPDATE org_members SET team_access = $3 WHERE org_id = $1 AND user_id = $2;

-- name: ReadAccess :one
SELECT team_access FROM org_members WHERE org_id = $1;

-- name: AddTeamRow :exec
INSERT INTO team_members (org_id, user_id, team_id) VALUES ($1, $2, $3);

-- name: Noted :exec
-- an UPDATE of team_access is not here
UPDATE org_members SET role = $2 WHERE org_id = $1;
`)
	if got := sqlAccessWriters(units); !slices.Equal(got, []string{"db/queries/x.sql (AddTeamRow)", "db/queries/x.sql (SetAccess)"}) {
		t.Fatalf("SQL writers = %v", got)
	}
	src := "package x\ntype In struct {\n\tAccess string `json:\"teamAccess,omitempty\"`\n\tTeamAccess string\n\tRestricted bool `db:\"r\"`\n\tTeams []string `json:\"teams\"`\n}\n" +
		"type Out struct {\n\tTeamAccess string `json:\"-\"`\n\tTeamAccess2 string\n}\n"
	if got := goAccessFields(map[string][]byte{"internal/x/x.go": []byte(src)}); !slices.Equal(got, []string{"internal/x/x.go:3", "internal/x/x.go:4", "internal/x/x.go:5"}) {
		t.Fatalf("Go fields = %v", got)
	}
	// A write split over two constant pieces is folded before the scan.
	gu, _, err := parseGoSource("internal/x/w.go", []byte("package x\nfunc w() {\n\t_ = \"UPDATE org_members SET \" +\n\t\t\"team_access = $3 WHERE org_id = $1\"\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sqlAccessWriters(gu); !slices.Equal(got, []string{"internal/x/w.go (w)"}) {
		t.Fatalf("folded SQL writers = %v", got)
	}
	open := map[string]teamRule{"GET /teams": listRule("team", "3"), "GET /": listRule("org row", "3")}
	if p := selectedWriterProblems(writers, open); len(p) != 1 || !strings.Contains(p[0], "2 team rules are not Enforced") {
		t.Fatalf("guard problems = %v", p)
	}
	done := map[string]teamRule{}
	for k, r := range open {
		r.Enforced = true
		done[k] = r
	}
	if p := selectedWriterProblems(writers, done); len(p) != 0 {
		t.Fatalf("every rule is enforced, but the guard says %v", p)
	}
}
