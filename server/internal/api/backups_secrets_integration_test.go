//go:build backuptest

package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"devops/internal/backupapi"
)

// TestBackups_PlantedSecrets: every secret that goes in through the API is
// absent from the log, the events table, the idempotency table, every response
// and every row that is not a sealed column. No write emits an event.
func TestBackups_PlantedSecrets(t *testing.T) {
	bk := newBkEnv(t, bkOpts{types: []string{"s3", "webdav", "sftp", "dropbox"}})
	bk.ready()
	events := bk.countRows("events")
	var responses []string
	note := func(r resp) resp { responses = append(responses, string(r.body)); return r }

	s3 := bk.dest("one", "s3")
	note(bk.do("GET", bkp+"/destinations", bk.admin, nil))
	note(bk.patch(s3.ID, map[string]any{"secrets": map[string]any{"secretKey": bkSecretKey + "-2"}}))
	note(bk.patch(s3.ID, map[string]any{"enabled": true}))
	sf := bk.create(map[string]any{"name": "sf", "type": "sftp", "config": cfgFor("sf"),
		"secrets": map[string]any{"privateKey": bkPrivateKey}})
	note(sf)
	var sfd backupapi.Destination
	sf.json(t, &sfd)
	var dbx backupapi.Destination
	note(bk.create(map[string]any{"name": "dbx", "type": "dropbox", "config": cfgFor("dbx"), "secrets": secretsOf["dropbox"]})).json(t, &dbx)
	note(bk.act(bk.admin, "/destinations/"+dbx.ID.String()+"/exchange", map[string]any{"code": bkDropboxCode}))
	note(bk.act(bk.admin, "/destinations/test", map[string]any{"type": "s3", "config": cfgFor("form"),
		"secrets": map[string]any{"accessKey": "a", "secretKey": bkTestSecret}}))
	note(bk.act(bk.admin, "/destinations/"+s3.ID.String()+"/test", map[string]any{"confirmedFingerprint": "sha256:Pinned0123456789"}))
	// A refused write that carried secrets must not echo them either.
	note(bk.patch(s3.ID, map[string]any{"config": cfgFor("moved"), "secrets": map[string]any{"accessKey": bkAccessKey + "-3"}}))
	note(bk.create(map[string]any{"name": "one", "type": "s3", "config": cfgFor("zzz"), "secrets": map[string]any{"secretKey": bkSecretKey + "-4"}}))
	note(bk.act(bk.admin, "/destinations/"+dbx.ID.String()+"/exchange", map[string]any{"code": strings.Repeat("c", 3000)}))
	note(bk.do("GET", bkp+"/changes?limit=200", bk.admin, nil))
	note(bk.do("GET", bkp+"/runs", bk.admin, nil))
	note(bk.do("GET", bkp+"/status", bk.admin, nil))

	planted := []string{bkSecretKey, bkAccessKey, bkPassword, bkPrivateKey, bkAppSecret, bkDropboxCode, bkTestSecret}
	forms := func(p string) []string {
		return []string{p, fmt.Sprintf("%x", p), base64.StdEncoding.EncodeToString([]byte(p)), base64.RawURLEncoding.EncodeToString([]byte(p))}
	}
	check := func(where, hay string) {
		t.Helper()
		for _, p := range planted {
			for _, f := range forms(p) {
				if strings.Contains(hay, f) {
					t.Errorf("%s holds a planted secret (%s...)", where, p[:12])
				}
			}
		}
	}
	check("the log", bk.logs.String())
	for _, r := range responses {
		check("a response", r)
	}
	ctx := context.Background()
	// Every non-sealed column of every backup table, as text and hex; the sealed
	// columns are checked by their ciphertext not holding the plaintext.
	for _, tbl := range []string{"backup_settings", "backup_destinations", "backup_runs", "backup_requests", "backup_change_log", "events", "idempotency_keys"} {
		rows, err := bk.pool.Query(ctx, `SELECT row_to_json(x)::text || ' ' || encode(convert_to(row_to_json(x)::text,'UTF8'),'hex') FROM `+tbl+` x`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			check(tbl, s)
		}
		rows.Close()
	}
	// The sealed blobs are bytea: look at the raw bytes too.
	for _, q := range []string{`SELECT secrets_enc FROM backup_destinations WHERE secrets_enc IS NOT NULL`, `SELECT test_secrets_enc FROM backup_runs WHERE test_secrets_enc IS NOT NULL`} {
		rows, err := bk.pool.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				t.Fatal(err)
			}
			n++
			check("a sealed column", string(b))
		}
		rows.Close()
		if n == 0 {
			t.Errorf("no sealed rows for %s", q)
		}
	}
	// No write emitted an event (decisions-T16 #26).
	if got := bk.countRows("events"); got != events {
		t.Errorf("%d events were emitted", got-events)
	}
	// No Idempotency-Key row (idempotency.Reject).
	if n := bk.countRows("idempotency_keys"); n != 0 {
		t.Errorf("%d idempotency rows", n)
	}
	// The secrets do print redacted if they reach fmt.
	if s := fmt.Sprintf("%v %+v", backupapi.CreateDestinationInput{Secrets: nil}, sfd); strings.Contains(s, bkPrivateKey) {
		t.Error("a view prints a secret")
	}
}
