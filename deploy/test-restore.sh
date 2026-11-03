#!/usr/bin/env bash
# The backup + restore test. Needs only docker; starts nothing
# of any stack and publishes no port.
#
#   deploy/test-restore.sh            (make restore-test in server/)
#
# 1. A throwaway postgres container, the same image as the production stack.
# 2. Every server migration (server/db/migrations/*.up.sql), then a fixture
#    about the size of a large Linear workspace (3,000 issues, 6,000
#    comments, labels, events, attachments, state history).
# 3. deploy/backup/backup.sh once  -> a real custom-format dump, and the
#    30-day prune (an old dump is planted and must be deleted).
# 4. deploy/restore.sh --compare latest  -> a scratch database whose every
#    table count must equal the source. The restore time is printed.
#    Also: each dump is mode 0600; a bad BACKUP_KEEP_DAYS stops backup.sh
#    before it dumps or prunes; a truncated .partial is never renamed into
#    place.
# 5. A truncated dump must FAIL to restore, and with --keep it must leave no
#    scratch database. The runbook's rollback swap is one transaction: a
#    failed second rename moves nothing.
# Scratch: DEPLOY_TMPDIR (default $TMPDIR, else /tmp).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
image="$(grep -m1 -E '^\s+image: postgres:' "$here/docker-compose.yml" | awk '{print $2}')"
[[ -n "$image" ]] || {
	echo "test-restore: no postgres image in deploy/docker-compose.yml" >&2
	exit 2
}
c="dev-ops-restore-test-$$"
scratch="$(mktemp -d "${DEPLOY_TMPDIR:-${TMPDIR:-/tmp}}/devops-restore-test.XXXXXX")"
stack=""
cleanup() {
	if [[ -n "$stack" ]]; then
		docker compose -p "$stack" -f "$here/docker-compose.yml" --env-file "$scratch/stack.env" \
			down -v --remove-orphans >/dev/null 2>&1 || true
	fi
	docker rm -f "$c" >/dev/null 2>&1 || true
	rm -rf "$scratch"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "test-restore: FAIL: $*" >&2
	exit 1
}

step "postgres $image (container $c, no published port)"
docker run -d --rm --name "$c" \
	-e POSTGRES_USER=devops -e POSTGRES_DB=devops -e POSTGRES_PASSWORD=restore-test-only \
	-e PGHOST=127.0.0.1 -e PGUSER=devops -e PGPASSWORD=restore-test-only -e PGDATABASE=devops \
	-e BACKUP_DIR=/backups -e BACKUP_KEEP_DAYS=30 \
	--tmpfs /backups:mode=1777 \
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
# The init entrypoint restarts the server once; wait for the final one.
sleep 2
until docker exec "$c" psql -tAc 'select 1' >/dev/null 2>&1; do sleep 1; done

