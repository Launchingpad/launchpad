//go:build backuptest

package offsite

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

// Retention of the attachment copy (ticket 15, LAUN-81): real silo, fake destination.

// backdate rewrites the lastSeen of the given keys in the state file.
func backdate(t *testing.T, path string, ages map[string]time.Duration) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			out = append(out, line)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if age, ok := ages[m["key"].(string)]; ok {
			m["lastSeen"] = time.Now().Add(-age).UTC().Format(time.RFC3339)
		}
		nb, _ := json.Marshal(m)
		out = append(out, string(nb))
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *filesEnv) objectName(key string) string {
	n, _ := remotename.FilesObjectName(e.h.settings().InstallID, key)
	return n
}

func (e *filesEnv) hasObject(key string) bool {
	_, err := os.Stat(filepath.Join(e.objDir(), e.objectName(key)))
	return err == nil
}

const day = 24 * time.Hour

func TestFilesRetention_PrunesByLastSeen(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	live, goneOld, goneYoung := okey(org, "live.bin"), okey(org, "gone-old.bin"), okey(org, "gone-young.bin")
	for _, k := range []string{live, goneOld, goneYoung} {
		e.put(k, randBytes(t, 5000))
	}
	if _, s := e.run(); num(s, "uploaded") != 3 {
		t.Fatalf("run 1 = %v", s)
	}
	e.newManifest()
	e.del(goneOld)
	e.del(goneYoung)
	// The live key is old in the state too: it must survive, because it is in the silo listing.
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{live: 90 * day, goneOld: 45 * day, goneYoung: 5 * day})
	_, s := e.run()
	if s["ok"] != true || num(s, "pruned") != 1 || num(s, "gone") != 1 || num(s, "objects") != 2 {
		t.Fatalf("run 2 = %v", s)
	}
	if e.hasObject(goneOld) || !e.hasObject(goneYoung) || !e.hasObject(live) {
		t.Fatalf("objects after prune: %v", dirNames(e.objDir()))
	}
	ls := stateLastSeen(t, e.h.w.statePath(e.dest))
	if _, ok := ls[goneOld]; ok || len(ls) != 2 {
		t.Fatalf("state after prune: %v", ls)
	}
	_, _, list := e.newManifest()
	if _, ok := list[goneOld]; ok || len(list) != 2 {
		t.Fatalf("list after prune holds %d entries", len(list))
	}
}

