package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/platform"
)

// Personal webhooks (LAUN-229, docs/internal/decisions-T40-personal-webhooks.md):
// a member's own webhooks. They post when an issue is assigned to the member
// or when an issue the member owns changes. Only the member reads or changes
// them; no event names them; the URL is write-only.

// PersonalMaxWebhooks is the most webhooks one member may have in one org.
const PersonalMaxWebhooks = 3

// The event choices of a personal webhook (decisions-T40 #7).
const (
	// PersonalAssigned: an issue is assigned to me.
	PersonalAssigned = "assigned"
	// PersonalMyIssues: an issue I own (assignee or creator) changes.
	PersonalMyIssues = "my_issues"
)

// PersonalEvents lists the choices in the order the page shows them.
var PersonalEvents = []string{PersonalAssigned, PersonalMyIssues}

// PersonalWebhooks is the member API of the caller's own webhooks.
type PersonalWebhooks struct {
	pool      *pgxpool.Pool
	q         *dbq.Queries
	keys      *platform.SlackKeyring // nil: no SETTINGS_ENCRYPTION_KEY
	poster    *ChatPoster
	log       *slog.Logger
	baseURL   string
	testLimit *login.KeyLimit
}

// NewPersonalWebhooks returns the service. keys may be nil (no key): a webhook
// can then be neither created nor opened. poster is the chat poster (one
// guarded client for every chat and personal post).
func NewPersonalWebhooks(pool *pgxpool.Pool, log *slog.Logger, keys *platform.SlackKeyring, poster *ChatPoster, publicBaseURL string) *PersonalWebhooks {
	return &PersonalWebhooks{pool: pool, q: dbq.New(pool), keys: keys, poster: poster, log: log,
		baseURL: trimBase(publicBaseURL),
		// The test message: 6 a minute per member and org, in memory and per
		// process. Per member, so one member cannot use up the others' tests.
		testLimit: login.NewKeyLimit(PersonalTestPerMinute, time.Minute, personalTestKeys, nil)}
}

// PersonalTestPerMinute is how many test messages one member may send a
// minute in one org.
const PersonalTestPerMinute = 6

// personalTestKeys bounds the limiter table (a full table refuses).
const personalTestKeys = 10000

// errPersonalTestLimited is the wire error of a refused test message.
var errPersonalTestLimited = httpx.Errorf(http.StatusTooManyRequests, httpx.CodeRateLimited,
	"too many test messages; try again later")

// PersonalTestLimitedError is the 429 of the test route, with the time to
// wait (the handler sets Retry-After from RetryAfterSeconds).
type PersonalTestLimitedError struct{ Wait time.Duration }

func (e *PersonalTestLimitedError) Error() string { return errPersonalTestLimited.Error() }

// Unwrap exposes the wire error.
func (e *PersonalTestLimitedError) Unwrap() error { return errPersonalTestLimited }

// RetryAfterSeconds is Wait rounded up to whole seconds, at least 1.
func (e *PersonalTestLimitedError) RetryAfterSeconds() int {
	return max(int((e.Wait+time.Second-1)/time.Second), 1)
}

