// Package backup is the only place in this codebase that shells out to
// another program. pg_dump and pg_restore do the actual dump and restore
// work; nothing here re-implements what they already do correctly, and
// nothing above this package — internal/app/backup_service.go — knows that a
// process is involved at all. It sees a Runner interface and calls its
// methods.
//
// Every database-administration statement here (CREATE DATABASE, DROP
// DATABASE, ALTER DATABASE ... RENAME, pg_terminate_backend) runs over its own
// short-lived connection to the maintenance database, never through the
// application's pool: a database cannot rename itself while a connection to
// it is open, and pg_terminate_backend on the application's own pool would be
// the app closing its own legs mid-stride.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/platform/config"
)

// RequiredTables are the tables whose presence in a dump's listing proves it
// is a backup of this system rather than an empty or unrelated database. The
// same set scripts/backup.sh has always checked.
var RequiredTables = []string{"financial_account", "payment", "installment", "audit_log", "student"}

// Runner drives pg_dump and pg_restore, and the database-administration
// statements a restore's live swap needs.
type Runner struct {
	db      config.Database
	maint   string // maintenance database name, e.g. "postgres"
	timeout time.Duration
}

// NewRunner builds a Runner over the same credentials the application pool
// uses. Backup and restore need CREATE DATABASE, DROP DATABASE and
// pg_terminate_backend on top of the ordinary table privileges — the same
// requirement scripts/backup.sh and scripts/restore-drill.sh already carry
// for the operator's own role, extended to the process that now performs it.
func NewRunner(db config.Database, maintenanceDatabase string, timeout time.Duration) *Runner {
	if maintenanceDatabase == "" {
		maintenanceDatabase = "postgres"
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Runner{db: db, maint: maintenanceDatabase, timeout: timeout}
}

func (r *Runner) dsn(database string) string {
	cfg := r.db
	cfg.Name = database
	return cfg.DSN()
}

// pgEnv builds the environment for a pg_dump/pg_restore child process.
//
// The password travels as PGPASSWORD, never as part of a --dbname argument:
// command-line arguments are visible to every local user via `ps`, and a
// backup tool leaking the database password to `ps aux` on a shared machine
// would be a strange thing for a financial system to ship.
func (r *Runner) pgEnv(database string) []string {
	env := append(os.Environ(),
		"PGHOST="+r.db.Host,
		"PGPORT="+fmt.Sprintf("%d", r.db.Port),
		"PGUSER="+r.db.User,
		"PGDATABASE="+database,
	)
	if r.db.Password != "" {
		env = append(env, "PGPASSWORD="+r.db.Password)
	}
	if r.db.SSLMode != "" {
		env = append(env, "PGSSLMODE="+r.db.SSLMode)
	}
	return env
}

// ---------------------------------------------------------------------------
// Dump and verify
// ---------------------------------------------------------------------------

// DumpResult is what one pg_dump produced.
type DumpResult struct {
	Bytes         int64
	SHA256        string
	SchemaVersion int64
	ServerVersion string
	RowCounts     map[string]int64
}

// Dump takes a custom-format pg_dump of the named database to destPath, then
// gathers the facts a manifest needs — checksum, byte count, schema version,
// row counts — from the live database at the moment of the dump.
//
// --no-owner --no-privileges: a restore that fails on a missing role is a
// restore that fails for a reason unrelated to the data, and this system's
// only restore target is a scratch database owned by whoever is running it.
func (r *Runner) Dump(ctx context.Context, database, destPath string) (DumpResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return DumpResult{}, fmt.Errorf("creating backup directory: %w", err)
	}

	cmd := exec.CommandContext(ctx, "pg_dump",
		"--dbname="+database,
		"--format=custom",
		"--compress=6",
		"--no-owner",
		"--no-privileges",
		"--file="+destPath,
	)
	cmd.Env = r.pgEnv(database)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(destPath)
		return DumpResult{}, fmt.Errorf("pg_dump: %w: %s", err, firstLine(stderr.String()))
	}

	info, err := os.Stat(destPath)
	if err != nil {
		return DumpResult{}, fmt.Errorf("reading dump file: %w", err)
	}
	if info.Size() == 0 {
		os.Remove(destPath)
		return DumpResult{}, fmt.Errorf("pg_dump produced an empty file")
	}

	sum, err := Checksum(destPath)
	if err != nil {
		return DumpResult{}, err
	}

	facts, err := r.gatherFacts(ctx, database)
	if err != nil {
		// The dump itself succeeded; a failure gathering descriptive facts
		// must not be reported as a failed backup, or a working file gets
		// discarded over a query that timed out.
		return DumpResult{Bytes: info.Size(), SHA256: sum}, nil
	}
	facts.Bytes = info.Size()
	facts.SHA256 = sum
	return facts, nil
}

