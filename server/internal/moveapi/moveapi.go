// Package moveapi is the HTTP side of moving an install (LAUN-249,
// docs/internal/decisions-T46-move-server.md): the export ticket, the export
// stream and the restore. The bundle format is internal/move.
//
// Export tickets live in MEMORY, in this process only. A ticket is a
// 256-bit random value; the table is keyed by its sha256, so the value is
// never held after it is returned. The passphrase the ticket will encrypt
// with is held next to the key until the ticket is used or expires (the
// export is made at download time, so the passphrase must outlive the POST).
// Neither is ever written to the database, a log line, an event or an
// idempotency row. One process serves an install, so a ticket is only good on
// the process that made it; a restart drops every ticket.
package moveapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/move"
	"devops/internal/setup"
	"devops/internal/tracker"
)

// TicketTTL is how long a download ticket works.
const TicketTTL = 5 * time.Minute

// SessionTTL is the life of the session a restore opens (as a login).
const SessionTTL = 30 * 24 * time.Hour

// ticketBytes is the entropy of a ticket: 256 bits.
const ticketBytes = 32

// FileExt is the extension of a bundle file.
const FileExt = ".lpmove"

var ticketLen = base64.RawURLEncoding.EncodedLen(ticketBytes)

// Options configure the Service.
type Options struct {
	Pool       *pgxpool.Pool
	Keys       move.Keys
	Store      move.ObjectStore // nil: no attachment files
	AppVersion string
	Log        *slog.Logger
	Now        func() time.Time // nil: time.Now
	TTL        time.Duration    // 0: TicketTTL
	// ScryptWorkFactor: see move.ExportOptions. Tests only.
	ScryptWorkFactor int
}

type ticket struct {
	user       uuid.UUID
	passphrase string
	attach     bool
	filename   string
	expires    time.Time
}

// Service makes tickets, streams exports and restores bundles. It is safe for
// concurrent use.
type Service struct {
	o       Options
	mu      sync.Mutex
	tickets map[[32]byte]*ticket
}

// New returns a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TTL <= 0 {
		o.TTL = TicketTTL
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{o: o, tickets: map[[32]byte]*ticket{}}
}

// ErrNoTicket: the ticket is unknown, used, expired or another person's.
// The cases are not told apart.
var ErrNoTicket = httpx.NotFound("export ticket")

// ErrBusy: another export is streaming.
var ErrBusy = httpx.Errorf(http.StatusConflict, httpx.CodeExportInProgress,
	"another export is running; wait for it to finish and start again")

// StartResult is the POST /platform/move/export 201 body. Passphrase and
// Ticket are shown here once and stored nowhere (the ticket only as sha256,
// in memory).
type StartResult struct {
	Passphrase         string    `json:"passphrase"`
	Ticket             string    `json:"ticket"`
	DownloadPath       string    `json:"downloadPath"`
	Filename           string    `json:"filename"`
	ExpiresAt          time.Time `json:"expiresAt"`
	IncludeAttachments bool      `json:"includeAttachments"`
}

// DownloadPrefix is the path a ticket is appended to.
const DownloadPrefix = "/api/v1/platform/move/export/"

func hashTicket(t string) [32]byte { return sha256.Sum256([]byte(t)) }

func wellFormedTicket(t string) bool {
	if len(t) != ticketLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(t)
	return err == nil && len(b) == ticketBytes
}

