package tracker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/gitapi"
	"devops/internal/scm"
)

// The two admin actions of LAUN-83 on a mapped repo (decisions-T17 #21, #24):
// refresh its hook to the current event set, and backfill its open merge
// requests. Both call the git host through the guarded client, so both are
// token mode only and take the allowance of #28 (the handler passes allow).
// Neither emits an event of its own; a backfill writes through the same code
// as a live webhook (applySCMEvent), which emits.

// maxBackfill is the most open changes one backfill reads (decisions-T17 #21).
const maxBackfill = 200

// HookRefreshResult is the answer of a hook refresh.
type HookRefreshResult struct {
	HookState string `json:"hookState"`
}

// BackfillResult is the answer of a backfill: how many open changes were read,
// how many issues got a new link, how many issues moved forward, and whether
// more open changes were left (truncated; run it again after the oldest close).
type BackfillResult struct {
	Seen      int  `json:"seen"`
	Linked    int  `json:"linked"`
	Moved     int  `json:"moved"`
	Truncated bool `json:"truncated"`
}

// RefreshGitHook re-ensures the hook of one mapped repo with the current
// HookSpec (list and match by URL, then update), so a hook made before a new
// event set learns it. It refuses while a secret rotation is pending: the
// rotation owns the hooks until it is committed or discarded.
func (s *Service) RefreshGitHook(ctx context.Context, org *auth.Org, integrationID, id uuid.UUID, allow func() error) (HookRefreshResult, error) {
	row, err := s.getGitRepo(ctx, s.q, org.ID, integrationID, id)
	if err != nil {
		return HookRefreshResult{}, err
	}
	pre, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: integrationID})
	if err != nil {
		return HookRefreshResult{}, notFound(err, "integration")
	}
	if pre.Mode != ModeToken {
		return HookRefreshResult{}, conflict("this integration has no token; this route works in token mode only")
	}
	if len(pre.WebhookSecretNextHash) > 0 {
		return HookRefreshResult{}, conflict("a secret rotation is pending; commit or discard it before you refresh hooks")
	}
	if !s.gitPublicURLOK() {
		return HookRefreshResult{}, errPublicURLRequired()
	}
	secret, err := s.hookSecret(ctx, org.ID, pre)
	if err != nil {
		return HookRefreshResult{}, err
	}
	client, _, err := s.openGitClient(ctx, org.ID, integrationID)
	if err != nil {
		return HookRefreshResult{}, err
	}
	if allow != nil {
		if err := allow(); err != nil {
			return HookRefreshResult{}, err
		}
	}
	ref := gitRepoRef(pre.Provider, row.RepoKey, row.RepoPath)
	h, herr := client.EnsureHook(ctx, ref, scm.HookSpec{URL: s.gitWebhookURL(pre.Provider, org.Slug), Secret: secret, Reviews: true, Checks: true})
	upd := dbq.UpdateGitRepoHookParams{OrgID: org.ID, IntegrationID: integrationID, ID: id}
	if herr != nil {
		code := hookErrCode(herr)
		upd.HookState, upd.HookError, upd.HookID = hookFailed, &code, row.HookID
		if uerr := s.updateHookDetached(ctx, upd); uerr != nil {
			return HookRefreshResult{}, fmt.Errorf("update git repo hook: %w", uerr)
		}
		s.setHookAttention(ctx, org.ID, integrationID, herr)
		return HookRefreshResult{}, hostCallError(herr)
	}
	hid := h.ID
	upd.HookState, upd.HookID = hookActive, &hid
	if uerr := s.updateHookDetached(ctx, upd); uerr != nil {
		return HookRefreshResult{}, fmt.Errorf("update git repo hook: %w", uerr)
	}
	return HookRefreshResult{HookState: hookActive}, nil
}

// applyStandalone applies one event with no delivery row (the backfill, which
// has no delivery keys: a repeat is safe because link writes and moves are
// idempotent and forward-only). It emits the events last, in the same
// transaction.
func (s *Service) applyStandalone(ctx context.Context, hook GitHook, onlyTeam *uuid.UUID, repoKey string, ev scm.Event) (scmTally, error) {
	var out scmTally
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// A repository with no team matches every team: under the guard's
		// lock, refuse while restricted access exists (decisions-T20 #65).
		if onlyTeam == nil {
			if err := LockTeamAccessGuard(ctx, q, hook.OrgID); err != nil {
				return err
			}
			if err := refuseUnmappedBackfill(ctx, q, hook.OrgID); err != nil {
				return err
			}
		}
		var res scmTally
		_, evs, err := s.applySCMEvent(ctx, tx, q, hook, onlyTeam, repoKey, ev, ev.Plan(), &res)
		if err != nil {
			return err
		}
		if err := events.EmitAll(ctx, q, hook.OrgID, evs); err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

// SetBackfillTimeout shortens the time one backfill may run (tests). Call it
// before the service is shared.
func (s *Service) SetBackfillTimeout(d time.Duration) { s.backfillTimeout = d }

// backfillOutOfTime reports an error that is the backfill's own 30 s deadline
// running out (the run's context expired), not a failure of the write.
func backfillOutOfTime(ctx context.Context, err error) bool {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return false
	}
	var pe *pgconn.PgError
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &pe) && pe.Code == "57014")
}