// VerifyDump proves an archive is structurally readable and carries every
// table this system's financial state lives in. pg_restore --list is a real
// structural check on the archive's table of contents; it does not touch any
// database.
func (r *Runner) VerifyDump(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pg_restore", "--list", path)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("this file is not a readable backup archive: %w", err)
	}

	listing := string(out)
	var missing []string
	for _, table := range RequiredTables {
		if !bytes.Contains(out, []byte("TABLE DATA public "+table+" ")) {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("archive is missing data for: %v (found %d entries in its listing)",
			missing, len(splitLines(listing)))
	}
	return nil
}

// Checksum is the sha256 of a file, hex-encoded.
func Checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening file to checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading file to checksum: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// gatherFacts records what a manifest needs about the database at the moment
// of the dump: the row counts a restore can be compared against, and the
// schema/server versions that decide whether an imported backup is
// compatible with this build.
func (r *Runner) gatherFacts(ctx context.Context, database string) (DumpResult, error) {
	conn, err := pgx.Connect(ctx, r.dsn(database))
	if err != nil {
		return DumpResult{}, err
	}
	defer conn.Close(ctx)

	var facts DumpResult
	if err := conn.QueryRow(ctx, `SHOW server_version`).Scan(&facts.ServerVersion); err != nil {
		return DumpResult{}, err
	}
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) FROM schema_migrations`,
	).Scan(&facts.SchemaVersion); err != nil {
		return DumpResult{}, err
	}

	facts.RowCounts = map[string]int64{}
	for _, table := range RequiredTables {
		var n int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+quoteIdent(table)).Scan(&n); err != nil {
			return DumpResult{}, err
		}
		facts.RowCounts[table] = n
	}
	return facts, nil
}

// CurrentSchemaVersion reads the migration head of the named database. Used
// before a restore begins, to refuse up front a backup made by a newer build
// than the one running — its schema may name views and functions this binary
// has never created, and the failure that produces is easier to explain
// before a scratch database exists than after.
func (r *Runner) CurrentSchemaVersion(ctx context.Context, database string) (int64, error) {
	conn, err := pgx.Connect(ctx, r.dsn(database))
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)

	var version int64
	err = conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version)
	return version, err
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

// RestoreInto runs pg_restore against an existing (normally scratch) database.
//
// --jobs=4: a restore that takes hours single-threaded is an outage of that
// length. --exit-on-error: a restore that half-works and reports success is
// worse than one that stops and says so.
func (r *Runner) RestoreInto(ctx context.Context, database, archivePath string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pg_restore",
		"--dbname="+database,
		"--no-owner",
		"--no-privileges",
		"--jobs=4",
		"--exit-on-error",
		archivePath,
	)
	cmd.Env = r.pgEnv(database)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore: %w: %s", err, firstLine(stderr.String()))
	}
	return nil
}

// RunChecks runs the same reconciliation queries restore-drill.sh has always
// run, against the named database, and reports each as one named pass/fail —
// the UI shows exactly which one refused a restore rather than a bare error.
func (r *Runner) RunChecks(ctx context.Context, database string) ([]Check, error) {
	conn, err := pgx.Connect(ctx, r.dsn(database))
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)

	checks := []struct {
		name  string
		query string
	}{
		{"account caches match their transactions", `SELECT count(*) FROM v_account_reconciliation`},
		{"installment caches match their allocations", `SELECT count(*) FROM v_installment_reconciliation`},
		{"no payment is refunded beyond what it took", `SELECT count(*) FROM v_over_refunded_payments`},
		{"the audit chain verifies end to end", `SELECT count(*) FROM verify_audit_chain(0)`},
	}

	results := make([]Check, 0, len(checks))
	for _, c := range checks {
		var count int64
		if err := conn.QueryRow(ctx, c.query).Scan(&count); err != nil {
			results = append(results, Check{Name: c.name, Passed: false,
				Detail: fmt.Sprintf("could not run: %v", err)})
			continue
		}
		results = append(results, Check{
			Name:   c.name,
			Passed: count == 0,
			Detail: detailFor(count),
		})
	}
	return results, nil
}

// Check is one named pass/fail, kept dependency-free of the port package so
// this adapter has nothing importing app-layer types.
type Check struct {
	Name   string
	Passed bool
	Detail string
}

func detailFor(count int64) string {
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("%d row(s) disagree", count)
}

// ---------------------------------------------------------------------------
// Database administration — CREATE, DROP, RENAME, terminate
// ---------------------------------------------------------------------------

func (r *Runner) admin(ctx context.Context) (*pgx.Conn, error) {
	return pgx.Connect(ctx, r.dsn(r.maint))
}

// CreateScratchDatabase creates an empty database with the given name, ready
// for RestoreInto.
func (r *Runner) CreateScratchDatabase(ctx context.Context, name string) error {
	conn, err := r.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `CREATE DATABASE `+quoteIdent(name))
	return err
}

// DropDatabase terminates any connections to the named database and drops it.
// Safe to call on a database that does not exist.
func (r *Runner) DropDatabase(ctx context.Context, name string) error {
	conn, err := r.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	if err := terminateConnections(ctx, conn, name); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name))
	return err
}

// SwapLive replaces the live database with the scratch database: the live
// database is renamed to previousName (not dropped — it stays available as an
// extra recovery point) and the scratch database is renamed into the live
// name. The caller must have released every connection to both databases from
// its own pool before calling this, and must reconnect afterwards: a database
// cannot be renamed while anything holds it open.
func (r *Runner) SwapLive(ctx context.Context, liveName, scratchName, previousName string) error {
	conn, err := r.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	if err := terminateConnections(ctx, conn, liveName); err != nil {
		return fmt.Errorf("disconnecting the live database: %w", err)
	}
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+quoteIdent(liveName)+` RENAME TO `+quoteIdent(previousName)); err != nil {
		return fmt.Errorf("renaming the live database aside: %w", err)
	}
	if err := terminateConnections(ctx, conn, scratchName); err != nil {
		return fmt.Errorf("disconnecting the scratch database: %w", err)
	}
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+quoteIdent(scratchName)+` RENAME TO `+quoteIdent(liveName)); err != nil {
		// The live database's old name is now free, and the swap has failed
		// with nothing holding that name. Put it back rather than leave the
		// database nameless.
		_, _ = conn.Exec(ctx, `ALTER DATABASE `+quoteIdent(previousName)+` RENAME TO `+quoteIdent(liveName))
		return fmt.Errorf("renaming the restored database into place: %w", err)
	}
	return nil
}

func terminateConnections(ctx context.Context, conn *pgx.Conn, database string) error {
	_, err := conn.Exec(ctx, `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid()`, database)
	return err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func quoteIdent(s string) string {
	return `"` + s + `"`
}

func firstLine(s string) string {
	lines := splitLines(s)
	if len(lines) == 0 {
		return s
	}
	return lines[0]
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
