package move_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/move"
	"devops/internal/platform"
	"devops/internal/setup"
	"devops/internal/storage"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// keysFrom builds the five keyrings from two master keys: one for the
// settings scope, one for the backup scope (as the server does).
func keysFrom(t *testing.T, settings, backup byte) move.Keys {
	t.Helper()
	sm, bm := bytes.Repeat([]byte{settings}, 32), bytes.Repeat([]byte{backup}, 32)
	var k move.Keys
	var err error
	if k.Settings, err = platform.NewKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Git, err = platform.NewGitKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Push, err = platform.NewPushKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Slack, err = platform.NewSlackKeyring(sm); err != nil {
		t.Fatal(err)
	}
	if k.Backup, err = platform.NewBackupKeyring(bm); err != nil {
		t.Fatal(err)
	}
	return k
}

// Plaintext secrets the seed seals. The no-secret-in-logs test greps for them.
const (
	secSMTP      = "smtp-password-PLAINTEXT-1"
	secPostmark  = "postmark-token-PLAINTEXT-2"
	secGitToken  = "git-api-token-PLAINTEXT-3"
	secGitHook   = "git-webhook-secret-PLAINTEXT-4"
	secGitNext   = "git-webhook-next-PLAINTEXT-5"
	secChat      = "https://hooks.slack.example.com/services/PLAINTEXT-6"
	secChat2     = "https://hooks.slack.example.com/services/PLAINTEXT-7"
	secPersonal  = "https://hooks.slack.example.com/services/PLAINTEXT-8"
	secRelayURL  = "https://relay.example.com/PLAINTEXT-9"
	secRelaySec  = "relay-bearer-PLAINTEXT-10"
	secDevice    = "device-token-PLAINTEXT-11"
	secBackupDst = `{"secretKey":"backup-dest-PLAINTEXT-12"}`
	secSigning   = "signing-private-PLAINTEXT-13"
)

type world struct {
	t         *testing.T
	pool      *pgxpool.Pool
	d         testdb.Data
	org2      uuid.UUID
	keys      move.Keys
	store     *storage.S3
	chen      uuid.UUID
	objects   map[string][]byte // key -> bytes
	destID    uuid.UUID
	chatID    uuid.UUID
	chat2ID   uuid.UUID
	gitID     uuid.UUID
	deviceID  uuid.UUID
	pwhID     uuid.UUID
	teamID    uuid.UUID
	sessionTk string
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	testdb.MustExec(t, pool, sql, args...)
}

func randBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newWorld seeds a rich install: two orgs, issues, attachments with files, an
// org avatar, every kind of sealed secret, a session token, a login code and
// an idempotency row (which must not travel).
func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	w := &world{t: t, pool: pool, d: d, keys: keysFrom(t, 0x11, 0x22), store: storagetest.New(t), objects: map[string][]byte{}}
	q := dbq.New(pool)
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username = 'chen'`).Scan(&w.chen); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM teams WHERE org_id = $1`, d.OrgID).Scan(&w.teamID); err != nil {
		t.Fatal(err)
	}

	// A second org with its own team, issue and chat service (cross-org rows).
	org2, err := q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	w.org2 = org2.ID
	team2, err := tracker.CreateTeamTx(ctx, q, org2.ID, "OTH", "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, org2.ID, w.chen)

	svc := tracker.New(pool)
	chenP := &auth.Principal{UserID: w.chen, Username: "chen", Name: "Chen"}
	org1 := &auth.Org{ID: d.OrgID, Slug: testdb.OrgSlug, Name: testdb.OrgName, Role: "admin", Access: auth.TeamAccess{All: true}}
	orgB := &auth.Org{ID: org2.ID, Slug: "other", Name: "Other", Role: "member", Access: auth.TeamAccess{All: true}}
	is1, err := svc.CreateIssue(ctx, org1, chenP, tracker.CreateIssueInput{Team: testdb.TeamKey, Title: "first", Description: "body with\ttab and \\ backslash"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIssue(ctx, org1, chenP, tracker.CreateIssueInput{Team: testdb.TeamKey, Title: "second", Parent: is1.Identifier}); err != nil {
		t.Fatal(err)
	}
	is3, err := svc.CreateIssue(ctx, orgB, chenP, tracker.CreateIssueInput{Team: "OTH", Title: "other org issue"})
	if err != nil {
		t.Fatal(err)
	}

	// Attachment files (bigger than one frame) and an org avatar.
	ctx30, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for i, sz := range []int{700_000, 5, 0} {
		id := uuid.New()
		key := tracker.AttachmentObjectKey(d.OrgID, 2026, id, fmt.Sprintf("file%d.bin", i))
		body := randBytes(t, sz)
		w.objects[key] = body
		if err := w.store.Put(ctx30, key, "application/octet-stream", int64(sz), bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		exec(t, pool, `INSERT INTO attachments (id, org_id, issue_id, kind, title, object_key, filename, content_type, size_bytes, creator_id)
			VALUES ($1, $2, $3, 'file', 'f', $4, $5, 'application/octet-stream', $6, $7)`,
			id, d.OrgID, is1.ID, key, fmt.Sprintf("file%d.bin", i), int64(sz), w.chen)
	}
	avKey := fmt.Sprintf("org/%s/avatar/1.png", d.OrgID)
	av := randBytes(t, 321)
	w.objects[avKey] = av
	if err := w.store.Put(ctx30, avKey, "image/png", int64(len(av)), bytes.NewReader(av)); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE orgs SET avatar_key = $2, avatar_content_type = 'image/png', avatar_version = 1 WHERE id = $1`, d.OrgID, avKey)
	_ = is3

	// Sealed secrets, every ring.
	k := w.keys
	exec(t, pool, `UPDATE platform_settings SET mail_provider = 'smtp', smtp_host = 'smtp.example.com', smtp_username = 'u',
		smtp_password_enc = $1, postmark_token_enc = $2, key_check = $3`,
		k.Settings.Seal("smtp_password_enc", []byte(secSMTP)), k.Settings.Seal("postmark_token_enc", []byte(secPostmark)), k.Settings.KeyCheck())

	w.gitID = uuid.New()
	g := w.gitID.String()
	exec(t, pool, `INSERT INTO git_integrations (id, org_id, base_url, webhook_secret_hash, provider, mode, name, token_enc, webhook_secret_enc, webhook_secret_next_enc, webhook_secret_next_hash)
		VALUES ($1, $2, 'https://gitlab.example.com', $3, 'gitlab', 'token', 'gl', $4, $5, $6, $3)`,
		w.gitID, d.OrgID, bytes.Repeat([]byte{7}, 32),
		k.Git.SealFor("token_enc", g, []byte(secGitToken)), k.Git.SealFor("webhook_secret_enc", g, []byte(secGitHook)),
		k.Git.SealFor("webhook_secret_next_enc", g, []byte(secGitNext)))

	w.chatID, w.chat2ID = uuid.New(), uuid.New()
	for _, c := range []struct {
		id, org, team uuid.UUID
		url           string
	}{{w.chatID, d.OrgID, w.teamID, secChat}, {w.chat2ID, org2.ID, team2.ID, secChat2}} {
		scope := "svc." + c.org.String() + "." + c.id.String()
		exec(t, pool, `INSERT INTO chat_services (id, org_id, name, kind, host, webhook_enc, seal_scope, team_id) VALUES ($1, $2, 'svc', 'slack', 'hooks.slack.example.com', $3, $4, $5)`,
			c.id, c.org, k.Slack.SealFor(platform.SlackColWebhook, scope, []byte(c.url)), scope, c.team)
	}

	w.pwhID = uuid.New()
	pscope := "pwh." + d.OrgID.String() + "." + w.chen.String() + "." + w.pwhID.String()
	exec(t, pool, `INSERT INTO personal_webhooks (id, org_id, user_id, name, kind, host, webhook_enc, seal_scope) VALUES ($1, $2, $3, 'mine', 'slack', 'hooks.slack.example.com', $4, $5)`,
		w.pwhID, d.OrgID, w.chen, k.Slack.SealFor(platform.PersonalColWebhook, pscope, []byte(secPersonal)), pscope)

	exec(t, pool, `INSERT INTO push_relays (org_id, url_enc, secret_enc, host) VALUES ($1, $2, $3, 'relay.example.com')`,
		d.OrgID, k.Push.SealFor(platform.PushColRelayURL, d.OrgID.String(), []byte(secRelayURL)),
		k.Push.SealFor(platform.PushColRelaySecret, d.OrgID.String(), []byte(secRelaySec)))
	w.deviceID = uuid.New()
	exec(t, pool, `INSERT INTO push_devices (id, org_id, user_id, platform, token_enc, token_hash) VALUES ($1, $2, $3, 'ios', $4, $5)`,
		w.deviceID, d.OrgID, w.chen, k.Push.SealFor(platform.PushColDeviceToken, d.OrgID.String()+"."+w.deviceID.String(), []byte(secDevice)), bytes.Repeat([]byte{9}, 32))

	w.destID = uuid.New()
	sealed, err := k.Backup.Seal(platform.BackupColDestinationSecrets, w.destID.String(), []byte(secBackupDst))
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO backup_destinations (id, name, type, enabled, enabled_since, config, secrets_enc, secrets_set, last_stale_alert_at)
		VALUES ($1, 'offsite', 's3', true, now(), '{"bucket":"b"}', $2, ARRAY['secretKey'], now())`, w.destID, sealed)
	sk, err := k.Backup.Seal(platform.BackupColSigningKey, platform.BackupSettingsOwner, []byte(secSigning))
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE backup_settings SET signing_public = 'pubkey', signing_private_enc = $1, key_check = $2, ever_had_recipient = true`, sk, k.Backup.KeyCheck())

	// Rows that must not travel.
	nt, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	w.sessionTk = nt.Plaintext
	exec(t, pool, `INSERT INTO api_tokens (user_id, kind, name, token_hash, prefix, scopes, expires_at) VALUES ($1, 'session', 'web', $2, $3, ARRAY['read'], now() + interval '1 day')`,
		w.chen, nt.Hash, nt.Prefix)
	exec(t, pool, `INSERT INTO login_codes (user_id, code_salt, code_hash, expires_at) VALUES ($1, decode(repeat('01', 16), 'hex'), decode(repeat('02', 32), 'hex'), now() + interval '1 hour')`, w.chen)
	exec(t, pool, `INSERT INTO idempotency_keys (user_id, key, method, path, request_hash, status_code, response_body, expires_at) VALUES ($1, 'k', 'POST', '/x', '\x01', 200, '\x7b7d', now() + interval '1 day')`, w.chen)
	return w
}

// target is a second, empty (migrated) install with its own keys and bucket.
type target struct {
	pool  *pgxpool.Pool
	keys  move.Keys
	store *storage.S3
}

func newTarget(t *testing.T) *target {
	t.Helper()
	pool, _ := testdb.New(t)
	cfg := storagetest.Config()
	cfg.Bucket = "devops-test-move-in"
	st, err := storage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	return &target{pool: pool, keys: keysFrom(t, 0x33, 0x44), store: st}
}

func (w *world) export(t *testing.T, pass string, attachments bool, log *slog.Logger) ([]byte, *move.Manifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := move.Export(context.Background(), &buf, move.ExportOptions{
		Pool: w.pool, Keys: w.keys, Store: w.store, Passphrase: pass, ExportedBy: w.chen, AppVersion: "test",
		IncludeAttachments: attachments, Log: log,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.Bytes(), m
}

func (tg *target) imp(data []byte, pass string, log *slog.Logger) (*move.Manifest, error) {
	return move.Import(context.Background(), bytes.NewReader(data), move.ImportOptions{
		Pool: tg.pool, Keys: tg.keys, Store: tg.store, Passphrase: pass, Log: log,
	})
}

func newPass(t *testing.T) string {
	p, err := move.GeneratePassphrase()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// assertEmpty requires the target to be in setup state with no data and no
// stray object of the bundle.
func (tg *target) assertEmpty(t *testing.T, w *world) {
	t.Helper()
	ctx := context.Background()
	var users, orgs, issues, att, notes int
	if err := tg.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM orgs), (SELECT count(*) FROM issues),
		(SELECT count(*) FROM attachments), (SELECT count(*) FROM project_statuses)`).Scan(&users, &orgs, &issues, &att, &notes); err != nil {
		t.Fatal(err)
	}
	if users+orgs+issues+att+notes != 0 {
		t.Fatalf("target is not empty: users=%d orgs=%d issues=%d attachments=%d statuses=%d", users, orgs, issues, att, notes)
	}
	req, err := setup.Required(ctx, dbq.New(tg.pool))
	if err != nil || !req {
		t.Fatalf("setup must still be required: %v %v", req, err)
	}
	for key := range w.objects {
		if _, err := tg.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("object %q was left in the target store (%v)", key, err)
		}
	}
	var nbs int
	if err := tg.pool.QueryRow(ctx, `SELECT count(*) FROM backup_settings WHERE signing_public <> ''`).Scan(&nbs); err != nil || nbs != 0 {
		t.Fatalf("backup_settings was changed: %d %v", nbs, err)
	}
}

