package backupapi

import (
	"context"
	"fmt"
	"time"

	"devops/internal/backupnotice"
	"devops/internal/dbq"
)

// addNotices fills the LAUN-75 part of the status (decisions-T16 #41): the stale
// destinations by the stale rule, the destinations whose newest backup run failed
// for good, and the mail state. now is the status clock (the database's, or the
// injected one), so the page and the stale mail use one rule and one clock.
func (s *Service) addNotices(ctx context.Context, q *dbq.Queries, out *Status, now time.Time) error {
	if out.StaleAlertHours > 0 {
		rows, err := q.ListBackupStaleInputs(ctx)
		if err != nil {
			return fmt.Errorf("list the stale inputs: %w", err)
		}
		for _, r := range rows {
			in := backupnotice.StaleInputOf(r)
			stale, since, th := backupnotice.Stale(in, out.StaleAlertHours, now)
			if !stale {
				continue
			}
			out.StaleDestinations = append(out.StaleDestinations, StaleDestination{ID: r.ID, Name: r.Name,
				LastSuccessAt: in.LastSuccess, Since: since, ThresholdHours: int(th.Hours()), LastAlertAt: r.LastStaleAlertAt})
		}
	}
	last, err := q.ListBackupDestinationLastRuns(ctx)
	if err != nil {
		return fmt.Errorf("list the last runs: %w", err)
	}
	enabled := map[string]bool{}
	dests, err := q.ListBackupDestinations(ctx)
	if err != nil {
		return fmt.Errorf("list destinations: %w", err)
	}
	for _, d := range dests {
		if d.Enabled {
			enabled[d.ID.String()] = true
		}
	}
	for _, r := range last {
		if r.DestinationID == nil || !enabled[r.DestinationID.String()] || r.Status != "failed" || r.WillRetry || r.FinishedAt == nil {
			continue
		}
		out.FailedDestinations = append(out.FailedDestinations, FailedDestination{ID: *r.DestinationID, Name: r.DestinationName,
			RunID: r.ID, FinishedAt: *r.FinishedAt, ErrorCode: r.ErrorCode, ErrorText: r.ErrorText})
	}
	out.FilesNotBackedUp = []FilesDestination{}
	if s.filesStored != nil && s.filesStored() {
		out.FilesStored = true
		okRows, err := q.ListBackupFilesLastOk(ctx)
		if err != nil {
			return fmt.Errorf("list the files backups: %w", err)
		}
		lastOk := map[string]time.Time{}
		for _, r := range okRows {
			if r.DestinationID != nil && r.FinishedAt != nil {
				lastOk[r.DestinationID.String()] = *r.FinishedAt
			}
		}
		for _, d := range dests {
			if !d.Enabled {
				continue
			}
			at, ok := lastOk[d.ID.String()]
			if ok && now.Sub(at) <= FilesStaleHours*time.Hour {
				continue
			}
			fd := FilesDestination{ID: d.ID, Name: d.Name}
			if ok {
				t := at
				fd.LastBackupAt = &t
			}
			out.FilesNotBackedUp = append(out.FilesNotBackedUp, fd)
		}
		if len(out.FilesNotBackedUp) > 0 {
			out.Banners = append(out.Banners, BannerFilesNotBackedUp)
		}
	}
	if len(out.StaleDestinations) > 0 {
		out.Banners = append(out.Banners, BannerStale)
	}
	if len(out.FailedDestinations) > 0 {
		out.Banners = append(out.Banners, BannerRunFailed)
	}
	if !out.MailReady && out.EnabledCount > 0 {
		out.Banners = append(out.Banners, BannerMailOff)
	}
	return nil
}
