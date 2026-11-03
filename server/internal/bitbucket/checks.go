package bitbucket

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"devops/internal/scm"
)

// The commit status events (LAUN-85 left them to LAUN-84, decisions-T17 #19).
// A status event names a commit, not a pull request: the tracker applies it to
// the links whose head sha is that commit. The check name is the build key.
//
// Cloud: repo:commit_status_created and repo:commit_status_updated carry
// commit_status {key, name, state, created_on, updated_on, commit.hash,
// links.commit.href}. UNVERIFIED: whether commit.hash is present (the
// commit href is the fallback).
//
// Data Center: the build status event (repo:build_status) is UNVERIFIED in
// name and in shape; the parser reads buildStatus {key, name, state,
// dateAdded}, a top-level repository {id, slug, project.key} and a commit
// given as a string or as {id}. A delivery that has none of these is a 400.

// checkState maps a Bitbucket build state (Cloud and Data Center share the
// words SUCCESSFUL, FAILED, INPROGRESS; STOPPED and CANCELLED are a stopped
// build, a failure as GitLab's canceled is). ok is false for any other word.
func checkState(s string) (scm.ChecksState, bool) {
	switch strings.ToUpper(s) {
	case "SUCCESSFUL":
		return scm.ChecksSuccess, true
	case "FAILED", "STOPPED", "CANCELLED", "CANCELED":
		return scm.ChecksFailure, true
	case "INPROGRESS":
		return scm.ChecksPending, true
	}
	return scm.ChecksNone, false
}

type cloudStatusEvent struct {
	Repository   cloudRepo `json:"repository"`
	CommitStatus struct {
		Key       string `json:"key"`
		Name      string `json:"name"`
		State     string `json:"state"`
		CreatedOn string `json:"created_on"`
		UpdatedOn string `json:"updated_on"`
		Commit    struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Links struct {
			Commit struct {
				Href string `json:"href"`
			} `json:"commit"`
		} `json:"links"`
	} `json:"commit_status"`
}

func parseCloudStatus(body []byte) (scm.Event, error) {
	var e cloudStatusEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return scm.Event{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	rk, ok := e.Repository.key()
	if !ok {
		return scm.Event{}, errors.New("repository.uuid or repository.workspace.uuid is missing")
	}
	cs := e.CommitStatus
	sha := cs.Commit.Hash
	if sha == "" && len(cs.Links.Commit.Href) <= maxCommitHrefBytes {
		// .../commit/<hash>
		h := strings.TrimRight(cs.Links.Commit.Href, "/")
		sha = h[strings.LastIndex(h, "/")+1:]
	}
	if !hexSHARE.MatchString(sha) {
		return scm.Event{}, errors.New("commit_status.commit.hash is missing")
	}
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChecks, RepoKey: rk, RepoPath: e.Repository.FullName,
		HeadSHA: sha, CheckName: firstNonEmpty(cs.Key, cs.Name, defaultCheckName),
		Checks: scm.ChecksNone, Review: scm.ReviewNone,
		OccurredAt: parseTime(cloudTimeFmt, firstNonEmpty(cs.UpdatedOn, cs.CreatedOn)),
	}
	if st, ok := checkState(cs.State); ok {
		ev.Action, ev.Checks = scm.ActionChecks, st
	}
	return ev, nil
}

type dcBuildEvent struct {
	Date        string          `json:"date"`
	Repository  dcRepo          `json:"repository"`
	Commit      json.RawMessage `json:"commit"`
	BuildStatus struct {
		Key       string `json:"key"`
		Name      string `json:"name"`
		State     string `json:"state"`
		DateAdded int64  `json:"dateAdded"`
	} `json:"buildStatus"`
}

// dcCommit reads a commit given as a bare string or as an object with an id.
func dcCommit(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.ID
	}
	return ""
}

func parseDCBuild(body []byte) (scm.Event, error) {
	var e dcBuildEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return scm.Event{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	if e.Repository.ID < 1 {
		return scm.Event{}, errors.New("repository.id is missing")
	}
	sha := dcCommit(e.Commit)
	if !hexSHARE.MatchString(sha) {
		return scm.Event{}, errors.New("commit is missing")
	}
	at := parseTime(dcDateFmt, e.Date)
	if e.BuildStatus.DateAdded > 0 {
		at = time.UnixMilli(e.BuildStatus.DateAdded).UTC()
	}
	bs := e.BuildStatus
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChecks, RepoKey: strconv.FormatInt(e.Repository.ID, 10), RepoPath: e.Repository.path(),
		HeadSHA: sha, CheckName: firstNonEmpty(bs.Key, bs.Name, defaultCheckName),
		Checks: scm.ChecksNone, Review: scm.ReviewNone, OccurredAt: at,
	}
	if st, ok := checkState(bs.State); ok {
		ev.Action, ev.Checks = scm.ActionChecks, st
	}
	return ev, nil
}
