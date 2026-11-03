package tracker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
)

// DeleteIssue removes an issue for good (LAUN-201, decisions-T32-delete-issues.md).
// Its comments, attachments, relations, activity, label links, state changes,
// subscriptions, git links and notifications go by their foreign keys; its child
// issues keep their row with no parent. events.Emit is the last statement of the
// transaction and the payload holds ids and the identifier only. The attachment
// objects are removed after the commit, best effort.
//
// Lock order is the one of UpdateIssue: the parent lock, the rank lock, then the
// issue row.
func (s *Service) DeleteIssue(ctx context.Context, org *auth.Org, p *auth.Principal, ref string) error {
	scopeAll, scopeIDs := org.Access.Args()
	var keys []string
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		keys = nil
		q := s.q.WithTx(tx)
		id, err := resolveIssueID(ctx, q, org, ref)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:issue-parents:' || $1::text, 0))`, org.ID); err != nil {
			return fmt.Errorf("parent lock: %w", err)
		}
		var teamID uuid.UUID
		if err := tx.QueryRow(ctx, `/* scope: admin */ SELECT team_id FROM issues WHERE org_id = $1 AND id = $2`, org.ID, id).Scan(&teamID); err != nil {
			return notFound(err, "issue")
		}
		if err := rankLock(ctx, tx, teamID); err != nil {
			return err
		}
		cur, err := q.LockIssue(ctx, dbq.LockIssueParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "issue")
		}
		team, err := q.GetTeamByID(ctx, dbq.GetTeamByIDParams{OrgID: org.ID, ID: cur.TeamID, AllTeams: scopeAll, TeamIds: scopeIDs})
		if err != nil {
			return fmt.Errorf("issue team: %w", err)
		}
		ks, err := q.ListIssueObjectKeys(ctx, dbq.ListIssueObjectKeysParams{OrgID: org.ID, IssueID: id})
		if err != nil {
			return fmt.Errorf("list attachment objects: %w", err)
		}
		for _, k := range ks {
			if k != nil {
				keys = append(keys, *k)
			}
		}
		if err := q.DeleteIssueImportMapping(ctx, dbq.DeleteIssueImportMappingParams{OrgID: org.ID, TargetID: id}); err != nil {
			return fmt.Errorf("delete import mapping: %w", err)
		}
		// Other issues' history keeps a line that names this one (a parent change, a removed
		// relation). The title goes; the id and the identifier stay (decisions-T32 #4).
		if _, err := q.RedactIssueSnapshots(ctx, dbq.RedactIssueSnapshotsParams{OrgID: org.ID, ID: id.String()}); err != nil {
			return fmt.Errorf("redact activity snapshots: %w", err)
		}
		n, err := q.DeleteIssue(ctx, dbq.DeleteIssueParams{OrgID: org.ID, ID: id})
		if err != nil {
			return fmt.Errorf("delete issue: %w", err)
		}
		if n != 1 {
			return notFound(pgx.ErrNoRows, "issue")
		}
		_, err = events.Emit(ctx, q, org.ID, events.Event{Type: events.IssueDeleted, EntityType: "issue", EntityID: id, ActorID: &p.UserID,
			// No parentId and no child count: the parent and the children may be
			// in a team a reader of this event cannot see (T20 #42, LAUN-221).
			Payload: map[string]any{"id": id, "identifier": Identifier(team.Key, cur.Number), "teamId": team.ID}},
			events.ScopeTeams(team.ID))
		return err
	})
	if err != nil {
		return err
	}
	if s.store != nil {
		for _, k := range keys {
			if err := s.store.Delete(context.WithoutCancel(ctx), k); err != nil {
				slog.Error("issue delete: attachment object removal failed", "org_id", org.ID, "code", "attachment_delete_failed")
			}
		}
	}
	return nil
}
