package backup

import (
	"reflect"
	"testing"
	"time"

	"devops/internal/backup/remotename"
)

var rnow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) string { return rnow.Add(-d).Format(time.RFC3339) }

func TestFilesWindow(t *testing.T) {
	day := 24 * time.Hour
	if got := FilesWindow(rnow, rnow.Add(-10*day), true); got != 30*day {
		t.Fatalf("young dump: %v", got)
	}
	if got := FilesWindow(rnow, rnow.Add(-40*day), true); got != 41*day {
		t.Fatalf("40 day dump: %v", got)
	}
	if got := FilesWindow(rnow, time.Time{}, false); got != 30*day {
		t.Fatalf("no dump: %v", got)
	}
}

func TestPlanFilesPrune(t *testing.T) {
	day := 24 * time.Hour
	st := map[string]FileEntry{
		"in-silo-old":  {LastSeen: ago(90 * day)},
		"gone-old":     {LastSeen: ago(31 * day)},
		"gone-young":   {LastSeen: ago(29 * day)},
		"gone-bad":     {LastSeen: "yesterday"},
		"gone-empty":   {},
		"gone-future":  {LastSeen: rnow.Add(day).Format(time.RFC3339)},
		"gone-old-two": {LastSeen: ago(400 * day)},
		"live-fresh":   {LastSeen: ago(time.Hour)},
	}
	seen := map[string]bool{"in-silo-old": true, "live-fresh": true}
	newest := rnow.Add(-time.Hour)
	got, skip := PlanFilesPrune(st, seen, rnow, 30*day, newest, 0)
	// 8 entries: at most 4. Oldest first.
	if skip != "" || !reflect.DeepEqual(got, []string{"gone-old-two", "gone-old"}) {
		t.Fatalf("plan = %v %q", got, skip)
	}
	got, _ = PlanFilesPrune(st, seen, rnow, 41*day, newest, 0)
	if !reflect.DeepEqual(got, []string{"gone-old-two"}) {
		t.Fatalf("41 day window plan = %v", got)
	}
}

func TestPlanFilesPrune_CutoffBoundary(t *testing.T) {
	day := 24 * time.Hour
	st := map[string]FileEntry{
		"at":     {LastSeen: ago(30 * day)}, // exactly the cutoff: kept
		"before": {LastSeen: ago(30*day + time.Second)},
		"live":   {LastSeen: ago(0)},
		"live2":  {LastSeen: ago(0)},
	}
	got, _ := PlanFilesPrune(st, map[string]bool{"live": true, "live2": true}, rnow, 30*day, rnow, 0)
	if !reflect.DeepEqual(got, []string{"before"}) {
		t.Fatalf("plan = %v", got)
	}
}

func TestPlanFilesPrune_Guards(t *testing.T) {
	day := 24 * time.Hour
	old := FileEntry{LastSeen: ago(60 * day)}
	st := map[string]FileEntry{"a": old, "b": old, "live": {LastSeen: ago(0)}}
	live := map[string]bool{"live": true}
	if k, sk := PlanFilesPrune(st, map[string]bool{}, rnow, 30*day, rnow, 0); k != nil || sk != PruneSkipEmptyListing {
		t.Fatalf("empty listing: %v %q", k, sk)
	}
	if k, sk := PlanFilesPrune(st, live, rnow, 30*day, rnow, 1); k != nil || sk != PruneSkipBadKeys {
		t.Fatalf("bad keys: %v %q", k, sk)
	}
	if k, sk := PlanFilesPrune(st, live, rnow, 30*day, rnow.Add(-45*day), 0); k != nil || sk != PruneSkipLongGap {
		t.Fatalf("long gap: %v %q", k, sk)
	}
	if k, sk := PlanFilesPrune(st, live, rnow, 30*day, time.Time{}, 0); k != nil || sk != PruneSkipLongGap {
		t.Fatalf("no newest: %v %q", k, sk)
	}
	// An empty state has nothing to prune and no reason to skip.
	if k, sk := PlanFilesPrune(nil, nil, rnow, 30*day, time.Time{}, 0); k != nil || sk != "" {
		t.Fatalf("empty state: %v %q", k, sk)
	}
	// mass_gone: 2 of 3 entries would go (more than half): nothing goes.
	if k, sk := PlanFilesPrune(st, live, rnow, 30*day, rnow, 0); k != nil || sk != PruneSkipMassGone {
		t.Fatalf("mass gone: %v %q", k, sk)
	}
	// 1 of 3 is fine, and the oldest goes.
	st["b"] = FileEntry{LastSeen: ago(0)}
	st["a"] = FileEntry{LastSeen: ago(90 * day)}
	k, sk := PlanFilesPrune(st, live, rnow, 30*day, rnow, 0)
	if sk != "" || !reflect.DeepEqual(k, []string{"a"}) {
		t.Fatalf("cap: %v %q", k, sk)
	}
	// Two entries, none seen newly gone but one live: never everything.
	two := map[string]FileEntry{"a": old, "live": {LastSeen: ago(0)}}
	if k, _ := PlanFilesPrune(two, live, rnow, 30*day, rnow, 0); !reflect.DeepEqual(k, []string{"a"}) {
		t.Fatalf("two: %v", k)
	}
	one := map[string]FileEntry{"a": old}
	if k, _ := PlanFilesPrune(one, map[string]bool{"zz": true}, rnow, 30*day, rnow, 0); k != nil {
		t.Fatalf("a single entry state is never emptied: %v", k)
	}
}

