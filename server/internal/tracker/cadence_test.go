package tracker

import (
	"testing"
	"time"
)

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := LoadZone("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// orbit is the Orbit cadence: 14 days from Sunday 00:00 Europe/London.
func orbit(t *testing.T) Cadence {
	return Cadence{LengthDays: 14, Weekday: time.Sunday, Loc: london(t)}
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCadence_FirstStart(t *testing.T) {
	loc := london(t)
	tests := []struct {
		name string
		c    Cadence
		now  time.Time
		want time.Time
	}{
		{"midweek in BST goes back to Sunday 00:00 BST", orbit(t), utc("2026-10-21T11:00:00Z"), utc("2026-10-17T23:00:00Z")},
		{"exactly on the boundary is that boundary", orbit(t), utc("2026-10-17T23:00:00Z"), utc("2026-10-17T23:00:00Z")},
		{"one second before the boundary is the week before", orbit(t), utc("2026-10-17T22:59:59Z"), utc("2026-10-10T23:00:00Z")},
		{"midweek in GMT", orbit(t), utc("2026-11-04T12:00:00Z"), utc("2026-11-01T00:00:00Z")},
		{"Sunday before a 09:00 start goes back a week",
			Cadence{LengthDays: 7, Weekday: time.Sunday, Hour: 9, Loc: loc},
			time.Date(2026, 11, 8, 8, 0, 0, 0, loc), time.Date(2026, 11, 1, 9, 0, 0, 0, loc)},
		{"Monday cadence on a Monday after the start time",
			Cadence{LengthDays: 7, Weekday: time.Monday, Hour: 6, Min: 30, Loc: loc},
			time.Date(2026, 11, 9, 7, 0, 0, 0, loc), time.Date(2026, 11, 9, 6, 30, 0, 0, loc)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.FirstStart(tt.now); !got.Equal(tt.want) {
				t.Fatalf("FirstStart(%s) = %s, want %s", tt.now, got.UTC(), tt.want.UTC())
			}
		})
	}
}

// AC: a cycle starting Sunday 00:00 local must not drift an hour across DST.
func TestCadence_EndKeepsLocalWallClockAcrossDST(t *testing.T) {
	c := orbit(t)
	tests := []struct {
		name       string
		start, end time.Time
		length     time.Duration
	}{
		// BST ends 2026-10-25 01:00 UTC: the spanning cycle is an hour longer.
		{"autumn change", utc("2026-10-17T23:00:00Z"), utc("2026-11-01T00:00:00Z"), 14*24*time.Hour + time.Hour},
		// BST starts 2027-03-28 01:00 UTC: the spanning cycle is an hour shorter.
		{"spring change", utc("2027-03-21T00:00:00Z"), utc("2027-04-03T23:00:00Z"), 14*24*time.Hour - time.Hour},
		{"no change", utc("2026-11-01T00:00:00Z"), utc("2026-11-15T00:00:00Z"), 14 * 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.End(tt.start)
			if !got.Equal(tt.end) {
				t.Fatalf("End(%s) = %s, want %s", tt.start, got.UTC(), tt.end)
			}
			if d := got.Sub(tt.start); d != tt.length {
				t.Fatalf("length %s, want %s", d, tt.length)
			}
		})
	}
}

// A year of chained cycles: every start is Sunday 00:00 London, and each
// cycle starts where the one before ended.
func TestCadence_ChainStaysOnTheBoundaryForAYear(t *testing.T) {
	c := orbit(t)
	start := c.FirstStart(utc("2026-06-03T12:00:00Z"))
	for i := 0; i < 30; i++ {
		l := start.In(c.Loc)
		if l.Weekday() != time.Sunday || l.Hour() != 0 || l.Minute() != 0 {
			t.Fatalf("cycle %d starts %s local", i+1, l)
		}
		end := c.End(start)
		if !end.After(start) {
			t.Fatalf("cycle %d: end %s not after start %s", i+1, end, start)
		}
		start = end
	}
}

func TestCadence_OneDayCyclesInNewYork(t *testing.T) {
	ny, err := LoadZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	c := Cadence{LengthDays: 1, Weekday: time.Sunday, Hour: 9, Loc: ny}
	// US DST ends 2026-11-01 02:00 local.
	start := time.Date(2026, 10, 31, 9, 0, 0, 0, ny)
	end := c.End(start)
	if want := time.Date(2026, 11, 1, 9, 0, 0, 0, ny); !end.Equal(want) || end.Sub(start) != 25*time.Hour {
		t.Fatalf("End = %s (%s), want %s (25h)", end, end.Sub(start), want)
	}
}

func TestCadence_LocalDateAndLastDay(t *testing.T) {
	c := orbit(t)
	if got := c.LocalDate(utc("2026-10-17T23:30:00Z")); got.Format(time.DateOnly) != "2026-10-18" {
		t.Fatalf("LocalDate in BST = %s", got.Format(time.DateOnly))
	}
	if got := c.LocalDate(utc("2026-11-01T00:30:00Z")); got.Format(time.DateOnly) != "2026-11-01" {
		t.Fatalf("LocalDate in GMT = %s", got.Format(time.DateOnly))
	}
	if got := c.LastDay(utc("2026-11-01T00:00:00Z")); got.Format(time.DateOnly) != "2026-10-31" {
		t.Fatalf("LastDay = %s", got.Format(time.DateOnly))
	}
	if got := c.LocalDate(utc("2026-10-17T23:30:00Z")); got.Location() != time.UTC || got.Hour() != 0 {
		t.Fatalf("LocalDate is not midnight UTC: %s", got)
	}
}

func TestClock_ParseAndFormat(t *testing.T) {
	for _, s := range []string{"00:00", "09:30", "23:59"} {
		c, err := parseClock(s)
		if err != nil {
			t.Fatalf("parseClock(%q): %v", s, err)
		}
		if got := formatClock(c); got != s {
			t.Fatalf("round trip %q -> %q", s, got)
		}
	}
	for _, s := range []string{"24:00", "9:00", "09:60", "09:00:00", "", "noon"} {
		if _, err := parseClock(s); err == nil {
			t.Fatalf("parseClock(%q) accepted", s)
		}
	}
}

func TestLoadZone(t *testing.T) {
	for _, bad := range []string{"", "Local", "local", "Mars/Olympus"} {
		if _, err := LoadZone(bad); err == nil {
			t.Fatalf("LoadZone(%q) accepted", bad)
		}
	}
	if _, err := LoadZone("UTC"); err != nil {
		t.Fatal(err)
	}
}
