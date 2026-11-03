package tracker_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// The cycle job against a real database, with the clock injected: every run
// gets an explicit `now`, never time.Now.

type cycEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *dbq.Queries
	svc  *tracker.Service
	org  *auth.Org
	team dbq.Team
	chen *auth.Principal
	loc  *time.Location
}

func newCycEnv(t *testing.T) *cycEnv {
	t.Helper()
	pool, _ := testdb.New(t)
	ctx := context.Background()
	testdb.Fixture(t, pool)
	q := dbq.New(pool)
	o, err := q.GetOrgBySlug(ctx, testdb.OrgSlug)
	if err != nil {
		t.Fatal(err)
	}
	team, err := q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: o.ID, Key: testdb.TeamKey})
	if err != nil {
		t.Fatal(err)
	}
	u, err := q.GetUserByUsername(ctx, "chen")
	if err != nil {
		t.Fatal(err)
	}
	loc, err := tracker.LoadZone("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return &cycEnv{t: t, pool: pool, q: q, svc: tracker.New(pool), team: team, loc: loc,
		org:  &auth.Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Role: "admin", Access: auth.TeamAccess{All: true}},
		chen: &auth.Principal{UserID: u.ID, Username: u.Username, Name: u.Name, Scopes: []auth.Scope{auth.ScopeAdmin}}}
}

// enable turns cycles on with the Orbit cadence (the 0001 defaults: 14 days,
// Sunday 00:00 Europe/London, rollover on).
func (e *cycEnv) enable(rollover bool) {
	e.t.Helper()
	e.exec(`UPDATE teams SET cycles_enabled = true, cycle_rollover = $2 WHERE id = $1`, e.team.ID, rollover)
}

