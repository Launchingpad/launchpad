package s3

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
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/storage"
)

func init() { driver.Register(Type, Driver{}) }

// Limits and timeouts (T16 #40). The 5 GiB limit is the S3 limit for one PUT.
const (
	// MaxObject is the largest object Put sends: S3's limit for one request.
	MaxObject = 5 << 30
	// responseHeaderTimeout is "TLS and first byte 30 s".
	responseHeaderTimeout = 30 * time.Second
	signExpires           = time.Hour
	// errBodyMax is how much of an error reply is read to find its code.
	errBodyMax = 64 << 10
	// skewLimit is the clock difference at which a 403 is read as clock_skew.
	skewLimit = 5 * time.Minute
	maxPages  = 1000
)

// Test seams (package-level so a test can set them; never changed in
// production).
var (
	stallAfter = 120 * time.Second // "no progress for 120 s is stalled" (T16 #40)
	clockNow   = time.Now
)

// testRoots replaces the system roots in tests only (never set in production).
var testRoots *x509.CertPool

var errStalled = errors.New("s3: stalled")

type secrets struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

func parseSecrets(raw json.RawMessage) (secrets, error) {
	var s secrets
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s.AccessKey == "" || s.SecretKey == "" {
		return secrets{}, driver.Errf(driver.SecretsUnreadable)
	}
	return s, nil
}

// newClient takes the guarded transport of backup.NewHTTPClient (guarded
// dialer, no proxy, no redirect) and adds the pin and the plain-HTTP rule.
func newClient(p parsed, c driver.Config) (*http.Client, *http.Transport, error) {
	if c.Dialer == nil {
		return nil, nil, driver.Errf(driver.ServerError)
	}
	if p.scheme == "http" && !c.AllowPlainHTTP {
		return nil, nil, driver.Errf(driver.SSRFBlocked)
	}
	hc := backup.NewHTTPClient(c.Dialer, 0)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		return nil, nil, driver.Errf(driver.ServerError)
	}
	tr.ForceAttemptHTTP2 = false
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	tr.DisableCompression = true
	if p.scheme == "https" {
		tr.TLSClientConfig = tlsConfig(p)
	} else {
		tr.TLSClientConfig = nil
		tr.DialContext = plainDialer(c.Dialer.DialContext, c.Dialer.Policy.Private)
	}
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
		got := spki(leaf)
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
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

// plainDialer enforces "http:// only to an operator-listed address" at DIAL
// time, on the address the socket really connected to (T16 #34; this closes
// the rebinding gap of the pre-check in the worker). The inner dialer is the
// guarded one: it has already refused every always-refused address.
func plainDialer(inner func(context.Context, string, string) (net.Conn, error), listed []netip.Prefix) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := inner(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		ap, perr := netip.ParseAddrPort(conn.RemoteAddr().String())
		ok := false
		if perr == nil {
			a := ap.Addr().Unmap().WithZone("")
			for _, r := range listed {
				if r.Contains(a) {
					ok = true
					break
				}
			}
		}
		if !ok {
			_ = conn.Close()
			return nil, driver.Errf(driver.SSRFBlocked)
		}
		return conn, nil
	}
}

