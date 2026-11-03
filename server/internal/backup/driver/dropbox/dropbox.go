package dropbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
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

// Limits and timeouts. The numbers are from Dropbox's documentation and T16
// #40; none was measured against a real account.
const (
	// MaxObject is the largest object Put sends: Dropbox's limit for an upload
	// session (350 GB, documented). A larger object is too_large before any byte
	// is read.
	MaxObject = 350 << 30
	// maxCall is the documented limit of ONE content request (150 MB). The
	// chunk size stays below it; a test proves no request is larger.
	maxCall = 150_000_000
	// rpcHeaderTimeout is "TLS and first byte 30 s" (T16 #40) for the API host.
	rpcHeaderTimeout = 30 * time.Second
	// contentHeaderTimeout is longer: after the last byte of a chunk Dropbox
	// assembles it before it answers (not measured).
	contentHeaderTimeout = 120 * time.Second
	errBodyMax           = 64 << 10
	smallReplyMax        = 64 << 10
	maxPages             = 1000
	maxCursorLen         = 4096
	// maxRateWaits is how many 429 answers one call waits through.
	maxRateWaits = 6
	// maxRetryAfter caps one wait; minRetryAfter is the floor.
	maxRetryAfter = 5 * time.Minute
	minRetryAfter = time.Second
	// transientTries is how often an upload chunk is sent when the failure is
	// one T16 #31 lists as transient (timeout, connect, dns, stalled, 5xx).
	transientTries = 3
	// hashBlock is the block size of Dropbox's content_hash.
	hashBlock = 4 << 20
	// tokenSlack is how long before expiry an access token is replaced.
	tokenSlack = 10 * time.Minute
)

