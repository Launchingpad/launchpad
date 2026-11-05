package api

// Team rules (decisions-T20-team-access.md #17): every route declares how its
// team access is decided, so a new route cannot merge without a decision.
//
// teamRules holds the routes under /orgs/{org} (keyed "METHOD pattern", with
// the pattern relative to /api/v1/orgs/{org}); outsideRules holds every other
// route, keyed by the full pattern (public and install-level routes, and the
// three webhooks, the only routes under /orgs/{org} outside Authenticate).
// TestTeamRulesCoverRouter (Test A) fails on a route with no entry and on an
// entry with no route.
//
// Enforced says the route already filters by the caller's visible teams. Each
// build ticket flips the flags of its routes (Ticket names it) and adds the
// Test B samples of the rule and its Refs. TestNoSelectedWriterWhileUnenforced
// fails while anything can write team access (the spec, the SQL, a Go request
// struct) and any flag, inside or outside /orgs/{org}, is false (#19). Since
// ticket 8 every flag is true (TestTeamRules_AllEnforced), so a new route must
// be enforced, with its samples, when it is added.

// ruleKind is the kind of a team rule (#17).
type ruleKind string

const (
	ruleList   ruleKind = "list"   // a list: rows of hidden teams are left out
	rulePath   ruleKind = "path"   // a path parameter names the object: hidden is 404
	ruleBody   ruleKind = "body"   // a body field names the object: hidden is 400 unknown
	ruleFilter ruleKind = "filter" // a query parameter names the object: as unknown
	ruleFeed   ruleKind = "feed"   // the change feed (#55, #56)
	ruleOwn    ruleKind = "own"    // the caller's own rows (#62, #64)
	ruleAdmin  ruleKind = "admin"  // org admin only: a restricted member gets 403
	ruleNone   ruleKind = "none"   // no team data; Reason says why
)

// teamRule is the rule of one route.
type teamRule struct {
	Kind ruleKind
	// Resource is what the route reads or writes (list, path, body, filter).
	Resource string
	// Param is the path parameter (path), the body field (body) or the query
	// parameter (filter) that names the object.
	Param string
	// Refs are the other body fields or query parameters that name a
	// team-owned object; the same rule covers them (#10, #21).
	Refs []string
	// Reason says why a none, own or admin route reads no hidden row.
	Reason string
	// Ticket is the build ticket of the note's ticket list that enforces the
	// rule and flips Enforced.
	Ticket string
	// Enforced is true once the route filters by the visible set and Test B
	// has a sample for it.
	Enforced bool
}

func listRule(resource, ticket string, refs ...string) teamRule {
	return teamRule{Kind: ruleList, Resource: resource, Ticket: ticket, Refs: refs}
}

func pathRule(resource, param, ticket string, refs ...string) teamRule {
	return teamRule{Kind: rulePath, Resource: resource, Param: param, Ticket: ticket, Refs: refs}
}

func bodyRule(resource, field, ticket string, refs ...string) teamRule {
	return teamRule{Kind: ruleBody, Resource: resource, Param: field, Ticket: ticket, Refs: refs}
}

func feedRule(ticket string) teamRule {
	return teamRule{Kind: ruleFeed, Resource: "event", Ticket: ticket}
}

func ownRule(reason, ticket string) teamRule {
	return teamRule{Kind: ruleOwn, Reason: reason, Ticket: ticket}
}

// admin routes need the org admin role (auth.RequireScope(admin)); admins see
// every team (#2, #12). Ticket 8 flipped their flags with the last ones: Test
// B checks that a restricted member gets 403 forbidden on each.
func adminRule() teamRule {
	return teamRule{Kind: ruleAdmin, Reason: "org admin only (#2)", Ticket: "8", Enforced: true}
}

// enforced marks a rule whose route filters by the visible set and whose
// Test B samples (the rule and each Ref) pass.
func (r teamRule) enforced() teamRule {
	r.Enforced = true
	return r
}

// noneRule: the route reads or writes no team-owned row (Reason says why).
// Ticket 8 reviewed every one and flipped their flags with the last ones.
func noneRule(reason string) teamRule {
	return teamRule{Kind: ruleNone, Reason: reason, Ticket: "8", Enforced: true}
}

// webhookRule: a public webhook acts with system authority (#65). Its answer
// is a status only (tracker.WebhookResult), which never depends on whether an
// issue matched; a repository with a team matches that team only, and the
// guard (tracker.CheckWebhookGuard) keeps a repository with no team from
// existing next to restricted access. Ticket 8.
func webhookRule() teamRule {
	return teamRule{Kind: ruleNone, Reason: "public webhook, system authority; the answer is a status only and a mapped team limits the match (#65)", Ticket: "8", Enforced: true}
}

