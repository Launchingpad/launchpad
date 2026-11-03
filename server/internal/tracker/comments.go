package tracker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
)

// CommentInput is the body of POST .../comments and PATCH /comments/{id}.
type CommentInput struct {
	Body     string     `json:"body"`
	ParentID *uuid.UUID `json:"parentId"` // POST only: reply to a comment on the same issue
}

func validateBody(b string) (string, error) {
	if strings.TrimSpace(b) == "" || utf8.RuneCountInString(b) > 100_000 {
		return "", httpx.Invalid("body must be 1-100000 characters")
	}
	return b, nil
}

func (s *Service) listComments(ctx context.Context, q *dbq.Queries, orgID, issueID uuid.UUID) ([]Comment, error) {
	rows, err := q.ListIssueComments(ctx, dbq.ListIssueCommentsParams{OrgID: orgID, IssueID: &issueID})
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	out := make([]Comment, 0, len(rows))
	for _, r := range rows {
		out = append(out, Comment{
			ID: r.ID, IssueID: &issueID, ParentID: r.ParentID, Body: r.Body,
			Author:    userRef(r.AuthorID, r.AuthorUsername, r.AuthorName),
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, EditedAt: r.EditedAt,
		})
	}
	return out, nil
}

// ListComments lists an issue's comments, oldest first.
func (s *Service) ListComments(ctx context.Context, org *auth.Org, issueRef string) ([]Comment, error) {
	id, err := resolveIssueID(ctx, s.q, org, issueRef)
	if err != nil {
		return nil, err
	}
	return s.listComments(ctx, s.q, org.ID, id)
}

func toComment(c dbq.Comment, p *auth.Principal) Comment {
	out := Comment{ID: c.ID, IssueID: c.IssueID, DocumentID: c.DocumentID, ParentID: c.ParentID, Body: c.Body,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, EditedAt: c.EditedAt}
	if c.AuthorID != nil && *c.AuthorID == p.UserID {
		out.Author = &UserRef{ID: p.UserID, Username: p.Username, Name: p.Name}
	}
	return out
}

// commentTarget is what a new comment is attached to: an issue or a
// document (exactly one is set).
type commentTarget struct {
	issueID, documentID *uuid.UUID
}

// CreateComment adds a comment to an issue and emits comment.created.
func (s *Service) CreateComment(ctx context.Context, org *auth.Org, p *auth.Principal, issueRef string, in CommentInput) (Comment, error) {
	return s.createComment(ctx, org, p, in, func(q *dbq.Queries) (commentTarget, error) {
		id, err := resolveIssueID(ctx, q, org, issueRef)
		return commentTarget{issueID: &id}, err
	})
}

