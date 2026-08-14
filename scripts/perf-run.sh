#!/usr/bin/env bash
#
# Measure the API against a real database, and print numbers rather than
# impressions.
#
# What this is for is the question a university actually has: when the debt
# report is running over a hundred thousand accounts, how long does a cashier
# wait for a balance? So the workers run a mixed scenario — a search, a
# statement, an account, a report — rather than one endpoint at a time. Measuring
# endpoints in isolation hides exactly the interaction that hurts.
#
#   scripts/perf-run.sh                       # against PERF_DB, seeding if empty
#   SCALE=medium scripts/perf-run.sh          # a bigger dataset
#   DURATION=60s WORKERS=16 scripts/perf-run.sh
#
# It uses its own database (flowed_perf by default) so a run cannot damage the
# development data, and leaves it in place afterwards so a second run does not
# pay to build it again.

cd "$(dirname "$0")/.."
. scripts/lib.sh

need psql
need createdb

PERF_DB="${PERF_DB:-flowed_perf}"
SCALE="${SCALE:-small}"
DURATION="${DURATION:-30s}"
WORKERS="${WORKERS:-8}"
# desk, reports, or mixed. The desk figure is the one that decides whether the
# queue at the counter moves; the mixed figure is what that becomes while the
# finance office is running its morning.
SCENARIO="${SCENARIO:-mixed}"
PERF_PORT="${PERF_PORT:-8091}"
# Not derived from the username: the password policy refuses a password that
# contains the account name, and the first attempt at this file used exactly
# that and failed at sign-in rather than at creation.
PERF_PASSWORD="${PERF_PASSWORD:-Qias-2026-تحميل-مقياس}"
OUT_DIR="${OUT_DIR:-./perf}"

mkdir -p "$OUT_DIR"

say "flowed performance run"
say "  database: $PERF_DB"
say "  scale:    $SCALE"
say "  load:     $WORKERS workers for $DURATION ($SCENARIO)"
say ""

# The binaries first: a stale build measured against a new schema is the most
# expensive way to get a wrong number.
go build -o ./bin/api ./cmd/api || die "building the server"
go build -o ./bin/perf ./cmd/perf || die "building the load harness"

createdb "$PERF_DB" 2>/dev/null || true
# DB_USER is exported by lib.sh from the same defaults the Makefile uses; the
# migrate command would otherwise fall back to "postgres", which is not the
# role a developer's database has.
DB_NAME="$PERF_DB" DB_USER="$DB_USER" go run ./cmd/migrate up >/dev/null || die "migrating $PERF_DB"

accounts="$(psql --dbname="$PERF_DB" --tuples-only --no-align \
	--command="SELECT count(*) FROM financial_account" 2>/dev/null || echo 0)"

if [ "${accounts:-0}" -lt 100 ]; then
	say "seeding (the database has $accounts accounts)"
	# The seeder hangs its dataset off real configuration rows, so the demo
	# load runs first — it is also the only thing that creates an operator to
	# sign in as.
	DB_NAME="$PERF_DB" DB_USER="$DB_USER" ./bin/api demo >/dev/null 2>&1 || die "loading the demo configuration"
	DB_NAME="$PERF_DB" DB_USER="$DB_USER" \
		SEED_ADMIN_USERNAME=perf SEED_ADMIN_PASSWORD="$PERF_PASSWORD" \
		./bin/api seed >/dev/null 2>&1 \
		|| die "creating the operator to sign in as (is PERF_PASSWORD strong enough?)"
	DB_NAME="$PERF_DB" DB_USER="$DB_USER" ./bin/api perf-seed "--scale=$SCALE" || die "seeding"

	# A credential somebody else set is must-change, and rightly: an
	# administrator who knows a working password for another account can act as
	# that account. The load harness is not a person, and this is a throwaway
	# database, so the flag is cleared here rather than teaching the harness to
	# negotiate a password change it would then have to remember.
	psql --dbname="$PERF_DB" --quiet --command="
		UPDATE app_user SET must_change_password = false WHERE username = 'perf'" >/dev/null
else
	say "using the existing dataset ($accounts accounts)"
fi

sizes="$(psql --dbname="$PERF_DB" --tuples-only --no-align --command="
	SELECT 'accounts ' || (SELECT count(*) FROM financial_account)
	    || ', installments ' || (SELECT count(*) FROM installment)
	    || ', payments ' || (SELECT count(*) FROM payment)
	    || ', on disk ' || pg_size_pretty(pg_database_size('$PERF_DB'))")"
say "  dataset:  $sizes"
say ""

# Rate limiting off: this measures the system, not the limiter, and the limiter
# has its own tests. Metrics off for the same reason — a scrape during a run
# would be measured as part of it.
DB_NAME="$PERF_DB" \
DB_USER="$DB_USER" \
HTTP_PORT="$PERF_PORT" \
HTTP_RATE_LIMIT_PER_MINUTE=0 \
OBS_METRICS_ENABLED=false \
LOG_LEVEL=warn \
	./bin/api serve >"$OUT_DIR/server.log" 2>&1 &
server_pid=$!

cleanup() {
	kill "$server_pid" 2>/dev/null || true
	wait "$server_pid" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for the port rather than sleeping a guess.
for attempt in $(seq 1 40); do
	if curl -fsS "http://127.0.0.1:$PERF_PORT/health" >/dev/null 2>&1; then
		break
	fi
	if [ "$attempt" = "40" ]; then
		tail -20 "$OUT_DIR/server.log" >&2
		die "the server did not come up"
	fi
	sleep 0.5
done

stamp="$(now_stamp)"
./bin/perf \
	-base "http://127.0.0.1:$PERF_PORT" \
	-user perf \
	-password "$PERF_PASSWORD" \
	-duration "$DURATION" \
	-workers "$WORKERS" \
	-scenario "$SCENARIO" \
	-json "$OUT_DIR/results-$stamp.json"
status=$?

say ""
say "results in $OUT_DIR/results-$stamp.json"
say "Plans for the slowest queries: scripts/perf-explain.sh"
exit $status
