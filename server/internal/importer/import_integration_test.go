package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// The importer against a real, freshly migrated database with the test fixture
// (INTEGRATION=1). The fixture is the hand-made workspace in fixture_test.go.

type impEnv struct {
	t     *testing.T
	store *storage.S3
	dir   string // the export directory the files are read from
	pool  *pgxpool.Pool
	q     *dbq.Queries
	org   *auth.Org
	chen  *auth.Principal
	svc   *tracker.Service
}

func newImpEnv(t *testing.T) *impEnv {
	t.Helper()
	pool, _ := testdb.New(t)
	ctx := context.Background()
	testdb.Fixture(t, pool)
	q := dbq.New(pool)
	o, err := q.GetOrgBySlug(ctx, testdb.OrgSlug)
	if err != nil {
		t.Fatal(err)
	}
	u, err := q.GetUserByUsername(ctx, "chen")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for rel, body := range fixtureFiles {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &impEnv{t: t, pool: pool, q: q, svc: tracker.New(pool), store: storagetest.New(t), dir: dir,
		org:  &auth.Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Role: "admin", Access: auth.TeamAccess{All: true}},
		chen: &auth.Principal{UserID: u.ID, Username: u.Username, Name: u.Name, Scopes: []auth.Scope{auth.ScopeAdmin}}}
}

func (e *impEnv) run(ex *Export, dry bool) (*Report, error) {
	e.t.Helper()
	p, err := BuildPlan(ex, fixtureUserMap(), map[string]int32{"ORB": 3405})
	if err != nil {
		e.t.Fatal(err)
	}
	return Run(context.Background(), e.pool, ex, p, e.opts(dry))
}

func (e *impEnv) opts(dry bool) Options {
	return Options{Org: testdb.OrgSlug, DryRun: dry, Store: e.store, ExportDir: e.dir, PublicBaseURL: "https://tracker.example"}
}

func (e *impEnv) mustRun(ex *Export, dry bool) *Report {
	e.t.Helper()
	rep, err := e.run(ex, dry)
	if err != nil {
		e.t.Fatalf("Run(dry=%v): %v", dry, err)
	}
	return rep
}

