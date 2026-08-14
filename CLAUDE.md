# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go backend for tuition fees, discounts, installments and payments at an
**Iraqi university**. It is a financial system, not a CRUD app: money is
integer dinars, financial rows are append-only, and several rules exist
specifically because the naive version of them loses or duplicates money.

Two documents carry the reasoning:

- `DOMAIN-DESIGN.md` — the full domain analysis this implements (23 sections,
  written before any code). Read the section relevant to what you are changing.
- `README.md` — architecture and the rationale behind each load-bearing choice.

## Commands

```bash
make help              # every target, self-documenting
make run               # build then exec the binary (not `go run` — see below)
make stop              # kill a server left running from an earlier session
make test              # full suite, race detector on; needs a database
make test-short        # unit only, no database
make check             # what CI runs: lint + test + migrate-validate
make demo-reset        # rebuild the database and load the exploration dataset
```

Single test:

```bash
go test ./internal/domain/money/ -run TestApplyRate -v
go test ./internal/app/ -run Credit -v
```

Migrations:

```bash
make migrate-up
make migrate-down STEPS=2
make migrate-status            # every migration and its state
make migrate-validate          # does the database match this build?
make migrate-new NAME=add_x    # scaffolds the up/down pair
```

Operational commands on the binary itself:

```bash
api serve | healthcheck | seed | create-user <user> <name> <roles> | demo
```

Local defaults live in the Makefile (`DB_NAME=flowed_dev`). `.env.example`
documents every variable.

### Two gotchas that cost time

`make run` builds and **execs** rather than using `go run`. `go run` starts the
program as a child, and interrupting the parent can leave that child alive
holding its connection pool — which then blocks `make db-drop` with a message
about "other users" that looks like a permissions problem. `make db-drop`
terminates leftover connections before dropping, and `make stop` kills strays.

A **stale binary** is the most common cause of "my fix did nothing". Rebuild
before testing against a running server.

## Architecture

Dependencies point inward. `internal/domain` imports nothing from the project;
`internal/app` depends on `internal/port`, never on `internal/adapter`.

```
internal/domain    pure rules — no database, no HTTP, no framework
internal/port      repository + transaction interfaces (the domain's view of storage)
internal/app       one method per domain command, each its own transaction
internal/adapter   postgres (pgx), httpapi (Gin), receipt (printable documents)
internal/platform  config, logger, pg, migrate, auth, httpx, observability
migrations         versioned SQL, embedded into the binary
```

`cmd/api/main.go` `buildEngine` is the only place concrete types meet their
interfaces. Handler groups each own their routes through a `Register(g)` method
and are wired in `RouterDeps`.

### The rules that must not be broken

These are not style preferences. Each one exists because its absence has a
specific, traceable failure.

**Money is `money.Amount` (int64 whole dinars) everywhere.** Never float, never
`NUMERIC`. Percentages are `money.BasisPoints` with half-up integer rounding, so
a discount recomputed in five years is bit-identical. `BIGINT` in the schema —
plain `INTEGER` overflows at 2.1 billion IQD, which a college-year total passes.

**The financial unit is the enrollment, not the student.** A student row holds
identity only. Anything whose value could differ between two academic years
lives on an enrollment.

**Configuration freezes into history at account generation.** Fee components are
copied into `fee_snapshot_line`; each discount application stores its computed
amount and a foreign key to the exact definition *version*. Nothing afterwards
recomputes from configuration. There is no path from a config change to a
historical number because no historical row points at "the current value".

**The frozen net never moves.** `account.net_total` is set once. Every later
change is a signed `account_adjustment` row, and what is owed is
`net_total + Σ adjustments` (`Account.EffectiveNet()`).

**Nothing financial is edited or deleted.** Database triggers enforce it, not
just Go — the app connects as a role that owns its tables, so a guard living
only in Go is one a psql session walks past. Corrections are void / refund /
adjustment, each a new row.

