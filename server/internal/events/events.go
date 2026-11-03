// Package events writes the change feed and fans out "something changed"
// wake-ups to SSE subscribers.
//
// Write path: Emit runs inside the writing transaction. It allocates the next
// per-org seq (the counter row lock is held to COMMIT, so seq order is commit
// order), inserts the events row and queues pg_notify, which Postgres
// delivers only on COMMIT.
//
// Read path: one Listener goroutine holds a dedicated LISTEN connection and
// calls Broker.Publish(org) per notification. SSE handlers subscribe per org,
// and on each wake-up read `events WHERE seq > last`. The notification is
// only a hint; the table is the truth, so a lost notification costs latency
// (the handler also polls), never an event. SubscribeAll wakes on any org
// (the notification consumer uses it).
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
)

// Channel is the Postgres NOTIFY channel. The payload is the org id.
const Channel = "devops_events"

// Event types.
const (
	IssueCreated    = "issue.created"
	IssueUpdated    = "issue.updated"
	IssueDeleted    = "issue.deleted" // LAUN-201
	CommentCreated  = "comment.created"
	CommentUpdated  = "comment.updated"
	CommentDeleted  = "comment.deleted"
	RelationCreated = "relation.created"
	RelationDeleted = "relation.deleted"
	LabelCreated    = "label.created"
	TeamCreated     = "team.created"

	// T3: planning objects. cycle.created and cycle.completed
	// are written by the cycle job, with no actor.
	TeamUpdated          = "team.updated"
	ProjectCreated       = "project.created"
	ProjectUpdated       = "project.updated"
	ProjectDeleted       = "project.deleted"        // LAUN-191
	TeamDeleted          = "team.deleted"           // LAUN-191
	ProjectUpdateCreated = "project_update.created" // LAUN-196
	ProjectUpdateDue     = "project.update_due"     // LAUN-196: the update job, no actor
	MilestoneCreated     = "milestone.created"
	MilestoneUpdated     = "milestone.updated"
	DocumentCreated      = "document.created"
	DocumentUpdated      = "document.updated"
	CycleCreated         = "cycle.created"
	CycleUpdated         = "cycle.updated"
	CycleCompleted       = "cycle.completed"
	CycleDeleted         = "cycle.deleted" // LAUN-172

	// attachments.
	AttachmentCreated = "attachment.created"

	// LAUN-62 (T18 #19): products. Archive and restore are product.updated;
	// product.teams_changed is a later change of the team set (a create with
	// teams emits product.created only).
	ProductCreated      = "product.created"
	ProductUpdated      = "product.updated"
	ProductDeleted      = "product.deleted"
	ProductTeamsChanged = "product.teams_changed"

	// LAUN-195 (decisions-T29): project statuses. Entity type project_status; a reorder uses the first id.
	ProjectStatusCreated     = "project_status.created"
	ProjectStatusUpdated     = "project_status.updated"
	ProjectStatusDeleted     = "project_status.deleted"
	ProjectStatusesReordered = "project_status.reordered"
	// LAUN-194 (T28): project labels. A change to the labels ON a project is
	// project.updated with "labels" in changes.
	ProjectLabelCreated = "project_label.created"
	ProjectLabelUpdated = "project_label.updated"
	ProjectLabelDeleted = "project_label.deleted"

	// LAUN-56: org settings (PATCH /orgs/{org}) and a skipped checklist step.
	// Both use entityType org and the org id as the entity id.
	OrgUpdated            = "org.updated"
	OnboardingStepChanged = "onboarding.step_changed"

	// LAUN-200 (T31): agent guidance written or cleared. entityType org, the
	// org id as the entity id; the payload holds the team key (or null) only,
	// never the text.
	AgentGuidanceUpdated = "agent_guidance.updated"

	// LAUN-199 (T34): an org admin removed a member's git handle link. Own
	// link changes are user-private and emit nothing. entityType org, the org
	// id as the entity id; the payload holds the member id and the connection
	// id only, never the handle.
	GitIdentityRemoved = "git_identity.removed"

	// LAUN-198 (T33): the org's Slack channel was set, changed or removed.
	// Legacy since LAUN-204: nothing emits it now (chat_service.updated does),
	// but old feed rows keep the type.
	// entityType org, the org id as the entity id; the payload holds a
	// "configured" boolean and the event kinds only, never the webhook URL.
	SlackUpdated = "slack.updated"

	// LAUN-204 (T36): an org chat service was created, changed or removed.
	// entityType org, the org id as the entity id; the payload holds the
	// action, the service id, its kind (slack, discord, fluxer or custom), an "enabled"
	// boolean and the event kinds only: never the name, host or webhook URL.
	// It replaces slack.updated (LAUN-198), which no route emits any more.
	ChatServiceUpdated = "chat_service.updated"

	// LAUN-197 (T35): the org's push relay was set, changed or removed.
	// entityType org, the org id as the entity id; the payload holds a
	// "configured" boolean only, never the relay URL or secret. A member's own
	// device registration is user-private and emits nothing.
	PushUpdated = "push.updated"

	// LAUN-225 (decisions-T20 #48): an org admin changed a member's team
	// access. entityType member, the member's user id as the entity id; the
	// payload holds ids and counts only (the member, the access kind, the
	// team ids, the lost and added team ids, the unassigned count). Org
	// admins only read it (scope admin).
	MemberAccessChanged = "member.access_changed"
)

