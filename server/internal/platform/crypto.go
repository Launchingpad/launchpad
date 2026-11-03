package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
)

// HKDF info strings: one purpose each, so the sealing key and the key check
// are independent outputs of the one SETTINGS_ENCRYPTION_KEY. Changing one
// makes every stored secret unreadable: never change them.
const (
	infoSeal  = "launchpad platform_settings secret sealing v1"
	infoCheck = "launchpad platform_settings key check v1"
)

// ErrUnreadable: a stored secret did not open (another key, or the blob was
// changed). The error says nothing about the secret.
var ErrUnreadable = errors.New("a stored secret could not be opened with this server's SETTINGS_ENCRYPTION_KEY")

// Keyring seals and opens the platform_settings secrets with AES-256-GCM
// (a random 96-bit nonce per seal, prepended). The key is HKDF-SHA256 of
// SETTINGS_ENCRYPTION_KEY (golang-security: AES-GCM, crypto/rand nonces,
// stdlib crypto/hkdf). Each blob is bound to its column by the additional
// data, so a token blob copied into the password column does not open.
type Keyring struct {
	aead  cipher.AEAD
	check []byte
}

// NewKeyring derives the keys from the 32-byte master key.
func NewKeyring(master []byte) (*Keyring, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("the settings key must be 32 bytes, got %d", len(master))
	}
	seal, err := hkdf.Key(sha256.New, master, nil, infoSeal, 32)
	if err != nil {
		return nil, fmt.Errorf("derive sealing key: %w", err)
	}
	check, err := hkdf.Key(sha256.New, master, nil, infoCheck, 32)
	if err != nil {
		return nil, fmt.Errorf("derive key check: %w", err)
	}
	block, err := aes.NewCipher(seal)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Keyring{aead: aead, check: check}, nil
}

func aad(column string) []byte { return []byte("platform_settings." + column) }

// Seal encrypts plaintext for column: nonce || ciphertext || tag.
func (k *Keyring) Seal(column string, plaintext []byte) []byte {
	return k.aead.Seal(nil, nil, plaintext, aad(column))
}

// Open decrypts a blob of column. Any failure is ErrUnreadable.
func (k *Keyring) Open(column string, blob []byte) ([]byte, error) {
	pt, err := k.aead.Open(nil, nil, blob, aad(column))
	if err != nil {
		return nil, ErrUnreadable
	}
	return pt, nil
}

// KeyCheck is the value stored in platform_settings.key_check.
func (k *Keyring) KeyCheck() []byte { return append([]byte(nil), k.check...) }

// Matches reports whether a stored key_check is this key's (constant time).
func (k *Keyring) Matches(stored []byte) bool {
	return len(stored) == len(k.check) && subtle.ConstantTimeCompare(stored, k.check) == 1
}

// infoGitSeal is the HKDF info string of the git_integrations scope
// (T17 #11, LAUN-5). It is a second, independent key from the same
// SETTINGS_ENCRYPTION_KEY, so a platform_settings blob never opens as a git
// blob. There is no key-check string for this scope: git finds a wrong key
// when a blob fails to open (secret_unreadable). Never change it.
const infoGitSeal = "launchpad git_integrations sealing v1"

// GitKeyring seals and opens the git_integrations secrets (token_enc,
// webhook_secret_enc, webhook_secret_next_enc) with AES-256-GCM. The
// additional data is "git_integrations." + column + "." + row id, so a blob
// cannot be moved to another row or another column.
type GitKeyring struct {
	aead cipher.AEAD
}

// NewGitKeyring derives the git sealing key from the 32-byte master key.
func NewGitKeyring(master []byte) (*GitKeyring, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("the settings key must be 32 bytes, got %d", len(master))
	}
	seal, err := hkdf.Key(sha256.New, master, nil, infoGitSeal, 32)
	if err != nil {
		return nil, fmt.Errorf("derive git sealing key: %w", err)
	}
	block, err := aes.NewCipher(seal)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &GitKeyring{aead: aead}, nil
}

func gitAAD(column, rowID string) []byte { return []byte("git_integrations." + column + "." + rowID) }

// SealFor encrypts plaintext for column of the row rowID: nonce || ciphertext
// || tag. The row id is made in the app before the INSERT.
func (k *GitKeyring) SealFor(column, rowID string, plaintext []byte) []byte {
	return k.aead.Seal(nil, nil, plaintext, gitAAD(column, rowID))
}

