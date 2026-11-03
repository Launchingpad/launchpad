#!/bin/sh
# dev-ops database backup. Runs in the `backup` service of
# deploy/docker-compose.yml (the postgres image, as uid 70), and in
# deploy/test-restore.sh.
#
#   backup.sh once   one pg_dump now, then prune
#   backup.sh loop   once a day at BACKUP_AT_UTC (HH:MM, UTC), for ever; and
#                    every 30 seconds it looks for a queued dump request
#                    (backup_requests) and makes that dump too
#
# A dump is custom format (pg_dump -Fc), written to a .partial file, read in
# full with `pg_restore -f /dev/null` (every data block, not only the table
# of contents that `pg_restore --list` reads), then renamed, so a
# half-written or truncated file is never taken for a backup. Dumps older
# than BACKUP_KEEP_DAYS days are deleted; it must be a whole number of days,
# 1 or more, or the script stops before it dumps or prunes anything.
# Connection: the standard PG* variables (PGHOST, PGUSER, PGPASSWORD,
# PGDATABASE). Nothing secret is printed.
#
# (decisions-T16 #28, #29): each dump is also RECORDED in backup_dumps
# (running at the start, then ok with its size, sha256 and the database's
# migration version, or failed), and a pruned file sets file_removed_at. The
# rules:
#   - Recording never fails a dump. If the tables are missing (the app has not
#     migrated yet, an older database) or psql fails, it logs one line and goes on.
#   - Every SQL statement goes to psql on STANDARD INPUT, and every value is a
#     `psql -v name=value` variable used as :'name' (psql quotes it). psql -c does
#     not interpolate. No value from the database or from a file name is ever
#     built into a SQL string or a shell command; ids are checked with a `case`
#     pattern first (a uuid, a file name, digits).
#   - One dump runs at a time (a lock directory in /tmp, which is a tmpfs: a killed
#     or restarted container loses it). A run that gets the lock first marks every
#     `running` row failed: none of them can belong to a live run.
#   - The stamps started_at / finished_at are SQL now(), the database clock.
set -eu
# Every dump holds all data: readable by its owner only.
umask 077

dir="${BACKUP_DIR:-/backups}"
keep="${BACKUP_KEEP_DAYS:-30}"
at="${BACKUP_AT_UTC:-03:15}"
# How often `loop` looks for a queued dump request (decisions-T16 #28). 30 seconds;
# the tests set a shorter time. A plain number of seconds, 1 to 3600.
poll="${BACKUP_REQUEST_POLL_SECONDS:-30}"
lockdir="${BACKUP_LOCK_DIR:-/tmp/devops-backup.lock}"
# A psql that cannot reach the database must not hang a dump.
PGCONNECT_TIMEOUT="${PGCONNECT_TIMEOUT:-10}"
export PGCONNECT_TIMEOUT

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) backup: $*"; }

# BACKUP_KEEP_DAYS goes into shell arithmetic: a word there reads as an
# unset variable (0), so `-mmin +0` would prune every older dump. Only a
# plain decimal number is allowed (no sign, no leading zero, which reads as
# octal; at most 5 digits). 0 is refused too: the next night would delete
# every older dump, including the pre-update one a rollback needs.
case "$keep" in
[1-9] | [1-9][0-9] | [1-9][0-9][0-9] | [1-9][0-9][0-9][0-9] | [1-9][0-9][0-9][0-9][0-9]) ;;
*)
	log "ERROR BACKUP_KEEP_DAYS must be a whole number of days, 1 or more (got '$keep'); nothing was dumped or pruned"
	exit 2
	;;
esac
case "$poll" in
[1-9] | [1-9][0-9] | [1-9][0-9][0-9] | [1-9][0-9][0-9][0-9]) ;;
*)
	log "ERROR BACKUP_REQUEST_POLL_SECONDS must be a whole number of seconds, 1 to 9999 (got '$poll')"
	exit 2
	;;
esac

# ---------------------------------------------------------------- checks of values
# Nothing read from the database or from a directory listing reaches a shell
# command or a SQL string. A value is checked with one of these `case` patterns
# first, and then only ever passed as a `psql -v` variable.

