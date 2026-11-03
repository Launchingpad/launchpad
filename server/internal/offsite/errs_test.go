package offsite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"devops/internal/backup/driver"
)

// The retry rule of decisions-T16 #31 as a table, through the same function the
// worker uses (classify then retryable): transient codes retry once; every code that
// needs a person does not, including a 3xx and not_found.
func TestClassify_RetryTable(t *testing.T) {
	se := driver.ServerErr
	cases := []struct {
		err   error
		retry bool
	}{
		{driver.Errf(driver.Timeout), true},
		{driver.Errf(driver.ConnectRefused), true},
		{driver.Errf(driver.DNSFailed), true},
		{driver.Errf(driver.Stalled), true},
		{se(500), true},
		{se(502), true},
		{se(503), true},
		{se(599), true},
		{se(301), false},
		{se(302), false},
		{se(307), false},
		{se(400), false},
		{se(404), false},
		{se(0), false},
		{driver.Errf(driver.NotFound), false},
		{driver.Errf(driver.AuthFailed), false},
		{driver.Errf(driver.Forbidden), false},
		{driver.Errf(driver.QuotaExceeded), false},
		{driver.Errf(driver.TLSUntrusted), false},
		{driver.Errf(driver.TLSWrongHost), false},
		{driver.Errf(driver.TLSPinMismatch), false},
		{driver.Errf(driver.HostKeyChanged), false},
		{driver.Errf(driver.SSRFBlocked), false},
		{driver.Errf(driver.SecretsUnreadable), false},
		{driver.Errf(driver.TooLarge), false},
		{driver.Errf(driver.NoSpace), false},
		{driver.Errf(driver.DumpMismatch), false},
		{driver.Errf(driver.DumpUnavailable), false},
		{driver.Errf(driver.ClockSkew), false},
		{driver.Errf(driver.InstanceChanged), false},
		{driver.Errf(driver.ScratchFull), false},
		{driver.Errf(driver.NoRecipient), false},
		{driver.Errf(driver.ObjectMismatch), false},
		{fmt.Errorf("wrapped: %w", driver.Errf(driver.Timeout)), true},
		{internal(errors.New("db down")), false},
		{errors.New("a driver that broke the contract"), false},
	}
	for _, c := range cases {
		f := classify(context.Background(), c.err)
		if f.retryable() != c.retry {
			t.Errorf("%v: retryable = %v, want %v", c.err, f.retryable(), c.retry)
		}
	}
}

// What a run stores is a code and a fixed text. A planted secret in an error that
// is not ours never reaches the text.
func TestClassify_TextsAreFixed(t *testing.T) {
	const planted = "PLANTED-SECRET-VALUE"
	for _, err := range []error{
		errors.New("server said: bad password " + planted),
		internal(errors.New("dial tcp " + planted)),
		fmt.Errorf("wrapped %s: %w", planted, errors.New(planted)),
	} {
		f := classify(context.Background(), err)
		if strings.Contains(f.text, planted) || strings.Contains(string(f.code), planted) {
			t.Errorf("%v: the failure holds the planted text: %+v", err, f)
		}
		if f.text == "" {
			t.Errorf("%v: empty text", err)
		}
	}
	de := classify(context.Background(), driver.ServerErr(503))
	if de.code != driver.ServerError || de.status != 503 || !strings.Contains(de.text, "503") {
		t.Errorf("server_error keeps its status: %+v", de)
	}
	for _, c := range driver.Codes() {
		if fixedText(c) == "" || strings.Contains(fixedText(c), string(c)+":") {
			t.Errorf("code %s has a bad fixed text %q", c, fixedText(c))
		}
	}
}

func TestClassify_ContextCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errStalled)
	if f := classify(ctx, context.Canceled); f.code != driver.Stalled || !f.retryable() {
		t.Fatalf("stall: %+v", f)
	}
	ctx, cancel = context.WithCancelCause(context.Background())
	cancel(errRunTimeout)
	if f := classify(ctx, context.Canceled); f.code != driver.Timeout {
		t.Fatalf("timeout: %+v", f)
	}
	if f := classify(context.Background(), context.DeadlineExceeded); f.code != driver.Timeout {
		t.Fatalf("deadline: %+v", f)
	}
}
