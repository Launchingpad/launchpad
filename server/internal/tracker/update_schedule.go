package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// Project update schedule and reminders (LAUN-196 part B,
// docs/internal/decisions-T30-project-update-schedule.md).
//
// A project may have a schedule: weekly, biweekly or monthly, on a weekday, at
// an hour of the org's time zone. At each due time the lead is reminded when no
// project update was posted since the PREVIOUS due time. The job only emits an
// event; the notify consumer makes the notification from the change feed.

// The update frequencies.
const (
	FreqNone     = "none"
	FreqWeekly   = "weekly"
	FreqBiweekly = "biweekly"
	FreqMonthly  = "monthly"
)

var updateFrequencies = []string{FreqNone, FreqWeekly, FreqBiweekly, FreqMonthly}

// UpdateSchedule is a project's update schedule. Weekday is 0 (Sunday) to 6
// (Saturday), like time.Weekday. Hour is 0-23 in the org's time zone. With
// frequency none the weekday and hour are kept but unused.
type UpdateSchedule struct {
	Frequency string `json:"frequency"`
	Weekday   int    `json:"weekday"`
	Hour      int    `json:"hour"`
}

// UpdateScheduleInput is the PATCH body part: absent fields keep their value.
type UpdateScheduleInput struct {
	Frequency *string `json:"frequency"`
	Weekday   *int    `json:"weekday"`
	Hour      *int    `json:"hour"`
}

func (s UpdateSchedule) apply(in UpdateScheduleInput) (UpdateSchedule, error) {
	if in.Frequency != nil {
		if !slices.Contains(updateFrequencies, *in.Frequency) {
			return s, httpx.Invalid("updateSchedule.frequency must be none, weekly, biweekly or monthly")
		}
		s.Frequency = *in.Frequency
	}
	if in.Weekday != nil {
		if *in.Weekday < 0 || *in.Weekday > 6 {
			return s, httpx.Invalid("updateSchedule.weekday must be 0 (Sunday) to 6 (Saturday)")
		}
		s.Weekday = *in.Weekday
	}
	if in.Hour != nil {
		if *in.Hour < 0 || *in.Hour > 23 {
			return s, httpx.Invalid("updateSchedule.hour must be 0 to 23")
		}
		s.Hour = *in.Hour
	}
	return s, nil
}

// isDueDay says whether the local calendar day d carries a due time.
// Biweekly counts Monday-start weeks from 1970-01-05, so the same weeks are
// due for every project. Monthly is the first such weekday of the month.
func (s UpdateSchedule) isDueDay(d time.Time) bool {
	if int(d.Weekday()) != s.Weekday {
		return false
	}
	switch s.Frequency {
	case FreqWeekly:
		return true
	case FreqBiweekly:
		days := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
		return ((days+3)/7)%2 == 0
	case FreqMonthly:
		return d.Day() <= 7
	}
	return false
}

// dueAtOrBefore returns the latest due instant that is not after t. The hour
// is wall-clock time in loc, so a DST change moves the UTC instant and never
// the local hour. An hour that does not exist that day (spring forward) falls
// on the instant time.Date picks for it: one valid instant, the same on every
// call. ok is false for frequency none.
func (s UpdateSchedule) dueAtOrBefore(loc *time.Location, t time.Time) (due time.Time, ok bool) {
	if s.Frequency == FreqNone || !slices.Contains(updateFrequencies, s.Frequency) {
		return time.Time{}, false
	}
	day := t.In(loc)
	for i := 0; i < 70; i++ { // monthly: at most 62 days between two due days
		d := time.Date(day.Year(), day.Month(), day.Day()-i, 12, 0, 0, 0, loc)
		if !s.isDueDay(d) {
			continue
		}
		c := time.Date(d.Year(), d.Month(), d.Day(), s.Hour, 0, 0, 0, loc)
		if !c.After(t) {
			return c.UTC(), true
		}
	}
	return time.Time{}, false
}

// ReminderRun counts what one run of the update job did.
type ReminderRun struct {
	Projects int // scheduled projects looked at
	Due      int // projects whose due time had come since the last run
	Reminded int // update_due events emitted
}

