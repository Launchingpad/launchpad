package tracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// Documents (T3). A document has a stable slug, made once from its first
// title plus a random suffix, so its URL survives a rename. GET accepts the
// id or the slug.

func validateDocTitle(t string) (string, error) {
	t = strings.TrimSpace(t)
	if l := utf8.RuneCountInString(t); l < 1 || l > 255 {
		return "", httpx.Invalid("title must be 1-255 characters")
	}
	return t, nil
}

// docSlug is slug(title, 50) + "-" + 12 random hex characters.
func docSlug(title string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("slug suffix: %w", err)
	}
	base := Slug(title, 50)
	if base == "" {
		base = "doc"
	}
	return base + "-" + hex.EncodeToString(b), nil
}

// resolveDocumentID turns an id or a slug into the document id, within org and
// the projects org.Access can see (T20 #14, #38).
func resolveDocumentID(ctx context.Context, q *dbq.Queries, org *auth.Org, ref string) (uuid.UUID, error) {
	all, ids := org.Access.Args()
	if id, err := uuid.Parse(ref); err == nil {
		d, err := q.GetDocument(ctx, dbq.GetDocumentParams{OrgID: org.ID, ID: id, AllTeams: all, TeamIds: ids})
		if err == nil {
			return d.ID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("document lookup: %w", err)
		}
		// Not an id: a title such as "deadbeef cafe babe ..." can slug to
		// something uuid.Parse accepts, so try the slug too.
	}
	d, err := q.GetDocumentBySlug(ctx, dbq.GetDocumentBySlugParams{OrgID: org.ID, Slug: ref, AllTeams: all, TeamIds: ids})
	return d.ID, notFound(err, "document")
}

// CreateDocumentInput is the POST /documents body.
type CreateDocumentInput struct {
	Title     string     `json:"title"`
	Content   string     `json:"content"`
	ProjectID *uuid.UUID `json:"projectId"`
}

// CreateDocument creates a document and emits document.created.
func (s *Service) CreateDocument(ctx context.Context, org *auth.Org, p *auth.Principal, in CreateDocumentInput) (Document, error) {
	scopeAll, scopeIDs := org.Access.Args()
	title, err := validateDocTitle(in.Title)
	if err != nil {
		return Document{}, err
	}
	if err := validateLongText("content", in.Content); err != nil {
		return Document{}, err
	}
	var id uuid.UUID
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if in.ProjectID != nil {
			if _, err := q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: *in.ProjectID, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
				return invalidRef(err, "unknown projectId")
			}
		}
		slug, err := docSlug(title)
		if err != nil {
			return err
		}
		// 48 random bits make a clash practically impossible; if one ever
		// happens the unique constraint refuses it and the caller retries.
		row, err := q.InsertDocument(ctx, dbq.InsertDocumentParams{OrgID: org.ID, ProjectID: in.ProjectID, Title: title,
			Content: in.Content, Slug: slug, CreatorID: &p.UserID})
		if err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("document slug clash; retry the request")
			}
			return fmt.Errorf("insert document: %w", err)
		}
		id = row.ID
		scope, err := documentEventScope(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.DocumentCreated, EntityType: "document", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "slug": slug, "projectId": in.ProjectID}}, scope)
		return err
	})
	if err != nil {
		return Document{}, err
	}
	return s.getDocument(ctx, org, id)
}

// UpdateDocumentInput is the PATCH /documents/{id} body.
type UpdateDocumentInput struct {
	Version   *int32               `json:"version"`
	Title     httpx.Opt[string]    `json:"title"`
	Content   httpx.Opt[string]    `json:"content"`
	ProjectID httpx.Opt[uuid.UUID] `json:"projectId"`
}

