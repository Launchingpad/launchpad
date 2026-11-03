package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Test B of decisions-T20 #18 (the skeleton of ticket 2b): every Enforced rule
// is called by a restricted member with a sample that names a hidden object,
// and so is every reference the rule covers (Refs). Build tickets 3 to 8 add
// the samples when they flip a rule's Enforced flag; an Enforced rule, or one
// of its Refs, with no sample fails.

// teamFixture: team A (ORB) is visible to the restricted member, team B (HID)
// is hidden, and B holds one object of each kind the samples name.
type teamFixture struct {
	e *env
	// restricted sees team ORB only; member sees every team (team_access
	// 'all'); outsider is an admin of another org. Each token has every
	// scope, so a refusal comes from the role or the team, not the scope.
	restricted, member, outsider string
	// hidden maps a path parameter to the reference of an object in team B.
	hidden map[string]string
	// marks are strings of team B's objects that no list for the restricted
	// member may contain.
	marks []string
	// visible is an issue of team ORB, the target of the body-reference samples.
	visible string
	// hiddenState and hiddenLabel are a workflow state and a team label of HID.
	hiddenState, hiddenLabel string
	// child is an ORB issue whose parent is the hidden issue and whose project
	// and milestone are the hidden ones: the restricted member reads it with
	// parent, projectId and milestoneId null (#40, #42).
	child string
	// hiddenAssignee is a member assigned only to the hidden issue.
	hiddenAssignee string
	// plan holds the planning objects of ticket 5 (addPlanningFixture).
	plan *planningFixture
	// inbox holds the restricted member's notification rows and settings of
	// ticket 7 (addNotifyFixture).
	inbox *notifyFixture
	// restrictedID is the restricted member's user id; orbID and hidID are
	// the two teams.
	restrictedID, orbID, hidID uuid.UUID
}

// teamSample is one request that names a hidden object, and the same request
// with a made-up reference in its place.
type teamSample struct {
	// Kind is how the answer is judged: path (404), body (400), filter (200
	// or 400, never 5xx) or list (no mark of team B). Empty: the rule's kind.
	Kind              ruleKind
	Method            string
	Path, MissingPath string
	Body, MissingBody any
	HiddenRef, Made   string // the echoed references, replaced before the bodies are compared
	// Snapshot reads the row a refused write must leave as it was (#18, "the
	// hidden row unchanged"). Test B also checks that no event was written.
	Snapshot func() string
	// Check, when set, judges the hidden answer's body further (a count, a
	// redacted field) and returns a problem or "".
	Check func(body []byte) string
	// Also are more samples of the same rule or Ref, judged the same way.
	Also []teamSample
	// Status, when set, is the status a body sample's two answers must have
	// instead of 400: the answer an unknown reference gets today (#10).
	Status int
	// Stream: the route is an SSE stream; it is read from seq 0 up to a
	// marker event the caller can see (readStream).
	Stream bool
}

