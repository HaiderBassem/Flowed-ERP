package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// ImportRepository stores staged import batches and their rows.
type ImportRepository struct{ db *pg.DB }

// NewImportRepository builds the staged-import store over a connection pool.
func NewImportRepository(db *pg.DB) *ImportRepository { return &ImportRepository{db: db} }

var _ port.ImportRepository = (*ImportRepository)(nil)

const importBatchColumns = `
	id, batch_type, source_filename, status, academic_year_id,
	total_rows, valid_rows, error_rows, created_rows, updated_rows, skipped_rows, failed_rows,
	heartbeat_at, started_at, completed_at, error_summary,
	created_at, created_by, confirmed_by, confirmed_at`

func scanImportBatch(row pgx.Row) (*port.ImportBatch, error) {
	var b port.ImportBatch
	if err := row.Scan(
		&b.ID, &b.BatchType, &b.SourceFilename, &b.Status, &b.AcademicYearID,
		&b.TotalRows, &b.ValidRows, &b.ErrorRows, &b.CreatedRows, &b.UpdatedRows, &b.SkippedRows, &b.FailedRows,
		&b.HeartbeatAt, &b.StartedAt, &b.CompletedAt, &b.ErrorSummary,
		&b.CreatedAt, &b.CreatedBy, &b.ConfirmedBy, &b.ConfirmedAt,
	); err != nil {
		return nil, err
	}
	return &b, nil
}

// CreateBatch records an uploaded spreadsheet.
func (r *ImportRepository) CreateBatch(ctx context.Context, b *port.ImportBatch) error {
	if err := r.db.RequireTx(ctx, "import_batch.CreateBatch"); err != nil {
		return err
	}
	const query = `
		INSERT INTO import_batch (
			id, batch_type, source_filename, status, academic_year_id,
			total_rows, valid_rows, error_rows, created_rows, updated_rows, skipped_rows, failed_rows,
			heartbeat_at, started_at, completed_at, error_summary,
			created_by, confirmed_by, confirmed_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16,
			$17, $18, $19
		)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		b.ID, b.BatchType, b.SourceFilename, b.Status, b.AcademicYearID,
		b.TotalRows, b.ValidRows, b.ErrorRows, b.CreatedRows, b.UpdatedRows, b.SkippedRows, b.FailedRows,
		b.HeartbeatAt, b.StartedAt, b.CompletedAt, b.ErrorSummary,
		b.CreatedBy, b.ConfirmedBy, b.ConfirmedAt,
	).Scan(&b.CreatedAt)
	return pg.WrapQuery("import_batch.CreateBatch", err)
}

// UpdateBatch writes back a batch whose status or tallies moved.
func (r *ImportRepository) UpdateBatch(ctx context.Context, b *port.ImportBatch) error {
	if err := r.db.RequireTx(ctx, "import_batch.UpdateBatch"); err != nil {
		return err
	}
	const query = `
		UPDATE import_batch SET
			batch_type       = $2,
			source_filename  = $3,
			status           = $4,
			academic_year_id = $5,
			total_rows       = $6,
			valid_rows       = $7,
			error_rows       = $8,
			created_rows     = $9,
			updated_rows     = $10,
			skipped_rows     = $11,
			failed_rows      = $12,
			heartbeat_at     = $13,
			started_at       = $14,
			completed_at     = $15,
			error_summary    = $16,
			confirmed_by     = $17,
			confirmed_at     = $18
		WHERE id = $1`

	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, query,
		b.ID, b.BatchType, b.SourceFilename, b.Status, b.AcademicYearID,
		b.TotalRows, b.ValidRows, b.ErrorRows, b.CreatedRows, b.UpdatedRows, b.SkippedRows, b.FailedRows,
		b.HeartbeatAt, b.StartedAt, b.CompletedAt, b.ErrorSummary,
		b.ConfirmedBy, b.ConfirmedAt,
	)
	if err != nil {
		return pg.WrapQuery("import_batch.UpdateBatch", err)
	}
	if tag.RowsAffected() == 0 {
		return shared.NotFound("import_batch.not_found", "import batch %s does not exist", b.ID)
	}
	return nil
}

// GetBatch returns one batch.
func (r *ImportRepository) GetBatch(ctx context.Context, id shared.ID) (*port.ImportBatch, error) {
	q := r.db.Conn(ctx)
	b, err := scanImportBatch(q.QueryRow(ctx, `SELECT`+importBatchColumns+` FROM import_batch WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("import_batch.GetBatch", err)
	}
	return b, nil
}

