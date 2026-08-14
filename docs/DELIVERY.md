# Delivery report

What was built, what was verified, and what was not. Every claim here names how
it was checked; anything that was not run says so.

Baseline: `7da3241` (the tree as audited). Head: `e94fee9`. Twenty-five commits,
196 files changed, 47,540 insertions.

## Verified at the end of the work

Run on this machine, on the tree at HEAD:

| Gate | Result |
|---|---|
| `make fmt-check` | pass |
| `make lint` (`go vet ./...`) | pass |
| `make staticcheck` | pass |
| `make vuln` (`govulncheck`) | pass |
| `make secrets` | pass |
| `make build` | pass |
| `make test-short` | pass |
| `make test-integration` (race detector) | pass |
| `make migrate-validate` | pass |
| `make openapi-validate` | pass |
| `make ui-build` | pass |
| Full suite, `go test ./...`, run twice | pass both times |
| 24 migrations up → down to zero → up, fresh database | pass |
| `make demo-reset` through the real commands | pass |

The four integrity queries, after the suite and after the demo load, on both
`flowed_dev` and `flowed_test`:

```
v_account_reconciliation      0
v_installment_reconciliation  0
v_over_refunded_payments      0
verify_audit_chain(0)         0
```

Counts at HEAD: 24 migrations, 103 documented API paths / 111 operations, 377
test functions across unit, integration and end-to-end.

## The audit gaps, and what happened to each

| # | Gap | Status | Evidence |
|---|---|---|---|
| 1 | No source control, no reproducible build identity, no CI | **Done** | `.github/workflows/ci.yml`; `buildinfo` resolves version → VCS stamp → unknown, and production refuses an unidentified build |
| 2 | No backup, restore or PITR story | **Done** | `scripts/backup.sh`, `scripts/restore-drill.sh`; drill run: 407 KB archive, restore verified, four queries zero, row counts matched the manifest |
| 3 | Audit trail could not survive the host that writes it | **Done** | Migration 000021, `api audit-ship verify`; integration tests delete an entry and edit an archived block, and require both to be caught |
| 4 | Reconciliation reported to a log and nowhere else | **Done** | Migration 000022; runs recorded, findings escalate on the third sighting, closure needs a reason; `deploy/alerts.yml` |
| 5 | No OpenAPI contract | **Done** | Generated from the router; a drift test fails the build when a route changes shape |
| 6 | No operator interface | **Done** | Shipped; superseded during this work by the React interface being built in parallel |
| 7 | Domain commands unreachable over HTTP; no user administration | **Done** | Operator administration, sessions, master data, the previously unreachable commands |
| 8 | Authorisation was role-only; no organisational scope | **Done** | `shared.Scope`, applied to the query rather than to its result |
| 9 | No bank reconciliation | **Done** | Migration 000015, settlement batches and matching |
| 10 | No electronic collection | **Done** | Provider abstraction, HMAC-verified callbacks, duplicate-event guards |
| 11 | No student-facing statement | **Done** | Migration 000016, portal with a verification code a third party can check |
| 12 | No reminders | **Done** | Migration 000018, scheduling with window keys |
| 13 | Reports could not leave the system | **Done** | CSV with BOM, XLSX written as a raw zip, print-ready RTL A4 |
| 14 | Sponsors and scholarships unmodelled | **Done** | Migration 000017; settlement mode has no default, because both answers are defensible and only the university can choose |
| 15 | Performance unmeasured | **Done** | `api perf-seed`, `cmd/perf`, `docs/operations/performance.md` — numbers below |
| 16 | Audit growth unaddressed | **Done** | Measured first: 500k entries = 251 MB, 879 ms to verify. Checkpointed verification and gated archival; no partitioning, with the reason |
| 17 | No end-to-end tests | **Done** | `test/e2e` drives the engine `cmd/api` builds; found three defects |
| 18 | Refusals not instrumented | **Done** | Auth, payment and export failures; verified on a live scrape |
| 19 | Deployment not reproducible | **Done, one part unbuilt** | systemd units, production compose, nginx, `.dockerignore`. The image itself was **not built** — no Docker daemon in this environment |
| 20 | Wiring lived in `main`, so e2e could not use it | **Done** | `internal/bootstrap`, shared by the server and the tests |
| 21 | Documentation described a different system | **Done** | README and CLAUDE.md corrected against the tree; six operations documents |
| 22 | No final sweep | **Done** | This report, plus the sweeps below |

## Defects found and fixed

Every one of these was found by running something, not by reading.

