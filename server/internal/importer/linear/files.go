package linear

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"devops/internal/importer"
)

// UploadsHost is the only host files are downloaded from, and the only
// host other than the API that ever sees the key.
const UploadsHost = "uploads.linear.app"

// ErrTotalLimit is the File.Error of a file DownloadAll skipped because the
// run reached its total download limit. The web export (decisions-T17 #39)
// fails the job with no_space when it finds this text in the manifest.
const ErrTotalLimit = "skipped: total download limit reached"

// Downloader fetches uploads.linear.app files into an export's files/
// directory.
type Downloader struct {
	key   string
	http  *http.Client
	log   *slog.Logger
	sleep func(context.Context, time.Duration) error
	// host is UploadsHost; tests point it at an httptest server.
	host   string
	scheme string
	// Pause between downloads, to stay gentle on Linear's CDN.
	Pause time.Duration
	// Progress, when set, gets the bytes downloaded so far and the byte
	// limit of the run after each file (decisions-T17 #40).
	Progress func(bytes, limit int64)
}

// NewDownloader makes a downloader that authenticates with key.
//
// key is a string, and a Go string cannot be wiped (decisions-T17 #38): the
// web export holds this one copy, and the client's, for the life of the
// export job and leaves them to the garbage collector. Changing it to []byte
// would not help: net/http keeps the header value as a string for every
// request. The key is never in a log line, an error, a row or an event.
//
// DownloadAll follows a redirect to Linear's signed storage without
// netguard: the target is Linear's choice, the key is dropped for any host
// but uploads.linear.app (CheckRedirect), and the risk is accepted.
func NewDownloader(key string, log *slog.Logger) (*Downloader, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrNoKey
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &Downloader{key: strings.TrimSpace(key), log: log, sleep: sleepCtx, host: UploadsHost, scheme: "https",
		Pause: 100 * time.Millisecond}
	d.http = &http.Client{
		Timeout: 5 * time.Minute,
		// Linear may redirect to signed storage. The key must never follow
		// a redirect off the uploads host: net/http only drops Authorization
		// for another HOSTNAME (it keeps it for another port), so it is
		// removed here for any other host:port. Only https is followed.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != d.scheme {
				return fmt.Errorf("refusing a redirect to a %s URL", req.URL.Scheme)
			}
			if req.URL.Host != d.host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	return d, nil
}

// allowed reports whether u is a file URL this downloader may fetch.
func (d *Downloader) allowed(u *url.URL) bool {
	return u.Scheme == d.scheme && u.Host == d.host && u.User == nil
}

// DownloadAll fetches every URL into root/files/<hash>/<filename>. A file
// already there from an earlier export (same URL, same size) is reused. A
// failed download is recorded in the manifest with its error, not fatal:
// the import then keeps that URL as it is. It stops when maxTotal bytes
// have been written.
func (d *Downloader) DownloadAll(ctx context.Context, root *os.Root, urls []string, maxFile, maxTotal int64) ([]importer.File, error) {
	if maxFile <= 0 {
		maxFile = 100 << 20
	}
	if maxTotal <= 0 {
		maxTotal = 10 << 30
	}
	if err := root.MkdirAll("files", 0o700); err != nil {
		return nil, fmt.Errorf("create files dir: %w", err)
	}
	prev := map[string]importer.File{}
	if old, err := importer.ReadJSONL[importer.File](root, importer.FileFiles); err == nil {
		for _, f := range old {
			prev[f.URL] = f
		}
	}
	out := make([]importer.File, 0, len(urls))
	var total int64
	failed := 0
	for i, raw := range urls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f, ok := prev[raw]; ok && f.Path != "" {
			if st, err := root.Stat(f.Path); err == nil && st.Size() == f.SizeBytes {
				out = append(out, f)
				total += f.SizeBytes
				continue
			}
		}
		f := importer.File{URL: raw}
		if total >= maxTotal {
			f.Error = ErrTotalLimit
		} else if err := d.fetch(ctx, root, &f, maxFile); err != nil {
			f.Error = err.Error()
		}
		if f.Error != "" {
			failed++
		} else {
			total += f.SizeBytes
		}
		out = append(out, f)
		if d.Progress != nil {
			d.Progress(total, maxTotal)
		}
		if (i+1)%100 == 0 {
			d.log.Info("export: files", "done", i+1, "of", len(urls), "failed", failed, "bytes", total)
		}
		if err := d.sleep(ctx, d.Pause); err != nil {
			return nil, err
		}
	}
	d.log.Info("export: files done", "files", len(urls), "failed", failed, "bytes", total)
	return out, nil
}

// fetch downloads one file. Errors name the status only, never a header.
func (d *Downloader) fetch(ctx context.Context, root *os.Root, f *importer.File, maxFile int64) error {
	u, err := url.Parse(f.URL)
	if err != nil || !d.allowed(u) {
		return errors.New("not an uploads.linear.app https URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", d.key)
	resp, err := d.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("download failed: %v", ue.Err)
		}
		return errors.New("download failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: status %d", resp.StatusCode)
	}
	if resp.ContentLength > maxFile {
		return fmt.Errorf("file is %d bytes, over the %d limit", resp.ContentLength, maxFile)
	}

	sum := sha256.Sum256([]byte(f.URL))
	dir := path.Join("files", hex.EncodeToString(sum[:8]))
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create file dir: %w", err)
	}
	name := SafeFilename(filenameOf(resp, u))
	rel := path.Join(dir, name)
	tmp := rel + ".part"
	out, err := root.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxFile+1))
	cerr := out.Close()
	if err != nil || cerr != nil {
		_ = root.Remove(tmp)
		return errors.Join(fmt.Errorf("write file: %w", err), cerr)
	}
	if n > maxFile {
		_ = root.Remove(tmp)
		return fmt.Errorf("file is over the %d byte limit", maxFile)
	}
	if err := root.Rename(tmp, rel); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	f.Path, f.Filename, f.ContentType, f.SizeBytes, f.SHA256 = rel, name, ct, n, hex.EncodeToString(h.Sum(nil))
	return nil
}

func filenameOf(resp *http.Response, u *url.URL) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		return params["filename"]
	}
	return path.Base(u.Path)
}

// SafeFilename keeps [A-Za-z0-9._-] of name (anything else becomes '_'),
// at most 100 bytes, never "." or "..", never empty. The result is a
// single path element, so it cannot leave its directory; os.Root is the
// second line of defence.
func SafeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 100 {
			break
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "file"
	}
	return out
}
