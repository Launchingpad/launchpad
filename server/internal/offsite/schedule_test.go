package offsite

import (
	"testing"
	"time"
)

func TestLastSlot(t *testing.T) {
	d := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	wd := func(i int) *int { return &i }
	cases := []struct {
		name     string
		now      string
		sched    string
		at       string
		weekday  *int
		want     string
		wantFail bool
	}{
		{"daily after the time", "2026-10-27T05:00:00Z", "daily", "04:15", nil, "2026-10-27T04:15:00Z", false},
		{"daily before the time", "2026-10-27T03:00:00Z", "daily", "04:15", nil, "2026-10-26T04:15:00Z", false},
		{"daily exactly at the time", "2026-10-27T04:15:00Z", "daily", "04:15", nil, "2026-10-27T04:15:00Z", false},
		{"daily across a month", "2026-11-01T01:00:00Z", "daily", "04:15", nil, "2026-10-31T04:15:00Z", false},
		// 2026-10-27 is a Tuesday (2).
		{"weekly same day after", "2026-10-27T05:00:00Z", "weekly", "04:15", wd(2), "2026-10-27T04:15:00Z", false},
		{"weekly same day before", "2026-10-27T03:00:00Z", "weekly", "04:15", wd(2), "2026-10-20T04:15:00Z", false},
		{"weekly sunday", "2026-10-27T05:00:00Z", "weekly", "04:15", wd(0), "2026-10-25T04:15:00Z", false},
		{"weekly saturday", "2026-10-27T05:00:00Z", "weekly", "23:59", wd(6), "2026-10-24T23:59:00Z", false},
		{"weekly no weekday", "2026-10-27T05:00:00Z", "weekly", "04:15", nil, "", true},
		{"bad time", "2026-10-27T05:00:00Z", "daily", "25:00", nil, "", true},
		{"bad schedule", "2026-10-27T05:00:00Z", "hourly", "04:15", nil, "", true},
	}
	for _, c := range cases {
		got, err := LastSlot(d(c.now), c.sched, c.at, c.weekday)
		if c.wantFail {
			if err == nil {
				t.Errorf("%s: want an error, got %v", c.name, got)
			}
			continue
		}
		if err != nil || !got.Equal(d(c.want)) {
			t.Errorf("%s: got %v %v, want %s", c.name, got, err, c.want)
		}
	}
}
