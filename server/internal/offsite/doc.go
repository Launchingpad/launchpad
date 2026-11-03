// Package offsite is the worker behind `server offsite` (LAUN-71,
// docs/internal/decisions-T16-backups.md, "Build notes: ticket 4"). It is the
// only process that dials a user-entered backup destination (#10). It claims
// the rows the API queues (backup_runs), makes the encrypted, signed copy of
// the newest local dump, uploads it through the driver registry, prunes by
// retention and writes the result back to the run row.
//
// It talks to Postgres as a restricted role (#15): it deletes no row, reads no
// issue, user or platform setting, and never uses INSERT ... RETURNING on
// backup_change_log. It holds BACKUP_ENCRYPTION_KEY and never the mail key.
//
// Rules this package keeps (its tests pin them):
//   - A secret is opened in memory, handed to a driver and dropped. It is never
//     logged, put in an error, a step or a run column. An error stored on a run
//     is a driver.Error (a code, maybe a status) or one fixed sentence.
//   - Slots, leases, retry times and stale ages use the DATABASE clock.
//   - A run is claimed in one statement; a run that was cancelled or
//     interrupted meanwhile is never overwritten by its worker.
//   - The scratch file of a run is deleted when the run ends and at boot.
package offsite
