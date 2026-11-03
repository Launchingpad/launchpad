package signup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/tracker"
)

// inviteSecretBytes is the random part of an invite token: 256 bits.
const inviteSecretBytes = 32

var inviteSecretLen = base64.RawURLEncoding.EncodedLen(inviteSecretBytes)

// ErrInvalidInvite: wrong, expired, used or revoked, not told apart. It is
// also the answer for an invite whose email now belongs to an agent or a
// deactivated user.
var ErrInvalidInvite = httpx.Errorf(http.StatusUnauthorized, httpx.CodeInvalidInvite,
	"the invite link is wrong, expired, used or revoked; ask an org admin for a new invite")

// newInviteSecret is 32 bytes from crypto/rand, base64url without padding
// (43 characters, safe in a URL fragment).
func newInviteSecret() (string, error) {
	b := make([]byte, inviteSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashInvite is the stored form of a token. No pepper: 256 random bits
// cannot be guessed from the hash (the setup link rule, decisions-T8 #4).
func hashInvite(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// inviteWellFormed reports whether s has the token shape; junk never
// reaches the query.
func inviteWellFormed(s string) bool {
	if len(s) != inviteSecretLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == inviteSecretBytes
}

// InviteLink is the invite URL for secret: the token is in the fragment, so
// a browser never sends it to a server or a proxy log. With no base it is
// the bare path.
func InviteLink(base, secret string) string {
	return strings.TrimRight(base, "/") + "/invite#" + secret
}

// inviteStatus derives the state. An admin's revoke (revoked_by set, or
// before the expiry) is revoked, also when it came after the expiry. An
// open invite past expires_at is expired, and so is one that
// CloseExpiredOpenInvites closed (revoked_by NULL, at or after the expiry).
func inviteStatus(r dbq.OrgInvite, now time.Time) string {
	switch {
	case r.AcceptedAt != nil:
		return InviteAccepted
	case r.RevokedAt != nil && (r.RevokedBy != nil || r.RevokedAt.Before(r.ExpiresAt)):
		return InviteRevoked
	case r.RevokedAt != nil || !now.Before(r.ExpiresAt):
		return StatusExpired
	default:
		return StatusPending
	}
}

// inviteView is the admin's view of an invite. teams are the keys of its
// teams that still exist (decisions-T20 #26); "all" has none.
func inviteView(r dbq.OrgInvite, now time.Time, teams []string) Invite {
	if teams == nil || r.TeamAccess != tracker.TeamAccessSelected {
		teams = []string{}
	}
	return Invite{ID: r.ID, Email: r.Email, Role: r.Role, Status: inviteStatus(r, now), Emailed: r.Emailed, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, AcceptedAt: r.AcceptedAt, AcceptedBy: r.AcceptedBy, RevokedAt: r.RevokedAt,
		TeamAccess: r.TeamAccess, Teams: teams}
}

// inviteTeamKeys reads the team keys of the invites ids of org, by key.
func inviteTeamKeys(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := map[uuid.UUID][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.ListOrgInviteTeamKeys(ctx, dbq.ListOrgInviteTeamKeysParams{OrgID: orgID, InviteIds: ids})
	if err != nil {
		return nil, fmt.Errorf("invite teams: %w", err)
	}
	for _, r := range rows {
		out[r.InviteID] = append(out[r.InviteID], r.Key)
	}
	return out, nil
}

// errAdminInviteTeams: an admin sees every team (decisions-T20 #2, #30).
var errAdminInviteTeams = httpx.Invalid(`an admin invite gives every team: leave teamAccess "selected" and teams out`)

// errInviteTeamsGone: every team a 'selected' invite names was deleted. The
// accept fails and the invite stays open (decisions-T20 #5, #27): the invite
// path never makes a restricted member with no team. It names no team.
var errInviteTeamsGone = httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
	"the teams this invite gives no longer exist; ask an org admin for a new invite")

// requireInvitesOpen answers ErrInvitesClosed in mode closed.
func (s *Service) requireInvitesOpen(ctx context.Context) error {
	m, err := s.mode(ctx)
	if err != nil {
		return err
	}
	if m == modeClosed {
		return ErrInvitesClosed
	}
	return nil
}

// CreateInvite is POST /orgs/{org}/invites. When mail works the link goes
// to the address by email ONLY, and the 201 body holds no token: then the
// admin cannot accept for the person (decisions-T12 #21). Without mail the
// 201 body has the link once (secret and url) for the admin to hand over.
// An email with an open invite, or of a member, is 409.
func (s *Service) CreateInvite(ctx context.Context, org *auth.Org, p *auth.Principal, in InviteInput) (CreatedInvite, error) {
	if err := s.requireInvitesOpen(ctx); err != nil {
		return CreatedInvite{}, err
	}
	email, err := login.NormalizeEmail(in.Email)
	if err != nil {
		return CreatedInvite{}, errBadEmail
	}
	role := strings.TrimSpace(in.Role)
	if role == "" {
		role = "member"
	}
	if role != "member" && role != "admin" {
		return CreatedInvite{}, httpx.Invalid("role must be member or admin")
	}
	access, err := tracker.NewMemberAccess(in.TeamAccess, in.Teams)
	if err != nil {
		return CreatedInvite{}, err
	}
	if role == "admin" && access != tracker.TeamAccessAll {
		return CreatedInvite{}, errAdminInviteTeams
	}
	secret, err := newInviteSecret()
	if err != nil {
		return CreatedInvite{}, err
	}
	emailed := s.mailReady()
	var (
		row  dbq.OrgInvite
		keys []string
	)
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		keys = nil
		q := dbq.New(tx)
		var teams []uuid.UUID
		if access == tracker.TeamAccessSelected {
			// The webhook guard's lock first (decisions-T20 #65, entry 2).
			if err := tracker.LockTeamAccessGuard(ctx, q, org.ID); err != nil {
				return err
			}
			if teams, err = tracker.ResolveAccessTeams(ctx, q, org, in.Teams); err != nil {
				return err
			}
		}
		if err := q.CloseExpiredOpenInvites(ctx, dbq.CloseExpiredOpenInvitesParams{OrgID: org.ID, Email: email}); err != nil {
			return fmt.Errorf("close expired invites: %w", err)
		}
		member, err := q.IsOrgMemberByEmail(ctx, dbq.IsOrgMemberByEmailParams{OrgID: org.ID, Email: email})
		if err != nil {
			return fmt.Errorf("member check: %w", err)
		}
		if member {
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a member of this org has that email already")
		}
		u, err := q.GetUserByEmail(ctx, email)
		switch {
		case err == nil && (u.IsAgent || u.DeactivatedAt != nil):
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "that email cannot be invited")
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("user lookup: %w", err)
		}
		uid := p.UserID
		row, err = q.InsertOrgInvite(ctx, dbq.InsertOrgInviteParams{
			OrgID: org.ID, Email: email, Role: role, TokenHash: hashInvite(secret), CreatedBy: &uid,
			Emailed: emailed, TeamAccess: access, TtlSeconds: s.opts.InviteTTL.Seconds(),
		})
		if errors.Is(err, pgx.ErrNoRows) { // the one-open index
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "an open invite for this email exists already; revoke it first")
		}
		if err != nil {
			return fmt.Errorf("insert invite: %w", err)
		}
		if access != tracker.TeamAccessSelected {
			return nil
		}
		if _, err := q.InsertOrgInviteTeams(ctx, dbq.InsertOrgInviteTeamsParams{OrgID: org.ID, InviteID: row.ID, TeamIds: teams}); err != nil {
			return fmt.Errorf("insert invite teams: %w", err)
		}
		// The open invite is restricted access now: no enabled repository
		// may lack a team (#65, entry 2). An admin route: the refusal names
		// the repositories.
		if err := tracker.CheckWebhookGuard(ctx, q, org.ID); err != nil {
			return err
		}
		byInvite, err := inviteTeamKeys(ctx, q, org.ID, []uuid.UUID{row.ID})
		if err != nil {
			return err
		}
		keys = byInvite[row.ID]
		return nil
	})
	if err != nil {
		return CreatedInvite{}, err
	}
	link := InviteLink(s.opts.PublicBaseURL, secret)
	out := CreatedInvite{Invite: inviteView(row, s.opts.Now(), keys), Emailed: emailed}
	if emailed {
		inviter := p.Name
		if inviter == "" {
			inviter = p.Username
		}
		s.send(ctx, email, inviteMail(inviter, org.Name, link, row.ExpiresAt))
	} else {
		out.Secret, out.URL = secret, link
	}
	s.log.InfoContext(ctx, "audit: invite created", "org", org.Slug, "actor", p.Username, "invite", row.ID,
		"role", row.Role, "team_access", row.TeamAccess, "teams", len(keys), "expires_at", row.ExpiresAt, "emailed", emailed)
	return out, nil
}

