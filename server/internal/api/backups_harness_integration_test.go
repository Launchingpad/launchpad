//go:build backuptest

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backupapi"
	"devops/internal/dbq"
	"devops/internal/platform"
	"devops/internal/setup"
)

// LAUN-69: the backups API against the real router and Postgres
// (docs/internal/decisions-T16-backups.md #43). The destination drivers do not
// exist yet, so a test registers a fake under a REAL type name with the
// backuptest tag (decisions-T16 #16).

const bkp = "/api/v1/platform/backups"

var (
	bkKey1 = bytes.Repeat([]byte{0x33}, 32)
	bkKey2 = bytes.Repeat([]byte{0x44}, 32)
)

// The planted secrets: each appears in a request and must appear nowhere else.
const (
	bkSecretKey   = "planted-s3-secret-key-7f3a91"
	bkAccessKey   = "planted-s3-access-key-c2d8e4"
	bkPassword    = "planted-sftp-password-91b6aa"
	bkPrivateKey  = "planted-sftp-private-key-5e0f2c"
	bkAppSecret   = "planted-dropbox-app-secret-3b7d10"
	bkDropboxCode = "planted-dropbox-code-a4c1f8"
	bkTestSecret  = "planted-unsaved-test-secret-88de52"
)

// bkClock is the injected clock of recipient times.
type bkClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *bkClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *bkClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeDriver is a destination driver that validates a small config. It dials
// nothing: the API never calls Probe or Open.
type fakeDriver struct{ typ string }

type fakeConfig struct {
	URL      string `json:"url"`
	Bucket   string `json:"bucket"`
	Folder   string `json:"folder"`
	Username string `json:"username"`
	Region   string `json:"region"`
	TLSPin   string `json:"tlsPin"`
	HostKey  string `json:"hostKey"`
}

func (fakeDriver) parse(raw json.RawMessage) (fakeConfig, *url.URL, error) {
	var c fakeConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, nil, errors.New("bad config")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" {
		return c, nil, errors.New("bad url")
	}
	return c, u, nil
}

func (d fakeDriver) Validate(raw json.RawMessage) error { _, _, err := d.parse(raw); return err }

func (d fakeDriver) Identity(raw json.RawMessage) (driver.Identity, error) {
	c, u, err := d.parse(raw)
	if err != nil {
		return driver.Identity{}, err
	}
	return driver.Identity{Type: d.typ, Host: strings.ToLower(u.Host), Bucket: c.Bucket, Folder: strings.Trim(c.Folder, "/")}, nil
}

func (fakeDriver) Probe(context.Context, driver.Config) (driver.ProbeResult, error) {
	return driver.ProbeResult{}, errors.New("the API must not probe")
}

func (fakeDriver) Open(context.Context, driver.Config) (driver.Remote, error) {
	return nil, errors.New("the API must not open a remote")
}

type bkOpts struct {
	noKey     bool
	key       []byte
	types     []string // fake drivers to register; default s3, webdav, sftp
	noDrivers bool
	perMinute int
	burst     int
	mailReady func() bool
	// filesStored: the server keeps attachments (LAUN-80).
	filesStored func() bool
}

type bkEnv struct {
	*env
	svc   *backupapi.Service
	log   *slog.Logger
	clock *bkClock
	keys  *platform.BackupKeyring
	// admin: an unpinned admin PAT of chen, a platform admin; session: chen's web session.
	chen           dbq.User
	admin, session string
	// admin2: a second platform admin (dana).
	dana   dbq.User
	admin2 string
}