// Test seams (package-level so a test can set them; never changed in
// production).
var (
	// apiBase and contentBase are Dropbox's two fixed hosts. A test points them
	// at httptest TLS servers; there is no config field for them.
	apiBase     = "https://api.dropboxapi.com"
	contentBase = "https://content.dropboxapi.com"
	stallAfter  = 120 * time.Second // "no progress for 120 s is stalled" (T16 #40)
	clockNow    = time.Now
	// chunkSize is the size of one upload chunk and of the biggest object sent in
	// ONE call. A multiple of 4 MiB (Dropbox's advice). The buffer is the only
	// object-sized memory the driver holds.
	chunkSize int64 = 32 << 20
	sleep           = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// testRoots replaces the system roots in tests only (never set in production).
var testRoots *x509.CertPool

var errStalled = errors.New("dropbox: stalled")

type secrets struct {
	AppKey       string `json:"appKey"`
	AppSecret    string `json:"appSecret"`
	RefreshToken string `json:"refreshToken"`
}

// printable reports a non-empty string of visible ASCII with no space (a token
// or code that is safe in a header and a form).
func printable(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func parseSecrets(raw json.RawMessage, needRefresh bool) (secrets, error) {
	var s secrets
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || !printable(s.AppKey, 512) || !printable(s.AppSecret, 512) ||
		(needRefresh && !printable(s.RefreshToken, 4096)) {
		return secrets{}, driver.Errf(driver.SecretsUnreadable)
	}
	return s, nil
}

// endpoint is a Dropbox base URL taken apart.
type endpoint struct {
	host string // host name (TLS server name)
	port string
}

func splitBase(base string) (endpoint, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return endpoint{}, driver.Errf(driver.ServerError) // TLS only
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return endpoint{host: strings.ToLower(u.Hostname()), port: port}, nil
}

// tlsConfig is TLS 1.2 or newer. Without a pin the system roots and the host
// name decide. With a pin the chain and the name are not checked: the leaf's
// SPKI sha256 must match and the leaf must be inside its validity dates.
func tlsConfig(p parsed, host string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: testRoots}
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

// client builds one guarded HTTP client (backup.NewHTTPClient: guarded dialer,
// no proxy, no redirect) with this driver's TLS rules.
func client(p parsed, c driver.Config, host string, header time.Duration) (*http.Client, *http.Transport, error) {
	if c.Dialer == nil {
		return nil, nil, driver.Errf(driver.ServerError)
	}
	hc := backup.NewHTTPClient(c.Dialer, 0)
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		return nil, nil, driver.Errf(driver.ServerError)
	}
	tr.ForceAttemptHTTP2 = false
	tr.ResponseHeaderTimeout = header
	tr.DisableCompression = true
	tr.TLSClientConfig = tlsConfig(p, host)
	return hc, tr, nil
}

// Probe implements driver.Driver: phase 1 of T16 #36 against the API host. A TCP
// connect through the guarded dialer and a TLS handshake WITHOUT verification
// that only records the leaf; nothing is sent after it.
func (Driver) Probe(ctx context.Context, c driver.Config) (driver.ProbeResult, error) {
	if _, err := parse(c.Config); err != nil {
		return driver.ProbeResult{}, err
	}
	ep, err := splitBase(apiBase)
	if err != nil {
		return driver.ProbeResult{}, err
	}
	if c.Dialer == nil {
		return driver.ProbeResult{}, driver.Errf(driver.ServerError)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := c.Dialer.DialContext(ctx, "tcp", net.JoinHostPort(ep.host, ep.port))
	if err != nil {
		return driver.ProbeResult{}, driver.MapNetError(err)
	}
	defer conn.Close()
	var leaf *x509.Certificate
	var chain []*x509.Certificate
	tc := tls.Client(conn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         ep.host,
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
	return driver.ProbeResult{Kind: "tls", Fingerprint: spki(leaf), ChainVerified: verifies(chain, ep.host)}, nil
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

// Open implements driver.Driver. All three secrets are required; the refresh
// token is written by the exchange run only.
func (Driver) Open(_ context.Context, c driver.Config) (driver.Remote, error) {
	p, err := parse(c.Config)
	if err != nil {
		return nil, err
	}
	sec, err := parseSecrets(c.Secrets, true)
	if err != nil {
		return nil, err
	}
	return newRemote(p, sec, c)
}

func newRemote(p parsed, sec secrets, c driver.Config) (*remote, error) {
	api, err := splitBase(apiBase)
	if err != nil {
		return nil, err
	}
	cont, err := splitBase(contentBase)
	if err != nil {
		return nil, err
	}
	rpc, rpcTr, err := client(p, c, api.host, rpcHeaderTimeout)
	if err != nil {
		return nil, err
	}
	ct, ctTr, err := client(p, c, cont.host, contentHeaderTimeout)
	if err != nil {
		return nil, err
	}
	return &remote{p: p, sec: sec, rpc: rpc, content: ct, trs: []*http.Transport{rpcTr, ctTr}}, nil
}

type remote struct {
	p       parsed
	sec     secrets
	rpc     *http.Client
	content *http.Client
	trs     []*http.Transport

	// fresh: the access token came from the exchange and there is no refresh token
	// to replace it (a 401 is then final, never a refresh).
	fresh bool

	mu  sync.Mutex // guards tok, exp and flight; NEVER held across a network call
	tok string     // the access token: memory only, never logged or put in an error
	exp time.Time
	// flight is the token refresh in progress, if any (LAUN-154: callers that need the
	// token wait on it with their own context, and no lock is held while it runs).
	flight *refreshFlight
}

// refreshFlight is one token refresh that several callers share.
type refreshFlight struct {
	done   chan struct{}
	tok    string
	err    error
	ctxErr bool // the leader's own context ended: a waiter must try again, not share the error
}

// respError marks an error that came from an HTTP reply (so the server was
// reached). It unwraps to the safe *driver.Error. It keeps only fixed tags and
// numbers read from the reply, never its words.
type respError struct {
	e          *driver.Error
	status     int
	tags       map[string]bool
	correct    int64 // correct_offset of an incorrect_offset reply, else -1
	rate       bool
	retryAfter time.Duration
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
	timer  *time.Timer
	d      time.Duration
}

func newWatch(parent context.Context) (context.Context, *watch) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &watch{cancel: cancel, d: stallAfter}
	w.timer = time.AfterFunc(w.d, func() { cancel(errStalled) })
	return ctx, w
}

func (w *watch) touch() { w.timer.Reset(w.d) }
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

// reqBody is a request body that can be shut down: once stop returns, no Read is
// running and none will touch the caller's buffer again.
type reqBody struct {
	mu   sync.Mutex
	done bool
	p    progress
}

func (b *reqBody) Read(x []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return 0, io.ErrClosedPipe
	}
	return b.p.Read(x)
}

func (b *reqBody) Close() error { return nil }

// stop waits for a Read in progress and refuses every later one.
func (b *reqBody) stop() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
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

// ---- access tokens -------------------------------------------------------

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

// tokenRequest posts a form to the OAuth token endpoint. The app secret and the
// token/code travel in the body only (never in a URL).
func (r *remote) tokenRequest(ctx context.Context, form url.Values) (tokenReply, error) {
	res, err := r.send(ctx, call{
		client: r.rpc, url: apiBase + "/oauth2/token", ctype: "application/x-www-form-urlencoded",
		body: []byte(form.Encode()), maxReply: smallReplyMax,
	})
	if err != nil {
		return tokenReply{}, err
	}
	var tr tokenReply
	if json.Unmarshal(res.body, &tr) != nil || !strings.EqualFold(tr.TokenType, "bearer") || !printable(tr.AccessToken, 4096) {
		return tokenReply{}, driver.Errf(driver.ServerError)
	}
	return tr, nil
}

func (r *remote) setToken(t tokenReply) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setTokenLocked(t)
}

func (r *remote) setTokenLocked(t tokenReply) {
	life := time.Duration(t.ExpiresIn) * time.Second
	if life <= 0 {
		life = 2 * time.Minute // unknown lifetime: assume short
	}
	slack := tokenSlack
	if life/2 < slack {
		slack = life / 2
	}
	r.tok, r.exp = t.AccessToken, clockNow().Add(life-slack)
}

// accessToken returns the cached access token, or refreshes it. One caller (the
// leader) runs the refresh; the others wait for its result or for their own
// context. The mutex is held only to read and write the cache, never across the
// token call (which may wait out 429 replies for many minutes): a Close, or a call
// whose context ends, never waits behind a refresh. A leader whose own context ended
// does not hand its error to the waiters; one of them leads the next try.
func (r *remote) accessToken(ctx context.Context) (string, error) {
	for {
		r.mu.Lock()
		if r.tok != "" && clockNow().Before(r.exp) {
			tok := r.tok
			r.mu.Unlock()
			return tok, nil
		}
		if r.fresh || r.sec.RefreshToken == "" {
			r.mu.Unlock()
			return "", driver.Errf(driver.AuthFailed)
		}
		if fl := r.flight; fl != nil {
			r.mu.Unlock()
			select {
			case <-fl.done:
			case <-ctx.Done():
				return "", driver.MapNetError(ctx.Err())
			}
			if fl.ctxErr {
				continue
			}
			return fl.tok, fl.err
		}
		fl := &refreshFlight{done: make(chan struct{})}
		r.flight = fl
		form := url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {r.sec.RefreshToken},
			"client_id": {r.sec.AppKey}, "client_secret": {r.sec.AppSecret},
		}
		r.mu.Unlock()
		tr, err := r.tokenRequest(ctx, form)
		r.mu.Lock()
		if err == nil {
			r.setTokenLocked(tr)
			fl.tok = r.tok
		}
		fl.err = err
		fl.ctxErr = err != nil && ctx.Err() != nil
		r.flight = nil
		r.mu.Unlock()
		close(fl.done)
		return fl.tok, fl.err
	}
}

