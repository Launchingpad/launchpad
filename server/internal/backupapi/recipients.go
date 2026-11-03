package backupapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/backup"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// The age recipients (decisions-T16 #19, #20). They live in
// backup_settings.recipients (a jsonb array). All times come from one clock
// (the database's, or the injected test clock).

// storedRecipient is one element of backup_settings.recipients.
type storedRecipient struct {
	PublicKey   string     `json:"publicKey"`
	Fingerprint string     `json:"fingerprint"`
	AddedAt     time.Time  `json:"addedAt"`
	AddedBy     *uuid.UUID `json:"addedBy"`
	EffectiveAt time.Time  `json:"effectiveAt"`
	RemoveAt    *time.Time `json:"removeAt"`
	RemoveBy    *uuid.UUID `json:"removeBy"`
}

// activeAt: offsite encrypts to the key at t.
func (r storedRecipient) activeAt(t time.Time) bool {
	return !r.EffectiveAt.After(t) && (r.RemoveAt == nil || r.RemoveAt.After(t))
}

func (r storedRecipient) state(t time.Time) string {
	switch {
	case r.RemoveAt != nil:
		return RecipientRemoving
	case r.EffectiveAt.After(t):
		return RecipientPending
	}
	return RecipientActive
}

func (r storedRecipient) view(t time.Time) Recipient {
	return Recipient{PublicKey: r.PublicKey, Fingerprint: r.Fingerprint, ShortFingerprint: r.Fingerprint[:12],
		State: r.state(t), AddedAt: r.AddedAt, AddedBy: r.AddedBy, EffectiveAt: r.EffectiveAt,
		RemoveAt: r.RemoveAt, RemoveBy: r.RemoveBy}
}

var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseStored reads the column and drops elements whose removal has passed
// (decisions-T16 #29: the next recipient write drops them).
func parseStored(raw json.RawMessage, now time.Time) ([]storedRecipient, error) {
	var all []storedRecipient
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("read the recipients: %w", err)
	}
	out := all[:0:0]
	for _, r := range all {
		if r.RemoveAt != nil && !r.RemoveAt.After(now) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out, nil
}

func views(list []storedRecipient, now time.Time) []Recipient {
	out := make([]Recipient, 0, len(list))
	for _, r := range list {
		out = append(out, r.view(now))
	}
	return out
}

func countActive(list []storedRecipient, now time.Time, skip string) int {
	n := 0
	for _, r := range list {
		if r.Fingerprint != skip && r.activeAt(now) {
			n++
		}
	}
	return n
}

// countProjected counts the keys that will be active after every pending change:
// those with no removal scheduled (a pending add counts).
func countProjected(list []storedRecipient, skip string) int {
	n := 0
	for _, r := range list {
		if r.Fingerprint != skip && r.RemoveAt == nil {
			n++
		}
	}
	return n
}

func find(list []storedRecipient, fp string) int {
	for i, r := range list {
		if r.Fingerprint == fp {
			return i
		}
	}
	return -1
}

func without(list []storedRecipient, i int) []storedRecipient {
	out := make([]storedRecipient, 0, len(list))
	out = append(out, list[:i]...)
	return append(out, list[i+1:]...)
}

var (
	errNoRecipient = httpx.NotFound("recipient")
	errWouldEmpty  = httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
		"this would leave no active recipient while a destination is enabled; add another key first, or disable the destinations")
)

func checkFingerprint(fp string) error {
	if !fingerprintRE.MatchString(fp) {
		return invalid("the recipient is named by its full fingerprint: 64 lower-case hex characters")
	}
	return nil
}

