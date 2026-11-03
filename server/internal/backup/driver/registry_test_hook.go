//go:build backuptest

package driver

import "testing"

// RegisterForTest replaces the driver of a type for one test and puts the old
// one back in t.Cleanup (T16 #16). Under this tag only. A test may register a
// fake under a REAL type name (s3, webdav, sftp, dropbox); a driver's own
// tests call the driver directly and never see the fake.
func RegisterForTest(t testing.TB, typ string, d Driver) {
	t.Helper()
	if typ == "" || d == nil {
		t.Fatal("RegisterForTest needs a type and a driver")
	}
	mu.Lock()
	old, had := registry[typ]
	registry[typ] = d
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if had {
			registry[typ] = old
		} else {
			delete(registry, typ)
		}
	})
}
