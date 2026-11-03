package setup_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"devops/internal/dbq"
	"devops/internal/setup"
	"devops/internal/testdb"
)

// Concurrent boots (several instances) each mint and close the older
// links; exactly one link is live afterwards.
func TestMintIfRequired_Concurrent(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, _, errs[i] = setup.MintIfRequired(ctx, pool) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total, live int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE consumed_at IS NULL) FROM setup_tokens`).Scan(&total, &live); err != nil {
		t.Fatal(err)
	}
	if total != n || live != 1 {
		t.Fatalf("%d links, %d live; want %d and 1", total, live, n)
	}
}

func TestGrantPlatformAdmin(t *testing.T) {
	pool, _ := testdb.New(t)
	testdb.Fixture(t, pool) // chen (human, org admin), claude (agent)
	ctx := context.Background()
	q := dbq.New(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO users (username, name, deactivated_at) VALUES ('gone', 'Gone', now())`); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, user string
		already    bool
		err        error
	}{
		{"unknown user", "nobody", false, setup.ErrUserNotFound},
		{"agent", "claude", false, setup.ErrNotEligible},
		{"deactivated", "gone", false, setup.ErrNotEligible},
		{"human", " Chen ", false, nil},
		{"again", "chen", true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			already, err := setup.GrantPlatformAdmin(ctx, pool, tt.user)
			if !errors.Is(err, tt.err) || already != tt.already {
				t.Fatalf("already %v err %v, want %v %v", already, err, tt.already, tt.err)
			}
		})
	}
	for user, want := range map[string]bool{"chen": true, "claude": false, "gone": false} {
		u, err := q.GetUserByUsername(ctx, user)
		if err != nil || u.IsPlatformAdmin != want {
			t.Fatalf("%s is_platform_admin %v err %v, want %v", user, u.IsPlatformAdmin, err, want)
		}
	}
	// An existing install with users never opens setup.
	if _, minted, err := setup.MintIfRequired(ctx, pool); err != nil || minted {
		t.Fatalf("minted %v err %v", minted, err)
	}
}