// Event is one change-feed entry before it has a seq.
type Event struct {
	Type       string
	EntityType string
	EntityID   uuid.UUID
	ActorID    *uuid.UUID
	Payload    any
}

// Scope says who may read an event (decisions-T20 #53). It is frozen at emit
// time; the viewer's side is evaluated when the feed is read (#55). The zero
// Scope is not valid: Emit refuses it, so an event can never be written
// without a decision.
type Scope struct {
	kind  string
	teams []uuid.UUID
}

// The stored values of events.scope. Legacy is never written by Emit: it is
// the column default for rows made before migration 0038 (and by an older
// image after a rollback); a restricted viewer never reads one.
const (
	ScopeKindAny    = "any"
	ScopeKindAll    = "all"
	ScopeKindOrg    = "org"
	ScopeKindAdmin  = "admin"
	ScopeKindLegacy = "legacy"
)

// ScopeTeams: a viewer who sees any one of the teams reads the event (an
// issue, a comment on it, a team, a cycle). Needs at least one team.
func ScopeTeams(ids ...uuid.UUID) Scope { return Scope{kind: ScopeKindAny, teams: teamSet(ids)} }

// ScopeAllTeams: only a viewer who sees every one of the teams reads the
// event (a relation between two issues, #54). Needs at least one team.
func ScopeAllTeams(ids ...uuid.UUID) Scope { return Scope{kind: ScopeKindAll, teams: teamSet(ids)} }

// ScopeProject is the scope of a project, milestone, project update or
// document event: the project's teams at emit time, or the whole org when the
// project has no team (a project with no team is org-level, #36, #54).
func ScopeProject(teamIDs []uuid.UUID) Scope {
	if len(teamSet(teamIDs)) == 0 {
		return ScopeOrg()
	}
	return ScopeTeams(teamIDs...)
}

// ScopeOrg: every member of the org reads the event (org-level things, #7).
func ScopeOrg() Scope { return Scope{kind: ScopeKindOrg} }

// ScopeAdmin: only org admins read the event (member.access_changed, #48).
func ScopeAdmin() Scope { return Scope{kind: ScopeKindAdmin} }

// Kind is the stored scope value; TeamIDs the stored team list (sorted, no
// duplicates, never nil).
func (s Scope) Kind() string { return s.kind }

// TeamIDs returns a copy of the scope's teams.
func (s Scope) TeamIDs() []uuid.UUID { return append([]uuid.UUID{}, s.teams...) }

