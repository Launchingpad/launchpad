package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Manual issue order (LAUN-51, docs/internal/decisions-T14-manual-order.md).
// A column is (org, team, state). Ranks ascend: a smaller rank is higher up.

const (
	// rankStep is the distance between neighbours after a backfill or a
	// rebalance, and the distance an end placement leaves.
	rankStep = 1024.0
	// minRankGap is the smallest gap a midpoint insert may split. A gap of
	// 1024 can be split 21 times, so a column is renumbered after about 21
	// drops into the same gap, never earlier.
	minRankGap = 1.0 / 1024
	// relRankGap guards large ranks, where a float64 step is coarse.
	relRankGap = 1e-9
	// maxRank bounds a client-chosen sortOrder (far below 2^53).
	maxRank = 1e12
)

// rankBetween returns the rank for an issue placed between lo and hi (lo <
// hi). ok is false when the gap is too small to halve: the caller rebalances
// the column and asks again.
func rankBetween(lo, hi float64) (float64, bool) {
	gap := hi - lo
	if !(gap >= minRankGap) || gap < math.Max(math.Abs(lo), math.Abs(hi))*relRankGap {
		return 0, false
	}
	mid := lo + gap/2
	if mid <= lo || mid >= hi {
		return 0, false
	}
	return mid, true
}

// rankLock serialises the placements and creations of one team. It is taken
// before any row lock of the transaction (a rebalance writes many rows, so a
// transaction that held one of them first could deadlock with it).
func rankLock(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:issue-rank:' || $1::text, 0))`, teamID); err != nil {
		return fmt.Errorf("rank lock: %w", err)
	}
	return nil
}

// topRank is the rank that puts an issue above its column.
func topRank(ctx context.Context, q *dbq.Queries, orgID, teamID, stateID uuid.UUID) (float64, error) {
	r, err := q.ColumnTopRank(ctx, dbq.ColumnTopRankParams{OrgID: orgID, TeamID: teamID, StateID: stateID})
	if err != nil {
		return 0, fmt.Errorf("column top rank: %w", err)
	}
	return r, nil
}

// placement is the parsed placement part of a PATCH.
type placement struct {
	Rank   *float64
	After  string
	Before string
}

func (p placement) set() bool { return p.Rank != nil || p.After != "" || p.Before != "" }

func (in UpdateIssueInput) placement() (placement, error) {
	p := placement{Rank: in.SortOrder, After: in.PlaceAfter, Before: in.PlaceBefore}
	n := 0
	for _, set := range []bool{p.Rank != nil, p.After != "", p.Before != ""} {
		if set {
			n++
		}
	}
	if n > 1 {
		return p, httpx.Invalid("use one of sortOrder, placeAfter and placeBefore")
	}
	if p.Rank != nil && (math.IsNaN(*p.Rank) || math.IsInf(*p.Rank, 0) || math.Abs(*p.Rank) > maxRank) {
		return p, httpx.Invalid("sortOrder must be a number between -%g and %g", maxRank, maxRank)
	}
	return p, nil
}

// stalePlacement turns a 404 from the neighbour lookup into 409
// stale_placement and leaves any other error alone.
func stalePlacement(err error, format string, args ...any) error {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return httpx.Errorf(http.StatusConflict, httpx.CodeStalePlacement, format, args...)
	}
	return err
}