// ListInvites is GET /orgs/{org}/invites, newest first.
func (s *Service) ListInvites(ctx context.Context, org *auth.Org, limit int32) ([]Invite, error) {
	q := dbq.New(s.pool)
	rows, err := q.ListOrgInvites(ctx, dbq.ListOrgInvitesParams{OrgID: org.ID, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	var selected []uuid.UUID
	for _, r := range rows {
		if r.TeamAccess == tracker.TeamAccessSelected {
			selected = append(selected, r.ID)
		}
	}
	keys, err := inviteTeamKeys(ctx, q, org.ID, selected)
	if err != nil {
		return nil, err
	}
	now := s.opts.Now()
	out := make([]Invite, 0, len(rows))
	for _, r := range rows {
		out = append(out, inviteView(r, now, keys[r.ID]))
	}
	return out, nil
}

var errNoInvite = httpx.NotFound("invite")

// RevokeInvite is DELETE /orgs/{org}/invites/{id}. Revoking twice is fine;
// an accepted invite is 409 (remove the member instead).
func (s *Service) RevokeInvite(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) error {
	q := dbq.New(s.pool)
	uid := p.UserID
	row, err := q.RevokeOrgInvite(ctx, dbq.RevokeOrgInviteParams{OrgID: org.ID, ID: id, RevokedBy: &uid})
	if err == nil {
		s.log.InfoContext(ctx, "audit: invite revoked", "org", org.Slug, "actor", p.Username, "invite", row.ID)
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("revoke invite: %w", err)
	}
	cur, err := q.GetOrgInvite(ctx, dbq.GetOrgInviteParams{OrgID: org.ID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoInvite
	}
	if err != nil {
		return fmt.Errorf("invite lookup: %w", err)
	}
	if cur.AcceptedAt != nil {
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "the invite was accepted; remove the member instead")
	}
	return nil // revoked already
}

// AcceptInvite is POST /invites/accept. In ONE transaction: the invite is
// consumed (the conditional UPDATE decides a race), then a new user, its
// membership and a session; or, when the email is a user already, only the
// membership (and NO session). Any failure rolls the consumption back.
// The session is zero for status joined.
func (s *Service) AcceptInvite(ctx context.Context, client string, in AcceptInput) (login.Session, AcceptResponse, error) {
	if !inviteWellFormed(in.Token) {
		return login.Session{}, AcceptResponse{}, ErrInvalidInvite
	}
	if err := s.requireInvitesOpen(ctx); err != nil {
		return login.Session{}, AcceptResponse{}, err
	}
	var (
		username, name string
		haveUser       = strings.TrimSpace(in.Username) != "" || strings.TrimSpace(in.Name) != ""
	)
	if haveUser {
		var err error
		if username, name, err = tracker.ValidateUser(in.Username, in.Name); err != nil {
			return login.Session{}, AcceptResponse{}, err
		}
	}
	if err := limited(s.acceptClient, client, nil, "", s.acceptLimit.Take); err != nil {
		return login.Session{}, AcceptResponse{}, err
	}
	var (
		sess login.Session
		out  AcceptResponse
		inv  dbq.OrgInvite
		uid  uuid.UUID
	)
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		sess, out = login.Session{}, AcceptResponse{}
		q := dbq.New(tx)
		hash := hashInvite(in.Token)
		// A 'selected' invite is an entry of the webhook guard (decisions-T20
		// #65, entry 2): its lock comes first in the transaction. The read
		// tells the caller nothing: every failure below is the same answer.
		peek, err := q.GetInviteAccessByHash(ctx, hash)
		switch {
		case err == nil && peek.TeamAccess == tracker.TeamAccessSelected:
			if err := tracker.LockTeamAccessGuard(ctx, q, peek.OrgID); err != nil {
				return err
			}
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("invite access: %w", err)
		}
		inv, err = q.ConsumeOrgInvite(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidInvite
		}
		if err != nil {
			return fmt.Errorf("consume invite: %w", err)
		}
		org, err := q.GetOrgByID(ctx, inv.OrgID)
		if err != nil {
			return fmt.Errorf("invite org: %w", err)
		}
		u, err := q.GetUserByEmail(ctx, inv.Email)
		switch {
		case err == nil:
			if u.IsAgent || u.DeactivatedAt != nil {
				return ErrInvalidInvite
			}
			n, err := q.InsertOrgMemberRole(ctx, dbq.InsertOrgMemberRoleParams{OrgID: org.ID, UserID: u.ID, Role: inv.Role})
			if err != nil {
				return fmt.Errorf("insert member: %w", err)
			}
			// A member already keeps the access they have (#27, #28).
			if n == 1 {
				if err := applyInviteAccess(ctx, q, inv, u.ID); err != nil {
					return err
				}
			}
			m, err := q.GetMembership(ctx, dbq.GetMembershipParams{OrgID: org.ID, UserID: u.ID})
			if err != nil {
				return fmt.Errorf("membership: %w", err)
			}
			uid = u.ID
			out = AcceptResponse{Status: AcceptJoined, Org: login.Org{ID: org.ID, Slug: org.Slug, Name: org.Name, Role: m.Role}}
		case errors.Is(err, pgx.ErrNoRows):
			if !haveUser {
				return httpx.Invalid("username and name are required: this email has no account yet")
			}
			email := inv.Email
			// A copied link (not emailed) proves nothing about the email:
			// the first email-code login revokes what this account had.
			nu, err := q.InsertInvitedUser(ctx, dbq.InsertInvitedUserParams{Username: username, Name: name, Email: &email, EmailUnproven: !inv.Emailed})
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a user with username %q exists already; choose another", username)
			}
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				// users_email_lower_key: the email became a user's after the
				// lookup above (the approve path answers the same).
				return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a user with this email exists already; open the invite link again")
			}
			if err != nil {
				return fmt.Errorf("insert user: %w", err)
			}
			n, err := q.InsertOrgMemberRole(ctx, dbq.InsertOrgMemberRoleParams{OrgID: org.ID, UserID: nu.ID, Role: inv.Role})
			if err != nil {
				return fmt.Errorf("insert member: %w", err)
			}
			if n == 1 {
				if err := applyInviteAccess(ctx, q, inv, nu.ID); err != nil {
					return err
				}
			}
			if sess, err = login.InsertSessionTx(ctx, q, nu.ID, login.SessionScopes(inv.Role == "admin"), s.opts.Now().Add(s.opts.SessionTTL)); err != nil {
				return err
			}
			uid = nu.ID
			lo := login.Org{ID: org.ID, Slug: org.Slug, Name: org.Name, Role: inv.Role}
			body := login.Response{
				User:      login.User{ID: nu.ID, Username: nu.Username, Name: nu.Name, Email: email},
				Orgs:      []login.Org{lo},
				CSRFToken: sess.CSRF,
			}
			sess.Body = body
			out = AcceptResponse{Status: AcceptCreated, Org: lo, Session: &body}
		default:
			return fmt.Errorf("user lookup: %w", err)
		}
		if err := q.SetInviteAcceptedBy(ctx, dbq.SetInviteAcceptedByParams{ID: inv.ID, AcceptedBy: &uid}); err != nil {
			return fmt.Errorf("record invite acceptance: %w", err)
		}
		return nil
	})
	if err != nil {
		return login.Session{}, AcceptResponse{}, err
	}
	s.log.InfoContext(ctx, "audit: invite accepted", "org", out.Org.Slug, "invite", inv.ID, "user", uid,
		"status", out.Status, "role", inv.Role, "team_access", inv.TeamAccess, "emailed", inv.Emailed)
	return sess, out, nil
}

