package api

import (
	"sort"
	"strings"
	"testing"

	"devops/internal/tracker"
)

// LAUN-54: issue counts on GET /labels and team labels
// (docs/internal/decisions-T21-team-labels.md).

func (e *env) labelList(token, query string) map[string]tracker.Label {
	e.t.Helper()
	var p page[tracker.Label]
	e.must(200, e.do("GET", O+"/labels"+query, token, nil)).json(e.t, &p)
	out := map[string]tracker.Label{}
	for _, l := range p.Items {
		k := l.Name
		if l.TeamKey != nil {
			k += "@" + *l.TeamKey
		}
		if _, dup := out[k]; dup {
			e.t.Fatalf("label %s listed twice", k)
		}
		out[k] = l
	}
	return out
}

func (e *env) mkLabel(token string, body map[string]any) tracker.Label {
	e.t.Helper()
	var l tracker.Label
	e.must(201, e.do("POST", O+"/labels", token, body)).json(e.t, &l)
	return l
}

func TestLabels_IssueCounts(t *testing.T) {
	e := newEnv(t)
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	flaky := e.mkLabel(e.chen, map[string]any{"name": "Flaky", "team": "ORB"})
	e.mkLabel(e.chen, map[string]any{"name": "Pixels", "team": "web"})
	if flaky.TeamKey == nil || *flaky.TeamKey != "ORB" || flaky.TeamID == nil || flaky.IssueCount != 0 {
		t.Fatalf("created team label: %+v", flaky)
	}

	e.createIssue(e.chen, map[string]any{"title": "a", "labels": []string{"Bug", "Flaky"}})                 // ORB-1
	e.createIssue(e.chen, map[string]any{"title": "b", "labels": []string{"Bug"}, "state": "Done"})         // ORB-2: closed counts
	e.createIssue(e.chen, map[string]any{"title": "c", "labels": []string{"Bug", "Flaky"}})                 // ORB-3: archived below
	e.createIssue(e.chen, map[string]any{"team": "WEB", "title": "d", "labels": []string{"Bug", "Pixels"}}) // WEB-1
	e.exec(`UPDATE issues SET archived_at = now() WHERE org_id = $1 AND number = 3
	        AND team_id = (SELECT id FROM teams WHERE org_id = $1 AND key = 'ORB')`, e.orgID)

	count := func(m map[string]tracker.Label, k string) int32 {
		l, ok := m[k]
		if !ok {
			t.Fatalf("label %s not listed: %v", k, m)
		}
		return l.IssueCount
	}
	all := e.labelList(e.chen, "")
	if count(all, "Bug") != 3 || count(all, "Flaky@ORB") != 1 || count(all, "Pixels@WEB") != 1 ||
		count(all, "Feature") != 0 || len(all) != 5 {
		t.Fatalf("all labels: %+v", all)
	}
	if l := all["Bug"]; l.TeamID != nil || l.TeamKey != nil {
		t.Fatalf("an org label has a team: %+v", l)
	}
	orb := e.labelList(e.chen, "?team=ORB")
	if count(orb, "Bug") != 2 || count(orb, "Flaky@ORB") != 1 || len(orb) != 4 {
		t.Fatalf("?team=ORB: %+v", orb)
	}
	if _, ok := orb["Pixels@WEB"]; ok {
		t.Fatal("?team=ORB lists another team's label")
	}
	webByID := e.labelList(e.chen, "?team=WEB")
	if count(webByID, "Bug") != 1 || count(webByID, "Pixels@WEB") != 1 || count(webByID, "Feature") != 0 {
		t.Fatalf("?team=WEB: %+v", webByID)
	}
	byID := e.labelList(e.chen, "?team="+flaky.TeamID.String())
	if count(byID, "Flaky@ORB") != 1 {
		t.Fatalf("?team=<id>: %+v", byID)
	}
	expectCode(t, e.do("GET", O+"/labels?team=NOPE", e.chen, nil), 404, "not_found")

	// An archived label is not listed.
	e.exec(`UPDATE labels SET archived_at = now() WHERE org_id = $1 AND name = 'Pixels'`, e.orgID)
	if _, ok := e.labelList(e.chen, "")["Pixels@WEB"]; ok {
		t.Fatal("an archived label is listed")
	}
}

