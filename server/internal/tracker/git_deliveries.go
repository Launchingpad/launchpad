package tracker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"

	"github.com/jackc/pgx/v5"
)

// The deliveries ring (T17 #9, LAUN-87). One summary row per webhook request:
// the provider's event name, the repo key, the result, the HTTP status and one
// of our own error codes. No payload field, no secret or signature, no caller
// IP, no User-Agent. The ring keeps the newest 100 rows per integration.

// The results of a delivery row (the CHECK of migration 0014).
const (
	DeliveryProcessed    = "processed"
	DeliveryIgnored      = "ignored"
	DeliveryDuplicate    = "duplicate"
	DeliveryUnmapped     = "unmapped"
	DeliveryBadSignature = "bad_signature"
	DeliveryBadRequest   = "bad_request"
	DeliveryError        = "error"
)

const (
	// GitDeliveryRingSize is the rows kept per integration (and, for the rows
	// of no integration, per org and provider).
	GitDeliveryRingSize = 100
	// gitDeliveryThrottle is the spacing of the rows of an unauthenticated
	// request (one per org, provider and result): the org slug is public, so a
	// row per bad request would let anyone rotate the ring.
	gitDeliveryThrottle = time.Minute
	// gitDeliveryWriteTimeout bounds the ring write. The webhook answer never
	// waits longer for it.
	gitDeliveryWriteTimeout = 2 * time.Second
	maxThrottleKeys         = 1024
	maxDeliveryEvent        = 64
	maxDeliveryRepoKey      = 200
	maxDeliveryErrorCode    = 64
)

// GitDeliveryRecord is what a webhook handler tells the ring. IntegrationID is
// nil for a request that matched no integration (a bad signature, or a failure
// before the signature was checked).
type GitDeliveryRecord struct {
	OrgID         uuid.UUID
	IntegrationID *uuid.UUID
	Provider      string
	Result        string
	Event         string
	RepoKey       string
	HTTPStatus    int
	ErrorCode     string
}

// gitDeliveryThrottleState is the in-memory throttle of the unbound rows. The
// zero value works. Dropped rows are only counted; a restart forgets both.
type gitDeliveryThrottleState struct {
	mu      sync.Mutex
	last    map[string]time.Time
	dropped int64
}

// allow reports whether a row for key may be written at now, and if so
// records it. Otherwise it counts a dropped row.
func (t *gitDeliveryThrottleState) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if at, ok := t.last[key]; ok && now.Sub(at) < gitDeliveryThrottle && !now.Before(at) {
		t.dropped++
		return false
	}
	if len(t.last) >= maxThrottleKeys {
		clear(t.last)
	}
	t.last[key] = now
	return true
}

// DroppedGitDeliveries is how many unbound rows the throttle has dropped since
// the process started (tests read it).
func (s *Service) DroppedGitDeliveries() int64 {
	s.gitDeliveryThrottle.mu.Lock()
	defer s.gitDeliveryThrottle.mu.Unlock()
	return s.gitDeliveryThrottle.dropped
}

// printable returns v when it is 0..max printable ASCII characters, else "".
func printable(v string, max int) string {
	if len(v) > max {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// RecordGitDelivery writes one ring row and trims the ring, in its own short
// transaction (database.InTx). The caller has already decided the answer: a
// failure here is returned for the caller to log, and must not change the
// answer. A row of no integration is throttled in memory BEFORE any database
// work. The write is bounded by its own timeout and does not stop when the
// request is canceled.
func (s *Service) RecordGitDelivery(ctx context.Context, rec GitDeliveryRecord) error {
	if rec.IntegrationID == nil {
		key := fmt.Sprintf("%s|%s|%s", rec.OrgID, rec.Provider, rec.Result)
		if !s.gitDeliveryThrottle.allow(key, s.now()) {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitDeliveryWriteTimeout)
	defer cancel()
	var repoKey, errCode *string
	if v := printable(rec.RepoKey, maxDeliveryRepoKey); v != "" {
		repoKey = &v
	}
	if v := printable(rec.ErrorCode, maxDeliveryErrorCode); v != "" {
		errCode = &v
	}
	if len(rec.Event) > maxDeliveryEvent {
		rec.Event = "" // the handler cuts it already; the CHECK would refuse a long one
	}
	ringKey := rec.OrgID.String() + "/" + rec.Provider
	if rec.IntegrationID != nil {
		ringKey = rec.IntegrationID.String()
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.LockGitDeliveryRing(ctx, ringKey); err != nil {
			return fmt.Errorf("lock delivery ring: %w", err)
		}
		// No events.Emit: the ring is an explicit exception (T17 #52).
		return q.InsertGitDelivery(ctx, dbq.InsertGitDeliveryParams{
			OrgID: rec.OrgID, IntegrationID: rec.IntegrationID, Provider: rec.Provider,
			Result: rec.Result, Event: rec.Event,
			RepoKey: repoKey, HttpStatus: int32(rec.HTTPStatus), ErrorCode: errCode,
		})
	})
}

// GitDelivery is one ring row as the API shows it.
type GitDelivery struct {
	ID uuid.UUID `json:"id"`
	// IntegrationID is null for a request that matched no integration.
	IntegrationID *uuid.UUID `json:"integrationId"`
	Provider      string     `json:"provider"`
	ReceivedAt    time.Time  `json:"receivedAt"`
	Result        string     `json:"result"`
	Event         string     `json:"event"`
	RepoKey       *string    `json:"repoKey"`
	HTTPStatus    int        `json:"httpStatus"`
	ErrorCode     *string    `json:"errorCode"`
}

// GitDeliverySummary is the last delivery of an integration, on the list
// route.
type GitDeliverySummary struct {
	ReceivedAt time.Time `json:"receivedAt"`
	Result     string    `json:"result"`
	HTTPStatus int       `json:"httpStatus"`
	ErrorCode  *string   `json:"errorCode"`
}

func lastDelivery(r dbq.ListGitIntegrationsRow) *GitDeliverySummary {
	if r.LastDeliveryAt == nil || r.LastDeliveryResult == nil || r.LastDeliveryStatus == nil {
		return nil
	}
	return &GitDeliverySummary{
		ReceivedAt: *r.LastDeliveryAt, Result: *r.LastDeliveryResult,
		HTTPStatus: int(*r.LastDeliveryStatus), ErrorCode: r.LastDeliveryError,
	}
}

// ListGitDeliveries is the ring of one integration, newest first (at most
// 100), with the rows of no integration (a bad signature) of its org and
// provider. A cross-org id is 404.
func (s *Service) ListGitDeliveries(ctx context.Context, org *auth.Org, id uuid.UUID) ([]GitDelivery, error) {
	row, err := s.getGitRow(ctx, s.q, org.ID, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListGitDeliveries(ctx, dbq.ListGitDeliveriesParams{OrgID: org.ID, IntegrationID: id, Provider: row.Provider})
	if err != nil {
		return nil, fmt.Errorf("list git deliveries: %w", err)
	}
	out := make([]GitDelivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, GitDelivery{
			ID: r.ID, IntegrationID: r.IntegrationID, Provider: r.Provider, ReceivedAt: r.ReceivedAt,
			Result: r.Result, Event: r.Event, RepoKey: r.RepoKey, HTTPStatus: int(r.HttpStatus), ErrorCode: r.ErrorCode,
		})
	}
	return out, nil
}
