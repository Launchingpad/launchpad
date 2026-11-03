package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// The cycle job (docs/internal/decisions-T3.md "Cycles").
//
// For every team with cycles enabled, one transaction under a per-team
// advisory lock:
//
//  1. Create cycles until one starts after now. A team with no cycle gets
//     cycle 1 at the latest cadence boundary at or before now. Each next
//     cycle starts where the last one ends. So after a run there is always
//     an upcoming cycle, and a server that was off for weeks creates every
//     missed cycle, in order, once. One run creates at most the cap; a team
//     further behind commits what it made and continues on the next run.
//     When cycles are turned back ON after a long pause (the team PATCH),
//     the chain restarts at the current cycle instead (no back-dated empty
//     cycles).
//  2. Complete every ended cycle that has a successor (the earliest cycle
//     starting at or after its end), oldest first: write its final
//     snapshot, roll its open issues over to the successor when the team's
//     cycle_rollover is on, and set completed_at. An ended cycle with no
//     successor yet waits for a later run; it is never an error.
//  3. Upsert today's snapshot of the current cycle.
//  4. Emit the events, last.
//
// Two instances (or a boot run racing the hourly one) serialise on the
// advisory lock, and the second finds nothing to do. Numbers are
// max(number) + 1 under that lock, so new numbers are consecutive with no
// gap; the UNIQUE (team_id, number) constraint is the backstop.

// CycleJobInterval is how often the job runs after its boot run. The job is
// "daily" in what it achieves (one snapshot row per day, cycles created a
// whole cycle ahead); it runs hourly so a rollover happens within the hour
// after a boundary. Every run is idempotent.
const CycleJobInterval = time.Hour

// DefaultMaxCyclesPerRun bounds the catch-up loop of one team in one run.
// A team still behind commits what it made and continues on the next run.
const DefaultMaxCyclesPerRun = 1000

// cycleRunOpts are the knobs of one team's maintenance.
type cycleRunOpts struct {
	maxCreate int // the catch-up cap
	// restart: cycles were just turned back on. If more than one whole
	// cycle was missed, the chain restarts at the current cycle.
	restart bool
}

// CycleRun counts what one run did.
type CycleRun struct {
	Teams     int // teams looked at
	Created   int // cycles created
	Completed int // cycles completed
	Moved     int // issues rolled over
	Snapshots int // snapshot rows written
}

func (r *CycleRun) add(o CycleRun) {
	r.Teams += o.Teams
	r.Created += o.Created
	r.Completed += o.Completed
	r.Moved += o.Moved
	r.Snapshots += o.Snapshots
}

// RunCycleJob runs the job once now (the boot run) and then every interval
// until ctx ends. clock is the time source; tests inject a fake one. A
// failed run is logged and the loop goes on: the next run repeats the work.
func RunCycleJob(ctx context.Context, run func(context.Context, time.Time) (CycleRun, error),
	clock func() time.Time, interval time.Duration, log *slog.Logger) {
	once := func() {
		r, err := run(ctx, clock())
		if ctx.Err() != nil {
			return // shutting down; a cancelled run is not a failure
		}
		if err != nil {
			log.ErrorContext(ctx, "cycle job failed", "err", err)
		}
		if r.Created+r.Completed+r.Moved > 0 {
			log.InfoContext(ctx, "cycle job", "teams", r.Teams, "created", r.Created,
				"completed", r.Completed, "rolled_over", r.Moved, "snapshots", r.Snapshots)
		}
	}
	once()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			once()
		}
	}
}

