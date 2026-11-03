package sftp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/netguard"
)

func init() { driver.Register(Type, Driver{}) }

const (
	maxPrefixLen = 128
	partPrefix   = ".launchpad-part-"
	posixRename  = "posix-rename@openssh.com"
	// maxConcurrent is how many 32 KiB requests one file transfer keeps in flight
	// (2 MiB), so a transfer is not one round trip per packet.
	maxConcurrent = 64
	// sweepTimeout bounds the best-effort removal of an orphaned temp file.
	sweepTimeout = 10 * time.Second
	// partMaxAge is how old a part file must be before the sweep removes it (LAUN-154).
	// A live upload never has a part file this old.
	partMaxAge = 24 * time.Hour
	// maxSweep bounds the removals of one sweep.
	maxSweep = 100
)

// Test seams (never changed in production). stallAfter is "no byte moved for
// 120 s is stalled" (T16 #40); handshakeTimeout bounds dial, key exchange and
// SSH authentication.
var (
	stallAfter       = 120 * time.Second
	handshakeTimeout = 30 * time.Second
	maxEntries       = 200_000  // entries in one listing, next to the 16 MiB byte cap
	randToken        = randHex  // the temp file's random part
	allowPosixRename = true     // false: a test forces the plain-rename fallback
	hostKeyHook      func()     // a test runs code inside the host key callback
	nowFn            = time.Now // the sweep's clock
)

var (
	errStalled  = errors.New("sftp: stalled")
	errHostKey  = errors.New("sftp: host key")
	errProbed   = errors.New("sftp: probe done")
	errTooLarge = errors.New("sftp: reply too large")
)

type secrets struct {
	Password   string `json:"password"`
	PrivateKey string `json:"privateKey"`
}

func parseSecrets(raw json.RawMessage) (secrets, error) {
	var s secrets
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || (s.Password == "" && s.PrivateKey == "") {
		return secrets{}, driver.Errf(driver.SecretsUnreadable)
	}
	return s, nil
}

// authMethods builds the SSH auth list: the key first, then the password
// (also as keyboard-interactive, which some servers offer instead of
// "password"). A key that does not parse (also one that needs a passphrase:
// there is no passphrase secret) is secrets_unreadable. No text is kept.
func (s secrets) authMethods() ([]ssh.AuthMethod, error) {
	var out []ssh.AuthMethod
	if s.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(s.PrivateKey))
		if err != nil {
			return nil, driver.Errf(driver.SecretsUnreadable)
		}
		out = append(out, ssh.PublicKeys(signer))
	}
	if s.Password != "" {
		pw := s.Password
		out = append(out, ssh.Password(pw), ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				ans := make([]string, len(questions))
				for i := range ans {
					ans[i] = pw
				}
				return ans, nil
			}))
	}
	return out, nil
}

// hostState records what the host key callback saw.
type hostState struct {
	mu       sync.Mutex
	fp       string
	trusted  bool
	mismatch bool
}

func (h *hostState) get() (fp string, trusted, mismatch bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fp, h.trusted, h.mismatch
}

// pinCallback accepts exactly the pinned host key (constant-time compare). A
// different key is never accepted and never saved (T16 #36).
func pinCallback(pin string, h *hostState) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if hostKeyHook != nil {
			hostKeyHook()
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.fp = fp
		if pin == "" || subtle.ConstantTimeCompare([]byte(fp), []byte(pin)) != 1 {
			h.mismatch = true
			return errHostKey
		}
		h.trusted = true
		return nil
	}
}

