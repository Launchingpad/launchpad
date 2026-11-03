package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
)

// Consumer constants (decisions-T13 #16).
const (
	// BatchSize is the most events one transaction reads.
	BatchSize = 200
	// StaleAfter: an event older than this when read makes no notification.
	StaleAfter = 24 * time.Hour
	// ConsumerInterval is the ticker of Run; the broker wakes it sooner.
	ConsumerInterval = 10 * time.Second
)

// Consumer reads the change feed and makes notifications (decisions-T13 #15
// to #17). No write path calls it: it only reads `events` and writes
// notifications, outbox rows and its cursor, so it can be stopped, or fail,
// without a write noticing.
type Consumer struct {
	pool *pgxpool.Pool
	q    *dbq.Queries
	log  *slog.Logger
	now  func() time.Time
}

// NewConsumer returns a consumer on pool.
func NewConsumer(pool *pgxpool.Pool, log *slog.Logger) *Consumer {
	return &Consumer{pool: pool, q: dbq.New(pool), log: log, now: time.Now}
}

// SetClock sets the clock behind the stale-event rule (tests).
func (c *Consumer) SetClock(now func() time.Time) { c.now = now }

// Result counts what one run did.
type Result struct {
	Orgs          int // orgs this run held the lock for
	Events        int // events read
	Stale         int // events too old to notify
	Poison        int // events skipped as unreadable
	Notifications int // notification rows made
	Emails        int // outbox rows made
	Pushes        int // push outbox rows made (one per device)
}

func (r *Result) add(o Result) {
	r.Orgs += o.Orgs
	r.Events += o.Events
	r.Stale += o.Stale
	r.Poison += o.Poison
	r.Notifications += o.Notifications
	r.Emails += o.Emails
	r.Pushes += o.Pushes
}

// Run consumes at boot, then on every wake-up and every interval, until ctx
// ends. A lost NOTIFY costs latency only. A panic is recovered per run and
// logged; the loop goes on (decisions-T13 #37).
func (c *Consumer) Run(ctx context.Context, wake <-chan struct{}, interval time.Duration) {
	once := func() {
		defer func() {
			if r := recover(); r != nil {
				c.log.ErrorContext(ctx, "notification consumer panicked", "panic", fmt.Sprint(r))
			}
		}()
		res, err := c.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.ErrorContext(ctx, "notification consumer failed; retrying on the next tick", "err", err)
		}
		if res.Notifications+res.Stale+res.Poison > 0 {
			c.log.InfoContext(ctx, "notification consumer", "events", res.Events, "notifications", res.Notifications,
				"emails", res.Emails, "pushes", res.Pushes, "stale", res.Stale, "poison", res.Poison)
		}
	}
	once()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			once()
		case <-t.C:
			once()
		}
	}
}

// RunOnce consumes every org until each has no more events (or its lock is
// held by another instance). The errors of single orgs are joined; one broken
// org does not stop the others.
func (c *Consumer) RunOnce(ctx context.Context) (Result, error) {
	orgs, err := c.q.ListOrgIDs(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list orgs: %w", err)
	}
	var total Result
	var errs []error
	for _, org := range orgs {
		for ctx.Err() == nil {
			res, more, err := c.ConsumeOrg(ctx, org)
			total.add(res)
			if err != nil {
				errs = append(errs, fmt.Errorf("org %s: %w", org, err))
				break
			}
			if !more {
				break
			}
		}
	}
	return total, errors.Join(errs...)
}

// poison marks an event the consumer cannot use: it is logged, skipped, and
// the cursor goes on. The reason is a fixed word, never payload or text.
type poison struct{ reason string }

func (p poison) Error() string { return "poison event: " + p.reason }