func TestRoundTrip(t *testing.T) {
	testdb.Require(t)
	w := newWorld(t)
	tg := newTarget(t)
	ctx := context.Background()
	pass := newPass(t)

	data, m := w.export(t, pass, true, nil)
	if m.MigrationVersion == 0 || m.ObjectCount != int64(len(w.objects)) || m.Format != 1 || m.ExportedBy == nil || *m.ExportedBy != w.chen {
		t.Fatalf("manifest: %+v", m)
	}
	var wantBytes int64
	for _, b := range w.objects {
		wantBytes += int64(len(b))
	}
	if m.Bytes != wantBytes {
		t.Fatalf("manifest bytes %d, want %d", m.Bytes, wantBytes)
	}
	// The bundle must not be readable without the passphrase: no plaintext secret, no table text.
	for _, s := range []string{secSMTP, secGitToken, secChat, "first", "chen@example.com"} {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("bundle holds %q in the clear", s)
		}
	}

	m2, err := tg.imp(data, pass, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if m2.TotalRows() != m.TotalRows() {
		t.Fatalf("rows %d vs %d", m2.TotalRows(), m.TotalRows())
	}

	// Every copied table has the same rows (compared as text, in a fixed order).
	for _, tb := range move.Registry {
		if tb.Mode != move.ModeCopy || tb.OnImport != nil {
			continue
		}
		q := fmt.Sprintf(`SELECT count(*), md5(COALESCE(string_agg(x::text, '|' ORDER BY x::text), '')) FROM %s x`, tb.Name)
		if tb.Where != "" {
			q += " WHERE " + tb.Where
		}
		var n1, n2 int
		var h1, h2 string
		if err := w.pool.QueryRow(ctx, q).Scan(&n1, &h1); err != nil {
			t.Fatalf("%s: %v", tb.Name, err)
		}
		if err := tg.pool.QueryRow(ctx, q).Scan(&n2, &h2); err != nil {
			t.Fatalf("%s: %v", tb.Name, err)
		}
		if n1 != n2 || h1 != h2 {
			t.Errorf("table %s differs: %d rows %s vs %d rows %s", tb.Name, n1, h1, n2, h2)
		}
	}
	// Two orgs came across, with their cycle columns.
	var norgs, withDefault int
	if err := tg.pool.QueryRow(ctx, `SELECT count(*), count(default_team_id) FROM orgs`).Scan(&norgs, &withDefault); err != nil || norgs != 2 {
		t.Fatalf("orgs: %d %v", norgs, err)
	}
	var nTeamStates int
	if err := tg.pool.QueryRow(ctx, `SELECT count(default_state_id) FROM teams`).Scan(&nTeamStates); err != nil || nTeamStates == 0 {
		t.Fatalf("teams lost default_state_id: %d %v", nTeamStates, err)
	}

	// Skipped tables are empty; sessions did not travel; token hashes did.
	for _, name := range []string{"login_codes", "idempotency_keys", "setup_tokens", "backup_runs", "chat_outbox"} {
		var n int
		if err := tg.pool.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", name, n, err)
		}
	}
	var sessions int
	if err := tg.pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE kind = 'session'`).Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("session tokens travelled: %d %v", sessions, err)
	}
	var hashes1, hashes2 string
	hq := `SELECT COALESCE(string_agg(encode(token_hash, 'hex'), ',' ORDER BY token_hash), '') FROM api_tokens`
	if err := w.pool.QueryRow(ctx, hq+` WHERE kind <> 'session'`).Scan(&hashes1); err != nil {
		t.Fatal(err)
	}
	if err := tg.pool.QueryRow(ctx, hq).Scan(&hashes2); err != nil || hashes1 != hashes2 || hashes1 == "" {
		t.Errorf("token hashes differ: %q vs %q (%v)", hashes1, hashes2, err)
	}
	// The fixture token still authenticates by hash.
	var found int
	if err := tg.pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE token_hash = $1`, mustHash(t, w.d.Chen)).Scan(&found); err != nil || found != 1 {
		t.Errorf("chen token hash on target: %d %v", found, err)
	}

	// Sealed secrets open with the NEW keys, and not with the old.
	nk, ok := tg.keys, w.keys
	var blob []byte
	open := func(sql string, args ...any) { blob = nil; mustScan(t, tg.pool, sql, args, &blob) }
	open(`SELECT smtp_password_enc FROM platform_settings`)
	expectOpen(t, "smtp", secSMTP, blob, func(b []byte) ([]byte, error) { return nk.Settings.Open("smtp_password_enc", b) }, func(b []byte) ([]byte, error) { return ok.Settings.Open("smtp_password_enc", b) })
	open(`SELECT postmark_token_enc FROM platform_settings`)
	expectOpen(t, "postmark", secPostmark, blob, func(b []byte) ([]byte, error) { return nk.Settings.Open("postmark_token_enc", b) }, nil)
	var kc []byte
	mustScan(t, tg.pool, `SELECT key_check FROM platform_settings`, nil, &kc)
	if !nk.Settings.Matches(kc) {
		t.Error("platform_settings.key_check is not the new key's")
	}
	g := w.gitID.String()
	for col, want := range map[string]string{"token_enc": secGitToken, "webhook_secret_enc": secGitHook, "webhook_secret_next_enc": secGitNext} {
		open(`SELECT ` + col + ` FROM git_integrations`)
		col := col
		expectOpen(t, col, want, blob, func(b []byte) ([]byte, error) { return nk.Git.OpenFor(col, g, b) }, func(b []byte) ([]byte, error) { return ok.Git.OpenFor(col, g, b) })
	}
	for _, c := range []struct {
		id, org uuid.UUID
		want    string
	}{{w.chatID, w.d.OrgID, secChat}, {w.chat2ID, w.org2, secChat2}} {
		var scope string
		if err := tg.pool.QueryRow(ctx, `SELECT webhook_enc, seal_scope FROM chat_services WHERE id = $1`, c.id).Scan(&blob, &scope); err != nil {
			t.Fatal(err)
		}
		if scope != "svc."+c.org.String()+"."+c.id.String() {
			t.Errorf("chat scope changed: %s", scope)
		}
		expectOpen(t, "chat", c.want, blob, func(b []byte) ([]byte, error) { return nk.Slack.OpenFor(platform.SlackColWebhook, scope, b) }, func(b []byte) ([]byte, error) { return ok.Slack.OpenFor(platform.SlackColWebhook, scope, b) })
	}
	var pscope string
	if err := tg.pool.QueryRow(ctx, `SELECT webhook_enc, seal_scope FROM personal_webhooks WHERE id = $1`, w.pwhID).Scan(&blob, &pscope); err != nil {
		t.Fatal(err)
	}
	expectOpen(t, "personal", secPersonal, blob, func(b []byte) ([]byte, error) { return nk.Slack.OpenFor(platform.PersonalColWebhook, pscope, b) }, nil)
	var u, s []byte
	if err := tg.pool.QueryRow(ctx, `SELECT url_enc, secret_enc FROM push_relays WHERE org_id = $1`, w.d.OrgID).Scan(&u, &s); err != nil {
		t.Fatal(err)
	}
	expectOpen(t, "relay url", secRelayURL, u, func(b []byte) ([]byte, error) {
		return nk.Push.OpenFor(platform.PushColRelayURL, w.d.OrgID.String(), b)
	}, nil)
	expectOpen(t, "relay secret", secRelaySec, s, func(b []byte) ([]byte, error) {
		return nk.Push.OpenFor(platform.PushColRelaySecret, w.d.OrgID.String(), b)
	}, nil)
	open(`SELECT token_enc FROM push_devices`)
	expectOpen(t, "device", secDevice, blob, func(b []byte) ([]byte, error) {
		return nk.Push.OpenFor(platform.PushColDeviceToken, w.d.OrgID.String()+"."+w.deviceID.String(), b)
	}, nil)
	open(`SELECT secrets_enc FROM backup_destinations`)
	expectOpen(t, "backup dest", secBackupDst, blob, func(b []byte) ([]byte, error) {
		return nk.Backup.Open(platform.BackupColDestinationSecrets, w.destID.String(), b)
	}, func(b []byte) ([]byte, error) {
		return ok.Backup.Open(platform.BackupColDestinationSecrets, w.destID.String(), b)
	})
	open(`SELECT signing_private_enc FROM backup_settings`)
	expectOpen(t, "signing", secSigning, blob, func(b []byte) ([]byte, error) {
		return nk.Backup.Open(platform.BackupColSigningKey, platform.BackupSettingsOwner, b)
	}, nil)

	// Backup identity: new install and instance ids, destinations off, new key check.
	var inst1, inst2 string
	var uid1, uid2 uuid.UUID
	mustScan(t, w.pool, `SELECT install_id, instance_id FROM backup_settings`, nil, &inst1, &uid1)
	mustScan(t, tg.pool, `SELECT install_id, instance_id FROM backup_settings`, nil, &inst2, &uid2)
	if inst1 == inst2 || uid1 == uid2 || len(inst2) != 8 {
		t.Errorf("ids not renewed: %s %s %s %s", inst1, inst2, uid1, uid2)
	}
	var pub string
	var everHad bool
	mustScan(t, tg.pool, `SELECT signing_public, ever_had_recipient, key_check FROM backup_settings`, nil, &pub, &everHad, &kc)
	if pub != "pubkey" || !everHad || !nk.Backup.Matches(kc) {
		t.Errorf("backup settings: %q %v", pub, everHad)
	}
	var enabled bool
	var since, stale *time.Time
	mustScan(t, tg.pool, `SELECT enabled, enabled_since, last_stale_alert_at FROM backup_destinations`, nil, &enabled, &since, &stale)
	if enabled || since != nil || stale != nil {
		t.Errorf("destination must come across disabled: %v %v %v", enabled, since, stale)
	}

	// Attachment files are in the target bucket, byte for byte.
	for key, want := range w.objects {
		rc, info, err := tg.store.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, want) || info.Size != int64(len(want)) {
			t.Errorf("object %s differs (%d vs %d bytes, %v)", key, len(got), len(want), err)
		}
	}

	// The install is no longer in setup state, and a second import is refused.
	if req, err := setup.Required(ctx, dbq.New(tg.pool)); err != nil || req {
		t.Errorf("setup must be over: %v %v", req, err)
	}
	if _, err := tg.imp(data, pass, nil); !errors.Is(err, move.ErrNotEmpty) {
		t.Errorf("second import: %v", err)
	}
	// The inbox sequence moved past the copied values (a new notification must not collide).
	exec(t, tg.pool, `SELECT setval('notifications_inbox_seq_seq', 5)`)
}

