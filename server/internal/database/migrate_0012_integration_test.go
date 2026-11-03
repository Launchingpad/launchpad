package database_test

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

type pgHelper struct {
	t    *testing.T
	ctx  context.Context
	conn *pgx.Conn
}

func newPGHelper(t *testing.T, dsn string) *pgHelper {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return &pgHelper{t: t, ctx: ctx, conn: conn}
}

func (h *pgHelper) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.conn.Exec(h.ctx, q, args...); err != nil {
		h.t.Fatalf("%s: %v", q, err)
	}
}

func (h *pgHelper) fails(q string, args ...any) {
	h.t.Helper()
	if _, err := h.conn.Exec(h.ctx, q, args...); err == nil {
		h.t.Fatalf("accepted, want a constraint error: %s", q)
	}
}

func (h *pgHelper) one(q string, args ...any) string {
	h.t.Helper()
	var s string
	if err := h.conn.QueryRow(h.ctx, q, args...).Scan(&s); err != nil {
		h.t.Fatalf("%s: %v", q, err)
	}
	return s
}

// TestMigration0012_GitTables checks migration A (LAUN-82, decisions-T17 #8):
// the backfill (integration_id, the gl: prefix, the casts), the new columns
// and their CHECKs, a GitHub row, and the down policy.
func TestMigration0012_GitTables(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 11)
	h := newPGHelper(t, dsn)

	newOrg := func(slug string) (org, issue, state string) {
		org = h.one(`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id::text`, slug)
		team := h.one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'ORB', 'Orbit') RETURNING id::text`, org)
		state = h.one(`INSERT INTO workflow_states (org_id, team_id, name, type) VALUES ($1, $2, 'Todo', 'unstarted') RETURNING id::text`, org, team)
		issue = h.one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 1, 'x', $3) RETURNING id::text`, org, team, state)
		return
	}
	org1, issue1, state1 := newOrg("one")
	org2, _, _ := newOrg("two") // no integration
	intA := h.one(`INSERT INTO gitlab_integrations (org_id, base_url, webhook_secret_hash, created_at)
		VALUES ($1, 'https://a.example', decode(repeat('aa', 32), 'hex'), now() - interval '1 day') RETURNING id::text`, org1)
	intB := h.one(`INSERT INTO gitlab_integrations (org_id, base_url, webhook_secret_hash, enabled)
		VALUES ($1, 'https://b.example', decode(repeat('bb', 32), 'hex'), false) RETURNING id::text`, org1)
	h.exec(`INSERT INTO gitlab_project_map (org_id, integration_id, gitlab_project_id, project_path) VALUES ($1, $2, 42, 'grp/p')`, org1, intA)
	evA := h.one(`INSERT INTO gitlab_webhook_events (org_id, event_uuid, event_type, gitlab_project_id)
		VALUES ($1, 'ev:abc:p42:mr17:open', 'merge_request', 42) RETURNING id::text`, org1)
	h.exec(`INSERT INTO gitlab_webhook_events (org_id, event_uuid, event_type) VALUES ($1, 'ik:k:p1:mr1:open', 'merge_request')`, org1)
	h.exec(`INSERT INTO gitlab_webhook_events (org_id, event_uuid, event_type) VALUES ($1, 'ev:orphan', 'merge_request')`, org2)
	link := h.one(`INSERT INTO mr_links (org_id, issue_id, integration_id, gitlab_project_id, mr_iid, url, state)
		VALUES ($1, $2, $3, 42, 17, 'https://x/17', 'opened') RETURNING id::text`, org1, issue1, intA)
	h.exec(`INSERT INTO issue_state_changes (org_id, issue_id, to_state_id, source) VALUES ($1, $2, $3, 'gitlab')`, org1, issue1, state1)

	migrateTo(t, m, 12)

	// Backfill and casts.
	if got := h.one(`SELECT integration_id::text FROM git_webhook_events WHERE id = $1`, evA); got != intA {
		t.Fatalf("event integration_id = %s, want the org's oldest integration %s", got, intA)
	}
	if got := h.one(`SELECT count(*)::text FROM git_webhook_events WHERE org_id = $1`, org2); got != "0" {
		t.Fatalf("an event of an org with no integration was kept (%s)", got)
	}
	if got := h.one(`SELECT event_uuid FROM git_webhook_events WHERE id = $1`, evA); got != "gl:ev:abc:p42:mr17:open" {
		t.Fatalf("event_uuid = %q, want the gl: prefix", got)
	}
	if got := h.one(`SELECT string_agg(event_uuid, ',' ORDER BY event_uuid) FROM git_webhook_events`); got != "gl:ev:abc:p42:mr17:open,gl:ik:k:p1:mr1:open" {
		t.Fatalf("event_uuids = %q", got)
	}
	if got := h.one(`SELECT repo_key || '/' || coalesce((SELECT repo_key FROM git_webhook_events WHERE id = $1), 'null') FROM git_repo_map`, evA); got != "42/42" {
		t.Fatalf("repo_key = %q, want text 42 in both tables", got)
	}
	if got := h.one(`SELECT repo_path FROM git_repo_map`); got != "grp/p" {
		t.Fatalf("repo_path = %q", got)
	}
	if got := h.one(`SELECT repo_key || ':' || number FROM mr_links WHERE id = $1`, link); got != "42:17" {
		t.Fatalf("mr_links key = %q", got)
	}
	if got := h.one(`SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'mr_links'::regclass AND attname = 'number'`); got != "bigint" {
		t.Fatalf("number type = %s", got)
	}
	if got := h.one(`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'git_webhook_events' AND column_name = 'integration_id'`); got != "NO" {
		t.Fatalf("integration_id nullable = %s", got)
	}

	// Existing rows: GitLab, webhook_only, the new columns NULL or default.
	for _, id := range []string{intA, intB} {
		if got := h.one(`SELECT provider || '|' || mode || '|' || (flavor IS NULL)::text || (api_base_url IS NULL)::text || (name IS NULL)::text
			|| (token_enc IS NULL)::text || (webhook_secret_enc IS NULL)::text || (webhook_secret_next_enc IS NULL)::text
			|| (webhook_secret_next_hash IS NULL)::text || (attention_code IS NULL)::text FROM git_integrations WHERE id = $1`, id); got != "gitlab|webhook_only|truetruetruetruetruetruetruetrue" {
			t.Fatalf("old row = %q", got)
		}
	}
	if got := h.one(`SELECT hook_state || (hook_id IS NULL)::text || (hook_error IS NULL)::text FROM git_repo_map`); got != "nonetruetrue" {
		t.Fatalf("map row = %q", got)
	}

	// The CHECKs hold.
	ins := func(provider, extra, base string) string {
		return `INSERT INTO git_integrations (org_id, base_url, webhook_secret_hash, provider` + extra + `) VALUES ($1, '` + base + `', decode(repeat('cc', 32), 'hex'), '` + provider + `'`
	}
	h.fails(ins("gitea", "", "https://g.example")+`)`, org1)
	h.fails(ins("github", ", flavor", "https://g.example")+`, 'cloudy')`, org1)
	h.fails(ins("github", ", mode", "https://g.example")+`, 'magic')`, org1)
	h.fails(ins("github", ", mode", "https://g.example")+`, 'token')`, org1) // token mode needs token_enc
	h.fails(ins("github", ", webhook_secret_next_hash", "https://g.example")+`, '\x00'::bytea)`, org1)
	h.fails(`UPDATE git_repo_map SET hook_state = 'weird'`)

	// A GitHub row, inserted directly (no create route yet). The same base URL
	// as a GitLab row is allowed; the same provider and URL is not.
	gh := h.one(ins("github", ", flavor, mode, api_base_url, name, token_enc, webhook_secret_enc, webhook_secret_next_enc, webhook_secret_next_hash, attention_code",
		"https://a.example")+`, 'cloud', 'token', 'https://api.github.com', 'GH', '\x01', '\x02', '\x03', decode(repeat('dd', 32), 'hex'), 'hook_failed') RETURNING id::text`, org1)
	h.fails(ins("github", "", "https://a.example")+`)`, org1)
	h.exec(`INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path, hook_id, hook_state) VALUES ($1, $2, 'R_kgDO123', 'o/r', '99', 'active')`, org1, gh)
	h.exec(`INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type, repo_key) VALUES ($1, $2, 'gh:d1:r5:pr1:opened', 'pull_request', '5')`, org1, gh)
	h.exec(`INSERT INTO mr_links (org_id, issue_id, integration_id, repo_key, number, url, state) VALUES ($1, $2, $3, 'R_kgDO123', 7, 'https://gh/7', 'opened')`, org1, issue1, gh)
	h.exec(`INSERT INTO issue_state_changes (org_id, issue_id, to_state_id, source) VALUES ($1, $2, $3, 'github')`, org1, issue1, state1)
	h.exec(`INSERT INTO issue_activity (org_id, issue_id, source, kind) VALUES ($1, $2, 'bitbucket', 'title')`, org1, issue1)
	h.fails(`INSERT INTO issue_activity (org_id, issue_id, source, kind) VALUES ($1, $2, 'gitea', 'title')`, org1, issue1)
	// The dedupe key is per integration: the same event_uuid under another
	// integration is a different row, under the same one it is refused.
	h.exec(`INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type) VALUES ($1, $2, 'gl:ev:abc:p42:mr17:open', 'merge_request')`, org1, intB)
	h.fails(`INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type) VALUES ($1, $2, 'gl:ev:abc:p42:mr17:open', 'merge_request')`, org1, intA)
	// Composite org FK: an event cannot name another org's integration.
	h.fails(`INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type) VALUES ($1, $2, 'x', 'merge_request')`, org2, intA)

	// Down: non-GitLab rows and what hangs on them go; sources become system;
	// the prefix is stripped; non-numeric GitLab keys go.
	h.exec(`INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path) VALUES ($1, $2, 'group/slug', 'x')`, org1, intA)
	h.exec(`INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type, repo_key) VALUES ($1, $2, 'gl:ev:nn', 'merge_request', 'abc')`, org1, intA)
	h.exec(`INSERT INTO mr_links (org_id, issue_id, integration_id, repo_key, number, url, state) VALUES ($1, $2, $3, 'abc', 1, 'https://x/1', 'opened')`, org1, issue1, intA)
	h.exec(`INSERT INTO mr_links (org_id, issue_id, integration_id, repo_key, number, url, state) VALUES ($1, $2, $3, '43', 5000000000, 'https://x/5', 'opened')`, org1, issue1, intA)
	// A collision the old (org_id, event_uuid) key cannot hold: the oldest stays.
	h.exec(`UPDATE git_webhook_events SET received_at = now() - interval '2 days' WHERE integration_id = $1 AND event_uuid = 'gl:ev:abc:p42:mr17:open'`, intA)

	migrateTo(t, m, 11)

	if got := h.one(`SELECT count(*)::text FROM gitlab_integrations`); got != "2" {
		t.Fatalf("down: %s integrations, want the two GitLab ones", got)
	}
	if got := h.one(`SELECT count(*)::text FROM gitlab_project_map`); got != "1" {
		t.Fatalf("down: %s map rows, want 1 (GitHub's and the non-numeric one go)", got)
	}
	if got := h.one(`SELECT string_agg(event_uuid, ',' ORDER BY event_uuid) FROM gitlab_webhook_events`); got != "ev:abc:p42:mr17:open,ik:k:p1:mr1:open" {
		t.Fatalf("down: event_uuids = %q", got)
	}
	if got := h.one(`SELECT id::text FROM gitlab_webhook_events WHERE event_uuid = 'ev:abc:p42:mr17:open'`); got != evA {
		t.Fatalf("down: kept %s of the colliding rows, want the oldest %s", got, evA)
	}
	if got := h.one(`SELECT string_agg(gitlab_project_id::text || ':' || mr_iid::text, ',') FROM mr_links`); got != "42:17" {
		t.Fatalf("down: links = %q, want only 42:17", got)
	}
	if got := h.one(`SELECT string_agg(source, ',' ORDER BY source) FROM issue_state_changes`); got != "gitlab,system" {
		t.Fatalf("down: state change sources = %q (github is rewritten to system, not deleted)", got)
	}
	if got := h.one(`SELECT source FROM issue_activity`); got != "system" {
		t.Fatalf("down: activity source = %q", got)
	}
	if got := h.one(`SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'gitlab_integrations' AND column_name IN ('provider','token_enc','attention_code')`); got != "0" {
		t.Fatal("down left the new columns")
	}
	h.fails(`INSERT INTO gitlab_integrations (org_id, base_url, webhook_secret_hash) VALUES ($1, 'https://a.example', decode(repeat('ee', 32), 'hex'))`, org1)

	migrateTo(t, m, 12) // and up again
	if got := h.one(`SELECT event_uuid FROM git_webhook_events WHERE id = $1`, evA); got != "gl:ev:abc:p42:mr17:open" {
		t.Fatalf("re-up: event_uuid = %q", got)
	}
}

