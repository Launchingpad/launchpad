package driver

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"devops/internal/netguard"
)

type fake struct{}

func (fake) Validate(json.RawMessage) error                     { return nil }
func (fake) Identity(json.RawMessage) (Identity, error)         { return Identity{}, nil }
func (fake) Probe(context.Context, Config) (ProbeResult, error) { return ProbeResult{}, nil }
func (fake) Open(context.Context, Config) (Remote, error)       { return nil, nil }

func TestRegistry(t *testing.T) {
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		delete(registry, "zz-test-a")
		delete(registry, "zz-test-b")
	})
	if got := SupportedTypes(); len(got) != 0 {
		t.Fatalf("production registry is not empty: %v", got) // no driver exists in this ticket
	}
	if _, ok := Lookup("s3"); ok {
		t.Fatal("s3 has no driver yet")
	}
	Register("zz-test-b", fake{})
	Register("zz-test-a", fake{})
	got := SupportedTypes()
	if strings.Join(got, ",") != "zz-test-a,zz-test-b" {
		t.Errorf("SupportedTypes = %v", got)
	}
	if _, ok := Lookup("s3"); ok {
		t.Error("a type with no driver must be absent")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("duplicate Register did not panic")
			}
		}()
		Register("zz-test-a", fake{})
	}()
	for _, bad := range []func(){func() { Register("", fake{}) }, func() { Register("zz-c", nil) }} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("bad Register did not panic")
				}
			}()
			bad()
		}()
	}
}

func TestErrorTextsAreFixedAndComplete(t *testing.T) {
	for _, c := range Codes() {
		if texts[c] == "" {
			t.Errorf("code %s has no text", c)
		}
	}
	if len(Codes()) != len(texts) {
		t.Error("Codes and texts differ")
	}
	// The type has no field for the remote's words: a planted secret in a
	// wrapped error never reaches Error().
	wrapped := fmt.Errorf("server said password=hunter2: %w", syscall.ECONNREFUSED)
	e := MapNetError(wrapped)
	if e.Code != ConnectRefused || strings.Contains(e.Error(), "hunter2") {
		t.Errorf("MapNetError = %v", e)
	}
	if got := ServerErr(502).Error(); !strings.Contains(got, "status 502") {
		t.Error(got)
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{Errf(Timeout), true}, {Errf(ConnectRefused), true}, {Errf(DNSFailed), true}, {Errf(Stalled), true},
		{ServerErr(500), true}, {ServerErr(503), true}, {ServerErr(599), true},
		{ServerErr(301), false}, {ServerErr(302), false}, {ServerErr(400), false}, {ServerErr(404), false}, {ServerErr(0), false},
		{Errf(AuthFailed), false}, {Errf(Forbidden), false}, {Errf(NotFound), false}, {Errf(QuotaExceeded), false},
		{Errf(TLSUntrusted), false}, {Errf(TLSWrongHost), false}, {Errf(TLSPinMismatch), false}, {Errf(HostKeyChanged), false},
		{Errf(SSRFBlocked), false}, {Errf(SecretsUnreadable), false}, {Errf(TooLarge), false}, {Errf(NoSpace), false},
		{Errf(DumpMismatch), false}, {Errf(DumpUnavailable), false}, {Errf(ClockSkew), false}, {Errf(InstanceChanged), false},
		{Errf(ScratchFull), false}, {errors.New("plain"), false}, {nil, false},
		{fmt.Errorf("wrapped: %w", Errf(Timeout)), true},
	} {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%v) = %v", tc.err, got)
		}
	}
}

func TestStatusError(t *testing.T) {
	for status, want := range map[int]Code{
		301: ServerError, 302: ServerError, 307: ServerError, 401: AuthFailed, 403: Forbidden, 404: NotFound,
		408: Timeout, 413: TooLarge, 507: QuotaExceeded, 500: ServerError, 418: ServerError,
	} {
		e := StatusError(status)
		if e.Code != want {
			t.Errorf("StatusError(%d) = %s", status, e.Code)
		}
		if want == ServerError && e.Status != status {
			t.Errorf("StatusError(%d) lost the status", status)
		}
	}
	if Retryable(StatusError(302)) || Retryable(StatusError(404)) || !Retryable(StatusError(502)) {
		t.Error("retry rule for statuses")
	}
}

func TestMapNetError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Code
	}{
		{&netguard.RefusedError{Target: "10.0.0.1", Reason: "x"}, SSRFBlocked},
		{fmt.Errorf("dial: %w", &netguard.RefusedError{}), SSRFBlocked},
		{context.DeadlineExceeded, Timeout},
		{&net.DNSError{Err: "no such host", Name: "h"}, DNSFailed},
		{&net.DNSError{Err: "t", IsTimeout: true}, Timeout},
		{x509.UnknownAuthorityError{}, TLSUntrusted},
		{x509.HostnameError{Host: "h"}, TLSWrongHost},
		{x509.CertificateInvalidError{Reason: x509.Expired}, TLSUntrusted},
		{syscall.ECONNREFUSED, ConnectRefused},
		{&net.OpError{Op: "read", Err: timeoutErr{}}, Timeout},
		{errors.New("anything else"), ServerError},
		{Errf(ClockSkew), ClockSkew},
	} {
		if got := MapNetError(tc.err); got.Code != tc.want {
			t.Errorf("MapNetError(%T) = %s, want %s", tc.err, got.Code, tc.want)
		}
	}
	if MapNetError(nil) != nil {
		t.Error("nil")
	}
	if CodeOf(errors.New("x")) != ServerError || CodeOf(Errf(NotFound)) != NotFound {
		t.Error("CodeOf")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestCaps(t *testing.T) {
	if _, err := ReadCapped(strings.NewReader("12345"), 5); err != nil {
		t.Errorf("at the cap: %v", err)
	}
	if _, err := ReadCapped(strings.NewReader("123456"), 5); CodeOf(err) != TooLarge {
		t.Errorf("above the cap: %v", err)
	}
	// A list reply above MaxListBody is refused, without reading it all.
	big := io.LimitReader(zeros{}, MaxListBody+10)
	if _, err := ReadCapped(big, MaxListBody); CodeOf(err) != TooLarge {
		t.Errorf("list above 16 MiB: %v", err)
	}
	if DownloadCap(10) != 10+DownloadSlack || DownloadCap(-5) != DownloadSlack {
		t.Error("DownloadCap")
	}
	for _, tc := range []struct {
		body, max int
		fail      bool
	}{{10, 10, false}, {11, 10, true}, {0, 0, false}, {1, 0, true}, {100000, 99999, true}} {
		_, err := io.Copy(io.Discard, CapReader(bytes.NewReader(make([]byte, tc.body)), int64(tc.max)))
		if (err != nil) != tc.fail || (err != nil && CodeOf(err) != TooLarge) {
			t.Errorf("CapReader body=%d max=%d: %v", tc.body, tc.max, err)
		}
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }

func TestCheckName(t *testing.T) {
	if CheckName("launchpad-a1b2c3d4-20261026T031500Z-09af.json") != nil {
		t.Error("good name refused")
	}
	for _, n := range []string{"../x", "a/b", "", "launchpad-a1b2c3d4-20261026T031500Z-09af.json/..", "/abs"} {
		if CheckName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
}