// createComment is the shared body of the issue and document comment
// creates. resolve finds the target inside the transaction (a 404 there is a
// path 404). A reply must be to a comment on the same target.
func (s *Service) createComment(ctx context.Context, org *auth.Org, p *auth.Principal, in CommentInput,
	resolve func(q *dbq.Queries) (commentTarget, error)) (Comment, error) {
	scopeAll, scopeIDs := org.Access.Args()
	body, err := validateBody(in.Body)
	if err != nil {
		return Comment{}, err
	}
	var out dbq.Comment
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		target, err := resolve(q)
		if err != nil {
			return err
		}
		if in.ParentID != nil {
			// A reply joins a comment tree: serialise with tree deletes (see
			// lockCommentTrees), so the parent check below and a concurrent
			// delete cannot interleave.
			if err := lockCommentTrees(ctx, tx, org.ID); err != nil {
				return err
			}
			parent, err := q.GetComment(ctx, dbq.GetCommentParams{OrgID: org.ID, ID: *in.ParentID, AllTeams: scopeAll, TeamIds: scopeIDs})
			if err != nil {
				return invalidRef(err, "unknown parentId")
			}
			if !eqPtr(parent.IssueID, target.issueID) || !eqPtr(parent.DocumentID, target.documentID) {
				// T20 #23: for a caller who does not see every team, a
				// parent that is not a comment of this target is "unknown",
				// the text of a hidden or missing one. A caller who sees
				// every team keeps the plain text.
				if !org.Access.All {
					return httpx.Invalid("unknown parentId")
				}
				if target.documentID != nil {
					return httpx.Invalid("parentId is a comment on another document")
				}
				return httpx.Invalid("parentId is a comment on another issue")
			}
		}
		if target.documentID != nil {
			out, err = q.InsertDocumentComment(ctx, dbq.InsertDocumentCommentParams{
				OrgID: org.ID, DocumentID: target.documentID, ParentID: in.ParentID, AuthorID: &p.UserID, Body: body,
			})
		} else {
			out, err = q.InsertComment(ctx, dbq.InsertCommentParams{
				OrgID: org.ID, IssueID: target.issueID, ParentID: in.ParentID, AuthorID: &p.UserID, Body: body,
			})
		}
		if err != nil {
			if database.IsPgCode(err, database.CodeForeignKeyViolation) && database.ConstraintName(err) == "comments_parent_fk" {
				return httpx.Invalid("unknown parentId")
			}
			return fmt.Errorf("insert comment: %w", err)
		}
		scope, err := commentEventScope(ctx, q, org.ID, out.IssueID, out.DocumentID)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.CommentCreated, EntityType: "comment", EntityID: out.ID, ActorID: &p.UserID,
			Payload: commentPayload(out.ID, out.IssueID, out.DocumentID),
		}, scope)
		return err
	})
	if err != nil {
		return Comment{}, err
	}
	return toComment(out, p), nil
}

// commentPayload is the event payload of a comment: its id and its target.
// issueId stays in every payload (null for a document comment) so T1
// clients that read it keep working.
func commentPayload(id uuid.UUID, issueID, documentID *uuid.UUID) map[string]any {
	m := map[string]any{"id": id, "issueId": issueID}
	if documentID != nil {
		m["documentId"] = documentID
	}
	return m
}

// UpdateComment edits a comment. Only its author may edit it.
func (s *Service) UpdateComment(ctx context.Context, org *auth.Org, p *auth.Principal, commentID uuid.UUID, in CommentInput) (Comment, error) {
	scopeAll, scopeIDs := org.Access.Args()
	if in.ParentID != nil {
		return Comment{}, httpx.Invalid("parentId cannot be changed")
	}
	body, err := validateBody(in.Body)
	if err != nil {
		return Comment{}, err
	}
	var out dbq.Comment
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		cur, err := q.GetCommentForUpdate(ctx, dbq.GetCommentForUpdateParams{OrgID: org.ID, ID: commentID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "comment")
		}
		if cur.AuthorID == nil || *cur.AuthorID != p.UserID {
			return httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden, "only the author can edit a comment")
		}
		if out, err = q.UpdateCommentBody(ctx, dbq.UpdateCommentBodyParams{OrgID: org.ID, ID: commentID, Body: body}); err != nil {
			return fmt.Errorf("update comment: %w", err)
		}
		scope, err := commentEventScope(ctx, q, org.ID, cur.IssueID, cur.DocumentID)
		if err != nil {
			return err
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.CommentUpdated, EntityType: "comment", EntityID: commentID, ActorID: &p.UserID,
			Payload: commentPayload(commentID, cur.IssueID, cur.DocumentID),
		}, scope)
		return err
	})
	if err != nil {
		return Comment{}, err
	}
	return toComment(out, p), nil
}

