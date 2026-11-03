package offsite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

type strictDriver struct{}

func (strictDriver) Identity(json.RawMessage) (driver.Identity, error) { return driver.Identity{}, nil }
func (strictDriver) Probe(context.Context, driver.Config) (driver.ProbeResult, error) {
	return driver.ProbeResult{}, nil
}
func (strictDriver) Open(context.Context, driver.Config) (driver.Remote, error) { return nil, nil }

func (strictDriver) Validate(raw json.RawMessage) error {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return driver.Errf(driver.ServerError)
	}
	if f, _ := m["folder"].(string); len(f) > 60 {
		return driver.Errf(driver.ServerError)
	}
	return nil
}

func TestSubFolderConfig(t *testing.T) {
	d := strictDriver{}
	cases := []struct{ in, want string }{
		{`{"endpoint":"https://s3.example","folder":"backups"}`, `"folder":"backups/launchpad-files-a1b2c3d4"`},
		{`{"endpoint":"https://s3.example"}`, `"folder":"launchpad-files-a1b2c3d4"`},
		{`{"url":"https://dav.example","folder":"/abs/path"}`, `"folder":"/abs/path/launchpad-files-a1b2c3d4"`},
		{`{"dir":"/tmp/x"}`, `"dir":"/tmp/x/launchpad-files-a1b2c3d4"`},
	}
	for _, c := range cases {
		got, err := subFolderConfig(d, json.RawMessage(c.in), "launchpad-files-a1b2c3d4")
		if err != nil || !strings.Contains(string(got), c.want) {
			t.Errorf("%s -> %s, %v; want %s", c.in, got, err, c.want)
		}
		if strings.Contains(c.in, "endpoint") && !strings.Contains(string(got), "https://s3.example") {
			t.Errorf("other keys were lost: %s", got)
		}
	}
	if _, err := subFolderConfig(d, json.RawMessage(`not json`), "x"); err == nil {
		t.Error("junk config")
	}
	if _, err := subFolderConfig(d, json.RawMessage(`{"folder":5}`), "x"); err == nil {
		t.Error("a non-string folder")
	}
	long := `{"folder":"` + strings.Repeat("a", 50) + `"}`
	if _, err := subFolderConfig(d, json.RawMessage(long), "launchpad-files-a1b2c3d4"); err == nil {
		t.Error("the driver's own validation did not run on the new folder")
	}
}

func fileEntry(t *testing.T, key string) backup.FileEntry {
	t.Helper()
	name, _ := remotename.FilesObjectName("a1b2c3d4", key)
	return backup.FileEntry{Key: key, Size: 10, ETag: "e", Modified: "2026-10-26T03:00:00Z", SHA256: strings.Repeat("11", 32),
		Remote: name, CipherSize: 200, CipherSHA256: strings.Repeat("22", 32), LastSeen: "2026-10-26T03:15:00Z"}
}

func TestState_RoundTripAndRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "files-state-x.jsonl")
	m := map[string]backup.FileEntry{}
	for _, k := range []string{"org/a/1/x/one.png", "org/a/2/y/two.txt"} {
		m[k] = fileEntry(t, k)
	}
	if err := saveState(path, "a1b2c3d4", "cfg1", m); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v", fi.Mode())
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("the temp file stayed")
	}
	got := loadState(path, "a1b2c3d4", "cfg1")
	if len(got) != 2 || got["org/a/1/x/one.png"] != m["org/a/1/x/one.png"] {
		t.Fatalf("loaded %v", got)
	}
	// Another install, another config, a missing file and damage are all "no state".
	if loadState(path, "ffffffff", "cfg1") != nil || loadState(path, "a1b2c3d4", "cfg2") != nil || loadState(path+"x", "a1b2c3d4", "cfg1") != nil {
		t.Fatal("a wrong state was loaded")
	}
	b, _ := os.ReadFile(path)
	for name, data := range map[string][]byte{
		"truncated": b[:len(b)-7], "junk": []byte("hello\n"), "empty": nil,
		"header only junk": []byte(`{"version":9,"installId":"a1b2c3d4","config":"cfg1"}` + "\n"),
	} {
		_ = os.WriteFile(path, data, 0o600)
		if loadState(path, "a1b2c3d4", "cfg1") != nil {
			t.Errorf("%s: a damaged state was loaded", name)
		}
	}
}

// ---- the silo source against a fake S3 server ----

type fakeSilo struct {
	mu      sync.Mutex
	srv     *httptest.Server
	methods []string
	queries []url.Values
	pages   [][]string // XML <Contents> per page
	objects map[string]string
	status  int
	// getStatus fails object GETs only (a listing still works); hang blocks them until
	// the request ends; gets counts them.
	getStatus int
	hang      bool
	gets      int
}

