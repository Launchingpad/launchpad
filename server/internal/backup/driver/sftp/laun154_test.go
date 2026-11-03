package sftp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gosftp "github.com/pkg/sftp"

	"devops/internal/backup/driver"
)

// ---- the swap (LAUN-154) ------------------------------------------------------------------

// swapEnv is a server without posix-rename that refuses to replace (noReplace), plus a
// scripted failure of chosen renames. renames records every rename as "from>to".
type swapEnv struct {
	h       *hookFS
	nr      *noReplace
	mu      sync.Mutex
	renames []string
	// fail decides, by the 1-based number of the rename, whether it fails (generic failure).
	fail func(n int, from, to string) bool
	// failRemove: a Remove of a path that contains this word fails.
	failRemove string
}

func newSwapEnv(t *testing.T) (*testServer, *swapEnv) {
	s := newTestServer(t)
	h := hooked(s)
	nr := &noReplace{}
	replaceRefuser(t, h, nr)
	e := &swapEnv{h: h, nr: nr}
	inner := h.cmd
	h.cmd = func(r *gosftp.Request) (error, bool) {
		if r.Method == "Rename" {
			e.mu.Lock()
			e.renames = append(e.renames, r.Filepath+">"+r.Target)
			n := len(e.renames)
			f := e.fail
			e.mu.Unlock()
			// rename 1 is the plain rename, refused by noReplace (target exists)
			if f != nil && f(n, r.Filepath, r.Target) {
				return gosftp.ErrSSHFxFailure, true
			}
		}
		if r.Method == "Remove" && e.failRemove != "" && strings.Contains(r.Filepath, e.failRemove) {
			return gosftp.ErrSSHFxFailure, true
		}
		return inner(r)
	}
	return s, e
}

func parts(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.Contains(n, partPrefix) {
			out = append(out, n)
		}
	}
	return out
}

func TestSwapSuccessUsesASecondNameAndLeavesNothing(t *testing.T) {
	s, e := newSwapEnv(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	e.mu.Lock()
	e.renames = nil
	e.mu.Unlock()
	put(t, r, validName, []byte("new"))
	if got := readAll(t, r, validName); string(got) != "new" {
		t.Fatalf("%q", got)
	}
	if p := parts(memNames(t, s)); len(p) != 0 {
		t.Fatalf("part files left: %v", p)
	}
	// plain rename (refused), old -> second name, new -> final: in this order, and the old
	// file is never removed before the new one is in place.
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.renames) != 3 || !strings.HasSuffix(strings.Split(e.renames[1], ">")[0], validName) ||
		!strings.HasSuffix(strings.Split(e.renames[2], ">")[1], validName) {
		t.Fatalf("renames: %v", e.renames)
	}
	if !strings.Contains(strings.Split(e.renames[1], ">")[1], partPrefix) {
		t.Fatalf("second name is not a part name: %v", e.renames)
	}
}

