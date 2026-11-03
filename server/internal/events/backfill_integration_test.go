package events_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// The events backfill job (decisions-T20 #57, ticket 6b): batches, a crash
// and a resume, the advisory lock, re-runs, cross-org and the feed a
// restricted member reads. The per-type rules are in backfill_test.go; the
// round trip over every kind made through the routes is
// TestEventsBackfill_RoundTrip (internal/api).

type bfEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	q     *dbq.Queries
	org   uuid.UUID // orbit
	other uuid.UUID // a second org
	orb   uuid.UUID // team ORB of orbit
	hid   uuid.UUID // team HID of orbit
	oth   uuid.UUID // team OTH of the second org
}

func newBfEnv(t *testing.T) *bfEnv {
	t.Helper()
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	ctx := context.Background()
	e := &bfEnv{t: t, pool: pool, q: dbq.New(pool), org: d.OrgID}
	if err := pool.QueryRow(ctx, `SELECT id FROM teams WHERE org_id = $1 AND key = $2`, d.OrgID, testdb.TeamKey).Scan(&e.orb); err != nil {
		t.Fatal(err)
	}
	hid, err := tracker.CreateTeamTx(ctx, e.q, e.org, "HID", "Hidden", "")
	if err != nil {
		t.Fatal(err)
	}
	e.hid = hid.ID
	other, err := e.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "elsewhere", Name: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	e.other = other.ID
	oth, err := tracker.CreateTeamTx(ctx, e.q, e.other, "OTH", "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	e.oth = oth.ID
	return e
}

// legacy inserts n legacy rows (as before migration 0038) of one type about
// entity, with payload, and returns their seqs.
func (e *bfEnv) legacy(org uuid.UUID, n int, typ, entityType string, entity uuid.UUID, payload string) []int64 {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		WITH base AS (SELECT COALESCE(MAX(seq), 0) AS m FROM events WHERE org_id = $1)
		INSERT INTO events (org_id, seq, type, entity_type, entity_id, payload)
		SELECT $1, base.m + s, $3, $4, $5, $6::jsonb FROM base, generate_series(1, $2::int) s
		RETURNING seq`, org, n, typ, entityType, entity, payload)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

type stored struct {
	scope string
	teams []uuid.UUID
}

// rows reads every event of every org: (org, seq) -> scope and teams.
func (e *bfEnv) rows() map[[2]string]stored {
	e.t.Helper()
	rs, err := e.pool.Query(context.Background(), `SELECT org_id, seq, scope, team_ids FROM events`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	out := map[[2]string]stored{}
	for rs.Next() {
		var org uuid.UUID
		var seq int64
		var s stored
		if err := rs.Scan(&org, &seq, &s.scope, &s.teams); err != nil {
			e.t.Fatal(err)
		}
		out[[2]string{org.String(), strconv.FormatInt(seq, 10)}] = s
	}
	if err := rs.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *bfEnv) count(where string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE `+where, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *bfEnv) run(o events.BackfillOptions) events.BackfillResult {
	e.t.Helper()
	res, locked, err := events.Backfill(context.Background(), e.pool, o)
	if err != nil || !locked {
		e.t.Fatalf("backfill: locked %v, err %v", locked, err)
	}
	return res
}

// feed reads the org's feed from seq 0 with the #55 arguments.
func (e *bfEnv) feed(isAdmin, allTeams bool, teams ...uuid.UUID) []int64 {
	e.t.Helper()
	got, err := e.q.ListEventsAfter(context.Background(), dbq.ListEventsAfterParams{
		OrgID: e.org, Seq: 0, Limit: 100000, IsAdmin: isAdmin, AllTeams: allTeams, TeamIds: append([]uuid.UUID{}, teams...),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]int64, 0, len(got))
	for _, r := range got {
		out = append(out, r.Seq)
	}
	return out
}

// A large history in two orgs, read in batches of 100: every placeable row is
// placed with its team, every orphan stays legacy, and nothing else changes.
func TestBackfill_Batches(t *testing.T) {
	e := newBfEnv(t)
	orbRows := e.legacy(e.org, 1250, events.TeamUpdated, "team", e.orb, `{}`)
	orphans := e.legacy(e.org, 37, events.IssueUpdated, "issue", uuid.New(), `{"marker":"secret-body"}`)
	hidRows := e.legacy(e.org, 600, events.TeamUpdated, "team", e.hid, `{}`)
	othRows := e.legacy(e.other, 513, events.TeamUpdated, "team", e.oth, `{}`)
	total := int64(len(orbRows) + len(orphans) + len(hidRows) + len(othRows))
	before := e.count(`true`)

	res := e.run(events.BackfillOptions{BatchSize: 100, Pause: -1})
	if res.Scanned != total || res.Placed != total-int64(len(orphans)) || res.Legacy != int64(len(orphans)) {
		t.Fatalf("result %+v, want scanned %d, placed %d, legacy %d", res, total, total-int64(len(orphans)), len(orphans))
	}
	if want := int((total + 99) / 100); res.Batches != want {
		t.Fatalf("batches %d, want %d", res.Batches, want)
	}
	if n := e.count(`true`); n != before {
		t.Fatalf("the job changed the number of events: %d, want %d (it emits none)", n, before)
	}
	for _, c := range []struct {
		org  uuid.UUID
		seqs []int64
		team uuid.UUID
	}{{e.org, orbRows, e.orb}, {e.org, hidRows, e.hid}, {e.other, othRows, e.oth}} {
		if n := e.count(`org_id = $1 AND seq = ANY($2) AND scope = 'any' AND team_ids = ARRAY[$3::uuid]`, c.org, c.seqs, c.team); n != int64(len(c.seqs)) {
			t.Errorf("team %s: %d of %d rows placed", c.team, n, len(c.seqs))
		}
	}
	if n := e.count(`org_id = $1 AND seq = ANY($2) AND scope = 'legacy' AND team_ids = '{}'`, e.org, orphans); n != int64(len(orphans)) {
		t.Errorf("orphans: %d of %d still legacy", n, len(orphans))
	}
}

// A run that stops after two batches (a crash) leaves the rest legacy; the
// next run places them, and the end state is the state of one clean run.
func TestBackfill_CrashAndResume(t *testing.T) {
	e := newBfEnv(t)
	seqs := e.legacy(e.org, 450, events.TeamUpdated, "team", e.hid, `{}`)
	orphans := e.legacy(e.org, 5, events.TeamDeleted, "team", uuid.New(), `{}`)
	crash := errors.New("crash")
	res, locked, err := events.BackfillWithHook(context.Background(), e.pool, events.BackfillOptions{BatchSize: 100, Pause: -1},
		func(batch int) error {
			if batch == 2 {
				return crash
			}
			return nil
		})
	if !errors.Is(err, crash) || !locked {
		t.Fatalf("locked %v, err %v, want the crash", locked, err)
	}
	if res.Placed != 200 {
		t.Fatalf("placed %d before the crash, want 200", res.Placed)
	}
	if n := e.count(`org_id = $1 AND scope = 'legacy'`, e.org); n != int64(len(seqs)+len(orphans)-200) {
		t.Fatalf("legacy rows after the crash: %d", n)
	}
	// The lock went with the crashed run: the next run takes it.
	res = e.run(events.BackfillOptions{BatchSize: 100, Pause: -1})
	if res.Placed != int64(len(seqs)-200) || res.Legacy != int64(len(orphans)) {
		t.Fatalf("resume %+v", res)
	}
	if n := e.count(`org_id = $1 AND seq = ANY($2) AND scope = 'any' AND team_ids = ARRAY[$3::uuid]`, e.org, seqs, e.hid); n != int64(len(seqs)) {
		t.Fatalf("%d of %d rows placed after the resume", n, len(seqs))
	}
	// A cancelled context stops a run and places nothing.
	late := e.legacy(e.org, 3, events.TeamUpdated, "team", e.orb, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := events.Backfill(ctx, e.pool, events.BackfillOptions{}); err == nil {
		t.Fatal("a cancelled run reports no error")
	}
	if n := e.count(`org_id = $1 AND seq = ANY($2) AND scope = 'legacy'`, e.org, late); n != 3 {
		t.Fatalf("a cancelled run placed %d rows", 3-n)
	}
}

// A second run changes nothing; a legacy row written later (an older image
// after a rollback) is placed by the next run (the next boot).
func TestBackfill_Rerun(t *testing.T) {
	e := newBfEnv(t)
	e.legacy(e.org, 20, events.TeamUpdated, "team", e.orb, `{}`)
	e.legacy(e.org, 2, events.IssueUpdated, "issue", uuid.New(), `{}`)
	first := e.run(events.BackfillOptions{})
	if first.Placed != 20 || first.Legacy != 2 {
		t.Fatalf("first run %+v", first)
	}
	snap := e.rows()
	second := e.run(events.BackfillOptions{})
	if second.Placed != 0 || second.Scanned != 2 {
		t.Fatalf("second run %+v, want only the two orphans read and nothing placed", second)
	}
	if after := e.rows(); !sameRows(snap, after) {
		t.Fatal("the second run changed a row")
	}
	late := e.legacy(e.org, 1, events.TeamCreated, "team", e.hid, `{}`)
	third := e.run(events.BackfillOptions{})
	if third.Placed != 1 {
		t.Fatalf("third run %+v, want the late legacy row placed", third)
	}
	if n := e.count(`org_id = $1 AND seq = $2 AND scope = 'any' AND team_ids = ARRAY[$3::uuid]`, e.org, late[0], e.hid); n != 1 {
		t.Fatal("the late row is not placed")
	}
}

func sameRows(a, b map[[2]string]stored) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || v.scope != w.scope || !slices.Equal(v.teams, w.teams) {
			return false
		}
	}
	return true
}

