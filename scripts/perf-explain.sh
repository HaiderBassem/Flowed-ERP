#!/usr/bin/env bash
#
# Print the plans for the queries the timings say are slowest.
#
# A latency number tells you something is slow; a plan tells you why, and the
# difference decides whether the answer is an index, a rewrite, or accepting
# the cost. These are run with ANALYZE against the real dataset, because a plan
# from an empty database is a plan for a different query.
#
#   PERF_DB=flowed_perf scripts/perf-explain.sh
#   scripts/perf-explain.sh > plans.txt

cd "$(dirname "$0")/.."
. scripts/lib.sh

need psql

PERF_DB="${PERF_DB:-flowed_perf}"
export PGDATABASE="$PERF_DB"

year="$(psql --tuples-only --no-align \
	--command="SELECT id FROM academic_year ORDER BY start_date DESC LIMIT 1")"
[ -n "$year" ] || die "no academic year in $PERF_DB; run scripts/perf-run.sh first"

say "plans against $PERF_DB"
psql --tuples-only --no-align --command="
	SELECT '  ' || (SELECT count(*) FROM financial_account) || ' accounts, '
	    || (SELECT count(*) FROM student) || ' students, '
	    || (SELECT count(*) FROM payment) || ' payments'"
say ""

explain() {
	local title="$1" sql="$2"
	printf '\n===== %s\n' "$title"
	psql --command="EXPLAIN (ANALYZE, BUFFERS, COSTS OFF) $sql"
}

# The debt report: the heaviest read in the system, and the one the finance
# office runs while the desks are open.
explain "debt report, one page of the current year" "
	SELECT v.account_id, v.remaining, count(*) OVER () AS total
	FROM v_debt v
	JOIN department d ON d.id = v.department_id
	WHERE v.academic_year_id = '$year'
	ORDER BY v.remaining DESC, v.student_no
	LIMIT 50"

# The screen a cashier opens with a student in front of them.
explain "student search by name" "
	SELECT s.id, s.student_no, count(*) OVER () AS total
	FROM student s
	WHERE s.full_name_norm LIKE '%' || normalize_arabic('محمد') || '%'
	ORDER BY s.student_no
	LIMIT 20"

explain "student by number" "
	SELECT s.id FROM student s WHERE s.student_no = 'PERF00000001'"

# The listing screens. Both pay for an exact total over the whole match set,
# which is where their time goes rather than in reading the page.
explain "student listing, one page with an exact total" "
	SELECT s.id, count(*) OVER () AS total
	FROM student s
	ORDER BY s.student_no
	LIMIT 20"

explain "account balance for one account" "
	SELECT * FROM v_account_balance
	WHERE account_id = (SELECT id FROM financial_account LIMIT 1)"

explain "department summary for a year" "
	SELECT * FROM v_year_department_summary WHERE academic_year_id = '$year'"

explain "aging buckets for a year" "
	SELECT bucket, count(*), sum(remaining)
	FROM (
	    SELECT CASE
	        WHEN vi.due_date > current_date THEN 'not_due'
	        WHEN current_date - vi.due_date <= 30 THEN '1_30'
	        WHEN current_date - vi.due_date <= 60 THEN '31_60'
	        ELSE 'over_60'
	    END AS bucket, vi.remaining
	    FROM v_installment_status vi
	    WHERE vi.academic_year_id = '$year' AND vi.remaining > 0
	) buckets
	GROUP BY bucket"

say ""
say "Reading these: 'loops=N' on a lateral is a per-account subquery, and N is"
say "the number of accounts scanned before the LIMIT applies. A high buffer hit"
say "count with a low row count is a query reading far more than it returns."
