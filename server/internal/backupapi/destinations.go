package backupapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/backup/driver"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// Change kinds written for destinations (decisions-T16 #27). Each is mailed by
// ticket 8's job.
const (
	kindDestAdded          = "destination_added"
	kindDestDeleted        = "destination_deleted"
	kindDestRenamed        = "destination_renamed"
	kindDestConfigChanged  = "destination_config_changed"
	kindDestSecretsChanged = "destination_secrets_changed"
	kindDestScheduleChange = "destination_schedule_changed"
	kindDestRetentionChang = "destination_retention_changed"
	kindDestEnabled        = "destination_enabled"
	kindDestDisabled       = "destination_disabled"
	kindDestPinSet         = "destination_pin_set"
	kindDestHostKeySet     = "destination_host_key_set"
	kindNoticeChanged      = "notice_settings_changed"
)

var atUTCRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

const (
	defaultSchedule = "daily"
	defaultAtUTC    = "04:15"
	defaultKeepLast = 14
	defaultVerify   = 7
)

// schedule is the schedule and retention part of a destination.
type schedule struct {
	kind     string
	atUTC    string
	weekday  *int
	keepLast int
	verify   int
}

// validate checks the CHECKs of the table, with plain messages. lowKeepAsked is
// true when keepLast is being set now (create, or a PATCH that sends it).
func (sc schedule) validate(lowKeepAsked, confirmLow bool) error {
	switch sc.kind {
	case "daily":
		if sc.weekday != nil {
			return invalid("weekday must be absent or null for a daily schedule")
		}
	case "weekly":
		if sc.weekday == nil || *sc.weekday < 0 || *sc.weekday > 6 {
			return invalid("weekday must be 0 (Sunday) to 6 (Saturday) for a weekly schedule")
		}
	default:
		return invalid("schedule must be daily or weekly")
	}
	if !atUTCRE.MatchString(sc.atUTC) {
		return invalid("atUtc must be HH:MM, 00:00 to 23:59 (UTC)")
	}
	if sc.keepLast < 1 || sc.keepLast > 365 {
		return invalid("keepLast must be 1 to 365")
	}
	if lowKeepAsked && sc.keepLast < 2 && !confirmLow {
		return invalid("keepLast 1 keeps a single copy, and a prune would delete every older one; send confirmLowKeep: true to confirm")
	}
	if sc.verify < 0 || sc.verify > 90 {
		return invalid("verifyEveryDays must be 0 to 90")
	}
	return nil
}

func (sc schedule) equal(o schedule) bool {
	return sc.kind == o.kind && sc.atUTC == o.atUTC && equalIntPtr(sc.weekday, o.weekday) &&
		sc.keepLast == o.keepLast && sc.verify == o.verify
}

func scheduleOf(d dbq.BackupDestination) schedule {
	sc := schedule{kind: d.Schedule, atUTC: d.AtUtc, keepLast: int(d.KeepLast), verify: int(d.VerifyEveryDays)}
	if d.Weekday != nil {
		sc.weekday = intp(*d.Weekday)
	}
	return sc
}

func weekday16(w *int) *int16 {
	if w == nil {
		return nil
	}
	v := int16(*w)
	return &v
}

func checkName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if c := utf8.RuneCountInString(n); c < 1 || c > 60 {
		return "", invalid("name must be 1 to 60 characters")
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return "", invalid("name must not hold control characters")
		}
	}
	return n, nil
}

// typeSupported: this build has a driver for the type.
func typeSupported(typ string) bool {
	_, ok := driver.Lookup(typ)
	return ok
}

func errUnsupportedWrite(typ string) error {
	return conflict("this build has no driver for %s destinations yet, so it cannot make or run one (see supportedTypes in the status)", typ)
}

func errUnsupportedAction(typ string) error {
	return unavailable("this build has no driver for %s destinations yet (see supportedTypes in the status)", typ)
}