// GetBatchForUpdate locks the batch row for the rest of the transaction.
//
// Every command that moves a batch between states takes this first. Without
// it, a reaper resuming a stalled batch and the original worker waking up
// again would both believe they own the run and process the same rows twice.
func (r *ImportRepository) GetBatchForUpdate(ctx context.Context, id shared.ID) (*port.ImportBatch, error) {
	if err := r.db.RequireTx(ctx, "import_batch.GetBatchForUpdate"); err != nil {
		return nil, err
	}
	q := r.db.Conn(ctx)
	b, err := scanImportBatch(q.QueryRow(ctx,
		`SELECT`+importBatchColumns+` FROM import_batch WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, pg.WrapQuery("import_batch.GetBatchForUpdate", err)
	}
	return b, nil
}

// ListBatches returns a page of batches, newest first, and the full count.
func (r *ImportRepository) ListBatches(
	ctx context.Context, status *port.ImportBatchStatus, limit, offset int,
) ([]*port.ImportBatch, int, error) {
	args := &argList{}
	where := "true"
	if status != nil {
		where = "status = " + args.next(string(*status))
	}

	limit = boundedLimit(limit, 50)
	offset = max(offset, 0)

	query := `
		SELECT` + importBatchColumns + `, count(*) OVER () AS total_count
		FROM import_batch
		WHERE ` + where + `
		ORDER BY created_at DESC, id DESC
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("import_batch.ListBatches", err)
	}

	var total int64
	batches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.ImportBatch, error) {
		var b port.ImportBatch
		if err := row.Scan(
			&b.ID, &b.BatchType, &b.SourceFilename, &b.Status, &b.AcademicYearID,
			&b.TotalRows, &b.ValidRows, &b.ErrorRows, &b.CreatedRows, &b.UpdatedRows, &b.SkippedRows, &b.FailedRows,
			&b.HeartbeatAt, &b.StartedAt, &b.CompletedAt, &b.ErrorSummary,
			&b.CreatedAt, &b.CreatedBy, &b.ConfirmedBy, &b.ConfirmedAt,
			&total,
		); err != nil {
			return nil, err
		}
		return &b, nil
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("import_batch.ListBatches", err)
	}
	return batches, int(total), nil
}

// Heartbeat records that the worker is still alive.
//
// It writes one column and nothing else on purpose: the run loop calls it
// while rows are being committed around it, and a full row update would
// clobber tallies written by the row that just finished.
func (r *ImportRepository) Heartbeat(ctx context.Context, batchID shared.ID, at time.Time) error {
	if err := r.db.RequireTx(ctx, "import_batch.Heartbeat"); err != nil {
		return err
	}
	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, `UPDATE import_batch SET heartbeat_at = COALESCE($2, now()) WHERE id = $1`,
		batchID, instant(at))
	return pg.WrapQuery("import_batch.Heartbeat", err)
}

