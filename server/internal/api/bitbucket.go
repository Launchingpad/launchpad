package api

import (
	"fmt"
	"net/http"

	"devops/internal/bitbucket"
)

// T17 (LAUN-84): the Bitbucket webhook, Cloud and Data Center. Both sign the
// body (X-Hub-Signature: sha256=<hex>), so it takes the same road as GitHub's
// (signedBodyWebhook, decisions-T17 #14). Parser and fixtures: internal/bitbucket.

var bitbucketSigned = signedWebhook{
	name: bitbucket.ProviderName, prov: bitbucket.Provider{},
	signatureOK: func(h http.Header) bool { _, ok := bitbucket.ParseSignature(h); return ok },
	eventHeader: bitbucket.HeaderEvent,
	handles:     bitbucket.Handles,
	// Data Center's "Test connection" sends diagnostics:ping.
	ping: func(e string) bool { return e == bitbucket.EventDCDiagnostics },
	deliveryOK: func(h http.Header) bool {
		_, ok := bitbucket.Delivery(h)
		return ok
	},
	deliveryMsg: fmt.Sprintf("%s (Cloud) or %s (Data Center) is required: 1-%d printable characters",
		bitbucket.HeaderRequestUUID, bitbucket.HeaderRequestID, bitbucket.MaxDeliveryBytes),
}

// bitbucketWebhook: POST /orgs/{org}/integrations/bitbucket/webhook.
func (s *Server) bitbucketWebhook(w http.ResponseWriter, r *http.Request) {
	s.signedBodyWebhook(w, r, bitbucketSigned)
}
