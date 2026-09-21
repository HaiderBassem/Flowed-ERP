package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// BackupRepository stores backup and restore metadata.
//
// Like ReconciliationRepository, this holds operational records rather than
// financial ones: nothing here is money, and rows may be updated in place as
// a run's status moves, which the audit-log discipline elsewhere in this
// package deliberately forbids for anything that is.
type BackupRepository struct{ db *pg.DB }

// NewBackupRepository builds the store over a pool.
func NewBackupRepository(db *pg.DB) *BackupRepository {
	return &BackupRepository{db: db}
}

var _ port.BackupRepository = (*BackupRepository)(nil)

const backupColumns = `
	id, kind, status, started_at, finished_at, file_path, bytes, sha256,
	schema_version, server_version, row_counts, verified, error, created_by`

func (r *BackupRepository) CreateBackup(ctx context.Context, run *port.BackupRun) error {
	const query = `
		INSERT INTO backup_run (id, kind, status, started_at, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING started_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("backup.CreateBackup", q.QueryRow(ctx, query,
		run.ID, string(run.Kind), string(run.Status), instant(run.StartedAt), idOrNilPtr(run.CreatedBy),
	).Scan(&run.StartedAt))
}

// FinishBackup records the outcome of a dump. An upsert rather than a plain
// UPDATE: ordinarily the row from CreateBackup is right there to update, but
// a safety backup and the backup being restored both need to survive a
// restore's live-database swap, which replaces this very table with the
// snapshot's older copy of it — the row this call started with is gone by the
// time BackupService re-registers it afterwards, and an UPDATE finding no
// match would silently do nothing.
func (r *BackupRepository) FinishBackup(ctx context.Context, run *port.BackupRun) error {
	counts, err := json.Marshal(run.RowCounts)
	if err != nil {
		return shared.Internal("backup.row_counts_encode", err, "encoding backup row counts")
	}

	const query = `
		INSERT INTO backup_run (
			id, kind, status, started_at, finished_at, file_path, bytes, sha256,
			schema_version, server_version, row_counts, verified, error, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status, finished_at = EXCLUDED.finished_at,
			file_path = EXCLUDED.file_path, bytes = EXCLUDED.bytes, sha256 = EXCLUDED.sha256,
			schema_version = EXCLUDED.schema_version, server_version = EXCLUDED.server_version,
			row_counts = EXCLUDED.row_counts, verified = EXCLUDED.verified, error = EXCLUDED.error
		RETURNING finished_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("backup.FinishBackup", q.QueryRow(ctx, query,
		run.ID, string(run.Kind), string(run.Status), instant(run.StartedAt), instant(nowOrZero(run.FinishedAt)),
		nullString(run.FilePath), nullInt64(run.Bytes), nullString(run.SHA256),
		nullInt64(run.SchemaVersion), nullString(run.ServerVersion), counts,
		run.Verified, nullString(run.Error), idOrNilPtr(run.CreatedBy),
	).Scan(&run.FinishedAt))
}

func (r *BackupRepository) GetBackup(ctx context.Context, id shared.ID) (*port.BackupRun, error) {
	query := `SELECT` + backupColumns + ` FROM backup_run WHERE id = $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, id)
	if err != nil {
		return nil, pg.WrapQuery("backup.GetBackup", err)
	}
	run, err := pgx.CollectOneRow(rows, scanBackupRun)
	if err != nil {
		return nil, pg.WrapQuery("backup.GetBackup", err)
	}
	return run, nil
}

func (r *BackupRepository) ListBackups(ctx context.Context, limit int) ([]*port.BackupRun, error) {
	const query = `
		SELECT` + backupColumns + `
		FROM backup_run
		ORDER BY started_at DESC
		LIMIT $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, boundedLimit(limit, 200))
	if err != nil {
		return nil, pg.WrapQuery("backup.ListBackups", err)
	}
	runs, err := pgx.CollectRows(rows, scanBackupRun)
	return runs, pg.WrapQuery("backup.ListBackups", err)
}

func (r *BackupRepository) DeleteBackup(ctx context.Context, id shared.ID) error {
	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, `DELETE FROM backup_run WHERE id = $1`, id)
	if err != nil {
		return pg.WrapQuery("backup.DeleteBackup", err)
	}
	if tag.RowsAffected() == 0 {
		return shared.NotFound("backup.not_found", "no such backup")
	}
	return nil
}

func scanBackupRun(row pgx.CollectableRow) (*port.BackupRun, error) {
	var run port.BackupRun
	var kind, status string
	var filePath, sha, serverVersion, errMsg *string
	var bytes, schemaVersion *int64
	var counts []byte
	err := row.Scan(
		&run.ID, &kind, &status, &run.StartedAt, &run.FinishedAt,
		&filePath, &bytes, &sha, &schemaVersion, &serverVersion, &counts,
		&run.Verified, &errMsg, &run.CreatedBy,
	)
	if err != nil {
		return nil, err
	}
	run.Kind = port.BackupKind(kind)
	run.Status = port.BackupStatus(status)
	run.FilePath = strOrEmpty(filePath)
	run.SHA256 = strOrEmpty(sha)
	run.ServerVersion = strOrEmpty(serverVersion)
	run.Error = strOrEmpty(errMsg)
	if bytes != nil {
		run.Bytes = *bytes
	}
	if schemaVersion != nil {
		run.SchemaVersion = *schemaVersion
	}
	if len(counts) > 0 {
		_ = json.Unmarshal(counts, &run.RowCounts)
	}
	return &run, nil
}