// MaintainCycles is one run of the cycle job at the instant now. Each team
// is its own transaction, so one broken team (a bad zone) does not stop
// the others; their errors are joined.
func (s *Service) MaintainCycles(ctx context.Context, now time.Time) (CycleRun, error) {
	teams, err := s.q.ListCycleTeams(ctx)
	if err != nil {
		return CycleRun{}, fmt.Errorf("list cycle teams: %w", err)
	}
	var run CycleRun
	var errs []error
	for _, t := range teams {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		var r CycleRun
		err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			q := s.q.WithTx(tx)
			var evs []events.Event
			var err error
			if r, evs, err = maintainTeamCyclesTx(ctx, tx, q, t.OrgID, t.ID, now, s.cycleOpts(false)); err != nil {
				return err
			}
			return emitAll(ctx, q, t.OrgID, events.ScopeTeams(t.ID), evs)
		})
		if errors.Is(err, errTeamGone) {
			// Deleted after ListCycleTeams (LAUN-191); only the team lookup
			// says so, any other no-rows is a real error.
			slog.Debug("cycle job: team deleted since the list; skipped", "team_id", t.ID)
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("team %s: %w", t.ID, err))
			continue
		}
		run.add(r)
	}
	return run, errors.Join(errs...)
}

// emitAll emits the events of one team's cycle work (the cycles, their
// issues, the team) with that team's scope (T20 #54).
func emitAll(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, scope events.Scope, evs []events.Event) error {
	for _, ev := range evs {
		if _, err := events.Emit(ctx, q, orgID, ev, scope); err != nil {
			return err
		}
	}
	return nil
}

// lockTeamCycles takes the team's cycle advisory lock until the end of the
// transaction. Every writer of a team's cycles takes it first, before any
// row lock (the team PATCH too), so the lock order is always the same.
func lockTeamCycles(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:team-cycles:' || $1::text, 0))`, teamID); err != nil {
		return fmt.Errorf("team cycle lock: %w", err)
	}
	return nil
}

// errTeamGone: the team vanished between ListCycleTeams and the per-team transaction.
var errTeamGone = errors.New("team deleted")