// invalidate drops tok if it is still the cached one.
func (r *remote) invalidate(tok string) {
	r.mu.Lock()
	if r.tok == tok {
		r.tok = ""
	}
	r.mu.Unlock()
}

// ---- one request, with the retry rules -----------------------------------

type call struct {
	client    *http.Client
	url       string
	auth      bool   // send the access token
	apiArg    string // the Dropbox-API-Arg header, "" for none
	ctype     string // Content-Type, "" for none
	body      []byte // nil for no body
	maxReply  int64
	stream    bool // return the reply body as a stream (download)
	transient bool // retry a transient failure (upload chunks; T16 #31's list)
}

type result struct {
	body   []byte
	stream io.ReadCloser
	header http.Header // the reply headers of a streamed reply
}

// send runs a call and applies three rules. A 429 (or a too_many_* tag) waits
// Retry-After, capped, and tries again, a few times. A 401 on a call with an
// access token drops the token, refreshes it and tries once more. A transient
// failure of an upload chunk (timeout, connection, stall, 5xx) is sent again, up
// to transientTries times; the caller's offset rules make that safe.
func (r *remote) send(ctx context.Context, c call) (*result, error) {
	rateWaits, refreshed, transient := 0, false, 0
	for {
		tok := ""
		if c.auth {
			var err error
			if tok, err = r.accessToken(ctx); err != nil {
				return nil, err
			}
		}
		res, err := r.once(ctx, c, tok)
		if err == nil {
			return res, nil
		}
		var re *respError
		switch {
		case errors.As(err, &re) && re.rate:
			if rateWaits >= maxRateWaits {
				return nil, &respError{e: driver.ServerErr(429), status: 429, correct: -1}
			}
			rateWaits++
			if serr := sleep(ctx, re.retryAfter); serr != nil {
				return nil, driver.MapNetError(serr)
			}
			continue
		case errors.As(err, &re) && re.status == http.StatusUnauthorized && c.auth && !refreshed &&
			!re.tags["missing_scope"] && !r.fresh:
			refreshed = true
			r.invalidate(tok)
			continue
		case c.transient && transient < transientTries-1 && transientErr(err):
			transient++
			if serr := sleep(ctx, time.Duration(transient*transient)*time.Second); serr != nil {
				return nil, driver.MapNetError(serr)
			}
			continue
		}
		return nil, err
	}
}