// Probe implements driver.Driver: phase 1 of T16 #36. The SSH key exchange, with
// a host key callback that records the key and ABORTS: no authentication request
// (not even "none", which would name the user) and nothing else is sent.
func (Driver) Probe(ctx context.Context, c driver.Config) (driver.ProbeResult, error) {
	p, err := parse(c.Config)
	if err != nil {
		return driver.ProbeResult{}, err
	}
	if c.Dialer == nil {
		return driver.ProbeResult{}, driver.Errf(driver.ServerError)
	}
	var h hostState
	cfg := &ssh.ClientConfig{
		User: "launchpad-probe",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			h.mu.Lock()
			h.fp = ssh.FingerprintSHA256(key)
			h.mu.Unlock()
			return errProbed
		},
		Timeout: handshakeTimeout,
	}
	conn, err := backup.DialSSH(ctx, c.Dialer, p.addr, cfg) // bounded by cfg.Timeout and ctx
	if conn != nil {
		_ = conn.Close()
	}
	if fp, _, _ := h.get(); fp != "" {
		return driver.ProbeResult{Kind: "ssh", Fingerprint: fp}, nil
	}
	if err == nil {
		return driver.ProbeResult{}, driver.Errf(driver.ServerError)
	}
	if e := ctxEnded(ctx); e != nil { // the context ended: its closing of the socket is not "server error"
		return driver.ProbeResult{}, driver.MapNetError(e)
	}
	return driver.ProbeResult{}, driver.MapNetError(err)
}

// Open implements driver.Driver. It does not dial: every call opens its own
// connection (so a stall or a cancel closes only that call's connection). With
// no saved host key it refuses (host_key_changed: no host key is trusted yet).
func (Driver) Open(_ context.Context, c driver.Config) (driver.Remote, error) {
	p, err := parse(c.Config)
	if err != nil {
		return nil, err
	}
	sec, err := parseSecrets(c.Secrets)
	if err != nil {
		return nil, err
	}
	auth, err := sec.authMethods()
	if err != nil {
		return nil, err
	}
	if c.Dialer == nil {
		return nil, driver.Errf(driver.ServerError)
	}
	if p.HostKey == "" {
		return nil, driver.Errf(driver.HostKeyChanged)
	}
	return &remote{p: p, auth: auth, dialer: c.Dialer}, nil
}

type remote struct {
	p      parsed
	auth   []ssh.AuthMethod
	dialer *netguard.Dialer
}

// Close implements driver.Remote (nothing is kept open).
func (r *remote) Close() error { return nil }

// stage names the Test step a connect failure belongs to.
type stageErr struct {
	stage string // "connect" or "auth"
	err   error
}

// watch cancels a call when no byte moved for stallAfter.
type watch struct {
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func newWatch(parent context.Context) (context.Context, *watch) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &watch{cancel: cancel}
	w.timer = time.AfterFunc(stallAfter, func() { cancel(errStalled) })
	return ctx, w
}

func (w *watch) touch() { w.timer.Reset(stallAfter) }
func (w *watch) stop()  { w.timer.Stop(); w.cancel(nil) }

// counter counts and bounds the bytes read from the server and reports progress.
type counter struct {
	r        io.Reader
	w        *watch
	n        atomic.Int64
	limit    atomic.Int64 // -1: none
	exceeded atomic.Bool
	kill     func()
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.w.touch()
		total := c.n.Add(int64(n))
		if l := c.limit.Load(); l >= 0 && total > l {
			c.exceeded.Store(true)
			c.kill()
			return 0, errTooLarge
		}
	}
	return n, err
}

// session is one authenticated connection with the sftp client on it.
type session struct {
	r   *remote
	ctx context.Context
	w   *watch
	sc  *ssh.Client
	ss  *ssh.Session
	c   *gosftp.Client
	in  *counter
	// pending is a temp file this session made and could not remove or rename.
	pending string
	// swapBak/swapFinal: a replace moved the old file to swapBak and the call ended before
	// the swap was settled. finish settles it on a fresh connection (LAUN-154).
	swapBak, swapFinal string
	stop               func() bool
	close              sync.Once
}

func (s *session) shut() {
	s.close.Do(func() {
		s.stop()
		_ = s.sc.Close() // first: it ends the reads the sftp client waits on
		if s.c != nil {
			_ = s.c.Close()
		}
		if s.ss != nil {
			_ = s.ss.Close()
		}
		s.w.stop()
	})
}

// ctxEnded reports why a context is over, or nil. A connection deadline is the
// context's deadline, so a socket can time out a few microseconds before the
// context's own timer sets Err: a passed deadline counts as ended.
func ctxEnded(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
		return context.DeadlineExceeded
	}
	return nil
}

