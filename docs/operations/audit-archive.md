# The audit archive

Every command that touches money writes an entry to `audit_log`, and each entry
is hash-chained to the one before it. That chain proves no entry was **edited**:
change a field and its hash stops matching, and `verify_audit_chain(0)` says so.

It cannot prove no entry was **removed**. Delete the last hundred rows and
everything left verifies perfectly, because verification walks what is present.
Truncation is the blind spot, and it is the likelier story — an administrator
removing the record of what they did is ordinary; forging a sha256 is not.

The control for that is a copy somewhere this database cannot reach.

## What it does

Every quarter of an hour (and on demand) the shipping job takes the entries
written since the last block, writes them as newline-delimited JSON to the
archive destination, and records the block: the range it covered, the chain hash
at each end, the checksum of the bytes, and where they went.

Verification then asks four questions, and each catches something the others
cannot:

| Question | What it catches |
|---|---|
| Do the shipments cover a contiguous range? | A block of entries that never left the host |
| Do the archived bytes still hash to what was recorded? | An archive edited in place |
| Does every archived entry still exist in the database, with the same hash? | **A deleted entry** — the check nothing else can make |
| Does each entry in a block follow the one before it? | A block assembled from unrelated entries |

```bash
api audit-ship            # copy everything not yet copied
api audit-ship verify     # compare the archive against the database
```

`verify` exits non-zero when the two disagree, so a monitoring check can run it
directly. The same two operations are on the API for an auditor who has no
shell: `POST /api/v1/audit/archive/ship` and `GET /api/v1/audit/archive/verify`.

Verification answers `200` with `ok: false` when it finds something. The request
succeeded; what it found is the answer. A 5xx would read as "the check failed",
and those two must not look alike.

## Configuring a destination

```bash
AUDIT_ARCHIVE_DIR=/srv/audit-archive       # a directory, in practice a mount
AUDIT_ARCHIVE_INTERVAL=15m
AUDIT_ARCHIVE_BATCH=500
```

The directory has to be somewhere this host cannot rewrite. That is a property
of the deployment, not of this code, and it is the whole value of the control:

```
# on the archive host, /etc/exports
/srv/audit-archive  10.0.0.11(rw,sync,no_subtree_check,root_squash)
```

Files are written `0400` and never opened for writing twice; a block that
already exists is a duplicate shipment, and overwriting it would destroy the
only copy of what was actually sent. An archive host that also refuses
`unlink` and `write` on existing files — a WORM mount, an object store with
object-lock, or simply a different account — is what turns "hard to tamper
with" into "cannot".

The alternative is an append-only endpoint:

```bash
AUDIT_ARCHIVE_URL=https://archive.university.edu.iq/flowed
AUDIT_ARCHIVE_SECRET=<32+ random bytes>
```

Each block is PUT with an HMAC-SHA256 of the body in `X-Flowed-Signature`. The
secret is required: an unsigned append endpoint accepts entries from anyone who
finds the URL. A receiver that already holds a block should answer `409`, which
is read as "already there" rather than as a failure — that is what a retry after
a lost response looks like.

## Reading the reports

```json
{
  "ok": false,
  "report": {
    "destination": "dir:/srv/audit-archive",
    "shipments": 412,
    "entries_checked": 205318,
    "unshipped_entries": 0,
    "archive_readable": true,
    "problems": [
      {
        "kind": "entry_missing_from_database",
        "sequence_no": 184402,
        "artifact": "audit-000000184001-000000184500.ndjson",
        "detail": "the archive holds payment.voided on payment by n.hassan; the database no longer does"
      }
    ]
  }
}
```

| `kind` | What happened | What to do |
|---|---|---|
| `entry_missing_from_database` | An entry was deleted from `audit_log` | Treat as an incident. The archived copy names the action and the operator |
| `entry_altered` | A row's hash no longer matches the shipped copy | The same |
| `archive_altered` | The archived bytes were changed | Somebody has write access to the archive that they should not |
| `coverage_gap` | A range never shipped | Usually shipping was off for a period; check when, and what happened then |
| `chain_break` | A block's entries do not follow each other | Almost always a symptom of one of the above |
| `archive_unreadable` | A block could not be read | The archive host, or a deleted file |

`archive_readable: false` means the destination accepts writes but will not
serve reads back to this host. That is a legitimate and slightly stronger
deployment; verification then checks the shipment records and the coverage, and
says which half it could not do rather than reporting a verified archive it
never read. Verify from the archive host in that case.

## What to watch

- `flowed_audit_ship_lag` — entries written but not yet copied. A number that
  stops falling is shipping that has quietly stopped; everything else looks
  healthy while it climbs.
- `flowed_audit_ship_failures_total` — by stage (`read`, `encode`, `write`,
  `record`, `verify`). Any value at `verify` is an incident, not a defect
  report.
- `flowed_scheduler_job_runs_total{job="audit_ship",outcome="defect"}` — a pass
  that ended still behind.

## If a destination was never configured

The system runs, and the log says at every start-up that the trail is not
copied off-host. The API routes stay mounted and answer with
`audit.archive_not_configured` and the setting to change, so "is the trail
archived here?" is a question with an answer rather than a 404.

That is a weaker system, and it should be a decision rather than an oversight.
