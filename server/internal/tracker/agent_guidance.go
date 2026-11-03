package tracker

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// Agent guidance (LAUN-200; docs/internal/decisions-T31-agent-guidance.md):
// text an org admin writes for the agents of the org. One org-wide entry and
// at most one per team. It is data for an agent. It changes no scope and no
// permission.

// MaxGuidanceBytes is the cap on one entry (UTF-8 bytes).
const MaxGuidanceBytes = 8192

// GuidanceEntry is one piece of guidance. Team is the team key, or nil for
// the org-wide entry.
type GuidanceEntry struct {
	Team      *string   `json:"team"`
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Guidance is the GET /agent-guidance body: the org-wide entry first, then
// the team entries by key.
type Guidance struct {
	Items []GuidanceEntry `json:"items"`
}

// GuidanceInput is the PUT body. Team is a team key or id; empty or absent
// is the org-wide entry.
type GuidanceInput struct {
	Team *string `json:"team"`
	Text string  `json:"text"`
}

// GetGuidance lists the entries. With team: the org-wide entry and that team's
// entry only (404 for a team of another org).
func (s *Service) GetGuidance(ctx context.Context, org *auth.Org, team string) (Guidance, error) {
	// A hidden team's entry is not listed (T20 #9).
	all, ids := org.Access.Args()
	arg := dbq.ListAgentGuidanceParams{OrgID: org.ID, AllTeams: all, TeamIds: ids}
	if team = strings.TrimSpace(team); team != "" {
		t, err := resolveTeam(ctx, s.q, org, team)
		if err != nil {
			return Guidance{}, notFound(err, "team")
		}
		arg.OnlyTeam, arg.TeamID = true, &t.ID
	}
	rows, err := s.q.ListAgentGuidance(ctx, arg)
	if err != nil {
		return Guidance{}, fmt.Errorf("list agent guidance: %w", err)
	}
	out := Guidance{Items: make([]GuidanceEntry, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, GuidanceEntry{Team: r.TeamKey, Text: r.Body, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// hiddenGuidanceRune: bidi controls and zero-width characters hide or reorder
// text, so an admin cannot hide a line from the next admin who reads it.
func hiddenGuidanceRune(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	}
	switch r {
	case 0x200E, 0x200F, 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF:
		return true
	}
	return false
}

// GuidanceHiddenText is the fixed 400 sentence for such a character.
const GuidanceHiddenText = "text must not contain bidirectional control or zero-width characters"

func validateGuidance(text string) (string, error) {
	if strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
		return "", httpx.Invalid("text must be valid text without NUL characters")
	}
	if strings.IndexFunc(text, hiddenGuidanceRune) >= 0 {
		return "", httpx.Invalid(GuidanceHiddenText)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", httpx.Invalid("text is required; to remove the guidance use DELETE")
	}
	if len(text) > MaxGuidanceBytes {
		return "", httpx.Invalid("text must be at most %d bytes", MaxGuidanceBytes)
	}
	return text, nil
}

// SetGuidance writes the org-wide or a team entry. A write of the same text
// changes nothing and emits nothing.
func (s *Service) SetGuidance(ctx context.Context, org *auth.Org, p *auth.Principal, in GuidanceInput) (GuidanceEntry, error) {
	text, err := validateGuidance(in.Text)
	if err != nil {
		return GuidanceEntry{}, err
	}
	var out GuidanceEntry
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		team, err := guidanceTeam(ctx, q, org, in.Team)
		if err != nil {
			return err
		}
		var cur dbq.AgentGuidance
		if team == nil {
			cur, err = q.GetOrgAgentGuidance(ctx, org.ID)
		} else {
			cur, err = q.GetTeamAgentGuidance(ctx, dbq.GetTeamAgentGuidanceParams{OrgID: org.ID, TeamID: &team.ID})
		}
		exists := err == nil
		if err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("agent guidance: %w", err)
		}
		var key *string
		if team != nil {
			key = &team.Key
		}
		if exists && cur.Body == text {
			out = GuidanceEntry{Team: key, Text: cur.Body, UpdatedAt: cur.UpdatedAt}
			return nil
		}
		var row dbq.AgentGuidance
		if team == nil {
			row, err = q.UpsertOrgAgentGuidance(ctx, dbq.UpsertOrgAgentGuidanceParams{OrgID: org.ID, Body: text})
		} else {
			row, err = q.UpsertTeamAgentGuidance(ctx, dbq.UpsertTeamAgentGuidanceParams{OrgID: org.ID, TeamID: &team.ID, Body: text})
		}
		if err != nil {
			return fmt.Errorf("write agent guidance: %w", err)
		}
		out = GuidanceEntry{Team: key, Text: row.Body, UpdatedAt: row.UpdatedAt}
		return emitGuidance(ctx, q, org, p, team, false)
	})
	return out, err
}

// DeleteGuidance removes the org-wide or a team entry. A missing entry is not
// an error and emits nothing.
func (s *Service) DeleteGuidance(ctx context.Context, org *auth.Org, p *auth.Principal, teamRef string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var ref *string
		if strings.TrimSpace(teamRef) != "" {
			ref = &teamRef
		}
		team, err := guidanceTeam(ctx, q, org, ref)
		if err != nil {
			return err
		}
		var n int64
		if team == nil {
			n, err = q.DeleteOrgAgentGuidance(ctx, org.ID)
		} else {
			n, err = q.DeleteTeamAgentGuidance(ctx, dbq.DeleteTeamAgentGuidanceParams{OrgID: org.ID, TeamID: &team.ID})
		}
		if err != nil {
			return fmt.Errorf("delete agent guidance: %w", err)
		}
		if n == 0 {
			return nil
		}
		return emitGuidance(ctx, q, org, p, team, true)
	})
}

// guidanceTeam resolves the optional team of a request; nil is the org-wide
// entry. A team of another org is 404.
func guidanceTeam(ctx context.Context, q *dbq.Queries, org *auth.Org, ref *string) (*dbq.Team, error) {
	if ref == nil || strings.TrimSpace(*ref) == "" {
		return nil, nil
	}
	t, err := resolveTeam(ctx, q, org, strings.TrimSpace(*ref))
	if err != nil {
		return nil, notFound(err, "team")
	}
	return &t, nil
}

// emitGuidance: the org-wide entry is org-level; a team's entry names the team
// key, so it follows the team (T20 #9).
func emitGuidance(ctx context.Context, q *dbq.Queries, org *auth.Org, p *auth.Principal, team *dbq.Team, cleared bool) error {
	var key *string
	scope := events.ScopeOrg()
	if team != nil {
		key, scope = &team.Key, events.ScopeTeams(team.ID)
	}
	_, err := events.Emit(ctx, q, org.ID, events.Event{Type: events.AgentGuidanceUpdated, EntityType: "org",
		EntityID: org.ID, ActorID: &p.UserID, Payload: map[string]any{"team": key, "cleared": cleared}}, scope)
	return err
}
