package tracker

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/httpx"
)

// The webhook guard (decisions-T20 #65, open point M). A git webhook acts
// with system authority: it links and moves the issues a change names. A
// repository map row with a team matches that team only; a row with no team
// (team_id NULL) matches every team of the org. So while any member has
// restricted team access, every repository of an enabled integration must
// name a team; otherwise its pushers could link and move issues of every
// team, those a restricted member cannot see included.
//
// One rule, checked by one helper in the transaction of every write that
// could break it:
//
//  1. the team access setter (ticket 9), when it makes or keeps a member
//     with team_access 'selected';
//  2. an invite or its accept with selected access (ticket 10);
//  3. a sign-up approval with selected access (ticket 10);
//  4. an agent created with selected access (ticket 9);
//  5. an integration created or enabled (CreateGitIntegration,
//     UpdateGitIntegration);
//  6. a repository mapped, or its team cleared, on an enabled integration
//     (insertGitRepo, UpdateGitRepo);
//  7. a team deleted (DeleteTeam): git_repo_map_team_fk is ON DELETE SET
//     NULL, so the team's repositories lose their team.
//
// Each entry calls LockTeamAccessGuard first in its transaction, makes its
// write, then CheckWebhookGuard before it emits. The lock is one advisory
// lock per org, so two such writes run one after the other and the second
// sees the first.

// maxGuardRepos is how many repositories a refusal names.
const maxGuardRepos = 20

// LockTeamAccessGuard takes the org's team access lock for the rest of the
// transaction q runs in. Every entry of the guard takes it first.
func LockTeamAccessGuard(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) error {
	if err := q.LockTeamAccessGuard(ctx, orgID); err != nil {
		return fmt.Errorf("team access lock: %w", err)
	}
	return nil
}

// CheckWebhookGuard refuses (409 conflict, naming the repositories) when the
// org has restricted team access and an enabled integration has a repository
// with no team. Call it in the transaction of the write, after the write: it
// takes the lock again (a no-op when the caller holds it) and reads the state
// the write leaves.
func CheckWebhookGuard(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) error {
	return checkWebhookGuard(ctx, q, orgID, true)
}

// CheckWebhookGuardNameless is CheckWebhookGuard with a refusal that names no
// repository, for a caller who is not an org admin (the accept of an invite,
// ticket 10): repository paths are admin data.
func CheckWebhookGuardNameless(ctx context.Context, q *dbq.Queries, orgID uuid.UUID) error {
	return checkWebhookGuard(ctx, q, orgID, false)
}

func checkWebhookGuard(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, names bool) error {
	if err := LockTeamAccessGuard(ctx, q, orgID); err != nil {
		return err
	}
	restricted, err := q.RestrictedAccessExists(ctx, orgID)
	if err != nil {
		return fmt.Errorf("restricted access: %w", err)
	}
	if !restricted {
		return nil
	}
	repos, err := q.ListEnabledReposWithoutTeam(ctx, orgID)
	if err != nil {
		return fmt.Errorf("repositories without a team: %w", err)
	}
	if len(repos) == 0 {
		return nil
	}
	if !names {
		return httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
			"this cannot be done now: an organisation admin must first give a team to every connected repository")
	}
	more := ""
	if len(repos) > maxGuardRepos {
		repos, more = repos[:maxGuardRepos], ", ..."
	}
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
		"some members see only selected teams, so every repository of an enabled integration needs a team; these have none: %s%s",
		strings.Join(repos, ", "), more)
}
