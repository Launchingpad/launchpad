package importer

import (
	"slices"
	"testing"
)

func TestUploadURLs(t *testing.T) {
	text := "img ![a](https://uploads.linear.app/o/1/2) and <img src=\"https://uploads.linear.app/o/3/4\"> " +
		"bare https://uploads.linear.app/o/1/2 [l](https://uploads.linear.app/o/5/6?x=1) " +
		"not https://linear.app/orbit/issue/ORB-1, but http://uploads.linear.app/o/7/8 too"
	got := UploadURLs(text, "", "https://uploads.linear.app/o/9/9")
	want := []string{
		"https://uploads.linear.app/o/1/2", "https://uploads.linear.app/o/3/4",
		"https://uploads.linear.app/o/5/6?x=1", "https://uploads.linear.app/o/7/8", "https://uploads.linear.app/o/9/9",
	}
	if !slices.Equal(got, want) {
		t.Errorf("UploadURLs = %v, want %v", got, want)
	}
}

func TestIsUploadURL(t *testing.T) {
	tests := map[string]bool{
		"https://uploads.linear.app/o/1/2":          true,
		" https://uploads.linear.app/o/1/2":         false,
		"https://uploads.linear.app/o/1/2 and more": false,
		"https://gitlab.example/mr/1":               false,
		"":                                          false,
	}
	for in, want := range tests {
		if got := IsUploadURL(in); got != want {
			t.Errorf("IsUploadURL(%q) = %v", in, got)
		}
	}
}

func TestRewriteUploads(t *testing.T) {
	known := "https://uploads.linear.app/o/1/2"
	text := "![a](" + known + ") and ![b](https://uploads.linear.app/o/3/4) and ![c](" + known + ")"
	got, pending := RewriteUploads(text, func(u string) (string, bool) {
		if u == known {
			return "/api/v1/orgs/orbit/attachments/x/download", true
		}
		return "", false
	})
	want := "![a](/api/v1/orgs/orbit/attachments/x/download) and ![b](https://uploads.linear.app/o/3/4) and " +
		"![c](/api/v1/orgs/orbit/attachments/x/download)"
	if got != want || pending != 1 {
		t.Errorf("RewriteUploads = %q, %d pending", got, pending)
	}
}

func TestFileResolver(t *testing.T) {
	r := newFileResolver()
	r.byURL[fxUploadA] = "/api/v1/orgs/orbit/attachments/x/download"
	in := "see ![x](" + fxUploadA + ") and [y](" + fxUploadC + ")"
	got := r.rewrite(t.Context(), in, Owner{Entity: EntIssue})
	if got != "see ![x](/api/v1/orgs/orbit/attachments/x/download) and [y]("+fxUploadC+")" {
		t.Errorf("rewrite = %q", got)
	}
	if !r.pending[fxUploadC] || len(r.pending) != 1 {
		t.Errorf("pending %v, want only the unstored file", r.pending)
	}
}

func TestUploadURLs_oneDefinition(t *testing.T) {
	text := "a http://uploads.linear.app/o/1/2 b HTTPS://Uploads.Linear.App/o/1/2 c https://uploads.linear.app/O/9 " +
		"prose: uploads.linear.app/x, https://uploads.linear.app/<org>/<id>, `https://uploads.linear.app/`"
	got := UploadURLs(text)
	want := []string{"https://uploads.linear.app/O/9", "https://uploads.linear.app/o/1/2"}
	if !slices.Equal(got, want) {
		t.Errorf("UploadURLs = %v, want %v (canonical scheme and host, path kept)", got, want)
	}
	out, pending := RewriteUploads("![x](HTTP://UPLOADS.linear.app/o/1/2)", func(u string) (string, bool) {
		return "/dl", u == "https://uploads.linear.app/o/1/2"
	})
	if out != "![x](/dl)" || pending != 0 {
		t.Errorf("RewriteUploads = %q, %d", out, pending)
	}
	if !IsUploadURL("http://UPLOADS.LINEAR.APP/a") || IsUploadURL("https://uploads.linear.app/") {
		t.Error("IsUploadURL disagrees with the definition")
	}
}