func (e *cycEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *cycEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (e *cycEnv) run(now time.Time) tracker.CycleRun {
	e.t.Helper()
	r, err := e.svc.MaintainCycles(context.Background(), now)
	if err != nil {
		e.t.Fatalf("MaintainCycles(%s): %v", now, err)
	}
	return r
}

func (e *cycEnv) cycles() []dbq.Cycle {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT * FROM cycles WHERE team_id = $1 ORDER BY number`, e.team.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	cs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dbq.Cycle])
	if err != nil {
		e.t.Fatal(err)
	}
	return cs
}

// checkChain asserts the invariants of a team's cycles: numbers 1..n with
// no gap, each cycle starting where the last ended, and every start on the
// cadence boundary (Sunday 00:00 London) whatever the DST state.
func (e *cycEnv) checkChain(cs []dbq.Cycle) {
	e.t.Helper()
	for i, c := range cs {
		if c.Number != int32(i+1) {
			e.t.Fatalf("cycle at %d has number %d", i, c.Number)
		}
		l := c.StartsAt.In(e.loc)
		if l.Weekday() != time.Sunday || l.Hour() != 0 || l.Minute() != 0 {
			e.t.Fatalf("cycle %d starts %s local, not Sunday 00:00", c.Number, l)
		}
		if i > 0 && !c.StartsAt.Equal(cs[i-1].EndsAt) {
			e.t.Fatalf("cycle %d starts %s, cycle %d ended %s", c.Number, c.StartsAt, i, cs[i-1].EndsAt)
		}
	}
}

func (e *cycEnv) issue(title, state string, cycleID uuid.UUID, estimate int32) tracker.Issue {
	e.t.Helper()
	is, err := e.svc.CreateIssue(context.Background(), e.org, e.chen,
		tracker.CreateIssueInput{Team: "ORB", Title: title, State: state, CycleID: &cycleID, Estimate: &estimate})
	if err != nil {
		e.t.Fatalf("create issue %q: %v", title, err)
	}
	return is
}

func (e *cycEnv) issueCycle(id uuid.UUID) *uuid.UUID {
	e.t.Helper()
	var c *uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT cycle_id FROM issues WHERE id = $1`, id).Scan(&c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func at(loc *time.Location, y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, loc)
}

// AC: with the Orbit cadence the next cycle exists before the current one
// ends. The first run makes cycle 1 (the current one, from the last Sunday
// 00:00 London) and cycle 2, and a second run at the same instant does
// nothing.
func TestCycleJob_FirstRunCreatesCurrentAndNext(t *testing.T) {
	e := newCycEnv(t)
	now := at(e.loc, 2026, 10, 21, 12) // a Wednesday in BST
	if r := e.run(now); r.Created != 0 || r.Teams != 0 {
		t.Fatalf("cycles are off by default, but the run did %+v", r)
	}
	e.enable(true)
	r := e.run(now)
	if r.Created != 2 {
		t.Fatalf("created %d cycles, want 2", r.Created)
	}
	cs := e.cycles()
	e.checkChain(cs)
	if want := at(e.loc, 2026, 10, 18, 0); !cs[0].StartsAt.Equal(want) {
		t.Fatalf("cycle 1 starts %s, want %s", cs[0].StartsAt, want)
	}
	// Cycle 1 spans the 2026-10-25 change: it ends at 00:00 GMT, not 23:00.
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !cs[0].EndsAt.Equal(want) {
		t.Fatalf("cycle 1 ends %s, want %s (no DST drift)", cs[0].EndsAt.UTC(), want)
	}
	if !cs[1].StartsAt.After(now) {
		t.Fatal("cycle 2 is not upcoming")
	}
	if r := e.run(now); r.Created != 0 || r.Completed != 0 {
		t.Fatalf("second run at the same instant did %+v", r)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.created'`); n != 2 {
		t.Fatalf("%d cycle.created events, want 2", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.created' AND actor_id IS NOT NULL`); n != 0 {
		t.Fatalf("job events have an actor")
	}
}

// AC: a cycle starting Sunday 00:00 local must not drift across DST, and
// the next cycle always exists. A fake clock walks one day at a time over
// fourteen months (two autumn and one spring change).
func TestCycleJob_DailyForFourteenMonthsAcrossDST(t *testing.T) {
	e := newCycEnv(t)
	e.enable(true)
	start := at(e.loc, 2026, 9, 2, 6)
	for now := start; now.Before(start.AddDate(1, 2, 0)); now = now.AddDate(0, 0, 1) {
		e.run(now)
		cs := e.cycles()
		last := cs[len(cs)-1]
		if !last.StartsAt.After(now) {
			t.Fatalf("at %s no upcoming cycle (last %d starts %s)", now, last.Number, last.StartsAt)
		}
		var current int
		for _, c := range cs {
			if !c.StartsAt.After(now) && c.EndsAt.After(now) {
				current++
				if c.CompletedAt != nil {
					t.Fatalf("at %s the running cycle %d is completed", now, c.Number)
				}
			} else if !c.EndsAt.After(now) && c.CompletedAt == nil {
				t.Fatalf("at %s ended cycle %d is not completed", now, c.Number)
			}
		}
		if current != 1 {
			t.Fatalf("at %s %d cycles are running", now, current)
		}
	}
	cs := e.cycles()
	e.checkChain(cs)
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.created'`); n != len(cs) {
		t.Fatalf("%d cycle.created events for %d cycles", n, len(cs))
	}
	if len(cs) < 30 {
		t.Fatalf("only %d cycles in fourteen months", len(cs))
	}
}

// AC: the server was off for three weeks. The next run creates the missing
// cycles in order, once, completes the ended ones in order and walks the
// open issues along the chain (1 -> 2 -> 3).
func TestCycleJob_CatchUpAfterThreeWeeksOff(t *testing.T) {
	e := newCycEnv(t)
	e.enable(true)
	e.run(at(e.loc, 2026, 10, 21, 12)) // cycle 1 is 2026-10-18 .. 2026-11-01
	t0 := at(e.loc, 2026, 10, 31, 12)  // the last run before the outage: cycle 1's last day
	e.run(t0)
	cs := e.cycles()
	open := e.issue("still open", "Todo", cs[0].ID, 3)
	done := e.issue("finished", "Done", cs[0].ID, 2)

	t1 := t0.AddDate(0, 0, 21) // Saturday 2026-11-21: cycle 3 is running
	r := e.run(t1)
	if r.Created != 2 || r.Completed != 2 || r.Moved != 2 {
		t.Fatalf("catch-up run did %+v, want 2 created, 2 completed, 2 moves", r)
	}
	cs = e.cycles()
	e.checkChain(cs)
	if len(cs) != 4 {
		t.Fatalf("%d cycles after catch-up, want 4", len(cs))
	}
	for _, c := range cs[:2] {
		if c.CompletedAt == nil || !c.CompletedAt.Equal(c.EndsAt) {
			t.Fatalf("cycle %d completed_at = %v, want its end %s", c.Number, c.CompletedAt, c.EndsAt)
		}
	}
	if got := e.issueCycle(open.ID); got == nil || *got != cs[2].ID {
		t.Fatalf("open issue is in %v, want cycle 3", got)
	}
	if got := e.issueCycle(done.ID); got == nil || *got != cs[0].ID {
		t.Fatalf("done issue moved to %v", got)
	}
	// The created events are in number order (the order they were made).
	rows, err := e.pool.Query(context.Background(),
		`SELECT (payload->>'number')::int FROM events WHERE type = 'cycle.created' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	nums, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(nums) != "[1 2 3 4]" {
		t.Fatalf("cycle.created numbers in feed order: %v", nums)
	}
	if r := e.run(t1); r.Created+r.Completed+r.Moved != 0 {
		t.Fatalf("a repeat of the catch-up did %+v", r)
	}
}

// AC: open issues roll over; closed ones stay; rollover writes an event but
// no state history (the state does not change); the final snapshot counts
// the cycle as it ended.
func TestCycleJob_RolloverMovesOnlyOpenIssues(t *testing.T) {
	e := newCycEnv(t)
	e.enable(true)
	t0 := at(e.loc, 2026, 11, 4, 9)
	e.run(t0)
	cs := e.cycles()
	c1, c2 := cs[0], cs[1]
	todo := e.issue("todo", "Todo", c1.ID, 1)
	prog := e.issue("in progress", "In Progress", c1.ID, 2)
	rev := e.issue("in review", "In Review", c1.ID, 3)
	back := e.issue("backlog", "Backlog", c1.ID, 0)
	done := e.issue("done", "Done", c1.ID, 5)
	canc := e.issue("canceled", "Canceled", c1.ID, 8)
	dup := e.issue("duplicate", "Duplicate", c1.ID, 1)
	tri := e.issue("triage", "Triage", c1.ID, 4) // LAUN-20: not planned work, never rolled
	arch := e.issue("archived", "Todo", c1.ID, 1)
	e.exec(`UPDATE issues SET archived_at = now() WHERE id = $1`, arch.ID)
	historyBefore := e.count(`SELECT count(*) FROM issue_state_changes`)

	r := e.run(c1.EndsAt.Add(time.Minute))
	if r.Completed != 1 || r.Moved != 4 {
		t.Fatalf("run did %+v, want 1 completed and 4 moved", r)
	}
	for _, is := range []tracker.Issue{todo, prog, rev, back} {
		if got := e.issueCycle(is.ID); got == nil || *got != c2.ID {
			t.Fatalf("%s is in %v, want cycle 2", is.Title, got)
		}
		var v int32
		if err := e.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE id = $1`, is.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != is.Version+1 {
			t.Fatalf("%s version %d, want %d", is.Title, v, is.Version+1)
		}
	}
	for _, is := range []tracker.Issue{done, canc, dup, tri, arch} {
		if got := e.issueCycle(is.ID); got == nil || *got != c1.ID {
			t.Fatalf("%s moved to %v", is.Title, got)
		}
	}
	if n := e.count(`SELECT count(*) FROM issue_state_changes`); n != historyBefore {
		t.Fatalf("rollover wrote %d state history rows", n-historyBefore)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'issue.updated' AND payload->>'reason' = 'rollover'
		AND payload->'changes' = '["cycleId"]'::jsonb AND (payload->>'toCycleId')::uuid = $1`, c2.ID); n != 4 {
		t.Fatalf("%d rollover events, want 4", n)
	}
	if n := e.count(`SELECT count(*) FROM issue_activity WHERE kind = 'cycle' AND source = 'system' AND actor_id IS NULL
		AND ("to"->>'id')::uuid = $1 AND ("from"->>'id')::uuid = $2`, c2.ID, c1.ID); n != 4 {
		t.Fatalf("%d rollover activity rows, want 4", n)
	}
	var s dbq.CycleSnapshot
	err := e.pool.QueryRow(context.Background(), `SELECT * FROM cycle_snapshots WHERE cycle_id = $1 AND snapshot_date = $2`,
		c1.ID, "2026-11-14").Scan(&s.ID, &s.OrgID, &s.CycleID, &s.SnapshotDate, &s.ScopeCount, &s.StartedCount,
		&s.CompletedCount, &s.ScopePoints, &s.CompletedPoints, &s.CreatedAt, &s.UpdatedAt, &s.TriageCount,
		&s.BacklogCount, &s.UnstartedCount, &s.CanceledCount, &s.StartedPoints)
	if err != nil {
		t.Fatalf("final snapshot of cycle 1 (its last local day, 2026-11-14): %v", err)
	}
	// Not archived: todo, prog, rev, back, done (scope 5) + canceled, duplicate.
	// The triage issue (4 points) is counted as triage only, not in scope.
	if s.TriageCount != 1 || s.ScopeCount != 5 || s.UnstartedCount != 1 || s.StartedCount != 2 || s.BacklogCount != 1 ||
		s.CompletedCount != 1 || s.CanceledCount != 2 || s.ScopePoints != 11 || s.StartedPoints != 5 || s.CompletedPoints != 5 {
		t.Fatalf("final snapshot %+v", s)
	}
}

// AC: with the team setting off, nothing moves; the cycle still completes.
func TestCycleJob_RolloverOffMovesNothing(t *testing.T) {
	e := newCycEnv(t)
	e.enable(false)
	e.run(at(e.loc, 2026, 11, 4, 9))
	cs := e.cycles()
	todo := e.issue("todo", "Todo", cs[0].ID, 1)
	r := e.run(cs[0].EndsAt.Add(time.Hour))
	if r.Completed != 1 || r.Moved != 0 {
		t.Fatalf("run did %+v", r)
	}
	if got := e.issueCycle(todo.ID); got == nil || *got != cs[0].ID {
		t.Fatalf("issue moved to %v with rollover off", got)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE payload->>'reason' = 'rollover'`); n != 0 {
		t.Fatalf("%d rollover events with rollover off", n)
	}
}

// Two instances (and a boot run racing the hourly one) at the same instant:
// the advisory lock serialises them, so numbers stay 1..n with no gap and
// no cycle or event is written twice.
func TestCycleJob_ConcurrentRunsNoGapsNoDuplicates(t *testing.T) {
	e := newCycEnv(t)
	e.enable(true)
	now := at(e.loc, 2026, 10, 21, 12)
	runOnce := func(now time.Time) {
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				// A Service per goroutine, as separate instances would have.
				if _, err := tracker.New(e.pool).MaintainCycles(context.Background(), now); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent run: %v", err)
		}
	}
	runOnce(now)
	if cs := e.cycles(); len(cs) != 2 {
		t.Fatalf("%d cycles after 8 concurrent first runs, want 2", len(cs))
	}
	runOnce(now.AddDate(0, 0, 50)) // a catch-up, raced
	cs := e.cycles()
	e.checkChain(cs)
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.created'`); n != len(cs) {
		t.Fatalf("%d cycle.created events for %d cycles", n, len(cs))
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.completed'`); n != e.count(`SELECT count(*) FROM cycles WHERE completed_at IS NOT NULL`) {
		t.Fatalf("cycle.completed events (%d) do not match completed cycles", n)
	}
}

