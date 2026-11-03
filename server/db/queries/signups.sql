-- Sign-up requests, org invites and platform admins
-- (docs/internal/decisions-T12-signups-invites.md). Expiry uses the database clock
-- (now()) in every query, so one clock decides it.

-- name: GetSignupMode :one
-- scope: none
SELECT signup_mode FROM platform_settings WHERE id;

-- name: GetSignupOrg :one
-- scope: none
-- The install's org for a public sign-up request: one instance is one org
-- (decisions-T8 #1), so this is the oldest org, the one setup made.
SELECT id, slug, name FROM orgs ORDER BY created_at, id LIMIT 1;

-- name: LockSignups :exec
-- scope: none
-- Serialises sign-up writes, so the pending cap is exact.
SELECT pg_advisory_xact_lock(hashtext('launchpad-signups'));

-- name: ExpireSignupRequests :execrows
-- scope: system
-- The lazy expiry: a pending row older than its expires_at becomes expired
-- (and loses its code). Runs before each read and write of the table.
UPDATE signup_requests
SET status = 'expired', code_salt = NULL, code_hash = NULL, code_expires_at = NULL
WHERE status = 'pending' AND expires_at <= now();

-- name: CountPendingSignups :one
-- scope: system
-- awaiting true: the requests waiting for their email confirm code (their
-- own, larger cap); false: every other pending request (the admin queue).
SELECT count(*) FROM signup_requests
WHERE org_id = $1 AND status = 'pending'
  AND (email_verified_at IS NULL AND code_hash IS NOT NULL) = sqlc.arg(awaiting)::boolean;

-- name: SignupEmailIsUser :one
-- scope: none
SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower(sqlc.arg(email)::text)) AS is_user;

-- name: SignupUsernameTaken :one
-- scope: none
SELECT EXISTS (SELECT 1 FROM users WHERE username = $1) AS taken;

-- name: SignupEmailBlocked :one
-- scope: system
SELECT EXISTS (SELECT 1 FROM signup_requests WHERE org_id = $1 AND email = $2 AND blocked) AS blocked;

-- name: GetPendingSignupByEmailForUpdate :one
-- scope: system
-- code_live: the row holds a code that has not expired (database clock).
SELECT id, email_verified_at, code_salt, code_hash, code_attempts, code_attempts_total,
       (code_expires_at IS NOT NULL AND code_expires_at > now())::boolean AS code_live
FROM signup_requests
WHERE org_id = $1 AND email = $2 AND status = 'pending'
FOR UPDATE;

-- name: InsertSignupRequest :one
-- scope: system
-- ON CONFLICT on the one-pending index: a second request for an address
-- that has a pending one inserts nothing (no row returned).
INSERT INTO signup_requests (org_id, email, name, username, note, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => sqlc.arg(ttl_seconds)::double precision))
ON CONFLICT (org_id, email) WHERE status = 'pending' DO NOTHING
RETURNING *;

-- name: SetSignupCode :exec
-- scope: system
-- A new confirm code replaces the open one. code_attempts (this code's
-- wrong tries) starts again; code_attempts_total does not. An unconfirmed
-- request lives at most unconfirmed_ttl from now (never longer than it
-- had), so asking again does not keep it alive.
UPDATE signup_requests
SET code_salt = $2, code_hash = $3, code_attempts = 0,
    code_expires_at = now() + make_interval(secs => sqlc.arg(ttl_seconds)::double precision),
    expires_at = least(expires_at, now() + make_interval(secs => sqlc.arg(unconfirmed_ttl_seconds)::double precision))
WHERE id = $1 AND status = 'pending' AND email_verified_at IS NULL;

-- name: RecordSignupCodeAttempt :exec
-- scope: system
UPDATE signup_requests
SET code_attempts = code_attempts + 1, code_attempts_total = code_attempts_total + 1
WHERE id = $1;

-- name: ConfirmSignupEmail :execrows
-- scope: system
-- A confirmed request joins the admin queue and lives pending_ttl from
-- when it was made.
UPDATE signup_requests
SET email_verified_at = now(), code_salt = NULL, code_hash = NULL, code_expires_at = NULL, code_attempts = 0,
    expires_at = greatest(now() + interval '1 minute', created_at + make_interval(secs => sqlc.arg(pending_ttl_seconds)::double precision))
