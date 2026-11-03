package backup

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"devops/internal/backup/driver"
)

const rInst = "a1b2c3d4"

var now = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func stemAt(inst string, day, hour int, tag string) string {
	return fmt.Sprintf("launchpad-%s-202610%02dT%02d0000Z-%s", inst, day, hour, tag)
}

func pairNames(stems ...string) []string {
	var out []string
	for _, s := range stems {
		out = append(out, s+".dump.age", s+".json")
	}
	return out
}

func sorted(s []string) string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return strings.Join(c, " ")
}

func TestPlanPrune(t *testing.T) {
	s := func(day int, tag string) string { return stemAt(rInst, day, 4, tag) }
	foreign := []string{
		"notes.txt", "launchpad-ffffffff-20260101T000000Z-0000.dump.age", "launchpad-ffffffff-20260101T000000Z-0000.json",
		"launchpad-a1b2c3d4-test-0123abcd.tmp", "launchpad-a1b2c3d4-20261301T000000Z-0000.dump.age", // month 13
		"launchpad-a1b2c3d4-20260101T000000Z-0000.dump", "launchpad-a1b2c3d4-20260101T000000Z-00.json",
		"LAUNCHPAD-a1b2c3d4-20260101T000000Z-0000.json", "sub/launchpad-a1b2c3d4-20260101T000000Z-0000.json",
	}
	newest := s(26, "aaaa")

	t.Run("keeps the newest N pairs and deletes older pairs whole", func(t *testing.T) {
		names := append(pairNames(s(20, "0001"), s(21, "0002"), s(22, "0003"), s(23, "0004"), newest), foreign...)
		p, err := PlanPrune(rInst, 2, names, now, nil, newest)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(p.Keep, " ") != newest+" "+s(23, "0004") {
			t.Errorf("keep = %v", p.Keep)
		}
		want := pairNames(s(20, "0001"), s(21, "0002"), s(22, "0003"))
		if sorted(p.Delete) != sorted(want) || len(p.Orphans) != 0 {
			t.Errorf("delete = %v", p.Delete)
		}
		for _, d := range p.Delete {
			for _, f := range foreign {
				if d == f {
					t.Errorf("deleted foreign file %s", d)
				}
			}
		}
	})
	t.Run("sorted by the time in the name, then the stem, not by listing order", func(t *testing.T) {
		a, b, c := stemAt(rInst, 25, 4, "bbbb"), stemAt(rInst, 25, 4, "cccc"), stemAt(rInst, 25, 3, "ffff")
		names := pairNames(c, newest, a, b) // scrambled
		p, _ := PlanPrune(rInst, 2, names, now, nil, newest)
		if strings.Join(p.Keep, " ") != newest+" "+b {
			t.Errorf("keep = %v", p.Keep)
		}
		if sorted(p.Delete) != sorted(pairNames(a, c)) {
			t.Errorf("delete = %v", p.Delete)
		}
	})
	t.Run("two copies of one dump both count", func(t *testing.T) {
		a, b := stemAt(rInst, 25, 4, "1111"), stemAt(rInst, 25, 4, "2222")
		p, _ := PlanPrune(rInst, 3, pairNames(a, b, newest), now, nil, newest)
		if len(p.Keep) != 3 || len(p.Delete) != 0 {
			t.Errorf("plan = %+v", p)
		}
	})
	t.Run("orphans: old ones go, young and running ones stay, they never count toward N", func(t *testing.T) {
		oldObj := stemAt(rInst, 20, 4, "0a0a")  // object only, 7 days old
		oldMeta := stemAt(rInst, 21, 4, "0b0b") // manifest only, old
		young := stemAt(rInst, 27, 4, "0c0c")   // 8 hours old
		edge := stemAt(rInst, 26, 12, "0d0d")   // exactly 24 hours old: not "more than"
		over := stemAt(rInst, 26, 11, "0e0e")   // 25 hours old
		busy := stemAt(rInst, 22, 4, "0f0f")    // old but a running run names it
		names := append([]string{oldObj + ".dump.age", oldMeta + ".json", young + ".dump.age", edge + ".json", over + ".dump.age", busy + ".dump.age"}, pairNames(newest)...)
		p, err := PlanPrune(rInst, 1, names, now, map[string]bool{busy: true}, newest)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{oldObj + ".dump.age", oldMeta + ".json", over + ".dump.age"}
		if sorted(p.Delete) != sorted(want) || sorted(p.Orphans) != sorted(want) {
			t.Errorf("delete = %v", p.Delete)
		}
		if len(p.Keep) != 1 {
			t.Errorf("an orphan counted toward N: %v", p.Keep)
		}
	})
	t.Run("another install's files are never touched, even old orphans", func(t *testing.T) {
		other := stemAt("deadbeef", 1, 4, "0000")
		names := append(pairNames(other, newest), other+"x.json")
		p, _ := PlanPrune(rInst, 1, names, now, nil, newest)
		if len(p.Delete) != 0 {
			t.Errorf("delete = %v", p.Delete)
		}
	})
	t.Run("a failed run deletes nothing", func(t *testing.T) {
		names := pairNames(s(20, "0001"), s(21, "0002"))
		for name, stem := range map[string]string{"missing": newest, "empty": "", "object only": s(21, "0002")} {
			ns := names
			if name == "object only" {
				ns = []string{s(21, "0002") + ".dump.age", s(20, "0001") + ".json", s(20, "0001") + ".dump.age"}
			}
			p, err := PlanPrune(rInst, 1, ns, now, nil, stem)
			if err == nil || len(p.Delete) != 0 {
				t.Errorf("%s: err=%v plan=%+v", name, err, p)
			}
		}
	})
	t.Run("keep_last and install id bounds", func(t *testing.T) {
		for _, k := range []int{0, -1, 366, 1000} {
			if _, err := PlanPrune(rInst, k, pairNames(newest), now, nil, newest); err == nil {
				t.Errorf("keep_last %d accepted", k)
			}
		}
		for _, k := range []int{1, 365} {
			if _, err := PlanPrune(rInst, k, pairNames(newest), now, nil, newest); err != nil {
				t.Errorf("keep_last %d: %v", k, err)
			}
		}
		if _, err := PlanPrune(".*", 1, pairNames(newest), now, nil, newest); err == nil {
			t.Error("bad install id accepted")
		}
	})
	t.Run("duplicate names in a listing", func(t *testing.T) {
		names := append(pairNames(newest), pairNames(newest)...)
		p, err := PlanPrune(rInst, 1, names, now, nil, newest)
		if err != nil || len(p.Delete) != 0 || len(p.Keep) != 1 {
			t.Errorf("plan = %+v, %v", p, err)
		}
	})
}

