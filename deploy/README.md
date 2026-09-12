# Running Flowed on a real server with Docker

This is the full path from a bare Linux machine to a university collecting money
through Flowed. Every command here was run against this compose file and the
output quoted is real; where something refuses, the refusal is quoted too,
because the refusals are the part that costs an afternoon if you meet them
without warning.

If you maintain the machine with `systemctl` rather than `docker`, use
`docs/operations/deployment.md` instead — the systemd path is not worse, it is
for a different person on call.

---

## 1. What you are deploying

One host. Five containers, of which two are one-shot:

| Container | Lives | What it is for |
|---|---|---|
| `pg-tls` | exits immediately | Generates the database's TLS material once, into its own volume |
| `postgres` | always | PostgreSQL 18. Not published: reachable only from the other containers |
| `migrate` | exits at each deploy | Applies migrations, then exits 0. The API waits for it |
| `api` | always | The application and its embedded web interface, on `127.0.0.1:8080` |
| `backup` | always | A clock. Backup at 01:30, restore drill Sunday 03:30 |

In front of them, on the host, nginx terminates TLS and forwards to
`127.0.0.1:8080`. The API never terminates TLS and never listens on a public
address.

This is a single-host deployment on purpose. The failure modes of a cluster
nobody on site can debug are worse than the failure modes of one host with a
tested restore — and the restore drill below is what makes that trade honest.

## 2. What the machine needs

- **Docker Engine 24+ with the Compose plugin.** Verified here on Docker 29.6
  and Compose v5.3. `docker compose version` must answer.
- **4 GB RAM minimum, 8 GB comfortable.** The compose file gives PostgreSQL
  1 GB of shared buffers and the API a 1 GB ceiling.
- **Disk**: the database, plus 30 days of backups. The audit trail runs about
  500 bytes an entry, so it is not what fills the disk; the backups are.
- **A DNS name and a TLS certificate** for it (`fees.university.edu.iq` in the
  examples).
- **A directory on another machine**, mounted here, for the audit archive. Read
  §6 before deciding this is optional — it is the only control in the system
  that can detect a *deleted* audit entry.
- The clock synchronised (`chronyd` or `systemd-timesyncd`). Receipt times,
  installment due dates and the shift close all read it.

## 3. Get the image onto the server

The image builds the web interface, compiles both binaries, stamps them with a
version and commit, and then **runs `api version` during the build** — so an
image that cannot say what it is fails at build time rather than in production.

**Build it where you have the source:**

```bash
make docker-build          # from the repository root, always
```

That produces `flowed:<VERSION>` and `flowed:latest`, where `VERSION` comes from
the `VERSION` file. It is a multi-stage build; the result is about 86 MB.

**Then get it to the server**, whichever fits your network:

```bash
# A registry you control
docker tag flowed:1.0.0 registry.university.edu.iq/flowed:1.0.0
docker push registry.university.edu.iq/flowed:1.0.0

# Or no registry at all — a file on a USB stick works
docker save flowed:1.0.0 | gzip > flowed-1.0.0.tar.gz
#   on the server:
gunzip -c flowed-1.0.0.tar.gz | docker load
```

Never deploy `latest`. `FLOWED_VERSION` has no default in the compose file for
exactly this reason: `latest` is how a host ends up running a build nobody
chose, and a rollback needs a version to roll back *to*.

## 4. Prepare the deployment directory

Copy the `deploy/` directory to the server — say `/opt/flowed/deploy` — then,
inside it:

```bash
mkdir -p secrets audit-archive backups postgres/wal
chmod 700 secrets

openssl rand -base64 48 > secrets/jwt_secret
openssl rand -base64 32 > secrets/db_password
chmod 600 secrets/*
```

Secrets are **files, not environment variables**. A variable is visible in
`docker inspect`, in `/proc/<pid>/environ` to anything running as the same user,
and in a crash dump. Every setting in Flowed accepts a `_FILE` form, and the
file wins when both are set.

> Passwords from `openssl rand -base64` contain `/` and `+` freely, and that is
> fine — the connection string percent-encodes them. It did not always: see
> §11.

Now write `.env` beside the compose file. Compose reads it automatically. **No
secrets go in here** — this file holds the choices, not the keys:

```ini
# The image to run and the exact version. Both are mandatory.
FLOWED_IMAGE=registry.university.edu.iq/flowed
FLOWED_VERSION=1.0.0

# The address the university reaches this system at. It becomes the allowed
# browser origin; production refuses the "*" default.
FLOWED_PUBLIC_ORIGIN=https://fees.university.edu.iq

# What the receipt letterhead says.
RECEIPT_UNIVERSITY_NAME=جامعة بغداد الأهلية
```

