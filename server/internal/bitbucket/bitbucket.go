// Package bitbucket reads Bitbucket webhook deliveries (T17, LAUN-84) and is
// the Bitbucket scm.Provider for Bitbucket Cloud and Bitbucket Data Center. It
// verifies the HMAC-SHA256 signature of a delivery, turns a pull request,
// approval or commit status payload into the neutral scm.Event and makes the
// delivery keys. It is pure: no database, no HTTP server and no HTTP client of
// its own (the API client is internal/gitapi/bitbucket, reached only through
// Provider.Client). The fixtures in testdata/ are hand-written from the
// vendors' documented payload shapes with invented data, not recorded
// deliveries (testdata/README.md); every shape fact that was not seen live is
// listed in docs/internal/decisions-T17-git-connections-import.md ("LAUN-84").
package bitbucket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"devops/internal/gitapi"
	gitbb "devops/internal/gitapi/bitbucket"
	"devops/internal/scm"
)

// ProviderName is Provider.Name.
const ProviderName = "bitbucket"

// The webhook request headers. Cloud sends X-Request-UUID (per delivery) and
// X-Hook-UUID (per hook, not part of any key); Data Center sends X-Request-Id.
// Both sign the body with X-Hub-Signature: sha256=<hex> when the hook has a
// secret.
const (
	HeaderSignature   = "X-Hub-Signature"
	HeaderEvent       = "X-Event-Key"
	HeaderRequestUUID = "X-Request-UUID"
	HeaderRequestID   = "X-Request-Id"
)

// The X-Event-Key values the tracker uses. Cloud's are pullrequest:* and
// repo:commit_status_*; Data Center's are pr:* and repo:build_status.
const (
	EventCloudCreated    = "pullrequest:created"
	EventCloudUpdated    = "pullrequest:updated"
	EventCloudFulfilled  = "pullrequest:fulfilled"
	EventCloudRejected   = "pullrequest:rejected"
	EventCloudApproved   = "pullrequest:approved"
	EventCloudUnapproved = "pullrequest:unapproved"
	EventCloudStatusNew  = "repo:commit_status_created"
	EventCloudStatusUpd  = "repo:commit_status_updated"

	EventDCOpened      = "pr:opened"
	EventDCFromRef     = "pr:from_ref_updated"
	EventDCModified    = "pr:modified"
	EventDCMerged      = "pr:merged"
	EventDCDeclined    = "pr:declined"
	EventDCApproved    = "pr:reviewer:approved"
	EventDCUnapproved  = "pr:reviewer:unapproved"
	EventDCNeedsWork   = "pr:reviewer:needs_work"
	EventDCBuildStatus = "repo:build_status" // UNVERIFIED name and payload (decisions-T17, LAUN-84)
	EventDCDiagnostics = "diagnostics:ping"  // Data Center's "Test connection"
)

const (
	maxRepoKeyBytes    = 200
	signaturePrefix    = "sha256="
	defaultCheckName   = "check"
	maxDeliveryBytes   = 100
	maxCommitHrefBytes = 1000
)

// MaxDeliveryBytes bounds the delivery header values.
const MaxDeliveryBytes = maxDeliveryBytes

// Handles reports whether the tracker reads an X-Event-Key value. Any other
// event is acknowledged and ignored without its body being parsed.
func Handles(event string) bool {
	switch event {
	case EventCloudCreated, EventCloudUpdated, EventCloudFulfilled, EventCloudRejected, EventCloudApproved,
		EventCloudUnapproved, EventCloudStatusNew, EventCloudStatusUpd,
		EventDCOpened, EventDCFromRef, EventDCModified, EventDCMerged, EventDCDeclined, EventDCApproved,
		EventDCUnapproved, EventDCNeedsWork, EventDCBuildStatus:
		return true
	}
	return false
}

// ParseSignature returns the 32 signature bytes of an X-Hub-Signature header,
// or false when the header is missing or not "sha256=" plus 64 hex characters.
func ParseSignature(h http.Header) ([]byte, bool) {
	v := h.Get(HeaderSignature)
	if len(v) != len(signaturePrefix)+2*sha256.Size || v[:len(signaturePrefix)] != signaturePrefix {
		return nil, false
	}
	sig, err := hex.DecodeString(v[len(signaturePrefix):])
	if err != nil {
		return nil, false
	}
	return sig, true
}

// Sign returns the X-Hub-Signature value of body under secret.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return signaturePrefix + hex.EncodeToString(m.Sum(nil))
}

