package bitbucket

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"devops/internal/scm"
)

var (
	hexSHARE     = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	cloudTimeFmt = []string{time.RFC3339Nano, time.RFC3339}
	dcDateFmt    = []string{"2006-01-02T15:04:05Z0700", time.RFC3339Nano, time.RFC3339}
)

// CanonicalRepoKey returns the stored form of a Bitbucket repo key: a Data
// Center numeric id, or a Cloud "{workspace uuid}/{repo uuid}" pair in lower
// case (T17 #15).
func CanonicalRepoKey(key string) (string, bool) { return scm.CanonicalRepoKey(ProviderName, key) }

// ---- Cloud (API 2.0) ----

type cloudRepo struct {
	UUID      string `json:"uuid"`
	FullName  string `json:"full_name"`
	Workspace *struct {
		UUID string `json:"uuid"`
	} `json:"workspace"`
	Owner *struct {
		UUID string `json:"uuid"`
	} `json:"owner"`
}

// key is "{workspace-uuid}/{repo-uuid}". The workspace uuid is read from
// repository.workspace (current payloads), else repository.owner (older ones):
// both are UNVERIFIED (decisions-T17, LAUN-84).
func (r cloudRepo) key() (string, bool) {
	ws := ""
	switch {
	case r.Workspace != nil && r.Workspace.UUID != "":
		ws = r.Workspace.UUID
	case r.Owner != nil:
		ws = r.Owner.UUID
	}
	return CanonicalRepoKey(ws + "/" + r.UUID)
}

type cloudPR struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Draft       bool   `json:"draft"` // absent on older payloads: not a draft
	UpdatedOn   string `json:"updated_on"`
	Author      struct {
		Nickname  string `json:"nickname"`
		AccountID string `json:"account_id"`
	} `json:"author"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Repository *cloudRepo `json:"repository"` // null for a deleted fork
	} `json:"source"`
	Destination struct {
		Repository *cloudRepo `json:"repository"`
	} `json:"destination"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type cloudEvent struct {
	PullRequest cloudPR   `json:"pullrequest"`
	Repository  cloudRepo `json:"repository"`
}

func parseCloudPR(key string, body []byte) (scm.Event, error) {
	var e cloudEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return scm.Event{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	pr := e.PullRequest
	if err := checkNumber(pr.ID); err != nil {
		return scm.Event{}, err
	}
	repo := e.Repository
	if repo.UUID == "" && pr.Destination.Repository != nil {
		repo = *pr.Destination.Repository
	}
	rk, ok := repo.key()
	if !ok {
		return scm.Event{}, errors.New("repository.uuid or repository.workspace.uuid is missing")
	}
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChange, RepoKey: rk, RepoPath: repo.FullName,
		Number: pr.ID, URL: pr.Links.HTML.Href, Title: pr.Title, SourceBranch: pr.Source.Branch.Name,
		Description: pr.Description, Author: firstNonEmpty(pr.Author.Nickname, pr.Author.AccountID),
		Draft: pr.Draft, HeadSHA: pr.Source.Commit.Hash, Review: scm.ReviewNone, Checks: scm.ChecksNone,
		OccurredAt: parseTime(cloudTimeFmt, pr.UpdatedOn),
	}
	// A fork (or a deleted fork: source.repository null) may name any ticket:
	// open point 16. The source is compared with the destination repository
	// (the repository the hook is on), else with the payload's repository.
	baseUUID := repo.UUID
	if d := pr.Destination.Repository; d != nil && d.UUID != "" {
		baseUUID = d.UUID
	}
	ev.Fork = true
	if s := pr.Source.Repository; s != nil && s.UUID != "" {
		ev.SourceRepoKey = strings.ToLower(s.UUID)
		ev.Fork = !strings.EqualFold(s.UUID, baseUUID)
	}
	switch key {
	case EventCloudCreated:
		ev.Action = scm.ActionOpened
	case EventCloudUpdated:
		ev.Action = scm.ActionUpdated
	case EventCloudFulfilled:
		ev.Action = scm.ActionMerged
	case EventCloudRejected:
		ev.Action = scm.ActionClosed
	case EventCloudApproved:
		ev.Kind, ev.Action, ev.Review = scm.KindReview, scm.ActionApproved, scm.ReviewApproved
	case EventCloudUnapproved:
		ev.Kind, ev.Action = scm.KindReview, scm.ActionUnapproved
	}
	return ev, nil
}