func (e *impEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// LAUN-51: Linear's sortOrder is kept on the first import, and a re-run
// never undoes a later drag (decisions-T14).
func TestRun_keepsLinearSortOrderAndNeverResetsADrag(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	ex := fixtureExport()
	e.mustRun(ex, false)
	is, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-2557", tracker.Includes{})
	if err != nil {
		t.Fatal(err)
	}
	if is.SortOrder != ex.Issues[0].SortOrder || is.SortOrder != -12.5 {
		t.Fatalf("sortOrder %v, want Linear's %v", is.SortOrder, ex.Issues[0].SortOrder)
	}
	// A drag after the cutover, then the same export again.
	if _, err := e.svc.UpdateIssue(ctx, e.org, e.chen, "ORB-2557", tracker.UpdateIssueInput{SortOrder: new(float64)}); err != nil {
		t.Fatal(err)
	}
	rep := e.mustRun(ex, false)
	if bad := Mismatches(rep.Verify); len(bad) > 0 {
		t.Fatalf("verification: %+v", bad)
	}
	again, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-2557", tracker.Includes{})
	if err != nil {
		t.Fatal(err)
	}
	if again.SortOrder != 0 {
		t.Fatalf("a re-run put sortOrder back to %v", again.SortOrder)
	}

	// A re-import that changes an issue's state puts it on top of the new
	// column (its old rank means nothing there).
	ex2 := fixtureExport()
	ex2.Issues[0].State = ref(fxStTodo) // Done -> Todo, where ORB-... already sit
	var topBefore float64
	if err := e.pool.QueryRow(ctx, `SELECT min(i.sort_order) FROM issues i JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
		WHERE i.org_id = $1 AND s.name = 'Todo'`, e.org.ID).Scan(&topBefore); err != nil {
		t.Fatal(err)
	}
	e.mustRun(ex2, false)
	moved, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-2557", tracker.Includes{})
	if err != nil {
		t.Fatal(err)
	}
	if moved.State.Name != "Todo" || moved.SortOrder != topBefore-1024 {
		t.Fatalf("after a state change in the export: %s, sortOrder %v, want Todo and %v", moved.State.Name, moved.SortOrder, topBefore-1024)
	}
}

func TestRun_dryRunWritesNothing(t *testing.T) {
	e := newImpEnv(t)
	before := e.count(`SELECT count(*) FROM labels`)
	rep := e.mustRun(fixtureExport(), true)
	if !rep.DryRun {
		t.Error("report must say dry run")
	}
	if c := rep.Counts[EntIssue]; c.Create != 4 || c.Excluded != 1 || c.Export != 5 {
		t.Errorf("issue counts %+v, want export 5, excluded 1, create 4", *c)
	}
	if bad := Mismatches(rep.Verify); len(bad) > 0 {
		t.Errorf("the dry run's own verification failed: %+v", bad)
	}
	for table, want := range map[string]int{"issues": 0, "comments": 0, "cycles": 0, "import_mappings": 0, "documents": 0} {
		if n := e.count(`SELECT count(*) FROM ` + table); n != want {
			t.Errorf("%s has %d rows after a dry run", table, n)
		}
	}
	if n := e.count(`SELECT count(*) FROM labels`); n != before {
		t.Errorf("labels %d -> %d after a dry run", before, n)
	}
}

func TestRun_importsWithOriginalTimestampsAndIsIdempotent(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	ex := fixtureExport()
	rep := e.mustRun(ex, false)

	if bad := Mismatches(rep.Verify); len(bad) > 0 {
		t.Fatalf("verification: %+v", bad)
	}
	want := map[string]Counts{
		EntTeam: {Export: 1, Update: 1}, // the seed's ORB, adopted
		// Triage, Backlog, Todo, In Progress, Done and Duplicate are all the
		// seed's (adopted by name: update); Triage since LAUN-20.
		EntState:      {Export: 6, Update: 6},
		EntLabel:      {Export: 5, Create: 3, Update: 2},   // Bug adopted, bug merged, Area, Backend and the team label Flaky new
		EntIssue:      {Export: 5, Excluded: 1, Create: 4}, // ORB-100 trashed
		EntCycle:      {Export: 3, Create: 3},              // 7, 8, 10
		EntSnapshot:   {Export: 3, Create: 3},
		EntComment:    {Export: 8, Excluded: 4, Create: 4},
		EntRelation:   {Export: 5, Excluded: 1, Create: 3, Update: 1}, // r3 merged into r2
		EntAttachment: {Export: 3, Excluded: 1, Create: 2},            // a2 is the file row of fxUploadA
		EntFile:       {Export: 2, Create: 2},                         // a2's and I3's text-only CSV
	}
	for ent, w := range want {
		if got := *rep.Counts[ent]; got != w {
			t.Errorf("%s counts %+v, want %+v", ent, got, w)
		}
	}
	if rep.PendingUploads != 0 || rep.Uploads.Uploaded != 2 || rep.Uploads.Present != 0 {
		t.Errorf("uploads %+v pending %d, want 2 uploaded and none left", rep.Uploads, rep.PendingUploads)
	}

	// get_issue description, comments and history, all with the
	// original timestamps.
	inc := tracker.Includes{Comments: true, StateHistory: true, Relations: true, Attachments: true}
	is, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-2557", inc)
	if err != nil {
		t.Fatal(err)
	}
	src := ex.Issues[0]
	// The description is Linear's, with the upload URL rewritten (checked
	// exactly in TestRun_storesFilesAndRewritesEveryText).
	if is.Title != src.Title || !strings.HasPrefix(is.Description, "Needs `-p`. See ![x](https://tracker.example/api/v1/orgs/orbit/attachments/") ||
		strings.Contains(is.Description, "uploads.linear.app") || !is.CreatedAt.Equal(src.CreatedAt) ||
		!is.UpdatedAt.Equal(src.UpdatedAt) || is.CompletedAt == nil || !is.CompletedAt.Equal(*src.CompletedAt) {
		t.Errorf("issue fields: %+v", is)
	}
	if !V7Time(is.ID).Equal(src.CreatedAt.Truncate(time.Millisecond)) {
		t.Errorf("issue id time %s, want createdAt %s", V7Time(is.ID), src.CreatedAt)
	}
	if is.State.Name != "Done" || is.Priority != 2 || is.Estimate == nil || *is.Estimate != 3 ||
		is.Assignee == nil || is.Assignee.Username != "chen" || is.DueDate == nil || *is.DueDate != "2026-09-05" {
		t.Errorf("issue mapping: state %s priority %d estimate %v assignee %v due %v",
			is.State.Name, is.Priority, is.Estimate, is.Assignee, is.DueDate)
	}
	if len(is.Comments) != 2 || is.Comments[0].Body != "First" || !is.Comments[0].CreatedAt.Equal(ts("2026-09-01T11:00:00Z")) ||
		is.Comments[1].ParentID == nil || *is.Comments[1].ParentID != is.Comments[0].ID ||
		is.Comments[1].Author == nil || is.Comments[1].Author.Username != "claude" {
		t.Errorf("comments %+v", is.Comments)
	}
	if len(is.StateHistory) != 3 || is.StateHistory[0].FromStateID != nil || is.StateHistory[2].ToStateName != "Done" ||
		!is.StateHistory[1].CreatedAt.Equal(ts("2026-09-01T10:00:00Z")) || is.StateHistory[1].Source != "import" {
		t.Errorf("state history %+v", is.StateHistory)
	}
	labelNames := []string{}
	for _, l := range is.Labels {
		labelNames = append(labelNames, l.Name)
	}
	if strings.Join(labelNames, ",") != "Backend,Bug" && strings.Join(labelNames, ",") != "Bug,Backend" {
		t.Errorf("labels %v, want Bug (merged) and Backend", labelNames)
	}
	if n := e.count(`SELECT count(*) FROM labels WHERE org_id = $1 AND lower(name) = 'bug'`, e.org.ID); n != 1 {
		t.Errorf("%d Bug labels, want 1 (the seed's, adopted)", n)
	}
	if len(is.Attachments) != 2 {
		t.Errorf("%d attachments, want 2", len(is.Attachments))
	}
	// LAUN-54: Linear's team label keeps its team; the org labels have none.
	if n := e.count(`SELECT count(*) FROM labels l JOIN teams t ON t.org_id = l.org_id AND t.id = l.team_id
	                 WHERE l.org_id = $1 AND l.name = 'Flaky' AND t.key = 'ORB'`, e.org.ID); n != 1 {
		t.Errorf("%d Flaky labels on team ORB, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM labels WHERE org_id = $1 AND team_id IS NOT NULL`, e.org.ID); n != 1 {
		t.Errorf("%d team labels, want 1 (bug is merged into Bug)", n)
	}

	// The old Linear UUID resolves to the same issue.
	byLinear, err := e.svc.GetIssue(ctx, e.org, e.chen, fxI1, tracker.Includes{})
	if err != nil || byLinear.ID != is.ID {
		t.Errorf("GetIssue(linear uuid) = %v, %v", byLinear.ID, err)
	}

	// A parent written after its child.
	child, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-3404", tracker.Includes{})
	if err != nil || child.Parent == nil || child.Parent.Identifier != "ORB-3403" {
		t.Errorf("ORB-3404 parent = %+v, %v", child.Parent, err)
	}

	// Relations: r1, r2 (r3 merged), r5.
	if n := e.count(`SELECT count(*) FROM issue_relations WHERE org_id = $1`, e.org.ID); n != 3 {
		t.Errorf("%d relation rows, want 3", n)
	}
	if n := e.count(`SELECT count(*) FROM documents WHERE org_id = $1 AND slug = 'cutover-plan-a1b2c3d4e5f6'`, e.org.ID); n != 1 {
		t.Error("the document keeps Linear's slug")
	}
	if n := e.count(`SELECT count(*) FROM cycle_snapshots WHERE org_id = $1`, e.org.ID); n != 3 {
		t.Errorf("%d snapshots, want 3", n)
	}

	// Cadence set, cycles still off.
	team, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: e.org.ID, Key: "ORB"})
	if err != nil {
		t.Fatal(err)
	}
	if team.CyclesEnabled || team.CycleLengthDays != 14 || team.CycleStartWeekday != 0 || team.CycleTimezone != "Europe/London" {
		t.Errorf("team cadence %+v", team)
	}
	if rep.NextNumbers["ORB"] != 3405 {
		t.Errorf("next number %d, want 3405", rep.NextNumbers["ORB"])
	}

	// A second run with the same export changes nothing.
	again := e.mustRun(ex, false)
	for _, ent := range Entities {
		c := again.Counts[ent]
		if c.Create != 0 || c.Update != 0 {
			t.Errorf("second run %s: %+v, want only skips", ent, *c)
		}
	}
	if bad := Mismatches(again.Verify); len(bad) > 0 {
		t.Errorf("second run verification: %+v", bad)
	}
	reread, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-2557", tracker.Includes{})
	if err != nil || reread.Version != is.Version || !reread.UpdatedAt.Equal(is.UpdatedAt) {
		t.Errorf("the second run touched ORB-2557: version %d -> %d", is.Version, reread.Version)
	}

	// A change in Linear is an update on the next run.
	ex.Issues[0].Title = "Compose project name (renamed)"
	ex.Issues[0].UpdatedAt = ts("2026-10-24T09:00:00Z")
	third := e.mustRun(ex, false)
	if c := third.Counts[EntIssue]; c.Update != 1 || c.Create != 0 {
		t.Errorf("third run issue counts %+v, want one update", *c)
	}

	// The next issue made through the API is ORB-3405.
	made, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "After the import"})
	if err != nil {
		t.Fatal(err)
	}
	if made.Number != 3405 {
		t.Errorf("next issue is ORB-%d, want ORB-3405", made.Number)
	}
}

