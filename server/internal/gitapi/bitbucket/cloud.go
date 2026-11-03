package bitbucket

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"devops/internal/gitapi"
	"devops/internal/scm"
)

var (
	uuidRE = regexp.MustCompile(`^\{[0-9a-fA-F-]{36}\}$`)
	slugRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// cloudPart checks one path part (a {uuid} or a slug) and escapes it. The
// braces of a {uuid} are percent-encoded (%7B, %7D): whether Bitbucket wants
// that form or the literal braces is "verify at build" (T17 #15).
func cloudPart(s string) (string, bool) {
	if !uuidRE.MatchString(s) && !(slugRE.MatchString(s) && gitapi.SafeSegment(s)) {
		return "", false
	}
	return url.PathEscape(s), true
}

// cloudBase turns the repo id "{workspace-uuid}/{repo-uuid}" into its path.
func cloudBase(id string) (string, bool) {
	ws, repo, ok := strings.Cut(id, "/")
	if !ok {
		return "", false
	}
	w, ok1 := cloudPart(ws)
	r, ok2 := cloudPart(repo)
	if !ok1 || !ok2 {
		return "", false
	}
	return "/repositories/" + w + "/" + r, true
}

type cloudRepoJSON struct {
	UUID     string `json:"uuid"`
	FullName string `json:"full_name"`
	Links    struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Workspace struct {
		UUID string `json:"uuid"`
	} `json:"workspace"`
}

func (r cloudRepoJSON) ref() (scm.RepoRef, bool) {
	if !uuidRE.MatchString(r.UUID) || !uuidRE.MatchString(r.Workspace.UUID) {
		return scm.RepoRef{}, false
	}
	id := strings.ToLower(r.Workspace.UUID) + "/" + strings.ToLower(r.UUID)
	return scm.RepoRef{ID: id, Key: id, Path: r.FullName, URL: r.Links.HTML.Href}, true
}

// cloudCheck reads /user. A repository access token has no user: a 403 or 404
// there falls back to a repository list, which must show at least one
// repository (a token that sees none can do nothing for us). Whether a
// repository access token can list at all is "verify at build".
func (c *Client) cloudCheck(ctx context.Context) (scm.Identity, error) {
	resp, err := c.c.Do(ctx, http.MethodGet, "/user", nil, nil)
	if err == nil {
		var u struct {
			Nickname    string `json:"nickname"`
			DisplayName string `json:"display_name"`
		}
		if err := resp.JSON(&u); err != nil {
			return scm.Identity{}, err
		}
		return scm.Identity{Login: u.Nickname, Name: u.DisplayName}, nil
	}
	if code := gitapi.CodeOf(err); code != gitapi.CodeForbidden && code != gitapi.CodeNotFound {
		return scm.Identity{}, err
	}
	resp, lerr := c.c.Do(ctx, http.MethodGet, "/repositories", url.Values{"role": {"member"}, "pagelen": {"1"}}, nil)
	if lerr != nil {
		return scm.Identity{}, lerr
	}
	lp, jerr := c.page(resp)
	if jerr != nil {
		return scm.Identity{}, jerr
	}
	if len(lp.Values) == 0 {
		return scm.Identity{}, &gitapi.Error{Code: gitapi.CodeForbidden}
	}
	return scm.Identity{}, nil
}

func cleanQuery(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, s)
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func (c *Client) cloudRepos(ctx context.Context, query string, page int, param string) (scm.RepoPage, error) {
	q := url.Values{"role": {"admin"}, "pagelen": {"100"}, "sort": {"full_name"}}
	if s := cleanQuery(query); s != "" {
		q.Set("q", `name ~ "`+s+`"`)
	}
	if param != "" {
		q.Set("page", param)
	}
	resp, err := c.c.Do(ctx, http.MethodGet, "/repositories", q, nil)
	if err != nil {
		return scm.RepoPage{}, err
	}
	lp, err := c.page(resp)
	if err != nil {
		return scm.RepoPage{}, err
	}
	out := scm.RepoPage{}
	for _, raw := range lp.Values {
		var r cloudRepoJSON
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

func (c *Client) cloudRepo(ctx context.Context, id string) (scm.RepoRef, error) {
	base, ok := cloudBase(id)
	if !ok {
		return scm.RepoRef{}, gitapi.ErrBadRepo
	}
	resp, err := c.c.Do(ctx, http.MethodGet, base, nil, nil)
	if err != nil {
		return scm.RepoRef{}, err
	}
	var r cloudRepoJSON
	if err := resp.JSON(&r); err != nil {
		return scm.RepoRef{}, err
	}
	ref, ok := r.ref()
	if !ok {
		return scm.RepoRef{}, &gitapi.Error{Code: gitapi.CodeError}
	}
	return ref, nil
}

type cloudRepoRef struct {
	Repository *struct {
		UUID string `json:"uuid"`
	} `json:"repository"`
}

type cloudPR struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	Draft       bool      `json:"draft"`
	UpdatedOn   time.Time `json:"updated_on"`
	// Participants carry each person's review (LAUN-96). Nil when the host
	// left the field out: the review is then not known.
	Participants *[]struct {
		Approved bool   `json:"approved"`
		State    string `json:"state"`
	} `json:"participants"`
	Author struct {
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
		cloudRepoRef
	} `json:"source"`
	Destination struct {
		cloudRepoRef
	} `json:"destination"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (p cloudPR) change() scm.Change {
	state := "closed" // DECLINED, SUPERSEDED
	switch p.State {
	case "OPEN":
		state = "opened"
	case "MERGED":
		state = "merged"
	}
	fork := p.Source.Repository == nil || p.Destination.Repository == nil ||
		!strings.EqualFold(p.Source.Repository.UUID, p.Destination.Repository.UUID)
	author := p.Author.Nickname
	if author == "" {
		author = p.Author.AccountID
	}
	ch := scm.Change{
		Number: p.ID, URL: p.Links.HTML.Href, Title: p.Title, SourceBranch: p.Source.Branch.Name,
		Description: p.Description, Author: author, Draft: p.Draft, Fork: fork,
		HeadSHA: p.Source.Commit.Hash, State: state, Review: scm.ReviewNone, Checks: scm.ChecksNone,
		UpdatedAt: p.UpdatedOn,
	}
	if p.Participants != nil {
		ch.ReviewKnown = true
		for _, r := range *p.Participants {
			switch {
			case strings.EqualFold(r.State, "changes_requested"):
				ch.Review = scm.ReviewChangesRequested
			case (r.Approved || strings.EqualFold(r.State, "approved")) && ch.Review == scm.ReviewNone:
				ch.Review = scm.ReviewApproved
			}
			if ch.Review == scm.ReviewChangesRequested {
				break
			}
		}
	}
	return ch
}
