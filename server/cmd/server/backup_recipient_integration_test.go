package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/backup"
	"devops/internal/backupapi"
	"devops/internal/testdb"
)

// `server backup-recipient-remove` and `-cancel` against a real database
// (INTEGRATION=1): they act at once, write a change-log row with no actor, and
// refuse to empty the set while a destination is enabled.
func TestBackupRecipient_Commands(t *testing.T) {
	pool, _ := testdb.New(t)
	testdb.Fixture(t, pool)
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username = 'chen'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	p := &auth.Principal{UserID: uid, Username: "chen"}
	svc, err := backupapi.New(pool, nil, backupapi.Options{})
	if err != nil {
		t.Fatal(err)
	}
	add := func() string {
		t.Helper()
		_, pub, err := backup.GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		r, err := svc.AddRecipient(ctx, p, backupapi.AddRecipientInput{PublicKey: &pub})
		if err != nil {
			t.Fatal(err)
		}
		return r.Fingerprint
	}
	run := func(fp string, cancel bool) (int, string, string) {
		var out, errb bytes.Buffer
		rc := backupRecipient(ctx, svc, fp, cancel, &out, &errb)
		return rc, out.String(), errb.String()
	}
	a := add() // active at once
	b := add() // pending for 24 hours

	// Cancel a pending add.
	if rc, out, e := run(b, true); rc != 0 || !strings.Contains(out, b[:12]) {
		t.Fatalf("cancel: %d %q %q", rc, out, e)
	}
	if rc, _, e := run(b, true); rc != 1 || e == "" {
		t.Fatalf("second cancel: %d %q", rc, e)
	}
	// A bad fingerprint and an unknown one.
	if rc, _, _ := run("abc", false); rc != 1 {
		t.Fatalf("short fingerprint: %d", rc)
	}
	if rc, _, _ := run(strings.Repeat("a", 64), false); rc != 1 {
		t.Fatalf("unknown fingerprint: %d", rc)
	}
	// With a destination enabled, the last active key stays.
	if _, err := pool.Exec(ctx, `INSERT INTO backup_destinations (name, type, config, enabled, enabled_since) VALUES ('d','s3','{}',true,now())`); err != nil {
		t.Fatal(err)
	}
	if rc, _, e := run(a, false); rc != 1 || e == "" {
		t.Fatalf("removed the last key under an enabled destination: %d", rc)
	}
	if _, err := pool.Exec(ctx, `UPDATE backup_destinations SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	if rc, out, e := run(a, false); rc != 0 || !strings.Contains(out, a[:12]) {
		t.Fatalf("remove: %d %q %q", rc, out, e)
	}
	// The host rows have no actor.
	var withActor, host int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE actor_id IS NOT NULL AND kind LIKE '%_by_host'), count(*) FILTER (WHERE kind LIKE '%_by_host') FROM backup_change_log`).Scan(&withActor, &host); err != nil {
		t.Fatal(err)
	}
	if withActor != 0 || host != 2 {
		t.Fatalf("host rows: %d with an actor, %d in all", withActor, host)
	}
	// Usage.
	if rc := backupRecipientCmd([]string{}, &bytes.Buffer{}, &bytes.Buffer{}, false); rc != 2 {
		t.Fatalf("usage exit %d", rc)
	}
}