// connect dials through the guarded dialer, checks the pinned host key, signs in
// and starts the sftp subsystem. The whole call is under the stall watch.
func (r *remote) connect(parent context.Context) (*session, *stageErr) {
	ctx, w := newWatch(parent)
	var h hostState
	cfg := &ssh.ClientConfig{
		User:            r.p.Username,
		Auth:            r.auth,
		HostKeyCallback: pinCallback(r.p.HostKey, &h),
		Timeout:         handshakeTimeout,
	}
	sc, err := backup.DialSSH(ctx, r.dialer, r.p.addr, cfg)
	if err != nil {
		cause := context.Cause(ctx)
		w.stop()
		_, trusted, mismatch := h.get()
		switch {
		case errors.Is(cause, errStalled):
			return nil, &stageErr{"connect", driver.Errf(driver.Stalled)}
		case mismatch:
			// Definitive and security-relevant: never hidden as a timeout.
			return nil, &stageErr{"connect", driver.Errf(driver.HostKeyChanged)}
		case ctxEnded(parent) != nil:
			// The caller's context ended and closed the socket, in whatever phase after the
			// host key was checked: that is a timeout (or a cancel), never an auth or server error.
			st := "connect"
			if trusted {
				st = "auth"
			}
			return nil, &stageErr{st, driver.MapNetError(ctxEnded(parent))}
		case trusted:
			return nil, &stageErr{"auth", authError(err)}
		}
		return nil, &stageErr{"connect", driver.MapNetError(err)}
	}
	s := &session{r: r, ctx: ctx, w: w, sc: sc}
	s.stop = context.AfterFunc(ctx, func() { _ = sc.Close() })
	fail := func(err error) (*session, *stageErr) {
		e := s.fail(err)
		s.shut()
		return nil, &stageErr{"auth", e}
	}
	ss, err := sc.NewSession()
	if err != nil {
		return fail(err)
	}
	s.ss = ss
	if err := ss.RequestSubsystem("sftp"); err != nil {
		return fail(err)
	}
	wr, err := ss.StdinPipe()
	if err != nil {
		return fail(err)
	}
	rd, err := ss.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	er, err := ss.StderrPipe()
	if err != nil {
		return fail(err)
	}
	go func() { _, _ = io.Copy(io.Discard, er) }() // the server's stderr is never kept
	s.in = &counter{r: rd, w: w, kill: func() { _ = sc.Close() }}
	s.in.limit.Store(-1)
	c, err := gosftp.NewClientPipe(s.in, wr,
		gosftp.UseConcurrentWrites(true), gosftp.UseConcurrentReads(true),
		gosftp.MaxConcurrentRequestsPerFile(maxConcurrent))
	if err != nil {
		return fail(err)
	}
	s.c = c
	return s, nil
}

// authError maps a failure after the host key was accepted. The ssh package has
// no typed "bad credentials" error, so a failure that is not a timeout or a
// dropped connection is auth_failed. The text is never read.
func authError(err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return driver.Errf(driver.Timeout)
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.Is(err, context.Canceled):
		return driver.Errf(driver.ServerError)
	}
	return driver.Errf(driver.AuthFailed)
}

// fail turns any error of a call on this session into a safe error.
func (s *session) fail(err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	switch {
	case errors.As(err, &de):
		return de
	case errors.Is(context.Cause(s.ctx), errStalled):
		return driver.Errf(driver.Stalled)
	case s.in != nil && s.in.exceeded.Load():
		return driver.Errf(driver.TooLarge)
	}
	if e := ctxEnded(s.ctx); e != nil {
		return driver.MapNetError(e)
	}
	return mapSFTP(err)
}

