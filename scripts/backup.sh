#!/usr/bin/env bash
#
# Take a backup and prove it is readable before saying so.
#
# The failure this is built against is the ordinary one: a nightly cron writes
# a file for two years, nobody reads it, and the morning it is needed the file
# turns out to be a truncated dump from a disk that filled up in March. So the
# archive is verified here, in the same run that produced it — pg_restore has
# to be able to list its contents, the checksum written beside it has to match
# the bytes on disk, and the row counts recorded in the manifest come from the
# live database at the moment of the dump so a restore can be compared against
# something.
#
# A custom-format dump is used rather than plain SQL: it can be restored
# selectively, restores in parallel, and pg_restore --list is a real structural
# check rather than a grep.
#
#   BACKUP_DIR      where archives go (default ./backups)
#   RETENTION_DAYS  prune older than this (default 30)
#   BACKUP_GPG_RECIPIENT  if set, the archive is encrypted to that key
#
# This covers the daily archive. Recovery to a point in time between archives
# is WAL archiving, which is a server setting rather than a script — see
# docs/operations/backup-restore.md.

cd "$(dirname "$0")/.."
. scripts/lib.sh

need pg_dump
need pg_restore
need psql

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR" 2>/dev/null || true

stamp="$(now_stamp)"
base="$BACKUP_DIR/${DB_NAME}-${stamp}"
archive="${base}.dump"
manifest="${base}.manifest.json"

say "flowed backup"
say "  database:  $DB_NAME@$DB_HOST:$DB_PORT"
say "  archive:   $archive"

# Facts recorded before the dump, so the manifest describes what went in.
schema_version="$(psql_value "SELECT coalesce(max(version), 0) FROM schema_migrations")"
server_version="$(psql_value "SHOW server_version")"
students="$(psql_value "SELECT count(*) FROM student")"
payments="$(psql_value "SELECT count(*) FROM payment")"
accounts="$(psql_value "SELECT count(*) FROM financial_account")"
audit_rows="$(psql_value "SELECT count(*) FROM audit_log")"
audit_head="$(psql_value "SELECT coalesce(max(sequence_no), 0) FROM audit_log")"

started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
seconds_start=$(date +%s)

# --no-owner and --no-privileges: the drill restores into a scratch database
# owned by whoever runs it, and a restore that fails on a missing role is a
# restore that fails at 3am for a reason unrelated to the data.
pg_dump \
	--dbname="$DB_NAME" \
	--format=custom \
	--compress=6 \
	--no-owner \
	--no-privileges \
	--file="$archive"

seconds_end=$(date +%s)
finished="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
duration=$((seconds_end - seconds_start))

# --- verification, in the same run -----------------------------------------

# 1. The archive is structurally readable and contains the tables that carry
#    money. A dump that lists no financial_account is not a backup of this
#    system, whatever its size.
listing="$(pg_restore --list "$archive")" || die "the archive cannot be read by pg_restore"
for table in financial_account payment installment audit_log student; do
	printf '%s' "$listing" | grep -q "TABLE DATA public $table " \
		|| die "the archive contains no data for $table"
done

sum="$(checksum "$archive")"
bytes="$(file_bytes "$archive")"
[ "$bytes" -gt 0 ] || die "the archive is empty"

if [ -n "${BACKUP_GPG_RECIPIENT:-}" ]; then
	need gpg
	gpg --batch --yes --encrypt --recipient "$BACKUP_GPG_RECIPIENT" --output "${archive}.gpg" "$archive"
	rm -f "$archive"
	archive="${archive}.gpg"
	sum="$(checksum "$archive")"
	bytes="$(file_bytes "$archive")"
	say "  encrypted to $BACKUP_GPG_RECIPIENT"
fi

cat >"$manifest" <<JSON
{
  "archive": "$(basename "$archive")",
  "database": "$DB_NAME",
  "host": "$DB_HOST",
  "server_version": "$server_version",
  "schema_migration_version": $schema_version,
  "started_at": "$started",
  "finished_at": "$finished",
  "duration_seconds": $duration,
  "bytes": $bytes,
  "sha256": "$sum",
  "encrypted": $([ -n "${BACKUP_GPG_RECIPIENT:-}" ] && echo true || echo false),
  "verified_listing": true,
  "row_counts": {
    "student": $students,
    "financial_account": $accounts,
    "payment": $payments,
    "audit_log": $audit_rows
  },
  "audit_head_sequence": $audit_head
}
JSON

chmod 600 "$archive" "$manifest" 2>/dev/null || true

# --- retention --------------------------------------------------------------
#
# Age alone is not enough: keep the newest RETENTION_MIN_KEEP whatever their
# age, so a system that has been quiet for a month is not left with nothing.

# Written as a read loop rather than with mapfile: the macOS bash a developer
# runs this on is 3.2, which has neither mapfile nor readarray.
pruned=0
index=0
while IFS= read -r old; do
	[ -n "$old" ] || continue
	index=$((index + 1))
	if [ "$index" -le "$RETENTION_MIN_KEEP" ]; then
		continue
	fi
	if [ -n "$(find "$old" -mtime +"$RETENTION_DAYS" 2>/dev/null)" ]; then
		rm -f "$old" "${old%.dump*}.manifest.json"
		pruned=$((pruned + 1))
	fi
done <<EOF
$(ls -1t "$BACKUP_DIR"/${DB_NAME}-*.dump* 2>/dev/null || true)
EOF

say "  size:      $bytes bytes in ${duration}s"
say "  sha256:    $sum"
say "  schema:    migration $schema_version, server $server_version"
say "  contents:  $students students, $accounts accounts, $payments payments, $audit_rows audit rows"
[ "$pruned" -gt 0 ] && say "  pruned:    $pruned archive(s) older than $RETENTION_DAYS days"
say "  verified:  pg_restore can read it and it carries every financial table"

# What this archive cannot do is recover the hours since it was taken. That is
# WAL archiving, and whether it is on is a server setting nobody sees unless
# something says so — so the backup says so, every night.
archive_mode="$(psql_value "SHOW archive_mode")"
if [ "$archive_mode" = "on" ]; then
	failed="$(psql_value "SELECT failed_count FROM pg_stat_archiver")"
	last_ok="$(psql_value "SELECT coalesce(last_archived_time::text, 'never') FROM pg_stat_archiver")"
	say "  wal:       archiving on, last archived $last_ok, $failed failures"
	if [ "${failed:-0}" != "0" ]; then
		warn "WAL archiving has failed $failed times; point-in-time recovery is not intact"
	fi
else
	warn "WAL archiving is off: recovery can only go back to a nightly archive, so a
         failure at 16:00 loses the day's collection. See docs/operations/backup-restore.md."
fi
say ""
say "Restoring it is the only proof that matters: make restore-drill"
