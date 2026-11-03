#!/usr/bin/env bash
# The off-site backup deploy pieces against a REAL postgres (decisions-T16
# #15, #28, #29). Needs only docker; starts nothing of any stack and publishes no port.
#
#   deploy/test-offsite.sh            (make deploy-test in server/)
#
# A throwaway postgres container (the production image), every server migration, then:
#
# A. deploy/backup/offsite-grants.sql
#    - without the password line it fails and makes no role;
#    - it applies twice without an error, and the role then holds EXACTLY the list of
#      decisions-T16 #15 (table, column, schema, database, function and sequence
#      privileges read from the catalog), is no superuser and has no membership;
#    - a grant added by hand (a table, a column, CREATE on the schema, a role) and a
#      changed attribute disappear at the next run; a dropped grant comes back;
#    - a database restored the way restore.sh restores (--no-owner --no-privileges) has
#      no grants, and the file gives them back;
#    - the role logs in with its password (and not with a wrong one) and can do what the
#      worker does, but cannot read users, issues or platform_settings, delete, or create.
# B. deploy/backup/backup.sh
#    - `once` records one backup_dumps row: ok, bytes, the sha256 of the file, the
#      migration version, mode 0600; a prune sets file_removed_at; a file that went
#      missing is marked; a stale `running` row and `taken` request are failed;
#    - a killed backup.sh leaves no `running` row after the next run;
#    - with the tables missing it still dumps;
#    - a queued request makes a dump (the time is printed), the request is done with its
#      dump_id, a request the app already failed is not flipped back, and a failed
#      pg_dump fails the request;
#    - hostile values (a request id, a dump row's file name, a file name in the
#      directory) reach no shell and no SQL string; the script has no `psql -c`.
# Scratch: DEPLOY_TMPDIR (default $TMPDIR, else /tmp).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
image="$(grep -m1 -E '^\s+image: postgres:' "$here/docker-compose.yml" | awk '{print $2}')"
[[ -n "$image" ]] || {
	echo "test-offsite: no postgres image in deploy/docker-compose.yml" >&2
	exit 2
}
c="launchpad-offsite-test-$$"
scratch="$(mktemp -d "${DEPLOY_TMPDIR:-${TMPDIR:-/tmp}}/launchpad-offsite-test.XXXXXX")"
cleanup() {
	docker rm -f "$c" >/dev/null 2>&1 || true
	rm -rf "$scratch"
}
trap cleanup EXIT
step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "test-offsite: FAIL: $*" >&2
	exit 1
}
ok() { echo "ok: $*"; }