// stubRemote lists whatever it is given, hrefs and paths included, and records Delete calls.
type stubRemote struct {
	list    []string
	deleted []string
}

func (s *stubRemote) Test(context.Context, string) []driver.Step          { return nil }
func (s *stubRemote) Put(context.Context, string, int64, io.Reader) error { return nil }
func (s *stubRemote) Get(context.Context, string) (io.ReadCloser, error)  { return nil, nil }
func (s *stubRemote) Close() error                                        { return nil }
func (s *stubRemote) List(context.Context, string) ([]driver.Entry, error) {
	var out []driver.Entry
	for _, n := range s.list {
		out = append(out, driver.Entry{Name: n, Size: 1})
	}
	return out, nil
}
func (s *stubRemote) Delete(_ context.Context, n string) error {
	s.deleted = append(s.deleted, n)
	return nil
}

func TestPruneDeletesOnlyMatchingNames(t *testing.T) {
	newest := stemAt(rInst, 26, 4, "aaaa")
	old := stemAt(rInst, 20, 4, "0001")
	evil := []string{
		"../" + old + ".json", "/" + old + ".json", "https://evil.example/" + old + ".json",
		old + ".json/../../etc/passwd", old + ".json?x=1", "%2e%2e/" + old + ".json", old + ".json\x00",
		"launchpad-a1b2c3d4-20200101T000000Z-0000.dump.age/",
	}
	r := &stubRemote{list: append(append(pairNames(old, newest), evil...), "launchpad-deadbeef-20200101T000000Z-0000.json")}
	res, err := Prune(context.Background(), r, rInst, 1, now, nil, newest)
	if err != nil || res.Deleted != 2 {
		t.Fatalf("prune = %+v, %v", res, err)
	}
	if sorted(r.deleted) != sorted(pairNames(old)) {
		t.Errorf("deleted = %q", r.deleted)
	}
}