// DeleteComment removes a comment and, by FK cascade, every reply under it.
//
// Who may delete: a moderator (org admin role AND an admin-scoped token) may
// delete any comment. Anyone else may delete only a comment whose WHOLE
// subtree is their own: the cascade would otherwise remove other users'
// replies, which is moderation. One comment.deleted event is emitted per
// removed comment (the root's payload lists deletedReplyIds, each reply's
// has cascadedFrom).
func (s *Service) DeleteComment(ctx context.Context, org *auth.Org, p *auth.Principal, commentID uuid.UUID) error {
	scopeAll, scopeIDs := org.Access.Args()
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Taken before the subtree is read, so the subtree cannot grow
		// between the read and the cascade: every reply insert takes it too.
		if err := lockCommentTrees(ctx, tx, org.ID); err != nil {
			return err
		}
		cur, err := q.GetCommentForUpdate(ctx, dbq.GetCommentForUpdateParams{OrgID: org.ID, ID: commentID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return notFound(err, "comment")
		}
		moderator := org.Role == "admin" && p.HasScope(auth.ScopeAdmin)
		forbidden := httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"only the author of a comment and all its replies, or an org admin with an admin-scoped token, can delete it")
		if !moderator && (cur.AuthorID == nil || *cur.AuthorID != p.UserID) {
			return forbidden
		}
		replies, err := commentDescendants(ctx, tx, org.ID, commentID)
		if err != nil {
			return err
		}
		replyIDs := make([]uuid.UUID, 0, len(replies))
		for _, r := range replies {
			if !moderator && (r.author == nil || *r.author != p.UserID) {
				return forbidden // another user's reply would be cascaded away
			}
			replyIDs = append(replyIDs, r.id)
		}
		if err := q.DeleteComment(ctx, dbq.DeleteCommentParams{OrgID: org.ID, ID: commentID}); err != nil {
			return fmt.Errorf("delete comment: %w", err)
		}
		// The target outlives the comment, so its scope is read after the delete.
		scope, err := commentEventScope(ctx, q, org.ID, cur.IssueID, cur.DocumentID)
		if err != nil {
			return err
		}
		if _, err := events.Emit(ctx, q, org.ID, events.Event{
			Type: events.CommentDeleted, EntityType: "comment", EntityID: commentID, ActorID: &p.UserID,
			Payload: withField(commentPayload(commentID, cur.IssueID, cur.DocumentID), "deletedReplyIds", replyIDs),
		}, scope); err != nil {
			return err
		}
		for _, rid := range replyIDs {
			if _, err := events.Emit(ctx, q, org.ID, events.Event{
				Type: events.CommentDeleted, EntityType: "comment", EntityID: rid, ActorID: &p.UserID,
				Payload: withField(commentPayload(rid, cur.IssueID, cur.DocumentID), "cascadedFrom", commentID),
			}, scope); err != nil {
				return err
			}
		}
		return nil
	})
}

// lockCommentTrees takes the org's comment-tree advisory lock until the end
// of the transaction. Every reply insert and every delete takes it, so a
// delete's subtree read is exact: no reply can join the tree between that
// read and the cascade (a cascade with no event would leave a ghost comment
// in clients). Root comments (no parent) never join a tree and skip it.
func lockCommentTrees(ctx context.Context, tx pgx.Tx, orgID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:comment-trees:' || $1::text, 0))`, orgID); err != nil {
		return fmt.Errorf("comment tree lock: %w", err)
	}
	return nil
}

type commentNode struct {
	id     uuid.UUID
	author *uuid.UUID
}

// commentDescendants lists every reply below id, at any depth. The parent
// links cannot form a cycle (a reply's parent exists before it and parentId
// never changes), so the recursion ends. Call it under lockCommentTrees.
func commentDescendants(ctx context.Context, tx pgx.Tx, orgID, id uuid.UUID) ([]commentNode, error) {
	rows, err := tx.Query(ctx, `/* scope: resolved */
		WITH RECURSIVE d(id, author_id, depth) AS (
			SELECT c.id, c.author_id, 1 FROM comments c WHERE c.org_id = $1 AND c.parent_id = $2
			UNION ALL
			SELECT c.id, c.author_id, d.depth + 1 FROM comments c JOIN d ON c.org_id = $1 AND c.parent_id = d.id
		)
		SELECT id, author_id FROM d ORDER BY depth, id`, orgID, id)
	if err != nil {
		return nil, fmt.Errorf("comment replies: %w", err)
	}
	defer rows.Close()
	var out []commentNode
	for rows.Next() {
		var n commentNode
		if err := rows.Scan(&n.id, &n.author); err != nil {
			return nil, fmt.Errorf("comment replies: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("comment replies: %w", err)
	}
	return out, nil
}

func withField(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}