// ---------------------------------------------------------------------------
// Restores
// ---------------------------------------------------------------------------

const restoreColumns = `
	id, backup_run_id, safety_backup_id, status, started_at, finished_at,
	checks, previous_database, error, created_by`

func (r *BackupRepository) CreateRestore(ctx context.Context, run *port.RestoreRun) error {
	const query = `
		INSERT INTO restore_run (id, backup_run_id, safety_backup_id, status, started_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING started_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("backup.CreateRestore", q.QueryRow(ctx, query,
		run.ID, run.BackupRunID, idOrNilPtr(run.SafetyBackupID), string(run.Status),
		instant(run.StartedAt), idOrNilPtr(run.CreatedBy),
	).Scan(&run.StartedAt))
}

// UpdateRestore records how far a restore attempt has gotten. An upsert for
// the same reason FinishBackup is one: a restore that reaches its own
// live-database swap replaces this table with the snapshot's older copy of
// it, so the row this attempt started with may no longer exist by the time
// its final outcome is written — CreateRestore's insert and this call's
// insert branch can each be the one that actually creates the row.
func (r *BackupRepository) UpdateRestore(ctx context.Context, run *port.RestoreRun) error {
	checks, err := json.Marshal(run.Checks)
	if err != nil {
		return shared.Internal("backup.checks_encode", err, "encoding restore checks")
	}

	const query = `
		INSERT INTO restore_run (
			id, backup_run_id, safety_backup_id, status, started_at, finished_at,
			checks, previous_database, error, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			safety_backup_id = EXCLUDED.safety_backup_id, status = EXCLUDED.status,
			finished_at = EXCLUDED.finished_at, checks = EXCLUDED.checks,
			previous_database = EXCLUDED.previous_database, error = EXCLUDED.error
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err = q.QueryRow(ctx, query,
		run.ID, run.BackupRunID, idOrNilPtr(run.SafetyBackupID), string(run.Status),
		instant(run.StartedAt), instantOrNil(run.FinishedAt),
		checks, nullString(run.PreviousDatabase), nullString(run.Error), idOrNilPtr(run.CreatedBy),
	).Scan(&id)
	return pg.WrapQuery("backup.UpdateRestore", err)
}

func (r *BackupRepository) GetRestore(ctx context.Context, id shared.ID) (*port.RestoreRun, error) {
	query := `SELECT` + restoreColumns + ` FROM restore_run WHERE id = $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, id)
	if err != nil {
		return nil, pg.WrapQuery("backup.GetRestore", err)
	}
	run, err := pgx.CollectOneRow(rows, scanRestoreRun)
	if err != nil {
		return nil, pg.WrapQuery("backup.GetRestore", err)
	}
	return run, nil
}

func (r *BackupRepository) ListRestores(ctx context.Context, limit int) ([]*port.RestoreRun, error) {
	const query = `
		SELECT` + restoreColumns + `
		FROM restore_run
		ORDER BY started_at DESC
		LIMIT $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, boundedLimit(limit, 200))
	if err != nil {
		return nil, pg.WrapQuery("backup.ListRestores", err)
	}
	runs, err := pgx.CollectRows(rows, scanRestoreRun)
	return runs, pg.WrapQuery("backup.ListRestores", err)
}

func scanRestoreRun(row pgx.CollectableRow) (*port.RestoreRun, error) {
	var run port.RestoreRun
	var status string
	var checks []byte
	var previousDB, errMsg *string
	err := row.Scan(
		&run.ID, &run.BackupRunID, &run.SafetyBackupID, &status, &run.StartedAt, &run.FinishedAt,
		&checks, &previousDB, &errMsg, &run.CreatedBy,
	)
	if err != nil {
		return nil, err
	}
	run.Status = port.RestoreStatus(status)
	run.PreviousDatabase = strOrEmpty(previousDB)
	run.Error = strOrEmpty(errMsg)
	if len(checks) > 0 {
		_ = json.Unmarshal(checks, &run.Checks)
	}
	return &run, nil
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

func (r *BackupRepository) GetSchedule(ctx context.Context) (*port.BackupSchedule, error) {
	const query = `
		SELECT enabled, interval_hours, retention_count, last_run_at, updated_at, updated_by
		FROM backup_schedule WHERE id = true`

	q := r.db.Conn(ctx)
	var s port.BackupSchedule
	err := q.QueryRow(ctx, query).Scan(
		&s.Enabled, &s.IntervalHours, &s.RetentionCount, &s.LastRunAt, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		return nil, pg.WrapQuery("backup.GetSchedule", err)
	}
	return &s, nil
}

func (r *BackupRepository) UpdateSchedule(ctx context.Context, s *port.BackupSchedule) error {
	const query = `
		UPDATE backup_schedule
		SET enabled = $1, interval_hours = $2, retention_count = $3,
		    last_run_at = $4, updated_at = $5, updated_by = $6
		WHERE id = true
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("backup.UpdateSchedule", q.QueryRow(ctx, query,
		s.Enabled, s.IntervalHours, s.RetentionCount,
		instantOrNil(s.LastRunAt), instant(s.UpdatedAt), idOrNilPtr(s.UpdatedBy),
	).Scan(&s.UpdatedAt))
}

// ---------------------------------------------------------------------------
// Small scalar helpers. instant, idOrNilPtr, boundedLimit and instantOrNil are
// shared across the package already; these four are new because nothing
// before this needed a nullable string, byte count or schema version.
// ---------------------------------------------------------------------------

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