func newBkEnv(t *testing.T, o bkOpts) *bkEnv {
	t.Helper()
	if o.key == nil && !o.noKey {
		o.key = bkKey1
	}
	if o.types == nil && !o.noDrivers {
		o.types = []string{"s3", "webdav", "sftp"}
	}
	if o.perMinute == 0 {
		o.perMinute, o.burst = 1_000_000, 1_000_000
	}
	for _, typ := range o.types {
		driver.RegisterForTest(t, typ, fakeDriver{typ: typ})
	}
	bk := &bkEnv{clock: &bkClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}}
	bk.env = newEnvWith(t, envOpts{logLevel: slog.LevelDebug, configure: func(e *env, s *Server) {
		bk.log = slog.New(slog.NewTextHandler(io.MultiWriter(tWriter{t}, e.logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
		svc, err := backupapi.New(e.pool, bk.log, backupapi.Options{
			Key: o.key, Clock: bk.clock.Now, ActionPerMinute: o.perMinute, ActionBurst: o.burst, MailReady: o.mailReady, FilesStored: o.filesStored})
		if err != nil {
			t.Fatal(err)
		}
		bk.svc = svc
		s.Backups = svc
	}})
	if o.key != nil {
		k, err := platform.NewBackupKeyring(o.key)
		if err != nil {
			t.Fatal(err)
		}
		bk.keys = k
	}
	ctx := context.Background()
	if _, err := setup.GrantPlatformAdmin(ctx, bk.pool, "chen"); err != nil {
		t.Fatal(err)
	}
	bk.chen = bk.user("chen")
	bk.admin, _ = bk.newToken(bk.chen.ID, tokenOpts{})
	bk.session, _ = bk.loginAs(chenEmail)
	bk.dana = bk.newHuman("dana", "dana@example.com", "member")
	if _, err := setup.GrantPlatformAdmin(ctx, bk.pool, "dana"); err != nil {
		t.Fatal(err)
	}
	bk.admin2, _ = bk.newToken(bk.dana.ID, tokenOpts{})
	return bk
}

// newKey makes an age public key.
func (bk *bkEnv) newKey() string {
	bk.t.Helper()
	_, pub, err := backup.GenerateKeyPair()
	if err != nil {
		bk.t.Fatal(err)
	}
	return pub
}

// addRecipient adds a key as token and returns the recipient (201).
func (bk *bkEnv) addRecipient(token string) backupapi.Recipient {
	bk.t.Helper()
	var r backupapi.Recipient
	bk.must(201, bk.do("POST", bkp+"/recipients", token, map[string]any{"publicKey": bk.newKey()})).json(bk.t, &r)
	return r
}

func (bk *bkEnv) recipients() backupapi.RecipientList {
	bk.t.Helper()
	var l backupapi.RecipientList
	bk.must(200, bk.do("GET", bkp+"/recipients", bk.admin, nil)).json(bk.t, &l)
	return l
}

// signing stores a sealed signing key, as offsite would at its first start.
func (bk *bkEnv) signing() {
	bk.t.Helper()
	blob, err := bk.keys.Seal(platform.BackupColSigningKey, platform.BackupSettingsOwner, []byte("signing-private-key-fixture"))
	if err != nil {
		bk.t.Fatal(err)
	}
	n, err := bk.q.ClaimBackupSigningKey(context.Background(), dbq.ClaimBackupSigningKeyParams{
		SigningPublic: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), SigningPrivateEnc: blob})
	if err != nil || n != 1 {
		bk.t.Fatalf("store the signing key: %d %v", n, err)
	}
}

var secretsOf = map[string]map[string]any{
	"s3":      {"accessKey": bkAccessKey, "secretKey": bkSecretKey},
	"webdav":  {"password": bkPassword},
	"sftp":    {"password": bkPassword},
	"dropbox": {"appKey": "app-key-fixture", "appSecret": bkAppSecret},
}

func cfgFor(name string) map[string]any {
	return map[string]any{"url": "https://" + name + ".example.com", "bucket": "b-" + name, "folder": "backups"}
}

// dest makes a destination (disabled) with the secrets of its type.
func (bk *bkEnv) dest(name, typ string) backupapi.Destination {
	bk.t.Helper()
	var d backupapi.Destination
	bk.must(201, bk.do("POST", bkp+"/destinations", bk.admin, map[string]any{
		"name": name, "type": typ, "config": cfgFor(name), "secrets": secretsOf[typ]})).json(bk.t, &d)
	return d
}

func (bk *bkEnv) patch(id any, body map[string]any) resp {
	bk.t.Helper()
	return bk.do("PATCH", fmt.Sprintf("%s/destinations/%v", bkp, id), bk.admin, body)
}

// ready gives the install a signing key and one active recipient.
func (bk *bkEnv) ready() backupapi.Recipient {
	bk.t.Helper()
	bk.signing()
	return bk.addRecipient(bk.admin)
}

// enabled makes a destination and enables it (the install must be ready).
func (bk *bkEnv) enabled(name, typ string) backupapi.Destination {
	bk.t.Helper()
	d := bk.dest(name, typ)
	var out backupapi.Destination
	bk.must(200, bk.patch(d.ID, map[string]any{"enabled": true})).json(bk.t, &out)
	return out
}

// forceEnable sets enabled in SQL, for tests that do not exercise the enable checks.
func (bk *bkEnv) forceEnable(id any) {
	bk.t.Helper()
	if _, err := bk.pool.Exec(context.Background(), `UPDATE backup_destinations SET enabled = true, enabled_since = now() WHERE id = $1`, id); err != nil {
		bk.t.Fatal(err)
	}
}

func (bk *bkEnv) changes() []backupapi.ChangeEntry {
	bk.t.Helper()
	var p struct {
		Items []backupapi.ChangeEntry `json:"items"`
	}
	bk.must(200, bk.do("GET", bkp+"/changes?limit=200", bk.admin, nil)).json(bk.t, &p)
	return p.Items
}

// kinds lists the change kinds, oldest first.
func (bk *bkEnv) kinds() []string {
	items := bk.changes()
	out := make([]string, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		out = append(out, items[i].Kind)
	}
	return out
}

func (bk *bkEnv) countRows(table string) int {
	bk.t.Helper()
	var n int
	if err := bk.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		bk.t.Fatal(err)
	}
	return n
}

func newUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// otherService is a second server on the same database with another key (or none).
func (bk *bkEnv) otherService(key []byte) *backupapi.Service {
	bk.t.Helper()
	svc, err := backupapi.New(bk.pool, bk.log, backupapi.Options{Key: key, Clock: bk.clock.Now})
	if err != nil {
		bk.t.Fatal(err)
	}
	return svc
}