step "migrations"
n=0 version=0
for f in "$repo"/server/db/migrations/*.up.sql; do
	docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 <"$f" >/dev/null
	n=$((n + 1))
	# The migrator records the highest migration NUMBER, not a count.
	b="$(basename "$f")"
	num=$((10#${b%%_*}))
	((num > version)) && version=$num
done
# The server's migrator records its version here; a restore must carry it.
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL);
INSERT INTO schema_migrations VALUES ($version, false);
SQL
echo "applied $n migrations, schema_migrations version $version"

step "fixture"
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
BEGIN;
INSERT INTO orgs (slug, name) VALUES ('orbit', 'Orbit');
INSERT INTO users (username, name, email, is_agent) VALUES
  ('chen', 'Chen', 'chen@example.com', false), ('claude', 'Claude', NULL, true), ('friend', 'Friend', 'f@example.com', false);
INSERT INTO org_members (org_id, user_id, role) SELECT o.id, u.id, CASE u.username WHEN 'friend' THEN 'member' ELSE 'admin' END FROM orgs o, users u;
INSERT INTO teams (org_id, key, name) SELECT id, 'ORB', 'Orbit' FROM orgs;
INSERT INTO workflow_states (org_id, team_id, name, type, position)
  SELECT t.org_id, t.id, s.name, s.type, s.pos FROM teams t,
  (VALUES ('Backlog','backlog',0), ('Todo','unstarted',1), ('In Progress','started',2), ('In Review','started',3),
          ('In Testing','started',4), ('Done','completed',5), ('Canceled','canceled',6), ('Duplicate','canceled',7)) AS s(name, type, pos);
UPDATE teams t SET default_state_id = (SELECT id FROM workflow_states w WHERE w.team_id = t.id AND w.name = 'Backlog');
INSERT INTO issue_counters (team_id, org_id, next_number) SELECT id, org_id, 3001 FROM teams;
INSERT INTO labels (org_id, name) SELECT o.id, 'label-' || g FROM orgs o, generate_series(1, 40) g;
INSERT INTO issues (org_id, team_id, number, title, description, state_id, priority, creator_id)
  SELECT t.org_id, t.id, g, 'Issue ' || g, repeat('Body text of issue ' || g || '. ', 20),
         (SELECT id FROM workflow_states w WHERE w.team_id = t.id ORDER BY position LIMIT 1 OFFSET (g % 8)),
         g % 5, (SELECT id FROM users WHERE username = 'chen')
  FROM teams t, generate_series(1, 3000) g;
INSERT INTO issue_labels (org_id, issue_id, label_id)
  SELECT i.org_id, i.id, (SELECT id FROM labels l ORDER BY name LIMIT 1 OFFSET (i.number % 40)) FROM issues i;
INSERT INTO comments (org_id, issue_id, author_id, body)
  SELECT i.org_id, i.id, (SELECT id FROM users WHERE username = 'claude'), 'Comment ' || g || ' on ' || i.number
  FROM issues i, generate_series(1, 2) g;
INSERT INTO issue_state_changes (org_id, issue_id, to_state_id, actor_id)
  SELECT i.org_id, i.id, i.state_id, (SELECT id FROM users WHERE username = 'chen') FROM issues i;
INSERT INTO attachments (org_id, issue_id, kind, title, url)
  SELECT i.org_id, i.id, 'link', 'MR ' || i.number, 'https://gitlab.example.com/mr/' || i.number FROM issues i WHERE i.number % 6 = 0;
INSERT INTO events (org_id, seq, type, entity_type, entity_id, payload)
  SELECT i.org_id, i.number, 'issue.created', 'issue', i.id, jsonb_build_object('n', i.number) FROM issues i;
INSERT INTO org_event_counters (org_id, next_seq) SELECT id, 3001 FROM orgs;
COMMIT;
SQL
docker exec "$c" psql -X -tAc "SELECT 'issues ' || count(*) FROM issues UNION ALL SELECT 'comments ' || count(*) FROM comments"

step "backup (backup.sh once, as the backup service's uid 70), with a planted 31-day-old dump"
docker exec -u 70:70 "$c" sh -c 'echo stale > /backups/devops-20000101T000000Z.dump && touch -t 200001010000 /backups/devops-20000101T000000Z.dump && echo recent > /backups/devops-29990101T000000Z.dump'
docker exec -u 70:70 "$c" sh /backup/backup.sh once
listing="$(docker exec "$c" ls -1 /backups)"
echo "$listing"
grep -q 'devops-20000101T000000Z.dump' <<<"$listing" && fail "the 31-day-old dump was not pruned"
grep -q 'devops-29990101T000000Z.dump' <<<"$listing" || fail "a recent file was pruned"
docker exec "$c" rm -f /backups/devops-29990101T000000Z.dump
grep -q '^\.devops-.*partial$' <<<"$listing" && fail "a .partial file was left behind"
[[ "$(grep -c '^devops-.*\.dump$' <<<"$listing")" -ge 1 ]] || fail "no dump written"
modes="$(docker exec "$c" sh -c 'stat -c %a /backups/devops-2*.dump' | sort -u)"
[[ "$modes" == 600 ]] || fail "a dump is not mode 0600 (got: $modes)"
echo "ok: the dump is mode 0600"

step "a bad BACKUP_KEEP_DAYS stops backup.sh before it dumps or prunes anything"
# A day-old dump: any prune of "+0" minutes would delete it.
docker exec -u 70:70 "$c" sh -c 'echo day-old > /backups/devops-20991231T000000Z.dump &&
  touch -t "$(date -u -d "@$(($(date +%s) - 86400))" +%Y%m%d%H%M)" /backups/devops-20991231T000000Z.dump'
before_n="$(docker exec "$c" sh -c 'ls -1 /backups/devops-*.dump | wc -l')"
for bad in 0 abc -1 1.5 08 ' 30'; do
	rc=0
	docker exec -u 70:70 -e BACKUP_KEEP_DAYS="$bad" "$c" sh /backup/backup.sh once >"$scratch/keep.out" 2>&1 || rc=$?
	[[ $rc -ne 0 ]] || {
		cat "$scratch/keep.out"
		fail "BACKUP_KEEP_DAYS='$bad' was accepted"
	}
	grep -q 'BACKUP_KEEP_DAYS must be a whole number of days' "$scratch/keep.out" || {
		cat "$scratch/keep.out"
		fail "no clear message for BACKUP_KEEP_DAYS='$bad'"
	}
	docker exec "$c" test -f /backups/devops-20991231T000000Z.dump || fail "BACKUP_KEEP_DAYS='$bad' pruned the day-old dump"
	[[ "$(docker exec "$c" sh -c 'ls -1 /backups/devops-*.dump | wc -l')" == "$before_n" ]] ||
		fail "BACKUP_KEEP_DAYS='$bad' changed the dumps"
	echo "ok: BACKUP_KEEP_DAYS='$bad' -> exit $rc, nothing dumped or pruned"
done
docker exec "$c" rm -f /backups/devops-20991231T000000Z.dump

step "a truncated .partial is never renamed into place"
# pg_dump succeeds, then the .partial loses its second half (a disk-full or
# a cut write). The table of contents is intact, so `pg_restore --list`
# passes it; only reading every data block finds the cut.
docker exec "$c" sh -c 'mkdir -p /tmp/cutbin && cat > /tmp/cutbin/pg_dump <<'"'EOF'"'
#!/bin/sh
/usr/local/bin/pg_dump "$@" || exit $?
for f in /backups/.devops-*.dump.partial; do
	sz=$(wc -c <"$f")
	truncate -s $((sz / 2)) "$f"
done
EOF
chmod 755 /tmp/cutbin/pg_dump'
before_list="$(docker exec "$c" sh -c 'ls -1A /backups')"
rc=0
docker exec -u 70:70 -e PATH=/tmp/cutbin:/usr/local/bin:/usr/bin:/bin "$c" sh /backup/backup.sh once >"$scratch/cut.out" 2>&1 || rc=$?
cat "$scratch/cut.out"
((rc != 0)) || fail "backup.sh exited 0 with a truncated dump"
after_list="$(docker exec "$c" sh -c 'ls -1A /backups')"
[[ "$after_list" == "$before_list" ]] || {
	diff <(printf '%s\n' "$before_list") <(printf '%s\n' "$after_list") || true
	fail "a truncated dump was renamed into place (or its .partial was left)"
}
echo "ok: exit $rc, the backups directory is unchanged (no new dump, no .partial)"

step "restore latest into a scratch database and compare every table"
out="$("$here/restore.sh" --container "$c" --compare latest)"
echo "$out"
grep -q '^rows issues 3000$' <<<"$out" || fail "the restored issues count is not 3000"
grep -q '^rows comments 6000$' <<<"$out" || fail "the restored comments count is not 6000"
grep -q 'every table.s count equals the live database' <<<"$out" || fail "no compare result"
[[ -z "$(docker exec "$c" psql -X -tAc "SELECT datname FROM pg_database WHERE datname LIKE 'restore_%'")" ]] ||
	fail "the scratch database was not dropped"

step "restore a dump file from the host, too"
dump_name="$(docker exec "$c" sh -c 'ls -1t /backups/devops-*.dump | head -n 1')"
# /backups is a tmpfs here, which `docker cp` cannot read.
docker exec "$c" cat "$dump_name" >"$scratch/good.dump"
"$here/restore.sh" --container "$c" --name restore_hostfile "$scratch/good.dump" | tail -n 1

grep -q "migration version $version" <<<"$out" || fail "the restore did not report migration version $version"

step "restore a host file through the STACK's backup service (the rollback path, no --container)"
# A throwaway compose project from the real deploy/docker-compose.yml, only
# postgres + backup (+ volume-init): no build, no published port. Its name
# is unique and its volumes are this file's, so `down -v` removes only them.
stack="devops-restore-test-$$"
sed -e "s/^DEPLOY_PROJECT=.*/DEPLOY_PROJECT='$stack'/" "$here/.env.example" >"$scratch/stack.env"
chmod 600 "$scratch/stack.env"
docker compose -p "$stack" -f "$here/docker-compose.yml" --env-file "$scratch/stack.env" \
	up -d postgres backup >"$scratch/stack-up.out" 2>&1 || {
	cat "$scratch/stack-up.out"
	fail "could not start the test stack"
}
for _ in $(seq 1 60); do
	docker compose -p "$stack" -f "$here/docker-compose.yml" --env-file "$scratch/stack.env" \
		exec -T postgres psql -U devops -d devops -tAc 'select 1' >/dev/null 2>&1 && break
	sleep 1
