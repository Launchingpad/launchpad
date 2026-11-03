// Package scm holds the neutral types of the source-control connections
// (T17, LAUN-4): the event a webhook delivery becomes, what an event asks
// for (Plan), the issue identifiers a change names (Refs, MRRefs), the
// Provider interface a host family implements, and the client types the API
// clients implement (LAUN-6). It is pure: no database, no HTTP. The rules and
// judgment calls are docs/internal/decisions-T17-git-connections-import.md
// (P1 to P3) and decisions-T5.md.
package scm

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind is what a delivery is about.
type Kind string

// The kinds of event.
const (
	KindChange Kind = "change" // a merge request or pull request
	KindReview Kind = "review"
	KindChecks Kind = "checks"
)

// Action is what happened to the change.
type Action string

// The actions of an Event.
const (
	ActionOpened     Action = "opened"
	ActionReopened   Action = "reopened"
	ActionReady      Action = "ready" // a draft was marked ready, on an open change
	ActionDrafted    Action = "drafted"
	ActionUpdated    Action = "updated"
	ActionMerged     Action = "merged"
	ActionClosed     Action = "closed"
	ActionApproved   Action = "approved"
	ActionUnapproved Action = "unapproved"
	// ActionChangesRequested: a reviewer asked for changes (GitHub
	// pull_request_review with state changes_requested).
	ActionChangesRequested Action = "changes_requested"
	ActionChecks           Action = "checks"
)

// Review is the review state of a change.
type Review string

// The review states.
const (
	ReviewNone             Review = "none"
	ReviewApproved         Review = "approved"
	ReviewChangesRequested Review = "changes_requested"
)

// ChecksState is the state of a change's checks.
type ChecksState string

// The checks states.
const (
	ChecksNone    ChecksState = "none"
	ChecksPending ChecksState = "pending"
	ChecksSuccess ChecksState = "success"
	ChecksFailure ChecksState = "failure"
)

// Event is one delivery in the neutral shape. It carries no other host data:
// the store never becomes a copy of host content (T5 #14).
type Event struct {
	Provider string // "gitlab", "github", "bitbucket"
	Kind     Kind
	// RepoKey is the provider's stable repository id as text (T17 #8).
	RepoKey  string
	RepoPath string
	// Number is the MR iid or the PR number.
	Number       int64
	Action       Action
	URL          string
	Title        string
	SourceBranch string
	Description  string
	// Author is a login only (a display name is dropped, T17 #18).
	Author string
	Draft  bool
	// Fork is true when the head repository differs from the base repository.
	Fork          bool
	SourceRepoKey string
	HeadSHA       string
	Review        Review
	// ReviewRead is true when Review was read from the host's API (the
	// backfill, LAUN-96): the review of an EXISTING link is then refreshed. A
	// webhook change event leaves it false, so its Review (always none) never
	// wipes a review a review event set.
	ReviewRead bool
	Checks     ChecksState
	// CheckName names the check a KindChecks event is about (a GitHub app slug
	// or status context, a GitLab pipeline name, decisions-T17 #19). Host text.
	CheckName string
	// OccurredAt is when the host says the change happened; it fills
	// provider_updated_at later.
	OccurredAt time.Time
}

// Move is the built-in state an event moves its linked issues towards. A
// team's git workflow rule replaces it (tracker, decisions-T44).
type Move int

// The moves. A built-in move is only ever forward (the tracker checks that).
const (
	MoveNone      Move = iota
	MoveToReview       // the team's review_state_id (In Review)
	MoveToTesting      // the team's testing_state_id (In Testing)
)

func (m Move) String() string {
	switch m {
	case MoveToReview:
		return "review"
	case MoveToTesting:
		return "testing"
	default:
		return "none"
	}
}

// Plan is what one event asks for.
type Plan struct {
	// Link: find the issues the MR names, store an mr_links row for each
	// (and a link attachment the first time) with MRState.
	Link bool
	// CloseLinks: set MRState on the MR's existing links only; link nothing.
	CloseLinks bool
	Move       Move
	// MRState is the mr_links state: opened, merged or closed.
	MRState string
	// Refresh: update the existing links with the event's title, URL, branch,
	// head sha and draft flag (an updated or drafted event). It links nothing
	// and moves nothing (decisions-T17 #16).
	Refresh bool
	// SetReview: set the review state of the existing links (an approval or
	// a request for changes). It moves nothing.
	SetReview bool
	// Reopen marks an explicit reopen: the one "opened" event that may
	// turn a closed link open again (a delayed open or update may not).
	Reopen bool
	// Checks: record one check result on the links whose head sha matches
	// (LAUN-85, decisions-T17 #19). It names no MR, links nothing, moves nothing.
	Checks bool
}

// Noop reports a plan that writes nothing but the event record.
func (p Plan) Noop() bool { return !p.Link && !p.CloseLinks && !p.Refresh && !p.SetReview && !p.Checks }