// validateConfig runs the type's driver over a config. The driver's own error
// text is dropped: it may echo the value.
func validateConfig(typ string, cfg json.RawMessage) error {
	d, ok := driver.Lookup(typ)
	if !ok {
		return errUnsupportedWrite(typ)
	}
	if err := d.Validate(cfg); err != nil {
		return invalid("config is not valid for a %s destination", typ)
	}
	return nil
}

// checkUnique: no OTHER destination has the same (type, host, bucket, folder)
// (decisions-T16 #25). skip is the destination being edited.
func checkUnique(ctx context.Context, q *dbq.Queries, typ string, cfg json.RawMessage, skip uuid.UUID) error {
	d, ok := driver.Lookup(typ)
	if !ok {
		return nil
	}
	id, err := d.Identity(cfg)
	if err != nil {
		return invalid("config is not valid for a %s destination", typ)
	}
	others, err := q.ListBackupDestinations(ctx)
	if err != nil {
		return fmt.Errorf("list destinations: %w", err)
	}
	for _, o := range others {
		if o.ID == skip || o.Type != typ {
			continue
		}
		od, ok := driver.Lookup(o.Type)
		if !ok {
			continue
		}
		oid, err := od.Identity(o.Config)
		if err == nil && oid == id {
			return conflict("another destination already uses this host, bucket and folder; two would prune each other's copies")
		}
	}
	return nil
}

// view builds the wire form. It opens the secrets only to say whether they open.
func (s *Service) view(d dbq.BackupDestination, last *dbq.BackupRun) Destination {
	_, unreadable := s.openSecrets(d)
	out := Destination{
		ID: d.ID, Name: d.Name, Type: d.Type, TypeSupported: typeSupported(d.Type), Enabled: d.Enabled,
		EnabledSince: d.EnabledSince, Config: d.Config, SecretsSet: d.SecretsSet, SecretsUnreadable: unreadable,
		Schedule: d.Schedule, AtUTC: d.AtUtc, KeepLast: int(d.KeepLast), VerifyEveryDays: int(d.VerifyEveryDays),
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, UpdatedBy: d.UpdatedBy,
	}
	if out.SecretsSet == nil {
		out.SecretsSet = []string{}
	}
	if d.Weekday != nil {
		out.Weekday = intp(*d.Weekday)
	}
	if last != nil {
		r := runView(*last)
		out.LastRun = &r
	}
	return out
}

// ListDestinations is GET .../destinations.
func (s *Service) ListDestinations(ctx context.Context) (DestinationList, error) {
	q := dbq.New(s.pool)
	rows, err := q.ListBackupDestinations(ctx)
	if err != nil {
		return DestinationList{}, fmt.Errorf("list destinations: %w", err)
	}
	runs, err := q.ListBackupDestinationLastRuns(ctx)
	if err != nil {
		return DestinationList{}, fmt.Errorf("list last runs: %w", err)
	}
	last := map[uuid.UUID]dbq.BackupRun{}
	for _, r := range runs {
		if r.DestinationID != nil {
			last[*r.DestinationID] = r
		}
	}
	out := DestinationList{Items: make([]Destination, 0, len(rows))}
	for _, d := range rows {
		var lr *dbq.BackupRun
		if r, ok := last[d.ID]; ok {
			lr = &r
		}
		out.Items = append(out.Items, s.view(d, lr))
	}
	return out, nil
}

// GetDestination is GET .../destinations/{id}.
func (s *Service) GetDestination(ctx context.Context, id uuid.UUID) (Destination, error) {
	q := dbq.New(s.pool)
	d, err := q.GetBackupDestination(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Destination{}, errNoDestination
	}
	if err != nil {
		return Destination{}, fmt.Errorf("read destination: %w", err)
	}
	return s.view(d, s.lastRun(ctx, q, id)), nil
}