// teamRules: the routes under /orgs/{org}, in the order of router.go.
var teamRules = map[string]teamRule{
	// The org row carries the default team's key (GetOrgFull): a hidden
	// default team is not named (#9); the key is null. Ticket 3.
	"GET /":                              listRule("org row (default team key)", "3").enforced(),
	"PATCH /":                            adminRule(),
	"GET /avatar":                        noneRule("the org avatar (#7)"),
	"POST /avatar/prepare":               adminRule(),
	"PUT /avatar":                        adminRule(),
	"DELETE /avatar":                     adminRule(),
	"GET /onboarding":                    adminRule(),
	"PUT /onboarding/skip":               adminRule(),
	"GET /agent-guidance":                listRule("agent guidance", "3", "team").enforced(),
	"PUT /agent-guidance":                adminRule(),
	"DELETE /agent-guidance":             adminRule(),
	"GET /chat-services":                 adminRule(),
	"POST /chat-services":                adminRule(),
	"GET /chat-services/{service}":       adminRule(),
	"PATCH /chat-services/{service}":     adminRule(),
	"DELETE /chat-services/{service}":    adminRule(),
	"POST /chat-services/{service}/test": adminRule(),
	"GET /push":                          adminRule(),
	"PUT /push":                          adminRule(),
	"DELETE /push":                       adminRule(),
	"POST /push/test":                    adminRule(),

	"GET /teams":                     listRule("team", "3").enforced(),
	"POST /teams":                    adminRule(),
	"GET /teams/{team}":              pathRule("team", "team", "3").enforced(),
	"PATCH /teams/{team}":            adminRule(),
	"DELETE /teams/{team}":           adminRule(),
	"GET /teams/{team}/states":       pathRule("team", "team", "3").enforced(),
	"GET /teams/{team}/products":     pathRule("team", "team", "5").enforced(),
	"GET /teams/{team}/git-workflow": pathRule("team", "team", "LAUN-244").enforced(),
	"PUT /teams/{team}/git-workflow": adminRule(),

	// Products follow the project rule (#69). The ticket list names no
	// product ticket; ticket 5 (planning objects) takes them.
	"GET /products":                           listRule("product", "5").enforced(),
	"POST /products":                          adminRule(),
	"GET /products/{product}":                 pathRule("product", "product", "5").enforced(),
	"PATCH /products/{product}":               adminRule(),
	"DELETE /products/{product}":              adminRule(),
	"PUT /products/{product}/teams":           adminRule(),
	"PUT /products/{product}/teams/{team}":    adminRule(),
	"DELETE /products/{product}/teams/{team}": adminRule(),

	// lead and labelIds name no team-owned object: any member may lead (#46),
	// and project labels are org-wide (decisions-T28). They are not Refs.
	"GET /projects":              listRule("project", "5", "team", "label").enforced(),
	"POST /projects":             bodyRule("project", "teams", "5").enforced(),
	"GET /projects/{project}":    pathRule("project", "project", "5").enforced(),
	"PATCH /projects/{project}":  pathRule("project", "project", "5", "teams").enforced(),
	"DELETE /projects/{project}": adminRule(),
	// The catalogue is org-wide (decisions-T28); projectCount counts visible
	// projects only (#36).
	"GET /project-labels":                      listRule("project label (projectCount)", "3").enforced(),
	"POST /project-labels":                     adminRule(),
	"PATCH /project-labels/{label}":            adminRule(),
	"DELETE /project-labels/{label}":           adminRule(),
	"GET /projects/{project}/avatar":           pathRule("project", "project", "5").enforced(),
	"POST /projects/{project}/avatar/prepare":  pathRule("project", "project", "5").enforced(),
	"PUT /projects/{project}/avatar":           pathRule("project", "project", "5").enforced(),
	"DELETE /projects/{project}/avatar":        pathRule("project", "project", "5").enforced(),
	"GET /projects/{project}/updates":          pathRule("project", "project", "5").enforced(),
	"POST /projects/{project}/updates":         pathRule("project", "project", "5").enforced(),
	"GET /projects/{project}/updates/{update}": pathRule("project", "project", "5").enforced(),
	"GET /project-statuses":                    noneRule("org-wide catalogue (decisions-T29)"),
	"POST /project-statuses":                   adminRule(),
	"PUT /project-statuses/order":              adminRule(),
	"PATCH /project-statuses/{status}":         adminRule(),
	"DELETE /project-statuses/{status}":        adminRule(),

	"GET /milestones":               listRule("milestone", "5", "project").enforced(),
	"POST /milestones":              bodyRule("milestone", "projectId", "5").enforced(),
	"GET /milestones/{milestone}":   pathRule("milestone", "milestone", "5").enforced(),
	"PATCH /milestones/{milestone}": pathRule("milestone", "milestone", "5").enforced(),

	"GET /cycles":                   listRule("cycle", "5", "team").enforced(),
	"GET /cycles/{cycle}":           pathRule("cycle", "cycle", "5").enforced(),
	"PATCH /cycles/{cycle}":         pathRule("cycle", "cycle", "5").enforced(),
	"POST /cycles/{cycle}/complete": adminRule(),
	"DELETE /cycles/{cycle}":        adminRule(),
	"GET /cycles/{cycle}/snapshots": pathRule("cycle", "cycle", "5").enforced(),

	"GET /documents":                      listRule("document", "5", "project").enforced(),
	"POST /documents":                     bodyRule("document", "projectId", "5").enforced(),
	"GET /documents/{document}":           pathRule("document", "document", "5").enforced(),
	"PATCH /documents/{document}":         pathRule("document", "document", "5", "projectId").enforced(),
	"GET /documents/{document}/comments":  pathRule("document", "document", "5").enforced(),
	"POST /documents/{document}/comments": pathRule("document", "document", "5", "parentId").enforced(),

	// Labels are org-wide or of one team (LAUN-54): a team label follows its
	// team (#8); issueCount counts visible issues only.
	"GET /labels":  listRule("label", "3", "team").enforced(),
	"POST /labels": bodyRule("label", "team", "3").enforced(),

	"GET /issues":            listRule("issue", "3", "team", "project", "cycle", "parent", "assignee", "label", "state", "query", "product").enforced(),
	"POST /issues":           bodyRule("issue", "team", "5", "parent", "projectId", "milestoneId", "cycleId", "state", "labels").enforced(),
	"GET /issues/{issue}":    pathRule("issue", "issue", "4", "include").enforced(),
	"PATCH /issues/{issue}":  pathRule("issue", "issue", "5", "parent", "projectId", "milestoneId", "cycleId", "state", "labels", "addLabels", "removeLabels", "placeAfter", "placeBefore").enforced(),
	"DELETE /issues/{issue}": adminRule(),

	// Snapshots that name a hidden issue, project, milestone or cycle are
	// null for a restricted viewer (#43).
	"GET /issues/{issue}/activity":  pathRule("issue", "issue", "4").enforced(),
	"GET /issues/{issue}/comments":  pathRule("issue", "issue", "4").enforced(),
	"POST /issues/{issue}/comments": pathRule("issue", "issue", "4", "parentId").enforced(),
	// A comment on an issue or on a document (GetCommentForUpdate judges
	// either); the samples hold one of each.
	"PATCH /comments/{comment}":  pathRule("comment", "comment", "4").enforced(),
	"DELETE /comments/{comment}": pathRule("comment", "comment", "4").enforced(),

	"POST /attachments/prepare":        noneRule("no issue yet: the upload key is bound to an issue at attach time"),
	"GET /issues/{issue}/attachments":  pathRule("issue", "issue", "4").enforced(),
	"POST /issues/{issue}/attachments": pathRule("issue", "issue", "4").enforced(),
	"GET /attachments/{attachment}":    pathRule("attachment", "attachment", "4").enforced(),
	// The scoped lookup runs before the presign: no URL is signed for a
	// hidden attachment.
	"GET /attachments/{attachment}/download": pathRule("attachment", "attachment", "4").enforced(),

	// Edges to a hidden issue are left out, and cannot be made or removed (#41).
	"GET /issues/{issue}/relations":  pathRule("issue", "issue", "4").enforced(),
	"POST /issues/{issue}/relations": pathRule("issue", "issue", "4", "issue").enforced(),
	// Ref relation: an edge of a visible issue whose other side is hidden.
	"DELETE /issues/{issue}/relations/{relation}": pathRule("issue", "issue", "4", "relation").enforced(),

	"GET /users": noneRule("the member roster (#7)"),
	// The team access setter (#48, ticket 9): org admins only.
	"PUT /users/{user}/team-access": adminRule(),

	"GET /me/notification-settings":              ownRule("the caller's settings; lists visible teams only (#64)", "7").enforced(),
	"PUT /me/notification-settings":              ownRule("the caller's global setting, no team", "7").enforced(),
	"PUT /me/notification-settings/teams/{team}": pathRule("team", "team", "7").enforced(),
	"PUT /me/teams/{team}/follow":                pathRule("team", "team", "7").enforced(),
	"DELETE /me/teams/{team}/follow":             pathRule("team", "team", "7").enforced(),
	"GET /me/views":                              ownRule("the caller's remembered views; no team row", "8").enforced(),
	"PUT /me/views/{place}":                      ownRule("the caller's remembered views; no team row", "8").enforced(),
	"GET /me/git-accounts":                       ownRule("the caller's git handles (decisions-T34)", "8").enforced(),
	"PUT /me/git-accounts/{integration}":         noneRule("the caller's own git handle on one integration (decisions-T34); no team row"),
	"DELETE /me/git-accounts/{integration}":      noneRule("the caller's own git handle on one integration (decisions-T34); no team row"),
	"GET /me/push-devices":                       noneRule("the caller's own devices (decisions-T35); no team row"),
	"PUT /me/push-devices/{device}":              noneRule("the caller's own device row (decisions-T35); no team row"),
	"DELETE /me/push-devices/{device}":           noneRule("the caller's own device row (decisions-T35); no team row"),
	"GET /me/webhooks":                           noneRule("the caller's own webhooks (decisions-T40); no team row"),
	"POST /me/webhooks":                          noneRule("the caller's own webhook row (decisions-T40); no team row"),
	"PATCH /me/webhooks/{webhook}":               noneRule("the caller's own webhook row (decisions-T40); no team row"),
	"DELETE /me/webhooks/{webhook}":              noneRule("the caller's own webhook row (decisions-T40); no team row"),
	"POST /me/webhooks/{webhook}/test":           noneRule("the caller's own webhook; a fixed test sentence, no team row (decisions-T40)"),
	"GET /git-identities":                        adminRule(),
	"DELETE /git-identities/{identity}":          adminRule(),
	"GET /issues/{issue}/subscription":           pathRule("issue", "issue", "7").enforced(),
	"PUT /issues/{issue}/subscription":           pathRule("issue", "issue", "7").enforced(),
	"DELETE /issues/{issue}/subscription":        pathRule("issue", "issue", "7").enforced(),
	"GET /notifications":                         ownRule("the caller's inbox; the issue join is filtered (#62)", "7").enforced(),
	"GET /notifications/unread-count":            ownRule("the caller's inbox; the issue join is filtered (#62)", "7").enforced(),
	"GET /notifications/stream":                  ownRule("the caller's inbox; the issue join is filtered (#62)", "7").enforced(),
	"POST /notifications/read-all":               ownRule("the caller's inbox; the issue join is filtered (#62)", "7").enforced(),
	"PATCH /notifications/{id}":                  ownRule("the caller's inbox; the issue join is filtered (#62)", "7").enforced(),
	"POST /agents":                               adminRule(),

	"GET /signups":                   adminRule(),
	"DELETE /signups/unconfirmed":    adminRule(),
	"POST /signups/{signup}/approve": adminRule(),
	"POST /signups/{signup}/reject":  adminRule(),
	"GET /invites":                   adminRule(),
	"POST /invites":                  adminRule(),
	"DELETE /invites/{invite}":       adminRule(),

	"GET /tokens":            adminRule(),
	"POST /tokens":           adminRule(),
	"DELETE /tokens/{token}": adminRule(),

	"GET /integrations":                                          adminRule(),
	"POST /integrations":                                         adminRule(),
	"GET /integrations/{integration}":                            adminRule(),
	"POST /integrations/{integration}/check":                     adminRule(),
	"GET /integrations/{integration}/deliveries":                 adminRule(),
	"PATCH /integrations/{integration}":                          adminRule(),
	"DELETE /integrations/{integration}":                         adminRule(),
	"POST /integrations/{integration}/rotate-secret":             adminRule(),
	"DELETE /integrations/{integration}/rotate-secret":           adminRule(),
	"POST /integrations/{integration}/rotate-secret/commit":      adminRule(),
	"GET /integrations/{integration}/available-repos":            adminRule(),
	"GET /integrations/{integration}/repos":                      adminRule(),
	"POST /integrations/{integration}/repos":                     adminRule(),
	"PATCH /integrations/{integration}/repos/{repo}":             adminRule(),
	"DELETE /integrations/{integration}/repos/{repo}":            adminRule(),
	"POST /integrations/{integration}/repos/{repo}/hook/refresh": adminRule(),
	"POST /integrations/{integration}/repos/{repo}/backfill":     adminRule(),

	"POST /import/sessions":                   adminRule(),
	"GET /import/sessions/current":            adminRule(),
	"POST /import/sessions/{session}/export":  adminRule(),
	"GET /import/sessions/{session}/users":    adminRule(),
	"PUT /import/sessions/{session}/usermap":  adminRule(),
	"POST /import/sessions/{session}/dry-run": adminRule(),
	"POST /import/sessions/{session}/run":     adminRule(),
	"POST /import/sessions/{session}/verify":  adminRule(),
	"DELETE /import/sessions/{session}":       adminRule(),
	"GET /import/jobs/{job}":                  adminRule(),
	"POST /import/jobs/{job}/cancel":          adminRule(),

	"GET /events":        feedRule("6a").enforced(),
	"GET /events/stream": feedRule("6a").enforced(),
}