**Lock order is always: account → academic year → number series.** One order
everywhere is why concurrent commands cannot deadlock. The year lock is not
decorative: under read-committed a payment can read "open", the close can
commit, and the payment then commits into shut books.

**Every money command follows the same shape** (documented at the top of
`internal/app/app.go`): check authority → open transaction → take locks in order
→ re-read and re-check preconditions under the lock → mutate through domain
methods → append the audit entry.

**Void requires zero posted refunds.** Refund 400,000 of a million then void the
whole payment and the university has paid out 1,400,000 against a million
received. Checked when the request is raised and again under the lock.

**Refund reversals are scoped to the refunded payment's own allocations.**
Unwinding "the newest installments" across the account would let a refund of
payment A strip funding payment B provided.

**A year has two closes.** `financially_closed` freezes money while academic
recording stays open, because second-round (دور ثاني) results arrive weeks after
the treasury shuts its books. **Closing a year does not stop collection** of its
debt — the payment posts against the current open year and allocates to the old
year's installments.

**Overdue is never stored.** It is `due_date < today` with money outstanding.

**Money counters are incremented after the commit, never inside it.** A counter
bumped inside the transaction counts money a rollback never took, and a
dashboard that disagrees with the ledger costs more hours than it saves. The
call sits after `Tx.Write` returned nil, in `payment_service.go`,
`refund_service.go` and `account_service.go`.

**Nothing that identifies a student may become a metric label or a span
attribute.** A label outlives the request in a store with none of the
database's access control. The rules that follow from it: HTTP metrics are
keyed by matched route template and never by path (`/students/:id`, and one
constant for every unmatched request); the pgx tracer records the statement and
never the arguments; a failed statement is recorded as its SQLSTATE and
constraint name, never through `span.RecordError`, because PostgreSQL's DETAIL
line quotes the offending values — `Key (student_no)=(2024001) already exists`.
`pgxtrace_internal_test.go` is what keeps that true.

### PostgreSQL is load-bearing

Partial unique indexes encode business rules directly (one live enrollment per
student per year; one live account per enrollment). `NULLS NOT DISTINCT` makes
fee-policy scope uniqueness actually bite. Generated columns compute the
Arabic-folded search fields and the specificity score. A deferred constraint
trigger lets the supersede pair be written in the only order the indexes allow
while still refusing an orphan at commit. Trigram GIN indexes answer
`LIKE '%محمد%'` on folded names.