func newFakeSilo(t *testing.T) *fakeSilo {
	f := &fakeSilo{objects: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods = append(f.methods, r.Method)
		f.queries = append(f.queries, r.URL.Query())
		if f.status != 0 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>SECRETWORDS</Message></Error>")
			return
		}
		if r.URL.Path == "/bkt" {
			page := 0
			if tok := r.URL.Query().Get("continuation-token"); tok != "" {
				fmt.Sscanf(tok, "t%d", &page)
			}
			if len(f.pages) == 0 {
				f.pages = [][]string{{}}
			}
			trunc := page+1 < len(f.pages)
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>%v</IsTruncated><NextContinuationToken>t%d</NextContinuationToken>%s</ListBucketResult>`,
				trunc, page+1, strings.Join(f.pages[page], ""))
			return
		}
		f.gets++
		if f.hang {
			f.mu.Unlock()
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		if f.getStatus != 0 {
			w.WriteHeader(f.getStatus)
			return
		}
		if body, ok := f.objects[strings.TrimPrefix(r.URL.Path, "/bkt/")]; ok {
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func contents(key string, size int) string {
	return fmt.Sprintf(`<Contents><Key>%s</Key><Size>%d</Size><ETag>&quot;abc&quot;</ETag><LastModified>2026-10-26T03:00:00.000Z</LastModified></Contents>`, key, size)
}

func siloFor(t *testing.T, f *fakeSilo) *siloSource {
	t.Helper()
	s, err := newSiloSource(SiloConfig{Endpoint: f.srv.URL, Bucket: "bkt", AccessKey: "AK", SecretKey: "SK-PLANTED"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSiloSource_ListFiltersPagesAndOnlyReads(t *testing.T) {
	f := newFakeSilo(t)
	f.pages = [][]string{
		{contents("org/o1/2026/u1/a.png", 5) + contents("other/b.png", 6) + contents("org/o1/2026/u1/dir/", 0) + contents("org/o1/ctrl&#10;x", 1)},
		{contents("org/o2/2026/u2/c.txt", 7)},
	}
	s := siloFor(t, f)
	var keys []string
	bad, err := s.list(context.Background(), func(o siloObject) error { keys = append(keys, o.Key); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "org/o1/2026/u1/a.png" || keys[1] != "org/o2/2026/u2/c.txt" || bad != 2 {
		t.Fatalf("keys %v bad %d", keys, bad)
	}
	for i, m := range f.methods {
		if m != http.MethodGet {
			t.Fatalf("request %d was a %s", i, m)
		}
		q := f.queries[i]
		if q.Get("prefix") != "org/" || q.Get("X-Amz-Signature") == "" || strings.Contains(fmt.Sprint(q), "SK-PLANTED") {
			t.Fatalf("query %v", q)
		}
	}
	// An error from fn stops the listing.
	stop := errors.New("stop")
	if _, err := s.list(context.Background(), func(siloObject) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("fn error: %v", err)
	}
}

func TestSiloSource_ErrorsAreCodesOnly(t *testing.T) {
	f := newFakeSilo(t)
	s := siloFor(t, f)
	f.status = http.StatusForbidden
	_, err := s.list(context.Background(), func(siloObject) error { return nil })
	if driver.CodeOf(err) != driver.AuthFailed || strings.Contains(err.Error(), "SECRETWORDS") || strings.Contains(err.Error(), "SK-PLANTED") {
		t.Fatalf("list error = %v", err)
	}
	f.status = 0
	if _, _, err := s.get(context.Background(), "org/o/missing"); driver.CodeOf(err) != driver.NotFound {
		t.Fatalf("get missing = %v", err)
	}
	if _, _, err := s.get(context.Background(), "other/x"); driver.CodeOf(err) != driver.NotFound {
		t.Fatalf("a key outside org/ must not be read: %v", err)
	}
	f.objects["org/o/here"] = "hello"
	rc, n, err := s.get(context.Background(), "org/o/here")
	if err != nil || n != 5 {
		t.Fatal(err, n)
	}
	rc.Close()
	f.status = http.StatusInternalServerError
	if _, _, err := s.get(context.Background(), "org/o/here"); driver.CodeOf(err) != driver.ServerError {
		t.Fatalf("500 = %v", err)
	}
}

// The source connects to its one configured host and port; any other address is
// refused before a lookup (the silo dialer, decisions-T16 #34).
func TestSiloSource_OtherHostRefused(t *testing.T) {
	f := newFakeSilo(t)
	s := siloFor(t, f)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("the other server was reached") }))
	defer other.Close()
	for _, target := range []string{other.URL + "/x", "http://example.invalid:9000/x", "http://169.254.169.254/latest"} {
		resp, err := s.hc.Get(target)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s was reached", target)
		}
	}
	if _, err := newSiloSource(SiloConfig{Endpoint: "http://silo:9000/path", Bucket: "bkt", AccessKey: "a", SecretKey: "b"}, time.Second); err == nil {
		t.Fatal("an endpoint with a path was accepted")
	}
	if _, err := newSiloSource(SiloConfig{Endpoint: "http://silo:9000", Bucket: "Bad_Bucket", AccessKey: "a", SecretKey: "b"}, time.Second); err == nil {
		t.Fatal("a bad bucket name was accepted")
	}
	if _, err := newSiloSource(SiloConfig{Endpoint: "http://silo:9000", Bucket: "bkt", AccessKey: "", SecretKey: "b"}, time.Second); err == nil {
		t.Fatal("an empty access key was accepted")
	}
}

// A listing is capped per PAGE, never in total: 60 pages of 1000 keys (about 21 MiB of
// XML in all, over the old 16 MiB total) are listed (LAUN-80 review).
func TestSiloSource_ListManyPagesIsNotCappedInTotal(t *testing.T) {
	f := newFakeSilo(t)
	pad := strings.Repeat("p", 180)
	const pages, per = 60, 1000
	var total int
	for p := 0; p < pages; p++ {
		var b strings.Builder
		for i := 0; i < per; i++ {
			b.WriteString(contents(fmt.Sprintf("org/o1/2026/u%05d/%s-%d.png", p*per+i, pad, i), 7))
		}
		total += b.Len()
		f.pages = append(f.pages, []string{b.String()})
	}
	if total <= 16<<20 {
		t.Fatalf("the fake listing is only %d bytes: it must pass the old total cap", total)
	}
	s := siloFor(t, f)
	n := 0
	bad, err := s.list(context.Background(), func(siloObject) error { n++; return nil })
	if err != nil || bad != 0 || n != pages*per {
		t.Fatalf("listed %d (bad %d) err %v", n, bad, err)
	}
}

// One page over the per-page cap is refused with too_large; the page count is capped too.
func TestSiloSource_OnePageOverTheCapIsRefused(t *testing.T) {
	f := newFakeSilo(t)
	if siloListMaxPage != 8<<20 {
		t.Fatalf("siloListMaxPage = %d: the per-page cap is pinned at 8 MiB", siloListMaxPage)
	}
	f.pages = [][]string{{contents("org/o1/ok.png", 1), strings.Repeat("x", 9<<20)}}
	_, err := siloFor(t, f).list(context.Background(), func(siloObject) error { return nil })
	if driver.CodeOf(err) != driver.TooLarge {
		t.Fatalf("an oversized page = %v", err)
	}
	f = newFakeSilo(t)
	for i := 0; i < siloListMaxPages+1; i++ {
		f.pages = append(f.pages, []string{contents(fmt.Sprintf("org/o1/k%d", i), 1)})
	}
	_, err = siloFor(t, f).list(context.Background(), func(siloObject) error { return nil })
	if driver.CodeOf(err) != driver.TooLarge {
		t.Fatalf("too many pages = %v", err)
	}
}

// The install id of a state is checked on its own: same config, entries valid for the
// file's install, only the header's install differs.
func TestLoadState_InstallIDAloneIsChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "files-state-y.jsonl")
	m := map[string]backup.FileEntry{"org/a/1/x/one.png": fileEntry(t, "org/a/1/x/one.png")}
	if err := saveState(path, "a1b2c3d4", "cfg1", m); err != nil {
		t.Fatal(err)
	}
	if loadState(path, "a1b2c3d4", "cfg1") == nil {
		t.Fatal("the matching state was not loaded")
	}
	if got := loadState(path, "a1b2c3d5", "cfg1"); got != nil {
		t.Fatalf("a state of another install was loaded: %v", got)
	}
	// An EMPTY state has no entry for the list reader to check: only the header's install id can refuse it.
	if err := saveState(path, "a1b2c3d4", "cfg1", map[string]backup.FileEntry{}); err != nil {
		t.Fatal(err)
	}
	if got := loadState(path, "a1b2c3d4", "cfg1"); got == nil || len(got) != 0 {
		t.Fatalf("an empty state = %v", got)
	}
	if got := loadState(path, "a1b2c3d5", "cfg1"); got != nil {
		t.Fatalf("an empty state of another install was loaded: %v", got)
	}
	// And the config alone.
	if got := loadState(path, "a1b2c3d4", "cfg2"); got != nil {
		t.Fatalf("a state of another config was loaded: %v", got)
	}
}
