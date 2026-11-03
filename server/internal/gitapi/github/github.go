// Package github is the GitHub API client (T17 #32, scm.APIClient). All HTTP
// goes through gitapi.Core. Repositories are addressed BY ID
// (/repositories/{id}); a renamed repo answers 301 on owner/name.
package github

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"devops/internal/gitapi"
	"devops/internal/scm"
)

// ChangeEvents are the events a hook made now subscribes to (T17 #24).
var ChangeEvents = []string{"pull_request"}

// ReviewEvents are added when the spec asks for reviews (LAUN-83, T17 #24).
var ReviewEvents = []string{"pull_request_review"}

// CheckEvents are added when the spec asks for checks (LAUN-85, T17 #24).
var CheckEvents = []string{"check_suite", "status"}

// HookEvents is the event set of a hook made for spec.
func HookEvents(spec scm.HookSpec) []string {
	out := append([]string(nil), ChangeEvents...)
	if spec.Reviews {
		out = append(out, ReviewEvents...)
	}
	if spec.Checks {
		out = append(out, CheckEvents...)
	}
	return out
}

// Client implements scm.APIClient for GitHub and GitHub Enterprise.
type Client struct{ c *gitapi.Core }

var _ scm.APIClient = (*Client)(nil)

// New builds the client of cfg.BaseURL (github.com or an Enterprise host).
func New(cfg scm.ClientConfig, o gitapi.Options) (*Client, error) {
	root, err := gitapi.APIRoot("github", cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return NewAt(root, cfg.Token, o)
}

// NewAt builds a client on an explicit API root (the admin's override, and tests).
func NewAt(root, token string, o gitapi.Options) (*Client, error) {
	h := http.Header{}
	for k, v := range o.Headers {
		h[k] = v
	}
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	o.Headers = h
	c, err := gitapi.NewCore(root, token, o)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

func repoID(r scm.RepoRef) (string, error) {
	id := r.ID
	if id == "" {
		id = r.Key
	}
	if !gitapi.IsNumeric(id) {
		return "", gitapi.ErrBadRepo
	}
	return id, nil
}

// CheckToken reads /user. A classic token whose scope list is empty can do
// nothing for us and is refused (provider_forbidden); a fine-grained token
// sends no scope header and passes.
func (c *Client) CheckToken(ctx context.Context) (scm.Identity, error) {
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	resp, err := c.c.Do(ctx, http.MethodGet, "/user", nil, nil)
	if err != nil {
		return scm.Identity{}, err
	}
	var u struct {
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := resp.JSON(&u); err != nil {
		return scm.Identity{}, err
	}
	if vals := resp.Header.Values("X-OAuth-Scopes"); len(vals) > 0 && strings.TrimSpace(strings.Join(vals, "")) == "" {
		return scm.Identity{}, &gitapi.Error{Code: gitapi.CodeForbidden}
	}
	return scm.Identity{Login: u.Login, Name: u.Name}, nil
}

type repoJSON struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

func (r repoJSON) ref() scm.RepoRef {
	id := strconv.FormatInt(r.ID, 10)
	return scm.RepoRef{ID: id, Key: id, Path: r.FullName, URL: r.HTMLURL}
}

// ListRepos lists the repositories the token reaches, 50 a page. GitHub has
// no name filter on this list, so query filters the page it returns (a page
// can come back short or empty while Next is set).
func (c *Client) ListRepos(ctx context.Context, query, cursor string) (scm.RepoPage, error) {
	page, param, err := gitapi.PageNumber(cursor)
	if err != nil {
		return scm.RepoPage{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	q := url.Values{"per_page": {"50"}, "sort": {"full_name"}, "direction": {"asc"},
		"affiliation": {"owner,collaborator,organization_member"}}
	if param != "" {
		q.Set("page", param)
	}
	resp, err := c.c.Do(ctx, http.MethodGet, "/user/repos", q, nil)
	if err != nil {
		return scm.RepoPage{}, err
	}
	var rs []repoJSON
	if err := resp.JSON(&rs); err != nil {
		return scm.RepoPage{}, err
	}
	out := scm.RepoPage{}
	needle := strings.ToLower(strings.TrimSpace(query))
	for _, r := range rs {
		if needle == "" || strings.Contains(strings.ToLower(r.FullName), needle) {
			out.Items = append(out.Items, r.ref())
		}
	}
	out.Next, err = c.next(resp, page)
	return out, err
}

// next builds the cursor of the following page from the Link header, or "".
func (c *Client) next(resp *gitapi.Response, page int) (string, error) {
	p, ok, err := c.c.NextParam(gitapi.NextLink(resp.Header), "page")
	if err != nil || !ok {
		return "", err
	}
	return gitapi.EncodeCursor(page+1, p), nil
}

// GetRepo reads a repository by the id ListRepos made.
func (c *Client) GetRepo(ctx context.Context, id string) (scm.RepoRef, error) {
	if !gitapi.IsNumeric(id) {
		return scm.RepoRef{}, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	return c.getRepo(ctx, id)
}

func (c *Client) getRepo(ctx context.Context, id string) (scm.RepoRef, error) {
	resp, err := c.c.Do(ctx, http.MethodGet, "/repositories/"+id, nil, nil)
	if err != nil {
		return scm.RepoRef{}, err
	}
	var r repoJSON
	if err := resp.JSON(&r); err != nil {
		return scm.RepoRef{}, err
	}
	return r.ref(), nil
}

var fullNameRE = regexp.MustCompile(`^([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})$`)

// repoCall calls a path under the repo. It tries /repositories/{id}{suffix}
// first; if that is a 404 it looks the full name up by id and tries
// /repos/{owner}/{name}{suffix} (decisions-T17 #15: whether the id route
// exists for every call is "verify at build"; the fallback is the rebuilt
// owner/name path). A 404 on the second path is the answer.
func (c *Client) repoCall(ctx context.Context, repo scm.RepoRef, method, suffix string, q url.Values, body any) (*gitapi.Response, error) {
	id, err := repoID(repo)
	if err != nil {
		return nil, err
	}
	resp, err := c.c.Do(ctx, method, "/repositories/"+id+suffix, q, body)
	if err == nil || !gitapi.Is404(err) {
		return resp, err
	}
	r, rerr := c.getRepo(ctx, id)
	if rerr != nil {
		return nil, rerr
	}
	m := fullNameRE.FindStringSubmatch(r.Path)
	if m == nil || !gitapi.SafeSegment(m[1]) || !gitapi.SafeSegment(m[2]) {
		return nil, &gitapi.Error{Code: gitapi.CodeError}
	}
	return c.c.Do(ctx, method, "/repos/"+url.PathEscape(m[1])+"/"+url.PathEscape(m[2])+suffix, q, body)
}

type hookJSON struct {
	ID     int64 `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// findHook lists the repo's hooks (at most 5 pages) and returns the id of the
// one whose URL is hookURL, or "".
func (c *Client) findHook(ctx context.Context, repo scm.RepoRef, hookURL string) (string, error) {
	for page := 1; page <= gitapi.MaxPages; page++ {
		resp, err := c.repoCall(ctx, repo, http.MethodGet, "/hooks", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}, nil)
		if err != nil {
			return "", err
		}
		var hs []hookJSON
		if err := resp.JSON(&hs); err != nil {
			return "", err
		}
		for _, h := range hs {
			if h.Config.URL == hookURL {
				return strconv.FormatInt(h.ID, 10), nil
			}
		}
		if _, ok, err := c.c.NextParam(gitapi.NextLink(resp.Header), "page"); err != nil || !ok {
			return "", err
		}
	}
	return "", nil
}

func hookBody(spec scm.HookSpec, create bool) map[string]any {
	b := map[string]any{
		"active": true,
		"events": HookEvents(spec),
		"config": map[string]any{
			"url": spec.URL, "content_type": "json", "secret": spec.Secret, "insecure_ssl": "0",
		},
	}
	if create {
		b["name"] = "web"
	}
	return b
}

// EnsureHook makes the hook, or updates the one with the same URL (list and
// match). After a failed create it lists again before it gives up, so a lost
// answer does not leave a second hook.
func (c *Client) EnsureHook(ctx context.Context, repo scm.RepoRef, spec scm.HookSpec) (scm.HookRef, error) {
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	id, err := c.findHook(ctx, repo, spec.URL)
	if err != nil {
		return scm.HookRef{}, err
	}
	if id != "" {
		if _, err := c.repoCall(ctx, repo, http.MethodPatch, "/hooks/"+id, nil, hookBody(spec, false)); err != nil {
			return scm.HookRef{}, err
		}
		return scm.HookRef{ID: id}, nil
	}
	resp, err := c.repoCall(ctx, repo, http.MethodPost, "/hooks", nil, hookBody(spec, true))
	if err != nil {
		if again, ferr := c.findHook(ctx, repo, spec.URL); ferr == nil && again != "" {
			return scm.HookRef{ID: again}, nil
		}
		return scm.HookRef{}, err
	}
	var h hookJSON
	if err := resp.JSON(&h); err != nil || h.ID == 0 {
		if again, ferr := c.findHook(ctx, repo, spec.URL); ferr == nil && again != "" {
			return scm.HookRef{ID: again}, nil
		}
		return scm.HookRef{}, &gitapi.Error{Code: gitapi.CodeError}
	}
	return scm.HookRef{ID: strconv.FormatInt(h.ID, 10)}, nil
}

// DeleteHook removes a hook; one that is already gone is not an error.
func (c *Client) DeleteHook(ctx context.Context, repo scm.RepoRef, hook scm.HookRef) error {
	if !gitapi.IsNumeric(hook.ID) {
		return gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	_, err := c.repoCall(ctx, repo, http.MethodDelete, "/hooks/"+hook.ID, nil, nil)
	if gitapi.Is404(err) {
		return nil
	}
	return err
}

type prJSON struct {
	Number    int64     `json:"number"`
	HTMLURL   string    `json:"html_url"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	MergedAt  *string   `json:"merged_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo *struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

func (p prJSON) change() scm.Change {
	state := "closed"
	switch {
	case p.Merged || p.MergedAt != nil:
		state = "merged"
	case p.State == "open":
		state = "opened"
	}
	fork := p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.ID != p.Base.Repo.ID
	return scm.Change{
		Number: p.Number, URL: p.HTMLURL, Title: p.Title, SourceBranch: p.Head.Ref,
		Description: p.Body, Author: p.User.Login, Draft: p.Draft, Fork: fork,
		HeadSHA: p.Head.SHA, State: state, Review: scm.ReviewNone, Checks: scm.ChecksNone,
		UpdatedAt: p.UpdatedAt,
	}
}

// GetChange reads one pull request. Its review and checks state are not read
// here (T17 tickets 11 and 12).
func (c *Client) GetChange(ctx context.Context, repo scm.RepoRef, number int64) (scm.Change, error) {
	if number < 1 {
		return scm.Change{}, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	resp, err := c.repoCall(ctx, repo, http.MethodGet, "/pulls/"+strconv.FormatInt(number, 10), nil, nil)
	if err != nil {
		return scm.Change{}, err
	}
	var p prJSON
	if err := resp.JSON(&p); err != nil {
		return scm.Change{}, err
	}
	return p.change(), nil
}

// ListOpenChanges lists open pull requests, newest first, 100 a page.
func (c *Client) ListOpenChanges(ctx context.Context, repo scm.RepoRef, cursor string) (scm.ChangePage, error) {
	page, param, err := gitapi.PageNumber(cursor)
	if err != nil {
		return scm.ChangePage{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	q := url.Values{"state": {"open"}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"100"}}
	if param != "" {
		q.Set("page", param)
	}
	resp, err := c.repoCall(ctx, repo, http.MethodGet, "/pulls", q, nil)
	if err != nil {
		return scm.ChangePage{}, err
	}
	var ps []prJSON
	if err := resp.JSON(&ps); err != nil {
		return scm.ChangePage{}, err
	}
	out := scm.ChangePage{}
	for _, p := range ps {
		out.Items = append(out.Items, p.change())
	}
	out.Next, err = c.next(resp, page)
	return out, err
}

var _ scm.ReviewReader = (*Client)(nil)

// ReadReview reads the review state of one pull request from its reviews
// (LAUN-96, for the backfill). Each reviewer counts by their LATEST approval
// or request for changes; a dismissed review removes that reviewer's entry; a
// plain comment changes nothing. Any outstanding request for changes wins over
// an approval. A list longer than the page limit is an error (the newest
// reviews would be missing), never a guess.
func (c *Client) ReadReview(ctx context.Context, repo scm.RepoRef, number int64) (scm.Review, error) {
	if number < 1 {
		return scm.ReviewNone, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	latest := map[string]string{} // reviewer login -> APPROVED | CHANGES_REQUESTED
	cursor := ""
	for i := 0; i < gitapi.MaxPages; i++ {
		page, param, err := gitapi.PageNumber(cursor)
		if err != nil {
			return scm.ReviewNone, err
		}
		q := url.Values{"per_page": {"100"}}
		if param != "" {
			q.Set("page", param)
		}
		resp, err := c.repoCall(ctx, repo, http.MethodGet, "/pulls/"+strconv.FormatInt(number, 10)+"/reviews", q, nil)
		if err != nil {
			return scm.ReviewNone, err
		}
		var rs []struct {
			State string `json:"state"`
			User  *struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := resp.JSON(&rs); err != nil {
			return scm.ReviewNone, err
		}
		for _, r := range rs {
			login := ""
			if r.User != nil {
				login = strings.ToLower(r.User.Login)
			}
			switch strings.ToUpper(r.State) {
			case "APPROVED", "CHANGES_REQUESTED":
				latest[login] = strings.ToUpper(r.State)
			case "DISMISSED":
				delete(latest, login)
			}
		}
		more := gitapi.NextLink(resp.Header) != ""
		if cursor, err = c.next(resp, page); err != nil {
			return scm.ReviewNone, err
		}
		if cursor == "" {
			if more { // the page limit: the newest reviews are not here
				return scm.ReviewNone, &gitapi.Error{Code: gitapi.CodeError}
			}
			break
		}
	}
	out := scm.ReviewNone
	for _, st := range latest {
		if st == "CHANGES_REQUESTED" {
			return scm.ReviewChangesRequested, nil
		}
		out = scm.ReviewApproved
	}
	return out, nil
}