func (s *Service) lastRun(ctx context.Context, q *dbq.Queries, id uuid.UUID) *dbq.BackupRun {
	runs, err := q.ListBackupDestinationLastRuns(ctx)
	if err != nil {
		return nil
	}
	for _, r := range runs {
		if r.DestinationID != nil && *r.DestinationID == id {
			return &r
		}
	}
	return nil
}

// CreateDestination is POST .../destinations. The destination is made
// disabled: probe, test (and for Dropbox, exchange) come first.
func (s *Service) CreateDestination(ctx context.Context, p *auth.Principal, in CreateDestinationInput) (Destination, error) {
	if in.Name == nil || in.Type == nil {
		return Destination{}, invalid("name and type are required")
	}
	name, err := checkName(*in.Name)
	if err != nil {
		return Destination{}, err
	}
	typ := *in.Type
	if !knownType(typ) {
		return Destination{}, invalid("type must be one of %s", strings.Join(knownTypes, ", "))
	}
	sc := schedule{kind: defaultSchedule, atUTC: defaultAtUTC, keepLast: defaultKeepLast, verify: defaultVerify, weekday: in.Weekday}
	if in.Schedule != nil {
		sc.kind = *in.Schedule
	}
	if in.AtUTC != nil {
		sc.atUTC = *in.AtUTC
	}
	if in.KeepLast != nil {
		sc.keepLast = *in.KeepLast
	}
	if in.VerifyEveryDays != nil {
		sc.verify = *in.VerifyEveryDays
	}
	if err := sc.validate(true, in.ConfirmLowKeep); err != nil {
		return Destination{}, err
	}
	secrets := map[string]string{}
	for k, v := range in.Secrets {
		if err := checkSecretName(typ, k, v.Reveal(), true); err != nil {
			return Destination{}, err
		}
		if v.Reveal() != "" {
			secrets[k] = v.Reveal()
		}
	}
	if !typeSupported(typ) {
		return Destination{}, errUnsupportedWrite(typ)
	}
	cfg := json.RawMessage(`{}`)
	if len(in.Config) > 0 {
		cfg = in.Config
	}
	_, cfg, err = parseConfig(cfg)
	if err != nil {
		return Destination{}, err
	}
	if err := validateConfig(typ, cfg); err != nil {
		return Destination{}, err
	}
	if len(secrets) > 0 && s.keys == nil {
		return Destination{}, errNoKeyWrite
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Destination{}, fmt.Errorf("make a destination id: %w", err)
	}
	blob, err := s.sealSecrets(platform.BackupColDestinationSecrets, id.String(), secrets)
	if err != nil {
		return Destination{}, err
	}
	var out Destination
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		// The settings row lock serialises every write that checks the unique rule.
		if _, err := q.GetBackupSettingsForUpdate(ctx); err != nil {
			return fmt.Errorf("lock the backup settings: %w", err)
		}
		if err := checkUnique(ctx, q, typ, cfg, uuid.Nil); err != nil {
			return err
		}
		row, err := q.InsertBackupDestination(ctx, dbq.InsertBackupDestinationParams{
			ID: id, Name: name, Type: typ, Config: cfg, SecretsEnc: blob, SecretsSet: setNames(secrets),
			Schedule: sc.kind, AtUtc: sc.atUTC, Weekday: weekday16(sc.weekday), KeepLast: int32(sc.keepLast),
			VerifyEveryDays: int32(sc.verify), UpdatedBy: &p.UserID,
		})
		if err != nil {
			return mapDestinationErr(err)
		}
		if blob != nil {
			if _, err := s.keys.RecordBackupKeyCheck(ctx, q); err != nil {
				return err
			}
		}
		detail := map[string]any{"type": typ, "schedule": sc.kind, "atUtc": sc.atUTC, "keepLast": sc.keepLast,
			"verifyEveryDays": sc.verify, "secretsSet": setNames(secrets)}
		if sc.weekday != nil {
			detail["weekday"] = *sc.weekday
		}
		if idn := identityDetail(typ, cfg); idn != nil {
			detail["identity"] = idn
		}
		if err := logChange(ctx, q, &p.UserID, kindDestAdded, &row.ID, row.Name, detail); err != nil {
			return err
		}
		out = s.view(row, nil)
		return nil
	})
	return out, err
}