# own SQL: as the owner role (devops), SQL on stdin.
own() { docker exec -i "$c" psql -X -q -t -A -v ON_ERROR_STOP=1 "$@"; }
# as_offsite SQL: as the offsite role over TCP, with its password.
as_offsite() {
	printf '%s\n' "$1" | docker exec -i -e PGPASSWORD="$pw" "$c" sh -c 'psql -X -q -t -A -v ON_ERROR_STOP=1 -h "$(hostname -i)" -U offsite -d "${DB:-devops}"'
}
# grants: the file, the way deploy.sh sends it.
grants() { # [database]
	{
		printf '\\set offsite_pw '"'"'%s'"'"'\n' "$pw"
		cat "$here/backup/offsite-grants.sql"
	} | docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 -U devops -d "${1:-devops}" >/dev/null
}
# The privileges of the role in a database, one line each, sorted.
privs() { # [database]
	own -d "${1:-devops}" <<'SQL'
SELECT x FROM (
  SELECT 'table ' || c.relname || ' ' || a.privilege_type AS x
    FROM pg_class c, aclexplode(c.relacl) a
   WHERE a.grantee = 'offsite'::regrole AND c.relnamespace = 'public'::regnamespace
  UNION ALL
  SELECT 'column ' || c.relname || '.' || t.attname || ' ' || a.privilege_type
    FROM pg_attribute t JOIN pg_class c ON c.oid = t.attrelid, aclexplode(t.attacl) a
   WHERE a.grantee = 'offsite'::regrole
  UNION ALL
  SELECT 'schema ' || n.nspname || ' ' || a.privilege_type
    FROM pg_namespace n, aclexplode(n.nspacl) a WHERE a.grantee = 'offsite'::regrole
  UNION ALL
  SELECT 'database ' || d.datname || ' ' || a.privilege_type
    FROM pg_database d, aclexplode(d.datacl) a WHERE a.grantee = 'offsite'::regrole AND d.datname = current_database()
  UNION ALL
  SELECT 'function ' || p.proname || ' ' || a.privilege_type
    FROM pg_proc p, aclexplode(p.proacl) a WHERE a.grantee = 'offsite'::regrole
  UNION ALL
  SELECT 'role member of ' || r.rolname
    FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid WHERE m.member = 'offsite'::regrole
) s ORDER BY x;
SQL
}
# The list of decisions-T16 #15 (and of TestRestrictedRole in server/internal/offsite).
expected_privs() {
	cat <<'EOF'
column backup_destinations.secrets_enc UPDATE
column backup_destinations.secrets_set UPDATE
column backup_settings.signing_private_enc UPDATE
column backup_settings.signing_public UPDATE
database devops CONNECT
schema public USAGE
table backup_change_log INSERT
table backup_destinations SELECT
table backup_dumps SELECT
table backup_requests INSERT
table backup_requests SELECT
table backup_runs INSERT
table backup_runs SELECT
table backup_runs UPDATE
table backup_settings SELECT
table schema_migrations SELECT
EOF
}
want_privs() { # [database] [why]
	local got
	got="$(privs "${1:-devops}")"
	if [[ "$got" != "$(expected_privs | sed "s/^database devops /database ${1:-devops} /")" ]]; then
		diff <(printf '%s\n' "$got") <(expected_privs | sed "s/^database devops /database ${1:-devops} /") >&2 || true
		fail "${2:-the privileges of the role are not the list of decisions-T16 #15}"
	fi
}

step "postgres $image (container $c, no published port)"
docker run -d --rm --name "$c" \
	-e POSTGRES_USER=devops -e POSTGRES_DB=devops -e POSTGRES_PASSWORD=offsite-test-only \
	-e PGHOST=127.0.0.1 -e PGUSER=devops -e PGPASSWORD=offsite-test-only -e PGDATABASE=devops \
	-e BACKUP_DIR=/backups -e BACKUP_KEEP_DAYS=30 \
	--tmpfs /backups:mode=1777 --tmpfs /tmp:mode=1777,exec \
	-v "$here/backup/backup.sh:/backup/backup.sh:ro" \
	"$image" >/dev/null
ready=0
for _ in $(seq 1 60); do
	if docker exec "$c" psql -h 127.0.0.1 -tAc 'select 1' >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 1
done
((ready)) || fail "postgres did not start"
sleep 2 # the init entrypoint restarts the server once
until docker exec "$c" psql -tAc 'select 1' >/dev/null 2>&1; do sleep 1; done