// teamRuleSamples: one per Enforced rule that is not none or admin, keyed as
// teamRules, and one per Ref, keyed "<rule key> <ref>". A sample may exist
// before its rule is Enforced; Test B runs it only once the flag is on, and
// TestTeamRules_SamplesRun runs every sample today to show it is well formed.
var teamRuleSamples = map[string]func(f *teamFixture) teamSample{
	"GET /teams/{team}": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/teams/" + f.hidden["team"]), MissingPath: f.orgPath("/teams/ZZZ"),
			HiddenRef: f.hidden["team"], Made: "ZZZ"}
	},
	// The fixture's default team is the hidden team: these find its key if
	// GetOrgFull or ListOrgsForUser names it.
	"GET /": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("")}
	},
	"GET /api/v1/orgs": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: "/api/v1/orgs"}
	},
	"GET /teams": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/teams")}
	},
	"GET /teams/{team}/states": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/teams/" + f.hidden["team"] + "/states"), MissingPath: f.orgPath("/teams/ZZZ/states"),
			HiddenRef: f.hidden["team"], Made: "ZZZ"}
	},
	// LAUN-244: a hidden team's git workflow is the 404 of a made-up team.
	"GET /teams/{team}/git-workflow": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/teams/" + f.hidden["team"] + "/git-workflow"), MissingPath: f.orgPath("/teams/ZZZ/git-workflow"),
			HiddenRef: f.hidden["team"], Made: "ZZZ"}
	},
	"GET /agent-guidance": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/agent-guidance")}
	},
	// An unknown ?team= on these two lists is 404 today; a hidden team is the
	// same 404, byte for byte.
	"GET /agent-guidance team": func(f *teamFixture) teamSample {
		s := f.filter("/agent-guidance?team=", f.hidden["team"], "ZZZ")
		s.Kind = rulePath
		return s
	},
	// The catalogue is org-wide; the count leaves out the hidden project.
	"GET /project-labels": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/project-labels"), Check: f.projectLabelCount("secret-pl", 0)}
	},
	"GET /labels": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/labels")}
	},
	"GET /labels team": func(f *teamFixture) teamSample {
		s := f.filter("/labels?team=", f.hidden["team"], "ZZZ")
		s.Kind = rulePath
		return s
	},
	"POST /labels": func(f *teamFixture) teamSample {
		return teamSample{Method: "POST", Path: f.orgPath("/labels"), MissingPath: f.orgPath("/labels"),
			Body: map[string]any{"name": "probe", "team": f.hidden["team"]}, MissingBody: map[string]any{"name": "probe", "team": "ZZZ"},
			HiddenRef: f.hidden["team"], Made: "ZZZ", Snapshot: f.labelRows}
	},
	// The list holds f.child: its parent, project and milestone are hidden,
	// so a mark in the list would also catch a redaction miss (#40, #42).
	"GET /issues": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/issues?limit=200")}
	},
	// project=none: an issue in a hidden project is in "no project" for the
	// caller, as its redacted projectId says (#40).
	"GET /issues project": func(f *teamFixture) teamSample {
		s := f.filter("/issues?project=", f.hidden["project"], uuid.NewString())
		s.Also = []teamSample{{Kind: ruleList, Method: "GET", Path: f.orgPath("/issues?project=none&limit=200"),
			Check: f.listHolds(f.child, func(is tracker.Issue) bool { return is.ProjectID == nil && is.MilestoneID == nil })}}
		return s
	},
	"GET /issues assignee": func(f *teamFixture) teamSample {
		return f.filter("/issues?assignee=", f.hiddenAssignee, "nobody")
	},
	"GET /issues label": func(f *teamFixture) teamSample {
		return f.filter("/issues?label=", "hidden-label", "made-label")
	},
	"GET /issues state": func(f *teamFixture) teamSample {
		return f.filter("/issues?state=", f.hiddenState, uuid.NewString())
	},
	// A ticket reference as the search text: the match-first path (LAUN-133).
	"GET /issues query": func(f *teamFixture) teamSample {
		return f.filter("/issues?query=", f.hidden["issue"], "HID-999")
	},
	"GET /issues team": func(f *teamFixture) teamSample {
		return f.filter("/issues?team=", f.hidden["team"], "ZZZ")
	},
	"GET /issues cycle": func(f *teamFixture) teamSample {
		return f.filter("/issues?cycle=", f.hidden["cycle"], uuid.NewString())
	},
	"GET /issues parent": func(f *teamFixture) teamSample {
		return f.filter("/issues?parent=", f.hidden["issue"], "HID-999")
	},
	"PATCH /issues/{issue}": func(f *teamFixture) teamSample {
		id := f.hidden["issue"]
		return teamSample{Method: "PATCH", Path: f.orgPath("/issues/" + id), MissingPath: f.orgPath("/issues/HID-999"),
			Body: map[string]any{"title": "changed"}, MissingBody: map[string]any{"title": "changed"},
			HiddenRef: id, Made: "HID-999", Snapshot: f.issueRow(id)}
	},
	// Ticket 4: the issue by path and its children.
	"GET /issues/{issue}": func(f *teamFixture) teamSample { return f.issuePath("GET", "", "", nil, nil) },
	// Every include of a hidden issue is the 404 of a missing one; the
	// includes of the visible issue (an edge to the hidden one) and of the
	// child (a hidden parent, project and milestone) hold nothing of team B.
	"GET /issues/{issue} include": func(f *teamFixture) teamSample {
		const inc = "?include=relations,attachments,stateHistory,comments,changeRequests"
		s := f.issuePath("GET", "", inc, nil, nil)
		s.Also = []teamSample{
			{Kind: ruleList, Method: "GET", Path: f.orgPath("/issues/" + f.visible + inc)},
			{Kind: ruleList, Method: "GET", Path: f.orgPath("/issues/" + f.child + inc)},
		}
		return s
	},
	// The child's activity names the hidden parent, project, milestone and a
	// removed edge to the hidden issue: each snapshot is null (#43).
	"GET /issues/{issue}/activity": func(f *teamFixture) teamSample {
		s := f.issuePath("GET", "/activity", "", nil, nil)
		s.Also = []teamSample{{Kind: ruleList, Method: "GET", Path: f.orgPath("/issues/" + f.child + "/activity"), Check: f.activityRedacted}}
		return s
	},
	"GET /issues/{issue}/comments": func(f *teamFixture) teamSample { return f.issuePath("GET", "/comments", "", nil, nil) },
	"POST /issues/{issue}/comments": func(f *teamFixture) teamSample {
		return f.issuePath("POST", "/comments", "", map[string]any{"body": "probe"}, f.rows("comments"))
	},
	// A reply to a hidden issue's comment, or to a hidden document's comment,
	// is "unknown parentId", as a made-up id (#23).
	"POST /issues/{issue}/comments parentId": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		path := f.orgPath("/issues/" + f.visible + "/comments")
		s := teamSample{Kind: ruleBody, Method: "POST", Path: path, MissingPath: path,
			Body: map[string]any{"body": "probe", "parentId": f.hidden["comment"]}, MissingBody: map[string]any{"body": "probe", "parentId": made},
			HiddenRef: f.hidden["comment"], Made: made, Snapshot: f.rows("comments")}
		doc := s
		doc.Body, doc.HiddenRef = map[string]any{"body": "probe", "parentId": f.hidden["docComment"]}, f.hidden["docComment"]
		s.Also = []teamSample{doc}
		return s
	},
	"PATCH /comments/{comment}": func(f *teamFixture) teamSample {
		return f.commentPath("PATCH", map[string]any{"body": "changed"})
	},
	"DELETE /comments/{comment}": func(f *teamFixture) teamSample { return f.commentPath("DELETE", nil) },
	"GET /issues/{issue}/attachments": func(f *teamFixture) teamSample {
		return f.issuePath("GET", "/attachments", "", nil, nil)
	},
	"POST /issues/{issue}/attachments": func(f *teamFixture) teamSample {
		return f.issuePath("POST", "/attachments", "", map[string]any{"kind": "link", "url": "https://example.com/probe"}, f.rows("attachments"))
	},
	"GET /attachments/{attachment}": func(f *teamFixture) teamSample { return f.attachmentPath("") },
	// A 404 here is the scoped lookup (the visible path is a 302).
	// TestTeamAccess_AttachmentDownload has the store spy.
	"GET /attachments/{attachment}/download": func(f *teamFixture) teamSample { return f.attachmentPath("/download") },
	"GET /issues/{issue}/relations": func(f *teamFixture) teamSample {
		s := f.issuePath("GET", "/relations", "", nil, nil)
		s.Also = []teamSample{{Kind: ruleList, Method: "GET", Path: f.orgPath("/issues/" + f.visible + "/relations")}}
		return s
	},
	"POST /issues/{issue}/relations": func(f *teamFixture) teamSample {
		return f.issuePath("POST", "/relations", "", map[string]any{"type": "related", "issue": f.child}, f.rows("issue_relations"))
	},
	"POST /issues/{issue}/relations issue": func(f *teamFixture) teamSample {
		path := f.orgPath("/issues/" + f.child + "/relations")
		return teamSample{Kind: ruleBody, Method: "POST", Path: path, MissingPath: path,
			Body: map[string]any{"type": "blocks", "issue": f.hidden["issue"]}, MissingBody: map[string]any{"type": "blocks", "issue": "HID-999"},
			HiddenRef: f.hidden["issue"], Made: "HID-999", Snapshot: f.rows("issue_relations")}
	},
	"DELETE /issues/{issue}/relations/{relation}": func(f *teamFixture) teamSample {
		return f.issuePath("DELETE", "/relations/"+f.hidden["relation"], "", nil, f.rows("issue_relations"))
	},
	// The edge of a visible issue whose other side is hidden: the 404 of an
	// unknown relation, and the edge stays (#41).
	"DELETE /issues/{issue}/relations/{relation} relation": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		base := "/issues/" + f.visible + "/relations/"
		return teamSample{Kind: rulePath, Method: "DELETE", Path: f.orgPath(base + f.hidden["relation"]), MissingPath: f.orgPath(base + made),
			HiddenRef: f.hidden["relation"], Made: made, Snapshot: f.rows("issue_relations")}
	},
	"PATCH /issues/{issue} parent":    func(f *teamFixture) teamSample { return f.bodyRef("parent", f.hidden["issue"], "HID-999") },
	"PATCH /issues/{issue} projectId": func(f *teamFixture) teamSample { return f.bodyRef("projectId", f.hidden["project"], uuid.NewString()) },
	"PATCH /issues/{issue} milestoneId": func(f *teamFixture) teamSample {
		return f.bodyRef("milestoneId", f.hidden["milestone"], uuid.NewString())
	},
	"PATCH /issues/{issue} cycleId": func(f *teamFixture) teamSample { return f.bodyRef("cycleId", f.hidden["cycle"], uuid.NewString()) },
	"PATCH /issues/{issue} state":   func(f *teamFixture) teamSample { return f.bodyRef("state", f.hiddenState, uuid.NewString()) },
	"PATCH /issues/{issue} labels": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.bodyRef("labels", f.hiddenLabel, made)
		s.Body, s.MissingBody = map[string]any{"labels": []string{f.hiddenLabel}}, map[string]any{"labels": []string{made}}
		return s
	},
	// Ticket 6a: the feed and its stream, from the first event on. The
	// fixture made every hidden object through its route, so the feed holds
	// their events; none may reach the restricted member.
	"GET /events": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/events?after=0&limit=500")}
	},
	"GET /events/stream": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/events/stream"), Stream: true}
	},
	// An unknown neighbour is 409 stale_placement today (rank.go); a hidden
	// one is the same 409.
	"PATCH /issues/{issue} placeAfter": func(f *teamFixture) teamSample {
		s := f.bodyRef("placeAfter", f.hidden["issue"], "HID-999")
		s.Status = http.StatusConflict
		return s
	},
	"PATCH /issues/{issue} placeBefore": func(f *teamFixture) teamSample {
		s := f.bodyRef("placeBefore", f.hidden["issue"], "HID-999")
		s.Status = http.StatusConflict
		return s
	},
}

