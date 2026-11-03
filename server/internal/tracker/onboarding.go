package tracker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // zone names must validate on a slim image too (LAUN-56)
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// The first-run checklist and the org settings (LAUN-56;
// docs/internal/decisions-T17-onboarding.md).

// Onboarding step ids. The order is the order of the checklist.
var onboardingSteps = []string{"organisation", "mail", "signups", "team", "agent", "import", "githost", "invite", "products"}

// optionalSteps never count in Remaining. "products" (LAUN-67) is also listed only while it applies: the org has
// two or more teams and no product (decisions-T18 #32).
var optionalSteps = []string{"products"}

// Capability names (the `capabilities` list of GET /orgs/{org}/onboarding).
const (
	CapLinearImport = "linearImport"
	CapGitHosts     = "gitHosts"
	CapAttachments  = "attachments"
)

// OnboardingStep is one step of the checklist. Done is computed by the
// server; Skipped is an org admin's choice, shared by all admins of the org.
type OnboardingStep struct {
	ID        string     `json:"id"`
	Done      bool       `json:"done"`
	Skipped   bool       `json:"skipped"`
	SkippedBy *UserRef   `json:"skippedBy"`
	SkippedAt *time.Time `json:"skippedAt"`
}

// Onboarding is the GET /orgs/{org}/onboarding body. It holds booleans and
// ids only: never a setting, a secret or a platform value (decisions #3).
type Onboarding struct {
	Steps []OnboardingStep `json:"steps"`
	// Remaining counts steps that are not done, not skipped and available here. An optional step never counts.
	Remaining    int      `json:"remaining"`
	Capabilities []string `json:"capabilities"`
}

// PlatformFacts are the install-level facts the API layer hands in: the
// tracker package does not know the mail switch.
type PlatformFacts struct {
	MailWorking  bool
	Capabilities []string
}

// ValidOnboardingStep reports whether id is a step.
func ValidOnboardingStep(id string) bool { return slices.Contains(onboardingSteps, id) }

// GetOnboarding computes the checklist for org.
func (s *Service) GetOnboarding(ctx context.Context, org *auth.Org, pf PlatformFacts) (Onboarding, error) {
	return buildOnboarding(ctx, s.q, org.ID, pf)
}

func buildOnboarding(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, pf PlatformFacts) (Onboarding, error) {
	f, err := q.OnboardingFacts(ctx, orgID)
	if err != nil {
		return Onboarding{}, fmt.Errorf("onboarding facts: %w", err)
	}
	skips, err := q.ListOnboardingSkips(ctx, orgID)
	if err != nil {
		return Onboarding{}, fmt.Errorf("onboarding skips: %w", err)
	}
	done := map[string]bool{
		"organisation": true, // setup made it
		"mail":         pf.MailWorking,
		"signups":      f.SignupChosen,
		"team":         f.CyclesOn,
		"agent":        f.AgentToken,
		"import":       f.Imported,
		"githost":      f.GitHost,
		"invite":       f.Humans > 1,
		"products":     f.HasProduct,
	}
	caps := pf.Capabilities
	if caps == nil {
		caps = []string{}
	}
	out := Onboarding{Steps: make([]OnboardingStep, 0, len(onboardingSteps)), Capabilities: caps}
	for _, id := range onboardingSteps {
		if id == "products" && (f.Teams < 2 || f.HasProduct) {
			continue // not offered: it shows only for two or more teams and no product
		}
		st := OnboardingStep{ID: id, Done: done[id]}
		for _, k := range skips {
			if k.Step == id {
				st.Skipped, st.SkippedBy = true, userRef(k.SkippedBy, k.SkippedByUsername, k.SkippedByName)
				t := k.CreatedAt
				st.SkippedAt = &t
			}
		}
		avail := (id != "import" || slices.Contains(caps, CapLinearImport)) &&
			(id != "githost" || slices.Contains(caps, CapGitHosts))
		if !st.Done && !st.Skipped && avail && !slices.Contains(optionalSteps, id) {
			out.Remaining++
		}
		out.Steps = append(out.Steps, st)
	}
	return out, nil
}

// SetOnboardingSkip stores or removes an org's skip of one step. It is
// idempotent: a repeat changes nothing and emits no event.
func (s *Service) SetOnboardingSkip(ctx context.Context, org *auth.Org, p *auth.Principal, step string, skipped bool, pf PlatformFacts) (Onboarding, error) {
	if !ValidOnboardingStep(step) {
		return Onboarding{}, httpx.Invalid("step must be one of %s", strings.Join(onboardingSteps, ", "))
	}
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var n int64
		var err error
		if skipped {
			uid := p.UserID
			n, err = q.InsertOnboardingSkip(ctx, dbq.InsertOnboardingSkipParams{OrgID: org.ID, Step: step, SkippedBy: &uid})
		} else {
			n, err = q.DeleteOnboardingSkip(ctx, dbq.DeleteOnboardingSkipParams{OrgID: org.ID, Step: step})
		}
		if err != nil {
			return fmt.Errorf("onboarding skip: %w", err)
		}
		if n == 0 {
			return nil
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.OnboardingStepChanged, EntityType: "org", EntityID: org.ID,
			ActorID: &p.UserID, Payload: map[string]any{"step": step, "skipped": skipped}}, events.ScopeOrg())
		return err
	})
	if err != nil {
		return Onboarding{}, err
	}
	return s.GetOnboarding(ctx, org, pf)
}

