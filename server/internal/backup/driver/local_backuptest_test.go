//go:build backuptest

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localRemoteFor(t *testing.T) (Remote, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, _ := json.Marshal(map[string]string{"dir": dir})
	r, err := Local{}.Open(context.Background(), Config{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func TestLocalDriver(t *testing.T) {
	ctx := context.Background()
	r, dir := localRemoteFor(t)
	name := "launchpad-a1b2c3d4-20261026T031500Z-09af.dump.age"
	if err := r.Put(ctx, name, 5, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	es, err := r.List(ctx, "launchpad-a1b2c3d4-")
	if err != nil || len(es) != 1 || es[0].Name != name || es[0].Size != 5 {
		t.Fatalf("List = %v, %v", es, err)
	}
	rc, err := r.Get(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Errorf("Get = %q", b)
	}
	if err := r.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, name); err != nil {
		t.Errorf("missing delete: %v", err)
	}
	if _, err := r.Get(ctx, name); CodeOf(err) != NotFound {
		t.Errorf("Get missing: %v", err)
	}
	// Wrong sizes store nothing and leave no temp file.
	for _, tc := range []struct {
		size int64
		body string
	}{{5, "toolong"}, {5, "abc"}, {-1, "x"}} {
		if err := r.Put(ctx, name, tc.size, strings.NewReader(tc.body)); err == nil {
			t.Errorf("Put size %d body %q accepted", tc.size, tc.body)
		}
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
	steps := r.Test(ctx, "launchpad-a1b2c3d4-test-0123abcd.tmp")
	if len(steps) != 5 {
		t.Fatalf("steps = %v", steps)
	}
	for _, s := range steps {
		if !s.OK {
			t.Errorf("step %+v", s)
		}
	}
}

func TestLocalDriverRefusesHostileNames(t *testing.T) {
	ctx := context.Background()
	r, dir := localRemoteFor(t)
	outside := filepath.Join(filepath.Dir(dir), "outside.json")
	for _, n := range []string{"../outside.json", "../../etc/passwd", "a/b.json", "/abs.json", "", "..", ".",
		"launchpad-a1b2c3d4-20261026T031500Z-09af.json/../../x", "launchpad-a1b2c3d4-20261026T031500Z-09af.json\x00x"} {
		if err := r.Put(ctx, n, 1, strings.NewReader("x")); err == nil {
			t.Errorf("Put %q accepted", n)
		}
		if _, err := r.Get(ctx, n); err == nil {
			t.Errorf("Get %q accepted", n)
		}
		if err := r.Delete(ctx, n); err == nil {
			t.Errorf("Delete %q accepted", n)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a file was written outside the directory")
	}
}

func TestLocalConfigValidation(t *testing.T) {
	for _, c := range []string{`{}`, `{"dir":""}`, `{"dir":"rel/path"}`, `{"dir":"/a/../b"}`, `{"dir":"/a","x":1}`, `not json`} {
		if (Local{}).Validate(json.RawMessage(c)) == nil {
			t.Errorf("%s accepted", c)
		}
	}
	if id, err := (Local{}).Identity(json.RawMessage(`{"dir":"/srv/x"}`)); err != nil || id.Folder != "srv/x" {
		t.Errorf("Identity = %+v, %v", id, err)
	}
}

func TestRegisterForTestUnderRealTypeName(t *testing.T) {
	var before []string
	for _, typ := range []string{"s3", "webdav", "sftp", "dropbox"} {
		before = append(before, fmt.Sprint(Lookup(typ)))
	}
	t.Run("inner", func(t *testing.T) {
		for _, typ := range []string{"s3", "webdav", "sftp", "dropbox"} {
			RegisterForTest(t, typ, Local{})
			d, ok := Lookup(typ)
			if !ok || d == nil {
				t.Fatalf("%s not registered", typ)
			}
		}
		if got := strings.Join(SupportedTypes(), ","); got != "dropbox,s3,sftp,webdav" {
			t.Errorf("SupportedTypes = %s", got)
		}
	})
	// Cleanup restored the empty state: the types are absent again.
	for i, typ := range []string{"s3", "webdav", "sftp", "dropbox"} {
		if fmt.Sprint(Lookup(typ)) != before[i] {
			t.Errorf("%s not restored", typ)
		}
	}
	if len(SupportedTypes()) != 0 {
		t.Errorf("types left: %v", SupportedTypes())
	}
}

func TestRegisterForTestReplacesAndRestores(t *testing.T) {
	Register("zz-real", fake{})
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		delete(registry, "zz-real")
	})
	t.Run("inner", func(t *testing.T) {
		RegisterForTest(t, "zz-real", Local{})
		if d, _ := Lookup("zz-real"); d == (fake{}) {
			t.Error("not replaced")
		}
	})
	if d, _ := Lookup("zz-real"); d != (fake{}) {
		t.Error("not restored")
	}
}
