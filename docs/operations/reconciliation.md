# Reconciliation

The system keeps caches: `financial_account.paid_total`, an installment's
`paid_amount`, an account's `adjustment_total`. They are maintained inside the
same transaction as the payment or refund that changes them, under a lock on the
account row. That is what makes a balance readable without summing a student's
whole history at a cashier's desk.

A cache is a second copy of a fact, and a second copy can be wrong. So four
checks ask, on a schedule, whether the copies still agree with the rows they
were derived from:

| Check | The question |
|---|---|
| `accounts` | Does each account's cached paid, refunded, adjusted and credit total match its transactions? |
| `installments` | Does each installment's cached paid amount match its allocations? |
| `refunds` | Has any payment been refunded beyond what it collected? |
| `audit_chain` | Does the audit hash chain still verify end to end? |

All four are expected to return nothing. Every row one returns is a defect.

## Working the queue

```
GET  /api/v1/reconciliation/findings          the queue, worst and oldest first
GET  /api/v1/reconciliation/runs              the passes, newest first
POST /api/v1/reconciliation/run               run the checks now
POST /api/v1/reconciliation/findings/:id/acknowledge
POST /api/v1/reconciliation/findings/:id/resolve
```

A finding is **one row for as long as it survives**. The second night the same
account drifts, `seen_count` becomes 2 rather than a second row appearing — a
queue where one problem produces a row a night is unreadable within a week, and
that is how a real finding gets lost among its own copies.

After three passes with nobody explaining it, a finding escalates from
`warning` to `critical`. Drift that is fixed the same evening never escalates;
drift nobody has touched by the end of the week is unmissable.

**Acknowledge** means somebody is on it. It does not close it. Two people
investigating the same drift is waste, and a queue where "somebody is on it" and
"it is fixed" look alike is a queue that closes things nobody fixed.

**Resolve** requires saying what was done, and the text is read by whoever sees
the same account drift again. "Corrected" teaches nothing. "The void path was
not decrementing paid_total when the payment carried a partial refund; fixed in
build 1.4.2 and the account re-derived by posting adjustment X" is the sentence
that stops it recurring.

A finding a later pass no longer sees is closed automatically, with a note
saying so. Drift that stops is drift somebody corrected; leaving it open forever
teaches operators that the queue is noise, and a queue nobody reads is the same
as no queue.

## The remedy is never to edit the cache

When an account's cached paid total disagrees with its payments, the temptation
is an `UPDATE` that makes them agree. Do not.

The cache being wrong means a command failed to maintain it, and that command
will do the same thing again tomorrow to a different account. The `UPDATE` hides
the evidence and leaves the defect. Worse, `trg_account_immutable` refuses to
let the pricing be rewritten once money has moved against an account, so the
"fix" would have to disable a control to apply.

What to do instead:

1. Read the finding's `detail`. It carries the cached and computed values, and
   the difference is usually recognisable — one payment's worth, one refund's.
2. Find the transaction the cache is missing or double-counting. The audit trail
   for the account gives the command, the operator and the request id.
3. Correct the account the way everything else is corrected: an adjustment, a
   void, a refund. Each is a new row.
4. Fix the command. Then resolve the finding naming the build.

## What the schedule is

The reconciliation job runs daily by default. It is a singleton across replicas
— it takes an advisory lock — so running four API processes does not run it four
times.

A run is recorded whether or not it found anything, and that is the point of the
table: **a check that stopped running looks exactly like a system with nothing
wrong**. `FlowedSchedulerJobStopped` in `deploy/alerts.yml` fires when
reconciliation has not run in over a day, and it is a critical alert for that
reason.

## Alerts

`deploy/alerts.yml` carries the rules. The four that matter here:

- `FlowedInvariantViolation` — a finding has survived three passes.
- `FlowedOverRefund` — fires at any severity, without waiting for escalation:
  money left the university that never entered it.
- `FlowedAuditChainBroken` — the chain does not verify. Compare against the
  off-host archive, which is the only thing that can tell an alteration from a
  deletion.
- `FlowedSchedulerJobStopped` — the check itself is not running.