// Probe implements driver.Driver: phase 1 of T16 #36. A TCP connect through the
// guarded dialer and a TLS handshake WITHOUT verification that only records the
// leaf; nothing is sent after it. For http:// there is no handshake: the kind is
// "plain".
func (Driver) Probe(ctx context.Context, c driver.Config) (driver.ProbeResult, error) {
	p, err := parse(c.Config)
	if err != nil {
		return driver.ProbeResult{}, err
	}
	if c.Dialer == nil {
		return driver.ProbeResult{}, driver.Errf(driver.ServerError)
	}
	if p.scheme == "http" && !c.AllowPlainHTTP {
		return driver.ProbeResult{}, driver.Errf(driver.SSRFBlocked)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dial := c.Dialer.DialContext
	if p.scheme == "http" {
		dial = plainDialer(dial, c.Dialer.Policy.Private)
	}
	addr := net.JoinHostPort(p.reqHost, p.port)
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return driver.ProbeResult{}, driver.MapNetError(err)
	}
	defer conn.Close()
	if p.scheme == "http" {
		return driver.ProbeResult{Kind: "plain"}, nil
	}
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
	opts := x509.VerifyOptions{Roots: testRoots, Intermediates: inter, CurrentTime: clockNow()}
	if _, err := chain[0].Verify(opts); err != nil {
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
}

// respError marks an error that came from an HTTP reply (so the server was
// reached). It unwraps to the safe *driver.Error.
type respError struct {
	e *driver.Error
	// missingKey: the reply says the OBJECT is absent (NoSuchKey, or a 404 that
	// does not name a missing bucket). Delete swallows only this.
	missingKey bool
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

// fail turns a transport error into a safe error; a stall wins over the
// cancel it caused.
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

func (r *remote) key(name string) string {
	if r.p.Folder == "" {
		return name
	}
	return r.p.Folder + "/" + name
}

// target returns the signed URL for an object key ("" is the bucket).
func (r *remote) target(method, key string, q url.Values) string {
	host, path := r.p.host, "/"+r.p.Bucket
	if key != "" {
		path += "/" + key
	}
	if r.p.Addressing == "virtual" {
		host = r.p.Bucket + "." + r.p.host
		path = "/"
		if key != "" {
			path = "/" + key
		}
	}
	return storage.PresignURL(storage.PresignParams{
		AccessKey: r.sec.AccessKey, SecretKey: r.sec.SecretKey, Region: r.p.Region,
		Method: method, Scheme: r.p.scheme, Host: host, Path: path, Query: q,
		Now: clockNow(), Expires: signExpires,
	})
}

// do sends one signed request. On a non-2xx status it returns a *respError and
// has closed the body. The caller closes resp.Body otherwise.
func (r *remote) do(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64, w *watch) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.target(method, key, q), nil)
	if err != nil {
		return nil, driver.Errf(driver.ServerError)
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
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fail(ctx, err)
	}
	w.touch()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	e, missing := statusToError(resp)
	_ = resp.Body.Close()
	return nil, &respError{e: e, missingKey: missing}
}

type errReply struct {
	Code string `xml:"Code"`
}

// statusToError maps an error reply to a safe code. It reads at most
// errBodyMax bytes of the body to find the S3 error code and uses nothing else
// from it: no message, no resource, no request id.
func statusToError(resp *http.Response) (*driver.Error, bool) {
	var er errReply
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyMax))
		_ = xml.NewDecoder(bytes.NewReader(b)).Decode(&er)
	}
	missing := er.Code == "NoSuchKey" || (resp.StatusCode == http.StatusNotFound && er.Code != "NoSuchBucket")
	return classify(resp, er.Code), missing
}

func classify(resp *http.Response, code string) *driver.Error {
	switch code {
	case "RequestTimeTooSkewed":
		return driver.Errf(driver.ClockSkew)
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidSecurity", "AuthorizationHeaderMalformed",
		"AuthorizationQueryParametersError", "InvalidToken", "ExpiredToken":
		if code != "InvalidAccessKeyId" && skewed(resp) {
			return driver.Errf(driver.ClockSkew)
		}
		return driver.Errf(driver.AuthFailed)
	case "EntityTooLarge":
		return driver.Errf(driver.TooLarge)
	case "QuotaExceeded", "InsufficientStorage", "storage_cap_exceeded", "MaxStorageExceeded":
		return driver.Errf(driver.QuotaExceeded)
	case "NoSuchBucket", "NoSuchKey":
		return driver.Errf(driver.NotFound)
	}
	if resp.StatusCode == http.StatusForbidden {
		// RequestExpired and AccessDenied "Request has expired" are how a skewed
		// clock shows on a signed URL: the server's own Date header tells.
		if code == "RequestExpired" || skewed(resp) {
			return driver.Errf(driver.ClockSkew)
		}
		return driver.Errf(driver.Forbidden)
	}
	return driver.StatusError(resp.StatusCode)
}

func skewed(resp *http.Response) bool {
	d := resp.Header.Get("Date")
	if d == "" {
		return false
	}
	t, err := http.ParseTime(d)
	if err != nil {
		return false
	}
	diff := clockNow().Sub(t)
	if diff < 0 {
		diff = -diff
	}
	return diff > skewLimit
}

// Put implements driver.Remote: ONE PUT of exactly size bytes (T16 #40). An
// object above 5 GiB is too_large before any byte is read or sent.
func (r *remote) Put(ctx context.Context, name string, size int64, src io.Reader) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	if size < 0 || size > MaxObject {
		return driver.Errf(driver.TooLarge)
	}
	ctx, w := newWatch(ctx)
	defer w.stop()
	resp, err := r.do(ctx, http.MethodPut, r.key(name), nil, src, size, w)
	if err != nil {
		return plain(err)
	}
	_ = resp.Body.Close()
	// Verify the stored size (one HEAD): a service that took fewer bytes is a failed upload.
	hr, err := r.do(ctx, http.MethodHead, r.key(name), nil, nil, 0, w)
	if err != nil {
		return plain(err)
	}
	_ = hr.Body.Close()
	if hr.ContentLength != size {
		return driver.Errf(driver.ServerError)
	}
	return nil
}

