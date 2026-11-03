package tracker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Cycle options (LAUN-172, decisions-T27). The clock is injected: the service
// reads `clk`, and the job gets an explicit `now`.

type optEnv struct {
	*cycEnv
	mu  sync.Mutex
	now time.Time
}

func (o *optEnv) clock() time.Time { o.mu.Lock(); defer o.mu.Unlock(); return o.now }
func (o *optEnv) setNow(t time.Time) {
	o.mu.Lock()
	o.now = t
	o.mu.Unlock()
}

// newOptEnv: cycles on, the clock on Wed 2026-10-21 12:00 London. After the
// first run cycle 1 is 10-18 .. 11-01 (current) and cycle 2 is 11-01 .. 11-15.
func newOptEnv(t *testing.T, rollover bool) *optEnv {
	t.Helper()
	e := &optEnv{cycEnv: newCycEnv(t)}
	e.svc.SetClock(e.clock)
	e.enable(rollover)
	e.setNow(at(e.loc, 2026, 10, 21, 12))
	e.run(e.clock())
	if n := len(e.cycles()); n != 2 {
		t.Fatalf("%d cycles after the first run", n)
	}
	return e
}

func (e *optEnv) wantErr(err error, status int, code httpx.Code) {
	e.t.Helper()
	var he *httpx.Error
	if !errors.As(err, &he) || he.Status != status || he.Code != code {
		e.t.Fatalf("error %v, want %d %s", err, status, code)
	}
}

func (e *optEnv) redate(id uuid.UUID, start, end string) error {
	in := tracker.UpdateCycleInput{}
	if start != "" {
		in.StartsAt = optString(start)
	}
	if end != "" {
		in.EndsAt = optString(end)
	}
	_, err := e.svc.UpdateCycle(context.Background(), e.org, e.chen, id, in)
	return err
}

func (e *optEnv) complete(id uuid.UUID) error {
	_, err := e.svc.CompleteCycle(context.Background(), e.org, e.chen, id)
	return err
}

func (e *optEnv) del(id uuid.UUID) error {
	return e.svc.DeleteCycle(context.Background(), e.org, e.chen, id)
}

// contiguous: sorted by start, each cycle starts where the last ends, and the
// numbers rise with the dates.
func (e *optEnv) contiguous() []dbq.Cycle {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT * FROM cycles WHERE team_id = $1 ORDER BY starts_at`, e.team.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	cs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dbq.Cycle])
	if err != nil {
		e.t.Fatal(err)
	}
	for i, c := range cs[1:] {
		if !c.StartsAt.Equal(cs[i].EndsAt) {
			e.t.Fatalf("cycle %d starts %s, the one before ends %s", c.Number, c.StartsAt, cs[i].EndsAt)
		}
		if c.Number <= cs[i].Number {
			e.t.Fatalf("cycle numbers do not rise: %d after %d", c.Number, cs[i].Number)
		}
	}
	return cs
}

func (e *optEnv) get(id uuid.UUID) tracker.Cycle {
	e.t.Helper()
	c, err := e.svc.GetCycle(context.Background(), e.org, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *optEnv) eventsOf(typ string) int {
	return e.count(`SELECT count(*) FROM events WHERE type = $1`, typ)
}

func TestCycleOptions_NameNeedsNoDatesAndTakesNoEventOnNoop(t *testing.T) {
	e := newOptEnv(t, true)
	cs := e.cycles()
	n := "Sprint A"
	if _, err := e.svc.UpdateCycle(context.Background(), e.org, e.chen, cs[0].ID, tracker.UpdateCycleInput{Name: optString(n)}); err != nil {
		t.Fatal(err)
	}
	// A completed cycle can still be renamed.
	e.exec(`UPDATE cycles SET completed_at = ends_at WHERE id = $1`, cs[0].ID)
	if _, err := e.svc.UpdateCycle(context.Background(), e.org, e.chen, cs[0].ID, tracker.UpdateCycleInput{Description: optString("d")}); err != nil {
		t.Fatal(err)
	}
	before := e.eventsOf("cycle.updated")
	// Same dates (as days and as instants) change nothing and write no event.
	if err := e.redate(cs[1].ID, "2026-11-01", "2026-11-15"); err != nil {
		t.Fatal(err)
	}
	if err := e.redate(cs[1].ID, cs[1].StartsAt.Format(time.RFC3339), cs[1].EndsAt.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if got := e.eventsOf("cycle.updated"); got != before {
		t.Fatalf("a no-op wrote %d event(s)", got-before)
	}
}

func TestCycleOptions_EditEndMovesNextStart(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	if err := e.redate(c[0].ID, "", "2026-11-05"); err != nil {
		t.Fatal(err)
	}
	cs := e.contiguous()
	if want := at(e.loc, 2026, 11, 5, 0); !cs[0].EndsAt.Equal(want) || !cs[1].StartsAt.Equal(want) {
		t.Fatalf("cycle 1 ends %s, cycle 2 starts %s, want %s", cs[0].EndsAt, cs[1].StartsAt, want)
	}
	// Both cycles are in the feed with the actor; the cycle's own event names the change.
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.updated' AND actor_id = $1
		AND payload->'changes' = '["endsAt"]'::jsonb`, e.chen.UserID); n != 1 {
		t.Fatalf("%d own events", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.updated' AND entity_id = $1
		AND payload->'changes' = '["startsAt"]'::jsonb`, c[1].ID); n != 1 {
		t.Fatalf("%d neighbour events", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE seq > (SELECT max(seq) FROM events WHERE type = 'cycle.updated')`); n != 0 {
		t.Fatal("cycle.updated is not last")
	}
	// The job still works from the new chain: it leaves cycle 2 alone.
	if r := e.run(e.clock()); r.Created != 0 || r.Completed != 0 {
		t.Fatalf("job after edit did %+v", r)
	}
	e.contiguous()
}

