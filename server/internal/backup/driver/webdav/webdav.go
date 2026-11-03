package webdav

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

func init() { driver.Register(Type, Driver{}) }

// Limits and timeouts (T16 #40).
const (
	// MaxObject is the largest object Put sends. WebDAV has no protocol limit
	// (the server's own limit applies); 5 GiB is what `fetch` can frame back (T16
	// ticket 4 note 17), so a larger object could be written but not restored.
	MaxObject = 5 << 30
	// responseHeaderTimeout is "TLS and first byte 30 s".
	responseHeaderTimeout = 30 * time.Second
	// expectContinueTimeout: how long a PUT waits for "100 Continue" before it
	// sends the body anyway.
	expectContinueTimeout = 5 * time.Second
	errDrain              = 4 << 10
)

// Test seams (package-level so a test can set them; never changed in production).
var (
	stallAfter = 120 * time.Second // "no progress for 120 s is stalled" (T16 #40)
	clockNow   = time.Now
)

// testRoots replaces the system roots in tests only (never set in production).
var testRoots *x509.CertPool

var errStalled = errors.New("webdav: stalled")

type secrets struct {
	Password string `json:"password"`
}

func parseSecrets(raw json.RawMessage) (secrets, error) {
	var s secrets
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s.Password == "" {
		return secrets{}, driver.Errf(driver.SecretsUnreadable)
	}
	return s, nil
}

// newClient takes the guarded transport of backup.NewHTTPClient (guarded dialer,
// no proxy, no redirect) and adds TLS 1.2+ and the pin.
func newClient(p parsed, c driver.Config) (*http.Client, *http.Transport, error) {
	if c.Dialer == nil {
		return nil, nil, driver.Errf(driver.ServerError)
	}
	hc := backup.NewHTTPClient(c.Dialer, 0)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		return nil, nil, driver.Errf(driver.ServerError)
	}
	tr.ForceAttemptHTTP2 = false
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	tr.ExpectContinueTimeout = expectContinueTimeout
	tr.DisableCompression = true
	tr.TLSClientConfig = tlsConfig(p)
	return hc, tr, nil
}

// tlsConfig is TLS 1.2 or newer. Without a pin the system roots and the host
// name decide. With a pin the chain and the name are not checked: the leaf's
// SPKI sha256 must match and the leaf must be inside its validity dates
// (T16 #36).
func tlsConfig(p parsed) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.reqHost, RootCAs: testRoots}
	if p.TLSPin == "" {
		return cfg
	}
	want := p.TLSPin
	cfg.InsecureSkipVerify = true // the pin replaces the chain check; VerifyConnection below is mandatory
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return driver.Errf(driver.TLSPinMismatch)
		}
		leaf := cs.PeerCertificates[0]
		now := clockNow()
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return driver.Errf(driver.TLSUntrusted)
		}
		if subtle.ConstantTimeCompare([]byte(spki(leaf)), []byte(want)) != 1 {
			return driver.Errf(driver.TLSPinMismatch)
		}
		return nil
	}
	return cfg
}

