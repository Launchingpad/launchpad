package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Service is the preference, team-follow and issue-subscription API
// (decisions-T13 #32). Every method works on the caller's own rows in one org.
type Service struct {
	pool      *pgxpool.Pool
	q         *dbq.Queries
	mailReady func() bool
	now       func() time.Time
}

// New returns a Service on pool.
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: dbq.New(pool), now: time.Now}
}

// SetClock sets the clock of token expiry (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetMailReady sets the check behind mail.available in the view (the server
// passes MailSwitch.Ready). Unset: mail is reported as not available.
func (s *Service) SetMailReady(f func() bool) { s.mailReady = f }

// MailReady is the check behind mail.available: the transport is ready AND
// PUBLIC_BASE_URL is set. With no base URL the sender skips every mail
// (no_base_url), so the settings page must say mail is off (LAUN-36).
func MailReady(transportReady func() bool, publicBaseURL string) func() bool {
	return func() bool {
		return strings.TrimRight(strings.TrimSpace(publicBaseURL), "/") != "" && transportReady != nil && transportReady()
	}
}

// --- wire models -------------------------------------------------------------

// DefaultPair is a built-in default: "on" or "off" per channel.
type DefaultPair struct {
	Email string `json:"email"`
	Inbox string `json:"inbox"`
	Push  string `json:"push"`
}

// Override holds stored values: "on", "off" or null (inherit).
type Override struct {
	Email *string `json:"email"`
	Inbox *string `json:"inbox"`
	Push  *string `json:"push"`
}

// ResolvedChannel is one resolved channel: "on" or "off" and its source.
type ResolvedChannel struct {
	Value  string `json:"value"`
	Source Source `json:"source"`
}

// ResolvedPair is the resolved value of both channels.
type ResolvedPair struct {
	Email ResolvedChannel `json:"email"`
	Inbox ResolvedChannel `json:"inbox"`
	Push  ResolvedChannel `json:"push"`
}

// TeamEventView is one event at one team.
type TeamEventView struct {
	TeamID   uuid.UUID    `json:"teamId"`
	TeamKey  string       `json:"teamKey"`
	Override Override     `json:"override"`
	Resolved ResolvedPair `json:"resolved"`
}

// EventView is one event: the default, the global stored values, the global
// resolved value and every team.
type EventView struct {
	Event          string          `json:"event"`
	Default        DefaultPair     `json:"default"`
	Global         Override        `json:"global"`
	Resolved       ResolvedPair    `json:"resolved"`
	RequiresFollow bool            `json:"requiresFollow"`
	Teams          []TeamEventView `json:"teams"`
}

// MailInfo says whether this install can send email at all.
type MailInfo struct {
	Available bool `json:"available"`
}

// PushInfo says whether the org has a push relay (LAUN-197). Without one the
// push channel does nothing, and the page shows no Push column.
type PushInfo struct {
	Available bool `json:"available"`
	// HasDevice: the caller has at least one registered device in this org. The Push
	// radios show only when both are true.
	HasDevice bool `json:"hasDevice"`
}

// View is GET /me/notification-settings.
type View struct {
	EmailEnabled   bool        `json:"emailEnabled"`
	IncludeExcerpt bool        `json:"includeExcerpt"`
	Mail           MailInfo    `json:"mail"`
	Push           PushInfo    `json:"push"`
	Follows        []uuid.UUID `json:"follows"`
	Events         []EventView `json:"events"`
}

// EventInput sets the stored values of one event: "on", "off", or null to
// inherit. An absent column is left as it is.
type EventInput struct {
	Email httpx.Opt[string] `json:"email"`
	Inbox httpx.Opt[string] `json:"inbox"`
	Push  httpx.Opt[string] `json:"push"`
	// unknown holds channel keys other than email, inbox and push; the decoder
	// keeps them so checkEvents can answer 400 validation_failed (not the
	// generic bad_request of an unknown field).
	unknown []string
}

