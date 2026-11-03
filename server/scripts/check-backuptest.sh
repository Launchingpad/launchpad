#!/usr/bin/env bash
# Prove the `backuptest` build tag keeps test-only backup code out of the
# production binary (T16 #16, LAUN-70). Needs Go (CI has it; on the Mac run it
# through `make docker-test`). Checks:
#   1. `go build ./cmd/server` WITHOUT the tag has no reference to the local
#      driver or RegisterForTest (symbol table of the binary).
#   2. No untagged file uses a tag-only name: `go build ./...` without the tag
#      compiles.
#   3. The check itself works: a planted untagged file that uses the local
#      driver makes step 2 fail.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp="$(mktemp -d)"
plant=internal/backup/driver/zz_plant_check.go
cleanup() { rm -rf "$tmp"; rm -f "$plant"; }
trap cleanup EXIT

go build -o "$tmp/server" ./cmd/server
if go tool nm "$tmp/server" | grep -E 'backup/driver\.(Local|RegisterForTest|localRemote|parseLocal)' >/dev/null; then
  echo "check-backuptest: the production binary references the backuptest driver" >&2
  exit 1
fi
go build ./...

cat >"$plant" <<'GO'
package driver

var _ = Local{}
GO
if go build ./internal/backup/driver/ 2>/dev/null; then
  echo "check-backuptest: self-test failed: an untagged file using the local driver still builds" >&2
  exit 1
fi
rm -f "$plant"
echo "check-backuptest: ok"
