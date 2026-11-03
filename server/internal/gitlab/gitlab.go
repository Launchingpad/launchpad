// Package gitlab reads GitLab merge_request webhook payloads (T5)
// and is the first scm.Provider (T17, LAUN-4): it turns a payload into the
// neutral scm.Event. What an event asks for (scm.Plan) and the issue
// identifiers an MR names (scm.Refs) live in internal/scm. It is pure: no database, no HTTP
// server, so every rule here is unit-tested against the fixtures in
// testdata/ (hand-written from GitLab's documented payload shape, not
// recorded deliveries; see testdata/README.md). The database side is internal/tracker/gitlab_webhook.go; the
// rules and judgment calls are docs/internal/decisions-T5.md.
package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"crypto/subtle"

	"devops/internal/scm"
)

// The webhook request headers GitLab sends.
const (
	HeaderToken     = "X-Gitlab-Token"
	HeaderEvent     = "X-Gitlab-Event"
	HeaderEventUUID = "X-Gitlab-Event-UUID"
	// HeaderIdempotencyKey is sent by newer GitLab versions (reported as
	// 17.4+, not verified here) and is meant to stay the same when one
	// delivery is sent again. It is preferred for the dedupe key when present.
	HeaderIdempotencyKey = "Idempotency-Key"

	// MergeRequestHook is the X-Gitlab-Event value of a merge_request event.
	// Every other event type is acknowledged and ignored.
	MergeRequestHook = "Merge Request Hook"
	// PipelineHook is the X-Gitlab-Event value of a pipeline event (LAUN-85).
	PipelineHook = "Pipeline Hook"
)

// MaxEventUUIDBytes bounds X-Gitlab-Event-UUID (GitLab sends a 36-character
// UUID) and the Idempotency-Key header.
const MaxEventUUIDBytes = 100

// maxActionBytes bounds the action part of a delivery key.
const maxActionBytes = 32

// DeliveryKeys are the dedupe keys of one webhook delivery, stored in
// git_webhook_events.event_uuid (text, UNIQUE per org and integration):
//
//   - "gl:ik:" + the Idempotency-Key header, when GitLab sent one;
//   - "gl:ev:" + X-Gitlab-Event-UUID (always: the handler requires it);
//
// each followed by ":p" + project id + ":mr" + iid + ":" + action. With
// both headers there are two keys, the gl:ik: one first; the delivery is a
// duplicate if EITHER was seen before, so a resend that drops or changes
// one header still dedupes.
//
// Neither header alone is enough. GitLab can give every hook that one
// request or job fires the same Event UUID (and possibly the same
// Idempotency-Key; neither is verified in T5), so two MRs merged by one push
// would share it and the second would be answered "duplicate". The MR's
// project, iid and action tell those apart, and a delivery sent again still
// has the same keys. The prefixes keep the two forms apart. Both headers are
// length-checked printable ASCII (the handler checks them; the action is cut
// here), so a key is at most about 200 bytes and holds nothing secret.
func DeliveryKeys(idempotencyKey, eventUUID string, ev MergeRequestEvent) []string {
	action := ev.Attrs.Action
	clean := make([]byte, 0, min(len(action), maxActionBytes))
	for i := 0; i < len(action) && len(clean) < maxActionBytes; i++ {
		if c := action[i]; c > 0x20 && c < 0x7f && c != ':' {
			clean = append(clean, c)
		}
	}
	suffix := ":p" + strconv.FormatInt(ev.Project.ID, 10) + ":mr" + strconv.FormatInt(ev.Attrs.IID, 10) + ":" + string(clean)
	keys := make([]string, 0, 2)
	if idempotencyKey != "" {
		keys = append(keys, "gl:ik:"+idempotencyKey+suffix)
	}
	return append(keys, "gl:ev:"+eventUUID+suffix)
}

// MergeRequestEvent is the part of a merge_request payload the tracker reads.
// Everything else in the payload is ignored and never stored.
type MergeRequestEvent struct {
	ObjectKind string `json:"object_kind"`
	// User is who caused the event (for an open, the author).
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	Attrs struct {
		IID          int64  `json:"iid"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		SourceBranch string `json:"source_branch"`
		State        string `json:"state"`
		Action       string `json:"action"`
		URL          string `json:"url"`
		UpdatedAt    string `json:"updated_at"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		// draft is the current name; work_in_progress the older one. GitLab
		// sends both today.
		Draft          *bool `json:"draft"`
		WorkInProgress *bool `json:"work_in_progress"`
	} `json:"object_attributes"`
	Changes struct {
		Draft          *BoolChange `json:"draft"`
		WorkInProgress *BoolChange `json:"work_in_progress"`
	} `json:"changes"`
}

// BoolChange is one entry of a payload's "changes" object.
type BoolChange struct {
	Previous *bool `json:"previous"`
	Current  *bool `json:"current"`
}