// transientErr is T16 #31's list (timeout, connect, dns, stalled, 5xx) plus a
// transport failure that gave no status (a reset or a cut connection): for an
// upload chunk a resend is safe because the offset rules settle a chunk that
// had landed.
func transientErr(err error) bool {
	if driver.Retryable(plain(err)) {
		return true
	}
	var re *respError
	var de *driver.Error
	return !errors.As(err, &re) && errors.As(err, &de) && de.Code == driver.ServerError && de.Status == 0
}

// once sends one request. A non-2xx reply is a *respError (the body is closed).
func (r *remote) once(parent context.Context, c call, tok string) (*result, error) {
	ctx, w := newWatch(parent)
	keep := false
	defer func() {
		if !keep {
			w.stop()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, nil)
	if err != nil {
		return nil, driver.Errf(driver.ServerError)
	}
	if c.body != nil {
		req.ContentLength = int64(len(c.body))
		if len(c.body) == 0 {
			req.Body = http.NoBody
		} else {
			// The transport may keep reading the body from its own goroutine after
			// Do returns (an early reply, a cancel). Put reuses one buffer for every
			// chunk, so the body must be dead before once returns (LAUN-174).
			rb := &reqBody{p: progress{r: bytes.NewReader(c.body), w: w, ctx: ctx}}
			defer rb.stop()
			req.Body = rb
		}
	}
	if c.ctype != "" {
		req.Header.Set("Content-Type", c.ctype)
	}
	if c.apiArg != "" {
		req.Header.Set("Dropbox-API-Arg", c.apiArg)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fail(ctx, err)
	}
	w.touch()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if c.stream {
			keep = true
			return &result{stream: &body{rc: resp.Body, w: w, ctx: ctx}, header: resp.Header}, nil
		}
		b, rerr := io.ReadAll(io.LimitReader(&progress{r: resp.Body, w: w, ctx: ctx}, c.maxReply+1))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, fail(ctx, rerr)
		}
		if int64(len(b)) > c.maxReply {
			return nil, driver.Errf(driver.TooLarge)
		}
		return &result{body: b}, nil
	}
	b, _ := io.ReadAll(io.LimitReader(&progress{r: resp.Body, w: w, ctx: ctx}, errBodyMax))
	_ = resp.Body.Close()
	return nil, classify(resp.StatusCode, resp.Header, b)
}

// walk collects the fixed tags (".tag" values and an OAuth "error" string),
// correct_offset and retry_after of an error reply. Nothing else is read: no
// error_summary, no description, no message.
func walk(v any, depth int, tags map[string]bool, correct *int64, retry *int64) {
	if depth > 6 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			switch k {
			case ".tag":
				if s, ok := x.(string); ok {
					addTag(tags, s)
				}
			case "correct_offset":
				if f, ok := x.(float64); ok && f >= 0 && f < 1<<53 {
					*correct = int64(f)
				}
			case "retry_after":
				if f, ok := x.(float64); ok && f >= 0 && f < 1<<31 {
					*retry = int64(f)
				}
			default:
				walk(x, depth+1, tags, correct, retry)
			}
		}
	case []any:
		for _, x := range t {
			walk(x, depth+1, tags, correct, retry)
		}
	}
}

func addTag(tags map[string]bool, s string) {
	if len(s) == 0 || len(s) > 64 {
		return
	}
	for i := 0; i < len(s); i++ {
		if !(s[i] >= 'a' && s[i] <= 'z' || s[i] == '_') {
			return
		}
	}
	tags[s] = true
}