// Matches reports whether sig is the HMAC-SHA256 of body under secret, in
// constant time. An empty secret never matches.
func Matches(secret, body, sig []byte) bool {
	if len(secret) == 0 {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), sig)
}

// Delivery returns the delivery id of a request: X-Request-UUID (Cloud), else
// X-Request-Id (Data Center). ok is false when neither is 1-100 printable
// ASCII characters (T17 #15).
func Delivery(h http.Header) (string, bool) {
	d := h.Get(HeaderRequestUUID)
	if d == "" {
		d = h.Get(HeaderRequestID)
	}
	return d, validDelivery(d)
}

func validDelivery(s string) bool {
	if s == "" || len(s) > maxDeliveryBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Provider is the Bitbucket scm.Provider. Options carries the guarded HTTP
// client for Client; the zero value is guarded by default (gitapi).
type Provider struct {
	Options gitapi.Options
}

var _ scm.Provider = Provider{}

// Name implements scm.Provider.
func (Provider) Name() string { return ProviderName }

// Verify implements scm.Provider: the X-Hub-Signature header against the
// HMAC-SHA256 of body under secret, in constant time.
func (Provider) Verify(h http.Header, body []byte, secret []byte) bool {
	sig, ok := ParseSignature(h)
	return ok && Matches(secret, body, sig)
}

// Parse implements scm.Provider. The X-Event-Key header picks the flavour and
// the payload shape; an event the tracker has no use for is ignored and its
// body is not read.
func (Provider) Parse(h http.Header, body []byte) (scm.Event, scm.ParseResult, error) {
	key := h.Get(HeaderEvent)
	if !Handles(key) {
		return scm.Event{}, scm.ParseIgnored, nil
	}
	var (
		ev  scm.Event
		err error
	)
	switch key {
	case EventCloudStatusNew, EventCloudStatusUpd:
		ev, err = parseCloudStatus(body)
	case EventDCBuildStatus:
		ev, err = parseDCBuild(body)
	case EventCloudCreated, EventCloudUpdated, EventCloudFulfilled, EventCloudRejected, EventCloudApproved, EventCloudUnapproved:
		ev, err = parseCloudPR(key, body)
	default:
		ev, err = parseDCPR(key, body)
	}
	if err != nil {
		return scm.Event{}, scm.ParseOK, err
	}
	return ev, scm.ParseOK, nil
}

// DeliveryKeys implements scm.Provider (T17 #17): one key,
//
//	bb:<delivery>:r<repo key>:pr<n>:<action>
//	bb:<delivery>:r<repo key>:sha<first 12 of the sha>:checks   (a check event)
//
// The repo key has ":" and "/" replaced by "_" so the separators stay
// unambiguous. The hook UUID is not part of a key: it names the hook.
func (Provider) DeliveryKeys(h http.Header, ev scm.Event) ([]string, error) {
	d, ok := Delivery(h)
	if !ok {
		return nil, errors.New(HeaderRequestUUID + " or " + HeaderRequestID + " must be 1-100 printable characters")
	}
	if len(ev.RepoKey) == 0 || len(ev.RepoKey) > maxRepoKeyBytes {
		return nil, errors.New("repository key is missing or too long")
	}
	repo := strings.NewReplacer(":", "_", "/", "_").Replace(ev.RepoKey)
	if ev.Kind == scm.KindChecks {
		return []string{"bb:" + d + ":r" + repo + ":sha" + scm.ShortSHA(ev.HeadSHA) + ":checks"}, nil
	}
	return []string{"bb:" + d + ":r" + repo + ":pr" + strconv.FormatInt(ev.Number, 10) + ":" + cleanAction(string(ev.Action))}, nil
}

func cleanAction(a string) string {
	var b strings.Builder
	for i := 0; i < len(a) && b.Len() < 32; i++ {
		if c := a[i]; c > 0x20 && c < 0x7f && c != ':' {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// Client implements scm.Provider: the Bitbucket API client of internal/gitapi
// (Bearer tokens only). All HTTP is there, behind the dial-time guard.
func (p Provider) Client(cfg scm.ClientConfig) (scm.APIClient, error) {
	return gitbb.New(cfg, p.Options)
}

func checkNumber(n int64) error {
	if n < 1 || n > math.MaxInt32 {
		return errors.New("pull request id is missing or out of range")
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func parseTime(layouts []string, s string) time.Time {
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
