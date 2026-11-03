package importer

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestStateType(t *testing.T) {
	tests := []struct {
		in, want string
		err      bool
	}{
		{"triage", "triage", false}, {"backlog", "backlog", false}, {"unstarted", "unstarted", false},
		{"started", "started", false}, {"completed", "completed", false}, {"canceled", "canceled", false},
		{"duplicate", "canceled", false}, {"Started", "", true}, {"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := stateType(tt.in)
			if (err != nil) != tt.err || got != tt.want {
				t.Errorf("stateType(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

func TestRelationType(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"blocks", "blocks", true}, {"duplicate", "duplicate_of", true}, {"related", "related", true},
		{"similar", "", false}, {"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got, ok := relationType(tt.in); got != tt.want || ok != tt.ok {
				t.Errorf("relationType(%q) = %q, %v", tt.in, got, ok)
			}
		})
	}
}

func TestPriorityAndEstimate(t *testing.T) {
	for in, want := range map[float64]int16{0: 0, 1: 1, 4: 4, 5: 0, -1: 0, 2.4: 2, math.NaN(): 0} {
		if got := priority(in); got != want {
			t.Errorf("priority(%v) = %d, want %d", in, got, want)
		}
	}
	if estimate(nil) != nil || estimate(fp(-1)) != nil || estimate(fp(math.Inf(1))) != nil {
		t.Error("nil, negative and infinite estimates must be no estimate")
	}
	if got := estimate(fp(2.5)); got == nil || *got != 3 {
		t.Errorf("estimate(2.5) = %v, want 3", got)
	}
	if got := estimate(fp(0)); got == nil || *got != 0 {
		t.Errorf("estimate(0) = %v, want 0 (an explicit zero is kept)", got)
	}
}

func TestClip(t *testing.T) {
	tests := []struct {
		name, in string
		n        int
		want     string
	}{
		{"short", "abc", 5, "abc"},
		{"runes not bytes", "ééééé", 3, "ééé"},
		{"trimmed", "  x  ", 5, "x"},
		{"empty uses fallback", "   ", 5, "fb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clip(tt.in, tt.n, "fb"); got != tt.want {
				t.Errorf("clip(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestDocSlug(t *testing.T) {
	tests := []struct{ title, id, want string }{
		{"Cutover plan", "a1b2c3d4e5f6", "cutover-plan-a1b2c3d4e5f6"},
		{"!!!", "a1b2c3d4e5f6", "a1b2c3d4e5f6"},
		{"Plan", "Not Hex!", "plan-not-hex"},
	}
	for _, tt := range tests {
		if got := docSlug(tt.title, tt.id); got != tt.want {
			t.Errorf("docSlug(%q, %q) = %q, want %q", tt.title, tt.id, got, tt.want)
		}
	}
}

func TestDeriveCadence(t *testing.T) {
	team := Team{ID: "t", CycleDuration: 2, CycleStartDay: 3, Timezone: "Europe/London"}
	t.Run("from the newest cycle, in the team's zone", func(t *testing.T) {
		cycles := []Cycle{
			{Team: ref("t"), Number: 6, StartsAt: ts("2026-03-01T00:00:00Z")},  // a Sunday in GMT
			{Team: ref("t"), Number: 7, StartsAt: ts("2026-08-29T23:00:00Z")},  // Sunday 00:00 BST
			{Team: ref("x"), Number: 99, StartsAt: ts("2026-09-02T10:00:00Z")}, // another team
		}
		c, ok := deriveCadence(team, cycles)
		if !ok || c.LengthDays != 14 || c.Weekday != 0 || c.Hour != 0 || c.Min != 0 {
			t.Errorf("cadence %+v ok=%v, want 14 days Sunday 00:00", c, ok)
		}
	})
	t.Run("no cycles: cycleStartDay at 00:00", func(t *testing.T) {
		c, ok := deriveCadence(team, nil)
		if !ok || c.Weekday != 3 || c.Hour != 0 {
			t.Errorf("cadence %+v ok=%v, want Wednesday 00:00", c, ok)
		}
	})
	t.Run("an unusable zone changes nothing", func(t *testing.T) {
		bad := team
		bad.Timezone = "Mars/Olympus"
		if _, ok := deriveCadence(bad, nil); ok {
			t.Error("want ok=false")
		}
	})
	t.Run("an out-of-range duration falls back to 14 days", func(t *testing.T) {
		long := team
		long.CycleDuration = 20
		if c, _ := deriveCadence(long, nil); c.LengthDays != 14 {
			t.Errorf("length %d", c.LengthDays)
		}
	})
}

func TestSnapshots(t *testing.T) {
	cy := Cycle{StartsAt: ts("2026-10-17T23:00:00Z"), EndsAt: ts("2026-10-20T23:00:00Z"), // Sun 18 Oct 00:00 BST
		IssueCountHistory: []float64{1, 2, 3, 4, 5, 6}, CompletedIssueCountHistory: []float64{0, 1},
		ScopeHistory: []float64{1.4, 2}, CompletedScopeHistory: []float64{-3}, InProgressScopeHistory: []float64{}}
	got := snapshots(cy, "Europe/London")
	// Three days plus the end day at most, whatever the arrays hold.
	if len(got) != 4 {
		t.Fatalf("%d rows, want 4", len(got))
	}
	// Dates are LOCAL: the cycle starts on the 18th in London although the
	// instant is the 17th in UTC.
	if want := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC); !got[0].Date.Equal(want) {
		t.Errorf("first date %s, want %s", got[0].Date, want)
	}
	if !got[3].Date.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last date %s", got[3].Date)
	}
	if got[1].ScopeCount != 2 || got[1].CompletedCount != 1 || got[0].ScopePoints != 1 || got[0].CompletedPoints != 0 {
		t.Errorf("rows %+v", got[:2])
	}
	if got[2].CompletedCount != 0 || got[2].ScopePoints != 0 {
		t.Errorf("a short array reads as 0: %+v", got[2])
	}
	if snapshots(Cycle{StartsAt: cy.StartsAt, EndsAt: cy.EndsAt}, "Europe/London") != nil {
		t.Error("no history: no rows")
	}
}

func TestMissingNumbers(t *testing.T) {
	tests := []struct {
		in, want []int32
	}{
		{nil, nil}, {[]int32{3}, nil}, {[]int32{1, 2, 3}, nil},
		{[]int32{7, 10, 8}, []int32{9}}, {[]int32{5, 1}, []int32{2, 3, 4}},
	}
	for _, tt := range tests {
		if got := missingNumbers(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("missingNumbers(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestWhole(t *testing.T) {
	for _, bad := range []float64{0, -1, 1.5, math.NaN(), 3e9} {
		if _, err := whole(bad); err == nil {
			t.Errorf("whole(%v): want an error", bad)
		}
	}
	if n, err := whole(3404); err != nil || n != 3404 {
		t.Errorf("whole(3404) = %d, %v", n, err)
	}
}

func TestFreeName(t *testing.T) {
	taken := map[string]bool{"Done": true, "Done (2)": true}
	if got := freeName("Done", 100, func(s string) bool { return taken[s] }); got != "Done (3)" {
		t.Errorf("freeName = %q", got)
	}
	if got := freeName("New", 100, func(s string) bool { return taken[s] }); got != "New" {
		t.Errorf("freeName = %q", got)
	}
}