// ---- Data Center (REST 1.0) ----

type dcRepo struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Project struct {
		Key string `json:"key"`
	} `json:"project"`
}

func (r dcRepo) path() string {
	if r.Project.Key == "" || r.Slug == "" {
		return ""
	}
	return r.Project.Key + "/" + r.Slug
}

type dcRef struct {
	DisplayID    string  `json:"displayId"`
	LatestCommit string  `json:"latestCommit"`
	Repository   *dcRepo `json:"repository"`
}

type dcPR struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Draft       bool   `json:"draft"` // absent in the versions known: not a draft
	UpdatedDate int64  `json:"updatedDate"`
	FromRef     dcRef  `json:"fromRef"`
	ToRef       dcRef  `json:"toRef"`
	Author      struct {
		User struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"user"`
	} `json:"author"`
	Links struct {
		Self []struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"links"`
}

type dcEvent struct {
	EventKey    string `json:"eventKey"`
	Date        string `json:"date"`
	PullRequest dcPR   `json:"pullRequest"`
}

func parseDCPR(key string, body []byte) (scm.Event, error) {
	var e dcEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return scm.Event{}, fmt.Errorf("invalid JSON body: %w", err)
	}
	pr := e.PullRequest
	if err := checkNumber(pr.ID); err != nil {
		return scm.Event{}, err
	}
	base := pr.ToRef.Repository
	if base == nil || base.ID < 1 {
		return scm.Event{}, errors.New("pullRequest.toRef.repository.id is missing")
	}
	href := ""
	if len(pr.Links.Self) > 0 {
		href = pr.Links.Self[0].Href
	}
	at := parseTime(dcDateFmt, e.Date)
	if pr.UpdatedDate > 0 {
		at = time.UnixMilli(pr.UpdatedDate).UTC()
	}
	ev := scm.Event{
		Provider: ProviderName, Kind: scm.KindChange, RepoKey: strconv.FormatInt(base.ID, 10), RepoPath: base.path(),
		Number: pr.ID, URL: href, Title: pr.Title, SourceBranch: pr.FromRef.DisplayID,
		Description: pr.Description, Author: firstNonEmpty(pr.Author.User.Slug, pr.Author.User.Name),
		Draft: pr.Draft, HeadSHA: pr.FromRef.LatestCommit, Review: scm.ReviewNone, Checks: scm.ChecksNone,
		OccurredAt: at,
	}
	ev.Fork = true
	if f := pr.FromRef.Repository; f != nil && f.ID > 0 {
		ev.SourceRepoKey = strconv.FormatInt(f.ID, 10)
		ev.Fork = f.ID != base.ID
	}
	switch key {
	case EventDCOpened:
		ev.Action = scm.ActionOpened
	case EventDCFromRef, EventDCModified:
		ev.Action = scm.ActionUpdated
	case EventDCMerged:
		ev.Action = scm.ActionMerged
	case EventDCDeclined:
		ev.Action = scm.ActionClosed
	case EventDCApproved:
		ev.Kind, ev.Action, ev.Review = scm.KindReview, scm.ActionApproved, scm.ReviewApproved
	case EventDCUnapproved:
		ev.Kind, ev.Action = scm.KindReview, scm.ActionUnapproved
	case EventDCNeedsWork:
		ev.Kind, ev.Action, ev.Review = scm.KindReview, scm.ActionChangesRequested, scm.ReviewChangesRequested
	}
	return ev, nil
}
