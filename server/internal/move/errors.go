package move

import (
	"errors"
	"fmt"
)

// The typed errors of export and import. None holds a secret, a passphrase or
// a row value.
var (
	// ErrBadPassphrase: the passphrase does not open the bundle.
	ErrBadPassphrase = errors.New("move: the passphrase is wrong, or this file is not a Launchpad bundle")
	// ErrBundleCorrupt: the bundle is damaged, cut short, changed, or not in the format this server reads.
	ErrBundleCorrupt = errors.New("move: the bundle is damaged, incomplete or changed")
	// ErrNotEmpty: import needs an install with no user and no org.
	ErrNotEmpty = errors.New("move: this install is not empty (it has a user or an org); import only into a new install in setup state")
	// ErrNoStore: the bundle holds attachment objects and this server has no object store.
	ErrNoStore = errors.New("move: the bundle holds attachment files but this server has no object store (S3_*)")
	// ErrWeakPassphrase: export takes only a passphrase from GeneratePassphrase (or as long).
	ErrWeakPassphrase = errors.New("move: the passphrase is too short; use GeneratePassphrase")
	// ErrChecksum: an object did not match its sha256.
	ErrChecksum = errors.New("move: an attachment file did not match its checksum")
)

// VersionError: the bundle was made at another migration version (rule v1:
// import needs the exact same version).
type VersionError struct{ Bundle, Server uint }

func (e *VersionError) Error() string {
	return fmt.Sprintf("move: the bundle is at migration version %d and this server is at %d; run the same Launchpad version on both sides", e.Bundle, e.Server)
}

// KeyMissingError: the server has no key for a sealed column the bundle holds
// (or, on export, a column that holds a secret).
type KeyMissingError struct{ Keyring string }

func (e *KeyMissingError) Error() string {
	return "move: this server has no " + e.Keyring + " key, but the install holds secrets sealed with it (set SETTINGS_ENCRYPTION_KEY / BACKUP_ENCRYPTION_KEY)"
}

// UnreadableSecretError: a stored secret did not open with this server's key
// (another key, or the blob changed). Export refuses rather than lose it.
type UnreadableSecretError struct{ Table, Column string }

func (e *UnreadableSecretError) Error() string {
	return "move: a stored secret in " + e.Table + "." + e.Column + " cannot be opened with this server's key; fix or clear it first"
}

// ObjectExistsError: the store already holds an object the import would
// write. The import never overwrites (a failed import deletes only what it
// made, so it must know it made it).
type ObjectExistsError struct{ Key string }

func (e *ObjectExistsError) Error() string {
	return "move: the object store already holds " + e.Key + "; import into an empty bucket, or remove the stray object"
}

// ObjectMissingError: an attachment row points at an object the store lacks.
type ObjectMissingError struct{ Key string }

func (e *ObjectMissingError) Error() string {
	return "move: the object store lacks " + e.Key + " (an attachment row points at it); export with attachments excluded, or repair the store"
}