// teamSet sorts the ids and drops duplicates and the nil id.
func teamSet(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != uuid.Nil {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// kindScopes is the scope rule of every event type (#54): the scope kinds an
// Emit of that type may use. Emit refuses a type that is not listed and a
// scope kind the type does not allow. TestEventsEmitScope (Test D) fails when
// an event type constant has no entry.
var kindScopes = map[string][]string{
	// The issue's team.
	IssueCreated:      {ScopeKindAny},
	IssueUpdated:      {ScopeKindAny},
	IssueDeleted:      {ScopeKindAny},
	AttachmentCreated: {ScopeKindAny},
	// The issue's team; a document comment follows the document's project
	// (org when the document has no project or the project no team).
	CommentCreated: {ScopeKindAny, ScopeKindOrg},
	CommentUpdated: {ScopeKindAny, ScopeKindOrg},
	CommentDeleted: {ScopeKindAny, ScopeKindOrg},
	// Both issues' teams, all of them.
	RelationCreated: {ScopeKindAll},
	RelationDeleted: {ScopeKindAll},
	// An org label is org-wide; a team label (LAUN-54) follows its team.
	LabelCreated: {ScopeKindAny, ScopeKindOrg},
	// That team.
	TeamCreated: {ScopeKindAny},
	TeamUpdated: {ScopeKindAny},
	TeamDeleted: {ScopeKindAny},
	// The cycle's team.
	CycleCreated:   {ScopeKindAny},
	CycleUpdated:   {ScopeKindAny},
	CycleCompleted: {ScopeKindAny},
	CycleDeleted:   {ScopeKindAny},
	// The project's teams at emit time; org when it has none.
	ProjectCreated:       {ScopeKindAny, ScopeKindOrg},
	ProjectUpdated:       {ScopeKindAny, ScopeKindOrg},
	ProjectDeleted:       {ScopeKindAny, ScopeKindOrg},
	ProjectUpdateCreated: {ScopeKindAny, ScopeKindOrg},
	ProjectUpdateDue:     {ScopeKindAny, ScopeKindOrg},
	MilestoneCreated:     {ScopeKindAny, ScopeKindOrg},
	MilestoneUpdated:     {ScopeKindAny, ScopeKindOrg},
	DocumentCreated:      {ScopeKindAny, ScopeKindOrg},
	DocumentUpdated:      {ScopeKindAny, ScopeKindOrg},
	// A product follows the project rule (#69): its teams; org when none. An
	// event whose payload lists the product's team ids is all-of, so it never
	// names a hidden team to a viewer (#9).
	ProductUpdated:      {ScopeKindAny, ScopeKindOrg},
	ProductCreated:      {ScopeKindAll, ScopeKindOrg},
	ProductDeleted:      {ScopeKindAll, ScopeKindOrg},
	ProductTeamsChanged: {ScopeKindAll, ScopeKindOrg},
	// Org-wide catalogues and settings (#7): every member reads them today.
	ProjectStatusCreated:     {ScopeKindOrg},
	ProjectStatusUpdated:     {ScopeKindOrg},
	ProjectStatusDeleted:     {ScopeKindOrg},
	ProjectStatusesReordered: {ScopeKindOrg},
	ProjectLabelCreated:      {ScopeKindOrg},
	ProjectLabelUpdated:      {ScopeKindOrg},
	ProjectLabelDeleted:      {ScopeKindOrg},
	OrgUpdated:               {ScopeKindOrg},
	OnboardingStepChanged:    {ScopeKindOrg},
	GitIdentityRemoved:       {ScopeKindOrg},
	SlackUpdated:             {ScopeKindOrg},
	PushUpdated:              {ScopeKindOrg},
	// The org's guidance is org-wide; a team's names the team key.
	AgentGuidanceUpdated: {ScopeKindAny, ScopeKindOrg},
	// A chat service belongs to one team (T38); its event names the team id.
	ChatServiceUpdated: {ScopeKindAny},
	// Who sees which team is admin data (#33, #48).
	MemberAccessChanged: {ScopeKindAdmin},
}

// KindScopes returns a copy of the scope rule of every event type, for tests.
func KindScopes() map[string][]string {
	out := make(map[string][]string, len(kindScopes))
	for k, v := range kindScopes {
		out[k] = slices.Clone(v)
	}
	return out
}

// checkScope refuses an event whose scope is missing, empty where it needs a
// team, or not allowed for its type.
func checkScope(typ string, s Scope) error {
	allowed, ok := kindScopes[typ]
	switch {
	case !ok:
		return fmt.Errorf("event type %q has no scope rule (events.kindScopes)", typ)
	case s.kind == "":
		return fmt.Errorf("event %s has no scope", typ)
	case !slices.Contains(allowed, s.kind):
		return fmt.Errorf("event %s may not have scope %q (allowed %v)", typ, s.kind, allowed)
	case (s.kind == ScopeKindAny || s.kind == ScopeKindAll) && len(s.teams) == 0:
		return fmt.Errorf("event %s: scope %q needs a team", typ, s.kind)
	}
	return nil
}

// Emit appends ev to the org's feed inside the caller's transaction, with the
// scope that says who may read it (decisions-T20 #53). Call it as the last
// write of the transaction so the counter lock is short.
func Emit(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, ev Event, scope Scope) (int64, error) {
	if err := checkScope(ev.Type, scope); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return 0, fmt.Errorf("marshal event payload: %w", err)
	}
	if ev.Payload == nil {
		payload = []byte("{}")
	}
	seq, err := q.NextEventSeq(ctx, orgID)
	if err != nil {
		return 0, fmt.Errorf("next event seq: %w", err)
	}
	if err := q.InsertEvent(ctx, dbq.InsertEventParams{
		OrgID: orgID, Seq: seq, Type: ev.Type, EntityType: ev.EntityType,
		EntityID: ev.EntityID, ActorID: ev.ActorID, Payload: payload,
		Scope: scope.kind, TeamIds: scope.TeamIDs(),
	}); err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	if err := q.NotifyEvents(ctx, orgID.String()); err != nil {
		return 0, fmt.Errorf("notify events: %w", err)
	}
	return seq, nil
}

// Scoped is an event with its scope, for code that collects events and
// emits them at the end of the transaction (EmitAll).
type Scoped struct {
	Event Event
	Scope Scope
}

// In pairs an event with its scope.
func (ev Event) In(s Scope) Scoped { return Scoped{Event: ev, Scope: s} }

// EmitAll emits the events in order (each with its own scope).
func EmitAll(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, evs []Scoped) error {
	for _, ev := range evs {
		if _, err := Emit(ctx, q, orgID, ev.Event, ev.Scope); err != nil {
			return err
		}
	}
	return nil
}

// Broker fans wake-ups out to subscribers, per org. It is safe for
// concurrent use. A subscriber channel has a buffer of one, so a slow
// subscriber collapses many wake-ups into one and never blocks Publish.
type Broker struct {
	mu        sync.Mutex
	subs      map[uuid.UUID]map[chan struct{}]struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[uuid.UUID]map[chan struct{}]struct{}), done: make(chan struct{})}
}