// One snapshot row per cycle per day: a second run the same day rewrites
// the row, the next day adds one.
func TestCycleJob_SnapshotIsOneRowPerDay(t *testing.T) {
	e := newCycEnv(t)
	e.enable(true)
	day1 := at(e.loc, 2026, 11, 3, 8)
	e.run(day1)
	cs := e.cycles()
	cur := cs[0]
	is := e.issue("work", "Todo", cur.ID, 3)
	e.run(day1.Add(2 * time.Hour))
	if n := e.count(`SELECT count(*) FROM cycle_snapshots WHERE cycle_id = $1`, cur.ID); n != 1 {
		t.Fatalf("%d rows after two runs on one day", n)
	}
	if _, err := e.svc.UpdateIssue(context.Background(), e.org, e.chen, is.Identifier,
		tracker.UpdateIssueInput{State: optString("Done")}); err != nil {
		t.Fatal(err)
	}
	e.run(day1.Add(3 * time.Hour))
	if n := e.count(`SELECT completed_count FROM cycle_snapshots WHERE cycle_id = $1 AND snapshot_date = '2026-11-03'`, cur.ID); n != 1 {
		t.Fatalf("today's row was not refreshed: completed_count %d", n)
	}
	// A refresh that changes nothing writes nothing: the GET (read scope)
	// cannot churn the row.
	stamp := func() time.Time {
		var u time.Time
		if err := e.pool.QueryRow(context.Background(), `SELECT updated_at FROM cycle_snapshots
			WHERE cycle_id = $1 AND snapshot_date = '2026-11-03'`, cur.ID).Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	before := stamp()
	e.svc.SetClock(func() time.Time { return day1.Add(4 * time.Hour) })
	for range 2 {
		if _, err := e.svc.CycleSnapshots(context.Background(), e.org, cur.ID); err != nil {
			t.Fatal(err)
		}
	}
	e.run(day1.Add(5 * time.Hour))
	if after := stamp(); !after.Equal(before) {
		t.Fatalf("an unchanged refresh rewrote the row (%s -> %s)", before, after)
	}
	e.run(day1.AddDate(0, 0, 1))
	if n := e.count(`SELECT count(*) FROM cycle_snapshots WHERE cycle_id = $1`, cur.ID); n != 2 {
		t.Fatalf("%d rows after two days", n)
	}
	// The database refuses a second row for a day, whoever writes it.
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO cycle_snapshots (org_id, cycle_id, snapshot_date) VALUES ($1, $2, '2026-11-03')`, e.org.ID, cur.ID)
	if err == nil {
		t.Fatal("a duplicate snapshot day was accepted")
	}
}

func optString(v string) httpx.Opt[string] { return httpx.Opt[string]{Set: true, V: v} }

// Review blocker: estimates are int4 with no upper bound in old data, so a
// per-cycle sum can pass 2^31. It must not fail the job (which would stop the
// team's cycles for good) nor the snapshot GET. The rows are written
// directly, as old or imported data could be, past the API's 0-1000 bound.
func TestCycleJob_HugeEstimatesDoNotBreakTheJob(t *testing.T) {
	e := newCycEnv(t)
	e.enable(true)
	now := at(e.loc, 2026, 11, 4, 9)
	e.run(now)
	cur := e.cycles()[0]
	a := e.issue("huge a", "Done", cur.ID, 1)
	b := e.issue("huge b", "Done", cur.ID, 1)
	e.exec(`UPDATE issues SET estimate = 2147483647 WHERE id = ANY($1)`, []uuid.UUID{a.ID, b.ID})
	e.run(now.Add(time.Hour))
	e.svc.SetClock(func() time.Time { return now.Add(2 * time.Hour) })
	snaps, err := e.svc.CycleSnapshots(context.Background(), e.org, cur.ID)
	if err != nil {
		t.Fatalf("snapshot GET: %v", err)
	}
	const want = int64(2 * 2147483647)
	if len(snaps) != 1 || snaps[0].ScopePoints != want || snaps[0].CompletedPoints != want {
		t.Fatalf("snapshots %+v, want points %d", snaps, want)
	}
	// The team PATCH (which runs the job) and the completion still work.
	if _, err := e.svc.UpdateTeam(context.Background(), e.org, e.chen, "ORB",
		tracker.UpdateTeamInput{CycleRollover: httpx.Opt[bool]{Set: true, V: false}}); err != nil {
		t.Fatalf("team PATCH: %v", err)
	}
	if r := e.run(cur.EndsAt.Add(time.Minute)); r.Completed != 1 {
		t.Fatalf("completion run did %+v", r)
	}
}

// Review: the catch-up cap commits the partial chain (no rollback), and the
// next runs finish the work. Ended cycles without a successor wait.
func TestCycleJob_CapCommitsPartialChainAndContinues(t *testing.T) {
	e := newCycEnv(t)
	e.svc.SetMaxCyclesPerRun(3)
	e.enable(true)
	t0 := at(e.loc, 2026, 10, 21, 12)
	e.run(t0) // cycles 1 and 2
	open := e.issue("open", "Todo", e.cycles()[0].ID, 1)
	t1 := t0.AddDate(0, 0, 7*20) // twenty weeks later: ten cycles behind
	r := e.run(t1)
	if r.Created != 3 {
		t.Fatalf("capped run created %d, want 3", r.Created)
	}
	cs := e.cycles()
	e.checkChain(cs)
	last := cs[len(cs)-1]
	for _, c := range cs {
		if c.ID == last.ID {
			if c.CompletedAt != nil {
				t.Fatal("the last cycle (no successor yet) was completed")
			}
		} else if c.CompletedAt == nil {
			t.Fatalf("cycle %d has a successor but was not completed", c.Number)
		}
	}
	if got := e.issueCycle(open.ID); got == nil || *got != last.ID {
		t.Fatalf("issue in %v, want the last cycle so far", got)
	}
	for i := 0; i < 10; i++ {
		if r = e.run(t1); r.Created == 0 {
			break
		}
	}
	cs = e.cycles()
	e.checkChain(cs)
	if !cs[len(cs)-1].StartsAt.After(t1) {
		t.Fatal("never caught up")
	}
	var current dbq.Cycle
	for _, c := range cs {
		if !c.StartsAt.After(t1) && c.EndsAt.After(t1) {
			current = c
		} else if !c.EndsAt.After(t1) && c.CompletedAt == nil {
			t.Fatalf("ended cycle %d left open", c.Number)
		}
	}
	if got := e.issueCycle(open.ID); got == nil || *got != current.ID {
		t.Fatalf("issue in %v, want the current cycle %d", got, current.Number)
	}
}

// Review: a gap in the numbers (a partial import) must not stop the job.
// The successor is found by time, not by number + 1.
func TestCycleJob_GapInNumbersStillRollsOver(t *testing.T) {
	e := newCycEnv(t)
	c1Start, c3Start := at(e.loc, 2026, 10, 18, 0), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	e.exec(`INSERT INTO cycles (org_id, team_id, number, starts_at, ends_at) VALUES ($1, $2, 1, $3, $4), ($1, $2, 3, $4, $5)`,
		e.org.ID, e.team.ID, c1Start, c3Start, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))
	cs := e.cycles()
	open := e.issue("open", "Todo", cs[0].ID, 1)
	e.enable(true)
	r, err := e.svc.MaintainCycles(context.Background(), at(e.loc, 2026, 11, 4, 9))
	if err != nil {
		t.Fatalf("run with a gap: %v", err)
	}
	if r.Completed != 1 || r.Moved != 1 || r.Created != 1 {
		t.Fatalf("run did %+v", r)
	}
	if got := e.issueCycle(open.ID); got == nil || *got != cs[1].ID {
		t.Fatalf("issue in %v, want cycle 3", got)
	}
	if cs = e.cycles(); cs[len(cs)-1].Number != 4 {
		t.Fatalf("new cycle is %d, want 4 (max + 1)", cs[len(cs)-1].Number)
	}
}

// One broken team (a zone that no longer loads) is logged and skipped; the
// other teams still get their cycles.
func TestCycleJob_OneBrokenTeamDoesNotStopTheOthers(t *testing.T) {
	e := newCycEnv(t)
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	web, err := tracker.CreateTeamTx(context.Background(), e.q.WithTx(tx), e.org.ID, "WEB", "Web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE teams SET cycles_enabled = true`)
	e.exec(`UPDATE teams SET cycle_timezone = 'Mars/Olympus' WHERE id = $1`, e.team.ID)
	r, err := e.svc.MaintainCycles(context.Background(), at(e.loc, 2026, 11, 4, 9))
	if err == nil {
		t.Fatal("the broken team's error was not reported")
	}
	if r.Created != 2 || e.count(`SELECT count(*) FROM cycles WHERE team_id = $1`, web.ID) != 2 {
		t.Fatalf("the healthy team did not get its cycles: %+v", r)
	}
}

