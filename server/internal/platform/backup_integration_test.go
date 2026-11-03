package platform_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/platform"
	"devops/internal/testdb"
)

func backupKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// TestBackupKeyring_KeyChangeRecovery: after BACKUP_ENCRYPTION_KEY changes,
// every destination is unreadable and key_check shows a mismatch; entering ONE
// destination's secrets again makes it readable and replaces key_check in the
// same transaction; the others and the signing key stay unreadable
// (decisions-T16 #12, #22, #24). Nothing here crashes on a wrong key.
func TestBackupKeyring_KeyChangeRecovery(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	q := dbq.New(pool)
	oldK, err := platform.NewBackupKeyring(backupKey(1))
	if err != nil {
		t.Fatal(err)
	}
	newK, err := platform.NewBackupKeyring(backupKey(2))
	if err != nil {
		t.Fatal(err)
	}

	mk := func(name string, secret string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		blob, err := oldK.Seal(platform.BackupColDestinationSecrets, id.String(), []byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.InsertBackupDestination(ctx, dbq.InsertBackupDestinationParams{
			ID: id, Name: name, Type: "s3", Config: []byte(`{}`), SecretsEnc: blob, SecretsSet: []string{"secretKey"},
			Schedule: "daily", AtUtc: "04:15", KeepLast: 14, VerifyEveryDays: 7,
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	d1, d2 := mk("one", `{"secretKey":"planted-one"}`), mk("two", `{"secretKey":"planted-two"}`)
	// The signing key, sealed under the old key by the first-use compare-and-set.
	sig, err := oldK.Seal(platform.BackupColSigningKey, platform.BackupSettingsOwner, []byte("private-seed"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := q.ClaimBackupSigningKey(ctx, dbq.ClaimBackupSigningKeyParams{SigningPublic: "PUB", SigningPrivateEnc: sig}); err != nil || n != 1 {
		t.Fatalf("claim: %d %v", n, err)
	}
	if n, err := q.ClaimBackupSigningKey(ctx, dbq.ClaimBackupSigningKeyParams{SigningPublic: "PUB2", SigningPrivateEnc: sig}); err != nil || n != 0 {
		t.Fatalf("a second claim changed %d rows: %v", n, err)
	}

	// The first seal records the check; a second call changes nothing.
	if changed, err := oldK.RecordBackupKeyCheck(ctx, q); err != nil || !changed {
		t.Fatalf("first record: %v %v", changed, err)
	}
	if changed, err := oldK.RecordBackupKeyCheck(ctx, q); err != nil || changed {
		t.Fatalf("second record changed the row: %v %v", changed, err)
	}
	st, err := q.GetBackupSettings(ctx)
	if err != nil || oldK.BackupKeyMismatch(st.KeyCheck) || !newK.BackupKeyMismatch(st.KeyCheck) {
		t.Fatalf("mismatch flags after the first record: %v", err)
	}

	open := func(kr *platform.BackupKeyring, id uuid.UUID) error {
		row, err := q.GetBackupDestination(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = kr.Open(platform.BackupColDestinationSecrets, id.String(), row.SecretsEnc)
		return err
	}
	// The key changed: both unreadable (an error, not a crash).
	for _, id := range []uuid.UUID{d1, d2} {
		if err := open(newK, id); !errors.Is(err, platform.ErrUnreadable) {
			t.Fatalf("new key opened an old blob: %v", err)
		}
		if err := open(oldK, id); err != nil {
			t.Fatalf("old key: %v", err)
		}
	}
	// A blob copied from one destination to the other does not open there.
	row1, _ := q.GetBackupDestination(ctx, d1)
	if _, err := oldK.Open(platform.BackupColDestinationSecrets, d2.String(), row1.SecretsEnc); !errors.Is(err, platform.ErrUnreadable) {
		t.Fatalf("a blob opened on another destination: %v", err)
	}

	// Re-enter destination one's secrets under the new key, in one transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tq := q.WithTx(tx)
	if _, err := tq.GetBackupDestinationForUpdate(ctx, d1); err != nil {
		t.Fatal(err)
	}
	blob, err := newK.Seal(platform.BackupColDestinationSecrets, d1.String(), []byte(`{"secretKey":"planted-one-again"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tq.SetBackupDestinationSecrets(ctx, dbq.SetBackupDestinationSecretsParams{ID: d1, SecretsEnc: blob, SecretsSet: []string{"secretKey"}}); err != nil {
		t.Fatal(err)
	}
	if changed, err := newK.RecordBackupKeyCheck(ctx, tq); err != nil || !changed {
		t.Fatalf("replace: %v %v", changed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := open(newK, d1); err != nil {
		t.Fatalf("destination one is still unreadable: %v", err)
	}
	if err := open(newK, d2); !errors.Is(err, platform.ErrUnreadable) {
		t.Fatalf("destination two became readable: %v", err)
	}
	st, err = q.GetBackupSettings(ctx)
	if err != nil || newK.BackupKeyMismatch(st.KeyCheck) || !newK.Matches(st.KeyCheck) {
		t.Fatalf("key_check was not replaced: %v", err)
	}
	if _, err := newK.Open(platform.BackupColSigningKey, platform.BackupSettingsOwner, st.SigningPrivateEnc); !errors.Is(err, platform.ErrUnreadable) {
		t.Fatalf("the signing key became readable under the new key: %v", err)
	}
	if _, err := oldK.Open(platform.BackupColSigningKey, platform.BackupSettingsOwner, st.SigningPrivateEnc); err != nil {
		t.Fatalf("the signing key does not open under the old key: %v", err)
	}
	// Only the sealed columns hold the secrets: the clear text is nowhere else.
	var leaks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM backup_destinations d WHERE d.config::text LIKE '%planted%'
		OR array_to_string(d.secrets_set, ',') LIKE '%planted%'`).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("clear text found: %d %v", leaks, err)
	}
}

// TestBackupKeyring_RunTestSecrets: the test secrets of a run open only on that run.
func TestBackupKeyring_RunTestSecrets(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	q := dbq.New(pool)
	kr, err := platform.NewBackupKeyring(backupKey(3))
	if err != nil {
		t.Fatal(err)
	}
	mk := func() (dbq.BackupRun, uuid.UUID) {
		id := uuid.Must(uuid.NewV7())
		blob, err := kr.Seal(platform.BackupColRunTestSecrets, id.String(), []byte(`{"password":"planted-run"}`))
		if err != nil {
			t.Fatal(err)
		}
		r, err := q.InsertBackupRun(ctx, dbq.InsertBackupRunParams{ID: id, Trigger: "test", Status: "queued", Attempt: 1,
			TestConfig: []byte(`{"host":"example.test"}`), TestSecretsEnc: blob})
		if err != nil {
			t.Fatal(err)
		}
		return r, id
	}
	a, aid := mk()
	b, bid := mk()
	if _, err := kr.Open(platform.BackupColRunTestSecrets, aid.String(), a.TestSecretsEnc); err != nil {
		t.Fatal(err)
	}
	if _, err := kr.Open(platform.BackupColRunTestSecrets, bid.String(), a.TestSecretsEnc); !errors.Is(err, platform.ErrUnreadable) {
		t.Fatalf("run a's secrets opened on run b: %v", err)
	}
	_ = b
}