// applyInviteAccess gives the membership userID, which the accept of inv has
// just inserted, the invite's team access (decisions-T20 #27), in the
// accept's transaction. "all" is the column default: nothing to write. For
// "selected" it locks the invite's teams that still exist (a team deleted
// before is gone by the cascade and is skipped); with none left it fails
// with 409 and the whole accept rolls back, so the invite stays open. The
// access comes from the invite row only: the accept body cannot name teams.
// Then the webhook guard, in the form that names no repository: the caller
// is not an org admin (#65, entry 2).
func applyInviteAccess(ctx context.Context, q *dbq.Queries, inv dbq.OrgInvite, userID uuid.UUID) error {
	if inv.TeamAccess != tracker.TeamAccessSelected {
		return nil
	}
	teams, err := q.LockInviteTeamsForAccept(ctx, dbq.LockInviteTeamsForAcceptParams{OrgID: inv.OrgID, InviteID: inv.ID})
	if err != nil {
		return fmt.Errorf("invite teams: %w", err)
	}
	if len(teams) == 0 {
		return errInviteTeamsGone
	}
	if err := q.SetInvitedMemberSelected(ctx, dbq.SetInvitedMemberSelectedParams{OrgID: inv.OrgID, UserID: userID}); err != nil {
		return fmt.Errorf("set team access: %w", err)
	}
	if _, err := q.InsertInvitedMemberTeams(ctx, dbq.InsertInvitedMemberTeamsParams{OrgID: inv.OrgID, UserID: userID, TeamIds: teams}); err != nil {
		return fmt.Errorf("insert member teams: %w", err)
	}
	return tracker.CheckWebhookGuardNameless(ctx, q, inv.OrgID)
}
