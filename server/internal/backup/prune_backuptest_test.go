//go:build backuptest

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

// failDelete wraps a Remote and fails Delete for chosen names.
type failDelete struct {
	driver.Remote
	fail    map[string]bool
	deleted []string
	listErr error
}

func (f *failDelete) List(ctx context.Context, p string) ([]driver.Entry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Remote.List(ctx, p)
}

func (f *failDelete) Delete(ctx context.Context, n string) error {
	if f.fail[n] {
		return driver.ServerErr(500)
	}
	f.deleted = append(f.deleted, n)
	return f.Remote.Delete(ctx, n)
}

func localRemote(t *testing.T, files ...string) (driver.Remote, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := json.Marshal(map[string]string{"dir": dir})
	r, err := driver.Local{}.Open(context.Background(), driver.Config{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r, dir
}

func remaining(t *testing.T, dir string) string {
	des, _ := os.ReadDir(dir)
	var n []string
	for _, d := range des {
		n = append(n, d.Name())
	}
	return sorted(n)
}

func TestPruneOnLocalDriver(t *testing.T) {
	ctx := context.Background()
	s1, s2, s3 := stemAt(rInst, 20, 4, "0001"), stemAt(rInst, 21, 4, "0002"), stemAt(rInst, 26, 4, "aaaa")
	other := "launchpad-deadbeef-20250101T000000Z-0000.dump.age"
	files := append(pairNames(s1, s2, s3), other, "readme.txt", "launchpad-a1b2c3d4-test-0123abcd.tmp")

	r, dir := localRemote(t, files...)
	res, err := Prune(ctx, r, rInst, 1, now, nil, s3)
	if err != nil || res.Deleted != 4 || res.Failed != 0 {
		t.Fatalf("prune = %+v, %v", res, err)
	}
	want := sorted(append(pairNames(s3), other, "readme.txt", "launchpad-a1b2c3d4-test-0123abcd.tmp"))
	if got := remaining(t, dir); got != want {
		t.Errorf("remaining = %s", got)
	}

	// Not verified: nothing is deleted.
	r, dir = localRemote(t, files...)
	if _, err := Prune(ctx, r, rInst, 1, now, nil, stemAt(rInst, 27, 4, "bbbb")); !errors.Is(err, ErrNotVerifiedNew) {
		t.Errorf("err = %v", err)
	}
	if got := remaining(t, dir); got != sorted(files) {
		t.Errorf("files changed: %s", got)
	}

	// A list error deletes nothing.
	r, dir = localRemote(t, files...)
	fd := &failDelete{Remote: r, listErr: driver.Errf(driver.Timeout)}
	if _, err := Prune(ctx, fd, rInst, 1, now, nil, s3); err == nil || len(fd.deleted) != 0 {
		t.Errorf("list error: %v %v", err, fd.deleted)
	}

	// A failed delete is counted, the rest go on (the run stays succeeded with prune_failed).
	r, dir = localRemote(t, files...)
	fd = &failDelete{Remote: r, fail: map[string]bool{s1 + ".json": true}}
	res, err = Prune(ctx, fd, rInst, 1, now, nil, s3)
	if err != nil || res.Failed != 1 || res.Deleted != 3 {
		t.Errorf("prune = %+v, %v", res, err)
	}

	// Every Delete name is one the matcher produced.
	m, _ := remotename.NewMatcher(rInst)
	for _, n := range fd.deleted {
		if _, ok := m.Parse(n); !ok {
			t.Errorf("deleted a name the matcher does not accept: %s", n)
		}
	}
}