// issuePath is a request on the hidden issue (suffix after the issue, query
// after that) and the same on HID-999, an issue that does not exist.
func (f *teamFixture) issuePath(method, suffix, query string, body any, snap func() string) teamSample {
	id := f.hidden["issue"]
	return teamSample{Kind: rulePath, Method: method, Path: f.orgPath("/issues/" + id + suffix + query),
		MissingPath: f.orgPath("/issues/HID-999" + suffix + query), Body: body, MissingBody: body,
		HiddenRef: id, Made: "HID-999", Snapshot: snap}
}

// commentPath is a write of the hidden issue's comment, and (Also) of the
// hidden document's comment, against a made-up comment id.
func (f *teamFixture) commentPath(method string, body any) teamSample {
	one := func(ref string) teamSample {
		made := uuid.NewString()
		return teamSample{Kind: rulePath, Method: method, Path: f.orgPath("/comments/" + ref), MissingPath: f.orgPath("/comments/" + made),
			Body: body, MissingBody: body, HiddenRef: ref, Made: made, Snapshot: f.rows("comments")}
	}
	s := one(f.hidden["comment"])
	s.Also = []teamSample{one(f.hidden["docComment"])}
	return s
}

// attachmentPath reads the hidden attachment (suffix "" or "/download").
func (f *teamFixture) attachmentPath(suffix string) teamSample {
	made := uuid.NewString()
	return teamSample{Kind: rulePath, Method: "GET", Path: f.orgPath("/attachments/" + f.hidden["attachment"] + suffix),
		MissingPath: f.orgPath("/attachments/" + made + suffix), HiddenRef: f.hidden["attachment"], Made: made}
}