// StartExport makes a passphrase and a one-time ticket for user.  includeAttachments nil
// means "yes when the install has an object store".
func (s *Service) StartExport(user uuid.UUID, includeAttachments *bool) (StartResult, error) {
	attach := s.o.Store != nil
	if includeAttachments != nil {
		attach = *includeAttachments
	}
	if attach && s.o.Store == nil {
		return StartResult{}, httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked,
			"this server has no object store (S3_*), so there are no attachment files to include; set includeAttachments to false")
	}
	pass, err := move.GeneratePassphrase()
	if err != nil {
		return StartResult{}, err
	}
	raw := make([]byte, ticketBytes)
	if _, err := rand.Read(raw); err != nil {
		return StartResult{}, fmt.Errorf("read random: %w", err)
	}
	tk := base64.RawURLEncoding.EncodeToString(raw)
	now := s.o.Now()
	t := &ticket{user: user, passphrase: pass, attach: attach, expires: now.Add(s.o.TTL),
		filename: "launchpad-" + now.UTC().Format("20060102-150405") + FileExt}
	h := hashTicket(tk)
	s.mu.Lock()
	// One live ticket per user: a new start drops that user's older one (and
	// every expired one), never another admin's. The map stays bounded.
	s.purgeLocked(now, &user)
	s.tickets[h] = t
	s.mu.Unlock()
	// The passphrase must not outlive the TTL even when nobody calls again.
	time.AfterFunc(s.o.TTL, func() {
		s.mu.Lock()
		if s.tickets[h] == t {
			delete(s.tickets, h)
		}
		s.mu.Unlock()
	})
	return StartResult{Passphrase: pass, Ticket: tk, DownloadPath: DownloadPrefix + tk, Filename: t.filename,
		ExpiresAt: t.expires, IncludeAttachments: attach}, nil
}

// purgeLocked deletes every expired ticket, and (user non-nil) that user's
// tickets. s.mu must be held.
func (s *Service) purgeLocked(now time.Time, user *uuid.UUID) {
	for h, t := range s.tickets {
		if !now.Before(t.expires) || (user != nil && t.user == *user) {
			delete(s.tickets, h)
		}
	}
}

// peek returns the live ticket without using it.
func (s *Service) peek(tk string, user uuid.UUID) (*ticket, error) {
	if !wellFormedTicket(tk) {
		return nil, ErrNoTicket
	}
	h := hashTicket(tk)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(s.o.Now(), nil)
	t, ok := s.tickets[h]
	if !ok || t.user != user {
		return nil, ErrNoTicket
	}
	return t, nil
}

// take uses the ticket: it is gone after this, whatever the export does.
func (s *Service) take(tk string, user uuid.UUID) (*ticket, error) {
	t, err := s.peek(tk, user)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := hashTicket(tk)
	if s.tickets[h] != t { // a concurrent download took it
		return nil, ErrNoTicket
	}
	delete(s.tickets, h)
	return t, nil
}

// Filename is the download name of a live ticket (it does not use it). It is
// for the Content-Disposition header.
func (s *Service) Filename(tk string, user uuid.UUID) (string, error) {
	t, err := s.peek(tk, user)
	if err != nil {
		return "", err
	}
	return t.filename, nil
}

// exportLockKey is the key of the one-export-at-a-time advisory lock (a
// session lock, so it covers the whole stream and dies with a lost connection).
const exportLockKey = `hashtext('devops-move-export')`

// Export checks the ticket, takes the export lock (ErrBusy when another
// export holds it; the ticket is then NOT used), uses the ticket and streams
// the bundle to w. The ticket stays valid until the lock is won.
func (s *Service) Export(ctx context.Context, tk string, user uuid.UUID, w io.Writer) (*move.Manifest, error) {
	if _, err := s.peek(tk, user); err != nil {
		return nil, err
	}
	conn, err := s.o.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+exportLockKey+`)`).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("export lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, ErrBusy
	}
	defer func() {
		bg := context.WithoutCancel(ctx)
		var ok bool
		if err := conn.QueryRow(bg, `SELECT pg_advisory_unlock(`+exportLockKey+`)`).Scan(&ok); err != nil || !ok {
			// Do not return a connection that may still hold the lock.
			_ = conn.Hijack().Close(bg)
			return
		}
		conn.Release()
	}()
	t, err := s.take(tk, user)
	if err != nil {
		return nil, err
	}
	m, err := move.Export(ctx, w, move.ExportOptions{
		Pool: s.o.Pool, Keys: s.o.Keys, Store: s.o.Store, Passphrase: t.passphrase,
		ExportedBy: user, AppVersion: s.o.AppVersion, IncludeAttachments: t.attach, Log: s.o.Log, Now: s.o.Now, ScryptWorkFactor: s.o.ScryptWorkFactor,
	})
	if err != nil {
		return nil, MapMoveError(err)
	}
	return m, nil
}