// RunUpdateReminderJob runs the job once now (the boot run), then every
// interval until ctx ends. A failed run is logged; the next run repeats the work.
func RunUpdateReminderJob(ctx context.Context, run func(context.Context, time.Time) (ReminderRun, error),
	clock func() time.Time, interval time.Duration, log *slog.Logger) {
	once := func() {
		r, err := run(ctx, clock())
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.ErrorContext(ctx, "project update job failed", "err", err)
		}
		if r.Reminded > 0 {
			log.InfoContext(ctx, "project update job", "projects", r.Projects, "due", r.Due, "reminded", r.Reminded)
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

// RemindProjectUpdates is one run of the job at the instant now. Each project
// is its own transaction under its row lock, so two instances, a restart or a
// repeated run never remind twice for one due time: the due time is stored
// (update_last_due_at) in the same transaction as the event.
func (s *Service) RemindProjectUpdates(ctx context.Context, now time.Time) (ReminderRun, error) {
	list, err := s.q.ListUpdateScheduledProjects(ctx)
	if err != nil {
		return ReminderRun{}, fmt.Errorf("list scheduled projects: %w", err)
	}
	var run ReminderRun
	var errs []error
	for _, it := range list {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		due, reminded, err := s.remindProject(ctx, it.OrgID, it.ID, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("project %s: %w", it.ID, err))
			continue
		}
		run.Projects++
		if due {
			run.Due++
		}
		if reminded {
			run.Reminded++
		}
	}
	return run, errors.Join(errs...)
}

func (s *Service) remindProject(ctx context.Context, orgID, id uuid.UUID, now time.Time) (due, reminded bool, err error) {
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		due, reminded = false, false // a retried closure never double counts
		q := s.q.WithTx(tx)
		p, err := q.LockScheduledProject(ctx, dbq.LockScheduledProjectParams{OrgID: orgID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted since the list
		}
		if err != nil {
			return err
		}
		if p.ArchivedAt != nil || p.State == "completed" || p.State == "canceled" {
			return nil
		}
		sched := UpdateSchedule{Frequency: p.UpdateFrequency, Weekday: int(p.UpdateWeekday), Hour: int(p.UpdateHour)}
		loc, err := time.LoadLocation(p.TimeZone)
		if err != nil {
			return fmt.Errorf("org time zone %q: %w", p.TimeZone, err)
		}
		dueAt, ok := sched.dueAtOrBefore(loc, now)
		if !ok {
			return nil
		}
		if p.UpdateLastDueAt != nil && !p.UpdateLastDueAt.Before(dueAt) {
			return nil // this due time was already handled
		}
		due = true
		first := p.UpdateLastDueAt == nil
		if err := q.SetProjectUpdateLastDue(ctx, dbq.SetProjectUpdateLastDueParams{OrgID: orgID, ID: id, UpdateLastDueAt: &dueAt}); err != nil {
			return err
		}
		if first || p.LeadID == nil {
			return nil
		}
		human, err := q.LeadIsHuman(ctx, dbq.LeadIsHumanParams{OrgID: orgID, ID: *p.LeadID})
		if err != nil || !human {
			return err
		}
		prev, ok := sched.dueAtOrBefore(loc, dueAt.Add(-time.Nanosecond))
		if ok {
			posted, err := q.ProjectHasUpdateSince(ctx, dbq.ProjectHasUpdateSinceParams{OrgID: orgID, ProjectID: id, CreatedAt: prev})
			if err != nil || posted {
				return err
			}
		}
		reminded = true
		scope, err := projectEventScope(ctx, q, orgID, id)
		if err != nil {
			return err
		}
		// Ids and a time only. Last statement of the transaction.
		_, err = events.Emit(ctx, q, orgID, events.Event{Type: events.ProjectUpdateDue, EntityType: "project", EntityID: id,
			Payload: map[string]any{"projectId": id, "leadId": *p.LeadID, "dueAt": dueAt.UTC().Format(time.RFC3339)}}, scope)
		return err
	})
	return due, reminded, err
}

// scheduleStart is the due time stored when a schedule is set: the latest
// one that is already past, so the first reminder is the NEXT due time and
// setting a schedule never reminds at once for the past.
func (s *Service) scheduleStart(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, sched UpdateSchedule) (*time.Time, error) {
	if sched.Frequency == FreqNone {
		return nil, nil
	}
	tz, err := q.GetOrgTimeZone(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("org time zone: %w", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("org time zone %q: %w", tz, err)
	}
	due, ok := sched.dueAtOrBefore(loc, s.now())
	if !ok {
		return nil, nil
	}
	return &due, nil
}