// OpenFor decrypts a blob of column of the row rowID. Any failure is
// ErrUnreadable.
func (k *GitKeyring) OpenFor(column, rowID string, blob []byte) ([]byte, error) {
	pt, err := k.aead.Open(nil, nil, blob, gitAAD(column, rowID))
	if err != nil {
		return nil, ErrUnreadable
	}
	return pt, nil
}

// infoSlackSeal is the HKDF info string of the slack_channels scope (LAUN-198,
// decisions-T33). A third independent key from the same SETTINGS_ENCRYPTION_KEY:
// a platform_settings or git blob never opens as a Slack blob. Never change it.
const infoSlackSeal = "launchpad slack_channels sealing v1"

// SlackColWebhook is the sealed column slack_channels.webhook_enc.
const SlackColWebhook = "webhook_enc"

// PersonalColWebhook is the sealed column personal_webhooks.webhook_enc
// (LAUN-229, decisions-T40 #19). It is sealed with the SlackKeyring (the chat
// key) under its own column name and a scope per webhook, so its additional
// data never equals that of a chat service blob.
const PersonalColWebhook = "personal_webhooks.webhook_enc"

// SlackKeyring seals and opens the Slack webhook URL. The additional data is
// "slack_channels." + column + "." + org id, so a blob cannot be moved to
// another org or another column.
type SlackKeyring struct {
	aead cipher.AEAD
}

// NewSlackKeyring derives the Slack sealing key from the 32-byte master key.
func NewSlackKeyring(master []byte) (*SlackKeyring, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("the settings key must be 32 bytes, got %d", len(master))
	}
	seal, err := hkdf.Key(sha256.New, master, nil, infoSlackSeal, 32)
	if err != nil {
		return nil, fmt.Errorf("derive slack sealing key: %w", err)
	}
	block, err := aes.NewCipher(seal)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &SlackKeyring{aead: aead}, nil
}

func slackAAD(column, orgID string) []byte { return []byte("slack_channels." + column + "." + orgID) }

// SealFor encrypts plaintext for column of the org's row: nonce || ciphertext || tag.
func (k *SlackKeyring) SealFor(column, orgID string, plaintext []byte) []byte {
	return k.aead.Seal(nil, nil, plaintext, slackAAD(column, orgID))
}

// OpenFor decrypts a blob of column of the org's row. Any failure is ErrUnreadable.
func (k *SlackKeyring) OpenFor(column, orgID string, blob []byte) ([]byte, error) {
	pt, err := k.aead.Open(nil, nil, blob, slackAAD(column, orgID))
	if err != nil {
		return nil, ErrUnreadable
	}
	return pt, nil
}

// infoPushSeal is the HKDF info string of the push scope (LAUN-197,
// decisions-T35). A fourth independent key from the same SETTINGS_ENCRYPTION_KEY:
// a platform_settings, git or Slack blob never opens as a push blob. Never change it.
const infoPushSeal = "launchpad push sealing v1"

// The sealed push columns. A relay blob is bound to its org; a device token is
// bound to its org and its device id.
const (
	PushColRelayURL    = "push_relays.url_enc"
	PushColRelaySecret = "push_relays.secret_enc"
	PushColDeviceToken = "push_devices.token_enc"
)

// PushKeyring seals and opens the relay URL, the relay secret and the device
// tokens. The additional data is "push." + column + "." + scope, where scope is
// the org id (relay) or "org id.device id" (token), so a blob cannot be moved
// to another org, device or column.
type PushKeyring struct {
	aead cipher.AEAD
}

// NewPushKeyring derives the push sealing key from the 32-byte master key.
func NewPushKeyring(master []byte) (*PushKeyring, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("the settings key must be 32 bytes, got %d", len(master))
	}
	seal, err := hkdf.Key(sha256.New, master, nil, infoPushSeal, 32)
	if err != nil {
		return nil, fmt.Errorf("derive push sealing key: %w", err)
	}
	block, err := aes.NewCipher(seal)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &PushKeyring{aead: aead}, nil
}

func pushAAD(column, scope string) []byte { return []byte("push." + column + "." + scope) }

// SealFor encrypts plaintext for column of the row named by scope: nonce || ciphertext || tag.
func (k *PushKeyring) SealFor(column, scope string, plaintext []byte) []byte {
	return k.aead.Seal(nil, nil, plaintext, pushAAD(column, scope))
}

// OpenFor decrypts a blob of column of the row named by scope. Any failure is ErrUnreadable.
func (k *PushKeyring) OpenFor(column, scope string, blob []byte) ([]byte, error) {
	pt, err := k.aead.Open(nil, nil, blob, pushAAD(column, scope))
	if err != nil {
		return nil, ErrUnreadable
	}
	return pt, nil
}
