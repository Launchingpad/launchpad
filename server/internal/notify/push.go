package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/platform"
)

// Mobile push constants (decisions-T35). Constants in v1, not settings.
const (
	// PushMaxDevices is the most devices one member may register in one org.
	PushMaxDevices = 10
	// PushDeviceRetention: a device not seen for this long is deleted (on the
	// owner's next registration and by the sender's prune).
	PushDeviceRetention = 90 * 24 * time.Hour
	// MaxPushToken bounds a device token.
	MaxPushToken = 4096
)

// PushService is the push API: the admin's relay setting (read state, set,
// remove, test) and a member's own devices. The relay URL, the relay secret
// and the device tokens are write-only: no method returns them.
type PushService struct {
	pool      *pgxpool.Pool
	q         *dbq.Queries
	keys      *platform.PushKeyring // nil: no SETTINGS_ENCRYPTION_KEY
	poster    *PushPoster
	log       *slog.Logger
	now       func() time.Time
	testLimit *login.RouteLimit
}

// NewPushService returns the service. keys may be nil (the key is not set):
// the relay can then be neither set nor opened, and no device can register.
func NewPushService(pool *pgxpool.Pool, log *slog.Logger, keys *platform.PushKeyring, poster *PushPoster) *PushService {
	return &PushService{pool: pool, q: dbq.New(pool), keys: keys, poster: poster, log: log, now: time.Now,
		// The test push: 6 a minute, burst 3, per process (like the mail and Slack tests).
		testLimit: login.NewRouteLimit(6, 3, nil)}
}

// SetClock sets the clock (tests).
func (s *PushService) SetClock(now func() time.Time) { s.now = now }

// PushView is the GET /push body. It never holds the URL, the secret, a part
// of either, or a hash.
type PushView struct {
	// Available: this server can store a relay (SETTINGS_ENCRYPTION_KEY is set).
	Available  bool       `json:"available"`
	Configured bool       `json:"configured"`
	Host       *string    `json:"host"`
	LastOkAt   *time.Time `json:"lastOkAt"`
	LastError  *string    `json:"lastError"`
	// SecretUnreadable: a stored blob does not open with this server's key.
	SecretUnreadable bool       `json:"secretUnreadable"`
	UpdatedAt        *time.Time `json:"updatedAt"`
	// Counts only: devices in the org, pushes the hourly cap dropped in the
	// last 24 hours, and pushes that ended dead.
	DeviceCount     int64 `json:"deviceCount"`
	RateCappedCount int64 `json:"rateCappedCount"`
	DeadCount       int64 `json:"deadCount"`
}

// PushInput is the PUT /push body. An absent or null field keeps the stored
// value; a value replaces it. A new relay needs both.
type PushInput struct {
	RelayURL    *string `json:"relayUrl"`
	RelaySecret *string `json:"relaySecret"`
}

// PushTestResult is the 200 body of the test push.
type PushTestResult struct {
	Sent    int `json:"sent"`
	Removed int `json:"removed"`
}

func (s *PushService) openRelay(org uuid.UUID, row *dbq.PushRelay) (url, secret string, err error) {
	if s.keys == nil {
		return "", "", platform.ErrUnreadable
	}
	u, err := s.keys.OpenFor(platform.PushColRelayURL, org.String(), row.UrlEnc)
	if err != nil {
		return "", "", err
	}
	sec, err := s.keys.OpenFor(platform.PushColRelaySecret, org.String(), row.SecretEnc)
	if err != nil {
		return "", "", err
	}
	return string(u), string(sec), nil
}

// Get is GET /push.
func (s *PushService) Get(ctx context.Context, org uuid.UUID) (PushView, error) {
	v := PushView{Available: s.keys != nil}
	row, err := s.q.GetPushRelay(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return PushView{}, fmt.Errorf("push relay: %w", err)
	}
	host := row.Host
	ua := row.UpdatedAt
	v.Configured, v.Host, v.UpdatedAt = true, &host, &ua
	v.LastOkAt, v.LastError = row.LastOkAt, row.LastError
	if _, _, err := s.openRelay(org, &row); err != nil {
		v.SecretUnreadable = true
	}
	if v.DeviceCount, err = s.q.CountPushDevicesOfOrg(ctx, org); err != nil {
		return PushView{}, fmt.Errorf("push device count: %w", err)
	}
	if v.RateCappedCount, err = s.q.CountPushRateCapped(ctx, dbq.CountPushRateCappedParams{OrgID: org, Since: s.now().Add(-24 * time.Hour)}); err != nil {
		return PushView{}, fmt.Errorf("push rate cap count: %w", err)
	}
	if v.DeadCount, err = s.q.CountDeadPushOutbox(ctx, org); err != nil {
		return PushView{}, fmt.Errorf("push dead count: %w", err)
	}
	return v, nil
}

