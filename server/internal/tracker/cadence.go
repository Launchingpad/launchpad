package tracker

import (
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"devops/internal/dbq"
)

// Cadence is a team's cycle rhythm: cycles of LengthDays days that start on
// Weekday at Hour:Min, all read as wall-clock time in Loc.
//
// Every boundary is computed on the LOCAL calendar and converted to an
// instant only at the end, so a cycle that starts Sunday 00:00
// Europe/London starts at 00:00 local on both sides of a DST change (the
// cycle that spans the change is one hour longer or shorter). Adding
// 14*24h to an instant would drift by an hour instead.
type Cadence struct {
	LengthDays int
	Weekday    time.Weekday
	Hour, Min  int
	Loc        *time.Location
}

// maxCycleLengthDays is the teams_cycle_length_check bound.
const maxCycleLengthDays = 56

// LoadZone loads an IANA zone. "" and "Local" are refused: the server's
// own zone is not a team setting.
func LoadZone(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "local") {
		return nil, fmt.Errorf("timezone must be an IANA name such as Europe/London")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", name)
	}
	return loc, nil
}

// cadenceOf reads a team's cadence columns.
func cadenceOf(t dbq.Team) (Cadence, error) {
	loc, err := LoadZone(t.CycleTimezone)
	if err != nil {
		return Cadence{}, fmt.Errorf("team %s cadence: %w", t.Key, err)
	}
	if t.CycleLengthDays < 1 || t.CycleLengthDays > maxCycleLengthDays {
		return Cadence{}, fmt.Errorf("team %s cadence: length %d days", t.Key, t.CycleLengthDays)
	}
	h, m := clockOf(t.CycleStartTime)
	return Cadence{LengthDays: int(t.CycleLengthDays), Weekday: time.Weekday(t.CycleStartWeekday), Hour: h, Min: m, Loc: loc}, nil
}

// clockOf splits a time-of-day column into hour and minute (seconds are
// not part of a cadence). A NULL reads as 00:00.
func clockOf(t pgtype.Time) (int, int) {
	if !t.Valid {
		return 0, 0
	}
	mins := t.Microseconds / int64(time.Minute/time.Microsecond)
	return int(mins / 60 % 24), int(mins % 60)
}

// formatClock is the wire form of a time-of-day column, "HH:MM".
func formatClock(t pgtype.Time) string {
	h, m := clockOf(t)
	return fmt.Sprintf("%02d:%02d", h, m)
}

// parseClock reads "HH:MM" (24-hour) into a time-of-day column.
func parseClock(s string) (pgtype.Time, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return pgtype.Time{}, fmt.Errorf("cycleStartTime must be HH:MM (24-hour)")
	}
	us := int64(t.Hour())*int64(time.Hour/time.Microsecond) + int64(t.Minute())*int64(time.Minute/time.Microsecond)
	return pgtype.Time{Microseconds: us, Valid: true}, nil
}

// at is Hour:Min on the local date y-m-d. time.Date normalises a day out of
// range (d = 0, d = 32), so callers can add days to a date freely.
func (c Cadence) at(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, c.Hour, c.Min, 0, 0, c.Loc)
}

// FirstStart is the start of the first cycle of a team that has none: the
// latest cadence boundary (Weekday at Hour:Min, local) at or before now.
func (c Cadence) FirstStart(now time.Time) time.Time {
	l := now.In(c.Loc)
	back := (int(l.Weekday()) - int(c.Weekday) + 7) % 7
	s := c.at(l.Year(), l.Month(), l.Day()-back)
	if s.After(now) {
		s = c.at(l.Year(), l.Month(), l.Day()-back-7)
	}
	return s
}

// End is where a cycle that starts at start ends: LengthDays later on the
// local calendar, at the cadence's Hour:Min. The next cycle starts exactly
// there, so cycles never overlap and never leave a gap.
func (c Cadence) End(start time.Time) time.Time {
	l := start.In(c.Loc)
	e := c.at(l.Year(), l.Month(), l.Day()+c.LengthDays)
	if !e.After(start) {
		// Not reachable for a length of at least one day; it keeps the
		// cycles_range_check (ends_at > starts_at) true whatever the zone does.
		e = start.Add(time.Duration(c.LengthDays) * 24 * time.Hour)
	}
	return e
}

// LocalDate is the team-local calendar date of t, as midnight UTC (the
// form a Postgres date column is written and read in).
func (c Cadence) LocalDate(t time.Time) time.Time {
	l := t.In(c.Loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// LastDay is the local date of the last moment of a cycle that ends at end.
func (c Cadence) LastDay(end time.Time) time.Time {
	return c.LocalDate(end.Add(-time.Microsecond))
}
