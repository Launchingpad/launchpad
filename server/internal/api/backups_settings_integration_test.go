//go:build backuptest

package api

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"devops/internal/backupapi"
	"devops/internal/dbq"
	"devops/internal/platform"
)

func (bk *bkEnv) settings() backupapi.Settings {
	bk.t.Helper()
	var s backupapi.Settings
	bk.must(200, bk.do("GET", bkp+"/settings", bk.admin, nil)).json(bk.t, &s)
	return s
}

func (bk *bkEnv) status() backupapi.Status {
	bk.t.Helper()
	var s backupapi.Status
	bk.must(200, bk.do("GET", bkp+"/status", bk.admin, nil)).json(bk.t, &s)
	return s
}

func TestBackups_Settings(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	s := bk.settings()
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(s.InstallID) || s.SigningPublic != "" ||
		!s.NotifyOnFailure || s.NotifyOnSuccess || s.StaleAlertHours != 48 {
		t.Fatalf("defaults: %+v", s)
	}
	valid := map[string]any{"notifyOnFailure": false, "notifyOnSuccess": true, "staleAlertHours": 0}
	bad := []struct {
		name string
		body map[string]any
		want string
	}{
		{"missing field", map[string]any{"notifyOnFailure": true, "notifyOnSuccess": true}, "staleAlertHours"},
		{"negative hours", map[string]any{"notifyOnFailure": true, "notifyOnSuccess": true, "staleAlertHours": -1}, "staleAlertHours must be"},
		{"hours too large", map[string]any{"notifyOnFailure": true, "notifyOnSuccess": true, "staleAlertHours": 8761}, "staleAlertHours must be"},
		{"unknown field", map[string]any{"notifyOnFailure": true, "notifyOnSuccess": true, "staleAlertHours": 1, "signingPublic": "x"}, "signingPublic"},
		{"string for a bool", map[string]any{"notifyOnFailure": "yes", "notifyOnSuccess": true, "staleAlertHours": 1}, "invalid JSON"},
	}
	for _, b := range bad {
		r := bk.do("PUT", bkp+"/settings", bk.admin, b.body)
		if r.status != 400 || !strings.Contains(string(r.body), b.want) {
			t.Errorf("%s: %d %s, want 400 %q", b.name, r.status, r.body, b.want)
		}
	}
	if n := bk.countRows("backup_change_log"); n != 0 {
		t.Fatalf("a refused PUT wrote %d change rows", n)
	}
	var out backupapi.Settings
	bk.must(200, bk.do("PUT", bkp+"/settings", bk.admin, valid)).json(t, &out)
	if out.NotifyOnFailure || !out.NotifyOnSuccess || out.StaleAlertHours != 0 || out.UpdatedBy == nil || *out.UpdatedBy != bk.chen.ID {
		t.Fatalf("stored: %+v", out)
	}
	if got := bk.kinds(); len(got) != 1 || got[0] != "notice_settings_changed" {
		t.Fatalf("change kinds %v", got)
	}
	// The same values again change nothing and write nothing.
	bk.must(200, bk.do("PUT", bkp+"/settings", bk.admin, valid))
	if got := bk.kinds(); len(got) != 1 {
		t.Fatalf("a save that changed nothing wrote a row: %v", got)
	}
	ch := bk.changes()[0]
	if ch.ActorID == nil || *ch.ActorID != bk.chen.ID || ch.ActorUsername == nil || *ch.ActorUsername != "chen" {
		t.Fatalf("actor: %+v", ch)
	}
	// The signing key is not writable through the API (there is no route, and the field is unknown).
	r := bk.do("PUT", bkp+"/settings", bk.admin, map[string]any{"notifyOnFailure": true, "notifyOnSuccess": true, "staleAlertHours": 1, "signingPublic": "AAAA"})
	if r.status != 400 {
		t.Fatalf("signingPublic in a PUT: %d", r.status)
	}
	for _, m := range []string{"PUT", "POST", "PATCH", "DELETE"} {
		if r := bk.do(m, bkp+"/signing", bk.admin, map[string]any{}); r.status != 404 {
			t.Errorf("%s /signing: %d (there is no route to replace the signing key)", m, r.status)
		}
	}
}