// Put is PUT /push.
func (s *PushService) Put(ctx context.Context, org uuid.UUID, p *auth.Principal, in PushInput) (PushView, error) {
	if in.RelayURL == nil && in.RelaySecret == nil {
		return PushView{}, httpx.Invalid("relayUrl or relaySecret is required")
	}
	var newURL, newSecret []byte
	var host string
	if in.RelayURL != nil {
		u, err := ParsePushRelayURL(*in.RelayURL)
		if err != nil {
			return PushView{}, httpx.Invalid("relayUrl: %s", err.Error())
		}
		if err := CheckRelayHostLiteral(u); err != nil {
			return PushView{}, httpx.Invalid("relayUrl: %s", err.Error())
		}
		host, newURL = relayHost(u), []byte(*in.RelayURL)
	}
	if in.RelaySecret != nil {
		if err := ValidRelaySecret(*in.RelaySecret); err != nil {
			return PushView{}, httpx.Invalid("relaySecret: %s", err.Error())
		}
		newSecret = []byte(*in.RelaySecret)
	}
	if s.keys == nil {
		return PushView{}, httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
			"this server has no SETTINGS_ENCRYPTION_KEY, so it cannot store the push relay")
	}
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, gerr := q.GetPushRelayForUpdate(ctx, org)
		exists := gerr == nil
		if gerr != nil && !errors.Is(gerr, pgx.ErrNoRows) {
			return fmt.Errorf("push relay: %w", gerr)
		}
		urlEnc, secEnc := cur.UrlEnc, cur.SecretEnc
		if !exists && (newURL == nil || newSecret == nil) {
			return httpx.Invalid("relayUrl and relaySecret are both required to turn push on")
		}
		if newURL != nil && exists && host != cur.Host && newSecret == nil {
			// The kept secret must never go to a host the admin has just named.
			return httpx.Invalid("relaySecret: enter the secret again when the relay host changes")
		}
		if newURL != nil {
			urlEnc = s.keys.SealFor(platform.PushColRelayURL, org.String(), newURL)
		} else {
			host = cur.Host
		}
		if newSecret != nil {
			secEnc = s.keys.SealFor(platform.PushColRelaySecret, org.String(), newSecret)
		}
		if _, err := q.UpsertPushRelay(ctx, dbq.UpsertPushRelayParams{OrgID: org, UrlEnc: urlEnc, SecretEnc: secEnc, Host: host, UpdatedBy: &p.UserID}); err != nil {
			return fmt.Errorf("write push relay: %w", err)
		}
		return emitPush(ctx, q, org, p, true)
	})
	if err != nil {
		return PushView{}, err
	}
	return s.Get(ctx, org)
}

// Delete is DELETE /push: the relay and its waiting pushes go. A missing
// relay is not an error and emits nothing.
func (s *PushService) Delete(ctx context.Context, org uuid.UUID, p *auth.Principal) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		n, err := q.DeletePushRelay(ctx, org)
		if err != nil {
			return fmt.Errorf("delete push relay: %w", err)
		}
		if n == 0 {
			return nil
		}
		if _, err := q.DeleteOpenPushOutbox(ctx, org); err != nil {
			return fmt.Errorf("delete open pushes: %w", err)
		}
		return emitPush(ctx, q, org, p, false)
	})
}

// emitPush is the LAST statement of a write. The payload holds no secret.
func emitPush(ctx context.Context, q *dbq.Queries, org uuid.UUID, p *auth.Principal, configured bool) error {
	_, err := events.Emit(ctx, q, org, events.Event{Type: events.PushUpdated, EntityType: "org", EntityID: org,
		ActorID: &p.UserID, Payload: map[string]any{"configured": configured}}, events.ScopeOrg())
	return err
}