// PersonalView is one webhook in a response. It never holds the URL, a part
// of it but the host, or a hash.
type PersonalView struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Kind      string     `json:"kind"`
	Host      string     `json:"host"`
	Events    []string   `json:"events"`
	Enabled   bool       `json:"enabled"`
	LastOkAt  *time.Time `json:"lastOkAt"`
	LastError *string    `json:"lastError"`
	// SecretUnreadable: the stored URL does not open with this server's key.
	SecretUnreadable bool      `json:"secretUnreadable"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// PersonalList is the GET list body.
type PersonalList struct {
	// Available: this server can store a webhook (SETTINGS_ENCRYPTION_KEY is set).
	Available bool `json:"available"`
	// LinksAvailable: PUBLIC_BASE_URL is set. Without it nothing posts.
	LinksAvailable bool           `json:"linksAvailable"`
	Limit          int            `json:"limit"`
	Items          []PersonalView `json:"items"`
}

// PersonalCreateInput is the POST body. Events absent means both choices.
type PersonalCreateInput struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	WebhookURL string   `json:"webhookUrl"`
	Events     []string `json:"events"`
	Enabled    *bool    `json:"enabled"`
}

// PersonalPatchInput is the PATCH body. An absent field keeps its value.
type PersonalPatchInput struct {
	Name       *string  `json:"name"`
	WebhookURL *string  `json:"webhookUrl"`
	Events     []string `json:"events"`
	Enabled    *bool    `json:"enabled"`
}

// personalScope is the additional data of a webhook's sealed URL: bound to
// the org, the member and the row (decisions-T40 #19).
func personalScope(org, user, id uuid.UUID) string {
	return "pwh." + org.String() + "." + user.String() + "." + id.String()
}

func openPersonalSecret(keys *platform.SlackKeyring, row dbq.PersonalWebhook) ([]byte, error) {
	if keys == nil || row.SealScope != personalScope(row.OrgID, row.UserID, row.ID) {
		return nil, platform.ErrUnreadable
	}
	return keys.OpenFor(platform.PersonalColWebhook, row.SealScope, row.WebhookEnc)
}

func (s *PersonalWebhooks) view(row dbq.PersonalWebhook) PersonalView {
	v := PersonalView{ID: row.ID, Name: row.Name, Kind: row.Kind, Host: row.Host, Events: append([]string{}, row.Events...),
		Enabled: row.Enabled, LastOkAt: row.LastOkAt, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if _, err := openPersonalSecret(s.keys, row); err != nil {
		v.SecretUnreadable = true
	}
	return v
}

var errPersonalNotFound = httpx.NotFound("webhook")

// List is GET /me/webhooks: the caller's own webhooks.
func (s *PersonalWebhooks) List(ctx context.Context, org, user uuid.UUID) (PersonalList, error) {
	rows, err := s.q.ListMyPersonalWebhooks(ctx, dbq.ListMyPersonalWebhooksParams{OrgID: org, UserID: user})
	if err != nil {
		return PersonalList{}, fmt.Errorf("personal webhooks: %w", err)
	}
	out := PersonalList{Available: s.keys != nil, LinksAvailable: s.baseURL != "", Limit: PersonalMaxWebhooks, Items: []PersonalView{}}
	for _, r := range rows {
		out.Items = append(out.Items, s.view(r))
	}
	return out, nil
}

// parsePersonalEvents checks the choices: known, no duplicates, stored in the
// order of PersonalEvents.
func parsePersonalEvents(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, e := range in {
		if e != PersonalAssigned && e != PersonalMyIssues {
			return nil, httpx.Invalid("events: must be assigned or my_issues")
		}
		seen[e] = true
	}
	out := []string{}
	for _, e := range PersonalEvents {
		if seen[e] {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *PersonalWebhooks) noKey() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
		"this server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a webhook")
}

func personalNameTaken() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "you have a webhook with this name")
}

// Create is POST /me/webhooks.
func (s *PersonalWebhooks) Create(ctx context.Context, org, user uuid.UUID, in PersonalCreateInput) (PersonalView, error) {
	name, err := parseChatName(in.Name)
	if err != nil {
		return PersonalView{}, err
	}
	switch in.Kind {
	case ChatSlack, ChatDiscord, ChatFluxer, ChatCustom:
	default:
		return PersonalView{}, httpx.Invalid("kind must be slack, discord, fluxer or custom")
	}
	evs := []string{PersonalAssigned, PersonalMyIssues}
	if in.Events != nil {
		if evs, err = parsePersonalEvents(in.Events); err != nil {
			return PersonalView{}, err
		}
	}
	u, err := ParseChatWebhook(in.Kind, in.WebhookURL)
	if err != nil {
		return PersonalView{}, httpx.Invalid("webhookUrl: %s", err.Error())
	}
	if s.keys == nil {
		return PersonalView{}, s.noKey()
	}
	id, err := uuid.NewV7()
	if err != nil {
		return PersonalView{}, fmt.Errorf("webhook id: %w", err)
	}
	scope := personalScope(org, user, id)
	sealed := s.keys.SealFor(platform.PersonalColWebhook, scope, []byte(in.WebhookURL))
	enabled := in.Enabled == nil || *in.Enabled
	var row dbq.PersonalWebhook
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.LockMemberForPersonalWebhook(ctx, dbq.LockMemberForPersonalWebhookParams{OrgID: org, UserID: user}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.NotFound("member")
			}
			return fmt.Errorf("lock member: %w", err)
		}
		n, err := q.CountMyPersonalWebhooks(ctx, dbq.CountMyPersonalWebhooksParams{OrgID: org, UserID: user})
		if err != nil {
			return fmt.Errorf("count webhooks: %w", err)
		}
		if n >= PersonalMaxWebhooks {
			return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationLimit,
				"you may have at most %d personal webhooks", PersonalMaxWebhooks)
		}
		row, err = q.InsertPersonalWebhook(ctx, dbq.InsertPersonalWebhookParams{ID: id, OrgID: org, UserID: user, Name: name,
			Kind: in.Kind, Host: ChatHost(u), WebhookEnc: sealed, SealScope: scope, Events: evs, Enabled: enabled})
		if database.IsPgCode(err, database.CodeUniqueViolation) {
			return personalNameTaken()
		}
		if err != nil {
			return fmt.Errorf("insert webhook: %w", err)
		}
		return nil // a private row of one member: no event (decisions-T40 #21)
	})
	if err != nil {
		return PersonalView{}, err
	}
	return s.view(row), nil
}

// Patch is PATCH /me/webhooks/{id}. Another member's or org's webhook is not found.
func (s *PersonalWebhooks) Patch(ctx context.Context, org, user, id uuid.UUID, in PersonalPatchInput) (PersonalView, error) {
	arg := dbq.UpdateMyPersonalWebhookParams{OrgID: org, UserID: user, ID: id, Enabled: in.Enabled}
	if in.Name != nil {
		n, err := parseChatName(*in.Name)
		if err != nil {
			return PersonalView{}, err
		}
		arg.Name = &n
	}
	if in.Events != nil {
		evs, err := parsePersonalEvents(in.Events)
		if err != nil {
			return PersonalView{}, err
		}
		arg.Events = evs
	}
	if in.WebhookURL != nil && s.keys == nil {
		return PersonalView{}, s.noKey()
	}
	var row dbq.PersonalWebhook
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetMyPersonalWebhookForUpdate(ctx, dbq.GetMyPersonalWebhookForUpdateParams{OrgID: org, UserID: user, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return errPersonalNotFound
		}
		if err != nil {
			return fmt.Errorf("webhook: %w", err)
		}
		if in.WebhookURL != nil {
			// The kind never changes, so the URL is checked against the stored kind.
			u, err := ParseChatWebhook(cur.Kind, *in.WebhookURL)
			if err != nil {
				return httpx.Invalid("webhookUrl: %s", err.Error())
			}
			arg.Host = ptr(ChatHost(u))
			arg.WebhookEnc = s.keys.SealFor(platform.PersonalColWebhook, cur.SealScope, []byte(*in.WebhookURL))
		}
		row, err = q.UpdateMyPersonalWebhook(ctx, arg)
		if database.IsPgCode(err, database.CodeUniqueViolation) {
			return personalNameTaken()
		}
		if err != nil {
			return fmt.Errorf("update webhook: %w", err)
		}
		if !row.Enabled {
			// Off drops the waiting posts: turning it on later must not send them.
			if _, err := q.DeleteOpenPersonalWebhookOutbox(ctx, dbq.DeleteOpenPersonalWebhookOutboxParams{OrgID: org, WebhookID: id}); err != nil {
				return fmt.Errorf("delete open posts: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return PersonalView{}, err
	}
	return s.view(row), nil
}

// Delete is DELETE /me/webhooks/{id}: the webhook and its posts go (cascade).
// A missing one, or another member's, is not an error and removes nothing.
func (s *PersonalWebhooks) Delete(ctx context.Context, org, user, id uuid.UUID) error {
	if _, err := s.q.DeleteMyPersonalWebhook(ctx, dbq.DeleteMyPersonalWebhookParams{OrgID: org, UserID: user, ID: id}); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	return nil
}

// Test is POST /me/webhooks/{id}/test: one fixed message. A failure is 502
// chat_send_failed with the fixed word.
func (s *PersonalWebhooks) Test(ctx context.Context, org, user, id uuid.UUID) (ChatTestResult, error) {
	// The member's own webhook first: a probe of an unknown id uses no token.
	row, err := s.q.GetMyPersonalWebhook(ctx, dbq.GetMyPersonalWebhookParams{OrgID: org, UserID: user, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatTestResult{}, errPersonalNotFound
	}
	if err != nil {
		return ChatTestResult{}, fmt.Errorf("webhook: %w", err)
	}
	if err := s.testLimit.Take(org.String() + "." + user.String()); err != nil {
		var rl *login.RateLimitedError
		if errors.As(err, &rl) {
			return ChatTestResult{}, &PersonalTestLimitedError{Wait: rl.Wait}
		}
		return ChatTestResult{}, err
	}
	hook, err := openPersonalSecret(s.keys, row)
	if err != nil {
		s.record(ctx, org, id, ChatUnreadable)
		return ChatTestResult{}, httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"the stored webhook cannot be opened with this server's key; enter it again")
	}
	if perr := s.poster.Post(ctx, row.Kind, string(hook), ChatMessage{Event: EventTest, Personal: true}); perr != nil {
		word := ChatUnreachable
		var ce *ChatError
		if errors.As(perr, &ce) {
			word = ce.Word
		}
		s.record(ctx, org, id, word)
		// The word only: no URL, no host, no reply.
		s.log.WarnContext(ctx, "personal webhook test failed", "org_id", org, "webhook_id", id, "reason", word)
		return ChatTestResult{}, httpx.Errorf(http.StatusBadGateway, httpx.CodeChatSendFailed, "the test message was not sent: %s", word)
	}
	s.record(ctx, org, id, "")
	return ChatTestResult{OK: true}, nil
}

func (s *PersonalWebhooks) record(ctx context.Context, org, id uuid.UUID, word string) {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	if err := recordPersonalResult(rctx, s.q, org, id, time.Now(), word); err != nil {
		s.log.WarnContext(ctx, "personal webhook result not stored", "org_id", org, "webhook_id", id)
	}
}

func recordPersonalResult(ctx context.Context, q *dbq.Queries, org, id uuid.UUID, at time.Time, word string) error {
	arg := dbq.SetPersonalWebhookResultParams{OrgID: org, ID: id, Ok: word == "", At: at}
	if word != "" {
		arg.LastError = &word
	}
	_, err := q.SetPersonalWebhookResult(ctx, arg)
	return err
}

// personalPostKind is the post kind of a member's webhook for one event
// (decisions-T40 #10): assigned first, when the event assigns the member and
// the webhook wants it; else the change kind when the member owns the issue
// and the webhook wants my_issues; else nothing.
func personalPostKind(events []string, assigned bool, change Event) (Event, bool) {
	wants := func(e string) bool {
		for _, x := range events {
			if x == e {
				return true
			}
		}
		return false
	}
	if assigned && wants(PersonalAssigned) {
		return EventAssigned, true
	}
	if change != "" && wants(PersonalMyIssues) {
		return change, true
	}
	return "", false
}

// personalChoice is the event choice a post kind needs.
func personalChoice(kind Event) string {
	if kind == EventAssigned {
		return PersonalAssigned
	}
	return PersonalMyIssues
}

// personalDedupe is the dedupe key of a member's post: one per webhook per event.
func personalDedupe(seq int64) string { return "pwh:evt:" + fmt.Sprint(seq) }

// enqueuePersonal makes the personal webhook rows of one issue or comment
// event (decisions-T40 #11, #12). assignedTo is the member the event assigns
// (nil: none); owners are the members who own the issue (assignee, creator);
// change is the change kind of the event ("" for an event that is no change,
// such as issue.created). It is called inside the consumer's transaction,
// after every read that can poison the event.
func (c *Consumer) enqueuePersonal(ctx context.Context, q *dbq.Queries, org uuid.UUID, ev dbq.ListEventsAfterSystemRow,
	issueID, teamID uuid.UUID, assignedTo *uuid.UUID, owners []uuid.UUID, change Event, muted map[uuid.UUID]bool) (int, error) {
	type want struct {
		assigned bool
		owner    bool
	}
	cand := map[uuid.UUID]*want{}
	get := func(u uuid.UUID) *want {
		if cand[u] == nil {
			cand[u] = &want{}
		}
		return cand[u]
	}
	if assignedTo != nil {
		get(*assignedTo).assigned = true
	}
	if change != "" {
		for _, u := range owners {
			get(u).owner = true
		}
	}
	ids := make([]uuid.UUID, 0, len(cand))
	for u := range cand {
		if ev.ActorID != nil && u == *ev.ActorID {
			continue // my own change posts nothing to me
		}
		if muted[u] {
			continue // I muted the issue
		}
		ids = append(ids, u)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	hooks, err := q.ListEnabledPersonalWebhooksForUsers(ctx, dbq.ListEnabledPersonalWebhooksForUsersParams{OrgID: org, UserIds: ids})
	if err != nil || len(hooks) == 0 {
		return 0, err
	}
	// Active, human members who see the issue's team (T20 #61): the same
	// query as inbox and mail.
	eligible, err := q.ListNotifyEligibleUsers(ctx, dbq.ListNotifyEligibleUsersParams{OrgID: org, UserIds: ids, TeamID: teamID})
	if err != nil {
		return 0, err
	}
	ok := map[uuid.UUID]bool{}
	for _, u := range eligible {
		ok[u] = true
	}
	seq := ev.Seq
	n := 0
	for _, h := range hooks {
		w := cand[h.UserID]
		if w == nil || !ok[h.UserID] {
			continue
		}
		ch := Event("")
		if w.owner {
			ch = change
		}
		kind, post := personalPostKind(h.Events, w.assigned, ch)
		if !post {
			continue
		}
		_, err := q.InsertPersonalWebhookOutbox(ctx, dbq.InsertPersonalWebhookOutboxParams{OrgID: org, WebhookID: h.ID,
			Kind: string(kind), IssueID: issueID, DedupeKey: personalDedupe(ev.Seq), EventSeq: &seq})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // a replay
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
