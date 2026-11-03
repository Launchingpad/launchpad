package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/scm"
)

// The GitLab merge_request webhook (T5). One event is ONE
// transaction: the idempotency row, the links, the moves, their history rows
// and their events commit together or not at all. A database error is
// returned as it is (the handler answers 500), the transaction rolls back
// with the idempotency row: a failed event keeps nothing, and processing it
// again is safe (forward-only moves, unique mr_links).
// Rules and judgment calls: docs/internal/decisions-T5.md.

// The statuses of a webhook answer. processed and ignored are also the
// status of the git_webhook_events row; duplicate and unmapped keep no
// row of their own.
const (
	WebhookProcessed = "processed"
	WebhookIgnored   = "ignored"   // an event that asks for nothing
	WebhookUnmapped  = "unmapped"  // the GitLab project is not in the map
	WebhookDuplicate = "duplicate" // one of the delivery keys (gitlab.DeliveryKeys) was seen before
)

// errUnmapped rolls back an unmapped project's event row; it never reaches
// the caller as an error.
var errUnmapped = errors.New("unmapped git repository")

// errDuplicate rolls back a delivery whose second key was seen before (see
// HandleSCMEvent); it never reaches the caller as an error.
var errDuplicate = errors.New("git delivery seen before")

const maxMRTextRunes = 500

// WebhookResult is the webhook's answer body: a status and nothing else
// (decisions-T20 #65). The status depends on the delivery (duplicate), the
// map (unmapped) and the kind of event (ignored: an event that asks for
// nothing), never on whether an issue matched, so a repository's sender
// learns nothing about the issues of a team (a hidden, a visible and a
// missing issue give the same body).
type WebhookResult struct {
	Status string `json:"status"`
	// Outcome is what the event did (processed or ignored): it goes to the
	// deliveries ring, which only org admins read, and never into the answer.
	Outcome string `json:"-"`
	// NoLink is set for a check event whose head sha no link has (LAUN-97). It
	// is not in the answer: the handler reads it to keep the event out of the
	// deliveries ring.
	NoLink bool `json:"-"`
}

// scmTally counts what one event linked and moved. The backfill (an admin
// route) reports the counts; the webhook answer never holds them (#65).
type scmTally struct {
	linked, moved int
}

// answerStatus is the status a webhook answers for an event whose plan is
// plan and whose outcome is outcome: processed for every event that asks for
// something, whether or not an issue matched or a link existed (#65).
//
// A check event for a sha no link has keeps no delivery row (LAUN-97), so a
// resend of it (GitHub's Redeliver, GitLab's Resend keep the delivery key) is
// not a duplicate, while a resend of a check that found a link is. So a
// duplicate check answers processed too: the first delivery and its resend
// answer the same whether or not an issue is linked. Every other event writes
// its delivery row before it looks at any issue, so its duplicate answer
// depends on the delivery only and stays duplicate (decisions-T5 #26: an
// unmapped event keeps no row and is applied when sent again after a map).
func answerStatus(plan scm.Plan, outcome string) string {
	switch {
	case outcome == WebhookDuplicate && plan.Checks:
		return WebhookProcessed
	case outcome == WebhookProcessed || outcome == WebhookIgnored:
		if plan.Noop() {
			return WebhookIgnored
		}
		return WebhookProcessed
	}
	return outcome
}

// stateTypeRank orders state types for the forward-only rule.
var stateTypeRank = map[string]int{"triage": 0, "backlog": 1, "unstarted": 2, "started": 3, "completed": 4, "canceled": 5}

// isForwardMove reports whether moving an issue from cur to target is
// forward: a later state type, or the same type at a later board position
// (In Progress, In Review, In Testing are all "started"). An issue in a
// completed or canceled state (Done, Canceled, Duplicate) never moves, and
// nothing moves into an archived state.
func isForwardMove(cur, target dbq.WorkflowState) bool {
	if cur.Type == "completed" || cur.Type == "canceled" || target.ArchivedAt != nil || cur.ID == target.ID {
		return false
	}
	cr, tr := stateTypeRank[cur.Type], stateTypeRank[target.Type]
	if tr != cr {
		return tr > cr
	}
	return target.Position > cur.Position
}

