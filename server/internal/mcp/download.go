package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// download_attachment (LAUN-178, decisions-T2 #44 and on). The bearer token
// goes to the tracker API only: the API answers 302 with a presigned URL, and
// the follow-up GET to that URL carries NO Authorization header. No error,
// log line or result holds the token, the presigned URL (its query is a
// credential), the file bytes or the user-supplied file name.

const (
	// defaultDownloadBytes is the default size cap of one download.
	defaultDownloadBytes int64 = 25 << 20
	// hardDownloadBytes is the cap no setting can raise (the upload limit).
	hardDownloadBytes int64 = 100 << 20
	// downloadTimeout bounds the whole follow-up GET.
	downloadTimeout = 5 * time.Minute
)

// downloadTypes is the upload allowlist (internal/tracker allowedContentTypes)
// with the file extension each type is saved under.
var downloadTypes = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"text/plain":      ".txt",
	"application/pdf": ".pdf",
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// dlErr is a fixed-sentence refusal with a code. It never carries a URL,
// header, body or file name.
func dlErr(code, msg string) error { return fmt.Errorf("download refused (%s): %s", code, msg) }

// attachmentID takes an attachment id, or the downloadUrl / url from
// get_issue (.../attachments/{id}/download), and returns the id.
func attachmentID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, "/:") {
		u, err := url.Parse(s)
		if err != nil {
			return "", dlErr("bad_attachment", "the attachment is not an id or a download URL")
		}
		parts := strings.Split(u.Path, "/")
		s = ""
		for i, p := range parts {
			if p == "attachments" && i+1 < len(parts) {
				s = parts[i+1]
				break
			}
		}
	}
	s = strings.ToLower(s)
	if !uuidRE.MatchString(s) {
		return "", dlErr("bad_attachment", "the attachment is not an id or a download URL")
	}
	return s, nil
}

// prepareDownloadDir creates the folder when it is missing and checks it: a
// real directory (never a symlink), owned by this user, mode 0700.
func prepareDownloadDir(dir string) (string, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "launchpad-mcp")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", dlErr("download_dir", "the download folder is not usable")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", dlErr("download_dir", "the download folder cannot be created")
	}
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", dlErr("download_dir", "the download folder must be a real directory, not a symlink")
	}
	if !dirOwnedPrivate(fi) {
		return "", dlErr("download_dir", "the download folder must be owned by this user with mode 0700")
	}
	return dir, nil
}

// checkRedirect validates the presigned URL the API handed back. Its host
// (with port) must be the API's or the configured files host, and it must not
// carry credentials.
func (c *Client) checkRedirect(loc string) (*url.URL, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return nil, dlErr("bad_redirect", "the API sent an unusable file address")
	}
	if !u.IsAbs() {
		// Relative to the API; the follow-up still carries no token.
		u = c.root.ResolveReference(u)
	}
	if u.User != nil || u.Host == "" {
		return nil, dlErr("bad_redirect", "the API sent an unusable file address")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (c.allowHTTP || isLoopback(u.Hostname()))) {
		return nil, dlErr("insecure_redirect", "the file address is not https")
	}
	// The host must be a configured one AND the scheme must be the one that
	// was configured for it, so LAUNCHPAD_ALLOW_INSECURE_HTTP cannot
	// downgrade an https host.
	hostOK, schemeOK := false, false
	for _, ref := range []*url.URL{c.root, c.files} {
		if ref != nil && strings.EqualFold(u.Host, ref.Host) {
			hostOK = true
			if u.Scheme == ref.Scheme {
				schemeOK = true
			}
		}
	}
	if !hostOK {
		return nil, dlErr("host_not_allowed",
			"the file host is neither the tracker nor LAUNCHPAD_FILES_URL")
	}
	if !schemeOK {
		return nil, dlErr("insecure_redirect", "the file address does not use the scheme configured for its host")
	}
	return u, nil
}

// locate calls the API download route with the token and returns the
// presigned address from its 302. It does not follow the redirect.
func (c *Client) locate(ctx context.Context, path string) (string, error) {
	u := *c.base
	u.Path = c.base.Path + "/" + unescapeForPath(path)
	u.RawPath = c.base.EscapedPath() + "/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", u.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusTemporaryRedirect {
		if loc := resp.Header.Get("Location"); loc != "" {
			return loc, nil
		}
		return "", dlErr("bad_redirect", "the API sent no file address")
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ae := &APIError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return "", dlErr("bad_redirect", "the API sent no file address")
	}
	var env struct{ Error, Code string }
	if json.Unmarshal(data, &env) == nil && env.Error != "" {
		ae.Message, ae.Code = env.Error, env.Code
	}
	return "", ae
}

