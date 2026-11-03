// Package github reads GitHub pull_request webhook deliveries (T17, LAUN-5)
// and is the GitHub scm.Provider: it verifies the HMAC-SHA256 signature of a
// delivery, turns a pull_request payload into the neutral scm.Event and makes
// the delivery keys. It is pure: no database, no HTTP server, no outbound
// HTTP (its API client arrives with LAUN-7). The fixtures in testdata/ are
// hand-written from GitHub's documented payload shape with invented data, not
// recorded deliveries (testdata/README.md). The rules are
// docs/internal/decisions-T17-git-connections-import.md (#14 to #17, open
// point 16).
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"devops/internal/scm"
)

// ProviderName is Provider.Name.
const ProviderName = "github"

// The webhook request headers GitHub sends.
const (
	HeaderSignature = "X-Hub-Signature-256"
	HeaderEvent     = "X-GitHub-Event"
	HeaderDelivery  = "X-GitHub-Delivery"

	// EventPullRequest is the X-GitHub-Event value the tracker uses. "ping"
	// is acknowledged; every other event type is acknowledged and ignored.
	EventPullRequest = "pull_request"
	// EventPullRequestReview carries an approval or a request for changes
	// (LAUN-83). The hook of a mapped repo gets it after a refresh.
	EventPullRequestReview = "pull_request_review"
	EventPing              = "ping"
)

// MaxDeliveryBytes bounds X-GitHub-Delivery (GitHub sends a 36-character
// GUID).
const MaxDeliveryBytes = 100

// maxRepoKeyBytes bounds the repo part of a delivery key (T17 #15).
const maxRepoKeyBytes = 200

// signaturePrefix starts the X-Hub-Signature-256 value.
const signaturePrefix = "sha256="

// ParseSignature returns the 32 signature bytes of an X-Hub-Signature-256
// header, or false when the header is missing or not "sha256=" plus 64 hex
// characters. It looks at the header only.
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

// Sign returns the X-Hub-Signature-256 value of body under secret.
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

