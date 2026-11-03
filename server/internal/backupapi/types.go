package backupapi

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/platform"
)

// The wire types of the backups API (decisions-T16 #43). A response type never
// holds a secret: the destination view says WHICH secrets are set, never their
// values. The input types hold platform.Secret, which prints as [redacted].

// Recipient states.
const (
	RecipientPending  = "pending"  // added, not yet effective (24 hour hold, #20)
	RecipientActive   = "active"   // offsite encrypts to it
	RecipientRemoving = "removing" // active until removeAt
)

// Recipient is one age public key of the install.
type Recipient struct {
	PublicKey        string     `json:"publicKey"`
	Fingerprint      string     `json:"fingerprint"` // the full sha256 (64 hex): the route key
	ShortFingerprint string     `json:"shortFingerprint"`
	State            string     `json:"state"`
	AddedAt          time.Time  `json:"addedAt"`
	AddedBy          *uuid.UUID `json:"addedBy"`
	EffectiveAt      time.Time  `json:"effectiveAt"`
	RemoveAt         *time.Time `json:"removeAt"`
	RemoveBy         *uuid.UUID `json:"removeBy"`
}

// RecipientList is GET .../recipients and the answer of a cancel or removal.
type RecipientList struct {
	Items []Recipient `json:"items"`
	// EverHadRecipient: the install had a recipient once; every later add is held.
	EverHadRecipient bool `json:"everHadRecipient"`
	// Limit is the most live recipients (5).
	Limit int `json:"limit"`
}

// AddRecipientInput is POST .../recipients.
type AddRecipientInput struct {
	PublicKey *string `json:"publicKey"`
}

// Settings is GET .../settings.
type Settings struct {
	InstallID       string     `json:"installId"`
	SigningPublic   string     `json:"signingPublic"`
	NotifyOnFailure bool       `json:"notifyOnFailure"`
	NotifyOnSuccess bool       `json:"notifyOnSuccess"`
	StaleAlertHours int        `json:"staleAlertHours"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	UpdatedBy       *uuid.UUID `json:"updatedBy"`
}

// SettingsInput is PUT .../settings: the whole notice object.
type SettingsInput struct {
	NotifyOnFailure *bool `json:"notifyOnFailure"`
	NotifyOnSuccess *bool `json:"notifyOnSuccess"`
	StaleAlertHours *int  `json:"staleAlertHours"`
}

// Run is one row of the run table. It never carries test_config or
// test_secrets_enc.
type Run struct {
	ID              uuid.UUID       `json:"id"`
	DestinationID   *uuid.UUID      `json:"destinationId"`
	DestinationName string          `json:"destinationName"`
	DumpID          *uuid.UUID      `json:"dumpId"`
	Trigger         string          `json:"trigger"`
	Status          string          `json:"status"`
	Attempt         int             `json:"attempt"`
	WillRetry       bool            `json:"willRetry"`
	RetryAt         *time.Time      `json:"retryAt"`
	QueuedAt        time.Time       `json:"queuedAt"`
	StartedAt       *time.Time      `json:"startedAt"`
	FinishedAt      *time.Time      `json:"finishedAt"`
	Bytes           *int64          `json:"bytes"`
	SHA256          *string         `json:"sha256"`
	RemoteName      *string         `json:"remoteName"`
	Steps           json.RawMessage `json:"steps"`
	ErrorCode       string          `json:"errorCode"`
	ErrorText       string          `json:"errorText"`
}

// Destination is a backup destination. Secrets are write-only.
type Destination struct {
	ID                uuid.UUID       `json:"id"`
	Name              string          `json:"name"`
	Type              string          `json:"type"`
	TypeSupported     bool            `json:"typeSupported"`
	Enabled           bool            `json:"enabled"`
	EnabledSince      *time.Time      `json:"enabledSince"`
	Config            json.RawMessage `json:"config"`
	SecretsSet        []string        `json:"secretsSet"`
	SecretsUnreadable bool            `json:"secretsUnreadable"`
	Schedule          string          `json:"schedule"`
	AtUTC             string          `json:"atUtc"`
	Weekday           *int            `json:"weekday"`
	KeepLast          int             `json:"keepLast"`
	VerifyEveryDays   int             `json:"verifyEveryDays"`
	LastRun           *Run            `json:"lastRun"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
	UpdatedBy         *uuid.UUID      `json:"updatedBy"`
}

