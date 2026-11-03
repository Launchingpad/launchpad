// Package idempotency implements the Idempotency-Key header on POST creates.
//
// States of a key row (one per user and key):
//
//   - in flight: status_code NULL, committed_at NULL. A retry gets 409
//     idempotency_in_progress, until the claim is older than StaleAfter (on
//     the database clock): then the retry takes the key over and runs. If the
//     first request was only slow, not dead, its commit then finds its claim
//     gone and rolls back (database.ErrIdempotencyClaimLost), so the write
//     still happens once.
//   - committed, response unknown: status_code NULL, committed_at set.
//     database.InTx sets committed_at in the same transaction as the write,
//     so this means "the write happened but the response was never stored"
//     (a crash, a failed store, or a 5xx after commit). A retry gets 409
//     idempotency_outcome_unknown and is NEVER re-run, at any age.
//   - done: status_code set. A retry with the same request replays the
//     stored response (header Idempotent-Replayed: true).
//
// The same key with a different method, path or body is 409
// idempotency_key_reused. A 5xx, 401 or 403 is not stored; its claim is
// released only if nothing committed. A response that carries a token secret
// is never stored (POST /tokens does not use this middleware at all, and a
// secret-shaped body is refused as a second line of defence).
//
// Mount it AFTER the route's scope check, so a replay never reaches a caller
// whose token could not have made the request.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Header is the request header name.
const Header = "Idempotency-Key"

// TTL is how long a key is remembered.
const TTL = 24 * time.Hour

// DefaultStaleAfter is when an uncommitted in-flight claim counts as abandoned.
const DefaultStaleAfter = 60 * time.Second

// Store is the slice of dbq.Queries this package needs.
type Store interface {
	ClaimIdempotencyKey(ctx context.Context, arg dbq.ClaimIdempotencyKeyParams) (uuid.UUID, error)
	GetIdempotencyKey(ctx context.Context, arg dbq.GetIdempotencyKeyParams) (dbq.IdempotencyKey, error)
	CompleteIdempotencyKey(ctx context.Context, arg dbq.CompleteIdempotencyKeyParams) error
	ReleaseUncommittedIdempotencyKey(ctx context.Context, id uuid.UUID) error
	TakeOverAbandonedIdempotencyKey(ctx context.Context, arg dbq.TakeOverAbandonedIdempotencyKeyParams) (int64, error)
	DeleteExpiredIdempotencyKey(ctx context.Context, arg dbq.DeleteExpiredIdempotencyKeyParams) error
	PurgeExpiredIdempotencyKeys(ctx context.Context) (int64, error)
}

// Options tune the middleware. Zero values take the defaults.
type Options struct {
	StaleAfter time.Duration    // default DefaultStaleAfter; compared on the DB clock
	Now        func() time.Time // default time.Now; only sets expires_at
}