// StalledBatches lists importing batches whose heartbeat stopped.
func (r *ImportRepository) StalledBatches(
	ctx context.Context, silentSince time.Time, limit int,
) ([]*port.ImportBatch, error) {
	const query = `
		SELECT` + importBatchColumns + `
		FROM import_batch
		WHERE status = 'importing'
		  AND (heartbeat_at IS NULL OR heartbeat_at < $1)
		ORDER BY heartbeat_at NULLS FIRST
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, silentSince, boundedLimit(limit, 50))
	if err != nil {
		return nil, pg.WrapQuery("import_batch.StalledBatches", err)
	}
	batches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.ImportBatch, error) {
		return scanImportBatch(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("import_batch.StalledBatches", err)
	}
	return batches, nil
}

const importRowColumns = `
	id, batch_id, row_no, raw_data, dedup_hash,
	validation_status, disposition, errors, warnings,
	preview_hash, matched_entity_id, created_entity_id, processed_at, error_message`

func scanImportRow(row pgx.Row, extra ...any) (*port.ImportRow, error) {
	var (
		r        port.ImportRow
		raw      []byte
		errsJSON []byte
		warnJSON []byte
	)
	dest := []any{
		&r.ID, &r.BatchID, &r.RowNo, &raw, &r.DedupHash,
		&r.ValidationStatus, &r.Disposition, &errsJSON, &warnJSON,
		&r.PreviewHash, &r.MatchedEntityID, &r.CreatedEntityID, &r.ProcessedAt, &r.ErrorMessage,
	}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	if err := jsonInto(raw, &r.RawData); err != nil {
		return nil, err
	}
	if err := jsonInto(errsJSON, &r.Errors); err != nil {
		return nil, err
	}
	if err := jsonInto(warnJSON, &r.Warnings); err != nil {
		return nil, err
	}
	return &r, nil
}

// InsertRows stages parsed rows in one round trip.
//
// A row colliding on (batch_id, dedup_hash) is an exact re-statement of a line
// already present, so it is dropped rather than duplicated. The number
// actually inserted is returned: the caller reports the difference, because a
// load that quietly swallowed thirty lines and said "success" is how a
// spreadsheet ends up half-imported with nobody aware.
func (r *ImportRepository) InsertRows(ctx context.Context, rows []*port.ImportRow) (int, error) {
	if err := r.db.RequireTx(ctx, "import_row.InsertRows"); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	const query = `
		INSERT INTO import_row (
			id, batch_id, row_no, raw_data, dedup_hash,
			validation_status, disposition, errors, warnings,
			preview_hash, matched_entity_id, created_entity_id, processed_at, error_message
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13, $14
		)
		ON CONFLICT (batch_id, dedup_hash) DO NOTHING`

	batch := &pgx.Batch{}
	for _, row := range rows {
		raw, err := jsonOrNil(row.RawData)
		if err != nil {
			return 0, err
		}
		errsJSON, err := jsonOrNil(findingsOrNil(row.Errors))
		if err != nil {
			return 0, err
		}
		warnJSON, err := jsonOrNil(findingsOrNil(row.Warnings))
		if err != nil {
			return 0, err
		}
		batch.Queue(query,
			row.ID, row.BatchID, row.RowNo, raw, row.DedupHash,
			row.ValidationStatus, row.Disposition, errsJSON, warnJSON,
			row.PreviewHash, row.MatchedEntityID, row.CreatedEntityID, row.ProcessedAt, row.ErrorMessage,
		)
	}

	results := r.db.Conn(ctx).SendBatch(ctx, batch)
	inserted := 0
	var firstErr error
	for range batch.Len() {
		tag, err := results.Exec()
		if err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		inserted += int(tag.RowsAffected())
	}
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		return 0, pg.WrapQuery("import_row.InsertRows", firstErr)
	}
	return inserted, nil
}

// UpdateRow writes back a row whose validation, disposition or outcome moved.
func (r *ImportRepository) UpdateRow(ctx context.Context, row *port.ImportRow) error {
	if err := r.db.RequireTx(ctx, "import_row.UpdateRow"); err != nil {
		return err
	}

	errsJSON, err := jsonOrNil(findingsOrNil(row.Errors))
	if err != nil {
		return err
	}
	warnJSON, err := jsonOrNil(findingsOrNil(row.Warnings))
	if err != nil {
		return err
	}

	const query = `
		UPDATE import_row SET
			validation_status = $2,
			disposition       = $3,
			errors            = $4,
			warnings          = $5,
			preview_hash      = $6,
			matched_entity_id = $7,
			created_entity_id = $8,
			processed_at      = $9,
			error_message     = $10
		WHERE id = $1`

	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, query,
		row.ID, row.ValidationStatus, row.Disposition, errsJSON, warnJSON,
		row.PreviewHash, row.MatchedEntityID, row.CreatedEntityID, row.ProcessedAt, row.ErrorMessage,
	)
	if err != nil {
		return pg.WrapQuery("import_row.UpdateRow", err)
	}
	if tag.RowsAffected() == 0 {
		return shared.NotFound("import_row.not_found", "import row %s does not exist", row.ID)
	}
	return nil
}

// GetRow returns one row by its position in the file.
func (r *ImportRepository) GetRow(ctx context.Context, batchID shared.ID, rowNo int) (*port.ImportRow, error) {
	q := r.db.Conn(ctx)
	row, err := scanImportRow(q.QueryRow(ctx,
		`SELECT`+importRowColumns+` FROM import_row WHERE batch_id = $1 AND row_no = $2`, batchID, rowNo))
	if err != nil {
		return nil, pg.WrapQuery("import_row.GetRow", err)
	}
	return row, nil
}

// ListRows returns a page of a batch's rows in file order, and the full count.
func (r *ImportRepository) ListRows(
	ctx context.Context, batchID shared.ID, f port.ImportRowFilter,
) ([]*port.ImportRow, int, error) {
	args := &argList{}
	where := []string{"batch_id = " + args.next(batchID)}
	if len(f.Statuses) > 0 {
		where = append(where, "validation_status = ANY("+args.next(toStrings(f.Statuses))+")")
	}
	if len(f.Dispositions) > 0 {
		where = append(where, "disposition = ANY("+args.next(toStrings(f.Dispositions))+")")
	}
	if f.OnlyProblems {
		where = append(where, "(validation_status IN ('error', 'warning', 'failed') OR disposition = 'error')")
	}

	limit := boundedLimit(f.Limit, 100)
	offset := max(f.Offset, 0)

	query := `
		SELECT` + importRowColumns + `, count(*) OVER () AS total_count
		FROM import_row
		WHERE ` + strings.Join(where, "\n\t\t  AND ") + `
		ORDER BY row_no
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("import_row.ListRows", err)
	}

	var total int64
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.ImportRow, error) {
		return scanImportRow(row, &total)
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("import_row.ListRows", err)
	}
	return found, int(total), nil
}

