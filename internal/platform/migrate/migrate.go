// Package migrate is the schema migration engine.
//
// The design targets one specific failure mode: a financial database whose
// schema drifts silently between environments. Four properties defend against
// it, and each one exists because its absence has burned real systems.
//
//   - Every applied migration's checksum is stored and re-verified on each
//     run. Editing a migration file that production already applied is caught
//     at start-up, not discovered months later when two databases disagree.
//   - A PostgreSQL advisory lock serialises runners, so rolling deploys where
//     three API replicas boot at once cannot race each other through the same
//     migration.
//   - Each migration runs inside its own transaction together with the
//     bookkeeping row that records it, which makes a partial apply
//     impossible. Statements that PostgreSQL forbids in a transaction, such as
//     CREATE INDEX CONCURRENTLY, opt out explicitly and are protected instead
//     by a dirty-state marker that blocks further migration until resolved.
//   - Down migrations are mandatory. A migration with no reverse is a
//     one-way door, and a system that manages money needs the door to swing
//     both ways during an incident.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is an arbitrary but stable 64-bit constant identifying this
// application's migration lock. Any process holding it is migrating.
const advisoryLockKey int64 = 8_531_224_907_143_662

// noTransactionDirective opts a migration out of the enclosing transaction.
const noTransactionDirective = "migrate:no-transaction"

var filenamePattern = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)

// Migration is one versioned schema change with its forward and reverse SQL.
type Migration struct {
	Version int64
	Name    string
	UpSQL   string
	DownSQL string
	// Checksum covers the up SQL only. Down SQL is deliberately excluded:
	// fixing a broken rollback script for a migration already applied in
	// production is legitimate, while editing the forward script is not.
	Checksum string
	// UpNoTransaction is set by the `-- migrate:no-transaction` directive.
	UpNoTransaction bool
	// DownNoTransaction is the same directive on the reverse script.
	DownNoTransaction bool
}

// AppliedMigration is the record of a migration already run against a database.
type AppliedMigration struct {
	Version     int64
	Name        string
	Checksum    string
	AppliedAt   time.Time
	ExecutionMS int64
	AppliedBy   string
}

// Status pairs a migration with its application state, for the status command.
type Status struct {
	Version         int64
	Name            string
	Applied         bool
	AppliedAt       time.Time
	ExecutionMS     int64
	ChecksumMatches bool
	// MissingFromDisk marks a migration recorded in the database whose file is
	// gone — usually a branch switch, occasionally a deleted migration that
	// production still has.
	MissingFromDisk bool
}

// Runner applies migrations to a database.
type Runner struct {
	pool       *pgxpool.Pool
	migrations []Migration
	log        *slog.Logger
	actor      string
}

// New builds a runner from a filesystem of migration files. The directory is
// typically embedded into the binary so the deployed artifact carries exactly
// the migrations it was built with.
func New(pool *pgxpool.Pool, fsys fs.FS, dir string, log *slog.Logger) (*Runner, error) {
	migrations, err := Load(fsys, dir)
	if err != nil {
		return nil, err
	}
	actor := "unknown"
	if host, err := os.Hostname(); err == nil {
		actor = host
	}
	return &Runner{pool: pool, migrations: migrations, log: log, actor: actor}, nil
}