// classify turns an error reply into a safe error.
func classify(status int, h http.Header, b []byte) *respError {
	tags := map[string]bool{}
	correct, retry := int64(-1), int64(-1)
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) == nil {
		if raw, ok := top["error"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil { // OAuth: {"error":"invalid_grant",...}
				addTag(tags, s)
			} else {
				var v any
				if json.Unmarshal(raw, &v) == nil {
					walk(v, 0, tags, &correct, &retry)
				}
			}
		}
	}
	re := &respError{status: status, tags: tags, correct: correct}
	switch {
	case status == http.StatusTooManyRequests || tags["too_many_requests"] || tags["too_many_write_operations"]:
		re.rate = true
		re.e = driver.ServerErr(status)
		wait := minRetryAfter
		if s, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64); err == nil && s >= 0 {
			wait = time.Duration(s) * time.Second
		} else if retry >= 0 {
			wait = time.Duration(retry) * time.Second
		}
		if wait < minRetryAfter {
			wait = minRetryAfter
		}
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
		re.retryAfter = wait
	case tags["missing_scope"]:
		// Dropbox's status for a missing scope is not verified (401 as an AuthError, or
		// 403): the tag decides, before the 401 rule, so no refresh is wasted on it.
		re.e = driver.Errf(driver.Forbidden)
	case status == http.StatusUnauthorized:
		re.e = driver.Errf(driver.AuthFailed)
	case tags["insufficient_space"]:
		re.e = driver.Errf(driver.QuotaExceeded)
	case tags["invalid_grant"] || tags["invalid_client"] || tags["unauthorized_client"] || tags["access_denied"]:
		re.e = driver.Errf(driver.AuthFailed)
	case status == http.StatusForbidden || tags["no_write_permission"] || tags["disallowed_name"] ||
		tags["restricted_content"] || tags["team_folder"]:
		re.e = driver.Errf(driver.Forbidden)
	case status == http.StatusConflict && (tags["not_found"] || tags["not_file"] || tags["not_folder"]):
		re.e = driver.Errf(driver.NotFound)
	case tags["too_large"]:
		re.e = driver.Errf(driver.TooLarge)
	default:
		re.e = driver.StatusError(status)
	}
	return re
}

// ---- helpers for the API -------------------------------------------------

func (r *remote) rpcCall(arg any, maxReply int64) call {
	b, _ := json.Marshal(arg)
	return call{client: r.rpc, auth: true, ctype: "application/json", body: b, maxReply: maxReply}
}

func (r *remote) apiPath(name string) string {
	if r.p.Folder == "" {
		return "/" + name
	}
	return "/" + r.p.Folder + "/" + name
}

func (r *remote) rootPath() string {
	if r.p.Folder == "" {
		return ""
	}
	return "/" + r.p.Folder
}

func apiArg(v any) string {
	b, _ := json.Marshal(v) // Go escapes non-ASCII only if told; every value here is ASCII by validation
	return string(b)
}

// ---- upload --------------------------------------------------------------

type fileMeta struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentHash string `json:"content_hash"`
}

// contentHasher is Dropbox's content_hash: sha256 over the concatenated sha256
// digests of each 4 MiB block.
type contentHasher struct {
	block hash.Hash
	sum   hash.Hash
	n     int
}

func newHasher() *contentHasher { return &contentHasher{block: sha256.New(), sum: sha256.New()} }

func (h *contentHasher) Write(p []byte) {
	for len(p) > 0 {
		take := min(len(p), hashBlock-h.n)
		h.block.Write(p[:take])
		h.n += take
		p = p[take:]
		if h.n == hashBlock {
			h.sum.Write(h.block.Sum(nil))
			h.block.Reset()
			h.n = 0
		}
	}
}

func (h *contentHasher) Final() string {
	if h.n > 0 {
		h.sum.Write(h.block.Sum(nil))
		h.block.Reset()
		h.n = 0
	}
	return hex.EncodeToString(h.sum.Sum(nil))
}

// check compares the metadata Dropbox returned with what was sent. A reply
// without a content_hash is checked by size alone.
func (m fileMeta) check(size int64, hashHex string) error {
	if m.Size != size || (m.ContentHash != "" && m.ContentHash != hashHex) {
		return driver.Errf(driver.ServerError)
	}
	return nil
}

func parseMeta(b []byte) (fileMeta, error) {
	var m fileMeta
	if json.Unmarshal(b, &m) != nil {
		return fileMeta{}, driver.Errf(driver.ServerError)
	}
	return m, nil
}

type commitArg struct {
	Path       string `json:"path"`
	Mode       string `json:"mode"`
	Autorename bool   `json:"autorename"`
	Mute       bool   `json:"mute"`
}

func (r *remote) commit(name string) commitArg {
	// overwrite: names are unique per attempt (T16 #31), and a retry of one name
	// must replace its own partial result, not fail on it.
	return commitArg{Path: r.apiPath(name), Mode: "overwrite", Autorename: false, Mute: true}
}

