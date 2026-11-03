package backupapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// The actions only QUEUE rows (decisions-T16 #10, #43): probe, test, exchange
// and run. offsite claims them and does the network work. Each insert also sends
// pg_notify('backup_work') in its transaction (#30).

var fingerprintTextRE = regexp.MustCompile(`^[A-Za-z0-9+/=:_.-]{8,128}$`)

func checkConfirmed(fp string) error {
	if !fingerprintTextRE.MatchString(fp) {
		return invalid("confirmedFingerprint must be the fingerprint the probe showed (8 to 128 letters, digits and + / = : _ . -)")
	}
	return nil
}

// queueRun inserts a run row and wakes offsite, in one transaction. pre runs
// first in the same transaction (a pin save, a request row).
func (s *Service) queueRun(ctx context.Context, params dbq.InsertBackupRunParams, pre func(q *dbq.Queries) (*uuid.UUID, error)) (Run, error) {
	// The shared rate limit is taken HERE, after the body, the destination, the
	// key, the driver and the config checks, so a bad request spends no token.
	if err := s.TakeAction(); err != nil {
		return Run{}, err
	}
	var out Run
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if pre != nil {
			req, err := pre(q)
			if err != nil {
				return err
			}
			params.RequestID = req
		}
		row, err := q.InsertBackupRun(ctx, params)
		if err != nil {
			if c, ok := pgUnique(err); ok && c == "backup_runs_one_active_idx" {
				return errBusy
			}
			return fmt.Errorf("queue run: %w", err)
		}
		if err := q.NotifyBackupWork(ctx); err != nil {
			return fmt.Errorf("wake offsite: %w", err)
		}
		out = runView(row)
		return nil
	})
	return out, err
}

func newID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("make an id: %w", err)
	}
	return id, nil
}

// destFor loads a destination and applies the action preconditions in the
// order: not found 404, no key 503, no driver 503.
func (s *Service) destFor(ctx context.Context, id uuid.UUID) (dbq.BackupDestination, error) {
	d, err := dbq.New(s.pool).GetBackupDestination(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errNoDestination
	}
	if err != nil {
		return d, fmt.Errorf("read destination: %w", err)
	}
	if s.keys == nil {
		return d, errNoKeyAction
	}
	if !typeSupported(d.Type) {
		return d, errUnsupportedAction(d.Type)
	}
	return d, nil
}

// QueueProbe is POST .../destinations/{id}/probe: phase 1 of #36 for a saved destination.
func (s *Service) QueueProbe(ctx context.Context, id uuid.UUID) (Run, error) {
	d, err := s.destFor(ctx, id)
	if err != nil {
		return Run{}, err
	}
	rid, err := newID()
	if err != nil {
		return Run{}, err
	}
	return s.queueRun(ctx, dbq.InsertBackupRunParams{ID: rid, DestinationID: &d.ID, Trigger: "probe", Status: "queued", Attempt: 1}, nil)
}

// unsavedTarget checks the type and config of an unsaved form for probe and test.
func (s *Service) unsavedTarget(typ *string, cfg json.RawMessage) (string, json.RawMessage, error) {
	if typ == nil || !knownType(*typ) {
		return "", nil, invalid("type must be one of %s", joinTypes())
	}
	if s.keys == nil {
		return "", nil, errNoKeyAction
	}
	if !typeSupported(*typ) {
		return "", nil, errUnsupportedAction(*typ)
	}
	_, c, err := parseConfig(cfg)
	if err != nil {
		return "", nil, err
	}
	if err := validateConfig(*typ, c); err != nil {
		return "", nil, err
	}
	return *typ, c, nil
}

