package backupnotice

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func ago(h float64) *time.Time {
	t := t0.Add(-time.Duration(h * float64(time.Hour)))
	return &t
}

func TestThreshold(t *testing.T) {
	tests := []struct {
		hours    int
		schedule string
		want     time.Duration
		on       bool
	}{
		{0, "daily", 0, false},
		{0, "weekly", 0, false},
		{48, "daily", 48 * time.Hour, true},
		{12, "daily", 48 * time.Hour, true},   // two daily slots beat a short setting
		{48, "weekly", 336 * time.Hour, true}, // a weekly destination never alerts at 48 hours
		{400, "weekly", 400 * time.Hour, true},
		{8760, "daily", 8760 * time.Hour, true},
	}
	for _, tt := range tests {
		got, on := Threshold(tt.hours, tt.schedule)
		if got != tt.want || on != tt.on {
			t.Errorf("Threshold(%d, %s) = %v, %v; want %v, %v", tt.hours, tt.schedule, got, on, tt.want, tt.on)
		}
	}
}

func TestStale(t *testing.T) {
	created := *ago(1000)
	tests := []struct {
		name  string
		in    StaleInput
		hours int
		want  bool
	}{
		{"daily 47h after a success", StaleInput{Schedule: "daily", CreatedAt: created, LastSuccess: ago(47)}, 48, false},
		{"daily exactly at the threshold", StaleInput{Schedule: "daily", CreatedAt: created, LastSuccess: ago(48)}, 48, false},
		{"daily just past the threshold", StaleInput{Schedule: "daily", CreatedAt: created, LastSuccess: ago(48.01)}, 48, true},
		{"weekly at 60h: no alert", StaleInput{Schedule: "weekly", CreatedAt: created, LastSuccess: ago(60)}, 48, false},
		{"weekly after two missed slots", StaleInput{Schedule: "weekly", CreatedAt: created, LastSuccess: ago(337)}, 48, true},
		{"setting 0 is off", StaleInput{Schedule: "daily", CreatedAt: created, LastSuccess: ago(5000)}, 0, false},
		{"never succeeded: counts from creation", StaleInput{Schedule: "daily", CreatedAt: *ago(60)}, 48, true},
		{"never succeeded: counts from enabling", StaleInput{Schedule: "daily", CreatedAt: created, EnabledSince: ago(10)}, 48, false},
		{"never succeeded, enabled long ago", StaleInput{Schedule: "daily", CreatedAt: created, EnabledSince: ago(50)}, 48, true},
		{"re-enabled: the off time does not count", StaleInput{Schedule: "daily", CreatedAt: created, EnabledSince: ago(5), LastSuccess: ago(500)}, 48, false},
	}
	for _, tt := range tests {
		got, _, _ := Stale(tt.in, tt.hours, t0)
		if got != tt.want {
			t.Errorf("%s: stale = %v, want %v", tt.name, got, tt.want)
		}
	}
}