// maintainTeamCyclesTx does steps 1-3 for one team inside tx and returns the
// events for the caller to emit as the last writes of the transaction.
func maintainTeamCyclesTx(ctx context.Context, tx pgx.Tx, q *dbq.Queries, orgID, teamID uuid.UUID, now time.Time,
	opts cycleRunOpts) (CycleRun, []events.Event, error) {
	run := CycleRun{Teams: 1}
	if err := lockTeamCycles(ctx, tx, teamID); err != nil {
		return run, nil, err
	}
	// Read the team AFTER the lock, so a run sees the settings the last
	// committed PATCH wrote.
	team, err := q.GetTeamByIDSystem(ctx, dbq.GetTeamByIDSystemParams{OrgID: orgID, ID: teamID})
	if errors.Is(err, pgx.ErrNoRows) {
		return run, nil, errTeamGone
	}
	if err != nil {
		return run, nil, fmt.Errorf("team: %w", err)
	}
	if !team.CyclesEnabled || team.ArchivedAt != nil {
		return run, nil, nil
	}
	cad, err := cadenceOf(team)
	if err != nil {
		return run, nil, err
	}
	var evs []events.Event
	insert := func(number int32, start time.Time) (dbq.Cycle, error) {
		c, err := q.InsertCycle(ctx, dbq.InsertCycleParams{OrgID: orgID, TeamID: teamID, Number: number,
			StartsAt: start, EndsAt: cad.End(start)})
		if err != nil {
			return c, fmt.Errorf("insert cycle %d: %w", number, err)
		}
		run.Created++
		evs = append(evs, events.Event{Type: events.CycleCreated, EntityType: "cycle", EntityID: c.ID,
			Payload: map[string]any{"id": c.ID, "teamId": c.TeamID, "number": c.Number, "startsAt": c.StartsAt, "endsAt": c.EndsAt}})
		return c, nil
	}

	// 1. Create.
	last, err := q.LastCycle(ctx, dbq.LastCycleParams{OrgID: orgID, TeamID: teamID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if last, err = insert(1, cad.FirstStart(now)); err != nil {
			return run, nil, err
		}
	case err != nil:
		return run, nil, fmt.Errorf("last cycle: %w", err)
	case opts.restart && !cad.End(last.EndsAt).After(now):
		// Turned back on after more than one whole missed cycle: start
		// the chain again at the current cycle (realigned to the cadence),
		// so no back-dated empty cycles are made. Open issues walk only the
		// cycles that already existed, then land in the current one.
		start := cad.FirstStart(now)
		if start.Before(last.EndsAt) {
			start = last.EndsAt
		}
		if last, err = insert(last.Number+1, start); err != nil {
			return run, nil, err
		}
	}
	for !last.StartsAt.After(now) && run.Created < opts.maxCreate {
		next, err := insert(last.Number+1, last.EndsAt)
		if err != nil {
			return run, nil, err
		}
		last = next
	}

	// 2. Complete what has ended and has a successor, oldest first, so a
	// catch-up rolls issues along the chain: cycle N's open issues reach
	// N+1 before N+1 is done.
	ended, err := q.ListEndedOpenCycles(ctx, dbq.ListEndedOpenCyclesParams{OrgID: orgID, TeamID: teamID, Now: now})
	if err != nil {
		return run, nil, fmt.Errorf("ended cycles: %w", err)
	}
	for _, c := range ended {
		next, err := q.NextCycleAfter(ctx, dbq.NextCycleAfterParams{OrgID: orgID, TeamID: teamID, After: c.EndsAt})
		if errors.Is(err, pgx.ErrNoRows) {
			break // no successor yet (the cap); a later run completes it
		}
		if err != nil {
			return run, nil, fmt.Errorf("cycle after %d: %w", c.Number, err)
		}
		if err := completeCycleTx(ctx, q, orgID, team, c, next, cad.LastDay(c.EndsAt), c.EndsAt, nil, &run, &evs); err != nil {
			return run, nil, err
		}
	}

	// 3. Today's snapshot of the current cycle.
	cur, err := q.CurrentCycle(ctx, dbq.CurrentCycleParams{OrgID: orgID, TeamID: teamID, Now: now})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return run, nil, fmt.Errorf("current cycle: %w", err)
	default:
		if err := upsertSnapshot(ctx, q, orgID, cur.ID, cad.LocalDate(now), &run); err != nil {
			return run, nil, err
		}
	}
	return run, evs, nil
}

// completeCycleTx completes one cycle c whose successor is next: the final
// snapshot (dated finalDay, written BEFORE the rollover empties the cycle),
// the rollover of its open issues to next when the team's cycle_rollover is
// on, then completed_at = completedAt. The cycle job calls it for an ended
// cycle (actor nil); Mark complete calls it for the running one (actor set,
// decisions-T27 #5). Events are appended to evs for the caller to emit last.
func completeCycleTx(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, team dbq.Team, c, next dbq.Cycle,
	finalDay, completedAt time.Time, actor *uuid.UUID, run *CycleRun, evs *[]events.Event) error {
	if err := upsertSnapshot(ctx, q, orgID, c.ID, finalDay, run); err != nil {
		return err
	}
	var moved []dbq.RolloverIssuesRow
	var nextID *uuid.UUID
	var err error
	if team.CycleRollover {
		nextID = &next.ID
		if moved, err = q.RolloverIssues(ctx, dbq.RolloverIssuesParams{OrgID: orgID, FromCycleID: &c.ID, ToCycleID: &next.ID}); err != nil {
			return fmt.Errorf("roll over cycle %d: %w", c.Number, err)
		}
		if len(moved) > 0 {
			from, err := snapCycle(ctx, q, orgID, &c.ID)
			if err != nil {
				return err
			}
			to, err := snapCycle(ctx, q, orgID, &next.ID)
			if err != nil {
				return err
			}
			source := "system"
			if actor != nil {
				source = "api"
			}
			for _, m := range moved {
				if err := insertActivity(ctx, q, orgID, m.ID, actor, source, activityRow{ActCycle, from, to}); err != nil {
					return err
				}
			}
		}
		for _, m := range moved {
			*evs = append(*evs, events.Event{Type: events.IssueUpdated, EntityType: "issue", EntityID: m.ID, ActorID: actor,
				Payload: map[string]any{"id": m.ID, "identifier": Identifier(team.Key, m.Number),
					"changes": []string{"cycleId"}, "version": m.Version,
					"fromCycleId": c.ID, "toCycleId": next.ID, "reason": "rollover",
					"teamId": team.ID}})
		}
	}
	if err := q.CompleteCycle(ctx, dbq.CompleteCycleParams{OrgID: orgID, ID: c.ID, CompletedAt: &completedAt}); err != nil {
		return fmt.Errorf("complete cycle %d: %w", c.Number, err)
	}
	run.Completed++
	run.Moved += len(moved)
	payload := map[string]any{"id": c.ID, "teamId": team.ID, "number": c.Number,
		"rolledOverTo": nextID, "rolledOver": len(moved)}
	if actor != nil {
		payload["reason"] = "manual"
	}
	*evs = append(*evs, events.Event{Type: events.CycleCompleted, EntityType: "cycle", EntityID: c.ID, ActorID: actor, Payload: payload})
	return nil
}