// Plan maps the event to what the tracker does (T5 #13 to #16):
//
//   - opened / reopened: link; move to review, unless the change is a draft.
//   - ready (a draft marked ready on an open change): link; move to review.
//   - merged: link; move to testing, even for a draft.
//   - closed (unmerged): existing links become closed; no move.
//   - updated, drafted (LAUN-83): refresh the existing links; no move.
//   - approved, unapproved, changes_requested (LAUN-83): set the review of the
//     existing links; no move.
//   - checks (LAUN-85): record the check on the links with this head sha; no move.
//   - anything else: nothing.
func (e Event) Plan() Plan {
	switch e.Action {
	case ActionOpened, ActionReopened:
		p := Plan{Link: true, MRState: "opened", Reopen: e.Action == ActionReopened}
		if !e.Draft {
			p.Move = MoveToReview
		}
		return p
	case ActionReady:
		return Plan{Link: true, MRState: "opened", Move: MoveToReview}
	case ActionMerged:
		return Plan{Link: true, MRState: "merged", Move: MoveToTesting}
	case ActionClosed:
		return Plan{CloseLinks: true, MRState: "closed"}
	case ActionUpdated, ActionDrafted:
		return Plan{Refresh: true}
	case ActionApproved, ActionUnapproved, ActionChangesRequested:
		return Plan{SetReview: true}
	case ActionChecks:
		return Plan{Checks: true}
	default:
		return Plan{}
	}
}

// ReviewValue is the review state a review event sets on a link.
func (e Event) ReviewValue() Review {
	switch e.Action {
	case ActionApproved:
		return ReviewApproved
	case ActionChangesRequested:
		return ReviewChangesRequested
	default:
		return ReviewNone
	}
}

// Ref is an issue identifier: a team key and a number.
type Ref struct {
	Key    string // upper case
	Number int32
}

func (r Ref) String() string { return r.Key + "-" + strconv.Itoa(int(r.Number)) }

// MaxRefs is the most issues one MR event links. More are ignored.
const MaxRefs = 25

// maxMatchesPerText bounds the regex work on one text (a description can be
// megabytes).
const maxMatchesPerText = 1000

// identifierRe is the architecture's matcher. \b is ASCII, so "ORB-12_fix"
// does not match (the "_" is a word character).
var identifierRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]+)-(\d+)\b`)

// Refs finds the identifiers in texts, in order (source branch, then title,
// then description), and keeps the first MaxRefs distinct ones whose key
// passes keep (a team key of the org). Numbers that are not 1..MaxInt32 are
// skipped.
func Refs(keep func(key string) bool, texts ...string) []Ref {
	out := []Ref{}
	seen := map[Ref]bool{}
	for _, t := range texts {
		for _, m := range identifierRe.FindAllStringSubmatch(t, maxMatchesPerText) {
			key := strings.ToUpper(m[1])
			n, err := strconv.ParseInt(m[2], 10, 32)
			if err != nil || n < 1 || !keep(key) {
				continue
			}
			r := Ref{Key: key, Number: int32(n)}
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
			if len(out) == MaxRefs {
				return out
			}
		}
	}
	return out
}

// LinkRef is an issue an MR names, and whether the event may move it.
type LinkRef struct {
	Ref
	// Move is true when the issue is named in the source branch or the
	// title. An issue named ONLY in the description is linked but never
	// moved (Chen, 2026-10-24): a description mentions related tickets
	// ("Refs ORB-2") that this MR does not deliver.
	Move bool
}

// MRRefs is Refs over an MR's branch, title and description (in that order,
// one MaxRefs cap for all three), with Move set for the issues named in the
// branch or the title. An issue named in both places is listed once, with
// Move set.
func MRRefs(keep func(key string) bool, branch, title, description string) []LinkRef {
	movable := map[Ref]bool{}
	for _, r := range Refs(keep, branch, title) {
		movable[r] = true
	}
	all := Refs(keep, branch, title, description)
	out := make([]LinkRef, 0, len(all))
	for _, r := range all {
		out = append(out, LinkRef{Ref: r, Move: movable[r]})
	}
	return out
}

// ShortSHA is the first 12 characters of a head sha, cut to printable ASCII
// without ":" (the part of a check delivery key, decisions-T17 #19). Host
// text: anything else is dropped.
func ShortSHA(sha string) string {
	b := make([]byte, 0, 12)
	for i := 0; i < len(sha) && len(b) < 12; i++ {
		if c := sha[i]; c > 0x20 && c < 0x7f && c != ':' {
			b = append(b, c)
		}
	}
	return string(b)
}

// Repo keys (T17 #8, #15). GitLab and GitHub: the numeric project or
// repository id. Bitbucket Data Center: the numeric repository id. Bitbucket
// Cloud: "{workspace-uuid}/{repo-uuid}", braces kept, lower case.
var (
	numericKeyRE = regexp.MustCompile(`^[0-9]{1,18}$`)
	uuidKeyPart  = `\{[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\}`
	cloudKeyRE   = regexp.MustCompile(`^` + uuidKeyPart + `/` + uuidKeyPart + `$`)
)

// CanonicalRepoKey returns the stored form of a repo key for a provider (a
// numeric key without leading zeros, a Cloud key in lower case), or false when
// the key is not one of the provider's forms. It is the one rule the webhook
// handler, the repo map and the backfill share, so a key written by one is
// found by the others.
func CanonicalRepoKey(provider, key string) (string, bool) {
	k := strings.TrimSpace(key)
	if provider == "bitbucket" {
		k = strings.ToLower(k)
		if cloudKeyRE.MatchString(k) {
			return k, true
		}
	}
	if !numericKeyRE.MatchString(k) {
		return "", false
	}
	n, err := strconv.ParseInt(k, 10, 64)
	if err != nil || n < 1 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

// IsCloudRepoKey reports a Bitbucket Cloud key ("{workspace}/{repo}").
func IsCloudRepoKey(key string) bool { return cloudKeyRE.MatchString(key) }