# is_uuid S: exactly 36 characters, lower-case hex in the 8-4-4-4-12 shape.
is_uuid() {
	case "$1" in
	[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) return 0 ;;
	esac
	return 1
}
# is_dump_name S: devops-YYYYMMDDTHHMMSSZ.dump, the only name offsite opens.
is_dump_name() {
	case "$1" in
	devops-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump) return 0 ;;
	esac
	return 1
}
# is_digits S: one to 19 decimal digits and nothing else.
is_digits() {
	case "$1" in
	"" | *[!0-9]*) return 1 ;;
	esac
	[ "${#1}" -le 19 ]
}

# ---------------------------------------------------------------- recording (backup_dumps)

# sql [-v name=value ...]: the statements on STANDARD INPUT (psql -c does not
# interpolate :'name'). -X: no psqlrc. The caller handles failure.
sql() { psql -X -q -t -A -v ON_ERROR_STOP=1 "$@" -f -; }

# rec is 1 while backup_dumps and backup_requests are there. A database from
# before the tables (the app has not migrated yet) is dumped all the same.
rec=0
probe_tables() {
	probe_out="$(sql 2>/dev/null <<'SQL'
SELECT to_regclass('public.backup_dumps') IS NOT NULL AND to_regclass('public.backup_requests') IS NOT NULL;
SQL
	)" || probe_out=""
	[ "$probe_out" = t ]
}

# mark_stale REQUEST: the lock is ours, so no other run is live and no `running`
# dump and no `taken` request (but the one we hold) can belong to a live run:
# a killed backup.sh, a restored database that carries another host's rows.
mark_stale() {
	sql -v "req=${1:-}" >/dev/null 2>&1 <<'SQL' || log "WARN could not mark the dumps and requests an earlier run left"
UPDATE backup_dumps SET status = 'failed', finished_at = now(), error_text = 'interrupted'
 WHERE status = 'running';
UPDATE backup_requests SET state = 'failed', finished_at = now()
 WHERE state = 'taken' AND id IS DISTINCT FROM NULLIF(:'req', '')::uuid;
SQL
}

# mark_removed NAME: the file is gone, so offsite must not use the row.
mark_removed() {
	is_dump_name "$1" || return 0
	sql -v "name=$1" >/dev/null 2>&1 <<'SQL' || log "WARN could not record that a dump file is gone"
UPDATE backup_dumps SET file_removed_at = now()
 WHERE file_name = :'name' AND file_removed_at IS NULL;
SQL
}

# mark_missing: rows that say `ok` for a file that is no longer there (a hand
# delete, a restored database).
mark_missing() {
	miss_names="$(sql 2>/dev/null <<'SQL'
SELECT file_name FROM backup_dumps WHERE status = 'ok' AND file_removed_at IS NULL ORDER BY started_at;
SQL
	)" || return 0
	printf '%s\n' "$miss_names" | while IFS= read -r miss_n; do
		is_dump_name "$miss_n" || continue
		[ -f "$dir/$miss_n" ] || mark_removed "$miss_n"
	done
}

# start_record NAME REQUEST: the `running` row; prints its id (a uuid) or nothing.
start_record() {
	st_out="$(sql -v "name=$1" -v "req=${2:-}" 2>/dev/null <<'SQL'
INSERT INTO backup_dumps (file_name, status, request_id)
VALUES (:'name', 'running', NULLIF(:'req', '')::uuid)
RETURNING id;
SQL
	)" || return 0
	if is_uuid "$st_out"; then printf '%s' "$st_out"; fi
}

# finish_failed ID TEXT: TEXT is a fixed sentence of this script, never pg_dump output.
finish_failed() {
	[ -n "$1" ] || return 0
	sql -v "id=$1" -v "err=$2" >/dev/null 2>&1 <<'SQL' || log "WARN could not record the failed dump"
UPDATE backup_dumps SET status = 'failed', finished_at = now(), error_text = :'err'
 WHERE id = :'id'::uuid AND status = 'running';
SQL
}

# finish_ok ID BYTES SHA256 MIGRATION_VERSION
finish_ok() {
	[ -n "$1" ] || return 0
	sql -v "id=$1" -v "bytes=$2" -v "sha=$3" -v "mv=${4:-}" >/dev/null 2>&1 <<'SQL' || log "WARN could not record the finished dump"
UPDATE backup_dumps SET status = 'ok', finished_at = now(), bytes = :'bytes'::bigint,
       sha256 = :'sha', migration_version = NULLIF(:'mv', '')::bigint, error_text = ''
 WHERE id = :'id'::uuid AND status = 'running';
SQL
}