// ConsumeOrg is one transaction for one org: take the try lock, read up to
// BatchSize events after the cursor, make the rows, advance the cursor,
// commit; then wake the broker. more is true when the batch was full. A
// second instance gets no lock and returns an empty result.
func (c *Consumer) ConsumeOrg(ctx context.Context, org uuid.UUID) (res Result, more bool, err error) {
	// An org with no cursor starts at its current max seq (no history).
	if err := c.q.InsertNotificationCursor(ctx, org); err != nil {
		return res, false, fmt.Errorf("cursor: %w", err)
	}
	err = database.InTx(ctx, c.pool, func(tx pgx.Tx) error {
		res = Result{} // a retried closure never double counts
		q := c.q.WithTx(tx)
		locked, err := q.TryLockNotifyOrg(ctx, org.String())
		if err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if !locked {
			return nil
		}
		res.Orgs = 1
		cur, err := q.LockNotificationCursor(ctx, org)
		if err != nil {
			return fmt.Errorf("read cursor: %w", err)
		}
		evs, err := q.ListEventsAfterSystem(ctx, dbq.ListEventsAfterSystemParams{OrgID: org, Seq: cur.LastSeq, Limit: BatchSize})
		if err != nil {
			return fmt.Errorf("read events: %w", err)
		}
		more = len(evs) == BatchSize
		if len(evs) == 0 {
			return nil
		}
		cutoff := c.now().Add(-StaleAfter)
		for _, ev := range evs {
			res.Events++
			if ev.CreatedAt.Before(cutoff) {
				res.Stale++
				continue
			}
			n, e, pu, perr := c.process(ctx, q, org, ev)
			var p poison
			if errors.As(perr, &p) {
				res.Poison++
				c.log.WarnContext(ctx, "notification consumer skipped an event",
					"org_id", org, "seq", ev.Seq, "type", ev.Type, "reason", p.reason)
				continue
			}
			if perr != nil {
				return fmt.Errorf("event %d: %w", ev.Seq, perr)
			}
			res.Notifications += n
			res.Emails += e
			res.Pushes += pu
		}
		return q.AdvanceNotificationCursor(ctx, dbq.AdvanceNotificationCursorParams{
			OrgID: org, LastSeq: evs[len(evs)-1].Seq,
		})
	})
	if err != nil {
		return Result{}, false, err
	}
	if res.Notifications > 0 {
		// After COMMIT: wake every server's streams. A failure costs latency only.
		if nerr := c.q.NotifyEvents(ctx, org.String()); nerr != nil {
			c.log.WarnContext(ctx, "notification consumer could not send its wake-up", "org_id", org, "err", nerr)
		}
	}
	return res, more, nil
}

// issuePayload is the part of an issue.created / issue.updated payload the
// consumer reads.
type issuePayload struct {
	teamID      *uuid.UUID
	assigneeID  *uuid.UUID
	hasAssignee bool
	changes     []string
	fromState   *uuid.UUID
	toState     *uuid.UUID
}

func decodeIssuePayload(raw json.RawMessage) (issuePayload, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return issuePayload{}, poison{"payload is not an object"}
	}
	var p issuePayload
	getID := func(key string) (*uuid.UUID, bool, error) {
		v, ok := m[key]
		if !ok {
			return nil, false, nil
		}
		var id *uuid.UUID
		if err := json.Unmarshal(v, &id); err != nil {
			return nil, true, poison{"bad " + key}
		}
		return id, true, nil
	}
	var err error
	if p.teamID, _, err = getID("teamId"); err != nil {
		return p, err
	}
	if p.assigneeID, p.hasAssignee, err = getID("assigneeId"); err != nil {
		return p, err
	}
	if p.fromState, _, err = getID("fromStateId"); err != nil {
		return p, err
	}
	if p.toState, _, err = getID("toStateId"); err != nil {
		return p, err
	}
	if v, ok := m["changes"]; ok {
		if err := json.Unmarshal(v, &p.changes); err != nil {
			return p, poison{"bad changes"}
		}
	}
	return p, nil
}

// candidate is one possible notification for one recipient from one event.
type candidate struct {
	kind  Event
	key   string
	data  map[string]any
	team  *uuid.UUID
	isMen bool // a direct mention
}

// precedence is the order of decisions-T13 #13.
var precedence = map[Event]int{
	EventMention: 0, EventAssigned: 1, EventComment: 2, EventStateChange: 3, EventNewIssue: 4,
}

