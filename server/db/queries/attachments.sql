-- Attachments. Every query filters by org_id; the issue is the
-- composite (org_id, issue_id) foreign key.

-- name: InsertAttachment :one
-- scope: resolved
-- id is the uuid inside a file's object key (minted at prepare), or a new
-- uuidv7() for a link.
INSERT INTO attachments (id, org_id, issue_id, kind, title, subtitle, url, object_key,
                         filename, content_type, size_bytes, creator_id)
VALUES (COALESCE(sqlc.narg(id)::uuid, uuidv7()), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, created_at;

-- name: GetAttachment :one
-- scope: team
-- Team scoped by the attachment's issue (T20 #11): an attachment of a hidden
-- issue is not found, before any presign (DownloadURL).
SELECT a.id, a.issue_id, a.kind, a.title, a.subtitle, a.url, a.object_key, a.filename,
       a.content_type, a.size_bytes, a.creator_id, a.created_at,
       u.username AS creator_username, u.name AS creator_name
FROM attachments a
JOIN issues i ON i.org_id = a.org_id AND i.id = a.issue_id
LEFT JOIN users u ON u.id = a.creator_id
WHERE a.org_id = $1 AND a.id = $2
  AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: ListIssueAttachments :many
-- scope: resolved
-- Served by attachments_issue_idx (org_id, issue_id, created_at).
SELECT a.id, a.issue_id, a.kind, a.title, a.subtitle, a.url, a.object_key, a.filename,
       a.content_type, a.size_bytes, a.creator_id, a.created_at,
       u.username AS creator_username, u.name AS creator_name
FROM attachments a
LEFT JOIN users u ON u.id = a.creator_id
WHERE a.org_id = $1 AND a.issue_id = $2
ORDER BY a.created_at, a.id
LIMIT 500;

-- name: AttachmentKeyHeldSystem :one
-- scope: system
-- Whether any row of the org holds exactly this object key, whatever the
-- caller sees. The attach path asks it before the store is touched: a key
-- held by a hidden issue's row answers the same 409 as one held by a visible
-- one, with no Head and no Delete (T20, LAUN-219 review). It matches the
-- exact key only, never the id inside it: a key made from a known id with
-- another filename must not answer differently from a made-up key (#10). It
-- returns a boolean only, never the row.
SELECT EXISTS (
    SELECT 1 FROM attachments WHERE org_id = $1 AND object_key = sqlc.arg(object_key)::text
) AS held;