// upsertSnapshot writes (or rewrites) one day of a cycle and counts it in
// run. No row comes back when the counts did not change, or for a completed
// cycle (the query refuses it); neither is an error.
func upsertSnapshot(ctx context.Context, q *dbq.Queries, orgID, cycleID uuid.UUID, day time.Time, run *CycleRun) error {
	_, err := q.UpsertCycleSnapshot(ctx, dbq.UpsertCycleSnapshotParams{OrgID: orgID, CycleID: cycleID, SnapshotDate: day})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("snapshot: %w", err)
	}
	if run != nil {
		run.Snapshots++
	}
	return nil
}

// --- the cycle API -----------------------------------------------------------

const cycleSelect = `/* scope: team */
SELECT c.id, c.team_id, t.key, t.name, c.number, c.name, c.description, c.starts_at, c.ends_at,
       c.completed_at, c.created_at, c.updated_at, c.archived_at
FROM cycles c
JOIN teams t ON t.org_id = c.org_id AND t.id = c.team_id
`

func scanCycle(row pgx.Row) (Cycle, error) {
	var c Cycle
	err := row.Scan(&c.ID, &c.Team.ID, &c.Team.Key, &c.Team.Name, &c.Number, &c.Name, &c.Description,
		&c.StartsAt, &c.EndsAt, &c.CompletedAt, &c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt)
	return c, err
}

// CycleFilter is a parsed GET /cycles query.
type CycleFilter struct {
	listQuery
	Team   string
	Active *bool
}

// ParseCycleFilter validates the GET /cycles query string.
func ParseCycleFilter(v url.Values) (CycleFilter, error) {
	lq, err := parseListQuery(v, "cycles", "team", "active")
	if err != nil {
		return CycleFilter{}, err
	}
	f := CycleFilter{listQuery: lq, Team: strings.TrimSpace(v.Get("team"))}
	if a := v.Get("active"); a != "" {
		b, err := strconv.ParseBool(a)
		if err != nil {
			return f, httpx.Invalid("active must be true or false")
		}
		f.Active = &b
	}
	return f, nil
}

// activeCycleSQL is "the cycle is running now", the same test as the issue
// list's cycle=active.
const activeCycleSQL = "(c.starts_at <= now() AND c.ends_at > now() AND c.completed_at IS NULL)"