// planAdd adds a key. It is immediate only when the install never had a
// recipient (#20); else it is held for 24 hours.
func planAdd(list []storedRecipient, now time.Time, key string, actor *uuid.UUID, ever bool) ([]storedRecipient, storedRecipient, error) {
	rcp, err := backup.ParseRecipient(key)
	if err != nil {
		return nil, storedRecipient{}, invalid("publicKey must be an age X25519 public key (age1...); scrypt, SSH and plugin recipients are not accepted")
	}
	canon := rcp.String()
	fp := backup.Fingerprint(canon)
	if len(list) >= backup.MaxRecipients {
		return nil, storedRecipient{}, conflict("at most %d recipients can be live at once; wait for a removal to land, or remove a key", backup.MaxRecipients)
	}
	for _, r := range list {
		switch {
		case r.Fingerprint == fp && r.RemoveAt != nil:
			return nil, storedRecipient{}, conflict("this key is already a recipient and its removal is scheduled; cancel the removal instead")
		case r.Fingerprint == fp:
			return nil, storedRecipient{}, conflict("this key is already a recipient")
		case r.Fingerprint[:12] == fp[:12]:
			return nil, storedRecipient{}, conflict("another recipient has the same short fingerprint")
		}
	}
	eff := now.Add(Hold)
	if !ever {
		eff = now
	}
	el := storedRecipient{PublicKey: canon, Fingerprint: fp, AddedAt: now, AddedBy: actor, EffectiveAt: eff}
	return append(append([]storedRecipient{}, list...), el), el, nil
}

// Change kinds written for recipients.
const (
	kindRecipientAdded            = "recipient_added"
	kindRecipientRemovalScheduled = "recipient_removal_scheduled"
	kindRecipientAddCancelled     = "recipient_add_cancelled"
	kindRecipientRemovalCancelled = "recipient_removal_cancelled"
	kindRecipientRemovedByHost    = "recipient_removed_by_host"
	kindRecipientCancelledByHost  = "recipient_cancelled_by_host"
)

// planRemove is DELETE .../recipients/{fp}: a pending add is cancelled; an
// active key gets a removal scheduled. enabled: a destination is enabled, so the
// projected set must stay non-empty.
func planRemove(list []storedRecipient, now time.Time, fp string, actor *uuid.UUID, enabled bool) ([]storedRecipient, string, error) {
	i := find(list, fp)
	if i < 0 {
		return nil, "", errNoRecipient
	}
	el := list[i]
	switch {
	case el.RemoveAt != nil:
		return nil, "", conflict("a removal of this key is already scheduled")
	case el.EffectiveAt.After(now): // a pending add: cancel it
		if enabled && countProjected(list, fp) == 0 {
			return nil, "", errWouldEmpty
		}
		return without(list, i), kindRecipientAddCancelled, nil
	}
	if enabled && countProjected(list, fp) == 0 {
		return nil, "", errWouldEmpty
	}
	at := now.Add(Hold)
	out := append([]storedRecipient{}, list...)
	out[i].RemoveAt, out[i].RemoveBy = &at, actor
	return out, kindRecipientRemovalScheduled, nil
}

// planCancel is POST .../recipients/{fp}/cancel. A pending add: any platform
// admin, the one who made it too. A pending removal: only a DIFFERENT admin
// than the one who scheduled it (#20).
func planCancel(list []storedRecipient, now time.Time, fp string, actor *uuid.UUID, enabled bool) ([]storedRecipient, string, error) {
	i := find(list, fp)
	if i < 0 {
		return nil, "", errNoRecipient
	}
	el := list[i]
	switch {
	case el.RemoveAt != nil:
		if el.RemoveBy != nil && actor != nil && *el.RemoveBy == *actor {
			return nil, "", httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
				"a pending removal can be cancelled only by a different platform admin than the one who scheduled it; ask another admin, or use the host command")
		}
		out := append([]storedRecipient{}, list...)
		out[i].RemoveAt, out[i].RemoveBy = nil, nil
		return out, kindRecipientRemovalCancelled, nil
	case el.EffectiveAt.After(now):
		if enabled && countProjected(list, fp) == 0 {
			return nil, "", errWouldEmpty
		}
		return without(list, i), kindRecipientAddCancelled, nil
	}
	return nil, "", conflict("this key has no pending change to cancel")
}

