package driver

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
	"syscall"

	"devops/internal/netguard"
)

// Code is a safe error code (T16 #32). A code is all a person or a log gets:
// never the remote server's words, a URL, a path or a secret.
type Code string

// The codes of T16 #32.
const (
	DNSFailed         Code = "dns_failed"
	ConnectRefused    Code = "connect_refused"
	TLSUntrusted      Code = "tls_untrusted"
	TLSWrongHost      Code = "tls_wrong_host"
	TLSPinMismatch    Code = "tls_pin_mismatch"
	AuthFailed        Code = "auth_failed"
	Forbidden         Code = "forbidden"
	NotFound          Code = "not_found"
	Timeout           Code = "timeout"
	TooLarge          Code = "too_large"
	QuotaExceeded     Code = "quota_exceeded"
	HostKeyChanged    Code = "host_key_changed"
	SSRFBlocked       Code = "ssrf_blocked"
	SecretsUnreadable Code = "secrets_unreadable"
	NoSpace           Code = "no_space"
	Stalled           Code = "stalled"
	DumpMismatch      Code = "dump_mismatch"
	DumpUnavailable   Code = "dump_unavailable"
	ClockSkew         Code = "clock_skew"
	InstanceChanged   Code = "instance_changed"
	ScratchFull       Code = "scratch_full"
	// NoRecipient: no age recipient is active when the run starts (added by
	// LAUN-71: a removal can land between the enable check and the run).
	NoRecipient Code = "no_recipient"
	// ObjectMismatch: a verify run found the remote object differs from the
	// recorded sha256 or size (LAUN-71).
	ObjectMismatch Code = "object_mismatch"
	ServerError    Code = "server_error"
)

var texts = map[Code]string{
	DNSFailed:         "the host name could not be resolved",
	ConnectRefused:    "the connection was refused",
	TLSUntrusted:      "the certificate is not trusted",
	TLSWrongHost:      "the certificate is for another host",
	TLSPinMismatch:    "the certificate does not match the saved pin",
	AuthFailed:        "the server refused the credentials",
	Forbidden:         "the server refused the request",
	NotFound:          "the file or folder was not found",
	Timeout:           "the server did not answer in time",
	TooLarge:          "the data is larger than the limit",
	QuotaExceeded:     "the destination has no space left",
	HostKeyChanged:    "the SSH host key changed",
	SSRFBlocked:       "the address is not allowed",
	SecretsUnreadable: "the saved secrets cannot be read",
	NoSpace:           "not enough free space for the backup",
	Stalled:           "the transfer stalled",
	DumpMismatch:      "the local dump changed while it was read",
	DumpUnavailable:   "no usable local dump",
	ClockSkew:         "the clock differs from the server's",
	InstanceChanged:   "this database is not the one that made the backups",
	ScratchFull:       "the scratch volume is full",
	NoRecipient:       "no encryption key is active for this backup",
	ObjectMismatch:    "the remote copy does not match the recorded hash",
	ServerError:       "the server returned an error",
}

// Codes lists every code, in a fixed order.
func Codes() []Code {
	return []Code{DNSFailed, ConnectRefused, TLSUntrusted, TLSWrongHost, TLSPinMismatch,
		AuthFailed, Forbidden, NotFound, Timeout, TooLarge, QuotaExceeded, HostKeyChanged,
		SSRFBlocked, SecretsUnreadable, NoSpace, Stalled, DumpMismatch, DumpUnavailable,
		ClockSkew, InstanceChanged, ScratchFull, NoRecipient, ObjectMismatch, ServerError}
}

// Error is the one error type a driver returns. It holds a code and, for
// server_error, a status number. Error() is a fixed text per code; nothing the
// remote said can get into it, because the type has no field for it.
type Error struct {
	Code   Code
	Status int // HTTP or SFTP status; only with ServerError
}

func (e *Error) Error() string {
	t, ok := texts[e.Code]
	if !ok {
		t = "backup error"
	}
	if e.Code == ServerError && e.Status != 0 {
		return string(e.Code) + ": " + t + " (status " + strconv.Itoa(e.Status) + ")"
	}
	return string(e.Code) + ": " + t
}

// Errf returns an *Error for a code.
func Errf(c Code) *Error { return &Error{Code: c} }

// ServerErr returns server_error with a status.
func ServerErr(status int) *Error { return &Error{Code: ServerError, Status: status} }

// CodeOf returns the code of err, or ServerError for any other error, so a
// caller can store a code without ever storing err's text.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ServerError
}

// Retryable is T16 #31's list: timeout, connect_refused, dns_failed, stalled
// and server_error with a 5xx status. Everything else needs a person (a 3xx
// is a configuration fault; 4xx a refusal). A status of 0 is not retried.
func Retryable(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case Timeout, ConnectRefused, DNSFailed, Stalled:
		return true
	case ServerError:
		return e.Status >= 500 && e.Status <= 599
	}
	return false
}

// StatusError maps an HTTP status the driver did not expect to a safe error.
// A 3xx is server_error with its status: the guarded client follows no
// redirect (T16 #34), so a redirect reaches here.
func StatusError(status int) *Error {
	switch {
	case status == 401:
		return Errf(AuthFailed)
	case status == 403:
		return Errf(Forbidden)
	case status == 404:
		return Errf(NotFound)
	case status == 408:
		return Errf(Timeout)
	case status == 413:
		return Errf(TooLarge)
	case status == 507:
		return Errf(QuotaExceeded)
	}
	return ServerErr(status)
}

// MapNetError turns a dial, TLS or I/O error into a safe *Error. The text of
// err is dropped on purpose.
func MapNetError(err error) *Error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	var ref *netguard.RefusedError
	switch {
	case errors.As(err, &ref):
		return Errf(SSRFBlocked)
	case errors.Is(err, context.DeadlineExceeded):
		return Errf(Timeout)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTimeout {
			return Errf(Timeout)
		}
		return Errf(DNSFailed)
	}
	var ua x509.UnknownAuthorityError
	var host x509.HostnameError
	var inv x509.CertificateInvalidError
	switch {
	case errors.As(err, &ua):
		return Errf(TLSUntrusted)
	case errors.As(err, &host):
		return Errf(TLSWrongHost)
	case errors.As(err, &inv):
		return Errf(TLSUntrusted)
	case errors.Is(err, syscall.ECONNREFUSED):
		return Errf(ConnectRefused)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Errf(Timeout)
	}
	return Errf(ServerError)
}
