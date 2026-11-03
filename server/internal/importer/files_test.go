package importer

import (
	"testing"
	"time"
)

func TestFileContentType(t *testing.T) {
	tests := []struct {
		name string
		f    File
		want string
		ok   bool
	}{
		{"allowed as is", File{ContentType: "image/png", Filename: "a.png"}, "image/png", true},
		{"parameters dropped", File{ContentType: "text/plain; charset=utf-8", Filename: "a.txt"}, "text/plain", true},
		{"csv is plain text", File{ContentType: "text/csv", Filename: "a.csv"}, "text/plain", true},
		{"markdown is plain text", File{ContentType: "text/markdown", Filename: "a.md"}, "text/plain", true},
		{"by extension", File{ContentType: "application/octet-stream", Filename: "shot.JPG"}, "image/jpeg", true},
		{"no type: by extension", File{ContentType: "", Filename: "shot.png"}, "image/png", true},
		{"a declared svg named .png is refused", File{ContentType: "image/svg+xml", Filename: "x.png"}, "", false},
		{"svg is refused", File{ContentType: "image/svg+xml", Filename: "a.svg"}, "", false},
		{"unknown is refused", File{ContentType: "application/zip", Filename: "a.zip"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := fileContentType(tt.f)
			if got != tt.want || ok != tt.ok {
				t.Errorf("fileContentType = %q, %v", got, ok)
			}
		})
	}
}

func TestSeededV7(t *testing.T) {
	at := ts("2026-09-02T08:10:00Z")
	a, err := seededV7(at, "org:file:x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := seededV7(at, "org:file:x")
	c, _ := seededV7(at, "org:file:y")
	if a != b || a == c || a.Version() != 7 || !V7Time(a).Equal(at) {
		t.Errorf("seeded ids %s %s %s", a, b, c)
	}
	if d, _ := seededV7(at.Add(time.Second), "org:file:x"); d == a {
		t.Error("another time must give another id")
	}
}

func TestBuildPlan_fileDisplayName(t *testing.T) {
	tests := map[string]string{
		"ok.png":          "ok.png",
		"bad\x01name.png": "file.png",
		"a/b.png":         "file.png",
		"x.p\x02g":        "file",
		"..":              "file",
	}
	for in, want := range tests {
		ex := fixtureExport()
		ex.Files[0].Filename = in
		p, err := BuildPlan(ex, fixtureUserMap(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Files[0].Filename; got != want {
			t.Errorf("display name of %q = %q, want %q", in, got, want)
		}
	}
}

func TestBuildPlan_fileOnlyADocumentCommentRefersTo(t *testing.T) {
	ex := fixtureExport()
	ex.Comments[5].Body = "see " + fxUploadB // lin-cm6, on the document
	ex.Files = append(ex.Files, File{URL: fxUploadB, Path: "files/bb/b.png", Filename: "b.png", ContentType: "image/png", SizeBytes: 3})
	p, err := BuildPlan(ex, fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reasons(p)[EntFile+" "+fxUploadB]; got != "only a document (or a comment on one), a project or a milestone refers to it (no issue for its row)" {
		t.Errorf("reason %q", got)
	}
}

func TestBuildPlan_files(t *testing.T) {
	p, err := BuildPlan(fixtureExport(), fixtureUserMap(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("%d files", len(p.Files))
	}
	a, c := p.Files[0], p.Files[1]
	if a.AttachmentID != "lin-a2" || a.IssueID != fxI1 || p.FileByAttachment["lin-a2"] != 0 {
		t.Errorf("the upload attachment must be the file row: %+v", a)
	}
	if c.AttachmentID != "" || c.IssueID != fxI3 || c.ContentType != "text/plain" || !c.At.Equal(ts("2026-10-21T10:00:00Z")) {
		t.Errorf("the text-only CSV: %+v", c)
	}
	p.WithoutFiles()
	if len(p.Files) != 0 || p.Expected[EntFile] != 0 || !p.SkipFiles {
		t.Error("WithoutFiles must drop every file")
	}
}
