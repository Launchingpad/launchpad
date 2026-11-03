package offsite

import (
	"fmt"
	"time"
)

// LastSlot is the most recent scheduled time at or before now (decisions-T16
// #31 "Schedule"): daily at HH:MM UTC, or weekly on a weekday (0 Sunday to 6
// Saturday, as time.Weekday) at HH:MM UTC. UTC only in v1.
func LastSlot(now time.Time, schedule, atUTC string, weekday *int) (time.Time, error) {
	var h, m int
	if _, err := fmt.Sscanf(atUTC, "%02d:%02d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(atUTC) != 5 {
		return time.Time{}, fmt.Errorf("offsite: bad at_utc %q", atUTC)
	}
	now = now.UTC()
	slot := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
	switch schedule {
	case "daily":
		if slot.After(now) {
			slot = slot.AddDate(0, 0, -1)
		}
	case "weekly":
		if weekday == nil || *weekday < 0 || *weekday > 6 {
			return time.Time{}, fmt.Errorf("offsite: weekly schedule without a weekday")
		}
		back := (int(now.Weekday()) - *weekday + 7) % 7
		slot = slot.AddDate(0, 0, -back)
		if slot.After(now) {
			slot = slot.AddDate(0, 0, -7)
		}
	default:
		return time.Time{}, fmt.Errorf("offsite: unknown schedule %q", schedule)
	}
	return slot, nil
}

// scheduleGrace: a slot noticed within this long after it passed is a normal
// `schedule` run; later it is a `catchup` run (a missed slot, #31).
const scheduleGrace = 5 * time.Minute
