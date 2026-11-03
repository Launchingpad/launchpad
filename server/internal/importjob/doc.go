// Package importjob runs the Linear import as a background job inside the API
// process (LAUN-8, docs/internal/decisions-T17-git-connections-import.md #36
// to #45).
//
// One Manager owns the import sessions of an install and their jobs (export,
// dry run, import, verify). A job is a goroutine that calls internal/importer
// (and, for the export, internal/importer/linear); it is not a queue. There is
// one active job per org: the partial unique index of migration 0013 is the
// guard across instances, and importer.Run's own advisory lock (ImportLockOrg)
// stays the guard between the CLI and the web.
//
// What a restart or a cancel does: importer.Run writes every row in ONE
// transaction, so a cancel or a restart during `import` rolls back all of it
// and the next run starts again (restartable, not resumable). The files it
// uploaded stay in the store, so the second run uploads nothing.
//
// The Linear API key arrives once, in the export request, lives in the export
// goroutine and in nothing else: not a row, a log line, an event, an error
// text or an idempotency record (#38).
package importjob