func TestSwapNewRenameFailsRollsBack(t *testing.T) {
	s, e := newSwapEnv(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	e.mu.Lock()
	e.renames = nil
	e.mu.Unlock()
	e.fail = func(n int, from, to string) bool { return n == 3 } // the tmp -> final of the swap
	err := r.Put(context.Background(), validName, 3, strings.NewReader("new"))
	if code(err) != driver.ServerError {
		t.Fatalf("%v", err)
	}
	e.fail = nil
	if got := readAll(t, r, validName); string(got) != "old" {
		t.Fatalf("rollback failed: %q", got)
	}
	if p := parts(memNames(t, s)); len(p) != 0 {
		t.Fatalf("part files left: %v", p)
	}
}

func TestSwapRollbackFailsFinishRestoresOnAFreshConnection(t *testing.T) {
	s, e := newSwapEnv(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	e.mu.Lock()
	e.renames = nil
	e.mu.Unlock()
	// swap's tmp -> final (3) and the rollback (4) fail; finish's restore (5) works.
	e.fail = func(n int, from, to string) bool { return n == 3 || n == 4 }
	err := r.Put(context.Background(), validName, 3, strings.NewReader("new"))
	if code(err) != driver.ServerError {
		t.Fatalf("%v", err)
	}
	e.fail = nil
	if got := readAll(t, r, validName); string(got) != "old" {
		t.Fatalf("the old copy was not restored: %q", got)
	}
	if p := parts(memNames(t, s)); len(p) != 0 {
		t.Fatalf("part files left: %v", p)
	}
}

func TestSwapNothingWorksAfterTheFirstMoveKeepsTheOldCopyUnderAPartName(t *testing.T) {
	s, e := newSwapEnv(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("old-bytes"))
	e.mu.Lock()
	e.renames = nil
	e.mu.Unlock()
	e.fail = func(n int, from, to string) bool { return n >= 3 } // every rename after the move out fails
	if err := r.Put(context.Background(), validName, 3, strings.NewReader("new")); err == nil {
		t.Fatal("expected an error")
	}
	e.fail = nil
	// The final name is empty, but the old bytes still exist under a part name, and the new
	// temp file was removed.
	names := memNames(t, s)
	p := parts(names)
	if len(p) != 1 {
		t.Fatalf("want exactly the old copy under a part name: %v", names)
	}
	rr := r.(*remote)
	ss, se := rr.connect(context.Background())
	if se != nil {
		t.Fatal(se.err)
	}
	defer ss.shut()
	f, err := ss.c.Open(p[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 20)
	n, _ := f.Read(buf)
	if string(buf[:n]) != "old-bytes" {
		t.Fatalf("%q", buf[:n])
	}
}

func TestSwapMoveOutFailsChangesNothing(t *testing.T) {
	s, e := newSwapEnv(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	e.mu.Lock()
	e.renames = nil
	e.mu.Unlock()
	e.fail = func(n int, from, to string) bool { return n == 2 }
	if err := r.Put(context.Background(), validName, 3, strings.NewReader("new")); err == nil {
		t.Fatal("expected an error")
	}
	e.fail = nil
	if got := readAll(t, r, validName); string(got) != "old" {
		t.Fatalf("%q", got)
	}
	if p := parts(memNames(t, s)); len(p) != 0 {
		t.Fatalf("%v", p)
	}
}

func TestSwapLeftoverSecondNameDoesNotFailThePut(t *testing.T) {
	s, e := newSwapEnv(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	e.failRemove = partPrefix // no part file can be removed at all
	// the temp file is renamed away, so only the second name is left to remove
	if err := r.Put(context.Background(), validName, 3, strings.NewReader("new")); err != nil {
		t.Fatalf("a failed removal of the second name failed the put: %v", err)
	}
	e.failRemove = ""
	if got := readAll(t, r, validName); string(got) != "new" {
		t.Fatalf("%q", got)
	}
	// List never shows the leftover as a backup.
	es, err := r.List(context.Background(), "")
	if err != nil || len(es) != 1 || es[0].Name != validName {
		t.Fatalf("%v %v", es, err)
	}
}

func TestSwapNeverStartsOnOtherCodes(t *testing.T) {
	for _, e := range []error{gosftp.ErrSSHFxPermissionDenied, gosftp.ErrSSHFxOpUnsupported} {
		s := newTestServer(t)
		h := hooked(s)
		r := open(t, s, "")
		put(t, r, validName, []byte("old"))
		refuseRenames(t, h, e)
		var mu sync.Mutex
		n := 0
		inner := h.cmd
		h.cmd = func(rq *gosftp.Request) (error, bool) {
			if rq.Method == "Rename" {
				mu.Lock()
				n++
				mu.Unlock()
			}
			return inner(rq)
		}
		_ = r.Put(context.Background(), validName, 3, strings.NewReader("new"))
		if n != 1 {
			t.Fatalf("%v: %d renames, the swap started", e, n)
		}
	}
}

// ---- the sweep (LAUN-154) -----------------------------------------------------------------

func TestIsPartName(t *testing.T) {
	for n, want := range map[string]bool{
		".launchpad-part-0123456789abcdef":           true,
		".launchpad-part-0":                          true,
		".launchpad-part-":                           false,
		".launchpad-part-XYZ":                        false,
		".launchpad-part-0123456789ABCDEF":           false,
		".launchpad-part-0123456789abcdef.bak":       false,
		".launchpad-part-" + strings.Repeat("a", 65): false,
		"x.launchpad-part-0123":                      false,
		validName:                                    false,
		"launchpad-part-0123":                        false,
	} {
		if got := isPartName(n); got != want {
			t.Errorf("%q: %v want %v", n, got, want)
		}
	}
}

type sweepInfo struct {
	fakeInfo
	mt time.Time
}

func (s sweepInfo) ModTime() time.Time { return s.mt }

func TestSweepableRule(t *testing.T) {
	cutoff := time.Unix(2_000_000_000, 0)
	part := ".launchpad-part-0123abcd"
	cases := []struct {
		name string
		fi   sweepInfo
		want bool
	}{
		{"old part", sweepInfo{fakeInfo{part, 0o600, 1}, cutoff.Add(-time.Second)}, true},
		{"exactly at the cutoff", sweepInfo{fakeInfo{part, 0o600, 1}, cutoff}, false},
		{"newer", sweepInfo{fakeInfo{part, 0o600, 1}, cutoff.Add(time.Second)}, false},
		{"zero time", sweepInfo{fakeInfo{part, 0o600, 1}, time.Time{}}, false},
		{"epoch", sweepInfo{fakeInfo{part, 0o600, 1}, time.Unix(0, 0)}, false},
		{"directory", sweepInfo{fakeInfo{part, os.ModeDir | 0o700, 1}, cutoff.Add(-time.Hour)}, false},
		{"symlink", sweepInfo{fakeInfo{part, os.ModeSymlink | 0o777, 1}, cutoff.Add(-time.Hour)}, false},
		{"backup name", sweepInfo{fakeInfo{validName, 0o600, 1}, cutoff.Add(-time.Hour)}, false},
		{"stranger", sweepInfo{fakeInfo{"notes.txt", 0o600, 1}, cutoff.Add(-time.Hour)}, false},
	}
	for _, c := range cases {
		if got := sweepable(c.fi, cutoff); got != c.want {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
	}
}

// realSweepEnv is a real file system under a server, a fixed clock and a helper that
// plants a file with a chosen age.
type realSweepEnv struct {
	dir string
	now time.Time
	r   driver.Remote
}

func newRealSweep(t *testing.T) *realSweepEnv {
	dir := t.TempDir()
	s := newTestServer(t)
	s.realDir = dir
	if err := os.MkdirAll(filepath.Join(dir, "bk"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	old := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = old })
	return &realSweepEnv{dir: filepath.Join(dir, "bk"), now: now, r: open(t, s, "bk")}
}

func (e *realSweepEnv) plant(t *testing.T, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := e.now.Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *realSweepEnv) exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestSweepRemovesOnlyOldRegularPartFiles(t *testing.T) {
	e := newRealSweep(t)
	oldPart := e.plant(t, ".launchpad-part-aaaa0001", 24*time.Hour+time.Second)
	atEdge := e.plant(t, ".launchpad-part-aaaa0002", 24*time.Hour)
	young := e.plant(t, ".launchpad-part-aaaa0003", 23*time.Hour)
	oldDump := e.plant(t, validName2, 400*time.Hour)
	oldTest := e.plant(t, testFile, 400*time.Hour)
	stranger := e.plant(t, "notes.txt", 400*time.Hour)
	badSuffix := e.plant(t, ".launchpad-part-aaaa0004.bak", 400*time.Hour)
	noRand := e.plant(t, ".launchpad-part-", 400*time.Hour)
	upper := e.plant(t, ".launchpad-part-AAAA", 400*time.Hour)
	otherHidden := e.plant(t, ".launchpad-other", 400*time.Hour)
	// a directory and a symlink with a part name, both old
	dirPart := filepath.Join(e.dir, ".launchpad-part-dddd0001")
	if err := os.Mkdir(dirPart, 0o755); err != nil {
		t.Fatal(err)
	}
	target := e.plant(t, "target.txt", 400*time.Hour)
	linkPart := filepath.Join(e.dir, ".launchpad-part-eeee0001")
	if err := os.Symlink(target, linkPart); err != nil {
		t.Fatal(err)
	}
	put(t, e.r, validName, []byte("fresh upload"))

	if e.exists(oldPart) {
		t.Error("an old part file stayed")
	}
	for name, p := range map[string]string{"at the cutoff": atEdge, "young part": young, "old dump": oldDump, "old test file": oldTest,
		"stranger": stranger, "bad suffix": badSuffix, "no random": noRand, "upper case": upper, "other hidden": otherHidden,
		"part-named directory": dirPart, "target": target} {
		if !e.exists(p) {
			t.Errorf("%s was removed", name)
		}
	}
	if fi, err := os.Lstat(linkPart); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the part-named symlink was removed or replaced: %v", err)
	}
	if got := readAll(t, e.r, validName); string(got) != "fresh upload" {
		t.Fatalf("%q", got)
	}
}

func TestSweepOnlyRunsAfterASuccessfulPut(t *testing.T) {
	e := newRealSweep(t)
	oldPart := e.plant(t, ".launchpad-part-aaaa0001", 48*time.Hour)
	// Test() and a failed Put do not sweep.
	if err := e.r.Put(context.Background(), validName, 5, strings.NewReader("abc")); err == nil {
		t.Fatal("expected a size mismatch")
	}
	for _, st := range e.r.Test(context.Background(), testFile) {
		if !st.OK {
			t.Fatalf("%+v", st)
		}
	}
	if !e.exists(oldPart) {
		t.Fatal("swept without a successful Put")
	}
	put(t, e.r, validName, []byte("ok"))
	if e.exists(oldPart) {
		t.Fatal("not swept after a successful Put")
	}
}

func TestSweepIsBounded(t *testing.T) {
	e := newRealSweep(t)
	for i := 0; i < maxSweep+5; i++ {
		e.plant(t, fmt.Sprintf(".launchpad-part-%08x", i), 48*time.Hour)
	}
	put(t, e.r, validName, []byte("ok"))
	left := 0
	ents, _ := os.ReadDir(e.dir)
	for _, en := range ents {
		if isPartName(en.Name()) {
			left++
		}
	}
	if left != 5 {
		t.Fatalf("%d part files left, want 5 (a sweep removes at most %d)", left, maxSweep)
	}
}

func TestSweepNeverTouchesTheFileBeingUploaded(t *testing.T) {
	// The upload's own temp file is renamed before the sweep; with the clock far in the
	// future every part file looks old, and the finished backup must still be there.
	e := newRealSweep(t)
	nowFn = func() time.Time { return e.now.Add(1000 * time.Hour) }
	put(t, e.r, validName, []byte("kept"))
	if got := readAll(t, e.r, validName); string(got) != "kept" {
		t.Fatalf("%q", got)
	}
}

func TestSweepListingFailureDoesNotFailThePut(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	r := open(t, s, "")
	calls := 0
	h.list = func(rq *gosftp.Request) (gosftp.ListerAt, error, bool) {
		if rq.Method == "List" {
			calls++
			return nil, gosftp.ErrSSHFxFailure, true
		}
		return nil, nil, false
	}
	put(t, r, validName, []byte("ok"))
	if calls == 0 {
		t.Fatal("the sweep never listed")
	}
	h.list = nil
	if got := readAll(t, r, validName); string(got) != "ok" {
		t.Fatalf("%q", got)
	}
}

// ---- review additions: the guards of swapReplace --------------------------------------

// refuseFirstRenameToFinal refuses ONLY the first plain rename onto the final name with the
// generic failure code (a server that will not replace). Every other rename works, so a swap
// that wrongly starts would succeed and the test would see the damage.
func refuseFirstRenameToFinal(t *testing.T, h *hookFS, final string) *int {
	allowPosixRename = false
	t.Cleanup(func() { allowPosixRename = true })
	n := 0
	var mu sync.Mutex
	h.cmd = func(r *gosftp.Request) (error, bool) {
		if r.Method == "Rename" && r.Target == final {
			mu.Lock()
			defer mu.Unlock()
			n++
			if n == 1 {
				return gosftp.ErrSSHFxFailure, true
			}
		}
		return nil, false
	}
	return &n
}

func TestSwapNeverStartsOnADirectoryOrASymlink(t *testing.T) {
	for _, kind := range []string{"dir", "symlink"} {
		s := newTestServer(t)
		h := hooked(s)
		r := open(t, s, "")
		rr := r.(*remote)
		ss, se := rr.connect(context.Background())
		if se != nil {
			t.Fatal(se.err)
		}
		if kind == "dir" {
			if err := ss.c.Mkdir(validName); err != nil {
				t.Fatal(err)
			}
		} else {
			f, err := ss.c.Create(validName2)
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			if err := ss.c.Symlink("/"+validName2, validName); err != nil {
				t.Fatal(err)
			}
		}
		ss.shut()
		refuseFirstRenameToFinal(t, h, "/"+validName)
		err := r.Put(context.Background(), validName, 3, strings.NewReader("new"))
		if err == nil {
			t.Fatalf("%s: the put replaced a %s", kind, kind)
		}
		h.cmd = nil
		ss, se = rr.connect(context.Background())
		if se != nil {
			t.Fatal(se.err)
		}
		fi, lerr := ss.c.Lstat(validName)
		ss.shut()
		if lerr != nil {
			t.Fatalf("%s: gone: %v", kind, lerr)
		}
		if kind == "dir" && !fi.IsDir() || kind == "symlink" && fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: replaced by %v", kind, fi.Mode())
		}
		if p := parts(memNames(t, s)); len(p) != 0 {
			t.Fatalf("%s: part files left: %v", kind, p)
		}
	}
}

// The second name gets the time of now, so a sweep cannot take an old copy at once.
func TestSwapSetsTheSecondNamesTimeToNow(t *testing.T) {
	s, e := newSwapEnv(t)
	fixed := time.Unix(1_900_000_000, 0)
	old := nowFn
	nowFn = func() time.Time { return fixed }
	defer func() { nowFn = old }()
	hk := e.h
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	var mu sync.Mutex
	var got []uint32
	inner := hk.cmd
	hk.cmd = func(rq *gosftp.Request) (error, bool) {
		if rq.Method == "Setstat" && rq.AttrFlags().Acmodtime && strings.Contains(rq.Filepath, partPrefix) {
			mu.Lock()
			got = append(got, rq.Attributes().Mtime)
			mu.Unlock()
		}
		return inner(rq)
	}
	put(t, r, validName, []byte("new"))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != uint32(fixed.Unix()) {
		t.Fatalf("times set on the second name: %v, want [%d]", got, fixed.Unix())
	}
}