func TestCycleOptions_EditStartOfUpcomingMovesPreviousEnd(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	if err := e.redate(c[1].ID, "2026-11-03", ""); err != nil {
		t.Fatal(err)
	}
	cs := e.contiguous()
	if want := at(e.loc, 2026, 11, 3, 0); !cs[0].EndsAt.Equal(want) || !cs[1].StartsAt.Equal(want) {
		t.Fatalf("cycle 1 ends %s, cycle 2 starts %s, want %s", cs[0].EndsAt, cs[1].StartsAt, want)
	}
}

func TestCycleOptions_DateRules(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles() // c[0] running, c[1] upcoming
	inst := func(tm time.Time) string { return tm.Format(time.RFC3339) }
	conflict, invalid := httpx.CodeCycleConflict, httpx.CodeValidationFailed
	cases := []struct {
		name       string
		id         uuid.UUID
		start, end string
		status     int
		code       httpx.Code
	}{
		{"start of a running cycle", c[0].ID, "2026-10-19", "", 409, conflict},
		{"end in the past", c[0].ID, "", "2026-10-20", 400, invalid},
		{"end now", c[0].ID, "", inst(e.clock()), 400, invalid},
		{"start in the past", c[1].ID, "2026-10-20", "", 400, invalid},
		{"end before start", c[1].ID, "2026-11-20", "2026-11-18", 400, invalid},
		{"end at the next cycle's end", c[0].ID, "", "2026-11-15", 409, conflict},
		{"end beyond the next cycle", c[0].ID, "", "2026-11-20", 409, conflict},
		{"garbage", c[1].ID, "tomorrow", "", 400, invalid},
	}
	for _, tc := range cases {
		err := e.redate(tc.id, tc.start, tc.end)
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		e.wantErr(err, tc.status, tc.code)
	}
	_, err := e.svc.UpdateCycle(context.Background(), e.org, e.chen, c[1].ID,
		tracker.UpdateCycleInput{StartsAt: httpx.Opt[string]{Set: true, Null: true}})
	e.wantErr(err, 400, invalid)
	got := e.cycles()
	if !got[0].EndsAt.Equal(c[0].EndsAt) || !got[1].StartsAt.Equal(c[1].StartsAt) {
		t.Fatal("a refused edit changed a date")
	}
}