// DestinationList is GET .../destinations.
type DestinationList struct {
	Items []Destination `json:"items"`
}

// CreateDestinationInput is POST .../destinations. A destination is made
// disabled; enabling is a PATCH once the checks pass.
type CreateDestinationInput struct {
	Name            *string                    `json:"name"`
	Type            *string                    `json:"type"`
	Config          json.RawMessage            `json:"config"`
	Secrets         map[string]platform.Secret `json:"secrets"`
	Schedule        *string                    `json:"schedule"`
	AtUTC           *string                    `json:"atUtc"`
	Weekday         *int                       `json:"weekday"`
	KeepLast        *int                       `json:"keepLast"`
	VerifyEveryDays *int                       `json:"verifyEveryDays"`
	ConfirmLowKeep  bool                       `json:"confirmLowKeep"`
}

// UpdateDestinationInput is PATCH .../destinations/{id}. Absent leaves a field
// alone. A secret absent or null is kept, "" clears it, a value replaces it.
type UpdateDestinationInput struct {
	Name            *string                               `json:"name"`
	Config          json.RawMessage                       `json:"config"`
	Secrets         map[string]httpx.Opt[platform.Secret] `json:"secrets"`
	Schedule        *string                               `json:"schedule"`
	AtUTC           *string                               `json:"atUtc"`
	Weekday         httpx.Opt[int]                        `json:"weekday"`
	KeepLast        *int                                  `json:"keepLast"`
	VerifyEveryDays *int                                  `json:"verifyEveryDays"`
	Enabled         *bool                                 `json:"enabled"`
	ConfirmLowKeep  bool                                  `json:"confirmLowKeep"`
}

// ProbeInput is POST .../destinations/probe: an unsaved form. A probe sends no
// credentials, so there are no secrets.
type ProbeInput struct {
	Type   *string         `json:"type"`
	Config json.RawMessage `json:"config"`
}

// TestInput is the optional body of POST .../destinations/{id}/test.
type TestInput struct {
	ConfirmedFingerprint *string `json:"confirmedFingerprint"`
}

// UnsavedTestInput is POST .../destinations/test: a form that is not saved.
// UseSavedSecretsFrom names a stored destination whose secrets fill the names
// the body leaves out; the config must then equal that destination's.
type UnsavedTestInput struct {
	Type                 *string                    `json:"type"`
	Config               json.RawMessage            `json:"config"`
	Secrets              map[string]platform.Secret `json:"secrets"`
	UseSavedSecretsFrom  *uuid.UUID                 `json:"useSavedSecretsFrom"`
	ConfirmedFingerprint *string                    `json:"confirmedFingerprint"`
}

// ExchangeInput is POST .../destinations/{id}/exchange: the Dropbox
// authorization code. It is single use and sealed at once.
type ExchangeInput struct {
	Code *platform.Secret `json:"code"`
}

// ChangeEntry is one row of the change log. Detail holds no secret.
type ChangeEntry struct {
	ID              uuid.UUID       `json:"id"`
	At              time.Time       `json:"at"`
	ActorID         *uuid.UUID      `json:"actorId"`
	ActorUsername   *string         `json:"actorUsername"`
	Kind            string          `json:"kind"`
	DestinationID   *uuid.UUID      `json:"destinationId"`
	DestinationName string          `json:"destinationName"`
	Detail          json.RawMessage `json:"detail"`
}

// Dump is the newest local dump row (written by backup.sh).
type Dump struct {
	ID               uuid.UUID  `json:"id"`
	FileName         string     `json:"fileName"`
	Status           string     `json:"status"`
	StartedAt        time.Time  `json:"startedAt"`
	FinishedAt       *time.Time `json:"finishedAt"`
	Bytes            *int64     `json:"bytes"`
	MigrationVersion *int64     `json:"migrationVersion"`
	FileRemoved      bool       `json:"fileRemoved"`
	ErrorText        string     `json:"errorText"`
}