If you built the image locally on the server and did not push it anywhere, set
`FLOWED_IMAGE=flowed`.

## 5. Two settings that are wrong by default for your site

**The trusted proxy.** `HTTP_TRUSTED_PROXIES` in the compose file is
`172.16.0.0/12`, the usual Docker bridge range. It must cover the address nginx
connects from. Get this wrong and every audit entry records the proxy as the
client, and the rate limiter buckets the entire university into one key.

**The audit archive.** `AUDIT_ARCHIVE_DIR` is mounted from `./audit-archive`,
a directory on this same disk — which is *worse than nothing*, because it looks
like a control while offering none. The hash chain detects an altered entry and
cannot detect a deleted one: verification walks what is present, so a removed
tail leaves an intact chain behind it. Point the mount at another machine:

```yaml
    volumes:
      - /mnt/audit-archive:/srv/audit-archive   # an NFS/SMB mount of another host
```

The account this container writes as should be able to create files there and
not to rewrite them. An append-only HTTP endpoint works too
(`AUDIT_ARCHIVE_URL` with `AUDIT_ARCHIVE_SECRET`).

## 6. First start

```bash
cd /opt/flowed/deploy
docker compose -f docker-compose.prod.yml up -d
```

What should happen, in order: `pg-tls` generates a certificate and exits 0;
`postgres` comes up and reports healthy; `migrate` applies every migration and
exits 0; `api` starts. Confirm:

```bash
docker compose -f docker-compose.prod.yml ps -a
```

```
api        Up 12 seconds (healthy)
backup     Up 12 seconds
migrate    Exited (0) 12 seconds ago
pg-tls     Exited (0) 18 seconds ago
postgres   Up 18 seconds (healthy)
```

`migrate` and `pg-tls` showing **Exited (0)** is success, not failure. If
`migrate` exits non-zero the API never starts — a partial deploy stops rather
than serving queries against a schema this build does not understand.

The API's own log tells you the three things worth knowing:

```json
{"msg":"database connected","dsn":"[redacted]","max_conns":25}
{"msg":"audit trail is copied off-host","destination":"dir:/srv/audit-archive"}
{"msg":"scheduler started","jobs":11}
```

If the second line says the trail is *not* copied off-host, go back to §5.

## 7. Create the first administrator

`api seed` deliberately refuses here:

```
api: refusing to seed in production: create the first administrator
deliberately, with a password nobody else has seen
```

So create the account explicitly:

```bash
docker compose -f docker-compose.prod.yml run --rm \
  --entrypoint /app/api api create-user admin "System Administrator" admin
```

```
created admin (01a002a0-…) with roles [admin]
temporary password: 6sy3q3z4ayrshjwmpgmsydqk
the holder must change this password before any other route will answer them
```

That password is printed once and is not recoverable. The holder must change it
at first login before any other route answers them. From there, create the real
operators through the interface (**المستخدمون والجلسات**) — roles are
`admin`, `finance_manager`, `cashier`, `registrar`, `academic_officer`,
`auditor`, `report_viewer`.

**A cashier needs a desk.** A cashier account without `cashier_desk_id` cannot
sign in at all (`auth.cashier_desk_required`), because receipt numbers run per
desk per year. Create the desks in the reference-data screen first.

## 8. Put nginx in front

`nginx/flowed.conf` is a working configuration; change the `server_name` and the
certificate paths. Three things in it are load-bearing rather than decorative:

- `X-Forwarded-For`, paired with `HTTP_TRUSTED_PROXIES` above.
- A 180-second timeout on `/api/v1/reports/` — a debt report over a large year
  outlives a default proxy timeout, and a proxy that gives up mid-response
  leaves the operator with half a spreadsheet and no error.
- `location = /metrics { return 404; }` — the scrape surface enumerates every
  route and its error rate. It binds loopback inside the container and its bind
  address is its only access control.

