// Package move exports a whole install into ONE encrypted bundle and imports
// that bundle into an empty install (LAUN-248).
//
// The bundle is an age stream encrypted to a passphrase (scrypt). The
// passphrase is made by the server (GeneratePassphrase), never chosen by a
// person, and is never logged, emitted or stored. Inside the stream is a
// sequence of frames (frames.go): the manifest first, then the table data
// (PostgreSQL COPY text, one repeatable-read snapshot), then the attachment
// objects, each followed by its sha256. Nothing touches a temp file: the app
// root file system is read-only, so everything streams.
//
// Sealed columns (the *_enc secrets) travel as PLAINTEXT inside the
// passphrase-encrypted stream: export opens each with the exporting server's
// keyrings, import seals again with the importing server's keyrings. The
// registry (registry.go) lists every table and every sealed column; a drift
// test fails when the schema has one the registry does not know.
//
// Import runs in ONE transaction under the setup advisory lock, only into a
// database with no user, and only when the bundle's migration version equals
// the database's. Any failure rolls back and deletes the objects it wrote, so
// the install stays in setup state. This package is install-level: it emits
// no event, and it never imports internal/notify.
package move