**The audit chain could fork under concurrent writes.** `sequence_no` came from
a column default, evaluated before the trigger that chains the entry, so
numbering and chaining could disagree; the trigger then inferred the chain tip
from the highest sequence number, and two entries chained onto the same
predecessor. Two cashiers posting at the same moment would produce it, and the
nightly check would report tampering. Fixed by an explicit head row taken with
`SELECT ... FOR UPDATE` — a row lock re-reads what the winner committed, an
advisory lock does not — and verification now follows the links rather than the
numbering. *Found by running the CI gate.*

**Disabling an operator returned 500.** A `CASE` with a bare `NULL` branch made
PostgreSQL infer `text` for a UUID parameter. Logout-on-disable had never
worked. *Found by the end-to-end suite.*

**Re-splitting an installment plan failed against the real schema, every time.**
The write order is forced by a partial unique index; a row-level CHECK refused
the only order that works. Moved to a deferred constraint trigger. *Found by the
end-to-end suite.*

**Merging two student records left the money behind.** `financial_account`
carries its own `student_id`; the merge moved enrollments and counted accounts
it did not move, so the merged student's statement came back empty. *Found by
the end-to-end suite.*

**`financial_account` had no immutability trigger at all.** The frozen net that
every document describes was enforced only in Go, and the application owns its
tables. Migration 000020. *Found by reading the schema while writing the merge
fix.*

**The scheduler never ran a first pass in production wiring.** The configuration
was built from a literal, and `RunOnStart` is a bool, which the defaults could
not fill — so daily jobs on a service redeployed every afternoon never ran.
Reconciliation had effectively never run outside tests. *Found while checking
that a metric reached the scrape surface.*

**Audit shipping stopped forever on a block the archive already held.** An
ordinary situation — a database restored from backup, an intact archive — left
the lag climbing while everything else looked healthy. *Found by watching the
scheduler after configuring an archive.*

Also fixed: an unlogged config loader omission that made logout and lockout
silently no-ops; three test-isolation defects that made the suite unrepeatable.

## Measured, not asserted

From `docs/operations/performance.md`, on 20,051 accounts / 40,204 installments
/ 13,372 payments:

| | uncontended | while four report workers run |
|---|---|---|
| student search (Arabic) | 3.8 ms | 16.2 ms |
| account detail | 1.1 ms | 11.6 ms |
| student statement | 2.6 ms | 27.8 ms |
| debt report, one page | — | 318 ms (83 ms of it in the database) |

Audit trail: 526 bytes an entry; a full chain verification of 500,219 entries
takes 879 ms, and the incremental pass that follows it takes 0 ms.

## What was not done, and why

- **The container image was never built.** No Docker daemon in this
  environment. The compose file parses, the ldflags path was verified to
  produce an identified build, and `api healthcheck` and `/ready` were both
  exercised against a running server — but the Dockerfile itself is reviewed,
  not built. Build it before trusting it.
- **The historical chain fork was observed, not reproduced on demand.** Racing
  goroutines produce it only sometimes, which is why it survived until a CI
  run. The test that exists drives the interleaving by hand and asserts the
  property the new design guarantees.
- **A covering index for the debt report was tried and dropped.** Five percent
  on a synthetic shape does not earn its write cost on the busiest table in the
  system. Recorded so nobody tries it blind again.
- **Listing screens pay for an exact total** — 18 ms of the 22 ms a page over
  20,000 students costs. Bounding the count would flatten them at the price of
  an approximate total, and that is a change every client sees, so it is
  written down rather than made quietly.
- **The SMS gateway is barely covered** (8%): nothing exercises a real gateway,
  and a fake one would prove only that the fake works.

## Two things outside this work

- `webui/` is a React interface being built in parallel by other work. It is
  referenced by `internal/adapter/httpapi/router.go` and by `make ui-build`,
  both of which pass. Its files are not committed by this work.
- `GET /api/v1/public/cashier-desks` was added by that work and is
  unauthenticated. The reasoning is sound — a cashier cannot obtain a token
  without naming a desk — and the contract entries declaring it public are in
  the working tree, uncommitted, because committing them without the handler
  would leave HEAD inconsistent.

## Sweeps

- **TODO/FIXME/HACK**: none in Go or SQL. The single hit is a comment about
  phone-number formatting.
- **Authority**: a test reads the router's own registrations and requires every
  route to have an authority decision — a role check, a declaration that it is
  public, or an entry recording that the service decides and why. Thirteen
  routes have no role check; all thirteen are deliberate and now recorded.
- **Coverage**: `internal/domain/discount` sat at 33% — the resolution engine
  was tested and the two state machines beside it were not, so the four-eyes
  rule had no test at all. Now 89%.
