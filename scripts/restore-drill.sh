#!/usr/bin/env bash
#
# Restore a backup into a scratch database and check what came out.
#
# A backup nobody has restored is a hypothesis. This runs the real restore,
# times it — that time is the recovery objective, measured rather than assumed
# — and then asks the restored copy the four questions the system asks of
# itself: do the account caches match the transactions, do the installments,
# has any payment been refunded beyond what it took, and is the audit chain
# unbroken. A restore that produces a database failing any of them has restored
# something, but not this system.
#
#   scripts/restore-drill.sh [archive]
#
# With no argument it takes the newest archive in BACKUP_DIR. The scratch
# database is dropped afterwards unless KEEP_RESTORE=1.

cd "$(dirname "$0")/.."
. scripts/lib.sh

need pg_restore
need psql
need createdb
need dropdb

archive="${1:-}"
if [ -z "$archive" ]; then
	archive="$(ls -1t "$BACKUP_DIR"/${DB_NAME}-*.dump 2>/dev/null | head -1 || true)"
fi
[ -n "$archive" ] || die "no archive given and none found in $BACKUP_DIR — run make backup first"
[ -f "$archive" ] || die "no such archive: $archive"

manifest="${archive%.dump}.manifest.json"
scratch="${DB_NAME}_drill_$(date +%s)"

say "flowed restore drill"
say "  archive:  $archive"
say "  into:     $scratch"

# The checksum first: if the bytes moved, nothing after this means anything.
if [ -f "$manifest" ]; then
	recorded="$(grep -o '"sha256": *"[^"]*"' "$manifest" | sed 's/.*"\([^"]*\)"$/\1/')"
	actual="$(checksum "$archive")"
	if [ "$recorded" != "$actual" ]; then
		die "checksum mismatch: the manifest records $recorded, the file is $actual"
	fi
	say "  checksum: matches the manifest"
else
	warn "no manifest beside this archive; the checksum cannot be verified"
fi

cleanup() {
	if [ "${KEEP_RESTORE:-0}" = "1" ]; then
		say "  scratch database kept: $scratch"
		return
	fi
	dropdb --if-exists "$scratch" >/dev/null 2>&1 || true
}
trap cleanup EXIT

createdb "$scratch"

seconds_start=$(date +%s)
# --jobs: a restore that takes four hours single-threaded is a four-hour
# outage. Parallel restore is the difference between a morning and a day.
# Errors are not ignored: --exit-on-error means a drill that half-works fails.
pg_restore \
	--dbname="$scratch" \
	--no-owner \
	--no-privileges \
	--jobs=4 \
	--exit-on-error \
	"$archive"
seconds_end=$(date +%s)
restore_seconds=$((seconds_end - seconds_start))

failures=0
check() {
	local label="$1" query="$2" expect="$3"
	local got
	got="$(psql_value "$query" "$scratch")"
	if [ "$got" = "$expect" ]; then
		say "  ok    $label"
	else
		say "  FAIL  $label: got $got, want $expect"
		failures=$((failures + 1))
	fi
}

say ""
say "  restored in ${restore_seconds}s — that is the recovery time for this dataset"
say ""

# The schema is this build's schema. A restore of an older dump into a newer
# binary is a real scenario and it has to be noticed here rather than at boot.
restored_version="$(psql_value "SELECT coalesce(max(version), 0) FROM schema_migrations" "$scratch")"
# From the migration files when they are on disk, and from the binary when they
# are not. In the container image the migrations are embedded rather than
# shipped as files, so reading the directory made the drill die here — after
# reporting a successful restore and before checking a single invariant, which
# is the half of the drill that matters.
if ls migrations/*.up.sql >/dev/null 2>&1; then
	expected_version="$(ls -1 migrations/*.up.sql | tail -1 | sed 's#.*/0*\([0-9]*\)_.*#\1#')"
else
	# Never fatal: under `set -o pipefail` a failing binary here would end the
	# drill before it checked a single invariant, which is the half that
	# matters. An unknown expectation prints a note; a broken restore must not
	# be hidden behind it.
	expected_version="$(./migrate status 2>/dev/null | awk '/^[0-9][0-9]*/ { v = $1 } END { print v + 0 }')" \
		|| expected_version="unknown"
fi
if [ "$restored_version" = "$expected_version" ]; then
	say "  ok    schema is at migration $restored_version, matching this build"
else
	say "  note  restored schema is at migration $restored_version; this build carries $expected_version"
	say "        (run the migrations against the restored copy before serving from it)"
fi

check "account caches match their transactions" \
	"SELECT count(*) FROM v_account_reconciliation" 0
check "installment caches match their allocations" \
	"SELECT count(*) FROM v_installment_reconciliation" 0
check "no payment is refunded beyond what it took" \
	"SELECT count(*) FROM v_over_refunded_payments" 0
check "the audit chain verifies end to end" \
	"SELECT count(*) FROM verify_audit_chain(0)" 0

# Row counts against the manifest: a restore that silently dropped a table
# passes every integrity view, because an empty table reconciles perfectly.
if [ -f "$manifest" ]; then
	for table in student financial_account payment audit_log; do
		recorded="$(grep -o "\"$table\": *[0-9]*" "$manifest" | grep -o '[0-9]*$' || true)"
		[ -n "$recorded" ] || continue
		got="$(psql_value "SELECT count(*) FROM $table" "$scratch")"
		if [ "$got" = "$recorded" ]; then
			say "  ok    $table restored $got rows, as recorded"
		else
			say "  FAIL  $table restored $got rows, the manifest recorded $recorded"
			failures=$((failures + 1))
		fi
	done
fi

say ""
if [ "$failures" -gt 0 ]; then
	die "$failures check(s) failed — this backup is not one the university can rely on"
fi
say "Drill passed: the archive restores into a database that reconciles."
say "Recovery time for this dataset: ${restore_seconds}s."
