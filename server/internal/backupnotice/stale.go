// Package backupnotice is the app's side of backup reporting (LAUN-75,
// docs/internal/decisions-T16-backups.md #29 and #41): the mail for a failed or
// successful run (at-most-once), the mail for a change-log row (at-least-once),
// the stale alert, and the sweep of sealed unsaved-test secrets.
//
// It works on rows (backup_runs, backup_change_log, backup_destinations), never
// on the change feed: backups emit no events (#26). It sends through the
// platform mail switch. A mail holds a destination name, a time, a status, a
// fixed code and text and a link: never a host, a URL, a secret, a body of a
// dump or an address in a log line.
package backupnotice

import "time"

// Schedule intervals of the stale rule (#41): two missed slots of a daily or a
// weekly destination.
const (
	dailyInterval  = 24 * time.Hour
	weeklyInterval = 168 * time.Hour
)

// StaleInput is what the stale rule reads about one enabled destination.
type StaleInput struct {
	Schedule     string // "daily" or "weekly"
	CreatedAt    time.Time
	EnabledSince *time.Time
	// LastSuccess is the finish of the newest schedule, catchup, retry or manual
	// run that succeeded. Test, probe, verify and exchange runs never count.
	LastSuccess *time.Time
}

// Threshold is how long a destination may go without a success before it is
// stale: max(staleAlertHours, 2 x its schedule interval) (#41). staleAlertHours
// 0 turns the rule off; the answer is then 0 and false.
func Threshold(staleAlertHours int, schedule string) (time.Duration, bool) {
	if staleAlertHours <= 0 {
		return 0, false
	}
	interval := dailyInterval
	if schedule == "weekly" {
		interval = weeklyInterval
	}
	th := time.Duration(staleAlertHours) * time.Hour
	if two := 2 * interval; two > th {
		th = two
	}
	return th, true
}

// Since is the moment the stale clock counts from: the last success, but never
// before the destination was created or last enabled. A destination that never
// succeeded counts from max(created_at, enabled_since) (#41); a re-enabled one
// does not alert for the time it was off (judgment call, decisions-T16 ticket 8
// note 3).
func Since(in StaleInput) time.Time {
	t := in.CreatedAt
	if in.EnabledSince != nil && in.EnabledSince.After(t) {
		t = *in.EnabledSince
	}
	if in.LastSuccess != nil && in.LastSuccess.After(t) {
		t = *in.LastSuccess
	}
	return t
}

// Stale reports whether the destination is stale at now: its clock (Since) is
// older than the threshold. It also returns the threshold. With the rule off
// (staleAlertHours 0) it is never stale.
func Stale(in StaleInput, staleAlertHours int, now time.Time) (stale bool, since time.Time, threshold time.Duration) {
	th, on := Threshold(staleAlertHours, in.Schedule)
	since = Since(in)
	if !on {
		return false, since, 0
	}
	return now.Sub(since) > th, since, th
}

// AlertEvery is the least time between two stale alerts of one destination.
const AlertEvery = 24 * time.Hour
