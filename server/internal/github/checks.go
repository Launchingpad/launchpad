package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"devops/internal/scm"
)

// The check events (LAUN-85, decisions-T17 #19). A check event names no pull
// request: it names a commit, and the tracker applies it to the links whose
// head sha is that commit. The shapes are GitHub's documented check_suite and
// status payloads; the fixtures are hand-written (testdata/README.md).
const (
	EventCheckSuite = "check_suite"
	EventStatus     = "status"
)

// defaultCheckName is the check name when the host gives none.
const defaultCheckName = "check"

type repoRef struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// CheckSuiteEvent is the part of a check_suite payload the tracker reads.
type CheckSuiteEvent struct {
	Action     string  `json:"action"`
	Repository repoRef `json:"repository"`
	CheckSuite struct {
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		UpdatedAt  string `json:"updated_at"`
		App        struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"app"`
	} `json:"check_suite"`
}

// StatusEvent is the part of a status payload the tracker reads.
type StatusEvent struct {
	SHA        string  `json:"sha"`
	Context    string  `json:"context"`
	State      string  `json:"state"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	Repository repoRef `json:"repository"`
}

func checkRepo(r repoRef) error {
	if r.ID < 1 {
		return errors.New("repository.id is missing")
	}
	return nil
}

// ParseCheckSuite decodes a check_suite payload.
func ParseCheckSuite(body []byte) (CheckSuiteEvent, error) {
	var ev CheckSuiteEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return CheckSuiteEvent{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := checkRepo(ev.Repository); err != nil {
		return CheckSuiteEvent{}, err
	}
	if ev.CheckSuite.HeadSHA == "" {
		return CheckSuiteEvent{}, errors.New("check_suite.head_sha is missing")
	}
	return ev, nil
}

// ParseStatus decodes a status payload.
func ParseStatus(body []byte) (StatusEvent, error) {
	var ev StatusEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return StatusEvent{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := checkRepo(ev.Repository); err != nil {
		return StatusEvent{}, err
	}
	if ev.SHA == "" {
		return StatusEvent{}, errors.New("sha is missing")
	}
	return ev, nil
}

func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// checkSuiteState maps a suite to a checks state: a suite that has not
// completed is pending; a completed one is success (success, neutral,
// skipped), failure (failure, timed_out, cancelled, action_required,
// startup_failure) or, when GitHub calls it stale, pending. ok is false for a
// conclusion the tracker does not know.
func checkSuiteState(action, status, conclusion string) (scm.ChecksState, bool) {
	if status != "completed" && action != "completed" {
		switch status {
		case "", "queued", "in_progress", "requested", "waiting", "pending":
			return scm.ChecksPending, true
		}
		return scm.ChecksNone, false
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return scm.ChecksSuccess, true
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
		return scm.ChecksFailure, true
	case "stale":
		return scm.ChecksPending, true
	}
	return scm.ChecksNone, false
}

// Event maps the suite to the neutral event. A state the tracker does not
// know leaves Action empty. The check name is the app slug (decisions-T17 #19).
func (e CheckSuiteEvent) Event() scm.Event {
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChecks,
		RepoKey: strconv.FormatInt(e.Repository.ID, 10), RepoPath: e.Repository.FullName,
		HeadSHA: e.CheckSuite.HeadSHA, CheckName: firstNonEmpty(e.CheckSuite.App.Slug, e.CheckSuite.App.Name, defaultCheckName),
		Checks: scm.ChecksNone, Review: scm.ReviewNone, OccurredAt: parseTime(e.CheckSuite.UpdatedAt),
	}
	if st, ok := checkSuiteState(e.Action, e.CheckSuite.Status, e.CheckSuite.Conclusion); ok {
		ev.Action, ev.Checks = scm.ActionChecks, st
	}
	return ev
}

// Event maps the status to the neutral event: pending, success, failure and
// error (a failure) are known; the name is the status context.
func (e StatusEvent) Event() scm.Event {
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChecks,
		RepoKey: strconv.FormatInt(e.Repository.ID, 10), RepoPath: e.Repository.FullName,
		HeadSHA: e.SHA, CheckName: firstNonEmpty(e.Context, defaultCheckName),
		Checks: scm.ChecksNone, Review: scm.ReviewNone,
		OccurredAt: parseTime(firstNonEmpty(e.UpdatedAt, e.CreatedAt)),
	}
	switch e.State {
	case "pending":
		ev.Action, ev.Checks = scm.ActionChecks, scm.ChecksPending
	case "success":
		ev.Action, ev.Checks = scm.ActionChecks, scm.ChecksSuccess
	case "failure", "error":
		ev.Action, ev.Checks = scm.ActionChecks, scm.ChecksFailure
	}
	return ev
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// checkKey is the delivery key of a check event (decisions-T17 #19):
//
//	gh:<X-GitHub-Delivery>:r<repo id>:sha<first 12 of the head sha>:checks
//
// It has no pull request number.
func checkKey(delivery string, ev scm.Event) string {
	return "gh:" + delivery + ":r" + ev.RepoKey + ":sha" + scm.ShortSHA(ev.HeadSHA) + ":checks"
}
