# The audit trail as it grows

Every command that touches money writes an entry, and nothing deletes one. That
is the design, and the question it raises is what the table costs after five
years of a real university.

Measured rather than guessed, with `scripts/audit-bench.sh`:

| entries | total | heap | indexes | per entry | full chain verification |
|---|---|---|---|---|---|
| 200,000 | 95 MB | 56 MB | 39 MB | 495 B | 259 ms |
| 500,000 | 251 MB | 152 MB | 99 MB | 526 B | 879 ms |

A university of twenty thousand students writes on the order of 300,000 entries
a year — a registration, an account, two to four payments and a handful of
changes per student. So:

| | |
|---|---|
| one year | ~150 MB |
| five years | ~750 MB |
| ten years | ~1.5 GB |

That is affordable. **The trail does not need partitioning**, and partitioning
would cost something real: the chain trigger reads the previous entry on every
insert, and across sixty monthly partitions that becomes sixty index probes on
the write path of every payment.

Two things do stop being free, and both are handled.

## 1. Verification resumes from a checkpoint

Walking the whole chain every night is work that grows forever, and the growth
is invisible until the night it does not finish.

A clean pass records where it reached and the hash it stopped at, in
`audit_verification`. The next pass resumes there. Measured on the 500,000-entry
database:

```
 kind        | entries | problems | took_ms
-------------+---------+----------+---------
 incremental |       3 |        0 |       0
 full        |  500219 |        0 |     879
```

The prefix is not trusted on faith. The checkpoint stores the hash of the entry
it stopped at, and every pass re-checks that entry first: if it has been removed
or rewritten, the pass reports it and starts again from zero. That is what stops
the checkpoint becoming a place to hide behind, and there is an integration test
that removes the checkpointed entry and requires the resumed pass to notice.

What neither a full nor an incremental pass can see is a **deleted** entry that
takes its successors with it — the remaining chain is intact, because
verification walks what is present. That is what the off-host archive is for;
see `docs/operations/audit-archive.md`.

Run a full pass on a slower schedule anyway — monthly is generous at these
timings:

```sql
SELECT * FROM verify_audit_chain(0);
```

## 2. Archival, gated on the off-host copy

For an installation that does outgrow the table, entries can be moved out of the
working set into `audit_log_archive` — moved, not deleted:

```sql
SELECT * FROM archive_audit_entries('2024-01-01'::timestamptz, 100000);
```

It refuses unless three things hold, and each is a refusal rather than a filter,
because a silent partial archive is how a hole appears in a record nobody is
looking at:

1. **Something has been shipped off-host.** Archiving with only one copy in
   existence is the deletion this system must never do.
2. **Every entry being moved is covered by a shipment.** The gate is
   `audit_shipment.to_sequence`, so an entry that has not left this host does
   not leave the live table either.
3. **The chain verifies across the range being moved.** Moving a range that
   already disagrees with itself would bury the evidence in a table nobody
   reads.

Read the whole trail — live and archived — through `v_audit_trail`. A student
history query should use that rather than `audit_log`, so an archived entry does
not silently vanish from an investigation.

## What to watch

- `flowed_audit_ship_lag` — entries not yet copied off-host. Archival is blocked
  behind this by design, so a stalled shipper eventually shows up as a table
  that will not shrink.
- The newest row in `audit_verification` — if it is old, the nightly check has
  stopped running, which looks exactly like a system with nothing wrong.
- `pg_total_relation_size('audit_log')` against the projection above. A number
  well ahead of it means something is writing entries nobody expected, and that
  is worth understanding before it is worth archiving.