// rows reads every row of an org table: a refused write changes none.
func (f *teamFixture) rows(table string) func() string {
	return func() string {
		var out string
		if err := f.e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY x.id), '')
			FROM `+table+` x WHERE x.org_id = $1`, f.e.orgID).Scan(&out); err != nil {
			f.e.t.Fatalf("rows of %s: %v", table, err)
		}
		return out
	}
}

// activityRedacted checks the child's activity: a parent, project,
// milestone and relationRemoved row are each listed with both sides null.
func (f *teamFixture) activityRedacted(body []byte) string {
	var page struct {
		Items []tracker.Activity `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return err.Error()
	}
	seen := map[string]bool{}
	for _, a := range page.Items {
		switch a.Kind {
		case tracker.ActParent, tracker.ActProject, tracker.ActMilestone, tracker.ActRelationRemove:
			seen[a.Kind] = true
			if string(a.From) != "null" || string(a.To) != "null" {
				return fmt.Sprintf("activity %s is not redacted: from %s to %s", a.Kind, a.From, a.To)
			}
		}
	}
	for _, k := range []string{tracker.ActParent, tracker.ActProject, tracker.ActMilestone, tracker.ActRelationRemove} {
		if !seen[k] {
			return fmt.Sprintf("the child's activity has no %s row: %s", k, body)
		}
	}
	return ""
}

// filter is a list request with a query parameter naming a hidden object.
func (f *teamFixture) filter(prefix, hidden, made string) teamSample {
	return teamSample{Kind: ruleFilter, Method: "GET", Path: f.orgPath(prefix + hidden), MissingPath: f.orgPath(prefix + made),
		HiddenRef: hidden, Made: made}
}

// bodyRef is a PATCH of a visible issue whose body field names a hidden
// object: 400 as for a made-up one, and the visible issue unchanged.
func (f *teamFixture) bodyRef(field, hidden, made string) teamSample {
	return teamSample{Kind: ruleBody, Method: "PATCH", Path: f.orgPath("/issues/" + f.visible), MissingPath: f.orgPath("/issues/" + f.visible),
		Body: map[string]any{field: hidden}, MissingBody: map[string]any{field: made},
		HiddenRef: hidden, Made: made, Snapshot: f.issueRow(f.visible)}
}

// issueRow reads every field of an issue a PATCH could change, and its labels.
func (f *teamFixture) issueRow(identifier string) func() string {
	return func() string {
		var out string
		err := f.e.pool.QueryRow(context.Background(), `SELECT row_to_json(i)::text || coalesce((SELECT string_agg(label_id::text, ',' ORDER BY label_id)
			FROM issue_labels l WHERE l.org_id = i.org_id AND l.issue_id = i.id), '')
			FROM issues i JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
			WHERE i.org_id = $1 AND t.key || '-' || i.number = $2`, f.e.orgID, identifier).Scan(&out)
		if err != nil {
			f.e.t.Fatalf("issue row %s: %v", identifier, err)
		}
		return out
	}
}

// listHolds checks that an issue list holds identifier, and that ok holds for it.
func (f *teamFixture) listHolds(identifier string, ok func(tracker.Issue) bool) func([]byte) string {
	return func(body []byte) string {
		var page struct {
			Items []tracker.Issue `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err.Error()
		}
		for _, is := range page.Items {
			if is.Identifier == identifier {
				if !ok(is) {
					return fmt.Sprintf("%s is listed but not as expected: %+v", identifier, is)
				}
				return ""
			}
		}
		return fmt.Sprintf("the list does not hold %s: %s", identifier, body)
	}
}

// projectLabelCount checks the projectCount of one project label.
func (f *teamFixture) projectLabelCount(name string, want int) func([]byte) string {
	return func(body []byte) string {
		var page struct {
			Items []tracker.ProjectLabel `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err.Error()
		}
		for _, l := range page.Items {
			if l.Name == name {
				if l.ProjectCount != want {
					return fmt.Sprintf("project label %s: projectCount %d, want %d", name, l.ProjectCount, want)
				}
				return ""
			}
		}
		return fmt.Sprintf("no project label %s: %s", name, body)
	}
}

