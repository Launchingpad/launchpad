package backup

import (
	"net/http"
	"time"

	"devops/internal/netguard"
)

// NewHTTPClient is the HTTP client of a user-entered destination: the guarded
// dialer, Proxy nil (the environment's proxy variables are ignored), TLS 1.2 or
// newer with the system roots, and NO redirect followed (a 3xx comes back as a
// response, which drivers map with driver.StatusError to server_error with the
// status, never retried). It is netguard.NewClient; it is here so a driver
// imports one package.
func NewHTTPClient(d *netguard.Dialer, timeout time.Duration) *http.Client {
	return netguard.NewClient(d, timeout)
}
