package backup

import (
	"context"
	"errors"
	"sort"
	"time"

	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

// Retention (T16 #33): keep the last N complete backups per destination.
//
// A backup is a PAIR (object and manifest with one stem). Only names that
// match the install's exact expression are looked at; every other name (other
// files, other installs, test files, a name with a bad date) is invisible and
// is never deleted.

// OrphanAge is how old (by the time IN THE NAME) an unpaired file must be
// before it is deleted.
const OrphanAge = 24 * time.Hour

// Retention bounds of `keep_last`.
const (
	MinKeepLast = 1
	MaxKeepLast = 365
)

// Errors of retention.
var (
	ErrKeepLast       = errors.New("backup: keep_last must be 1 to 365")
	ErrNotVerifiedNew = errors.New("backup: the new upload is not a complete pair in the listing")
)

// PrunePlan is what a prune would delete. Names are file names, in the order
// they are deleted: for a pair the manifest first, then the object.
type PrunePlan struct {
	Keep    []string // stems of the pairs kept, newest first
	Pairs   []string // stems of the pairs to delete (older than the newest N)
	Orphans []string // file names without a partner, older than OrphanAge
	Delete  []string // every file name to delete, in order
}

// PlanPrune decides what to delete from a listing. It is pure.
//
//   - names are the file names the remote listed (anything else is ignored);
//   - running holds the stems a `running` run names (their files are never
//     orphans);
//   - verifiedStem is the pair just uploaded and verified. Retention runs only
//     after that, so if it is not a COMPLETE pair in the listing the plan is an
//     error and nothing is deleted (a failed run deletes nothing).
//
// Pairs sort by the time in the name, then the stem (descending): the remote's
// modified time is never used. Two copies of one dump are two pairs and both
// count. The pair just uploaded and verified is NEVER deleted: if the newest N
// (by the time in the name) do not include it, which happens when the clock
// stepped back or pairs with future dates were planted, it is kept IN ADDITION
// to the newest N (decisions note, ticket 3, item 12). Retention must not
// delete the copy the run has just proved good.
func PlanPrune(installID string, keepLast int, names []string, now time.Time, running map[string]bool, verifiedStem string) (PrunePlan, error) {
	if keepLast < MinKeepLast || keepLast > MaxKeepLast {
		return PrunePlan{}, ErrKeepLast
	}
	m, err := remotename.NewMatcher(installID)
	if err != nil {
		return PrunePlan{}, err
	}
	type pair struct {
		stem            string
		t               time.Time
		hasObj, hasMeta bool
	}
	byStem := map[string]*pair{}
	for _, n := range names {
		p, ok := m.Parse(n)
		if !ok {
			continue
		}
		e := byStem[p.Stem]
		if e == nil {
			e = &pair{stem: p.Stem, t: p.Time}
			byStem[p.Stem] = e
		}
		if p.Kind == remotename.KindObject {
			e.hasObj = true
		} else {
			e.hasMeta = true
		}
	}
	if v, ok := byStem[verifiedStem]; !ok || !v.hasObj || !v.hasMeta {
		return PrunePlan{}, ErrNotVerifiedNew
	}
	var complete, partial []*pair
	for _, e := range byStem {
		if e.hasObj && e.hasMeta {
			complete = append(complete, e)
		} else {
			partial = append(partial, e)
		}
	}
	sort.Slice(complete, func(i, j int) bool {
		if !complete[i].t.Equal(complete[j].t) {
			return complete[i].t.After(complete[j].t)
		}
		return complete[i].stem > complete[j].stem
	})
	var plan PrunePlan
	for i, e := range complete {
		if i < keepLast || e.stem == verifiedStem {
			plan.Keep = append(plan.Keep, e.stem)
			continue
		}
		plan.Pairs = append(plan.Pairs, e.stem)
		plan.Delete = append(plan.Delete, remotename.ManifestName(e.stem), remotename.ObjectName(e.stem))
	}
	sort.Slice(partial, func(i, j int) bool { return partial[i].stem < partial[j].stem })
	for _, e := range partial {
		if running[e.stem] || now.Sub(e.t) <= OrphanAge {
			continue
		}
		name := remotename.ObjectName(e.stem)
		if e.hasMeta {
			name = remotename.ManifestName(e.stem)
		}
		plan.Orphans = append(plan.Orphans, name)
		plan.Delete = append(plan.Delete, name)
	}
	return plan, nil
}

// PruneResult counts the deletes of Prune.
type PruneResult struct {
	Deleted int
	Failed  int // a failed delete is a warning: the run stays succeeded (prune_failed)
}

// Prune lists the folder, plans and deletes. Every Delete name was produced by
// the install's matcher, never taken from a reply's href or path. A failed
// delete is counted and the rest go on. A list error or an unverified new
// upload returns an error and deletes nothing.
func Prune(ctx context.Context, r driver.Remote, installID string, keepLast int, now time.Time, running map[string]bool, verifiedStem string) (PruneResult, error) {
	// Validate the install id BEFORE it is put in a List prefix. Drivers must
	// still treat the prefix as untrusted input (a hostile id never gets here,
	// but a driver is the last defence for its own protocol).
	if !remotename.ValidInstallID(installID) {
		return PruneResult{}, errors.New("backup: bad install id")
	}
	entries, err := r.List(ctx, "launchpad-"+installID+"-")
	if err != nil {
		return PruneResult{}, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	plan, err := PlanPrune(installID, keepLast, names, now, running, verifiedStem)
	if err != nil {
		return PruneResult{}, err
	}
	var res PruneResult
	for _, n := range plan.Delete {
		if ctx.Err() != nil {
			res.Failed++
			continue
		}
		if err := r.Delete(ctx, n); err != nil {
			res.Failed++
			continue
		}
		res.Deleted++
	}
	return res, nil
}
