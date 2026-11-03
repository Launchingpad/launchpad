package scm

import (
	"context"
	"net/http"
	"time"
)

// ParseResult says what Parse made of a delivery.
type ParseResult int

// The parse results.
const (
	// ParseOK: the Event is filled and goes to the shared writer.
	ParseOK ParseResult = iota
	// ParseIgnored: a delivery of a kind the tracker does not use (a push
	// event, say). It is acknowledged and never stored.
	ParseIgnored
)

// Provider is one host family (T17 P1). It is pure: it never dials.
type Provider interface {
	Name() string // "gitlab", "github", "bitbucket"
	// Verify checks the delivery against ONE secret. It sees the headers and
	// the body (GitHub and Bitbucket sign the body; GitLab compares a token).
	Verify(h http.Header, body []byte, secret []byte) bool
	// Parse turns a delivery into a neutral Event, or reports "ignored".
	Parse(h http.Header, body []byte) (Event, ParseResult, error)
	// DeliveryKeys are the dedupe keys of a delivery (T17 #14).
	DeliveryKeys(h http.Header, ev Event) ([]string, error)
	// Client returns the API client, or an error when the provider has none.
	Client(cfg ClientConfig) (APIClient, error)
}

// ClientConfig is what a provider needs to build its API client.
type ClientConfig struct {
	BaseURL string // the host's base URL, normalised
	Token   string // the integration's API token; never logged
}

// RepoRef names a repository. ID is opaque: ListRepos makes it and GetRepo
// takes it back.
type RepoRef struct {
	ID   string
	Key  string // the provider's stable id as text (Event.RepoKey)
	Path string // owner/name or path_with_namespace
	URL  string
}

// HookSpec describes the webhook the tracker wants on a repository.
type HookSpec struct {
	URL    string
	Secret string // never logged
	// Reviews adds the review events (LAUN-83, decisions-T17 #24): GitHub
	// pull_request_review. GitLab sends approvals in its merge request
	// events, so it has nothing to add.
	Reviews bool
	// Checks adds the check events (LAUN-85, decisions-T17 #19, #24): GitHub
	// check_suite and status, GitLab pipeline events. Bitbucket's commit
	// status events come with LAUN-84.
	Checks bool
}

// HookRef names a webhook the host holds.
type HookRef struct {
	ID string
}

// Identity is who an API token belongs to.
type Identity struct {
	Login string
	Name  string
}

// RepoPage is one page of repositories. Next is empty on the last page.
type RepoPage struct {
	Items []RepoRef
	Next  string
}

// Change is a merge request or pull request read through the API.
type Change struct {
	Number       int64
	URL          string
	Title        string
	SourceBranch string
	Description  string
	Author       string
	Draft        bool
	Fork         bool
	HeadSHA      string
	State        string // opened, merged or closed
	Review       Review
	// ReviewKnown is true when Review is the host's own answer (it came in the
	// same payload). Otherwise Review is none and means "not read"; the
	// backfill asks a ReviewReader (LAUN-96).
	ReviewKnown bool
	Checks      ChecksState
	UpdatedAt   time.Time
}

// ReviewReader is implemented by an APIClient that can read the review state
// of one change with an extra call (GitHub reviews, GitLab approvals). The
// backfill uses it for a change whose list answer did not carry the review.
type ReviewReader interface {
	ReadReview(ctx context.Context, repo RepoRef, number int64) (Review, error)
}

// ChangePage is one page of changes. Next is empty on the last page.
type ChangePage struct {
	Items []Change
	Next  string
}

// APIClient is the outbound side of a provider (LAUN-6 implements it, all
// HTTP in internal/gitapi).
type APIClient interface {
	CheckToken(ctx context.Context) (Identity, error)
	ListRepos(ctx context.Context, query, cursor string) (RepoPage, error)
	EnsureHook(ctx context.Context, repo RepoRef, hook HookSpec) (HookRef, error)
	DeleteHook(ctx context.Context, repo RepoRef, hook HookRef) error
	GetRepo(ctx context.Context, id string) (RepoRef, error)
	GetChange(ctx context.Context, repo RepoRef, number int64) (Change, error)
	// ListOpenChanges is for the backfill, newest first.
	ListOpenChanges(ctx context.Context, repo RepoRef, cursor string) (ChangePage, error)
}
