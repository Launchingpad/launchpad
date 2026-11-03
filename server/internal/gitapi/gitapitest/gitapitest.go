// Package gitapitest helps the provider client tests: an HTTP client on the
// netguard dialer with the test seam that lets httptest's 127.0.0.1 through.
// Only _test.go files import it.
package gitapitest

import (
	"net/http"
	"time"

	"devops/internal/gitapi"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

// Options returns client options with a guarded HTTP client that permits
// loopback and a Sleep that records waits instead of sleeping.
func Options(waits *[]time.Duration) gitapi.Options {
	d := &netguard.Dialer{Policy: netguardtest.Loopback()}
	return gitapi.Options{
		HTTP: gitapi.NewHTTPClient(d),
		Sleep: func(w time.Duration) {
			if waits != nil {
				*waits = append(*waits, w)
			}
		},
	}
}

// HTTPClient returns just the client.
func HTTPClient() *http.Client {
	return gitapi.NewHTTPClient(&netguard.Dialer{Policy: netguardtest.Loopback()})
}