// readChunk fills buf from src. A source that ends early is a dump that changed
// while it was read (dump_mismatch).
func readChunk(src io.Reader, buf []byte) error {
	if _, err := io.ReadFull(src, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return driver.Errf(driver.DumpMismatch)
		}
		return driver.MapNetError(err)
	}
	return nil
}

// noMore refuses a source that holds more than its declared size.
func noMore(src io.Reader) error {
	var one [1]byte
	n, err := io.ReadFull(src, one[:])
	if n > 0 {
		return driver.Errf(driver.DumpMismatch)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return driver.MapNetError(err)
	}
	return nil
}

// Put implements driver.Remote. An object up to one chunk goes in ONE upload
// call; a larger one in an upload session of chunk-sized pieces. Only one chunk
// is held in memory. The result is checked against Dropbox's reply (size and
// content_hash). Above MaxObject it is too_large before any byte is read.
func (r *remote) Put(ctx context.Context, name string, size int64, src io.Reader) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	if size < 0 || size > MaxObject {
		return driver.Errf(driver.TooLarge)
	}
	chunk := min(chunkSize, int64(maxCall))
	buf := make([]byte, min(size, chunk))
	h := newHasher()
	if size <= chunk {
		if err := readChunk(src, buf); err != nil {
			return err
		}
		if err := noMore(src); err != nil {
			return err
		}
		h.Write(buf)
		res, err := r.send(ctx, call{
			client: r.content, url: contentBase + "/2/files/upload", auth: true, transient: true,
			apiArg: apiArg(r.commit(name)), ctype: "application/octet-stream", body: buf, maxReply: smallReplyMax,
		})
		if err != nil {
			return plain(err)
		}
		m, err := parseMeta(res.body)
		if err != nil {
			return err
		}
		return m.check(size, h.Final())
	}
	n := (size + chunk - 1) / chunk
	var sid string
	var off int64
	for i := int64(0); i < n; i++ {
		if err := ctx.Err(); err != nil {
			return driver.MapNetError(err)
		}
		want := min(chunk, size-off)
		piece := buf[:want]
		if err := readChunk(src, piece); err != nil {
			return err
		}
		h.Write(piece)
		var err error
		switch {
		case i == 0:
			sid, err = r.sessionStart(ctx, piece)
		case i < n-1:
			err = r.sessionAppend(ctx, sid, off, piece)
		default:
			if err = noMore(src); err == nil {
				err = r.sessionFinish(ctx, sid, off, name, piece, size, h.Final())
			}
		}
		if err != nil {
			return err
		}
		off += want
	}
	return nil
}

type cursorArg struct {
	SessionID string `json:"session_id"`
	Offset    int64  `json:"offset"`
}

func (r *remote) sessionStart(ctx context.Context, piece []byte) (string, error) {
	res, err := r.send(ctx, call{
		client: r.content, url: contentBase + "/2/files/upload_session/start", auth: true, transient: true,
		apiArg: apiArg(map[string]bool{"close": false}), ctype: "application/octet-stream", body: piece, maxReply: smallReplyMax,
	})
	if err != nil {
		return "", plain(err)
	}
	var s struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(res.body, &s) != nil || s.SessionID == "" || len(s.SessionID) > 512 {
		return "", driver.Errf(driver.ServerError)
	}
	return s.SessionID, nil
}

// sessionAppend sends one middle chunk. If an earlier try reached Dropbox but its
// answer was lost, the resend gets incorrect_offset with the offset Dropbox is
// at: when that is the end of this chunk, the chunk is already there.
func (r *remote) sessionAppend(ctx context.Context, sid string, off int64, piece []byte) error {
	_, err := r.send(ctx, call{
		client: r.content, url: contentBase + "/2/files/upload_session/append_v2", auth: true, transient: true,
		apiArg: apiArg(map[string]any{"cursor": cursorArg{sid, off}, "close": false}),
		ctype:  "application/octet-stream", body: piece, maxReply: smallReplyMax,
	})
	var re *respError
	if errors.As(err, &re) && re.tags["incorrect_offset"] && re.correct == off+int64(len(piece)) {
		return nil
	}
	return plain(err)
}

