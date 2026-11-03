package bitbucket

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"devops/internal/gitapi"
	"devops/internal/scm"
)

// The events a hook subscribes to (T17 #24). The base set moves tickets; the
// review events come with HookSpec.Reviews (LAUN-83) and the check events with
// HookSpec.Checks (LAUN-85). The names are from the vendors' documentation;
// none was tried on a live host (decisions-T17, LAUN-84).
var (
	CloudEvents       = []string{"pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected"}
	CloudReviewEvents = []string{"pullrequest:approved", "pullrequest:unapproved"}
	CloudCheckEvents  = []string{"repo:commit_status_created", "repo:commit_status_updated"}

	DCEvents       = []string{"pr:opened", "pr:from_ref_updated", "pr:modified", "pr:merged", "pr:declined"}
	DCReviewEvents = []string{"pr:reviewer:approved", "pr:reviewer:unapproved", "pr:reviewer:needs_work"}
	// DCCheckEvents is Data Center's build status event. UNVERIFIED name: when
	// the host refuses the hook with a 400, EnsureHook tries again without it.
	DCCheckEvents = []string{"repo:build_status"}
)

// eventsFor is the event list of a hook for spec.
func (c *Client) eventsFor(spec scm.HookSpec, checks bool) []string {
	var ev []string
	if c.cloud {
		ev = append(ev, CloudEvents...)
		if spec.Reviews {
			ev = append(ev, CloudReviewEvents...)
		}
		if checks {
			ev = append(ev, CloudCheckEvents...)
		}
		return ev
	}
	ev = append(ev, DCEvents...)
	if spec.Reviews {
		ev = append(ev, DCReviewEvents...)
	}
	if checks {
		ev = append(ev, DCCheckEvents...)
	}
	return ev
}

// callFn calls a path under one repository. A Data Center call follows a
// renamed slug (T17 #15).
type callFn func(ctx context.Context, method, suffix string, q url.Values, body any) (*gitapi.Response, error)

func (c *Client) callFor(repo scm.RepoRef) callFn {
	if c.cloud {
		id := repo.ID
		if id == "" {
			id = repo.Key
		}
		base, ok := cloudBase(id)
		if !ok {
			return nil
		}
		return func(ctx context.Context, method, suffix string, q url.Values, body any) (*gitapi.Response, error) {
			return c.c.Do(ctx, method, base+suffix, q, body)
		}
	}
	key, slug, id, ok := parseDCID(repo.ID)
	if !ok {
		return nil
	}
	return func(ctx context.Context, method, suffix string, q url.Values, body any) (*gitapi.Response, error) {
		resp, err := c.c.Do(ctx, method, dcBase(key, slug)+suffix, q, body)
		if !gitapi.Is404(err) {
			return resp, err
		}
		// 404: the slug may have been renamed. Find the repo by its numeric id.
		now, lerr := c.dcSlugByID(ctx, key, id)
		if lerr != nil || now == "" || now == slug {
			return nil, err
		}
		return c.c.Do(ctx, method, dcBase(key, now)+suffix, q, body)
	}
}

func (c *Client) hooksSeg() string {
	if c.cloud {
		return "hooks"
	}
	return "webhooks"
}

// hookSeg validates a hook id and returns its path segment.
func (c *Client) hookSeg(id string) (string, bool) {
	if c.cloud {
		if !uuidRE.MatchString(id) {
			return "", false
		}
		return url.PathEscape(id), true
	}
	return id, gitapi.IsNumeric(id)
}

func (c *Client) hookIDOf(uuid, num string) string {
	if c.cloud {
		if uuidRE.MatchString(uuid) {
			return uuid
		}
		return ""
	}
	if gitapi.IsNumeric(num) {
		return num
	}
	return ""
}

func (c *Client) hookBody(spec scm.HookSpec, checks bool) map[string]any {
	events := c.eventsFor(spec, checks)
	if c.cloud {
		return map[string]any{
			"description": "Launchpad", "url": spec.URL, "active": true, "secret": spec.Secret,
			"skip_cert_verification": false, "events": events,
		}
	}
	return map[string]any{
		"name": "Launchpad", "url": spec.URL, "active": true, "events": events,
		"configuration": map[string]any{"secret": spec.Secret}, "sslVerificationRequired": true,
	}
}

func (c *Client) pageKey() string {
	if c.cloud {
		return "page"
	}
	return "start"
}

