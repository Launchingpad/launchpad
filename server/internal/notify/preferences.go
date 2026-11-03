package notify

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// A person's own display preferences (LAUN-235, docs/internal/decisions-T42-themes.md).
// One row per user, install-wide: the theme follows the person across devices
// and orgs. User-private like the remembered views (decisions-T23 #4): own row
// only, no event.

// DefaultTheme is the theme of a person with no row.
const DefaultTheme = "linear"

var themeValues = map[string]bool{"linear": true, "paper": true, "terminal": true}

// Preferences is the answer of the preference routes.
type Preferences struct {
	// Theme is linear (the default), paper or terminal.
	Theme string `json:"theme"`
}

// PreferencesInput is the body of a PUT. Theme is required.
type PreferencesInput struct {
	Theme *string `json:"theme"`
}

// GetPreferences returns the caller's preferences; the defaults when none are stored.
func (s *Service) GetPreferences(ctx context.Context, userID uuid.UUID) (Preferences, error) {
	theme, err := s.q.GetUserPreferences(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preferences{Theme: DefaultTheme}, nil
	}
	if err != nil {
		return Preferences{}, fmt.Errorf("get preferences: %w", err)
	}
	return Preferences{Theme: theme}, nil
}

// PutPreferences stores the caller's theme (last write wins) and answers what is stored.
func (s *Service) PutPreferences(ctx context.Context, userID uuid.UUID, in PreferencesInput) (Preferences, error) {
	if in.Theme == nil || !themeValues[*in.Theme] {
		return Preferences{}, httpx.Invalid("theme must be linear, paper or terminal")
	}
	var out Preferences
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		theme, err := s.q.WithTx(tx).UpsertUserTheme(ctx, dbq.UpsertUserThemeParams{UserID: userID, Theme: *in.Theme})
		if err != nil {
			return fmt.Errorf("put preferences: %w", err)
		}
		out = Preferences{Theme: theme}
		return nil
	})
	return out, err
}