// A neighbour must stay non-empty: the start of cycle 3 cannot move to or
// before the start of cycle 2.
func TestCycleOptions_NeighbourStaysNonEmpty(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	c3 := e.insertCycle(3, c[1].EndsAt, c[1].EndsAt.Add(14*24*time.Hour))
	e.wantErr(e.redate(c3, "2026-11-01", ""), 409, httpx.CodeCycleConflict)
	e.wantErr(e.redate(c3, "2026-10-30", ""), 409, httpx.CodeCycleConflict)
	if err := e.redate(c3, "2026-11-10", ""); err != nil {
		t.Fatal(err)
	}
	cs := e.contiguous()
	if want := at(e.loc, 2026, 11, 10, 0); !cs[1].EndsAt.Equal(want) {
		t.Fatalf("cycle 2 ends %s, want %s", cs[1].EndsAt, want)
	}
}

// The new range must not overlap a cycle that is not a touching neighbour
// (a chain with a gap, as an import can leave).
func TestCycleOptions_RefusesOverlapWithAnotherCycle(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	e.insertCycle(3, c[1].EndsAt.Add(7*24*time.Hour), c[1].EndsAt.Add(21*24*time.Hour)) // a gap of a week
	e.wantErr(e.redate(c[1].ID, "", "2026-11-25"), 409, httpx.CodeCycleConflict)
	if err := e.redate(c[1].ID, "", "2026-11-20"); err != nil { // fills the gap, no overlap
		t.Fatal(err)
	}
}

func TestCycleOptions_PastCyclesAreHistory(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	// Ended, job not run yet: the write catches the chain up under the lock, so
	// it sees a completed cycle; the refusal rolls the catch-up back.
	e.setNow(at(e.loc, 2026, 11, 2, 9))
	e.wantErr(e.redate(c[0].ID, "", "2026-11-05"), 409, httpx.CodeCycleConflict)
	if e.cycles()[0].CompletedAt != nil {
		t.Fatal("a refused write kept its catch-up")
	}
	e.run(e.clock())
	e.wantErr(e.redate(c[0].ID, "", "2026-11-05"), 409, httpx.CodeCycleConflict)
}

