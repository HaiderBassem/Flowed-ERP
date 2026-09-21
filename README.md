# Flowed — Iraqi University Tuition & Installment Backend

A Go backend for managing student fees, discounts, installments and payments at
an Iraqi university. Built around one idea: **the financial unit is the
enrollment, not the student.** A person studies for six years; each of those
years has its own prices, its own discounts, and its own debt, and collapsing
them into one balance makes it impossible to answer what a student owed for
2024-2025 three years later.

The domain analysis this implements is in [DOMAIN-DESIGN.md](DOMAIN-DESIGN.md).

## Stack

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.26 | |
| HTTP | Gin | |
| Database | PostgreSQL 18 | Partial unique indexes, `NULLS NOT DISTINCT`, generated columns, deferred constraint triggers and trigram search all do real work here — see below |
| Driver | pgx v5, native | No `database/sql`: it erases pgx's typed parameters and makes row-locking and batch APIs awkward, and this workload depends on both |
| Migrations | Custom engine | Checksum verification, advisory locking, dirty-state tracking, mandatory down scripts |
| Money | `int64` whole dinars | Never float, never `NUMERIC`, not at any layer |
| Telemetry | OpenTelemetry | Metrics pulled as Prometheus text, traces pushed over OTLP/HTTP. One SDK for both, and no gRPC to get through a university network |
| Rate limiting | Token bucket in PostgreSQL | The database is already there; Redis would be a second thing to keep alive, patch and back up |

## Getting started

```bash
make db-create && make migrate-up && make run
```

Configuration comes from the environment; copy `.env.example` and adjust. To
run PostgreSQL in Docker instead of locally:

```bash
make docker-up && make migrate-up && make run
```

Useful targets: `make test`, `make check`, `make migrate-status`,
`make db-reset`, `make verify-audit`, `make help`.

