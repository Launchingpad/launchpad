// Package migrations embeds the numbered SQL migrations. golang-migrate reads
// them through iofs at boot (internal/database.Migrate).
//
// Rules (AGENTS.md): every database change is a new NNNN_name.up.sql with a
// matching .down.sql; numbers are contiguous; a shipped file is never edited.
package migrations

import "embed"

// FS holds every *.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS
