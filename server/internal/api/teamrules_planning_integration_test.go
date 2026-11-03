package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 5 of decisions-T20 (LAUN-220): planning objects for a restricted
// member. Projects, milestones, documents and their comments, cycles and their
// snapshots, project updates and avatars, products, and the planning refs of
// POST and PATCH /issues. The samples live here and join teamRuleSamples in
// init, so the shared table of Test B keeps small hunks.

// planningFixture holds the objects ticket 5 adds to the Test B fixture.
type planningFixture struct {
	// shared is a project of ORB and HID: the restricted member sees it with
	// team ORB only (#37). sharedHiddenIssue is an issue of HID in it, so
	// its issue counts would show a hidden issue.
	shared            uuid.UUID
	sharedHiddenIssue string
	// openDoc is a document with no project: org-level, visible (#7, #38).
	openDoc uuid.UUID
	// hiddenDocSlug is the slug of the hidden project's document.
	hiddenDocSlug string
	// hiddenDocComment is a comment on the hidden document.
	hiddenDocComment uuid.UUID
	// mixed is a product of ORB and HID; hiddenProductSlug is the slug of
	// the product of HID only.
	mixed             uuid.UUID
	hiddenProductSlug string
	// cycleLinear is a Linear id (import_mappings) of the hidden cycle.
	cycleLinear string
}

// addPlanningFixture adds ticket 5's objects to the Test B fixture.
func (f *teamFixture) addPlanningFixture(t *testing.T) {
	t.Helper()
	e := f.e
	pf := &planningFixture{}
	var shared struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), e.chen,
		map[string]any{"name": "Shared plan", "teams": []string{testdb.TeamKey, "HID"}})).json(t, &shared)
	pf.shared = shared.ID
	pf.sharedHiddenIssue = e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden shared work", "projectId": shared.ID}).Identifier

	var doc, hiddenDoc, dc, update struct {
		ID   uuid.UUID `json:"id"`
		Slug string    `json:"slug"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/documents"), e.chen, map[string]any{"title": "Open doc"})).json(t, &doc)
	pf.openDoc = doc.ID
	e.must(http.StatusOK, e.do("GET", f.orgPath("/documents/"+f.hidden["document"]), e.chen, nil)).json(t, &hiddenDoc)
	pf.hiddenDocSlug = hiddenDoc.Slug
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/documents/"+f.hidden["document"]+"/comments"), e.chen,
		map[string]any{"body": "hidden doc talk"})).json(t, &dc)
	pf.hiddenDocComment = dc.ID
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects/"+f.hidden["project"]+"/updates"), e.chen,
		map[string]any{"health": "at_risk", "body": "hidden update body"})).json(t, &update)

	var hp, mixed struct {
		ID   uuid.UUID `json:"id"`
		Slug string    `json:"slug"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/products"), e.chen,
		map[string]any{"name": "Secret product", "slug": "secret-product", "teams": []string{"HID"}})).json(t, &hp)
	pf.hiddenProductSlug = hp.Slug
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/products"), e.chen,
		map[string]any{"name": "Mixed", "slug": "mixed", "teams": []string{testdb.TeamKey, "HID"}})).json(t, &mixed)
	pf.mixed = mixed.ID

	pf.cycleLinear = uuid.NewString()
	testdb.MustExec(t, e.pool, `INSERT INTO import_mappings (org_id, entity_type, source_id, target_id) VALUES ($1, 'cycle', $2, $3)`,
		e.orgID, pf.cycleLinear, f.hidden["cycle"])

	f.hidden["update"] = update.ID.String()
	f.hidden["product"] = hp.ID.String()
	f.marks = append(f.marks, "Hidden step", "Hidden doc", pf.hiddenDocSlug, dc.ID.String(), "hidden doc talk",
		update.ID.String(), "hidden update body", hp.ID.String(), "Secret product", "secret-product",
		pf.sharedHiddenIssue, "hidden shared work")
	f.plan = pf
}

// rowJSON reads one row of a table as JSON: a refused write leaves it as it was.
func (f *teamFixture) rowJSON(table, id string) func() string {
	return func() string {
		var out string
		if err := f.e.pool.QueryRow(context.Background(), fmt.Sprintf(`SELECT row_to_json(x)::text FROM %s x WHERE x.org_id = $1 AND x.id = $2`, table),
			f.e.orgID, id).Scan(&out); err != nil {
			f.e.t.Fatalf("%s row %s: %v", table, id, err)
		}
		return out
	}
}

// countOf counts rows: a refused create adds none, a refused read writes none.
func (f *teamFixture) countOf(sql string, args ...any) func() string {
	return func() string {
		var n int
		if err := f.e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
			f.e.t.Fatalf("count: %v", err)
		}
		return fmt.Sprint(n)
	}
}

// projectTeams is a project row and its team rows.
func (f *teamFixture) projectTeams(id string) func() string {
	row := f.rowJSON("projects", id)
	return func() string {
		return row() + f.countOf(`SELECT count(*) FROM project_teams WHERE project_id = $1`, id)() +
			f.countOf(`SELECT count(*) FROM project_teams pt JOIN teams t ON t.id = pt.team_id WHERE pt.project_id = $1 AND t.key = 'HID'`, id)()
	}
}

// path is a request on a hidden object's path and the same on a made-up one.
func (f *teamFixture) path(method, hiddenPath, madePath, hiddenRef, made string, body any, snap func() string) teamSample {
	return teamSample{Kind: rulePath, Method: method, Path: f.orgPath(hiddenPath), MissingPath: f.orgPath(madePath),
		Body: body, MissingBody: body, HiddenRef: hiddenRef, Made: made, Snapshot: snap}
}

// body is a write whose body field names a hidden object.
func (f *teamFixture) body(method, p string, base map[string]any, field string, hidden, made any, hiddenRef, madeRef string, snap func() string) teamSample {
	hb, mb := map[string]any{}, map[string]any{}
	for k, v := range base {
		hb[k], mb[k] = v, v
	}
	hb[field], mb[field] = hidden, made
	return teamSample{Kind: ruleBody, Method: method, Path: f.orgPath(p), MissingPath: f.orgPath(p),
		Body: hb, MissingBody: mb, HiddenRef: hiddenRef, Made: madeRef, Snapshot: snap}
}

// issueCount is the number of issues of the org: a refused POST /issues adds none.
func (f *teamFixture) issueCount() string {
	return f.countOf(`SELECT count(*) FROM issues WHERE org_id = $1`, f.e.orgID)()
}

// newIssue is a POST /issues sample whose body field names a hidden object.
func (f *teamFixture) newIssue(field string, hidden, made any, hiddenRef, madeRef string) teamSample {
	return f.body("POST", "/issues", map[string]any{"team": testdb.TeamKey, "title": "probe"}, field, hidden, made, hiddenRef, madeRef, f.issueCount)
}

// listCheck judges a list body with ok.
func listCheck[T any](ok func([]T) string) func([]byte) string {
	return func(body []byte) string {
		var page struct {
			Items []T `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err.Error()
		}
		return ok(page.Items)
	}
}

// sharedSeen checks the shared project in a project list: its teams are ORB
// only, and its issue counts hold no hidden issue (#37).
func (f *teamFixture) sharedSeen(ps []tracker.Project) string {
	for _, p := range ps {
		if p.ID == f.plan.shared {
			if len(p.Teams) != 1 || p.Teams[0].Key != testdb.TeamKey {
				return fmt.Sprintf("shared project teams = %+v, want ORB only", p.Teams)
			}
			if p.IssueCounts.Total != 0 {
				return fmt.Sprintf("shared project issueCounts = %+v, want no hidden issue counted", p.IssueCounts)
			}
			return ""
		}
	}
	return "the shared project is not listed"
}

// mixedSeen checks the mixed product in a product list: its teamKeys are ORB only (#69).
func (f *teamFixture) mixedSeen(body []byte) string {
	return listCheck(func(ps []tracker.Product) string {
		for _, p := range ps {
			if p.ID == f.plan.mixed {
				if len(p.TeamKeys) != 1 || p.TeamKeys[0] != testdb.TeamKey {
					return fmt.Sprintf("mixed product teamKeys = %v, want ORB only", p.TeamKeys)
				}
				return ""
			}
		}
		return "the mixed product is not listed"
	})(body)
}

// childPlanning is a PATCH of f.child (its project and milestone are hidden)
// with two planning fields: the hidden current id in one field answers as a
// made-up id, byte for byte, and checkPlanningRefs keeps its order (#40).
func (f *teamFixture) childPlanning(hidden, made map[string]any, hiddenRef, madeRef string) teamSample {
	return teamSample{Kind: ruleBody, Method: "PATCH", Path: f.orgPath("/issues/" + f.child), MissingPath: f.orgPath("/issues/" + f.child),
		Body: hidden, MissingBody: made, HiddenRef: hiddenRef, Made: madeRef, Snapshot: f.issueRow(f.child)}
}

var planningSamples = map[string]func(f *teamFixture) teamSample{
	// --- PATCH /issues/{issue}: label adds and removals, and ?product= ---
	"PATCH /issues/{issue} addLabels": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.bodyRef("addLabels", f.hiddenLabel, made)
		s.Body, s.MissingBody = map[string]any{"addLabels": []string{f.hiddenLabel}}, map[string]any{"addLabels": []string{made}}
		return s
	},
	"PATCH /issues/{issue} removeLabels": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.bodyRef("removeLabels", f.hiddenLabel, made)
		s.Body, s.MissingBody = map[string]any{"removeLabels": []string{f.hiddenLabel}}, map[string]any{"removeLabels": []string{made}}
		return s
	},
	// This sample can show only no 5xx and the same body for a hidden and a
	// made-up slug (both are empty lists for the restricted member, scoped or
	// not, because the hidden product's teams are hidden). That
	// resolveProduct is scoped is shown by TestPlanningAccess_Products and
	// TestPlanningAccess_CrossOrg (the mixed product lists ORB issues only).
	"GET /issues product": func(f *teamFixture) teamSample {
		return f.filter("/issues?product=", f.plan.hiddenProductSlug, "made-product")
	},

	// --- products (#69) ---
	"GET /teams/{team}/products": func(f *teamFixture) teamSample {
		s := f.path("GET", "/teams/HID/products", "/teams/ZZZ/products", "HID", "ZZZ", nil, nil)
		// The visible team's products: the mixed product shows team ORB only.
		s.Also = []teamSample{{Kind: ruleList, Method: "GET", Path: f.orgPath("/teams/" + testdb.TeamKey + "/products"), Check: f.mixedSeen}}
		return s
	},
	"GET /products": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/products"), Check: f.mixedSeen}
	},
	"GET /products/{product}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.path("GET", "/products/"+f.hidden["product"], "/products/"+made, f.hidden["product"], made, nil, nil)
		s.Also = []teamSample{f.path("GET", "/products/"+f.plan.hiddenProductSlug, "/products/made-product", f.plan.hiddenProductSlug, "made-product", nil, nil),
			{Kind: ruleList, Method: "GET", Path: f.orgPath("/products/" + f.plan.mixed.String())}}
		return s
	},

	// --- projects (#36, #37, #39) ---
	"GET /projects": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/projects?limit=200"), Check: listCheck(f.sharedSeen)}
	},
	// ?team=HID must not match the shared project through its hidden team row.
	"GET /projects team": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.filter("/projects?team=", "HID", "ZZZ")
		s.Also = []teamSample{f.filter("/projects?team=", f.teamID(f.e.t, "HID").String(), made)}
		return s
	},
	"GET /projects label": func(f *teamFixture) teamSample { return f.filter("/projects?label=", "secret-pl", "made-pl") },
	"POST /projects": func(f *teamFixture) teamSample {
		return f.body("POST", "/projects", map[string]any{"name": "probe"}, "teams", []string{"HID"}, []string{"ZZZ"}, "HID", "ZZZ",
			f.countOf(`SELECT count(*) FROM projects WHERE org_id = $1`, f.e.orgID))
	},
	"GET /projects/{project}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.path("GET", "/projects/"+f.hidden["project"], "/projects/"+made, f.hidden["project"], made, nil, nil)
		s.Also = []teamSample{{Kind: ruleList, Method: "GET", Path: f.orgPath("/projects/" + f.plan.shared.String()),
			Check: func(b []byte) string {
				var p tracker.Project
				if err := json.Unmarshal(b, &p); err != nil {
					return err.Error()
				}
				return f.sharedSeen([]tracker.Project{p})
			}}}
		return s
	},
	"PATCH /projects/{project}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("PATCH", "/projects/"+f.hidden["project"], "/projects/"+made, f.hidden["project"], made,
			map[string]any{"name": "changed"}, f.projectTeams(f.hidden["project"]))
	},
	"PATCH /projects/{project} teams": func(f *teamFixture) teamSample {
		return f.body("PATCH", "/projects/"+f.plan.shared.String(), nil, "teams", []string{testdb.TeamKey, "HID"}, []string{testdb.TeamKey, "ZZZ"},
			"HID", "ZZZ", f.projectTeams(f.plan.shared.String()))
	},
	"GET /projects/{project}/avatar": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("GET", "/projects/"+f.hidden["project"]+"/avatar", "/projects/"+made+"/avatar", f.hidden["project"], made, nil, nil)
	},
	"POST /projects/{project}/avatar/prepare": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("POST", "/projects/"+f.hidden["project"]+"/avatar/prepare", "/projects/"+made+"/avatar/prepare", f.hidden["project"], made,
			map[string]any{"contentType": "image/png", "sizeBytes": 100}, nil)
	},
	"PUT /projects/{project}/avatar": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("PUT", "/projects/"+f.hidden["project"]+"/avatar", "/projects/"+made+"/avatar", f.hidden["project"], made,
			map[string]any{"uploadKey": "probe"}, f.rowJSON("projects", f.hidden["project"]))
	},
	"DELETE /projects/{project}/avatar": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("DELETE", "/projects/"+f.hidden["project"]+"/avatar", "/projects/"+made+"/avatar", f.hidden["project"], made,
			nil, f.rowJSON("projects", f.hidden["project"]))
	},
	"GET /projects/{project}/updates": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("GET", "/projects/"+f.hidden["project"]+"/updates", "/projects/"+made+"/updates", f.hidden["project"], made, nil, nil)
	},
	"POST /projects/{project}/updates": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("POST", "/projects/"+f.hidden["project"]+"/updates", "/projects/"+made+"/updates", f.hidden["project"], made,
			map[string]any{"health": "on_track", "body": "probe"}, f.countOf(`SELECT count(*) FROM project_updates WHERE org_id = $1`, f.e.orgID))
	},
	"GET /projects/{project}/updates/{update}": func(f *teamFixture) teamSample {
		made, madeU := uuid.NewString(), uuid.NewString()
		return f.path("GET", "/projects/"+f.hidden["project"]+"/updates/"+f.hidden["update"], "/projects/"+made+"/updates/"+madeU,
			f.hidden["project"], made, nil, nil)
	},

	// --- milestones (#38) ---
	"GET /milestones": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/milestones?limit=200")}
	},
	"GET /milestones project": func(f *teamFixture) teamSample {
		return f.filter("/milestones?project=", f.hidden["project"], uuid.NewString())
	},
	"POST /milestones": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.body("POST", "/milestones", map[string]any{"name": "probe"}, "projectId", f.hidden["project"], made, f.hidden["project"], made,
			f.countOf(`SELECT count(*) FROM milestones WHERE org_id = $1`, f.e.orgID))
	},
	"GET /milestones/{milestone}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("GET", "/milestones/"+f.hidden["milestone"], "/milestones/"+made, f.hidden["milestone"], made, nil, nil)
	},
	"PATCH /milestones/{milestone}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("PATCH", "/milestones/"+f.hidden["milestone"], "/milestones/"+made, f.hidden["milestone"], made,
			map[string]any{"name": "changed"}, f.rowJSON("milestones", f.hidden["milestone"]))
	},

	// --- cycles ---
	"GET /cycles": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/cycles?limit=200")}
	},
	"GET /cycles team": func(f *teamFixture) teamSample {
		s := f.filter("/cycles?team=", "HID", "ZZZ")
		made := uuid.NewString()
		s.Also = []teamSample{f.filter("/cycles?team=", f.teamID(f.e.t, "HID").String(), made)}
		return s
	},
	"GET /cycles/{cycle}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.path("GET", "/cycles/"+f.hidden["cycle"], "/cycles/"+made, f.hidden["cycle"], made, nil, nil)
		made2 := uuid.NewString()
		s.Also = []teamSample{f.path("GET", "/cycles/"+f.plan.cycleLinear, "/cycles/"+made2, f.plan.cycleLinear, made2, nil, nil)}
		return s
	},
	"PATCH /cycles/{cycle}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.path("PATCH", "/cycles/"+f.hidden["cycle"], "/cycles/"+made, f.hidden["cycle"], made,
			map[string]any{"name": "changed"}, f.rowJSON("cycles", f.hidden["cycle"]))
		made2 := uuid.NewString()
		s.Also = []teamSample{f.path("PATCH", "/cycles/"+f.plan.cycleLinear, "/cycles/"+made2, f.plan.cycleLinear, made2,
			map[string]any{"name": "changed"}, f.rowJSON("cycles", f.hidden["cycle"]))}
		return s
	},
	// The lazy refresh of today's snapshot row runs only after the check: a
	// hidden running cycle gets no row (the count does not move).
	"GET /cycles/{cycle}/snapshots": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("GET", "/cycles/"+f.hidden["cycle"]+"/snapshots", "/cycles/"+made+"/snapshots", f.hidden["cycle"], made, nil,
			f.countOf(`SELECT count(*) FROM cycle_snapshots WHERE cycle_id = $1`, f.hidden["cycle"]))
	},

	// --- documents and their comments (#38) ---
	"GET /documents": func(f *teamFixture) teamSample {
		return teamSample{Method: "GET", Path: f.orgPath("/documents?limit=200"), Check: listCheck(func(ds []tracker.Document) string {
			for _, d := range ds {
				if d.ID == f.plan.openDoc {
					return ""
				}
			}
			return "the document with no project is not listed"
		})}
	},
	"GET /documents project": func(f *teamFixture) teamSample {
		return f.filter("/documents?project=", f.hidden["project"], uuid.NewString())
	},
	"POST /documents": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.body("POST", "/documents", map[string]any{"title": "probe"}, "projectId", f.hidden["project"], made, f.hidden["project"], made,
			f.countOf(`SELECT count(*) FROM documents WHERE org_id = $1`, f.e.orgID))
	},
	"GET /documents/{document}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.path("GET", "/documents/"+f.hidden["document"], "/documents/"+made, f.hidden["document"], made, nil, nil)
		s.Also = []teamSample{f.path("GET", "/documents/"+f.plan.hiddenDocSlug, "/documents/made-doc-0123456789ab", f.plan.hiddenDocSlug, "made-doc-0123456789ab", nil, nil),
			{Kind: ruleList, Method: "GET", Path: f.orgPath("/documents/" + f.plan.openDoc.String())}}
		return s
	},
	"PATCH /documents/{document}": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("PATCH", "/documents/"+f.hidden["document"], "/documents/"+made, f.hidden["document"], made,
			map[string]any{"title": "changed"}, f.rowJSON("documents", f.hidden["document"]))
	},
	"PATCH /documents/{document} projectId": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.body("PATCH", "/documents/"+f.plan.openDoc.String(), nil, "projectId", f.hidden["project"], made, f.hidden["project"], made,
			f.rowJSON("documents", f.plan.openDoc.String()))
	},
	"GET /documents/{document}/comments": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("GET", "/documents/"+f.hidden["document"]+"/comments", "/documents/"+made+"/comments", f.hidden["document"], made, nil, nil)
	},
	"POST /documents/{document}/comments": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.path("POST", "/documents/"+f.hidden["document"]+"/comments", "/documents/"+made+"/comments", f.hidden["document"], made,
			map[string]any{"body": "probe"}, f.countOf(`SELECT count(*) FROM comments WHERE org_id = $1`, f.e.orgID))
	},
	// A reply to a comment the caller cannot see (on a hidden issue, and on a
	// hidden document) answers as a made-up parent.
	"POST /documents/{document}/comments parentId": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		p := "/documents/" + f.plan.openDoc.String() + "/comments"
		cnt := f.countOf(`SELECT count(*) FROM comments WHERE org_id = $1`, f.e.orgID)
		s := f.body("POST", p, map[string]any{"body": "probe"}, "parentId", f.hidden["comment"], made, f.hidden["comment"], made, cnt)
		s.Also = []teamSample{f.body("POST", p, map[string]any{"body": "probe"}, "parentId", f.plan.hiddenDocComment.String(), made,
			f.plan.hiddenDocComment.String(), made, cnt)}
		return s
	},

	// --- POST /issues: the team and every planning and issue ref (#21, #40) ---
	"POST /issues": func(f *teamFixture) teamSample {
		return f.body("POST", "/issues", map[string]any{"title": "probe"}, "team", "HID", "ZZZ", "HID", "ZZZ", f.issueCount)
	},
	"POST /issues parent": func(f *teamFixture) teamSample {
		return f.newIssue("parent", f.hidden["issue"], "HID-999", f.hidden["issue"], "HID-999")
	},
	"POST /issues projectId": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.newIssue("projectId", f.hidden["project"], made, f.hidden["project"], made)
	},
	"POST /issues milestoneId": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.newIssue("milestoneId", f.hidden["milestone"], made, f.hidden["milestone"], made)
	},
	"POST /issues cycleId": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		s := f.newIssue("cycleId", f.hidden["cycle"], made, f.hidden["cycle"], made)
		made2 := uuid.NewString()
		s.Also = []teamSample{f.newIssue("cycleId", f.plan.cycleLinear, made2, f.plan.cycleLinear, made2)}
		return s
	},
	"POST /issues state": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.newIssue("state", f.hiddenState, made, f.hiddenState, made)
	},
	"POST /issues labels": func(f *teamFixture) teamSample {
		made := uuid.NewString()
		return f.newIssue("labels", []string{f.hiddenLabel}, []string{made}, f.hiddenLabel, made)
	},
}

// childPlanningAlso adds the two-field #40 samples to the PATCH /issues
// projectId and milestoneId samples of ticket 2b.
var childPlanningAlso = map[string]func(f *teamFixture) []teamSample{
	"PATCH /issues/{issue} projectId": func(f *teamFixture) []teamSample {
		hp, made, made2 := f.hidden["project"], uuid.NewString(), uuid.NewString()
		return []teamSample{
			// The current hidden project again.
			f.childPlanning(map[string]any{"projectId": hp}, map[string]any{"projectId": made}, hp, made),
			// It again, with a made-up milestone: projectId is judged first in both.
			f.childPlanning(map[string]any{"projectId": hp, "milestoneId": made2}, map[string]any{"projectId": made, "milestoneId": made2}, hp, made),
		}
	},
	"PATCH /issues/{issue} milestoneId": func(f *teamFixture) []teamSample {
		hm, made, made2 := f.hidden["milestone"], uuid.NewString(), uuid.NewString()
		return []teamSample{
			// The current hidden milestone again.
			f.childPlanning(map[string]any{"milestoneId": hm}, map[string]any{"milestoneId": made}, hm, made),
			// With a made-up project: "unknown projectId" in both (the review case).
			f.childPlanning(map[string]any{"projectId": made2, "milestoneId": hm}, map[string]any{"projectId": made2, "milestoneId": made}, hm, made),
			// With a visible project: "unknown milestoneId" in both, and the
			// hidden milestone is not cleared.
			f.childPlanning(map[string]any{"projectId": f.plan.shared.String(), "milestoneId": hm},
				map[string]any{"projectId": f.plan.shared.String(), "milestoneId": made}, hm, made),
		}
	},
}

func init() {
	for k, also := range childPlanningAlso {
		orig, also := teamRuleSamples[k], also
		if orig == nil {
			panic("no sample " + k + " to extend")
		}
		teamRuleSamples[k] = func(f *teamFixture) teamSample {
			s := orig(f)
			s.Also = append(s.Also, also(f)...)
			return s
		}
	}
	for k, v := range planningSamples {
		if _, dup := teamRuleSamples[k]; dup {
			panic("planning sample " + k + " is in teamRuleSamples twice")
		}
		teamRuleSamples[k] = v
	}
}

// TestPlanningSamples_Harness shows the ticket 5 samples can fail: read by
// the all-teams member, each list holds team B, and each Check finds the
// hidden team or issue.
func TestPlanningSamples_Harness(t *testing.T) {
	f := newTeamFixture(t)
	for _, k := range []string{"GET /products", "GET /projects", "GET /milestones", "GET /cycles", "GET /documents"} {
		s := planningSamples[k](f)
		res := f.e.do("GET", s.Path, f.member, nil)
		held := false
		for _, m := range f.marks {
			held = held || bytes.Contains(res.body, []byte(m))
		}
		if res.status != http.StatusOK || !held {
			t.Errorf("%s: the all-teams member does not see team B: %d %s", k, res.status, res.body)
		}
		// The documents Check is positive (the open document is listed).
		if s.Check != nil && k != "GET /documents" {
			if p := s.Check(res.body); p == "" {
				t.Errorf("%s: the Check passes for the all-teams member, so it cannot fail", k)
			}
		}
	}
	s := planningSamples["GET /projects/{project}"](f).Also[0]
	if p := s.Check(f.e.do("GET", s.Path, f.member, nil).body); !strings.Contains(p, "want ORB only") {
		t.Errorf("GET /projects/{shared} as the all-teams member: %q, want the teams check to fail", p)
	}
	// The ?team= sample on projects finds the shared project for the admin.
	if res := f.e.do("GET", f.orgPath("/projects?team=HID"), f.e.chen, nil); !bytes.Contains(res.body, []byte(f.plan.shared.String())) {
		t.Errorf("admin ?team=HID does not hold the shared project: %s", res.body)
	}
}
