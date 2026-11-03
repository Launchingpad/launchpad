package testdb_test

import (
	"context"
	"testing"

	"devops/internal/testdb"
)

// The fixture the integration tests are written against (it replaced the
// boot seed in).
func TestFixture(t *testing.T) {
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	if d.Chen == "" || d.Claude == "" || d.Chen == d.Claude {
		t.Fatalf("tokens %q %q", d.Chen, d.Claude)
	}
	ctx := context.Background()
	want := map[string]int{
		"SELECT count(*) FROM orgs":                                                         1,
		"SELECT count(*) FROM teams":                                                        1,
		"SELECT count(*) FROM workflow_states":                                              9,
		"SELECT count(*) FROM labels":                                                       3,
		"SELECT count(*) FROM users":                                                        2,
		"SELECT count(*) FROM users WHERE is_platform_admin":                                0,
		"SELECT count(*) FROM org_members WHERE role = 'admin'":                             1,
		"SELECT count(*) FROM api_tokens WHERE org_id IS NOT NULL":                          2,
		"SELECT count(*) FROM users WHERE username = 'chen' AND email = 'chen@example.com'": 1,
		"SELECT count(*) FROM users WHERE username = 'claude' AND is_agent":                 1,
	}
	for sql, n := range want {
		var got int
		if err := pool.QueryRow(ctx, sql).Scan(&got); err != nil || got != n {
			t.Errorf("%s = %d (%v), want %d", sql, got, err, n)
		}
	}
}
