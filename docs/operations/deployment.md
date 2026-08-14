# Deploying Flowed

Two supported shapes, and the choice is about who maintains the machine rather
than about scale:

- **systemd** (`deploy/systemd/`) — one binary, one unit file, `systemctl status`
  says what is wrong. For a university server room where the person on call
  knows Linux and not Docker, this is the better one.
- **Docker Compose** (`deploy/docker-compose.prod.yml`) — the database, the API,
  migrations and the backup job as one file.

Both put a reverse proxy in front (`deploy/nginx/flowed.conf`) which terminates
TLS. The API binds loopback and speaks plain HTTP; it never terminates TLS
itself.

There is no cluster configuration, on purpose. The failure modes of a cluster
nobody on site can debug are worse than the failure modes of one host with a
tested restore, and the tested restore is what this system spends its effort on.

## What must be true before the first start

**The build must identify itself.** `api version` prints a version, a commit and
whether the tree was clean. In production a build that cannot say what it is
refuses to serve — a deployment nobody can trace to a tree is one nobody can
roll back with confidence. `make build` and the Dockerfile both stamp it; a bare
`go build` does not, which is why the image runs `api version` at build time.

**Secrets are files, not variables.** An environment variable is visible in
`docker inspect`, in `/proc/<pid>/environ` to anything running as the same user,
and in a crash dump.

```bash
mkdir -p /etc/flowed && chmod 750 /etc/flowed
openssl rand -base64 48 > /etc/flowed/jwt-secret
openssl rand -base64 32 > /etc/flowed/db-password
chmod 640 /etc/flowed/*
chown root:flowed /etc/flowed/*
```

Then point the settings at them:

```ini
AUTH_JWT_SECRET_FILE=/etc/flowed/jwt-secret
DB_PASSWORD_FILE=/etc/flowed/db-password
```

Every setting supports the `_FILE` form. The file wins when both are set,
because a deployment that mounts a secret and leaves an old variable behind
means the mounted one.

**The audit archive must be somewhere this host cannot rewrite.** Set
`AUDIT_ARCHIVE_DIR` to a mount of a directory on another machine. Without it the
system runs and says at every start-up that the trail is not copied off-host —
the hash chain will detect an altered entry and cannot detect a deleted one.

**Name the proxy.** `HTTP_TRUSTED_PROXIES` must list the reverse proxy, or every
audit entry records the proxy's address as the client and the rate limiter
buckets the whole university into one key.

## systemd

```bash
useradd --system --home /opt/flowed --shell /usr/sbin/nologin flowed
install -o root -g flowed -m 0750 -d /opt/flowed /etc/flowed /srv/flowed/backups
install -o root -g root  -m 0755 api migrate /opt/flowed/
install -o root -g root  -m 0755 -d /opt/flowed/scripts
install -o root -g root  -m 0755 scripts/*.sh /opt/flowed/scripts/

install -o root -g flowed -m 0640 flowed.env /etc/flowed/flowed.env
cp deploy/systemd/*.service deploy/systemd/*.timer /etc/systemd/system/

systemctl daemon-reload
systemctl enable --now flowed-api
systemctl enable --now flowed-backup.timer flowed-restore-drill.timer flowed-audit-verify.timer
```

The unit runs `migrate up` before the API starts, and the API validates the
schema at boot and refuses to serve on a mismatch — so a partial deployment
stops rather than serving queries against a schema this build does not
understand.

Three timers, and none of them is optional:

| timer | when | why |
|---|---|---|
| `flowed-backup` | 01:30 daily | the archive |
| `flowed-restore-drill` | Sunday 03:30 | a backup nobody has restored is a hypothesis |
| `flowed-audit-verify` | 05:00 daily | the only check that can see a *deleted* audit entry |

`flowed-audit-verify` exits non-zero when the archive and the database disagree,
which systemd reports as a failed unit. For an installation with no Prometheus,
that is the alerting path.

## Docker Compose

```bash
cd deploy
mkdir -p secrets audit-archive backups postgres/wal && chmod 700 secrets
openssl rand -base64 48 > secrets/jwt_secret
openssl rand -base64 32 > secrets/db_password
chmod 600 secrets/*

FLOWED_VERSION=1.4.0 docker compose -f docker-compose.prod.yml up -d
```

`FLOWED_VERSION` has no default on purpose: `latest` is how a host ends up
running a build nobody chose, and a rollback needs a version to roll back to.

The containers run read-only with no capabilities and `no-new-privileges`; the
API's only writable path is `/tmp`. The database is not published — the API
reaches it over the compose network, and a database on the public interface is
the most common way a system like this is lost.

## Upgrading

1. Take a backup and check it: `make backup` prints a checksum and verifies the
   archive is readable.
2. Read the migrations in the release. Down scripts are mandatory and are the
   rollback path, but a down script that drops a column drops what was in it.
3. Deploy. Migrations run first; the API refuses to start on a mismatch.
4. Watch: `flowed_reconciliation_open` should stay at zero and the first
   reconciliation pass after the deploy is the one that would notice a
   regression in the caches.

Rolling back means the previous binary *and* rolling the migrations back to the
version it expects. A new binary against an old schema refuses to serve, which
is the safe direction; an old binary against a new schema also refuses, which is
the same protection working the other way.

## Draining

`SIGTERM` stops accepting, finishes what is in flight, and waits for any
scheduled job holding an advisory lock. Both unit and compose file allow 45
seconds for this, and the wait is real rather than a formality: a process killed
mid-job leaves that lock until PostgreSQL notices the connection is gone, and
until then the next process skips that job.

## Running more than one API process

The design supports it — the rate limiter is in PostgreSQL, scheduled jobs take
advisory locks so only one replica runs each, and every money command serialises
on a row lock rather than in memory.

What it does not do is make the desks faster. The measurements in
`docs/operations/performance.md` show the database as the bound, so a second API
process on the same machine buys queueing, not throughput. Add one when the
first is CPU-bound, which at these numbers it is not.

## The checks after any deployment

```bash
api version                                   # what is running
curl -fsS localhost:8080/health               # it is up
curl -fsS localhost:8080/ready                # the database is reachable
psql flowed -c "SELECT count(*) FROM v_account_reconciliation"      # 0
psql flowed -c "SELECT count(*) FROM v_installment_reconciliation"  # 0
psql flowed -c "SELECT count(*) FROM v_over_refunded_payments"      # 0
psql flowed -c "SELECT count(*) FROM verify_audit_chain(0)"         # 0
api audit-ship verify                         # the off-host copy agrees
```

All four queries return zero on a healthy system. A row in any of them is a
defect, and the reconciliation queue at `/api/v1/reconciliation/findings` is
where it is worked.
