package dropbox

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/backup/driver"
)

// ---- Get checks size and content_hash (LAUN-154) -----------------------------------

func getAll(t *testing.T, r driver.Remote) ([]byte, error) {
	t.Helper()
	rc, err := r.Get(context.Background(), objName)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestGetVerifiesSizeAndHash(t *testing.T) {
	body := payload(5000)
	cases := []struct {
		name string
		hdr  func(b []byte) (string, bool)
		body func(b []byte) []byte
		want driver.Code // "" = success
	}{
		{"matches", nil, nil, ""},
		{"short body", nil, func(b []byte) []byte { return b[:len(b)-1] }, driver.ServerError},
		{"long body", nil, func(b []byte) []byte { return append(append([]byte{}, b...), 'x') }, driver.ServerError},
		{"same size, flipped byte", nil, func(b []byte) []byte {
			c := append([]byte{}, b...)
			c[10] ^= 1
			return c
		}, driver.ServerError},
		{"wrong hash header only", func(b []byte) (string, bool) {
			return `{"size":5000,"content_hash":"` + dbxHash([]byte("other")) + `"}`, true
		}, nil, driver.ServerError},
		{"size only header, short", func(b []byte) (string, bool) { return `{"size":5000}`, true },
			func(b []byte) []byte { return b[:100] }, driver.ServerError},
		{"hash only header, flipped", func(b []byte) (string, bool) { return `{"content_hash":"` + dbxHash(b) + `"}`, true },
			func(b []byte) []byte { c := append([]byte{}, b...); c[0] ^= 1; return c }, driver.ServerError},
		// No usable header: unchecked (documented, UNVERIFIED against a real Dropbox).
		{"no header", func(b []byte) (string, bool) { return "-", true }, func(b []byte) []byte { return b[:10] }, ""},
		{"header without size or hash", func(b []byte) (string, bool) { return `{"name":"x"}`, true }, func(b []byte) []byte { return b[:10] }, ""},
		{"bad json header", func(b []byte) (string, bool) { return `{`, true }, func(b []byte) []byte { return b[:10] }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, r := fresh(t)
			put(t, r, objName, body)
			e.f.dlHeader, e.f.dlBody = c.hdr, c.body
			got, err := getAll(t, r)
			if c.want == "" {
				if err != nil {
					t.Fatalf("%v", err)
				}
				return
			}
			if code(err) != c.want {
				t.Fatalf("err %v (%d bytes)", err, len(got))
			}
			// a failed stream stays failed: no later Read gives a clean EOF
			rc, _ := r.Get(context.Background(), objName)
			defer rc.Close()
			_, _ = io.ReadAll(rc)
			if _, err := rc.Read(make([]byte, 1)); err == io.EOF || err == nil {
				t.Fatalf("a bad stream read cleanly after the error: %v", err)
			}
		})
	}
}

func TestGetMultiBlockHash(t *testing.T) {
	// more than one 4 MiB block: the driver's hasher must agree with the independent one in the fake
	e, r := fresh(t)
	big := payload(9 << 20)
	put(t, r, objName, big)
	got, err := getAll(t, r)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	e.f.dlBody = func(b []byte) []byte { c := append([]byte{}, b...); c[len(c)-1] ^= 1; return c }
	if _, err := getAll(t, r); code(err) != driver.ServerError {
		t.Fatalf("%v", err)
	}
}

// ---- the token refresh does not hold the lock (LAUN-154) ---------------------------

// keepBody reads the request body and puts it back, so the fake can still serve the request.
func keepBody(req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(b))
}

// blockTokens makes the token endpoint wait on release (or the request's end) and counts the waits.
func blockTokens(f *fakeDropbox, release chan struct{}, onlyFirst bool) {
	f.fault = func(ep string, n int, w http.ResponseWriter, req *http.Request) bool {
		if ep != "/oauth2/token" || (onlyFirst && n > 1) {
			return false
		}
		keepBody(req)
		select {
		case <-release:
		case <-req.Context().Done():
		}
		return false // serve normally afterwards (or the client is gone)
	}
}

func waitTokenCalls(t *testing.T, f *fakeDropbox, n int) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f.count("/oauth2/token") >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("token calls never reached %d", n)
}