func validKey(k string) bool {
	if len(k) < 1 || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// secretRe matches a token secret. Such a body is never stored.
var secretRe = regexp.MustCompile(`odt_[0-9A-Za-z]{43}`)

// storable: a 5xx may succeed on retry, and a 401/403 depends on the token,
// not the request, so none of those is remembered.
func storable(status int, body []byte) bool {
	return status < 500 && status != http.StatusUnauthorized && status != http.StatusForbidden &&
		!secretRe.Match(body)
}

// Reject answers 400 when the header is present. Routes whose response must
// never be stored (POST /tokens) use it instead of Middleware.
func Reject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(Header) != "" {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, "Idempotency-Key is not supported on this route")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Middleware applies to POST requests that carry the header; everything
// else passes straight through. It must run after auth and the scope check.
func Middleware(store Store, log *slog.Logger, opts Options) func(http.Handler) http.Handler {
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(Header)
			if r.Method != http.MethodPost || key == "" {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			p, ok := auth.PrincipalFrom(ctx)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if !validKey(key) {
				httpx.WriteError(w, http.StatusBadRequest, httpx.CodeBadRequest, "Idempotency-Key must be 1-255 printable ASCII characters")
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
			if err != nil {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "request body is too large")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := requestHash(r.Method, r.URL.Path, body)
			internal := func(msg string, err error) {
				log.ErrorContext(ctx, msg, "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
			}

			if err := store.DeleteExpiredIdempotencyKey(ctx, dbq.DeleteExpiredIdempotencyKeyParams{UserID: p.UserID, Key: key}); err != nil {
				internal("idempotency expiry failed", err)
				return
			}
			// At most two claim attempts: the second only after taking over an
			// abandoned claim.
			for attempt := 0; attempt < 2; attempt++ {
				id, err := store.ClaimIdempotencyKey(ctx, dbq.ClaimIdempotencyKeyParams{
					UserID: p.UserID, Key: key, Method: r.Method, Path: r.URL.Path, RequestHash: hash,
					ExpiresAt: opts.Now().Add(TTL),
				})
				if err == nil {
					run(w, r.WithContext(database.WithIdempotencyClaim(ctx, id)), next, store, log, id)
					return
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					internal("idempotency claim failed", err)
					return
				}
				row, err := store.GetIdempotencyKey(ctx, dbq.GetIdempotencyKeyParams{UserID: p.UserID, Key: key})
				if errors.Is(err, pgx.ErrNoRows) {
					continue // released between our claim and this read: claim again
				}
				if err != nil {
					internal("idempotency read failed", err)
					return
				}
				switch {
				case !bytes.Equal(row.RequestHash, hash):
					httpx.WriteError(w, http.StatusConflict, httpx.CodeIdempotencyKeyReused, "this Idempotency-Key was used for a different request")
					return
				case row.StatusCode != nil:
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Idempotent-Replayed", "true")
					w.WriteHeader(int(*row.StatusCode))
					_, _ = w.Write(row.ResponseBody)
					return
				case row.CommittedAt != nil:
					httpx.WriteError(w, http.StatusConflict, httpx.CodeIdempotencyOutcomeUnknown,
						"the request with this Idempotency-Key was applied but its response was not recorded; read the resource instead of retrying")
					return
				case attempt == 0:
					// In flight and never committed. The database decides,
					// on its own clock, whether it is old enough to be
					// abandoned; a young claim deletes nothing.
					n, err := store.TakeOverAbandonedIdempotencyKey(ctx, dbq.TakeOverAbandonedIdempotencyKeyParams{
						ID: row.ID, StaleSeconds: opts.StaleAfter.Seconds(),
					})
					if err != nil {
						internal("idempotency takeover failed", err)
						return
					}
					if n == 1 {
						log.WarnContext(ctx, "idempotency claim abandoned; re-running", "key_id", row.ID)
						continue
					}
				}
				break
			}
			httpx.WriteError(w, http.StatusConflict, httpx.CodeIdempotencyInProgress, "a request with this Idempotency-Key is in progress; retry")
		})
	}
}

func requestHash(method, path string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

// run executes the handler into a buffer, stores the outcome, then writes it.
// The bookkeeping uses a context that survives a client disconnect.
func run(w http.ResponseWriter, r *http.Request, next http.Handler, store Store, log *slog.Logger, id uuid.UUID) {
	bg := context.WithoutCancel(r.Context())
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	finished := false
	defer func() {
		if !finished { // the handler panicked: free the claim if nothing committed
			if err := store.ReleaseUncommittedIdempotencyKey(bg, id); err != nil {
				log.ErrorContext(bg, "idempotency release failed", "err", err)
			}
		}
	}()
	next.ServeHTTP(rec, r)
	finished = true
	body := rec.body.Bytes()
	if storable(rec.status, body) {
		status := int32(rec.status)
		if err := store.CompleteIdempotencyKey(bg, dbq.CompleteIdempotencyKeyParams{ID: id, StatusCode: &status, ResponseBody: body}); err != nil {
			// Do NOT release: if the write committed, committed_at is set and a
			// retry gets 409 idempotency_outcome_unknown instead of a second
			// create. If nothing committed, the claim becomes abandoned after
			// the stale window and a retry may run then.
			log.ErrorContext(bg, "idempotency complete failed; key kept", "key_id", id, "err", err)
		}
	} else if err := store.ReleaseUncommittedIdempotencyKey(bg, id); err != nil {
		log.ErrorContext(bg, "idempotency release failed", "err", err)
	}
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(body)
}

// recorder buffers a response. Only JSON handlers sit behind the middleware.
type recorder struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

// Sweep deletes expired keys every interval until ctx is done.
func Sweep(ctx context.Context, store Store, log *slog.Logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := store.PurgeExpiredIdempotencyKeys(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.WarnContext(ctx, "idempotency sweep failed", "err", err)
				}
				continue
			}
			if n > 0 {
				log.InfoContext(ctx, "idempotency keys purged", "count", strconv.FormatInt(n, 10))
			}
		}
	}
}
