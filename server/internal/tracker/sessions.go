package tracker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
)

// The caller's own login sessions (LAUN-130, decisions-T6a "Sessions list"). A session is
// an api_tokens row of kind session (web) or native. Every query filters on the caller's
// user id AND those two kinds: a PAT, another person's row and an unknown id are all the
// same 404.

// Session is one live login of the caller. It carries no secret, no hash and no prefix.
type Session struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	Current    bool       `json:"current"`
}

// RevokedSessions is the answer of "log out everywhere".
type RevokedSessions struct {
	Revoked int64 `json:"revoked"`
}

// RevokedSession is what the audit line needs.
type RevokedSession struct {
	ID    uuid.UUID
	Kind  string
	Name  string
	Newly bool
}

// ListSessions lists the caller's live sessions, newest first (200 at most).
func (s *Service) ListSessions(ctx context.Context, p *auth.Principal) ([]Session, error) {
	rows, err := s.q.ListMySessions(ctx, dbq.ListMySessionsParams{UserID: p.UserID, Now: s.now()})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := make([]Session, 0, len(rows))
	for _, r := range rows {
		out = append(out, Session{ID: r.ID, Kind: r.Kind, Name: r.Name, CreatedAt: r.CreatedAt,
			LastUsedAt: r.LastUsedAt, ExpiresAt: r.ExpiresAt, Current: r.ID == p.TokenID})
	}
	return out, nil
}

// RevokeSession revokes one of the caller's own sessions. Revoking twice is a no-op.
func (s *Service) RevokeSession(ctx context.Context, p *auth.Principal, id uuid.UUID) (RevokedSession, error) {
	r, err := s.q.RevokeMySession(ctx, dbq.RevokeMySessionParams{ID: id, UserID: p.UserID})
	if err != nil {
		return RevokedSession{}, notFound(err, "session")
	}
	return RevokedSession{ID: r.ID, Kind: r.Kind, Name: r.Name, Newly: r.NewlyRevoked}, nil
}

// RevokeOtherSessions revokes every live session of the caller except the one that
// made the call (a PAT caller has none, so all go).
func (s *Service) RevokeOtherSessions(ctx context.Context, p *auth.Principal) (RevokedSessions, error) {
	n, err := s.q.RevokeMyOtherSessions(ctx, dbq.RevokeMyOtherSessionsParams{UserID: p.UserID, ID: p.TokenID})
	if err != nil {
		return RevokedSessions{}, fmt.Errorf("revoke other sessions: %w", err)
	}
	return RevokedSessions{Revoked: n}, nil
}