// Summary is what a restore reports of the bundle: counts only.
type Summary struct {
	MigrationVersion    uint      `json:"migrationVersion"`
	AppVersion          string    `json:"appVersion"`
	ExportedAt          time.Time `json:"exportedAt"`
	Tables              int       `json:"tables"`
	Rows                int64     `json:"rows"`
	Objects             int64     `json:"objects"`
	AttachmentsIncluded bool      `json:"attachmentsIncluded"`
}

// RestoreResponse is the POST /setup/restore 201 body: the login body (as
// POST /setup) plus a summary of the bundle. token.secret is shown once.
type RestoreResponse struct {
	User      login.User           `json:"user"`
	Orgs      []login.Org          `json:"orgs"`
	CSRFToken string               `json:"csrfToken"`
	Token     tracker.CreatedToken `json:"token"`
	Summary   Summary              `json:"summary"`
}

// restoreTokenName names the token a restore mints.
const restoreTokenName = "restore admin token"

// Restore imports the bundle in r into this empty install. The setup link is
// used inside the import transaction (so two restores cannot both use it);
// the session and the admin token are made in the same transaction, for the
// bundle's exporter when that person is still an active platform admin and
// an org admin, else the oldest such person. With none the restore rolls back
// (move_blocked): the install stays in setup state.
func (s *Service) Restore(ctx context.Context, r io.Reader, passphrase, setupToken string) (login.Session, RestoreResponse, error) {
	var (
		sess login.Session
		body RestoreResponse
	)
	m, err := move.Import(ctx, r, move.ImportOptions{
		Pool: s.o.Pool, Keys: s.o.Keys, Store: s.o.Store, Passphrase: passphrase, Log: s.o.Log,
		Authorize: func(ctx context.Context, tx pgx.Tx) error {
			return setup.ConsumeLinkTx(ctx, dbq.New(tx), setupToken)
		},
		Finish: func(ctx context.Context, tx pgx.Tx, m *move.Manifest) error {
			var err error
			sess, body, err = s.openAccess(ctx, tx, m)
			return err
		},
	})
	if err != nil {
		return login.Session{}, RestoreResponse{}, MapMoveError(err)
	}
	body.Summary = Summary{MigrationVersion: m.MigrationVersion, AppVersion: m.AppVersion, ExportedAt: m.ExportedAt,
		Tables: len(m.Tables), Rows: m.TotalRows(), Objects: m.ObjectCount, AttachmentsIncluded: m.AttachmentsIncluded}
	s.o.Log.Info("move: restore opened", "rows", body.Summary.Rows, "objects", body.Summary.Objects)
	return sess, body, nil
}

var errNoAdmin = httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked,
	"the bundle has no active human platform admin who is also an org admin, so no one could log in; nothing was restored")

var firstTokenScopes = []string{string(auth.ScopeRead), string(auth.ScopeWrite), string(auth.ScopeAdmin)}