// BackfillGitRepo reads the open changes of a mapped repo through the host's
// API (at most 200, newest first, at most 5 pages, 30 s) and runs each through
// the same writer as a live event, as action "opened": it links the issues the
// change names, moves forward only, and links a draft without moving it. A
// change from a fork is read but not linked (decisions-T17 open point 16).
func (s *Service) BackfillGitRepo(ctx context.Context, org *auth.Org, integrationID, id uuid.UUID, allow func() error) (BackfillResult, error) {
	row, err := s.getGitRepo(ctx, s.q, org.ID, integrationID, id)
	if err != nil {
		return BackfillResult{}, err
	}
	// A disabled integration takes no deliveries, so it takes no backfill
	// either: the webhook guard (decisions-T20 #65) lets its repositories have
	// no team, and a backfill acts with system authority.
	gi, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: integrationID})
	if err != nil {
		return BackfillResult{}, notFound(err, "integration")
	}
	if !gi.Enabled {
		return BackfillResult{}, conflict("this integration is disabled; enable it before a backfill")
	}
	if row.TeamID == nil {
		if err := refuseUnmappedBackfill(ctx, s.q, org.ID); err != nil {
			return BackfillResult{}, err
		}
	}
	client, integ, err := s.openGitClient(ctx, org.ID, integrationID)
	if err != nil {
		return BackfillResult{}, err
	}
	if allow != nil {
		if err := allow(); err != nil {
			return BackfillResult{}, err
		}
	}
	repoKey, ok := scm.CanonicalRepoKey(integ.Provider, row.RepoKey)
	if !ok {
		return BackfillResult{}, fmt.Errorf("git repo %s has a key that is not a repository key", id)
	}
	hook := GitHook{OrgID: org.ID, OrgSlug: org.Slug, IntegrationID: integrationID}
	ref := gitRepoRef(integ.Provider, row.RepoKey, row.RepoPath)
	limit := gitapi.RouteTimeout
	if s.backfillTimeout > 0 {
		limit = s.backfillTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	var res BackfillResult
	cursor := ""
pages:
	for page := 0; page < gitapi.MaxPages; page++ {
		p, err := client.ListOpenChanges(ctx, ref, cursor)
		if err != nil {
			if res.Seen > 0 && errors.Is(err, context.DeadlineExceeded) {
				res.Truncated = true // out of time: what was read is applied; run it again
				return res, nil
			}
			return BackfillResult{}, s.repoCallError(ctx, org.ID, integrationID, err)
		}
		for _, c := range p.Items {
			if res.Seen == maxBackfill {
				res.Truncated = true
				break pages
			}
			res.Seen++
			if c.Fork || c.Number < 1 || c.Number > math.MaxInt32 {
				continue
			}
			ev := scm.Event{
				Provider: integ.Provider, Kind: scm.KindChange, RepoKey: row.RepoKey, RepoPath: row.RepoPath,
				Number: c.Number, Action: scm.ActionOpened, URL: c.URL, Title: c.Title, SourceBranch: c.SourceBranch,
				Description: c.Description, Author: c.Author, Draft: c.Draft, HeadSHA: c.HeadSHA,
				Review: c.Review, ReviewRead: c.ReviewKnown, Checks: c.Checks, OccurredAt: c.UpdatedAt,
			}
			if _, err := validateLinkURL(ev.URL); err != nil {
				continue // a change the host gave no usable URL for is not linked
			}
			// The host's review state (LAUN-96): the list answer may not carry it.
			// A read that fails keeps the stored review (the change is still
			// linked); only running out of time stops the run.
			if rr, ok := client.(scm.ReviewReader); ok && !c.ReviewKnown {
				if rv, rerr := rr.ReadReview(ctx, ref, c.Number); rerr == nil {
					ev.Review, ev.ReviewRead = rv, true
				} else if backfillOutOfTime(ctx, rerr) {
					res.Truncated = true
					return res, nil
				}
			}
			r, err := s.applyStandalone(ctx, hook, row.TeamID, repoKey, ev)
			if err != nil {
				if backfillOutOfTime(ctx, err) {
					// The 30 s ran out during a database write. That change rolled
					// back (its own transaction); the ones before it are committed
					// and a repeat is safe (LAUN-96): answer what was done.
					res.Truncated = true
					return res, nil
				}
				return BackfillResult{}, err
			}
			res.Linked += r.linked
			res.Moved += r.moved
		}
		cursor = p.Next
		if cursor == "" {
			return res, nil
		}
	}
	if cursor != "" {
		res.Truncated = true // five pages read and the host has more
	}
	return res, nil
}

// refuseUnmappedBackfill answers 409 for a backfill of a repository with no
// team while a member of the org sees only selected teams (decisions-T20 #65).
func refuseUnmappedBackfill(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) error {
	restricted, err := q.RestrictedAccessExists(ctx, orgID)
	if err != nil {
		return fmt.Errorf("restricted access: %w", err)
	}
	if restricted {
		return conflict("some members see only selected teams, so this repository needs a team before a backfill")
	}
	return nil
}
