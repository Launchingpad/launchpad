//go:build backuptest

package offsite

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"devops/internal/backup/driver"
)

// Tests added after the first review of LAUN-80. They use the real silo where the
// bucket's own etag matters, and the fake silo of files_test.go where a silo failure
// must be made on purpose. Nothing here reaches a real remote.

// useFakeSilo points the worker at a fake silo.
func (e *filesEnv) useFakeSilo(f *fakeSilo, mut func(*Options)) {
	e.silo = SiloConfig{Endpoint: f.srv.URL, Bucket: "bkt", AccessKey: "AK", SecretKey: "SK-PLANTED"}
	e.h.w = e.h.newWorker(e.h.pool, e.h.dsn, func(o *Options) {
		o.Silo = e.silo
		if mut != nil {
			mut(o)
		}
	})
}

func stateLastSeen(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		var e struct {
			Key      string `json:"key"`
			LastSeen string `json:"lastSeen"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out[e.Key] = e.LastSeen
	}
	return out
}

// An object that keeps its SIZE and gets a new ETAG is copied again.
func TestFiles_EtagOnlyChangeIsCopiedAgain(t *testing.T) {
	e := newFilesEnv(t, nil)
	key := okey("0198aaaa-0000-7000-8000-00000000000a", "same-size.bin")
	e.put(key, randBytes(t, 4096))
	_, s1 := e.run()
	if num(s1, "uploaded") != 1 {
		t.Fatalf("run 1 = %v", s1)
	}
	_, _, l1 := e.newManifest()
	again := randBytes(t, 4096) // the same size, other bytes: silo gives another etag
	e.put(key, again)
	_, s2 := e.run()
	if s2["ok"] != true || num(s2, "uploaded") != 1 {
		t.Fatalf("an etag-only change was not copied: %v", s2)
	}
	_, _, l2 := e.newManifest()
	if l2[key].ETag == l1[key].ETag || l2[key].Size != l1[key].Size {
		t.Fatalf("the test did not change only the etag: %v -> %v", l1[key], l2[key])
	}
	e.checkObject(l2[key], again)
}

// Ten silo read failures in a row stop the phase (the silo is down); the run stays
// succeeded with files_failed, no list and no manifest are written.
func TestFiles_TenSiloReadFailuresInARowStop(t *testing.T) {
	e := newFilesEnv(t, nil)
	f := newFakeSilo(t)
	var page []string
	for i := 0; i < 25; i++ {
		page = append(page, contents(okey("0198aaaa-0000-7000-8000-00000000000a", "f"+string(rune('a'+i))), 5))
	}
	f.pages = [][]string{page}
	f.getStatus = 500
	e.useFakeSilo(f, nil)
	r, s := e.run()
	if s["ok"] != false || s["code"] != "server_error" || r.ErrorText != "files_failed" {
		t.Fatalf("step = %v row = %+v", s, r)
	}
	if num(s, "failed") != filesMaxSiloFails || f.gets != filesMaxSiloFails {
		t.Fatalf("failed %d, silo GETs %d: the phase must stop after %d in a row", num(s, "failed"), f.gets, filesMaxSiloFails)
	}
	if len(dirNames(e.listDir())) != 0 {
		t.Fatalf("a list or manifest was written: %v", dirNames(e.listDir()))
	}
}

// files_incomplete reaches error_text, and a prune_failed warning wins over it.
func TestFiles_IncompleteIsReportedAndPruneFailedWins(t *testing.T) {
	e := newFilesEnv(t, nil)
	e.h.exec(`UPDATE backup_destinations SET keep_last = 1 WHERE id = $1`, e.dest)
	f := newFakeSilo(t)
	f.pages = [][]string{{contents(okey("0198aaaa-0000-7000-8000-00000000000a", "vanished.bin"), 5)}}
	// No object body: the GET answers 404, the object "vanished" after the listing.
	e.useFakeSilo(f, nil)
	r1, s1 := e.run()
	if s1["ok"] != false || s1["code"] != "incomplete" || num(s1, "skipped") != 1 || r1.ErrorText != "files_incomplete" || r1.Status != "succeeded" {
		t.Fatalf("run 1 step = %v row = %+v", s1, r1)
	}
	// A second run, a newer dump, and a destination that cannot delete: prune_failed wins.
	e.h.addDump(5*time.Minute, nil)
	e.h.fake.del = func(string) error { return driver.ServerErr(500) }
	r2, s2 := e.run()
	if r2.ErrorText != "prune_failed" || s2["code"] != "incomplete" {
		t.Fatalf("run 2 error_text = %q step = %v", r2.ErrorText, s2)
	}
}

// The dump retention runs BEFORE the files phase, so a files phase that uses up the
// run's deadline cannot make prune fail or skip.
func TestFiles_PruneRunsBeforeTheFilesPhaseUsesTheDeadline(t *testing.T) {
	e := newFilesEnv(t, nil)
	e.h.exec(`UPDATE backup_destinations SET keep_last = 1 WHERE id = $1`, e.dest)
	f := newFakeSilo(t) // an empty bucket: run 1 makes the first dump pair
	e.useFakeSilo(f, nil)
	if _, s := e.run(); s["ok"] != true {
		t.Fatalf("run 1 = %v", s)
	}
	// Run 2: the silo hangs on the object, the run's deadline (3 s + 1 s) ends the phase.
	f.pages = [][]string{{contents(okey("0198aaaa-0000-7000-8000-00000000000a", "hang.bin"), 5)}}
	f.hang = true
	e.useFakeSilo(f, func(o *Options) { o.RunTimeout, o.FilesTimeout = 3*time.Second, time.Second })
	e.h.addDump(5*time.Minute, nil)
	r, s := e.run()
	if s["ok"] != false || s["code"] != "timeout" || r.ErrorText != "files_failed" {
		t.Fatalf("run 2 step = %v row = %+v\n%s", s, r, e.h.log.String())
	}
	if r.ErrorText == "prune_failed" {
		t.Fatal("prune ran on an expired deadline")
	}
	e.h.fake.mu.Lock()
	deleted := len(e.h.fake.deleted)
	e.h.fake.mu.Unlock()
	if deleted != 2 {
		t.Fatalf("prune deleted %d names, want the 2 of the oldest dump pair", deleted)
	}
}

// A user's filename is in the object key. It never reaches a log line or a run row,
// on the success path and on each failure path (destination, silo read, vanished).
func TestFiles_ObjectKeysNeverLeak(t *testing.T) {
	const marker = "PLANTED-FILENAME-MARKER-91ac"
	e := newFilesEnv(t, nil)
	key := okey("0198aaaa-0000-7000-8000-00000000000a", marker+".png")
	f := newFakeSilo(t)
	f.pages = [][]string{{contents(key, 5)}}
	f.objects[key] = "hello"
	e.useFakeSilo(f, nil)

	rows := []string{}
	check := func(label string) {
		t.Helper()
		if strings.Contains(e.h.log.String(), marker) {
			t.Fatalf("%s: the filename is in the log:\n%s", label, e.h.log.String())
		}
		for _, r := range rows {
			if strings.Contains(r, marker) {
				t.Fatalf("%s: the filename is in a run row: %s", label, r)
			}
		}
	}
	add := func() {
		id := e.h.queue(e.dest, "manual")
		e.h.scan()
		b, _ := json.Marshal(e.h.run(id))
		rows = append(rows, string(b))
	}
	e.failAt.Store(1) // the destination refuses the object
	add()
	check("destination failure")
	e.failAt.Store(0)
	f.getStatus = 500 // the silo cannot be read
	add()
	check("silo read failure")
	f.getStatus = 404 // the object vanished after the listing
	add()
	check("vanished object")
	f.getStatus = 0
	add() // success
	check("success")
	if !strings.Contains(rows[len(rows)-1], `"files"`) {
		t.Fatalf("the last run has no files step: %s", rows[len(rows)-1])
	}
}

// lastSeen moves for EVERY key still in silo, even when its copy failed (ticket 15
// prunes by it, so a live key must never look old).
func TestFiles_LastSeenMovesForLiveKeysWhoseCopyFailed(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	a, b := okey(org, "a.bin"), okey(org, "b.bin")
	e.put(a, randBytes(t, 1000))
	e.put(b, randBytes(t, 1001))
	if _, s := e.run(); num(s, "uploaded") != 2 {
		t.Fatalf("run 1 = %v", s)
	}
	before := stateLastSeen(t, e.h.w.statePath(e.dest))
	e.put(a, randBytes(t, 1002)) // both changed
	e.put(b, randBytes(t, 1003))
	time.Sleep(1100 * time.Millisecond)
	e.failAt.Store(e.objPuts.Load() + 1) // the first copy fails; the second is never tried
	_, s := e.run()
	if s["ok"] != false {
		t.Fatalf("run 2 = %v", s)
	}
	after := stateLastSeen(t, e.h.w.statePath(e.dest))
	for _, k := range []string{a, b} {
		if after[k] <= before[k] {
			t.Fatalf("%s: lastSeen did not move (%s -> %s)", k, before[k], after[k])
		}
	}
}

// A hostile silo repeats ONE key on many pages: it is queued and copied once, the
// repeats are dropped before they are stored (memory stays bounded) and are counted.
func TestFiles_RepeatedKeyIsQueuedOnce(t *testing.T) {
	e := newFilesEnv(t, nil)
	key := okey("0198aaaa-0000-7000-8000-00000000000a", "dup.bin")
	f := newFakeSilo(t)
	f.objects[key] = "hello"
	var page strings.Builder
	for i := 0; i < 1000; i++ {
		page.WriteString(contents(key, 5))
	}
	for p := 0; p < 40; p++ {
		f.pages = append(f.pages, []string{page.String()})
	}
	e.useFakeSilo(f, nil)
	_, s := e.run()
	if num(s, "uploaded") != 1 || num(s, "objects") != 1 || f.gets != 1 {
		t.Fatalf("step = %v, silo GETs %d: the key must be copied once", s, f.gets)
	}
	if num(s, "skipped") != 40*1000-1 || s["ok"] != false || s["code"] != "incomplete" {
		t.Fatalf("the repeats must be counted as skipped: %v", s)
	}
	if got := len(dirNames(e.objDir())); got != 1 {
		t.Fatalf("object folder = %d files", got)
	}
}
