package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// maxResponseBytes caps one REST response body. The largest legitimate body
// is a 250-item issue page with full descriptions, far below this.
const maxResponseBytes int64 = 32 << 20

// ErrReadOnly is returned by a read-only client for any method but GET. The
// reviewer profile builds its client read-only, so even a programming error
// that wired a write tool into that profile could not reach the API.
var ErrReadOnly = errors.New("this MCP profile is read-only")

// APIError is a non-2xx answer from the tracker API, decoded from its
// {"error": "...", "code": "..."} envelope.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("tracker API %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("tracker API %d %s: %s", e.Status, e.Code, e.Message)
}

// IsStatus reports whether err is an APIError with the given status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// ClientConfig configures a Client. Token is the bearer secret; it is sent
// only in the Authorization header, never logged and never put in an error.
type ClientConfig struct {
	BaseURL  string // e.g. https://tracker.example (with or without /api/v1)
	Token    string
	Org      string
	ReadOnly bool
	// AllowInsecureHTTP permits plain http to a non-loopback host. Loopback
	// (localhost, 127.0.0.0/8, ::1) is always allowed.
	AllowInsecureHTTP bool
	HTTPClient        *http.Client // nil: a client with a 30s timeout and no redirects
	UserAgent         string
	// DownloadDir is where download_attachment saves files (LAUNCHPAD_DOWNLOAD_DIR).
	// Empty: os.TempDir()/launchpad-mcp. Created 0700 on the first download.
	DownloadDir string
	// FilesURL is the origin of the attachment store's public endpoint
	// (LAUNCHPAD_FILES_URL, e.g. https://files.example). A download may
	// be redirected to this host or to the API host, and to no other.
	FilesURL string
	// DownloadMaxBytes caps one download. 0: 25 MiB; never above 100 MiB.
	DownloadMaxBytes int64
}

// Client calls the tracker REST API for one org.
type Client struct {
	base     *url.URL // ends at /api/v1/orgs/{org}
	root     *url.URL // ends at /api/v1
	token    string
	readOnly bool
	http     *http.Client
	ua       string
	maxBody  int64 // maxResponseBytes; tests lower it

	allowHTTP bool     // AllowInsecureHTTP
	files     *url.URL // FilesURL; nil when unset
	dlDir     string
	dlMax     int64
}

// NewClient validates cfg and builds a Client. Errors never contain the token.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("LAUNCHPAD_URL is not set")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("LAUNCHPAD_URL must be an absolute http(s) URL")
	}
	if u.User != nil {
		return nil, errors.New("LAUNCHPAD_URL must not carry credentials; use LAUNCHPAD_TOKEN")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("LAUNCHPAD_URL must not have a query or fragment")
	}
	if u.Scheme == "http" && !cfg.AllowInsecureHTTP && !isLoopback(u.Hostname()) {
		return nil, errors.New("LAUNCHPAD_URL uses plain http to a non-loopback host; use https " +
			"(or set LAUNCHPAD_ALLOW_INSECURE_HTTP=1 for a private network)")
	}
	if err := checkToken(cfg.Token); err != nil {
		return nil, err
	}
	if cfg.Org == "" {
		return nil, errors.New("LAUNCHPAD_ORG is not set")
	}
	if strings.ContainsAny(cfg.Org, "/?#% ") {
		return nil, errors.New("LAUNCHPAD_ORG must be an org slug")
	}

	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/api/v1")
	root := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: p + "/api/v1"}
	base := root.JoinPath("orgs", cfg.Org)

	hc := cfg.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if u.Scheme == "http" {
			// A proxy from HTTP_PROXY would see the bearer token in clear.
			// Over https a proxy only tunnels (CONNECT), so it is kept.
			tr.Proxy = nil
		}
		hc = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	}
	// Never follow a redirect: the bearer header must go to the configured
	// host only, and the API never redirects a JSON call.
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	var files *url.URL
	if strings.TrimSpace(cfg.FilesURL) != "" {
		f, err := url.Parse(strings.TrimSpace(cfg.FilesURL))
		if err != nil || f.Host == "" || (f.Scheme != "http" && f.Scheme != "https") || f.User != nil {
			return nil, errors.New("LAUNCHPAD_FILES_URL must be an absolute http(s) URL without credentials")
		}
		if f.Scheme == "http" && !cfg.AllowInsecureHTTP && !isLoopback(f.Hostname()) {
			return nil, errors.New("LAUNCHPAD_FILES_URL uses plain http to a non-loopback host; use https")
		}
		files = f
	}
	dlMax := cfg.DownloadMaxBytes
	if dlMax <= 0 {
		dlMax = defaultDownloadBytes
	}
	dlMax = min(dlMax, hardDownloadBytes)

	ua := cfg.UserAgent
	if ua == "" {
		ua = "launchpad-mcp"
	}
	return &Client{base: base, root: root, token: cfg.Token, readOnly: cfg.ReadOnly, http: &c, ua: ua,
		maxBody: maxResponseBytes, allowHTTP: cfg.AllowInsecureHTTP, files: files, dlDir: cfg.DownloadDir, dlMax: dlMax}, nil
}

