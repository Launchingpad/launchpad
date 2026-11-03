package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/scm"
)

// MR state on the ticket (LAUN-83, decisions-T17 #16, #18, #20, #52). A
// merge request's state lives on its mr_links row: draft, head sha, review,
// author, repo label and the host's own "last updated" time. The state the
// Changes block shows is computed from them, never stored.

// The Changes block states (decisions-T17 #18, Chen's list of five).
const (
	CRDraft    = "draft"
	CRopen     = "open"
	CRApproved = "approved"
	CRMerged   = "merged"
	CRClosed   = "closed"
)

// Column limits of mr_links (migration 0016). A value is cut to its limit
// BEFORE the write, so a CHECK never fails an event.
const (
	maxMRAuthorRunes   = 100
	maxMRRepoPathRunes = 300
	maxMRHeadSHARunes  = 64
)

// ChangeRequest is one merge or pull request linked to an issue. Title,
// Author and RepoPath are text from the git host: show them as plain text,
// never as HTML or Markdown (decisions-T17 open point 16). Checks is null
// until a checks event has come (a later ticket).
type ChangeRequest struct {
	ID       uuid.UUID `json:"id"`
	Provider string    `json:"provider"` // gitlab | github | bitbucket: the source of the text below
	// IntegrationID is the git connection the request came through (LAUN-239):
	// the web links the host name to its Settings page for an org admin. An id
	// only; the connection itself stays admin scope.
	IntegrationID uuid.UUID `json:"integrationId"`
	RepoPath      string    `json:"repoPath"`
	Number        int64     `json:"number"`
	URL           string    `json:"url"`
	Title         string    `json:"title"`
	Author        string    `json:"author"` // the host login; empty when unknown
	// AuthorUser is the member who linked this host login on this connection
	// (LAUN-199, decisions-T34). Display only; null when nobody did.
	AuthorUser *UserRef `json:"authorUser"`
	// State is draft | open | approved | merged | closed.
	State string `json:"state"`
	// ChangesRequested flags an open request whose reviewer asked for changes
	// (it is shown as open with this flag).
	ChangesRequested bool      `json:"changesRequested"`
	Checks           *string   `json:"checks"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// changeRequestState computes the state of the Changes block from a link row.
func changeRequestState(state string, draft bool, review string) string {
	switch {
	case state == "merged":
		return CRMerged
	case state == "closed":
		return CRClosed
	case draft:
		return CRDraft
	case review == string(scm.ReviewApproved):
		return CRApproved
	default:
		return CRopen
	}
}

func (s *Service) listChangeRequests(ctx context.Context, orgID, issueID uuid.UUID) ([]ChangeRequest, error) {
	rows, err := s.q.ListIssueMRLinks(ctx, dbq.ListIssueMRLinksParams{OrgID: orgID, IssueID: issueID})
	if err != nil {
		return nil, fmt.Errorf("list change requests: %w", err)
	}
	out := make([]ChangeRequest, 0, len(rows))
	for _, r := range rows {
		st := changeRequestState(r.State, r.Draft, r.Review)
		cr := ChangeRequest{
			ID: r.ID, IntegrationID: r.IntegrationID, Provider: r.Provider, RepoPath: r.RepoPath, Number: r.Number, URL: r.Url, Title: r.Title,
			Author: r.Author, AuthorUser: userRef(r.AuthorUserID, r.AuthorUsername, r.AuthorName), State: st, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			ChangesRequested: (st == CRopen || st == CRDraft) && r.Review == string(scm.ReviewChangesRequested),
		}
		if r.Checks != "none" {
			c := r.Checks
			cr.Checks = &c
		}
		out = append(out, cr)
	}
	return out, nil
}

// mrLinkRow is the part of a link row the decisions read.
type mrLinkRow struct {
	ID                                uuid.UUID
	State, URL, Title, Branch         string
	Draft                             bool
	HeadSHA, Review, Author, RepoPath string
	ProviderUpdatedAt                 *time.Time
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// reviewOrNone keeps a review value the column accepts.
func reviewOrNone(r scm.Review) string {
	switch r {
	case scm.ReviewApproved, scm.ReviewChangesRequested:
		return string(r)
	default:
		return string(scm.ReviewNone)
	}
}

// eventOlder reports an event whose host time is older than the stored one
// (decisions-T17 #18: such an event writes nothing and moves nothing; an equal
// time is accepted, hosts give second resolution). An event or a row with no
// time cannot be told apart, so it is not stale.
func eventOlder(ev scm.Event, cur mrLinkRow) bool {
	return !ev.OccurredAt.IsZero() && cur.ProviderUpdatedAt != nil && ev.OccurredAt.Before(*cur.ProviderUpdatedAt)
}

// withEventFields returns cur with the fields an event carries. Text is
// cleaned and cut to its column; a field the event does not carry (an empty
// author, repo path or head sha) keeps the stored value. A new head sha asks
// for the checks to be reset.
func withEventFields(cur mrLinkRow, ev scm.Event) (next mrLinkRow, resetChecks bool) {
	next = cur
	if ev.URL != "" {
		if u, err := validateLinkURL(ev.URL); err == nil {
			next.URL = u
		}
	}
	next.Title = cleanText(ev.Title, maxMRTextRunes)
	next.Branch = cleanText(ev.SourceBranch, maxMRTextRunes)
	next.Draft = ev.Draft
	if ev.ReviewRead {
		next.Review = reviewOrNone(ev.Review) // the host's own answer (the backfill, LAUN-96)
	}
	if h := cleanText(ev.HeadSHA, maxMRHeadSHARunes); h != "" {
		next.HeadSHA = h
		resetChecks = cur.HeadSHA != h
	}
	if a := cleanText(ev.Author, maxMRAuthorRunes); a != "" {
		next.Author = a
	}
	if p := cleanText(ev.RepoPath, maxMRRepoPathRunes); p != "" {
		next.RepoPath = p
	}
	if t := timePtr(ev.OccurredAt); t != nil && (cur.ProviderUpdatedAt == nil || !t.Before(*cur.ProviderUpdatedAt)) {
		next.ProviderUpdatedAt = t
	}
	return next, resetChecks
}

func sameLink(a, b mrLinkRow) bool {
	return a.State == b.State && a.URL == b.URL && a.Title == b.Title && a.Branch == b.Branch && a.Draft == b.Draft &&
		a.HeadSHA == b.HeadSHA && a.Review == b.Review && a.Author == b.Author && a.RepoPath == b.RepoPath
}

func writeLink(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, l mrLinkRow, reset bool) error {
	if err := q.ApplyMRLink(ctx, dbq.ApplyMRLinkParams{
		OrgID: orgID, ID: l.ID, Url: l.URL, Title: l.Title, SourceBranch: l.Branch, State: l.State, Draft: l.Draft,
		HeadSha: l.HeadSHA, Review: l.Review, Author: l.Author, RepoPath: l.RepoPath,
		ProviderUpdatedAt: l.ProviderUpdatedAt, ResetChecks: reset,
	}); err != nil {
		return fmt.Errorf("update mr link: %w", err)
	}
	return nil
}

// mrSnap is the snapshot a timeline row keeps (decisions-T15 #4): enough to
// show the request and link to it, written when the change happened.
type mrSnap struct {
	Provider string `json:"provider"`
	RepoPath string `json:"repoPath"`
	Number   int64  `json:"number"`
	URL      string `json:"url"`
	Title    string `json:"title"`
}

func snapMR(provider string, l mrLinkRow, number int64) mrSnap {
	return mrSnap{Provider: provider, RepoPath: l.RepoPath, Number: number, URL: l.URL, Title: l.Title}
}

// stateActivity is the timeline row of a state change of an existing link:
// merged or closed. A reopen has no kind of its own (decisions-T17 #20).
func stateActivity(from, to string, snap mrSnap) []activityRow {
	switch {
	case from != "merged" && to == "merged":
		return []activityRow{{kind: ActMRMerged, to: snap}}
	case from == "opened" && to == "closed":
		return []activityRow{{kind: ActMRClosed, to: snap}}
	}
	return nil
}

// mrEvent is the issue.updated event of a link change that did not move the
// issue (a moved issue carries "changeRequests" in the event of its move). Its
// scope is the issue's team (T20 #54).
func mrEvent(issueID uuid.UUID, identifier, source string, teamID uuid.UUID, assignee *uuid.UUID, mr map[string]any) events.Scoped {
	return events.Event{
		Type: events.IssueUpdated, EntityType: "issue", EntityID: issueID,
		Payload: map[string]any{
			"id": issueID, "identifier": identifier, "changes": []string{"changeRequests"}, "source": source,
			"mergeRequest": mr, "teamId": teamID, "assigneeId": assignee,
		},
	}.In(events.ScopeTeams(teamID))
}

// addChange marks a move event as also changing the change requests.
func addChange(e *events.Event, change string) {
	if p, ok := e.Payload.(map[string]any); ok {
		if c, ok := p["changes"].([]string); ok && !slices.Contains(c, change) {
			p["changes"] = append(slices.Clone(c), change)
		}
	}
}

// applySCMEvent applies one parsed event inside the caller's transaction and
// returns the events to emit (the caller emits them last). It is the one
// writer for live webhooks and for the backfill, so both make the same links.
func (s *Service) applySCMEvent(ctx context.Context, tx pgx.Tx, q *dbq.Queries, hook GitHook, onlyTeam *uuid.UUID, repoKey string,
	ev scm.Event, plan scm.Plan, res *scmTally) (status string, evs []events.Scoped, err error) {
	switch {
	case plan.Noop():
		return WebhookIgnored, nil, nil
	case plan.Link:
		evs, err = s.linkAndMove(ctx, tx, q, hook, onlyTeam, repoKey, ev, plan, res)
		return WebhookProcessed, evs, err
	}
	evs, seen, err := s.updateLinks(ctx, tx, q, hook, onlyTeam, repoKey, ev, plan)
	if err != nil {
		return "", nil, err
	}
	if seen == 0 && !plan.CloseLinks {
		return WebhookIgnored, nil, nil // a refresh or a review of an MR no issue is linked to
	}
	return WebhookProcessed, evs, nil
}

// updateLinks applies a close, a refresh or a review to the links that exist
// for the MR (it links nothing; only a close moves, and only under a team's
// closed rule, decisions-T44). seen is how many links the
// MR has. A merged link is terminal; an event older than the stored host time
// is stale (decisions-T17 #18); both write nothing. With onlyTeam set (the
// repository's mapped team) only links of that team's issues count and change
// (decisions-T20 #65): a link made before a remap to another team is left as
// it is.
func (s *Service) updateLinks(ctx context.Context, tx pgx.Tx, q *dbq.Queries, hook GitHook, onlyTeam *uuid.UUID, repoKey string, ev scm.Event, plan scm.Plan) ([]events.Scoped, int, error) {
	number := ev.Number
	// ONE lock order for everything that writes a link (LAUN-96): the team rank
	// locks, then the issue rows, then the link rows (lockLinkedIssues).
	issueIDs, err := lockLinkedIssues(ctx, tx, onlyTeam,
		func() ([]uuid.UUID, error) {
			return q.ListMRLinkTeams(ctx, dbq.ListMRLinkTeamsParams{OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, Number: number})
		},
		func() ([]lockedIssue, error) {
			rows, err := q.LockMRLinkIssues(ctx, dbq.LockMRLinkIssuesParams{OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, Number: number})
			out := make([]lockedIssue, len(rows))
			for i, r := range rows {
				out[i] = lockedIssue{r.ID, r.TeamID}
			}
			return out, err
		})
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.LockMRLinksByMR(ctx, dbq.LockMRLinksByMRParams{OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, Number: number, IssueIds: issueIDs})
	if err != nil {
		return nil, 0, fmt.Errorf("lock mr links: %w", err)
	}
	mr := mrMarker(ev, repoKey)
	// A close moves an issue only under a team rule (decisions-T44), and only
	// an issue the event names in its branch or title (decisions-T5 #21).
	var movable map[scm.Ref]bool
	ruleCache := gitRuleCache{}
	if plan.CloseLinks {
		keys := map[string]bool{}
		for _, r := range rows {
			keys[r.TeamKey] = true
		}
		movable = map[scm.Ref]bool{}
		for _, ref := range scm.MRRefs(func(k string) bool { return keys[k] }, ev.SourceBranch, ev.Title, ev.Description) {
			if ref.Move {
				movable[ref.Ref] = true
			}
		}
	}
	var out []events.Scoped
	for _, r := range rows {
		cur := mrLinkRow{ID: r.ID, State: r.State, URL: r.Url, Title: r.Title, Branch: r.SourceBranch, Draft: r.Draft,
			HeadSHA: r.HeadSha, Review: r.Review, Author: r.Author, RepoPath: r.RepoPath, ProviderUpdatedAt: r.ProviderUpdatedAt}
		if cur.State == "merged" || eventOlder(ev, cur) {
			continue
		}
		var next mrLinkRow
		reset := false
		switch {
		case plan.CloseLinks:
			if cur.State == "closed" {
				continue
			}
			next = cur
			next.State = plan.MRState
			if t := timePtr(ev.OccurredAt); t != nil {
				next.ProviderUpdatedAt = t
			}
		case plan.Refresh:
			next, reset = withEventFields(cur, ev)
		case plan.SetReview:
			next = cur
			next.Review = string(ev.ReviewValue())
			if t := timePtr(ev.OccurredAt); t != nil {
				next.ProviderUpdatedAt = t
			}
		}
		if err := writeLink(ctx, q, hook.OrgID, next, reset); err != nil {
			return nil, 0, err
		}
		if next.State != cur.State {
			if err := insertActivity(ctx, q, hook.OrgID, r.IssueID, nil, ev.Provider, stateActivity(cur.State, next.State, snapMR(ev.Provider, next, number))...); err != nil {
				return nil, 0, err
			}
		}
		identifier := Identifier(r.TeamKey, r.IssueNumber)
		var moved *events.Event
		if plan.CloseLinks && next.State != cur.State && movable[scm.Ref{Key: r.TeamKey, Number: r.IssueNumber}] {
			rules, err := ruleCache.get(ctx, q, hook.OrgID, r.TeamID)
			if err != nil {
				return nil, 0, err
			}
			// closed has no built-in move, so the team's anchors are not read.
			if mv := resolveGitMove(dbq.Team{ID: r.TeamID}, ev, rules); mv.target != nil {
				issue, err := q.LockIssue(ctx, dbq.LockIssueParams{OrgID: hook.OrgID, ID: r.IssueID})
				if err != nil {
					return nil, 0, fmt.Errorf("lock issue: %w", err)
				}
				if moved, err = s.moveByRule(ctx, q, hook.OrgID, issue, mv, identifier, ev.Provider, mr); err != nil {
					return nil, 0, err
				}
			}
		}
		switch {
		case moved != nil:
			addChange(moved, "changeRequests")
			out = append(out, moved.In(events.ScopeTeams(r.TeamID)))
		case !sameLink(cur, next) || reset:
			out = append(out, mrEvent(r.IssueID, identifier, ev.Provider, r.TeamID, r.AssigneeID, mr))
		}
	}
	return out, len(rows), nil
}

type lockedIssue struct{ ID, TeamID uuid.UUID }

// lockLinkedIssues takes the first two steps of the lock order of a link write
// (LAUN-96): the rank lock of every team the linked issues belong to, in team
// id order, then the issue rows FOR UPDATE in id order. It returns the locked
// issue ids; the caller locks the link rows of exactly those issues. With
// only set (a repository's mapped team, decisions-T20 #65) it returns the
// issues of that team only, so a link to an issue of another team (made
// before a remap) is not written.
//
// The teams come from a plain read before any lock. A rank lock is held to the
// end of the transaction. The
// read is repeated after the locks until no new team appears (a link made in
// the gap), and the locked rows are checked against the held teams: a row of a
// team not held is an error, never a silent wrong order.
// (A repeat read that finds a new team with a lower id than one already held
// takes its rank lock out of id order; that needs a link made in the gap to an
// issue of another team, and one repo maps to one team, so it is practically
// unreachable.) (No route moves an
// issue to another team today; the check keeps it so.) The link query then
// takes only the locked issues' links: a link made in the last gap is left
// alone, its writer committed with its own state.
func lockLinkedIssues(ctx context.Context, tx pgx.Tx, only *uuid.UUID, teamsOf func() ([]uuid.UUID, error), lockIssues func() ([]lockedIssue, error)) ([]uuid.UUID, error) {
	held := map[uuid.UUID]bool{}
	for {
		teams, err := teamsOf()
		if err != nil {
			return nil, fmt.Errorf("list link teams: %w", err)
		}
		var fresh []uuid.UUID
		for _, t := range teams {
			if !held[t] {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			break
		}
		slices.SortFunc(fresh, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		for _, t := range fresh {
			if err := rankLock(ctx, tx, t); err != nil {
				return nil, err
			}
			held[t] = true
		}
	}
	rows, err := lockIssues()
	if err != nil {
		return nil, fmt.Errorf("lock mr link issues: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if !held[r.TeamID] {
			return nil, errors.New("lock mr link issues: an issue changed team during the lock")
		}
		if only != nil && r.TeamID != *only {
			continue
		}
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// mrMarker is the "mergeRequest" object of an issue.updated event.
func mrMarker(ev scm.Event, repoKey string) map[string]any {
	if ev.Provider == "gitlab" {
		pid, _ := strconv.ParseInt(repoKey, 10, 64)
		return map[string]any{"gitlabProjectId": pid, "iid": ev.Number, "url": ev.URL}
	}
	return map[string]any{"repoKey": repoKey, "number": ev.Number, "url": ev.URL}
}

// linkAndMove finds the issues the MR names, links each (mr_links row, plus
// a link attachment the first time) and moves each by the team's git workflow
// rule for the event, else by the built-in move (forward only, decisions-T44).
// It returns the events to emit.
func (s *Service) linkAndMove(ctx context.Context, tx pgx.Tx, q *dbq.Queries, hook GitHook, onlyTeam *uuid.UUID, repoKey string,
	ev scm.Event, plan scm.Plan, res *scmTally) ([]events.Scoped, error) {
	teams, err := q.ListTeamsSystem(ctx, hook.OrgID)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	byKey := make(map[string]dbq.Team, len(teams))
	byID := make(map[uuid.UUID]dbq.Team, len(teams))
	for _, t := range teams {
		if onlyTeam != nil && t.ID != *onlyTeam {
			continue
		}
		byKey[t.Key] = t
		byID[t.ID] = t
	}
	// Only issues named in the branch or the title may move; an issue named
	// only in the description is linked and keeps its state (decisions-T5).
	refs := scm.MRRefs(func(k string) bool { _, ok := byKey[k]; return ok },
		ev.SourceBranch, ev.Title, ev.Description)
	if len(refs) == 0 {
		return nil, nil
	}
	want := make(map[scm.Ref]bool, len(refs)) // ref -> may move
	keys, numbers := make([]string, 0, len(refs)), make([]int32, 0, len(refs))
	for _, r := range refs {
		want[r.Ref] = r.Move
		keys = append(keys, r.Key)
		numbers = append(numbers, r.Number)
	}
	cands, err := q.FindIssuesByKeyNumber(ctx, dbq.FindIssuesByKeyNumberParams{OrgID: hook.OrgID, Keys: keys, Numbers: numbers})
	if err != nil {
		return nil, fmt.Errorf("find linked issues: %w", err)
	}

	title := cleanText(ev.Title, maxMRTextRunes)
	attTitle := cleanText(fmt.Sprintf("%s: %s", changeLabel(ev), title), maxTitleRunes)
	attSubtitle := cleanText(ev.RepoPath, maxTitleRunes)
	number := ev.Number
	mr := mrMarker(ev, repoKey)

	// The rank lock of every team the event may move, in team id order, BEFORE
	// any issue row lock: a rebalance writes many rows, and a transaction that
	// held one of them first could deadlock with it (decisions-T14 #10).
	var teamIDs []uuid.UUID
	for _, c := range cands {
		if _, named := want[scm.Ref{Key: c.TeamKey, Number: c.Number}]; named && !slices.Contains(teamIDs, c.TeamID) {
			if _, ok := byID[c.TeamID]; ok {
				teamIDs = append(teamIDs, c.TeamID)
			}
		}
	}
	slices.SortFunc(teamIDs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, id := range teamIDs {
		if err := rankLock(ctx, tx, id); err != nil {
			return nil, err
		}
	}

	ruleCache := gitRuleCache{}
	incoming, _ := withEventFields(mrLinkRow{State: plan.MRState, Review: reviewOrNone(ev.Review)}, ev)
	incoming.State = plan.MRState
	var out []events.Scoped
	// cands is ORDER BY id: the rows are locked in id order, so two events
	// naming the same issues cannot deadlock (supabase lock-deadlock-prevention).
	for _, c := range cands {
		ref := scm.Ref{Key: c.TeamKey, Number: c.Number}
		mayMove, named := want[ref]
		team, ok := byID[c.TeamID]
		if !named || !ok {
			continue
		}
		cur, err := q.LockIssue(ctx, dbq.LockIssueParams{OrgID: hook.OrgID, ID: c.ID})
		if err != nil {
			return nil, fmt.Errorf("lock issue: %w", err)
		}
		identifier := Identifier(team.Key, cur.Number)

		stale, changed := false, false
		// transitioned: this event made the link or changed its state (or made a
		// draft ready). A team rule moves an issue only then, so an event that
		// changes nothing on the link (a second merge, an open the backfill reads
		// again) moves nothing under a rule (decisions-T44).
		transitioned := false
		_, err = q.InsertMRLink(ctx, dbq.InsertMRLinkParams{OrgID: hook.OrgID, IssueID: cur.ID, IntegrationID: hook.IntegrationID,
			RepoKey: repoKey, Number: number, Url: incoming.URL, Title: incoming.Title, State: incoming.State, SourceBranch: incoming.Branch,
			Draft: incoming.Draft, HeadSha: incoming.HeadSHA, Review: incoming.Review, Author: incoming.Author,
			RepoPath: incoming.RepoPath, ProviderUpdatedAt: incoming.ProviderUpdatedAt})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Linked before: refresh it, no second attachment, not "linked"
			// by this event. The decision is made on the locked row (LockMRLink):
			// a stale event writes nothing and must not move the issue either.
			row, err := q.LockMRLink(ctx, dbq.LockMRLinkParams{OrgID: hook.OrgID, IssueID: cur.ID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, Number: number})
			if err != nil {
				return nil, fmt.Errorf("lock mr link: %w", err)
			}
			link := mrLinkRow{ID: row.ID, State: row.State, URL: row.Url, Title: row.Title, Branch: row.SourceBranch, Draft: row.Draft,
				HeadSHA: row.HeadSha, Review: row.Review, Author: row.Author, RepoPath: row.RepoPath, ProviderUpdatedAt: row.ProviderUpdatedAt}
			next, reset, st := nextLinkForLinkPlan(link, ev, plan)
			stale = st
			if next != nil {
				if err := writeLink(ctx, q, hook.OrgID, *next, reset); err != nil {
					return nil, err
				}
				changed = !sameLink(link, *next) || reset
				transitioned = next.State != link.State || (link.Draft && !next.Draft)
				if next.State != link.State {
					if err := insertActivity(ctx, q, hook.OrgID, cur.ID, nil, ev.Provider, stateActivity(link.State, next.State, snapMR(ev.Provider, *next, number))...); err != nil {
						return nil, err
					}
				}
			}
		case err != nil:
			return nil, fmt.Errorf("insert mr link: %w", err)
		default:
			// A new link: this event linked the issue.
			res.linked++
			changed, transitioned = true, true
			rows := []activityRow{{kind: ActMRLinked, to: snapMR(ev.Provider, incoming, number)}}
			if plan.MRState == "merged" {
				rows = append(rows, activityRow{kind: ActMRMerged, to: snapMR(ev.Provider, incoming, number)})
			}
			if err := insertActivity(ctx, q, hook.OrgID, cur.ID, nil, ev.Provider, rows...); err != nil {
				return nil, err
			}
			row, err := linkAttachmentParams(cur.ID, ev.URL, attTitle)
			if err != nil {
				return nil, err
			}
			// No creator: the git host made the link, not a user (the column is
			// nullable; the API path sets the caller).
			row.OrgID, row.Subtitle, row.CreatorID = hook.OrgID, attSubtitle, nil
			att, err := q.InsertAttachment(ctx, row)
			if err != nil {
				return nil, fmt.Errorf("insert mr link attachment: %w", err)
			}
			out = append(out, events.Event{
				Type: events.AttachmentCreated, EntityType: "attachment", EntityID: att.ID,
				Payload: map[string]any{"id": att.ID, "issueId": cur.ID, "kind": "link", "source": ev.Provider},
			}.In(events.ScopeTeams(cur.TeamID)))
		}

		var mv gitMove
		if mayMove && !stale && gitRuleEvent(ev) != "" {
			rules, err := ruleCache.get(ctx, q, hook.OrgID, team.ID)
			if err != nil {
				return nil, err
			}
			mv = resolveGitMove(team, ev, rules)
			if mv.custom && !transitioned {
				mv = gitMove{}
			}
		}
		e, err := s.moveByRule(ctx, q, hook.OrgID, cur, mv, identifier, ev.Provider, mr)
		if err != nil {
			return nil, err
		}
		switch {
		case e != nil:
			res.moved++
			if changed {
				addChange(e, "changeRequests")
			}
			out = append(out, e.In(events.ScopeTeams(cur.TeamID)))
		case changed:
			out = append(out, mrEvent(cur.ID, identifier, ev.Provider, cur.TeamID, cur.AssigneeID, mr))
		}
	}
	return out, nil
}

// nextLinkForLinkPlan decides what an opened, reopened, ready or merged event
// does to a link that exists. It returns the row to write (nil: write
// nothing) and whether the event is stale, which also means the issue must not
// move. Rules (decisions-T5 #25, decisions-T17 #18):
//
//   - a merged link is terminal: nothing changes it;
//   - a merge is a fact: it sets merged even when a later event was seen, but
//     then it writes the state only (the newer fields stay);
//   - a closed link opens again only on an explicit reopen;
//   - an event older than the stored host time is stale.
func nextLinkForLinkPlan(cur mrLinkRow, ev scm.Event, plan scm.Plan) (next *mrLinkRow, reset, stale bool) {
	if cur.State == "merged" {
		return nil, false, plan.MRState != "merged"
	}
	older := eventOlder(ev, cur)
	if plan.MRState == "merged" {
		if older {
			n := cur
			n.State = "merged"
			return &n, false, false
		}
		n, r := withEventFields(cur, ev)
		n.State = "merged"
		return &n, r, false
	}
	if older || (cur.State == "closed" && plan.MRState == "opened" && !plan.Reopen) {
		return nil, false, true
	}
	n, r := withEventFields(cur, ev)
	n.State = plan.MRState
	return &n, r, false
}