func joinTypes() string {
	out := ""
	for i, t := range knownTypes {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}

// QueueProbeUnsaved is POST .../destinations/probe: a form that is not saved.
// The form is in test_config; a probe sends no credentials, so there are no secrets.
func (s *Service) QueueProbeUnsaved(ctx context.Context, in ProbeInput) (Run, error) {
	typ, cfg, err := s.unsavedTarget(in.Type, in.Config)
	if err != nil {
		return Run{}, err
	}
	rid, err := newID()
	if err != nil {
		return Run{}, err
	}
	tc, _ := json.Marshal(map[string]any{"type": typ, "config": json.RawMessage(cfg)})
	return s.queueRun(ctx, dbq.InsertBackupRunParams{ID: rid, Trigger: "probe", Status: "queued", Attempt: 1, TestConfig: tc}, nil)
}

// QueueTest is POST .../destinations/{id}/test: phase 2 of #36. With
// confirmedFingerprint, the pin (TLS) or host key (SFTP) is saved first, in the
// same transaction as the queued run, and only when the destination has none.
func (s *Service) QueueTest(ctx context.Context, p *auth.Principal, id uuid.UUID, in TestInput) (Run, error) {
	if in.ConfirmedFingerprint != nil {
		if err := checkConfirmed(*in.ConfirmedFingerprint); err != nil {
			return Run{}, err
		}
	}
	d, err := s.destFor(ctx, id)
	if err != nil {
		return Run{}, err
	}
	rid, err := newID()
	if err != nil {
		return Run{}, err
	}
	var pre func(q *dbq.Queries) (*uuid.UUID, error)
	if in.ConfirmedFingerprint != nil {
		fp := *in.ConfirmedFingerprint
		pre = func(q *dbq.Queries) (*uuid.UUID, error) {
			return nil, s.savePin(ctx, q, p, id, fp)
		}
	}
	return s.queueRun(ctx, dbq.InsertBackupRunParams{ID: rid, DestinationID: &d.ID, Trigger: "test", Status: "queued", Attempt: 1}, pre)
}

// savePin stores a confirmed fingerprint as the destination's first pin or host
// key, with a change-log row (#36). An existing different one is 400: changing a
// trust anchor needs the secrets entered again by a PATCH (#25).
func (s *Service) savePin(ctx context.Context, q *dbq.Queries, p *auth.Principal, id uuid.UUID, fp string) error {
	cur, err := q.GetBackupDestinationForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoDestination
	}
	if err != nil {
		return fmt.Errorf("lock destination: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(cur.Config, &cfg); err != nil || cfg == nil {
		return fmt.Errorf("read the destination config: %w", err)
	}
	field := anchorField(cur.Type)
	switch have, _ := cfg[field].(string); {
	case have == fp:
		return nil // already pinned to this fingerprint
	case have != "":
		return invalid("this destination already has a %s; changing it needs the secrets entered again (a PATCH), then a new test", field)
	}
	cfg[field] = fp
	enc, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := validateConfig(cur.Type, enc); err != nil {
		return err
	}
	sc := scheduleOf(cur)
	if _, err := q.UpdateBackupDestination(ctx, dbq.UpdateBackupDestinationParams{
		ID: cur.ID, Name: cur.Name, Config: enc, Schedule: sc.kind, AtUtc: sc.atUTC, Weekday: weekday16(sc.weekday),
		KeepLast: int32(sc.keepLast), VerifyEveryDays: int32(sc.verify), Enabled: cur.Enabled, UpdatedBy: &p.UserID,
	}); err != nil {
		return fmt.Errorf("save the pin: %w", err)
	}
	kind := kindDestPinSet
	if field == fieldHostKey {
		kind = kindDestHostKeySet
	}
	return logChange(ctx, q, &p.UserID, kind, &cur.ID, cur.Name, map[string]any{"fingerprint": fp})
}

// QueueTestUnsaved is POST .../destinations/test: a form that is not saved. Its
// secrets are sealed into the run row (additional data: the run id) for the
// length of the test (#36). A Dropbox form cannot be tested: the code is single
// use, so the exchange is the first test (#9).
func (s *Service) QueueTestUnsaved(ctx context.Context, in UnsavedTestInput) (Run, error) {
	if in.Type != nil && *in.Type == TypeDropbox {
		return Run{}, invalid("a Dropbox destination cannot be tested before it is saved: the authorization code is single use, so the exchange is the first test")
	}
	if in.ConfirmedFingerprint != nil {
		if err := checkConfirmed(*in.ConfirmedFingerprint); err != nil {
			return Run{}, err
		}
	}
	typ, cfg, err := s.unsavedTarget(in.Type, in.Config)
	if err != nil {
		return Run{}, err
	}
	secrets := map[string]string{}
	if in.UseSavedSecretsFrom != nil {
		saved, err := dbq.New(s.pool).GetBackupDestination(ctx, *in.UseSavedSecretsFrom)
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, invalid("useSavedSecretsFrom: no such destination")
		}
		if err != nil {
			return Run{}, fmt.Errorf("read destination: %w", err)
		}
		if saved.Type != typ {
			return Run{}, invalid("useSavedSecretsFrom: that destination has another type")
		}
		var a, b map[string]any
		_ = json.Unmarshal(saved.Config, &a)
		_ = json.Unmarshal(cfg, &b)
		if !reflect.DeepEqual(a, b) {
			return Run{}, invalid("useSavedSecretsFrom: the saved secrets are used only with the saved config; the config differs, so enter the secrets again")
		}
		opened, unreadable := s.openSecrets(saved)
		if unreadable {
			return Run{}, conflict("the saved secrets cannot be opened with this server's BACKUP_ENCRYPTION_KEY; enter them again")
		}
		for k, v := range opened {
			secrets[k] = v
		}
	}
	for k, v := range in.Secrets {
		if err := checkSecretName(typ, k, v.Reveal(), true); err != nil {
			return Run{}, err
		}
		if v.Reveal() != "" {
			secrets[k] = v.Reveal()
		}
	}
	rid, err := newID()
	if err != nil {
		return Run{}, err
	}
	blob, err := s.sealSecrets(platform.BackupColRunTestSecrets, rid.String(), secrets)
	if err != nil {
		return Run{}, err
	}
	tcm := map[string]any{"type": typ, "config": json.RawMessage(cfg)}
	if in.ConfirmedFingerprint != nil {
		tcm["confirmedFingerprint"] = *in.ConfirmedFingerprint
	}
	tc, _ := json.Marshal(tcm)
	return s.queueRun(ctx, dbq.InsertBackupRunParams{ID: rid, Trigger: "test", Status: "queued", Attempt: 1,
		TestConfig: tc, TestSecretsEnc: blob}, nil)
}