// Mark complete: the job's completion run now, for the current cycle.
func TestCycleOptions_Complete(t *testing.T) {
	for _, rollover := range []bool{true, false} {
		e := newOptEnv(t, rollover)
		c := e.cycles()
		open := e.issue("open", "Todo", c[0].ID, 3)
		done := e.issue("done", "Done", c[0].ID, 2)
		now := at(e.loc, 2026, 10, 23, 15)
		e.setNow(now)

		got, err := e.svc.CompleteCycle(context.Background(), e.org, e.chen, c[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.CompletedAt == nil || !got.CompletedAt.Equal(now) || !got.EndsAt.Equal(now) {
			t.Fatalf("rollover=%v: completedAt %v endsAt %s, want %s", rollover, got.CompletedAt, got.EndsAt, now)
		}
		cs := e.contiguous()
		if len(cs) != 3 || !cs[1].StartsAt.Equal(now) || !cs[2].StartsAt.After(now) {
			t.Fatalf("chain after complete: %+v", cs)
		}
		wantOpen := c[0].ID
		if rollover {
			wantOpen = c[1].ID
		}
		if g := e.issueCycle(open.ID); g == nil || *g != wantOpen {
			t.Fatalf("rollover=%v: open issue in %v, want %s", rollover, g, wantOpen)
		}
		if g := e.issueCycle(done.ID); g == nil || *g != c[0].ID {
			t.Fatalf("done issue moved to %v", g)
		}
		// The final snapshot counts the cycle as it ended, dated the local day of now.
		if n := e.count(`SELECT count(*) FROM cycle_snapshots WHERE cycle_id = $1 AND snapshot_date = '2026-10-23'
			AND scope_count = 2 AND completed_count = 1`, c[0].ID); n != 1 {
			t.Fatalf("rollover=%v: the final snapshot must hold the cycle as it ended (2 in scope, 1 done): %d", rollover, n)
		}
		moves := 0
		if rollover {
			moves = 1
		}
		if n := e.count(`SELECT count(*) FROM events WHERE type = 'issue.updated' AND actor_id = $1
			AND payload->>'reason' = 'rollover'`, e.chen.UserID); n != moves {
			t.Fatalf("%d rollover events with the actor, want %d", n, moves)
		}
		if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.completed' AND entity_id = $1
			AND actor_id = $2 AND payload->>'reason' = 'manual'`, c[0].ID, e.chen.UserID); n != 1 {
			t.Fatalf("%d cycle.completed events", n)
		}
		// Our events come last, before only the cycle.created and cycle.updated of the same transaction.
		if n := e.count(`SELECT count(*) FROM events WHERE type NOT IN ('cycle.created', 'cycle.updated') AND seq > (SELECT max(seq) FROM events WHERE type = 'cycle.completed')`); n != 0 {
			t.Fatal("cycle.completed is not the last event")
		}
		// The job takes over from here: nothing to redo, and the chain keeps going.
		if r := e.run(now); r.Created != 0 || r.Completed != 0 || r.Moved != 0 {
			t.Fatalf("job right after complete did %+v", r)
		}
		e.run(at(e.loc, 2026, 11, 16, 9))
		for _, c := range e.contiguous()[:2] {
			if c.CompletedAt == nil {
				t.Fatalf("cycle %d not completed by the job", c.Number)
			}
		}
	}
}

func TestCycleOptions_CompleteRules(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	conflict := httpx.CodeCycleConflict
	e.wantErr(e.complete(c[1].ID), 409, conflict) // upcoming
	e.setNow(at(e.loc, 2026, 10, 22, 9))
	if err := e.complete(c[0].ID); err != nil {
		t.Fatal(err)
	}
	e.wantErr(e.complete(c[0].ID), 409, conflict) // already complete
	// Ended and not yet completed by the job: the write catches up first, so
	// the cycle is complete and the answer is the same 409.
	e.setNow(at(e.loc, 2026, 11, 20, 9))
	e.wantErr(e.complete(c[1].ID), 409, conflict)

	// Cycles off: no completion (a rollover needs the next cycle, which only the job makes).
	e2 := newOptEnv(t, true)
	c2 := e2.cycles()
	e2.exec(`UPDATE teams SET cycles_enabled = false WHERE id = $1`, e2.team.ID)
	e2.wantErr(e2.complete(c2[0].ID), 409, conflict)
	if e2.cycles()[0].CompletedAt != nil {
		t.Fatal("completed with cycles off")
	}
}

func TestCycleOptions_Delete(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	c3 := e.insertCycle(3, c[1].EndsAt, c[1].EndsAt.Add(14*24*time.Hour))
	conflict := httpx.CodeCycleConflict

	// An issue (even an archived one) blocks the delete; nothing changes.
	is := e.issue("in two", "Todo", c[1].ID, 1)
	e.exec(`UPDATE issues SET archived_at = now() WHERE id = $1`, is.ID)
	e.wantErr(e.del(c[1].ID), 409, httpx.CodeCycleInUse)
	if len(e.cycles()) != 3 {
		t.Fatal("a refused delete removed a cycle")
	}
	e.exec(`UPDATE issues SET cycle_id = NULL WHERE id = $1`, is.ID)

	// The next cycle takes over the time of the deleted one.
	if err := e.del(c[1].ID); err != nil {
		t.Fatal(err)
	}
	cs := e.contiguous()
	if len(cs) != 2 || cs[1].ID != c3 || !cs[1].StartsAt.Equal(c[1].StartsAt) {
		t.Fatalf("after delete: %+v", cs)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'cycle.deleted' AND actor_id = $1 AND entity_id = $2`, e.chen.UserID, c[1].ID); n != 1 {
		t.Fatalf("%d cycle.deleted events", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type NOT IN ('cycle.created', 'cycle.updated') AND seq > (SELECT max(seq) FROM events WHERE type = 'cycle.deleted')`); n != 0 {
		t.Fatal("cycle.deleted is not the last event")
	}
	// The job does not make the deleted cycle again (it keeps the chain).
	e.run(e.clock())
	if len(e.cycles()) != 2 {
		t.Fatal("the job made a cycle after the delete")
	}
	// The last cycle cannot go while cycles are on: the job would make it again.
	e.wantErr(e.del(c3), 409, conflict)
	// A completed cycle is history.
	e.exec(`UPDATE cycles SET completed_at = ends_at WHERE id = $1`, c[0].ID)
	e.wantErr(e.del(c[0].ID), 409, conflict)
	// With cycles off the last cycle may go.
	e.exec(`UPDATE teams SET cycles_enabled = false WHERE id = $1`, e.team.ID)
	if err := e.del(c3); err != nil {
		t.Fatal(err)
	}
}

// Deleting the running cycle makes the next one current and the job's
// maintenance in the same transaction makes a new upcoming cycle.
func TestCycleOptions_DeleteCurrentKeepsAnUpcomingCycle(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	if err := e.del(c[0].ID); err != nil {
		t.Fatal(err)
	}
	cs := e.contiguous()
	if len(cs) != 2 || cs[0].ID != c[1].ID || !cs[0].StartsAt.Equal(c[0].StartsAt) {
		t.Fatalf("after delete: %+v", cs)
	}
	if cs[0].StartsAt.After(e.clock()) || !cs[1].StartsAt.After(e.clock()) {
		t.Fatalf("want a current and an upcoming cycle: %+v", cs)
	}
	if cs[1].Number != 3 {
		t.Fatalf("new cycle is number %d, want 3 (max + 1)", cs[1].Number)
	}
}

// Every manual write waits for the team cycle lock. Deterministic: a
// transaction that holds the lock blocks the write until it commits.
// Mutant: remove lockTeamCycles from lockCycleTx and every row fails.
func TestCycleOptions_WritesWaitForTheCycleLock(t *testing.T) {
	ops := map[string]func(e *optEnv, c []dbq.Cycle) error{
		"name": func(e *optEnv, c []dbq.Cycle) error {
			_, err := e.svc.UpdateCycle(context.Background(), e.org, e.chen, c[0].ID, tracker.UpdateCycleInput{Name: optString("x")})
			return err
		},
		"dates":    func(e *optEnv, c []dbq.Cycle) error { return e.redate(c[0].ID, "", "2026-11-05") },
		"complete": func(e *optEnv, c []dbq.Cycle) error { return e.complete(c[0].ID) },
		"delete":   func(e *optEnv, c []dbq.Cycle) error { return e.del(c[1].ID) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			e := newOptEnv(t, true)
			c := e.cycles()
			e.insertCycle(3, c[1].EndsAt, c[1].EndsAt.Add(14*24*time.Hour)) // so cycle 2 is not the last
			tx, err := e.pool.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(context.Background(),
				`SELECT pg_advisory_xact_lock(hashtextextended('devops:team-cycles:' || $1::text, 0))`, e.team.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- op(e, c) }()
			select {
			case err := <-done:
				t.Fatalf("%s did not wait for the cycle lock (err %v)", name, err)
			case <-time.After(400 * time.Millisecond):
			}
			if err := tx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("blocked after the lock was released")
			}
		})
	}
}

// Race: the job and Mark complete at the same instant, after a boundary the
// job has not handled yet. Either order must end in the same chain.
func TestCycleOptions_RaceWithTheJob(t *testing.T) {
	e := newOptEnv(t, true)
	const rounds = 25
	for i := 0; i < rounds; i++ {
		e.exec(`DELETE FROM cycles WHERE team_id = $1`, e.team.ID)
		e.setNow(at(e.loc, 2026, 11, 7, 9))
		e.run(e.clock()) // cycles 1 (11-01, current) and 2 (11-15, upcoming)
		cs := e.cycles()
		now := at(e.loc, 2026, 11, 15, 0).Add(30 * time.Minute) // cycle 2 has just started, the job has not seen it
		e.setNow(now)

		var wg sync.WaitGroup
		var jobErr, opErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 1 { // both start orders
				time.Sleep(time.Duration(i%5) * time.Millisecond)
			}
			_, jobErr = e.svc.MaintainCycles(context.Background(), now)
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				time.Sleep(time.Duration(i%5) * time.Millisecond)
			}
			opErr = e.complete(cs[1].ID)
		}()
		close(start)
		wg.Wait()
		if jobErr != nil || opErr != nil {
			t.Fatalf("round %d: job %v, complete %v", i, jobErr, opErr)
		}
		got := e.contiguous()
		if len(got) != 4 {
			t.Fatalf("round %d: %d cycles", i, len(got))
		}
		for j, c := range got {
			if int(c.Number) != j+1 {
				t.Fatalf("round %d: cycle %d has number %d", i, j, c.Number)
			}
		}
		if got[0].CompletedAt == nil || got[1].CompletedAt == nil || got[2].CompletedAt != nil || got[3].CompletedAt != nil {
			t.Fatalf("round %d: completion %+v", i, got)
		}
		if !got[1].EndsAt.Equal(now) || !got[2].StartsAt.Equal(now) || !got[3].StartsAt.After(now) {
			t.Fatalf("round %d: dates %+v", i, got)
		}
	}
}

// The delete races an issue write that names the cycle: the issue lands (the
// delete is 409 cycle_in_use) or the delete wins and the issue write is a
// clean 400; never a 500 and never an issue detached by the foreign key.
// Mutant: delete the LockCycle call and a round fails.
func TestCycleOptions_DeleteRacesWithIssueAssign(t *testing.T) {
	e := newOptEnv(t, true)
	const rounds = 30
	for i := 0; i < rounds; i++ {
		e.exec(`DELETE FROM cycles WHERE team_id = $1 AND number > 2`, e.team.ID)
		c := e.cycles()
		c3 := e.insertCycle(3, c[1].EndsAt, c[1].EndsAt.Add(14*24*time.Hour))
		e.insertCycle(4, c[1].EndsAt.Add(14*24*time.Hour), c[1].EndsAt.Add(28*24*time.Hour)) // c3 is not the last cycle
		is := e.issue("racer", "Todo", c[0].ID, 1)
		var wg sync.WaitGroup
		var delErr, upErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; delErr = e.del(c3) }()
		go func() {
			defer wg.Done()
			<-start
			_, upErr = e.svc.UpdateIssue(context.Background(), e.org, e.chen, is.Identifier,
				tracker.UpdateIssueInput{CycleID: httpx.Opt[uuid.UUID]{Set: true, V: c3}})
		}()
		close(start)
		wg.Wait()
		var he *httpx.Error
		if upErr != nil && !(errors.As(upErr, &he) && he.Status == 400) {
			t.Fatalf("round %d: issue write: %v", i, upErr)
		}
		if delErr != nil && !(errors.As(delErr, &he) && he.Code == httpx.CodeCycleInUse) {
			t.Fatalf("round %d: delete: %v", i, delErr)
		}
		if (upErr == nil) == (delErr == nil) {
			t.Fatalf("round %d: issue write %v and delete %v: exactly one must win", i, upErr, delErr)
		}
		if delErr != nil {
			if g := e.issueCycle(is.ID); g == nil || *g != c3 {
				t.Fatalf("round %d: the issue lost its cycle", i)
			}
		}
		e.exec(`DELETE FROM issues WHERE id = $1`, is.ID)
	}
}

func (e *optEnv) insertCycle(n int, start, end time.Time) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO cycles (org_id, team_id, number, starts_at, ends_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		e.org.ID, e.team.ID, n, start, end).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// The write finds the chain exactly as the job would leave it, even when the
// job has not run since the boundary: Mark complete of the cycle that just
// became current works with no job run before it (the catch-up under the lock
// makes the next cycle). Mutant: no catch-up and this fails (no next cycle).
func TestCycleOptions_CompleteCatchesUpTheJobFirst(t *testing.T) {
	e := newOptEnv(t, true)
	c := e.cycles()
	open := e.issue("open", "Todo", c[0].ID, 1)
	now := at(e.loc, 2026, 11, 1, 0).Add(30 * time.Minute) // cycle 2 started, no job run since
	e.setNow(now)
	if err := e.complete(c[1].ID); err != nil {
		t.Fatal(err)
	}
	cs := e.contiguous()
	if len(cs) != 4 || cs[0].CompletedAt == nil || cs[1].CompletedAt == nil || !cs[1].EndsAt.Equal(now) || !cs[2].StartsAt.Equal(now) {
		t.Fatalf("chain %+v", cs)
	}
	// The job's own rollover of cycle 1 happened too: the open issue walked 1 -> 2 -> 3.
	if g := e.issueCycle(open.ID); g == nil || *g != cs[2].ID {
		t.Fatalf("open issue in %v, want cycle 3", g)
	}
}