// Review nit: turning cycles back on after months does not back-date every
// missed cycle; the chain restarts at the current cycle (decisions-T3 #34).
// A server that was merely off still backfills (CatchUpAfterThreeWeeksOff).
func TestUpdateTeam_ReEnableAfterLongPauseRestartsAtCurrent(t *testing.T) {
	e := newCycEnv(t)
	ctx := context.Background()
	on := func(v bool) {
		t.Helper()
		if _, err := e.svc.UpdateTeam(ctx, e.org, e.chen, "ORB", tracker.UpdateTeamInput{CyclesEnabled: httpx.Opt[bool]{Set: true, V: v}}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := at(e.loc, 2026, 10, 21, 12)
	e.svc.SetClock(func() time.Time { return t0 })
	on(true)
	open := e.issue("open", "Todo", e.cycles()[0].ID, 1)
	on(false)
	t1 := t0.AddDate(0, 6, 0) // 2027-04-21, a Wednesday in BST
	e.svc.SetClock(func() time.Time { return t1 })
	before := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.created'`)
	on(true)
	cs := e.cycles()
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.created'`) - before; n != 2 || len(cs) != 4 {
		t.Fatalf("re-enable made %d cycles (%d in all), want 2: the current and the next", n, len(cs))
	}
	cur := cs[2]
	if want := at(e.loc, 2027, 4, 18, 0); cur.Number != 3 || !cur.StartsAt.Equal(want) {
		t.Fatalf("restart cycle %d starts %s, want 3 at %s", cur.Number, cur.StartsAt, want)
	}
	if got := e.issueCycle(open.ID); got == nil || *got != cur.ID {
		t.Fatalf("open issue in %v, want the current cycle", got)
	}
	for _, c := range cs[:2] {
		if c.CompletedAt == nil {
			t.Fatalf("old cycle %d not completed", c.Number)
		}
	}
}

// Review nit: the team PATCH and the job run at the same time without a
// deadlock (they take the advisory lock in the same order) and leave a
// valid chain.
func TestUpdateTeam_ConcurrentWithTheJob(t *testing.T) {
	e := newCycEnv(t)
	now := at(e.loc, 2026, 10, 21, 12)
	e.svc.SetClock(func() time.Time { return now })
	e.enable(true)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 8 {
		wg.Go(func() {
			if _, err := tracker.New(e.pool).MaintainCycles(context.Background(), now.AddDate(0, 0, 7*i)); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			length := 14
			if i%2 == 1 {
				length = 7
			}
			if _, err := e.svc.UpdateTeam(context.Background(), e.org, e.chen, "ORB", tracker.UpdateTeamInput{
				CycleLengthDays: httpx.Opt[int]{Set: true, V: length},
				CycleRollover:   httpx.Opt[bool]{Set: true, V: i%3 != 0},
			}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent PATCH and job: %v", err)
	}
	e.checkChain(e.cycles())
}
