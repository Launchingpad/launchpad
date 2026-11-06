// Package api is the HTTP layer: the route tree, the cross-cutting
// middlewares and thin handlers over internal/tracker. The route tree here is
// the authority; api/openapi.yaml must describe exactly these routes
// (TestOpenAPIMatchesRoutes).
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/api"
	"devops/internal/auth"
	"devops/internal/backupapi"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/idempotency"
	"devops/internal/importjob"
	"devops/internal/login"
	"devops/internal/moveapi"
	"devops/internal/notify"
	"devops/internal/platform"
	"devops/internal/setup"
	"devops/internal/signup"
	"devops/internal/tracker"
)

// Server holds the handler dependencies.
type Server struct {
	svc    *tracker.Service
	authn  *auth.Authenticator
	broker *events.Broker
	idem   func(http.Handler) http.Handler
	pool   *pgxpool.Pool
	log    *slog.Logger
	// notify serves the preference, follow and subscription routes
	// (LAUN-32). NewServer makes it; main sets its mail check.
	notify *notify.Service
	// mintLimit limits token creation by web sessions (decisions-T11 #6).
	mintLimit *mintLimiter
	// gitLimit limits the routes that call a git host (T17 #28).
	gitLimit *gitLimiter

	// SSE timing; tests shorten them.
	SSEHeartbeat time.Duration
	SSEPoll      time.Duration
	// eventStreamPage is the page size of an event stream drain (0: the
	// default); eventPageHook runs before each page with the cursor. Tests
	// only (T20 #56).
	eventStreamPage int
	eventPageHook   func(after int64)

	// Web login (T6a). Login nil answers 503 login_unavailable on every
	// /auth route. SecureCookies sets Secure on the session cookies (off
	// only on a local dev server). CORSOrigins are the exact web origins
	// that get CORS headers; empty means none. Set them before Routes.
	Login         *login.Service
	SecureCookies bool
	CORSOrigins   []string

	// First-run setup. Nil: GET /setup says setupRequired false
	// and POST /setup answers 409 setup_complete. main always sets it.
	Setup *setup.Service

	// Move an install (LAUN-249, decisions-T46): the export ticket and
	// stream under /platform/move and the public POST /setup/restore. Nil:
	// they answer 404. main sets it.
	Move *moveapi.Service

	// Platform settings: mail provider and sign-up mode. Nil:
	// the /platform routes answer 404. main always sets it.
	Platform *platform.Service

	// Scheduled backups (LAUN-69, decisions-T16 #43): the /platform/backups
	// routes. Nil: they answer 404. main sets it.
	Backups *backupapi.Service

	// The org's chat services (LAUN-204, decisions-T36): the /chat-services
	// routes (they replace the /slack routes of LAUN-198). Nil: they answer 404.
	// main sets it.
	Chat *notify.ChatAdmin

	// Mobile push (LAUN-197, decisions-T35): the org's relay routes and a
	// member's devices. Nil: they answer 404. main sets it.
	Push *notify.PushService

	// A member's own webhooks (LAUN-229, decisions-T40): the /me/webhooks
	// routes. Nil: they answer 404. main sets it.
	Personal *notify.PersonalWebhooks

	// The web Linear import (LAUN-8, decisions-T17 #36 to #45). Nil: the
	// /import routes answer 404. main sets it.
	Import *importjob.Manager

	// Sign-up requests and invites. Nil: the public routes
	// answer 403 signup_unavailable (accept: 401 invalid_invite) and the
	// org routes 404. main always sets it.
	Signup *signup.Service
	// TrustedProxies (TRUSTED_PROXY_CIDRS) decide whose forwarding headers
	// pick the client address of the per-client sign-up limits
	// (clientip.go). Empty: none is trusted.
	TrustedProxies []netip.Prefix

	// UnsubscribeFloor is the least time POST /notifications/unsubscribe/{token}
	// takes (LAUN-34). NewServer sets DefaultUnsubscribeFloor; tests set 0.
	UnsubscribeFloor time.Duration
	unsub            *unsubLimiter

	// The signed-body git webhooks (GitHub, Bitbucket; LAUN-5, T17 #14): the budget of
	// in-flight unsigned body bytes and the body read deadline.
	// NewServer sets the defaults; SetGitWebhookLimits changes them.
	gitBudget       *byteBudget
	gitReadDeadline time.Duration
	// warnOnce state: one WARN per key per minute.
	gitWarnMu  sync.Mutex
	gitWarnAt  map[string]time.Time
	gitWarnNow func() time.Time
}