// Test is POST /push/test: one wake to each of the caller's own devices in
// this org. No device is 409. If nothing was sent the answer is 502
// push_send_failed with the fixed word of the last failure.
func (s *PushService) Test(ctx context.Context, org uuid.UUID, p *auth.Principal) (PushTestResult, error) {
	if err := s.testLimit.Take(); err != nil {
		return PushTestResult{}, err
	}
	row, err := s.q.GetPushRelay(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return PushTestResult{}, httpx.NotFound("push relay")
	}
	if err != nil {
		return PushTestResult{}, fmt.Errorf("push relay: %w", err)
	}
	relayURL, secret, oerr := s.openRelay(org, &row)
	if oerr != nil {
		s.record(ctx, org, PushUnreadable)
		return PushTestResult{}, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"the stored push relay cannot be opened with this server's key; enter it again")
	}
	devs, err := s.q.ListPushDevices(ctx, dbq.ListPushDevicesParams{OrgID: org, UserID: p.UserID})
	if err != nil {
		return PushTestResult{}, fmt.Errorf("push devices: %w", err)
	}
	if len(devs) == 0 {
		return PushTestResult{}, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"you have no registered device; open the app and sign in first")
	}
	var res PushTestResult
	lastWord := ""
	for _, d := range devs {
		tok, terr := s.keys.OpenFor(platform.PushColDeviceToken, deviceScope(org, d.ID), d.TokenEnc)
		if terr != nil {
			lastWord = PushUnreadable
			continue
		}
		perr := s.poster.Wake(ctx, relayURL, secret, PushWake{Token: string(tok), Platform: d.Platform, ID: uuid.NewString()})
		if perr == nil {
			res.Sent++
			continue
		}
		word := PushUnreachable
		var pe *PushError
		if errors.As(perr, &pe) {
			word = pe.Word
		}
		lastWord = word
		if word == PushTokenInvalid {
			// Only while the device still holds the token that was sent.
			sentHash := sha256.Sum256(tok)
			if n, derr := s.q.DeletePushDeviceIfToken(ctx, dbq.DeletePushDeviceIfTokenParams{OrgID: org, ID: d.ID, TokenHash: sentHash[:]}); derr == nil {
				res.Removed += int(n)
			}
		}
	}
	switch {
	case res.Sent > 0:
		s.record(ctx, org, "")
		return res, nil
	case lastWord == PushTokenInvalid:
		// The relay answered; the tokens were stale and are gone. The relay is fine.
		s.record(ctx, org, "")
	default:
		s.record(ctx, org, lastWord)
	}
	// The word only: no URL, no token, no reply.
	s.log.WarnContext(ctx, "push test failed", "org_id", org, "reason", lastWord)
	return PushTestResult{}, httpx.Errorf(http.StatusBadGateway, httpx.CodePushSendFailed, "the test push was not sent: %s", lastWord)
}

func (s *PushService) record(ctx context.Context, org uuid.UUID, word string) {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	if err := recordPushResult(rctx, s.q, org, s.now(), word); err != nil {
		s.log.WarnContext(ctx, "push result not stored", "org_id", org)
	}
}

func recordPushResult(ctx context.Context, q *dbq.Queries, org uuid.UUID, at time.Time, word string) error {
	arg := dbq.SetPushRelayResultParams{OrgID: org, Ok: word == "", At: at}
	if word != "" {
		arg.LastError = &word
	}
	_, err := q.SetPushRelayResult(ctx, arg)
	return err
}

// --- a member's own devices --------------------------------------------------

// DeviceInput is the PUT /me/push-devices/{deviceId} body.
type DeviceInput struct {
	Platform   string `json:"platform"`
	Token      string `json:"token"`
	AppVersion string `json:"appVersion"`
	Label      string `json:"label"`
}