done
# The same data in the stack's live database, so --compare can match it.
docker compose -p "$stack" -f "$here/docker-compose.yml" --env-file "$scratch/stack.env" \
	exec -T postgres pg_restore -U devops -d devops --no-owner --exit-on-error <"$scratch/good.dump"
sout="$(DEPLOY_ENV_FILE="$scratch/stack.env" "$here/restore.sh" --compare --name restore_viastack "$scratch/good.dump")"
echo "$sout" | tail -n 2
grep -q "every table.s count equals the live database" <<<"$sout" || fail "the stack-path restore did not match"
grep -q '^rows issues 3000$' <<<"$sout" || fail "the stack-path restore lost issues"

step "a truncated dump must fail"
head -c 20000 "$scratch/good.dump" >"$scratch/bad.dump"
if "$here/restore.sh" --container "$c" --name restore_bad "$scratch/bad.dump" >"$scratch/bad.out" 2>&1; then
	cat "$scratch/bad.out"
	fail "a truncated dump restored without an error"
fi
tail -n 2 "$scratch/bad.out"
[[ -z "$(docker exec "$c" psql -X -tAc "SELECT datname FROM pg_database WHERE datname = 'restore_bad'")" ]] ||
	fail "the failed restore left its scratch database"

step "a truncated dump with --keep must fail AND leave no scratch database"
# The rollback runs restore.sh --keep, then renames the kept database into
# place: a partial copy left behind would be swapped in.
rc=0
"$here/restore.sh" --container "$c" --keep --name restore_bad_keep "$scratch/bad.dump" >"$scratch/badkeep.out" 2>&1 || rc=$?
tail -n 2 "$scratch/badkeep.out"
((rc != 0)) || fail "a truncated dump restored with --keep without an error"
grep -q 'restore: OK' "$scratch/badkeep.out" && fail "a failed restore printed 'restore: OK'"
left="$(docker exec "$c" psql -X -tAc "SELECT datname FROM pg_database WHERE datname = 'restore_bad_keep'")"
[[ -z "$left" ]] || fail "the failed --keep restore left its scratch database $left (exit $rc)"
echo "ok: exit $rc, no scratch database left"