step "migrations"
version=0
for f in "$repo"/server/db/migrations/*.up.sql; do
	own <"$f" >/dev/null
	b="$(basename "$f")"
	num=$((10#${b%%_*}))
	((num > version)) && version=$num
done
own >/dev/null <<SQL
CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL);
INSERT INTO schema_migrations VALUES ($version, false);
SQL
echo "applied the migrations, schema_migrations version $version"
pw="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"

# ---------------------------------------------------------------- A. the grants
step "A1. without the password line the file fails and makes no role"
rc=0
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 -U devops -d devops <"$here/backup/offsite-grants.sql" >"$scratch/a1.out" 2>&1 || rc=$?
((rc != 0)) || fail "the file ran without offsite_pw"
grep -q 'offsite_pw is not set' "$scratch/a1.out" || fail "no message about offsite_pw"
[[ "$(own -c "SELECT count(*) FROM pg_roles WHERE rolname = 'offsite'")" == 0 ]] || fail "a role was made without the password line"
ok "exit $rc, no role"

step "A2. it applies twice and gives exactly the list"
grants
grants
want_privs devops
[[ "$(own -c "SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR NOT rolcanlogin FROM pg_roles WHERE rolname = 'offsite'")" == f ]] ||
	fail "the role has a forbidden attribute or cannot log in"
if grep -q "$pw" "$scratch/a1.out"; then fail "the password is in the output"; fi
ok "applied twice, 16 privileges, no more, no superuser/createdb/createrole/replication/bypassrls"

step "A3. drift: grants and attributes added by hand disappear, a dropped grant comes back"
own >/dev/null <<'SQL'
GRANT SELECT ON users TO offsite;
GRANT SELECT, DELETE ON backup_runs TO offsite;
GRANT UPDATE (name) ON backup_destinations TO offsite;
GRANT ALL ON SCHEMA public TO offsite;
GRANT TEMPORARY ON DATABASE devops TO offsite;
GRANT SELECT ON backup_change_log TO offsite;
CREATE ROLE offsite_other NOLOGIN;
GRANT offsite_other TO offsite;
ALTER ROLE offsite CREATEDB;
REVOKE SELECT ON backup_dumps FROM offsite;
SQL
if [[ "$(privs devops)" == "$(expected_privs)" ]]; then fail "A3: the setup did not change the privileges"; fi
grants
want_privs devops "A3: the drift was not undone"
[[ "$(own -c "SELECT rolcreatedb FROM pg_roles WHERE rolname = 'offsite'")" == f ]] || fail "A3: CREATEDB stayed"
own -c 'DROP ROLE offsite_other' >/dev/null
ok "extra table, column, schema, database and role rights are gone; the dropped SELECT on backup_dumps is back"

step "A4. a database restored with --no-owner --no-privileges has no grants, and the file gives them back"
own -d postgres -c 'CREATE DATABASE restored' >/dev/null
docker exec "$c" sh -c 'pg_dump --format=custom -d devops | pg_restore --no-owner --no-privileges --exit-on-error -d restored'
got="$(privs restored)"
[[ "$got" != *"table backup_runs"* ]] || fail "A4: the restored database already had the table grants"
grants restored
want_privs restored "A4: the grants of the restored database are not the list"
ok "after the restore: none; after the file: exactly the list"

step "A5. the role logs in with its password only, and does the worker's work and nothing else"
rc=0
printf 'SELECT 1\n' | docker exec -i -e PGPASSWORD=wrong-password "$c" sh -c 'psql -X -q -h "$(hostname -i)" -U offsite -d devops' >/dev/null 2>&1 || rc=$?
((rc != 0)) || fail "A5: a wrong password logged in"
as_offsite 'SELECT count(*) FROM backup_settings' >/dev/null || fail "A5: cannot read backup_settings"
as_offsite 'SELECT version FROM schema_migrations' >/dev/null || fail "A5: cannot read schema_migrations"
as_offsite 'LISTEN backup_work' >/dev/null || fail "A5: cannot LISTEN"
as_offsite "INSERT INTO backup_change_log (kind, detail) VALUES ('signing_key_replaced', '{}')" >/dev/null || fail "A5: cannot insert a change-log row"
as_offsite "INSERT INTO backup_requests (state, requested_by) VALUES ('queued', 'offsite')" >/dev/null || fail "A5: cannot insert a request"
as_offsite "UPDATE backup_settings SET signing_public = 'x', signing_private_enc = decode(repeat('00', 40), 'hex')" >/dev/null || fail "A5: cannot set the signing key in the empty state"
# shellcheck disable=SC2016  # $$ is SQL dollar quoting, not a shell variable
for q in 'SELECT 1 FROM users' 'SELECT 1 FROM issues' 'SELECT 1 FROM platform_settings' 'SELECT 1 FROM backup_change_log' \
	'DELETE FROM backup_runs' 'DELETE FROM backup_requests' "UPDATE backup_destinations SET name = 'x'" "UPDATE backup_settings SET recipients = '[]'" \
	"UPDATE backup_settings SET key_check = NULL" 'INSERT INTO backup_dumps (file_name, status) VALUES ($$devops-20200101T000000Z.dump$$, $$ok$$)' \
	'CREATE TABLE offsite_x (a int)' 'DROP TABLE backup_runs' 'TRUNCATE backup_runs' 'CREATE ROLE evil'; do
	if as_offsite "$q" >/dev/null 2>&1; then fail "A5: the role may run: $q"; fi
done
ok "wrong password refused; reads, LISTEN, change-log and request INSERT and the signing-key UPDATE work; users, issues, platform_settings, backup_change_log reads, deletes, other columns, CREATE and DROP are refused (14 statements)"
own >/dev/null <<'SQL'
DELETE FROM backup_requests;
DELETE FROM backup_change_log;
SQL

# ---------------------------------------------------------------- B. backup.sh
dumpdir=/backups
in_c() { docker exec -u 70:70 -e PGHOST=127.0.0.1 "$c" "$@"; }
nrows() { own -c "$1"; }

step "B0. the script has no psql -c, and it sends its SQL on standard input"
if grep -vE '^[[:space:]]*#' "$here/backup/backup.sh" | grep -nE 'psql[^#|]* -c[ =]'; then fail "B0: backup.sh calls psql -c"; fi
grep -q -- '-f -' "$here/backup/backup.sh" || fail "B0: no psql -f -"
ok "no psql -c; psql -f -"

step "B1. once records the dump: ok, bytes, sha256, migration version, mode 0600"
in_c sh /backup/backup.sh once >"$scratch/b1.out" 2>&1 || {
	cat "$scratch/b1.out"
	fail "B1: backup.sh once failed"
}
[[ "$(nrows "SELECT count(*) FROM backup_dumps")" == 1 ]] || fail "B1: not exactly one backup_dumps row"
row="$(own -F '|' -c "SELECT file_name, status, bytes, sha256, migration_version, finished_at IS NOT NULL, file_removed_at IS NULL, error_text FROM backup_dumps")"
IFS='|' read -r fname status bytes sha mv fin notrem errt <<<"$row"
[[ "$fname" =~ ^devops-[0-9]{8}T[0-9]{6}Z\.dump$ ]] || fail "B1: file name $fname"
[[ "$status" == ok && "$fin" == t && "$notrem" == t && -z "$errt" ]] || fail "B1: row is $row"
real_bytes="$(in_c sh -c "wc -c <$dumpdir/$fname" | tr -d ' \r')"
real_sha="$(in_c sh -c "sha256sum $dumpdir/$fname" | cut -d' ' -f1)"
[[ "$bytes" == "$real_bytes" ]] || fail "B1: bytes $bytes != file $real_bytes"
[[ "$sha" == "$real_sha" ]] || fail "B1: sha256 differs from the file's"
[[ "$mv" == "$version" ]] || fail "B1: migration_version $mv != $version"
[[ "$(in_c sh -c "stat -c %a $dumpdir/$fname")" == 600 ]] || fail "B1: the dump is not mode 0600"
ok "$fname: ok, $bytes bytes, sha256 equal to the file's, migration version $mv, mode 0600"

step "B2. a prune sets file_removed_at; a file that went missing is marked; stale rows are failed"
old=devops-20200101T010101Z.dump
gone=devops-20200202T020202Z.dump
in_c sh -c "umask 077; echo old >$dumpdir/$old; touch -t 202001010101 $dumpdir/$old"
own >/dev/null <<SQL
INSERT INTO backup_dumps (file_name, status, bytes, sha256) VALUES ('$old', 'ok', 4, repeat('a', 64)), ('$gone', 'ok', 4, repeat('b', 64));
INSERT INTO backup_dumps (file_name, status) VALUES ('devops-20200303T030303Z.dump', 'running');
INSERT INTO backup_requests (state, requested_by, taken_at) VALUES ('taken', 'app', now());
SQL
sleep 1
in_c sh /backup/backup.sh once >"$scratch/b2.out" 2>&1 || {
	cat "$scratch/b2.out"
	fail "B2: backup.sh once failed"
}
in_c test ! -e "$dumpdir/$old" || fail "B2: the old dump was not pruned"
[[ "$(nrows "SELECT file_removed_at IS NOT NULL FROM backup_dumps WHERE file_name = '$old'")" == t ]] || fail "B2: pruned file: file_removed_at not set"
[[ "$(nrows "SELECT file_removed_at IS NOT NULL FROM backup_dumps WHERE file_name = '$gone'")" == t ]] || fail "B2: missing file not marked"
[[ "$(nrows "SELECT status || '/' || error_text FROM backup_dumps WHERE file_name = 'devops-20200303T030303Z.dump'")" == failed/interrupted ]] || fail "B2: stale running row not failed"
[[ "$(nrows "SELECT state FROM backup_requests WHERE requested_by = 'app'")" == failed ]] || fail "B2: stale taken request not failed"
[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE status = 'ok' AND file_removed_at IS NULL")" == 2 ]] || fail "B2: the two real dumps are not both ok and present"
ok "prune -> file_removed_at; missing file marked; stale running -> failed/interrupted; stale taken request -> failed"
own -c 'DELETE FROM backup_requests' >/dev/null

step "B3. a killed backup.sh leaves no running row after the next run"
docker exec -u 70:70 "$c" sh -c 'mkdir -p /tmp/shim && printf "#!/bin/sh\nsleep 300\nexit 0\n" >/tmp/shim/pg_dump && chmod +x /tmp/shim/pg_dump'
docker exec -u 70:70 -d "$c" sh -c 'echo $$ >/tmp/b3.pid; export PATH=/tmp/shim:$PATH; exec sh /backup/backup.sh once >/tmp/b3.log 2>&1'
for _ in $(seq 1 30); do
	[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE status = 'running'")" == 1 ]] && break
	sleep 1
done
[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE status = 'running'")" == 1 ]] || {
	docker exec "$c" sh -c 'cat /tmp/b3.log; ps' >&2 || true
	fail "B3: the slow run never recorded a running row"
}
# Why the shim ends with `exit 0`: this container has no init, so postgres is
# PID 1. When backup.sh is killed, the shim (a child of backup.sh) becomes a child of the
# postmaster. When its sleep is then killed, the shim ends; the postmaster reaps it, and
# an unknown child that ends with a non-zero status (137) is taken for a crashed backend:
# "all server processes terminated; reinitializing", and the database is in recovery mode.
# With `exit 0` the postmaster sees a clean exit. Then wait (bounded) until the shim is
# gone and the database answers, so no later step meets a restart.
docker exec -u 70:70 "$c" sh -c 'kill -9 "$(cat /tmp/b3.pid)"; pkill -9 -f "[s]leep 300" || true'
gone=0
for _ in $(seq 1 30); do
	if ! docker exec "$c" pgrep -f '[/]tmp/shim/pg_dump' >/dev/null 2>&1; then
		gone=1
		break
	fi
	sleep 1
done
((gone)) || fail "B3: the pg_dump shim did not end in 30 s"
back=0
for _ in $(seq 1 60); do
	if docker exec "$c" psql -X -q -t -A -U devops -d devops -c 'select 1' >/dev/null 2>&1; then
		back=1
		break
	fi
	sleep 1
done
((back)) || fail "B3: the database did not come back in 60 s"
[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE status = 'running'")" == 1 ]] || {
	nrows "SELECT status, file_name, error_text FROM backup_dumps ORDER BY started_at" >&2
	docker exec "$c" sh -c 'ps; ls -la /tmp /tmp/devops-backup.lock' >&2 || true
	fail "B3: the kill did not leave a running row"
}
in_c test -d /tmp/devops-backup.lock || fail "B3: the kill left no lock (the test does not test the stale lock)"
in_c sh /backup/backup.sh once >"$scratch/b3.out" 2>&1 || {
	cat "$scratch/b3.out"
	fail "B3: the run after the kill failed"
}
[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE status = 'running'")" == 0 ]] || fail "B3: a running row is left after the next run"
ok "after kill -9: a running row and a stale lock; the next run takes the lock, marks the row failed and dumps"
docker exec -u 70:70 "$c" rm -rf /tmp/shim

step "B4. the tables are missing: the dump is made all the same"
own -d postgres -c 'CREATE DATABASE bare' >/dev/null
before="$(in_c sh -c "ls $dumpdir/devops-*.dump | wc -l" | tr -d ' \r')"
docker exec -u 70:70 -e PGHOST=127.0.0.1 -e PGDATABASE=bare "$c" sh /backup/backup.sh once >"$scratch/b4.out" 2>&1 || {
	cat "$scratch/b4.out"
	fail "B4: backup.sh once failed on a database without the backup tables"
}
after="$(in_c sh -c "ls $dumpdir/devops-*.dump | wc -l" | tr -d ' \r')"
((after == before + 1)) || fail "B4: no new dump file"
grep -q 'the backup tables are not there (yet)' "$scratch/b4.out" || fail "B4: it did not say the tables are missing"
ok "exit 0, a new dump file, one log line about the missing tables"

step "B5. a queued request makes a dump within 60 seconds; the request is done with its dump_id"
req="$(own -c "INSERT INTO backup_requests (state, requested_by) VALUES ('queued', 'app') RETURNING id")"
t0="$(date +%s)"
# The daily time is far away: the dump comes from the request, nothing else.
at="$(date -u -v+12H +%H:%M 2>/dev/null || date -u -d '+12 hours' +%H:%M)"
docker exec -u 70:70 -d -e BACKUP_REQUEST_POLL_SECONDS=2 -e BACKUP_AT_UTC="$at" "$c" sh -c 'exec sh /backup/backup.sh loop >/tmp/loop.log 2>&1'
for _ in $(seq 1 60); do
	[[ "$(nrows "SELECT state FROM backup_requests WHERE id = '$req'")" == 'done' ]] && break
	sleep 1
done
t1="$(date +%s)"
[[ "$(nrows "SELECT state FROM backup_requests WHERE id = '$req'")" == 'done' ]] || {
	docker exec "$c" cat /tmp/loop.log >&2 || true
	fail "B5: the request is not done after 60 seconds"
}
row="$(own -F '|' -c "SELECT d.status, d.request_id = r.id, r.dump_id = d.id, d.started_at >= r.requested_at FROM backup_requests r JOIN backup_dumps d ON d.id = r.dump_id WHERE r.id = '$req'")"
[[ "$row" == "ok|t|t|t" ]] || fail "B5: the request's dump is wrong: $row"
echo "   request to done: $((t1 - t0)) seconds with a 2 second poll (the stack's poll is 30 seconds)"
ok "request done, dump ok, its request_id and the request's dump_id agree, started after the request"

step "B6. a request the app already failed is not flipped back; a failed pg_dump fails its request"
docker exec -u 70:70 "$c" sh -c 'mkdir -p /tmp/shim && printf "#!/bin/sh\nsleep 6\nexec /usr/local/bin/pg_dump \"\$@\"\n" >/tmp/shim/pg_dump && chmod +x /tmp/shim/pg_dump'
docker exec -u 70:70 "$c" pkill -f '[b]ackup.sh loop' || true
sleep 1
docker exec -u 70:70 -d -e PATH="/tmp/shim:/usr/local/bin:/usr/bin:/bin" -e BACKUP_REQUEST_POLL_SECONDS=2 -e BACKUP_AT_UTC="$at" "$c" sh -c 'exec sh /backup/backup.sh loop >/tmp/loop.log 2>&1'
req2="$(own -c "INSERT INTO backup_requests (state, requested_by) VALUES ('queued', 'app') RETURNING id")"
for _ in $(seq 1 30); do
	[[ "$(nrows "SELECT state FROM backup_requests WHERE id = '$req2'")" == taken ]] && break
	sleep 1
done
own -c "UPDATE backup_requests SET state = 'failed', finished_at = now() WHERE id = '$req2'" >/dev/null
for _ in $(seq 1 40); do
	[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE request_id = '$req2' AND status = 'ok'")" == 1 ]] && break
	sleep 1
done
[[ "$(nrows "SELECT count(*) FROM backup_dumps WHERE request_id = '$req2' AND status = 'ok'")" == 1 ]] || fail "B6: the slow dump never finished"
sleep 2
[[ "$(nrows "SELECT state FROM backup_requests WHERE id = '$req2'")" == failed ]] || fail "B6: a failed request was flipped back"
ok "the app's 'failed' stayed 'failed' although the dump finished"
docker exec -u 70:70 "$c" pkill -f '[b]ackup.sh loop' || true
sleep 1
docker exec -u 70:70 "$c" sh -c 'printf "#!/bin/sh\nexit 1\n" >/tmp/shim/pg_dump'
docker exec -u 70:70 -d -e PATH="/tmp/shim:/usr/local/bin:/usr/bin:/bin" -e BACKUP_REQUEST_POLL_SECONDS=2 -e BACKUP_AT_UTC="$at" "$c" sh -c 'exec sh /backup/backup.sh loop >/tmp/loop.log 2>&1'
req3="$(own -c "INSERT INTO backup_requests (state, requested_by) VALUES ('queued', 'app') RETURNING id")"
for _ in $(seq 1 30); do
	[[ "$(nrows "SELECT state FROM backup_requests WHERE id = '$req3'")" == failed ]] && break
	sleep 1
done
[[ "$(nrows "SELECT state FROM backup_requests WHERE id = '$req3'")" == failed ]] || fail "B6: a failing pg_dump did not fail the request"
[[ "$(nrows "SELECT status || '/' || error_text FROM backup_dumps WHERE request_id = '$req3'")" == 'failed/pg_dump failed' ]] || fail "B6: the failed dump row is wrong"
docker exec -u 70:70 "$c" pkill -f '[b]ackup.sh loop' || true
docker exec -u 70:70 "$c" rm -rf /tmp/shim
ok "a failing pg_dump: request failed, dump row failed with the fixed text 'pg_dump failed'"

step "B7. hostile values reach no shell and no SQL string"
# (a) a request id that is not a uuid, from a psql that lies; the shim logs every argument and statement.
docker exec -i -u 70:70 "$c" sh -c 'mkdir -p /tmp/shim && cat >/tmp/shim/psql' <<'SHIM'
#!/bin/sh
{ printf 'ARGS:'; for a in "$@"; do printf ' [%s]' "$a"; done; printf '\n'; } >>/tmp/psql.log
sql="$(cat)"
printf 'SQL: %s\n----\n' "$sql" >>/tmp/psql.log
case "$sql" in
*"state = 'taken', taken_at"*) printf '%s\n' "x'; touch /tmp/pwned; echo '"; exit 0 ;;
*to_regclass*) echo t; exit 0 ;;
*"INSERT INTO backup_dumps"*) printf '%s\n' '$(touch /tmp/pwned2)'; exit 0 ;;
*"SELECT file_name FROM backup_dumps"*) printf '%s\n' "devops-20200101T000000Z.dump'; touch /tmp/pwned3; --" 'devops-20200101T000000Z.dump$(touch /tmp/pwned4)'; exit 0 ;;
esac
exit 0
SHIM
docker exec -u 70:70 "$c" chmod +x /tmp/shim/psql
# (b) a hostile file name in the directory, old enough to be pruned.
docker exec -u 70:70 "$c" sh -c "umask 077; : >\"$dumpdir/devops-x'; touch pwned5; --.dump\"; touch -t 202001010101 \"$dumpdir/devops-x'; touch pwned5; --.dump\""
docker exec -u 70:70 -w /tmp -e PATH="/tmp/shim:/usr/local/bin:/usr/bin:/bin" -e BACKUP_REQUEST_POLL_SECONDS=1 -e BACKUP_AT_UTC="$at" "$c" \
	sh -c 'timeout 8 sh /backup/backup.sh loop >/tmp/hostile.out 2>&1; PATH=/tmp/shim:/usr/local/bin:/usr/bin:/bin sh /backup/backup.sh once >>/tmp/hostile.out 2>&1; true'
for f in pwned pwned2 pwned3 pwned4 pwned5; do
	docker exec "$c" test ! -e "/tmp/$f" || fail "B7: /tmp/$f exists: a hostile value reached a shell"
done
docker exec "$c" test -s /tmp/psql.log || fail "B7: the psql shim saw nothing (the test did not run the script)"
grep -q 'ignored a dump request whose id is not a uuid' <(docker exec "$c" cat /tmp/hostile.out) || fail "B7: the hostile request id was not ignored by name"
docker exec "$c" cat /tmp/psql.log >"$scratch/psql.log"
if grep -n -e 'pwned' "$scratch/psql.log" | grep -v '^[0-9]*:ARGS:'; then fail "B7: a hostile value is in a SQL statement"; fi
if grep -n 'ARGS:.*pwned' "$scratch/psql.log"; then fail "B7: a hostile value is in a psql argument"; fi
if docker exec "$c" find /backups -name "devops-x*" | grep -q .; then fail "B7: the hostile file was not pruned (the test does not test the prune)"; fi
ok "a non-uuid request id, a hostile dump id, hostile file names from the database and from the directory: nothing ran, nothing reached an argument or a statement"
docker exec -u 70:70 "$c" rm -rf /tmp/shim /tmp/psql.log

# (c) the same with the real psql: a dump row whose file_name is an injection.
bad="devops-20200101T000000Z.dump'; CREATE TABLE pwned_table (x int); --"
own -c "INSERT INTO backup_dumps (file_name, status, bytes, sha256) VALUES (\$q\$$bad\$q\$, 'ok', 1, repeat('c', 64))" >/dev/null
in_c sh /backup/backup.sh once >"$scratch/b7c.out" 2>&1 || {
	cat "$scratch/b7c.out"
	fail "B7c: backup.sh once failed"
}
[[ "$(own -c "SELECT count(*) FROM pg_tables WHERE tablename = 'pwned_table'")" == 0 ]] || fail "B7c: a hostile file name made a table"
ok "a hostile file_name in backup_dumps (real psql) made no table"

step "B8. the cost of the 30 second request check"
cost="$(docker exec -u 70:70 "$c" sh -c '
t0=$(date +%s)
i=0
while [ $i -lt 100 ]; do
	psql -X -q -t -A -v ON_ERROR_STOP=1 -f - >/dev/null <<\EOSQL
UPDATE backup_requests SET state = $$taken$$ WHERE id = (SELECT id FROM backup_requests WHERE state = $$queued$$ ORDER BY requested_at LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING id;
EOSQL
	i=$((i + 1))
done
echo $(( $(date +%s) - t0 ))')"
echo "   100 idle request checks (psql start, connect, one statement, exit): ${cost} seconds in total (the clock has a 1 second resolution)"
ok "measured (see the line above); the stack runs two of these a minute"

printf '\ntest-offsite: PASS\n'