// UpdateDocument applies a partial update under the row lock, with
// optional compare-and-swap on version. The slug never changes.
func (s *Service) UpdateDocument(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, in UpdateDocumentInput) (Document, error) {
	scopeAll, scopeIDs := org.Access.Args()
	var id uuid.UUID
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		if id, err = resolveDocumentID(ctx, q, org, ref); err != nil {
			return err
		}
		cur, err := q.LockDocument(ctx, dbq.LockDocumentParams{OrgID: org.ID, ID: id, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "document")
		}
		before, err := documentEventScope(ctx, q, org.ID, id) // the event goes to the readers before and after
		if err != nil {
			return err
		}
		if in.Version != nil && *in.Version != cur.Version {
			return httpx.Errorf(http.StatusConflict, httpx.CodeVersionConflict,
				"document is at version %d, not %d; re-read it and retry", cur.Version, *in.Version)
		}
		next := dbq.UpdateDocumentParams{OrgID: org.ID, ID: id, Title: cur.Title, Content: cur.Content, ProjectID: cur.ProjectID}
		var changes []string
		if in.Title.Set {
			if in.Title.Null {
				return httpx.Invalid("title cannot be null")
			}
			t, err := validateDocTitle(in.Title.V)
			if err != nil {
				return err
			}
			if t != cur.Title {
				next.Title, changes = t, append(changes, "title")
			}
		}
		if in.Content.Set {
			c := ""
			if !in.Content.Null {
				c = in.Content.V
			}
			if err := validateLongText("content", c); err != nil {
				return err
			}
			if c != cur.Content {
				next.Content, changes = c, append(changes, "content")
			}
		}
		if in.ProjectID.Set {
			var pid *uuid.UUID
			if !in.ProjectID.Null {
				v := in.ProjectID.V
				if _, err := q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: v, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
					return invalidRef(err, "unknown projectId")
				}
				pid = &v
			}
			if !eqPtr(pid, cur.ProjectID) {
				next.ProjectID, changes = pid, append(changes, "projectId")
			}
		}
		if len(changes) == 0 {
			return nil
		}
		updated, err := q.UpdateDocument(ctx, next)
		if err != nil {
			return fmt.Errorf("update document: %w", err)
		}
		after, err := documentEventScope(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.DocumentUpdated, EntityType: "document", EntityID: id,
			ActorID: &p.UserID, Payload: map[string]any{"id": id, "changes": changes, "version": updated.Version}},
			eitherScope(before, after))
		return err
	})
	if err != nil {
		return Document{}, err
	}
	return s.getDocument(ctx, org, id)
}

const documentSelectTmpl = `/* scope: project */
SELECT d.id, d.title, %s, d.slug, d.project_id, d.creator_id, u.username, u.name, d.version,
       d.created_at, d.updated_at, d.archived_at
FROM documents d
LEFT JOIN users u ON u.id = d.creator_id
`

var (
	documentSelectFull = fmt.Sprintf(documentSelectTmpl, "d.content")
	documentSelectList = fmt.Sprintf(documentSelectTmpl, fmt.Sprintf("left(d.content, %d)", listDescriptionRunes+1))
)

func scanDocument(row pgx.Row, truncate bool) (Document, error) {
	var d Document
	var creatorID *uuid.UUID
	var creatorUser, creatorName *string
	err := row.Scan(&d.ID, &d.Title, &d.Content, &d.Slug, &d.ProjectID, &creatorID, &creatorUser, &creatorName,
		&d.Version, &d.CreatedAt, &d.UpdatedAt, &d.ArchivedAt)
	if err != nil {
		return d, err
	}
	if truncate {
		d.Content, d.ContentTruncated = truncateRunes(d.Content, listDescriptionRunes)
	}
	d.Creator = userRef(creatorID, creatorUser, creatorName)
	return d, nil
}

func (s *Service) getDocument(ctx context.Context, org *auth.Org, id uuid.UUID) (Document, error) {
	args := []any{org.ID, id}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	d, err := scanDocument(s.pool.QueryRow(ctx, documentSelectFull+" WHERE d.org_id = $1 AND d.id = $2 AND "+documentScope(org, "d", arg), args...), false)
	return d, notFound(err, "document")
}

// GetDocument reads one document by id or slug.
func (s *Service) GetDocument(ctx context.Context, org *auth.Org, ref string) (Document, error) {
	id, err := resolveDocumentID(ctx, s.q, org, ref)
	if err != nil {
		return Document{}, err
	}
	return s.getDocument(ctx, org, id)
}

