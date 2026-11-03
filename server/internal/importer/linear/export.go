package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"devops/internal/importer"
)

// The GraphQL selections. Each list is paginated with first/after and
// includes archived rows. Page sizes keep every query well under Linear's
// complexity limit (a nested connection multiplies by its page size).
const (
	qTeams = `query Teams($after: String) { teams(first: 50, after: $after, includeArchived: true) {
  nodes { id key name description createdAt updatedAt archivedAt
          cyclesEnabled cycleStartDay cycleDuration cycleCooldownTime timezone }
  pageInfo { hasNextPage endCursor } } }`

	qStates = `query States($after: String) { workflowStates(first: 100, after: $after, includeArchived: true) {
  nodes { id name type color description position createdAt updatedAt archivedAt team { id } }
  pageInfo { hasNextPage endCursor } } }`

	qLabels = `query Labels($after: String) { issueLabels(first: 100, after: $after, includeArchived: true) {
  nodes { id name color description isGroup createdAt updatedAt archivedAt team { id } parent { id } }
  pageInfo { hasNextPage endCursor } } }`

	qUsers = `query Users($after: String) { users(first: 100, after: $after, includeDisabled: true, includeArchived: true) {
  nodes { id name displayName email active app guest createdAt archivedAt }
  pageInfo { hasNextPage endCursor } } }`

	qProjects = `query Projects($after: String) { projects(first: 25, after: $after, includeArchived: true) {
  nodes { id name description content slugId status { type name } lead { id }
          startDate targetDate createdAt updatedAt startedAt completedAt canceledAt archivedAt
          teams(first: 50) { nodes { id } } }
  pageInfo { hasNextPage endCursor } } }`

	qMilestones = `query Milestones($after: String) { projectMilestones(first: 100, after: $after, includeArchived: true) {
  nodes { id name description targetDate sortOrder createdAt updatedAt archivedAt project { id } }
  pageInfo { hasNextPage endCursor } } }`

	qCycles = `query Cycles($after: String) { cycles(first: 50, after: $after, includeArchived: true) {
  nodes { id number name description startsAt endsAt completedAt createdAt updatedAt archivedAt team { id }
          issueCountHistory completedIssueCountHistory scopeHistory completedScopeHistory inProgressScopeHistory }
  pageInfo { hasNextPage endCursor } } }`

	qIssues = `query Issues($after: String) { issues(first: 50, after: $after, includeArchived: true, orderBy: createdAt) {
  nodes { id identifier number title description priority estimate sortOrder
          createdAt updatedAt startedAt completedAt canceledAt archivedAt trashed dueDate labelIds
          team { id } state { id } assignee { id } creator { id } parent { id } project { id }
          projectMilestone { id } cycle { id }
          history(first: 50) { nodes { id createdAt fromStateId toStateId actorId }
                               pageInfo { hasNextPage endCursor } } }
  pageInfo { hasNextPage endCursor } } }`

	qIssueHistory = `query IssueHistory($id: String!, $after: String) { issue(id: $id) {
  history(first: 100, after: $after) { nodes { id createdAt fromStateId toStateId actorId }
                                       pageInfo { hasNextPage endCursor } } } }`

	qComments = `query Comments($after: String) { comments(first: 100, after: $after, includeArchived: true) {
  nodes { id body createdAt updatedAt editedAt archivedAt issueId documentContentId projectUpdateId parentId user { id } }
  pageInfo { hasNextPage endCursor } } }`

	qDocuments = `query Documents($after: String) { documents(first: 50, after: $after, includeArchived: true) {
  nodes { id title content slugId createdAt updatedAt archivedAt trashed documentContentId
          project { id } creator { id } issue { id } }
  pageInfo { hasNextPage endCursor } } }`

	qRelations = `query Relations($after: String) { issueRelations(first: 100, after: $after, includeArchived: true) {
  nodes { id type createdAt updatedAt archivedAt issue { id } relatedIssue { id } }
  pageInfo { hasNextPage endCursor } } }`

	qAttachments = `query Attachments($after: String) { attachments(first: 100, after: $after, includeArchived: true) {
  nodes { id title subtitle url sourceType metadata createdAt updatedAt archivedAt creator { id } issue { id } }
  pageInfo { hasNextPage endCursor } } }`
)

