package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/platform"
)

// ChatMaxServices is the most chat services one org may have.
const ChatMaxServices = 10

// ChatMaxName is the longest service name, in characters.
const ChatMaxName = 40

// ChatAdmin is the admin API of the org's chat services (LAUN-204,
// decisions-T36; the Slack channel it replaces is LAUN-198, decisions-T33):
// list, create, change, remove, post a test message. The webhook URL is
// write-only: no method returns it.
type ChatAdmin struct {
	pool      *pgxpool.Pool
	q         *dbq.Queries
	keys      *platform.SlackKeyring // nil: no SETTINGS_ENCRYPTION_KEY
	poster    *ChatPoster
	log       *slog.Logger
	baseURL   string
	now       func() time.Time
	testLimit *login.RouteLimit
}

// NewChatAdmin returns the service. keys may be nil (the key is not set): a
// service can then be neither created nor opened. The keyring is the one of
// the Slack channel (its HKDF info string is frozen), see sealScope.
func NewChatAdmin(pool *pgxpool.Pool, log *slog.Logger, keys *platform.SlackKeyring, poster *ChatPoster, publicBaseURL string) *ChatAdmin {
	return &ChatAdmin{pool: pool, q: dbq.New(pool), keys: keys, poster: poster, log: log,
		baseURL: trimBase(publicBaseURL), now: time.Now,
		// The test message: 6 a minute, burst 3, per process (like the mail test).
		testLimit: login.NewRouteLimit(6, 3, nil)}
}

// SetClock sets the clock (tests).
func (s *ChatAdmin) SetClock(now func() time.Time) { s.now = now }