// ListCycles returns one keyset page, newest start first.
func (s *Service) ListCycles(ctx context.Context, org *auth.Org, f CycleFilter) (httpx.Page[Cycle], error) {
	args := []any{org.ID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"c.org_id = $1", teamScope(org, "c.team_id", arg)}
	if !f.IncludeArchived {
		where = append(where, "c.archived_at IS NULL")
	}
	if f.Team != "" {
		if id, err := uuid.Parse(f.Team); err == nil {
			if id, err = teamFilterID(ctx, s.q, org, id); err != nil {
				return httpx.Page[Cycle]{}, err
			}
			where = append(where, "c.team_id = "+arg(id))
		} else {
			where = append(where, "t.key = "+arg(strings.ToUpper(f.Team)))
		}
	}
	if f.Active != nil {
		if *f.Active {
			where = append(where, activeCycleSQL)
		} else {
			where = append(where, "NOT "+activeCycleSQL)
		}
	}
	if f.Cursor != nil {
		where = append(where, fmt.Sprintf("(c.starts_at, c.id) < (%s, %s)", arg(f.Cursor.At), arg(f.Cursor.ID)))
	}
	sql := cycleSelect + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY c.starts_at DESC, c.id DESC LIMIT " + arg(f.Limit+1)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.Page[Cycle]{}, fmt.Errorf("list cycles: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Cycle, error) { return scanCycle(r) })
	if err != nil {
		return httpx.Page[Cycle]{}, fmt.Errorf("scan cycles: %w", err)
	}
	return pageOf(items, f.Limit, "cycles", func(c Cycle) (time.Time, uuid.UUID) { return c.StartsAt, c.ID }), nil
}

// pageOf cuts a limit+1 result to limit and makes the next cursor from the
// last item kept.
func pageOf[T any](items []T, limit int, kind string, key func(T) (time.Time, uuid.UUID)) httpx.Page[T] {
	page := httpx.Page[T]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		at, id := key(page.Items[limit-1])
		c := encodeCursor(cursor{OrderBy: kind, At: at, ID: id})
		page.NextCursor = &c
	}
	return page
}

// GetCycle reads one cycle.
func (s *Service) GetCycle(ctx context.Context, org *auth.Org, id uuid.UUID) (Cycle, error) {
	id, err := s.cyclePathID(ctx, org, id)
	if err != nil {
		return Cycle{}, err
	}
	args := []any{org.ID, id}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	c, err := scanCycle(s.pool.QueryRow(ctx, cycleSelect+" WHERE c.org_id = $1 AND c.id = $2 AND "+teamScope(org, "c.team_id", arg), args...))
	return c, notFound(err, "cycle")
}