// PageInfo is a GraphQL connection's cursor.
type PageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type page[T any] struct {
	Nodes    []T      `json:"nodes"`
	PageInfo PageInfo `json:"pageInfo"`
}

// maxPages bounds one list, so a server that always says hasNextPage
// cannot loop for ever (a large workspace's list is a few hundred pages).
const maxPages = 10000

// fetchAll pages through one root connection. field is the root field.
func fetchAll[T any](ctx context.Context, c *Client, doc, field string) ([]T, error) {
	out := []T{}
	var after *string
	for i := 0; i < maxPages; i++ {
		var data map[string]page[T]
		if err := c.Query(ctx, doc, map[string]any{"after": after}, &data); err != nil {
			return nil, fmt.Errorf("%s page %d: %w", field, i+1, err)
		}
		pg, ok := data[field]
		if !ok {
			return nil, fmt.Errorf("%s: the response has no %q field", field, field)
		}
		out = append(out, pg.Nodes...)
		if !pg.PageInfo.HasNextPage || pg.PageInfo.EndCursor == nil {
			return out, nil
		}
		after = pg.PageInfo.EndCursor
	}
	return nil, fmt.Errorf("%s: more than %d pages", field, maxPages)
}

// issueNode is an issue with its nested history connection.
type issueNode struct {
	importer.Issue
	HistoryConn page[importer.HistoryEntry] `json:"history"`
}

// fetchIssues pages through the issues and follows each issue's history
// connection to its end.
func fetchIssues(ctx context.Context, c *Client, log *slog.Logger) ([]importer.Issue, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	nodes, err := fetchAll[issueNode](ctx, c, qIssues, "issues")
	if err != nil {
		return nil, err
	}
	out := make([]importer.Issue, 0, len(nodes))
	extra := 0
	for _, n := range nodes {
		is := n.Issue
		is.History = append([]importer.HistoryEntry{}, n.HistoryConn.Nodes...)
		after := n.HistoryConn.PageInfo.EndCursor
		more := n.HistoryConn.PageInfo.HasNextPage
		for i := 0; more && after != nil; i++ {
			if i >= maxPages {
				return nil, fmt.Errorf("issue %s history: more than %d pages", is.ID, maxPages)
			}
			var data struct {
				Issue struct {
					History page[importer.HistoryEntry] `json:"history"`
				} `json:"issue"`
			}
			if err := c.Query(ctx, qIssueHistory, map[string]any{"id": is.ID, "after": after}, &data); err != nil {
				return nil, fmt.Errorf("issue %s history: %w", is.ID, err)
			}
			is.History = append(is.History, data.Issue.History.Nodes...)
			more, after = data.Issue.History.PageInfo.HasNextPage, data.Issue.History.PageInfo.EndCursor
			extra++
		}
		out = append(out, is)
	}
	log.Info("export: issue history", "issues", len(out), "extra_history_pages", extra)
	return out, nil
}

// Options configure an export.
type Options struct {
	OutDir string
	// Files: download every uploads.linear.app file the texts refer to.
	Files bool
	// MaxFileBytes and MaxTotalBytes bound the downloads (defaults 100 MB,
	// the attachment limit, and 10 GB).
	MaxFileBytes, MaxTotalBytes int64
	Log                         *slog.Logger
	Now                         func() time.Time
	// Progress, when set, is told where the export is (decisions-T17 #40):
	// the entity being read with done / total entities, then the files phase
	// counting bytes. It never sees a record or a file name.
	Progress func(phase string, done, total int64)
}

