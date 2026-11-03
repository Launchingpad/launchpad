package linear

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"devops/internal/importer"
)

func TestSafeFilename(t *testing.T) {
	tests := map[string]string{
		"shot.png":               "shot.png",
		"../../etc/passwd":       "passwd",
		"..":                     "file",
		"":                       "file",
		"a b/c?.png":             "c_.png",
		`C:\Users\x\evil.exe`:    "evil.exe",
		"ünï.txt":                "_n_.txt",
		strings.Repeat("a", 300): strings.Repeat("a", 100),
		".hidden":                "hidden",
	}
	for in, want := range tests {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func newTestDownloader(t *testing.T, h http.HandlerFunc) (*Downloader, string, *os.Root) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	d, err := NewDownloader(testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	d.host = u.Host
	client := srv.Client()
	client.CheckRedirect = d.http.CheckRedirect
	d.http = client
	d.sleep = func(context.Context, time.Duration) error { return nil }
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return d, srv.URL, root
}

func TestDownloadAll(t *testing.T) {
	d, base, root := newTestDownloader(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/o/1/2":
			w.Header().Set("Content-Type", "image/png; charset=binary")
			w.Header().Set("Content-Disposition", `attachment; filename="../screen shot.png"`)
			_, _ = w.Write([]byte("PNG!"))
		case "/o/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 64)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	urls := []string{base + "/o/1/2", base + "/o/big", base + "/o/missing", "https://evil.example/o/1/2"}
	files, err := d.DownloadAll(t.Context(), root, urls, 16, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("%d files", len(files))
	}
	ok := files[0]
	if ok.Error != "" || ok.Filename != "screen_shot.png" || ok.ContentType != "image/png" || ok.SizeBytes != 4 || ok.SHA256 == "" {
		t.Errorf("downloaded file %+v", ok)
	}
	b, err := root.ReadFile(ok.Path)
	if err != nil || string(b) != "PNG!" {
		t.Errorf("file content %q, %v", b, err)
	}
	if st, _ := root.Stat(ok.Path); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", st.Mode().Perm())
	}
	if !strings.Contains(files[1].Error, "limit") {
		t.Errorf("an over-limit file: %+v", files[1])
	}
	if !strings.Contains(files[2].Error, "status 404") {
		t.Errorf("a missing file: %+v", files[2])
	}
	if !strings.Contains(files[3].Error, "not an uploads.linear.app") {
		t.Errorf("another host must be refused before any request: %+v", files[3])
	}
	for _, f := range files {
		if strings.Contains(f.Error, testKey) {
			t.Errorf("an error carries the key: %q", f.Error)
		}
	}

	// A second export reuses the file instead of downloading it again.
	if err := importer.WriteJSONL(root, importer.FileFiles, files); err != nil {
		t.Fatal(err)
	}
	d.host = "unreachable.invalid"
	again, err := d.DownloadAll(t.Context(), root, urls[:1], 16, 0)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Error != "" || again[0].Path != ok.Path {
		t.Errorf("the earlier download was not reused: %+v", again[0])
	}
}

func TestDownloader_redirectToAnotherHostDropsTheKey(t *testing.T) {
	var gotAuth = "unset"
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(other.Close)
	d, base, root := newTestDownloader(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/signed", http.StatusFound)
	})
	// Both test servers share one CA, so the redirect target is trusted.
	files, err := d.DownloadAll(t.Context(), root, []string{base + "/o/1/2"}, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Error != "" {
		t.Fatalf("download failed: %s", files[0].Error)
	}
	// Both servers are 127.0.0.1 on different ports. net/http alone would
	// forward the header (same hostname); CheckRedirect must remove it.
	if gotAuth != "" {
		t.Errorf("the redirect target received Authorization %q", gotAuth)
	}
}
