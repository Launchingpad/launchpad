package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Platform admins on the API (docs/internal/decisions-T12-signups-invites.md):
// GET / POST / DELETE /platform/admins, behind auth.RequirePlatformAdmin.
// The CLI `server grant-platform-admin` stays for an install with no
// platform admin at all.

// PlatformAdmin is a platform admin as GET /platform/admins lists one.
type PlatformAdmin struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	Name        string    `json:"name"`
	Email       *string   `json:"email"`
	Deactivated bool      `json:"deactivated"`
}

// AddAdminInput is the POST /platform/admins body: the username or the
// email of an existing, active human user.
type AddAdminInput struct {
	User string `json:"user"`
}

// ListPlatformAdmins lists every user with the flag, by username.
func ListPlatformAdmins(ctx context.Context, pool *pgxpool.Pool) ([]PlatformAdmin, error) {
	rows, err := dbq.New(pool).ListPlatformAdmins(ctx)
	if err != nil {
		return nil, fmt.Errorf("list platform admins: %w", err)
	}
	out := make([]PlatformAdmin, 0, len(rows))
	for _, r := range rows {
		out = append(out, PlatformAdmin{ID: r.ID, Username: r.Username, Name: r.Name, Email: r.Email, Deactivated: r.DeactivatedAt != nil})
	}
	return out, nil
}

func adminView(u dbq.User) PlatformAdmin {
	return PlatformAdmin{ID: u.ID, Username: u.Username, Name: u.Name, Email: u.Email, Deactivated: u.DeactivatedAt != nil}
}

// AddPlatformAdmin makes an existing active human a platform admin.
// created is false when the user was one already (nothing changes).
func AddPlatformAdmin(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, p *auth.Principal, in AddAdminInput) (PlatformAdmin, bool, error) {
	ident := strings.TrimSpace(in.User)
	if ident == "" || len(ident) > 254 {
		return PlatformAdmin{}, false, httpx.Invalid("user must be the username or email of an existing user")
	}
	var (
		out     PlatformAdmin
		created bool
	)
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		created = false
		q := dbq.New(tx)
		u, err := q.FindUserByUsernameOrEmail(ctx, ident)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("user")
		}
		if err != nil {
			return fmt.Errorf("user lookup: %w", err)
		}
		if u, err = q.GetUserByIDForUpdate(ctx, u.ID); err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		if u.IsAgent || u.DeactivatedAt != nil {
			return httpx.Invalid("%s", ErrNotEligible.Error())
		}
		out = adminView(u)
		if u.IsPlatformAdmin {
			return nil
		}
		n, err := q.SetPlatformAdmin(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("grant platform admin: %w", err)
		}
		created = n == 1
		return nil
	})
	if err != nil {
		return PlatformAdmin{}, false, err
	}
	if created {
		log.InfoContext(ctx, "audit: platform admin added", "actor", p.Username, "user", out.ID, "username", out.Username)
	}
	return out, created, nil
}

// errLastAdmin: the install would have no active platform admin left.
var errLastAdmin = httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
	"this is the last active platform admin; add another platform admin first")

// RemovePlatformAdmin takes the flag away. It refuses when no other ACTIVE
// human platform admin would remain: the advisory lock serialises two
// removals, so two admins removing each other at once leave one.
func RemovePlatformAdmin(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, p *auth.Principal, id uuid.UUID) error {
	var username string
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.LockPlatformAdmins(ctx); err != nil {
			return fmt.Errorf("platform admin lock: %w", err)
		}
		u, err := q.GetUserByIDForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !u.IsPlatformAdmin) {
			return httpx.NotFound("platform admin")
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		active, err := q.CountActivePlatformAdmins(ctx)
		if err != nil {
			return fmt.Errorf("count platform admins: %w", err)
		}
		left := active
		if !u.IsAgent && u.DeactivatedAt == nil {
			left--
		}
		if left < 1 {
			return errLastAdmin
		}
		if _, err := q.UnsetPlatformAdmin(ctx, u.ID); err != nil {
			return fmt.Errorf("remove platform admin: %w", err)
		}
		username = u.Username
		return nil
	})
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "audit: platform admin removed", "actor", p.Username, "user", id, "username", username)
	return nil
}