// Parse decodes a merge_request payload and checks the fields the tracker
// cannot work without.
func Parse(body []byte) (MergeRequestEvent, error) {
	var ev MergeRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return MergeRequestEvent{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	switch {
	case ev.ObjectKind != "merge_request":
		return MergeRequestEvent{}, errors.New("object_kind must be merge_request")
	case ev.Project.ID < 1:
		return MergeRequestEvent{}, errors.New("project.id is missing")
	case ev.Attrs.IID < 1 || ev.Attrs.IID > math.MaxInt32:
		return MergeRequestEvent{}, errors.New("object_attributes.iid is missing or out of range")
	}
	return ev, nil
}

// IsDraft reports whether the MR is a draft now.
func (e MergeRequestEvent) IsDraft() bool {
	if e.Attrs.Draft != nil {
		return *e.Attrs.Draft
	}
	return e.Attrs.WorkInProgress != nil && *e.Attrs.WorkInProgress
}

// becameReady reports an update that marked a draft MR ready.
func (e MergeRequestEvent) becameReady() bool {
	c := e.Changes.Draft
	if c == nil {
		c = e.Changes.WorkInProgress
	}
	return c != nil && c.Previous != nil && c.Current != nil && *c.Previous && !*c.Current
}

// Event maps the payload to the neutral event. "update" is "ready" when a
// draft MR became ready on an open MR, else "updated".
func (e MergeRequestEvent) Event() scm.Event {
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChange,
		RepoKey: strconv.FormatInt(e.Project.ID, 10), RepoPath: e.Project.PathWithNamespace,
		Number: e.Attrs.IID, URL: e.Attrs.URL, Title: e.Attrs.Title,
		SourceBranch: e.Attrs.SourceBranch, Description: e.Attrs.Description,
		Draft: e.IsDraft(), HeadSHA: e.Attrs.LastCommit.ID, OccurredAt: parseTime(e.Attrs.UpdatedAt),
	}
	switch e.Attrs.Action {
	case "open":
		ev.Action = scm.ActionOpened
		// The user of an open event is the author. Later events name whoever
		// acted, so they leave the author as it is.
		ev.Author = e.User.Username
	case "reopen":
		ev.Action = scm.ActionReopened
	case "update":
		ev.Action = scm.ActionUpdated
		if e.Attrs.State == "opened" && e.becameReady() {
			ev.Action = scm.ActionReady
		}
	case "merge":
		ev.Action = scm.ActionMerged
	case "close":
		ev.Action = scm.ActionClosed
	case "approved":
		// GitLab's "approved" is the WHOLE merge request fully approved by all
		// required approvers; "unapproved" is it losing that status. The single
		// person's "approval" / "unapproval" actions are ignored on purpose
		// (LAUN-96): one person's removal must not clear another's approval.
		ev.Action = scm.ActionApproved
	case "unapproved":
		ev.Action = scm.ActionUnapproved
	default:
		ev.Action = "" // an action the tracker does not know is never a real scm action
	}
	return ev
}

// parseTime reads GitLab's "2006-01-02 15:04:05 UTC" (and the ISO form some
// versions send). A time that does not parse is the zero time, which means
// "unknown" to the tracker.
func parseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Plan is what the event asks for; see scm.Event.Plan.
func (e MergeRequestEvent) Plan() scm.Plan { return e.Event().Plan() }

// ProviderName is Provider.Name.
const ProviderName = "gitlab"

// Provider is the GitLab scm.Provider. It has no API client yet (LAUN-6).
type Provider struct{}

var _ scm.Provider = Provider{}

// Name implements scm.Provider.
func (Provider) Name() string { return ProviderName }

// Verify compares X-Gitlab-Token with the secret in constant time. The
// webhook route does not use it: a stored GitLab integration is verified by
// the hash of its secret (T5), before the body is read.
func (Provider) Verify(h http.Header, _ []byte, secret []byte) bool {
	return len(secret) > 0 && subtle.ConstantTimeCompare([]byte(h.Get(HeaderToken)), secret) == 1
}

// Parse implements scm.Provider: anything but a Merge Request Hook is ignored
// (and its body is not read).
func (Provider) Parse(h http.Header, body []byte) (scm.Event, scm.ParseResult, error) {
	switch h.Get(HeaderEvent) {
	case MergeRequestHook:
	case PipelineHook:
		p, err := ParsePipeline(body)
		if err != nil {
			return scm.Event{}, scm.ParseOK, err
		}
		return p.Event(), scm.ParseOK, nil
	default:
		return scm.Event{}, scm.ParseIgnored, nil
	}
	mr, err := Parse(body)
	if err != nil {
		return scm.Event{}, scm.ParseOK, err
	}
	return mr.Event(), scm.ParseOK, nil
}

// DeliveryKeys implements scm.Provider: see the package function. The key
// keeps GitLab's own action words ("merge", not "merged"), so keys stored
// before the refactor still match a resend.
func (Provider) DeliveryKeys(h http.Header, ev scm.Event) ([]string, error) {
	uuid := h.Get(HeaderEventUUID)
	if uuid == "" {
		return nil, errors.New(HeaderEventUUID + " is missing")
	}
	var mr MergeRequestEvent
	mr.Project.ID, _ = strconv.ParseInt(ev.RepoKey, 10, 64)
	mr.Attrs.IID, mr.Attrs.Action = ev.Number, rawAction(ev.Action)
	if ev.Kind == scm.KindChecks {
		return checkKeys(h.Get(HeaderIdempotencyKey), uuid, mr.Project.ID, ev), nil
	}
	return DeliveryKeys(h.Get(HeaderIdempotencyKey), uuid, mr), nil
}

// Client implements scm.Provider.
func (Provider) Client(scm.ClientConfig) (scm.APIClient, error) {
	return nil, errors.New("gitlab: no API client yet")
}

// rawAction is the inverse of Event's action mapping.
func rawAction(a scm.Action) string {
	switch a {
	case scm.ActionOpened:
		return "open"
	case scm.ActionReopened:
		return "reopen"
	case scm.ActionReady, scm.ActionUpdated:
		return "update"
	case scm.ActionMerged:
		return "merge"
	case scm.ActionClosed:
		return "close"
	default:
		return string(a)
	}
}
