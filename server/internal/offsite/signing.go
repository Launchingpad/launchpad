package offsite

import (
	"bytes"
	"context"
	"crypto/ed25519"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// ensureSigningKey makes the manifest signing key on first use (decisions-T16 #22).
// It acts ONLY when both columns are empty. A blanked, half-empty or unreadable
// key never makes a new one: a silent new key would hide a tampered row, so runs
// fail secrets_unreadable until `rotate-signing`. The write is a compare-and-set;
// of two workers one wins and the other reads the stored key. It never touches
// key_check.
func (w *Worker) ensureSigningKey(ctx context.Context) error {
	q := dbq.New(w.pool)
	s, err := q.GetBackupSettings(ctx)
	if err != nil {
		return internal(err)
	}
	if s.SigningPublic != "" || s.SigningPrivateEnc != nil {
		return nil
	}
	pub, priv, err := backup.GenerateSigningKey()
	if err != nil {
		return internal(err)
	}
	blob, err := w.keys.Seal(platform.BackupColSigningKey, platform.BackupSettingsOwner, priv)
	if err != nil {
		return internal(err)
	}
	n, err := q.ClaimBackupSigningKey(ctx, dbq.ClaimBackupSigningKeyParams{
		SigningPublic: backup.EncodePublicKey(pub), SigningPrivateEnc: blob,
	})
	if err != nil {
		return internal(err)
	}
	if n > 0 {
		w.log.Info("offsite: made the manifest signing key (keep its public half off this host)")
	}
	return nil
}

// loadSigning opens the signing key for one run. A key that cannot be read, a half
// empty pair and a private half that does not match the public one are all
// secrets_unreadable. The empty state makes the key (first use) and reads it back.
func (w *Worker) loadSigning(ctx context.Context) (ed25519.PrivateKey, string, error) {
	q := dbq.New(w.pool)
	s, err := q.GetBackupSettings(ctx)
	if err != nil {
		return nil, "", internal(err)
	}
	if s.SigningPublic == "" && s.SigningPrivateEnc == nil {
		if err := w.ensureSigningKey(ctx); err != nil {
			return nil, "", err
		}
		if s, err = q.GetBackupSettings(ctx); err != nil {
			return nil, "", internal(err)
		}
	}
	if s.SigningPublic == "" || s.SigningPrivateEnc == nil {
		return nil, "", driver.Errf(driver.SecretsUnreadable)
	}
	raw, err := w.keys.Open(platform.BackupColSigningKey, platform.BackupSettingsOwner, s.SigningPrivateEnc)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, "", driver.Errf(driver.SecretsUnreadable)
	}
	priv := ed25519.PrivateKey(raw)
	pub, err := backup.ParsePublicKey(s.SigningPublic)
	if err != nil || !bytes.Equal(priv.Public().(ed25519.PublicKey), pub) {
		return nil, "", driver.Errf(driver.SecretsUnreadable)
	}
	return priv, s.SigningPublic, nil
}
