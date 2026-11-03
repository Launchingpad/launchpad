package offsite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// otherRunLimitTime bounds a probe, test or exchange (not a verify, which downloads).
const otherRunTime = 10 * time.Minute

// execOther runs a claimed probe, test, exchange or verify run.
func (w *Worker) execOther(parent context.Context, run dbq.BackupRun) {
	sc := w.begin(parent, run.ID)
	defer sc.stop()
	limit := otherRunTime
	if run.Trigger == "verify" {
		limit = w.o.RunTimeout
	}
	ctx, cancel := context.WithTimeoutCause(sc.ctx, limit, errRunTimeout)
	defer cancel()
	var (
		res result
		err error
	)
	switch run.Trigger {
	case "probe":
		res, err = w.doProbe(ctx, run)
	case "test":
		res, err = w.doTest(ctx, run)
	case "exchange":
		res, err = w.doExchange(ctx, run)
	case "verify":
		res, err = w.doVerify(ctx, run)
	default:
		err = internal(errors.New("unknown trigger"))
	}
	w.finish(parent, sc, run, res, err)
}

// stepOut is one entry of backup_runs.steps. A probe run has one entry with the
// fingerprint; a test run has connect, auth, write, read, delete.
type stepOut struct {
	Name          string `json:"name"`
	OK            bool   `json:"ok"`
	Code          string `json:"code,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	ChainVerified *bool  `json:"chainVerified,omitempty"`
}

func stepsJSON(s []stepOut) []byte {
	b, _ := json.Marshal(s)
	return b
}

// target is what a probe, test or exchange talks to.
type target struct {
	typ       string
	cfg       json.RawMessage
	secrets   json.RawMessage // opened secrets of the destination, or of an unsaved form
	runSecret map[string]string
	confirmed string
	destID    *uuid.UUID
	installID string
}

// resolveTarget reads the destination (saved), or the form in test_config with
// its sealed secrets (unsaved), following the contract of decisions-T16 ticket 2 #13.
func (w *Worker) resolveTarget(ctx context.Context, run dbq.BackupRun) (target, error) {
	q := dbq.New(w.pool)
	s, err := q.GetBackupSettings(ctx)
	if err != nil {
		return target{}, internal(err)
	}
	t := target{installID: s.InstallID, secrets: json.RawMessage(`{}`)}
	if run.TestSecretsEnc != nil {
		pt, err := w.keys.Open(platform.BackupColRunTestSecrets, run.ID.String(), run.TestSecretsEnc)
		if err != nil {
			return target{}, driver.Errf(driver.SecretsUnreadable)
		}
		if err := json.Unmarshal(pt, &t.runSecret); err != nil {
			return target{}, driver.Errf(driver.SecretsUnreadable)
		}
		t.secrets = pt
	}
	if run.DestinationID == nil {
		var tc struct {
			Type                 string          `json:"type"`
			Config               json.RawMessage `json:"config"`
			ConfirmedFingerprint string          `json:"confirmedFingerprint"`
		}
		if err := json.Unmarshal(run.TestConfig, &tc); err != nil || tc.Type == "" || len(tc.Config) == 0 {
			return target{}, internal(errors.New("an unsaved run without a usable test_config"))
		}
		t.typ, t.cfg, t.confirmed = tc.Type, tc.Config, tc.ConfirmedFingerprint
		return t, nil
	}
	d, err := q.GetBackupDestination(ctx, *run.DestinationID)
	if err != nil {
		return target{}, internal(err)
	}
	t.typ, t.cfg, t.destID = d.Type, d.Config, &d.ID
	if run.Trigger != "exchange" { // an exchange uses the app key and secret the run carries
		if t.secrets, err = w.openDestinationSecrets(d); err != nil {
			return target{}, err
		}
	}
	return t, nil
}

// anchorField is where a trust anchor lives in config: tlsPin, or hostKey for sftp.
func anchorField(typ string) string {
	if typ == "sftp" {
		return "hostKey"
	}
	return "tlsPin"
}

func (w *Worker) driverFor(typ string) (driver.Driver, error) {
	d, ok := driver.Lookup(typ)
	if !ok {
		return nil, driver.Errf(driver.ServerError)
	}
	return d, nil
}

// doProbe is phase 1 of #36: the handshake with no credentials; the fingerprint and
// whether the chain verified go into steps.
func (w *Worker) doProbe(ctx context.Context, run dbq.BackupRun) (result, error) {
	t, err := w.resolveTarget(ctx, run)
	if err != nil {
		return result{steps: stepsJSON([]stepOut{{Name: "probe", Code: string(driver.CodeOf(err))}})}, err
	}
	drv, err := w.driverFor(t.typ)
	if err != nil {
		return result{steps: stepsJSON([]stepOut{{Name: "probe", Code: string(driver.CodeOf(err))}})}, err
	}
	pr, err := drv.Probe(ctx, driver.Config{Config: t.cfg, Dialer: w.dialer,
		AllowPlainHTTP: w.plainHTTPAllowed(ctx, drv, t.cfg)})
	if err != nil {
		return result{steps: stepsJSON([]stepOut{{Name: "probe", Code: string(driver.CodeOf(err))}})}, err
	}
	cv := pr.ChainVerified
	return result{steps: stepsJSON([]stepOut{{Name: "probe", OK: true, Kind: pr.Kind, Fingerprint: pr.Fingerprint, ChainVerified: &cv}})}, nil
}

// doTest is phase 2 of #36. It starts only when the certificate or host key is
// trusted: a valid chain for the host, a stored pin or host key (the driver checks
// it), or the admin's confirmed fingerprint equal to the one just probed. A
// confirmed fingerprint never overrides a trust anchor the config already has.
func (w *Worker) doTest(ctx context.Context, run dbq.BackupRun) (result, error) {
	fail := func(name string, err error) (result, error) {
		return result{steps: stepsJSON([]stepOut{{Name: name, Code: string(driver.CodeOf(err))}})}, err
	}
	t, err := w.resolveTarget(ctx, run)
	if err != nil {
		return fail("connect", err)
	}
	drv, err := w.driverFor(t.typ)
	if err != nil {
		return fail("connect", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(t.cfg, &cfg); err != nil || cfg == nil {
		return fail("connect", internal(errors.New("config is not an object")))
	}
	field := anchorField(t.typ)
	if have, _ := cfg[field].(string); have == "" {
		pr, err := drv.Probe(ctx, driver.Config{Config: t.cfg, Dialer: w.dialer,
			AllowPlainHTTP: w.plainHTTPAllowed(ctx, drv, t.cfg)})
		if err != nil {
			return fail("connect", err)
		}
		// "plain" is an http:// destination (listed range only): there is no certificate
		// to pin, and the driver enforces the listed range at dial time.
		trusted := pr.Kind == "plain" || pr.Kind == "tls" && pr.ChainVerified
		if !trusted && t.confirmed != "" && t.confirmed == pr.Fingerprint {
			cfg[field] = pr.Fingerprint
			if t.cfg, err = json.Marshal(cfg); err != nil {
				return fail("connect", internal(err))
			}
			trusted = true
		}
		if !trusted {
			code := driver.TLSUntrusted
			if pr.Kind == "ssh" {
				code = driver.HostKeyChanged // no host key is trusted yet
			}
			return fail("connect", driver.Errf(code))
		}
	}
	remote, err := drv.Open(ctx, driver.Config{Config: t.cfg, Secrets: t.secrets, Dialer: w.dialer,
		AllowPlainHTTP: w.plainHTTPAllowed(ctx, drv, t.cfg)})
	if err != nil {
		return fail("connect", err)
	}
	defer remote.Close()
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fail("write", internal(err))
	}
	name, err := remotename.TestName(t.installID, hex.EncodeToString(rnd[:]))
	if err != nil {
		return fail("write", internal(err))
	}
	steps := remote.Test(ctx, name)
	out := make([]stepOut, 0, len(steps))
	var first error
	for _, s := range steps {
		so := stepOut{Name: s.Name, OK: s.OK}
		if !s.OK {
			so.Code = string(s.Code)
			if first == nil {
				first = driver.Errf(s.Code)
			}
		}
		out = append(out, so)
	}
	if first == nil && len(out) == 0 {
		first = driver.Errf(driver.ServerError)
	}
	return result{steps: stepsJSON(out)}, first
}

// doExchange turns a Dropbox authorization code into a refresh token, once, and
// merges it into the destination's secrets under the row lock (#9, #15). The
// destination stays disabled; a failed exchange needs a new code.
func (w *Worker) doExchange(ctx context.Context, run dbq.BackupRun) (result, error) {
	fail := func(err error) (result, error) {
		return result{steps: stepsJSON([]stepOut{{Name: "exchange", Code: string(driver.CodeOf(err))}})}, err
	}
	t, err := w.resolveTarget(ctx, run)
	if err != nil {
		return fail(err)
	}
	if t.destID == nil || t.runSecret["code"] == "" {
		return fail(driver.Errf(driver.SecretsUnreadable))
	}
	drv, err := w.driverFor(t.typ)
	if err != nil {
		return fail(err)
	}
	ex, ok := drv.(driver.Exchanger)
	if !ok {
		return fail(driver.Errf(driver.ServerError))
	}
	app, _ := json.Marshal(map[string]string{"appKey": t.runSecret["appKey"], "appSecret": t.runSecret["appSecret"]})
	token, err := ex.Exchange(ctx, driver.Config{Config: t.cfg, Secrets: app, Dialer: w.dialer}, t.runSecret["code"])
	if err != nil {
		return fail(err)
	}
	if token == "" {
		return fail(driver.Errf(driver.ServerError))
	}
	if err := w.MergeSecrets(ctx, *t.destID, func(m map[string]string) { m["refreshToken"] = token }); err != nil {
		return fail(err)
	}
	return result{steps: stepsJSON([]stepOut{{Name: "exchange", OK: true}})}, nil
}

// MergeSecrets reads a destination's secrets, applies fn and seals them again under
// the row lock (SELECT ... FOR UPDATE), so a driver's token refresh and an API PATCH
// never lose each other's write (decisions-T16 #15, T9 #15). It writes only
// secrets_enc and secrets_set (the role's column grant) and never key_check.
func (w *Worker) MergeSecrets(ctx context.Context, id uuid.UUID, fn func(map[string]string)) error {
	return database.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		d, err := q.GetBackupDestinationForUpdate(ctx, id)
		if err != nil {
			return internal(err)
		}
		m := map[string]string{}
		if d.SecretsEnc != nil {
			pt, err := w.keys.Open(platform.BackupColDestinationSecrets, d.ID.String(), d.SecretsEnc)
			if err != nil || json.Unmarshal(pt, &m) != nil {
				return driver.Errf(driver.SecretsUnreadable)
			}
		}
		fn(m)
		names := make([]string, 0, len(m))
		for k, v := range m {
			if v != "" {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		var blob []byte
		if len(names) > 0 {
			pt, err := json.Marshal(m)
			if err != nil {
				return internal(err)
			}
			if blob, err = w.keys.Seal(platform.BackupColDestinationSecrets, d.ID.String(), pt); err != nil {
				return internal(err)
			}
		}
		return q.SetBackupDestinationSecretsByWorker(ctx, dbq.SetBackupDestinationSecretsByWorkerParams{ID: id, SecretsEnc: blob, SecretsSet: names})
	})
}

// ageHeader starts every age file (the v1 format).
const ageHeader = "age-encryption.org/v1\n"

// doVerify downloads the newest uploaded object of a destination and checks its
// sha256 and size against the DATABASE (backup_runs, not the remote manifest, #22)
// and that it starts with the age header (#42). It does not prove the object
// decrypts: the server has no private key.
func (w *Worker) doVerify(ctx context.Context, run dbq.BackupRun) (result, error) {
	q := dbq.New(w.pool)
	if run.DestinationID == nil {
		return result{}, internal(errors.New("a verify run without a destination"))
	}
	dest, err := q.GetBackupDestination(ctx, *run.DestinationID)
	if err != nil {
		return result{}, internal(err)
	}
	last, err := q.GetNewestSucceededBackupRun(ctx, run.DestinationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return result{}, driver.Errf(driver.NotFound)
	}
	if err != nil {
		return result{}, internal(err)
	}
	secrets, err := w.openDestinationSecrets(dest)
	if err != nil {
		return result{}, err
	}
	drv, err := w.driverFor(dest.Type)
	if err != nil {
		return result{}, err
	}
	remote, err := drv.Open(ctx, driver.Config{Config: dest.Config, Secrets: secrets, Dialer: w.dialer,
		AllowPlainHTTP: w.plainHTTPAllowed(ctx, drv, dest.Config)})
	if err != nil {
		return result{}, err
	}
	defer remote.Close()
	name := *last.RemoteName
	if !remotename.Valid(name) || last.Bytes == nil || last.Sha256 == nil {
		return result{}, internal(errors.New("a succeeded run without a usable remote name"))
	}
	rc, err := remote.Get(ctx, name)
	if err != nil {
		return result{}, err
	}
	defer rc.Close()
	h := sha256.New()
	head := make([]byte, 0, len(ageHeader))
	body := io.TeeReader(driver.CapReader(rc, driver.DownloadCap(*last.Bytes)), h)
	var n int64
	buf := make([]byte, 64<<10)
	for {
		m, rerr := body.Read(buf)
		if len(head) < len(ageHeader) {
			head = append(head, buf[:min(m, len(ageHeader)-len(head))]...)
		}
		n += int64(m)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return result{}, driver.MapNetError(rerr)
		}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if n != *last.Bytes || sum != *last.Sha256 || string(head) != ageHeader {
		return result{}, driver.Errf(driver.ObjectMismatch)
	}
	return result{bytes: &n, sha: &sum, remote: &name}, nil
}

// plainHTTPAllowed reports whether an http:// URL may be used for a destination:
// only when the host's every address is in an operator-listed range (#34). It is a
// convenience for the driver; the dial-time guard still checks each connection.
func (w *Worker) plainHTTPAllowed(ctx context.Context, d driver.Driver, cfg json.RawMessage) bool {
	if len(w.o.Policy.Private) == 0 {
		return false
	}
	id, err := d.Identity(cfg)
	if err != nil || id.Host == "" {
		return false
	}
	host := id.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lookup := w.o.Lookup
		if lookup == nil {
			lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		lctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		if addrs, err = lookup(lctx, host); err != nil {
			return false
		}
	}
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !backup.OperatorListed(w.o.Policy, a) {
			return false
		}
	}
	return true
}