func TestBackups_Status(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	s := bk.status()
	if !s.EncryptionKeySet || s.KeyMismatch || s.SigningPublic != "" || s.NewestDump != nil || !s.NoRecentDump ||
		s.Destinations != 0 || len(s.Recipients) != 0 || s.EverHadRecipient {
		t.Fatalf("empty install: %+v", s)
	}
	// supportedTypes comes from the driver registry: the three fakes, no dropbox.
	if got := strings.Join(sliceOf(s.SupportedTypes), ","); got != "s3,sftp,webdav" {
		t.Fatalf("supportedTypes = %s", got)
	}
	want := []string{backupapi.BannerSigningMissing, backupapi.BannerNoRecentDump}
	if strings.Join(s.Banners, ",") != strings.Join(want, ",") {
		t.Fatalf("banners %v, want %v", s.Banners, want)
	}
	bk.ready()
	bk.dest("one", "s3")
	// A dump row, as backup.sh writes it.
	ctx := context.Background()
	if _, err := bk.pool.Exec(ctx, `INSERT INTO backup_dumps (file_name, status, finished_at, bytes, migration_version)
		VALUES ('devops-20261027T031500Z.dump', 'ok', now(), 1234, 20)`); err != nil {
		t.Fatal(err)
	}
	s = bk.status()
	if s.NoRecentDump || s.NewestDump == nil || s.NewestDump.FileName != "devops-20261027T031500Z.dump" || *s.NewestDump.Bytes != 1234 ||
		s.SigningPublic == "" || s.ActiveRecipients != 1 || !s.EverHadRecipient || s.Destinations != 1 || s.EnabledCount != 0 || len(s.Banners) != 0 {
		t.Fatalf("ready install: %+v", s)
	}
	// An ok dump older than 36 hours is no recent dump.
	if _, err := bk.pool.Exec(ctx, `UPDATE backup_dumps SET finished_at = now() - interval '37 hours'`); err != nil {
		t.Fatal(err)
	}
	if s = bk.status(); !s.NoRecentDump || len(s.Banners) != 1 || s.Banners[0] != backupapi.BannerNoRecentDump {
		t.Fatalf("a stale dump: %+v", s)
	}
}

func sliceOf(v []string) []string { return v }

