package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0034_ChatServices checks the down policy of 0034 (LAUN-204,
// decisions-T36 #6): the one Slack service whose blob still has its migrated
// scope comes back as the Slack channel with its waiting posts; the others
// (Discord, custom, a second Slack service) are lost, and so are their posts.
// A disabled service returns with an empty event list. Then up again.
func TestMigration0034_ChatServices(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 34)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	one := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	org := one(`INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`)
	org2 := one(`INSERT INTO orgs (slug, name) VALUES ('beta-org', 'beta') RETURNING id::text`)
	team := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'AAA', 'AAA') RETURNING id::text`, org)
	state := one(`INSERT INTO workflow_states (org_id, team_id, name, type, position) VALUES ($1, $2, 'Todo', 'unstarted', 1) RETURNING id::text`, org, team)
	issue := one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 1, 'x', $3) RETURNING id::text`, org, team, state)
	blob := func(s string) []byte { return []byte("0123456789abcdef0123456789abcdef-" + s) }
	svc := func(o, name, kind, scope, ev string, enabled bool, lastErr any) string {
		return one(`INSERT INTO chat_services (org_id, name, kind, host, webhook_enc, seal_scope, events, enabled, last_error)
			VALUES ($1, $2, $3, 'h.example.com', $4, $5, $6::text[], $7, $8) RETURNING id::text`, o, name, kind, blob(name), scope, ev, enabled, lastErr)
	}
	legacy := svc(org, "Slack", "slack", org, "{assigned,comment}", true, "chat_gone")
	second := svc(org, "Second", "slack", "svc.x.y", "{assigned}", true, nil)
	disc := svc(org, "Disc", "discord", "svc.x.z", "{assigned}", true, nil)
	svc(org2, "Off", "slack", org2, "{assigned,comment}", false, nil)
	post := func(o, sid, key, st string) {
		exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state, last_error)
			VALUES ($1, $2, 'assigned', $3, $4, $5, CASE WHEN $5 = 'dead' THEN 'chat_rejected' END)`, o, sid, issue, key, st)
	}
	post(org, legacy, "k1", "pending")
	post(org, legacy, "k2", "dead")
	post(org, second, "k1", "pending") // same key, other service: legal now, lost on down
	post(org, disc, "k3", "pending")

	migrateTo(t, m, 33)

	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM slack_channels`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("channels after down = %d (%v), want the legacy one and the disabled one", n, err)
	}
	if got := one(`SELECT array_to_string(events, ',') || '|' || last_error FROM slack_channels WHERE org_id = $1`, org); got != "assigned,comment|slack_gone" {
		t.Errorf("restored channel %q", got)
	}
	if got := one(`SELECT count(*)::text FROM slack_channels WHERE org_id = $1 AND webhook_enc = $2`, org2, blob("Off")); got != "1" {
		t.Errorf("the sealed blob did not come back unchanged")
	}
	if got := one(`SELECT cardinality(events)::text FROM slack_channels WHERE org_id = $1`, org2); got != "0" {
		t.Errorf("a disabled service returned with %s events", got)
	}
	rows := map[string]bool{}
	rs, err := conn.Query(ctx, `SELECT dedupe_key || '/' || state || '/' || coalesce(last_error, '') FROM slack_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var k string
		_ = rs.Scan(&k)
		rows[k] = true
	}
	rs.Close()
	if len(rows) != 2 || !rows["k1/pending/"] || !rows["k2/dead/slack_rejected"] {
		t.Fatalf("outbox after down %v", rows)
	}

	migrateTo(t, m, 34)
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM chat_services WHERE kind = 'slack' AND seal_scope = org_id::text`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("services after up again = %d (%v)", n, err)
	}
}