// Two instances: while one holds the lock the other does nothing; two runs
// at once place every row once between them.
func TestBackfill_AdvisoryLock(t *testing.T) {
	e := newBfEnv(t)
	e.legacy(e.org, 300, events.TeamUpdated, "team", e.orb, `{}`)
	ctx := context.Background()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('devops:events:backfill', 0))`); err != nil {
		t.Fatal(err)
	}
	res, locked, err := events.Backfill(ctx, e.pool, events.BackfillOptions{})
	if err != nil || locked || res != (events.BackfillResult{}) {
		t.Fatalf("with the lock held elsewhere: %+v locked %v err %v", res, locked, err)
	}
	if n := e.count(`scope = 'legacy'`); n != 300 {
		t.Fatalf("%d legacy rows, want 300: the locked-out run wrote", n)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('devops:events:backfill', 0))`); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var placed int64
	for range 2 {
		wg.Go(func() {
			r, _, err := events.Backfill(ctx, e.pool, events.BackfillOptions{BatchSize: 50, Pause: -1})
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			placed += r.Placed
			mu.Unlock()
		})
	}
	wg.Wait()
	if placed != 300 || e.count(`scope = 'legacy'`) != 0 {
		t.Fatalf("two runs placed %d, want 300 in all", placed)
	}
}

