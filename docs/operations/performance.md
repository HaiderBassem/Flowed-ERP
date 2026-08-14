# Performance, as measured

Every number here came out of `scripts/perf-run.sh` against a real server and a
real database. Nothing is projected and nothing is rounded in our favour. Re-run
it after any change to a report or an index; the numbers below are the baseline
to compare against.

## How to reproduce

```bash
SCENARIO=desk    WORKERS=1 DURATION=15s scripts/perf-run.sh   # uncontended
SCENARIO=desk    WORKERS=8 DURATION=20s scripts/perf-run.sh   # a busy counter
SCENARIO=reports WORKERS=4 DURATION=20s scripts/perf-run.sh   # the finance office
SCENARIO=mixed   WORKERS=8 DURATION=20s scripts/perf-run.sh   # both at once
scripts/perf-explain.sh                                       # plans for the slow ones
```

The run builds its own database (`flowed_perf`), seeds it with
`api perf-seed`, starts a server with rate limiting off, discards a warm-up
period, and reports p50/p95/p99 per endpoint with the error count beside them.
A fast endpoint that is failing is not a fast endpoint, so any error exits
non-zero.

The seeded dataset is written as SQL rather than through the commands — thirty
thousand payments through the full command path takes hours — but it is checked
against the three reconciliation views before it is handed over. A perf run on
inconsistent data measures queries nobody will ever run.

## The dataset these numbers came from

| | |
|---|---|
| accounts | 20,051 |
| installments | 40,204 |
| payments | 13,372 |
| students | 20,030 |
| on disk | 72 MB |

That is roughly one year of a whole university. PostgreSQL 18 on an
Apple M-series laptop, everything on one machine, so the absolute numbers will
differ on a server — the shape of them will not.

## What a cashier waits for

`SCENARIO=desk`, one worker, so nothing is queueing behind anything else. This
is the floor: what the system costs when it is not busy.

| endpoint | p50 | p95 | p99 |
|---|---|---|---|
| student by number | 0.7 ms | 0.9 ms | 1.2 ms |
| account detail | 1.1 ms | 1.4 ms | 1.9 ms |
| student audit trail | 1.2 ms | 2.1 ms | 2.5 ms |
| student statement | 2.6 ms | 3.1 ms | 3.4 ms |
| student search (Arabic, `q=محمد`) | 3.8 ms | 4.2 ms | 4.7 ms |
| student listing, no filter | 18.2 ms | 19.4 ms | 20.1 ms |
| student listing, this year | 22.4 ms | 24.1 ms | 24.4 ms |

The Arabic search is fast because both sides are folded and the trigram index
does the work: 1.6 ms in the database, the rest is the request. A search by
student number is an index lookup and costs almost nothing.

At eight concurrent workers, the same desk scenario holds: p99 stays under
73 ms at ~293 requests per second across the five endpoints, with no errors.

## What the finance office costs

`SCENARIO=reports`, four workers running reports continuously — heavier than any
real office, deliberately.

| report | p50 | p95 | p99 | in the database |
|---|---|---|---|---|
| debt report (one page) | 318 ms | 333 ms | 336 ms | 83 ms |
| installment report | 174 ms | 183 ms | 192 ms | — |
| department summary | 173 ms | 181 ms | 197 ms | 73 ms |
| aging report | 166 ms | 172 ms | 173 ms | 49 ms |

## The interaction that matters

`SCENARIO=mixed`, eight workers, reports and desks together. This is the
question the harness exists to answer: what does the counter feel like while the
finance office is working?

| endpoint | alone | while reports run |
|---|---|---|
| student search | 3.8 ms | 16.2 ms |
| account detail | 1.1 ms | 11.6 ms |
| student statement | 2.6 ms | 27.8 ms |
| student listing, this year | 22.4 ms | 75.6 ms |

Four to ten times slower, and still well inside what a person notices. The desks
keep working while the reports run, which is the property that matters; nothing
here queues badly enough to make a student wait.

## Where the time actually goes

**The debt report scans the year, not the page.** `v_debt` computes each
account's position from the transaction rows — adjustments, posted payments,
posted refunds, open credits — as four correlated subqueries per account, and
the `remaining > 0` filter is on the computed value, so all 20,000 accounts are
evaluated before the 50-row page is taken. 83 ms in the database for 13,350
debtors.

It computes rather than reads the cached totals **on purpose**: `v_account_balance`
is the definition that `v_account_reconciliation` compares the caches against.
Making the report read the caches would make the check compare a thing to
itself, and the day a cache drifted the report would be confidently wrong. The
cost is the price of that independence, and at this size it is a report that
takes a third of a second.

A covering index on `payment (account_id) INCLUDE (amount, posted_at) WHERE
status = 'posted'` was tried: it turned the payment subquery into an index-only
scan and moved the total from 46 ms to 44 ms — about 5% — because this dataset
has at most one payment per account. It was not kept: an index on the busiest
table in the system has to earn its write cost, and 5% on a synthetic shape does
not. Try it again against real data with several payments per account, where it
should matter more.

**Listing screens pay for an exact total.** `count(*) OVER ()` gives the page and
the total in one query, which is right, but the count is over the whole match
set — every one of 20,000 students for an unfiltered list. That is the entire
18 ms, and filtering by year makes it slower (22 ms) rather than faster, because
the year filter joins enrollment and still counts everything.

Bounding it — counting at most 1,001 and showing "1000+" — would make these
screens flat, at the price of an approximate total on a list. That is a change
to what every client sees, so it is recorded here rather than made quietly.
Reports, where an exact total is worth paying for, are unaffected either way.

## What to watch in production

The instruments already carry it:

- `http_server_request_duration_seconds` by route template — the same p50/p95/p99
  as above, on real traffic.
- `flowed_db_pool_acquires_empty_total` — desks queueing for a connection. The
  usual cause is a report against a year with no filter, not load.

`FlowedPoolExhausted` in `deploy/alerts.yml` fires on the second, which is the
signal that the interaction above has stopped being comfortable.