func TestRun_thenEnablingCyclesContinuesTheNumbers(t *testing.T) {
	e := newImpEnv(t)
	e.mustRun(fixtureExport(), false)
	e.svc.SetClock(func() time.Time { return ts("2026-10-24T12:00:00Z") })
	team, err := e.svc.UpdateTeam(context.Background(), e.org, e.chen, "ORB",
		tracker.UpdateTeamInput{CyclesEnabled: httpx.Opt[bool]{Set: true, V: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !team.CyclesEnabled {
		t.Fatal("cycles not on")
	}
	var lo, hi int
	if err := e.pool.QueryRow(context.Background(), `SELECT min(number), max(number) FROM cycles WHERE org_id = $1`, e.org.ID).
		Scan(&lo, &hi); err != nil {
		t.Fatal(err)
	}
	// Linear's numbers stay; the job goes on after 10 (the current and the
	// next cycle), and never makes a cycle 1.
	if lo != 7 || hi < 11 {
		t.Errorf("cycle numbers %d..%d, want 7..11 or more", lo, hi)
	}
	if n := e.count(`SELECT count(*) FROM cycles WHERE org_id = $1 AND starts_at <= $2 AND ends_at > $2`,
		e.org.ID, ts("2026-10-24T12:00:00Z")); n != 1 {
		t.Errorf("%d current cycles, want 1", n)
	}

	// A re-run after cycles are on still changes nothing and keeps the
	// admin's cadence.
	rep := e.mustRun(fixtureExport(), false)
	if !strings.HasPrefix(rep.Cadence["ORB"], "unchanged: cycles are already on") {
		t.Errorf("cadence note %q", rep.Cadence["ORB"])
	}
}

func TestRun_refusesClashingNumbers(t *testing.T) {
	t.Run("a cycle the job made first", func(t *testing.T) {
		e := newImpEnv(t)
		testdb.MustExec(t, e.pool, `INSERT INTO cycles (org_id, team_id, number, starts_at, ends_at)
			SELECT org_id, id, 7, now(), now() + interval '14 days' FROM teams WHERE key = 'ORB'`)
		_, err := e.run(fixtureExport(), false)
		if err == nil || !strings.Contains(err.Error(), "ORB cycle 7") {
			t.Fatalf("err = %v, want a cycle 7 clash", err)
		}
		if n := e.count(`SELECT count(*) FROM issues`); n != 0 {
			t.Errorf("a refused import wrote %d issues", n)
		}
	})
	t.Run("an issue made through the API first", func(t *testing.T) {
		e := newImpEnv(t)
		testdb.MustExec(t, e.pool, `UPDATE issue_counters SET next_number = 50`)
		if _, err := e.svc.CreateIssue(context.Background(), e.org, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "x"}); err != nil {
			t.Fatal(err)
		}
		_, err := e.run(fixtureExport(), false)
		if err == nil || !strings.Contains(err.Error(), "ORB-50") {
			t.Fatalf("err = %v, want an ORB-50 clash", err)
		}
	})
}

func TestVerify_reportsALostRow(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	e.mustRun(ex, false)
	// Deleting comment cm1 also deletes its reply cm2 (FK cascade).
	testdb.MustExec(t, e.pool, `DELETE FROM comments WHERE body = 'First'`)
	p, err := BuildPlan(ex, fixtureUserMap(), map[string]int32{"ORB": 3405})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Verify(context.Background(), e.q, e.org.ID, p, e.store)
	if err != nil {
		t.Fatal(err)
	}
	bad := Mismatches(rows)
	if len(bad) != 1 || bad[0].Entity != EntComment || bad[0].Expected != 4 || bad[0].Actual != 2 {
		t.Fatalf("mismatches %+v, want comment 4 vs 2", bad)
	}
	// A re-run repairs it: the comments come back with their old ids.
	rep := e.mustRun(ex, false)
	if c := rep.Counts[EntComment]; c.Update != 2 {
		t.Errorf("re-run comment counts %+v, want 2 restored", *c)
	}
}

func TestRun_unknownOrgAndMember(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	p, err := BuildPlan(ex, fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), e.pool, ex, p, Options{Org: "nope"}); err == nil ||
		!strings.Contains(err.Error(), `org "nope" does not exist`) {
		t.Errorf("err = %v", err)
	}
	um := &UserMap{Default: "nobody"}
	p, err = BuildPlan(ex, um, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), e.pool, ex, p, e.opts(false))
	if err == nil || !strings.Contains(err.Error(), `"nobody", which is not a member`) {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, ErrMismatch) {
		t.Error("a refused run is not a mismatch")
	}
}

