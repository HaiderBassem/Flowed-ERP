#!/usr/bin/env bash
#
# How much does the audit trail cost as it grows?
#
# The question behind this is whether the trail has to be partitioned, archived
# or pruned, and the honest way to answer it is to write a few hundred thousand
# entries and measure. It writes into its own database, so nothing here can
# touch real data.
#
#   scripts/audit-bench.sh              # 500,000 entries
#   ENTRIES=2000000 scripts/audit-bench.sh
#
# The entries go through the real chain trigger, so what is measured is the
# actual write path: hash, advisory lock, index maintenance.

cd "$(dirname "$0")/.."
. scripts/lib.sh

need psql
need createdb

BENCH_DB="${BENCH_DB:-flowed_audit_bench}"
ENTRIES="${ENTRIES:-500000}"
BATCH="${BATCH:-100000}"

say "audit growth benchmark"
say "  database: $BENCH_DB"
say "  entries:  $ENTRIES"
say ""

dropdb --if-exists "$BENCH_DB" >/dev/null 2>&1 || true
createdb "$BENCH_DB" || die "creating $BENCH_DB"
DB_NAME="$BENCH_DB" DB_USER="$DB_USER" go run ./cmd/migrate up >/dev/null || die "migrating"

export PGDATABASE="$BENCH_DB"

written=0
while [ "$written" -lt "$ENTRIES" ]; do
	size=$BATCH
	if [ $((written + size)) -gt "$ENTRIES" ]; then
		size=$((ENTRIES - written))
	fi

	start=$(date +%s)
	psql --quiet --command="
		INSERT INTO audit_log (id, entity_type, action, actor_username, occurred_at, metadata)
		SELECT gen_random_uuid(), 'payment', 'payment.recorded', 'bench',
		       now() - (i || ' minutes')::interval,
		       jsonb_build_object('amount', 250000, 'receipt', 'R-' || i)
		FROM generate_series($((written + 1)), $((written + size))) i" >/dev/null \
		|| die "writing entries"
	end=$(date +%s)

	written=$((written + size))
	elapsed=$((end - start))
	[ "$elapsed" -eq 0 ] && elapsed=1
	say "  $written entries (last batch: $size in ${elapsed}s, ~$((size / elapsed))/s)"
done

say ""
psql --command="
	SELECT count(*) AS entries,
	       pg_size_pretty(pg_total_relation_size('audit_log')) AS total,
	       pg_size_pretty(pg_relation_size('audit_log'))       AS heap,
	       pg_size_pretty(pg_indexes_size('audit_log'))        AS indexes,
	       (pg_total_relation_size('audit_log') / count(*))::int AS bytes_each
	FROM audit_log"

say "full chain verification:"
psql --command="\timing on" --command="SELECT count(*) AS problems FROM verify_audit_chain(0)" \
	| grep -E "problems|[0-9]|Time" | head -5

say ""
say "a year of a 20,000-student university is on the order of 300,000 entries;"
say "divide the size above by $ENTRIES to project."
say ""
say "Drop it with: dropdb $BENCH_DB"
