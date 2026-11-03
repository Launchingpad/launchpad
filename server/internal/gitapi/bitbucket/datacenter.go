package bitbucket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"devops/internal/gitapi"
	"devops/internal/scm"
)

// jsonUnmarshal is json.Unmarshal with our error.
func jsonUnmarshal(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return &gitapi.Error{Code: gitapi.CodeError}
	}
	return nil
}

// The Data Center repo id is "KEY/slug#id": the paths take the key and slug,
// the number is the stable part. Personal projects have keys like ~USER.
var dcIDRE = regexp.MustCompile(`^(~?[A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})#([0-9]{1,18})$`)

func parseDCID(id string) (key, slug, num string, ok bool) {
	m := dcIDRE.FindStringSubmatch(id)
	if m == nil {
		return "", "", "", false
	}
	if !gitapi.SafeSegment(m[1]) || !gitapi.SafeSegment(m[2]) {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

func dcBase(key, slug string) string {
	return "/projects/" + url.PathEscape(key) + "/repos/" + url.PathEscape(slug)
}

type dcRepoJSON struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Project struct {
		Key string `json:"key"`
	} `json:"project"`
	Links struct {
		Self []struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"links"`
}

func (r dcRepoJSON) ref() (scm.RepoRef, bool) {
	if r.ID < 1 || r.Slug == "" || r.Project.Key == "" {
		return scm.RepoRef{}, false
	}
	num := strconv.FormatInt(r.ID, 10)
	id := r.Project.Key + "/" + r.Slug + "#" + num
	if _, _, _, ok := parseDCID(id); !ok {
		return scm.RepoRef{}, false
	}
	href := ""
	if len(r.Links.Self) > 0 {
		href = r.Links.Self[0].Href
	}
	return scm.RepoRef{ID: id, Key: num, Path: r.Project.Key + "/" + r.Slug, URL: href}, true
}

// dcCheck lists one project: a token that sees none can do nothing for us.
// Data Center has no "who am I" call on the REST root, so the identity is empty.
func (c *Client) dcCheck(ctx context.Context) (scm.Identity, error) {
	resp, err := c.c.Do(ctx, http.MethodGet, "/projects", url.Values{"limit": {"1"}}, nil)
	if err != nil {
		return scm.Identity{}, err
	}
	lp, err := c.page(resp)
	if err != nil {
		return scm.Identity{}, err
	}
	if len(lp.Values) == 0 {
		return scm.Identity{}, &gitapi.Error{Code: gitapi.CodeForbidden}
	}
	return scm.Identity{}, nil
}

func (c *Client) dcRepos(ctx context.Context, query string, page int, param string) (scm.RepoPage, error) {
	q := url.Values{"permission": {"REPO_ADMIN"}, "limit": {"100"}}
	if s := cleanQuery(query); s != "" {
		q.Set("name", s)
	}
	if param != "" {
		q.Set("start", param)
	}
	resp, err := c.c.Do(ctx, http.MethodGet, "/repos", q, nil)
	if err != nil {
		return scm.RepoPage{}, err
	}
	lp, err := c.page(resp)
	if err != nil {
		return scm.RepoPage{}, err
	}
	out := scm.RepoPage{}
	for _, raw := range lp.Values {
		var r dcRepoJSON
		if err := jsonUnmarshal(raw, &r); err != nil {
			return scm.RepoPage{}, err
		}
		if ref, ok := r.ref(); ok {
			out.Items = append(out.Items, ref)
		}
	}
	out.Next, err = c.nextCursor(lp, page)
	return out, err
}

// dcSlugByID lists a project's repositories (at most 5 pages) and returns the
// slug of the one with the numeric id, or "".
func (c *Client) dcSlugByID(ctx context.Context, key, num string) (string, error) {
	param := ""
	for p := 1; p <= gitapi.MaxPages; p++ {
		q := url.Values{"limit": {"100"}}
		if param != "" {
			q.Set("start", param)
		}
		resp, err := c.c.Do(ctx, http.MethodGet, "/projects/"+url.PathEscape(key)+"/repos", q, nil)
		if err != nil {
			return "", err
		}
		lp, err := c.page(resp)
		if err != nil {
			return "", err
		}
		for _, raw := range lp.Values {
			var r dcRepoJSON
			if json.Unmarshal(raw, &r) == nil && strconv.FormatInt(r.ID, 10) == num && r.Slug != "" {
				return r.Slug, nil
			}
		}
		var more bool
		if param, more, err = c.nextParam(lp); err != nil || !more {
			return "", err
		}
	}
	return "", nil
}

// dcRepo reads a repository. A 404 on the stored slug triggers the rename
// re-lookup by numeric id; the returned ref carries the current slug.
func (c *Client) dcRepo(ctx context.Context, id string) (scm.RepoRef, error) {
	key, slug, num, ok := parseDCID(id)
	if !ok {
		return scm.RepoRef{}, gitapi.ErrBadRepo
	}
	resp, err := c.c.Do(ctx, http.MethodGet, dcBase(key, slug), nil, nil)
	if gitapi.Is404(err) {
		now, lerr := c.dcSlugByID(ctx, key, num)
		if lerr != nil || now == "" {
			return scm.RepoRef{}, err
		}
		resp, err = c.c.Do(ctx, http.MethodGet, dcBase(key, now), nil, nil)
	}
	if err != nil {
		return scm.RepoRef{}, err
	}
	var r dcRepoJSON
	if err := resp.JSON(&r); err != nil {
		return scm.RepoRef{}, err
	}
	ref, ok := r.ref()
	if !ok || ref.Key != num {
		return scm.RepoRef{}, &gitapi.Error{Code: gitapi.CodeNotFound}
	}
	return ref, nil
}

type dcPRRef struct {
	DisplayID    string `json:"displayId"`
	LatestCommit string `json:"latestCommit"`
	Repository   *struct {
		ID int64 `json:"id"`
	} `json:"repository"`
}

type dcPR struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Draft       bool   `json:"draft"`
	UpdatedDate int64  `json:"updatedDate"` // milliseconds since the epoch
	// Reviewers carry each reviewer's status (LAUN-96). Nil when the host left
	// the field out: the review is then not known.
	Reviewers *[]struct {
		Approved bool   `json:"approved"`
		Status   string `json:"status"`
	} `json:"reviewers"`
	FromRef dcPRRef `json:"fromRef"`
	ToRef   dcPRRef `json:"toRef"`
	Author  struct {
		User struct {
			Slug string `json:"slug"`
		} `json:"user"`
	} `json:"author"`
	Links struct {
		Self []struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"links"`
}

func (p dcPR) change() scm.Change {
	state := "closed" // DECLINED
	switch p.State {
	case "OPEN":
		state = "opened"
	case "MERGED":
		state = "merged"
	}
	fork := p.FromRef.Repository == nil || p.ToRef.Repository == nil || p.FromRef.Repository.ID != p.ToRef.Repository.ID
	href := ""
	if len(p.Links.Self) > 0 {
		href = p.Links.Self[0].Href
	}
	var updated time.Time
	if p.UpdatedDate > 0 {
		updated = time.UnixMilli(p.UpdatedDate).UTC()
	}
	ch := scm.Change{
		Number: p.ID, URL: href, Title: p.Title, SourceBranch: p.FromRef.DisplayID,
		Description: p.Description, Author: p.Author.User.Slug, Draft: p.Draft, Fork: fork,
		HeadSHA: p.FromRef.LatestCommit, State: state, Review: scm.ReviewNone, Checks: scm.ChecksNone,
		UpdatedAt: updated,
	}
	if p.Reviewers != nil {
		ch.ReviewKnown = true
		for _, r := range *p.Reviewers {
			switch {
			case strings.EqualFold(r.Status, "NEEDS_WORK"):
				ch.Review = scm.ReviewChangesRequested
			case (r.Approved || strings.EqualFold(r.Status, "APPROVED")) && ch.Review == scm.ReviewNone:
				ch.Review = scm.ReviewApproved
			}
		}
	}
	return ch
}