// waitWaiters blocks until n goroutines are parked in accessToken itself (waiting on
// the refresh in flight, not the leader, whose top frame is the HTTP client). It
// reads the goroutine dump, so it needs no clock and no change to the driver
// (LAUN-176: this replaced a sleep that guessed when the others had arrived).
//
// It depends on the structure of accessToken's wait: a goroutine parked in a select
// (two cases) or a channel receive (a select cut to one case), with accessToken as
// the first non-runtime frame. A refactor of that wait must keep this true or change
// the helper; the failure then names the missing waiters rather than hanging.
// onFail runs before the test fails (close the token endpoint's release channel, so a
// failing run does not wait for Go's test timeout).
func waitWaiters(t *testing.T, n int, onFail func()) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		got := 0
		for _, g := range strings.Split(string(buf), "\n\n") {
			lines := strings.Split(g, "\n")
			if len(lines) < 2 || !(strings.Contains(lines[0], "[select") || strings.Contains(lines[0], "[chan receive")) {
				continue
			}
			for _, l := range lines[1:] {
				if strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "runtime.") {
					continue // file lines and the parking frames
				}
				if strings.Contains(l, "dropbox.(*remote).accessToken(") {
					got++
				}
				break
			}
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			if onFail != nil {
				onFail()
			}
			t.Fatalf("only %d of %d callers reached the wait", got, n)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

// manualDeadline is a context that ends when the test says, with DeadlineExceeded,
// so a test can end a waiter's context AFTER the waiter is parked.
type manualDeadline struct {
	context.Context
	done chan struct{}
}

func (m *manualDeadline) Done() <-chan struct{} { return m.done }
func (m *manualDeadline) Err() error {
	select {
	case <-m.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestConcurrentCallersShareOneRefresh(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	release := make(chan struct{})
	blockTokens(f, release, false)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.List(context.Background(), "launchpad-")
			errs <- err
		}()
	}
	waitTokenCalls(t, f, 1)
	waitWaiters(t, 7, func() { close(release) }) // the other seven are parked behind the refresh
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.count("/oauth2/token") != 1 {
		t.Fatalf("token calls = %d, want one shared refresh", f.count("/oauth2/token"))
	}
}

func TestWaiterLeavesWithItsOwnContextWhileARefreshRuns(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	release := make(chan struct{})
	blockTokens(f, release, true)
	leader := make(chan error, 1)
	go func() { _, err := r.List(context.Background(), "launchpad-"); leader <- err }()
	waitTokenCalls(t, f, 1)

	ctx := &manualDeadline{Context: context.Background(), done: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := r.List(ctx, "launchpad-"); done <- err }()
	waitWaiters(t, 1, func() { close(release) })
	close(ctx.done) // the waiter's own deadline passes while the refresh is still running
	select {
	case err := <-done:
		if code(err) != driver.Timeout {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("a waiter stayed behind the token refresh after its own context ended")
	}
	if f.count("/oauth2/token") != 1 {
		t.Fatalf("the waiter made its own token call: %d", f.count("/oauth2/token"))
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatalf("leader: %v", err)
	}
}

func TestCloseDoesNotWaitForARefresh(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	release := make(chan struct{})
	blockTokens(f, release, true)
	leader := make(chan error, 1)
	go func() { _, err := r.List(context.Background(), "launchpad-"); leader <- err }()
	waitTokenCalls(t, f, 1)
	closed := make(chan struct{})
	go func() { _ = r.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("Close waited for the token refresh")
	}
	close(release)
	<-leader
}

func TestLeaderCancelledWaiterTakesOver(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	f.fault = func(ep string, n int, w http.ResponseWriter, req *http.Request) bool {
		if ep == "/oauth2/token" && n == 1 {
			keepBody(req)          // the server notices a closed client only after the body is read
			<-req.Context().Done() // the first token call never answers
			return true
		}
		return false
	}
	lctx, lcancel := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() { _, err := r.List(lctx, "launchpad-"); leader <- err }()
	waitTokenCalls(t, f, 1)
	waiter := make(chan error, 1)
	go func() { _, err := r.List(context.Background(), "launchpad-"); waiter <- err }()
	waitWaiters(t, 1, nil)
	lcancel()
	if err := <-leader; err == nil {
		t.Fatal("the cancelled leader succeeded")
	}
	select {
	case err := <-waiter:
		if err != nil { // must not inherit the leader's cancel
			t.Fatalf("waiter got the leader's error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never took over")
	}
	if f.count("/oauth2/token") != 2 {
		t.Fatalf("token calls = %d", f.count("/oauth2/token"))
	}
}

func TestLeaderRealErrorIsSharedNotRepeated(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	release := make(chan struct{})
	f.fault = func(ep string, n int, w http.ResponseWriter, req *http.Request) bool {
		if ep != "/oauth2/token" {
			return false
		}
		<-release
		w.WriteHeader(503)
		return true
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.List(context.Background(), "launchpad-"); errs <- err }()
	}
	waitTokenCalls(t, f, 1)
	waitWaiters(t, 3, func() { close(release) })
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("expected the refresh error")
		}
	}
	if f.count("/oauth2/token") != 1 {
		t.Fatalf("token calls = %d: waiters repeated a failed refresh", f.count("/oauth2/token"))
	}
}

