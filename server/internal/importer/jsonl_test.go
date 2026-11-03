package importer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFixture(t *testing.T) (string, *Export) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	ex := fixtureExport()
	if err := WriteExport(root, ex, ts("2026-10-24T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	return dir, ex
}

func TestWriteExport_ReadExport_roundTrip(t *testing.T) {
	dir, ex := writeFixture(t)
	got, err := ReadExport(dir)
	if err != nil {
		t.Fatal(err)
	}
	pairs := map[string][2]any{
		"teams": {got.Teams, ex.Teams}, "states": {got.States, ex.States}, "labels": {got.Labels, ex.Labels},
		"users": {got.Users, ex.Users}, "projects": {got.Projects, ex.Projects},
		"milestones": {got.Milestones, ex.Milestones}, "cycles": {got.Cycles, ex.Cycles},
		"issues": {got.Issues, ex.Issues}, "comments": {got.Comments, ex.Comments},
		"documents": {got.Documents, ex.Documents}, "relations": {got.Relations, ex.Relations},
		"attachments": {got.Attachments, ex.Attachments}, "files": {got.Files, ex.Files},
	}
	for name, p := range pairs {
		if !reflect.DeepEqual(p[0], p[1]) {
			t.Errorf("%s changed in the round trip:\n got %+v\nwant %+v", name, p[0], p[1])
		}
	}
	st, err := os.Stat(filepath.Join(dir, "issues.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("issues.jsonl mode %v, want 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "issues.jsonl.tmp")); !os.IsNotExist(err) {
		t.Error("the temporary file must be renamed away")
	}
}

func TestReadExport_refusesABadExport(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(dir string) error
		want   string
	}{
		{"a truncated file", func(dir string) error {
			p := filepath.Join(dir, "comments.jsonl")
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			lines := strings.SplitAfter(string(b), "\n")
			return os.WriteFile(p, []byte(strings.Join(lines[:len(lines)-2], "")), 0o600)
		}, "comments.jsonl has 7 records, manifest.json says 8"},
		{"a missing file", func(dir string) error { return os.Remove(filepath.Join(dir, "documents.jsonl")) },
			"open documents.jsonl"},
		{"a corrupt line", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "teams.jsonl"), []byte("{not json\n"), 0o600)
		}, "teams.jsonl line 1: invalid record"},
		{"no manifest", func(dir string) error { return os.Remove(filepath.Join(dir, "manifest.json")) },
			"read manifest.json"},
		{"another format", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"format":99}`), 0o600)
		}, "export format 99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, _ := writeFixture(t)
			if err := tt.break_(dir); err != nil {
				t.Fatal(err)
			}
			_, err := ReadExport(dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestReadJSONL_errorHasNoRecordText(t *testing.T) {
	dir := t.TempDir()
	secret := "TICKET-TEXT-THAT-MUST-NOT-LEAK"
	if err := os.WriteFile(filepath.Join(dir, "issues.jsonl"), []byte(`{"id":"x","title":5,"description":"`+secret+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	_, err = ReadJSONL[Issue](root, "issues")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v: want an error without the record's text", err)
	}
}
