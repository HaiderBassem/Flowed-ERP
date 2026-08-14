#!/usr/bin/env bash
# Shared helpers for the operational scripts.
#
# Sourced, never executed. Everything here is deliberately POSIX-plain: these
# run on a university server that may be an older Ubuntu with whatever bash it
# shipped with, and on a developer's macOS where sha256sum does not exist.

set -euo pipefail

# Connection settings, resolved the same way the Makefile and .env.example do,
# so a backup is taken against the database the application actually uses.
DB_NAME="${DB_NAME:-flowed_dev}"
DB_USER="${DB_USER:-${USER:-postgres}}"
DB_HOST="${DB_HOST:-localhost}"
DB_PORT="${DB_PORT:-5432}"

export PGHOST="$DB_HOST" PGPORT="$DB_PORT" PGUSER="$DB_USER"

# The password may arrive as a file, which is how Docker and Kubernetes hand a
# process a secret and materially safer than an environment variable: a
# variable is visible in `docker inspect` and in /proc/<pid>/environ. The
# application reads DB_PASSWORD_FILE the same way.
if [ -n "${DB_PASSWORD_FILE:-}" ] && [ -r "${DB_PASSWORD_FILE}" ]; then
	DB_PASSWORD="$(tr -d '\r\n' < "$DB_PASSWORD_FILE")"
fi
[ -n "${DB_PASSWORD:-}" ] && export PGPASSWORD="$DB_PASSWORD"

BACKUP_DIR="${BACKUP_DIR:-./backups}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"
# Never prune below this many, however old they are. A retention window alone
# would delete every backup on a system that has been down for a month, which
# is precisely when the oldest one is the only one left.
RETENTION_MIN_KEEP="${RETENTION_MIN_KEEP:-3}"

say()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not on PATH"
}

# checksum prints the sha256 of a file, with the tool that exists on this host.
checksum() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# file_bytes prints a file's size. stat's flags differ between GNU and BSD.
file_bytes() {
	if stat -c %s "$1" >/dev/null 2>&1; then
		stat -c %s "$1"
	else
		stat -f %z "$1"
	fi
}

# psql_value runs a query and prints the single value it returns.
psql_value() {
	psql --dbname="${2:-$DB_NAME}" --tuples-only --no-align --quiet --command="$1"
}

# now_stamp is the timestamp used in file names: sortable, no colons, so it is
# still a legal name on every filesystem the university might copy it to.
now_stamp() { date -u +%Y%m%dT%H%M%SZ; }
