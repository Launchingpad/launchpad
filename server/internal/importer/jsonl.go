package importer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// maxLine bounds one JSONL record. An issue with a long description and
// its whole history is far below this; a larger line means a corrupt file.
const maxLine = 64 << 20

// WriteJSONL writes items to name+".jsonl" in root, one JSON object per
// line. It writes a temporary file and renames it, so a crash never leaves
// a half-written file that looks complete. Files are 0600: an export holds
// user emails and every ticket's text.
func WriteJSONL[T any](root *os.Root, name string, items []T) error {
	final := name + ".jsonl"
	tmp := final + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for i := range items {
		if err := enc.Encode(items[i]); err != nil {
			_ = f.Close()
			return fmt.Errorf("encode %s record %d: %w", name, i, err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := root.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// ReadJSONL reads name+".jsonl" from root. A missing file is an error: an
// export always writes every file, even an empty one.
func ReadJSONL[T any](root *os.Root, name string) ([]T, error) {
	f, err := root.Open(name + ".jsonl")
	if err != nil {
		return nil, fmt.Errorf("open %s.jsonl: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	return decodeJSONL[T](f, name)
}

func decodeJSONL[T any](r io.Reader, name string) ([]T, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	out := []T{}
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			// encoding/json errors name the field and the offset, never the
			// value, so no ticket text reaches the message.
			return nil, fmt.Errorf("%s.jsonl line %d: invalid record: %w", name, line, err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s.jsonl: %w", name, err)
	}
	return out, nil
}

// WriteManifest writes manifest.json.
func WriteManifest(root *os.Root, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	f, err := root.OpenFile("manifest.json.tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	return root.Rename("manifest.json.tmp", "manifest.json")
}

// ReadExport reads a whole export directory and checks every file against
// the manifest's counts, so a truncated or hand-edited file is refused
// before anything is written to the database.
func ReadExport(dir string) (*Export, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open export dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	return ReadExportRoot(root)
}

// ReadExportRoot is ReadExport on an already opened directory (the web
// import opens an adopted export with os.Root, so no step can leave it).
func ReadExportRoot(root *os.Root) (*Export, error) {
	mb, err := root.ReadFile("manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read manifest.json: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("export format %d, this importer reads %d", m.Format, ManifestFormat)
	}
	ex := &Export{Manifest: m}
	steps := []struct {
		name string
		read func() (int, error)
	}{
		{FileTeams, func() (int, error) { return readInto(root, FileTeams, &ex.Teams) }},
		{FileStates, func() (int, error) { return readInto(root, FileStates, &ex.States) }},
		{FileLabels, func() (int, error) { return readInto(root, FileLabels, &ex.Labels) }},
		{FileUsers, func() (int, error) { return readInto(root, FileUsers, &ex.Users) }},
		{FileProjects, func() (int, error) { return readInto(root, FileProjects, &ex.Projects) }},
		{FileMilestones, func() (int, error) { return readInto(root, FileMilestones, &ex.Milestones) }},
		{FileCycles, func() (int, error) { return readInto(root, FileCycles, &ex.Cycles) }},
		{FileIssues, func() (int, error) { return readInto(root, FileIssues, &ex.Issues) }},
		{FileComments, func() (int, error) { return readInto(root, FileComments, &ex.Comments) }},
		{FileDocuments, func() (int, error) { return readInto(root, FileDocuments, &ex.Documents) }},
		{FileRelations, func() (int, error) { return readInto(root, FileRelations, &ex.Relations) }},
		{FileAttachments, func() (int, error) { return readInto(root, FileAttachments, &ex.Attachments) }},
		{FileFiles, func() (int, error) { return readInto(root, FileFiles, &ex.Files) }},
	}
	for _, s := range steps {
		n, err := s.read()
		if err != nil {
			return nil, err
		}
		want, ok := m.Counts[s.name]
		if !ok {
			return nil, fmt.Errorf("manifest.json has no count for %s", s.name)
		}
		if n != want {
			return nil, fmt.Errorf("%s.jsonl has %d records, manifest.json says %d", s.name, n, want)
		}
	}
	return ex, nil
}

func readInto[T any](root *os.Root, name string, dst *[]T) (int, error) {
	v, err := ReadJSONL[T](root, name)
	if err != nil {
		return 0, err
	}
	*dst = v
	return len(v), nil
}

// NewManifest is a manifest for ex with the counts of every file.
func NewManifest(ex *Export, at time.Time) Manifest {
	return Manifest{Format: ManifestFormat, ExportedAt: at.UTC(), Counts: map[string]int{
		FileTeams: len(ex.Teams), FileStates: len(ex.States), FileLabels: len(ex.Labels),
		FileUsers: len(ex.Users), FileProjects: len(ex.Projects), FileMilestones: len(ex.Milestones),
		FileCycles: len(ex.Cycles), FileIssues: len(ex.Issues), FileComments: len(ex.Comments),
		FileDocuments: len(ex.Documents), FileRelations: len(ex.Relations),
		FileAttachments: len(ex.Attachments), FileFiles: len(ex.Files),
	}}
}

// WriteExport writes every file of ex and then the manifest (last, so an
// export with a manifest is complete).
func WriteExport(root *os.Root, ex *Export, at time.Time) error {
	writes := []func() error{
		func() error { return WriteJSONL(root, FileTeams, ex.Teams) },
		func() error { return WriteJSONL(root, FileStates, ex.States) },
		func() error { return WriteJSONL(root, FileLabels, ex.Labels) },
		func() error { return WriteJSONL(root, FileUsers, ex.Users) },
		func() error { return WriteJSONL(root, FileProjects, ex.Projects) },
		func() error { return WriteJSONL(root, FileMilestones, ex.Milestones) },
		func() error { return WriteJSONL(root, FileCycles, ex.Cycles) },
		func() error { return WriteJSONL(root, FileIssues, ex.Issues) },
		func() error { return WriteJSONL(root, FileComments, ex.Comments) },
		func() error { return WriteJSONL(root, FileDocuments, ex.Documents) },
		func() error { return WriteJSONL(root, FileRelations, ex.Relations) },
		func() error { return WriteJSONL(root, FileAttachments, ex.Attachments) },
		func() error { return WriteJSONL(root, FileFiles, ex.Files) },
	}
	for _, w := range writes {
		if err := w(); err != nil {
			return err
		}
	}
	return WriteManifest(root, NewManifest(ex, at))
}