// cleanText replaces control characters with spaces, trims, and cuts to max
// runes. GitLab text goes into titles here, so it must pass the attachment
// title check.
func cleanText(s string, maxRunes int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > maxRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxRunes]))
	}
	return s
}

// eventSummary is what git_webhook_events.payload keeps: the fields that
// say what the event was, and no free text (no title, description, branch
// or user data), so the table never becomes a copy of GitLab content.
func eventSummary(ev scm.Event) json.RawMessage {
	m := map[string]any{"action": ev.Action, "iid": ev.Number, "draft": ev.Draft}
	if ev.Kind == scm.KindChecks { // no sha, no check name: host text and ids stay out
		m = map[string]any{"action": ev.Action, "checks": ev.Checks}
	}
	b, err := json.Marshal(m)
	if err != nil { // a map of plain values cannot fail; keep the column valid anyway
		return json.RawMessage("{}")
	}
	return b
}

// HandleSCMEvent applies one authenticated merge_request event.
// deliveryKeys are gitlab.DeliveryKeys of the request (one or two); each
// gets a git_webhook_events row (event_uuid), and the delivery is a
// duplicate if any of them exists.
func (s *Service) HandleSCMEvent(ctx context.Context, hook GitHook, deliveryKeys []string, ev scm.Event) (WebhookResult, error) {
	if len(deliveryKeys) == 0 {
		return WebhookResult{}, errors.New("git webhook: no delivery key")
	}
	var res WebhookResult
	var tally scmTally
	plan := ev.Plan()
	switch ev.Provider {
	case "gitlab", "github", "bitbucket":
	default:
		return WebhookResult{}, fmt.Errorf("scm event: unknown provider %q", ev.Provider)
	}
	// The canonical key, as the map stores it: a decimal id (GitLab, GitHub,
	// Bitbucket Data Center) or Bitbucket Cloud's lower-case uuid pair.
	repoKey, ok := scm.CanonicalRepoKey(ev.Provider, ev.RepoKey)
	if !ok {
		return WebhookResult{}, errors.New("scm event: RepoKey is not a valid repository key")
	}
	eventType := "merge_request" // the host's own word: GitLab's merge request, GitHub's pull request
	if ev.Provider != "gitlab" {
		eventType = "pull_request"
	}
	if plan.Checks {
		eventType = "checks"
	}

	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// A check event (LAUN-85) first looks for the links of its head sha:
		// with none it is answered ignored and keeps NOTHING (no delivery row),
		// since a busy repo sends many check events for commits no ticket
		// names. The link rows stay locked to the commit, so a repeat of the
		// same delivery waits here and then finds its key (decisions-T17 #19).
		var checkRows []dbq.LockMRLinksByHeadSHARow
		if plan.Checks {
			cm, err := q.GetGitlabProjectMapping(ctx, dbq.GetGitlabProjectMappingParams{
				OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errUnmapped
			} else if err != nil {
				return fmt.Errorf("project mapping: %w", err)
			}
			// Lock order (LAUN-96): the team rank locks, the issue rows, then the link rows.
			headSHA := cleanText(ev.HeadSHA, maxMRHeadSHARunes)
			issueIDs, err := lockLinkedIssues(ctx, tx, cm.TeamID,
				func() ([]uuid.UUID, error) {
					return q.ListMRLinkTeamsByHeadSHA(ctx, dbq.ListMRLinkTeamsByHeadSHAParams{OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, HeadSha: headSHA})
				},
				func() ([]lockedIssue, error) {
					rows, err := q.LockMRLinkIssuesByHeadSHA(ctx, dbq.LockMRLinkIssuesByHeadSHAParams{OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, HeadSha: headSHA})
					out := make([]lockedIssue, len(rows))
					for i, r := range rows {
						out[i] = lockedIssue{r.ID, r.TeamID}
					}
					return out, err
				})
			if err != nil {
				return err
			}
			rows, err := q.LockMRLinksByHeadSHA(ctx, dbq.LockMRLinksByHeadSHAParams{
				OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey, HeadSha: headSHA, IssueIds: issueIDs,
			})
			if err != nil {
				return fmt.Errorf("lock mr links by head sha: %w", err)
			}
			if len(rows) == 0 {
				res.Status = WebhookIgnored
				res.NoLink = true
				return nil
			}
			checkRows = rows
		}
		// Every key in a fixed order (ik: then ev:), each ON CONFLICT DO
		// NOTHING. No row back for the FIRST key: seen before, nothing was
		// written. No row back for a LATER key: the delivery was seen before
		// under its other header, so roll this transaction back (the first
		// key's new row with it) and answer duplicate. The fixed order and the
		// distinct prefixes mean two deliveries cannot wait on each other in
		// a cycle.
		eventIDs := make([]uuid.UUID, 0, len(deliveryKeys))
		for i, key := range deliveryKeys {
			id, err := q.InsertGitlabWebhookEvent(ctx, dbq.InsertGitlabWebhookEventParams{
				OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, EventUuid: key, EventType: eventType,
				RepoKey: &repoKey, Payload: eventSummary(ev),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				if i == 0 {
					res.Status = WebhookDuplicate // a delivery seen before: change nothing
					return nil
				}
				return errDuplicate
			}
			if err != nil {
				return fmt.Errorf("record webhook event: %w", err)
			}
			eventIDs = append(eventIDs, id)
		}
		finish := func(status string, note *string) error {
			for _, id := range eventIDs {
				if err := q.FinishGitlabWebhookEvent(ctx, dbq.FinishGitlabWebhookEventParams{
					OrgID: hook.OrgID, ID: id, Status: status, Error: note,
				}); err != nil {
					return fmt.Errorf("finish webhook event: %w", err)
				}
			}
			return nil
		}

		mapping, err := q.GetGitlabProjectMapping(ctx, dbq.GetGitlabProjectMappingParams{
			OrgID: hook.OrgID, IntegrationID: hook.IntegrationID, RepoKey: repoKey,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Roll the event row back: once an admin maps the project, a
			// resend of this delivery (same key) must be applied, not be
			// answered "duplicate" (decisions-T5 #26).
			return errUnmapped
		}
		if err != nil {
			return fmt.Errorf("project mapping: %w", err)
		}

		// The URL becomes an mr_links row and a link attachment. It is
		// checked only now, after the duplicate and unmapped answers, so a
		// replay is still a duplicate and an unmapped project still 202. A
		// bad URL returns an error, which rolls the event row back: nothing
		// is kept for a 400.
		if plan.Link {
			if _, err := validateLinkURL(ev.URL); err != nil {
				return httpx.Invalid("object_attributes.url must be an absolute http or https URL")
			}
		}

		var status string
		var evs []events.Scoped
		if plan.Checks {
			var changed bool
			evs, changed, err = s.applyChecks(ctx, q, hook, repoKey, ev, checkRows)
			status = WebhookIgnored // every check was older than the stored one
			if changed {
				status = WebhookProcessed
			}
		} else {
			status, evs, err = s.applySCMEvent(ctx, tx, q, hook, mapping.TeamID, repoKey, ev, plan, &tally)
		}
		if err != nil {
			return err
		}
		res.Status = status
		if err := finish(status, nil); err != nil {
			return err
		}
		// Events last: Emit holds the org's seq counter lock to COMMIT.
		return events.EmitAll(ctx, q, hook.OrgID, evs)
	})
	if errors.Is(err, errUnmapped) {
		return WebhookResult{Status: WebhookUnmapped, Outcome: WebhookUnmapped}, nil
	}
	if errors.Is(err, errDuplicate) {
		return WebhookResult{Status: answerStatus(plan, WebhookDuplicate), Outcome: WebhookDuplicate}, nil
	}
	if err != nil {
		return WebhookResult{}, err
	}
	res.Outcome = res.Status
	res.Status = answerStatus(plan, res.Status)
	return res, nil
}

// changeLabel is the start of a link attachment's title: "MR !17" for GitLab,
// "PR #17" for the others.
func changeLabel(ev scm.Event) string {
	if ev.Provider == "gitlab" {
		return fmt.Sprintf("MR !%d", ev.Number)
	}
	return fmt.Sprintf("PR #%d", ev.Number)
}

// moveByRule moves a locked issue to the target of mv when mayMoveTo allows
// it: the team's rule for the event, or the built-in move to the review or
// testing state (decisions-T44). It writes the history row (source: the
// provider, no actor) and returns the issue.updated event, or nil when nothing
// moved.
func (s *Service) moveByRule(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, cur dbq.Issue,
	mv gitMove, identifier, source string, mr map[string]any) (*events.Event, error) {
	targetID := mv.target
	// No target (no anchor state, or a rule of "no move"), an archived issue
	// (never changed silently, as in the cycle rollover) or the issue is there
	// already: nothing to do.
	if targetID == nil || cur.ArchivedAt != nil || *targetID == cur.StateID {
		return nil, nil
	}
	from, err := q.GetStateByIDSystem(ctx, dbq.GetStateByIDSystemParams{OrgID: orgID, ID: cur.StateID})
	if err != nil {
		return nil, fmt.Errorf("current state: %w", err)
	}
	to, err := q.GetStateByIDSystem(ctx, dbq.GetStateByIDSystemParams{OrgID: orgID, ID: *targetID})
	if err != nil {
		return nil, fmt.Errorf("target state: %w", err)
	}
	// A rule's state belongs to the issue's team (the composite FK and the
	// rules are read per team); the check keeps a wrong row from moving an
	// issue into another team's state.
	if to.TeamID != cur.TeamID || !mayMoveTo(mv, from, to) {
		return nil, nil
	}
	started, completed, canceled := stateTimestamps(to.Type, s.now(), cur.StartedAt, cur.CompletedAt, cur.CanceledAt)
	// Top of the new column (decisions-T14). linkAndMove holds the team's
	// rank lock.
	rank, err := topRank(ctx, q, orgID, cur.TeamID, to.ID)
	if err != nil {
		return nil, err
	}
	updated, err := q.UpdateIssue(ctx, dbq.UpdateIssueParams{
		OrgID: orgID, ID: cur.ID, Title: cur.Title, Description: cur.Description, StateID: to.ID,
		Priority: cur.Priority, Estimate: cur.Estimate, AssigneeID: cur.AssigneeID, ParentID: cur.ParentID,
		ProjectID: cur.ProjectID, MilestoneID: cur.MilestoneID, CycleID: cur.CycleID, DueDate: cur.DueDate,
		StartedAt: started, CompletedAt: completed, CanceledAt: canceled, SortOrder: rank,
	})
	if err != nil {
		return nil, fmt.Errorf("move issue: %w", err)
	}
	if err := q.InsertStateChange(ctx, dbq.InsertStateChangeParams{
		OrgID: orgID, IssueID: cur.ID, FromStateID: &from.ID, ToStateID: to.ID, Source: source,
	}); err != nil {
		return nil, fmt.Errorf("insert state change: %w", err)
	}
	return &events.Event{
		Type: events.IssueUpdated, EntityType: "issue", EntityID: cur.ID,
		Payload: map[string]any{
			"id": cur.ID, "identifier": identifier, "changes": []string{"state", "sortOrder"}, "version": updated.Version, "sortOrder": rank, "stateId": to.ID,
			"fromStateId": from.ID, "toStateId": to.ID, "source": source, "mergeRequest": mr,
			"teamId": cur.TeamID, "assigneeId": cur.AssigneeID,
		},
	}, nil
}