func TestPlanListPrune(t *testing.T) {
	day := 24 * time.Hour
	const inst = "a1b2c3d4"
	mk := func(age time.Duration, h string) string {
		return "launchpad-files-" + inst + "-list-" + rnow.Add(-age).Format(remotename.TimeLayout) + "-" + h
	}
	old, young, cur := mk(60*day, "aaaa"), mk(5*day, "bbbb"), mk(31*day, "cccc")
	names := []string{
		remotename.FilesListName(old), remotename.FilesManifestName(old),
		remotename.FilesListName(young), remotename.FilesManifestName(young),
		remotename.FilesListName(cur),                              // the stem this run wrote, old by a stepped clock: kept
		"launchpad-files-ffffffff-list-20200101T000000Z-aaaa.json", // another install
		"launchpad-a1b2c3d4-20200101T000000Z-aaaa.json",            // a dump name
		"readme.txt",
	}
	got := PlanListPrune(inst, names, rnow, 30*day, cur)
	want := []string{remotename.FilesManifestName(old), remotename.FilesListName(old)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
}

func TestOldestDumpTime(t *testing.T) {
	const inst = "a1b2c3d4"
	names := []string{
		"launchpad-a1b2c3d4-20260101T000000Z-aaaa.dump.age",
		"launchpad-a1b2c3d4-20260101T000000Z-aaaa.json",
		"launchpad-a1b2c3d4-20250601T000000Z-bbbb.json", // a half pair counts: the larger window is safe
		"launchpad-ffffffff-20200101T000000Z-aaaa.json",
		"launchpad-files-a1b2c3d4-obj-00000000000000000000000000000000.age",
	}
	got, ok := OldestDumpTime(inst, names)
	if !ok || !got.Equal(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("oldest = %v %v", got, ok)
	}
	if _, ok := OldestDumpTime(inst, nil); ok {
		t.Fatal("no names, but a time")
	}
}

func TestPlanFilesPrune_MassGoneSubsetNeverErodes(t *testing.T) {
	day := 24 * time.Hour
	st := map[string]FileEntry{}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		k := "k" + string(rune('A'+i%26)) + string(rune('a'+i/26%26)) + string(rune('0'+i/676))
		st[k] = FileEntry{LastSeen: ago(60 * day)}
		if i < 10 {
			seen[k] = true
			st[k] = FileEntry{LastSeen: ago(0)}
		}
	}
	// Repeated passes (a "run now" many times a day) never prune: the plan is the same every time.
	for i := 0; i < 10; i++ {
		if k, sk := PlanFilesPrune(st, seen, rnow, 30*day, rnow, 0); k != nil || sk != PruneSkipMassGone {
			t.Fatalf("pass %d: %d keys, %q", i, len(k), sk)
		}
	}
}

func TestNewestLastSeen_IgnoresTheFuture(t *testing.T) {
	day := 24 * time.Hour
	st := map[string]FileEntry{
		"old":    {LastSeen: ago(45 * day)},
		"future": {LastSeen: rnow.Add(1000 * day).Format(time.RFC3339)},
		"junk":   {LastSeen: "x"},
	}
	if got := NewestLastSeen(st, rnow); !got.Equal(rnow.Add(-45 * day)) {
		t.Fatalf("newest = %v", got)
	}
	// A future lastSeen cannot defeat the long_gap guard.
	live := map[string]bool{"live": true}
	st["live"] = FileEntry{LastSeen: ago(45 * day)}
	if _, sk := PlanFilesPrune(st, live, rnow, 30*day, NewestLastSeen(st, rnow), 0); sk != PruneSkipLongGap {
		t.Fatalf("skip = %q", sk)
	}
}

// The boundary of mass_gone: a state of two or more. Two entries, both past the window: nothing goes.
// One entry: nothing goes either (the cap rounds to zero).
func TestPlanFilesPrune_MassGoneBoundary(t *testing.T) {
	day := 24 * time.Hour
	old := FileEntry{LastSeen: ago(60 * day)}
	two := map[string]FileEntry{"a": old, "b": old}
	if k, sk := PlanFilesPrune(two, map[string]bool{"zz": true}, rnow, 30*day, rnow, 0); k != nil || sk != PruneSkipMassGone {
		t.Fatalf("two old entries: %v %q", k, sk)
	}
	one := map[string]FileEntry{"a": old}
	if k, sk := PlanFilesPrune(one, map[string]bool{"zz": true}, rnow, 30*day, rnow, 0); k != nil || sk != "" {
		t.Fatalf("one entry: %v %q", k, sk)
	}
}