// A dump 40 days old is kept (keep_last 14): the window grows to 41 days, so an object that
// left silo 35 days ago stays, one that left 50 days ago goes. Old lists follow the same window.
func TestFilesRetention_KeptDumpWidensTheWindow(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	k35, k50 := okey(org, "gone-35.bin"), okey(org, "gone-50.bin")
	e.put(okey(org, "live.bin"), randBytes(t, 100)) // a listing that is not empty
	e.put(k35, randBytes(t, 3000))
	e.put(k50, randBytes(t, 3000))
	e.run()
	e.newManifest()
	e.del(k35)
	e.del(k50)
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{k35: 35 * day, k50: 50 * day})
	inst := e.h.settings().InstallID
	old := time.Now().Add(-40 * day).UTC()
	stem, _ := remotename.NewStem(inst, old, nil)
	for _, n := range []string{remotename.ObjectName(stem), remotename.ManifestName(stem)} {
		if err := os.WriteFile(filepath.Join(e.h.remoteDir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two old lists in the list folder: 35 days (inside 41) and 60 days (outside).
	mkList := func(age time.Duration, h string) string {
		s := "launchpad-files-" + inst + "-list-" + time.Now().Add(-age).UTC().Format(remotename.TimeLayout) + "-" + h
		for _, n := range []string{remotename.FilesListName(s), remotename.FilesManifestName(s)} {
			if err := os.WriteFile(filepath.Join(e.listDir(), n), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	l35, l60 := mkList(35*day, "aaaa"), mkList(60*day, "bbbb")
	_, s := e.run()
	if num(s, "pruned") != 1 || num(s, "listsPruned") != 1 {
		t.Fatalf("step = %v", s)
	}
	if !e.hasObject(k35) || e.hasObject(k50) {
		t.Fatalf("objects: %v", dirNames(e.objDir()))
	}
	if _, err := os.Stat(filepath.Join(e.h.remoteDir, remotename.ObjectName(stem))); err != nil {
		t.Fatal("the 40 day dump was deleted")
	}
	names := strings.Join(dirNames(e.listDir()), ",")
	if !strings.Contains(names, l35) || strings.Contains(names, l60) {
		t.Fatalf("lists: %s", names)
	}
}

// A destination error on the delete keeps the object and its state entry; the next run retries.
func TestFilesRetention_DestinationErrorKeepsTheEntry(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	k := okey(org, "gone.bin")
	e.put(okey(org, "live.bin"), randBytes(t, 100))
	e.put(k, randBytes(t, 3000))
	e.run()
	e.newManifest()
	e.del(k)
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{k: 45 * day})
	fail := true
	e.h.fake.del = func(name string) error {
		if fail && strings.Contains(name, "-obj-") {
			return driver.Errf(driver.ConnectRefused)
		}
		return nil
	}
	r, s := e.run()
	// A retention failure alone keeps ok=true: the copy is complete, and files_not_backed_up counts ok steps.
	if num(s, "pruned") != 0 || num(s, "pruneFailed") != 1 || s["code"] != "prune_failed" || s["ok"] != true || r.ErrorText != "prune_failed" {
		t.Fatalf("run = %v / %+v", s, r)
	}
	if !e.hasObject(k) {
		t.Fatal("the object is gone though the delete failed")
	}
	if _, ok := stateLastSeen(t, e.h.w.statePath(e.dest))[k]; !ok {
		t.Fatal("the state entry was dropped though the delete failed")
	}
	e.newManifest()
	fail = false
	_, s = e.run()
	if num(s, "pruned") != 1 || e.hasObject(k) {
		t.Fatalf("retry = %v", s)
	}
	if _, ok := stateLastSeen(t, e.h.w.statePath(e.dest))[k]; ok {
		t.Fatal("the entry stayed after a good delete")
	}
	// An object already absent on the destination is a success (a crash after the delete).
}

// injectGone adds n gone entries (valid shape, never in silo) to the state file, last seen `age` ago.
func injectGone(t *testing.T, e *filesEnv, org string, n int, age time.Duration) {
	t.Helper()
	path := e.h.w.statePath(e.dest)
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var extra []string
	for i := 0; i < n; i++ {
		var m map[string]any
		_ = json.Unmarshal([]byte(lines[1]), &m)
		k := okey(org, "gone"+strings.Repeat("x", i%7)+string(rune('A'+i%26))+string(rune('a'+i/26%26))+string(rune('0'+i/676)))
		m["key"] = k
		nn, _ := remotename.FilesObjectName(e.h.settings().InstallID, k)
		m["remote"] = nn
		m["lastSeen"] = time.Now().Add(-age).UTC().Format(time.RFC3339)
		nb, _ := json.Marshal(m)
		extra = append(extra, string(nb))
	}
	if err := os.WriteFile(path, []byte(strings.Join(append(lines, extra...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Half of the state or less may go in one run, oldest first; the state shrinks run after run.
func TestFilesRetention_HalfOfTheStateMayGo(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	e.put(okey(org, "a.bin"), randBytes(t, 100))
	e.run()
	e.newManifest()
	injectGone(t, e, org, 1, 60*day) // 1 of 2: exactly half
	_, s := e.run()
	if num(s, "pruned") != 1 || num(s, "objects") != 1 {
		t.Fatalf("run = %v", s)
	}
}

// A silo that keeps listing a SUBSET never erodes the remote, however often runs repeat.
func TestFilesRetention_MassGoneNeverErodes(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	for i := 0; i < 10; i++ {
		e.put(okey(org, "live"+string(rune('a'+i))+".bin"), randBytes(t, 50))
	}
	e.run()
	e.newManifest()
	injectGone(t, e, org, 990, 60*day)
	for i := 0; i < 10; i++ {
		_, s := e.run()
		if num(s, "pruned") != 0 || s["pruneSkip"] != "mass_gone" || s["pruneSkipped"] != true || num(s, "objects") != 1000 {
			t.Fatalf("run %d = %v", i, s)
		}
		e.newManifest()
	}
	// The operator clears it by removing the state file: a full copy, nothing pruned, nothing lost.
	if err := os.Remove(e.h.w.statePath(e.dest)); err != nil {
		t.Fatal(err)
	}
	_, s := e.run()
	if s["initial"] != true || num(s, "objects") != 10 {
		t.Fatalf("after clearing = %v", s)
	}
}

// A far-future lastSeen cannot freshen the state and defeat the long_gap guard.
func TestFilesRetention_FutureLastSeenCannotDefeatLongGap(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	live, gone := okey(org, "live.bin"), okey(org, "gone.bin")
	e.put(live, randBytes(t, 100))
	e.put(gone, randBytes(t, 100))
	e.put(okey(org, "other.bin"), randBytes(t, 100))
	e.run()
	e.newManifest()
	e.del(gone)
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{live: 45 * day, gone: 45 * day, okey(org, "other.bin"): -1000 * day})
	_, s := e.run()
	if num(s, "pruned") != 0 || s["pruneSkip"] != "long_gap" || !e.hasObject(gone) {
		t.Fatalf("step = %v", s)
	}
}

// An empty silo listing with a non-empty state never prunes (a wrong bucket, a new volume).
func TestFilesRetention_EmptyListingNeverWipes(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	k1, k2 := okey(org, "one.bin"), okey(org, "two.bin")
	e.put(k1, randBytes(t, 1000))
	e.put(k2, randBytes(t, 1000))
	e.run()
	e.newManifest()
	e.del(k1)
	e.del(k2)
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{k1: 45 * day, k2: 45 * day})
	_, s := e.run()
	if num(s, "pruned") != 0 || s["pruneSkipped"] != true || s["pruneSkip"] != "empty_listing" || s["ok"] != true {
		t.Fatalf("step = %v", s)
	}
	if !e.hasObject(k1) || !e.hasObject(k2) || len(stateLastSeen(t, e.h.w.statePath(e.dest))) != 2 {
		t.Fatal("an empty listing deleted objects or state entries")
	}
}

// No key seen for longer than the window: this run bumps what is still in silo, the next one prunes.
func TestFilesRetention_LongGapBumpsFirstAndPrunesNextRun(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	live, gone := okey(org, "live.bin"), okey(org, "gone.bin")
	e.put(live, randBytes(t, 1000))
	e.put(gone, randBytes(t, 1000))
	e.run()
	e.newManifest()
	e.del(gone)
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{live: 45 * day, gone: 45 * day})
	_, s := e.run()
	if num(s, "pruned") != 0 || s["pruneSkip"] != "long_gap" || !e.hasObject(gone) {
		t.Fatalf("run 1 = %v", s)
	}
	e.newManifest()
	_, s = e.run()
	if num(s, "pruned") != 1 || e.hasObject(gone) || !e.hasObject(live) {
		t.Fatalf("run 2 = %v", s)
	}
}

// A listing with a key the phase cannot read is not trusted for pruning.
func TestFilesRetention_UnreadableKeyInTheListingSkipsPrune(t *testing.T) {
	e := newFilesEnv(t, nil)
	org := "0198aaaa-0000-7000-8000-00000000000a"
	live, gone := okey(org, "live.bin"), okey(org, "gone.bin")
	e.put(live, randBytes(t, 1000))
	e.put(gone, randBytes(t, 1000))
	e.run()
	e.newManifest()
	e.del(gone)
	backdate(t, e.h.w.statePath(e.dest), map[string]time.Duration{gone: 45 * day})
	bad := "org/" + org + "/2026/u/bad\x7fname.bin"
	e.put(bad, randBytes(t, 10))
	_, s := e.run()
	if num(s, "pruned") != 0 || s["pruneSkip"] != "bad_keys" || !e.hasObject(gone) {
		t.Fatalf("step = %v", s)
	}
}

// A state line whose remote name is not the one derived from its key is never deleted.
func TestFilesRetention_ForeignRemoteNameIsNeverDeleted(t *testing.T) {
	const inst = "a1b2c3d4"
	dir := t.TempDir()
	r, _ := driver.Local{}.Open(context.Background(), driver.Config{Config: json.RawMessage(`{"dir":"` + dir + `"}`)})
	key := okey("0198aaaa-0000-7000-8000-00000000000a", "x.bin")
	foreign := "launchpad-files-" + inst + "-obj-" + strings.Repeat("e", 32) + ".age" // valid shape, not this key's hash
	if err := r.Put(context.Background(), foreign, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	state := map[string]backup.FileEntry{key: {Key: key, Remote: foreign, LastSeen: "2020-01-01T00:00:00Z"}}
	f := &filesRun{installID: inst, objRemote: r, window: 30 * day, now: time.Now()}
	var st FilesRunStats
	if err := f.pruneGone(context.Background(), []string{key}, state, &st, filepath.Join(dir, "state"), "x"); err != nil {
		t.Fatal(err)
	}
	if st.Pruned != 0 || st.PruneFailed != 1 || len(state) != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, foreign)); err != nil {
		t.Fatal("the foreign-named object was deleted")
	}
}

type failDeleteRemote struct {
	driver.Remote
	failSuffix string
}

func (r failDeleteRemote) Delete(ctx context.Context, name string) error {
	if strings.HasSuffix(name, r.failSuffix) {
		return driver.Errf(driver.ConnectRefused)
	}
	return r.Remote.Delete(ctx, name)
}

// If the manifest of an old pair cannot be deleted, its list stays (no manifest naming a missing list).
func TestFilesRetention_ListIsKeptWhenItsManifestStays(t *testing.T) {
	const inst = "a1b2c3d4"
	dir := t.TempDir()
	r, _ := driver.Local{}.Open(context.Background(), driver.Config{Config: json.RawMessage(`{"dir":"` + dir + `"}`)})
	now := time.Now().UTC()
	old := "launchpad-files-" + inst + "-list-" + now.Add(-90*day).Format(remotename.TimeLayout) + "-aaaa"
	cur := "launchpad-files-" + inst + "-list-" + now.Format(remotename.TimeLayout) + "-bbbb"
	for _, n := range []string{remotename.FilesListName(old), remotename.FilesManifestName(old), remotename.FilesListName(cur), remotename.FilesManifestName(cur)} {
		if err := r.Put(context.Background(), n, 1, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	f := &filesRun{installID: inst, listRemote: failDeleteRemote{Remote: r, failSuffix: ".json"}, window: 30 * day, now: now}
	var st FilesRunStats
	f.pruneLists(context.Background(), cur, &st)
	if st.PruneFailed != 1 || st.ListsPruned != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, remotename.FilesListName(old))); err != nil {
		t.Fatal("the list was deleted while its manifest stayed")
	}
	f.listRemote = r // the destination recovers: the pair goes
	st = FilesRunStats{}
	f.pruneLists(context.Background(), cur, &st)
	if st.ListsPruned != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, remotename.FilesListName(cur))); err != nil {
		t.Fatal("this run's list was deleted")
	}
}

func TestFilesOutcome_Precedence(t *testing.T) {
	cases := []struct {
		st         FilesRunStats
		ok         bool
		code, warn string
	}{
		{FilesRunStats{}, true, "", ""},
		{FilesRunStats{PruneFailed: 2}, true, "prune_failed", "prune_failed"},
		{FilesRunStats{Skipped: 1}, false, "incomplete", "files_incomplete"},
		{FilesRunStats{Failed: 1, PruneFailed: 2}, false, "incomplete", "files_incomplete"},
	}
	for i, c := range cases {
		ok, code, warn := filesOutcome(&c.st)
		if ok != c.ok || code != c.code || warn != c.warn {
			t.Fatalf("case %d = %v %q %q", i, ok, code, warn)
		}
	}
}
