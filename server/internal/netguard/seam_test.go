package netguard_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T17 #31: production code has no flag, variable or build tag that lets a
// loopback address through. The seam is Policy.TestPermit, set only by
// netguardtest, which only _test.go files import.
func TestNoProductionBypass(t *testing.T) {
	root, err := filepath.Abs("../..") // server/
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		seen++
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		inGuard := strings.HasPrefix(rel, "internal/netguard/")
		// Test-helper packages may import the seam; nothing else outside a
		// _test.go file may import them.
		helper := strings.HasPrefix(rel, "internal/netguard/netguardtest/") || strings.HasPrefix(rel, "internal/gitapi/gitapitest/")
		if (strings.Contains(src, "netguardtest\"") || strings.Contains(src, "gitapitest\"")) && !helper {
			t.Errorf("%s imports a test helper package", rel)
		}
		if strings.Contains(src, "TestPermit") && !(rel == "internal/netguard/netguard.go" || rel == "internal/netguard/netguardtest/netguardtest.go") {
			t.Errorf("%s names TestPermit", rel)
		}
		if strings.Contains(src, "go:build") && inGuard || strings.Contains(src, "+build") && inGuard {
			if strings.Contains(src, "netguard") {
				t.Errorf("%s: a build tag in the guard", rel)
			}
		}
		// No production code reads an environment variable that opens the guard.
		if strings.Contains(src, "Getenv(\"NETGUARD") || strings.Contains(src, "INSECURE_ALLOW") {
			t.Errorf("%s reads a bypass variable", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 10 {
		t.Fatalf("walked only %d production files: wrong root", seen)
	}
}