The API listens on `:8080` and the Prometheus endpoint on its own listener at
`127.0.0.1:9464/metrics` — separate on purpose, see
[Metrics and tracing](#metrics-and-tracing).

## Architecture

```
cmd/api          HTTP server
cmd/migrate      migration CLI

internal/domain  pure business rules — no database, no HTTP, no framework
  money            Amount (int64 IQD) and BasisPoints, with exact half-up arithmetic
  shared           errors, identifiers, actor and roles, calendar dates
  student          identity, and only identity
  academic         academic year, enrollment, hosting, reference data
  billing          fee policy, account, installment plan, allocation engine
  discount         definitions, grants, and the stacking engine
  payment          payments, refunds, voids

internal/port    repository and transaction interfaces — the domain's view of storage
internal/app     one method per domain command, each its own transaction
internal/adapter
  postgres         pgx implementations of the ports
  httpapi          Gin handlers, DTOs, routing
internal/platform
  config logger pg migrate auth httpx observability

migrations       versioned SQL, embedded into the binary
```

Dependencies point inward. `domain` imports nothing from the project;
`app` depends on `port`, never on `adapter`. The financial rules are testable
without a database, which is why the discount and allocation engines have
thorough unit tests and no fixtures.

## The parts that carry the weight

### Money is an integer, everywhere

`money.Amount` is an `int64` of whole dinars. IQD has no circulating subunit,
so a dinar is the atom. Percentages are `money.BasisPoints` — 1000 is ten
percent — and `ApplyRate` rounds half-up in pure integer arithmetic, so a
discount computed today and recomputed in five years is bit-identical.

The JSON decoder rejects a fractional amount rather than truncating it. The
database columns are `BIGINT`, never `NUMERIC` and never `DOUBLE PRECISION`.

### Configuration becomes history at one moment

Fee policies and discount definitions are versioned configuration. When an
account is generated, the resolved fee components are copied into it as frozen
snapshot lines, and each discount is materialised with its computed amount and
a foreign key to the exact definition version used.

Nothing afterwards recomputes from configuration. Raising next year's
teachers'-children discount from twenty to twenty-five percent publishes a new
version row; last year's applications still point at the old one, which the
database refuses to let anyone edit. There is no path from a configuration
change to a historical number, because no historical row holds a reference to
"the current value".

### The frozen net never moves

An account's `net_total` is set once. Every later change to what is owed — a
retroactive discount, a balance transferred from a superseded enrollment, a
correction to a closed year — is a signed `account_adjustment` row, and the
amount actually owed is `net_total + Σ adjustments`.

This is what keeps "the fee was two million, and here is every reason it is now
one" answerable, instead of leaving a single rewritten number nobody can
explain.

### Nothing financial is ever edited or deleted

Posted payments, refunds, allocations, snapshot lines and discount
applications are append-only. A mistake becomes a void; returned money becomes
a refund; a changed obligation becomes an adjustment. Database triggers enforce
this, not just application code — the application connects as a role that owns
its own tables, so a guard that lives only in Go is a guard a psql session
walks past.

A 500,000 payment with a 100,000 refund stays two rows and a computed net of
400,000. It never becomes a 400,000 payment.

### Locks, in one order, always

Every money-mutating command takes row locks in the same sequence: **account,
then academic year, then number series.** One order everywhere means two
concurrent commands cannot deadlock against each other.

The year lock is not decorative. Under read-committed, a payment can read
"open", the close command can commit, and the payment can then commit into a
year whose books were just shut. `SELECT ... FOR UPDATE` on the year row
serialises the two.

### Idempotency that does not lie

Every payment carries a client-generated idempotency key with a unique index.
A retry returns the original receipt rather than collecting twice.

Two refinements matter. The key is stored with a hash of the request payload,
so a client that reuses a key with a different amount gets a hard error instead
of a receipt for the wrong collection. And a replay reports the payment's
current status — if it was voided in the meantime, the client must not print a
receipt for a collection that no longer exists.

Beneath that sits a heuristic for the case a key cannot cover: the same
account, amount and method within five minutes warns, and the cashier must
confirm the collection is genuinely separate. The override is audited.

### Void and refund are different things

| | Void | Refund |
|---|---|---|
| Means | The payment should never have existed | The payment was valid; money is going back |
| Scope | Full reversal only | Partial or full |
| Who | Cashier raises a request, finance manager executes it | Requester and approver must differ |
| Blocked when | The payment already carries a posted refund | The total would exceed what was collected |

That last row is not a detail. Refund 400,000 of a 1,000,000 payment, then void
the whole payment, and the university has paid out 1,400,000 against a million
received. Void requires zero posted refunds, checked when the request is raised
and again under the lock when it executes.

Refund reversals are scoped to the refunded payment's **own** allocations.
Unwinding "the newest installments" across the account would let a refund of
payment A strip funding that payment B provided — and a later void of B would
then reverse the same money a second time.

### Two closes, because Iraq has two calendars

An academic year moves `draft → open → financially_closed → closed`.

The treasury wants its books shut soon after the year ends. Second-round
results (دور ثاني) arrive weeks later. A single "closed" flag forces somebody
to either falsify a date or leave results unrecorded — so financial close
freezes money while academic recording stays open, and only the later academic
close freezes everything.

Two years are legitimately open at once every autumn, and the model permits it.

**Closing a year does not stop the university collecting what it is owed.** A
student settling a 2023-2024 debt in 2026 posts against the current open year
and allocates to the old year's installments. Closing prevents rewriting
records, not collection — and refusing collection is how staff end up working
around the system.

The only route back into closed books is `adjustment_open`: a written reason,
an administrator, a bounded window, an audit entry, and corrections that post
as adjustments rather than edits.

### Superseding preserves the past

A student who changes department in March does not have their enrollment
updated. The original is marked superseded and a replacement is created
pointing back at it, so "what was this student registered as on the day that
receipt printed" always has an answer.

The write order is forced by the schema and worth understanding: the old row
must be superseded before the replacement can be inserted, because a partial
unique index permits only one live enrollment per student per year. A **deferred
constraint trigger** then verifies at commit that a replacement really was
created, so an enrollment cannot be retired with nothing in its place —
silently erasing a registration somebody may have paid into.

The money does not follow the student directly either. The old account is
cancelled with its payments untouched, and the balance moves as a visible pair
of transfer adjustments. Re-pointing payments would falsify receipts already in
students' hands.

### Discounts: three layers

**Definition** (what it is, versioned and frozen) → **Assignment** (who was
granted it, for which years) → **Application** (how much came off one account,
frozen at computation).

Percentages compute against the **original discountable base**, never a running
remainder. Sequential application makes the answer depend on evaluation order —
a ten percent grant plus a flat 100,000 gives 1,700,000 one way and 1,710,000
the other — and that cannot be defended at a cashier's window or in a ministry
audit. It also matches how circulars are phrased: fifteen percent *of the
tuition*.

The floor is the **non-discountable remainder**, not zero. Registration and
identity-card charges are collected from everyone, including a fully exempt
student.

Nothing is truncated silently: an application clipped by a cap keeps its
computed amount beside the applied one and names the reason.

An all-years grant materialises automatically at each account generation, but
whether it takes effect depends on the definition's `annual_reconfirmation`
flag. A staff benefit runs on; a hardship discount waits for an officer to
confirm the hardship has not ended. That is what makes an all-years grant safe
to give.

### Overdue is never stored

It is `due_date < today` with money outstanding — a fact about right now. A
stored flag needs a nightly sweep, and the sweep's failure leaves stale flags
that quietly misreport the debt.

### Caches, and how they are kept honest

`paid_total`, `refunded_total` and `credit_balance` on an account are caches,
maintained inside the same transaction as the payment that changes them, under
that account's row lock. The `v_account_balance` view recomputes the truth from
transaction rows.

`v_account_reconciliation` lists any account where the two disagree. It is
expected to be empty, and the financial year close **refuses** while it is not
— closing over a discrepancy freezes the discrepancy permanently.

Anything printed on a receipt or examined by an auditor reads raw rows.
Dashboards may read caches.

### The audit trail is hash-chained

Every financial mutation appends to `audit_log` with actor, before, after,
reason and request id — in the same transaction as the change, so the two can
never disagree.

Rows are chained: each carries the hash of its predecessor, computed by a
database trigger so an entry inserted by any route is chained too. An entry
altered or removed later breaks verification of everything after it.

```bash
make verify-audit
```

Permissions cannot fully solve this — Go connects as a role that owns its
tables. The chain does not prevent tampering; it makes tampering visible, and
names where.

## What PostgreSQL is doing that a lesser database would not

- **Partial unique indexes** state business rules directly. One live enrollment
  per student per year; one live account per enrollment, so a cancelled one can
  be regenerated; one reversal per allocation. Elsewhere these need a nullable flag column and a convention
  nobody can see in the schema.
- **`NULLS NOT DISTINCT`** makes fee-policy scope uniqueness actually bite.
  Without it, two rows both meaning "2025-2026, engineering, any department"
  insert cleanly and resolution finds two winners at equal specificity.
- **Generated columns** compute the Arabic-folded search columns and the
  specificity score, so they cannot drift from their sources.
- **Deferred constraint triggers** let the supersede pair be written in the only
  order the indexes allow while still refusing an orphaned supersede at commit.
- **Trigram (GIN) indexes** answer `LIKE '%محمد%'` on a folded name, which a
  B-tree cannot. In Iraqi names the distinguishing part is often in the middle.
- **`CHECK` constraints** encode the enrollment status/result legality matrix,
  so no code path can store a combination the domain has no meaning for.

### Arabic search

`normalize_arabic` folds the orthographic variation that makes Arabic name
search fail: tashkeel and tatweel stripped, hamza carriers unified to alef,
ta marbuta to ha, alef maqsura to ya. A clerk typing `فاطمه` finds `فاطمة`;
`احمد` finds `أحمد`.

The same function is applied on write, through a generated column, and to the
search term on read — both sides of every comparison are folded identically. It
is not left to the collation, because no collation folds ة against ه
consistently across versions.

`normalize_phone` canonicalises to `964XXXXXXXXXX`, translating Arabic-Indic
numerals, so `07701234567`, `+964 770 123 4567` and `٠٧٧٠١٢٣٤٥٦٧` all match. A
reversed column makes "the number ending 4567" an indexed prefix scan.

## Migrations

```bash
make migrate-status      # every migration and its state
make migrate-up
make migrate-down STEPS=2
make migrate-new NAME=add_scholarship_table
make migrate-validate    # does the database match this build?
```

Four properties, each because its absence has burned real systems:

- **Checksums are verified on every run.** Editing a migration production has
  already applied is caught at start-up, not discovered when two databases
  disagree.
- **An advisory lock serialises runners**, so three replicas booting at once
  cannot race through the same migration.
- **Each migration commits with its own bookkeeping row**, making a partial
  apply impossible. Statements PostgreSQL forbids in a transaction opt out with
  `-- migrate:no-transaction` and are protected by a dirty-state marker instead.
- **Down scripts are mandatory.** A migration with no reverse is a one-way
  door, and a system managing money needs the door to swing both ways during an
  incident.

Out-of-order migrations are refused: a pending version numbered below one
already applied means two branches merged badly, and applying it would produce
a schema no other environment can reproduce by replaying in order.

## API

All routes are under `/api/v1`, except `/health` and `/ready`, which sit
outside authentication because a load balancer has no credentials and a
readiness probe that needs a token cannot report that authentication is broken.

| Area | Routes |
|---|---|
| Auth | `POST /auth/login`, `POST /auth/refresh`, `GET /auth/me` |
| Students | `GET/POST /students`, `GET /students/:id`, `PATCH /students/:id/contact`, `GET /students/:id/{enrollments,accounts,discounts,audit}` |
| Enrollments | `POST /enrollments`, `GET /enrollments/:id`, `POST /enrollments/:id/{supersede,result,status}` |
| Accounts | `POST /accounts` (with `dry_run`), `GET /accounts/:id`, `POST /accounts/adjustments` |
| Payments | `POST /payments`, `GET /payments/:id` |
| Voids | `POST /voids/execute` (one call), `POST /voids`, `POST /voids/:id/execute`, `GET /voids/pending` |
| Refunds | `POST /refunds/issue` (one call), `POST /refunds`, `POST /refunds/:id/{approve,reject,post}`, `GET /refunds/pending` |
| Discounts | `POST /discounts/grants` (one call), `POST /discounts/assignments`, `POST /discounts/assignments/:id/{approve,revoke}`, `POST /discounts/applications/:id/confirm` |
| Data | `GET /data/export`, `POST /data/import` — the whole database as a ZIP of CSV files |
| Years | `GET/POST /academic-years`, `POST /academic-years/:id/{open,close-financially,close,reopen}` |
| Configuration | `POST/GET /fee-policies`, `POST /fee-policies/:id/publish`, `POST /fee-policies/preview-resolution`, `POST/GET /installment-templates` (+ `/:id/publish`), `POST/GET /discounts/definitions` (+ `/:id/versions`, `/versions/:id/publish`), `POST /{colleges,departments,study-types}` |
| Bulk | `POST /bulk/promotions`, `POST /bulk/accounts` (both `dry_run` first) |
| Imports | `POST /imports/students`, `GET /imports`, `GET /imports/:id`, `POST /imports/:id/{validate,confirm,run,resume,cancel}`, `PATCH /imports/:id/rows/:row_no` |
| Receipts | `GET /payments/:id/receipt`, `GET /refunds/:id/receipt` (`?format=html\|text`) |
| Reports | `GET /reports/{students/:id/statement,departments,study-types,stages,years/:id,installments,debt,aging,discounts,collection-trend,cash-flow,cashier-daily,voids,refunds,exemptions}` |
| Oversight | `GET /oversight/audit/verify`, `GET /oversight/reconciliation` |
| Reference | `GET /{study-types,colleges,departments,payment-methods}` |

Roughly a hundred routes. Two conventions worth knowing before you call them:

**Anything that moves money or a cohort runs a dry run first.** `POST /accounts`,
`POST /bulk/promotions` and `POST /bulk/accounts` all accept `dry_run: true`,
return the full computed outcome, and write nothing. The bulk commands then
require the *hash* of the plan that was approved; a row whose numbers moved
since is skipped with `changed_since_preview` rather than charged under an
approval given for different figures.

**Reports require a year, and say why when you omit one.** The year filter is
what bounds the scan, and figures from two years are not comparable anyway. The
cashier daily sheet takes a date range instead, because a shift belongs to a
day rather than to an academic year.

**Every report exports, and the file is the whole result.** Add `?format=csv`,
`xlsx` or `pdf` to any `/reports/` path — including one student's statement,
which exports as the ledger that explains the balance rather than as the
balance. Omit the parameter and the same route answers JSON. Finance offices
run on spreadsheets, and a report that exists only as JSON is a report somebody
re-types into Excel; a re-typed figure is a figure that can be wrong, which is
the whole reason this system exists. CSV carries a byte-order mark so Excel
opens Arabic names as UTF-8; `pdf` is a print-ready HTML page, because an
Arabic PDF needs font shaping and bidirectional layout the browser already has.
A paged report ignores `limit` under an export and returns the whole match set
up to 50,000 rows: a spreadsheet of the first fifty rows is worse than no
spreadsheet, because it looks complete.

Every money-moving `POST` requires an `Idempotency-Key` header. A retry with
the same key replays the original response and sets `Idempotent-Replay: true`;
the same key with a different body is rejected outright.

Errors share one shape, and the code is stable enough to branch on:

```json
{"error": {"code": "payment.has_refunds", "message": "…", "details": {…}, "request_id": "…"}}
```

## Operating it

```bash
api                                  # serve
api version                          # what this build is; production refuses an unidentified one
api healthcheck                      # probe (used by the container health check)
api seed                             # first administrator, development only
api create-user <user> <name> <roles># CREATE_USER_PASSWORD supplies the password
api demo                             # exploration dataset, development only
api audit-ship                       # copy the audit trail off-host
api audit-ship verify                # compare that copy against the database
api perf-seed --scale=medium         # a dataset big enough to measure, development only
```

`seed`, `demo` and `perf-seed` all refuse to run in production: a known starting
password is exactly the kind of thing that survives into a live system for
years, and a synthetic dataset in a live database is worse.

Operational scripts, all of them exercised rather than described:

```bash
make backup            # dump, then prove pg_restore can read it
make restore-drill     # restore into a scratch database and reconcile it
make perf              # measure p50/p95/p99 against a seeded dataset
make docker-build      # the deployable image, stamped so it can identify itself
```

Deployment — systemd units, a production compose file, nginx in front — is in
`deploy/`, and `docs/operations/deployment.md` explains the choices.

## Exploring it

```bash
make demo-reset
```

This rebuilds the database and loads three academic years of a plausible Iraqi
university — two engineering and two medical departments, morning, evening and
parallel study, thirty students, fifty enrollments and accounts.

It is generated **through the real commands**, not inserted as SQL. That costs
some seconds and buys two things: the data cannot violate an invariant the
system enforces, and a successful run is itself evidence that registration,
pricing, collection, refunding, superseding and year closing work together.

Every scenario the design was written for is in there to look at:

| Look at | To see |
|---|---|
| `CPE-2025-020` | A mid-year move to morning study: the old enrollment superseded, its obligation written off, its 900,000 carried forward as credit, and the replacement settled by it |
| `CIV-2024-012` | A martyr-family exemption: tuition cleared, the 100,000 of registration and card fees still payable |
| `CIV-2025-021` | Two stacked discounts, both computed against the original discountable base |
| `CPE-2025-017` | An overpayment that spilled across every installment and became credit |
| `CPE-2025-018` | A payment of the full fee with a 200,000 refund beside it — the payment row still says what it collected |
| `CPE-2025-019` | A voided payment that kept its receipt number and appears in the void register |
| `CIV-2023-005` | A dropout in 2023 who returned in 2025, with the abandoned year's debt still on its own account |
| `ELE-2025-024` | An incoming hosted student whose home university collects, so no account here carries their debt |
| `CPE-2025-029` | Registered but never priced — the gap a debt report must not mistake for a settled student |
| Any repeating student | A quarter more tuition than a first-attempt student in the same seat, from a policy row rather than a branch in code |

The years are left in all three lifecycle states at once: 2023-2024 and
2024-2025 financially closed, 2025-2026 open. Sign in as `admin`; the password
is printed when the demo finishes.

After it loads, these four queries should all return zero, and the demo is only
considered good if they do:

```bash
psql flowed -c "SELECT count(*) FROM v_account_reconciliation"      # cached vs computed
psql flowed -c "SELECT count(*) FROM v_installment_reconciliation"  # installment drift
psql flowed -c "SELECT count(*) FROM v_over_refunded_payments"      # paid out more than taken in
psql flowed -c "SELECT count(*) FROM verify_audit_chain(0)"         # tampered audit entries
```

## Testing

```bash
make test        # unit + integration, race detector on
make test-short  # unit only, no database needed
```

The domain tests carry the weight, because that is where the money rules live:
half-up rounding is exact and reproducible; split shares always sum to the
whole across hundreds of amounts and shapes; discount stacking is
order-independent and cannot reach non-discountable components; allocation
spills oldest-due-first and turns the excess into credit; a refund can only
unwind its own payment's allocations; a plan always sums to the net and a
re-split never touches a paid installment; financial close freezes money while
leaving second-round results writable.

`test/integration` goes at the schema directly, proving the guarantees are in
the database rather than only in Go: the one-live-enrollment index, the
status/result matrix, the deferred supersede check, wildcard scope uniqueness,
the append-only triggers, the audit hash chain, and a ten-goroutine test that
the account row lock prevents lost updates.

## What the end-to-end run exercises

A scripted pass over a live server covers the path a real desk walks: register
and enroll a student, grant and approve a discount, price the enrollment, take
a partial payment, retry it and get the same receipt back, overpay and watch
the excess become credit, refund part of it, fail to void a refunded payment,
fail to over-refund, supersede the enrollment mid-year, close the books, fail
to collect into the closed year, and still record a second-round result
afterwards — ending with the audit chain intact and no cached-total drift.

## Printed receipts

A receipt is the only part of this system a student physically holds, and the
document they bring back when something is disputed. Three properties follow.

**The amount appears twice** — in figures and written out in Arabic. Changing
500,000 to 5,000,000 in figures is a stroke of a pen; changing
`خمسمائة ألف` to `خمسة ملايين` is not. The written form closes with
`لا غير`, joined by a non-breaking space so a 42-character thermal roll cannot
split the marker across two lines.

The Arabic spelling (التفقيط) follows the grammar rather than approximating it,
because an accountant notices immediately when it does not:

| Amount | Spelled | The rule |
|---|---|---|
| 1,000 | ألف | One is not counted out loud |
| 2,000 | ألفا دينار | The dual sheds its nūn before what it governs |
| 3,000 | ثلاثة آلاف | Three to ten take the broken plural |
| 11,000 | أحد عشر ألفاً | Eleven upwards returns to the singular accusative |
| 200,000 | مائتا ألف | Not مائتان ألف — the dual is in construct |
| 320,000 | ثلاثمائة وعشرون ألف دينار | The scale word sheds its tanween before the currency |
| 500,000 | خمسمائة ألف | A round hundred goes back to the plain singular |

Every figure is in **Western digits** even on an otherwise Arabic page: ٠ and 0
are easy to confuse on a poorly printed slip, and a figure that can be misread
is a figure that can be disputed.

**It reproduces exactly.** The renderer reads only frozen rows — the payment,
its allocations, the account snapshot — so a reprint three years later is the
same document, not a recalculation against configuration that has since moved.

**A reprint announces itself.** The first print is the original; every one after
is stamped `نسخة طبق الأصل` across its face and numbered, and each print is
written to the audit trail with who did it. Reprints are legitimate — students
lose paper — but a burst of them against one payment is a pattern worth
seeing, and it only exists if each print is recorded.

Two renderings:

- **HTML** (default) — A5, right-to-left, entirely self-contained. A desk with
  no network prints it identically, because losing a stylesheet would print a
  column of unformatted text no finance office would accept.
- **`?format=text`** — 80mm thermal roll, which is what most Iraqi cashier
  desks actually feed. Right-aligned by padding rather than trusting cheap
  printer firmware to handle bidirectional layout, and wrapped on word
  boundaries so an Arabic phrase never splits into unconnected fragments.

A voided payment still prints — an auditor asking about it needs to see it —
struck through and labelled `سند ملغى`, never as a live collection. A refund
prints as a `سند صرف` naming the receipt it reverses, so it can never be
mistaken for a second collection.

The letterhead is configuration (`RECEIPT_UNIVERSITY_NAME` and friends): one
binary serves any university.

## Background jobs

The scheduler starts before the listener and takes a PostgreSQL advisory lock
per job, so three replicas booting together run each job once rather than three
times. A job that panics is logged and retried on the next tick; it does not
take the scheduler down with it.

| Job | Cadence | What it is for |
|---|---|---|
| Reconciliation | nightly | Compares cached account totals against the transaction rows. **Reports, never repairs** — a silent auto-fix would hide the bug that caused the drift. |
| Adjustment-window reaper | 15 min | Closes reopened years past their deadline. Without it, a year reopened "for the afternoon" stays writable until somebody remembers, which is how a controlled exception becomes a standing hole. |
| Idempotency purge | daily | Drops expired command records. They are not an audit trail; the audit log is. |
| Stalled import reaper | 10 min | Fails import batches whose worker heartbeat stopped, so a killed worker does not strand a batch in `importing` forever. |
| Overdue snapshot | daily | Logs the overdue count and total, so the trend is visible before anyone opens a report. |
| Rate-limit purge | 15 min | Deletes buckets nobody has touched. Registered only when the shared limiter is on. Without it the table grows by a row per client address and never shrinks — the same leak the in-process sweeper prevents, now somewhere a restart cannot clear. |
| Audit shipping | 15 min | Copies the trail to a host this database cannot reach. The interval is the exposure: an entry written just after a pass exists only where somebody with the database role can delete it until the next one. Registered only when a destination is configured. |
| Session purge | daily | Expired sessions and old sign-in attempts. Neither is an audit trail, and left to grow both are read on the login path. |
| Reminder queue and delivery | daily / frequent | Consults the reminder policy, then hands what it queued to the gateway. A message queued at nine that arrives at five is a message about a due date that has passed. |
| Payment intent expiry | 30 min | Closes abandoned electronic payments. |

Every job runs a first pass shortly after start-up rather than waiting a full
interval. That is not a nicety: a daily job on a service redeployed every
afternoon would otherwise never run at all, and for a while reconciliation
did not — the wiring built its configuration from a literal, and the flag that
asks for the first pass is a bool, which the defaults could not fill.

## Metrics and tracing

Metrics are **pulled**, traces are **pushed**, and the asymmetry is deliberate:
a Prometheus scrape needs nothing running between this process and the
dashboard, while a trace exporter with no collector in front of it produces
retry noise and nothing else. So metrics default to on and tracing defaults to
off.

`/metrics` gets **its own listener**, `127.0.0.1:9464` by default. The scrape
surface enumerates every route the API has and how often each one fails, which
is a map of the system, and it carries no authentication — the bind address is
the access control. A deployment scraped from another pod sets `0.0.0.0`
deliberately and firewalls the port.

**No label anywhere identifies a student.** A metric label outlives the request
in a store with none of the database's access control, so this is a disclosure
rule rather than a cardinality preference. Three things follow from it: HTTP
metrics are keyed by matched route template (`/students/:id`) and never by
path, with one constant label for every unmatched request so a port scan cannot
mint a series per URL; the database tracer records the statement text and never
the arguments; and a failed statement is recorded as its SQLSTATE and
constraint name rather than through `RecordError`, because PostgreSQL's DETAIL
line quotes the offending values — `Key (student_no)=(2024001) already exists`
— and the constraint violations this schema raises deliberately are the most
likely errors in production.

The money counters are incremented **after** the transaction commits. Bumping
one inside the transaction counts money a rollback never took, and a dashboard
that disagrees with the ledger is worse than no dashboard.

| Series | What it says |
|---|---|
| `flowed_payment_posted_total{method_code}` / `flowed_payment_amount_IQD_total{method_code}` | Collections and dinars, per method. The currency is in the series name, so a dinar figure cannot be read as anything else. |
| `flowed_payment_voided_total` / `..._amount_IQD_total` | Reversals. Climbing against collections is the earliest visible sign of a cashier reversing receipts a student is still holding; the void register is where that is then confirmed. |
| `flowed_refund_posted_total` / `flowed_refund_amount_IQD_total` | Money returned. Only the posting is counted — the request and the approval move a document, this moves money. |
| `flowed_account_generated_total` / `flowed_account_obligation_IQD_total` | Accounts opened and the obligation frozen with them. Dry runs are excluded: they priced an account nobody owes. |
| `flowed_account_adjustment_amount_IQD_total{direction}` | Adjustments as a magnitude with a direction, because a counter that can go down is not a counter and `rate()` over one reports nonsense on every credit. |
| `flowed_discount_applied_total{status}` / `flowed_discount_amount_IQD_total{status}` | Relief granted, as applied rather than as computed — a grant cut short by a cap cost the smaller figure. Split by status, so relief still provisional on an eligibility re-check is visible. |
| `flowed_db_pool_acquires_empty_total` | Times a caller wanted a connection and the pool had none. The shape of a cashier desk hanging behind a report. |
| `flowed_ratelimit_decisions_total{tier,outcome}` | `tier="degraded"` at anything but zero means the shared store is unreachable and the configured limit has quietly reverted to per-process. |
| `flowed_scheduler_job_runs_total{job,outcome}` | `outcome` separates a clean pass from one that found a defect from one that failed, because reconciliation finding drift is the job working. |
| `http_server_request_duration_seconds{http_route,...}` | Standard OTel name and buckets tuned to this API, so an off-the-shelf dashboard works without translation. |

Tracing is OTLP over HTTP — it goes through an ordinary reverse proxy, which is
what a university network has. Set `OBS_TRACING_ENABLED=true` and point
`OBS_OTLP_ENDPOINT` at the collector's base URL; the `/v1/traces` path is
completed for you. Sampling is parent-based, so a decision taken at the edge
holds for the whole trace and no trace is ever half-recorded. Every log line of
a request carries its `trace_id`, which is what makes the two searchable
together.

`DB_LOG_QUERIES` and tracing cannot both be on: pgx takes one tracer per
connection, and the configuration refuses the pair at start-up rather than
letting one silently win.

## Rate limiting

The limiter was per process, which meant a deployment of N replicas admitted N
times the configured rate and every restart forgot every bucket. It is now a
token bucket in PostgreSQL, so the number in the configuration is the number
being enforced.

Two tiers, in order. The **local** in-memory bucket still runs first: it sees
only this replica's traffic, so it can never admit what the shared budget would
refuse — only refuse sooner — which makes it a free pre-filter that turns away
a terminal stuck in a retry loop without a database round trip. The **shared**
bucket is the budget itself.

When the shared store cannot be reached, the request is admitted on the local
decision. **Failing open is deliberate**: a limiter protects availability, and
one that turns a database blip into a total outage has inverted its own
purpose. The degradation is counted and logged — throttled to one line per
thirty seconds, because the condition that produces it produces it on every
request.

Health and readiness probes are exempt. They were not exempt while the limiter
was per process, and leaving them in once it is shared would be a regression:
every replica's probe now draws on one budget keyed to the load balancer's
address, and the desks behind that address would start being refused because
the infrastructure was checking whether they were up.

The budget is keyed by client address, which at a university means keyed by
campus NAT — a hall of thirty terminals shares one bucket. That is what the
default of 600 a minute is sized for, and a large site raises it in
configuration.

## Status

Complete and running end to end. Schema (32 migrations), migration engine,
domain layer, application services, PostgreSQL adapter, 104 documented HTTP
paths, authentication with key rotation and session revocation, organisational
scope, reports and exports, CSV export and import of the whole database,
configuration administration, bulk promotion and account generation, staged
import, the background scheduler, printable Arabic receipts, metrics and
tracing, the cross-replica rate limiter, the off-host audit archive, tracked
reconciliation, and the operational commands. Build, vet, staticcheck, gofmt
and the full test suite — 357 test functions across unit, integration and
end-to-end — are clean under the race detector, and the demo dataset loads
through the real commands with all four integrity checks at zero.

It is run from a single account. Roles, shifts, the cash drawer, the four-eyes
rule, sponsors, settlement, electronic payment providers, the student portal
and notifications were removed rather than switched off — see the section of
the same name in `CLAUDE.md` for what went and what was kept.

Alert rules now exist for the instruments, in `deploy/alerts.yml`: an
unexplained invariant violation, voids climbing against collection, an
over-refund, a broken audit chain, the pool starving, the limiter reporting
`tier="degraded"`, and — the one most often missing from a system like this —
reconciliation not having run at all, because a check that stopped running looks
exactly like a system with nothing wrong.

What the operations documents cover, all of them written from runs rather than
from intent:

| Document | What it answers |
|---|---|
| `docs/operations/backup-restore.md` | The archive, the drill, and point-in-time recovery |
| `docs/operations/audit-archive.md` | The off-host copy, and what each verification failure means |
| `docs/operations/audit-growth.md` | What the trail costs at scale, measured |
| `docs/operations/reconciliation.md` | Working the invariant queue, and why the fix is never an UPDATE |
| `docs/operations/performance.md` | Measured latencies, and where the time goes |
| `docs/operations/deployment.md` | systemd or Docker, secrets, draining, rollback |

Two things are ruled out rather than pending. There will be **no external
general-ledger integration** — Flowed is the system of record for fees and
collection. And the JSON tags in `internal/port/reporting.go` **stay**: moving
them would be thirty structs and some seven hundred lines of field-for-field
mapping on a read-only path, whose only failure mode is the drift the layering
rule exists to prevent. The file's header comment argues it, and names why
`import_views.go` is genuinely a different case.
