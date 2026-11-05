package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMoveArgs(t *testing.T) {
	var se bytes.Buffer
	a, ok := parseMoveExportArgs([]string{"--no-attachments", "--user", "chen", "out.bundle"}, &se)
	if !ok || a.path != "out.bundle" || a.user != "chen" || !a.noAttachments {
		t.Fatalf("export args: %+v %v", a, ok)
	}
	for _, bad := range [][]string{nil, {"a", "b"}, {"--bogus", "a"}} {
		if _, ok := parseMoveExportArgs(bad, &se); ok {
			t.Errorf("export accepted %v", bad)
		}
		if _, ok := parseMoveImportArgs(bad, &se); ok {
			t.Errorf("import accepted %v", bad)
		}
	}
	// The passphrase has no flag that takes a value, and no environment variable.
	if _, ok := parseMoveImportArgs([]string{"--passphrase", "secret", "f"}, &se); ok {
		t.Error("import must not take the passphrase as an argument")
	}
	b, ok := parseMoveImportArgs([]string{"--passphrase-stdin", "f"}, &se)
	if !ok || !b.passphraseStdin || b.path != "f" {
		t.Fatalf("import args: %+v", b)
	}
	if !strings.Contains(se.String(), "usage: server move-") {
		t.Error("usage text missing")
	}
}

func TestReadPassphrase(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) *os.File {
		p := filepath.Join(dir, "stdin")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	var se bytes.Buffer
	got, err := readPassphrase(write("ABCD-EFGH\nsecond line\n"), true, &se)
	if err != nil || got != "ABCD-EFGH" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, err = readPassphrase(write("no newline"), true, &se); err != nil || got != "no newline" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err = readPassphrase(write(""), true, &se); err == nil {
		t.Fatal("empty stdin must fail")
	}
	// A file is not a terminal: without --passphrase-stdin the prompt refuses.
	if _, err = readPassphrase(write("x\n"), false, &se); err == nil {
		t.Fatal("a non-terminal must fail the hidden prompt")
	}
	if se.Len() != 0 {
		t.Errorf("nothing should be written to stderr: %q", se.String())
	}
}

func TestMoveCmdUsageExitCode(t *testing.T) {
	var so, se bytes.Buffer
	if rc := moveExportCmd(nil, &so, &se); rc != 2 {
		t.Errorf("export exit %d", rc)
	}
	if rc := moveImportCmd(nil, os.Stdin, &so, &se); rc != 2 {
		t.Errorf("import exit %d", rc)
	}
	if so.Len() != 0 {
		t.Errorf("stdout %q", so.String())
	}
}