// An id of another org's row never places an event: the entity and the
// payload ids are looked up in the event's own org.
func TestBackfill_CrossOrg(t *testing.T) {
	e := newBfEnv(t)
	byEntity := e.legacy(e.org, 1, events.TeamUpdated, "team", e.oth, `{}`)
	byPayload := e.legacy(e.org, 1, events.IssueDeleted, "issue", uuid.New(), `{"teamId":"`+e.oth.String()+`"}`)
	cycle := e.legacy(e.org, 1, events.CycleDeleted, "cycle", uuid.New(), `{"teamId":"`+e.oth.String()+`"}`)
	own := e.legacy(e.other, 1, events.TeamUpdated, "team", e.oth, `{}`)
	e.run(events.BackfillOptions{})
	for _, s := range [][]int64{byEntity, byPayload, cycle} {
		if n := e.count(`org_id = $1 AND seq = $2 AND scope = 'legacy'`, e.org, s[0]); n != 1 {
			t.Errorf("orbit row %d naming the other org's team was placed", s[0])
		}
	}
	if n := e.count(`org_id = $1 AND seq = $2 AND scope = 'any' AND team_ids = ARRAY[$3::uuid]`, e.other, own[0], e.oth); n != 1 {
		t.Error("the other org's own row was not placed")
	}
}

// The feed (#55) before and after the job: a restricted member reads no
// legacy row before, and after reads the rows of its team and the org rows
// only, never an orphan. Admins and all-teams members read the same rows
// before and after.
func TestBackfill_Feed(t *testing.T) {
	e := newBfEnv(t)
	orb := e.legacy(e.org, 3, events.TeamUpdated, "team", e.orb, `{}`)
	hid := e.legacy(e.org, 3, events.TeamUpdated, "team", e.hid, `{}`)
	org := e.legacy(e.org, 2, events.OrgUpdated, "org", e.org, `{}`)
	orphan := e.legacy(e.org, 2, events.IssueUpdated, "issue", uuid.New(), `{}`)
	admin, member := e.feed(true, true), e.feed(false, true)
	if got := e.feed(false, false, e.orb); len(got) != 0 {
		t.Fatalf("before the job a restricted member reads %v", got)
	}
	e.run(events.BackfillOptions{})
	if got := e.feed(true, true); !slices.Equal(got, admin) {
		t.Errorf("admin feed changed: %v, want %v", got, admin)
	}
	if got := e.feed(false, true); !slices.Equal(got, member) {
		t.Errorf("all-teams member feed changed: %v, want %v", got, member)
	}
	want := slices.Concat(orb, org)
	slices.Sort(want)
	if got := e.feed(false, false, e.orb); !slices.Equal(got, want) {
		t.Errorf("restricted member of ORB reads %v, want %v (not HID %v, not orphans %v)", got, want, hid, orphan)
	}
	if got := e.feed(false, false); !slices.Equal(got, org) {
		t.Errorf("restricted member of no team reads %v, want the org rows %v", got, org)
	}
}