// The verified pair is never deleted, even when the clock stepped back or
// future-dated pairs were planted (it falls outside the newest N).
func TestPlanPruneNeverDeletesVerifiedStem(t *testing.T) {
	verified := stemAt(rInst, 27, 4, "aaaa") // "now" is 2026-10-27 12:00

	t.Run("clock stepped back, keep_last 1", func(t *testing.T) {
		// Earlier runs were dated after the stepped-back clock.
		fut1, fut2 := stemAt(rInst, 29, 4, "0001"), stemAt(rInst, 30, 4, "0002")
		p, err := PlanPrune(rInst, 1, pairNames(verified, fut1, fut2), now, nil, verified)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Delete {
			if strings.HasPrefix(d, verified) {
				t.Fatalf("verified pair in Delete: %v", p.Delete)
			}
		}
		if sorted(p.Keep) != sorted([]string{verified, fut2}) {
			t.Errorf("keep = %v", p.Keep)
		}
		if sorted(p.Delete) != sorted(pairNames(fut1)) {
			t.Errorf("delete = %v", p.Delete)
		}
	})
	t.Run("365+ future-dated pairs, keep_last 365", func(t *testing.T) {
		var names []string
		for i := 0; i < 366; i++ {
			d := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
			names = append(names, pairNames(fmt.Sprintf("launchpad-%s-%sT000000Z-%04x", rInst, d.Format("20060102"), i))...)
		}
		names = append(names, pairNames(verified)...)
		p, err := PlanPrune(rInst, 365, names, now, nil, verified)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Delete {
			if strings.HasPrefix(d, verified) {
				t.Fatalf("verified pair in Delete")
			}
		}
		if len(p.Keep) != 366 { // the newest 365 plus the verified one
			t.Errorf("keep = %d", len(p.Keep))
		}
		if len(p.Delete) != 2*(367-366) {
			t.Errorf("delete = %d names", len(p.Delete))
		}
	})
	t.Run("normal case is unchanged", func(t *testing.T) {
		old := stemAt(rInst, 20, 4, "0001")
		p, _ := PlanPrune(rInst, 1, pairNames(old, verified), now, nil, verified)
		if len(p.Keep) != 1 || sorted(p.Delete) != sorted(pairNames(old)) {
			t.Errorf("plan = %+v", p)
		}
	})
}

type listSpy struct {
	stubRemote
	listed bool
}

func (l *listSpy) List(ctx context.Context, p string) ([]driver.Entry, error) {
	l.listed = true
	return l.stubRemote.List(ctx, p)
}

func TestPruneValidatesInstallIDBeforeList(t *testing.T) {
	for _, id := range []string{"", ".*", "../x", "a1b2c3d", "A1B2C3D4", "a1b2c3d4/", "a1b2c3d4\n"} {
		r := &listSpy{}
		if _, err := Prune(context.Background(), r, id, 1, now, nil, "x"); err == nil {
			t.Errorf("install id %q accepted", id)
		}
		if r.listed || len(r.deleted) != 0 {
			t.Errorf("install id %q: List called=%v deleted=%v", id, r.listed, r.deleted)
		}
	}
}