// CycleSnapshots lists a cycle's burn-up, oldest day first. While the cycle
// is running it first refreshes today's row (team-local date), so the last
// point is current. A completed or upcoming cycle is only read.
//
// This is a derived write behind a read-scoped GET: it recomputes counts
// from rows the caller can already read, changes no user data and emits no
// event.
func (s *Service) CycleSnapshots(ctx context.Context, org *auth.Org, id uuid.UUID) ([]CycleSnapshot, error) {
	scopeAll, scopeIDs := org.Access.Args()
	id, err := s.cyclePathID(ctx, org, id)
	if err != nil {
		return nil, err
	}
	c, err := s.q.GetCycle(ctx, dbq.GetCycleParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return nil, notFound(err, "cycle")
	}
	now := s.now()
	if c.CompletedAt == nil && !now.Before(c.StartsAt) && now.Before(c.EndsAt) {
		team, err := s.q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: c.TeamID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return nil, fmt.Errorf("cycle team: %w", err)
		}
		cad, err := cadenceOf(team)
		if err != nil {
			return nil, err
		}
		if err := upsertSnapshot(ctx, s.q, org.ID, c.ID, cad.LocalDate(now), nil); err != nil {
			return nil, err
		}
	}
	rows, err := s.q.ListCycleSnapshots(ctx, dbq.ListCycleSnapshotsParams{OrgID: org.ID, CycleID: id})
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	out := make([]CycleSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, CycleSnapshot{
			CycleID: r.CycleID, Date: r.SnapshotDate.Format(time.DateOnly),
			ScopeCount: r.ScopeCount, TriageCount: r.TriageCount, BacklogCount: r.BacklogCount,
			UnstartedCount: r.UnstartedCount, StartedCount: r.StartedCount, CompletedCount: r.CompletedCount,
			CanceledCount: r.CanceledCount, ScopePoints: r.ScopePoints, StartedPoints: r.StartedPoints,
			CompletedPoints: r.CompletedPoints, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// --- team settings -------------------------------------------------------------

// UpdateTeamInput is the PATCH /teams/{team} body: the name, the
// description and the cycle cadence. Absent fields are left alone; none of
// them can be null.
type UpdateTeamInput struct {
	Name              httpx.Opt[string] `json:"name"`
	Description       httpx.Opt[string] `json:"description"`
	CyclesEnabled     httpx.Opt[bool]   `json:"cyclesEnabled"`
	CycleLengthDays   httpx.Opt[int]    `json:"cycleLengthDays"`
	CycleStartWeekday httpx.Opt[int]    `json:"cycleStartWeekday"`
	CycleStartTime    httpx.Opt[string] `json:"cycleStartTime"`
	CycleTimezone     httpx.Opt[string] `json:"cycleTimezone"`
	CycleRollover     httpx.Opt[bool]   `json:"cycleRollover"`
}

func notNull[T any](o httpx.Opt[T], field string) error {
	if o.Set && o.Null {
		return httpx.Invalid("%s cannot be null", field)
	}
	return nil
}

// UpdateTeam changes a team's settings and emits team.updated. When cycles
// are enabled after the change, the cycle job runs for this team in the same
// transaction, so the current and the next cycle exist when the PATCH
// returns.
func (s *Service) UpdateTeam(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, in UpdateTeamInput) (Team, error) {
	scopeAll, scopeIDs := org.Access.Args()
	for _, err := range []error{
		notNull(in.Name, "name"), notNull(in.Description, "description"),
		notNull(in.CyclesEnabled, "cyclesEnabled"), notNull(in.CycleLengthDays, "cycleLengthDays"),
		notNull(in.CycleStartWeekday, "cycleStartWeekday"), notNull(in.CycleStartTime, "cycleStartTime"),
		notNull(in.CycleTimezone, "cycleTimezone"), notNull(in.CycleRollover, "cycleRollover"),
	} {
		if err != nil {
			return Team{}, err
		}
	}
	var teamID uuid.UUID
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		t, err := resolveTeam(ctx, q, org, ref)
		if err != nil {
			return notFound(err, "team")
		}
		teamID = t.ID
		// The cycle lock BEFORE the row lock: the job's order.
		if err := lockTeamCycles(ctx, tx, t.ID); err != nil {
			return err
		}
		cur, err := q.LockTeam(ctx, dbq.LockTeamParams{OrgID: org.ID, ID: t.ID})
		if err != nil {
			return notFound(err, "team")
		}
		next := dbq.UpdateTeamSettingsParams{OrgID: org.ID, ID: cur.ID, Name: cur.Name, Description: cur.Description,
			CyclesEnabled: cur.CyclesEnabled, CycleLengthDays: cur.CycleLengthDays, CycleStartWeekday: cur.CycleStartWeekday,
			CycleStartTime: cur.CycleStartTime, CycleTimezone: cur.CycleTimezone, CycleRollover: cur.CycleRollover}
		var changes []string
		if in.Name.Set {
			n := strings.TrimSpace(in.Name.V)
			if l := utf8.RuneCountInString(n); l < 1 || l > 200 {
				return httpx.Invalid("name must be 1-200 characters")
			}
			if n != cur.Name {
				next.Name, changes = n, append(changes, "name")
			}
		}
		if in.Description.Set && in.Description.V != cur.Description {
			if err := validateDescription(in.Description.V); err != nil {
				return err
			}
			next.Description, changes = in.Description.V, append(changes, "description")
		}
		if in.CyclesEnabled.Set && in.CyclesEnabled.V != cur.CyclesEnabled {
			// The web import's guard (decisions-T17 #43): turning cycles ON
			// while an import is under way makes a cycle 1 that clashes with
			// Linear's numbers. Turning them off is never blocked.
			if in.CyclesEnabled.V {
				blocked, err := q.ImportSessionBlocksCycles(ctx, org.ID)
				if err != nil {
					return fmt.Errorf("import session lookup: %w", err)
				}
				if blocked {
					return httpx.Errorf(http.StatusConflict, httpx.CodeImportInProgress,
						"an import is under way: finish or close it before turning cycles on (decisions-T3 #35)")
				}
			}
			next.CyclesEnabled, changes = in.CyclesEnabled.V, append(changes, "cyclesEnabled")
		}
		if in.CycleLengthDays.Set {
			n := in.CycleLengthDays.V
			if n < 1 || n > maxCycleLengthDays {
				return httpx.Invalid("cycleLengthDays must be 1-%d", maxCycleLengthDays)
			}
			if int32(n) != cur.CycleLengthDays {
				next.CycleLengthDays, changes = int32(n), append(changes, "cycleLengthDays")
			}
		}
		if in.CycleStartWeekday.Set {
			d := in.CycleStartWeekday.V
			if d < 0 || d > 6 {
				return httpx.Invalid("cycleStartWeekday must be 0 (Sunday) to 6 (Saturday)")
			}
			if int16(d) != cur.CycleStartWeekday {
				next.CycleStartWeekday, changes = int16(d), append(changes, "cycleStartWeekday")
			}
		}
		if in.CycleStartTime.Set {
			c, err := parseClock(in.CycleStartTime.V)
			if err != nil {
				return httpx.Invalid("%v", err)
			}
			if formatClock(c) != formatClock(cur.CycleStartTime) {
				next.CycleStartTime, changes = c, append(changes, "cycleStartTime")
			}
		}
		if in.CycleTimezone.Set {
			if _, err := LoadZone(in.CycleTimezone.V); err != nil {
				return httpx.Invalid("cycleTimezone: %v", err)
			}
			if in.CycleTimezone.V != cur.CycleTimezone {
				next.CycleTimezone, changes = in.CycleTimezone.V, append(changes, "cycleTimezone")
			}
		}
		if in.CycleRollover.Set && in.CycleRollover.V != cur.CycleRollover {
			next.CycleRollover, changes = in.CycleRollover.V, append(changes, "cycleRollover")
		}
		if len(changes) == 0 {
			return nil
		}
		updated, err := q.UpdateTeamSettings(ctx, next)
		if err != nil {
			return fmt.Errorf("update team: %w", err)
		}
		var evs []events.Event
		if updated.CyclesEnabled {
			restart := !cur.CyclesEnabled // turned back on: decisions-T3 #34
			if _, evs, err = maintainTeamCyclesTx(ctx, tx, q, org.ID, cur.ID, s.now(), s.cycleOpts(restart)); err != nil {
				return err
			}
		}
		evs = append(evs, events.Event{Type: events.TeamUpdated, EntityType: "team", EntityID: cur.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": cur.ID, "key": cur.Key, "changes": changes}})
		return emitAll(ctx, q, org.ID, events.ScopeTeams(cur.ID), evs)
	})
	if err != nil {
		return Team{}, err
	}
	t, err := s.q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: teamID, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return Team{}, fmt.Errorf("read team: %w", err)
	}
	out := []Team{toTeam(t)}
	if err := attachProductIDs(ctx, s.q, org.ID, out); err != nil {
		return Team{}, err
	}
	return out[0], nil
}

// cycleOpts are this service's maintenance knobs.
func (s *Service) cycleOpts(restart bool) cycleRunOpts {
	n := s.maxCycles
	if n <= 0 {
		n = DefaultMaxCyclesPerRun
	}
	return cycleRunOpts{maxCreate: n, restart: restart}
}

// SetMaxCyclesPerRun changes the catch-up cap (tests use a small one). Call
// it before the service is shared.
func (s *Service) SetMaxCyclesPerRun(n int) { s.maxCycles = n }

// SetClock replaces the service clock (tests inject a fake one). Call it
// before the service is shared. The cycle job takes its clock separately.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// DeleteTeam removes a team (LAUN-191, decisions-T26-delete.md). It is refused
// with 409 team_in_use while the team has any issue (archived ones too), when
// it is the org's only team, when it is the org's default team and while the
// team has chat services (LAUN-209). Everything
// else that hangs on the team (states, labels, cycles, follows, notification
// settings, product links) goes by foreign key; no issue is ever deleted.
func (s *Service) DeleteTeam(ctx context.Context, org *auth.Org, p *auth.Principal, ref string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// The webhook guard's lock first (decisions-T20 #65, entry 7): the
		// team's repositories lose their team with it (ON DELETE SET NULL).
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		t, err := resolveTeam(ctx, q, org, ref)
		if err != nil {
			return notFound(err, "team")
		}
		// The cycle lock BEFORE the row lock: the job's order.
		if err := lockTeamCycles(ctx, tx, t.ID); err != nil {
			return err
		}
		// Lock order (decisions-T26 #6): cycle lock, rank lock, org row (NO KEY
		// UPDATE), team row. Every issue create and move takes the rank lock
		// first, so none can run between the issue count and the delete.
		if err := rankLock(ctx, tx, t.ID); err != nil {
			return err
		}
		// The org row, NO KEY UPDATE: it serialises deletes in this org and
		// does not block the KEY SHARE that events.Emit needs on it.
		def, err := q.LockOrgDefaultTeam(ctx, org.ID)
		if err != nil {
			return fmt.Errorf("lock org: %w", err)
		}
		cur, err := q.LockTeamForDelete(ctx, dbq.LockTeamForDeleteParams{OrgID: org.ID, ID: t.ID})
		if err != nil {
			return notFound(err, "team")
		}
		inUse := func(format string, args ...any) error {
			return httpx.Errorf(http.StatusConflict, httpx.CodeTeamInUse, format, args...)
		}
		if def != nil && *def == cur.ID {
			return inUse("team %s is the default team of the org; choose another default team first", cur.Key)
		}
		teams, err := q.CountOrgTeams(ctx, org.ID)
		if err != nil {
			return fmt.Errorf("count teams: %w", err)
		}
		if teams <= 1 {
			return inUse("team %s is the only team of the org and cannot be deleted", cur.Key)
		}
		issues, err := q.CountTeamIssues(ctx, dbq.CountTeamIssuesParams{OrgID: org.ID, TeamID: cur.ID})
		if err != nil {
			return fmt.Errorf("count team issues: %w", err)
		}
		if issues > 0 {
			return inUse("team %s still has %d issue(s); a team that has issues cannot be deleted", cur.Key, issues)
		}
		// LAUN-209 (decisions-T38 #10): a team that has chat services keeps them.
		// The team row lock above conflicts with the KEY SHARE a service insert takes.
		svcs, err := q.CountTeamChatServices(ctx, dbq.CountTeamChatServicesParams{OrgID: org.ID, TeamID: cur.ID})
		if err != nil {
			return fmt.Errorf("count team chat services: %w", err)
		}
		if svcs > 0 {
			return inUse("team %s still has %d chat service(s); move or remove them first", cur.Key, svcs)
		}
		if err := q.DeleteTeam(ctx, dbq.DeleteTeamParams{OrgID: org.ID, ID: cur.ID}); err != nil {
			return fmt.Errorf("delete team: %w", err)
		}
		if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.TeamDeleted, EntityType: "team", EntityID: cur.ID,
			ActorID: &p.UserID, Payload: map[string]any{"id": cur.ID, "key": cur.Key}}, events.ScopeTeams(cur.ID))
		return err
	})
}