// The boot job logs counts only: never a payload.
func TestBackfill_LogsCountsOnly(t *testing.T) {
	e := newBfEnv(t)
	e.legacy(e.org, 2, events.IssueUpdated, "issue", uuid.New(), `{"title":"secret-title-text"}`)
	e.legacy(e.org, 2, events.TeamUpdated, "team", e.orb, `{"key":"secret-key-text"}`)
	var buf bytes.Buffer
	events.RunBackfill(context.Background(), e.pool, slog.New(slog.NewJSONHandler(&buf, nil)))
	out := buf.String()
	if !strings.Contains(out, `"placed":2`) || !strings.Contains(out, `"legacy":2`) {
		t.Fatalf("log %q has no counts", out)
	}
	if strings.Contains(out, "secret-") || strings.Contains(out, e.orb.String()) {
		t.Fatalf("log %q holds payload text or an id", out)
	}
}

// An issue.deleted row of the shape before 6a names the parent and counts the
// children (T20 #42): it stays legacy. The shape of today is placed.
func TestBackfill_OldIssueDeletedShape(t *testing.T) {
	e := newBfEnv(t)
	team := `"teamId":"` + e.orb.String() + `"`
	oldParent := e.legacy(e.org, 1, events.IssueDeleted, "issue", uuid.New(), `{`+team+`,"parentId":"`+uuid.NewString()+`"}`)
	oldChildren := e.legacy(e.org, 1, events.IssueDeleted, "issue", uuid.New(), `{`+team+`,"parentId":null,"detachedChildren":3}`)
	current := e.legacy(e.org, 1, events.IssueDeleted, "issue", uuid.New(), `{"id":"`+uuid.NewString()+`","identifier":"ORB-9",`+team+`}`)
	res := e.run(events.BackfillOptions{})
	if res.Placed != 1 || res.Legacy != 2 {
		t.Fatalf("result %+v, want one placed and two legacy", res)
	}
	for _, s := range [][]int64{oldParent, oldChildren} {
		if n := e.count(`org_id = $1 AND seq = $2 AND scope = 'legacy'`, e.org, s[0]); n != 1 {
			t.Errorf("old-shape issue.deleted row %d was placed", s[0])
		}
	}
	if n := e.count(`org_id = $1 AND seq = $2 AND scope = 'any' AND team_ids = ARRAY[$3::uuid]`, e.org, current[0], e.orb); n != 1 {
		t.Error("the current-shape issue.deleted row was not placed")
	}
	if got := e.feed(false, false, e.orb); !slices.Equal(got, current) {
		t.Errorf("restricted member of ORB reads %v, want only %v", got, current)
	}
}

// LAUN-225: a legacy member.access_changed row (one can come back as legacy
// after a migration down past 0038 and up) is admin data. Before the job an
// all-teams member and a restricted member read none of it, the admin reads
// it; the job places it with scope admin, and the feeds stay the same.
func TestBackfill_MemberAccessChangedStaysAdmin(t *testing.T) {
	e := newBfEnv(t)
	rows := e.legacy(e.org, 2, events.MemberAccessChanged, "member", uuid.New(),
		`{"userId":"`+uuid.NewString()+`","teamAccess":"selected","teamIds":["`+e.hid.String()+`"]}`)
	plain := e.legacy(e.org, 1, events.TeamUpdated, "team", e.orb, `{}`)
	check := func(when string) {
		t.Helper()
		if got := e.feed(true, true); !slices.Equal(got, slices.Concat(rows, plain)) {
			t.Errorf("%s: the admin reads %v, want %v", when, got, slices.Concat(rows, plain))
		}
		if got := e.feed(false, true); slices.ContainsFunc(got, func(s int64) bool { return slices.Contains(rows, s) }) {
			t.Errorf("%s: an all-teams member reads %v, holding the admin rows %v", when, got, rows)
		}
		for _, teams := range [][]uuid.UUID{{e.orb}, {e.hid}, nil} {
			if got := e.feed(false, false, teams...); slices.ContainsFunc(got, func(s int64) bool { return slices.Contains(rows, s) }) {
				t.Errorf("%s: a restricted member of %v reads %v", when, teams, got)
			}
		}
	}
	check("before the job")
	if got := e.feed(false, true); !slices.Equal(got, plain) {
		t.Fatalf("before the job the all-teams member reads %v, want the other legacy row %v", got, plain)
	}
	e.run(events.BackfillOptions{})
	for _, s := range rows {
		if n := e.count(`org_id = $1 AND seq = $2 AND scope = 'admin' AND team_ids = '{}'`, e.org, s); n != 1 {
			t.Errorf("row %d was not placed as admin", s)
		}
	}
	check("after the job")
}