# take_request: the oldest queued request, taken with one statement; prints its id.
take_request() {
	tk_out="$(sql 2>/dev/null <<'SQL'
UPDATE backup_requests SET state = 'taken', taken_at = now()
 WHERE id = (SELECT id FROM backup_requests WHERE state = 'queued'
              ORDER BY requested_at LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING id;
SQL
	)" || return 0
	if [ -z "$tk_out" ]; then return 0; fi
	if is_uuid "$tk_out"; then
		printf '%s' "$tk_out"
	else
		log "WARN ignored a dump request whose id is not a uuid"
	fi
}

# finish_request ID done|failed DUMP_ID: `AND state = 'taken'` so a request the
# app already marked failed is never flipped back.
finish_request() {
	sql -v "rid=$1" -v "st=$2" -v "did=${3:-}" >/dev/null 2>&1 <<'SQL' || log "WARN could not record the end of a dump request"
UPDATE backup_requests SET state = :'st', finished_at = now(), dump_id = NULLIF(:'did', '')::uuid
 WHERE id = :'rid'::uuid AND state = 'taken';
SQL
}

# ---------------------------------------------------------------- the lock

# One dump at a time in this container. The lock is a directory in /tmp (a tmpfs
# in the stack), so a killed or restarted container loses it; a process that died
# alone leaves a pid that kill -0 does not find.
# lock WAIT_SECONDS: 0 = got it, 1 = gave up.
lock() {
	lk_n=0
	while ! mkdir "$lockdir" 2>/dev/null; do
		lk_holder="$(cat "$lockdir/pid" 2>/dev/null || true)"
		if [ -n "$lk_holder" ]; then
			if ! kill -0 "$lk_holder" 2>/dev/null; then
				rm -rf "$lockdir"
				continue
			fi
		elif [ -d "$lockdir" ] && [ -n "$(find "$lockdir" -maxdepth 0 -mmin +1 2>/dev/null)" ]; then
			# No pid after a minute: its owner died between mkdir and the write.
			rm -rf "$lockdir"
			continue
		fi
		[ "$lk_n" -ge "$1" ] && return 1
		sleep 1
		lk_n=$((lk_n + 1))
	done
	echo "$$" >"$lockdir/pid"
}
unlock() {
	if [ "$(cat "$lockdir/pid" 2>/dev/null || true)" = "$$" ]; then rm -rf "$lockdir"; fi
	return 0
}
trap unlock EXIT

# ---------------------------------------------------------------- the dump

# last_dump_id: the backup_dumps row of the last once_locked (empty: not recorded).
last_dump_id=""