WHERE id = $1 AND status = 'pending';

-- name: ListSignupRequests :many
-- scope: admin
-- hide_unverified: leave out pending requests that wait for their confirm
-- code (unverified, with a code): while mail works, such a request shows
-- only after the proof. A request made while mail was off has no code and
-- stays visible.
SELECT * FROM signup_requests
WHERE org_id = $1 AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND NOT (sqlc.arg(hide_unverified)::boolean AND status = 'pending' AND email_verified_at IS NULL AND code_hash IS NOT NULL)
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: GetSignupRequestForUpdate :one
-- scope: admin
SELECT * FROM signup_requests WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: ApproveSignupRequest :one
-- scope: admin
UPDATE signup_requests
SET status = 'approved', decided_at = now(), decided_by = $2, user_id = $3,
    code_salt = NULL, code_hash = NULL, code_expires_at = NULL
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: RejectSignupRequest :one
-- scope: admin
-- Also on a rejected row, to add the block later. A row stays rejected.
UPDATE signup_requests
SET status = 'rejected', decided_at = coalesce(decided_at, now()), decided_by = coalesce(decided_by, sqlc.narg(decided_by)),
    reject_reason = CASE WHEN status = 'pending' THEN sqlc.arg(reason)::text ELSE reject_reason END,
    blocked = blocked OR sqlc.arg(block)::boolean,
    code_salt = NULL, code_hash = NULL, code_expires_at = NULL
WHERE id = $1 AND status IN ('pending', 'rejected')
RETURNING *;

-- name: GetUserByEmail :one
-- scope: none
SELECT * FROM users WHERE lower(email) = lower(sqlc.arg(email)::text);

-- name: IsOrgMemberByEmail :one
-- scope: system
SELECT EXISTS (
    SELECT 1 FROM org_members m JOIN users u ON u.id = m.user_id
    WHERE m.org_id = $1 AND lower(u.email) = lower(sqlc.arg(email)::text)) AS member;

-- name: CloseExpiredOpenInvites :exec
-- scope: system
-- An expired open invite is closed when a new one for the same email is
-- made, so org_invites_one_open lets the new one in.
UPDATE org_invites SET revoked_at = now()
WHERE org_id = $1 AND email = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= now();

-- name: InsertOrgInvite :one
-- scope: admin
INSERT INTO org_invites (org_id, email, role, token_hash, created_by, emailed, team_access, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.arg(team_access), now() + make_interval(secs => sqlc.arg(ttl_seconds)::double precision))
ON CONFLICT (org_id, email) WHERE accepted_at IS NULL AND revoked_at IS NULL DO NOTHING
RETURNING *;