Get a certificate (certbot or your ministry's CA), then reload nginx. The
interface is then at `https://fees.university.edu.iq/app/`.

## 9. Verify the deployment

Run all of it. Every line here was run against this stack.

```bash
cd /opt/flowed/deploy
C="docker compose -f docker-compose.prod.yml"

$C exec -T api /app/api version
# version=1.0.0 commit=29f76c4ec257 built=2026-08-14T23:25:33Z go=go1.26.6

curl -fsS localhost:8080/health
# {"service":"flowed-tuition","status":"ok","version":"1.0.0"}

curl -fsS localhost:8080/ready
# {"database":{"idle_conns":6,"max_conns":25,...},"status":"ready"}

curl -o /dev/null -w '%{http_code}\n' localhost:8080/app/
# 200
```

Then the four money invariants. **All four must return zero**, now and after
every upgrade:

```bash
for v in v_account_reconciliation v_installment_reconciliation \
         v_over_refunded_payments "verify_audit_chain(0)"; do
  printf '%-32s ' "$v"
  $C exec -T postgres psql -U flowed -d flowed -tAc "SELECT count(*) FROM $v"
done
```

```
v_account_reconciliation         0
v_installment_reconciliation     0
v_over_refunded_payments         0
verify_audit_chain(0)            0
```

A row in any of them is a defect, and the reconciliation queue at
`/api/v1/reconciliation/findings` is where it gets worked. The remedy for a
drifting cache is never an `UPDATE` to the cache.

Then prove the off-host copy agrees:

```bash
$C exec -T api /app/api audit-ship verify
# archive verified: 1 shipments, 2 entries, 0 entries not yet shipped
```

And confirm the database traffic is actually encrypted:

```bash
$C exec -T postgres psql -U flowed -d flowed \
  -c "SELECT ssl, version, count(*) FROM pg_stat_ssl s
      JOIN pg_stat_activity a USING (pid)
      WHERE a.usename='flowed' GROUP BY 1,2"
```

```
 ssl | version | count
-----+---------+-------
 t   | TLSv1.3 |     6
```

Finally, confirm nothing is exposed that should not be:

```bash
$C ps --format '{{.Service}}\t{{.Ports}}'
# api        127.0.0.1:8080->8080/tcp
# postgres   5432/tcp          ← no host binding: correct
```

## 10. Running it day to day

### Backups, and the drill that makes them real

The `backup` container is a clock: a backup at 01:30, a restore drill on Sundays
at 03:30. Both can be run by hand, and should be at least once before you trust
them:

```bash
$C exec -T backup /app/scripts/backup.sh
```

```
  archive:   /srv/backups/flowed-20260814T233616Z.dump
  sha256:    dc39e3869c293e3964a7aee1d7ec97ca85375a1285822ec6ad395961bbf31f17
  schema:    migration 24, server 18.6
  verified:  pg_restore can read it and it carries every financial table
  wal:       archiving on, last archived 2026-08-14 23:32:34+00, 0 failures
```

The archive is verified in the same run that produced it. The drill goes
further — it restores into a scratch database and reconciles it:

```bash
$C exec -T backup /app/scripts/restore-drill.sh
```

```
  restored in 1s — that is the recovery time for this dataset
  ok    schema is at migration 24, matching this build
  ok    account caches match their transactions
  ok    installment caches match their allocations
  ok    no payment is refunded beyond what it took
  ok    the audit chain verifies end to end
Drill passed: the archive restores into a database that reconciles.
```

A backup nobody has restored is a hypothesis. Copy `backups/` off this machine
as well — a backup on the disk that fails is not a backup.

### Logs

```bash
$C logs -f api                        # JSON, one object per line
$C logs api | grep '"level":"ERROR"'
$C logs api | grep invariant_violation # this one should never match
```

### Metrics

Prometheus metrics are on a **separate listener bound to loopback inside the
container**, deliberately unreachable from the host. To scrape from elsewhere,
publish it explicitly and firewall the port:

```yaml
    environment:
      OBS_METRICS_ADDR: "0.0.0.0:9464"
    ports:
      - "127.0.0.1:9464:9464"     # then let Prometheus reach it over a tunnel
```

`deploy/alerts.yml` carries the rules that matter, including
`FlowedSchedulerJobStopped` — a checker that stopped checking looks exactly like
a system with nothing wrong.

### Upgrading

```bash
$C exec -T backup /app/scripts/backup.sh     # 1. take one, and read the output
# 2. read the migrations in the release; down scripts are the rollback path,
#    but a down script that drops a column drops what was in it
sed -i 's/^FLOWED_VERSION=.*/FLOWED_VERSION=1.1.0/' .env   # 3.
$C pull && $C up -d                                        # 4.
$C exec -T api /app/api version                            # 5. confirm
```

Migrations run first; the API refuses to start on a schema mismatch. Rolling
back means the previous image **and** rolling the migrations back to the version
it expects — a new binary against an old schema refuses to serve, and an old
binary against a new schema also refuses, which is the same protection working
in both directions.

### Stopping

```bash
$C stop api        # SIGTERM: stops accepting, finishes what is in flight,
                   # waits for any scheduled job holding an advisory lock
$C down            # everything, volumes kept
$C down -v         # everything, DATABASE DELETED
```

The 45-second grace period is real, not a formality: a process killed mid-job
leaves its advisory lock until PostgreSQL notices the connection is gone, and
until then the next process skips that job.

### A staging box

On a machine that is not production, `APP_ENV=staging` plus `api demo` loads a
dataset generated **through the real commands**, so a successful load is itself
evidence the system works. `api demo` refuses to run in production.

## 11. When it refuses to start

Every message below is one this stack actually produced.

**`DB_SSLMODE must not be disable in production`** and
**`HTTP_CORS_ORIGINS must list explicit origins in production`**
The configuration is validated as a whole at start-up. The compose file now
enables TLS on PostgreSQL (via the `pg-tls` step) and takes the origin from
`FLOWED_PUBLIC_ORIGIN`; if you see this, `.env` is missing or the file was
edited. Note that **`migrate` is held to the same rules** even though it never
serves a request — which is why the migrator is given a signing secret it does
not use.

**`AUTH_JWT_SECRET still holds the development default`**
The `jwt_secret` file is not mounted into that service, or is empty. Check
`secrets/` exists and the service lists `jwt_secret`.

**`cannot parse ...: invalid port ":VVpvi…" after host`**
A password containing `/` used to truncate the connection URL. Fixed — the DSN
now percent-encodes credentials, pinned by a test. If you meet this, your image
predates the fix.

**`Error: in 18+, these Docker images are configured to store database data
in a format ...`**
The data volume is mounted at `/var/lib/postgresql/data`. PostgreSQL 18 images
want it at `/var/lib/postgresql`. Fixed in this compose file.

**`env: can't execute 'bash': No such file or directory`**
The runtime image lacked bash, so the nightly backup and weekly drill failed
every time they ran. Fixed — bash is installed in the image.

**`ls: migrations/*.up.sql: No such file or directory`** during the drill
The image embeds migrations rather than shipping files; the drill now asks the
binary instead. Fixed.

**`dependency failed to start: container flowed-postgres-1 is unhealthy`**
Read `docker logs flowed-postgres-1`. Usually the volume-path issue above, or a
`shared_buffers=1GB` the machine cannot give.

**A cashier cannot sign in**
`auth.cashier_desk_required` — the account has no desk. Assign one; receipt
series run per desk per year.

## 12. What this runbook changed

Deploying this stack for the first time surfaced six defects, all fixed in the
repository, all verified by running the stack afterwards:

1. **`Dockerfile`** — asked for `postgresql17-client`, which Alpine 3.20 does
   not carry; the image could not be built at all. Base moved to Alpine 3.23
   with `postgresql18-client`, which also matches the PostgreSQL 18 server —
   `pg_dump` refuses to dump from a server newer than itself, so a mismatched
   client turns the nightly backup into a nightly error.
2. **`Dockerfile`** — no `bash`, so both operational scripts failed on every
   scheduled run.
3. **`internal/platform/config/config.go`** — the DSN interpolated the password
   into a URL unescaped. `openssl rand -base64 32`, which every instruction here
   tells you to run, produces a `/` about half the time, making a first
   deployment a coin flip on an error that named a port nobody configured. Now
   escaped, with `dsn_test.go` pinning it.
4. **`docker-compose.prod.yml`** — PostgreSQL data volume on the pre-18 path.
5. **`docker-compose.prod.yml`** — `APP_ENV=production` with
   `DB_SSLMODE=disable` and no CORS origins: refused by the application's own
   validation. TLS is now generated for PostgreSQL by a one-shot step and the
   origin comes from `.env`. `migrate` and `backup` also receive the signing
   secret their whole-config validation demands.
6. **`scripts/restore-drill.sh`** — read `migrations/*.up.sql` from disk, which
   does not exist in the image; it died after reporting a successful restore and
   before checking a single invariant. It now asks the binary, and never lets
   that lookup abort the drill.

## 13. The security posture, in one place

- Containers run **read-only**, with **all capabilities dropped** and
  `no-new-privileges`. The API's only writable path is a 64 MB tmpfs.
- **PostgreSQL is not published.** Only the compose network reaches it.
- **The API binds `127.0.0.1:8080`.** Only nginx reaches it; TLS is terminated
  there.
- **Metrics bind loopback inside the container** and are 404'd at the proxy.
- **Secrets are files**, mounted at `/run/secrets`, never variables.
- **Database traffic is encrypted** (`sslmode=require`, TLS 1.3 confirmed).
  Self-signed means encrypted, not authenticated — when the database moves to
  another host, issue a real certificate and move to `verify-full`.
- **The audit trail leaves the host** every 15 minutes. That interval is your
  maximum exposure to an undetectable deletion.
- The application enforces the rest itself and will not be talked out of it:
  four-eyes on voids, refunds, discounts and drawer approvals, down to database
  CHECK constraints; append-only financial rows enforced by triggers; and a
  refusal to close a year over unexplained drift.
