package tracker

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)

func TestWorstCheck(t *testing.T) {
	m := map[string]checkEntry{}
	if worstCheck(m) != "none" {
		t.Fatal("empty map is none")
	}
	mergeCheck(m, "a", "success", t0)
	if worstCheck(m) != "success" {
		t.Fatal("one success")
	}
	mergeCheck(m, "b", "pending", t0)
	if worstCheck(m) != "pending" {
		t.Fatal("pending over success")
	}
	mergeCheck(m, "c", "failure", t0)
	mergeCheck(m, "b", "success", t0.Add(time.Minute))
	if worstCheck(m) != "failure" {
		t.Fatal("one failing app plus succeeding apps is failure")
	}
	mergeCheck(m, "c", "success", t0.Add(time.Minute))
	if worstCheck(m) != "success" {
		t.Fatal("the failing app fixed its check")
	}
}

func TestMergeCheck_OutOfOrderKeepsTheLaterPerName(t *testing.T) {
	m := map[string]checkEntry{}
	if !mergeCheck(m, "ci", "success", t0.Add(time.Minute)) {
		t.Fatal("first write")
	}
	if mergeCheck(m, "ci", "pending", t0) { // older: dropped
		t.Fatal("an older result replaced a later one")
	}
	if m["ci"].State != "success" {
		t.Fatalf("entry = %+v", m["ci"])
	}
	// The time of another name does not matter.
	if !mergeCheck(m, "lint", "pending", t0) {
		t.Fatal("another name with an older time is a new entry")
	}
	// An equal time replaces; the same state and time is no change.
	if !mergeCheck(m, "ci", "failure", t0.Add(time.Minute)) || m["ci"].State != "failure" {
		t.Fatal("an equal time replaces")
	}
	if mergeCheck(m, "ci", "failure", t0.Add(time.Minute)) {
		t.Fatal("the same result is no change")
	}
}

func TestMergeCheck_CapEvictsTheOldest(t *testing.T) {
	m := map[string]checkEntry{}
	for i := 0; i < maxCheckNames; i++ {
		mergeCheck(m, fmt.Sprintf("c%02d", i), "success", t0.Add(time.Duration(i)*time.Second))
	}
	if len(m) != maxCheckNames {
		t.Fatalf("%d names", len(m))
	}
	mergeCheck(m, "new", "failure", t0.Add(time.Hour))
	if len(m) != maxCheckNames {
		t.Fatalf("%d names after the 51st", len(m))
	}
	if _, ok := m["c00"]; ok {
		t.Fatal("the oldest entry stayed")
	}
	if worstCheck(m) != "failure" {
		t.Fatal("the new failure must show")
	}
	// An update of a name that exists never evicts.
	mergeCheck(m, "c10", "pending", t0.Add(2*time.Hour))
	if len(m) != maxCheckNames || m["c10"].State != "pending" {
		t.Fatal("update of an existing name")
	}
}

func TestClampCheckTime(t *testing.T) {
	now := t0
	for _, c := range []struct {
		name    string
		at, out time.Time
	}{
		{"no host time takes the server time", time.Time{}, now},
		{"past is kept", now.Add(-time.Hour), now.Add(-time.Hour)},
		{"small skew is kept", now.Add(maxCheckSkew), now.Add(maxCheckSkew)},
		{"far future takes the server time", now.Add(maxCheckSkew + time.Second), now},
		{"year 2099 takes the server time", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), now},
	} {
		if got := clampCheckTime(c.at, now); !got.Equal(c.out) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.out)
		}
	}
}