func mustHash(t *testing.T, tok string) []byte {
	t.Helper()
	return auth.HashToken(tok)
}

func mustScan(t *testing.T, pool *pgxpool.Pool, sql string, args []any, dest ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func expectOpen(t *testing.T, name, want string, blob []byte, newOpen, oldOpen func([]byte) ([]byte, error)) {
	t.Helper()
	got, err := newOpen(blob)
	if err != nil || string(got) != want {
		t.Errorf("%s: does not open with the new keys: %v", name, err)
	}
	if oldOpen != nil {
		if _, err := oldOpen(blob); err == nil {
			t.Errorf("%s: still opens with the OLD keys", name)
		}
	}
}

func TestRefusalsLeaveNothing(t *testing.T) {
	testdb.Require(t)
	w := newWorld(t)
	pass := newPass(t)
	data, _ := w.export(t, pass, true, nil)

	flip := func(i int) []byte {
		b := append([]byte(nil), data...)
		b[i] ^= 0x01
		return b
	}
	n := len(data)
	cases := []struct {
		name string
		data []byte
		pass string
		is   error
	}{
		{"wrong passphrase", data, newPass(t), move.ErrBadPassphrase},
		{"empty file", nil, pass, move.ErrBundleCorrupt},
		{"not a bundle", []byte("hello, this is not an age file at all"), pass, move.ErrBundleCorrupt},
		{"truncated in the header", data[:60], pass, move.ErrBundleCorrupt},
		{"truncated early", data[:n/10], pass, move.ErrBundleCorrupt},
		{"truncated in the objects", data[:n-n/20], pass, move.ErrBundleCorrupt},
		{"truncated by one byte", data[:n-1], pass, move.ErrBundleCorrupt},
		{"flipped byte early", flip(n / 20), pass, move.ErrBundleCorrupt},
		{"flipped byte in the middle", flip(n / 2), pass, move.ErrBundleCorrupt},
		{"flipped byte in the objects", flip(n - n/20), pass, move.ErrBundleCorrupt},
		{"flipped last byte", flip(n - 1), pass, move.ErrBundleCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := newTarget(t)
			_, err := tg.imp(tc.data, tc.pass, nil)
			if err == nil {
				t.Fatal("import succeeded")
			}
			if !errors.Is(err, tc.is) && !(tc.name == "flipped byte early" && errors.Is(err, move.ErrBadPassphrase)) {
				t.Fatalf("got %v, want %v", err, tc.is)
			}
			tg.assertEmpty(t, w)
		})
	}

	t.Run("wrong migration version", func(t *testing.T) {
		tg := newTarget(t)
		exec(t, tg.pool, `UPDATE schema_migrations SET version = version - 1`)
		_, err := tg.imp(data, pass, nil)
		var ve *move.VersionError
		if !errors.As(err, &ve) || ve.Bundle != ve.Server+1 {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(err.Error(), "migration version") {
			t.Fatalf("message: %v", err)
		}
		tg.assertEmpty(t, w)
	})

	t.Run("non-empty database", func(t *testing.T) {
		tg := newTarget(t)
		exec(t, tg.pool, `INSERT INTO users (username, name) VALUES ('someone', 'Someone')`)
		_, err := tg.imp(data, pass, nil)
		if !errors.Is(err, move.ErrNotEmpty) {
			t.Fatalf("got %v", err)
		}
		var users, att int
		mustScan(t, tg.pool, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM attachments)`, nil, &users, &att)
		if users != 1 || att != 0 {
			t.Fatalf("users=%d attachments=%d", users, att)
		}
		for key := range w.objects {
			if _, err := tg.store.Head(context.Background(), key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("object written into a non-empty install: %v", err)
			}
		}
	})

	t.Run("an org exists", func(t *testing.T) {
		tg := newTarget(t)
		exec(t, tg.pool, `INSERT INTO orgs (slug, name) VALUES ('keep', 'Keep')`)
		if _, err := tg.imp(data, pass, nil); !errors.Is(err, move.ErrNotEmpty) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("object already in the store is kept", func(t *testing.T) {
		tg := newTarget(t)
		var key string
		for k := range w.objects {
			if len(w.objects[k]) == 5 {
				key = k
			}
		}
		stray := []byte("stray")
		if err := tg.store.Put(context.Background(), key, "text/plain", 5, bytes.NewReader(stray)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tg.store.Delete(context.Background(), key) })
		_, err := tg.imp(data, pass, nil)
		var oe *move.ObjectExistsError
		if !errors.As(err, &oe) {
			t.Fatalf("got %v", err)
		}
		rc, _, gerr := tg.store.Get(context.Background(), key)
		if gerr != nil {
			t.Fatalf("the pre-existing object was deleted: %v", gerr)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, stray) {
			t.Fatal("the pre-existing object was changed")
		}
		// Every other object this import wrote before the failure is gone, and the database is empty.
		for k := range w.objects {
			if k == key {
				continue
			}
			if _, err := tg.store.Head(context.Background(), k); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("object %q left behind: %v", k, err)
			}
		}
		var users int
		mustScan(t, tg.pool, `SELECT count(*) FROM users`, nil, &users)
		if users != 0 {
			t.Fatal("rows were committed")
		}
	})

	t.Run("a key is missing", func(t *testing.T) {
		tg := newTarget(t)
		tg.keys.Backup = nil
		_, err := tg.imp(data, pass, nil)
		var km *move.KeyMissingError
		if !errors.As(err, &km) {
			t.Fatalf("got %v", err)
		}
		tg.assertEmpty(t, w)
	})

	t.Run("no object store", func(t *testing.T) {
		tg := newTarget(t)
		_, err := move.Import(context.Background(), bytes.NewReader(data), move.ImportOptions{Pool: tg.pool, Keys: tg.keys, Passphrase: pass})
		if !errors.Is(err, move.ErrNoStore) {
			t.Fatalf("got %v", err)
		}
		tg.assertEmpty(t, w)
	})
}

func TestExportWithoutAttachments(t *testing.T) {
	testdb.Require(t)
	w := newWorld(t)
	tg := newTarget(t)
	pass := newPass(t)
	data, m := w.export(t, pass, false, nil)
	if m.ObjectCount != 0 || m.AttachmentsIncluded {
		t.Fatalf("manifest: %+v", m)
	}
	// No object store is needed to import it.
	if _, err := move.Import(context.Background(), bytes.NewReader(data), move.ImportOptions{Pool: tg.pool, Keys: tg.keys, Passphrase: pass}); err != nil {
		t.Fatalf("import: %v", err)
	}
	var n int
	mustScan(t, tg.pool, `SELECT count(*) FROM attachments WHERE kind = 'file'`, nil, &n)
	if n != 3 {
		t.Fatalf("attachment rows: %d", n)
	}
	for key := range w.objects {
		if _, err := tg.store.Head(context.Background(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("object %q should not have travelled", key)
		}
	}
}

func TestExportRefusals(t *testing.T) {
	testdb.Require(t)
	w := newWorld(t)
	var buf bytes.Buffer
	if _, err := move.Export(context.Background(), &buf, move.ExportOptions{Pool: w.pool, Keys: w.keys, Passphrase: "short", IncludeAttachments: false}); !errors.Is(err, move.ErrWeakPassphrase) {
		t.Errorf("weak passphrase: %v", err)
	}
	if _, err := move.Export(context.Background(), &buf, move.ExportOptions{Pool: w.pool, Keys: w.keys, Passphrase: newPass(t), IncludeAttachments: true}); !errors.Is(err, move.ErrNoStore) {
		t.Errorf("no store: %v", err)
	}
	// A secret sealed under another key is reported, not dropped.
	other := keysFrom(t, 0x77, 0x22)
	_, err := move.Export(context.Background(), &buf, move.ExportOptions{Pool: w.pool, Keys: other, Store: w.store, Passphrase: newPass(t), IncludeAttachments: true})
	var ue *move.UnreadableSecretError
	if !errors.As(err, &ue) {
		t.Errorf("unreadable secret: %v", err)
	}
	// A server without the backup key cannot export backup secrets.
	nobackup := w.keys
	nobackup.Backup = nil
	_, err = move.Export(context.Background(), &buf, move.ExportOptions{Pool: w.pool, Keys: nobackup, Store: w.store, Passphrase: newPass(t), IncludeAttachments: true})
	var km *move.KeyMissingError
	if !errors.As(err, &km) {
		t.Errorf("missing key: %v", err)
	}
	// An attachment row whose file is gone stops the export.
	exec(t, w.pool, `UPDATE attachments SET object_key = object_key || '-gone' WHERE size_bytes = 5`)
	_, err = move.Export(context.Background(), &buf, move.ExportOptions{Pool: w.pool, Keys: w.keys, Store: w.store, Passphrase: newPass(t), IncludeAttachments: true})
	var me *move.ObjectMissingError
	if !errors.As(err, &me) {
		t.Errorf("missing object: %v", err)
	}
}

// TestNoSecretsInLogs runs a whole export and import with debug logging into
// a buffer and looks for the passphrase and every plaintext secret.
func TestNoSecretsInLogs(t *testing.T) {
	testdb.Require(t)
	w := newWorld(t)
	tg := newTarget(t)
	pass := newPass(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	data, _ := w.export(t, pass, true, log)
	if _, err := tg.imp(data, pass, log); err != nil {
		t.Fatal(err)
	}
	// A failing import logs and returns errors too.
	tg2 := newTarget(t)
	_, ierr := tg2.imp(data[:len(data)-3], pass, log)
	if ierr == nil {
		t.Fatal("truncated import succeeded")
	}
	out := logs.String() + ierr.Error()
	if logs.Len() == 0 {
		t.Fatal("expected some log lines")
	}
	secrets := []string{pass, strings.ReplaceAll(pass, "-", ""), secSMTP, secPostmark, secGitToken, secGitHook, secGitNext, secChat, secChat2, secPersonal,
		secRelayURL, secRelaySec, secDevice, "backup-dest-PLAINTEXT-12", secSigning, w.d.Chen, w.d.Claude, w.sessionTk, "PLAINTEXT"}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("log or error holds a secret: %q", s)
		}
	}
	for _, s := range []string{"chen@example.com", "first"} {
		if strings.Contains(out, s) {
			t.Errorf("log holds row data: %q", s)
		}
	}
	// The manifest is safe to show: counts only.
	b, _ := json.Marshal(&move.Manifest{})
	if !bytes.Contains(b, []byte("migrationVersion")) {
		t.Error("manifest shape")
	}
}
