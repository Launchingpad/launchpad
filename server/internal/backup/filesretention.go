package backup

import (
	"sort"
	"time"

	"devops/internal/backup/remotename"
)

// Retention of the attachment (files) copy (T16 #38, ticket 15, LAUN-81). All of it is
// pure: the worker lists, asks these functions and deletes the names they return.
//
// The rule: a remote attachment object is deleted only when
//  1. its key was NOT in the silo listing of this run, and
//  2. its `lastSeen` in the local state is older than the window.
//
// The window is max(FilesKeepMin, age of the oldest dump kept on the destination +
// FilesKeepMargin). A kept dump can name any attachment that was in silo when it was made;
// such an object has lastSeen no older than that dump's run, so it survives as long as the
// dump does. A time that cannot be read keeps the object.

const (
	FilesKeepMin    = 30 * 24 * time.Hour
	FilesKeepMargin = 24 * time.Hour
)

// OldestDumpTime returns the oldest time in the names of the install's dump files
// (complete pairs and half pairs: the larger window is the safe one).
func OldestDumpTime(installID string, names []string) (time.Time, bool) {
	m, err := remotename.NewMatcher(installID)
	if err != nil {
		return time.Time{}, false
	}
	var oldest time.Time
	found := false
	for _, n := range names {
		p, ok := m.Parse(n)
		if !ok {
			continue
		}
		if !found || p.Time.Before(oldest) {
			oldest, found = p.Time, true
		}
	}
	return oldest, found
}

// FilesWindow is the retention window. haveOldest false (no dump known) gives the minimum.
func FilesWindow(now, oldestDump time.Time, haveOldest bool) time.Duration {
	w := FilesKeepMin
	if haveOldest {
		if d := now.Sub(oldestDump) + FilesKeepMargin; d > w {
			w = d
		}
	}
	return w
}

// FilesPruneMaxFraction: one run deletes at most this share of the state entries (rounded
// down), so no run can empty the remote copy whatever the listing said.
const FilesPruneMaxFraction = 0.5

// Reasons a prune pass is skipped (fixed words, shown in the files step).
const (
	PruneSkipEmptyListing = "empty_listing" // silo listed nothing but the state has objects
	PruneSkipBadKeys      = "bad_keys"      // the listing held keys the phase could not read
	PruneSkipLongGap      = "long_gap"      // no key was seen for longer than the window
	PruneSkipMassGone     = "mass_gone"     // more than half of the state would be pruned at once
)

// PlanFilesPrune returns the state keys whose remote object may be deleted (oldest lastSeen
// first, at most FilesPruneMaxFraction of the state), or a skip reason and no keys.
//
// Guards (ticket 15 review): a pass is skipped when
//   - the listing was empty but the state is not (a wrong bucket, a new silo volume, or a silo
//     that answered 200 with nothing: it must never mean "everything is gone");
//   - the listing had keys the phase could not read (bad > 0): the listing is not trustworthy;
//   - the newest lastSeen in the state BEFORE this run is older than the window (the files phase
//     did not succeed for longer than the window: the bump of this run comes first, prune next run).
//
// stateNewest is the newest lastSeen of the state as it was loaded (zero for an empty state).
// An entry whose lastSeen is exactly at the cutoff, unreadable, empty or newer is kept. If more than half
// of the state would be pruned at once the pass is skipped (mass_gone).
func PlanFilesPrune(state map[string]FileEntry, seen map[string]bool, now time.Time, window time.Duration, stateNewest time.Time, bad int) (keys []string, skip string) {
	if len(state) == 0 {
		return nil, ""
	}
	if len(seen) == 0 {
		return nil, PruneSkipEmptyListing
	}
	if bad > 0 {
		return nil, PruneSkipBadKeys
	}
	cutoff := now.Add(-window)
	if stateNewest.IsZero() || stateNewest.Before(cutoff) {
		return nil, PruneSkipLongGap
	}
	type cand struct {
		k string
		t time.Time
	}
	var cs []cand
	for k, e := range state {
		if seen[k] {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.LastSeen)
		if err != nil || !t.Before(cutoff) {
			continue
		}
		cs = append(cs, cand{k, t})
	}
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].t.Equal(cs[j].t) {
			return cs[i].t.Before(cs[j].t)
		}
		return cs[i].k < cs[j].k
	})
	// mass_gone: a silo that keeps returning a SUBSET (a partly lost volume, a buggy listing) must not
	// erode the remote a half at a time. The app cannot delete silo objects today, so a legitimate
	// mass disappearance does not occur; an operator who really removed them clears the state file.
	if len(state) >= 2 && float64(len(cs)) > float64(len(state))*FilesPruneMaxFraction {
		return nil, PruneSkipMassGone
	}
	if max := int(float64(len(state)) * FilesPruneMaxFraction); len(cs) > max {
		cs = cs[:max]
	}
	for _, c := range cs {
		keys = append(keys, c.k)
	}
	return keys, ""
}

// NewestLastSeen is the newest readable lastSeen in the state that is not in the future
// (zero when none). A future value is treated as unreadable: it must not freshen the state.
func NewestLastSeen(state map[string]FileEntry, now time.Time) time.Time {
	var n time.Time
	for _, e := range state {
		if t, err := time.Parse(time.RFC3339, e.LastSeen); err == nil && !t.After(now) && t.After(n) {
			n = t
		}
	}
	return n
}

// PlanListPrune returns the names of files-manifest and list files to delete: a stem whose
// time (in its name) is older than now-window, except keepStem. Manifest first, then list.
// Names that are not this install's list or manifest names are ignored.
func PlanListPrune(installID string, names []string, now time.Time, window time.Duration, keepStem string) []string {
	m, err := remotename.NewFilesMatcher(installID)
	if err != nil {
		return nil
	}
	cutoff := now.Add(-window)
	byStem := map[string]time.Time{}
	var out []string
	for _, n := range names {
		p, ok := m.Parse(n)
		if !ok || p.Stem == keepStem || !p.Time.Before(cutoff) {
			continue
		}
		byStem[p.Stem] = p.Time
	}
	stems := make([]string, 0, len(byStem))
	for s := range byStem {
		stems = append(stems, s)
	}
	sort.Strings(stems)
	for _, s := range stems {
		out = append(out, remotename.FilesManifestName(s), remotename.FilesListName(s))
	}
	return out
}