// QueueExchange is POST .../destinations/{id}/exchange: the Dropbox
// authorization code (#9). The code is sealed with the app key and secret into
// the run row; offsite exchanges it once and writes the refresh token.
func (s *Service) QueueExchange(ctx context.Context, id uuid.UUID, in ExchangeInput) (Run, error) {
	if in.Code == nil || in.Code.Reveal() == "" {
		return Run{}, invalid("code is required")
	}
	if len(in.Code.Reveal()) > 2048 {
		return Run{}, invalid("code is longer than 2048 bytes")
	}
	d, err := s.destFor(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if d.Type != TypeDropbox {
		return Run{}, invalid("the exchange is for Dropbox destinations")
	}
	stored, unreadable := s.openSecrets(d)
	if unreadable {
		return Run{}, conflict("the saved secrets cannot be opened with this server's BACKUP_ENCRYPTION_KEY; enter them again")
	}
	if stored["appKey"] == "" || stored["appSecret"] == "" {
		return Run{}, conflict("save the Dropbox app key and app secret (secrets appKey and appSecret) before the exchange")
	}
	rid, err := newID()
	if err != nil {
		return Run{}, err
	}
	blob, err := s.sealSecrets(platform.BackupColRunTestSecrets, rid.String(),
		map[string]string{"code": in.Code.Reveal(), "appKey": stored["appKey"], "appSecret": stored["appSecret"]})
	if err != nil {
		return Run{}, err
	}
	tc, _ := json.Marshal(map[string]any{"type": TypeDropbox})
	return s.queueRun(ctx, dbq.InsertBackupRunParams{ID: rid, DestinationID: &d.ID, Trigger: "exchange", Status: "queued",
		Attempt: 1, TestConfig: tc, TestSecretsEnc: blob}, nil)
}

// QueueRun is POST .../destinations/{id}/run, "Back up now" (#28, #31). It asks
// for a fresh dump (a request row) and queues a manual run that waits for it. A
// queued RETRY is cancelled first; any other open backup run is 409
// destination_busy.
func (s *Service) QueueRun(ctx context.Context, p *auth.Principal, id uuid.UUID) (Run, error) {
	d, err := s.destFor(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if !d.Enabled {
		return Run{}, conflict("this destination is disabled; enable it before a backup run")
	}
	rid, err := newID()
	if err != nil {
		return Run{}, err
	}
	if err := s.TakeAction(); err != nil { // after the 404, 503 and disabled checks
		return Run{}, err
	}
	var out Run
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.GetBackupDestinationForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoDestination
		}
		if err != nil {
			return fmt.Errorf("lock destination: %w", err)
		}
		if !cur.Enabled {
			return conflict("this destination is disabled; enable it before a backup run")
		}
		open, err := q.LockBackupDestinationRuns(ctx, &id)
		if err != nil {
			return fmt.Errorf("read open runs: %w", err)
		}
		for _, r := range open {
			if r.Trigger != "retry" || r.Status != "queued" {
				return errBusy
			}
			n, err := q.CancelQueuedBackupRetry(ctx, r.ID)
			if err != nil {
				return fmt.Errorf("cancel the queued retry: %w", err)
			}
			if n == 0 {
				return errBusy // the retry was just claimed
			}
		}
		req, err := q.InsertBackupRequest(ctx, dbq.InsertBackupRequestParams{RequestedBy: "app", RequestedByUser: &p.UserID})
		if err != nil {
			return fmt.Errorf("request a dump: %w", err)
		}
		row, err := q.InsertBackupRun(ctx, dbq.InsertBackupRunParams{ID: rid, DestinationID: &id, RequestID: &req.ID,
			Trigger: "manual", Status: "queued", Attempt: 1})
		if err != nil {
			if c, ok := pgUnique(err); ok && c == "backup_runs_one_active_idx" {
				return errBusy
			}
			return fmt.Errorf("queue run: %w", err)
		}
		if err := q.NotifyBackupWork(ctx); err != nil {
			return fmt.Errorf("wake offsite: %w", err)
		}
		out = runView(row)
		return nil
	})
	return out, err
}