func spki(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// Probe implements driver.Driver: phase 1 of T16 #36. A TCP connect through the
// guarded dialer and a TLS handshake WITHOUT verification that only records the
// leaf; nothing is sent after it (no request, so no credential).
func (Driver) Probe(ctx context.Context, c driver.Config) (driver.ProbeResult, error) {
	p, err := parse(c.Config)
	if err != nil {
		return driver.ProbeResult{}, err
	}
	if c.Dialer == nil {
		return driver.ProbeResult{}, driver.Errf(driver.ServerError)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := c.Dialer.DialContext(ctx, "tcp", net.JoinHostPort(p.reqHost, p.port))
	if err != nil {
		return driver.ProbeResult{}, driver.MapNetError(err)
	}
	defer conn.Close()
	var leaf *x509.Certificate
	var chain []*x509.Certificate
	tc := tls.Client(conn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         p.reqHost,
		InsecureSkipVerify: true, // phase 1 records the chain; it sends nothing after the handshake (T16 #36)
		VerifyConnection: func(cs tls.ConnectionState) error {
			chain = cs.PeerCertificates
			if len(chain) > 0 {
				leaf = chain[0]
			}
			return nil
		},
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		return driver.ProbeResult{}, driver.MapNetError(err)
	}
	if leaf == nil {
		return driver.ProbeResult{}, driver.Errf(driver.ServerError)
	}
	return driver.ProbeResult{Kind: "tls", Fingerprint: spki(leaf), ChainVerified: verifies(chain, p.reqHost)}, nil
}

func verifies(chain []*x509.Certificate, host string) bool {
	if len(chain) == 0 {
		return false
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: testRoots, Intermediates: inter, CurrentTime: clockNow()}); err != nil {
		return false
	}
	return chain[0].VerifyHostname(host) == nil
}

// Open implements driver.Driver.
func (Driver) Open(_ context.Context, c driver.Config) (driver.Remote, error) {
	p, err := parse(c.Config)
	if err != nil {
		return nil, err
	}
	sec, err := parseSecrets(c.Secrets)
	if err != nil {
		return nil, err
	}
	hc, tr, err := newClient(p, c)
	if err != nil {
		return nil, err
	}
	return &remote{p: p, sec: sec, hc: hc, tr: tr}, nil
}

type remote struct {
	p   parsed
	sec secrets
	hc  *http.Client
	tr  *http.Transport

	mu       sync.Mutex
	folderOK bool // every folder level exists (MKCOL done)
}

// respError marks an error that came from an HTTP reply (so the server was
// reached). It unwraps to the safe *driver.Error.
type respError struct {
	e      *driver.Error
	status int
}

func (r *respError) Error() string { return r.e.Error() }
func (r *respError) Unwrap() error { return r.e }

func plain(err error) error {
	var re *respError
	if errors.As(err, &re) {
		return re.e
	}
	return err
}

func reached(err error) bool {
	var re *respError
	return errors.As(err, &re)
}

func statusOf(err error) int {
	var re *respError
	if errors.As(err, &re) {
		return re.status
	}
	return 0
}

// mapStatus is the WebDAV status mapping: 401 auth_failed, 403 forbidden, 404
// and 409 (a missing parent collection) not_found, 413 too_large, 507
// quota_exceeded, 423 (locked) and 405 server_error with the status, any 3xx
// server_error with the status (a redirect is never followed).
func mapStatus(status int) *driver.Error {
	if status == http.StatusConflict {
		return driver.Errf(driver.NotFound)
	}
	return driver.StatusError(status)
}

// watch cancels a request when it makes no progress for stallAfter.
type watch struct {
	cancel context.CancelCauseFunc
	timer  stallTimer
}

// stallTimer is the part of *time.Timer the watch uses. newStallTimer is a test
// seam (LAUN-175): production always uses a real timer, a test can drive the
// stall clock by hand so "moving is not a stall" needs no wall-clock margin.
type stallTimer interface {
	Reset(time.Duration) bool
	Stop() bool
}

var newStallTimer = func(d time.Duration, f func()) stallTimer { return time.AfterFunc(d, f) }

func newWatch(parent context.Context) (context.Context, *watch) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &watch{cancel: cancel}
	w.timer = newStallTimer(stallAfter, func() { cancel(errStalled) })
	return ctx, w
}

func (w *watch) touch() { w.timer.Reset(stallAfter) }
func (w *watch) stop()  { w.timer.Stop(); w.cancel(nil) }

// fail turns a transport error into a safe error; a stall wins over the cancel
// it caused.
func fail(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errStalled) {
		return driver.Errf(driver.Stalled)
	}
	return driver.MapNetError(err)
}

// progress reports every read to the stall watch.
type progress struct {
	r   io.Reader
	w   *watch
	ctx context.Context
}

func (p *progress) Read(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.r.Read(b)
	if n > 0 {
		p.w.touch()
	}
	return n, err
}

// urlFor builds a request URL from the configured host and base path, the folder
// and a checked file name, never from a reply. segs are appended to the base.
func (r *remote) urlFor(segs []string, collection bool) string {
	u := url.URL{Scheme: "https", Host: r.p.host, Path: r.pathFor(segs, collection)}
	return u.String()
}