// TestBackups_Status_Key: no key, a changed key, an unreadable signing key.
func TestBackups_Status_Key(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	bk.dest("one", "s3") // seals a secret: key_check is stored
	s := bk.status()
	if s.KeyMismatch || s.SigningKeyUnreadable || s.DestinationsUnreadable != 0 {
		t.Fatalf("under the right key: %+v", s)
	}
	// The same database under another key.
	st, err := bk.otherService(bkKey2).GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.KeyMismatch || !st.SigningKeyUnreadable || st.DestinationsUnreadable != 1 || !contains(st.Banners, backupapi.BannerKeyMismatch) ||
		!contains(st.Banners, backupapi.BannerSigningUnreadble) {
		t.Fatalf("under another key: %+v", st)
	}
	// No key at all.
	st, err = bk.otherService(nil).GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.EncryptionKeySet || !contains(st.Banners, backupapi.BannerKeyMissing) || st.DestinationsUnreadable != 1 {
		t.Fatalf("with no key: %+v", st)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// TestBackups_Runs_Changes: paging, filter, 404 and the fields a run never shows.
func TestBackups_Runs_Changes(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d1, d2 := bk.dest("one", "s3"), bk.dest("two", "webdav")
	ctx := context.Background()
	q := dbq.New(bk.pool)
	var ids []string
	for i := 0; i < 5; i++ {
		dest := d1.ID
		if i%2 == 1 {
			dest = d2.ID
		}
		trigger, status := "schedule", "succeeded"
		run, err := q.InsertBackupRun(ctx, dbq.InsertBackupRunParams{ID: newUUID(t), DestinationID: &dest, Trigger: trigger, Status: "queued", Attempt: 1,
			TestConfig: []byte(`{"secret":"` + bkTestSecret + `"}`), TestSecretsEnc: make([]byte, 40)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bk.pool.Exec(ctx, `UPDATE backup_runs SET status = $2, finished_at = now(), steps = '[{"name":"connect","ok":true}]' WHERE id = $1`, run.ID, status); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID.String())
	}
	var all []backupapi.Run
	cursor := ""
	for page := 0; page < 10; page++ {
		var p struct {
			Items      []backupapi.Run `json:"items"`
			NextCursor *string         `json:"nextCursor"`
		}
		path := bkp + "/runs?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		bk.must(200, bk.do("GET", path, bk.admin, nil)).json(t, &p)
		all = append(all, p.Items...)
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(all) != 5 {
		t.Fatalf("paged %d runs, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if !(all[i-1].QueuedAt.After(all[i].QueuedAt) || all[i-1].QueuedAt.Equal(all[i].QueuedAt)) {
			t.Fatalf("runs are not newest first at %d", i)
		}
	}
	var one struct {
		Items []backupapi.Run `json:"items"`
	}
	bk.must(200, bk.do("GET", bkp+"/runs?destination="+d2.ID.String(), bk.admin, nil)).json(t, &one)
	if len(one.Items) != 2 || one.Items[0].DestinationName != "two" {
		t.Fatalf("filter: %+v", one.Items)
	}
	// A run shows its steps and never test_config or test_secrets_enc.
	r := bk.must(200, bk.do("GET", bkp+"/runs/"+ids[0], bk.admin, nil))
	var got backupapi.Run
	r.json(t, &got)
	var steps []map[string]any
	if err := json.Unmarshal(got.Steps, &steps); err != nil || len(steps) != 1 || steps[0]["name"] != "connect" || steps[0]["ok"] != true {
		t.Fatalf("steps: %s", r.body)
	}
	for _, banned := range []string{"testConfig", "test_config", "testSecrets", "test_secrets", bkTestSecret} {
		if strings.Contains(string(r.body), banned) {
			t.Fatalf("a run shows %q: %s", banned, r.body)
		}
	}
	for _, bad := range []string{"limit=0", "limit=201", "cursor=zzz", "foo=1", "destination=nope"} {
		if r := bk.do("GET", bkp+"/runs?"+bad, bk.admin, nil); r.status != 400 {
			t.Errorf("runs?%s: %d", bad, r.status)
		}
	}
	if r := bk.do("GET", bkp+"/runs/"+newUUID(t).String(), bk.admin, nil); r.status != 404 {
		t.Errorf("unknown run: %d", r.status)
	}
	if r := bk.do("GET", bkp+"/runs/not-a-uuid", bk.admin, nil); r.status != 404 {
		t.Errorf("malformed run id: %d", r.status)
	}
	// Changes page the same way: two destinations were added.
	var ch struct {
		Items      []backupapi.ChangeEntry `json:"items"`
		NextCursor *string                 `json:"nextCursor"`
	}
	bk.must(200, bk.do("GET", bkp+"/changes?limit=1", bk.admin, nil)).json(t, &ch)
	if len(ch.Items) != 1 || ch.NextCursor == nil || ch.Items[0].Kind != "destination_added" {
		t.Fatalf("changes page: %+v", ch)
	}
	var next struct {
		Items      []backupapi.ChangeEntry `json:"items"`
		NextCursor *string                 `json:"nextCursor"`
	}
	bk.must(200, bk.do("GET", bkp+"/changes?limit=1&cursor="+*ch.NextCursor, bk.admin, nil)).json(t, &next)
	if len(next.Items) != 1 || next.NextCursor != nil || next.Items[0].ID == ch.Items[0].ID {
		t.Fatalf("second changes page: %+v", next)
	}
	if r := bk.do("GET", bkp+"/changes?destination="+d1.ID.String(), bk.admin, nil); r.status != 400 {
		t.Errorf("changes accepted a destination filter: %d", r.status)
	}
	// The destination view carries the last backup run.
	var d backupapi.Destination
	bk.must(200, bk.do("GET", bkp+"/destinations/"+d1.ID.String(), bk.admin, nil)).json(t, &d)
	if d.LastRun == nil || d.LastRun.Trigger != "schedule" {
		t.Fatalf("lastRun: %+v", d.LastRun)
	}
	_ = json.Marshal
	_ = platform.BackupColRunTestSecrets
	_ = time.Second
}
