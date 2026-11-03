//go:build backuptest

package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"devops/internal/backupapi"
)

// LAUN-75: the status route's notice fields (docs/internal/decisions-T16-backups.md #41):
// mailReady, the stale and failed destinations and their banners. The clock is the
// injected bk.clock; the gate (platform admin only) is covered by the route matrix.

// place sets a destination's created_at and enabled_since to hours before the injected clock.
func (bk *bkEnv) place(id any, createdHoursAgo, enabledHoursAgo float64) {
	bk.t.Helper()
	at := func(h float64) time.Time { return bk.clock.Now().Add(-time.Duration(h * float64(time.Hour))) }
	if _, err := bk.pool.Exec(context.Background(),
		`UPDATE backup_destinations SET created_at = $2, enabled_since = $3 WHERE id = $1`, id, at(createdHoursAgo), at(enabledHoursAgo)); err != nil {
		bk.t.Fatal(err)
	}
}

func (bk *bkEnv) addRun(dest any, name, trigger, status string, willRetry bool, finishedHoursAgo float64, code string) {
	bk.t.Helper()
	fin := bk.clock.Now().Add(-time.Duration(finishedHoursAgo * float64(time.Hour)))
	if _, err := bk.pool.Exec(context.Background(), `INSERT INTO backup_runs (destination_id, destination_name, trigger, status, will_retry,
		queued_at, finished_at, error_code, error_text) VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8)`,
		dest, name, trigger, status, willRetry, fin, code, map[bool]string{true: "a fixed safe text", false: ""}[code != ""]); err != nil {
		bk.t.Fatal(err)
	}
}

func TestStatus_MailReadyAndMailOffBanner(t *testing.T) {
	ready := false
	bk := newBkEnv(t, bkOpts{mailReady: func() bool { return ready }})
	bk.ready()
	if st := bk.status(); st.MailReady || slices.Contains(st.Banners, backupapi.BannerMailOff) {
		t.Fatalf("no destination enabled yet: mailReady=%v banners=%v", st.MailReady, st.Banners)
	}
	bk.enabled("nas", "s3")
	st := bk.status()
	if st.MailReady || !slices.Contains(st.Banners, backupapi.BannerMailOff) {
		t.Fatalf("mail off with an enabled destination: mailReady=%v banners=%v", st.MailReady, st.Banners)
	}
	ready = true
	if st := bk.status(); !st.MailReady || slices.Contains(st.Banners, backupapi.BannerMailOff) {
		t.Fatalf("mail on: mailReady=%v banners=%v", st.MailReady, st.Banners)
	}
}

func TestStatus_StaleDestinationsFollowTheRule(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	daily := bk.enabled("daily", "s3")
	weekly := bk.enabled("weekly", "webdav")
	bk.must(200, bk.patch(weekly.ID, map[string]any{"schedule": "weekly", "weekday": 2, "atUtc": "04:15"}))
	bk.place(daily.ID, 1000, 1000)
	bk.place(weekly.ID, 1000, 1000)
	bk.addRun(daily.ID, "daily", "schedule", "succeeded", false, 47, "")
	bk.addRun(weekly.ID, "weekly", "schedule", "succeeded", false, 60, "")
	st := bk.status()
	if len(st.StaleDestinations) != 0 || slices.Contains(st.Banners, backupapi.BannerStale) || st.StaleAlertHours != 48 {
		t.Fatalf("47h daily and 60h weekly are not stale: %+v %v", st.StaleDestinations, st.Banners)
	}
	bk.clock.Advance(2 * time.Hour) // daily is now 49h, weekly 62h
	st = bk.status()
	if len(st.StaleDestinations) != 1 || st.StaleDestinations[0].Name != "daily" || st.StaleDestinations[0].ThresholdHours != 48 ||
		st.StaleDestinations[0].LastSuccessAt == nil || !slices.Contains(st.Banners, backupapi.BannerStale) {
		t.Fatalf("daily at 49h: %+v %v", st.StaleDestinations, st.Banners)
	}
	bk.clock.Advance(280 * time.Hour) // weekly past 336h
	if st = bk.status(); len(st.StaleDestinations) != 2 {
		t.Fatalf("weekly after two missed slots: %+v", st.StaleDestinations)
	}
	// A test, verify or exchange success does not clear it; a schedule success does.
	bk.addRun(daily.ID, "daily", "verify", "succeeded", false, 0, "")
	bk.addRun(daily.ID, "daily", "test", "succeeded", false, 0, "")
	if st = bk.status(); len(st.StaleDestinations) != 2 {
		t.Fatalf("test and verify runs counted: %+v", st.StaleDestinations)
	}
	bk.addRun(daily.ID, "daily", "manual", "succeeded", false, 0, "")
	if st = bk.status(); len(st.StaleDestinations) != 1 || st.StaleDestinations[0].Name != "weekly" {
		t.Fatalf("after a manual success: %+v", st.StaleDestinations)
	}
	// Setting 0 turns the rule and its banner off.
	bk.must(200, bk.do("PUT", bkp+"/settings", bk.admin, map[string]any{"notifyOnFailure": true, "notifyOnSuccess": false, "staleAlertHours": 0}))
	if st = bk.status(); len(st.StaleDestinations) != 0 || slices.Contains(st.Banners, backupapi.BannerStale) || st.StaleAlertHours != 0 {
		t.Fatalf("stale rule off: %+v %v", st.StaleDestinations, st.Banners)
	}
}

