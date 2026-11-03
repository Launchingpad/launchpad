package importjob

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Sweep is one pass of the housekeeping loop (decisions-T17 #36, #39):
//
//  1. a running job whose heartbeat is older than StaleAfter is marked failed
//     with code interrupted, and an export, dry-run or import job makes its
//     session failed in the SAME transaction (a verify leaves it, #43);
//  2. an open session with no active job that has been idle for SessionTTL
//     is closed and its directory removed;
//  3. an orphaned server-made session directory is removed.
func (m *Manager) Sweep(ctx context.Context) error {
	var errs []error
	if err := m.sweepStale(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.sweepSessions(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := m.sweepOrphans(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) sweepStale(ctx context.Context) error {
	text := errorTexts[CodeInterrupted]
	var swept []dbq.ImportJobsSweepStaleRow
	err := database.InTx(ctx, m.cfg.Pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		rows, err := q.ImportJobsSweepStale(ctx, dbq.ImportJobsSweepStaleParams{ErrorText: &text, StaleSecs: m.cfg.StaleAfter.Seconds()})
		if err != nil {
			return fmt.Errorf("sweep stale jobs: %w", err)
		}
		swept = rows
		for _, r := range rows {
			s, err := q.ImportSessionLock(ctx, dbq.ImportSessionLockParams{OrgID: r.OrgID, ID: r.SessionID})
			if err != nil {
				return fmt.Errorf("lock session: %w", err)
			}
			if next, ok := nextState(r.Kind, endFailed, s.State); ok {
				if err := q.ImportSessionSetState(ctx, dbq.ImportSessionSetStateParams{OrgID: s.OrgID, ID: s.ID, State: next}); err != nil {
					return fmt.Errorf("fail session: %w", err)
				}
			}
		}
		return nil
	})
	for _, r := range swept {
		if err == nil {
			m.log.WarnContext(ctx, "import: a job lost its owner and is marked interrupted", "job", r.ID, "kind", r.Kind)
		}
	}
	return err
}

func (m *Manager) sweepSessions(ctx context.Context) error {
	old, err := m.q.ImportSessionsExpired(ctx, m.cfg.Now().Add(-m.cfg.SessionTTL))
	if err != nil {
		return fmt.Errorf("expired sessions: %w", err)
	}
	var errs []error
	for _, s := range old {
		_, err := m.closeSession(ctx, s.OrgID, s.ID)
		var he *httpx.Error
		switch {
		case err == nil:
			m.removeDir(s.ExportDir)
			m.log.InfoContext(ctx, "import: an idle session was closed", "session", s.ID)
		case errors.As(err, &he): // a job started, or it closed: leave it
		default:
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