// Export reads the whole workspace into opts.OutDir. It writes the JSONL
// files, files/ (when opts.Files), usermap.template.json and, last,
// manifest.json, and returns the export it wrote.
func Export(ctx context.Context, c *Client, dl *Downloader, opts Options) (*importer.Export, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if err := os.MkdirAll(opts.OutDir, 0o700); err != nil {
		return nil, fmt.Errorf("create export dir: %w", err)
	}
	root, err := os.OpenRoot(opts.OutDir)
	if err != nil {
		return nil, fmt.Errorf("open export dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	ex := &importer.Export{}
	lists := []struct {
		name string
		run  func() (int, error)
	}{
		{importer.FileTeams, func() (int, error) { return fill(ctx, c, qTeams, "teams", &ex.Teams) }},
		{importer.FileStates, func() (int, error) { return fill(ctx, c, qStates, "workflowStates", &ex.States) }},
		{importer.FileLabels, func() (int, error) { return fill(ctx, c, qLabels, "issueLabels", &ex.Labels) }},
		{importer.FileUsers, func() (int, error) { return fill(ctx, c, qUsers, "users", &ex.Users) }},
		{importer.FileProjects, func() (int, error) { return fill(ctx, c, qProjects, "projects", &ex.Projects) }},
		{importer.FileMilestones, func() (int, error) {
			return fill(ctx, c, qMilestones, "projectMilestones", &ex.Milestones)
		}},
		{importer.FileCycles, func() (int, error) { return fill(ctx, c, qCycles, "cycles", &ex.Cycles) }},
		{importer.FileIssues, func() (int, error) {
			v, err := fetchIssues(ctx, c, log)
			ex.Issues = v
			return len(v), err
		}},
		{importer.FileComments, func() (int, error) { return fill(ctx, c, qComments, "comments", &ex.Comments) }},
		{importer.FileDocuments, func() (int, error) { return fill(ctx, c, qDocuments, "documents", &ex.Documents) }},
		{importer.FileRelations, func() (int, error) { return fill(ctx, c, qRelations, "issueRelations", &ex.Relations) }},
		{importer.FileAttachments, func() (int, error) {
			return fill(ctx, c, qAttachments, "attachments", &ex.Attachments)
		}},
	}
	report := func(phase string, done, total int64) {
		if opts.Progress != nil {
			opts.Progress(phase, done, total)
		}
	}
	for i, l := range lists {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("export %s: %w", l.name, err)
		}
		report(l.name, int64(i), int64(len(lists)))
		start := time.Now()
		n, err := l.run()
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", l.name, err)
		}
		log.Info("export: fetched", "entity", l.name, "records", n,
			"took", time.Since(start).Round(time.Millisecond).String(), "requests_so_far", c.Requests())
	}

	ex.Files = []importer.File{}
	if opts.Files && dl != nil {
		dl.Progress = func(bytes, limit int64) { report("files", bytes, limit) }
		files, err := dl.DownloadAll(ctx, root, UploadURLsOf(ex), opts.MaxFileBytes, opts.MaxTotalBytes)
		if err != nil {
			return nil, err
		}
		ex.Files = files
	}

	tmpl, err := importer.UserMapTemplate(ex.Users)
	if err != nil {
		return nil, fmt.Errorf("user map template: %w", err)
	}
	if err := root.WriteFile("usermap.template.json", append(tmpl, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("write user map template: %w", err)
	}
	if err := importer.WriteExport(root, ex, now()); err != nil {
		return nil, err
	}
	return ex, nil
}

func fill[T any](ctx context.Context, c *Client, doc, field string, dst *[]T) (int, error) {
	v, err := fetchAll[T](ctx, c, doc, field)
	if err != nil {
		return 0, err
	}
	*dst = v
	return len(v), nil
}

// UploadURLsOf is every uploads.linear.app URL in the export's texts and
// attachment links.
func UploadURLsOf(ex *importer.Export) []string {
	texts := []string{}
	for _, x := range ex.Issues {
		texts = append(texts, deref(x.Description))
	}
	for _, x := range ex.Comments {
		texts = append(texts, x.Body)
	}
	for _, x := range ex.Documents {
		texts = append(texts, deref(x.Content))
	}
	for _, x := range ex.Projects {
		texts = append(texts, x.Description, deref(x.Content))
	}
	for _, x := range ex.Milestones {
		texts = append(texts, deref(x.Description))
	}
	for _, x := range ex.Attachments {
		texts = append(texts, x.URL)
	}
	return importer.UploadURLs(texts...)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Counts is a summary of an export: records per file (no content).
func Counts(ex *importer.Export) map[string]int {
	m := importer.NewManifest(ex, time.Time{})
	return m.Counts
}

// MarshalCounts is Counts as indented JSON.
func MarshalCounts(ex *importer.Export) ([]byte, error) {
	return json.MarshalIndent(Counts(ex), "", "  ")
}
