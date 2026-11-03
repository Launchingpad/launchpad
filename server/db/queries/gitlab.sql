-- GitLab integration (T5). Every query filters by org_id. The
-- secret hash is read by exactly one query (ListGitlabSecretHashes), the one
-- the webhook token check needs; no other query returns it.

-- The webhook token check: the hash of every ENABLED integration of the org,
-- the current one and the pending one of a rotation (T17 #11).
-- The caller compares all of them in constant time.
-- name: ListGitlabSecretHashes :many
-- scope: system
SELECT id, webhook_secret_hash, webhook_secret_next_hash
FROM git_integrations
WHERE org_id = $1 AND provider = 'gitlab' AND enabled
ORDER BY id;

-- name: GetGitlabProjectMapping :one
-- scope: system
SELECT id, team_id
FROM git_repo_map
WHERE org_id = $1 AND integration_id = $2 AND repo_key = $3;

-- Idempotency on a delivery key (one of gitlab.DeliveryKeys, stored in
-- event_uuid; with both headers a delivery records two keys).
-- No row back means the delivery was seen before (a concurrent delivery of
-- the same key waits on the unique index and then gets no row once the
-- first commits).
-- name: InsertGitlabWebhookEvent :one
-- scope: system
INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type, repo_key, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (org_id, integration_id, event_uuid) DO NOTHING
RETURNING id;

-- name: FinishGitlabWebhookEvent :exec
-- scope: system
UPDATE git_webhook_events
SET status = $3, error = sqlc.narg(error), processed_at = now()
WHERE org_id = $1 AND id = $2;

-- Candidate issues for the identifiers an MR names. The pairs are filtered
-- exactly in Go; both arrays use indexes (teams key, issues team+number).
-- name: FindIssuesByKeyNumber :many
-- scope: system
SELECT i.id, i.team_id, t.key AS team_key, i.number
FROM issues i
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
WHERE i.org_id = $1
  AND t.key = ANY(sqlc.arg(keys)::text[])
  AND i.number = ANY(sqlc.arg(numbers)::integer[])
  AND t.archived_at IS NULL
ORDER BY i.id;

-- mr_links rows are scoped by integration (migration 0004): two GitLab
-- instances of one org may share a numeric project id and an MR iid.
-- The state columns are migration 0016 (LAUN-83, decisions-T17 #18).
-- name: InsertMRLink :one
-- scope: system
INSERT INTO mr_links (org_id, issue_id, integration_id, repo_key, number, url, title, state, source_branch,
                      draft, head_sha, review, author, repo_path, provider_updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (issue_id, integration_id, repo_key, number) DO NOTHING
RETURNING id;

-- The link row of one issue and one MR, locked. The webhook decides what an
-- event changes in Go (stale or not, decisions-T17 #18) from the row it reads
-- here, so the read and the write are one lock: a concurrent event on the
-- same MR waits and then reads the committed row.
-- name: LockMRLink :one
-- scope: system
SELECT id, state, url, title, source_branch, draft, head_sha, review, checks, author, repo_path, provider_updated_at
FROM mr_links
WHERE org_id = $1 AND issue_id = $2 AND integration_id = $3 AND repo_key = $4 AND number = $5
FOR UPDATE;

-- The teams of the issues of every link of one MR (a plain read, no lock): the
-- team rank locks are taken for them BEFORE the issue rows (LAUN-96), in team id order.
-- name: ListMRLinkTeams :many
-- scope: system
SELECT DISTINCT i.team_id
FROM issues i
WHERE i.org_id = $1 AND i.id IN (
    SELECT l.issue_id FROM mr_links l
    WHERE l.org_id = $1 AND l.integration_id = $2 AND l.repo_key = $3 AND l.number = $4)
ORDER BY i.team_id;

-- The same for the links of one repo with head sha sha (the checks path).
-- name: ListMRLinkTeamsByHeadSHA :many
-- scope: system
SELECT DISTINCT i.team_id
FROM issues i
WHERE i.org_id = $1 AND i.id IN (
    SELECT l.issue_id FROM mr_links l
    WHERE l.org_id = $1 AND l.integration_id = $2 AND l.repo_key = $3 AND l.head_sha = $4 AND l.head_sha <> '')
ORDER BY i.team_id;

-- The issue rows of every link of one MR, locked FOR UPDATE in id order
-- (LAUN-96). Every writer takes the issue row BEFORE the link row (linkAndMove
-- locks the issue, then LockMRLink); a close, refresh or review event takes
-- them here first, then LockMRLinksByMR, so a close racing a reopen cannot
-- deadlock. A single statement: the ORDER BY puts the locks in id order.
-- name: LockMRLinkIssues :many
-- scope: system
SELECT i.id, i.team_id
FROM issues i
WHERE i.org_id = $1 AND i.id IN (
    SELECT l.issue_id FROM mr_links l
    WHERE l.org_id = $1 AND l.integration_id = $2 AND l.repo_key = $3 AND l.number = $4)
ORDER BY i.id
FOR UPDATE OF i;

-- Every link of one MR, locked, with the issue it belongs to. Close, refresh
-- and review events work on links that exist; they link nothing.
-- name: LockMRLinksByMR :many
-- scope: system
SELECT l.id, l.issue_id, l.state, l.url, l.title, l.source_branch, l.draft, l.head_sha, l.review, l.checks,
       l.author, l.repo_path, l.provider_updated_at,
       i.number AS issue_number, i.team_id, i.assignee_id, t.key AS team_key
FROM mr_links l
JOIN issues i ON i.org_id = l.org_id AND i.id = l.issue_id
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
WHERE l.org_id = $1 AND l.integration_id = $2 AND l.repo_key = $3 AND l.number = $4
  AND l.issue_id = ANY(sqlc.arg(issue_ids)::uuid[])
ORDER BY l.id
FOR UPDATE OF l;

-- The write of a link row the caller has locked. Go computed every value.
-- reset_checks clears the checks of an older head sha (decisions-T17 #18).
-- name: ApplyMRLink :exec
-- scope: system
UPDATE mr_links SET
    url = sqlc.arg(url), title = sqlc.arg(title), source_branch = sqlc.arg(source_branch), state = sqlc.arg(state),
    draft = sqlc.arg(draft), head_sha = sqlc.arg(head_sha), review = sqlc.arg(review), author = sqlc.arg(author),
    repo_path = sqlc.arg(repo_path), provider_updated_at = sqlc.narg(provider_updated_at),
    checks = CASE WHEN sqlc.arg(reset_checks)::boolean THEN 'none' ELSE checks END,
    checks_detail = CASE WHEN sqlc.arg(reset_checks)::boolean THEN '{}'::jsonb ELSE checks_detail END,
    checks_updated_at = CASE WHEN sqlc.arg(reset_checks)::boolean THEN NULL ELSE checks_updated_at END,
    updated_at = now()
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id);

-- The issue rows of every link of one repo with head sha sha, locked FOR UPDATE
-- in id order (LAUN-96): the checks path takes the issue rows before the link
-- rows, like every other link writer. Link-id order is not issue-id order, so
-- locking the links alone could cross linkAndMove on an MR that names two issues.
-- name: LockMRLinkIssuesByHeadSHA :many
-- scope: system
SELECT i.id, i.team_id
FROM issues i
WHERE i.org_id = $1 AND i.id IN (
    SELECT l.issue_id FROM mr_links l
    WHERE l.org_id = $1 AND l.integration_id = $2 AND l.repo_key = $3 AND l.head_sha = $4 AND l.head_sha <> '')
ORDER BY i.id
FOR UPDATE OF i;

-- Every link of one repo whose head sha is sha, locked (LAUN-85, decisions-T17
-- #19). mr_links_checks_idx serves it; the head_sha <> '' repeats the index
-- predicate. A check event names a commit, not a merge request.
-- name: LockMRLinksByHeadSHA :many
-- scope: system
SELECT l.id, l.issue_id, l.number, l.url, l.checks, l.checks_detail, l.checks_updated_at,
       i.number AS issue_number, i.team_id, i.assignee_id, t.key AS team_key
FROM mr_links l
JOIN issues i ON i.org_id = l.org_id AND i.id = l.issue_id
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
WHERE l.org_id = $1 AND l.integration_id = $2 AND l.repo_key = $3 AND l.head_sha = $4 AND l.head_sha <> ''
  AND l.issue_id = ANY(sqlc.arg(issue_ids)::uuid[])
ORDER BY l.id
FOR UPDATE OF l;

-- The write of a check result: Go merged the entry and computed the worst
-- state. Only the checks columns change (the MR fields belong to MR events).
-- name: ApplyMRChecks :exec
-- scope: system
UPDATE mr_links SET checks = $3, checks_detail = $4, checks_updated_at = $5, updated_at = now()
WHERE org_id = $1 AND id = $2;

-- The Changes block of an issue (decisions-T17 #18). The provider comes from
-- the integration the link was made under.
-- name: ListIssueMRLinks :many
-- scope: resolved
-- LAUN-199 (T34): the author handle is matched to a linked member of the SAME
-- connection (lower case, one holder per handle). Only an ASCII login is
-- compared: lower() would fold U+212A (Kelvin sign) to 'k'. Display only; a deactivated
-- member shows as the plain handle.
SELECT l.id, l.integration_id, g.provider, l.repo_key, l.repo_path, l.number, l.url, l.title, l.state, l.draft,
       l.review, l.checks, l.author, l.created_at, l.updated_at,
       u.id AS author_user_id, u.username AS author_username, u.name AS author_name
FROM mr_links l
JOIN git_integrations g ON g.org_id = l.org_id AND g.id = l.integration_id
LEFT JOIN git_identities gi ON gi.org_id = l.org_id AND gi.integration_id = l.integration_id
                           AND l.author ~ '^[\x01-\x7f]+$' AND gi.handle_key = lower(l.author)
LEFT JOIN users u ON u.id = gi.user_id AND u.deactivated_at IS NULL
WHERE l.org_id = $1 AND l.issue_id = $2
ORDER BY l.created_at, l.id;

-- The dedupe-key store keeps 30 days (decisions-T17 #8). Install-level
-- housekeeping: not per org, so no org filter and no event.
-- name: PruneGitWebhookEvents :execrows
-- scope: system
DELETE FROM git_webhook_events WHERE received_at < $1;