func (r *remote) sessionFinish(ctx context.Context, sid string, off int64, name string, piece []byte, size int64, hashHex string) error {
	res, err := r.send(ctx, call{
		client: r.content, url: contentBase + "/2/files/upload_session/finish", auth: true, transient: true,
		apiArg: apiArg(map[string]any{"cursor": cursorArg{sid, off}, "commit": r.commit(name)}),
		ctype:  "application/octet-stream", body: piece, maxReply: smallReplyMax,
	})
	if err != nil {
		var re *respError
		if errors.As(err, &re) && (re.tags["incorrect_offset"] || re.tags["not_found"] || re.tags["closed"]) {
			// An earlier try may have committed and lost its answer: look at the file.
			if m, merr := r.metadata(ctx, name); merr == nil && m.check(size, hashHex) == nil {
				return nil
			}
		}
		return plain(err)
	}
	m, err := parseMeta(res.body)
	if err != nil {
		return err
	}
	return m.check(size, hashHex)
}

func (r *remote) metadata(ctx context.Context, name string) (fileMeta, error) {
	res, err := r.send(ctx, r.rpcCall(map[string]any{"path": r.apiPath(name)}, smallReplyMax).withURL(apiBase+"/2/files/get_metadata"))
	if err != nil {
		return fileMeta{}, plain(err)
	}
	return parseMeta(res.body)
}

func (c call) withURL(u string) call { c.url = u; return c }

// ---- list, get, delete ---------------------------------------------------

