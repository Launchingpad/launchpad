package offsite

import (
	"context"
	"errors"
	"strings"

	"devops/internal/backup/driver"
)

// Causes of a cancelled run context.
var (
	// errLost: the run row was cancelled or interrupted under the worker (or its
	// destination was deleted or disabled); the worker aborts and writes nothing.
	errLost = errors.New("offsite: the run was cancelled")
	// errStalled: no progress for StallAfter (#40).
	errStalled = errors.New("offsite: the transfer stalled")
	// errRunTimeout: the whole run took longer than RunTimeout (#40).
	errRunTimeout = errors.New("offsite: the run timed out")
)

// internalError marks a failure of our own code (a database or file error). Its
// text is for the server log only; a run row gets one fixed sentence.
type internalError struct{ err error }

func (e *internalError) Error() string { return e.err.Error() }
func (e *internalError) Unwrap() error { return e.err }

func internal(err error) error {
	if err == nil {
		return nil
	}
	var ie *internalError
	if errors.As(err, &ie) {
		return err
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	return &internalError{err: err}
}

const internalText = "the backup worker hit an internal error; see the server log"

// fixedText is the fixed sentence of a code, without the code (it has its own column).
func fixedText(c driver.Code) string {
	return strings.TrimPrefix(driver.Errf(c).Error(), string(c)+": ")
}

// failure is what a run stores when it fails: a safe code, a status for
// server_error, and a fixed text. Nothing a remote said can reach it.
type failure struct {
	code   driver.Code
	status int
	text   string
	// internalErr is logged by the worker (never stored).
	internalErr error
}

func (f failure) retryable() bool {
	return driver.Retryable(&driver.Error{Code: f.code, Status: f.status})
}

// classify turns the end of a run into a failure. ctx is the run's context: its
// cause says stall or timeout, which the driver may have reported as a plain
// cancellation.
func classify(ctx context.Context, err error) failure {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errStalled):
		return failure{code: driver.Stalled, text: fixedText(driver.Stalled)}
	case errors.Is(cause, errRunTimeout):
		return failure{code: driver.Timeout, text: fixedText(driver.Timeout)}
	}
	var de *driver.Error
	if errors.As(err, &de) {
		f := failure{code: de.Code, status: de.Status, text: fixedText(de.Code)}
		if de.Code == driver.ServerError && de.Status != 0 {
			f.text = driver.ServerErr(de.Status).Error()[len("server_error: "):]
		}
		return f
	}
	var ie *internalError
	if errors.As(err, &ie) {
		return failure{code: driver.ServerError, text: internalText, internalErr: ie.err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return failure{code: driver.Timeout, text: fixedText(driver.Timeout)}
	}
	// Anything else came from a driver that broke the contract: its words are not stored.
	return failure{code: driver.ServerError, text: fixedText(driver.ServerError)}
}
