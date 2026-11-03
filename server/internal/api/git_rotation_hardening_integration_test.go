package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/auth"
	"devops/internal/scm"
)

// Rotation hardening (LAUN-92).

// hookProbe wraps the server's API clients. Until armed it does nothing.
type hookProbe struct {
	mu     sync.Mutex
	armed  bool
	calls  atomic.Int32
	before func(n int32) // called before the host call, with the call number
	after  func(n int32) // called after a host call that succeeded
}

func (p *hookProbe) arm(before, after func(int32)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed, p.before, p.after = true, before, after
	p.calls.Store(0)
}

func (p *hookProbe) wrap(c scm.APIClient) scm.APIClient { return &probedClient{APIClient: c, p: p} }

type probedClient struct {
	scm.APIClient
	p *hookProbe
}

func (c *probedClient) EnsureHook(ctx context.Context, repo scm.RepoRef, hook scm.HookSpec) (scm.HookRef, error) {
	c.p.mu.Lock()
	armed, before, after := c.p.armed, c.p.before, c.p.after
	c.p.mu.Unlock()
	if !armed {
		return c.APIClient.EnsureHook(ctx, repo, hook)
	}
	n := c.p.calls.Add(1)
	if before != nil {
		before(n)
	}
	h, err := c.APIClient.EnsureHook(ctx, repo, hook)
	if err == nil && after != nil {
		after(n)
	}
	return h, err
}

// The client disconnects while the push runs: the state writes that follow
// still happen, so the repos are active and the swap is committed.
func TestGitRotate_ClientDisconnectDuringPush(t *testing.T) {
	probe := &hookProbe{}
	g := newGitEnv(t, gitEnvOpts{wrapClient: probe.wrap})
	h := newRepoHost(t, 1001, 1002)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	g.mapRepo(gi.ID, 1002)
	s0 := h.secret(1001)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe.arm(nil, func(n int32) {
		if n == 2 {
			cancel() // the last hook is on the host; the client is gone
		}
	})
	org := &auth.Org{ID: g.orgID, Slug: "orbit", Role: "admin", Access: auth.TeamAccess{All: true}}
	rot, err := g.server.svc.RotateGitSecret(ctx, org, gi.ID, "")
	if err != nil {
		t.Fatalf("rotate after a disconnect: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the test never cancelled the request")
	}
	st := g.hookStates(gi.ID)
	p := h.secret(1001)
	if !rot.Committed || len(rot.FailedRepos) != 0 || st["1001"] != "active" || st["1002"] != "active" || g.integration(gi.ID).SecretPending {
		t.Fatalf("rotate = %+v, states %v", rot, st)
	}
	if p == s0 || h.secret(1002) != p || g.pingStatus(p) != 200 || g.pingStatus(s0) != 401 {
		t.Fatal("the hooks and the stored secret must agree")
	}
}

// A crash between a host write and the DB write leaves a "not_updated" row
// that has a hook id: the host may hold the discarded secret. A discard marks it
// failed, so the next rotate pushes to it again; it is never "active".
func TestGitRotate_DiscardAfterCrashMarksFailed(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	g.mapRepo(gi.ID, 1002)
	h.fail(1001, 500)
	h.fail(1002, 500)
	g.rotate(gi.ID, http.StatusOK, nil) // a pending secret, both repos failed
	// The crash: 1001 took the host write, the row still says not_updated (it
	// has its hook id). 1002 never had a hook.
	if _, err := g.pool.Exec(context.Background(), `UPDATE git_repo_map SET hook_state = 'failed', hook_error = 'not_updated' WHERE integration_id = $1`, gi.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.pool.Exec(context.Background(), `UPDATE git_repo_map SET hook_id = NULL WHERE integration_id = $1 AND repo_key = '1002'`, gi.ID); err != nil {
		t.Fatal(err)
	}
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil))
	st := g.hookStates(gi.ID)
	if st["1001"] != "failed:secret_discarded" || st["1002"] != "none" {
		t.Fatalf("states after the discard: %v", st)
	}
	// The next rotate pushes to the failed repo again and commits.
	h.fail(1001, 0)
	h.fail(1002, 0)
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	if !rot.Committed || g.pingStatus(h.secret(1001)) != 200 {
		t.Fatalf("retry = %+v", rot)
	}
}

// Two rotations of one integration never push at the same time: the second
// waits for the first, so a late write of the first cannot overwrite the second.
func TestGitRotate_OverlappingRotationsAreSerialised(t *testing.T) {
	probe := &hookProbe{}
	g := newGitEnv(t, gitEnvOpts{wrapClient: probe.wrap})
	h := newRepoHost(t, 1001)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)

	entered := make(chan struct{})
	release := make(chan struct{})
	probe.arm(func(n int32) {
		if n == 1 {
			close(entered)
			<-release // rotation A stands in its push
		}
	}, nil)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	run := func(i int) {
		defer wg.Done()
		statuses[i] = g.do("POST", g.path(gi.ID, "/rotate-secret"), g.chen, nil).status
	}
	wg.Add(1)
	go run(0)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("rotation A never reached the host")
	}
	wg.Add(1)
	go run(1)
	time.Sleep(800 * time.Millisecond)
	if n := probe.calls.Load(); n != 1 {
		close(release)
		wg.Wait()
		t.Fatalf("rotation B reached the host while A was pushing (%d calls)", n)
	}
	// A discard while A pushes makes A's secret stale; B must still not race it.
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil))
	close(release)
	wg.Wait()
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
		t.Fatalf("statuses = %v", statuses)
	}
	// B ran after A and won: the host holds B's secret and the row accepts it.
	st := g.hookStates(gi.ID)
	if st["1001"] != "active" || g.integration(gi.ID).SecretPending || g.pingStatus(h.secret(1001)) != 200 {
		t.Fatalf("final states %v, pending %v", st, g.integration(gi.ID).SecretPending)
	}
}

// A force commit takes this run's pending secret during the push: the swap
// happened and it was this run's secret, so the run reports committed.
func TestGitRotate_ForceCommitDuringPushReportsCommitted(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	g.mapRepo(gi.ID, 1002)
	h.onWrite = func() {
		g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/rotate-secret/commit"), g.chen, map[string]any{"force": true}))
	}
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	p := h.secret(1001)
	if !rot.Committed || len(rot.FailedRepos) != 0 || rot.SecretPending || g.pingStatus(p) != 200 || h.secret(1002) != p {
		t.Fatalf("rotate = %+v", rot)
	}
}