func TestStatus_DisabledDestinationIsNeverStaleOrFailed(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	d := bk.dest("off", "s3")
	bk.place(d.ID, 1000, 1000)
	bk.addRun(d.ID, "off", "schedule", "failed", false, 1, "auth_failed")
	st := bk.status()
	if len(st.StaleDestinations) != 0 || len(st.FailedDestinations) != 0 {
		t.Fatalf("disabled destination listed: %+v %+v", st.StaleDestinations, st.FailedDestinations)
	}
}

func TestStatus_FailedDestinationsNeedAFinalFailureAsTheNewestBackupRun(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	d := bk.enabled("nas", "s3")
	bk.place(d.ID, 2, 2)
	bk.addRun(d.ID, "nas", "schedule", "failed", true, 1.5, "timeout") // will retry: not a final failure
	if st := bk.status(); len(st.FailedDestinations) != 0 || slices.Contains(st.Banners, backupapi.BannerRunFailed) {
		t.Fatalf("a failure with a retry coming is listed: %+v", st.FailedDestinations)
	}
	bk.addRun(d.ID, "nas", "retry", "failed", false, 1, "auth_failed")
	st := bk.status()
	if len(st.FailedDestinations) != 1 || st.FailedDestinations[0].ErrorCode != "auth_failed" || st.FailedDestinations[0].Name != "nas" ||
		!slices.Contains(st.Banners, backupapi.BannerRunFailed) {
		t.Fatalf("final failure: %+v %v", st.FailedDestinations, st.Banners)
	}
	// A failed TEST run is not a backup failure; a newer success clears it.
	bk.addRun(d.ID, "nas", "test", "failed", false, 0.5, "auth_failed")
	if st = bk.status(); len(st.FailedDestinations) != 1 {
		t.Fatalf("test run changed the answer: %+v", st.FailedDestinations)
	}
	bk.addRun(d.ID, "nas", "manual", "succeeded", false, 0.1, "")
	if st = bk.status(); len(st.FailedDestinations) != 0 {
		t.Fatalf("a newer success did not clear it: %+v", st.FailedDestinations)
	}
}

func TestStatus_NoticeFieldsCarryNoSecretAndNoHost(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	d := bk.enabled("nas", "s3")
	bk.place(d.ID, 1000, 1000)
	bk.addRun(d.ID, "nas", "schedule", "failed", false, 1, "auth_failed")
	r := bk.must(200, bk.do("GET", bkp+"/status", bk.admin, nil))
	body := string(r.body)
	for _, bad := range []string{bkSecretKey, bkAccessKey, "example.com", "https://"} {
		if strings.Contains(body, bad) {
			t.Errorf("status holds %q: %s", bad, body)
		}
	}
}

// LAUN-80: "files not backed up". Only a server that keeps attachments says it; an
// enabled destination needs a succeeded schedule/manual/catchup/retry run with an ok
// `files` step in the last 48 hours; a failed files step, a test run and a disabled
// destination do not count.
func TestStatus_FilesNotBackedUp(t *testing.T) {
	stored := false
	bk := newBkEnv(t, bkOpts{filesStored: func() bool { return stored }})
	bk.ready()
	d := bk.enabled("nas", "s3")
	off := bk.dest("off", "s3")
	bk.place(d.ID, 1000, 1000)
	bk.place(off.ID, 1000, 1000)
	if st := bk.status(); st.FilesStored || len(st.FilesNotBackedUp) != 0 || slices.Contains(st.Banners, backupapi.BannerFilesNotBackedUp) {
		t.Fatalf("a server without attachments: %+v %v", st.FilesNotBackedUp, st.Banners)
	}
	stored = true
	st := bk.status()
	if !st.FilesStored || len(st.FilesNotBackedUp) != 1 || st.FilesNotBackedUp[0].Name != "nas" || st.FilesNotBackedUp[0].LastBackupAt != nil ||
		!slices.Contains(st.Banners, backupapi.BannerFilesNotBackedUp) {
		t.Fatalf("no files backup yet: %+v %v", st.FilesNotBackedUp, st.Banners)
	}
	step := func(run string, dest any, trigger, steps string, hoursAgo float64) {
		fin := bk.clock.Now().Add(-time.Duration(hoursAgo * float64(time.Hour)))
		if _, err := bk.pool.Exec(context.Background(), `INSERT INTO backup_runs (destination_id, destination_name, trigger, status, queued_at, finished_at, steps)
			VALUES ($1, $2, $3, 'succeeded', $4, $4, $5::jsonb)`, dest, run, trigger, fin, steps); err != nil {
			t.Fatal(err)
		}
	}
	step("nas", d.ID, "schedule", `[{"name":"files","ok":false,"code":"auth_failed"}]`, 1)
	step("nas", d.ID, "test", `[{"name":"files","ok":true}]`, 1)
	step("nas", d.ID, "schedule", `[]`, 1)
	if st = bk.status(); len(st.FilesNotBackedUp) != 1 {
		t.Fatalf("a failed step, a test run or no step counted: %+v", st.FilesNotBackedUp)
	}
	step("nas", d.ID, "schedule", `[{"name":"files","ok":true,"objects":3}]`, 47)
	if st = bk.status(); len(st.FilesNotBackedUp) != 0 || slices.Contains(st.Banners, backupapi.BannerFilesNotBackedUp) {
		t.Fatalf("a files backup 47h ago: %+v %v", st.FilesNotBackedUp, st.Banners)
	}
	bk.clock.Advance(2 * time.Hour) // 49h
	st = bk.status()
	if len(st.FilesNotBackedUp) != 1 || st.FilesNotBackedUp[0].LastBackupAt == nil || !slices.Contains(st.Banners, backupapi.BannerFilesNotBackedUp) {
		t.Fatalf("a files backup 49h ago: %+v %v", st.FilesNotBackedUp, st.Banners)
	}
}