// listPage is the paging envelope of both flavours.
type listPage struct {
	Values        []json.RawMessage `json:"values"`
	Next          string            `json:"next"`          // Cloud: a URL the client never requests
	IsLastPage    bool              `json:"isLastPage"`    // Data Center
	NextPageStart *int              `json:"nextPageStart"` // Data Center
}

func (c *Client) page(resp *gitapi.Response) (listPage, error) {
	var lp listPage
	err := resp.JSON(&lp)
	return lp, err
}

// nextParam returns the provider page parameter of the following page, or
// more=false at the end. A Cloud next link must have the API root's origin.
func (c *Client) nextParam(lp listPage) (param string, more bool, err error) {
	if c.cloud {
		return c.c.NextParam(lp.Next, "page")
	}
	if lp.IsLastPage || lp.NextPageStart == nil {
		return "", false, nil
	}
	return strconv.Itoa(*lp.NextPageStart), true, nil
}

func (c *Client) nextCursor(lp listPage, page int) (string, error) {
	p, more, err := c.nextParam(lp)
	if err != nil || !more {
		return "", err
	}
	return gitapi.EncodeCursor(page+1, p), nil
}

type hookItem struct {
	UUID string      `json:"uuid"`
	ID   json.Number `json:"id"`
	URL  string      `json:"url"`
}

func (c *Client) findHook(ctx context.Context, call callFn, hookURL string) (string, error) {
	param := ""
	for p := 1; p <= gitapi.MaxPages; p++ {
		q := url.Values{}
		if c.cloud {
			q.Set("pagelen", "100")
		} else {
			q.Set("limit", "100")
		}
		if param != "" {
			q.Set(c.pageKey(), param)
		}
		resp, err := call(ctx, "GET", "/"+c.hooksSeg(), q, nil)
		if err != nil {
			return "", err
		}
		lp, err := c.page(resp)
		if err != nil {
			return "", err
		}
		for _, raw := range lp.Values {
			var h hookItem
			if json.Unmarshal(raw, &h) == nil && h.URL == hookURL {
				if id := c.hookIDOf(h.UUID, h.ID.String()); id != "" {
					return id, nil
				}
			}
		}
		var more bool
		if param, more, err = c.nextParam(lp); err != nil || !more {
			return "", err
		}
	}
	return "", nil
}

func (c *Client) getChange(ctx context.Context, call callFn, number int64) (scm.Change, error) {
	seg := "/pull-requests/"
	if c.cloud {
		seg = "/pullrequests/"
	}
	resp, err := call(ctx, "GET", seg+strconv.FormatInt(number, 10), nil, nil)
	if err != nil {
		return scm.Change{}, err
	}
	return c.decodeChange(resp.Body)
}

func (c *Client) decodeChange(raw []byte) (scm.Change, error) {
	if c.cloud {
		var p cloudPR
		if err := json.Unmarshal(raw, &p); err != nil {
			return scm.Change{}, &gitapi.Error{Code: gitapi.CodeError}
		}
		return p.change(), nil
	}
	var p dcPR
	if err := json.Unmarshal(raw, &p); err != nil {
		return scm.Change{}, &gitapi.Error{Code: gitapi.CodeError}
	}
	return p.change(), nil
}

func (c *Client) openChanges(ctx context.Context, call callFn, page int, param string) (scm.ChangePage, error) {
	q := url.Values{}
	seg := "/pull-requests"
	if c.cloud {
		seg = "/pullrequests"
		q.Set("state", "OPEN")
		q.Set("sort", "-created_on")
		q.Set("pagelen", "50")
	} else {
		q.Set("state", "OPEN")
		q.Set("order", "NEWEST")
		q.Set("limit", "100")
	}
	if param != "" {
		q.Set(c.pageKey(), param)
	}
	resp, err := call(ctx, "GET", seg, q, nil)
	if err != nil {
		return scm.ChangePage{}, err
	}
	lp, err := c.page(resp)
	if err != nil {
		return scm.ChangePage{}, err
	}
	out := scm.ChangePage{}
	for _, raw := range lp.Values {
		ch, err := c.decodeChange(raw)
		if err != nil {
			return scm.ChangePage{}, err
		}
		out.Items = append(out.Items, ch)
	}
	out.Next, err = c.nextCursor(lp, page)
	return out, err
}