Before changing a query, check the view and index comments in
`migrations/000009_reporting_views.up.sql` — several encode a rule (notably
**counts come from `v_enrollment_effective`, money comes from all non-cancelled
accounts**; mixing them double-counts or loses a superseded student's cash).

The API rate limiter is a token bucket in PostgreSQL (`rate_limit_take`,
migration 000012) rather than in Redis: the university runs this database and
does not run Redis, and one more thing that can be down is one more way the
desks stop. Three properties are load-bearing and each is tested.
`rate_limit_bucket` is **UNLOGGED** — logged, it would be a WAL record and a
replication round per admitted request, for state whose value expires within
one window. `rate_limit_take` must be called **outside any transaction** and
**once per statement**: it holds an ordinary row lock until the caller commits,
and several calls inside one statement share a snapshot, so each behaves like
the first and every one is admitted — the limiter keeps answering and quietly
stops limiting. The middleware keeps its in-memory bucket as a pre-filter and
as the fail-open fallback, so a database blip degrades the limit to
per-process rather than turning it into an outage; the degradation is counted
on `flowed_ratelimit_decisions_total{tier="degraded"}`.

### Observability

Metrics are pulled, traces are pushed. `/metrics` is Prometheus text on **its
own listener**, loopback by default — the scrape surface enumerates every route
and its error rate, and the bind address is its only access control. Tracing is
OTLP over HTTP and off unless a collector exists; the endpoint's `/v1/traces`
path is completed in code, because the exporter posts to the URL exactly as
given and a bare base URL fails silently.

`observability.Provider` and the `*Metrics` it hands out are **safe when nil**.
That is deliberate: services record unconditionally, because a `if metrics !=
nil` at the call site is a guard somebody eventually forgets, and the metric
that then stops being emitted is discovered on the day it was needed.

### Arabic

`normalize_arabic()` folds orthographic variation (tashkeel, tatweel, hamza
carriers, ta marbuta, alef maqsura) and is applied on write via generated
columns **and** to the search term on read — both sides folded identically.
Never rely on collation for this.

`money.SpellArabic` writes amounts in words for receipts (التفقيط). The grammar
is intricate and already correct — read the comments in
`internal/domain/money/arabic.go` before touching it. Receipts print Western
digits even in Arabic pages: ٠ and 0 are confusable on a bad print.

## Testing

The domain tests carry the weight; they are where the money rules live, and they
run without a database. `test/integration` goes at the schema directly to prove
guarantees are in the database rather than only in Go, including a
ten-goroutine test that the account row lock prevents lost updates and another
that ten concurrent connections cannot between them spend the rate-limit budget
more than once.

After any change touching money, these four must return zero:

```bash
psql flowed_dev -c "SELECT count(*) FROM v_account_reconciliation"
psql flowed_dev -c "SELECT count(*) FROM v_installment_reconciliation"
psql flowed_dev -c "SELECT count(*) FROM v_over_refunded_payments"
psql flowed_dev -c "SELECT count(*) FROM verify_audit_chain(0)"
```

The demo dataset (`make demo-reset`) is generated **through the real commands**,
so a successful load is itself evidence the system works. It plants every
awkward case on purpose — see the table in `README.md` for which student number
demonstrates what.

## Migrations

Applied migrations are **checksum-verified on every run**; editing one that
production has applied is caught at start-up. Never edit an applied migration —
write a new one. Down scripts are mandatory. Out-of-order versions are refused.
`api serve` validates the schema at boot and refuses to serve on a mismatch.

## Code style in this repo

Comments explain **why**, never what. `// get student by id` above `GetByID` is
noise; a comment explaining why a lock is taken in a particular order is the
point. Several comments name the specific failure a rule prevents — keep that
habit when adding rules.

Errors are `shared.Error` with a stable machine code and a `Kind` that maps to a
status; handlers switch on the kind and never parse messages. Refusals should
tell the operator what to do instead (`WithDetail("remedy", ...)`).

## Status and known gaps

Complete and running: 12 migrations, ~99 HTTP routes, 225 test functions, all
green under the race detector. Reports, configuration administration, bulk
promotion and import, cashier sessions, the background scheduler, printable
Arabic receipts, Prometheus metrics with OTLP tracing, and a cross-replica rate
limiter are all in.

Nothing on the original gap list is still open. What is worth doing next, if
anything:

- alert rules to go with the metrics. The instruments exist and nothing reads
  them yet; the ones that earn an alert are
  `flowed_payment_voided_total` climbing against `flowed_payment_posted_total`
  (a cashier reversing receipts), `flowed_db_pool_acquires_empty_total` moving
  at all (desks queueing behind a report), `flowed_ratelimit_decisions_total`
  with `tier="degraded"` (the limit has quietly reverted to per-process), and
  `flowed_scheduler_job_runs_total{outcome="defect"}` (reconciliation found
  drift).
- the money counters restart at zero with the process, as Prometheus counters
  are meant to. Anything that must survive a restart is a report, not a metric.

Ruled out on purpose, and not to be proposed again as future work:

- **Integration with an external general ledger** (ERPNext or any other).
  Flowed stays the system of record for fees and collection.
- **Moving the JSON tags out of `internal/port/reporting.go`.** Revisited and
  deliberately left; the reasoning is in the file's own header comment. Thirty
  structs and ~700 lines of field-for-field mapping on a read-only path, whose
  only failure mode is the drift the rule exists to prevent. `import_views.go`
  remains the right pattern for port structs the application *acts* on, and
  that comment names the distinction.