// NewServer wires a Server.
func NewServer(svc *tracker.Service, authn *auth.Authenticator, broker *events.Broker,
	idem func(http.Handler) http.Handler, pool *pgxpool.Pool, log *slog.Logger) *Server {
	return &Server{svc: svc, authn: authn, broker: broker, idem: idem, pool: pool, log: log, notify: notify.New(pool),
		mintLimit:        newMintLimiter(time.Now),
		gitLimit:         newGitLimiter(time.Now),
		unsub:            newUnsubLimiter(time.Now),
		gitBudget:        newByteBudget(DefaultGitWebhookBudget),
		gitReadDeadline:  DefaultGitWebhookReadDeadline,
		gitWarnAt:        map[string]time.Time{},
		gitWarnNow:       time.Now,
		UnsubscribeFloor: DefaultUnsubscribeFloor,
		SSEHeartbeat:     15 * time.Second, SSEPoll: 10 * time.Second, SecureCookies: true}
}

// Routes builds the route tree.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	// The authenticator reads the same cookie names this server sets.
	// SecureCookies must be set BEFORE Routes() is called: the names are
	// chosen here, once, from its value.
	s.authn.UseCookieNames(s.cookieNames())
	r.Use(s.baseMiddleware()...)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "method not allowed")
	})

	r.Get("/healthz", s.health) // public

	// Idempotency (s.idem) sits AFTER each route's scope check, so a stored
	// response is only ever replayed to a token allowed to make the request.
	read := auth.RequireScope(auth.ScopeRead)
	write := auth.RequireScope(auth.ScopeWrite)
	admin := auth.RequireScope(auth.ScopeAdmin)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) { // public
			w.Header().Set("Content-Type", "application/yaml")
			_, _ = w.Write(openapi.OpenAPI)
		})
		// Login (public). These routes never read a session cookie, so a
		// cookie cannot stand in for the code. Never idempotent: the verify
		// response carries the session.
		r.With(idempotency.Reject).Post("/auth/otp", s.requestLoginCode)
		r.With(idempotency.Reject).Post("/auth/otp/verify", s.verifyLoginCode)
		// First-run setup (public). The link token in the body is
		// the credential; the response carries a session and the first
		// admin token, so it is never idempotent. It reads no cookie.
		r.Get("/setup", s.getSetup)
		r.With(idempotency.Reject).Post("/setup", s.completeSetup)
		// Restore a bundle into the empty install (LAUN-249, public). The setup
		// link (X-Launchpad-Setup-Token) is the credential and is checked before
		// the body is read; no limiter and no Idempotency-Key middleware sit in
		// front of it (idempotency.Reject only refuses the header: the answer
		// holds a session and a token). It streams, so it reads no JSON.
		r.With(idempotency.Reject).Post("/setup/restore", s.restoreMove)
		// Sign-up requests and invite acceptance (public;
		// decisions-T12). The /auth/otp shape: JSON only, never
		// idempotent (accept returns a session), a floor and limits, an
		// answer that says nothing about who exists. They read no cookie.
		r.With(idempotency.Reject).Post("/signups", s.requestSignup)
		r.With(idempotency.Reject).Post("/signups/confirm", s.confirmSignup)
		r.With(idempotency.Reject).Post("/invites/accept", s.acceptInvite)
		// One-click unsubscribe of a notification email (LAUN-34, public;
		// decisions-T13 #25). The token in the path is the credential: never
		// logged (logPath), never idempotency-stored, 204 for any token.
		// GET is not registered: it answers 405 and does nothing.
		r.With(idempotency.Reject).Post("/notifications/unsubscribe/{token}", s.unsubscribeNotifications)

		// GitLab webhook (T5). Public on purpose: GitLab sends no
		// bearer token or cookie, it proves itself with X-Gitlab-Token, which
		// the handler checks (constant time) before it reads the body. It is
		// outside Authenticate and RequireOrgMember, and no rate limiter sits
		// in front of it, so a burst of real events is never refused
		// (decisions-T5). No Idempotency-Key middleware: its idempotency is
		// gitlab.DeliveryKeys (GitLab's own Idempotency-Key when sent AND the
		// Event UUID, each plus the MR; both are recorded, either seen before
		// is a duplicate), and nothing may answer before the token check.
		r.Post("/orgs/{org}/integrations/gitlab/webhook", s.gitlabWebhook)

		// GitHub webhook (LAUN-5, T17 #14). Public like the GitLab one, but
		// GitHub signs the BODY, so the handler reads it before the check:
		// org and integration lookup, signature header shape, Content-Length
		// (411, 413), the in-flight byte budget (503), a read deadline, then
		// the HMAC. No rate limiter and no Idempotency-Key middleware in front
		// (its idempotency is the delivery GUID, github.DeliveryKeys).
		r.Post("/orgs/{org}/integrations/github/webhook", s.githubWebhook)

		// Bitbucket webhook (LAUN-84, T17 #14), Cloud and Data Center. Signed
		// body like GitHub's (X-Hub-Signature), the same handler and order.
		r.Post("/orgs/{org}/integrations/bitbucket/webhook", s.bitbucketWebhook)

		r.Group(func(r chi.Router) {
			r.Use(s.authn.Authenticate)
			r.Post("/auth/logout", s.logout)
			r.With(read).Get("/auth/session", s.getSession)
			r.With(read).Get("/me", s.getMe)
			r.With(read).Get("/orgs", s.listOrgs)

			// The caller's own login sessions (LAUN-130, decisions-T6a "Sessions list"):
			// human only, user-private rows (no event, decisions-T13 #32). Revoke is
			// replay-safe by shape (a second DELETE is 204; "revoke-others" revokes
			// whatever is live), so no Idempotency-Key is mounted. Cookie writes need CSRF.
			r.With(read, refuseAgents).Get("/me/sessions", s.listSessions)
			r.With(write, refuseAgents).Post("/me/sessions/revoke-others", s.revokeOtherSessions)
			r.With(write, refuseAgents).Delete("/me/sessions/{id}", s.revokeSession)

			// Display preferences (LAUN-235, decisions-T42-themes.md): the theme,
			// per user and install-wide, so it follows the person across devices
			// and orgs. Human only, own row, no event (decisions-T23 #4). The PUT
			// replaces the value, so a repeat is a no-op; s.idem is mounted after
			// the scope check (it acts on POST only today).
			r.With(read, refuseAgentsPrefs).Get("/me/preferences", s.getPreferences)
			r.With(write, refuseAgentsPrefs, s.idem).Put("/me/preferences", s.putPreferences)

			// Platform settings: the whole install, so outside
			// /orgs/{org}. Platform admin only (an unpinned admin token of a
			// platform admin, or their session). Never idempotent: the PUT
			// body holds secrets, and the test sends an email.
			r.Route("/platform", func(r chi.Router) {
				r.Use(auth.RequirePlatformAdmin)
				r.Get("/settings", s.getPlatformSettings)
				r.With(idempotency.Reject).Put("/settings", s.updatePlatformSettings)
				r.With(idempotency.Reject).Post("/mail/test", s.testPlatformMail)
				// Platform admins. The last active one cannot be
				// removed (an advisory lock serialises removals).
				r.Get("/admins", s.listPlatformAdmins)
				r.With(idempotency.Reject).Post("/admins", s.addPlatformAdmin)
				r.Delete("/admins/{user}", s.removePlatformAdmin)

				// Move the install (LAUN-249, decisions-T46): a human platform admin
				// only (an agent is refused even with the flag). The POST holds the
				// passphrase and the ticket, so it is never idempotent; the GET
				// streams the bundle once. No event.
				noAgents := refuseAgentsWith("moving an install is not available to agent users")
				r.With(noAgents, idempotency.Reject).Post("/move/export", s.startMoveExport)
				r.With(noAgents).Get("/move/export/{ticket}", s.downloadMoveExport)

				// Scheduled backups (LAUN-69, decisions-T16 #43). They dial nothing:
				// probe, test, exchange and run only queue rows for offsite. Every
				// write is JSON-only with idempotency.Reject, and emits no event
				// (the record is backup_change_log).
				r.Route("/backups", func(r chi.Router) {
					rej := idempotency.Reject
					r.Get("/settings", s.getBackupSettings)
					r.With(rej).Put("/settings", s.updateBackupSettings)
					r.Get("/status", s.getBackupStatus)
					r.Get("/recipients", s.listBackupRecipients)
					r.With(rej).Post("/recipients", s.addBackupRecipient)
					r.With(rej).Delete("/recipients/{fingerprint}", s.removeBackupRecipient)
					r.With(rej).Post("/recipients/{fingerprint}/cancel", s.cancelBackupRecipient)
					r.Get("/destinations", s.listBackupDestinations)
					r.With(rej).Post("/destinations", s.createBackupDestination)
					r.With(rej).Post("/destinations/probe", s.probeUnsavedBackupDestination)
					r.With(rej).Post("/destinations/test", s.testUnsavedBackupDestination)
					r.Get("/destinations/{id}", s.getBackupDestination)
					r.With(rej).Patch("/destinations/{id}", s.updateBackupDestination)
					r.With(rej).Delete("/destinations/{id}", s.deleteBackupDestination)
					r.With(rej).Post("/destinations/{id}/probe", s.probeBackupDestination)
					r.With(rej).Post("/destinations/{id}/test", s.testBackupDestination)
					r.With(rej).Post("/destinations/{id}/exchange", s.exchangeBackupDestination)
					r.With(rej).Post("/destinations/{id}/run", s.runBackupDestination)
					r.Get("/runs", s.listBackupRuns)
					r.Get("/runs/{id}", s.getBackupRun)
					r.Get("/changes", s.listBackupChanges)
				})
			})

			r.Route("/orgs/{org}", func(r chi.Router) {
				r.Use(s.authn.RequireOrgMember)

				r.With(read).Get("/", s.getOrg)
				// LAUN-56: org settings and the first-run checklist (org admin).
				r.With(admin, s.idem).Patch("/", s.updateOrg)
				// LAUN-22 avatars (decisions-T24): admin sets the org's, write sets a
				// project's; prepare holds a credential, so it is never idempotent.
				r.With(read).Get("/avatar", s.getOrgAvatar)
				r.With(admin, idempotency.Reject).Post("/avatar/prepare", s.prepareOrgAvatar)
				r.With(admin).Put("/avatar", s.setOrgAvatar)
				r.With(admin).Delete("/avatar", s.clearOrgAvatar)
				r.With(admin).Get("/onboarding", s.getOnboarding)
				r.With(admin, s.idem).Put("/onboarding/skip", s.setOnboardingSkip)

				// LAUN-200 (decisions-T31): guidance text for agents. Any token
				// reads it; an org admin writes it. PUT and DELETE are idempotent
				// by shape, so no Idempotency-Key.
				r.With(read).Get("/agent-guidance", s.getAgentGuidance)
				r.With(admin).Put("/agent-guidance", s.putAgentGuidance)
				r.With(admin).Delete("/agent-guidance", s.deleteAgentGuidance)

				// LAUN-204 (decisions-T36): the org's chat services (Slack, Discord,
				// custom). Admin scope and no agent token. The POST and the PATCH can
				// hold a webhook URL, a secret, so they are never idempotent-stored;
				// the DELETE is idempotent by shape; the test posts a message, so a
				// replay must not be answered from a store.
				r.With(admin, refuseAgents).Get("/chat-services", s.listChat)
				r.With(admin, refuseAgents, idempotency.Reject).Post("/chat-services", s.createChat)
				r.With(admin, refuseAgents).Get("/chat-services/{service}", s.getChat)
				r.With(admin, refuseAgents, idempotency.Reject).Patch("/chat-services/{service}", s.patchChat)
				r.With(admin, refuseAgents).Delete("/chat-services/{service}", s.deleteChat)
				r.With(admin, refuseAgents, idempotency.Reject).Post("/chat-services/{service}/test", s.testChat)

				// LAUN-197 (decisions-T35): the org's push relay. Admin scope and no
				// agent token. The PUT holds the relay URL and secret, so it is never
				// idempotent-stored; the test sends a push, so a replay must not be
				// answered from a store.
				r.With(admin, refuseAgents).Get("/push", s.getPush)
				r.With(admin, refuseAgents, idempotency.Reject).Put("/push", s.putPush)
				r.With(admin, refuseAgents).Delete("/push", s.deletePush)
				r.With(admin, refuseAgents, idempotency.Reject).Post("/push/test", s.testPush)

				r.With(read).Get("/teams", s.listTeams)
				r.With(admin, s.idem).Post("/teams", s.createTeam)
				r.With(read).Get("/teams/{team}", s.getTeam)
				r.With(admin).Patch("/teams/{team}", s.updateTeam)
				r.With(admin).Delete("/teams/{team}", s.deleteTeam)
				r.With(read).Get("/teams/{team}/states", s.listStates)
				r.With(read).Get("/teams/{team}/products", s.listTeamProducts)
				// Git workflow rules (LAUN-244, decisions-T44): members read, org
				// admins replace; agents get neither.
				r.With(read, refuseGitWorkflowAgents).Get("/teams/{team}/git-workflow", s.getGitWorkflow)
				r.With(admin, refuseGitWorkflowAgents, s.idem).Put("/teams/{team}/git-workflow", s.putGitWorkflow)

				// Products (LAUN-62, decisions-T18 #16): a group of teams.
				// Admin scope writes, like teams; Idempotency-Key on the
				// create only (the PUTs and DELETEs are idempotent by shape).
				r.With(read).Get("/products", s.listProducts)
				r.With(admin, s.idem).Post("/products", s.createProduct)
				r.With(read).Get("/products/{product}", s.getProduct)
				r.With(admin).Patch("/products/{product}", s.updateProduct)
				r.With(admin).Delete("/products/{product}", s.deleteProduct)
				r.With(admin).Put("/products/{product}/teams", s.setProductTeams)
				r.With(admin).Put("/products/{product}/teams/{team}", s.addProductTeam)
				r.With(admin).Delete("/products/{product}/teams/{team}", s.removeProductTeam)

				// T3 planning objects. Cycles are created by the cycle job only (options: decisions-T27).
				r.With(read).Get("/projects", s.listProjects)
				r.With(write, s.idem).Post("/projects", s.createProject)
				r.With(read).Get("/projects/{project}", s.getProject)
				r.With(write).Patch("/projects/{project}", s.updateProject)
				r.With(admin).Delete("/projects/{project}", s.deleteProject)
				// LAUN-194 (decisions-T28): project labels. Reading is read scope; the
				// label set itself is admin (a catalog, like teams and products).
				r.With(read).Get("/project-labels", s.listProjectLabels)
				r.With(admin, s.idem).Post("/project-labels", s.createProjectLabel)
				r.With(admin).Patch("/project-labels/{label}", s.updateProjectLabel)
				r.With(admin).Delete("/project-labels/{label}", s.deleteProjectLabel)
				r.With(read).Get("/projects/{project}/avatar", s.getProjectAvatar)
				r.With(write, idempotency.Reject).Post("/projects/{project}/avatar/prepare", s.prepareProjectAvatar)
				r.With(write).Put("/projects/{project}/avatar", s.setProjectAvatar)
				r.With(write).Delete("/projects/{project}/avatar", s.clearProjectAvatar)

				// LAUN-196 (decisions-T30): project updates, create/list/read.
				r.With(read).Get("/projects/{project}/updates", s.listProjectUpdates)
				r.With(write, s.idem).Post("/projects/{project}/updates", s.createProjectUpdate)
				r.With(read).Get("/projects/{project}/updates/{update}", s.getProjectUpdate)

				// LAUN-195 project statuses (decisions-T29): read to list, admin to change. The reorder is a PUT of
				// the whole order, so it is idempotent by shape (no Idempotency-Key); the create takes one.
				r.With(read).Get("/project-statuses", s.listProjectStatuses)
				r.With(admin, s.idem).Post("/project-statuses", s.createProjectStatus)
				r.With(admin).Put("/project-statuses/order", s.reorderProjectStatuses)
				r.With(admin).Patch("/project-statuses/{status}", s.updateProjectStatus)
				r.With(admin).Delete("/project-statuses/{status}", s.deleteProjectStatus)

				r.With(read).Get("/milestones", s.listMilestones)
				r.With(write, s.idem).Post("/milestones", s.createMilestone)
				r.With(read).Get("/milestones/{milestone}", s.getMilestone)
				r.With(write).Patch("/milestones/{milestone}", s.updateMilestone)

				r.With(read).Get("/cycles", s.listCycles)
				r.With(read).Get("/cycles/{cycle}", s.getCycle)
				r.With(write).Patch("/cycles/{cycle}", s.updateCycle)
				// LAUN-172 (decisions-T27): Mark complete and Delete.
				r.With(admin, s.idem).Post("/cycles/{cycle}/complete", s.completeCycle)
				r.With(admin).Delete("/cycles/{cycle}", s.deleteCycle)
				r.With(read).Get("/cycles/{cycle}/snapshots", s.listCycleSnapshots)

				r.With(read).Get("/documents", s.listDocuments)
				r.With(write, s.idem).Post("/documents", s.createDocument)
				r.With(read).Get("/documents/{document}", s.getDocument)
				r.With(write).Patch("/documents/{document}", s.updateDocument)
				r.With(read).Get("/documents/{document}/comments", s.listDocumentComments)
				r.With(write, s.idem).Post("/documents/{document}/comments", s.createDocumentComment)

				r.With(read).Get("/labels", s.listLabels)
				r.With(write, s.idem).Post("/labels", s.createLabel)

				r.With(read).Get("/issues", s.listIssues)
				r.With(write, s.idem).Post("/issues", s.createIssue)
				r.With(read).Get("/issues/{issue}", s.getIssue)
				r.With(write).Patch("/issues/{issue}", s.updateIssue)
				// LAUN-201 (decisions-T32): a hard delete, admin scope, no Idempotency-Key (a repeat is 404).
				r.With(admin).Delete("/issues/{issue}", s.deleteIssue)

				r.With(read).Get("/issues/{issue}/activity", s.listActivity)
				r.With(read).Get("/issues/{issue}/comments", s.listComments)
				r.With(write, s.idem).Post("/issues/{issue}/comments", s.createComment)
				r.With(write).Patch("/comments/{comment}", s.updateComment)
				r.With(write).Delete("/comments/{comment}", s.deleteComment)

				// Attachments. prepare is never idempotent: its
				// response holds a presigned upload URL (a credential).
				r.With(write, idempotency.Reject).Post("/attachments/prepare", s.prepareAttachmentUpload)
				r.With(read).Get("/issues/{issue}/attachments", s.listAttachments)
				r.With(write, s.idem).Post("/issues/{issue}/attachments", s.createAttachment)
				r.With(read).Get("/attachments/{attachment}", s.getAttachment)
				r.With(read).Get("/attachments/{attachment}/download", s.downloadAttachment)

				r.With(read).Get("/issues/{issue}/relations", s.listRelations)
				r.With(write, s.idem).Post("/issues/{issue}/relations", s.createRelation)
				r.With(write).Delete("/issues/{issue}/relations/{relation}", s.deleteRelation)

				r.With(read).Get("/users", s.listUsers)
				// Team access of a member (decisions-T20 #48, LAUN-225): org
				// admins only. A PUT replaces the whole set, so a repeat is a
				// no-op; s.idem is mounted after the scope check (it acts on
				// POST only today). The body and the answer hold no secret.
				r.With(admin, s.idem).Put("/users/{user}/team-access", s.setTeamAccess)

				// Notification preferences, team follows and issue
				// subscriptions (LAUN-32, decisions-T13 #32): the caller's own
				// rows; agent tokens get 403. The PUT and DELETE routes are
				// replay-safe by themselves (the note, #32). s.idem is mounted
				// after the scope check as the AC asks, but today it acts on
				// POST only, so a key on these routes is ignored.
				r.With(read, refuseAgents).Get("/me/notification-settings", s.getNotificationSettings)
				r.With(write, refuseAgents, s.idem).Put("/me/notification-settings", s.updateNotificationSettings)
				r.With(write, refuseAgents, s.idem).Put("/me/notification-settings/teams/{team}", s.updateTeamNotificationSettings)
				r.With(write, refuseAgents, s.idem).Put("/me/teams/{team}/follow", s.followTeam)
				r.With(write, refuseAgents, s.idem).Delete("/me/teams/{team}/follow", s.unfollowTeam)
				// Remembered issue-page views (LAUN-141): the caller's own rows, no event.
				r.With(read, refuseAgents).Get("/me/views", s.listViews)
				r.With(write, refuseAgents, s.idem).Put("/me/views/{place}", s.putView)
				// Connected accounts (LAUN-199, decisions-T34): the caller's own git
				// handle per connection; agents refused, no event. PUT and DELETE are
				// replay-safe by shape. The admin pair lists and removes links.
				r.With(read, refuseAgents).Get("/me/git-accounts", s.listMyGitAccounts)
				r.With(write, refuseAgents).Put("/me/git-accounts/{integration}", s.putMyGitAccount)
				r.With(write, refuseAgents).Delete("/me/git-accounts/{integration}", s.deleteMyGitAccount)
				// Push devices (LAUN-197, decisions-T35): the caller's own devices; agents
				// refused, no event. The PUT holds the device token, so it is never
				// idempotent-stored; the DELETE is replay-safe by shape.
				r.With(read, refuseAgents).Get("/me/push-devices", s.listPushDevices)
				r.With(write, refuseAgents, idempotency.Reject).Put("/me/push-devices/{device}", s.putPushDevice)
				r.With(write, refuseAgents).Delete("/me/push-devices/{device}", s.deletePushDevice)
				// Personal webhooks (LAUN-229, decisions-T40): the caller's own rows;
				// agents refused, no event. The POST, the PATCH and the test hold
				// or use the secret URL, so they are never idempotent-stored; the
				// DELETE is replay-safe by shape.
				r.With(read, refuseAgents).Get("/me/webhooks", s.listPersonalWebhooks)
				r.With(write, refuseAgents, idempotency.Reject).Post("/me/webhooks", s.createPersonalWebhook)
				r.With(write, refuseAgents, idempotency.Reject).Patch("/me/webhooks/{webhook}", s.patchPersonalWebhook)
				r.With(write, refuseAgents).Delete("/me/webhooks/{webhook}", s.deletePersonalWebhook)
				r.With(write, refuseAgents, idempotency.Reject).Post("/me/webhooks/{webhook}/test", s.testPersonalWebhook)
				r.With(admin).Get("/git-identities", s.listGitIdentities)
				r.With(admin).Delete("/git-identities/{identity}", s.deleteGitIdentity)
				r.With(read, refuseAgents).Get("/issues/{issue}/subscription", s.getIssueSubscription)
				r.With(write, refuseAgents, s.idem).Put("/issues/{issue}/subscription", s.putIssueSubscription)
				r.With(write, refuseAgents, s.idem).Delete("/issues/{issue}/subscription", s.deleteIssueSubscription)
				// The inbox (LAUN-35, decisions-T13 #27-#31): the caller's own
				// rows; agent tokens get 403. PATCH and read-all set a state, so
				// a replay is safe without an Idempotency-Key.
				r.With(read, refuseAgents).Get("/notifications", s.listInbox)
				r.With(read, refuseAgents).Get("/notifications/unread-count", s.inboxUnreadCount)
				r.With(read, refuseAgents).Get("/notifications/stream", s.streamInbox)
				r.With(write, refuseAgents).Post("/notifications/read-all", s.readAllInbox)
				r.With(write, refuseAgents).Patch("/notifications/{id}", s.patchInbox)
				// Agent users (decisions-T10). JSON only; never
				// idempotent (a retried create is a plain 409, and nothing of
				// the body is stored in an idempotency row).
				r.With(admin, idempotency.Reject).Post("/agents", s.createAgent)
				// Delete an agent (decisions-T47): admin scope, 204, no Idempotency-Key
				// (a repeat is a 404); a human target is a 404.
				r.With(admin).Delete("/agents/{user}", s.deleteAgent)

				// Sign-up requests and invites (decisions-T12): org
				// admins only. Approve and reject are replay-safe by
				// themselves (a second approve makes no second user), and
				// their bodies hold no secret, so they take Idempotency-Key.
				// Creating an invite returns the token once: never idempotent.
				r.With(admin).Get("/signups", s.listSignups)
				r.With(admin).Delete("/signups/unconfirmed", s.purgeUnconfirmedSignups)
				r.With(admin, s.idem).Post("/signups/{signup}/approve", s.approveSignup)
				r.With(admin, s.idem).Post("/signups/{signup}/reject", s.rejectSignup)
				r.With(admin).Get("/invites", s.listInvites)
				r.With(admin, idempotency.Reject).Post("/invites", s.createInvite)
				r.With(admin).Delete("/invites/{invite}", s.revokeInvite)

				r.With(admin).Get("/tokens", s.listTokens)
				// Never idempotent: the response carries a token secret, which must
				// not be stored or replayed.
				r.With(admin, idempotency.Reject).Post("/tokens", s.createToken)
				r.With(admin).Delete("/tokens/{token}", s.revokeToken)

				// Git integration config (T17, LAUN-7): admin scope AND the org
				// admin role, GETs included. Secrets and tokens are write-only;
				// create is never idempotent (the body holds them). check and the
				// create / PATCH that call the host are rate limited (#28).
				r.With(admin).Get("/integrations", s.listGitIntegrations)
				r.With(admin, idempotency.Reject).Post("/integrations", s.createGitIntegration)
				r.With(admin).Get("/integrations/{integration}", s.getGitIntegration)
				r.With(admin, idempotency.Reject).Post("/integrations/{integration}/check", s.checkGitIntegration)
				r.With(admin).Get("/integrations/{integration}/deliveries", s.listGitDeliveries) // the ring (LAUN-87)
				r.With(admin, idempotency.Reject).Patch("/integrations/{integration}", s.updateGitIntegration)
				r.With(admin).Delete("/integrations/{integration}", s.deleteGitIntegration) // LAUN-104

				// Secret rotation (T17 #11, LAUN-86). rotate-secret returns the new
				// secret once in webhook-only mode, so it is never idempotent; commit
				// and discard hold no secret and are safe to repeat.
				r.With(admin, idempotency.Reject).Post("/integrations/{integration}/rotate-secret", s.rotateGitSecret)
				r.With(admin).Delete("/integrations/{integration}/rotate-secret", s.discardGitSecret)
				r.With(admin).Post("/integrations/{integration}/rotate-secret/commit", s.commitGitSecret)

				// The repo map (T17 routes table, LAUN-86). {repo} is the map row id.
				r.With(admin).Get("/integrations/{integration}/available-repos", s.availableGitRepos)
				r.With(admin).Get("/integrations/{integration}/repos", s.listGitRepos)
				r.With(admin, s.idem).Post("/integrations/{integration}/repos", s.createGitRepo)
				r.With(admin).Patch("/integrations/{integration}/repos/{repo}", s.updateGitRepo)
				r.With(admin).Delete("/integrations/{integration}/repos/{repo}", s.deleteGitRepo)
				// MR state (LAUN-83): token mode, rate limited (#28). The refresh is
				// safe to repeat; the backfill takes an Idempotency-Key.
				r.With(admin).Post("/integrations/{integration}/repos/{repo}/hook/refresh", s.refreshGitHook)
				r.With(admin, s.idem).Post("/integrations/{integration}/repos/{repo}/backfill", s.backfillGitRepo)

				// The Linear import (LAUN-8, decisions-T17 #41): org admins
				// only, GETs included (the user list holds Linear emails).
				// The export carries the Linear API key: never idempotent.
				// The other starts are replay-safe by their state rules; a
				// stored job body holds no secret.
				r.With(admin, s.idem).Post("/import/sessions", s.startImportSession)
				r.With(admin).Get("/import/sessions/current", s.getCurrentImportSession)
				r.With(admin, idempotency.Reject).Post("/import/sessions/{session}/export", s.startImportExport)
				r.With(admin).Get("/import/sessions/{session}/users", s.listImportUsers)
				r.With(admin).Put("/import/sessions/{session}/usermap", s.putImportUserMap)
				r.With(admin, s.idem).Post("/import/sessions/{session}/dry-run", s.startImportDryRun)
				r.With(admin, s.idem).Post("/import/sessions/{session}/run", s.startImportRun)
				r.With(admin, s.idem).Post("/import/sessions/{session}/verify", s.startImportVerify)
				r.With(admin).Delete("/import/sessions/{session}", s.closeImportSession)
				r.With(admin).Get("/import/jobs/{job}", s.getImportJob)
				r.With(admin, s.idem).Post("/import/jobs/{job}/cancel", s.cancelImportJob)

				r.With(read).Get("/events", s.listEvents)
				r.With(read).Get("/events/stream", s.streamEvents)
			})
		})
	})
	return r
}

// fail writes err, logging it when it is an internal error.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	// A deadlock or serialization failure answers 503 (database.Retryable); it
	// is not an internal error, so log it here to let an operator see the rate.
	var api *httpx.Error
	if errors.As(err, &api) && api.Status == http.StatusServiceUnavailable && api.Code == httpx.CodeOverloaded {
		s.log.WarnContext(r.Context(), "request collided with a concurrent write; answered 503", "method", r.Method, "path", logPath(r), "code", api.Code)
	}
	if httpx.WriteErr(w, err) {
		s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", logPath(r), "err", err)
	}
}

// ctxOrgPrincipal returns the values the middleware chain guarantees.
func ctxOrgPrincipal(r *http.Request) (*auth.Org, *auth.Principal) {
	o, _ := auth.OrgFrom(r.Context())
	p, _ := auth.PrincipalFrom(r.Context())
	return o, p
}
