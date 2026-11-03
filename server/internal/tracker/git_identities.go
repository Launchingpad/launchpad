package tracker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// Connected accounts (LAUN-199; docs/internal/decisions-T34-connected-accounts.md).
// A member links the handle they have on one git connection, so a change
// request from that handle shows the member. A link is a CLAIM the member
// typed: it is read for display only, never for a permission. One holder per
// handle per connection; a second claim is a 409 that names nobody.

// Handle rules per host (decisions-T34 #4). The handle is matched to the
// author login the webhook gave, in lower case.
var handleRE = map[string]*regexp.Regexp{
	"github":    regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`),
	"gitlab":    regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}$`),
	"bitbucket": regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9 _.:@-]{0,99}$`),
}

// HandleTakenText is the fixed 409 sentence. It does not say who holds it.
const HandleTakenText = "that handle is already linked on this connection; ask an org admin if it is yours"

// ConnectionOffText is the fixed 409 sentence for a link on a turned-off connection.
const ConnectionOffText = "this connection is turned off; ask an org admin"

// GitAccount is one connection of the org with the caller's handle on it.
type GitAccount struct {
	IntegrationID uuid.UUID  `json:"integrationId"`
	Provider      string     `json:"provider"`
	Name          string     `json:"name"`
	Host          string     `json:"host"`
	Enabled       bool       `json:"enabled"`
	Handle        *string    `json:"handle"`
	LinkedAt      *time.Time `json:"linkedAt"`
}

// GitAccounts is the answer of the list route.
type GitAccounts struct {
	Items []GitAccount `json:"items"`
}

// GitAccountInput is the PUT body.
type GitAccountInput struct {
	Handle string `json:"handle"`
}

// GitIdentity is one link, as an org admin sees all of them.
type GitIdentity struct {
	ID              uuid.UUID `json:"id"`
	IntegrationID   uuid.UUID `json:"integrationId"`
	Provider        string    `json:"provider"`
	IntegrationName string    `json:"integrationName"`
	Handle          string    `json:"handle"`
	User            UserRef   `json:"user"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// GitIdentities is the answer of the admin list.
type GitIdentities struct {
	Items []GitIdentity `json:"items"`
}

// CleanHandle trims the input, drops one leading "@" and checks the rules of
// the provider.
func CleanHandle(provider, in string) (string, error) {
	h := strings.TrimPrefix(strings.TrimSpace(in), "@")
	re := handleRE[provider]
	if re == nil || !re.MatchString(h) {
		return "", httpx.Invalid("handle is not a valid %s handle: use the name you sign in with on that host, up to %d characters", provider, handleMax(provider))
	}
	return h, nil
}

func handleMax(provider string) int {
	switch provider {
	case "github":
		return 39
	default:
		return 100
	}
}

// ListMyGitAccounts lists the org's connections with the caller's handle.
func (s *Service) ListMyGitAccounts(ctx context.Context, orgID, userID uuid.UUID) (GitAccounts, error) {
	rows, err := s.q.ListMyGitAccounts(ctx, dbq.ListMyGitAccountsParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return GitAccounts{}, fmt.Errorf("list git accounts: %w", err)
	}
	out := GitAccounts{Items: make([]GitAccount, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, GitAccount{
			IntegrationID: r.IntegrationID, Provider: r.Provider, Name: derefOr(r.Name, hostOf(r.BaseUrl)),
			Host: hostOf(r.BaseUrl), Enabled: r.Enabled, Handle: r.Handle, LinkedAt: r.LinkedAt,
		})
	}
	return out, nil
}

// SetMyGitAccount links or changes the caller's handle on one connection.
// A user-private row: no event (decisions-T34 #6).
func (s *Service) SetMyGitAccount(ctx context.Context, orgID, userID, integrationID uuid.UUID, in GitAccountInput) (GitAccount, error) {
	gi, err := s.q.GetGitIntegrationProvider(ctx, dbq.GetGitIntegrationProviderParams{OrgID: orgID, ID: integrationID})
	if err != nil {
		return GitAccount{}, notFound(err, "connection")
	}
	if !gi.Enabled {
		return GitAccount{}, httpx.Errorf(409, httpx.CodeConflict, "%s", ConnectionOffText)
	}
	handle, err := CleanHandle(gi.Provider, in.Handle)
	if err != nil {
		return GitAccount{}, err
	}
	_, err = s.q.UpsertGitIdentity(ctx, dbq.UpsertGitIdentityParams{
		OrgID: orgID, UserID: userID, IntegrationID: integrationID, Handle: handle, HandleKey: strings.ToLower(handle),
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "git_identities_handle_key" {
			return GitAccount{}, httpx.Errorf(409, httpx.CodeConflict, "%s", HandleTakenText)
		}
		return GitAccount{}, fmt.Errorf("link git account: %w", err)
	}
	return s.oneGitAccount(ctx, orgID, userID, integrationID)
}

func (s *Service) oneGitAccount(ctx context.Context, orgID, userID, integrationID uuid.UUID) (GitAccount, error) {
	all, err := s.ListMyGitAccounts(ctx, orgID, userID)
	if err != nil {
		return GitAccount{}, err
	}
	for _, a := range all.Items {
		if a.IntegrationID == integrationID {
			return a, nil
		}
	}
	return GitAccount{}, httpx.NotFound("connection")
}

// DeleteMyGitAccount removes the caller's link. A missing link is 204 (the
// DELETE is replay-safe); a connection of another org is 404.
func (s *Service) DeleteMyGitAccount(ctx context.Context, orgID, userID, integrationID uuid.UUID) error {
	if _, err := s.q.GetGitIntegrationProvider(ctx, dbq.GetGitIntegrationProviderParams{OrgID: orgID, ID: integrationID}); err != nil {
		return notFound(err, "connection")
	}
	if _, err := s.q.DeleteMyGitIdentity(ctx, dbq.DeleteMyGitIdentityParams{OrgID: orgID, UserID: userID, IntegrationID: integrationID}); err != nil {
		return fmt.Errorf("unlink git account: %w", err)
	}
	return nil
}

// ListGitIdentities lists every link of the org (org admin).
func (s *Service) ListGitIdentities(ctx context.Context, org *auth.Org) (GitIdentities, error) {
	rows, err := s.q.ListGitIdentities(ctx, org.ID)
	if err != nil {
		return GitIdentities{}, fmt.Errorf("list git identities: %w", err)
	}
	out := GitIdentities{Items: make([]GitIdentity, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, GitIdentity{
			ID: r.ID, IntegrationID: r.IntegrationID, Provider: r.Provider,
			IntegrationName: derefOr(r.IntegrationName, hostOf(r.BaseUrl)), Handle: r.Handle,
			User: UserRef{ID: r.UserID, Username: r.Username, Name: r.UserName}, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// DeleteGitIdentity is an admin unlink. It changes another member's state, so
// it emits git_identity.removed (ids only) as the last statement of its
// transaction. A link already gone is 404.
func (s *Service) DeleteGitIdentity(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		row, err := q.DeleteGitIdentity(ctx, dbq.DeleteGitIdentityParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "git identity")
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.GitIdentityRemoved, EntityType: "org",
			EntityID: org.ID, ActorID: &p.UserID,
			Payload: map[string]any{"userId": row.UserID, "integrationId": row.IntegrationID}}, events.ScopeOrg())
		return err
	})
}