// TestRun_findsAnAgentMadeByCreateAgent: with no claude the
// import stops and names POST /agents; after tracker.CreateAgent (the
// route's service call) makes claude, the same import runs.
func TestRun_findsAnAgentMadeByCreateAgent(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	// Take the fixture's claude out of the org and out of the way of the name.
	testdb.MustExec(t, e.pool, `DELETE FROM org_members WHERE user_id = (SELECT id FROM users WHERE username = 'claude')`)
	testdb.MustExec(t, e.pool, `UPDATE users SET username = 'claude-fixture' WHERE username = 'claude'`)
	_, err := e.run(fixtureExport(), true)
	if err == nil || !strings.Contains(err.Error(), `"claude", which is not a member`) ||
		!strings.Contains(err.Error(), "POST /api/v1/orgs/orbit/agents") {
		t.Fatalf("err = %v", err)
	}
	m, err := e.svc.CreateAgent(ctx, e.org, tracker.AgentInput{Username: "claude", Name: "Claude"})
	if err != nil {
		t.Fatal(err)
	}
	e.mustRun(fixtureExport(), false)
	if n := e.count(`SELECT count(*) FROM import_mappings WHERE entity_type = 'user' AND source_id = $1 AND target_id = $2`, fxUBot, m.ID); n != 1 {
		t.Fatalf("the Linear bot user maps to the new claude %d times, want 1", n)
	}
}

func TestCheckSchema(t *testing.T) {
	e := newImpEnv(t)
	if err := CheckSchema(context.Background(), e.pool); err != nil {
		t.Fatalf("a freshly migrated database: %v", err)
	}
	testdb.MustExec(t, e.pool, `UPDATE schema_migrations SET version = version - 1`)
	if err := CheckSchema(context.Background(), e.pool); err == nil {
		t.Error("an older schema must be refused")
	}
}

