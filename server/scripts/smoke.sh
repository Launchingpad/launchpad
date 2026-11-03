#!/usr/bin/env bash
# curl smoke test against a running stack. It creates, lists, filters and
# updates issues with a token, and checks the error paths the ACs name.
#
#   BASE_URL=http://localhost:8090 TOKEN=odt_... scripts/smoke.sh
#   (scratch: SMOKE_TMPDIR, default: a new folder under the system temp dir)
#
# TOKEN must be an org-admin token for ORG (default orbit), e.g. the first
# admin token that first-run setup returns (scripts/dev-setup.sh prints it). Every step prints its status; the script
# exits non-zero at the first unexpected one. Needs curl and python3.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8090}"
ORG="${ORG:-orbit}"
TEAM="${TEAM:-ORB}"
: "${TOKEN:?set TOKEN to an org-admin token (the first admin token from first-run setup)}"
API="$BASE_URL/api/v1/orgs/$ORG"

# Scratch goes in SMOKE_TMPDIR, else a folder made by mktemp (it follows
# TMPDIR). The body file briefly holds a minted token secret.
made_scratch=0
if [[ -z "${SMOKE_TMPDIR:-}" ]]; then
  SMOKE_TMPDIR="$(mktemp -d)"
  made_scratch=1
fi
scratch="$SMOKE_TMPDIR"
if [[ ! -d "$scratch" || ! -w "$scratch" ]]; then
  echo "smoke.sh: scratch dir $scratch is missing or not writable; set SMOKE_TMPDIR" >&2
  exit 2
fi
body_file="$(mktemp "$scratch/devops-smoke.XXXXXX")"
trap 'rm -f "$body_file"; ((made_scratch)) && rmdir "$scratch" 2>/dev/null || true' EXIT
echo "scratch file: $body_file"

# call METHOD URL EXPECTED_STATUS [JSON_BODY] [extra curl args...]
call() {
  local method="$1" url="$2" want="$3" data="${4:-}"
  shift 4 || shift $#
  local args=(-sS -o "$body_file" -w '%{http_code}' -X "$method" -H "Authorization: Bearer $AUTH")
  if [[ -n "$data" ]]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  local got
  got="$(curl "${args[@]}" "$@" "$url")"
  local shown="${url#$BASE_URL}"
  printf '%-6s %-70s -> %s (want %s)\n' "$method" "$shown" "$got" "$want"
  if [[ "$got" != "$want" ]]; then
    echo "  body: $(cat "$body_file")" >&2
    exit 1
  fi
}

# field EXPR: evaluate a Python expression over the last JSON body (as b).
field() {
  local expr="$1"
  python3 -c "import json,sys; b=json.load(open(sys.argv[1])); print(${expr})" "$body_file"
}

AUTH="$TOKEN"
echo "== health and identity"
call GET "$BASE_URL/healthz" 200
call GET "$BASE_URL/api/v1/me" 200
echo "   me: $(field 'b["username"]')"

echo "== create (with Idempotency-Key, sent twice)"
key="smoke-$(date +%s)-$$"
payload="{\"team\":\"$TEAM\",\"title\":\"Smoke test $(date -u +%FT%TZ)\",\"labels\":[\"Bug\"],\"priority\":2,\"assignee\":\"me\"}"
call POST "$API/issues" 201 "$payload" -H "Idempotency-Key: $key"
ident="$(field 'b["identifier"]')"
first_id="$(field 'b["id"]')"
echo "   created $ident ($(field 'b["gitBranchName"]'))"
call POST "$API/issues" 201 "$payload" -H "Idempotency-Key: $key"
[[ "$(field 'b["id"]')" == "$first_id" ]] || { echo "replay made a second issue" >&2; exit 1; }
echo "   replay returned the same issue"

echo "== read and filter"
call GET "$API/issues/$ident" 200
call GET "$API/issues?team=$TEAM&state=Backlog&label=Bug&assignee=me&limit=5" 200
echo "   filter matched: $(field '[i["identifier"] for i in b["items"]]')"
python3 -c "import json,sys; b=json.load(open(sys.argv[1])); sys.exit(0 if any(i['identifier']==sys.argv[2] for i in b['items']) else 1)" "$body_file" "$ident" \
  || { echo "new issue not in the filtered list" >&2; exit 1; }
call GET "$API/issues?createdAt=-P1D&orderBy=createdAt&limit=1" 200
echo "   newest created: $(field 'b["items"][0]["identifier"]'), nextCursor set: $(field 'b["nextCursor"] is not None')"

echo "== update (version compare-and-swap) and state history"
call PATCH "$API/issues/$ident" 200 '{"version":1,"state":"In Progress","priority":1}'
echo "   now: $(field 'b["state"]["name"]'), version $(field 'b["version"]')"
call PATCH "$API/issues/$ident" 409 '{"version":1,"title":"stale write"}'
echo "   stale version: $(field 'b["code"]')"
call GET "$API/issues/$ident?include=stateHistory" 200
echo "   history: $(field '[h["toStateName"] for h in b["stateHistory"]]')"
call GET "$API/events?after=0&limit=500" 200
echo "   events: $(field 'len(b["items"])') total, last: $(field 'b["items"][-1]["type"]')"

echo "== scopes and revocation"
call POST "$API/tokens" 201 '{"name":"smoke-read","scopes":["read"]}'
read_tok="$(field 'b["secret"]')"
read_id="$(field 'b["token"]["id"]')"
AUTH="$read_tok"
call GET "$API/issues/$ident" 200
call POST "$API/issues" 403 "{\"team\":\"$TEAM\",\"title\":\"should not exist\"}"
echo "   read token write: $(field 'b["code"]')"
AUTH="$TOKEN"
call DELETE "$API/tokens/$read_id" 204
AUTH="$read_tok"
call GET "$API/issues/$ident" 401
echo "   revoked token: $(field 'b["code"]')"
AUTH="odt_notarealtoken"
call GET "$API/issues" 401

echo "SMOKE OK"
