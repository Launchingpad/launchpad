package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// usernameRe is the users_username_format CHECK of 0001, checked before the
// insert so a bad username is a 400 and not a constraint error.
var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,38}$`)

// ValidateUser trims and checks a username (lower-cased first) and a display
// name: the users CHECKs. First-run setup and POST /agents share it.
func ValidateUser(username, name string) (string, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return "", "", httpx.Invalid("username must be 1-39 characters, a-z, 0-9, _ and -, starting with a letter")
	}
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return "", "", httpx.Invalid("name must be 1-200 characters")
	}
	return username, name, nil
}

// AgentInput is the POST /agents body. There is no email field on purpose:
// an agent has no email, so it can never be sent or verify a login code
// (decisions-T10 #2).
type AgentInput struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	// TeamAccess is "all" (the default when absent) or "selected"; Teams
	// are keys or ids of teams of this org, required and non-empty for
	// "selected" (decisions-T20 #5, #31).
	TeamAccess string   `json:"teamAccess"`
	Teams      []string `json:"teams"`
}

// AgentRole is the org role of every agent made by CreateAgent: the role the
// old seed and testdb.Fixture give `claude`, which the Linear importer's user
// map names (decisions-T10 #3).
const AgentRole = "member"

// CreateAgent makes an agent user (is_agent, no email) and its membership of
// org, in one transaction. Usernames are unique across the install, so a
// username that exists already (human or agent, in any org) is 409 conflict.
// Its tokens come from POST /tokens (an admin may mint for an agent) or from
// `server mint-token`.
func (s *Service) CreateAgent(ctx context.Context, org *auth.Org, in AgentInput) (Member, error) {
	username, name, err := ValidateUser(in.Username, in.Name)
	if err != nil {
		return Member{}, err
	}
	access, err := NewMemberAccess(in.TeamAccess, in.Teams)
	if err != nil {
		return Member{}, err
	}
	var out Member
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		var teams []uuid.UUID
		if access == TeamAccessSelected {
			// The webhook guard's lock first (decisions-T20 #65, entry 4).
			if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
				return err
			}
			if teams, err = resolveAccessTeams(ctx, q, org, in.Teams); err != nil {
				return err
			}
		}
		// Email NULL: GetLoginUserByEmail can never match an agent, and it
		// also refuses is_agent (two locks on one door).
		u, err := q.InsertUser(ctx, dbq.InsertUserParams{Username: username, Name: name, Email: nil, IsAgent: true})
		if errors.Is(err, pgx.ErrNoRows) { // ON CONFLICT (username) DO NOTHING
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a user with username %q exists already", username)
		}
		if err != nil {
			return fmt.Errorf("insert agent: %w", err)
		}
		if err := q.InsertOrgMember(ctx, dbq.InsertOrgMemberParams{OrgID: org.ID, UserID: u.ID, Role: AgentRole}); err != nil {
			return fmt.Errorf("insert agent member: %w", err)
		}
		var keys []string
		if access == TeamAccessSelected {
			if err := GrantNewMemberTeams(ctx, q, org.ID, u.ID, teams); err != nil {
				return fmt.Errorf("agent: %w", err)
			}
			refs, err := q.ListOrgTeamRefs(ctx, org.ID)
			if err != nil {
				return fmt.Errorf("teams: %w", err)
			}
			for _, t := range refs { // sorted by key
				if slices.Contains(teams, t.ID) {
					keys = append(keys, t.Key)
				}
			}
			// The agent is restricted: no enabled repository may lack a team
			// (#65, entry 4).
			if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
				return err
			}
		}
		out = Member{ID: u.ID, Username: u.Username, Name: u.Name, Email: nil, IsAgent: true,
			Role: AgentRole, CreatedAt: u.CreatedAt, DeactivatedAt: nil}
		out.TeamAccess, out.Teams = memberAccess(AgentRole, access, keys)
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	return out, nil
}

// DeletedAgent is what DeleteAgent tells the audit line: ids and a name, no
// secret.
type DeletedAgent struct {
	ID       uuid.UUID
	Username string
}

// DeleteAgent hard-deletes the agent userRef (an id or a username) of org
// (decisions-T47). One transaction, under the team access lock (the member
// row is locked by the lookup). A human, an unknown user and another org's
// user are all 404, so the route never says whether a human exists. An agent
// that is also a member of another org is 409 (the route would delete it
// there too). The foreign keys remove its tokens, membership and per-member
// rows (CASCADE) and null its authored, assigned and created rows (SET NULL).
// No event: there is no member event type and the guard cannot be broken by
// removing a member (#4, #6).
func (s *Service) DeleteAgent(ctx context.Context, org *auth.Org, userRef string) (DeletedAgent, error) {
	var out DeletedAgent
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		ref := memberRef(userRef)
		ref.OrgID = org.ID
		m, err := q.GetMemberForAccess(ctx, ref)
		if err != nil {
			return notFound(err, "agent")
		}
		if !m.IsAgent {
			return httpx.NotFound("agent")
		}
		others, err := q.CountOtherOrgMemberships(ctx, dbq.CountOtherOrgMembershipsParams{OrgID: org.ID, UserID: m.UserID})
		if err != nil {
			return fmt.Errorf("count memberships: %w", err)
		}
		if others > 0 {
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "this agent is a member of another org and cannot be deleted here")
		}
		n, err := q.DeleteAgentUser(ctx, m.UserID)
		if err != nil {
			return fmt.Errorf("delete agent: %w", err)
		}
		if n != 1 {
			return httpx.NotFound("agent")
		}
		out = DeletedAgent{ID: m.UserID, Username: m.Username}
		return nil
	})
	if err != nil {
		return DeletedAgent{}, err
	}
	return out, nil
}
