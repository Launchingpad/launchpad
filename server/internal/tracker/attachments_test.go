package tracker

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeContentType(t *testing.T) {
	tests := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "png", in: "image/png", want: "image/png"},
		{name: "upper case", in: "IMAGE/JPEG", want: "image/jpeg"},
		{name: "charset dropped", in: "text/plain; charset=utf-8", want: "text/plain"},
		{name: "pdf", in: "application/pdf", want: "application/pdf"},
		{name: "mp4", in: "video/mp4", want: "video/mp4"},
		{name: "gif", in: "image/gif", want: "image/gif"},
		{name: "html refused", in: "text/html", wantErr: true},
		{name: "svg refused", in: "image/svg+xml", wantErr: true},
		{name: "empty refused", in: "", wantErr: true},
		{name: "garbage refused", in: "not a type", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeContentType(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeContentType(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalizeContentType(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestCheckSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int64
		ok   bool
	}{
		{name: "zero", n: 0}, {name: "negative", n: -1}, {name: "one", n: 1, ok: true},
		{name: "limit", n: MaxAttachmentBytes, ok: true}, {name: "over limit", n: MaxAttachmentBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkSize(tc.n); (err == nil) != tc.ok {
				t.Fatalf("checkSize(%d) = %v, want ok=%v", tc.n, err, tc.ok)
			}
		})
	}
}

func TestValidateFilename(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		ok       bool
	}{
		{name: "plain", in: "shot.png", ok: true},
		{name: "unicode", in: "写真 1.png", ok: true},
		{name: "empty", in: "  "},
		{name: "dot", in: "."},
		{name: "dotdot", in: ".."},
		{name: "slash", in: "a/b.png"},
		{name: "backslash", in: `a\b.png`},
		{name: "newline", in: "a\nb.png"},
		{name: "nul", in: "a\x00.png"},
		{name: "too long", in: strings.Repeat("a", 256)},
		{name: "255 runes", in: strings.Repeat("é", 255), ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateFilename(tc.in); (err == nil) != tc.ok {
				t.Fatalf("validateFilename(%q) = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

func TestKeyName(t *testing.T) {
	long := strings.Repeat("a", 150) + ".png"
	for _, tc := range []struct{ name, in, want string }{
		{name: "safe kept", in: "Shot_1-a.png", want: "Shot_1-a.png"},
		{name: "space and unicode", in: "写真 1.png", want: "___1.png"},
		{name: "leading dots trimmed", in: "..env", want: "env"},
		{name: "only unsafe", in: "???", want: "___"},
		{name: "empty becomes file", in: "...", want: "file"},
		{name: "long keeps extension", in: long, want: strings.Repeat("a", 96) + ".png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keyName(tc.in)
			if got != tc.want {
				t.Fatalf("keyName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := keyName(got); again != got {
				t.Fatalf("keyName is not idempotent: %q -> %q", got, again)
			}
			if len(got) > maxKeyNameBytes {
				t.Fatalf("keyName(%q) is %d bytes", tc.in, len(got))
			}
		})
	}
}

func TestParseUploadKey(t *testing.T) {
	org := uuid.MustParse("019a0000-0000-7000-8000-000000000001")
	other := uuid.MustParse("019a0000-0000-7000-8000-000000000002")
	id := uuid.Must(uuid.NewV7())
	v4 := uuid.New()
	good := objectKey(org, 2026, id, "shot.png")
	if good != "org/"+org.String()+"/2026/"+id.String()+"/shot.png" {
		t.Fatalf("objectKey = %q", good)
	}
	for _, tc := range []struct {
		name, key, wantErr string
	}{
		{name: "good", key: good},
		{name: "other org", key: objectKey(other, 2026, id, "shot.png"), wantErr: "does not belong to this org"},
		{name: "no prefix", key: "x/" + org.String() + "/2026/" + id.String() + "/a.png", wantErr: "not a key"},
		{name: "extra segment", key: good + "/x", wantErr: "not a key"},
		{name: "traversal", key: "org/" + org.String() + "/2026/" + id.String() + "/..", wantErr: "not a key"},
		{name: "bad year", key: "org/" + org.String() + "/26/" + id.String() + "/a.png", wantErr: "not a key"},
		{name: "uuid v4", key: objectKey(org, 2026, v4, "a.png"), wantErr: "not a key"},
		{name: "upper-case uuid", key: "org/" + org.String() + "/2026/" + strings.ToUpper(id.String()) + "/a.png", wantErr: "not a key"},
		{name: "unsafe name", key: "org/" + org.String() + "/2026/" + id.String() + "/a b.png", wantErr: "not a key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotID, name, err := parseUploadKey(org, tc.key)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseUploadKey(%q) err = %v, want %q", tc.key, err, tc.wantErr)
				}
				return
			}
			if err != nil || gotID != id || name != "shot.png" {
				t.Fatalf("parseUploadKey = %s %q %v", gotID, name, err)
			}
		})
	}
}

func TestValidateLinkURL(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		ok       bool
	}{
		{name: "https", in: "https://gitlab.example.com/example-org/x/-/merge_requests/1", ok: true},
		{name: "http", in: "http://example.com", ok: true},
		{name: "javascript", in: "javascript:alert(1)"},
		{name: "relative", in: "/a/b"},
		{name: "user info", in: "https://u:p@example.com/"},
		{name: "too long", in: "https://example.com/" + strings.Repeat("a", 2048)},
		{name: "newline", in: "https://example.com/\nx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateLinkURL(tc.in); (err == nil) != tc.ok {
				t.Fatalf("validateLinkURL(%q) = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}

func TestContentDisposition(t *testing.T) {
	for _, tc := range []struct{ name, disp, file, want string }{
		{name: "ascii", disp: "inline", file: "shot.png", want: `inline; filename="shot.png"; filename*=UTF-8''shot.png`},
		{name: "quote and backslash", disp: "attachment", file: `a"b\c.txt`, want: `attachment; filename="a_b_c.txt"; filename*=UTF-8''a%22b%5Cc.txt`},
		{name: "unicode", disp: "inline", file: "é 1.png", want: `inline; filename="_ 1.png"; filename*=UTF-8''%C3%A9%201.png`},
		{name: "semicolon cannot add a parameter", disp: "inline", file: "a; filename=x", want: `inline; filename="a; filename=x"; filename*=UTF-8''a%3B%20filename%3Dx`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentDisposition(tc.disp, tc.file); got != tc.want {
				t.Fatalf("contentDisposition = %s\n                 want %s", got, tc.want)
			}
		})
	}
}

func TestDownloadPath(t *testing.T) {
	id := uuid.MustParse("019a0000-0000-7000-8000-0000000000aa")
	s := &Service{}
	if got := s.downloadPath("orbit", id); got != "/api/v1/orgs/orbit/attachments/"+id.String()+"/download" {
		t.Fatalf("no base: %s", got)
	}
	s.SetPublicBaseURL("https://tracker.example.com/")
	if got := s.downloadPath("orbit", id); got != "https://tracker.example.com/api/v1/orgs/orbit/attachments/"+id.String()+"/download" {
		t.Fatalf("with base: %s", got)
	}
}