// ---------- org settings ----------

// UpdateOrgInput is the PATCH /orgs/{org} body. The slug is fixed: sending it
// is an unknown field (400).
type UpdateOrgInput struct {
	Name        httpx.Opt[string] `json:"name"`
	TimeZone    httpx.Opt[string] `json:"timeZone"`
	DefaultTeam httpx.Opt[string] `json:"defaultTeam"` // team key or id; null clears
}

// GetOrg is GET /orgs/{org}: the org with its settings, for the caller's role.
func (s *Service) GetOrg(ctx context.Context, org *auth.Org) (Org, error) {
	// The default team's key is null when the caller cannot see it (T20 #9).
	all, ids := org.Access.Args()
	r, err := s.q.GetOrgFull(ctx, dbq.GetOrgFullParams{ID: org.ID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return Org{}, notFound(err, "org")
	}
	return Org{ID: r.ID, Slug: r.Slug, Name: r.Name, Role: org.Role, CreatedAt: r.CreatedAt,
		TimeZone: r.TimeZone, DefaultTeam: r.DefaultTeamKey,
		AvatarURL: s.avatarURL(r.Slug, "", r.HasAvatar, r.AvatarVersion)}, nil
}

func validateTimeZone(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 64 || v == "Local" || strings.ContainsAny(v, " \\") || strings.HasPrefix(v, "/") || strings.Contains(v, "..") {
		return "", httpx.Invalid("timeZone must be an IANA zone name such as Europe/London or UTC")
	}
	if _, err := time.LoadLocation(v); err != nil {
		return "", httpx.Invalid("timeZone must be an IANA zone name such as Europe/London or UTC")
	}
	return v, nil
}

// UpdateOrg is PATCH /orgs/{org}: name, time zone, default team. One
// transaction under the org row lock; events.Emit is the last statement. A
// request that changes nothing writes and emits nothing.
func (s *Service) UpdateOrg(ctx context.Context, org *auth.Org, p *auth.Principal, in UpdateOrgInput) (Org, error) {
	if !in.Name.Set && !in.TimeZone.Set && !in.DefaultTeam.Set {
		return Org{}, httpx.Invalid("send at least one of name, timeZone, defaultTeam")
	}
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.LockOrg(ctx, org.ID)
		if err != nil {
			return notFound(err, "org")
		}
		next := dbq.UpdateOrgParams{ID: org.ID, Name: cur.Name, TimeZone: cur.TimeZone, DefaultTeamID: cur.DefaultTeamID}
		var changes []string
		if in.Name.Set {
			if in.Name.Null {
				return httpx.Invalid("name cannot be null")
			}
			n := strings.TrimSpace(in.Name.V)
			if c := utf8.RuneCountInString(n); c < 1 || c > 200 {
				return httpx.Invalid("name must be 1 to 200 characters")
			}
			if n != cur.Name {
				next.Name, changes = n, append(changes, "name")
			}
		}
		if in.TimeZone.Set {
			if in.TimeZone.Null {
				return httpx.Invalid("timeZone cannot be null")
			}
			z, err := validateTimeZone(in.TimeZone.V)
			if err != nil {
				return err
			}
			if z != cur.TimeZone {
				next.TimeZone, changes = z, append(changes, "timeZone")
			}
		}
		if in.DefaultTeam.Set {
			var id *uuid.UUID
			if !in.DefaultTeam.Null {
				t, err := resolveTeam(ctx, q, org, strings.TrimSpace(in.DefaultTeam.V))
				if err != nil {
					return invalidRef(err, "unknown team %q", in.DefaultTeam.V)
				}
				id = &t.ID
			}
			if (id == nil) != (cur.DefaultTeamID == nil) || (id != nil && *id != *cur.DefaultTeamID) {
				next.DefaultTeamID, changes = id, append(changes, "defaultTeam")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		if err := q.UpdateOrg(ctx, next); err != nil {
			return fmt.Errorf("update org: %w", err)
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.OrgUpdated, EntityType: "org", EntityID: org.ID,
			ActorID: &p.UserID, Payload: map[string]any{"id": org.ID, "slug": org.Slug, "changes": changes}}, events.ScopeOrg())
		return err
	})
	if err != nil {
		return Org{}, err
	}
	return s.GetOrg(ctx, org)
}

// AttachmentsEnabled reports whether the attachment store is configured.
func (s *Service) AttachmentsEnabled() bool { return s.store != nil }

// PublicBaseURLSet reports whether PUBLIC_BASE_URL is set.
func (s *Service) PublicBaseURLSet() bool { return s.publicBaseURL != "" }