// ChatView is one service in a response. It never holds the URL, a part of it,
// or a hash. Host is the host name alone.
type ChatView struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Kind string    `json:"kind"`
	Host string    `json:"host"`
	// TeamID is the one team whose issues the service posts (LAUN-209).
	TeamID    uuid.UUID  `json:"teamId"`
	Events    []string   `json:"events"`
	Enabled   bool       `json:"enabled"`
	LastOkAt  *time.Time `json:"lastOkAt"`
	LastError *string    `json:"lastError"`
	// SecretUnreadable: the stored URL does not open with this server's key.
	SecretUnreadable bool      `json:"secretUnreadable"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	// RateCappedCount is how many posts the hourly cap dropped in the last 24
	// hours, and LastRateCappedAt the latest of them. A count and a time only.
	RateCappedCount  int64      `json:"rateCappedCount"`
	LastRateCappedAt *time.Time `json:"lastRateCappedAt"`
}

// ChatList is the GET list body.
type ChatList struct {
	// Available: this server can store a webhook (SETTINGS_ENCRYPTION_KEY is set).
	Available bool `json:"available"`
	// LinksAvailable: PUBLIC_BASE_URL is set. Without it no post is sent
	// (a post is a link).
	LinksAvailable bool `json:"linksAvailable"`
	// Limit is the most services one org may have.
	Limit int        `json:"limit"`
	Items []ChatView `json:"items"`
}

// ChatCreateInput is the POST body. Events may be absent (none).
type ChatCreateInput struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	TeamID     uuid.UUID `json:"teamId"`
	WebhookURL string    `json:"webhookUrl"`
	Events     []string  `json:"events"`
	Enabled    *bool     `json:"enabled"`
}

// ChatPatchInput is the PATCH body. An absent field keeps its value; Events is
// the whole list when present; WebhookURL replaces the stored URL.
type ChatPatchInput struct {
	Name       *string    `json:"name"`
	TeamID     *uuid.UUID `json:"teamId"`
	WebhookURL *string    `json:"webhookUrl"`
	Events     []string   `json:"events"`
	Enabled    *bool      `json:"enabled"`
}

// ChatTestResult is the 200 body of the test message.
type ChatTestResult struct {
	OK bool `json:"ok"`
}

// sealScope is the additional data a new service's URL is sealed under. A URL
// sealed by the Slack channel (before LAUN-204) used the org id alone; the
// migration keeps that scope (chat_services.seal_scope = the org id) so the old
// blob still opens with the same key. The scope is checked on every open, so a
// blob copied to another org or service does not open.
func sealScope(org, id uuid.UUID) string { return "svc." + org.String() + "." + id.String() }

func openChatSecret(keys *platform.SlackKeyring, row dbq.ChatService) ([]byte, error) {
	if keys == nil {
		return nil, platform.ErrUnreadable
	}
	legacy := row.Kind == ChatSlack && row.SealScope == row.OrgID.String()
	if !legacy && row.SealScope != sealScope(row.OrgID, row.ID) {
		return nil, platform.ErrUnreadable
	}
	return keys.OpenFor(platform.SlackColWebhook, row.SealScope, row.WebhookEnc)
}

func (s *ChatAdmin) view(row dbq.ChatService) ChatView {
	v := ChatView{ID: row.ID, Name: row.Name, Kind: row.Kind, Host: row.Host, TeamID: row.TeamID, Events: append([]string{}, row.Events...),
		Enabled: row.Enabled, LastOkAt: row.LastOkAt, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if _, err := openChatSecret(s.keys, row); err != nil {
		v.SecretUnreadable = true
	}
	return v
}

func (s *ChatAdmin) withCap(ctx context.Context, v ChatView, org uuid.UUID) (ChatView, error) {
	since := s.now().Add(-24 * time.Hour)
	n, err := s.q.CountChatRateCapped(ctx, dbq.CountChatRateCappedParams{OrgID: org, ServiceID: v.ID, Since: since})
	if err != nil {
		return v, fmt.Errorf("chat rate cap count: %w", err)
	}
	if n > 0 {
		last, err := s.q.LastChatRateCapped(ctx, dbq.LastChatRateCappedParams{OrgID: org, ServiceID: v.ID, Since: since})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return v, fmt.Errorf("chat rate cap last: %w", err)
		}
		if err == nil {
			v.RateCappedCount, v.LastRateCappedAt = n, &last
		}
	}
	return v, nil
}

// List is GET /chat-services.
func (s *ChatAdmin) List(ctx context.Context, org uuid.UUID) (ChatList, error) {
	rows, err := s.q.ListChatServices(ctx, org)
	if err != nil {
		return ChatList{}, fmt.Errorf("chat services: %w", err)
	}
	out := ChatList{Available: s.keys != nil, LinksAvailable: s.baseURL != "", Limit: ChatMaxServices, Items: []ChatView{}}
	for _, r := range rows {
		v, err := s.withCap(ctx, s.view(r), org)
		if err != nil {
			return ChatList{}, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

var errChatNotFound = httpx.NotFound("chat service")

// Get is GET /chat-services/{id}. Another org's service is not found.
func (s *ChatAdmin) Get(ctx context.Context, org, id uuid.UUID) (ChatView, error) {
	row, err := s.q.GetChatService(ctx, dbq.GetChatServiceParams{OrgID: org, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatView{}, errChatNotFound
	}
	if err != nil {
		return ChatView{}, fmt.Errorf("chat service: %w", err)
	}
	return s.withCap(ctx, s.view(row), org)
}

// parseChatEvents checks the list: known kinds, no duplicates, stored in the
// canonical order of Events.
func parseChatEvents(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, e := range in {
		if _, ok := ParseEvent(e); !ok {
			return nil, httpx.Invalid("events: unknown event kind")
		}
		seen[e] = true
	}
	out := []string{}
	for _, e := range Events {
		if seen[string(e)] {
			out = append(out, string(e))
		}
	}
	return out, nil
}

// parseChatName trims and checks a service name: 1 to 40 characters, no
// control characters.
func parseChatName(in string) (string, error) {
	n := strings.TrimSpace(in)
	if n == "" || utf8.RuneCountInString(n) > ChatMaxName || !utf8.ValidString(n) {
		return "", httpx.Invalid("name must be 1 to %d characters", ChatMaxName)
	}
	for _, r := range n {
		if r < ' ' || r == 0x7f {
			return "", httpx.Invalid("name must not contain control characters")
		}
	}
	return n, nil
}

func (s *ChatAdmin) noKey() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
		"this server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a webhook")
}

// checkTeam: the team must be a team of this org. Another org's team, or an
// unknown one, is 400 validation_failed (a body reference, AGENTS.md).
func checkTeam(ctx context.Context, q *dbq.Queries, org, team uuid.UUID) error {
	if team == uuid.Nil {
		return httpx.Invalid("teamId is required")
	}
	t, err := q.GetChatTeamExists(ctx, dbq.GetChatTeamExistsParams{OrgID: org, ID: team})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.Invalid("teamId: unknown team")
		}
		return fmt.Errorf("chat team: %w", err)
	}
	if t.ArchivedAt != nil {
		return httpx.Invalid("teamId: the team is archived")
	}
	return nil
}

func nameTaken() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a chat service with this name exists")
}

// Create is POST /chat-services.
func (s *ChatAdmin) Create(ctx context.Context, org uuid.UUID, p *auth.Principal, in ChatCreateInput) (ChatView, error) {
	name, err := parseChatName(in.Name)
	if err != nil {
		return ChatView{}, err
	}
	switch in.Kind {
	case ChatSlack, ChatDiscord, ChatFluxer, ChatCustom:
	default:
		return ChatView{}, httpx.Invalid("kind must be slack, discord, fluxer or custom")
	}
	evs, err := parseChatEvents(in.Events)
	if err != nil {
		return ChatView{}, err
	}
	u, err := ParseChatWebhook(in.Kind, in.WebhookURL)
	if err != nil {
		return ChatView{}, httpx.Invalid("webhookUrl: %s", err.Error())
	}
	if s.keys == nil {
		return ChatView{}, s.noKey()
	}
	id, err := uuid.NewV7()
	if err != nil {
		return ChatView{}, fmt.Errorf("service id: %w", err)
	}
	scope := sealScope(org, id)
	sealed := s.keys.SealFor(platform.SlackColWebhook, scope, []byte(in.WebhookURL))
	enabled := in.Enabled == nil || *in.Enabled
	var row dbq.ChatService
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockOrgForChat(ctx, org); err != nil {
			return fmt.Errorf("lock org: %w", err)
		}
		if err := checkTeam(ctx, q, org, in.TeamID); err != nil {
			return err
		}
		n, err := q.CountChatServices(ctx, org)
		if err != nil {
			return fmt.Errorf("count chat services: %w", err)
		}
		if n >= ChatMaxServices {
			return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationLimit,
				"an org may have at most %d chat services", ChatMaxServices)
		}
		row, err = q.InsertChatService(ctx, dbq.InsertChatServiceParams{ID: id, OrgID: org, Name: name, Kind: in.Kind,
			Host: ChatHost(u), WebhookEnc: sealed, SealScope: scope, Events: evs, Enabled: enabled, UpdatedBy: &p.UserID, TeamID: in.TeamID})
		if database.IsPgCode(err, database.CodeUniqueViolation) {
			return nameTaken()
		}
		if database.IsPgCode(err, database.CodeForeignKeyViolation) { // backstop: the team went away
			return httpx.Invalid("teamId: unknown team")
		}
		if err != nil {
			return fmt.Errorf("insert chat service: %w", err)
		}
		return emitChat(ctx, q, org, p, "created", row)
	})
	if err != nil {
		return ChatView{}, err
	}
	return s.view(row), nil
}

// Patch is PATCH /chat-services/{id}.
func (s *ChatAdmin) Patch(ctx context.Context, org, id uuid.UUID, p *auth.Principal, in ChatPatchInput) (ChatView, error) {
	arg := dbq.UpdateChatServiceParams{OrgID: org, ID: id, Enabled: in.Enabled, UpdatedBy: &p.UserID}
	if in.Name != nil {
		n, err := parseChatName(*in.Name)
		if err != nil {
			return ChatView{}, err
		}
		arg.Name = &n
	}
	if in.Events != nil {
		evs, err := parseChatEvents(in.Events)
		if err != nil {
			return ChatView{}, err
		}
		arg.Events = evs
	}
	if in.WebhookURL != nil && s.keys == nil {
		return ChatView{}, s.noKey()
	}
	var row dbq.ChatService
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetChatServiceForUpdate(ctx, dbq.GetChatServiceForUpdateParams{OrgID: org, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return errChatNotFound
		}
		if err != nil {
			return fmt.Errorf("chat service: %w", err)
		}
		if in.TeamID != nil {
			// Only a CHANGE is checked for archived: a service may keep its team.
			if *in.TeamID != cur.TeamID {
				if err := checkTeam(ctx, q, org, *in.TeamID); err != nil {
					return err
				}
			}
			arg.TeamID = in.TeamID
		}
		if in.WebhookURL != nil {
			// The kind never changes, so the URL is checked against the stored kind.
			u, err := ParseChatWebhook(cur.Kind, *in.WebhookURL)
			if err != nil {
				return httpx.Invalid("webhookUrl: %s", err.Error())
			}
			arg.Host = ptr(ChatHost(u))
			arg.WebhookEnc = s.keys.SealFor(platform.SlackColWebhook, cur.SealScope, []byte(*in.WebhookURL))
		}
		row, err = q.UpdateChatService(ctx, arg)
		if database.IsPgCode(err, database.CodeUniqueViolation) {
			return nameTaken()
		}
		if database.IsPgCode(err, database.CodeForeignKeyViolation) { // backstop: the team went away
			return httpx.Invalid("teamId: unknown team")
		}
		if err != nil {
			return fmt.Errorf("update chat service: %w", err)
		}
		// A disabled service drops its waiting posts: turning it on later must not
		// send them. A new team or a new webhook URL drops them too (LAUN-231): a
		// post queued for the old team must not reach the channel of the new team
		// or of the new URL, whose readers may lack access to the old team.
		teamMoved := in.TeamID != nil && *in.TeamID != cur.TeamID
		if !row.Enabled || teamMoved || in.WebhookURL != nil {
			if _, err := q.DeleteOpenChatOutbox(ctx, dbq.DeleteOpenChatOutboxParams{OrgID: org, ServiceID: id}); err != nil {
				return fmt.Errorf("delete open chat posts: %w", err)
			}
		}
		return emitChat(ctx, q, org, p, "updated", row)
	})
	if err != nil {
		return ChatView{}, err
	}
	return s.withCap(ctx, s.view(row), org)
}

// Delete is DELETE /chat-services/{id}: the service and its posts go (the
// outbox rows cascade). A missing service is not an error and emits nothing.
func (s *ChatAdmin) Delete(ctx context.Context, org, id uuid.UUID, p *auth.Principal) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetChatServiceForUpdate(ctx, dbq.GetChatServiceForUpdateParams{OrgID: org, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("chat service: %w", err)
		}
		if _, err := q.DeleteChatService(ctx, dbq.DeleteChatServiceParams{OrgID: org, ID: id}); err != nil {
			return fmt.Errorf("delete chat service: %w", err)
		}
		return emitChat(ctx, q, org, p, "deleted", cur)
	})
}

// emitChat is the LAST statement of a write. The payload holds ids, the kind
// of service, a boolean and the event kinds: no name, no URL, no host.
func emitChat(ctx context.Context, q *dbq.Queries, org uuid.UUID, p *auth.Principal, action string, row dbq.ChatService) error {
	_, err := events.Emit(ctx, q, org, events.Event{Type: events.ChatServiceUpdated, EntityType: "org", EntityID: org,
		ActorID: &p.UserID, Payload: map[string]any{"action": action, "serviceId": row.ID, "kind": row.Kind,
			"teamId": row.TeamID, "enabled": row.Enabled, "events": append([]string{}, row.Events...)}},
		events.ScopeTeams(row.TeamID)) // the service's team (T38); the payload names it

	return err
}

// Test is POST /chat-services/{id}/test: one fixed message to the service. A
// failure is 502 chat_send_failed with the fixed word.
func (s *ChatAdmin) Test(ctx context.Context, org, id uuid.UUID) (ChatTestResult, error) {
	if err := s.testLimit.Take(); err != nil {
		return ChatTestResult{}, err
	}
	row, err := s.q.GetChatService(ctx, dbq.GetChatServiceParams{OrgID: org, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatTestResult{}, errChatNotFound
	}
	if err != nil {
		return ChatTestResult{}, fmt.Errorf("chat service: %w", err)
	}
	hook, err := openChatSecret(s.keys, row)
	if err != nil {
		s.record(ctx, org, id, ChatUnreadable)
		return ChatTestResult{}, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"the stored webhook cannot be opened with this server's key; enter it again")
	}
	if perr := s.poster.Post(ctx, row.Kind, string(hook), ChatMessage{Event: EventTest}); perr != nil {
		word := ChatUnreachable
		var ce *ChatError
		if errors.As(perr, &ce) {
			word = ce.Word
		}
		s.record(ctx, org, id, word)
		// The word only: no URL, no reply.
		s.log.WarnContext(ctx, "chat test message failed", "org_id", org, "service_id", id, "reason", word)
		return ChatTestResult{}, httpx.Errorf(http.StatusBadGateway, httpx.CodeChatSendFailed, "the test message was not sent: %s", word)
	}
	s.record(ctx, org, id, "")
	return ChatTestResult{OK: true}, nil
}

// record stores the outcome of a post on the service row: word "" is success.
func (s *ChatAdmin) record(ctx context.Context, org, id uuid.UUID, word string) {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	if err := recordChatResult(rctx, s.q, org, id, s.now(), word); err != nil {
		s.log.WarnContext(ctx, "chat result not stored", "org_id", org, "service_id", id)
	}
}

func recordChatResult(ctx context.Context, q *dbq.Queries, org, id uuid.UUID, at time.Time, word string) error {
	arg := dbq.SetChatServiceResultParams{OrgID: org, ID: id, Ok: word == "", At: at}
	if word != "" {
		arg.LastError = &word
	}
	_, err := q.SetChatServiceResult(ctx, arg)
	return err
}

func ptr[T any](v T) *T { return &v }

func trimBase(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

// chatKinds is the set of kinds an event list names, for the consumer.
func chatKinds(evs []string) map[Event]bool {
	m := map[Event]bool{}
	for _, e := range evs {
		m[Event(e)] = true
	}
	return m
}
