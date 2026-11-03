package tracker

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func tm(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func TestDueAtOrBefore_Weekly(t *testing.T) {
	s := UpdateSchedule{Frequency: FreqWeekly, Weekday: 5, Hour: 9} // Friday 09:00
	utc := time.UTC
	for _, c := range []struct{ now, want time.Time }{
		{tm(2026, 10, 30, 9, 0), tm(2026, 10, 30, 9, 0)},  // exactly on the due time
		{tm(2026, 10, 30, 8, 59), tm(2026, 10, 23, 9, 0)}, // a minute before: last week's
		{tm(2026, 10, 31, 0, 0), tm(2026, 10, 30, 9, 0)},
		{tm(2026, 11, 5, 23, 59), tm(2026, 10, 30, 9, 0)},
	} {
		got, ok := s.dueAtOrBefore(utc, c.now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("now %v: got %v %v, want %v", c.now, got, ok, c.want)
		}
	}
	if _, ok := (UpdateSchedule{Frequency: FreqNone, Weekday: 5, Hour: 9}).dueAtOrBefore(utc, tm(2026, 10, 30, 9, 0)); ok {
		t.Error("frequency none has a due time")
	}
}

func TestDueAtOrBefore_BiweeklyIsEveryOtherWeek(t *testing.T) {
	s := UpdateSchedule{Frequency: FreqBiweekly, Weekday: 5, Hour: 9}
	var prev time.Time
	// Walk Fridays: due times are 14 days apart, never 7.
	for d := tm(2026, 1, 2, 9, 0); d.Before(tm(2026, 12, 31, 0, 0)); d = d.AddDate(0, 0, 7) {
		got, ok := s.dueAtOrBefore(time.UTC, d)
		if !ok {
			t.Fatal("no due time")
		}
		if !prev.IsZero() && !got.Equal(prev) && got.Sub(prev) != 14*24*time.Hour {
			t.Fatalf("due times %v and %v are not 14 days apart", prev, got)
		}
		prev = got
	}
}

func TestDueAtOrBefore_MonthlyIsTheFirstWeekdayOfTheMonth(t *testing.T) {
	s := UpdateSchedule{Frequency: FreqMonthly, Weekday: 5, Hour: 9}
	for _, c := range []struct{ now, want time.Time }{
		{tm(2026, 10, 30, 12, 0), tm(2026, 10, 2, 9, 0)}, // Oct 2026: first Friday is the 2nd
		{tm(2026, 11, 6, 8, 0), tm(2026, 10, 2, 9, 0)},   // Nov first Friday is the 6th, 09:00 not reached
		{tm(2026, 11, 6, 9, 0), tm(2026, 11, 6, 9, 0)},
		{tm(2026, 12, 31, 0, 0), tm(2026, 12, 4, 9, 0)},
	} {
		got, ok := s.dueAtOrBefore(time.UTC, c.now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("now %v: got %v, want %v", c.now, got, c.want)
		}
	}
}

// The local hour is fixed; the UTC instant moves with the clocks.
func TestDueAtOrBefore_DST(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	s := UpdateSchedule{Frequency: FreqWeekly, Weekday: 0, Hour: 10} // Sunday 10:00 local
	// 2026-03-29 clocks go forward at 01:00 UTC. Sunday 10:00 is 09:00 UTC (BST), a week before 10:00 UTC (GMT).
	if got, _ := s.dueAtOrBefore(london, tm(2026, 3, 29, 12, 0)); !got.Equal(tm(2026, 3, 29, 9, 0)) {
		t.Errorf("BST due time %v", got)
	}
	if got, _ := s.dueAtOrBefore(london, tm(2026, 3, 28, 12, 0)); !got.Equal(tm(2026, 3, 22, 10, 0)) {
		t.Errorf("GMT due time %v", got)
	}
	// An hour that does not exist (01:30 on the spring-forward day): one stable instant, once.
	skip := UpdateSchedule{Frequency: FreqWeekly, Weekday: 0, Hour: 1}
	a, ok := skip.dueAtOrBefore(london, tm(2026, 3, 29, 12, 0))
	b, _ := skip.dueAtOrBefore(london, tm(2026, 3, 29, 20, 0))
	if !ok || !a.Equal(b) || a.Day() != 29 || a.Month() != time.March {
		t.Errorf("missing hour: %v %v", a, b)
	}
	// An hour that happens twice (fall back, 2026-10-25 01:00-02:00 local): one instant, the same each time.
	twice := UpdateSchedule{Frequency: FreqWeekly, Weekday: 0, Hour: 1}
	c, _ := twice.dueAtOrBefore(london, tm(2026, 10, 25, 12, 0))
	d, _ := twice.dueAtOrBefore(london, tm(2026, 10, 25, 23, 0))
	if !c.Equal(d) {
		t.Errorf("repeated hour: %v %v", c, d)
	}
}

func TestUpdateScheduleApply(t *testing.T) {
	s := UpdateSchedule{Frequency: FreqNone, Weekday: 5, Hour: 9}
	f, w, h := FreqWeekly, 1, 17
	got, err := s.apply(UpdateScheduleInput{Frequency: &f, Weekday: &w, Hour: &h})
	if err != nil || got != (UpdateSchedule{FreqWeekly, 1, 17}) {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ := s.apply(UpdateScheduleInput{}); got != s {
		t.Errorf("empty input changed the schedule: %+v", got)
	}
	bad := "daily"
	w7, h24, wneg := 7, 24, -1
	for _, in := range []UpdateScheduleInput{{Frequency: &bad}, {Weekday: &w7}, {Weekday: &wneg}, {Hour: &h24}} {
		if _, err := s.apply(in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
}

// Biweekly parity is pinned: Mondays-start week index (days since 1970-01-01, plus 3, over 7) must be even.
// The week that starts Monday 1970-01-05 has index 1, so it is NOT due.
func TestDueAtOrBefore_BiweeklyParityIsPinned(t *testing.T) {
	s := UpdateSchedule{Frequency: FreqBiweekly, Weekday: 5, Hour: 9}
	for _, c := range []struct {
		day time.Time
		due bool
	}{
		{tm(2026, 10, 23, 9, 0), true},
		{tm(2026, 10, 30, 9, 0), false}, // the off Friday: the due time is still Oct 23
		{tm(2026, 11, 6, 9, 0), true},
		{tm(1970, 1, 9, 9, 0), false}, // Friday of the week of 1970-01-05
	} {
		got, ok := s.dueAtOrBefore(time.UTC, c.day)
		if c.due && (!ok || !got.Equal(c.day)) {
			t.Errorf("%v should be due, got %v", c.day, got)
		}
		if !c.due && ok && got.Equal(c.day) {
			t.Errorf("%v should not be due", c.day)
		}
	}
}
