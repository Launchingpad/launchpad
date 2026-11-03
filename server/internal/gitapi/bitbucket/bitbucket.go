// Package bitbucket is the Bitbucket API client (T17 #32, scm.APIClient) for
// Bitbucket Cloud (API 2.0) and Data Center (REST 1.0). Auth is a Bearer token
// only (repository access token, or Data Center HTTP access token); app
// passwords are not supported in v1. All HTTP goes through gitapi.Core, and no
// next link a host sends is ever requested.
package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"devops/internal/gitapi"
	"devops/internal/scm"
)

// Client implements scm.APIClient. The flavour follows the API root: a root
// that ends in /2.0 is Cloud, one that ends in /rest/api/1.0 is Data Center.
type Client struct {
	c     *gitapi.Core
	cloud bool
}

var _ scm.APIClient = (*Client)(nil)

// New builds the client of cfg.BaseURL (bitbucket.org or a Data Center host).
func New(cfg scm.ClientConfig, o gitapi.Options) (*Client, error) {
	root, err := gitapi.APIRoot("bitbucket", cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return NewAt(root, cfg.Token, o)
}

// NewAt builds a client on an explicit API root (the admin's override, and tests).
func NewAt(root, token string, o gitapi.Options) (*Client, error) {
	r := strings.TrimRight(root, "/")
	var cloud bool
	switch {
	case strings.HasSuffix(r, "/2.0"):
		cloud = true
	case strings.HasSuffix(r, "/rest/api/1.0"):
	default:
		return nil, errors.New("gitapi: a Bitbucket API root ends in /2.0 (Cloud) or /rest/api/1.0 (Data Center)")
	}
	c, err := gitapi.NewCore(r, token, o)
	if err != nil {
		return nil, err
	}
	return &Client{c: c, cloud: cloud}, nil
}

// CheckToken verifies the token (see the two flavours).
func (c *Client) CheckToken(ctx context.Context) (scm.Identity, error) {
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	if c.cloud {
		return c.cloudCheck(ctx)
	}
	return c.dcCheck(ctx)
}

// ListRepos lists the repositories the token administers, filtered by query.
func (c *Client) ListRepos(ctx context.Context, query, cursor string) (scm.RepoPage, error) {
	page, param, err := gitapi.DecodeCursor(cursor)
	if err != nil {
		return scm.RepoPage{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	if c.cloud {
		return c.cloudRepos(ctx, query, page, param)
	}
	return c.dcRepos(ctx, query, page, param)
}

// GetRepo reads a repository by the id ListRepos made.
func (c *Client) GetRepo(ctx context.Context, id string) (scm.RepoRef, error) {
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	if c.cloud {
		return c.cloudRepo(ctx, id)
	}
	return c.dcRepo(ctx, id)
}

// EnsureHook makes the hook, or updates the one with the same URL (list and
// match). After a failed create it lists again before it gives up.
func (c *Client) EnsureHook(ctx context.Context, repo scm.RepoRef, spec scm.HookSpec) (scm.HookRef, error) {
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	call := c.callFor(repo)
	if call == nil {
		return scm.HookRef{}, gitapi.ErrBadRepo
	}
	id, err := c.findHook(ctx, call, spec.URL)
	if err != nil {
		return scm.HookRef{}, err
	}
	checks := spec.Checks
	if id != "" {
		_, err := call(ctx, "PUT", "/"+c.hooksSeg()+"/"+id, nil, c.hookBody(spec, checks))
		if err != nil && c.dropChecks(spec, checks, err) {
			checks = false
			_, err = call(ctx, "PUT", "/"+c.hooksSeg()+"/"+id, nil, c.hookBody(spec, checks))
		}
		if err != nil {
			return scm.HookRef{}, err
		}
		return scm.HookRef{ID: id}, nil
	}
	resp, err := call(ctx, "POST", "/"+c.hooksSeg(), nil, c.hookBody(spec, checks))
	if err != nil && c.dropChecks(spec, checks, err) {
		checks = false
		resp, err = call(ctx, "POST", "/"+c.hooksSeg(), nil, c.hookBody(spec, checks))
	}
	if err == nil {
		var h struct {
			UUID string      `json:"uuid"`
			ID   json.Number `json:"id"`
		}
		if jerr := resp.JSON(&h); jerr == nil {
			if hid := c.hookIDOf(h.UUID, h.ID.String()); hid != "" {
				return scm.HookRef{ID: hid}, nil
			}
		}
		err = &gitapi.Error{Code: gitapi.CodeError}
	}
	if again, ferr := c.findHook(ctx, call, spec.URL); ferr == nil && again != "" {
		return scm.HookRef{ID: again}, nil
	}
	return scm.HookRef{}, err
}

// dropChecks reports whether a failed hook write should be tried again without
// the check events: Data Center refused the request with a 400 and the request
// carried the (UNVERIFIED) build status event. Cloud never retries: its event
// names are documented.
func (c *Client) dropChecks(spec scm.HookSpec, sent bool, err error) bool {
	var ge *gitapi.Error
	return !c.cloud && spec.Checks && sent && errors.As(err, &ge) && ge.Status == http.StatusBadRequest
}

// DeleteHook removes a hook; one that is already gone is not an error.
func (c *Client) DeleteHook(ctx context.Context, repo scm.RepoRef, hook scm.HookRef) error {
	call := c.callFor(repo)
	seg, ok := c.hookSeg(hook.ID)
	if call == nil || !ok {
		return gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	_, err := call(ctx, "DELETE", "/"+c.hooksSeg()+"/"+seg, nil, nil)
	if gitapi.Is404(err) {
		return nil
	}
	return err
}

// GetChange reads one pull request. Its review and checks state are not read
// here (T17 tickets 11 and 12).
func (c *Client) GetChange(ctx context.Context, repo scm.RepoRef, number int64) (scm.Change, error) {
	call := c.callFor(repo)
	if call == nil || number < 1 {
		return scm.Change{}, gitapi.ErrBadRepo
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	return c.getChange(ctx, call, number)
}

// ListOpenChanges lists open pull requests, newest first.
func (c *Client) ListOpenChanges(ctx context.Context, repo scm.RepoRef, cursor string) (scm.ChangePage, error) {
	call := c.callFor(repo)
	if call == nil {
		return scm.ChangePage{}, gitapi.ErrBadRepo
	}
	page, param, err := gitapi.DecodeCursor(cursor)
	if err != nil {
		return scm.ChangePage{}, err
	}
	ctx, cancel := gitapi.WithRouteTimeout(ctx)
	defer cancel()
	return c.openChanges(ctx, call, page, param)
}