// fetchPresigned GETs the presigned address with NO Authorization header and
// no redirect, and streams the body into dst, stopping at max bytes.
func (c *Client) fetchPresigned(ctx context.Context, u *url.URL, dst io.Writer, wantType string, wantSize, max int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, dlErr("request", "the file request could not be built")
	}
	req.Header.Set("User-Agent", c.ua)
	hc := *c.http // same transport; never follows a redirect; no overall timeout (ctx has it)
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		// The error text holds the presigned URL: drop it.
		if ctx.Err() != nil {
			return 0, dlErr("timeout", "the file download did not finish in time")
		}
		return 0, dlErr("unreachable", "the file host could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, dlErr("file_host_status", fmt.Sprintf("the file host answered %d", resp.StatusCode))
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != wantType {
		return 0, dlErr("type_mismatch", "the file host sent a different content type than the attachment record")
	}
	if resp.ContentLength > max || resp.ContentLength > wantSize {
		return 0, dlErr("too_large", "the file is larger than allowed")
	}
	ew := &errWriter{w: dst}
	n, err := io.Copy(ew, io.LimitReader(resp.Body, max+1))
	if err != nil {
		if ew.err != nil {
			return n, dlErr("write", "the file could not be written")
		}
		return n, dlErr("read", "the file download was cut off")
	}
	if n > max || n > wantSize {
		return n, dlErr("too_large", "the file is larger than allowed")
	}
	if n != wantSize {
		return n, dlErr("size_mismatch", "the file size differs from the attachment record")
	}
	return n, nil
}

// downloadAttachment is the download_attachment tool.
func downloadAttachment(ctx context.Context, t *Tools, a args) (any, error) {
	c := t.c
	id, err := attachmentID(a.str("attachment"))
	if err != nil {
		return nil, err
	}

	// The record: it must be a file attachment of THIS issue. The type and
	// size come from our own record, never from the file host.
	ip, err := orgPath("issues", a.str("issue"))
	if err != nil {
		return nil, err
	}
	var is issueHead
	if err := c.Get(ctx, ip, nil, &is); err != nil {
		return nil, err
	}
	ap, err := orgPath("attachments", id)
	if err != nil {
		return nil, err
	}
	var at struct {
		IssueID     string  `json:"issueId"`
		Kind        string  `json:"kind"`
		ContentType *string `json:"contentType"`
		SizeBytes   *int64  `json:"sizeBytes"`
	}
	if err := c.Get(ctx, ap, nil, &at); err != nil {
		return nil, err
	}
	if !strings.EqualFold(at.IssueID, is.ID) {
		return nil, &APIError{Status: 404, Code: "not_found", Message: "attachment not found on this issue"}
	}
	if at.Kind != "file" || at.ContentType == nil || at.SizeBytes == nil {
		return nil, dlErr("not_a_file", "this attachment is a link, not a file; open its url instead")
	}
	ext, ok := downloadTypes[*at.ContentType]
	if !ok {
		return nil, dlErr("type_not_allowed", "this content type cannot be downloaded")
	}
	max := c.dlMax
	if *at.SizeBytes < 0 || *at.SizeBytes > max {
		return nil, dlErr("too_large", fmt.Sprintf("the file is larger than the %d byte limit", max))
	}

	dir, err := prepareDownloadDir(c.dlDir)
	if err != nil {
		return nil, err
	}
	loc, err := c.locate(ctx, ap+"/download")
	if err != nil {
		return nil, err
	}
	pu, err := c.checkRedirect(loc)
	if err != nil {
		return nil, err
	}

	final := filepath.Join(dir, id+ext) // our id and a fixed extension only
	if fi, err := os.Lstat(final); err == nil && !fi.Mode().IsRegular() {
		return nil, dlErr("target_not_regular", "the target file name is taken by something that is not a file")
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, dlErr("write", "the file could not be written")
	}
	tmp := filepath.Join(dir, ".part-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, dlErr("write", "the file could not be written")
	}
	done := false
	defer func() {
		if !done {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	n, err := c.fetchPresigned(ctx, pu, f, *at.ContentType, *at.SizeBytes, max)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		return nil, dlErr("write", "the file could not be written")
	}
	if err := f.Close(); err != nil {
		return nil, dlErr("write", "the file could not be written")
	}
	// rename replaces a directory entry; it never writes through a symlink.
	if err := os.Rename(tmp, final); err != nil {
		return nil, dlErr("write", "the file could not be written")
	}
	done = true
	return map[string]any{"path": final, "contentType": *at.ContentType, "sizeBytes": n, "attachmentId": id}, nil
}

// errWriter remembers that a write failed, so a full disk is reported as a
// write error and not as a cut-off download.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}