// fileRows is the org's file attachments: id -> object key.
func (e *impEnv) fileRows() map[string]string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT id::text, object_key FROM attachments WHERE org_id = $1 AND kind = 'file'`, e.org.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			e.t.Fatal(err)
		}
		out[id] = key
	}
	return out
}

func verifyRow(rows []VerifyRow, ent string) (VerifyRow, bool) {
	for _, r := range rows {
		if r.Entity == ent {
			return r, true
		}
	}
	return VerifyRow{}, false
}

func TestRun_storesFilesAndRewritesEveryText(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	rep := e.mustRun(fixtureExport(), false)

	files := e.fileRows()
	if len(files) != 2 {
		t.Fatalf("%d file rows, want 2", len(files))
	}
	for id, key := range files {
		if !strings.HasPrefix(key, "org/"+e.org.ID.String()+"/") || !strings.Contains(key, "/"+id+"/") {
			t.Errorf("object key %q does not follow org/{org}/{yyyy}/{id}/{name}", key)
		}
		if _, err := e.store.Head(ctx, key); err != nil {
			t.Errorf("object %s: %v", key, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM attachments WHERE org_id = $1 AND kind = 'file' AND content_type = 'text/plain'`, e.org.ID); n != 1 {
		t.Errorf("the CSV must be stored as text/plain (%d rows)", n)
	}
	// Every text points at the API download URL; none at Linear.
	for _, q := range []string{
		`SELECT count(*) FROM issues WHERE org_id = $1 AND description LIKE '%uploads.linear.app%'`,
		`SELECT count(*) FROM comments WHERE org_id = $1 AND body LIKE '%uploads.linear.app%'`,
		`SELECT count(*) FROM projects WHERE org_id = $1 AND content LIKE '%uploads.linear.app%'`,
		`SELECT count(*) FROM attachments WHERE org_id = $1 AND url LIKE '%uploads.linear.app%'`,
	} {
		if n := e.count(q, e.org.ID); n != 0 {
			t.Errorf("%d rows still point at Linear: %s", n, q)
		}
	}
	is, err := e.svc.GetIssue(ctx, e.org, e.chen, "ORB-2557", tracker.Includes{Attachments: true, Comments: true})
	if err != nil {
		t.Fatal(err)
	}
	var fileID string
	for _, a := range is.Attachments {
		if a.Kind == "file" {
			fileID = a.ID.String()
		}
	}
	want := "https://tracker.example/api/v1/orgs/orbit/attachments/" + fileID + "/download"
	if fileID == "" || !strings.Contains(is.Description, "![x]("+want+")") || !strings.Contains(is.Comments[1].Body, want) {
		t.Errorf("ORB-2557 does not point at its file %s: %q", want, is.Description)
	}
	if n := e.count(`SELECT count(*) FROM projects WHERE org_id = $1 AND content LIKE '%' || $2 || '%'`, e.org.ID, want); n != 1 {
		t.Error("the project content must point at the same download URL")
	}
	for _, ent := range []string{VerifyLinearURLs, VerifyObjects, EntFile} {
		r, ok := verifyRow(rep.Verify, ent)
		if !ok || r.Expected != r.Actual {
			t.Errorf("verify %s = %+v (present %v)", ent, r, ok)
		}
	}

	// A second run uploads nothing and changes nothing.
	again := e.mustRun(fixtureExport(), false)
	if again.Uploads.Uploaded != 0 || again.Uploads.Present != 2 {
		t.Errorf("second run uploads %+v, want 0 uploaded, 2 present", again.Uploads)
	}
	for _, ent := range Entities {
		if c := again.Counts[ent]; c.Create != 0 || c.Update != 0 {
			t.Errorf("second run %s: %+v", ent, *c)
		}
	}
	if got := e.fileRows(); len(got) != 2 {
		t.Errorf("second run: %d file rows", len(got))
	}
}

func TestRun_dryRunUploadsNothing(t *testing.T) {
	e := newImpEnv(t)
	rep := e.mustRun(fixtureExport(), true)
	if rep.Uploads.Planned != 2 || rep.Uploads.Uploaded != 0 {
		t.Errorf("dry run uploads %+v, want 2 planned, 0 uploaded", rep.Uploads)
	}
	if r, ok := verifyRow(rep.Verify, VerifyLinearURLs); !ok || r.Actual != 0 {
		t.Errorf("the dry run's rewrite left %+v", r)
	}
	// The real run then has to upload both: the dry run put nothing.
	real := e.mustRun(fixtureExport(), false)
	if real.Uploads.Uploaded != 2 {
		t.Errorf("after a dry run, the import uploaded %d, want 2", real.Uploads.Uploaded)
	}
}

