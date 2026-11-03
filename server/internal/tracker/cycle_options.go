package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Cycle options (LAUN-172, docs/internal/decisions-T27-cycle-options.md):
// the name, the dates, Mark complete and Delete of one cycle. The cycle job
// owns the chain of dates, so every write here takes the team cycle lock the
// job takes, FIRST, and then re-reads the cycle.

func cycleConflict(format string, args ...any) error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeCycleConflict, format, args...)
}

// UpdateCycleInput is the PATCH /cycles/{id} body. StartsAt and EndsAt are an
// RFC 3339 instant or a YYYY-MM-DD team-local day (decisions-T27 #4).
type UpdateCycleInput struct {
	Name        httpx.Opt[string] `json:"name"`
	Description httpx.Opt[string] `json:"description"`
	StartsAt    httpx.Opt[string] `json:"startsAt"`
	EndsAt      httpx.Opt[string] `json:"endsAt"`
}

// lockCycleTx resolves the cycle's team, takes the team cycle lock and reads
// the cycle again under it (so a job run that just committed is seen). With
// catchUp it also runs the job's maintenance for the team first, under the
// same lock, so a rule never judges a chain the job is about to change (an
// ended cycle not yet completed, a missing upcoming cycle). The job's events
// come back for the caller to emit, last.
func (s *Service) lockCycleTx(ctx context.Context, tx pgx.Tx, q *dbq.Queries, org *auth.Org, id uuid.UUID, catchUp bool) (dbq.GetCycleRow, []events.Event, error) {
	scopeAll, scopeIDs := org.Access.Args()
	pre, err := q.GetCycleTeamID(ctx, dbq.GetCycleTeamIDParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return dbq.GetCycleRow{}, nil, notFound(err, "cycle")
	}
	if err := lockTeamCycles(ctx, tx, pre); err != nil {
		return dbq.GetCycleRow{}, nil, err
	}
	var evs []events.Event
	if catchUp {
		if _, evs, err = maintainTeamCyclesTx(ctx, tx, q, org.ID, pre, s.now().UTC().Truncate(time.Microsecond), s.cycleOpts(false)); err != nil {
			return dbq.GetCycleRow{}, nil, err
		}
	}
	cur, err := q.GetCycle(ctx, dbq.GetCycleParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return dbq.GetCycleRow{}, nil, notFound(err, "cycle") // deleted while we waited for the lock
	}
	return cur, evs, nil
}

// UpdateCycle changes a cycle's name, description and dates and emits
// cycle.updated (and one for each neighbour whose edge moved).
func (s *Service) UpdateCycle(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in UpdateCycleInput) (Cycle, error) {
	id, err := s.cyclePathID(ctx, org, id)
	if err != nil {
		return Cycle{}, err
	}
	if err := notNull(in.StartsAt, "startsAt"); err != nil {
		return Cycle{}, err
	}
	if err := notNull(in.EndsAt, "endsAt"); err != nil {
		return Cycle{}, err
	}
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, evs, err := s.lockCycleTx(ctx, tx, q, org, id, in.StartsAt.Set || in.EndsAt.Set)
		if err != nil {
			return err
		}
		name, desc := cur.Name, cur.Description
		var changes []string
		if in.Name.Set {
			var n *string
			if !in.Name.Null {
				v := strings.TrimSpace(in.Name.V)
				if l := utf8.RuneCountInString(v); l < 1 || l > 255 {
					return httpx.Invalid("name must be 1-255 characters, or null")
				}
				n = &v
			}
			if !eqPtr(n, cur.Name) {
				name = n
				changes = append(changes, "name")
			}
		}
		if in.Description.Set {
			d := ""
			if !in.Description.Null {
				d = in.Description.V
			}
			if err := validateDescription(d); err != nil {
				return err
			}
			if d != cur.Description {
				desc = d
				changes = append(changes, "description")
			}
		}
		var devs []events.Event
		if in.StartsAt.Set || in.EndsAt.Set {
			var dateChanges []string
			if dateChanges, devs, err = s.redateCycleTx(ctx, q, org, p, cycleOfRow(cur), in); err != nil {
				return err
			}
			changes = append(changes, dateChanges...)
		}
		if len(changes) == 0 && len(evs) == 0 {
			return nil
		}
		if len(changes) > 0 {
			if _, err := q.UpdateCycleText(ctx, dbq.UpdateCycleTextParams{OrgID: org.ID, ID: id, Name: name, Description: desc}); err != nil {
				return fmt.Errorf("update cycle: %w", err)
			}
			evs = append(evs, events.Event{Type: events.CycleUpdated, EntityType: "cycle", EntityID: id,
				ActorID: &p.UserID, Payload: map[string]any{"id": id, "changes": changes}})
		}
		return emitAll(ctx, q, org.ID, events.ScopeTeams(cur.TeamID), append(evs, devs...))
	})
	if err != nil {
		return Cycle{}, err
	}
	return s.GetCycle(ctx, org, id)
}