func TestLabels_TeamScopeAndNames(t *testing.T) {
	e := newEnv(t)
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	before := e.eventCount("label.created")

	// A team label may share a name with an org label and with another
	// team's label; inside one scope the name is unique ignoring case.
	webBug := e.mkLabel(e.chen, map[string]any{"name": "bug", "team": "WEB", "color": "#112233"})
	orbBug := e.mkLabel(e.chen, map[string]any{"name": "BUG", "team": "ORB"})
	expectCode(t, e.do("POST", O+"/labels", e.chen, map[string]any{"name": "Bug", "team": "WEB"}), 409, "conflict")
	expectCode(t, e.do("POST", O+"/labels", e.chen, map[string]any{"name": "bug"}), 409, "conflict")
	expectCode(t, e.do("POST", O+"/labels", e.chen, map[string]any{"name": "x", "team": "NOPE"}), 400, "validation_failed")
	if n := e.eventCount("label.created") - before; n != 2 {
		t.Fatalf("%d label.created events, want 2 (a refused write emits none)", n)
	}

	// By name, an issue of a team gets the team's label over the org's.
	w := e.createIssue(e.chen, map[string]any{"team": "WEB", "title": "w", "labels": []string{"bug"}})
	if len(w.Labels) != 1 || w.Labels[0].ID != webBug.ID {
		t.Fatalf("WEB issue labels: %+v, want the WEB label", w.Labels)
	}
	o := e.createIssue(e.chen, map[string]any{"title": "o", "labels": []string{"bug"}})
	if len(o.Labels) != 1 || o.Labels[0].ID != orbBug.ID {
		t.Fatalf("ORB issue labels: %+v, want the ORB label", o.Labels)
	}
	// A team with no label of that name gets the org label.
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "OPS", "name": "Ops"}))
	p := e.createIssue(e.chen, map[string]any{"team": "OPS", "title": "p", "labels": []string{"bug"}})
	if len(p.Labels) != 1 || p.Labels[0].ID == webBug.ID || p.Labels[0].ID == orbBug.ID {
		t.Fatalf("OPS issue labels: %+v, want the org label", p.Labels)
	}

	// Another team's label is refused, by id and by name.
	webOnly := e.mkLabel(e.chen, map[string]any{"name": "webonly", "team": "WEB"})
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"title": "x", "labels": []string{webOnly.ID.String()}}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"title": "x", "labels": []string{"webonly"}}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", O+"/issues/ORB-1", e.chen, map[string]any{"addLabels": []string{webOnly.ID.String()}}), 400, "validation_failed")
	e.must(200, e.do("PATCH", O+"/issues/"+w.Identifier, e.chen, map[string]any{"addLabels": []string{webOnly.ID.String()}}))

	// A write-scope agent may make a team label, as it may make an org label.
	e.must(201, e.do("POST", O+"/labels", e.claude, map[string]any{"name": "agentlabel", "team": "ORB"}))
}

func TestLabels_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acme := e.newOrg("acme", "ACM")
	e.addMember(acme, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	const A = "/api/v1/orgs/acme"

	orbLabel := e.mkLabel(tok, map[string]any{"name": "Flaky", "team": "ORB"})
	// acme neither lists it nor can it name orbit's team.
	var p page[tracker.Label]
	e.must(200, e.do("GET", A+"/labels", tok, nil)).json(t, &p)
	if len(p.Items) != 0 {
		t.Fatalf("acme sees labels %+v", p.Items)
	}
	e.must(404, e.do("GET", A+"/labels?team=ORB", tok, nil))
	e.must(400, e.do("POST", A+"/labels", tok, map[string]any{"name": "x", "team": "ORB"}))
	e.must(400, e.do("POST", A+"/labels", tok, map[string]any{"name": "x", "team": orbLabel.TeamID.String()}))
	// An acme issue cannot carry orbit's team label.
	e.must(400, e.do("POST", A+"/issues", tok, map[string]any{"team": "ACM", "title": "t", "labels": []string{orbLabel.ID.String()}}))
	// And acme's counts do not see orbit's issues.
	e.createIssue(tok, map[string]any{"title": "o", "labels": []string{"Flaky"}})
	acmeLabel := func() tracker.Label {
		var l tracker.Label
		e.must(201, e.do("POST", A+"/labels", tok, map[string]any{"name": "Flaky", "team": "ACM"})).json(t, &l)
		return l
	}()
	e.must(200, e.do("GET", A+"/labels", tok, nil)).json(t, &p)
	if len(p.Items) != 1 || p.Items[0].ID != acmeLabel.ID || p.Items[0].IssueCount != 0 {
		t.Fatalf("acme labels %+v", p.Items)
	}
}

// removeLabels by name removes the label the issue carries, in either scope.
func TestLabels_RemoveByNameUsesCarriedLabel(t *testing.T) {
	e := newEnv(t)
	i := e.createIssue(e.chen, map[string]any{"title": "r", "labels": []string{"Bug"}}) // the org label
	e.mkLabel(e.chen, map[string]any{"name": "bug", "team": "ORB"})
	var got tracker.Issue
	e.must(200, e.do("PATCH", O+"/issues/"+i.Identifier, e.chen, map[string]any{"removeLabels": []string{"bug"}})).json(t, &got)
	if len(got.Labels) != 0 {
		t.Fatalf("labels after removing by name: %+v", got.Labels)
	}
}

// LAUN-101: ?label= matches by name across scopes; team= narrows it to one team's issues.
func TestLabels_FilterCombinesWithTeam(t *testing.T) {
	e := newEnv(t)
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	e.mkLabel(e.chen, map[string]any{"name": "Flaky", "team": "ORB"})
	e.createIssue(e.chen, map[string]any{"title": "o1", "labels": []string{"Bug"}})
	e.createIssue(e.chen, map[string]any{"title": "o2", "labels": []string{"Flaky"}})
	e.createIssue(e.chen, map[string]any{"title": "o3"})
	e.createIssue(e.chen, map[string]any{"team": "WEB", "title": "w1", "labels": []string{"Bug"}})
	titles := func(q string) []string {
		var p page[tracker.Issue]
		e.must(200, e.do("GET", O+"/issues?"+q, e.chen, nil)).json(t, &p)
		var out []string
		for _, i := range p.Items {
			out = append(out, i.Title)
		}
		sort.Strings(out)
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	eq(titles("label=Bug"), "o1", "w1")
	eq(titles("label=Bug&team=ORB"), "o1")
	eq(titles("label=Bug&team=WEB"), "w1")
	eq(titles("label=Flaky&team=WEB"))
	eq(titles("label=Flaky,Bug&team=ORB"), "o1", "o2")
}
