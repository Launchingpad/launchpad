// Package gitlab is the GitLab API client (T17 #32, scm.APIClient). All HTTP
// goes through gitapi.Core. Projects are addressed by numeric id, and a page's
// follow-up is the X-Next-Page number: no URL a host sends is ever requested.
package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"devops/internal/gitapi"
	"devops/internal/scm"
)

// Client implements scm.APIClient for GitLab (gitlab.com and self-managed).
type Client struct{ c *gitapi.Core }

var _ scm.APIClient = (*Client)(nil)

// New builds the client of cfg.BaseURL.
func New(cfg scm.ClientConfig, o gitapi.Options) (*Client, error) {
	root, err := gitapi.APIRoot("gitlab", cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return NewAt(root, cfg.Token, o)
}

// NewAt builds a client on an explicit API root (the admin's override, and tests).
func NewAt(root, token string, o gitapi.Options) (*Client, error) {
	c, err := gitapi.NewCore(root, token, o)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

func projectID(r scm.RepoRef) (string, error) {
	id := r.ID
	if id == "" {
		id = r.Key
	}
	if !gitapi.IsNumeric(id) {
		return "", gitapi.ErrBadRepo
	}
	return id, nil
}

// next builds the cursor of the following page from X-Next-Page.
func next(resp *gitapi.Response, page int) string {
	return gitapi.EncodeCursor(page+1, strings.TrimSpace(resp.Header.Get("X-Next-Page")))
}

// CheckToken reads /user, then the token's own scopes. A token without the
// `api` scope cannot add a project hook (read_api cannot) and is refused with
// provider_forbidden. A host that has no token-self route (404) passes on /user.
func (c *Client) CheckToken(ctx context.Context) (scm.Identity, error) {
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	resp, err := c.c.Do(ctx, http.MethodGet, "/user", nil, nil)
	if err != nil {
		return scm.Identity{}, err
	}
	var u struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := resp.JSON(&u); err != nil {
		return scm.Identity{}, err
	}
	self, err := c.c.Do(ctx, http.MethodGet, "/personal_access_tokens/self", nil, nil)
	switch {
	case err == nil:
		var t struct {
			Scopes []string `json:"scopes"`
		}
		if err := self.JSON(&t); err != nil {
			return scm.Identity{}, err
		}
		has := false
		for _, s := range t.Scopes {
			if s == "api" {
				has = true
			}
		}
		if !has {
			return scm.Identity{}, &gitapi.Error{Code: gitapi.CodeForbidden}
		}
	case gitapi.Is404(err):
	default:
		return scm.Identity{}, err
	}
	return scm.Identity{Login: u.Username, Name: u.Name}, nil
}

type projectJSON struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
}

func (p projectJSON) ref() scm.RepoRef {
	id := strconv.FormatInt(p.ID, 10)
	return scm.RepoRef{ID: id, Key: id, Path: p.PathWithNamespace, URL: p.WebURL}
}

// ListRepos lists the projects the token is a Maintainer or better on (a hook
// needs it), 50 a page, filtered by query. Which token scope this list needs
// for a project access token is "verify at build" (T17 #15).
func (c *Client) ListRepos(ctx context.Context, query, cursor string) (scm.RepoPage, error) {
	page, param, err := gitapi.PageNumber(cursor)
	if err != nil {
		return scm.RepoPage{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	q := url.Values{"membership": {"true"}, "min_access_level": {"40"}, "simple": {"true"},
		"order_by": {"path"}, "sort": {"asc"}, "per_page": {"50"}}
	if s := strings.TrimSpace(query); s != "" {
		q.Set("search", s)
	}
	if param != "" {
		q.Set("page", param)
	}
	resp, err := c.c.Do(ctx, http.MethodGet, "/projects", q, nil)
	if err != nil {
		return scm.RepoPage{}, err
	}
	var ps []projectJSON
	if err := resp.JSON(&ps); err != nil {
		return scm.RepoPage{}, err
	}
	out := scm.RepoPage{Next: next(resp, page)}
	for _, p := range ps {
		out.Items = append(out.Items, p.ref())
	}
	return out, nil
}

// GetRepo reads a project by the id ListRepos made.
func (c *Client) GetRepo(ctx context.Context, id string) (scm.RepoRef, error) {
	if !gitapi.IsNumeric(id) {
		return scm.RepoRef{}, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	resp, err := c.c.Do(ctx, http.MethodGet, "/projects/"+id, nil, nil)
	if err != nil {
		return scm.RepoRef{}, err
	}
	var p projectJSON
	if err := resp.JSON(&p); err != nil {
		return scm.RepoRef{}, err
	}
	return p.ref(), nil
}

type hookJSON struct {
	ID  int64  `json:"id"`
	URL string `json:"url"`
}

func (c *Client) findHook(ctx context.Context, pid, hookURL string) (string, error) {
	page := 1
	for ; page <= gitapi.MaxPages; page++ {
		resp, err := c.c.Do(ctx, http.MethodGet, "/projects/"+pid+"/hooks",
			url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, nil)
		if err != nil {
			return "", err
		}
		var hs []hookJSON
		if err := resp.JSON(&hs); err != nil {
			return "", err
		}
		for _, h := range hs {
			if h.URL == hookURL {
				return strconv.FormatInt(h.ID, 10), nil
			}
		}
		if !gitapi.ValidParam(strings.TrimSpace(resp.Header.Get("X-Next-Page"))) {
			break
		}
	}
	return "", nil
}

func hookBody(spec scm.HookSpec) map[string]any {
	return map[string]any{
		"url": spec.URL, "token": spec.Secret,
		"merge_requests_events": true, "push_events": false,
		"pipeline_events":         spec.Checks, // LAUN-85: the checks of a head sha
		"enable_ssl_verification": true,
	}
}

// EnsureHook makes the project hook, or updates the one with the same URL
// (list and match). After a failed create it lists again before it gives up.
func (c *Client) EnsureHook(ctx context.Context, repo scm.RepoRef, spec scm.HookSpec) (scm.HookRef, error) {
	pid, err := projectID(repo)
	if err != nil {
		return scm.HookRef{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	id, err := c.findHook(ctx, pid, spec.URL)
	if err != nil {
		return scm.HookRef{}, err
	}
	if id != "" {
		if _, err := c.c.Do(ctx, http.MethodPut, "/projects/"+pid+"/hooks/"+id, nil, hookBody(spec)); err != nil {
			return scm.HookRef{}, err
		}
		return scm.HookRef{ID: id}, nil
	}
	resp, err := c.c.Do(ctx, http.MethodPost, "/projects/"+pid+"/hooks", nil, hookBody(spec))
	if err == nil {
		var h hookJSON
		if jerr := resp.JSON(&h); jerr == nil && h.ID != 0 {
			return scm.HookRef{ID: strconv.FormatInt(h.ID, 10)}, nil
		}
		err = &gitapi.Error{Code: gitapi.CodeError}
	}
	if again, ferr := c.findHook(ctx, pid, spec.URL); ferr == nil && again != "" {
		return scm.HookRef{ID: again}, nil
	}
	return scm.HookRef{}, err
}

// DeleteHook removes a hook; one that is already gone is not an error.
func (c *Client) DeleteHook(ctx context.Context, repo scm.RepoRef, hook scm.HookRef) error {
	pid, err := projectID(repo)
	if err != nil || !gitapi.IsNumeric(hook.ID) {
		return gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	_, err = c.c.Do(ctx, http.MethodDelete, "/projects/"+pid+"/hooks/"+hook.ID, nil, nil)
	if gitapi.Is404(err) {
		return nil
	}
	return err
}

type mrJSON struct {
	IID          int64     `json:"iid"`
	WebURL       string    `json:"web_url"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	SourceBranch string    `json:"source_branch"`
	SHA          string    `json:"sha"`
	State        string    `json:"state"`
	Draft        bool      `json:"draft"`
	WIP          bool      `json:"work_in_progress"`
	SourceID     int64     `json:"source_project_id"`
	TargetID     int64     `json:"target_project_id"`
	UpdatedAt    time.Time `json:"updated_at"`
	Author       struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (m mrJSON) change() scm.Change {
	state := "opened" // opened, and locked (a merge in progress)
	switch m.State {
	case "merged":
		state = "merged"
	case "closed":
		state = "closed"
	}
	return scm.Change{
		Number: m.IID, URL: m.WebURL, Title: m.Title, SourceBranch: m.SourceBranch,
		Description: m.Description, Author: m.Author.Username, Draft: m.Draft || m.WIP,
		Fork: m.SourceID != m.TargetID, HeadSHA: m.SHA, State: state,
		Review: scm.ReviewNone, Checks: scm.ChecksNone, UpdatedAt: m.UpdatedAt,
	}
}

// GetChange reads one merge request. Its review and checks state are not read
// here (T17 tickets 11 and 12).
func (c *Client) GetChange(ctx context.Context, repo scm.RepoRef, number int64) (scm.Change, error) {
	pid, err := projectID(repo)
	if err != nil || number < 1 {
		return scm.Change{}, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	resp, err := c.c.Do(ctx, http.MethodGet, "/projects/"+pid+"/merge_requests/"+strconv.FormatInt(number, 10), nil, nil)
	if err != nil {
		return scm.Change{}, err
	}
	var m mrJSON
	if err := resp.JSON(&m); err != nil {
		return scm.Change{}, err
	}
	return m.change(), nil
}

// ListOpenChanges lists open merge requests, newest first, 100 a page.
func (c *Client) ListOpenChanges(ctx context.Context, repo scm.RepoRef, cursor string) (scm.ChangePage, error) {
	pid, err := projectID(repo)
	if err != nil {
		return scm.ChangePage{}, err
	}
	page, param, err := gitapi.PageNumber(cursor)
	if err != nil {
		return scm.ChangePage{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	q := url.Values{"state": {"opened"}, "order_by": {"created_at"}, "sort": {"desc"}, "per_page": {"100"}}
	if param != "" {
		q.Set("page", param)
	}
	resp, err := c.c.Do(ctx, http.MethodGet, "/projects/"+pid+"/merge_requests", q, nil)
	if err != nil {
		return scm.ChangePage{}, err
	}
	var ms []mrJSON
	if err := resp.JSON(&ms); err != nil {
		return scm.ChangePage{}, err
	}
	out := scm.ChangePage{Next: next(resp, page)}
	for _, m := range ms {
		out.Items = append(out.Items, m.change())
	}
	return out, nil
}

var _ scm.ReviewReader = (*Client)(nil)

// ReadReview reads the approvals of one merge request (LAUN-96, for the
// backfill): approved when the answer's `approved` flag is true, which is what
// the "approved" webhook action means (the whole MR fully approved by all
// required approvers). An answer without the flag falls back to "someone
// approved" (approved_by not empty). GitLab's request-changes state is not in
// this answer, so it never returns changes_requested.
func (c *Client) ReadReview(ctx context.Context, repo scm.RepoRef, number int64) (scm.Review, error) {
	pid, err := projectID(repo)
	if err != nil || number < 1 {
		return scm.ReviewNone, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	resp, err := c.c.Do(ctx, http.MethodGet, "/projects/"+pid+"/merge_requests/"+strconv.FormatInt(number, 10)+"/approvals", nil, nil)
	if err != nil {
		return scm.ReviewNone, err
	}
	var a struct {
		Approved   *bool             `json:"approved"`
		ApprovedBy []json.RawMessage `json:"approved_by"`
	}
	if err := resp.JSON(&a); err != nil {
		return scm.ReviewNone, err
	}
	if a.Approved != nil {
		if *a.Approved {
			return scm.ReviewApproved, nil
		}
		return scm.ReviewNone, nil
	}
	if len(a.ApprovedBy) > 0 {
		return scm.ReviewApproved, nil
	}
	return scm.ReviewNone, nil
}
