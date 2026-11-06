-- Agent users (decisions-T10, decisions-T47): the delete.

-- name: CountOtherOrgMemberships :one
-- scope: admin
-- Memberships of the user in orgs other than $1. An agent made by POST
-- /agents has none; a user with some is not deleted here (decisions-T47 #4).
SELECT count(*) FROM org_members WHERE org_id <> $1 AND user_id = $2;

-- name: DeleteAgentUser :execrows
-- scope: admin
-- Hard delete of an agent user. The foreign keys do the rest: tokens,
-- memberships and every row keyed by the membership go (CASCADE); authors,
-- assignees, creators, leads and actors become NULL (SET NULL). The caller
-- has checked that the user is a member of its org.
DELETE FROM users WHERE id = $1 AND is_agent;
