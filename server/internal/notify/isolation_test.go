package notify_test

import (
	"go/build"
	"slices"
	"testing"
)

// TestNoWritePathImportsNotify: the consumer reads the feed; no write path
// calls into it (decisions-T13 #37). The packages that make events must not
// import internal/notify.
func TestNoWritePathImportsNotify(t *testing.T) {
	for _, dir := range []string{"../tracker", "../events", "../setup", "../signup", "../login", "../importer"} {
		pkg, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if slices.Contains(pkg.Imports, "devops/internal/notify") {
			t.Errorf("%s imports internal/notify", dir)
		}
	}
}