// DeviceView is one device. It never holds the token.
type DeviceView struct {
	ID         uuid.UUID `json:"id"`
	Platform   string    `json:"platform"`
	Label      string    `json:"label"`
	AppVersion string    `json:"appVersion"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

// DeviceList is the GET /me/push-devices body.
type DeviceList struct {
	// RelayConfigured: the admin has set a relay, so a push can be sent.
	RelayConfigured bool         `json:"relayConfigured"`
	MaxDevices      int          `json:"maxDevices"`
	Items           []DeviceView `json:"items"`
}

func deviceScope(org, device uuid.UUID) string { return org.String() + "." + device.String() }

func deviceView(d dbq.PushDevice) DeviceView {
	return DeviceView{ID: d.ID, Platform: d.Platform, Label: d.Label, AppVersion: d.AppVersion, CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt}
}

func checkDevice(in DeviceInput) error {
	if in.Platform != "ios" && in.Platform != "android" {
		return httpx.Invalid("platform must be ios or android")
	}
	if in.Token == "" || len(in.Token) > MaxPushToken {
		return httpx.Invalid("token must be 1 to 4096 characters")
	}
	for _, r := range in.Token {
		if r <= ' ' || r >= 0x7f {
			return httpx.Invalid("token must be printable ASCII without spaces")
		}
	}
	if utf8.RuneCountInString(in.AppVersion) > 64 || utf8.RuneCountInString(in.Label) > 80 {
		return httpx.Invalid("appVersion is at most 64 and label at most 80 characters")
	}
	for _, v := range []string{in.AppVersion, in.Label} {
		if !utf8.ValidString(v) {
			return httpx.Invalid("appVersion and label must be valid text")
		}
		for _, r := range v {
			if r < ' ' || r == 0x7f {
				return httpx.Invalid("appVersion and label must not contain control characters")
			}
		}
	}
	return nil
}

// ListDevices is GET /me/push-devices: the caller's own devices, never a token.
func (s *PushService) ListDevices(ctx context.Context, org, user uuid.UUID) (DeviceList, error) {
	rows, err := s.q.ListPushDevices(ctx, dbq.ListPushDevicesParams{OrgID: org, UserID: user})
	if err != nil {
		return DeviceList{}, fmt.Errorf("list push devices: %w", err)
	}
	configured, err := s.q.PushRelayConfigured(ctx, org)
	if err != nil {
		return DeviceList{}, fmt.Errorf("push relay: %w", err)
	}
	out := DeviceList{RelayConfigured: configured, MaxDevices: PushMaxDevices, Items: make([]DeviceView, 0, len(rows))}
	for _, d := range rows {
		out.Items = append(out.Items, deviceView(d))
	}
	return out, nil
}

// PutDevice is PUT /me/push-devices/{id}: register a device or refresh it. It
// emits no event (a per-user private row). The token is sealed, hashed for
// uniqueness, and never returned. A device id that belongs to another member
// or another org is 404. A new device over the cap is 409.
func (s *PushService) PutDevice(ctx context.Context, org, user, id uuid.UUID, in DeviceInput) (DeviceView, error) {
	if err := checkDevice(in); err != nil {
		return DeviceView{}, err
	}
	if s.keys == nil {
		return DeviceView{}, httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
			"this server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a device token")
	}
	sum := sha256.Sum256([]byte(in.Token))
	sealed := s.keys.SealFor(platform.PushColDeviceToken, deviceScope(org, id), []byte(in.Token))
	var out DeviceView
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Serialise one member's registrations, so the cap cannot be passed by two requests.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:push-devices:' || $1::text || ':' || $2::text, 0))`, org, user); err != nil {
			return fmt.Errorf("lock push devices: %w", err)
		}
		// Serialise one token across members: two members who register the same
		// phone at once must not hit the unique key (a 500).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:push-token:' || $1::text || ':' || $2::text, 0))`, org, hex.EncodeToString(sum[:])); err != nil {
			return fmt.Errorf("lock push token: %w", err)
		}
		now := s.now()
		// Retention on write: this member's devices not seen for 90 days go.
		if _, err := q.DeleteStalePushDevicesOfUser(ctx, dbq.DeleteStalePushDevicesOfUserParams{OrgID: org, UserID: user, Cutoff: now.Add(-PushDeviceRetention)}); err != nil {
			return fmt.Errorf("delete stale push devices: %w", err)
		}
		// One row per token in an org: a re-registration under another id takes it over. This runs before the cap check, so a member at the cap who
		// re-registers their own token under a new id keeps the same count (no 409).
		if _, err := q.DeletePushDevicesByTokenHash(ctx, dbq.DeletePushDevicesByTokenHashParams{OrgID: org, TokenHash: sum[:], KeepID: id}); err != nil {
			return fmt.Errorf("delete duplicate push token: %w", err)
		}
		cur, err := q.GetPushDevice(ctx, dbq.GetPushDeviceParams{OrgID: org, ID: id})
		switch {
		case err == nil:
			if cur.UserID != user {
				return httpx.NotFound("push device")
			}
		case errors.Is(err, pgx.ErrNoRows):
			n, cerr := q.CountPushDevices(ctx, dbq.CountPushDevicesParams{OrgID: org, UserID: user})
			if cerr != nil {
				return fmt.Errorf("count push devices: %w", cerr)
			}
			if n >= PushMaxDevices {
				return httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
					"you already have %d registered devices; remove one first", PushMaxDevices)
			}
		default:
			return fmt.Errorf("push device: %w", err)
		}
		d, err := q.UpsertPushDevice(ctx, dbq.UpsertPushDeviceParams{ID: id, OrgID: org, UserID: user, Platform: in.Platform,
			TokenEnc: sealed, TokenHash: sum[:], AppVersion: in.AppVersion, Label: in.Label, Now: now})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("push device")
		}
		if err != nil {
			return fmt.Errorf("write push device: %w", err)
		}
		out = deviceView(d)
		return nil
	})
	return out, err
}

// DeleteDevice is DELETE /me/push-devices/{id}. Another member's or another
// org's id, or a missing one, is 404.
func (s *PushService) DeleteDevice(ctx context.Context, org, user, id uuid.UUID) error {
	n, err := s.q.DeleteMyPushDevice(ctx, dbq.DeleteMyPushDeviceParams{OrgID: org, UserID: user, ID: id})
	if err != nil {
		return fmt.Errorf("delete push device: %w", err)
	}
	if n == 0 {
		return httpx.NotFound("push device")
	}
	return nil
}