// checkToken rejects an obviously wrong token without echoing it.
func checkToken(tok string) error {
	switch {
	case tok == "":
		return errors.New("LAUNCHPAD_TOKEN is not set")
	case !strings.HasPrefix(tok, "odt_"):
		return errors.New("LAUNCHPAD_TOKEN is not a tracker token (want odt_...)")
	case strings.ContainsAny(tok, " \t\r\n"):
		return errors.New("LAUNCHPAD_TOKEN contains whitespace")
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// seg escapes one caller-supplied path segment. "", "." and ".." are refused:
// they would change which route is called.
func seg(s string) (string, error) {
	if s == "" || s == "." || s == ".." {
		return "", fmt.Errorf("invalid id %q", s)
	}
	return url.PathEscape(s), nil
}

// path joins org-relative segments, escaping each one.
func orgPath(parts ...string) (string, error) {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		e, err := seg(p)
		if err != nil {
			return "", err
		}
		out = append(out, e)
	}
	return strings.Join(out, "/"), nil
}

// Get calls GET {org}/{path}?{q} and decodes the body into out.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, c.base, path, q, nil, out)
}

// GetRoot calls GET /api/v1/{path} (outside the org, e.g. "me").
func (c *Client) GetRoot(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, c.root, path, nil, nil, out)
}

// Post calls POST {org}/{path} with body as JSON.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, c.base, path, nil, body, out)
}

// Patch calls PATCH {org}/{path} with body as JSON.
func (c *Client) Patch(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPatch, c.base, path, nil, body, out)
}

// Delete calls DELETE {org}/{path}.
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, c.base, path, nil, nil, nil)
}

// transport returns the client's transport, for tests.
func (c *Client) transport() http.RoundTripper { return c.http.Transport }

func (c *Client) do(ctx context.Context, method string, base *url.URL, path string, q url.Values, body, out any) error {
	if c.readOnly && method != http.MethodGet {
		return ErrReadOnly
	}
	// path is built by orgPath (escaped segments) or is a constant; it is
	// appended to the already-escaped base path.
	u := *base
	if path != "" {
		u.Path = base.Path + "/" + unescapeForPath(path)
		u.RawPath = base.EscapedPath() + "/" + path
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error carries the URL (no secret in it) and the cause.
		return fmt.Errorf("%s %s: %w", method, u.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, u.Path, err)
	}
	if int64(len(data)) > c.maxBody {
		return fmt.Errorf("%s %s: response larger than %d bytes", method, u.Path, c.maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Status: resp.StatusCode}
		var env struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error != "" {
			ae.Message, ae.Code = env.Error, env.Code
		} else {
			ae.Message = http.StatusText(resp.StatusCode)
		}
		return ae
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, u.Path, err)
	}
	return nil
}

// unescapeForPath turns an escaped path back into its decoded form for
// url.URL.Path (RawPath carries the escaped form the server sees).
func unescapeForPath(p string) string {
	s, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return s
}

// CheckCredential calls GET /me once at start-up and logs what it finds. It
// never stops the server: a tracker that is down at start-up may be up by the
// first tool call, and each call reports its own error. It warns when the
// reviewer profile runs with a token that can write, because the read-only
// guard is then the only thing between a reviewer and a write.
func CheckCredential(ctx context.Context, c *Client, profile Profile, log *slog.Logger) {
	var me struct {
		Username string   `json:"username"`
		Scopes   []string `json:"scopes"`
	}
	if err := c.GetRoot(ctx, "me", &me); err != nil {
		log.Warn("credential check failed; tool calls will report the error", "err", err)
		return
	}
	log.Info("credential ok", "user", me.Username, "scopes", strings.Join(me.Scopes, ","))
	if profile == ProfileReviewer && slices.ContainsFunc(me.Scopes, func(s string) bool { return s != "read" }) {
		log.Warn("reviewer profile is running with a token that can write; mint a read-scoped token for reviewers")
	}
}