func (r *remote) pathFor(segs []string, collection bool) string {
	all := make([]string, 0, len(r.p.base)+len(segs))
	all = append(all, r.p.base...)
	all = append(all, segs...)
	p := "/" + strings.Join(all, "/")
	if collection && len(all) > 0 {
		p += "/"
	}
	return p
}

func (r *remote) fileSegs(name string) []string {
	s := make([]string, 0, len(r.p.folder)+1)
	s = append(s, r.p.folder...)
	return append(s, name)
}

// do sends one request with Basic auth. On a non-2xx status it returns a
// *respError and has closed the body. The caller closes resp.Body otherwise.
func (r *remote) do(ctx context.Context, method, target string, hdr map[string]string, body io.Reader, size int64, w *watch) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, driver.Errf(driver.ServerError)
	}
	req.SetBasicAuth(r.p.Username, r.sec.Password)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			// No body is sent, so look at the source here: a byte is a source longer than 0.
			if err := driver.NoMore(body); err != nil {
				return nil, fail(ctx, err)
			}
			req.Body = http.NoBody
		} else {
			req.Body = io.NopCloser(&progress{r: driver.Sized(body, size), w: w, ctx: ctx})
		}
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fail(ctx, err)
	}
	w.touch()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	// The reply text is never used: only the status maps to a code.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errDrain))
	_ = resp.Body.Close()
	return nil, &respError{e: mapStatus(resp.StatusCode), status: resp.StatusCode}
}

// ensureFolder makes every folder level with MKCOL (405 means it exists).
func (r *remote) ensureFolder(ctx context.Context, w *watch) error {
	r.mu.Lock()
	ok := r.folderOK
	r.mu.Unlock()
	if ok || len(r.p.folder) == 0 {
		return nil
	}
	for i := 1; i <= len(r.p.folder); i++ {
		resp, err := r.do(ctx, "MKCOL", r.urlFor(r.p.folder[:i], true), nil, nil, 0, w)
		if err != nil {
			if statusOf(err) == http.StatusMethodNotAllowed {
				continue // already exists
			}
			return err
		}
		_ = resp.Body.Close()
	}
	r.mu.Lock()
	r.folderOK = true
	r.mu.Unlock()
	return nil
}