// UnprocessedRows returns the worker's queue in file order.
//
// The predicate matches the schema's partial index exactly, so a resumed run
// picks up precisely the rows that were never attempted: everything applied,
// skipped or failed has already left these two statuses.
func (r *ImportRepository) UnprocessedRows(
	ctx context.Context, batchID shared.ID, limit int,
) ([]*port.ImportRow, error) {
	const query = `
		SELECT` + importRowColumns + `
		FROM import_row
		WHERE batch_id = $1 AND validation_status IN ('valid', 'warning')
		ORDER BY row_no
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, batchID, boundedLimit(limit, 500))
	if err != nil {
		return nil, pg.WrapQuery("import_row.UnprocessedRows", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.ImportRow, error) {
		return scanImportRow(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("import_row.UnprocessedRows", err)
	}
	return found, nil
}

// CountRows tallies a batch's rows by status.
//
// One query with FILTER clauses rather than a tally kept in the worker's
// memory: a batch resumed after a crash must report what every row did, not
// only what the surviving pass saw.
func (r *ImportRepository) CountRows(ctx context.Context, batchID shared.ID) (port.ImportRowCounts, error) {
	const query = `
		SELECT
			count(*)                                                              AS total,
			count(*) FILTER (WHERE validation_status = 'pending')                 AS pending,
			count(*) FILTER (WHERE validation_status = 'valid')                   AS valid,
			count(*) FILTER (WHERE validation_status = 'warning')                 AS warning,
			count(*) FILTER (WHERE validation_status = 'error')                   AS errored,
			count(*) FILTER (WHERE validation_status = 'processed')               AS processed,
			count(*) FILTER (WHERE validation_status = 'skipped')                 AS skipped,
			count(*) FILTER (WHERE validation_status = 'failed')                  AS failed,
			count(*) FILTER (WHERE validation_status = 'processed'
			                   AND disposition = 'create')                        AS created,
			count(*) FILTER (WHERE validation_status = 'processed'
			                   AND disposition = 'update')                        AS updated,
			count(*) FILTER (WHERE validation_status = 'error'
			                   AND disposition = 'error')                         AS unresolved
		FROM import_row
		WHERE batch_id = $1`

	var c port.ImportRowCounts
	q := r.db.Conn(ctx)
	if err := q.QueryRow(ctx, query, batchID).Scan(
		&c.Total, &c.Pending, &c.Valid, &c.Warning, &c.Error,
		&c.Processed, &c.Skipped, &c.Failed, &c.Created, &c.Updated, &c.UnresolvedErrors,
	); err != nil {
		return port.ImportRowCounts{}, pg.WrapQuery("import_row.CountRows", err)
	}
	return c, nil
}

// StudentNumberOccurrences counts how often each student number appears in a
// batch, which is what catches a number duplicated inside one file before any
// of its rows are applied.
func (r *ImportRepository) StudentNumberOccurrences(
	ctx context.Context, batchID shared.ID,
) (map[string]int, error) {
	const query = `
		SELECT btrim(raw_data ->> 'student_no') AS student_no, count(*)
		FROM import_row
		WHERE batch_id = $1 AND btrim(coalesce(raw_data ->> 'student_no', '')) <> ''
		GROUP BY 1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, batchID)
	if err != nil {
		return nil, pg.WrapQuery("import_row.StudentNumberOccurrences", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var (
			studentNo string
			n         int64
		)
		if err := rows.Scan(&studentNo, &n); err != nil {
			return nil, pg.WrapQuery("import_row.StudentNumberOccurrences", err)
		}
		counts[studentNo] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, pg.WrapQuery("import_row.StudentNumberOccurrences", err)
	}
	return counts, nil
}

// findingsOrNil maps an empty finding list onto SQL NULL, so "validated, found
// nothing" and "not validated yet" are not stored as the same empty array.
func findingsOrNil(findings []port.ImportFinding) any {
	if len(findings) == 0 {
		return nil
	}
	return findings
}
