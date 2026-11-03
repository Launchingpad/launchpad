package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
)

// maxTokenLen bounds the token a caller can send; a real one is 43 characters.
const maxTokenLen = 200

// Unsubscribe is the one-click unsubscribe (decisions-T13 #25). It sets
// email = 'off' for exactly the events and levels the token names, nothing
// else: not the account, not the master switch, not the inbox, not other
// events. A bad, used or expired token does nothing and returns nil, so the
// caller can answer 204 for every token and say nothing about who has an
// account. The token is looked up by its sha256 hash; it is never logged and
// never put in an error.
func (s *Service) Unsubscribe(ctx context.Context, token string) error {
	if token == "" || len(token) > maxTokenLen {
		return nil
	}
	hash := HashToken(token)
	row, err := s.q.GetNotificationMailToken(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unsubscribe: read token: %w", err)
	}
	if row.UsedAt != nil || !row.ExpiresAt.After(s.now()) {
		return nil
	}
	var targets []target
	if err := json.Unmarshal(row.Targets, &targets); err != nil {
		return nil // a token with unreadable targets can do nothing
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Used first: two posts of one token apply it once.
		n, err := q.MarkNotificationMailTokenUsed(ctx, hash)
		if err != nil {
			return fmt.Errorf("unsubscribe: use token: %w", err)
		}
		if n == 0 {
			return nil
		}
		rows, err := q.ListNotificationSettingsSystem(ctx, dbq.ListNotificationSettingsSystemParams{OrgID: row.OrgID, UserID: row.UserID})
		if err != nil {
			return fmt.Errorf("unsubscribe: read settings: %w", err)
		}
		off := valueOff
		for _, t := range targets {
			if _, ok := ParseEvent(t.Event); !ok {
				continue
			}
			// Keep the inbox value of the row as it is.
			var inbox *string
			for _, r := range rows {
				if r.Event == t.Event && ((t.TeamID == nil && r.TeamID == nil) || (t.TeamID != nil && r.TeamID != nil && *t.TeamID == *r.TeamID)) {
					inbox = r.Inbox
				}
			}
			if t.TeamID == nil {
				_, err = q.UpsertGlobalNotificationSetting(ctx, dbq.UpsertGlobalNotificationSettingParams{
					OrgID: row.OrgID, UserID: row.UserID, Event: t.Event, Email: &off, Inbox: inbox})
			} else {
				// A team of another org, or a deleted one, is skipped.
				if _, terr := q.GetTeamByIDSystem(ctx, dbq.GetTeamByIDSystemParams{OrgID: row.OrgID, ID: *t.TeamID}); terr != nil {
					if errors.Is(terr, pgx.ErrNoRows) {
						continue
					}
					return fmt.Errorf("unsubscribe: read team: %w", terr)
				}
				_, err = q.UpsertTeamNotificationSetting(ctx, dbq.UpsertTeamNotificationSettingParams{
					OrgID: row.OrgID, UserID: row.UserID, TeamID: t.TeamID, Event: t.Event, Email: &off, Inbox: inbox})
			}
			if err != nil {
				return fmt.Errorf("unsubscribe: write setting: %w", err)
			}
		}
		return nil
	})
}