// process makes the notifications of one event. It returns how many
// notification rows and outbox rows it made. All reads come before the
// first write, so a poison event leaves nothing behind.
func (c *Consumer) process(ctx context.Context, q *dbq.Queries, org uuid.UUID, ev dbq.ListEventsAfterSystemRow) (notifs, emails, pushes int, err error) {
	var issueID uuid.UUID
	var pl issuePayload
	var commentID *uuid.UUID
	var commentBody string
	switch ev.Type {
	case events.ProjectUpdateDue:
		return c.processProjectDue(ctx, q, org, ev)
	case events.IssueCreated, events.IssueUpdated:
		issueID = ev.EntityID
		if pl, err = decodeIssuePayload(ev.Payload); err != nil {
			return 0, 0, 0, err
		}
	case events.CommentCreated, events.CommentUpdated:
		var p struct {
			IssueID *uuid.UUID `json:"issueId"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return 0, 0, 0, poison{"payload is not an object"}
		}
		if p.IssueID == nil {
			return 0, 0, 0, nil // a document comment: not in v1
		}
		issueID = *p.IssueID
		cm, err := q.GetCommentSystem(ctx, dbq.GetCommentSystemParams{OrgID: org, ID: ev.EntityID})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, 0, poison{"comment is gone"}
		}
		if err != nil {
			return 0, 0, 0, err
		}
		id := ev.EntityID
		commentID, commentBody = &id, cm.Body
	default:
		return 0, 0, 0, nil // not a notification event
	}

	issue, err := q.GetNotifyIssue(ctx, dbq.GetNotifyIssueParams{OrgID: org, ID: issueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, poison{"issue is gone"}
	}
	if err != nil {
		return 0, 0, 0, err
	}
	if issue.IssueArchivedAt != nil || issue.TeamArchivedAt != nil {
		return 0, 0, 0, nil // #14: nothing for an archived issue or team
	}
	team := issue.TeamID
	if pl.teamID != nil {
		team = *pl.teamID
	}

	subs, err := q.ListIssueSubscriptions(ctx, dbq.ListIssueSubscriptionsParams{OrgID: org, IssueID: issueID})
	if err != nil {
		return 0, 0, 0, err
	}
	muted := map[uuid.UUID]bool{}
	for _, sub := range subs {
		if sub.State == "muted" {
			muted[sub.UserID] = true
		}
	}

	cands := map[uuid.UUID][]candidate{}
	addCand := func(u uuid.UUID, k candidate) {
		k.team = &team
		cands[u] = append(cands[u], k)
	}
	evKey := func(kind Event) string { return fmt.Sprintf("evt:%d:%s", ev.Seq, kind) }
	// assignee returns the assignee the event names; an event from before
	// the payload change falls back to the issue as it is now.
	assignee := func() *uuid.UUID {
		if pl.hasAssignee {
			return pl.assigneeID
		}
		return issue.AssigneeID
	}
	var mentionText, mentionPrefix string
	var mentionData map[string]any
	var mentionIDpart uuid.UUID
	// chatMen holds the mention candidates of every mentioned member, also
	// those who cannot see the team: the chat services are a shared channel
	// and keep their rules (decisions-T37). Only a member who sees the team
	// becomes a notification candidate (T20 #45, #61).
	chatMen := map[uuid.UUID][]candidate{}

	switch ev.Type {
	case events.IssueCreated:
		if a := assignee(); a != nil {
			addCand(*a, candidate{kind: EventAssigned, key: evKey(EventAssigned), data: map[string]any{}})
		}
		followers, err := q.ListTeamFollowers(ctx, dbq.ListTeamFollowersParams{OrgID: org, TeamID: team})
		if err != nil {
			return 0, 0, 0, err
		}
		for _, u := range followers {
			addCand(u, candidate{kind: EventNewIssue, key: evKey(EventNewIssue), data: map[string]any{}})
		}
		mentionText, mentionPrefix, mentionIDpart, mentionData = issue.Description, "mention:i:", issueID, map[string]any{}
	case events.IssueUpdated:
		if slices.Contains(pl.changes, "assignee") {
			if a := assignee(); a != nil {
				addCand(*a, candidate{kind: EventAssigned, key: evKey(EventAssigned), data: map[string]any{}})
			}
		}
		if slices.Contains(pl.changes, "state") {
			recips, err := c.issueRecipients(ctx, q, org, issue, subs, ev.CreatedAt)
			if err != nil {
				return 0, 0, 0, err
			}
			data := map[string]any{}
			if pl.fromState != nil {
				data["fromStateId"] = pl.fromState
			}
			if pl.toState != nil {
				data["toStateId"] = pl.toState
			}
			for _, u := range recips {
				addCand(u, candidate{kind: EventStateChange, key: evKey(EventStateChange), data: data})
			}
		}
		if slices.Contains(pl.changes, "description") {
			mentionText, mentionPrefix, mentionIDpart, mentionData = issue.Description, "mention:i:", issueID, map[string]any{}
		}
	case events.CommentCreated:
		recips, err := c.issueRecipients(ctx, q, org, issue, subs, ev.CreatedAt)
		if err != nil {
			return 0, 0, 0, err
		}
		for _, u := range recips {
			addCand(u, candidate{kind: EventComment, key: evKey(EventComment), data: map[string]any{"commentId": commentID}})
		}
		fallthrough
	case events.CommentUpdated:
		mentionText, mentionPrefix, mentionIDpart = commentBody, "mention:c:", *commentID
		mentionData = map[string]any{"commentId": commentID}
	}

	if mentionText != "" {
		names := ParseMentions(mentionText)
		if len(names) > 0 {
			users, err := q.ListMentionableUsers(ctx, dbq.ListMentionableUsersParams{OrgID: org, Usernames: names, TeamID: issue.TeamID})
			if err != nil {
				return 0, 0, 0, err
			}
			for _, u := range users {
				cd := candidate{kind: EventMention, key: mentionPrefix + mentionIDpart.String() + ":" + u.ID.String(),
					data: mentionData, isMen: true, team: &team}
				chatMen[u.ID] = append(chatMen[u.ID], cd)
				if u.SeesTeam {
					addCand(u.ID, cd)
				}
			}
		}
	}

	// The org's chat services (LAUN-208, decisions-T37): a shared channel posts
	// every kind the event made, whoever the issue belongs to and whoever acted.
	// It reads the event, not the recipients, so it runs before the recipient
	// filters below (which may end the function). Email, inbox and push are not
	// touched by it.
	chatBase := map[Event]bool{}
	switch ev.Type {
	case events.IssueCreated:
		chatBase[EventNewIssue] = true
		if assignee() != nil {
			chatBase[EventAssigned] = true
		}
	case events.IssueUpdated:
		if slices.Contains(pl.changes, "assignee") && assignee() != nil {
			chatBase[EventAssigned] = true
		}
		if slices.Contains(pl.changes, "state") {
			chatBase[EventStateChange] = true
		}
	case events.CommentCreated:
		chatBase[EventComment] = true
	}
	if err := c.enqueueChat(ctx, q, org, ev, []uuid.UUID{team}, &issueID, nil, "", nil, chatBase, chatMen, muted); err != nil {
		return 0, 0, 0, err
	}

	// The members' own webhooks (LAUN-229, decisions-T40): an assignment to the
	// member, or a change on an issue the member owns (assignee or creator).
	// The access is that of the issue's team, as for every person (T20 #61).
	var pwhAssigned *uuid.UUID
	var pwhChange Event
	switch ev.Type {
	case events.IssueCreated:
		pwhAssigned = assignee()
	case events.IssueUpdated:
		if slices.Contains(pl.changes, "assignee") {
			pwhAssigned = assignee()
		}
		// A move on a board alone (sortOrder) is no change of the issue.
		if slices.ContainsFunc(pl.changes, func(f string) bool { return f != "sortOrder" }) {
			pwhChange = EventIssueUpdated
		}
		if slices.Contains(pl.changes, "state") {
			pwhChange = EventStateChange
		}
	case events.CommentCreated:
		pwhChange = EventComment
	}
	var owners []uuid.UUID
	if issue.CreatorID != nil {
		owners = append(owners, *issue.CreatorID)
	}
	if a := assignee(); a != nil {
		owners = append(owners, *a)
	}
	if _, err := c.enqueuePersonal(ctx, q, org, ev, issueID, issue.TeamID, pwhAssigned, owners, pwhChange, muted); err != nil {
		return 0, 0, 0, err
	}
	if len(cands) == 0 {
		return 0, 0, 0, nil
	}

	// Recipient filters (#14): the actor, and anyone not an active
	// non-agent member who sees the issue's team (T20 #61: an assignee, a
	// subscriber, a commenter, a follower or a mentioned member without
	// access gets nothing). Then decide per recipient.
	ids := make([]uuid.UUID, 0, len(cands))
	for u := range cands {
		if ev.ActorID != nil && u == *ev.ActorID {
			continue
		}
		ids = append(ids, u)
	}
	if len(ids) == 0 {
		return 0, 0, 0, nil
	}
	eligible, err := q.ListNotifyEligibleUsers(ctx, dbq.ListNotifyEligibleUsersParams{OrgID: org, UserIds: ids, TeamID: issue.TeamID})
	if err != nil {
		return 0, 0, 0, err
	}
	slices.SortFunc(eligible, compareUUID)
	rows, err := q.ListNotificationSettingsForUsers(ctx, dbq.ListNotificationSettingsForUsersParams{OrgID: org, UserIds: eligible})
	if err != nil {
		return 0, 0, 0, err
	}
	prefRows, err := q.ListNotificationUserSettingsForUsers(ctx, dbq.ListNotificationUserSettingsForUsersParams{OrgID: org, UserIds: eligible})
	if err != nil {
		return 0, 0, 0, err
	}
	devices, err := c.pushDevices(ctx, q, org, eligible)
	if err != nil {
		return 0, 0, 0, err
	}
	rowsBy := map[uuid.UUID][]dbq.NotificationSetting{}
	for _, r := range rows {
		rowsBy[r.UserID] = append(rowsBy[r.UserID], r)
	}
	prefsBy := map[uuid.UUID]UserPrefs{}
	for _, r := range prefRows {
		prefsBy[r.UserID] = UserPrefs{EmailEnabled: r.EmailEnabled, IncludeExcerpt: r.IncludeExcerpt}
	}

	type decision struct {
		user uuid.UUID
		cand candidate
		res  Resolved
	}
	var decided []decision
	for _, u := range eligible {
		cs := cands[u]
		slices.SortStableFunc(cs, func(a, b candidate) int { return precedence[a.kind] - precedence[b.kind] })
		prefs, ok := prefsBy[u]
		if !ok {
			prefs = DefaultUserPrefs
		}
		for _, cd := range cs {
			if !cd.isMen && muted[u] {
				continue // a mute wins; only a direct mention passes (#9, #14)
			}
			if cd.isMen {
				fresh, err := c.mentionIsNew(ctx, q, org, u, cd.key, ev.Seq)
				if err != nil {
					return 0, 0, 0, err
				}
				if !fresh {
					continue // an earlier event already notified this name
				}
			}
			r := Resolve(prefs, cd.team, cd.kind, rowsBy[u])
			// Push counts only when the org has a relay and the person has a device
			// (devices is empty without a relay).
			r.Push.On = r.Push.On && len(devices[u]) > 0
			if r.Email.On || r.Inbox.On || r.Push.On {
				decided = append(decided, decision{u, cd, r})
				break // one kind per recipient per event (#13)
			}
		}
	}

	seq := ev.Seq
	for _, d := range decided {
		raw, err := json.Marshal(d.cand.data)
		if err != nil {
			return notifs, emails, pushes, fmt.Errorf("marshal notification data: %w", err)
		}
		n, err := q.InsertNotification(ctx, dbq.InsertNotificationParams{
			OrgID: org, RecipientID: d.user, Kind: string(d.cand.kind), IssueID: &issueID,
			ActorID: ev.ActorID, Data: raw, DedupeKey: d.cand.key, EventSeq: &seq, Inbox: d.res.Inbox.On,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // replay: already made, with its outbox row
		}
		if err != nil {
			return notifs, emails, pushes, err
		}
		notifs++
		if d.res.Email.On {
			if _, err := q.InsertNotificationEmail(ctx, dbq.InsertNotificationEmailParams{
				OrgID: org, NotificationID: n.ID, RecipientID: d.user, IssueID: &issueID,
			}); err != nil {
				return notifs, emails, pushes, err
			}
			emails++
		}
		if d.res.Push.On {
			np, err := c.enqueuePush(ctx, q, org, n.ID, devices[d.user])
			pushes += np
			if err != nil {
				return notifs, emails, pushes, err
			}
		}
		if d.cand.isMen {
			if err := q.InsertMentionSubscription(ctx, dbq.InsertMentionSubscriptionParams{
				OrgID: org, IssueID: issueID, UserID: d.user,
			}); err != nil {
				return notifs, emails, pushes, err
			}
		}
	}
	return notifs, emails, pushes, nil
}

// mentionIsNew reports whether the mention key is new, or was made by this
// same event (a replay must decide the same way as the first pass).
func (c *Consumer) mentionIsNew(ctx context.Context, q *dbq.Queries, org, user uuid.UUID, key string, seq int64) (bool, error) {
	got, err := q.GetNotificationEventSeqByKey(ctx, dbq.GetNotificationEventSeqByKeyParams{OrgID: org, RecipientID: user, DedupeKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return got != nil && *got == seq, nil
}

// issueRecipients are the implicit and explicit subscribers of an issue
// (#9): creator, assignee, commenters and subscribed rows. Muted users are
// filtered by the caller, for every kind but a direct mention.
func (c *Consumer) issueRecipients(ctx context.Context, q *dbq.Queries, org uuid.UUID, issue dbq.GetNotifyIssueRow,
	subs []dbq.IssueSubscription, asOf time.Time) ([]uuid.UUID, error) {
	set := map[uuid.UUID]bool{}
	if issue.CreatorID != nil {
		set[*issue.CreatorID] = true
	}
	if issue.AssigneeID != nil {
		set[*issue.AssigneeID] = true
	}
	commenters, err := q.ListIssueCommenterIDs(ctx, dbq.ListIssueCommenterIDsParams{OrgID: org, IssueID: &issue.ID, AsOf: asOf})
	if err != nil {
		return nil, err
	}
	for _, u := range commenters {
		set[u] = true
	}
	for _, s := range subs {
		// A subscription made after the event does not reach back to it.
		if s.State == "subscribed" && !s.CreatedAt.After(asOf) {
			set[s.UserID] = true
		}
	}
	out := make([]uuid.UUID, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	return out, nil
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return 0
}

// processProjectDue makes the reminder of a project.update_due event
// (LAUN-196). The recipient is the lead the job named, if that person is still
// an active, non-agent member (an agent gets nothing, #14) who sees the
// project (T20 #36, #61; a lead need not see it, #46). The setting is the
// person's global one: a project has no single team. The dedupe key holds the
// project and the due time, so a second event for one due time makes nothing.
// The row holds ids and a time only.
func (c *Consumer) processProjectDue(ctx context.Context, q *dbq.Queries, org uuid.UUID, ev dbq.ListEventsAfterSystemRow) (notifs, emails, pushes int, err error) {
	var pl struct {
		ProjectID *uuid.UUID `json:"projectId"`
		LeadID    *uuid.UUID `json:"leadId"`
		DueAt     time.Time  `json:"dueAt"`
	}
	if err := json.Unmarshal(ev.Payload, &pl); err != nil || pl.ProjectID == nil || pl.LeadID == nil || pl.DueAt.IsZero() {
		return 0, 0, 0, poison{"bad project update payload"}
	}
	proj, err := q.GetNotifyProject(ctx, dbq.GetNotifyProjectParams{OrgID: org, ID: *pl.ProjectID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && proj.ArchivedAt != nil) {
		return 0, 0, 0, nil // deleted or archived since
	}
	if err != nil {
		return 0, 0, 0, err
	}
	// The chat services need an active lead, whatever the lead's team access
	// (a shared channel, decisions-T38); the person's own notification needs
	// access to the project (below).
	active, err := q.ListActiveHumanMembers(ctx, dbq.ListActiveHumanMembersParams{OrgID: org, UserIds: []uuid.UUID{*pl.LeadID}})
	if err != nil {
		return 0, 0, 0, err
	}
	if len(active) == 0 {
		return 0, 0, 0, nil
	}
	// The teams of the project (project_teams): a reminder posts to their
	// services; a project with no team posts nowhere (decisions-T38 #8).
	projTeams, err := q.ListProjectTeamIDs(ctx, dbq.ListProjectTeamIDsParams{OrgID: org, ProjectID: *pl.ProjectID})
	if err != nil {
		return 0, 0, 0, err
	}
	if err := c.enqueueChat(ctx, q, org, ev, projTeams, nil, pl.ProjectID, fmt.Sprintf("chat:project-due:%s:%d", pl.ProjectID, pl.DueAt.Unix()), active, nil, nil, nil); err != nil {
		return 0, 0, 0, err
	}
	eligible, err := q.ListNotifyEligibleProjectUsers(ctx, dbq.ListNotifyEligibleProjectUsersParams{
		OrgID: org, UserIds: active, ProjectID: *pl.ProjectID})
	if err != nil {
		return 0, 0, 0, err
	}
	if len(eligible) == 0 {
		return 0, 0, 0, nil // the lead cannot see the project: nothing for the person
	}
	rows, err := q.ListNotificationSettingsForUsers(ctx, dbq.ListNotificationSettingsForUsersParams{OrgID: org, UserIds: eligible})
	if err != nil {
		return 0, 0, 0, err
	}
	prefs := DefaultUserPrefs
	prefRows, err := q.ListNotificationUserSettingsForUsers(ctx, dbq.ListNotificationUserSettingsForUsersParams{OrgID: org, UserIds: eligible})
	if err != nil {
		return 0, 0, 0, err
	}
	for _, r := range prefRows {
		prefs = UserPrefs{EmailEnabled: r.EmailEnabled, IncludeExcerpt: r.IncludeExcerpt}
	}
	devices, err := c.pushDevices(ctx, q, org, eligible)
	if err != nil {
		return 0, 0, 0, err
	}
	r := Resolve(prefs, nil, EventProjectUpdateDue, rows)
	r.Push.On = r.Push.On && len(devices[eligible[0]]) > 0
	if !r.Email.On && !r.Inbox.On && !r.Push.On {
		return 0, 0, 0, nil
	}
	raw, err := json.Marshal(map[string]any{"dueAt": pl.DueAt.UTC().Format(time.RFC3339)})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("marshal notification data: %w", err)
	}
	seq := ev.Seq
	n, err := q.InsertNotification(ctx, dbq.InsertNotificationParams{
		OrgID: org, RecipientID: eligible[0], Kind: string(EventProjectUpdateDue), ProjectID: pl.ProjectID,
		Data: raw, DedupeKey: fmt.Sprintf("project-due:%s:%d", pl.ProjectID, pl.DueAt.Unix()), EventSeq: &seq, Inbox: r.Inbox.On,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, nil // replay: already made
	}
	if err != nil {
		return 0, 0, 0, err
	}
	notifs = 1
	if r.Email.On {
		if _, err := q.InsertNotificationEmail(ctx, dbq.InsertNotificationEmailParams{
			OrgID: org, NotificationID: n.ID, RecipientID: eligible[0], ProjectID: pl.ProjectID,
		}); err != nil {
			return notifs, 0, 0, err
		}
		emails = 1
	}
	if r.Push.On {
		if pushes, err = c.enqueuePush(ctx, q, org, n.ID, devices[eligible[0]]); err != nil {
			return notifs, emails, pushes, err
		}
	}
	return notifs, emails, pushes, nil
}

// pushDevices returns the device ids of the recipients, by user (LAUN-197,
// decisions-T35). It returns an empty map when the org has no push relay, so
// the consumer then makes no push row. Ids only: no token is read.
func (c *Consumer) pushDevices(ctx context.Context, q *dbq.Queries, org uuid.UUID, users []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	out := map[uuid.UUID][]uuid.UUID{}
	on, err := q.PushRelayConfigured(ctx, org)
	if err != nil {
		return nil, err
	}
	if !on || len(users) == 0 {
		return out, nil
	}
	rows, err := q.ListPushDevicesForUsers(ctx, dbq.ListPushDevicesForUsersParams{OrgID: org, UserIds: users})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.ID)
	}
	return out, nil
}

// enqueuePush makes one push outbox row per device of the recipient for one
// notification. It returns how many rows are new. A replay makes none.
func (c *Consumer) enqueuePush(ctx context.Context, q *dbq.Queries, org, notification uuid.UUID, devices []uuid.UUID) (int, error) {
	n := 0
	for _, d := range devices {
		_, err := q.InsertPushOutbox(ctx, dbq.InsertPushOutboxParams{OrgID: org, NotificationID: notification, DeviceID: d})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// enqueueChat makes the chat outbox rows of one event (LAUN-198, LAUN-204,
// LAUN-208; decisions-T33 #6 to #9, decisions-T36, decisions-T37). It reads the
// enabled services of the event's teams first (LAUN-209, decisions-T38: an
// event posts only to the services of its team) and does nothing when it has none. A service
// gets one row per kind of the event, once per event.
//
// For an issue event the kinds come from the event itself (base): no recipient
// is needed, so an agent's change, or an issue nobody follows, posts. Two rules
// remain: an issue that ANY active member has muted posts nothing (a shared
// channel has no single muter), and a mention is posted only when an eligible
// person (active, not an agent, not the actor) is newly mentioned. A project
// reminder (projectID set) still needs its lead to be eligible, which the
// caller passes as eligible. The person's own settings never matter, and
// neither does the person's team access (T20): cands holds the mention
// candidates of every mentioned member, eligible means active and human, and
// a mention is new by the chat's own record (chat_mentions), not by the
// person's notification rows. The dedupe
// key holds the event seq, so a replay makes no second post. Called after every
// read that can poison the event and inside the same transaction as the
// notification writes.
func (c *Consumer) enqueueChat(ctx context.Context, q *dbq.Queries, org uuid.UUID, ev dbq.ListEventsAfterSystemRow,
	teams []uuid.UUID, issueID, projectID *uuid.UUID, projectKey string, eligible []uuid.UUID, base map[Event]bool,
	cands map[uuid.UUID][]candidate, muted map[uuid.UUID]bool) error {
	if len(teams) == 0 {
		return nil // an event with no team posts nowhere (decisions-T38 #7)
	}
	svcs, err := q.ListEnabledChatServiceEvents(ctx, dbq.ListEnabledChatServiceEventsParams{OrgID: org, TeamIds: teams})
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		return nil
	}
	wants := make([]map[Event]bool, len(svcs))
	anyKind := false
	for i, sv := range svcs {
		wants[i] = chatKinds(sv.Events)
		anyKind = anyKind || len(wants[i]) > 0
	}
	if !anyKind {
		return nil
	}
	kinds := map[Event]string{} // kind -> dedupe key
	if projectID != nil {
		if len(eligible) == 0 {
			return nil
		}
		kinds[EventProjectUpdateDue] = projectKey
	} else {
		if len(muted) > 0 {
			mutedIDs := make([]uuid.UUID, 0, len(muted))
			for u := range muted {
				mutedIDs = append(mutedIDs, u)
			}
			who, err := q.ListActiveHumanMembers(ctx, dbq.ListActiveHumanMembersParams{OrgID: org, UserIds: mutedIDs})
			if err != nil {
				return err
			}
			if len(who) > 0 {
				return nil // an active member muted the issue: the channel stays quiet
			}
		}
		for k := range base {
			kinds[k] = fmt.Sprintf("chat:evt:%d:%s", ev.Seq, k)
		}
		var menIDs []uuid.UUID
		for u, cs := range cands {
			if ev.ActorID != nil && u == *ev.ActorID {
				continue
			}
			if slices.ContainsFunc(cs, func(cd candidate) bool { return cd.isMen }) {
				menIDs = append(menIDs, u)
			}
		}
		if len(menIDs) > 0 {
			men, err := q.ListActiveHumanMembers(ctx, dbq.ListActiveHumanMembersParams{OrgID: org, UserIds: menIDs})
			if err != nil {
				return err
			}
			slices.SortFunc(men, compareUUID)
			for _, u := range men {
				for _, cd := range cands[u] {
					if !cd.isMen {
						continue
					}
					// The chat's own record, never the person's notification
					// rows: those exist only for a person who sees the team, so
					// they would make the post count an access probe (T20 #45).
					got, err := q.ClaimChatMention(ctx, dbq.ClaimChatMentionParams{OrgID: org, DedupeKey: cd.key, EventSeq: ev.Seq})
					if err != nil {
						return err
					}
					if got == ev.Seq {
						kinds[EventMention] = fmt.Sprintf("chat:evt:%d:%s", ev.Seq, EventMention)
					}
				}
			}
		}
	}
	seq := ev.Seq
	for i, sv := range svcs {
		for _, k := range Events { // a fixed order
			key, ok := kinds[k]
			if !ok || !wants[i][k] {
				continue
			}
			if _, err := q.InsertChatOutbox(ctx, dbq.InsertChatOutboxParams{
				OrgID: org, ServiceID: sv.ID, Kind: string(k), IssueID: issueID, ProjectID: projectID, DedupeKey: key, EventSeq: &seq,
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) { // ErrNoRows: a replay
				return err
			}
		}
	}
	return nil
}