// planHostRemove removes a key at once (server backup-recipient-remove). With a
// destination enabled it refuses to leave no key that is active NOW.
func planHostRemove(list []storedRecipient, now time.Time, fp string, enabled bool) ([]storedRecipient, error) {
	i := find(list, fp)
	if i < 0 {
		return nil, errNoRecipient
	}
	if enabled && countActive(list, now, fp) == 0 {
		return nil, errWouldEmpty
	}
	return without(list, i), nil
}

// planHostCancel cancels a pending add or removal at once (server
// backup-recipient-cancel).
func planHostCancel(list []storedRecipient, now time.Time, fp string, enabled bool) ([]storedRecipient, string, error) {
	i := find(list, fp)
	if i < 0 {
		return nil, "", errNoRecipient
	}
	el := list[i]
	switch {
	case el.RemoveAt != nil:
		out := append([]storedRecipient{}, list...)
		out[i].RemoveAt, out[i].RemoveBy = nil, nil
		return out, "removal", nil
	case el.EffectiveAt.After(now):
		if enabled && countProjected(list, fp) == 0 {
			return nil, "", errWouldEmpty
		}
		return without(list, i), "add", nil
	}
	return nil, "", conflict("this key has no pending change to cancel")
}

// recipientTx runs fn on the recipients under the settings row lock, stores the
// result, and returns the list as it stands after. fn returns the new list and
// the change-log kind and detail.
func (s *Service) recipientTx(ctx context.Context, actor *uuid.UUID,
	fn func(list []storedRecipient, now time.Time, ever bool, enabled bool) ([]storedRecipient, string, map[string]any, error)) (RecipientList, error) {
	var out RecipientList
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		row, err := q.GetBackupSettingsForUpdate(ctx)
		if err != nil {
			return fmt.Errorf("lock the backup settings: %w", err)
		}
		now, err := s.now(ctx, q)
		if err != nil {
			return err
		}
		list, err := parseStored(row.Recipients, now)
		if err != nil {
			return err
		}
		n, err := q.CountEnabledBackupDestinations(ctx)
		if err != nil {
			return fmt.Errorf("count enabled destinations: %w", err)
		}
		next, kind, detail, err := fn(list, now, row.EverHadRecipient, n > 0)
		if err != nil {
			return err
		}
		enc, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode the recipients: %w", err)
		}
		// ever_had_recipient is set by the first add and never reset (#20).
		ever := row.EverHadRecipient || kind == kindRecipientAdded
		if _, err := q.UpdateBackupRecipients(ctx, dbq.UpdateBackupRecipientsParams{
			Recipients: enc, EverHadRecipient: ever, UpdatedBy: actor,
		}); err != nil {
			return fmt.Errorf("store the recipients: %w", err)
		}
		if err := logChange(ctx, q, actor, kind, nil, "", detail); err != nil {
			return err
		}
		out = RecipientList{Items: views(next, now), EverHadRecipient: ever, Limit: backup.MaxRecipients}
		return nil
	})
	return out, err
}