// UnmarshalJSON reads email, inbox and push and records any other key.
func (e *EventInput) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k, v := range raw {
		var err error
		switch k {
		case "email":
			err = json.Unmarshal(v, &e.Email)
		case "inbox":
			err = json.Unmarshal(v, &e.Inbox)
		case "push":
			err = json.Unmarshal(v, &e.Push)
		default:
			e.unknown = append(e.unknown, k)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// SettingsInput is the PUT /me/notification-settings body (the global level).
// An absent field is left as it is; null resets emailEnabled to true and
// includeExcerpt to false.
type SettingsInput struct {
	EmailEnabled   httpx.Opt[bool]       `json:"emailEnabled"`
	IncludeExcerpt httpx.Opt[bool]       `json:"includeExcerpt"`
	Events         map[string]EventInput `json:"events"`
}

// TeamSettingsInput is the PUT /me/notification-settings/teams/{team} body.
type TeamSettingsInput struct {
	Events map[string]EventInput `json:"events"`
}

// Subscription is GET/PUT /issues/{issue}/subscription. state is "subscribed"
// or "muted" (a stored row) or "default" (no row: the implicit rules apply).
// reasons lists why the caller hears about the issue: "creator", "assignee",
// "commenter" and the stored reason ("manual", "mentioned") of a subscribed row.
type Subscription struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons"`
}

// SubscriptionInput is the PUT /issues/{issue}/subscription body.
type SubscriptionInput struct {
	Subscribed *bool `json:"subscribed"`
}

// --- the view ---------------------------------------------------------------

func onOff(b bool) string {
	if b {
		return valueOn
	}
	return valueOff
}

func resolvedPair(r Resolved) ResolvedPair {
	return ResolvedPair{
		Email: ResolvedChannel{Value: onOff(r.Email.On), Source: r.Email.Source},
		Inbox: ResolvedChannel{Value: onOff(r.Inbox.On), Source: r.Inbox.Source},
		Push:  ResolvedChannel{Value: onOff(r.Push.On), Source: r.Push.Source},
	}
}

func (s *Service) prefs(ctx context.Context, q *dbq.Queries, orgID, userID uuid.UUID) (UserPrefs, error) {
	row, err := q.GetNotificationUserSettings(ctx, dbq.GetNotificationUserSettingsParams{OrgID: orgID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultUserPrefs, nil
	}
	if err != nil {
		return UserPrefs{}, fmt.Errorf("read notification user settings: %w", err)
	}
	return UserPrefs{EmailEnabled: row.EmailEnabled, IncludeExcerpt: row.IncludeExcerpt}, nil
}

// View returns the whole resolved view of the caller's settings. It lists
// only the teams the caller sees, their follows and their team rows (T20
// #64); a row of a hidden team stays stored and is not shown.
func (s *Service) View(ctx context.Context, org *auth.Org, userID uuid.UUID) (View, error) {
	return s.view(ctx, s.q, org, userID)
}

func (s *Service) view(ctx context.Context, q *dbq.Queries, org *auth.Org, userID uuid.UUID) (View, error) {
	orgID := org.ID
	all, ids := org.Access.Args()
	prefs, err := s.prefs(ctx, q, orgID, userID)
	if err != nil {
		return View{}, err
	}
	rows, err := q.ListNotificationSettings(ctx, dbq.ListNotificationSettingsParams{OrgID: orgID, UserID: userID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return View{}, fmt.Errorf("list notification settings: %w", err)
	}
	teams, err := q.ListTeams(ctx, dbq.ListTeamsParams{OrgID: orgID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return View{}, fmt.Errorf("list teams: %w", err)
	}
	follows, err := q.ListTeamFollows(ctx, dbq.ListTeamFollowsParams{OrgID: orgID, UserID: userID, AllTeams: all, TeamIds: ids})
	if err != nil {
		return View{}, fmt.Errorf("list team follows: %w", err)
	}
	if follows == nil {
		follows = []uuid.UUID{}
	}
	pushOn, err := q.PushRelayConfigured(ctx, orgID)
	if err != nil {
		return View{}, fmt.Errorf("push relay: %w", err)
	}
	nDev, err := q.CountPushDevices(ctx, dbq.CountPushDevicesParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return View{}, fmt.Errorf("push devices: %w", err)
	}
	v := View{EmailEnabled: prefs.EmailEnabled, IncludeExcerpt: prefs.IncludeExcerpt,
		Mail: MailInfo{Available: s.mailReady != nil && s.mailReady()}, Push: PushInfo{Available: pushOn, HasDevice: nDev > 0}, Follows: follows,
		Events: make([]EventView, 0, len(Events))}
	for _, ev := range Events {
		d := DefaultFor(ev)
		e := EventView{Event: string(ev), Default: DefaultPair{Email: onOff(d.Email), Inbox: onOff(d.Inbox), Push: onOff(d.Push)},
			Resolved: resolvedPair(Resolve(prefs, nil, ev, rows)), RequiresFollow: ev == EventNewIssue,
			Teams: make([]TeamEventView, 0, len(teams))}
		for _, r := range rows {
			if r.Event == string(ev) && r.TeamID == nil {
				e.Global = Override{Email: r.Email, Inbox: r.Inbox, Push: r.Push}
			}
		}
		for _, t := range teams {
			if ev == EventProjectUpdateDue {
				break // a project belongs to several teams: this event has no team level
			}
			tid := t.ID
			te := TeamEventView{TeamID: t.ID, TeamKey: t.Key, Resolved: resolvedPair(Resolve(prefs, &tid, ev, rows))}
			for _, r := range rows {
				if r.Event == string(ev) && r.TeamID != nil && *r.TeamID == t.ID {
					te.Override = Override{Email: r.Email, Inbox: r.Inbox, Push: r.Push}
				}
			}
			e.Teams = append(e.Teams, te)
		}
		v.Events = append(v.Events, e)
	}
	return v, nil
}

// --- writes ------------------------------------------------------------------

// checkEvents validates the event names and channel values of in before any
// write, and returns the events in a fixed order.
func checkEvents(in map[string]EventInput) ([]Event, error) {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Event, 0, len(names))
	for _, name := range names {
		ev, ok := ParseEvent(name)
		if !ok {
			return nil, httpx.Invalid("unknown notification event %q", name)
		}
		if u := in[name].unknown; len(u) > 0 {
			sort.Strings(u)
			return nil, httpx.Invalid("unknown notification channel %q in events.%s (use email, inbox or push)", u[0], name)
		}
		for ch, o := range map[string]httpx.Opt[string]{"email": in[name].Email, "inbox": in[name].Inbox, "push": in[name].Push} {
			if o.Set && !o.Null && o.V != valueOn && o.V != valueOff {
				return nil, httpx.Invalid("events.%s.%s must be %q, %q or null", name, ch, valueOn, valueOff)
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

// merge applies o to the stored value cur: absent keeps, null clears.
func merge(cur *string, o httpx.Opt[string]) *string {
	switch {
	case !o.Set:
		return cur
	case o.Null:
		return nil
	default:
		v := o.V
		return &v
	}
}

// lockUser serialises one user's settings writes, so the read-merge-write of
// two requests cannot lose an update.
func lockUser(ctx context.Context, tx pgx.Tx, orgID, userID uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:notify-settings:' || $1::text || ':' || $2::text, 0))`, orgID, userID)
	if err != nil {
		return fmt.Errorf("lock notification settings: %w", err)
	}
	return nil
}

// UpdateGlobal applies the global level of a PUT /me/notification-settings.
func (s *Service) UpdateGlobal(ctx context.Context, org *auth.Org, userID uuid.UUID, in SettingsInput) (View, error) {
	orgID := org.ID
	events, err := checkEvents(in.Events)
	if err != nil {
		return View{}, err
	}
	var out View
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := lockUser(ctx, tx, orgID, userID); err != nil {
			return err
		}
		if in.EmailEnabled.Set || in.IncludeExcerpt.Set {
			p, err := s.prefs(ctx, q, orgID, userID)
			if err != nil {
				return err
			}
			if in.EmailEnabled.Set {
				p.EmailEnabled = DefaultUserPrefs.EmailEnabled
				if !in.EmailEnabled.Null {
					p.EmailEnabled = in.EmailEnabled.V
				}
			}
			if in.IncludeExcerpt.Set {
				p.IncludeExcerpt = DefaultUserPrefs.IncludeExcerpt
				if !in.IncludeExcerpt.Null {
					p.IncludeExcerpt = in.IncludeExcerpt.V
				}
			}
			if _, err := q.UpsertNotificationUserSettings(ctx, dbq.UpsertNotificationUserSettingsParams{
				OrgID: orgID, UserID: userID, EmailEnabled: p.EmailEnabled, IncludeExcerpt: p.IncludeExcerpt}); err != nil {
				return fmt.Errorf("upsert notification user settings: %w", err)
			}
		}
		if err := s.writeEvents(ctx, q, org, userID, nil, events, in.Events); err != nil {
			return err
		}
		var err error
		out, err = s.view(ctx, q, org, userID)
		return err
	})
	return out, err
}

// UpdateTeam applies PUT /me/notification-settings/teams/{team}. The team id
// is already resolved in the caller's org with the caller's scope (T20 #64).
func (s *Service) UpdateTeam(ctx context.Context, org *auth.Org, userID, teamID uuid.UUID, in TeamSettingsInput) (View, error) {
	orgID := org.ID
	events, err := checkEvents(in.Events)
	if err != nil {
		return View{}, err
	}
	if slices.Contains(events, EventProjectUpdateDue) {
		return View{}, httpx.Invalid("project_update_due has no team level: set it for the whole organisation")
	}
	var out View
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := lockUser(ctx, tx, orgID, userID); err != nil {
			return err
		}
		if err := s.writeEvents(ctx, q, org, userID, &teamID, events, in.Events); err != nil {
			return err
		}
		var err error
		out, err = s.view(ctx, q, org, userID)
		return err
	})
	return out, err
}

// writeEvents read-merges and upserts each named event at one level (team nil:
// global), then removes rows left with both columns NULL (#3).
func (s *Service) writeEvents(ctx context.Context, q *dbq.Queries, org *auth.Org, userID uuid.UUID, team *uuid.UUID, events []Event, in map[string]EventInput) error {
	if len(events) == 0 {
		return nil
	}
	orgID := org.ID
	// The read-merge reads the caller's own rows unscoped: the team was
	// resolved before this transaction, and if access went in between, a
	// scoped read would miss the stored row and the upsert would clear the
	// column the body did not name. Only the level written is merged.
	rows, err := q.ListNotificationSettingsSystem(ctx, dbq.ListNotificationSettingsSystemParams{OrgID: orgID, UserID: userID})
	if err != nil {
		return fmt.Errorf("list notification settings: %w", err)
	}
	for _, ev := range events {
		var cur dbq.NotificationSetting
		for _, r := range rows {
			if r.Event == string(ev) && ((team == nil && r.TeamID == nil) || (team != nil && r.TeamID != nil && *r.TeamID == *team)) {
				cur = r
			}
		}
		o := in[string(ev)]
		email, inbox, push := merge(cur.Email, o.Email), merge(cur.Inbox, o.Inbox), merge(cur.Push, o.Push)
		if team == nil {
			_, err = q.UpsertGlobalNotificationSetting(ctx, dbq.UpsertGlobalNotificationSettingParams{
				OrgID: orgID, UserID: userID, Event: string(ev), Email: email, Inbox: inbox, Push: push})
		} else {
			_, err = q.UpsertTeamNotificationSetting(ctx, dbq.UpsertTeamNotificationSettingParams{
				OrgID: orgID, UserID: userID, TeamID: team, Event: string(ev), Email: email, Inbox: inbox, Push: push})
		}
		if err != nil {
			return fmt.Errorf("upsert notification setting: %w", err)
		}
	}
	if err := q.DeleteEmptyNotificationSettings(ctx, dbq.DeleteEmptyNotificationSettingsParams{OrgID: orgID, UserID: userID}); err != nil {
		return fmt.Errorf("delete empty notification settings: %w", err)
	}
	return nil
}

// Follow makes the caller follow the team. A repeat changes nothing.
func (s *Service) Follow(ctx context.Context, orgID, userID, teamID uuid.UUID) error {
	if err := s.q.InsertTeamFollow(ctx, dbq.InsertTeamFollowParams{OrgID: orgID, UserID: userID, TeamID: teamID}); err != nil {
		return database.Retryable(fmt.Errorf("insert team follow: %w", err))
	}
	return nil
}

// Unfollow removes the follow. A repeat changes nothing.
func (s *Service) Unfollow(ctx context.Context, orgID, userID, teamID uuid.UUID) error {
	if err := s.q.DeleteTeamFollow(ctx, dbq.DeleteTeamFollowParams{OrgID: orgID, UserID: userID, TeamID: teamID}); err != nil {
		return fmt.Errorf("delete team follow: %w", err)
	}
	return nil
}

// --- issue subscription ------------------------------------------------------

// GetSubscription reads the caller's subscription state of an issue.
func (s *Service) GetSubscription(ctx context.Context, orgID, userID, issueID uuid.UUID) (Subscription, error) {
	return s.subscription(ctx, s.q, orgID, userID, issueID)
}

func (s *Service) subscription(ctx context.Context, q *dbq.Queries, orgID, userID, issueID uuid.UUID) (Subscription, error) {
	facts, err := q.GetIssueSubscriptionFacts(ctx, dbq.GetIssueSubscriptionFactsParams{OrgID: orgID, ID: issueID, UserID: &userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, httpx.NotFound("issue")
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("read issue subscription facts: %w", err)
	}
	sub := Subscription{State: "default", Reasons: []string{}}
	if facts.CreatorID != nil && *facts.CreatorID == userID {
		sub.Reasons = append(sub.Reasons, "creator")
	}
	if facts.AssigneeID != nil && *facts.AssigneeID == userID {
		sub.Reasons = append(sub.Reasons, "assignee")
	}
	if facts.Commenter {
		sub.Reasons = append(sub.Reasons, "commenter")
	}
	row, err := q.GetIssueSubscription(ctx, dbq.GetIssueSubscriptionParams{OrgID: orgID, IssueID: issueID, UserID: userID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Subscription{}, fmt.Errorf("read issue subscription: %w", err)
	default:
		sub.State = row.State
		if row.State == "subscribed" {
			sub.Reasons = append(sub.Reasons, row.Reason)
		}
	}
	return sub, nil
}

// SetSubscription stores subscribed (true) or muted (false) as a manual row.
func (s *Service) SetSubscription(ctx context.Context, orgID, userID, issueID uuid.UUID, subscribed bool) (Subscription, error) {
	state := "muted"
	if subscribed {
		state = "subscribed"
	}
	var out Subscription
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if _, err := q.UpsertIssueSubscription(ctx, dbq.UpsertIssueSubscriptionParams{
			OrgID: orgID, IssueID: issueID, UserID: userID, State: state, Reason: "manual"}); err != nil {
			return fmt.Errorf("upsert issue subscription: %w", err)
		}
		var err error
		out, err = s.subscription(ctx, q, orgID, userID, issueID)
		return err
	})
	return out, err
}

// DeleteSubscription removes the row: back to the implicit rules. A repeat
// changes nothing.
func (s *Service) DeleteSubscription(ctx context.Context, orgID, userID, issueID uuid.UUID) error {
	if err := s.q.DeleteIssueSubscription(ctx, dbq.DeleteIssueSubscriptionParams{OrgID: orgID, IssueID: issueID, UserID: userID}); err != nil {
		return fmt.Errorf("delete issue subscription: %w", err)
	}
	return nil
}