// mapSFTP maps an sftp client error. Only the status CODE is used; the one text
// look is for "no space" / "quota" on the generic failure code, which OpenSSH
// sends with strerror() text. The text is compared and dropped.
func mapSFTP(err error) *driver.Error {
	if errors.Is(err, os.ErrNotExist) {
		return driver.Errf(driver.NotFound)
	}
	if errors.Is(err, os.ErrPermission) {
		return driver.Errf(driver.Forbidden)
	}
	var se *gosftp.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case 2:
			return driver.Errf(driver.NotFound)
		case 3:
			return driver.Errf(driver.Forbidden)
		case 14, 15: // NO_SPACE_ON_FILESYSTEM, QUOTA_EXCEEDED
			return driver.Errf(driver.QuotaExceeded)
		case 4:
			t := strings.ToLower(se.Error())
			if strings.Contains(t, "no space left") || strings.Contains(t, "quota exceeded") {
				return driver.Errf(driver.QuotaExceeded)
			}
		}
		return driver.ServerErr(int(se.Code))
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return driver.Errf(driver.ServerError)
	}
	return driver.MapNetError(err)
}

// path joins the folder and a name that passed driver.CheckName.
func (r *remote) path(name string) string {
	if r.p.Folder == "" {
		return name
	}
	return r.p.Folder + "/" + name
}