// outsideRules: every route that is not under the RequireOrgMember group (#17,
// "Routes outside /orgs/{org}"). Only the three unauthenticated webhooks may sit
// under /orgs/{org} here (ruleProblems checks it). They have flags and tickets
// too, so the guard of #19 waits for them.
var outsideRules = map[string]teamRule{
	"GET /healthz":                                           noneRule("health check"),
	"GET /api/v1/openapi.yaml":                               noneRule("the published spec"),
	"POST /api/v1/auth/otp":                                  noneRule("login, before any org"),
	"POST /api/v1/auth/otp/verify":                           noneRule("login, before any org"),
	"POST /api/v1/platform/move/export":                      noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/move/export/{ticket}":              noneRule("install-level, no tenant row"),
	"POST /api/v1/setup/restore":                             noneRule("first-run restore: the bundle brings its own orgs"),
	"GET /api/v1/setup":                                      noneRule("first-run setup"),
	"POST /api/v1/setup":                                     noneRule("first-run setup: the first admin sees every team"),
	"POST /api/v1/signups":                                   noneRule("a sign-up request names no team"),
	"POST /api/v1/signups/confirm":                           noneRule("a sign-up request names no team"),
	"POST /api/v1/invites/accept":                            noneRule("creates the membership with the invite's access (#27)"),
	"POST /api/v1/notifications/unsubscribe/{token}":         noneRule("one-click unsubscribe: the token names one user's mail settings"),
	"POST /api/v1/orgs/{org}/integrations/gitlab/webhook":    webhookRule(),
	"POST /api/v1/orgs/{org}/integrations/github/webhook":    webhookRule(),
	"POST /api/v1/orgs/{org}/integrations/bitbucket/webhook": webhookRule(),
	"POST /api/v1/auth/logout":                               noneRule("the caller's own session"),
	"GET /api/v1/auth/session":                               noneRule("the caller's own session"),
	"GET /api/v1/me":                                         noneRule("the caller's own user"),
	// Each org carries its default team's key (ListOrgsForUser): a hidden
	// default team is not named (#9); the key is null. Ticket 3.
	"GET /api/v1/orgs":                                              ownRule("the caller's orgs; each has its default team key", "3").enforced(),
	"GET /api/v1/me/sessions":                                       noneRule("the caller's own sessions"),
	"POST /api/v1/me/sessions/revoke-others":                        noneRule("the caller's own sessions"),
	"DELETE /api/v1/me/sessions/{id}":                               noneRule("the caller's own sessions"),
	"GET /api/v1/me/preferences":                                    noneRule("the caller's own display preferences (no org row)"),
	"PUT /api/v1/me/preferences":                                    noneRule("the caller's own display preferences (no org row)"),
	"GET /api/v1/platform/settings":                                 noneRule("install-level, no tenant row"),
	"PUT /api/v1/platform/settings":                                 noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/mail/test":                               noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/admins":                                   noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/admins":                                  noneRule("install-level, no tenant row"),
	"DELETE /api/v1/platform/admins/{user}":                         noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/settings":                         noneRule("install-level, no tenant row"),
	"PUT /api/v1/platform/backups/settings":                         noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/status":                           noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/recipients":                       noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/recipients":                      noneRule("install-level, no tenant row"),
	"DELETE /api/v1/platform/backups/recipients/{fingerprint}":      noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/recipients/{fingerprint}/cancel": noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/destinations":                     noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations":                    noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations/probe":              noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations/test":               noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/destinations/{id}":                noneRule("install-level, no tenant row"),
	"PATCH /api/v1/platform/backups/destinations/{id}":              noneRule("install-level, no tenant row"),
	"DELETE /api/v1/platform/backups/destinations/{id}":             noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations/{id}/probe":         noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations/{id}/test":          noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations/{id}/exchange":      noneRule("install-level, no tenant row"),
	"POST /api/v1/platform/backups/destinations/{id}/run":           noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/runs":                             noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/runs/{id}":                        noneRule("install-level, no tenant row"),
	"GET /api/v1/platform/backups/changes":                          noneRule("install-level, no tenant row"),
}