type listPage struct {
	Entries []struct {
		Tag  string `json:".tag"`
		Name string `json:"name"`
		Size *int64 `json:"size"`
	} `json:"entries"`
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"has_more"`
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

// List implements driver.Remote. Only files whose name passes remotename.Valid
// and starts with prefix are returned (a folder, a deleted entry, a stranger's
// file or a hostile name is dropped, never used). The cursor is followed up to
// maxPages pages and 16 MiB of replies together. A folder that does not exist yet
// is an empty listing (the first upload makes it).
func (r *remote) List(ctx context.Context, prefix string) ([]driver.Entry, error) {
	es, err := r.list(ctx, prefix)
	return es, plain(err)
}

func (r *remote) list(ctx context.Context, prefix string) ([]driver.Entry, error) {
	if !prefixOK(prefix) || len(prefix) > 128 {
		return nil, driver.Errf(driver.NotFound)
	}
	var out []driver.Entry
	var total int64
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, driver.Errf(driver.TooLarge)
		}
		c := r.rpcCall(map[string]any{
			"path": r.rootPath(), "recursive": false, "include_deleted": false,
			"include_mounted_folders": false, "limit": 2000,
		}, int64(driver.MaxListBody)-total)
		c.url = apiBase + "/2/files/list_folder"
		if cursor != "" {
			c = r.rpcCall(map[string]any{"cursor": cursor}, int64(driver.MaxListBody)-total)
			c.url = apiBase + "/2/files/list_folder/continue"
		}
		res, err := r.send(ctx, c)
		if err != nil {
			var re *respError
			if page == 0 && errors.As(err, &re) && re.e.Code == driver.NotFound {
				return nil, nil
			}
			return nil, err
		}
		total += int64(len(res.body))
		var lp listPage
		if json.Unmarshal(res.body, &lp) != nil {
			return nil, driver.Errf(driver.ServerError)
		}
		for _, e := range lp.Entries {
			if e.Tag != "file" || !strings.HasPrefix(e.Name, prefix) || !remotename.Valid(e.Name) {
				continue
			}
			sz := int64(-1)
			if e.Size != nil {
				sz = *e.Size
			}
			out = append(out, driver.Entry{Name: e.Name, Size: sz})
		}
		if !lp.HasMore {
			return out, nil
		}
		if lp.Cursor == "" || len(lp.Cursor) > maxCursorLen || lp.Cursor == cursor {
			return nil, driver.Errf(driver.ServerError)
		}
		cursor = lp.Cursor
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
	res, err := r.send(ctx, call{
		client: r.content, url: contentBase + "/2/files/download", auth: true,
		apiArg: apiArg(map[string]string{"path": r.apiPath(name)}), stream: true,
	})
	if err != nil {
		return nil, plain(err)
	}
	return verifyDownload(res), nil
}

// maxResultHeader bounds the Dropbox-API-Result header a download is checked against.
const maxResultHeader = 64 << 10

// verifyDownload wraps a download so that a short or long body, or a body whose
// content_hash differs from the one Dropbox sent, ends in server_error instead of a
// clean EOF (LAUN-154). The size and the hash come from the Dropbox-API-Result header;
// a reply with no usable header, or without one of the two fields, is not checked
// for that field (UNVERIFIED against a real Dropbox; the runner's sha256 stays the backstop).
func verifyDownload(res *result) io.ReadCloser {
	h := res.header.Get("Dropbox-API-Result")
	if h == "" || len(h) > maxResultHeader {
		return res.stream
	}
	var m struct {
		Size        *int64 `json:"size"`
		ContentHash string `json:"content_hash"`
	}
	if json.Unmarshal([]byte(h), &m) != nil || (m.Size == nil && m.ContentHash == "") {
		return res.stream
	}
	v := &verified{rc: res.stream, size: -1, want: m.ContentHash}
	if m.Size != nil && *m.Size >= 0 {
		v.size = *m.Size
	}
	if v.want != "" {
		v.h = newHasher()
	}
	return v
}

type verified struct {
	rc   io.ReadCloser
	size int64 // -1: not announced
	want string
	h    *contentHasher
	got  int64
	bad  bool
}

func (v *verified) Read(p []byte) (int, error) {
	if v.bad {
		return 0, driver.Errf(driver.ServerError)
	}
	n, err := v.rc.Read(p)
	v.got += int64(n)
	if v.h != nil {
		v.h.Write(p[:n])
	}
	if v.size >= 0 && v.got > v.size {
		v.bad = true
		return n, driver.Errf(driver.ServerError)
	}
	if err == io.EOF {
		if v.size >= 0 && v.got != v.size || v.h != nil && v.h.Final() != v.want {
			v.bad = true
			return n, driver.Errf(driver.ServerError)
		}
	}
	return n, err
}

func (v *verified) Close() error { return v.rc.Close() }

// Delete implements driver.Remote. A missing file is not an error.
func (r *remote) Delete(ctx context.Context, name string) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	_, err := r.send(ctx, r.rpcCall(map[string]any{"path": r.apiPath(name)}, smallReplyMax).withURL(apiBase+"/2/files/delete_v2"))
	var re *respError
	if errors.As(err, &re) && re.status == http.StatusConflict && re.tags["not_found"] {
		return nil
	}
	return plain(err)
}

// Close implements driver.Remote.
func (r *remote) Close() error {
	for _, tr := range r.trs {
		tr.CloseIdleConnections()
	}
	r.mu.Lock()
	r.tok = "" // the access token does not outlive the remote
	r.mu.Unlock()
	return nil
}

// Test implements driver.Remote: T16 #36 phase 2. connect and auth are the token
// refresh and one listing (a reply of any status shows the server was reached);
// then write, read (a listing finds the file, a download matches it) and delete.
func (r *remote) Test(ctx context.Context, testName string) []driver.Step {
	var steps []driver.Step
	failAt := func(name string, err error) []driver.Step {
		return append(steps, driver.Step{Name: name, Code: driver.CodeOf(err)})
	}
	data := []byte("launchpad backup test " + strconv.FormatInt(clockNow().UnixNano(), 16))
	if _, err := r.list(ctx, "launchpad-"); err != nil {
		if !reached(err) {
			return failAt("connect", err)
		}
		steps = append(steps, driver.Step{Name: "connect", OK: true})
		return failAt("auth", err)
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

// ---- the exchange --------------------------------------------------------

// Exchange implements driver.Exchanger: it turns the single-use authorization
// code into a refresh token (T16 #9). c.Secrets holds appKey and appSecret. After
// the exchange it lists the folder once with the new access token, so a code that
// was made for an app without the file scopes fails here, with the refresh token
// NOT returned (and so not saved); any other listing failure is left to the test
// run (the code is already used). The refresh token is returned to the worker
// and nowhere else: not logged, not in an error.
func (Driver) Exchange(ctx context.Context, c driver.Config, code string) (string, error) {
	p, err := parse(c.Config)
	if err != nil {
		return "", err
	}
	sec, err := parseSecrets(c.Secrets, false)
	if err != nil {
		return "", err
	}
	if !printable(code, 512) {
		return "", driver.Errf(driver.AuthFailed)
	}
	r, err := newRemote(p, sec, c)
	if err != nil {
		return "", err
	}
	defer r.Close()
	tr, err := r.tokenRequest(ctx, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {sec.AppKey}, "client_secret": {sec.AppSecret},
	})
	if err != nil {
		return "", plain(err)
	}
	if !printable(tr.RefreshToken, 4096) {
		// The code came from an online authorization (no token_access_type=offline).
		return "", driver.Errf(driver.AuthFailed)
	}
	r.setToken(tr)
	r.fresh = true
	if _, err := r.list(ctx, "launchpad-"); err != nil {
		if cd := driver.CodeOf(plain(err)); cd == driver.AuthFailed || cd == driver.Forbidden {
			return "", plain(err)
		}
	}
	return tr.RefreshToken, nil
}
