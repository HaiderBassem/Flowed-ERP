# Backup, restore and point-in-time recovery

This system is the university's record of what every student owes and what
every student paid. Losing a day of it means asking students to prove they
paid, and the ones with receipts will be believed while the ones without will
not — so the recovery story is a financial control, not an IT chore.

Three things have to be true, and each is checked rather than assumed:

1. A backup exists and is readable. `make backup` verifies the archive in the
   same run that produced it.
2. It restores into a database that reconciles. `make restore-drill` proves it,
   and prints how long the restore took — that number is the recovery time.
3. The hours since the last backup are recoverable. That is WAL archiving,
   configured on the server; `make backup` warns every night that it is off
   until it is on.

## What is backed up

`scripts/backup.sh` takes a custom-format `pg_dump` of the whole database:
schema, data, indexes, triggers, views and the migration ledger. Beside each
archive it writes a manifest recording the checksum, the schema migration
version, the server version and the row counts of `student`,
`financial_account`, `payment` and `audit_log` at the moment of the dump.

The row counts are what catch a restore that silently dropped a table: an empty
table passes every reconciliation view, because nothing in it disagrees with
nothing else.

```bash
make backup                       # into ./backups
BACKUP_DIR=/srv/backups make backup
RETENTION_DAYS=90 make backup
BACKUP_GPG_RECIPIENT=finance@university.edu.iq make backup   # encrypted at rest
```

Retention prunes archives older than `RETENTION_DAYS` (default 30) but always
keeps the newest three whatever their age — a system that has been quiet for a
month must not be left with nothing.

### Nightly

```cron
# /etc/cron.d/flowed-backup
30 1 * * * flowed BACKUP_DIR=/srv/backups /opt/flowed/scripts/backup.sh >> /var/log/flowed/backup.log 2>&1
30 3 * * 0 flowed BACKUP_DIR=/srv/backups /opt/flowed/scripts/restore-drill.sh >> /var/log/flowed/drill.log 2>&1
```

The weekly drill line is not optional. A backup nobody has restored is a
hypothesis, and the morning it is needed is the wrong morning to test it.

### Off the machine

An archive on the same disk as the database survives a dropped table and
nothing else. Copy it to a second host the same night — `rsync` over ssh to a
machine in a different building is enough, and it is what the university can
actually operate.

```bash
rsync -a --remove-source-files /srv/backups/ backup-host:/srv/flowed/
```

If the copy leaves the campus, encrypt it: `BACKUP_GPG_RECIPIENT` makes the
archive unreadable without the key, and the key does not live on the backup
host.

## Restoring

### The whole database

```bash
createdb flowed_restored
pg_restore --dbname=flowed_restored --no-owner --no-privileges --jobs=4 \
           --exit-on-error /srv/backups/flowed-20260814T013000Z.dump
```

Then, before pointing the application at it:

```bash
psql flowed_restored -c "SELECT count(*) FROM v_account_reconciliation"
psql flowed_restored -c "SELECT count(*) FROM v_installment_reconciliation"
psql flowed_restored -c "SELECT count(*) FROM v_over_refunded_payments"
psql flowed_restored -c "SELECT count(*) FROM verify_audit_chain(0)"
```

All four must be zero. `make restore-drill` runs exactly this against a scratch
database and drops it afterwards.

### One table

Do not. Restoring `payment` alone into a live database produces payments whose
allocations do not exist and accounts whose caches do not match — the four
queries above would show it, but the desks would have taken money against it
first. Restore the whole database into a scratch copy, find what is needed, and
correct the live system through the ordinary commands: a void, a refund, an
adjustment. Every correction path in this system is a new row for exactly this
reason.

## Point-in-time recovery

A nightly archive means a failure at 16:00 loses the day's collection: every
receipt printed since 01:30 exists on paper in students' hands and nowhere
else. WAL archiving closes that gap to seconds.

On the database server:

```ini
# postgresql.conf
wal_level = replica
archive_mode = on
archive_command = 'test ! -f /srv/wal/%f && cp %p /srv/wal/%f'
archive_timeout = 300          # a segment at least every five minutes
```

`/srv/wal` must be on a different disk from the data directory, and is copied
off the host on the same schedule as the archives. `archive_timeout` bounds the
loss window on a quiet evening, when a 16 MB segment might otherwise take hours
to fill.

Take a base backup — a file-level copy the WAL can be replayed onto:

```bash
pg_basebackup --pgdata=/srv/base/$(date -u +%Y%m%d) --format=tar --gzip \
              --wal-method=stream --checkpoint=fast --progress
```

To recover to a moment — say, just before a bad bulk operation at 11:42:

```bash
systemctl stop flowed-api postgresql
mv /var/lib/postgresql/18/main /var/lib/postgresql/18/main.broken
tar -xzf /srv/base/20260814/base.tar.gz -C /var/lib/postgresql/18/main
cat >> /var/lib/postgresql/18/main/postgresql.auto.conf <<'CONF'
restore_command = 'cp /srv/wal/%f %p'
recovery_target_time = '2026-08-14 11:42:00+03'
recovery_target_action = 'promote'
CONF
touch /var/lib/postgresql/18/main/recovery.signal
systemctl start postgresql
```

PostgreSQL replays the WAL to that instant and promotes. Watch the log for
`recovery stopping before commit of transaction`, then run the four
reconciliation queries before starting the API. Only start `flowed-api` once
they are all zero: the binary validates the schema at boot, but nothing except
those queries checks the money.

Note the timezone in `recovery_target_time`. Baghdad is +03 and the server
clock is UTC; a three-hour error here recovers to the wrong morning.

### What point-in-time recovery cannot undo

Recovery to 11:42 discards everything after 11:42, including receipts printed
at 11:50 that students are holding. If the problem is a bad operation rather
than a broken disk, the correct answer is almost always a correction — a void,
a refund, an adjustment — not a rewind. Rewinding is for the case where the
data is gone, not for the case where it is wrong.

## Recovery objectives, as measured

| | Objective | How it is met | Measured |
|---|---|---|---|
| RPO, WAL archiving on | ≤ 5 minutes | `archive_timeout = 300` | segment age in `pg_stat_archiver` |
| RPO, archives only | ≤ 24 hours | nightly 01:30 | archive timestamps |
| RTO | under an hour | parallel `pg_restore` | printed by every drill |

The drill prints the restore time for the current dataset each run; on the
development dataset it is under a second, and it grows with the data rather
than with the schema. Record the number from the production drill in the
operations log — an RTO nobody has measured is a number in a document.

## When the database will not start

Do not initialise a new cluster over the old data directory. Move it aside
(`mv main main.broken`) and restore beside it: the broken directory is often
readable enough to recover the last transactions that the archives do not have,
and once it is overwritten that option is gone.