// TestMigration0012_ConstraintNames lists every constraint and index of the
// three git tables and of mr_links, and checks that no gitlab_ name is left,
// in the database and in db/schema.sql.
func TestMigration0012_ConstraintNames(t *testing.T) {
	_, dsn := testdb.New(t) // migrated to the latest version
	h := newPGHelper(t, dsn)
	rows, err := h.conn.Query(h.ctx, `
		SELECT c.relname || '.' || con.conname FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		WHERE c.relname IN ('git_integrations', 'git_repo_map', 'git_webhook_events')
		UNION ALL
		SELECT tablename || '.' || indexname FROM pg_indexes
		WHERE schemaname = 'public' AND tablename IN ('git_integrations', 'git_repo_map', 'git_webhook_events')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := strings.Fields(`
		git_integrations.git_integrations_flavor_check
		git_integrations.git_integrations_hash_len
		git_integrations.git_integrations_mode_check
		git_integrations.git_integrations_next_hash_len
		git_integrations.git_integrations_org_id_fkey
		git_integrations.git_integrations_org_id_id_key
		git_integrations.git_integrations_org_provider_base_key
		git_integrations.git_integrations_pkey
		git_integrations.git_integrations_provider_check
		git_integrations.git_integrations_token_mode_check
		git_repo_map.git_repo_map_hook_state_check
		git_repo_map.git_repo_map_integration_fk
		git_repo_map.git_repo_map_org_integration_idx
		git_repo_map.git_repo_map_pkey
		git_repo_map.git_repo_map_repo_key
		git_repo_map.git_repo_map_team_fk
		git_repo_map.git_repo_map_team_idx
		git_webhook_events.git_webhook_events_integration_fk
		git_webhook_events.git_webhook_events_org_id_fkey
		git_webhook_events.git_webhook_events_pkey
		git_webhook_events.git_webhook_events_received_idx
		git_webhook_events.git_webhook_events_status_check
		git_webhook_events.git_webhook_events_uuid_key`)
	// Each primary key and unique constraint is listed twice (constraint and index).
	var dedup []string
	for i, s := range got {
		if i == 0 || s != got[i-1] {
			dedup = append(dedup, s)
		}
	}
	sort.Strings(want)
	if strings.Join(dedup, "\n") != strings.Join(want, "\n") {
		t.Fatalf("constraint and index names:\n got %s\nwant %s", strings.Join(dedup, "\n     "), strings.Join(want, "\n     "))
	}

	// No gitlab_ name anywhere in the schema snapshot (columns and values are fine).
	b, err := os.ReadFile("../../db/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?i)(CONSTRAINT|INDEX|TABLE( ONLY)?( public\.)?|ON public\.)\s*gitlab_|public\.gitlab_|REFERENCES public\.gitlab_`)
	for i, line := range strings.Split(string(b), "\n") {
		if re.MatchString(line) {
			t.Errorf("db/schema.sql:%d still has a gitlab_ name: %s", i+1, line)
		}
	}
	if strings.Contains(string(b), "gitlab_integrations") || strings.Contains(string(b), "gitlab_project_map") || strings.Contains(string(b), "gitlab_webhook_events") {
		t.Error("db/schema.sql still names an old gitlab_ table")
	}
}
