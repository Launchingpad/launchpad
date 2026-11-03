package backupapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/backup/driver"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/platform"
)

func runView(r dbq.BackupRun) Run {
	steps := r.Steps
	if len(steps) == 0 {
		steps = json.RawMessage(`[]`)
	}
	return Run{ID: r.ID, DestinationID: r.DestinationID, DestinationName: r.DestinationName, DumpID: r.DumpID,
		Trigger: r.Trigger, Status: r.Status, Attempt: int(r.Attempt), WillRetry: r.WillRetry, RetryAt: r.RetryAt,
		QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Bytes: r.Bytes, SHA256: r.Sha256,
		RemoteName: r.RemoteName, Steps: steps, ErrorCode: r.ErrorCode, ErrorText: r.ErrorText}
}

// Paging (runs and changes): newest first, keyset on (time, id), an opaque cursor.
const (
	defaultPage = 50
	maxPage     = 200
)

type cursor struct {
	At time.Time `json:"a"`
	ID uuid.UUID `json:"i"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.At.IsZero() || c.ID == uuid.Nil {
		return nil, invalid("malformed cursor")
	}
	return &c, nil
}

// PageQuery is the parsed query of the run and change lists.
type PageQuery struct {
	Limit       int
	Cursor      *cursor
	Destination *uuid.UUID // runs only
}

// ParsePageQuery reads limit, cursor and (when withDestination) destination.
// Anything else is 400.
func ParsePageQuery(v url.Values, withDestination bool) (PageQuery, error) {
	pq := PageQuery{Limit: defaultPage}
	for k := range v {
		if k != "limit" && k != "cursor" && !(withDestination && k == "destination") {
			return pq, invalid("unknown query parameter %q", k)
		}
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxPage {
			return pq, invalid("limit must be 1 to %d", maxPage)
		}
		pq.Limit = n
	}
	if s := v.Get("cursor"); s != "" {
		c, err := decodeCursor(s)
		if err != nil {
			return pq, err
		}
		pq.Cursor = c
	}
	if s := v.Get("destination"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			return pq, invalid("destination must be a destination id")
		}
		pq.Destination = &id
	}
	return pq, nil
}

// ListRuns is GET .../runs.
func (s *Service) ListRuns(ctx context.Context, pq PageQuery) (httpx.Page[Run], error) {
	arg := dbq.ListBackupRunsPageParams{DestinationID: pq.Destination, RowLimit: int32(pq.Limit + 1)}
	if pq.Cursor != nil {
		arg.AfterAt, arg.AfterID = &pq.Cursor.At, &pq.Cursor.ID
	}
	rows, err := dbq.New(s.pool).ListBackupRunsPage(ctx, arg)
	if err != nil {
		return httpx.Page[Run]{}, fmt.Errorf("list runs: %w", err)
	}
	page := httpx.Page[Run]{Items: []Run{}}
	for i, r := range rows {
		if i == pq.Limit {
			c := cursor{At: rows[i-1].QueuedAt, ID: rows[i-1].ID}.encode()
			page.NextCursor = &c
			break
		}
		page.Items = append(page.Items, runView(r))
	}
	return page, nil
}

// GetRun is GET .../runs/{id}: a test or probe run returns its steps.
func (s *Service) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	r, err := dbq.New(s.pool).GetBackupRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, httpx.NotFound("run")
	}
	if err != nil {
		return Run{}, fmt.Errorf("read run: %w", err)
	}
	return runView(r), nil
}

// ListChanges is GET .../changes.
func (s *Service) ListChanges(ctx context.Context, pq PageQuery) (httpx.Page[ChangeEntry], error) {
	arg := dbq.ListBackupChangeLogPageParams{RowLimit: int32(pq.Limit + 1)}
	if pq.Cursor != nil {
		arg.AfterAt, arg.AfterID = &pq.Cursor.At, &pq.Cursor.ID
	}
	rows, err := dbq.New(s.pool).ListBackupChangeLogPage(ctx, arg)
	if err != nil {
		return httpx.Page[ChangeEntry]{}, fmt.Errorf("list changes: %w", err)
	}
	page := httpx.Page[ChangeEntry]{Items: []ChangeEntry{}}
	for i, r := range rows {
		if i == pq.Limit {
			c := cursor{At: rows[i-1].At, ID: rows[i-1].ID}.encode()
			page.NextCursor = &c
			break
		}
		page.Items = append(page.Items, ChangeEntry{ID: r.ID, At: r.At, ActorID: r.ActorID, ActorUsername: r.ActorUsername,
			Kind: r.Kind, DestinationID: r.DestinationID, DestinationName: r.DestinationName, Detail: r.Detail})
	}
	return page, nil
}

func settingsView(r dbq.BackupSetting) Settings {
	return Settings{InstallID: r.InstallID, SigningPublic: r.SigningPublic, NotifyOnFailure: r.NotifyOnFailure,
		NotifyOnSuccess: r.NotifyOnSuccess, StaleAlertHours: int(r.StaleAlertHours), UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy}
}

// GetSettings is GET .../settings.
func (s *Service) GetSettings(ctx context.Context) (Settings, error) {
	r, err := dbq.New(s.pool).GetBackupSettings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("read the backup settings: %w", err)
	}
	return settingsView(r), nil
}

// UpdateSettings is PUT .../settings: the whole notice object. A change writes
// one change-log row (#27); a save that changes nothing writes none.
func (s *Service) UpdateSettings(ctx context.Context, p *auth.Principal, in SettingsInput) (Settings, error) {
	if in.NotifyOnFailure == nil || in.NotifyOnSuccess == nil || in.StaleAlertHours == nil {
		return Settings{}, invalid("notifyOnFailure, notifyOnSuccess and staleAlertHours are required")
	}
	if *in.StaleAlertHours < 0 || *in.StaleAlertHours > 8760 {
		return Settings{}, invalid("staleAlertHours must be 0 (off) to 8760")
	}
	var out Settings
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.GetBackupSettingsForUpdate(ctx)
		if err != nil {
			return fmt.Errorf("lock the backup settings: %w", err)
		}
		if cur.NotifyOnFailure == *in.NotifyOnFailure && cur.NotifyOnSuccess == *in.NotifyOnSuccess &&
			int(cur.StaleAlertHours) == *in.StaleAlertHours {
			out = settingsView(cur)
			return nil
		}
		row, err := q.UpdateBackupNotice(ctx, dbq.UpdateBackupNoticeParams{NotifyOnFailure: *in.NotifyOnFailure,
			NotifyOnSuccess: *in.NotifyOnSuccess, StaleAlertHours: int32(*in.StaleAlertHours), UpdatedBy: &p.UserID})
		if err != nil {
			return fmt.Errorf("store the notice settings: %w", err)
		}
		if err := logChange(ctx, q, &p.UserID, kindNoticeChanged, nil, "", map[string]any{
			"from": map[string]any{"notifyOnFailure": cur.NotifyOnFailure, "notifyOnSuccess": cur.NotifyOnSuccess, "staleAlertHours": cur.StaleAlertHours},
			"to":   map[string]any{"notifyOnFailure": row.NotifyOnFailure, "notifyOnSuccess": row.NotifyOnSuccess, "staleAlertHours": row.StaleAlertHours},
		}); err != nil {
			return err
		}
		out = settingsView(row)
		return nil
	})
	return out, err
}

// GetStatus is GET .../status: the newest dump, the keys, the recipients, and
// (LAUN-75) the mail state, the stale and failed destinations and their banners.
func (s *Service) GetStatus(ctx context.Context) (Status, error) {
	q := dbq.New(s.pool)
	st, err := q.GetBackupSettings(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("read the backup settings: %w", err)
	}
	now, err := s.now(ctx, q)
	if err != nil {
		return Status{}, err
	}
	list, err := parseStored(st.Recipients, now)
	if err != nil {
		return Status{}, err
	}
	dests, err := q.ListBackupDestinations(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("list destinations: %w", err)
	}
	out := Status{
		EncryptionKeySet: s.keys != nil, SigningPublic: st.SigningPublic, SupportedTypes: driver.SupportedTypes(),
		Recipients: views(list, now), ActiveRecipients: countActive(list, now, ""), EverHadRecipient: st.EverHadRecipient,
		Destinations: len(dests), Banners: []string{}, StaleAlertHours: int(st.StaleAlertHours),
		StaleDestinations: []StaleDestination{}, FailedDestinations: []FailedDestination{},
		MailReady: s.mailReady != nil && s.mailReady(),
	}
	if s.keys != nil {
		out.KeyMismatch = s.keys.BackupKeyMismatch(st.KeyCheck)
		if st.SigningPrivateEnc != nil {
			_, err := s.keys.Open(platform.BackupColSigningKey, platform.BackupSettingsOwner, st.SigningPrivateEnc)
			out.SigningKeyUnreadable = err != nil
		}
	} else {
		out.SigningKeyUnreadable = st.SigningPrivateEnc != nil
	}
	for _, d := range dests {
		if d.Enabled {
			out.EnabledCount++
		}
		if _, unreadable := s.openSecrets(d); unreadable {
			out.DestinationsUnreadable++
		}
	}
	if dump, err := q.GetNewestBackupDump(ctx); err == nil {
		out.NewestDump = &Dump{ID: dump.ID, FileName: dump.FileName, Status: dump.Status, StartedAt: dump.StartedAt,
			FinishedAt: dump.FinishedAt, Bytes: dump.Bytes, MigrationVersion: dump.MigrationVersion,
			FileRemoved: dump.FileRemovedAt != nil, ErrorText: dump.ErrorText}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Status{}, fmt.Errorf("read the newest dump: %w", err)
	}
	recent, err := q.HasRecentOkBackupDump(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("read the dump age: %w", err)
	}
	out.NoRecentDump = !recent
	switch {
	case s.keys == nil:
		out.Banners = append(out.Banners, BannerKeyMissing)
	case out.KeyMismatch:
		out.Banners = append(out.Banners, BannerKeyMismatch)
	}
	if st.SigningPublic == "" {
		out.Banners = append(out.Banners, BannerSigningMissing)
	}
	if out.SigningKeyUnreadable {
		out.Banners = append(out.Banners, BannerSigningUnreadble)
	}
	if out.EnabledCount > 0 && out.ActiveRecipients == 0 {
		out.Banners = append(out.Banners, BannerNoRecipient)
	}
	if out.NoRecentDump {
		out.Banners = append(out.Banners, BannerNoRecentDump)
	}
	if err := s.addNotices(ctx, q, &out, now); err != nil {
		return Status{}, err
	}
	return out, nil
}