func recipientDetail(r storedRecipient, extra map[string]any) map[string]any {
	d := map[string]any{"fingerprint": r.Fingerprint, "shortFingerprint": r.Fingerprint[:12]}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// ListRecipients is GET .../recipients.
func (s *Service) ListRecipients(ctx context.Context) (RecipientList, error) {
	q := dbq.New(s.pool)
	row, err := q.GetBackupSettings(ctx)
	if err != nil {
		return RecipientList{}, fmt.Errorf("read the backup settings: %w", err)
	}
	now, err := s.now(ctx, q)
	if err != nil {
		return RecipientList{}, err
	}
	list, err := parseStored(row.Recipients, now)
	if err != nil {
		return RecipientList{}, err
	}
	return RecipientList{Items: views(list, now), EverHadRecipient: row.EverHadRecipient, Limit: backup.MaxRecipients}, nil
}

// AddRecipient is POST .../recipients.
func (s *Service) AddRecipient(ctx context.Context, p *auth.Principal, in AddRecipientInput) (Recipient, error) {
	if in.PublicKey == nil || strings.TrimSpace(*in.PublicKey) == "" {
		return Recipient{}, invalid("publicKey is required")
	}
	actor := &p.UserID
	var added storedRecipient
	var now time.Time
	_, err := s.recipientTx(ctx, actor, func(list []storedRecipient, n time.Time, ever, _ bool) ([]storedRecipient, string, map[string]any, error) {
		next, el, err := planAdd(list, n, strings.TrimSpace(*in.PublicKey), actor, ever)
		if err != nil {
			return nil, "", nil, err
		}
		added, now = el, n
		return next, kindRecipientAdded, recipientDetail(el, map[string]any{
			"publicKey": el.PublicKey, "effectiveAt": el.EffectiveAt, "immediate": !el.EffectiveAt.After(n)}), nil
	})
	if err != nil {
		return Recipient{}, err
	}
	return added.view(now), nil
}

// RemoveRecipient is DELETE .../recipients/{fingerprint}.
func (s *Service) RemoveRecipient(ctx context.Context, p *auth.Principal, fp string) (RecipientList, error) {
	if err := checkFingerprint(fp); err != nil {
		return RecipientList{}, err
	}
	actor := &p.UserID
	return s.recipientTx(ctx, actor, func(list []storedRecipient, now time.Time, _, enabled bool) ([]storedRecipient, string, map[string]any, error) {
		next, kind, err := planRemove(list, now, fp, actor, enabled)
		if err != nil {
			return nil, "", nil, err
		}
		el := list[find(list, fp)]
		extra := map[string]any{}
		if kind == kindRecipientRemovalScheduled {
			extra["removeAt"] = now.Add(Hold)
		}
		return next, kind, recipientDetail(el, extra), nil
	})
}

// CancelRecipient is POST .../recipients/{fingerprint}/cancel.
func (s *Service) CancelRecipient(ctx context.Context, p *auth.Principal, fp string) (RecipientList, error) {
	if err := checkFingerprint(fp); err != nil {
		return RecipientList{}, err
	}
	actor := &p.UserID
	return s.recipientTx(ctx, actor, func(list []storedRecipient, now time.Time, _, enabled bool) ([]storedRecipient, string, map[string]any, error) {
		next, kind, err := planCancel(list, now, fp, actor, enabled)
		if err != nil {
			return nil, "", nil, err
		}
		return next, kind, recipientDetail(list[find(list, fp)], nil), nil
	})
}

// HostRemoveRecipient is `server backup-recipient-remove`: it removes the key
// at once and writes a change-log row with no actor. It refuses to leave no
// active recipient while a destination is enabled.
func (s *Service) HostRemoveRecipient(ctx context.Context, fp string) error {
	if err := checkFingerprint(fp); err != nil {
		return err
	}
	_, err := s.recipientTx(ctx, nil, func(list []storedRecipient, now time.Time, _, enabled bool) ([]storedRecipient, string, map[string]any, error) {
		next, err := planHostRemove(list, now, fp, enabled)
		if err != nil {
			return nil, "", nil, err
		}
		return next, kindRecipientRemovedByHost, recipientDetail(list[find(list, fp)], nil), nil
	})
	return err
}

// HostCancelRecipient is `server backup-recipient-cancel`: it cancels a pending
// add or removal at once, with a change-log row and no actor.
func (s *Service) HostCancelRecipient(ctx context.Context, fp string) (what string, err error) {
	if err := checkFingerprint(fp); err != nil {
		return "", err
	}
	_, err = s.recipientTx(ctx, nil, func(list []storedRecipient, now time.Time, _, enabled bool) ([]storedRecipient, string, map[string]any, error) {
		next, w, err := planHostCancel(list, now, fp, enabled)
		if err != nil {
			return nil, "", nil, err
		}
		what = w
		return next, kindRecipientCancelledByHost, recipientDetail(list[find(list, fp)], map[string]any{"cancelled": w}), nil
	})
	return what, err
}