// parseCycleBound reads a startsAt or endsAt value. A bare day is the team
// zone's day at the cadence start time. It returns nil when the value does
// not change the stored instant (a bare day equal to the stored local day is
// "no change", so an imported cycle keeps its own time of day).
func parseCycleBound(raw, field string, stored time.Time, cad func() (Cadence, error)) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if d, err := time.Parse(time.DateOnly, raw); err == nil {
		c, err := cad()
		if err != nil {
			return nil, err
		}
		if c.LocalDate(stored).Equal(d) {
			return nil, nil
		}
		t := c.at(d.Year(), d.Month(), d.Day())
		return &t, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, httpx.Invalid("%s must be a date (YYYY-MM-DD) or an RFC 3339 time", field)
	}
	t = t.UTC().Truncate(time.Microsecond)
	if t.Equal(stored) {
		return nil, nil
	}
	return &t, nil
}

// redateCycleTx applies decisions-T27 #4 inside the locked transaction and
// returns the change names for the cycle's own event and the events of the
// neighbours that moved.
func (s *Service) redateCycleTx(ctx context.Context, q *dbq.Queries, org *auth.Org, p *auth.Principal,
	cur dbq.Cycle, in UpdateCycleInput) ([]string, []events.Event, error) {
	scopeAll, scopeIDs := org.Access.Args()
	var cadOnce *Cadence
	cad := func() (Cadence, error) {
		if cadOnce != nil {
			return *cadOnce, nil
		}
		team, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: cur.TeamID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return Cadence{}, fmt.Errorf("cycle team: %w", err)
		}
		c, err := cadenceOf(team)
		if err != nil {
			return Cadence{}, err
		}
		cadOnce = &c
		return c, nil
	}
	var newStart, newEnd *time.Time
	var err error
	if in.StartsAt.Set {
		if newStart, err = parseCycleBound(in.StartsAt.V, "startsAt", cur.StartsAt, cad); err != nil {
			return nil, nil, err
		}
	}
	if in.EndsAt.Set {
		if newEnd, err = parseCycleBound(in.EndsAt.V, "endsAt", cur.EndsAt, cad); err != nil {
			return nil, nil, err
		}
	}
	if newStart == nil && newEnd == nil {
		return nil, nil, nil
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if cur.CompletedAt != nil || !cur.EndsAt.After(now) {
		return nil, nil, cycleConflict("cycle %d has ended; the dates of a past cycle cannot change", cur.Number)
	}
	if newStart != nil && !cur.StartsAt.After(now) {
		return nil, nil, cycleConflict("cycle %d has started; only its end date can change", cur.Number)
	}
	start, end := cur.StartsAt, cur.EndsAt
	if newStart != nil {
		if !newStart.After(now) {
			return nil, nil, httpx.Invalid("startsAt must be in the future")
		}
		start = *newStart
	}
	if newEnd != nil {
		if !newEnd.After(now) {
			return nil, nil, httpx.Invalid("endsAt must be in the future; to end the running cycle now, use Mark complete")
		}
		end = *newEnd
	}
	if !end.After(start) {
		return nil, nil, httpx.Invalid("endsAt must be after startsAt")
	}

	var prev, next *dbq.Cycle
	except := []uuid.UUID{cur.ID}
	if newStart != nil {
		pc, err := q.CycleEndingAt(ctx, dbq.CycleEndingAtParams{OrgID: org.ID, TeamID: cur.TeamID, At: cur.StartsAt, ExceptID: cur.ID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, nil, fmt.Errorf("previous cycle: %w", err)
		default:
			if pc.CompletedAt != nil {
				return nil, nil, cycleConflict("the previous cycle (%d) is complete; this cycle's start cannot move", pc.Number)
			}
			if !start.After(pc.StartsAt) {
				return nil, nil, cycleConflict("the new start would leave the previous cycle (%d) empty", pc.Number)
			}
			prev = &pc
			except = append(except, pc.ID)
		}
	}
	if newEnd != nil {
		nc, err := q.CycleStartingAt(ctx, dbq.CycleStartingAtParams{OrgID: org.ID, TeamID: cur.TeamID, At: cur.EndsAt, ExceptID: cur.ID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, nil, fmt.Errorf("next cycle: %w", err)
		default:
			if nc.CompletedAt != nil {
				return nil, nil, cycleConflict("the next cycle (%d) is complete; this cycle's end cannot move", nc.Number)
			}
			if !end.Before(nc.EndsAt) {
				return nil, nil, cycleConflict("the new end would leave the next cycle (%d) empty", nc.Number)
			}
			next = &nc
			except = append(except, nc.ID)
		}
	}
	taken, err := q.CycleRangeTaken(ctx, dbq.CycleRangeTakenParams{OrgID: org.ID, TeamID: cur.TeamID,
		ExceptIds: except, RangeStart: start, RangeEnd: end})
	if err != nil {
		return nil, nil, fmt.Errorf("overlap check: %w", err)
	}
	if taken {
		return nil, nil, cycleConflict("the new dates overlap another cycle of the team")
	}

	if _, err := q.SetCycleRange(ctx, dbq.SetCycleRangeParams{OrgID: org.ID, ID: cur.ID, StartsAt: start, EndsAt: end}); err != nil {
		return nil, nil, fmt.Errorf("set cycle dates: %w", err)
	}
	var changes []string
	var evs []events.Event
	moved := func(c *dbq.Cycle, from, to time.Time, what string) error {
		if _, err := q.SetCycleRange(ctx, dbq.SetCycleRangeParams{OrgID: org.ID, ID: c.ID, StartsAt: from, EndsAt: to}); err != nil {
			return fmt.Errorf("move %s of cycle %d: %w", what, c.Number, err)
		}
		evs = append(evs, events.Event{Type: events.CycleUpdated, EntityType: "cycle", EntityID: c.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": c.ID, "changes": []string{what}}})
		return nil
	}
	if newStart != nil {
		changes = append(changes, "startsAt")
		if prev != nil {
			if err := moved(prev, prev.StartsAt, start, "endsAt"); err != nil {
				return nil, nil, err
			}
		}
	}
	if newEnd != nil {
		changes = append(changes, "endsAt")
		if next != nil {
			if err := moved(next, end, next.EndsAt, "startsAt"); err != nil {
				return nil, nil, err
			}
		}
	}
	return changes, evs, nil
}

// CompleteCycle is "Mark complete" (decisions-T27 #5): the job's completion
// of one cycle, now, for the CURRENT cycle only.
func (s *Service) CompleteCycle(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) (Cycle, error) {
	scopeAll, scopeIDs := org.Access.Args()
	id, err := s.cyclePathID(ctx, org, id)
	if err != nil {
		return Cycle{}, err
	}
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		row, jobEvs, err := s.lockCycleTx(ctx, tx, q, org, id, true)
		if err != nil {
			return err
		}
		cur := cycleOfRow(row)
		team, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: cur.TeamID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return fmt.Errorf("cycle team: %w", err)
		}
		now := s.now().UTC().Truncate(time.Microsecond)
		switch {
		case cur.CompletedAt != nil:
			return cycleConflict("cycle %d is already complete", cur.Number)
		case !team.CyclesEnabled || team.ArchivedAt != nil:
			return cycleConflict("cycles are off for team %s; turn them on to complete a cycle", team.Key)
		case !cur.EndsAt.After(now):
			return cycleConflict("cycle %d has ended", cur.Number)
		case !cur.StartsAt.Before(now):
			return cycleConflict("cycle %d has not started; only the current cycle can be completed", cur.Number)
		}
		next, err := q.NextCycleAfter(ctx, dbq.NextCycleAfterParams{OrgID: org.ID, TeamID: cur.TeamID, After: cur.EndsAt})
		if errors.Is(err, pgx.ErrNoRows) {
			return cycleConflict("cycle %d has no next cycle", cur.Number)
		}
		if err != nil {
			return fmt.Errorf("next cycle: %w", err)
		}
		cad, err := cadenceOf(team)
		if err != nil {
			return err
		}
		// The cycle ends now and the next one starts now: the chain stays contiguous.
		if _, err := q.SetCycleRange(ctx, dbq.SetCycleRangeParams{OrgID: org.ID, ID: cur.ID, StartsAt: cur.StartsAt, EndsAt: now}); err != nil {
			return fmt.Errorf("end cycle: %w", err)
		}
		if _, err := q.SetCycleRange(ctx, dbq.SetCycleRangeParams{OrgID: org.ID, ID: next.ID, StartsAt: now, EndsAt: next.EndsAt}); err != nil {
			return fmt.Errorf("start next cycle: %w", err)
		}
		cur.EndsAt = now
		var run CycleRun
		evs := jobEvs
		if err := completeCycleTx(ctx, q, org.ID, team, cur, next, cad.LocalDate(now), now, &p.UserID, &run, &evs); err != nil {
			return err
		}
		evs = append(evs, events.Event{Type: events.CycleUpdated, EntityType: "cycle", EntityID: next.ID, ActorID: &p.UserID,
			Payload: map[string]any{"id": next.ID, "changes": []string{"startsAt"}}})
		// The next cycle is current now: make the upcoming one, and snapshot.
		_, mevs, err := maintainTeamCyclesTx(ctx, tx, q, org.ID, team.ID, now, s.cycleOpts(false))
		if err != nil {
			return err
		}
		return emitAll(ctx, q, org.ID, events.ScopeTeams(team.ID), append(evs, mevs...))
	})
	if err != nil {
		return Cycle{}, err
	}
	return s.GetCycle(ctx, org, id)
}

// DeleteCycle removes a cycle (decisions-T27 #6): not completed, no issues,
// and the next cycle absorbs its time so the chain stays contiguous.
func (s *Service) DeleteCycle(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) error {
	scopeAll, scopeIDs := org.Access.Args()
	id, err := s.cyclePathID(ctx, org, id)
	if err != nil {
		return err
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		row, jobEvs, err := s.lockCycleTx(ctx, tx, q, org, id, true)
		if err != nil {
			return err
		}
		cur := cycleOfRow(row)
		// The row lock, then the count: an issue write naming this cycle holds
		// KEY SHARE on the row, so the count sees it or the write waits.
		if _, err := q.LockCycle(ctx, dbq.LockCycleParams{OrgID: org.ID, ID: id}); err != nil {
			return notFound(err, "cycle")
		}
		team, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: cur.TeamID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return fmt.Errorf("cycle team: %w", err)
		}
		if cur.CompletedAt != nil {
			return cycleConflict("cycle %d is complete; a completed cycle is history and cannot be deleted", cur.Number)
		}
		n, err := q.CountCycleIssues(ctx, dbq.CountCycleIssuesParams{OrgID: org.ID, CycleID: &id})
		if err != nil {
			return fmt.Errorf("count cycle issues: %w", err)
		}
		if n > 0 {
			return httpx.Errorf(http.StatusConflict, httpx.CodeCycleInUse,
				"cycle %d still has %d issue(s), including archived issues (the cycle page hides them); move them to another cycle first", cur.Number, n)
		}
		next, err := q.NextCycleAfter(ctx, dbq.NextCycleAfterParams{OrgID: org.ID, TeamID: cur.TeamID, After: cur.EndsAt})
		hasNext := true
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			hasNext = false
		case err != nil:
			return fmt.Errorf("next cycle: %w", err)
		}
		evs := jobEvs
		if hasNext {
			if next.CompletedAt != nil {
				return cycleConflict("the next cycle (%d) is complete and cannot take over the dates", next.Number)
			}
			if _, err := q.SetCycleRange(ctx, dbq.SetCycleRangeParams{OrgID: org.ID, ID: next.ID, StartsAt: cur.StartsAt, EndsAt: next.EndsAt}); err != nil {
				return fmt.Errorf("extend next cycle: %w", err)
			}
		} else if team.CyclesEnabled && team.ArchivedAt == nil {
			return cycleConflict("cycle %d is the last cycle; the cycle job would make it again", cur.Number)
		}
		if err := q.DeleteCycle(ctx, dbq.DeleteCycleParams{OrgID: org.ID, ID: id}); err != nil {
			return fmt.Errorf("delete cycle: %w", err)
		}
		evs = append(evs, events.Event{Type: events.CycleDeleted, EntityType: "cycle", EntityID: id, ActorID: &p.UserID,
			Payload: map[string]any{"id": id, "teamId": cur.TeamID, "number": cur.Number}})
		if hasNext {
			evs = append(evs, events.Event{Type: events.CycleUpdated, EntityType: "cycle", EntityID: next.ID, ActorID: &p.UserID,
				Payload: map[string]any{"id": next.ID, "changes": []string{"startsAt"}}})
		}
		// The next cycle may be the current one now: keep an upcoming cycle.
		_, mevs, err := maintainTeamCyclesTx(ctx, tx, q, org.ID, team.ID, s.now().UTC().Truncate(time.Microsecond), s.cycleOpts(false))
		if err != nil {
			return err
		}
		return emitAll(ctx, q, org.ID, events.ScopeTeams(team.ID), append(evs, mevs...))
	})
}

func cycleOfRow(r dbq.GetCycleRow) dbq.Cycle {
	return dbq.Cycle{ID: r.ID, OrgID: r.OrgID, TeamID: r.TeamID, Number: r.Number, Name: r.Name, Description: r.Description,
		StartsAt: r.StartsAt, EndsAt: r.EndsAt, CompletedAt: r.CompletedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		ArchivedAt: r.ArchivedAt}
}
