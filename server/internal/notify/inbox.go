package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// The inbox (LAUN-35, docs/internal/decisions-T13-notifications.md #27-#31).
// Every method works on the caller's own rows in one org and only on rows
// with inbox = true: an email-only row is never read, changed or counted.

// Inbox list limits (#27).
const (
	InboxDefaultLimit = 30
	InboxMaxLimit     = 100
)

// InboxActor is the person who caused a notification.
type InboxActor struct {
	ID       uuid.UUID `json:"id"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
}

// InboxIssue is the issue a notification is about, as it is now.
type InboxIssue struct {
	ID         uuid.UUID `json:"id"`
	Identifier string    `json:"identifier"`
	Title      string    `json:"title"`
	TeamKey    string    `json:"teamKey"`
	StateName  string    `json:"stateName"`
}

// InboxProject is the project a project_update_due notification is about.
type InboxProject struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// InboxData holds what the cause adds: the comment id of a comment or a
// mention, and the state names of a state change. No text of a comment or a
// description (#29).
type InboxData struct {
	CommentID     *uuid.UUID `json:"commentId,omitempty"`
	FromStateName *string    `json:"fromStateName,omitempty"`
	ToStateName   *string    `json:"toStateName,omitempty"`
}

// InboxItem is one inbox row.
type InboxItem struct {
	ID         uuid.UUID   `json:"id"`
	Kind       string      `json:"kind"`
	CreatedAt  time.Time   `json:"createdAt"`
	ReadAt     *time.Time  `json:"readAt"`
	ArchivedAt *time.Time  `json:"archivedAt"`
	Actor      *InboxActor `json:"actor"`
	// Issue is null for a project notification, Project is null for an issue one (LAUN-196).
	Issue   *InboxIssue   `json:"issue"`
	Project *InboxProject `json:"project"`
	Data    InboxData     `json:"data"`
}

// InboxPage is one page of the list.
type InboxPage = httpx.Page[InboxItem]

// UnreadCount is the body of the count route and the stream's count event.
type UnreadCount struct {
	Count int64 `json:"count"`
}

// InboxQuery is a list request: whether to show only unread rows, whether to
// show the archived rows instead of the live ones, the page size (0: the
// default) and the opaque cursor of the previous page.
type InboxQuery struct {
	UnreadOnly bool
	Archived   bool
	Limit      int
	Cursor     string
}

// InboxPatch is a state change: a nil field is left as it is.
type InboxPatch struct {
	Read     *bool `json:"read"`
	Archived *bool `json:"archived"`
}

// ReadAllInput is the body of POST /notifications/read-all.
type ReadAllInput struct {
	Before *time.Time `json:"before"`
}

// ReadAllResult says how many rows the call marked.
type ReadAllResult struct {
	Marked int64 `json:"marked"`
}

// StreamRow is one new row of the live stream.
type StreamRow struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	InboxSeq int64     `json:"inboxSeq"`
}

type inboxCursor struct {
	At time.Time `json:"t"`
	ID uuid.UUID `json:"id"`
}

func encodeInboxCursor(c inboxCursor) string {
	b, _ := json.Marshal(c) // plain fields cannot fail to marshal
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeInboxCursor(s string) (inboxCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	var c inboxCursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == uuid.Nil || c.At.IsZero() {
		return inboxCursor{}, httpx.Invalid("malformed cursor")
	}
	return c, nil
}

// inboxRow is the shared shape of the list and get queries.
type inboxRow struct {
	ID            uuid.UUID
	Kind          string
	CreatedAt     time.Time
	ReadAt        *time.Time
	ArchivedAt    *time.Time
	Data          json.RawMessage
	IssueID       *uuid.UUID
	Identifier    string
	IssueTitle    *string
	TeamKey       *string
	StateName     *string
	ProjectID     *uuid.UUID
	ProjectName   *string
	ActorID       *uuid.UUID
	ActorUsername *string
	ActorName     *string
	FromStateName *string
	ToStateName   *string
}

func toInboxItem(r inboxRow) InboxItem {
	it := InboxItem{ID: r.ID, Kind: r.Kind, CreatedAt: r.CreatedAt, ReadAt: r.ReadAt, ArchivedAt: r.ArchivedAt,
		Data: InboxData{FromStateName: r.FromStateName, ToStateName: r.ToStateName}}
	if r.IssueID != nil && r.IssueTitle != nil && r.TeamKey != nil && r.StateName != nil {
		it.Issue = &InboxIssue{ID: *r.IssueID, Identifier: r.Identifier, Title: *r.IssueTitle, TeamKey: *r.TeamKey, StateName: *r.StateName}
	}
	if r.ProjectID != nil && r.ProjectName != nil {
		it.Project = &InboxProject{ID: *r.ProjectID, Name: *r.ProjectName}
	}
	if r.ActorID != nil && r.ActorUsername != nil && r.ActorName != nil {
		it.Actor = &InboxActor{ID: *r.ActorID, Username: *r.ActorUsername, Name: *r.ActorName}
	}
	var d struct {
		CommentID *uuid.UUID `json:"commentId"`
	}
	if json.Unmarshal(r.Data, &d) == nil {
		it.Data.CommentID = d.CommentID
	}
	return it
}

// ListInbox returns one page of the caller's inbox, newest first.
func (s *Service) ListInbox(ctx context.Context, org *auth.Org, user uuid.UUID, in InboxQuery) (InboxPage, error) {
	scopeAll, scopeIDs := org.Access.Args()
	limit := in.Limit
	if limit == 0 {
		limit = InboxDefaultLimit
	}
	if limit < 1 || limit > InboxMaxLimit {
		return InboxPage{}, httpx.Invalid("limit must be 1-%d", InboxMaxLimit)
	}
	p := dbq.ListInboxParams{OrgID: org.ID, UserID: user, AllTeams: scopeAll, TeamIds: scopeIDs, Archived: in.Archived, UnreadOnly: in.UnreadOnly, RowLimit: int32(limit) + 1}
	if in.Cursor != "" {
		c, err := decodeInboxCursor(in.Cursor)
		if err != nil {
			return InboxPage{}, err
		}
		p.CursorAt, p.CursorID = &c.At, &c.ID
	}
	rows, err := s.q.ListInbox(ctx, p)
	if err != nil {
		return InboxPage{}, fmt.Errorf("list inbox: %w", err)
	}
	page := InboxPage{Items: make([]InboxItem, 0, min(len(rows), limit))}
	for i, r := range rows {
		if i == limit {
			// One more row than the page: there is a next page.
			last := page.Items[limit-1]
			c := encodeInboxCursor(inboxCursor{At: last.CreatedAt, ID: last.ID})
			page.NextCursor = &c
			break
		}
		page.Items = append(page.Items, toInboxItem(inboxRow(r)))
	}
	return page, nil
}

// UnreadCount counts the caller's unread, unarchived inbox rows.
func (s *Service) UnreadCount(ctx context.Context, org *auth.Org, user uuid.UUID) (int64, error) {
	scopeAll, scopeIDs := org.Access.Args()
	n, err := s.q.CountUnreadInbox(ctx, dbq.CountUnreadInboxParams{OrgID: org.ID, RecipientID: user, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return 0, fmt.Errorf("count unread: %w", err)
	}
	return n, nil
}

// PatchInbox sets the read and archived state of one row and returns it.
// Both values work in both directions; a repeat of the same call changes
// nothing (a repeat of read:true keeps the first read_at). An empty patch is
// 400; a row of another user or org, or an email-only row, is 404.
func (s *Service) PatchInbox(ctx context.Context, org *auth.Org, user, id uuid.UUID, in InboxPatch) (InboxItem, error) {
	scopeAll, scopeIDs := org.Access.Args()
	if in.Read == nil && in.Archived == nil {
		return InboxItem{}, httpx.Invalid("name read or archived (true or false)")
	}
	_, err := s.q.SetInboxItemState(ctx, dbq.SetInboxItemStateParams{
		OrgID: org.ID, UserID: user, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs,
		SetRead: in.Read != nil, Read: in.Read != nil && *in.Read,
		SetArchived: in.Archived != nil, Archived: in.Archived != nil && *in.Archived,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return InboxItem{}, httpx.NotFound("notification")
	}
	if err != nil {
		return InboxItem{}, fmt.Errorf("set inbox state: %w", err)
	}
	r, err := s.q.GetInboxItem(ctx, dbq.GetInboxItemParams{OrgID: org.ID, RecipientID: user, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
	if errors.Is(err, pgx.ErrNoRows) {
		return InboxItem{}, httpx.NotFound("notification")
	}
	if err != nil {
		return InboxItem{}, fmt.Errorf("get inbox item: %w", err)
	}
	return toInboxItem(inboxRow(r)), nil
}

// ReadAll marks the caller's unread, unarchived rows made at or before
// before as read. before is required, so a retry marks the same rows and
// never one that arrived later.
func (s *Service) ReadAll(ctx context.Context, org *auth.Org, user uuid.UUID, in ReadAllInput) (ReadAllResult, error) {
	scopeAll, scopeIDs := org.Access.Args()
	if in.Before == nil || in.Before.IsZero() {
		return ReadAllResult{}, httpx.Invalid("before is required (the createdAt of the newest row you have)")
	}
	n, err := s.q.MarkInboxReadBefore(ctx, dbq.MarkInboxReadBeforeParams{OrgID: org.ID, RecipientID: user, CreatedAt: *in.Before, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return ReadAllResult{}, fmt.Errorf("read all: %w", err)
	}
	return ReadAllResult{Marked: n}, nil
}

// StreamRows returns the caller's new inbox rows after seq, oldest first.
func (s *Service) StreamRows(ctx context.Context, org *auth.Org, user uuid.UUID, after int64, limit int32) ([]StreamRow, error) {
	scopeAll, scopeIDs := org.Access.Args()
	rows, err := s.q.ListInboxStreamRows(ctx, dbq.ListInboxStreamRowsParams{OrgID: org.ID, RecipientID: user, AfterSeq: after, RowLimit: limit, AllTeams: scopeAll, TeamIds: scopeIDs})
	if err != nil {
		return nil, fmt.Errorf("stream rows: %w", err)
	}
	out := make([]StreamRow, len(rows))
	for i, r := range rows {
		out[i] = StreamRow{ID: r.ID, Kind: r.Kind, InboxSeq: r.InboxSeq}
	}
	return out, nil
}

// LatestInboxSeq is the highest inbox_seq of the caller's rows (0: none), the
// start of a stream that has no resume point.
func (s *Service) LatestInboxSeq(ctx context.Context, org *auth.Org, user uuid.UUID) (int64, error) {
	n, err := s.q.MaxInboxSeq(ctx, dbq.MaxInboxSeqParams{OrgID: org.ID, RecipientID: user})
	if err != nil {
		return 0, fmt.Errorf("max inbox seq: %w", err)
	}
	return n, nil
}