// placeRank computes the rank of issue cur in the column (cur.TeamID, state)
// for a placement. rebalanced reports that it renumbered the column first.
// The caller holds the team's rank lock.
func placeRank(ctx context.Context, q *dbq.Queries, org *auth.Org, cur dbq.Issue, state uuid.UUID, p placement) (rank float64, rebalanced bool, err error) {
	if p.Rank != nil {
		return *p.Rank, false, nil
	}
	ref, after := p.After, true
	field := "placeAfter"
	if p.Before != "" {
		ref, after, field = p.Before, false, "placeBefore"
	}
	if _, _, ok := ParseIdentifier(ref); !ok {
		if _, err := uuid.Parse(ref); err != nil {
			return 0, false, httpx.Invalid("%s must be an issue id or identifier", field)
		}
	}
	nid, err := resolveIssueID(ctx, q, org, ref)
	if err != nil {
		// Unknown, vanished and other-org neighbours are the same answer
		// (another org's row must not be told apart from a missing one).
		return 0, false, stalePlacement(err, "%s: unknown issue %q", field, ref)
	}
	if nid == cur.ID {
		return 0, false, httpx.Invalid("%s: an issue cannot be placed next to itself", field)
	}
	nb, err := q.GetIssueRankRow(ctx, dbq.GetIssueRankRowParams{OrgID: org.ID, ID: nid})
	if err != nil {
		return 0, false, stalePlacement(notFound(err, "issue"), "%s: unknown issue %q", field, ref)
	}
	if nb.TeamID != cur.TeamID {
		return 0, false, httpx.Invalid("%s: issue %q is in another team", field, ref)
	}
	if nb.StateID != state {
		return 0, false, httpx.Errorf(http.StatusConflict, httpx.CodeStalePlacement,
			"%s: issue %q is not in the destination column any more; refetch it", field, ref)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var other *float64 // the rank on the far side of the neighbour; nil: the end of the column
		if after {
			r, err := q.ColumnSuccessor(ctx, dbq.ColumnSuccessorParams{
				OrgID: org.ID, TeamID: cur.TeamID, StateID: state, SkipID: cur.ID, AfterRank: nb.SortOrder, AfterID: nb.ID})
			if err == nil {
				other = &r.SortOrder
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return 0, false, fmt.Errorf("column successor: %w", err)
			}
			// Already there (a retried drag): leave the rank alone.
			if !rebalanced && cur.StateID == state && rankLess(nb.SortOrder, nb.ID, cur.SortOrder, cur.ID) &&
				(other == nil || rankLess(cur.SortOrder, cur.ID, r.SortOrder, r.ID)) {
				return cur.SortOrder, false, nil
			}
			if other == nil {
				return nb.SortOrder + rankStep, rebalanced, nil
			}
			if mid, ok := rankBetween(nb.SortOrder, *other); ok {
				return mid, rebalanced, nil
			}
		} else {
			r, err := q.ColumnPredecessor(ctx, dbq.ColumnPredecessorParams{
				OrgID: org.ID, TeamID: cur.TeamID, StateID: state, SkipID: cur.ID, BeforeRank: nb.SortOrder, BeforeID: nb.ID})
			if err == nil {
				other = &r.SortOrder
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return 0, false, fmt.Errorf("column predecessor: %w", err)
			}
			if !rebalanced && cur.StateID == state && rankLess(cur.SortOrder, cur.ID, nb.SortOrder, nb.ID) &&
				(other == nil || rankLess(r.SortOrder, r.ID, cur.SortOrder, cur.ID)) {
				return cur.SortOrder, false, nil
			}
			if other == nil {
				return nb.SortOrder - rankStep, rebalanced, nil
			}
			if mid, ok := rankBetween(*other, nb.SortOrder); ok {
				return mid, rebalanced, nil
			}
		}
		// The gap is too small (or two ranks are equal): renumber the column
		// and look again. Ranks are 1024 apart after this.
		if _, err := q.RebalanceColumn(ctx, dbq.RebalanceColumnParams{OrgID: org.ID, TeamID: cur.TeamID, StateID: state, SkipID: cur.ID}); err != nil {
			return 0, false, fmt.Errorf("rebalance column: %w", err)
		}
		rebalanced = true
		if nb, err = q.GetIssueRankRow(ctx, dbq.GetIssueRankRowParams{OrgID: org.ID, ID: nid}); err != nil {
			return 0, false, fmt.Errorf("reload neighbour: %w", err)
		}
	}
	return 0, false, fmt.Errorf("place issue: no rank after a rebalance")
}

// rankLess is the column order: rank, then id (bytewise, as Postgres
// compares uuids).
func rankLess(r1 float64, id1 uuid.UUID, r2 float64, id2 uuid.UUID) bool {
	if r1 != r2 {
		return r1 < r2
	}
	return bytes.Compare(id1[:], id2[:]) < 0
}
