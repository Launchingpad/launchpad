//go:build backuptest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/backupapi"
	"devops/internal/platform"
)

func (bk *bkEnv) act(token, path string, body any) resp {
	return bk.do("POST", bkp+path, token, body)
}

func (bk *bkEnv) runRow(id any) (trigger, status string, cfg []byte, sealed []byte, dest *string, req *string) {
	bk.t.Helper()
	err := bk.pool.QueryRow(context.Background(),
		`SELECT trigger, status, test_config, test_secrets_enc, destination_id::text, request_id::text FROM backup_runs WHERE id = $1`, id).
		Scan(&trigger, &status, &cfg, &sealed, &dest, &req)
	if err != nil {
		bk.t.Fatal(err)
	}
	return
}

// Every queueing action only writes a row and wakes offsite.
func TestBackups_Actions_Queue(t *testing.T) {
	bk := newBkEnv(t, bkOpts{types: []string{"s3", "webdav", "sftp", "dropbox"}})
	bk.ready()
	d := bk.enabled("one", "s3")
	ctx := context.Background()
	conn, err := bk.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN backup_work`); err != nil {
		t.Fatal(err)
	}
	wake := func(what string) {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := conn.Conn().WaitForNotification(wctx); err != nil {
			t.Fatalf("%s: offsite was not woken: %v", what, err)
		}
	}
	before := bk.countRows("events")
	var run backupapi.Run

	bk.must(202, bk.act(bk.admin, "/destinations/"+d.ID.String()+"/probe", nil)).json(t, &run)
	tr, st, _, _, dst, _ := bk.runRow(run.ID)
	if tr != "probe" || st != "queued" || dst == nil || *dst != d.ID.String() || run.Status != "queued" {
		t.Fatalf("probe row: %s %s %v", tr, st, dst)
	}
	wake("probe")

	// A probe of an unsaved form keeps its form in test_config, with no secrets.
	bk.must(202, bk.act(bk.admin, "/destinations/probe", map[string]any{"type": "s3", "config": cfgFor("form")})).json(t, &run)
	tr, _, cfg, sealed, dst, _ := bk.runRow(run.ID)
	if tr != "probe" || dst != nil || sealed != nil || !strings.Contains(string(cfg), "form.example.com") {
		t.Fatalf("unsaved probe: %s %v %s", tr, dst, cfg)
	}
	wake("unsaved probe")
	if r := bk.act(bk.admin, "/destinations/probe", map[string]any{"type": "s3", "config": cfgFor("form"), "secrets": map[string]any{"x": "y"}}); r.status != 400 {
		t.Errorf("a probe with secrets: %d", r.status)
	}

	// The test of a saved destination.
	bk.must(202, bk.act(bk.admin, "/destinations/"+d.ID.String()+"/test", nil)).json(t, &run)
	if tr, _, _, _, _, _ = bk.runRow(run.ID); tr != "test" {
		t.Fatalf("test trigger %s", tr)
	}
	wake("test")

	// The test of an unsaved form: secrets sealed under the RUN id, never the destination's.
	bk.must(202, bk.act(bk.admin, "/destinations/test", map[string]any{"type": "s3", "config": cfgFor("form"),
		"secrets": map[string]any{"accessKey": "a-test", "secretKey": bkTestSecret}})).json(t, &run)
	_, _, cfg, sealed, dst, _ = bk.runRow(run.ID)
	if dst != nil || len(sealed) < 28 {
		t.Fatalf("unsaved test row: %v %d", dst, len(sealed))
	}
	if strings.Contains(string(sealed), bkTestSecret) || strings.Contains(string(cfg), bkTestSecret) {
		t.Fatal("test secret in clear")
	}
	if _, err := bk.keys.Open(platform.BackupColRunTestSecrets, run.ID.String(), sealed); err != nil {
		t.Fatalf("not sealed under the run id: %v", err)
	}
	if _, err := bk.keys.Open(platform.BackupColRunTestSecrets, newUUID(t).String(), sealed); err == nil {
		t.Fatal("the blob opens under another id")
	}
	if _, err := bk.keys.Open(platform.BackupColDestinationSecrets, run.ID.String(), sealed); err == nil {
		t.Fatal("the blob opens under another column")
	}
	wake("unsaved test")
	if bk.countRows("backup_runs") != 4 {
		t.Fatalf("%d runs", bk.countRows("backup_runs"))
	}
	// Run rows never carry the test columns on the wire.
	r := bk.do("GET", fmt.Sprintf("%s/runs/%s", bkp, run.ID), bk.admin, nil)
	if strings.Contains(string(r.body), "testConfig") || strings.Contains(string(r.body), "test_secrets") {
		t.Fatalf("the run view shows test columns: %s", r.body)
	}
	if got := bk.countRows("events"); got != before {
		t.Fatalf("actions emitted %d events", got-before)
	}
}

func TestBackups_Actions_Unavailable(t *testing.T) {
	// No key: 503 backup_unavailable for every action that dials; unsupported type: 503.
	bk := newBkEnv(t, bkOpts{noKey: true})
	d := bk.create(map[string]any{"name": "a", "type": "s3", "config": cfgFor("a")})
	var dd backupapi.Destination
	bk.must(201, d).json(t, &dd)
	id := dd.ID.String()
	for _, p := range []string{"/destinations/" + id + "/probe", "/destinations/" + id + "/test", "/destinations/" + id + "/run"} {
		if r := bk.act(bk.admin, p, nil); r.status != 503 || r.errCode(t) != "backup_unavailable" {
			t.Errorf("%s without a key: %d %s", p, r.status, r.body)
		}
	}
	for _, p := range []string{"/destinations/probe", "/destinations/test"} {
		if r := bk.act(bk.admin, p, map[string]any{"type": "s3", "config": cfgFor("a")}); r.status != 503 {
			t.Errorf("%s without a key: %d", p, r.status)
		}
	}
	if r := bk.act(bk.admin, "/destinations/"+id+"/exchange", map[string]any{"code": "c"}); r.status != 503 {
		t.Errorf("exchange without a key: %d", r.status)
	}
	// An unknown destination is 404 first.
	if r := bk.act(bk.admin, "/destinations/"+newUUID(t).String()+"/probe", nil); r.status != 404 {
		t.Errorf("unknown destination: %d", r.status)
	}
	if bk.countRows("backup_runs") != 0 {
		t.Fatal("a refused action queued a run")
	}

}

func TestBackups_Actions_UnsupportedType(t *testing.T) {
	// A key, but no driver for the type: 503; a saved row of that type exists.
	bk2 := newBkEnv(t, bkOpts{types: []string{"webdav"}})
	if _, err := bk2.pool.Exec(context.Background(), `INSERT INTO backup_destinations (name, type, config) VALUES ('legacy','s3','{}')`); err != nil {
		t.Fatal(err)
	}
	var l backupapi.DestinationList
	bk2.must(200, bk2.do("GET", bkp+"/destinations", bk2.admin, nil)).json(t, &l)
	if len(l.Items) != 1 || l.Items[0].TypeSupported {
		t.Fatalf("list: %+v", l)
	}
	for _, p := range []string{"probe", "test", "run"} {
		if r := bk2.act(bk2.admin, "/destinations/"+l.Items[0].ID.String()+"/"+p, nil); r.status != 503 || r.errCode(t) != "backup_unavailable" {
			t.Errorf("%s on an unsupported type: %d %s", p, r.status, r.body)
		}
	}
	if r := bk2.act(bk2.admin, "/destinations/probe", map[string]any{"type": "s3", "config": cfgFor("a")}); r.status != 503 {
		t.Errorf("unsaved probe of an unsupported type: %d", r.status)
	}
	if r := bk2.act(bk2.admin, "/destinations/probe", map[string]any{"type": "ftp", "config": cfgFor("a")}); r.status != 400 {
		t.Errorf("unknown type: %d", r.status)
	}
}

func TestBackups_Actions_ConfirmedFingerprint(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	sf := bk.dest("sf", "sftp")
	id := d.ID.String()
	fp := "sha256:AbCdEf0123456789"
	// A bad fingerprint is 400 and queues nothing.
	if r := bk.act(bk.admin, "/destinations/"+id+"/test", map[string]any{"confirmedFingerprint": "x y"}); r.status != 400 {
		t.Fatalf("bad fingerprint: %d", r.status)
	}
	if bk.countRows("backup_runs") != 0 {
		t.Fatal("queued")
	}
	// The first pin is saved with a change row, before the run.
	bk.must(202, bk.act(bk.admin, "/destinations/"+id+"/test", map[string]any{"confirmedFingerprint": fp}))
	got := bk.getDest(d.ID)
	if !strings.Contains(string(got.Config), fp) || !strings.Contains(string(got.Config), `"tlsPin"`) {
		t.Fatalf("pin not saved: %s", got.Config)
	}
	if last := bk.changes()[0]; last.Kind != "destination_pin_set" {
		t.Fatalf("last change %s", last.Kind)
	}
	// Again with the same value: no second row. Another value: 400 and no run.
	runs := bk.countRows("backup_runs")
	bk.must(202, bk.act(bk.admin, "/destinations/"+id+"/test", map[string]any{"confirmedFingerprint": fp}))
	if r := bk.act(bk.admin, "/destinations/"+id+"/test", map[string]any{"confirmedFingerprint": "sha256:Different0123"}); r.status != 400 {
		t.Fatalf("a different pin: %d %s", r.status, r.body)
	}
	if bk.countRows("backup_runs") != runs+1 {
		t.Fatalf("runs %d", bk.countRows("backup_runs"))
	}
	if n := strings.Count(strings.Join(bk.kinds(), ","), "destination_pin_set"); n != 1 {
		t.Fatalf("%d pin rows", n)
	}
	// SFTP saves a host key.
	bk.must(202, bk.act(bk.admin, "/destinations/"+sf.ID.String()+"/test", map[string]any{"confirmedFingerprint": fp}))
	if g := bk.getDest(sf.ID); !strings.Contains(string(g.Config), `"hostKey"`) {
		t.Fatalf("host key not saved: %s", g.Config)
	}
	if last := bk.changes()[0]; last.Kind != "destination_host_key_set" {
		t.Fatalf("last change %s", last.Kind)
	}
	// The saved secrets stay readable after a pin save.
	if bk.getDest(d.ID).SecretsUnreadable {
		t.Fatal("the pin save broke the secrets")
	}
	// A pin on an unsaved test goes into the form, and is never written to a destination.
	bk.must(202, bk.act(bk.admin, "/destinations/test", map[string]any{"type": "s3", "config": cfgFor("form"),
		"secrets": secretsOf["s3"], "confirmedFingerprint": fp}))
}

func TestBackups_Actions_UseSavedSecrets(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	d := bk.dest("one", "s3")
	body := func(m map[string]any) map[string]any {
		b := map[string]any{"type": "s3", "config": cfgFor("one"), "useSavedSecretsFrom": d.ID.String()}
		for k, v := range m {
			b[k] = v
		}
		return b
	}
	var run backupapi.Run
	bk.must(202, bk.act(bk.admin, "/destinations/test", body(nil))).json(t, &run)
	_, _, _, sealed, _, _ := bk.runRow(run.ID)
	raw, err := bk.keys.Open(platform.BackupColRunTestSecrets, run.ID.String(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || m["secretKey"] != bkSecretKey || m["accessKey"] != bkAccessKey {
		t.Fatalf("saved secrets not copied: %v", err)
	}
	// An entered secret wins for its name.
	bk.must(202, bk.act(bk.admin, "/destinations/test", body(map[string]any{"secrets": map[string]any{"accessKey": "typed"}}))).json(t, &run)
	_, _, _, sealed, _, _ = bk.runRow(run.ID)
	raw, _ = bk.keys.Open(platform.BackupColRunTestSecrets, run.ID.String(), sealed)
	_ = json.Unmarshal(raw, &m)
	if m["accessKey"] != "typed" || m["secretKey"] != bkSecretKey {
		t.Fatalf("merge: %v", m)
	}
	// A changed config must not borrow the saved secrets.
	other := cfgFor("elsewhere")
	if r := bk.act(bk.admin, "/destinations/test", body(map[string]any{"config": other})); r.status != 400 {
		t.Fatalf("saved secrets to a new config: %d %s", r.status, r.body)
	}
	// Another type, an unknown id, unreadable secrets.
	if r := bk.act(bk.admin, "/destinations/test", body(map[string]any{"type": "webdav"})); r.status != 400 {
		t.Errorf("another type: %d", r.status)
	}
	if r := bk.act(bk.admin, "/destinations/test", body(map[string]any{"useSavedSecretsFrom": newUUID(t).String()})); r.status != 400 {
		t.Errorf("unknown id: %d", r.status)
	}
	bk.corruptSecrets(d.ID)
	if r := bk.act(bk.admin, "/destinations/test", body(nil)); r.status != 409 {
		t.Errorf("unreadable saved secrets: %d", r.status)
	}
	// Dropbox cannot be tested unsaved.
	if r := bk.act(bk.admin, "/destinations/test", map[string]any{"type": "dropbox", "config": cfgFor("d")}); r.status != 400 {
		t.Errorf("unsaved dropbox test: %d", r.status)
	}
}

func TestBackups_Actions_Exchange(t *testing.T) {
	bk := newBkEnv(t, bkOpts{types: []string{"s3", "webdav", "sftp", "dropbox"}})
	s3 := bk.dest("one", "s3")
	if r := bk.act(bk.admin, "/destinations/"+s3.ID.String()+"/exchange", map[string]any{"code": bkDropboxCode}); r.status != 400 {
		t.Fatalf("exchange on s3: %d", r.status)
	}
	// A Dropbox destination without its app key and secret.
	var d backupapi.Destination
	bk.must(201, bk.create(map[string]any{"name": "dbx", "type": "dropbox", "config": cfgFor("dbx")})).json(t, &d)
	id := d.ID.String()
	if r := bk.act(bk.admin, "/destinations/"+id+"/exchange", map[string]any{"code": bkDropboxCode}); r.status != 409 {
		t.Fatalf("no app key: %d %s", r.status, r.body)
	}
	bk.must(200, bk.patch(d.ID, map[string]any{"secrets": secretsOf["dropbox"]}))
	if r := bk.act(bk.admin, "/destinations/"+id+"/exchange", map[string]any{}); r.status != 400 {
		t.Fatalf("no code: %d", r.status)
	}
	var run backupapi.Run
	bk.must(202, bk.act(bk.admin, "/destinations/"+id+"/exchange", map[string]any{"code": bkDropboxCode})).json(t, &run)
	tr, _, cfg, sealed, _, _ := bk.runRow(run.ID)
	if tr != "exchange" || strings.Contains(string(cfg), bkDropboxCode) || strings.Contains(string(sealed), bkDropboxCode) {
		t.Fatalf("exchange row: %s", tr)
	}
	raw, err := bk.keys.Open(platform.BackupColRunTestSecrets, run.ID.String(), sealed)
	if err != nil || !strings.Contains(string(raw), bkDropboxCode) || !strings.Contains(string(raw), bkAppSecret) {
		t.Fatalf("sealed content: %v", err)
	}
}

func TestBackups_Actions_Run(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	d := bk.dest("one", "s3")
	id := d.ID.String()
	// Disabled: 409.
	if r := bk.act(bk.admin, "/destinations/"+id+"/run", nil); r.status != 409 {
		t.Fatalf("disabled: %d", r.status)
	}
	bk.must(200, bk.patch(d.ID, map[string]any{"enabled": true}))
	var run backupapi.Run
	bk.must(202, bk.act(bk.admin, "/destinations/"+id+"/run", nil)).json(t, &run)
	tr, st, _, _, _, req := bk.runRow(run.ID)
	if tr != "manual" || st != "queued" || req == nil {
		t.Fatalf("manual row: %s %s %v", tr, st, req)
	}
	if bk.countRows("backup_requests") != 1 {
		t.Fatal("no request row")
	}
	// A second open run is destination_busy.
	if r := bk.act(bk.admin, "/destinations/"+id+"/run", nil); r.status != 409 || r.errCode(t) != "destination_busy" {
		t.Fatalf("busy: %d %s", r.status, r.body)
	}
	if bk.countRows("backup_requests") != 1 {
		t.Fatal("a refused run left a request row")
	}
	// A queued RETRY is cancelled by a manual run; a claimed one is not.
	mustExec := func(q string, a ...any) {
		t.Helper()
		if _, err := bk.pool.Exec(context.Background(), q, a...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`UPDATE backup_runs SET status = 'succeeded', started_at = now(), finished_at = now() WHERE id = $1`, run.ID)
	retry := newUUID(t)
	mustExec(`INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, attempt) VALUES ($1,$2,'one','retry','queued',2)`, retry, d.ID)
	bk.must(202, bk.act(bk.admin, "/destinations/"+id+"/run", nil)).json(t, &run)
	var rs string
	if err := bk.pool.QueryRow(context.Background(), `SELECT status FROM backup_runs WHERE id = $1`, retry).Scan(&rs); err != nil || rs != "canceled" {
		t.Fatalf("queued retry: %s %v", rs, err)
	}
	mustExec(`UPDATE backup_runs SET status = 'succeeded', started_at = now(), finished_at = now() WHERE id = $1`, run.ID)
	retry2 := newUUID(t)
	mustExec(`INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, attempt, started_at) VALUES ($1,$2,'one','retry','running',2, now())`, retry2, d.ID)
	if r := bk.act(bk.admin, "/destinations/"+id+"/run", nil); r.status != 409 || r.errCode(t) != "destination_busy" {
		t.Fatalf("running retry: %d %s", r.status, r.body)
	}
}

func TestBackups_Actions_RunRace(t *testing.T) {
	bk := newBkEnv(t, bkOpts{})
	bk.ready()
	d := bk.enabled("one", "s3")
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = bk.act(bk.admin, "/destinations/"+d.ID.String()+"/run", nil).status
		}()
	}
	wg.Wait()
	ok, busy := 0, 0
	for _, c := range codes {
		switch c {
		case 202:
			ok++
		case 409:
			busy++
		}
	}
	if ok != 1 || busy != 5 {
		t.Fatalf("codes %v", codes)
	}
	if bk.countRows("backup_runs") != 1 || bk.countRows("backup_requests") != 1 {
		t.Fatalf("rows: %d runs, %d requests", bk.countRows("backup_runs"), bk.countRows("backup_requests"))
	}
}

func TestBackups_Actions_RateLimit(t *testing.T) {
	bk := newBkEnv(t, bkOpts{perMinute: 6, burst: 3})
	d := bk.dest("one", "s3")
	// Bad requests spend no token: 404, 400 and 503-less refusals come first.
	for i := 0; i < 8; i++ {
		if r := bk.act(bk.admin, "/destinations/probe", map[string]any{"type": "ftp", "config": cfgFor("x")}); r.status != 400 {
			t.Fatalf("bad type: %d", r.status)
		}
		if r := bk.act(bk.admin, "/destinations/"+newUUID(t).String()+"/probe", nil); r.status != 404 {
			t.Fatalf("unknown destination: %d", r.status)
		}
		if r := bk.act(bk.admin, "/destinations/"+d.ID.String()+"/run", nil); r.status != 409 { // disabled
			t.Fatalf("disabled run: %d", r.status)
		}
	}
	var limited resp
	n := 0
	for i := 0; i < 10; i++ {
		r := bk.act(bk.admin, "/destinations/probe", map[string]any{"type": "s3", "config": cfgFor(fmt.Sprint("f", i))})
		if r.status == 429 {
			limited = r
			break
		}
		if r.status != 202 {
			t.Fatalf("call %d: %d %s", i, r.status, r.body)
		}
		n++
	}
	if n < 3 || n > 4 || limited.status != 429 || limited.header.Get("Retry-After") == "" {
		t.Fatalf("burst %d, limited %d, Retry-After %q", n, limited.status, limited.header.Get("Retry-After"))
	}
	// Reads are not limited; the limit is shared by every action.
	bk.must(200, bk.do("GET", bkp+"/status", bk.admin, nil))
	if r := bk.act(bk.admin, "/destinations/"+d.ID.String()+"/test", nil); r.status != 429 {
		t.Fatalf("test after the limit: %d", r.status)
	}
}