// ensureFolder makes the folder when it is missing. A folder that exists (also
// one the user cannot create in) costs one Stat.
func (s *session) ensureFolder() error {
	f := s.r.p.Folder
	if f == "" {
		return nil
	}
	fi, err := s.c.Stat(f)
	if err == nil {
		if !fi.IsDir() {
			return driver.Errf(driver.NotFound)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return s.fail(err)
	}
	if err := s.c.MkdirAll(f); err != nil {
		return s.fail(err)
	}
	return nil
}

// progress reports every read of the source to the stall watch.
type progress struct {
	r   io.Reader
	w   *watch
	ctx context.Context
	err error
}

func (p *progress) Read(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.r.Read(b)
	if n > 0 {
		p.w.touch()
	}
	if err != nil && err != io.EOF {
		p.err = err
	}
	return n, err
}

func randHex() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// swapReplace handles a server without posix-rename whose plain rename refused to
// replace the existing file (LAUN-154). ONLY when the refusal is the generic failure
// code (4), the temp file is still there and the final name is a regular file, the old
// file is renamed to a second temp name, the new file is renamed in, and the second
// name is removed. There is no moment in which the old copy exists under no name: if
// the second rename fails the old file is renamed back, and if even that fails (or the
// connection is gone) the old copy stays under its part name and finish tries once more
// on a fresh connection. first is the plain rename's error and is returned whenever the
// swap does not start.
func (s *session) swapReplace(tmp, final string, first error) error {
	var se *gosftp.StatusError
	if !errors.As(first, &se) || se.Code != 4 {
		return first
	}
	if _, e := s.c.Lstat(tmp); e != nil {
		return first
	}
	if fi, e := s.c.Lstat(final); e != nil || !fi.Mode().IsRegular() {
		return first
	}
	rnd, rerr := randToken()
	if rerr != nil {
		return first
	}
	bak := s.r.path(partPrefix + rnd)
	if bak == tmp || bak == final {
		return first
	}
	if err := s.c.Rename(final, bak); err != nil {
		return err // nothing moved
	}
	s.swapBak, s.swapFinal = bak, final
	// A rename keeps the old file's modification time, which can be days old; the sweep must
	// never take an old copy that a failed swap left under a part name the moment it is made.
	now := nowFn()
	_ = s.c.Chtimes(bak, now, now)
	if err := s.c.Rename(tmp, final); err != nil {
		if s.c.Rename(bak, final) == nil {
			s.swapBak, s.swapFinal = "", ""
		}
		return err
	}
	if s.c.Remove(bak) == nil {
		s.swapBak, s.swapFinal = "", ""
	}
	return nil
}

// isPartName reports whether a name is one of our part files: the prefix and then
// 1 to 64 lower case hex digits. It is never a backup name.
func isPartName(n string) bool {
	if !strings.HasPrefix(n, partPrefix) || remotename.Valid(n) {
		return false
	}
	rest := n[len(partPrefix):]
	if len(rest) == 0 || len(rest) > 64 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// sweepable is the ONE rule for what the sweep may remove: a regular file with a part
// name and a known modification time strictly before the cutoff.
func sweepable(fi os.FileInfo, cutoff time.Time) bool {
	mt := fi.ModTime()
	return isPartName(fi.Name()) && fi.Mode().IsRegular() && !mt.IsZero() && mt.Unix() > 0 && mt.Before(cutoff)
}

// sweepParts removes part files that a crash left behind (LAUN-154), best effort and
// bounded: only a REGULAR file whose name is a part name and whose modification time is
// known and more than partMaxAge old is removed; nothing else is touched. A listing
// that fails is ignored. The server's clock and ours may differ by minutes; the age is
// a day, so that does not matter.
func (s *session) sweepParts() {
	dir := s.r.p.Folder
	if dir == "" {
		dir = "."
	}
	s.in.n.Store(0)
	s.in.limit.Store(driver.MaxListBody)
	infos, err := s.c.ReadDirContext(s.ctx, dir)
	s.in.limit.Store(-1)
	if err != nil || len(infos) > maxEntries {
		return
	}
	cutoff := nowFn().Add(-partMaxAge)
	removed := 0
	for _, fi := range infos {
		if !sweepable(fi, cutoff) {
			continue
		}
		if removed >= maxSweep {
			return
		}
		if s.c.Remove(s.r.path(fi.Name())) == nil {
			removed++
		}
	}
}

// put writes size bytes to a temporary hidden name, checks the stored size and
// renames it to name (atomic with posix-rename; otherwise rename, and on a refusal to
// replace an existing file of that name the swap of swapReplace). A failed upload removes its temporary file when
// it can; one that cannot be removed is not a backup name and List never shows it.
func (s *session) put(name string, size int64, src io.Reader) (err error) {
	if size < 0 {
		return driver.Errf(driver.TooLarge)
	}
	if err := s.ensureFolder(); err != nil {
		return err
	}
	rnd, rerr := randToken()
	if rerr != nil {
		return driver.Errf(driver.ServerError)
	}
	tmp := s.r.path(partPrefix + rnd)
	f, ferr := s.c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if ferr != nil {
		return s.fail(ferr)
	}
	s.pending = tmp
	// The file is private to the user (best effort: a server may refuse setstat).
	_ = s.c.Chmod(tmp, 0o600)
	renamed, abandoned := false, false
	defer func() {
		// An abandoned copy still holds the file's lock inside Read: closing it would
		// wait for a source that never returns. The connection is already closed.
		if !renamed && !abandoned {
			_ = f.Close()
			if e := s.c.Remove(tmp); e == nil || errors.Is(e, os.ErrNotExist) {
				s.pending = ""
			}
		}
	}()
	// A *io.LimitedReader on the outside tells the client the length, so it can
	// keep several writes in flight; the stall watch is touched by every source read.
	pr := &progress{r: src, w: s.w, ctx: s.ctx}
	lr := io.LimitReader(pr, size+1)
	// The copy runs in its own goroutine so a stalled or cancelled call returns
	// even when the source blocks in Read (the goroutine ends when Read returns).
	type copied struct {
		n   int64
		err error
	}
	done := make(chan copied, 1)
	go func() { n, err := io.Copy(f, lr); done <- copied{n, err} }()
	var n int64
	var cerr error
	select {
	case c := <-done:
		n, cerr = c.n, c.err
	case <-s.ctx.Done():
		abandoned = true
		return s.fail(s.ctx.Err())
	}
	if pr.err != nil {
		return s.fail(pr.err)
	}
	if cerr != nil {
		return s.fail(cerr)
	}
	if n != size { // short or longer than declared: the dump changed while it was read
		return driver.Errf(driver.DumpMismatch)
	}
	if cerr := f.Close(); cerr != nil {
		return s.fail(cerr)
	}
	fi, serr := s.c.Stat(tmp)
	if serr != nil {
		return s.fail(serr)
	}
	if fi.Size() != size {
		return driver.Errf(driver.ServerError)
	}
	final := s.r.path(name)
	if _, ok := s.c.HasExtension(posixRename); ok && allowPosixRename {
		err = s.c.PosixRename(tmp, final)
	} else if err = s.c.Rename(tmp, final); err != nil {
		err = s.swapReplace(tmp, final, err)
	}
	if err != nil {
		return s.fail(err)
	}
	renamed = true
	s.pending = ""
	return nil
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

// list reads the folder. The server's bytes are capped at MaxListBody for the
// whole listing (a hostile endless listing is closed and too_large). Only
// regular files whose name passes remotename.Valid and starts with prefix are
// returned; a symlink, a directory or a stranger's file is dropped. A folder that
// does not exist yet lists as empty.
func (s *session) list(prefix string) ([]driver.Entry, error) {
	if !prefixOK(prefix) || len(prefix) > maxPrefixLen {
		return nil, driver.Errf(driver.NotFound)
	}
	dir := s.r.p.Folder
	if dir == "" {
		dir = "."
	}
	s.in.n.Store(0)
	s.in.limit.Store(driver.MaxListBody)
	infos, err := s.c.ReadDirContext(s.ctx, dir)
	s.in.limit.Store(-1)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !s.in.exceeded.Load() {
			return nil, nil
		}
		return nil, s.fail(err)
	}
	if len(infos) > maxEntries {
		return nil, driver.Errf(driver.TooLarge)
	}
	var out []driver.Entry
	for _, fi := range infos {
		n := fi.Name()
		if !fi.Mode().IsRegular() || !strings.HasPrefix(n, prefix) || !remotename.Valid(n) {
			continue
		}
		out = append(out, driver.Entry{Name: n, Size: fi.Size()})
	}
	return out, nil
}

// List implements driver.Remote.
func (r *remote) List(ctx context.Context, prefix string) ([]driver.Entry, error) {
	if !prefixOK(prefix) || len(prefix) > maxPrefixLen {
		return nil, driver.Errf(driver.NotFound)
	}
	s, se := r.connect(ctx)
	if se != nil {
		return nil, se.err
	}
	defer s.shut()
	return s.list(prefix)
}

// finish ends a session that may have left a temp file. When the connection was
// cut mid-copy (stall, cancel, deadline) the file could not be removed on it, so
// a fresh short connection (same pin, same guarded dialer, a new 10 s context)
// removes it. Best effort: the returned error of the call is never changed. A hard
// crash of this process can still leave one; List never shows it.
func (r *remote) finish(s *session) {
	p, bak, fin := s.pending, s.swapBak, s.swapFinal
	s.shut()
	if p == "" && bak == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()
	s2, se := r.connect(ctx)
	if se != nil {
		return
	}
	defer s2.shut()
	if p != "" {
		_ = s2.c.Remove(p)
	}
	if bak != "" {
		// An unsettled swap: put the old file back when the final name is empty; when the
		// new file is in place the old copy is only a leftover.
		if _, e := s2.c.Lstat(fin); errors.Is(e, os.ErrNotExist) {
			_ = s2.c.Rename(bak, fin)
		} else if e == nil {
			_ = s2.c.Remove(bak)
		}
	}
}

// Put implements driver.Remote.
func (r *remote) Put(ctx context.Context, name string, size int64, src io.Reader) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	if size < 0 {
		return driver.Errf(driver.TooLarge)
	}
	s, se := r.connect(ctx)
	if se != nil {
		return se.err
	}
	defer r.finish(s)
	if err := s.put(name, size, src); err != nil {
		return err
	}
	s.sweepParts()
	return nil
}

// reader owns the session of a Get until it is closed. The download runs as the
// client's concurrent WriteTo (up to maxConcurrent requests in flight) into a pipe,
// so it is not one round trip per 32 KiB. A consumer that pauses for stallAfter
// stops the requests, and the call is then `stalled`.
type reader struct {
	f    *gosftp.File
	s    *session
	pr   *io.PipeReader
	done chan struct{}
	size int64 // the size the server's Lstat gave
	got  int64
}

func newReader(s *session, f *gosftp.File, size int64) *reader {
	pr, pw := io.Pipe()
	b := &reader{f: f, s: s, pr: pr, done: make(chan struct{}), size: size}
	go func() {
		defer close(b.done)
		_, err := f.WriteTo(pw)
		pw.CloseWithError(err) // nil: the reader sees io.EOF
	}()
	return b
}

func (b *reader) Read(p []byte) (int, error) {
	n, err := b.pr.Read(p)
	b.got += int64(n)
	if err != nil && err != io.EOF {
		return n, b.s.fail(err)
	}
	if err == io.EOF && b.got != b.size {
		// A server that caps read lengths below the packet size, or a file that changed,
		// must not look like a clean stream.
		return n, driver.Errf(driver.ServerError)
	}
	return n, err
}

// closeFile stops the transfer and closes the remote file; the connection stays.
func (b *reader) closeFile() {
	_ = b.pr.Close() // the next pipe write fails, so WriteTo returns
	<-b.done
	_ = b.f.Close()
}

func (b *reader) Close() error {
	_ = b.pr.Close()
	b.s.shut() // ends any request WriteTo waits on
	<-b.done
	return nil
}

func (s *session) get(name string) (*reader, error) {
	p := s.r.path(name)
	fi, err := s.c.Lstat(p)
	if err != nil {
		return nil, s.fail(err)
	}
	if !fi.Mode().IsRegular() { // a symlink or a directory under a backup name is not a backup
		return nil, driver.Errf(driver.NotFound)
	}
	f, err := s.c.Open(p)
	if err != nil {
		return nil, s.fail(err)
	}
	return newReader(s, f, fi.Size()), nil
}

// Get implements driver.Remote. The caller caps the reader (driver.CapReader)
// and closes it; closing ends the connection.
func (r *remote) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := driver.CheckName(name); err != nil {
		return nil, err
	}
	s, se := r.connect(ctx)
	if se != nil {
		return nil, se.err
	}
	rd, err := s.get(name)
	if err != nil {
		s.shut()
		return nil, err
	}
	return rd, nil
}