// mapDestinationErr turns the unique violation of the name into a 409.
func mapDestinationErr(err error) error {
	if c, ok := pgUnique(err); ok && c == "backup_destinations_name_key" {
		return conflict("a destination with this name already exists")
	}
	return fmt.Errorf("write destination: %w", err)
}

// UpdateDestination is PATCH .../destinations/{id} (decisions-T16 #25, #27).
func (s *Service) UpdateDestination(ctx context.Context, p *auth.Principal, id uuid.UUID, in UpdateDestinationInput) (Destination, error) {
	var newCfgMap map[string]any
	var newCfg json.RawMessage
	if in.Config != nil {
		var err error
		newCfgMap, newCfg, err = parseConfig(in.Config)
		if err != nil {
			return Destination{}, err
		}
	}
	var newName *string
	if in.Name != nil {
		n, err := checkName(*in.Name)
		if err != nil {
			return Destination{}, err
		}
		newName = &n
	}
	var out Destination
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		settings, err := q.GetBackupSettingsForUpdate(ctx)
		if err != nil {
			return fmt.Errorf("lock the backup settings: %w", err)
		}
		cur, err := q.GetBackupDestinationForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoDestination
		}
		if err != nil {
			return fmt.Errorf("lock destination: %w", err)
		}
		typ := cur.Type
		// Secrets named in the body: touched (a value or "") or kept (absent, null).
		touched := map[string]string{}
		for k, v := range in.Secrets {
			if err := checkSecretName(typ, k, v.V.Reveal(), v.Set && !v.Null); err != nil {
				return err
			}
			if v.Set && !v.Null {
				touched[k] = v.V.Reveal()
			}
		}
		// Config.
		cfg := cur.Config
		var cfgChanged []string
		if in.Config != nil {
			var oldMap map[string]any
			_ = json.Unmarshal(cur.Config, &oldMap)
			cfgChanged = changedFields(oldMap, newCfgMap)
			if len(cfgChanged) > 0 {
				if !typeSupported(typ) {
					return errUnsupportedWrite(typ)
				}
				if err := validateConfig(typ, newCfg); err != nil {
					return err
				}
				var kept []string
				for _, n := range cur.SecretsSet {
					if _, ok := touched[n]; !ok {
						kept = append(kept, n)
					}
				}
				if len(kept) > 0 {
					return invalid("config changes while the stored secret(s) %s are kept; a stored secret must not go to a new host, URL, user, folder, pin or host key by an edit that does not enter it again: send each of them with a new value, or \"\" to clear it",
						strings.Join(kept, ", "))
				}
				if err := checkUnique(ctx, q, typ, newCfg, cur.ID); err != nil {
					return err
				}
				cfg = newCfg
			}
		}
		// Schedule and retention.
		sc := scheduleOf(cur)
		if in.Schedule != nil {
			sc.kind = *in.Schedule
			if sc.kind == "daily" && !in.Weekday.Set {
				sc.weekday = nil
			}
		}
		if in.AtUTC != nil {
			sc.atUTC = *in.AtUTC
		}
		if in.Weekday.Set {
			if in.Weekday.Null {
				sc.weekday = nil
			} else {
				w := in.Weekday.V
				sc.weekday = &w
			}
		}
		if in.KeepLast != nil {
			sc.keepLast = *in.KeepLast
		}
		if in.VerifyEveryDays != nil {
			sc.verify = *in.VerifyEveryDays
		}
		if err := sc.validate(in.KeepLast != nil && *in.KeepLast != int(cur.KeepLast), in.ConfirmLowKeep); err != nil {
			return err
		}
		// Secrets: merge under the lock.
		secretsNow, unreadable := s.openSecrets(cur)
		secretsChanged := false
		var setNow, clearedNow []string
		blob, setList := cur.SecretsEnc, cur.SecretsSet
		if len(touched) > 0 {
			base := secretsNow
			if unreadable {
				base = map[string]string{} // the old blob is useless: the kept secrets are dropped (T9 #15)
			}
			merged := map[string]string{}
			for k, v := range base {
				merged[k] = v
			}
			for k, v := range touched {
				if v == "" {
					if merged[k] != "" || slices.Contains(cur.SecretsSet, k) {
						clearedNow = append(clearedNow, k)
					}
					delete(merged, k)
				} else {
					merged[k] = v
					setNow = append(setNow, k)
				}
			}
			slices.Sort(setNow)
			slices.Sort(clearedNow)
			blob, err = s.sealSecrets(platform.BackupColDestinationSecrets, cur.ID.String(), merged)
			if err != nil {
				return err
			}
			setList = setNames(merged)
			secretsNow = merged
			unreadable = false
			secretsChanged = len(setNow) > 0 || len(clearedNow) > 0
		}
		// Enabling.
		wantEnabled := cur.Enabled
		if in.Enabled != nil {
			wantEnabled = *in.Enabled
		}
		if wantEnabled && !cur.Enabled {
			if err := s.checkCanEnable(ctx, q, settings, typ, secretsNow, unreadable); err != nil {
				return err
			}
		} else if wantEnabled && (secretsChanged || len(cfgChanged) > 0) {
			if m := missingSecrets(typ, secretsNow); len(m) > 0 && !unreadable {
				return conflict("this would leave the enabled destination without its secret(s) %s", strings.Join(m, ", "))
			}
		}
		// Write.
		nm := cur.Name
		if newName != nil {
			nm = *newName
		}
		changed := nm != cur.Name || len(cfgChanged) > 0 || !sc.equal(scheduleOf(cur)) || wantEnabled != cur.Enabled
		var row dbq.BackupDestination
		if changed {
			row, err = q.UpdateBackupDestination(ctx, dbq.UpdateBackupDestinationParams{
				ID: cur.ID, Name: nm, Config: cfg, Schedule: sc.kind, AtUtc: sc.atUTC, Weekday: weekday16(sc.weekday),
				KeepLast: int32(sc.keepLast), VerifyEveryDays: int32(sc.verify), Enabled: wantEnabled, UpdatedBy: &p.UserID,
			})
			if err != nil {
				return mapDestinationErr(err)
			}
		} else {
			row = cur
		}
		if secretsChanged {
			row, err = q.SetBackupDestinationSecrets(ctx, dbq.SetBackupDestinationSecretsParams{
				ID: cur.ID, SecretsEnc: blob, SecretsSet: setList, UpdatedBy: &p.UserID,
			})
			if err != nil {
				return mapDestinationErr(err)
			}
			if blob != nil {
				if _, err := s.keys.RecordBackupKeyCheck(ctx, q); err != nil {
					return err
				}
			}
		}
		// One change-log row per kind of change.
		log := func(kind string, detail map[string]any) error {
			return logChange(ctx, q, &p.UserID, kind, &cur.ID, nm, detail)
		}
		if nm != cur.Name {
			if err := log(kindDestRenamed, map[string]any{"from": cur.Name, "to": nm}); err != nil {
				return err
			}
		}
		if len(cfgChanged) > 0 {
			d := map[string]any{"fields": cfgChanged}
			if from := identityDetail(typ, cur.Config); from != nil {
				d["from"] = from
			}
			if to := identityDetail(typ, cfg); to != nil {
				d["to"] = to
			}
			if err := log(kindDestConfigChanged, d); err != nil {
				return err
			}
		}
		if secretsChanged {
			if err := log(kindDestSecretsChanged, map[string]any{"set": nonNil(setNow), "cleared": nonNil(clearedNow)}); err != nil {
				return err
			}
		}
		old := scheduleOf(cur)
		if sc.kind != old.kind || sc.atUTC != old.atUTC || !equalIntPtr(sc.weekday, old.weekday) {
			if err := log(kindDestScheduleChange, map[string]any{
				"from": scheduleDetail(old), "to": scheduleDetail(sc)}); err != nil {
				return err
			}
		}
		if sc.keepLast != old.keepLast || sc.verify != old.verify {
			if err := log(kindDestRetentionChang, map[string]any{
				"keepLast":        map[string]any{"from": old.keepLast, "to": sc.keepLast},
				"verifyEveryDays": map[string]any{"from": old.verify, "to": sc.verify}}); err != nil {
				return err
			}
		}
		if wantEnabled != cur.Enabled {
			kind := kindDestDisabled
			if wantEnabled {
				kind = kindDestEnabled
			}
			if err := log(kind, nil); err != nil {
				return err
			}
		}
		out = s.view(row, s.lastRun(ctx, q, row.ID))
		return nil
	})
	return out, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func scheduleDetail(sc schedule) map[string]any {
	d := map[string]any{"schedule": sc.kind, "atUtc": sc.atUTC}
	if sc.weekday != nil {
		d["weekday"] = *sc.weekday
	}
	return d
}