// Subscribe returns a wake-up channel for org and a cancel func. The caller
// must call cancel exactly once (it is idempotent).
func (b *Broker) Subscribe(org uuid.UUID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	set, ok := b.subs[org]
	if !ok {
		set = make(map[chan struct{}]struct{})
		b.subs[org] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[org], ch)
			if len(b.subs[org]) == 0 {
				delete(b.subs, org)
			}
			b.mu.Unlock()
		})
	}
}

// anyOrg is the key of the subscribers of every org (SubscribeAll). It is
// never a real org id.
var anyOrg = uuid.Nil

// SubscribeAll is Subscribe for every org at once: the channel is woken by
// the Publish of any org and by PublishAll. The notification consumer uses it.
func (b *Broker) SubscribeAll() (<-chan struct{}, func()) { return b.Subscribe(anyOrg) }

// Publish wakes every subscriber of org, and of every org, without blocking.
func (b *Broker) Publish(org uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, key := range []uuid.UUID{org, anyOrg} {
		for ch := range b.subs[key] {
			select {
			case ch <- struct{}{}:
			default: // a wake-up is already pending
			}
		}
	}
}

// PublishAll wakes every subscriber. The listener calls it after a
// reconnect, because notifications sent while it was down are lost.
func (b *Broker) PublishAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, set := range b.subs {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Subscribers returns the number of live subscriptions (for tests and logs).
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, set := range b.subs {
		n += len(set)
	}
	return n
}

// Close tells every stream to finish. http.Server.Shutdown does not cancel
// the context of a long-lived request, so main calls Close from
// RegisterOnShutdown and SSE handlers select on Done.
func (b *Broker) Close() { b.closeOnce.Do(func() { close(b.done) }) }

// Done is closed by Close.
func (b *Broker) Done() <-chan struct{} { return b.done }

// Backoff bounds for the listener's reconnect loop.
const (
	listenBackoffMin = time.Second
	listenBackoffMax = 30 * time.Second
)

// Listen holds a LISTEN connection and publishes each notification to b. It
// reconnects with capped exponential backoff and returns only when ctx is
// done. After a connection that worked, the backoff starts again from 1s,
// so one drop after a long healthy run is retried quickly.
func Listen(ctx context.Context, connect func(context.Context) (*pgx.Conn, error), b *Broker, log *slog.Logger) {
	listenLoop(ctx, func(ctx context.Context) (bool, error) { return listenOnce(ctx, connect, b) }, sleepCtx, log)
}

// listenLoop is Listen's retry policy, apart from the connection so it can
// be tested. once reports whether it got as far as listening.
func listenLoop(ctx context.Context, once func(context.Context) (connected bool, err error),
	wait func(context.Context, time.Duration) bool, log *slog.Logger) {
	backoff := listenBackoffMin
	for {
		connected, err := once(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = listenBackoffMin
		}
		log.WarnContext(ctx, "event listener disconnected; reconnecting", "err", err, "backoff", backoff)
		if !wait(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, listenBackoffMax)
	}
}

// sleepCtx waits d, or reports false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func listenOnce(ctx context.Context, connect func(context.Context) (*pgx.Conn, error), b *Broker) (connected bool, err error) {
	conn, err := connect(ctx)
	if err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return false, fmt.Errorf("listen: %w", err)
	}
	b.PublishAll() // anything sent while we were not listening
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("wait: %w", err)
		}
		if org, perr := uuid.Parse(n.Payload); perr == nil {
			b.Publish(org)
		}
	}
}