func TestRun_aT4LinkImportBecomesFiles(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	p, err := BuildPlan(ex, fixtureUserMap(), map[string]int32{"ORB": 3405})
	if err != nil {
		t.Fatal(err)
	}
	opts := e.opts(false)
	opts.Store = nil
	first, err := Run(context.Background(), e.pool, ex, p.WithoutFiles(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.PendingUploads != 2 || len(e.fileRows()) != 0 {
		t.Fatalf("--skip-files: pending %d, %d file rows", first.PendingUploads, len(e.fileRows()))
	}
	if _, ok := verifyRow(first.Verify, VerifyLinearURLs); ok {
		t.Error("--skip-files must not check upload URLs")
	}
	second := e.mustRun(ex, false)
	if c := second.Counts[EntAttachment]; c.Update != 1 || c.Create != 0 {
		t.Errorf("attachment counts %+v: a2 must turn from link into file", *c)
	}
	if c := second.Counts[EntFile]; c.Create != 2 {
		t.Errorf("file counts %+v: both file mappings are new", *c)
	}
	if n := e.count(`SELECT count(*) FROM attachments WHERE org_id = $1 AND kind = 'link' AND url LIKE '%uploads.linear.app%'`, e.org.ID); n != 0 {
		t.Errorf("%d link rows still point at Linear", n)
	}
	if bad := Mismatches(second.Verify); len(bad) > 0 {
		t.Errorf("verify %+v", bad)
	}
}

func TestRun_aFileOnlyAProjectRefersToFailsVerify(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	ex.Projects[0].Content = sp("Plan ![shot](" + fxUploadB + ")")
	ex.Files = append(ex.Files, File{URL: fxUploadB, Path: "files/aa/shot.png", Filename: "b.png", ContentType: "image/png", SizeBytes: 3})
	p, err := BuildPlan(ex, fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reasons(p)[EntFile+" "+fxUploadB]; !strings.HasPrefix(got, "only a document (or a comment on one), a project or a milestone") {
		t.Fatalf("exclusion %q", got)
	}
	rep, err := Run(context.Background(), e.pool, ex, p, e.opts(false))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("err = %v, want ErrMismatch", err)
	}
	if r, _ := verifyRow(rep.Verify, VerifyLinearURLs); r.Actual != 1 {
		t.Errorf("linear_upload_url %+v, want 1 left", r)
	}
	if n := e.count(`SELECT count(*) FROM issues`); n != 0 {
		t.Errorf("a failed verification committed %d issues", n)
	}
	// The files went to the store before the transaction that rolled back.
	// A re-run (here on a corrected export) finds them under the same keys
	// and uploads nothing (decisions-T4b #5).
	if rep.Uploads.Uploaded != 2 {
		t.Fatalf("the failed run uploaded %d, want 2", rep.Uploads.Uploaded)
	}
	again, err := Run(context.Background(), e.pool, fixtureExport(), mustPlan(t, fixtureExport()), e.opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if again.Uploads.Uploaded != 0 || again.Uploads.Present != 2 {
		t.Errorf("the re-run uploads %+v, want 0 uploaded, 2 present", again.Uploads)
	}
	if strings.Join(again.Uploads.Keys, ",") != strings.Join(rep.Uploads.Keys, ",") {
		t.Errorf("keys changed:\n%v\n%v", rep.Uploads.Keys, again.Uploads.Keys)
	}
}

func mustPlan(t *testing.T, ex *Export) *Plan {
	t.Helper()
	p, err := BuildPlan(ex, fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun_skipFilesAfterAFileImportIsRefused(t *testing.T) {
	e := newImpEnv(t)
	e.mustRun(fixtureExport(), false)
	opts := e.opts(false)
	opts.Store = nil
	_, err := Run(context.Background(), e.pool, fixtureExport(), mustPlan(t, fixtureExport()).WithoutFiles(), opts)
	if !errors.Is(err, ErrFilesImported) {
		t.Fatalf("err = %v, want ErrFilesImported", err)
	}
	if n := e.count(`SELECT count(*) FROM issues WHERE description LIKE '%uploads.linear.app%'`); n != 0 {
		t.Errorf("a refused run put %d Linear URLs back", n)
	}
	if _, err := Verify(context.Background(), e.q, e.org.ID, mustPlan(t, fixtureExport()).WithoutFiles(), nil); !errors.Is(err, ErrFilesImported) {
		t.Errorf("a skip-files verify err = %v, want ErrFilesImported", err)
	}
}

func TestVerify_ignoresALinearURLPastedAfterTheImport(t *testing.T) {
	e := newImpEnv(t)
	e.mustRun(fixtureExport(), false)
	if _, err := e.svc.CreateComment(context.Background(), e.org, e.chen, "ORB-2557",
		tracker.CommentInput{Body: "old screenshot " + fxUploadB}); err != nil {
		t.Fatal(err)
	}
	rows, err := Verify(context.Background(), e.q, e.org.ID, mustPlan(t, fixtureExport()), e.store)
	if err != nil {
		t.Fatal(err)
	}
	if bad := Mismatches(rows); len(bad) > 0 {
		t.Errorf("a pasted Linear URL failed verify: %+v", bad)
	}
}

func TestVerify_reportsAMissingObject(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	e.mustRun(ex, false)
	for _, key := range e.fileRows() {
		if err := e.store.Delete(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		break
	}
	p, err := BuildPlan(ex, fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := Verify(context.Background(), e.q, e.org.ID, p, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := verifyRow(rows, VerifyObjects); r.Expected != 2 || r.Actual != 1 {
		t.Errorf("file_object %+v, want 2 vs 1", r)
	}
	// A re-run puts it back.
	rep := e.mustRun(ex, false)
	if rep.Uploads.Uploaded != 1 || rep.Uploads.Present != 1 {
		t.Errorf("re-run uploads %+v", rep.Uploads)
	}
}

// TestRun_anOwnerChangeKeepsTheFileRow: a file only a description referred
// to (run 1) gets a Linear attachment before the next export (run 2). The
// attachment takes over the SAME row, id and object: no second row, no
// second upload, no row left without a mapping (review).
func TestRun_anOwnerChangeKeepsTheFileRow(t *testing.T) {
	e := newImpEnv(t)
	first := fixtureExport()
	var kept []Attachment
	for _, a := range first.Attachments {
		if a.ID != "lin-a2" { // fxUploadA's attachment does not exist yet
			kept = append(kept, a)
		}
	}
	first.Attachments = kept
	rep1 := e.mustRun(first, false)
	if rep1.Uploads.Uploaded != 2 {
		t.Fatalf("run 1 uploaded %d", rep1.Uploads.Uploaded)
	}
	before := e.fileRows()

	second := fixtureExport()
	for i := range second.Attachments {
		if second.Attachments[i].ID == "lin-a2" {
			// Another year, so a key built afresh would differ.
			second.Attachments[i].CreatedAt = ts("2027-01-05T10:00:00Z")
			second.Attachments[i].UpdatedAt = ts("2027-01-05T10:00:00Z")
		}
	}
	rep2 := e.mustRun(second, false)
	if rep2.Uploads.Uploaded != 0 || rep2.Uploads.Present != 2 {
		t.Errorf("run 2 uploads %+v, want 0 uploaded, 2 present", rep2.Uploads)
	}
	after := e.fileRows()
	if len(after) != 2 {
		t.Fatalf("%d file rows after the owner change, want 2", len(after))
	}
	for id, key := range before {
		if after[id] != key {
			t.Errorf("row %s: key %q -> %q (or the row is gone)", id, key, after[id])
		}
	}
	if n := e.count(`SELECT count(*) FROM attachments a WHERE a.org_id = $1 AND NOT EXISTS
		(SELECT 1 FROM import_mappings m WHERE m.org_id = a.org_id AND m.target_id = a.id)`, e.org.ID); n != 0 {
		t.Errorf("%d attachment rows have no mapping (a duplicate)", n)
	}
	if n := e.count(`SELECT count(DISTINCT m.target_id) FROM import_mappings m WHERE m.org_id = $1
		AND ((m.entity_type = 'attachment' AND m.source_id = 'lin-a2') OR (m.entity_type = 'file' AND m.source_id = $2))`,
		e.org.ID, fxUploadA); n != 1 {
		t.Errorf("the attachment and the file URL map to %d rows, want 1", n)
	}
	if bad := Mismatches(rep2.Verify); len(bad) > 0 {
		t.Errorf("verify %+v", bad)
	}
}

func TestVerify_linearURLCheckUsesTheRewritersDefinition(t *testing.T) {
	e := newImpEnv(t)
	e.mustRun(fixtureExport(), false)
	ctx := context.Background()
	check := func(want int) VerifyRow {
		t.Helper()
		rows, err := Verify(ctx, e.q, e.org.ID, mustPlan(t, fixtureExport()), nil)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := verifyRow(rows, VerifyLinearURLs)
		if r.Actual != want {
			t.Fatalf("linear_upload_url %+v, want %d", r, want)
		}
		return r
	}
	// Prose that only names the host (a ticket about this importer) is not
	// a URL: it does not fail verify.
	testdb.MustExec(t, e.pool, `UPDATE issues SET description = description ||
		' rewrites uploads.linear.app/... and https://uploads.linear.app/<org>/<id> and `+"`https://uploads.linear.app/`"+`'
		WHERE org_id = $1 AND number = 2557`, e.org.ID)
	check(0)
	// A real URL in any scheme or case is caught, and the row is named.
	var id string
	if err := e.pool.QueryRow(ctx, `UPDATE issues SET description = description || ' HTTP://Uploads.Linear.App/o/1/2'
		WHERE org_id = $1 AND number = 2557 RETURNING id::text`, e.org.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r := check(1)
	if len(r.Rows) != 1 || r.Rows[0] != "issue "+id {
		t.Errorf("named rows %v, want [issue %s]", r.Rows, id)
	}
}

func TestRun_rewritesAnyFormOfTheURL(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	upper := "HTTP://UPLOADS.LINEAR.APP/org-1/aaaa/bbbb" // fxUploadA in another form
	ex.Comments[1].Body = "Reply with ![y](" + upper + ")"
	rep := e.mustRun(ex, false)
	if bad := Mismatches(rep.Verify); len(bad) > 0 {
		t.Fatalf("verify %+v", bad)
	}
	if n := e.count(`SELECT count(*) FROM comments WHERE org_id = $1 AND body ILIKE '%uploads.linear.app%'`, e.org.ID); n != 0 {
		t.Errorf("%d comments still point at Linear", n)
	}
	if n := e.count(`SELECT count(*) FROM comments WHERE org_id = $1 AND body LIKE '%/attachments/%/download%'`, e.org.ID); n != 1 {
		t.Errorf("the upper-case URL was not rewritten (%d)", n)
	}
}

// LAUN-54: a label imported before team labels existed is org-wide; the next
// run gives it its team (and never takes a team away).
func TestRun_givesAnOrgWideLabelItsTeam(t *testing.T) {
	e := newImpEnv(t)
	ex := fixtureExport()
	e.mustRun(ex, false)
	if _, err := e.pool.Exec(context.Background(), `UPDATE labels SET team_id = NULL WHERE org_id = $1 AND name = 'Flaky'`, e.org.ID); err != nil {
		t.Fatal(err)
	}
	rep := e.mustRun(ex, false)
	if c := rep.Counts[EntLabel]; c.Update != 1 || c.Create != 0 {
		t.Errorf("label counts %+v, want one update", *c)
	}
	if n := e.count(`SELECT count(*) FROM labels WHERE org_id = $1 AND name = 'Flaky' AND team_id IS NOT NULL`, e.org.ID); n != 1 {
		t.Errorf("Flaky has no team after the second run")
	}
	// The label stays on the team's issue.
	if n := e.count(`SELECT count(*) FROM issue_labels il JOIN labels l ON l.org_id = il.org_id AND l.id = il.label_id
	                 WHERE il.org_id = $1 AND l.name = 'Flaky'`, e.org.ID); n != 1 {
		t.Errorf("%d issues carry Flaky, want 1", n)
	}
}

// LAUN-54: labels that an earlier import merged (two teams' "bug" mapped to
// one org-wide row) stay org-wide; no issue ends up with a label of another
// team, and a second run changes nothing.
func TestRun_preMergedTeamLabelsStayOrgWide(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	ex := fixtureExport()
	ex.Teams = append(ex.Teams, Team{ID: "lin-team-web", Key: "WEB", Name: "Web", CreatedAt: ts("2025-01-01T09:00:00Z"),
		UpdatedAt: ts("2025-01-01T09:00:00Z"), Timezone: "Europe/London", CycleDuration: 2})
	ex.States = append(ex.States, WorkflowState{ID: "lin-st-web-todo", Name: "Todo", Type: "unstarted", Color: "#e2e2e2",
		CreatedAt: ts("2025-01-01T09:00:00Z"), UpdatedAt: ts("2025-01-01T09:00:00Z"), Team: ref("lin-team-web")})
	ex.Labels = append(ex.Labels,
		Label{ID: "l-orb-sec", Name: "Sec", CreatedAt: ts("2025-04-01T00:00:00Z"), Team: ref(fxTeam)},
		Label{ID: "l-web-sec", Name: "sec", CreatedAt: ts("2025-04-02T00:00:00Z"), Team: ref("lin-team-web")})
	ex.Issues = append(ex.Issues, Issue{ID: "lin-web-1", Identifier: "WEB-1", Number: 1, Title: "web issue",
		CreatedAt: ts("2026-10-22T10:00:00Z"), UpdatedAt: ts("2026-10-22T10:00:00Z"), Team: ref("lin-team-web"),
		State: ref("lin-st-web-todo"), Creator: ref(fxUChen), LabelIDs: []string{"l-web-sec"}})
	ex.Issues[1].LabelIDs = append(ex.Issues[1].LabelIDs, "l-orb-sec")

	// The state before this change: both labels merged into one org-wide row.
	e.mustRun(ex, false)
	var row string
	if err := e.pool.QueryRow(ctx, `SELECT target_id::text FROM import_mappings WHERE org_id = $1 AND entity_type = 'label' AND source_id = 'l-orb-sec'`, e.org.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Rebuild the old shape: delete the web label row, point its mapping and
	// its issue at the ORB label's row, and make that row org-wide.
	exec(`DELETE FROM labels WHERE org_id = $1 AND team_id = (SELECT id FROM teams WHERE org_id = $1 AND key = 'WEB')`, e.org.ID)
	exec(`UPDATE labels SET team_id = NULL WHERE org_id = $1 AND id = $2::uuid`, e.org.ID, row)
	exec(`INSERT INTO import_mappings (org_id, entity_type, source, source_id, target_id) VALUES ($1, 'label', 'linear', 'l-web-sec', $2::uuid)
	      ON CONFLICT (org_id, source, entity_type, source_id) DO UPDATE SET target_id = EXCLUDED.target_id`, e.org.ID, row)
	exec(`INSERT INTO issue_labels (org_id, issue_id, label_id)
	      SELECT i.org_id, i.id, $2::uuid FROM issues i JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
	      WHERE i.org_id = $1 AND t.key = 'WEB' ON CONFLICT DO NOTHING`, e.org.ID, row)

	snapshot := func() string {
		var s string
		if err := e.pool.QueryRow(ctx, `SELECT coalesce(string_agg(l.id::text || ':' || coalesce(l.team_id::text,'-') || ':' || il.issue_id::text, ',' ORDER BY l.id, il.issue_id), '')
			FROM labels l LEFT JOIN issue_labels il ON il.org_id = l.org_id AND il.label_id = l.id WHERE l.org_id = $1`, e.org.ID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	check := func() {
		t.Helper()
		if n := e.count(`SELECT count(*) FROM issue_labels il JOIN labels l ON l.org_id = il.org_id AND l.id = il.label_id
		                 JOIN issues i ON i.org_id = il.org_id AND i.id = il.issue_id
		                 WHERE il.org_id = $1 AND l.team_id IS NOT NULL AND l.team_id <> i.team_id`, e.org.ID); n != 0 {
			t.Fatalf("%d issues carry a label of another team", n)
		}
		if n := e.count(`SELECT count(*) FROM labels WHERE org_id = $1 AND id = $2::uuid AND team_id IS NULL`, e.org.ID, row); n != 1 {
			t.Fatal("the merged row must stay org-wide")
		}
	}
	e.mustRun(ex, false)
	check()
	if after := snapshot(); after != before {
		t.Fatalf("the run moved data:\nbefore %s\nafter  %s", before, after)
	}
	again := e.mustRun(ex, false)
	check()
	if snapshot() != before {
		t.Fatal("the second run changed data")
	}
	for _, ent := range Entities {
		if c := again.Counts[ent]; c.Create != 0 || c.Update != 0 {
			t.Errorf("second run %s: %+v", ent, *c)
		}
	}
}

// LAUN-195 (decisions-T29): an imported project gets the org's first status of its Linear kind, and a re-import
// does not undo a status an admin chose within the same kind.
func TestRun_importedProjectGetsAStatusOfItsKind(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	// An admin added "Active" to the started kind and put it first.
	var active uuid.UUID
	if err := e.pool.QueryRow(ctx, `INSERT INTO project_statuses (org_id, kind, name, color, position)
		VALUES ($1, 'started', 'Active', '#00ff00', -1) RETURNING id`, e.org.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	ex := fixtureExport()
	e.mustRun(ex, false)
	var got uuid.UUID
	var kind string
	row := func() {
		t.Helper()
		if err := e.pool.QueryRow(ctx, `SELECT status_id, state FROM projects WHERE org_id = $1 AND name = 'dev-ops'`, e.org.ID).Scan(&got, &kind); err != nil {
			t.Fatal(err)
		}
	}
	row()
	if got != active || kind != "started" {
		t.Fatalf("imported project has status %s kind %s, want the first started status %s", got, kind, active)
	}
	// The admin moves it to the default started status; a second run leaves it there.
	if _, err := e.pool.Exec(ctx, `UPDATE projects SET status_id = (SELECT id FROM project_statuses WHERE org_id = $1 AND name = 'In progress')
		WHERE org_id = $1 AND name = 'dev-ops'`, e.org.ID); err != nil {
		t.Fatal(err)
	}
	ex.Projects[0].UpdatedAt = ex.Projects[0].UpdatedAt.Add(time.Hour) // a changed row, so the upsert writes
	e.mustRun(ex, false)
	row()
	if got == active {
		t.Fatal("a re-import reset the status the admin chose")
	}
	// A kind change in Linear moves the project to the first status of the new kind.
	ex.Projects[0].Status = &ProjectStatus{Type: "completed"}
	ex.Projects[0].UpdatedAt = ex.Projects[0].UpdatedAt.Add(time.Hour)
	e.mustRun(ex, false)
	row()
	if kind != "completed" {
		t.Fatalf("kind %s after the Linear change", kind)
	}
}
