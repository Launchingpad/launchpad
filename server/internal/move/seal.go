package move

import (
	"devops/internal/platform"
)

// Keys are the keyrings of ONE server: the exporting server's on export, the
// importing server's on import. A nil keyring means the server has no such
// key. Backup secrets go through Backup only (AGENTS.md).
type Keys struct {
	Settings *platform.Keyring
	Git      *platform.GitKeyring
	Push     *platform.PushKeyring
	Slack    *platform.SlackKeyring
	Backup   *platform.BackupKeyring
}

// keyName names the environment variable a missing keyring needs.
func keyName(ring string) string {
	if ring == ringBackup {
		return "BACKUP_ENCRYPTION_KEY"
	}
	return "SETTINGS_ENCRYPTION_KEY"
}

func (k Keys) has(ring string) bool {
	switch ring {
	case ringSettings:
		return k.Settings != nil
	case ringGit:
		return k.Git != nil
	case ringPush:
		return k.Push != nil
	case ringSlack:
		return k.Slack != nil
	case ringBackup:
		return k.Backup != nil
	}
	return false
}

// open decrypts blob of the column of row r with the right keyring.
func (k Keys) open(s Sealed, r *row, blob []byte) ([]byte, error) {
	if !k.has(s.Ring) {
		return nil, &KeyMissingError{Keyring: keyName(s.Ring)}
	}
	col := s.aadColumn()
	switch s.Ring {
	case ringSettings:
		return k.Settings.Open(col, blob)
	case ringGit:
		return k.Git.OpenFor(col, s.Scope(r), blob)
	case ringPush:
		return k.Push.OpenFor(col, s.Scope(r), blob)
	case ringSlack:
		return k.Slack.OpenFor(col, s.Scope(r), blob)
	default:
		return k.Backup.Open(col, s.Scope(r), blob)
	}
}

// seal encrypts plaintext for the column of row r with the right keyring.
func (k Keys) seal(s Sealed, r *row, pt []byte) ([]byte, error) {
	if !k.has(s.Ring) {
		return nil, &KeyMissingError{Keyring: keyName(s.Ring)}
	}
	col := s.aadColumn()
	switch s.Ring {
	case ringSettings:
		return k.Settings.Seal(col, pt), nil
	case ringGit:
		return k.Git.SealFor(col, s.Scope(r), pt), nil
	case ringPush:
		return k.Push.SealFor(col, s.Scope(r), pt), nil
	case ringSlack:
		return k.Slack.SealFor(col, s.Scope(r), pt), nil
	default:
		return k.Backup.Seal(col, s.Scope(r), pt)
	}
}