type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
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

// List implements driver.Remote. Only names that pass remotename.Valid and start
// with prefix are returned, as bare names (the folder is removed): a key from a
// reply that does not fit is dropped, never used.
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
	folderPrefix := ""
	if r.p.Folder != "" {
		folderPrefix = r.p.Folder + "/"
	}
	var out []driver.Entry
	token := ""
	var total int64
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, driver.Errf(driver.TooLarge)
		}
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", "1000")
		q.Set("prefix", folderPrefix+prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := r.do(ctx, http.MethodGet, "", q, nil, 0, w)
		if err != nil {
			return nil, err
		}
		left := int64(driver.MaxListBody) - total
		b, rerr := io.ReadAll(io.LimitReader(&progress{r: resp.Body, w: w, ctx: ctx}, left+1))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, fail(ctx, rerr)
		}
		if int64(len(b)) > left {
			return nil, driver.Errf(driver.TooLarge)
		}
		total += int64(len(b))
		var lr listResult
		if err := xml.NewDecoder(bytes.NewReader(b)).Decode(&lr); err != nil {
			return nil, driver.Errf(driver.ServerError)
		}
		for _, c := range lr.Contents {
			if !strings.HasPrefix(c.Key, folderPrefix) {
				continue
			}
			n := c.Key[len(folderPrefix):]
			if !strings.HasPrefix(n, prefix) || !remotename.Valid(n) {
				continue
			}
			out = append(out, driver.Entry{Name: n, Size: c.Size})
		}
		if !lr.IsTruncated {
			return out, nil
		}
		token = lr.NextContinuationToken
		if token == "" || len(token) > 2048 {
			return nil, driver.Errf(driver.ServerError)
		}
	}
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
	resp, err := r.do(ctx, http.MethodGet, r.key(name), nil, nil, 0, w)
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

// Delete implements driver.Remote. A missing object is not an error.
func (r *remote) Delete(ctx context.Context, name string) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	ctx, w := newWatch(ctx)
	defer w.stop()
	resp, err := r.do(ctx, http.MethodDelete, r.key(name), nil, nil, 0, w)
	if err != nil {
		var re *respError
		if errors.As(err, &re) && re.missingKey {
			return nil // only an absent OBJECT; a missing bucket is an error
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
// signed listing (a reply of any status shows the server was reached); then
// write, read (a listing finds the file, a download matches it) and delete.
func (r *remote) Test(ctx context.Context, testName string) []driver.Step {
	var steps []driver.Step
	failAt := func(name string, err error) []driver.Step {
		return append(steps, driver.Step{Name: name, Code: driver.CodeOf(err)})
	}
	body := []byte("launchpad backup test " + strconv.FormatInt(clockNow().UnixNano(), 16))
	if _, err := r.list(ctx, "launchpad-"); err != nil {
		if !reached(err) {
			return failAt("connect", err)
		}
		steps = append(steps, driver.Step{Name: "connect", OK: true})
		return failAt("auth", err)
	}
	steps = append(steps, driver.Step{Name: "connect", OK: true}, driver.Step{Name: "auth", OK: true})
	if err := r.Put(ctx, testName, int64(len(body)), bytes.NewReader(body)); err != nil {
		return failAt("write", err)
	}
	steps = append(steps, driver.Step{Name: "write", OK: true})
	es, err := r.List(ctx, testName)
	if err != nil {
		return failAt("read", err)
	}
	found := false
	for _, e := range es {
		found = found || (e.Name == testName && e.Size == int64(len(body)))
	}
	if !found {
		return failAt("read", driver.Errf(driver.NotFound))
	}
	rc, err := r.Get(ctx, testName)
	if err != nil {
		return failAt("read", err)
	}
	got, err := io.ReadAll(driver.CapReader(rc, driver.DownloadCap(int64(len(body)))))
	_ = rc.Close()
	if err != nil {
		return failAt("read", err)
	}
	if !bytes.Equal(got, body) {
		return failAt("read", driver.Errf(driver.ServerError))
	}
	steps = append(steps, driver.Step{Name: "read", OK: true})
	if err := r.Delete(ctx, testName); err != nil {
		return failAt("delete", err)
	}
	return append(steps, driver.Step{Name: "delete", OK: true})
}