// PullRequestEvent is the part of a pull_request payload the tracker reads.
// Everything else in the payload is ignored and never stored.
type PullRequestEvent struct {
	Action string `json:"action"`
	// Review is only on a pull_request_review delivery: its state is the
	// one field the tracker reads.
	Review *struct {
		State string `json:"state"`
	} `json:"review"`
	Number     int64 `json:"number"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number    int64  `json:"number"`
		HTMLURL   string `json:"html_url"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		Draft     bool   `json:"draft"`
		Merged    bool   `json:"merged"`
		UpdatedAt string `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref  string    `json:"ref"`
			SHA  string    `json:"sha"`
			Repo *repoStub `json:"repo"` // null when the fork was deleted
		} `json:"head"`
		Base struct {
			Repo *repoStub `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

type repoStub struct {
	ID int64 `json:"id"`
}

// Parse decodes a pull_request payload and checks the fields the tracker
// cannot work without.
func Parse(body []byte) (PullRequestEvent, error) {
	var ev PullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return PullRequestEvent{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	n := ev.Number
	if n == 0 {
		n = ev.PullRequest.Number
	}
	switch {
	case ev.Action == "":
		return PullRequestEvent{}, errors.New("action is missing")
	case ev.Repository.ID < 1:
		return PullRequestEvent{}, errors.New("repository.id is missing")
	case n < 1 || n > math.MaxInt32:
		return PullRequestEvent{}, errors.New("number is missing or out of range")
	}
	ev.Number = n
	return ev, nil
}

// IsFork reports a pull request whose head repository is not its base
// repository: a fork, or a fork that was deleted (head.repo is null). Open
// point 16: such a delivery asks for nothing.
func (e PullRequestEvent) IsFork() bool {
	head := e.PullRequest.Head.Repo
	if head == nil {
		return true
	}
	base := e.Repository.ID
	if b := e.PullRequest.Base.Repo; b != nil && b.ID > 0 {
		base = b.ID
	}
	return head.ID != base
}

// Event maps the payload to the neutral event. A GitHub action the tracker
// has no use for (labeled, assigned, ...) leaves Action empty.
func (e PullRequestEvent) Event() scm.Event {
	pr := e.PullRequest
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChange,
		RepoKey: strconv.FormatInt(e.Repository.ID, 10), RepoPath: e.Repository.FullName,
		Number: e.Number, URL: pr.HTMLURL, Title: pr.Title,
		SourceBranch: pr.Head.Ref, Description: pr.Body,
		Author: pr.User.Login, Draft: pr.Draft, Fork: e.IsFork(), HeadSHA: pr.Head.SHA,
		Review: scm.ReviewNone, Checks: scm.ChecksNone,
	}
	if r := pr.Head.Repo; r != nil {
		ev.SourceRepoKey = strconv.FormatInt(r.ID, 10)
	}
	if t, err := time.Parse(time.RFC3339, pr.UpdatedAt); err == nil {
		ev.OccurredAt = t.UTC()
	}
	switch e.Action {
	case "opened":
		ev.Action = scm.ActionOpened
	case "reopened":
		ev.Action = scm.ActionReopened
	case "ready_for_review":
		ev.Action = scm.ActionReady
	case "converted_to_draft":
		ev.Action = scm.ActionDrafted
	case "closed":
		ev.Action = scm.ActionClosed
		if pr.Merged {
			ev.Action = scm.ActionMerged
		}
	case "edited", "synchronize":
		ev.Action = scm.ActionUpdated
	default:
		ev.Action = ""
	}
	return ev
}

// ReviewEvent maps a pull_request_review payload to the neutral event. Only
// a submitted approval or request for changes, and a dismissed review, ask for
// anything (a plain comment review leaves Action empty). The pull request is
// read exactly as in Event, so the fork rule and the host time work the same.
func (e PullRequestEvent) ReviewEvent() scm.Event {
	ev := e.Event() // the PR fields; Event maps the review action to none
	ev.Kind = scm.KindReview
	ev.Action = ""
	state := ""
	if e.Review != nil {
		state = e.Review.State
	}
	switch {
	case e.Action == "submitted" && state == "approved":
		ev.Action, ev.Review = scm.ActionApproved, scm.ReviewApproved
	case e.Action == "submitted" && state == "changes_requested":
		ev.Action, ev.Review = scm.ActionChangesRequested, scm.ReviewChangesRequested
	case e.Action == "dismissed":
		ev.Action, ev.Review = scm.ActionUnapproved, scm.ReviewNone
	}
	return ev
}

// ValidDelivery reports whether an X-GitHub-Delivery value is 1-100 printable
// ASCII characters (T17 #15).
func ValidDelivery(s string) bool {
	if s == "" || len(s) > MaxDeliveryBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Provider is the GitHub scm.Provider. It has no API client yet (LAUN-7).
type Provider struct{}

var _ scm.Provider = Provider{}

// Name implements scm.Provider.
func (Provider) Name() string { return ProviderName }

// Verify implements scm.Provider: the X-Hub-Signature-256 header against the
// HMAC-SHA256 of body under secret, in constant time.
func (Provider) Verify(h http.Header, body []byte, secret []byte) bool {
	sig, ok := ParseSignature(h)
	return ok && Matches(secret, body, sig)
}

// Parse implements scm.Provider: anything but a pull_request, a
// pull_request_review, a check_suite or a status event is ignored (and its body is not read).
func (Provider) Parse(h http.Header, body []byte) (scm.Event, scm.ParseResult, error) {
	kind := h.Get(HeaderEvent)
	switch kind {
	case EventCheckSuite:
		cs, err := ParseCheckSuite(body)
		if err != nil {
			return scm.Event{}, scm.ParseOK, err
		}
		return cs.Event(), scm.ParseOK, nil
	case EventStatus:
		st, err := ParseStatus(body)
		if err != nil {
			return scm.Event{}, scm.ParseOK, err
		}
		return st.Event(), scm.ParseOK, nil
	}
	if kind != EventPullRequest && kind != EventPullRequestReview {
		return scm.Event{}, scm.ParseIgnored, nil
	}
	pr, err := Parse(body)
	if err != nil {
		return scm.Event{}, scm.ParseOK, err
	}
	if kind == EventPullRequestReview {
		return pr.ReviewEvent(), scm.ParseOK, nil
	}
	return pr.Event(), scm.ParseOK, nil
}

// DeliveryKeys implements scm.Provider (T17 #17): one key,
//
//	gh:<X-GitHub-Delivery>:r<repo id>:pr<n>:<action>
//
// A redelivery from GitHub's UI keeps the GUID, so it has the same key. The
// action is the neutral action ("x" for none), cut to 32 printable
// characters without ":".
func (Provider) DeliveryKeys(h http.Header, ev scm.Event) ([]string, error) {
	d := h.Get(HeaderDelivery)
	if !ValidDelivery(d) {
		return nil, errors.New(HeaderDelivery + " must be 1-100 printable characters")
	}
	if len(ev.RepoKey) == 0 || len(ev.RepoKey) > maxRepoKeyBytes {
		return nil, errors.New("repository id is missing or too long")
	}
	if ev.Kind == scm.KindChecks {
		return []string{checkKey(d, ev)}, nil
	}
	action := cleanAction(string(ev.Action))
	return []string{"gh:" + d + ":r" + ev.RepoKey + ":pr" + strconv.FormatInt(ev.Number, 10) + ":" + action}, nil
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

// Client implements scm.Provider: not wired until LAUN-7.
func (Provider) Client(scm.ClientConfig) (scm.APIClient, error) {
	return nil, errors.New("github: API client not wired")
}