// Load reads and pairs migration files from a filesystem.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("reading migration directory %q: %w", dir, err)
	}

	type pair struct {
		name             string
		up, down         string
		upNoTx, downNoTx bool
		hasUp, hasDown   bool
	}
	byVersion := make(map[int64]*pair)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := filenamePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			if strings.HasSuffix(entry.Name(), ".sql") {
				return nil, fmt.Errorf(
					"migration file %q does not match the required pattern NNNNNN_lower_snake_name.(up|down).sql",
					entry.Name())
			}
			continue
		}

		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing version from %q: %w", entry.Name(), err)
		}
		name, direction := matches[2], matches[3]

		content, err := fs.ReadFile(fsys, path.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", entry.Name(), err)
		}
		sqlText := string(content)

		p, ok := byVersion[version]
		if !ok {
			p = &pair{name: name}
			byVersion[version] = p
		}
		if p.name != name {
			return nil, fmt.Errorf(
				"version %06d has conflicting names %q and %q; each version must use one name",
				version, p.name, name)
		}

		if direction == "up" {
			p.up, p.hasUp, p.upNoTx = sqlText, true, hasDirective(sqlText, noTransactionDirective)
		} else {
			p.down, p.hasDown, p.downNoTx = sqlText, true, hasDirective(sqlText, noTransactionDirective)
		}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for version, p := range byVersion {
		if !p.hasUp {
			return nil, fmt.Errorf("version %06d (%s) has a down migration but no up migration", version, p.name)
		}
		if !p.hasDown {
			return nil, fmt.Errorf(
				"version %06d (%s) has no down migration; every migration must be reversible, "+
					"and a genuinely irreversible one should say so in a down script that raises an exception",
				version, p.name)
		}
		if strings.TrimSpace(p.up) == "" {
			return nil, fmt.Errorf("version %06d (%s) has an empty up migration", version, p.name)
		}
		migrations = append(migrations, Migration{
			Version:           version,
			Name:              p.name,
			UpSQL:             p.up,
			DownSQL:           p.down,
			Checksum:          checksum(p.up),
			UpNoTransaction:   p.upNoTx,
			DownNoTransaction: p.downNoTx,
		})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// Up applies every pending migration in version order.
func (r *Runner) Up(ctx context.Context) error {
	return r.withLock(ctx, func(ctx context.Context) error {
		if err := r.ensureBookkeeping(ctx); err != nil {
			return err
		}
		if err := r.checkDirty(ctx); err != nil {
			return err
		}

		applied, err := r.appliedByVersion(ctx)
		if err != nil {
			return err
		}
		if err := r.verifyChecksums(applied); err != nil {
			return err
		}

		var highestApplied int64
		for version := range applied {
			if version > highestApplied {
				highestApplied = version
			}
		}

		pending := make([]Migration, 0, len(r.migrations))
		for _, m := range r.migrations {
			if _, done := applied[m.Version]; done {
				continue
			}
			// An unapplied migration numbered below one already applied means
			// two branches merged out of order. Applying it now would produce a
			// schema that no other environment can reproduce by replaying in
			// version order, so stop and make a human renumber it.
			if m.Version < highestApplied {
				return fmt.Errorf(
					"migration %06d_%s is pending but version %06d has already been applied; "+
						"out-of-order migrations make schema history unreproducible — renumber it above %06d",
					m.Version, m.Name, highestApplied, highestApplied)
			}
			pending = append(pending, m)
		}

		if len(pending) == 0 {
			r.log.Info("schema is up to date", slog.Int64("version", highestApplied))
			return nil
		}

		r.log.Info("applying migrations",
			slog.Int("pending", len(pending)),
			slog.Int64("current_version", highestApplied))

		for _, m := range pending {
			if err := r.applyUp(ctx, m); err != nil {
				return err
			}
		}

		r.log.Info("migrations applied",
			slog.Int("count", len(pending)),
			slog.Int64("version", pending[len(pending)-1].Version))
		return nil
	})
}

// Down rolls back the most recent steps migrations, newest first.
func (r *Runner) Down(ctx context.Context, steps int) error {
	if steps < 1 {
		return fmt.Errorf("steps must be at least 1, got %d", steps)
	}
	return r.withLock(ctx, func(ctx context.Context) error {
		if err := r.ensureBookkeeping(ctx); err != nil {
			return err
		}
		if err := r.checkDirty(ctx); err != nil {
			return err
		}

		applied, err := r.appliedByVersion(ctx)
		if err != nil {
			return err
		}

		versions := make([]int64, 0, len(applied))
		for version := range applied {
			versions = append(versions, version)
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })

		if len(versions) == 0 {
			r.log.Info("nothing to roll back")
			return nil
		}
		if steps > len(versions) {
			steps = len(versions)
		}

		byVersion := make(map[int64]Migration, len(r.migrations))
		for _, m := range r.migrations {
			byVersion[m.Version] = m
		}

		for _, version := range versions[:steps] {
			m, ok := byVersion[version]
			if !ok {
				return fmt.Errorf(
					"cannot roll back version %06d: its migration file is not present in this build",
					version)
			}
			if err := r.applyDown(ctx, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// To migrates forward or backward until the schema sits at targetVersion.
// Version 0 means "roll everything back".
func (r *Runner) To(ctx context.Context, targetVersion int64) error {
	current, err := r.Version(ctx)
	if err != nil {
		return err
	}
	switch {
	case targetVersion == current:
		r.log.Info("already at target version", slog.Int64("version", current))
		return nil
	case targetVersion > current:
		return r.upTo(ctx, targetVersion)
	default:
		applied, err := r.Applied(ctx)
		if err != nil {
			return err
		}
		steps := 0
		for _, a := range applied {
			if a.Version > targetVersion {
				steps++
			}
		}
		return r.Down(ctx, steps)
	}
}

func (r *Runner) upTo(ctx context.Context, targetVersion int64) error {
	return r.withLock(ctx, func(ctx context.Context) error {
		if err := r.ensureBookkeeping(ctx); err != nil {
			return err
		}
		if err := r.checkDirty(ctx); err != nil {
			return err
		}
		applied, err := r.appliedByVersion(ctx)
		if err != nil {
			return err
		}
		if err := r.verifyChecksums(applied); err != nil {
			return err
		}
		for _, m := range r.migrations {
			if m.Version > targetVersion {
				break
			}
			if _, done := applied[m.Version]; done {
				continue
			}
			if err := r.applyUp(ctx, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// Version returns the highest applied migration version, or 0 on a fresh
// database.
func (r *Runner) Version(ctx context.Context) (int64, error) {
	if err := r.ensureBookkeeping(ctx); err != nil {
		return 0, err
	}
	var version *int64
	err := r.pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("reading current schema version: %w", err)
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

// Applied lists applied migrations, oldest first.
func (r *Runner) Applied(ctx context.Context) ([]AppliedMigration, error) {
	if err := r.ensureBookkeeping(ctx); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT version, name, checksum, applied_at, execution_ms, applied_by
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("listing applied migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt, &a.ExecutionMS, &a.AppliedBy); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Status reports every known migration, from disk and from the database.
func (r *Runner) Status(ctx context.Context) ([]Status, error) {
	applied, err := r.appliedByVersion(ctx)
	if err != nil {
		return nil, err
	}

	onDisk := make(map[int64]bool, len(r.migrations))
	out := make([]Status, 0, len(r.migrations)+len(applied))

	for _, m := range r.migrations {
		onDisk[m.Version] = true
		s := Status{Version: m.Version, Name: m.Name, ChecksumMatches: true}
		if a, ok := applied[m.Version]; ok {
			s.Applied = true
			s.AppliedAt = a.AppliedAt
			s.ExecutionMS = a.ExecutionMS
			s.ChecksumMatches = a.Checksum == m.Checksum
		}
		out = append(out, s)
	}

	for version, a := range applied {
		if !onDisk[version] {
			out = append(out, Status{
				Version:         version,
				Name:            a.Name,
				Applied:         true,
				AppliedAt:       a.AppliedAt,
				ExecutionMS:     a.ExecutionMS,
				ChecksumMatches: true,
				MissingFromDisk: true,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Validate checks the database against the migration files without changing
// anything. The API server calls this at boot: a replica whose binary carries
// different migrations than the database has must refuse to serve rather than
// run queries against a schema it does not understand.
func (r *Runner) Validate(ctx context.Context) error {
	if err := r.ensureBookkeeping(ctx); err != nil {
		return err
	}
	if err := r.checkDirty(ctx); err != nil {
		return err
	}

	applied, err := r.appliedByVersion(ctx)
	if err != nil {
		return err
	}
	if err := r.verifyChecksums(applied); err != nil {
		return err
	}

	var pending []string
	for _, m := range r.migrations {
		if _, done := applied[m.Version]; !done {
			pending = append(pending, fmt.Sprintf("%06d_%s", m.Version, m.Name))
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("database is missing %d migration(s): %s", len(pending), strings.Join(pending, ", "))
	}

	onDisk := make(map[int64]bool, len(r.migrations))
	for _, m := range r.migrations {
		onDisk[m.Version] = true
	}
	var unknown []string
	for version, a := range applied {
		if !onDisk[version] {
			unknown = append(unknown, fmt.Sprintf("%06d_%s", version, a.Name))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf(
			"database has %d migration(s) this build does not know about: %s; "+
				"this binary is older than the database",
			len(unknown), strings.Join(unknown, ", "))
	}

	return nil
}

func (r *Runner) applyUp(ctx context.Context, m Migration) error {
	start := time.Now()
	r.log.Info("applying migration",
		slog.Int64("version", m.Version),
		slog.String("name", m.Name),
		slog.Bool("transactional", !m.UpNoTransaction))

	record := `
		INSERT INTO schema_migrations (version, name, checksum, applied_at, execution_ms, applied_by)
		VALUES ($1, $2, $3, now(), $4, $5)`

	if m.UpNoTransaction {
		// Outside a transaction a failure can leave the schema half-changed,
		// so flag the version as dirty first and clear the flag only after the
		// bookkeeping row lands. Any later run refuses to proceed until an
		// operator resolves it.
		if err := r.markDirty(ctx, m.Version); err != nil {
			return err
		}
		if _, err := r.pool.Exec(ctx, m.UpSQL); err != nil {
			return fmt.Errorf("applying migration %06d_%s (non-transactional, schema is now DIRTY): %w",
				m.Version, m.Name, err)
		}
		elapsed := time.Since(start).Milliseconds()
		if _, err := r.pool.Exec(ctx, record, m.Version, m.Name, m.Checksum, elapsed, r.actor); err != nil {
			return fmt.Errorf("recording migration %06d_%s (schema is now DIRTY): %w", m.Version, m.Name, err)
		}
		if err := r.clearDirty(ctx); err != nil {
			return err
		}
		r.logApplied(m, start)
		return nil
	}

	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, m.UpSQL); err != nil {
			return fmt.Errorf("executing migration SQL: %w", err)
		}
		elapsed := time.Since(start).Milliseconds()
		if _, err := tx.Exec(ctx, record, m.Version, m.Name, m.Checksum, elapsed, r.actor); err != nil {
			return fmt.Errorf("recording migration: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("applying migration %06d_%s (rolled back cleanly): %w", m.Version, m.Name, err)
	}

	r.logApplied(m, start)
	return nil
}

func (r *Runner) applyDown(ctx context.Context, m Migration) error {
	start := time.Now()
	r.log.Warn("rolling back migration",
		slog.Int64("version", m.Version),
		slog.String("name", m.Name))

	const remove = `DELETE FROM schema_migrations WHERE version = $1`

	if m.DownNoTransaction {
		if err := r.markDirty(ctx, m.Version); err != nil {
			return err
		}
		if _, err := r.pool.Exec(ctx, m.DownSQL); err != nil {
			return fmt.Errorf("rolling back %06d_%s (non-transactional, schema is now DIRTY): %w",
				m.Version, m.Name, err)
		}
		if _, err := r.pool.Exec(ctx, remove, m.Version); err != nil {
			return fmt.Errorf("removing migration record %06d (schema is now DIRTY): %w", m.Version, err)
		}
		if err := r.clearDirty(ctx); err != nil {
			return err
		}
	} else {
		err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.DownSQL); err != nil {
				return fmt.Errorf("executing rollback SQL: %w", err)
			}
			if _, err := tx.Exec(ctx, remove, m.Version); err != nil {
				return fmt.Errorf("removing migration record: %w", err)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("rolling back %06d_%s (rolled back cleanly): %w", m.Version, m.Name, err)
		}
	}

	r.log.Warn("migration rolled back",
		slog.Int64("version", m.Version),
		slog.String("name", m.Name),
		slog.Duration("took", time.Since(start)))
	return nil
}

func (r *Runner) logApplied(m Migration, start time.Time) {
	r.log.Info("migration applied",
		slog.Int64("version", m.Version),
		slog.String("name", m.Name),
		slog.Duration("took", time.Since(start)))
}

func (r *Runner) withLock(ctx context.Context, fn func(context.Context) error) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection for migration lock: %w", err)
	}
	defer conn.Release()

	r.log.Debug("waiting for migration advisory lock", slog.Int64("key", advisoryLockKey))
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		return fmt.Errorf("taking migration advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
			r.log.Error("releasing migration advisory lock", slog.String("error", err.Error()))
		}
	}()

	return fn(ctx)
}

func (r *Runner) ensureBookkeeping(ctx context.Context) error {
	const ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version      BIGINT      PRIMARY KEY,
			name         TEXT        NOT NULL,
			checksum     TEXT        NOT NULL,
			applied_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			execution_ms BIGINT      NOT NULL DEFAULT 0,
			applied_by   TEXT        NOT NULL DEFAULT 'unknown'
		);

		CREATE TABLE IF NOT EXISTS schema_migration_state (
			singleton     BOOLEAN     PRIMARY KEY DEFAULT true CHECK (singleton),
			dirty_version BIGINT,
			dirty_since   TIMESTAMPTZ
		);

		INSERT INTO schema_migration_state (singleton) VALUES (true) ON CONFLICT DO NOTHING;`
	if _, err := r.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("creating migration bookkeeping tables: %w", err)
	}
	return nil
}

func (r *Runner) appliedByVersion(ctx context.Context) (map[int64]AppliedMigration, error) {
	applied, err := r.Applied(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]AppliedMigration, len(applied))
	for _, a := range applied {
		out[a.Version] = a
	}
	return out, nil
}

func (r *Runner) verifyChecksums(applied map[int64]AppliedMigration) error {
	var drift []string
	for _, m := range r.migrations {
		a, ok := applied[m.Version]
		if !ok {
			continue
		}
		if a.Checksum != m.Checksum {
			drift = append(drift, fmt.Sprintf(
				"%06d_%s (database has %s, file has %s)",
				m.Version, m.Name, shortSum(a.Checksum), shortSum(m.Checksum)))
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		return fmt.Errorf(
			"migration files changed after they were applied: %s; "+
				"an applied migration must never be edited — the database cannot be rebuilt from these "+
				"files, so restore the original content and write a new migration for the change",
			strings.Join(drift, "; "))
	}
	return nil
}

func (r *Runner) checkDirty(ctx context.Context) error {
	var version *int64
	var since *time.Time
	err := r.pool.QueryRow(ctx,
		`SELECT dirty_version, dirty_since FROM schema_migration_state WHERE singleton`).Scan(&version, &since)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading migration state: %w", err)
	}
	if version == nil {
		return nil
	}
	return fmt.Errorf(
		"database is marked DIRTY at version %06d since %s: a non-transactional migration failed part-way. "+
			"Inspect the schema, finish or reverse that migration by hand, then clear the flag with "+
			"`UPDATE schema_migration_state SET dirty_version = NULL, dirty_since = NULL WHERE singleton`",
		*version, formatTime(since))
}

func (r *Runner) markDirty(ctx context.Context, version int64) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE schema_migration_state SET dirty_version = $1, dirty_since = now() WHERE singleton`, version)
	if err != nil {
		return fmt.Errorf("marking migration state dirty: %w", err)
	}
	return nil
}

func (r *Runner) clearDirty(ctx context.Context) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE schema_migration_state SET dirty_version = NULL, dirty_since = NULL WHERE singleton`)
	if err != nil {
		return fmt.Errorf("clearing dirty migration state: %w", err)
	}
	return nil
}

// Migrations exposes the loaded set, for the create and status commands.
func (r *Runner) Migrations() []Migration { return r.migrations }

func checksum(s string) string {
	// Normalise line endings so a file checked out on Windows does not read as
	// a modified migration.
	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func shortSum(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func hasDirective(sqlText, directive string) bool {
	for line := range strings.SplitSeq(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(trimmed, directive) {
			return true
		}
	}
	return false
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}
