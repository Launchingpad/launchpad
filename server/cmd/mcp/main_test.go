package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// --version needs no config: no URL, token or org is set, and it still
// prints the stamped version on stdout and succeeds.
func TestVersionFlag(t *testing.T) {
	for _, k := range []string{"LAUNCHPAD_URL", "LAUNCHPAD_TOKEN", "LAUNCHPAD_ORG"} {
		t.Setenv(k, "")
	}
	oldArgs, oldOut, oldVer := os.Args, os.Stdout, version
	t.Cleanup(func() { os.Args, os.Stdout, version = oldArgs, oldOut, oldVer })
	version = "abc123def456"
	os.Args = []string{"launchpad-mcp", "--version"}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := run()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if got := strings.TrimSpace(string(out)); got != "launchpad-mcp abc123def456" {
		t.Fatalf("stdout = %q", got)
	}
}