// openAccess runs in the import transaction, after the rows are loaded.
func (s *Service) openAccess(ctx context.Context, tx pgx.Tx, m *move.Manifest) (login.Session, RestoreResponse, error) {
	var by *uuid.UUID
	if m.ExportedBy != nil {
		by = m.ExportedBy
	}
	var (
		u     login.User
		email *string
	)
	err := tx.QueryRow(ctx, `/* scope: system */
SELECT u.id, u.username, u.name, u.email
FROM users u
WHERE u.is_platform_admin AND NOT u.is_agent AND u.deactivated_at IS NULL
  AND EXISTS (SELECT 1 FROM org_members m WHERE m.user_id = u.id AND m.role = 'admin')
ORDER BY (u.id = $1::uuid) DESC, u.created_at, u.id
LIMIT 1`, by).Scan(&u.ID, &u.Username, &u.Name, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return login.Session{}, RestoreResponse{}, errNoAdmin
	}
	if err != nil {
		return login.Session{}, RestoreResponse{}, fmt.Errorf("pick restore admin: %w", err)
	}
	if email != nil {
		u.Email = *email
	}
	u.IsPlatformAdmin = true
	rows, err := tx.Query(ctx, `/* scope: system */
SELECT o.id, o.slug, o.name, m.role
FROM org_members m JOIN orgs o ON o.id = m.org_id
WHERE m.user_id = $1 ORDER BY o.slug`, u.ID)
	if err != nil {
		return login.Session{}, RestoreResponse{}, fmt.Errorf("list orgs: %w", err)
	}
	orgs := []login.Org{}
	for rows.Next() {
		var o login.Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.Role); err != nil {
			rows.Close()
			return login.Session{}, RestoreResponse{}, fmt.Errorf("scan org: %w", err)
		}
		orgs = append(orgs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return login.Session{}, RestoreResponse{}, fmt.Errorf("list orgs: %w", err)
	}
	// The token is pinned to the first org where the user is an admin.
	var pin *login.Org
	for i := range orgs {
		if orgs[i].Role == "admin" {
			pin = &orgs[i]
			break
		}
	}
	if pin == nil {
		return login.Session{}, RestoreResponse{}, errNoAdmin
	}
	q := dbq.New(tx)
	sess, err := login.InsertSessionTx(ctx, q, u.ID, login.SessionScopes(true), s.o.Now().Add(SessionTTL))
	if err != nil {
		return login.Session{}, RestoreResponse{}, err
	}
	nt, err := auth.GenerateToken()
	if err != nil {
		return login.Session{}, RestoreResponse{}, err
	}
	row, err := q.InsertAPIToken(ctx, dbq.InsertAPITokenParams{
		UserID: u.ID, OrgID: &pin.ID, Kind: auth.KindPAT, Name: restoreTokenName, TokenHash: nt.Hash,
		Prefix: nt.Prefix, Scopes: append([]string(nil), firstTokenScopes...), CreatedBy: &u.ID,
	})
	if err != nil {
		return login.Session{}, RestoreResponse{}, fmt.Errorf("insert restore token: %w", err)
	}
	return sess, RestoreResponse{
		User: u, Orgs: orgs, CSRFToken: sess.CSRF,
		Token: tracker.CreatedToken{
			Token: tracker.Token{ID: row.ID, Kind: row.Kind, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
				User: tracker.UserRef{ID: u.ID, Username: u.Username, Name: u.Name}, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt},
			Secret: nt.Plaintext,
		},
	}, nil
}

// MapMoveError turns a move error into a wire error. Texts are fixed: none
// holds a secret, a passphrase, a key name of the object store or a row.
func MapMoveError(err error) error {
	var he *httpx.Error
	if errors.As(err, &he) {
		return err
	}
	var ver *move.VersionError
	var km *move.KeyMissingError
	var us *move.UnreadableSecretError
	var oe *move.ObjectExistsError
	var om *move.ObjectMissingError
	switch {
	case errors.Is(err, move.ErrBadPassphrase):
		return httpx.Errorf(http.StatusBadRequest, httpx.CodeBadPassphrase, "%s", move.ErrBadPassphrase.Error())
	case errors.Is(err, move.ErrBundleCorrupt), errors.Is(err, move.ErrChecksum):
		return httpx.Errorf(http.StatusBadRequest, httpx.CodeBundleInvalid, "the bundle is damaged, incomplete, changed or not in a format this server reads")
	case errors.Is(err, move.ErrNotEmpty):
		return setup.ErrComplete
	case errors.As(err, &ver):
		return httpx.Errorf(http.StatusConflict, httpx.CodeBundleVersionMismatch, "%s", ver.Error())
	case errors.As(err, &km):
		return httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked, "%s", km.Error())
	case errors.As(err, &us):
		return httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked, "%s", us.Error())
	case errors.Is(err, move.ErrNoStore):
		return httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked, "%s", move.ErrNoStore.Error())
	case errors.As(err, &oe):
		return httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked, "the object store already holds a file the restore would write; use an empty bucket")
	case errors.As(err, &om):
		return httpx.Errorf(http.StatusConflict, httpx.CodeMoveBlocked, "the object store lacks a file an attachment row points at; export without attachments, or repair the store")
	}
	return err
}