// Banner codes of Status.Banners.
const (
	BannerKeyMissing       = "encryption_key_missing"
	BannerKeyMismatch      = "key_mismatch"
	BannerSigningMissing   = "signing_key_missing"
	BannerSigningUnreadble = "signing_key_unreadable"
	BannerNoRecipient      = "no_active_recipient"
	BannerNoRecentDump     = "no_recent_dump"
	// LAUN-75: a destination is overdue (the stale rule of #41), its newest backup
	// run failed for good, or mail is off so no notice or alert is sent.
	BannerStale     = "backup_stale"
	BannerRunFailed = "backup_run_failed"
	BannerMailOff   = "mail_off"
	// BannerFilesNotBackedUp (LAUN-80): the server keeps attachments and an enabled
	// destination has no good files backup in the last FilesStaleHours.
	BannerFilesNotBackedUp = "files_not_backed_up"
)

// StaleDestination is an enabled destination whose last success is older than
// the stale threshold (decisions-T16 #41).
// FilesDestination is an enabled destination without a recent good files backup.
type FilesDestination struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// LastBackupAt is the finish of the newest run whose files step succeeded, or null.
	LastBackupAt *time.Time `json:"lastBackupAt"`
}

// FilesStaleHours: a files backup older than this is "not backed up". The dump
// runs once a day; 48 hours leaves one missed night.
const FilesStaleHours = 48

type StaleDestination struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// LastSuccessAt is the finish of its newest counted success, or null.
	LastSuccessAt *time.Time `json:"lastSuccessAt"`
	// Since is where the stale clock counts from.
	Since time.Time `json:"since"`
	// ThresholdHours is max(staleAlertHours, 2 x its schedule interval).
	ThresholdHours int `json:"thresholdHours"`
	// LastAlertAt is when the last stale mail was claimed, or null.
	LastAlertAt *time.Time `json:"lastAlertAt"`
}

// FailedDestination is an enabled destination whose newest backup run failed and
// will not be retried.
type FailedDestination struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	RunID      uuid.UUID `json:"runId"`
	FinishedAt time.Time `json:"finishedAt"`
	ErrorCode  string    `json:"errorCode"`
	ErrorText  string    `json:"errorText"`
}

// Status is GET .../status: what exists now (decisions-T16 #43).
type Status struct {
	// EncryptionKeySet: BACKUP_ENCRYPTION_KEY is set on this server.
	EncryptionKeySet bool `json:"encryptionKeySet"`
	// KeyMismatch: the stored key check differs from this server's key.
	KeyMismatch bool `json:"keyMismatch"`
	// SigningPublic is empty until offsite makes the signing key.
	SigningPublic string `json:"signingPublic"`
	// SigningKeyUnreadable: a signing key is stored and this server cannot open it.
	SigningKeyUnreadable bool `json:"signingKeyUnreadable"`
	// SupportedTypes are the destination types this build has a driver for.
	SupportedTypes   []string    `json:"supportedTypes"`
	Recipients       []Recipient `json:"recipients"`
	ActiveRecipients int         `json:"activeRecipients"`
	EverHadRecipient bool        `json:"everHadRecipient"`
	Destinations     int         `json:"destinations"`
	EnabledCount     int         `json:"enabledDestinations"`
	// DestinationsUnreadable counts destinations whose stored secrets do not open.
	DestinationsUnreadable int `json:"destinationsUnreadable"`
	// NewestDump is the newest local dump row, or null.
	NewestDump *Dump `json:"newestDump"`
	// NoRecentDump: no ok dump finished in the last 36 hours.
	NoRecentDump bool `json:"noRecentDump"`
	// MailReady: the platform mail transport can send. False: notices and stale
	// alerts are not sent ("alerts are not sent: mail is off").
	MailReady bool `json:"mailReady"`
	// StaleAlertHours is the setting (0: the stale rule and its banner are off).
	StaleAlertHours int `json:"staleAlertHours"`
	// StaleDestinations are the enabled destinations that are stale now.
	StaleDestinations []StaleDestination `json:"staleDestinations"`
	// FailedDestinations are the enabled destinations whose newest backup run failed for good.
	FailedDestinations []FailedDestination `json:"failedDestinations"`
	// FilesStored: this server keeps attachments, so they need a backup (LAUN-80).
	FilesStored bool `json:"filesStored"`
	// FilesNotBackedUp are the enabled destinations with no good files backup in
	// the last FilesStaleHours (only when FilesStored).
	FilesNotBackedUp []FilesDestination `json:"filesNotBackedUp"`
	Banners          []string           `json:"banners"`
}
