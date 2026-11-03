package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"

	"devops/internal/dbq"
)

// HKDF info strings of BACKUP_ENCRYPTION_KEY (decisions-T16 #12, #24): one
// purpose each, and different from every platform and git string, so a blob of
// another scope never opens here even under the same master key. Never change
// them: every stored backup secret would become unreadable.
const (
	infoBackupSeal  = "launchpad backup secret sealing v1"
	infoBackupCheck = "launchpad backup key check v1"
)

// The three sealed columns and the owner each one is bound to (decisions-T16
// #24). The additional data of a blob is "<column>.<owner>", so a blob copied
// to another row, or another column, does not open.
const (
	// BackupColDestinationSecrets is backup_destinations.secrets_enc; the owner is the destination id.
	BackupColDestinationSecrets = "backup_destinations.secrets_enc"
	// BackupColRunTestSecrets is backup_runs.test_secrets_enc; the owner is the run id
	// (an unsaved test has no destination id).
	BackupColRunTestSecrets = "backup_runs.test_secrets_enc"
	// BackupColSigningKey is backup_settings.signing_private_enc; the owner is always BackupSettingsOwner.
	BackupColSigningKey = "backup_settings.signing_private_enc"
	// BackupSettingsOwner is the fixed owner word of the signing key.
	BackupSettingsOwner = "settings"
)

// errBackupOwner reports a misuse (a column this keyring does not seal, or an
// owner of the wrong form). It names no secret.
var errBackupOwner = errors.New("backup keyring: unknown column or invalid owner")

// BackupKeyring seals and opens the backup secrets with AES-256-GCM under a
// key derived from BACKUP_ENCRYPTION_KEY (a random 96-bit nonce per seal,
// prepended). It is the backup scope of the T9 sealing code: the same
// construction as Keyring and GitKeyring, its own HKDF strings, and an owner id
// in the additional data (decisions-T16 #24). Keyring.Seal / Open keep their
// column-only form, so T9's callers do not change.
type BackupKeyring struct {
	aead  cipher.AEAD
	check []byte
}

// NewBackupKeyring derives the keys from the 32-byte master key.
func NewBackupKeyring(master []byte) (*BackupKeyring, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("the backup key must be 32 bytes, got %d", len(master))
	}
	seal, err := hkdf.Key(sha256.New, master, nil, infoBackupSeal, 32)
	if err != nil {
		return nil, fmt.Errorf("derive backup sealing key: %w", err)
	}
	check, err := hkdf.Key(sha256.New, master, nil, infoBackupCheck, 32)
	if err != nil {
		return nil, fmt.Errorf("derive backup key check: %w", err)
	}
	block, err := aes.NewCipher(seal)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &BackupKeyring{aead: aead, check: check}, nil
}

// backupAAD builds the additional data, or fails for a column that is not one
// of the three, an empty owner, or a signing key owner other than "settings".
func backupAAD(column, owner string) ([]byte, error) {
	switch column {
	case BackupColDestinationSecrets, BackupColRunTestSecrets:
		if owner == "" || owner == BackupSettingsOwner {
			return nil, errBackupOwner
		}
	case BackupColSigningKey:
		if owner != BackupSettingsOwner {
			return nil, errBackupOwner
		}
	default:
		return nil, errBackupOwner
	}
	return []byte(column + "." + owner), nil
}

// Seal encrypts plaintext for column of the row owner: nonce || ciphertext ||
// tag. The error is a misuse of the arguments, never about the plaintext.
func (k *BackupKeyring) Seal(column, owner string, plaintext []byte) ([]byte, error) {
	ad, err := backupAAD(column, owner)
	if err != nil {
		return nil, err
	}
	return k.aead.Seal(nil, nil, plaintext, ad), nil
}

// Open decrypts a blob of column of the row owner. Any failure (another key,
// another row or column, a changed blob, a misused argument) is ErrUnreadable,
// which says nothing about the secret.
func (k *BackupKeyring) Open(column, owner string, blob []byte) ([]byte, error) {
	ad, err := backupAAD(column, owner)
	if err != nil {
		return nil, ErrUnreadable
	}
	pt, err := k.aead.Open(nil, nil, blob, ad)
	if err != nil {
		return nil, ErrUnreadable
	}
	return pt, nil
}

// KeyCheck is the value stored in backup_settings.key_check.
func (k *BackupKeyring) KeyCheck() []byte { return append([]byte(nil), k.check...) }

// Matches reports whether a stored key_check is this key's (constant time).
func (k *BackupKeyring) Matches(stored []byte) bool {
	return len(stored) == len(k.check) && subtle.ConstantTimeCompare(stored, k.check) == 1
}

// BackupKeyMismatch is the global flag keyMismatch (decisions-T16 #12): the
// stored key_check is not NULL and differs from this key's. A NULL check
// (nothing sealed yet) is not a mismatch.
func (k *BackupKeyring) BackupKeyMismatch(stored []byte) bool {
	return stored != nil && !k.Matches(stored)
}

// RecordBackupKeyCheck stores this key's check on the first successful seal and
// replaces a different one (decisions-T16 #12). The caller runs it in the SAME
// transaction as the seal it follows (pass the transaction's queries), and only
// after a seal under the current key succeeded. It reports whether the row
// changed. Only the app calls it: offsite never writes key_check.
func (k *BackupKeyring) RecordBackupKeyCheck(ctx context.Context, q *dbq.Queries) (bool, error) {
	n, err := q.SetBackupKeyCheck(ctx, k.KeyCheck())
	if err != nil {
		return false, fmt.Errorf("store backup key check: %w", err)
	}
	return n > 0, nil
}