// A body longer than the announced size fails EARLY, before the stream delivers the rest.
func TestGetOverrunFailsBeforeTheEnd(t *testing.T) {
	e, r := fresh(t)
	put(t, r, objName, payload(5000))
	e.f.dlHeader = func(b []byte) (string, bool) { return `{"size":5000}`, true } // no hash: only the size rule can fire
	e.f.dlBody = func(b []byte) []byte { return append(append([]byte{}, b...), bytes.Repeat([]byte{'x'}, 60000)...) }
	rc, err := r.Get(context.Background(), objName)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, 1000)
	total := 0
	for {
		n, err := rc.Read(buf)
		total += n
		if err != nil {
			if code(err) != driver.ServerError {
				t.Fatalf("%v", err)
			}
			break
		}
	}
	if total >= 5000+60000 {
		t.Fatalf("the overrun was found only at the end (%d bytes delivered)", total)
	}
}

// slowReader blocks inside Read until released, like the copy of a chunk that the
// transport's write goroutine has begun.
type slowReader struct {
	in      chan struct{}
	release chan struct{}
}

func (s *slowReader) Read(b []byte) (int, error) {
	close(s.in)
	<-s.release
	return copy(b, "x"), nil
}

// LAUN-174: Put reuses one buffer for every chunk, and the transport may still be
// reading a request body from its own goroutine after Do returned. stop must wait
// for a Read in progress and refuse every later one, or that Read races with the
// copy of the next chunk into the same buffer.
func TestRequestBodyStopWaitsForReadInProgress(t *testing.T) {
	ctx, w := newWatch(context.Background())
	defer w.stop()
	sr := &slowReader{in: make(chan struct{}), release: make(chan struct{})}
	b := &reqBody{p: progress{r: sr, w: w, ctx: ctx}}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		b.Read(make([]byte, 4))
	}()
	<-sr.in // the Read is inside the source now
	stopped := make(chan struct{})
	go func() { b.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while a Read was still using the buffer")
	case <-time.After(100 * time.Millisecond):
	}
	close(sr.release)
	<-stopped
	<-readDone
	if n, err := b.Read(make([]byte, 4)); n != 0 || err == nil {
		t.Fatalf("a stopped body still reads: n=%d err=%v", n, err)
	}
}

// LAUN-174: end to end. The server answers 500 without reading a big body, and the
// caller overwrites the buffer as soon as once returns (as Put does with the next
// chunk). Without reqBody.stop in once, the transport's write goroutine still reads
// the buffer and -race reports it.
func TestEarlyReplyReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500) // reply without reading the body
	}))
	defer srv.Close()
	r := &remote{}
	buf := make([]byte, 8<<20)
	for i := 0; i < 100; i++ {
		_, _ = r.once(context.Background(), call{client: srv.Client(), url: srv.URL, body: buf, maxReply: 100}, "")
		for j := range buf {
			buf[j] = byte(i)
		}
	}
}