// labelRows reads every label of the org: a refused POST /labels adds none.
func (f *teamFixture) labelRows() string {
	var out string
	if err := f.e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(l)::text, ',' ORDER BY l.created_at, l.name), '')
		FROM labels l WHERE l.org_id = $1`, f.e.orgID).Scan(&out); err != nil {
		f.e.t.Fatalf("label rows: %v", err)
	}
	return out
}

func (f *teamFixture) orgPath(p string) string { return "/api/v1/orgs/" + testdb.OrgSlug + p }

func newTeamFixture(t *testing.T) *teamFixture { return newTeamFixtureWith(t, envOpts{}) }

// newTeamFixtureWith builds the fixture on a server made with o (a store spy,
// for example). With no store the server gets the test store, so a download
// that passes the scope is a 302, not a 503.
func newTeamFixtureWith(t *testing.T, o envOpts) *teamFixture {
	t.Helper()
	if o.store == nil {
		o.store = storagetest.New(t)
	}
	e := newEnvWith(t, o)
	f := &teamFixture{e: e, hidden: map[string]string{}}
	ctx := context.Background()
	all := tokenOpts{scopes: []string{"read", "write", "admin"}}

	var hid tracker.Team
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/teams"), e.chen, map[string]any{"key": "HID", "name": "Hidden"})).json(t, &hid)
	issue := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden issue"})
	var project struct {
		ID uuid.UUID `json:"id"`
	}
	// A project label on the hidden project only: its projectCount is 0 for
	// the restricted member (the name is org-wide, decisions-T28).
	var plabel struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/project-labels"), e.chen, map[string]any{"name": "secret-pl"})).json(t, &plabel)
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), e.chen,
		map[string]any{"name": "Hidden plan", "teams": []string{"HID"}, "labelIds": []string{plabel.ID.String()}})).json(t, &project)
	var milestone, document, comment struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/milestones"), e.chen, map[string]any{"projectId": project.ID, "name": "Hidden step"})).json(t, &milestone)
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/documents"), e.chen, map[string]any{"projectId": project.ID, "title": "Hidden doc"})).json(t, &document)
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+issue.Identifier+"/comments"), e.chen, map[string]any{"body": "hidden comment"})).json(t, &comment)
	// Cycles are made by the cycle job only; the fixture inserts one.
	var cycle uuid.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO cycles (org_id, team_id, number, starts_at, ends_at)
		VALUES ($1, $2, 1, now(), now() + interval '7 days') RETURNING id`, e.orgID, hid.ID).Scan(&cycle); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM workflow_states WHERE org_id = $1 AND team_id = $2 ORDER BY position LIMIT 1`,
		e.orgID, hid.ID).Scan(&f.hiddenState); err != nil {
		t.Fatal(err)
	}
	label, err := e.q.InsertLabel(ctx, dbq.InsertLabelParams{OrgID: e.orgID, Name: "hidden-label", Color: "#112233", TeamID: &hid.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.hiddenLabel = label.ID.String()
	// The org's default team is the hidden one: GET / and GET /orgs name it
	// to everyone who sees it.
	testdb.MustExec(t, e.pool, `UPDATE orgs SET default_team_id = $2 WHERE id = $1`, e.orgID, hid.ID)
	// A member assigned only to the hidden issue, which carries the hidden label.
	testdb.AddMember(t, e.pool, e.orgID, "hank", "member")
	f.hiddenAssignee = "hank"
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+issue.Identifier), e.chen,
		map[string]any{"assignee": "hank", "labels": []string{label.ID.String()}}))
	// Guidance for the hidden team (T31): not listed to the restricted member.
	e.must(http.StatusOK, e.do("PUT", f.orgPath("/agent-guidance"), e.chen, map[string]any{"team": "HID", "text": "hidden guidance"}))
	f.visible = e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible issue"}).Identifier
	// A visible issue under the hidden parent, in the hidden project and
	// milestone (an issue may sit in a project that lists none of its team, #40).
	f.child = e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "orb child"}).Identifier
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.child), e.chen,
		map[string]any{"parent": issue.Identifier, "projectId": project.ID, "milestoneId": milestone.ID}))
	// Ticket 4: a comment on the hidden document; a file attachment of the
	// hidden issue (a row with no object: a download only signs a URL); an edge from the visible issue to the
	// hidden one; and an edge from the child to the hidden issue that the
	// admin removed, so the child's activity names the hidden issue (#43)
	// next to its parent, project and milestone rows.
	var docComment struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/documents/"+document.ID.String()+"/comments"), e.chen,
		map[string]any{"body": "hidden doc comment"})).json(t, &docComment)
	key, fname, ctype, size := "org/"+e.orgID.String()+"/2026/"+uuid.NewString()+"/hidden.txt", "hidden.txt", "text/plain", int64(5)
	att, err := e.q.InsertAttachment(ctx, dbq.InsertAttachmentParams{OrgID: e.orgID, IssueID: issue.ID, Kind: "file", Title: "hidden file",
		ObjectKey: &key, Filename: &fname, ContentType: &ctype, SizeBytes: &size})
	if err != nil {
		t.Fatal(err)
	}
	var rel, gone tracker.Relation
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+f.visible+"/relations"), e.chen,
		map[string]any{"type": "related", "issue": issue.Identifier})).json(t, &rel)
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/issues/"+f.child+"/relations"), e.chen,
		map[string]any{"type": "blocks", "issue": issue.Identifier})).json(t, &gone)
	e.must(http.StatusNoContent, e.do("DELETE", f.orgPath("/issues/"+f.child+"/relations/"+gone.ID.String()), e.chen, nil))
	f.hidden = map[string]string{
		"team": "HID", "issue": issue.Identifier, "project": project.ID.String(),
		"milestone": milestone.ID.String(), "document": document.ID.String(), "comment": comment.ID.String(),
		"cycle": cycle.String(), "attachment": att.ID.String(), "relation": rel.ID.String(), "docComment": docComment.ID.String(),
	}
	f.marks = []string{hid.ID.String(), "HID", issue.ID.String(), issue.Identifier, "hidden issue", project.ID.String(), "Hidden plan",
		milestone.ID.String(), document.ID.String(), comment.ID.String(), "hidden comment", cycle.String(), f.hiddenState,
		f.hiddenLabel, "hidden-label", "hidden guidance", "Hidden step",
		att.ID.String(), "hidden file", "hidden.txt", rel.ID.String(), docComment.ID.String(), "hidden doc comment"}

	orb, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: e.orgID, Key: testdb.TeamKey})
	if err != nil {
		t.Fatal(err)
	}
	rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, rid, orb.ID)
	f.restrictedID, f.orbID, f.hidID = rid, orb.ID, hid.ID
	f.restricted, _ = e.newToken(rid, all)
	mid := testdb.AddMember(t, e.pool, e.orgID, "mona", "member")
	f.member, _ = e.newToken(mid, all)

	other := e.newOrg("elsewhere", "ELS")
	oid := testdb.AddMember(t, e.pool, other, "otto", "admin")
	f.outsider, _ = e.newToken(oid, all)
	f.addPlanningFixture(t)
	f.addNotifyFixture(t)
	return f
}

// fillPath puts a hidden reference (or a made-up id) in every path parameter.
func (f *teamFixture) fillPath(pattern string) string {
	re := regexp.MustCompile(`\{(\w+)\}`)
	return f.orgPath(strings.TrimSuffix(re.ReplaceAllStringFunc(pattern, func(m string) string {
		if v, ok := f.hidden[m[1:len(m)-1]]; ok {
			return v
		}
		return uuid.NewString()
	}), "/"))
}

// refKeys are the sample keys of a rule's Refs.
func refKeys(key string, r teamRule) []string {
	var out []string
	for _, ref := range r.Refs {
		out = append(out, key+" "+ref)
	}
	return out
}

// sampleRule finds the rule of a sample key ("GET /issues", "GET /issues team"
// or an outsideRules key such as "GET /api/v1/orgs").
func sampleRule(sk string) (teamRule, bool) {
	rules := allRules()
	if r, ok := rules[sk]; ok {
		return r, true
	}
	i := strings.LastIndexByte(sk, ' ')
	r, ok := rules[sk[:i]]
	if !ok || !slices.Contains(r.Refs, sk[i+1:]) {
		return teamRule{}, false
	}
	return r, true
}

func ruleKeys() []string {
	keys := make([]string, 0, len(teamRules))
	for k := range teamRules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// methodBody is an empty JSON body for a method that takes one.
func methodBody(method string) any {
	if method == "POST" || method == "PUT" || method == "PATCH" {
		return map[string]any{}
	}
	return nil
}

// eventSeq is the org's newest event seq.
func (f *teamFixture) eventSeq() int64 {
	var n int64
	if err := f.e.pool.QueryRow(context.Background(), `SELECT coalesce(max(seq), 0) FROM events WHERE org_id = $1`, f.e.orgID).Scan(&n); err != nil {
		f.e.t.Fatal(err)
	}
	return n
}

// checkSample runs a sample by its kind (#18): a path or body reference to a
// hidden object answers as a made-up one, byte for byte once the echoed
// reference is replaced; a filter does the same with 200 or 400 (never 5xx);
// a list, feed or own route shows nothing of team B. A refused write changes
// neither the row (Snapshot) nor the change feed.
func (f *teamFixture) sampleProblem(key string, r teamRule, s teamSample) string {
	e := f.e
	kind := s.Kind
	if kind == "" {
		kind = r.Kind
	}
	var before string
	if s.Snapshot != nil {
		before = s.Snapshot()
	}
	seq := f.eventSeq()
	var got resp
	if s.Stream {
		got = f.readStream(f.restricted, s.Path)
	} else {
		got = e.do(s.Method, s.Path, f.restricted, s.Body)
	}
	switch kind {
	case rulePath, ruleBody, ruleFilter:
		want := e.do(s.Method, s.MissingPath, f.restricted, s.MissingBody)
		switch kind {
		case rulePath:
			if got.status != http.StatusNotFound || want.status != http.StatusNotFound {
				return fmt.Sprintf("%s: hidden = %d, made-up = %d, want 404; body %s", key, got.status, want.status, got.body)
			}
		case ruleBody:
			status := http.StatusBadRequest
			if s.Status != 0 {
				status = s.Status
			}
			if got.status != status || want.status != status {
				return fmt.Sprintf("%s: hidden = %d, made-up = %d, want %d; body %s", key, got.status, want.status, status, got.body)
			}
		case ruleFilter:
			if (want.status != http.StatusOK && want.status != http.StatusBadRequest) || got.status != want.status {
				return fmt.Sprintf("%s: hidden = %d, made-up = %d, want both 200 or both 400; body %s", key, got.status, want.status, got.body)
			}
		}
		gb := bytes.ReplaceAll(got.body, []byte(s.HiddenRef), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(s.Made), []byte("<ref>"))
		if !bytes.Equal(gb, wb) {
			return fmt.Sprintf("%s: a hidden object answers differently from a made-up one:\nhidden  %s\nmade-up %s", key, gb, wb)
		}
		for _, h := range []string{"Content-Type", "Location"} {
			if got.header.Get(h) != want.header.Get(h) {
				return fmt.Sprintf("%s: header %s differs: %q vs %q", key, h, got.header.Get(h), want.header.Get(h))
			}
		}
	case ruleList, ruleFeed, ruleOwn:
		if got.status != http.StatusOK {
			return fmt.Sprintf("%s: status %d; body %s", key, got.status, got.body)
		}
		for _, m := range f.marks {
			if bytes.Contains(got.body, []byte(m)) {
				return fmt.Sprintf("%s: the restricted member's answer holds %q of the hidden team: %s", key, m, got.body)
			}
		}
	default:
		return fmt.Sprintf("%s: a %s rule has no sample check", key, kind)
	}
	if s.Snapshot != nil {
		if after := s.Snapshot(); after != before {
			return fmt.Sprintf("%s: a refused write changed the row:\nbefore %s\nafter  %s", key, before, after)
		}
	}
	if s.Method != "GET" {
		if n := f.eventSeq(); n != seq {
			return fmt.Sprintf("%s: a refused write emitted %d events", key, n-seq)
		}
	}
	if s.Check != nil {
		if p := s.Check(got.body); p != "" {
			return key + ": " + p
		}
	}
	for i, a := range s.Also {
		if p := f.sampleProblem(fmt.Sprintf("%s (also %d)", key, i+1), r, a); p != "" {
			return p
		}
	}
	return ""
}

// readStream reads an event stream from seq 0. It first makes a marker
// (an issue of the visible team, by the admin), so every caller of the
// fixture can see the newest event; it returns everything the stream wrote up
// to the end of that event. A stream that does not reach it in 15 s fails.
// The inbox stream (ticket 7) is read up to the end of its first count
// event, which ends its first drain.
func (f *teamFixture) readStream(token, path string) resp {
	e := f.e
	e.t.Helper()
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "stream marker"})
	want := fmt.Sprintf("id: %d\n", f.eventSeq())
	if strings.Contains(path, "/notifications/stream") {
		want = "event: count\n"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.authorize(req, token)
	req.Header.Set("Last-Event-ID", "0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return resp{status: res.StatusCode, body: b, header: res.Header}
	}
	var buf bytes.Buffer
	br := bufio.NewReader(res.Body)
	seen := false
	for {
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		if err != nil {
			e.t.Fatalf("stream %s ended before the marker (%v); read:\n%s", path, err, buf.String())
		}
		if line == want {
			seen = true
		}
		if seen && line == "\n" {
			return resp{status: res.StatusCode, body: buf.Bytes(), header: res.Header}
		}
	}
}

// checkSample fails the test on the sample's problem.
func (f *teamFixture) checkSample(t *testing.T, key string, r teamRule, s teamSample) {
	t.Helper()
	if p := f.sampleProblem(key, r, s); p != "" {
		t.Fatal(p)
	}
}

// TestTeamRules_RestrictedMember is Test B. An admin rule answers 403 to a
// restricted member today (the org role decides, #2), so its case runs whatever
// the flag says; every other Enforced rule runs its sample.
func TestTeamRules_RestrictedMember(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	var enforced int
	rules := allRules()
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := rules[key]
		method, pattern, _ := strings.Cut(key, " ")
		switch {
		case r.Kind == ruleAdmin && teamRules[key].Kind == ruleAdmin:
			res := e.do(method, f.fillPath(pattern), f.restricted, methodBody(method))
			if res.status != http.StatusForbidden || res.errCode(t) != "forbidden" {
				t.Errorf("admin rule %s: a restricted member got %d %s, want 403 forbidden", key, res.status, res.body)
			}
		case r.Kind == ruleNone || !r.Enforced:
		default:
			enforced++
			for _, sk := range append([]string{key}, refKeys(key, r)...) {
				sample, ok := teamRuleSamples[sk]
				if !ok {
					t.Errorf("rule %s is Enforced but has no Test B sample %q in teamRuleSamples", key, sk)
					continue
				}
				t.Run(sk, func(t *testing.T) { f.checkSample(t, sk, r, sample(f)) })
			}
		}
	}
	for k := range teamRuleSamples {
		if _, ok := sampleRule(k); !ok {
			t.Errorf("teamRuleSamples has %q, which names no rule or Ref in teamRules or outsideRules", k)
		}
	}
	t.Logf("%d of %d rules are Enforced", enforced, len(rules))
}

// TestTeamRules_Harness shows the harness works: the sample of GET
// /teams/{team} passes and an all-teams member and an admin still read the
// team; the body-ref samples of PATCH /issues (flipped in ticket 5) pass
// already; and the list marks are real.
func TestTeamRules_Harness(t *testing.T) {
	f := newTeamFixture(t)
	key := "GET /teams/{team}"
	s := teamRuleSamples[key](f)
	f.checkSample(t, key, teamRules[key], s)
	for who, tok := range map[string]string{"member": f.member, "admin": f.e.chen} {
		if res := f.e.do("GET", s.Path, tok, nil); res.status != http.StatusOK {
			t.Errorf("%s reads the team: %d %s", who, res.status, res.body)
		}
	}
	// The write and filter paths of the harness: these refs are scoped since 2a
	// (checkPlanningRefs, resolveIssueID), so they pass before their flags.
	for _, k := range []string{"PATCH /issues/{issue} projectId", "PATCH /issues/{issue} parent", "GET /issues parent", "GET /issues cycle"} {
		r, _ := sampleRule(k)
		t.Run(k, func(t *testing.T) { f.checkSample(t, k, r, teamRuleSamples[k](f)) })
	}
	// The marks are real: every list sample of the restricted member, read
	// by the all-teams member, holds team B (so the mark check can fail).
	for _, k := range []string{"GET /", "GET /api/v1/orgs", "GET /teams", "GET /agent-guidance", "GET /labels", "GET /issues"} {
		s := teamRuleSamples[k](f)
		res := f.e.do("GET", s.Path, f.member, nil)
		if res.status != http.StatusOK || !bytes.Contains(res.body, []byte("HID")) {
			t.Errorf("%s: the all-teams member does not see team B: %d %s", k, res.status, res.body)
		}
	}
	// Ticket 4: the all-teams member reads team B in the includes, the
	// relations and the activity the restricted member reads without it.
	for _, k := range []string{"GET /issues/{issue} include", "GET /issues/{issue}/activity", "GET /issues/{issue}/relations"} {
		a := teamRuleSamples[k](f).Also[0]
		res := f.e.do("GET", a.Path, f.member, nil)
		if res.status != http.StatusOK || !bytes.Contains(res.body, []byte(f.hidden["issue"])) {
			t.Errorf("%s: the all-teams member does not see the hidden issue: %d %s", k, res.status, res.body)
		}
		if a.Check != nil && a.Check(res.body) == "" {
			t.Errorf("%s: the Check passes for the all-teams member, so it cannot fail", k)
		}
	}
	// The Check functions are real: for the all-teams member the project
	// label counts the hidden project, and project=none leaves the child out.
	pl := teamRuleSamples["GET /project-labels"](f)
	if p := pl.Check(f.e.do("GET", pl.Path, f.member, nil).body); !strings.Contains(p, "projectCount 1") {
		t.Errorf("GET /project-labels as the all-teams member: %q, want a count of 1", p)
	}
	none := teamRuleSamples["GET /issues project"](f).Also[0]
	if p := none.Check(f.e.do("GET", none.Path, f.member, nil).body); !strings.Contains(p, "does not hold") {
		t.Errorf("project=none as the all-teams member: %q, want the child left out", p)
	}
}

// TestTeamRules_CrossOrg: every route under /orgs/{org} answers 404 not_found
// to an admin of another org, whatever the rule (the cross-org rule of
// AGENTS.md, over the whole rule table).
func TestTeamRules_CrossOrg(t *testing.T) {
	f := newTeamFixture(t)
	for _, key := range ruleKeys() {
		method, pattern, _ := strings.Cut(key, " ")
		res := f.e.do(method, f.fillPath(pattern), f.outsider, methodBody(method))
		if res.status != http.StatusNotFound || res.errCode(t) != "not_found" {
			t.Errorf("%s: an admin of another org got %d %s, want 404 not_found", key, res.status, res.body)
		}
	}
}

// TestTeamRules_SamplesRun runs every sample today, before its rule is
// Enforced, to show it is well formed: as the admin and as the restricted
// member, no answer is a 5xx, and each sample key names a rule or a Ref.
func TestTeamRules_SamplesRun(t *testing.T) {
	f := newTeamFixture(t)
	keys := make([]string, 0, len(teamRuleSamples))
	for k := range teamRuleSamples {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := sampleRule(k); !ok {
			t.Errorf("sample %q names no rule or Ref", k)
		}
		s := teamRuleSamples[k](f)
		for who, tok := range map[string]string{"admin": f.e.chen, "restricted": f.restricted} {
			for _, req := range []struct {
				path string
				body any
			}{{s.Path, s.Body}, {s.MissingPath, s.MissingBody}} {
				if req.path == "" {
					continue
				}
				if s.Stream {
					if res := f.readStream(tok, req.path); res.status != http.StatusOK {
						t.Errorf("%s as %s: stream %s answered %d %s", k, who, req.path, res.status, res.body)
					}
					continue
				}
				if res := f.e.do(s.Method, req.path, tok, req.body); res.status >= 500 {
					t.Errorf("%s as %s: %s %s answered %d %s", k, who, s.Method, req.path, res.status, res.body)
				}
			}
		}
	}
}
