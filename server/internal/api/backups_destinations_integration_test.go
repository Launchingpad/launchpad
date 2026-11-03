//go:build backuptest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"devops/internal/backup/driver"
	"devops/internal/backupapi"
	"devops/internal/platform"
)

func (bk *bkEnv) create(body map[string]any) resp {
	return bk.do("POST", bkp+"/destinations", bk.admin, body)
}

func (bk *bkEnv) getDest(id any) backupapi.Destination {
	bk.t.Helper()
	var d backupapi.Destination
	bk.must(200, bk.do("GET", fmt.Sprintf("%s/destinations/%v", bkp, id), bk.admin, nil)).json(bk.t, &d)
	return d
}

func (bk *bkEnv) corruptSecrets(id any) {
	bk.t.Helper()
	if _, err := bk.pool.Exec(context.Background(), `UPDATE backup_destinations SET secrets_enc = decode(repeat('ab', 40), 'hex') WHERE id = $1`, id); err != nil {
		bk.t.Fatal(err)
	}
}

func TestBackups_Destinations_Create(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	if d.Enabled || d.EnabledSince != nil || !d.TypeSupported || d.Schedule != "daily" || d.AtUTC != "04:15" || d.KeepLast != 14 ||
		d.VerifyEveryDays != 7 || strings.Join(d.SecretsSet, ",") != "accessKey,secretKey" || d.SecretsUnreadable {
		t.Fatalf("created: %+v", d)
	}
	// The secrets are sealed in the row and never returned.
	var raw string
	if err := bk.pool.QueryRow(context.Background(), `SELECT encode(secrets_enc,'hex') FROM backup_destinations WHERE id=$1`, d.ID).Scan(&raw); err != nil || raw == "" {
		t.Fatalf("no sealed secrets: %v", err)
	}
	for _, p := range []string{bkAccessKey, bkSecretKey} {
		if strings.Contains(raw, fmt.Sprintf("%x", p)) {
			t.Fatal("a secret is in the row in clear")
		}
	}
	r := bk.do("GET", bkp+"/destinations", bk.admin, nil)
	if strings.Contains(string(r.body), bkSecretKey) || strings.Contains(string(r.body), bkAccessKey) {
		t.Fatal("the list returns a secret")
	}
	var chk []byte
	if err := bk.pool.QueryRow(context.Background(), `SELECT key_check FROM backup_settings`).Scan(&chk); err != nil || len(chk) == 0 {
		t.Fatalf("key_check not recorded: %v", err)
	}
	if got := strings.Join(bk.kinds(), ","); got != "destination_added" {
		t.Fatalf("kinds %s", got)
	}
	// Refusals.
	cases := map[string]map[string]any{
		"no name":            {"type": "s3", "config": cfgFor("x")},
		"no type":            {"name": "x", "config": cfgFor("x")},
		"unknown type":       {"name": "x", "type": "ftp", "config": cfgFor("x")},
		"bad config":         {"name": "x", "type": "s3", "config": map[string]any{"nope": 1}},
		"empty name":         {"name": "  ", "type": "s3", "config": cfgFor("x")},
		"unknown secret":     {"name": "x", "type": "s3", "config": cfgFor("x"), "secrets": map[string]any{"token": "a"}},
		"weekday on daily":   {"name": "x", "type": "s3", "config": cfgFor("x"), "weekday": 2},
		"weekly no day":      {"name": "x", "type": "s3", "config": cfgFor("x"), "schedule": "weekly"},
		"weekly day 7":       {"name": "x", "type": "s3", "config": cfgFor("x"), "schedule": "weekly", "weekday": 7},
		"bad time":           {"name": "x", "type": "s3", "config": cfgFor("x"), "atUtc": "24:00"},
		"keep 0":             {"name": "x", "type": "s3", "config": cfgFor("x"), "keepLast": 0},
		"keep 366":           {"name": "x", "type": "s3", "config": cfgFor("x"), "keepLast": 366},
		"keep 1 unconfirmed": {"name": "x", "type": "s3", "config": cfgFor("x"), "keepLast": 1},
		"verify 91":          {"name": "x", "type": "s3", "config": cfgFor("x"), "verifyEveryDays": 91},
	}
	for name, body := range cases {
		if r := bk.create(body); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// Accepted: keepLast 1 confirmed, weekly.
	bk.must(201, bk.create(map[string]any{"name": "two", "type": "webdav", "config": cfgFor("two"), "keepLast": 1, "confirmLowKeep": true,
		"schedule": "weekly", "weekday": 0, "atUtc": "00:00", "verifyEveryDays": 0}))
	// 409s: the same name, the same identity, an unsupported type, secrets with no key.
	if r := bk.create(map[string]any{"name": "one", "type": "s3", "config": cfgFor("other")}); r.status != 409 {
		t.Errorf("same name: %d", r.status)
	}
	dup := cfgFor("one")
	dup["url"] = "https://ONE.example.com"
	if r := bk.create(map[string]any{"name": "dup", "type": "s3", "config": dup}); r.status != 409 {
		t.Errorf("same identity: %d %s", r.status, r.body)
	}
	if r := bk.create(map[string]any{"name": "dbx", "type": "dropbox", "config": map[string]any{}}); r.status != 409 {
		t.Errorf("unsupported type: %d %s", r.status, r.body)
	}
	// The same bucket on another type is fine.
	bk.must(201, bk.create(map[string]any{"name": "sf", "type": "sftp", "config": cfgFor("one")}))
	if n := bk.countRows("backup_destinations"); n != 3 {
		t.Errorf("%d destinations", n)
	}
	// Read one, unknown, bad id.
	if r := bk.do("GET", bkp+"/destinations/"+newUUID(t).String(), bk.admin, nil); r.status != 404 {
		t.Errorf("unknown id: %d", r.status)
	}
	if r := bk.do("GET", bkp+"/destinations/nope", bk.admin, nil); r.status != 404 {
		t.Errorf("bad id: %d", r.status)
	}
}

func TestBackups_Destinations_NoKey(t *testing.T) {
	bk := newBkEnv(t, bkOpts{noKey: true})
	if r := bk.create(map[string]any{"name": "a", "type": "s3", "config": cfgFor("a"), "secrets": secretsOf["s3"]}); r.status != 409 {
		t.Fatalf("secrets without a key: %d %s", r.status, r.body)
	}
	// With no secrets it works, and enabling is refused.
	var d backupapi.Destination
	bk.must(201, bk.create(map[string]any{"name": "a", "type": "s3", "config": cfgFor("a")})).json(t, &d)
	if r := bk.patch(d.ID, map[string]any{"enabled": true}); r.status != 409 {
		t.Fatalf("enable without a key: %d", r.status)
	}
	if r := bk.patch(d.ID, map[string]any{"secrets": map[string]any{"accessKey": "x"}}); r.status != 409 {
		t.Fatalf("secret write without a key: %d", r.status)
	}
}

func TestBackups_Destinations_Patch(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	// Rename, schedule, retention: one row each.
	bk.must(200, bk.patch(d.ID, map[string]any{"name": "uno"}))
	bk.must(200, bk.patch(d.ID, map[string]any{"schedule": "weekly", "weekday": 3, "atUtc": "02:30"}))
	bk.must(200, bk.patch(d.ID, map[string]any{"keepLast": 30, "verifyEveryDays": 0}))
	bk.must(200, bk.patch(d.ID, map[string]any{"name": "uno"})) // no change, no row
	if got := strings.Join(bk.kinds(), ","); got != "destination_added,destination_renamed,destination_schedule_changed,destination_retention_changed" {
		t.Fatalf("kinds %s", got)
	}
	// Back to daily clears the weekday; weekday null does the same.
	var got backupapi.Destination
	bk.must(200, bk.patch(d.ID, map[string]any{"schedule": "daily"})).json(t, &got)
	if got.Weekday != nil {
		t.Fatalf("weekday kept on daily: %+v", got)
	}
	if r := bk.patch(d.ID, map[string]any{"keepLast": 1}); r.status != 400 {
		t.Errorf("keepLast 1 unconfirmed: %d", r.status)
	}
	bk.must(200, bk.patch(d.ID, map[string]any{"keepLast": 1, "confirmLowKeep": true}))
	if r := bk.patch(d.ID, map[string]any{"schedule": "weekly"}); r.status != 400 {
		t.Errorf("weekly with no weekday: %d", r.status)
	}
	if r := bk.patch(d.ID, map[string]any{"name": ""}); r.status != 400 {
		t.Errorf("empty name: %d", r.status)
	}
	other := bk.dest("two", "webdav")
	if r := bk.patch(other.ID, map[string]any{"name": "uno"}); r.status != 409 {
		t.Errorf("name taken: %d", r.status)
	}
	if r := bk.patch(newUUID(t), map[string]any{"name": "x"}); r.status != 404 {
		t.Errorf("unknown: %d", r.status)
	}
	// Delete: the change row keeps the name, the run table keeps destination_name.
	if r := bk.do("DELETE", fmt.Sprintf("%s/destinations/%s", bkp, other.ID), bk.admin, nil); r.status != 204 {
		t.Fatalf("delete: %d", r.status)
	}
	if r := bk.do("DELETE", fmt.Sprintf("%s/destinations/%s", bkp, other.ID), bk.admin, nil); r.status != 404 {
		t.Fatalf("second delete: %d", r.status)
	}
	last := bk.changes()[0]
	if last.Kind != "destination_deleted" || last.DestinationName != "two" || last.DestinationID != nil {
		t.Fatalf("delete row: %+v", last)
	}
}

// The no-host-change rule (decisions-T16 #25): a stored secret never follows a
// config change unless the same request enters it again.
func TestBackups_Destinations_KeptSecretConfigChange(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	moved := cfgFor("evil")
	for name, body := range map[string]map[string]any{
		"config only":          {"config": moved},
		"one of two secrets":   {"config": moved, "secrets": map[string]any{"accessKey": "new-access"}},
		"null keeps":           {"config": moved, "secrets": map[string]any{"accessKey": nil, "secretKey": nil}},
		"one cleared one kept": {"config": moved, "secrets": map[string]any{"accessKey": ""}},
	} {
		if r := bk.patch(d.ID, body); r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	if got := bk.getDest(d.ID); string(got.Config) == "" || strings.Contains(string(got.Config), "evil") {
		t.Fatalf("config moved: %s", got.Config)
	}
	// Both entered again: allowed, two change rows.
	bk.must(200, bk.patch(d.ID, map[string]any{"config": moved, "secrets": map[string]any{"accessKey": "new-access", "secretKey": "new-secret"}}))
	if got := strings.Join(bk.kinds(), ","); got != "destination_added,destination_config_changed,destination_secrets_changed" {
		t.Fatalf("kinds %s", got)
	}
	// A config that changes nothing is not a change.
	bk.must(200, bk.patch(d.ID, map[string]any{"config": moved}))
	// A destination with no secrets may move freely.
	e := bk.create(map[string]any{"name": "free", "type": "webdav", "config": cfgFor("free")})
	var free backupapi.Destination
	bk.must(201, e).json(t, &free)
	bk.must(200, bk.patch(free.ID, map[string]any{"config": cfgFor("free2")}))
	// A move onto another destination's identity is 409.
	if r := bk.patch(free.ID, map[string]any{"config": cfgFor("free2"), "name": "free"}); r.status != 200 {
		t.Errorf("no-op config: %d", r.status)
	}
	third := bk.create(map[string]any{"name": "third", "type": "webdav", "config": cfgFor("third")})
	bk.must(201, third)
	if r := bk.patch(free.ID, map[string]any{"config": cfgFor("third")}); r.status != 409 {
		t.Errorf("identity clash on move: %d %s", r.status, r.body)
	}
}

func TestBackups_Destinations_SecretSemantics(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	// null and absent keep.
	bk.must(200, bk.patch(d.ID, map[string]any{"secrets": map[string]any{"accessKey": nil}}))
	if got := bk.getDest(d.ID); strings.Join(got.SecretsSet, ",") != "accessKey,secretKey" {
		t.Fatalf("null did not keep: %v", got.SecretsSet)
	}
	// A value replaces; one row, names only.
	bk.must(200, bk.patch(d.ID, map[string]any{"secrets": map[string]any{"accessKey": "replaced-value"}}))
	// "" clears.
	bk.must(200, bk.patch(d.ID, map[string]any{"secrets": map[string]any{"secretKey": ""}}))
	if got := bk.getDest(d.ID); strings.Join(got.SecretsSet, ",") != "accessKey" {
		t.Fatalf("clear: %v", got.SecretsSet)
	}
	for _, c := range bk.changes() {
		if c.Kind == "destination_secrets_changed" && strings.Contains(string(c.Detail), "replaced-value") {
			t.Fatal("the change log holds a secret value")
		}
	}
	// A wrong name for the type is 400.
	if r := bk.patch(d.ID, map[string]any{"secrets": map[string]any{"password": "x"}}); r.status != 400 {
		t.Errorf("wrong secret name: %d", r.status)
	}
	// A refreshToken cannot be written through the API.
	dbx := bk.do("PATCH", bkp+"/destinations/"+d.ID.String(), bk.admin, map[string]any{"secrets": map[string]any{"refreshToken": "x"}})
	if dbx.status != 400 {
		t.Errorf("refreshToken: %d", dbx.status)
	}
}

func TestBackups_Destinations_Enable(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	enable := func() resp { return bk.patch(d.ID, map[string]any{"enabled": true}) }
	// No signing key.
	if r := enable(); r.status != 409 {
		t.Fatalf("no signing key: %d", r.status)
	}
	bk.signing()
	// No recipient.
	if r := enable(); r.status != 409 {
		t.Fatalf("no recipient: %d", r.status)
	}
	bk.addRecipient(bk.admin)
	second := bk.addRecipient(bk.admin)
	_ = second
	// One active, one pending: fine. Pending only is not enough: tested below.
	// Missing secrets.
	bare := bk.create(map[string]any{"name": "bare", "type": "s3", "config": cfgFor("bare")})
	var b backupapi.Destination
	bk.must(201, bare).json(t, &b)
	if r := bk.patch(b.ID, map[string]any{"enabled": true}); r.status != 409 {
		t.Fatalf("missing secrets: %d", r.status)
	}
	// Unreadable secrets.
	bk.corruptSecrets(d.ID)
	if got := bk.getDest(d.ID); !got.SecretsUnreadable {
		t.Fatal("not flagged unreadable")
	}
	if r := enable(); r.status != 409 {
		t.Fatalf("unreadable secrets: %d", r.status)
	}
	// Re-entering both secrets recovers it; the other stays unreadable.
	e := bk.dest("two", "s3")
	bk.corruptSecrets(e.ID)
	bk.must(200, bk.patch(d.ID, map[string]any{"secrets": secretsOf["s3"]}))
	if bk.getDest(d.ID).SecretsUnreadable || !bk.getDest(e.ID).SecretsUnreadable {
		t.Fatal("recovery touched the wrong destination")
	}
	var on backupapi.Destination
	bk.must(200, enable()).json(t, &on)
	if !on.Enabled || on.EnabledSince == nil {
		t.Fatalf("not enabled: %+v", on)
	}
	// The stored blob dropped the old unreadable data, and new secret writes on an
	// enabled destination may not leave it without a required one.
	if r := bk.patch(d.ID, map[string]any{"secrets": map[string]any{"secretKey": ""}}); r.status != 409 {
		t.Fatalf("cleared a required secret of an enabled destination: %d", r.status)
	}
	// Disable and the row.
	bk.must(200, bk.patch(d.ID, map[string]any{"enabled": false}))
	got := strings.Join(bk.kinds(), ",")
	if !strings.HasSuffix(got, "destination_enabled,destination_disabled") {
		t.Fatalf("kinds %s", got)
	}
}

func TestBackups_Destinations_EnableChecks(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.signing()
	a := bk.addRecipient(bk.admin)
	d := bk.dest("one", "s3")
	// Empty the set (no destination is enabled), then add one: it is held, so
	// there is no ACTIVE recipient and enabling is refused.
	bk.must(200, bk.del(bk.admin, a.Fingerprint))
	bk.clock.Advance(24 * time.Hour)
	bk.addRecipient(bk.admin)
	if r := bk.patch(d.ID, map[string]any{"enabled": true}); r.status != 409 {
		t.Fatalf("pending-only recipients: %d %s", r.status, r.body)
	}
	bk.clock.Advance(24 * time.Hour)
	bk.must(200, bk.patch(d.ID, map[string]any{"enabled": true}))
	bk.must(200, bk.patch(d.ID, map[string]any{"enabled": false}))
	// A signing key this server cannot open blocks enabling. Replace it with one
	// sealed under another key (the trigger wants a change-log row in the same tx).
	k2, err := platform.NewBackupKeyring(bkKey2)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := k2.Seal(platform.BackupColSigningKey, platform.BackupSettingsOwner, []byte("other-signing-key"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := bk.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `INSERT INTO backup_change_log (kind) VALUES ('signing_key_replaced')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `UPDATE backup_settings SET signing_private_enc = $1`, blob); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := bk.patch(d.ID, map[string]any{"enabled": true}); r.status != 409 {
		t.Fatalf("unreadable signing key: %d %s", r.status, r.body)
	}
	if st := bk.status(); !st.SigningKeyUnreadable {
		t.Fatalf("status: %+v", st)
	}
}

// echoDriver fails with the config value in its error text. The API must drop it.
type echoDriver struct {
	fakeDriver
	failValidate, failIdentity bool
}

func (d echoDriver) Validate(raw json.RawMessage) error {
	if d.failValidate {
		return fmt.Errorf("bad config %s", raw)
	}
	return nil
}

func (d echoDriver) Identity(raw json.RawMessage) (driver.Identity, error) {
	if d.failIdentity {
		return driver.Identity{}, fmt.Errorf("bad identity %s", raw)
	}
	return d.fakeDriver.Identity(raw)
}

func TestBackups_Destinations_DriverErrorsNotEchoed(t *testing.T) {
	const planted = "planted-config-value-4d9e1b"
	bk := newBkEnv(t, bkOpts{types: []string{"sftp"}})
	driver.RegisterForTest(t, "s3", echoDriver{fakeDriver: fakeDriver{typ: "s3"}, failValidate: true})
	driver.RegisterForTest(t, "webdav", echoDriver{fakeDriver: fakeDriver{typ: "webdav"}, failIdentity: true})
	cfg := map[string]any{"url": "https://h.example.com", "bucket": planted}
	var resps []resp
	// Validate fails: create, unsaved probe, unsaved test.
	resps = append(resps,
		bk.create(map[string]any{"name": "a", "type": "s3", "config": cfg}),
		bk.act(bk.admin, "/destinations/probe", map[string]any{"type": "s3", "config": cfg}),
		bk.act(bk.admin, "/destinations/test", map[string]any{"type": "s3", "config": cfg, "secrets": secretsOf["s3"]}))
	// Identity fails (Validate passes): create and PATCH of a destination with no secrets.
	resps = append(resps, bk.create(map[string]any{"name": "w", "type": "webdav", "config": cfg}))
	if _, err := bk.pool.Exec(context.Background(), `INSERT INTO backup_destinations (name, type, config) VALUES ('w2','webdav','{"url":"https://ok.example.com"}')`); err != nil {
		t.Fatal(err)
	}
	var l backupapi.DestinationList
	bk.must(200, bk.do("GET", bkp+"/destinations", bk.admin, nil)).json(t, &l)
	resps = append(resps, bk.patch(l.Items[0].ID, map[string]any{"config": cfg}))
	for i, r := range resps {
		if r.status != 400 {
			t.Errorf("case %d: status %d %s", i, r.status, r.body)
		}
		if strings.Contains(string(r.body), planted) {
			t.Errorf("case %d: the 400 text echoes the config value: %s", i, r.body)
		}
	}
	if strings.Contains(bk.logs.String(), planted) {
		t.Error("the log echoes the config value")
	}
}