func (s *session) del(name string) error {
	if err := s.c.Remove(s.r.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return s.fail(err)
	}
	return nil
}

// Delete implements driver.Remote. A missing name is not an error.
func (r *remote) Delete(ctx context.Context, name string) error {
	if err := driver.CheckName(name); err != nil {
		return err
	}
	s, se := r.connect(ctx)
	if se != nil {
		return se.err
	}
	defer s.shut()
	return s.del(name)
}

// Test implements driver.Remote: T16 #36 phase 2 on ONE connection. connect is
// the dial and the host key check, auth is the SSH sign-in and the start of the
// sftp subsystem (a server without sftp fails here with server_error), then
// write (the folder is made when missing), read (listing and download) and delete.
func (r *remote) Test(ctx context.Context, testName string) []driver.Step {
	var steps []driver.Step
	failAt := func(name string, err error) []driver.Step {
		return append(steps, driver.Step{Name: name, Code: driver.CodeOf(err)})
	}
	s, se := r.connect(ctx)
	if se != nil {
		if se.stage == "auth" {
			steps = append(steps, driver.Step{Name: "connect", OK: true})
		}
		return failAt(se.stage, se.err)
	}
	defer r.finish(s)
	steps = append(steps, driver.Step{Name: "connect", OK: true}, driver.Step{Name: "auth", OK: true})
	if err := driver.CheckName(testName); err != nil {
		return failAt("write", err)
	}
	body := []byte("launchpad backup test " + strconv.FormatInt(time.Now().UnixNano(), 16))
	if err := s.put(testName, int64(len(body)), bytes.NewReader(body)); err != nil {
		return failAt("write", err)
	}
	steps = append(steps, driver.Step{Name: "write", OK: true})
	es, err := s.list(testName)
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
	rd, err := s.get(testName)
	if err != nil {
		return failAt("read", err)
	}
	got, err := io.ReadAll(driver.CapReader(rd, driver.DownloadCap(int64(len(body)))))
	rd.closeFile() // not rd.Close: the connection is still needed for the delete
	if err != nil {
		return failAt("read", err)
	}
	if !bytes.Equal(got, body) {
		return failAt("read", driver.Errf(driver.ServerError))
	}
	steps = append(steps, driver.Step{Name: "read", OK: true})
	if err := s.del(testName); err != nil {
		return failAt("delete", err)
	}
	return append(steps, driver.Step{Name: "delete", OK: true})
}