-- name: InsertOrgInviteTeams :execrows
-- scope: admin
-- The teams of a 'selected' invite (decisions-T20 #26). The composite FK
-- refuses a team of another org.
INSERT INTO org_invite_teams (org_id, invite_id, team_id)
SELECT sqlc.arg('org_id'), sqlc.arg('invite_id'), t
FROM unnest(sqlc.arg('team_ids')::uuid[]) AS t
ON CONFLICT DO NOTHING;

-- name: ListOrgInviteTeamKeys :many
-- scope: admin
-- The team keys of the invites listed to an org admin (#26), by key.
SELECT it.invite_id, t.key
FROM org_invite_teams it
JOIN teams t ON t.org_id = it.org_id AND t.id = it.team_id
WHERE it.org_id = sqlc.arg('org_id') AND it.invite_id = ANY(sqlc.arg('invite_ids')::uuid[])
ORDER BY t.key;

-- name: ListOrgInvites :many
-- scope: admin
SELECT * FROM org_invites WHERE org_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: GetOrgInvite :one
-- scope: system
SELECT * FROM org_invites WHERE org_id = $1 AND id = $2;

-- name: RevokeOrgInvite :one
-- scope: admin
UPDATE org_invites SET revoked_at = now(), revoked_by = $3
WHERE org_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL
RETURNING *;

-- name: ConsumeOrgInvite :one
-- scope: system
-- The race arbiter: of two accepts with one token, one gets the row and the
-- other none (the second waits on the row lock, then sees accepted_at).
UPDATE org_invites SET accepted_at = now()
WHERE token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING *;

-- name: GetInviteAccessByHash :one
-- scope: system
-- The accept reads which org and which access a token names before it uses
-- the invite up, so a 'selected' accept can take the webhook guard's lock
-- first (decisions-T20 #65, entry 2). It says nothing to the caller: an
-- unknown token and a used one get the same answer later.
SELECT org_id, team_access FROM org_invites WHERE token_hash = $1;

-- name: LockInviteTeamsForAccept :many
-- scope: system
-- The teams of a 'selected' invite that still exist, locked against a
-- concurrent team delete for the rest of the accept (#27). A team deleted
-- before is gone by the cascade; one deleted while this waits is skipped.
SELECT t.id
FROM org_invite_teams it
JOIN teams t ON t.org_id = it.org_id AND t.id = it.team_id
WHERE it.org_id = sqlc.arg('org_id') AND it.invite_id = sqlc.arg('invite_id')
ORDER BY t.id
FOR KEY SHARE OF t;

-- name: SetInvitedMemberSelected :exec
-- scope: system
-- The accept of a 'selected' invite makes the new membership restricted
-- (#27), in the transaction that inserted it.
UPDATE org_members SET team_access = 'selected'
WHERE org_id = sqlc.arg('org_id') AND user_id = sqlc.arg('user_id');

-- name: InsertInvitedMemberTeams :execrows
-- scope: system
INSERT INTO team_members (org_id, user_id, team_id)
SELECT sqlc.arg('org_id'), sqlc.arg('user_id'), t
FROM unnest(sqlc.arg('team_ids')::uuid[]) AS t
ON CONFLICT DO NOTHING;

-- name: SetInviteAcceptedBy :exec
-- scope: system
UPDATE org_invites SET accepted_by = $2 WHERE id = $1;

-- name: InsertInvitedUser :one
-- scope: none
-- A user made by accepting an invite or by approving a sign-up request.
-- email_unproven: nobody proved the email (a copied invite link, or a
-- request approved while mail was off; decisions-T12 #21).
INSERT INTO users (username, name, email, is_agent, email_unproven) VALUES ($1, $2, $3, false, $4)
ON CONFLICT (username) DO NOTHING
RETURNING *;

-- name: PurgeUnconfirmedSignups :execrows
-- scope: admin
-- DELETE /orgs/{org}/signups/unconfirmed: drop every request that waits
-- for its confirm code (decisions-T12 #10). They hold no decision and no
-- proof, so nothing of value is lost.
DELETE FROM signup_requests
WHERE org_id = $1 AND status = 'pending' AND email_verified_at IS NULL AND code_hash IS NOT NULL;

-- name: GetOrgByID :one
-- scope: none
SELECT * FROM orgs WHERE id = $1;

-- name: InsertOrgMemberRole :execrows
-- scope: system
INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id) DO NOTHING;

-- name: LockPlatformAdmins :exec
-- scope: none
-- Serialises removals, so "never remove the last platform admin" holds
-- with two removals at once.
SELECT pg_advisory_xact_lock(hashtext('launchpad-platform-admins'));

-- name: ListPlatformAdmins :many
-- scope: none
SELECT id, username, name, email, deactivated_at FROM users
WHERE is_platform_admin ORDER BY username;

-- name: CountActivePlatformAdmins :one
-- scope: none
SELECT count(*) FROM users WHERE is_platform_admin AND NOT is_agent AND deactivated_at IS NULL;

-- name: FindUserByUsernameOrEmail :one
-- scope: none
SELECT * FROM users
WHERE username = lower(sqlc.arg(ident)::text) OR lower(email) = lower(sqlc.arg(ident)::text)
ORDER BY (username = lower(sqlc.arg(ident)::text)) DESC
LIMIT 1;

-- name: GetUserByIDForUpdate :one
-- scope: none
SELECT * FROM users WHERE id = $1 FOR UPDATE;

-- name: UnsetPlatformAdmin :execrows
-- scope: none
UPDATE users SET is_platform_admin = false, updated_at = now() WHERE id = $1 AND is_platform_admin;