// checkCanEnable is the set of 409s of decisions-T16 #43: a destination cannot be
// enabled without the key, a driver, a signing key that opens, an active
// recipient, readable secrets and the secrets its type needs.
func (s *Service) checkCanEnable(ctx context.Context, q *dbq.Queries, st dbq.BackupSetting, typ string,
	secrets map[string]string, unreadable bool) error {
	if s.keys == nil {
		return errNoKeyWrite
	}
	if !typeSupported(typ) {
		return errUnsupportedWrite(typ)
	}
	if st.SigningPublic == "" {
		return conflict("there is no signing key yet: offsite makes it at its first start; try again then")
	}
	if st.SigningPrivateEnc == nil {
		return conflict("the signing key is incomplete; run deploy.sh --backup-rotate-signing")
	}
	if _, err := s.keys.Open(platform.BackupColSigningKey, platform.BackupSettingsOwner, st.SigningPrivateEnc); err != nil {
		return conflict("this server cannot open the signing key (BACKUP_ENCRYPTION_KEY changed or lost); run deploy.sh --backup-rotate-signing")
	}
	now, err := s.now(ctx, q)
	if err != nil {
		return err
	}
	list, err := parseStored(st.Recipients, now)
	if err != nil {
		return err
	}
	if countActive(list, now, "") == 0 {
		return conflict("there is no active recipient: add an age public key and wait for the hold to pass")
	}
	if unreadable {
		return conflict("the saved secrets cannot be opened with this server's BACKUP_ENCRYPTION_KEY; enter them again")
	}
	if m := missingSecrets(typ, secrets); len(m) > 0 {
		return conflict("the destination lacks its secret(s) %s", strings.Join(m, ", "))
	}
	return nil
}

// DeleteDestination is DELETE .../destinations/{id}. The runs keep destination_name.
func (s *Service) DeleteDestination(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.GetBackupDestinationForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoDestination
		}
		if err != nil {
			return fmt.Errorf("lock destination: %w", err)
		}
		// The log row names the destination while it exists; the delete then sets
		// its destination_id to NULL and the name stays.
		detail := map[string]any{"type": cur.Type, "wasEnabled": cur.Enabled}
		if idn := identityDetail(cur.Type, cur.Config); idn != nil {
			detail["identity"] = idn
		}
		if err := logChange(ctx, q, &p.UserID, kindDestDeleted, &id, cur.Name, detail); err != nil {
			return err
		}
		if _, err := q.DeleteBackupDestination(ctx, id); err != nil {
			return fmt.Errorf("delete destination: %w", err)
		}
		return nil
	})
}
