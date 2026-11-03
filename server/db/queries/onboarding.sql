-- Onboarding (LAUN-56, docs/internal/decisions-T17-onboarding.md). Every query is
-- scoped by org_id.

-- name: OnboardingFacts :one
-- scope: admin
-- The done flags of the org steps, in one statement. A live agent token is a
-- PAT of an active agent member that is not revoked, not expired, and pinned to
-- this org or to none.
SELECT
    EXISTS (SELECT 1 FROM teams t WHERE t.org_id = sqlc.arg(org_id) AND t.cycles_enabled) AS cycles_on,
    EXISTS (
        SELECT 1 FROM api_tokens k
        JOIN org_members m ON m.user_id = k.user_id AND m.org_id = sqlc.arg(org_id)
        JOIN users u ON u.id = k.user_id
        WHERE k.kind = 'pat' AND k.revoked_at IS NULL
          AND (k.expires_at IS NULL OR k.expires_at > now())
          AND (k.org_id IS NULL OR k.org_id = sqlc.arg(org_id))
          AND u.is_agent AND u.deactivated_at IS NULL) AS agent_token,
    (SELECT count(*) FROM org_members m JOIN users u ON u.id = m.user_id
        WHERE m.org_id = sqlc.arg(org_id) AND NOT u.is_agent AND u.deactivated_at IS NULL)::bigint AS humans,
    EXISTS (SELECT 1 FROM git_integrations g WHERE g.org_id = sqlc.arg(org_id) AND g.enabled) AS git_host,
    EXISTS (SELECT 1 FROM import_mappings im WHERE im.org_id = sqlc.arg(org_id)) AS imported,
    (SELECT count(*) FROM teams t WHERE t.org_id = sqlc.arg(org_id))::bigint AS teams,
    EXISTS (SELECT 1 FROM products p WHERE p.org_id = sqlc.arg(org_id)) AS has_product,
    COALESCE((SELECT signup_mode_chosen_at IS NOT NULL FROM platform_settings WHERE id), false)::boolean AS signup_chosen;

-- name: ListOnboardingSkips :many
-- scope: admin
SELECT s.step, s.created_at, s.skipped_by, u.username AS skipped_by_username, u.name AS skipped_by_name
FROM onboarding_skips s
LEFT JOIN users u ON u.id = s.skipped_by
WHERE s.org_id = $1;

-- name: InsertOnboardingSkip :execrows
-- scope: admin
INSERT INTO onboarding_skips (org_id, step, skipped_by) VALUES ($1, $2, $3)
ON CONFLICT (org_id, step) DO NOTHING;

-- name: DeleteOnboardingSkip :execrows
-- scope: admin
DELETE FROM onboarding_skips WHERE org_id = $1 AND step = $2;