// Put implements driver.Remote: ONE PUT of exactly size bytes. The folder is made
// first. After the PUT, a PROPFIND depth 0 compares the stored size.
func (r *remote) Put(ctx context.Context, name string, size int64, src io.Reader) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	if size < 0 || size > MaxObject {
		return driver.Errf(driver.TooLarge)
	}
	ctx, w := newWatch(ctx)
	defer w.stop()
	if err := r.ensureFolder(ctx, w); err != nil {
		return plain(err)
	}
	target := r.urlFor(r.fileSegs(name), false)
	hdr := map[string]string{"Content-Type": "application/octet-stream"}
	if size > 0 {
		hdr["Expect"] = "100-continue" // an early 401, 413 or 507 stops the body
	}
	resp, err := r.do(ctx, http.MethodPut, target, hdr, src, size, w)
	if err != nil {
		return plain(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return driver.ServerErr(resp.StatusCode)
	}
	items, err := r.propfind(ctx, target, "0", w)
	if err != nil {
		return plain(err)
	}
	if len(items) != 1 || items[0].dir || items[0].size != size {
		return driver.Errf(driver.ServerError)
	}
	return nil
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/><d:resourcetype/></d:prop></d:propfind>`

type multistatus struct {
	XMLName   xml.Name   `xml:"DAV: multistatus"`
	Responses []response `xml:"DAV: response"`
}

type response struct {
	Href     []string   `xml:"DAV: href"`
	Propstat []propstat `xml:"DAV: propstat"`
}

type propstat struct {
	Status string `xml:"DAV: status"`
	Prop   struct {
		Len  string `xml:"DAV: getcontentlength"`
		Type struct {
			Collection *struct{} `xml:"DAV: collection"`
		} `xml:"DAV: resourcetype"`
	} `xml:"DAV: prop"`
}

type item struct {
	href string
	size int64 // -1 when the server gave none
	dir  bool
}

// propfind sends PROPFIND with Depth depth and parses the 207 reply. The body is
// read through the 16 MiB cap and refused when it holds a DOCTYPE or an ENTITY
// declaration (Go's decoder expands no entity, so this is a second line).
func (r *remote) propfind(ctx context.Context, target, depth string, w *watch) ([]item, error) {
	resp, err := r.do(ctx, "PROPFIND", target, map[string]string{"Depth": depth, "Content-Type": "application/xml; charset=utf-8"},
		strings.NewReader(propfindBody), int64(len(propfindBody)), w)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, driver.ServerErr(resp.StatusCode)
	}
	b, err := driver.ReadCapped(&progress{r: resp.Body, w: w, ctx: ctx}, driver.MaxListBody)
	if err != nil {
		return nil, fail(ctx, err)
	}
	if lb := bytes.ToLower(b); bytes.Contains(lb, []byte("<!doctype")) || bytes.Contains(lb, []byte("<!entity")) {
		return nil, driver.Errf(driver.ServerError)
	}
	var ms multistatus
	if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&ms); err != nil {
		return nil, driver.Errf(driver.ServerError)
	}
	var out []item
	for _, rs := range ms.Responses {
		if len(rs.Href) != 1 {
			continue
		}
		it := item{href: rs.Href[0], size: -1}
		for _, ps := range rs.Propstat {
			if f := strings.Fields(ps.Status); ps.Status != "" && (len(f) < 2 || f[1] != "200") {
				continue
			}
			if ps.Prop.Type.Collection != nil {
				it.dir = true
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.Len), 10, 64); err == nil && n >= 0 {
				it.size = n
			}
		}
		out = append(out, it)
	}
	return out, nil
}

var prefixOK = func(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return false
		}
	}
	return true
}

// nameFromHref returns the bare file name a PROPFIND href stands for, or false.
// An href is data from the remote: it is accepted only when it is a path (or an
// https URL of THIS host) whose decoded form is exactly the folder's collection
// path plus ONE segment, with no encoded slash, and that segment passes
// remotename.Valid. Nothing else is used; the name is later re-joined to the
// configured base by urlFor, so no request goes to an href.
func (r *remote) nameFromHref(href, collection string) (string, bool) {
	low := strings.ToLower(href)
	if href == "" || len(href) > 2048 || strings.ContainsAny(href, "\\\x00\r\n\t") ||
		strings.Contains(low, "%2f") || strings.Contains(low, "%5c") || strings.Contains(low, "%00") {
		return "", false
	}
	u, err := url.Parse(href)
	if err != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	if u.Scheme != "" || u.Host != "" {
		port := u.Port()
		if port == "" {
			port = "443"
		}
		if u.Scheme != "https" || canonHost(strings.ToLower(u.Hostname()), port) != r.p.host {
			return "", false
		}
	}
	if !strings.HasPrefix(u.Path, collection) {
		return "", false
	}
	rest := u.Path[len(collection):]
	if rest == "" || strings.Contains(rest, "/") || !remotename.Valid(rest) {
		return "", false
	}
	return rest, true
}

// List implements driver.Remote. Only entries that are files, whose href maps to
// a name that passes nameFromHref and starts with prefix, are returned, as bare
// names. A folder that does not exist yet lists as empty.
func (r *remote) List(ctx context.Context, prefix string) ([]driver.Entry, error) {
	es, err := r.list(ctx, prefix)
	return es, plain(err)
}

func (r *remote) list(ctx context.Context, prefix string) ([]driver.Entry, error) {
	if !prefixOK(prefix) || len(prefix) > 128 {
		return nil, driver.Errf(driver.NotFound)
	}
	ctx, w := newWatch(ctx)
	defer w.stop()
	items, err := r.propfind(ctx, r.urlFor(r.p.folder, true), "1", w)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	collection := r.pathFor(r.p.folder, true)
	var out []driver.Entry
	seen := map[string]bool{}
	for _, it := range items {
		if it.dir {
			continue
		}
		n, ok := r.nameFromHref(it.href, collection)
		if !ok || !strings.HasPrefix(n, prefix) || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, driver.Entry{Name: n, Size: it.size})
	}
	return out, nil
}

type body struct {
	rc  io.ReadCloser
	w   *watch
	ctx context.Context
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.w.touch()
	}
	if err != nil && err != io.EOF {
		return n, fail(b.ctx, err)
	}
	return n, err
}

func (b *body) Close() error {
	err := b.rc.Close()
	b.w.stop()
	return err
}

// Get implements driver.Remote. The caller caps the reader (driver.CapReader).
func (r *remote) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := driver.CheckName(name); err != nil {
		return nil, err
	}
	ctx, w := newWatch(ctx)
	resp, err := r.do(ctx, http.MethodGet, r.urlFor(r.fileSegs(name), false), nil, nil, 0, w)
	if err != nil {
		w.stop()
		return nil, plain(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		w.stop()
		return nil, driver.ServerErr(resp.StatusCode)
	}
	return &body{rc: resp.Body, w: w, ctx: ctx}, nil
}

// Delete implements driver.Remote. A missing file (404) is not an error.
func (r *remote) Delete(ctx context.Context, name string) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	ctx, w := newWatch(ctx)
	defer w.stop()
	resp, err := r.do(ctx, http.MethodDelete, r.urlFor(r.fileSegs(name), false), nil, nil, 0, w)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil
		}
		return plain(err)
	}
	_ = resp.Body.Close()
	return nil
}

// Close implements driver.Remote.
func (r *remote) Close() error {
	r.tr.CloseIdleConnections()
	return nil
}

// Test implements driver.Remote: T16 #36 phase 2. connect and auth are one
// PROPFIND depth 0 on the base (a reply of any status shows the server was
// reached); then write (which makes the folder), read (a listing finds the file
// with its size, a download matches it) and delete.
func (r *remote) Test(ctx context.Context, testName string) []driver.Step {
	var steps []driver.Step
	failAt := func(name string, err error) []driver.Step {
		return append(steps, driver.Step{Name: name, Code: driver.CodeOf(err)})
	}
	data := []byte("launchpad backup test " + strconv.FormatInt(clockNow().UnixNano(), 16))
	{
		c, w := newWatch(ctx)
		resp, err := r.do(c, "PROPFIND", r.urlFor(nil, true), map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"},
			strings.NewReader(propfindBody), int64(len(propfindBody)), w)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errDrain))
			_ = resp.Body.Close()
		}
		w.stop()
		if err != nil {
			if !reached(err) {
				return failAt("connect", err)
			}
			steps = append(steps, driver.Step{Name: "connect", OK: true})
			return failAt("auth", err)
		}
	}
	steps = append(steps, driver.Step{Name: "connect", OK: true}, driver.Step{Name: "auth", OK: true})
	if err := r.Put(ctx, testName, int64(len(data)), bytes.NewReader(data)); err != nil {
		return failAt("write", err)
	}
	steps = append(steps, driver.Step{Name: "write", OK: true})
	es, err := r.List(ctx, testName)
	if err != nil {
		return failAt("read", err)
	}
	found := false
	for _, e := range es {
		found = found || (e.Name == testName && e.Size == int64(len(data)))
	}
	if !found {
		return failAt("read", driver.Errf(driver.NotFound))
	}
	rc, err := r.Get(ctx, testName)
	if err != nil {
		return failAt("read", err)
	}
	got, err := io.ReadAll(driver.CapReader(rc, driver.DownloadCap(int64(len(data)))))
	_ = rc.Close()
	if err != nil {
		return failAt("read", err)
	}
	if !bytes.Equal(got, data) {
		return failAt("read", driver.Errf(driver.ServerError))
	}
	steps = append(steps, driver.Step{Name: "read", OK: true})
	if err := r.Delete(ctx, testName); err != nil {
		return failAt("delete", err)
	}
	return append(steps, driver.Step{Name: "delete", OK: true})
}