# once_locked [REQUEST]: one pg_dump now, then prune. The lock is held. REQUEST is
# a checked uuid of the request this dump answers, or nothing.
once_locked() {
	req="${1:-}"
	ts="$(date -u +%Y%m%dT%H%M%SZ)"
	# A name is used once: backup_dumps.file_name is unique, and offsite checks a
	# file against the sha256 of the row of that name. Two dumps in one second
	# (a tiny database) wait for the next second.
	while [ -e "$dir/devops-$ts.dump" ]; do
		sleep 1
		ts="$(date -u +%Y%m%dT%H%M%SZ)"
	done
	name="devops-$ts.dump"
	final="$dir/$name"
	tmp="$dir/.devops-$ts.dump.partial"
	rm -f "$dir"/.devops-*.dump.partial
	last_dump_id=""
	did=""
	mv_ver=""
	rec=0
	if probe_tables; then
		rec=1
		mark_stale "$req"
		mark_missing
		did="$(start_record "$name" "$req")" || did=""
		[ -n "$did" ] || log "WARN this dump is not recorded (the row could not be made)"
		mv_ver="$(sql 2>/dev/null <<'SQL'
SELECT coalesce((SELECT version::text FROM schema_migrations LIMIT 1), '');
SQL
		)" || mv_ver=""
		is_digits "$mv_ver" || mv_ver=""
	else
		log "the backup tables are not there (yet): this dump is not recorded"
	fi
	last_dump_id="$did"
	start="$(date +%s)"
	if ! pg_dump --format=custom --compress=6 --file="$tmp"; then
		rm -f "$tmp"
		log "ERROR pg_dump failed"
		finish_failed "$did" "pg_dump failed"
		return 1
	fi
	# Read every entry, data blocks included: `pg_restore --list` reads only
	# the table of contents and passes a truncated file (measured). With -f
	# it connects to no database and writes the script to /dev/null.
	if ! pg_restore -f /dev/null "$tmp"; then
		rm -f "$tmp"
		log "ERROR the dump is not a complete, readable archive"
		finish_failed "$did" "the dump is not a readable archive"
		return 1
	fi
	mv "$tmp" "$final"
	touch "$dir/.last-success"
	bytes="$(wc -c <"$final" | tr -d ' ')"
	if [ "$rec" = 1 ] && [ -n "$did" ]; then
		sha="$(sha256sum "$final" | cut -d' ' -f1)" || sha=""
		if is_digits "$bytes" && [ "${#sha}" -eq 64 ]; then
			finish_ok "$did" "$bytes" "$sha" "$mv_ver"
		else
			finish_failed "$did" "the dump could not be measured"
		fi
	fi
	log "wrote $(basename "$final") ($bytes bytes, $(($(date +%s) - start))s)"
	# Keep BACKUP_KEEP_DAYS days: delete dumps last written longer ago.
	find "$dir" -maxdepth 1 -type f -name 'devops-*.dump' -mmin "+$((keep * 1440))" -print |
		while read -r old; do
			if rm -f "$old"; then
				log "pruned $(basename "$old") (older than $keep days)"
				if [ "$rec" = 1 ]; then mark_removed "$(basename "$old")"; fi
			fi
		done
}

# once: take the lock (up to 30 minutes: a dump of the loop may be running), dump.
once() {
	if ! lock 1800; then
		log "ERROR another dump is still running after 30 minutes; this one was not made"
		return 1
	fi
	once_rc=0
	once_locked "" || once_rc=$?
	unlock
	return "$once_rc"
}

# serve_requests: if no dump is running now, take one queued request (if any),
# make its dump and mark the request done or failed. Best effort: every database
# failure is silent here (the tables may not be there yet).
serve_requests() {
	lock 0 || return 0
	sr_rid="$(take_request)" || sr_rid=""
	if [ -n "$sr_rid" ]; then
		log "dump request $sr_rid"
		if once_locked "$sr_rid"; then sr_st="done"; else sr_st=failed; fi
		finish_request "$sr_rid" "$sr_st" "$last_dump_id"
	fi
	unlock
	return 0
}

# Seconds from now to the next HH:MM UTC. Plain arithmetic on the epoch, so
# it needs no `date -d` (busybox has a limited one).
seconds_to_next() {
	hh="${at%%:*}"
	mm="${at##*:}"
	now="$(date -u +%s)"
	target=$(((now / 86400) * 86400 + ${hh#0} * 3600 + ${mm#0} * 60))
	if [ "$target" -le "$now" ]; then
		target=$((target + 86400))
	fi
	echo $((target - now))
}

case "${1:-}" in
once)
	once
	;;
loop)
	case "$at" in
	[01][0-9]:[0-5][0-9] | 2[0-3]:[0-5][0-9]) ;;
	*)
		log "ERROR BACKUP_AT_UTC must be HH:MM (got $at)"
		exit 2
		;;
	esac
	# PID 1 in the container: without a trap, `docker stop` waits 10s and
	# kills. Sleep in the background so the trap runs at once.
	trap 'log "stopping"; exit 0' TERM INT
	log "nightly at $at UTC, keeping $keep days in $dir; a dump request is looked for every ${poll}s"
	while :; do
		wait_s="$(seconds_to_next)"
		log "next run in ${wait_s}s"
		deadline=$(($(date -u +%s) + wait_s))
		serve_requests
		while :; do
			left=$((deadline - $(date -u +%s)))
			[ "$left" -gt 0 ] || break
			step="$poll"
			[ "$left" -ge "$step" ] || step="$left"
			sleep "$step" &
			wait $!
			serve_requests
		done
		# A failed night is logged and retried the next night; the loop
		# never exits, so the container does not restart-loop.
		once || log "ERROR this run failed; the next run is tomorrow"
	done
	;;
*)
	echo "usage: backup.sh once|loop" >&2
	exit 2
	;;
esac