step "the rollback swap (docs/deploy.md) is one transaction"
# The same SQL as the runbook's swap block.
swap_sql='BEGIN;
ALTER DATABASE devops RENAME TO devops_before_rollback;
ALTER DATABASE devops_rollback RENAME TO devops;
COMMIT;'
dbs() { docker exec "$c" psql -X -tAc "SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database WHERE datname LIKE 'devops%'"; }
"$here/restore.sh" --container "$c" --keep --name devops_rollback "$scratch/good.dump" | tail -n 2
[[ "$(dbs)" == devops,devops_rollback ]] || fail "restore --keep after OK did not keep devops_rollback (have: $(dbs))"
# The FIRST rename succeeds and the SECOND fails (devops_rollback is not
# there): in one transaction nothing may have moved, so devops still exists.
docker exec "$c" psql -X -q -d postgres -c 'ALTER DATABASE devops_rollback RENAME TO devops_rollback_away'
rc=0
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 -d postgres <<<"$swap_sql" >"$scratch/swap1.out" 2>&1 || rc=$?
cat "$scratch/swap1.out"
((rc != 0)) || fail "the swap succeeded although devops_rollback was missing"
[[ "$(dbs)" == devops,devops_rollback_away ]] || fail "a failed swap moved a database (have: $(dbs))"
echo "ok: the second rename failed (exit $rc) and the first was undone: devops is still there"
docker exec "$c" psql -X -q -d postgres -c 'ALTER DATABASE devops_rollback_away RENAME TO devops_rollback'
docker exec "$c" psql -X -q -d devops -c 'INSERT INTO orgs (slug, name) VALUES ($$after-backup$$, $$written after the backup$$)'
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 -d postgres <<<"$swap_sql"
[[ "$(dbs)" == devops,devops_before_rollback ]] || fail "the swap did not land (have: $(dbs))"
[[ "$(docker exec "$c" psql -X -tAc "SELECT count(*) FROM orgs WHERE slug = 'after-backup'")" == 0 ]] ||
	fail "devops is not the restored copy after the swap"
[[ "$(docker exec "$c" psql -X -d devops_before_rollback -tAc "SELECT count(*) FROM orgs WHERE slug = 'after-backup'")" == 1 ]] ||
	fail "devops_before_rollback is not the old database"
echo "ok: the swap moved both names at once; devops is the restored copy, devops_before_rollback the old one"

printf '\ntest-restore: PASS\n'
