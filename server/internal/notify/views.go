package notify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Remembered issue-page views (LAUN-141, docs/internal/decisions-T23-view-preferences.md).
// User-private like the notification settings: own rows only, no event.

// MaxViewPlaces is the number of places kept per person and org.
const MaxViewPlaces = 100

var (
	viewPlaceRE = regexp.MustCompile(`^[a-z0-9._:-]{1,200}$`)
	viewValues  = map[string]bool{"list": true, "board": true}
	sortValues  = map[string]bool{"updated": true, "created": true, "priority": true, "manual": true}
	// LAUN-171: what the board groups its columns by. `state` is the default and the only value of a row before 0025.
	groupByValues = map[string]bool{"state": true, "assignee": true, "priority": true, "label": true, "project": true}
)

// ViewPreference is one remembered place. At is epoch milliseconds.
type ViewPreference struct {
	Place string `json:"place"`
	View  string `json:"view"`
	Sort  string `json:"sort"`
	// HideEmpty and HiddenColumns are the board's column choices (LAUN-163). The zero values are the
	// defaults: every column shows. HiddenColumns holds state names; it is never null in the answer.
	HideEmpty     bool     `json:"hideEmpty"`
	HiddenColumns []string `json:"hiddenColumns"`
	// GroupBy is the board's column source (LAUN-171): state, assignee, priority, label or project.
	GroupBy string `json:"groupBy"`
	At      int64  `json:"at"`
}

// ViewPreferenceInput is the body of a PUT.
type ViewPreferenceInput struct {
	View string `json:"view"`
	Sort string `json:"sort"`
	// LAUN-163: optional. Absent means the defaults (a PUT replaces the whole row).
	HideEmpty     bool     `json:"hideEmpty"`
	HiddenColumns []string `json:"hiddenColumns"`
	// LAUN-171: optional. Absent means "state" (a PUT replaces the whole row).
	GroupBy string `json:"groupBy"`
	At      *int64 `json:"at"`
}

// Limits of the hidden column list (decisions-T23 "LAUN-163"): the migration's CHECK is the backstop.
const (
	MaxHiddenColumns = 30
	MaxColumnNameLen = 100
)

func cleanColumns(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, n := range in {
		if n == "" || utf8.RuneCountInString(n) > MaxColumnNameLen || !utf8.ValidString(n) {
			return nil, httpx.Invalid("hiddenColumns holds names of 1-100 characters")
		}
		for _, r := range n {
			if unicode.IsControl(r) || r == ',' {
				return nil, httpx.Invalid("hiddenColumns names must not hold control characters or commas")
			}
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) > MaxHiddenColumns {
		return nil, httpx.Invalid("hiddenColumns holds at most 30 names")
	}
	return out, nil
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// ViewPreferenceList is the answer of the list route.
type ViewPreferenceList struct {
	Items []ViewPreference `json:"items"`
}

// ListViews returns every place the caller remembered in the org.
func (s *Service) ListViews(ctx context.Context, orgID, userID uuid.UUID) (ViewPreferenceList, error) {
	rows, err := s.q.ListViewPreferences(ctx, dbq.ListViewPreferencesParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return ViewPreferenceList{}, fmt.Errorf("list view preferences: %w", err)
	}
	out := ViewPreferenceList{Items: make([]ViewPreference, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, ViewPreference{Place: r.Place, View: r.View, Sort: r.Sort, HideEmpty: r.HideEmpty, HiddenColumns: nonNil(r.HiddenColumns), GroupBy: r.GroupBy, At: r.ChangedAt})
	}
	return out, nil
}

// PutView stores the choice when it is newer than the stored one; otherwise it
// keeps the stored one. Either way it answers the value now stored.
func (s *Service) PutView(ctx context.Context, orgID, userID uuid.UUID, place string, in ViewPreferenceInput) (ViewPreference, error) {
	if !viewPlaceRE.MatchString(place) {
		return ViewPreference{}, httpx.Invalid("place must be 1-200 characters of a-z, 0-9, '.', '_', ':' and '-'")
	}
	if !viewValues[in.View] {
		return ViewPreference{}, httpx.Invalid("view must be list or board")
	}
	if !sortValues[in.Sort] {
		return ViewPreference{}, httpx.Invalid("sort must be updated, created, priority or manual")
	}
	if in.GroupBy == "" {
		in.GroupBy = "state"
	}
	if !groupByValues[in.GroupBy] {
		return ViewPreference{}, httpx.Invalid("groupBy must be state, assignee, priority, label or project")
	}
	if in.At == nil || *in.At < 0 {
		return ViewPreference{}, httpx.Invalid("at is required and must not be negative")
	}
	hidden, err := cleanColumns(in.HiddenColumns)
	if err != nil {
		return ViewPreference{}, err
	}
	// A clock that runs ahead must not freeze the place against later writes.
	at := min(*in.At, s.now().UnixMilli())
	var out ViewPreference
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// One writer per person and org at a time: the cap holds under concurrent writes.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('user_view_preferences:' || $1::text || ':' || $2::text, 0))`, orgID, userID); err != nil {
			return fmt.Errorf("lock view preferences: %w", err)
		}
		q := s.q.WithTx(tx)
		row, err := q.UpsertViewPreference(ctx, dbq.UpsertViewPreferenceParams{
			OrgID: orgID, UserID: userID, Place: place, View: in.View, Sort: in.Sort, HideEmpty: in.HideEmpty, HiddenColumns: hidden, GroupBy: in.GroupBy, ChangedAt: at,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			cur, gerr := q.GetViewPreference(ctx, dbq.GetViewPreferenceParams{OrgID: orgID, UserID: userID, Place: place})
			if gerr != nil {
				return fmt.Errorf("get view preference: %w", gerr)
			}
			out = ViewPreference{Place: cur.Place, View: cur.View, Sort: cur.Sort, HideEmpty: cur.HideEmpty, HiddenColumns: nonNil(cur.HiddenColumns), GroupBy: cur.GroupBy, At: cur.ChangedAt}
			return nil
		}
		if err != nil {
			return fmt.Errorf("upsert view preference: %w", err)
		}
		out = ViewPreference{Place: row.Place, View: row.View, Sort: row.Sort, HideEmpty: row.HideEmpty, HiddenColumns: nonNil(row.HiddenColumns), GroupBy: row.GroupBy, At: row.ChangedAt}
		if err := q.TrimViewPreferences(ctx, dbq.TrimViewPreferencesParams{OrgID: orgID, UserID: userID, Offset: MaxViewPlaces}); err != nil {
			return fmt.Errorf("trim view preferences: %w", err)
		}
		// The trim may have removed the row just written (100 newer places exist). Answer what is stored.
		if _, err := q.GetViewPreference(ctx, dbq.GetViewPreferenceParams{OrgID: orgID, UserID: userID, Place: place}); errors.Is(err, pgx.ErrNoRows) {
			out = ViewPreference{Place: place, View: "list", Sort: "updated", HiddenColumns: []string{}, GroupBy: "state", At: 0}
		} else if err != nil {
			return fmt.Errorf("get view preference: %w", err)
		}
		return nil
	})
	return out, err
}