// DocumentFilter is a parsed GET /documents query.
type DocumentFilter struct {
	listQuery
	Project *uuid.UUID
	Query   string
	Full    bool
}

// ParseDocumentFilter validates the GET /documents query string.
func ParseDocumentFilter(v url.Values) (DocumentFilter, error) {
	lq, err := parseListQuery(v, "documents", "project", "query", "full")
	if err != nil {
		return DocumentFilter{}, err
	}
	f := DocumentFilter{listQuery: lq, Query: strings.TrimSpace(v.Get("query"))}
	if f.Project, err = parseUUIDParam(v, "project"); err != nil {
		return f, err
	}
	if f.Full, err = parseBool(v.Get("full")); err != nil {
		return f, httpx.Invalid("full must be true or false")
	}
	return f, nil
}

// ListDocuments returns one keyset page, most recently updated first.
// query is a case-insensitive substring of the title or the content.
func (s *Service) ListDocuments(ctx context.Context, org *auth.Org, f DocumentFilter) (httpx.Page[Document], error) {
	scopeAll, scopeIDs := org.Access.Args()
	args := []any{org.ID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"d.org_id = $1", documentScope(org, "d", arg)}
	if !f.IncludeArchived {
		where = append(where, "d.archived_at IS NULL")
	}
	if f.Project != nil {
		if _, err := s.q.ProjectExists(ctx, dbq.ProjectExistsParams{OrgID: org.ID, ID: *f.Project, AllTeams: scopeAll, TeamIds: scopeIDs}); err != nil {
			return httpx.Page[Document]{}, invalidRef(err, "unknown project")
		}
		where = append(where, "d.project_id = "+arg(*f.Project))
	}
	if f.Query != "" {
		where = append(where, fmt.Sprintf("(d.title ILIKE %s OR d.content ILIKE %[1]s)", arg(likePattern(f.Query))))
	}
	if f.Cursor != nil {
		where = append(where, fmt.Sprintf("(d.updated_at, d.id) < (%s, %s)", arg(f.Cursor.At), arg(f.Cursor.ID)))
	}
	sel := documentSelectList
	if f.Full {
		sel = documentSelectFull
	}
	sql := sel + " WHERE " + strings.Join(where, " AND ") + " ORDER BY d.updated_at DESC, d.id DESC LIMIT " + arg(f.Limit+1)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.Page[Document]{}, fmt.Errorf("list documents: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Document, error) { return scanDocument(r, !f.Full) })
	if err != nil {
		return httpx.Page[Document]{}, fmt.Errorf("scan documents: %w", err)
	}
	return pageOf(items, f.Limit, "documents", func(d Document) (time.Time, uuid.UUID) { return d.UpdatedAt, d.ID }), nil
}

// ListDocumentComments lists a document's comments, oldest first.
func (s *Service) ListDocumentComments(ctx context.Context, org *auth.Org, ref string) ([]Comment, error) {
	id, err := resolveDocumentID(ctx, s.q, org, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListDocumentComments(ctx, dbq.ListDocumentCommentsParams{OrgID: org.ID, DocumentID: &id})
	if err != nil {
		return nil, fmt.Errorf("list document comments: %w", err)
	}
	out := make([]Comment, 0, len(rows))
	for _, r := range rows {
		out = append(out, Comment{ID: r.ID, DocumentID: &id, ParentID: r.ParentID, Body: r.Body,
			Author:    userRef(r.AuthorID, r.AuthorUsername, r.AuthorName),
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, EditedAt: r.EditedAt})
	}
	return out, nil
}

// CreateDocumentComment adds a comment to a document and emits
// comment.created. Edit and delete are the shared /comments/{id} routes.
func (s *Service) CreateDocumentComment(ctx context.Context, org *auth.Org, p *auth.Principal, ref string, in CommentInput) (Comment, error) {
	return s.createComment(ctx, org, p, in, func(q *dbq.Queries) (commentTarget, error) {
		id, err := resolveDocumentID(ctx, q, org, ref)
		return commentTarget{documentID: &id}, err
	})
}
