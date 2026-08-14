package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/settlement"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// SettlementRepository stores imported statements and their match results.
type SettlementRepository struct{ db *pg.DB }

// NewSettlementRepository builds the settlement store over a connection pool.
func NewSettlementRepository(db *pg.DB) *SettlementRepository { return &SettlementRepository{db: db} }

var _ port.SettlementRepository = (*SettlementRepository)(nil)

const batchColumns = `
	id, source_code, source_name, filename, content_sha256,
	statement_from, statement_to, status, line_count, matched_count,
	total_amount, matched_amount, notes, uploaded_by, reconciled_by`

func scanBatch(row pgx.Row) (*settlement.Batch, error) {
	var (
		b    settlement.Batch
		from *time.Time
		to   *time.Time
	)
	if err := row.Scan(
		&b.ID, &b.SourceCode, &b.SourceName, &b.Filename, &b.ContentSHA256,
		&from, &to, &b.Status, &b.LineCount, &b.MatchedCount,
		&b.TotalAmount, &b.MatchedAmount, &b.Notes, &b.UploadedBy, &b.ReconciledBy,
	); err != nil {
		return nil, err
	}
	b.StatementFrom = dateOrNil(from)
	b.StatementTo = dateOrNil(to)
	return &b, nil
}

// CreateBatch records an imported statement.
func (r *SettlementRepository) CreateBatch(ctx context.Context, b *settlement.Batch) error {
	if err := r.db.RequireTx(ctx, "settlement.CreateBatch"); err != nil {
		return err
	}
	const query = `
		INSERT INTO settlement_batch (
			id, source_code, source_name, filename, content_sha256,
			statement_from, statement_to, status, line_count, matched_count,
			total_amount, matched_amount, notes, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query,
		b.ID, b.SourceCode, b.SourceName, b.Filename, b.ContentSHA256,
		timeOrNil(b.StatementFrom), timeOrNil(b.StatementTo), string(b.Status),
		b.LineCount, b.MatchedCount, b.TotalAmount.Int64(), b.MatchedAmount.Int64(),
		b.Notes, b.UploadedBy)
	return pg.WrapQuery("settlement.CreateBatch", err)
}

// UpdateBatch writes back a batch whose matching progressed.
func (r *SettlementRepository) UpdateBatch(ctx context.Context, b *settlement.Batch) error {
	if err := r.db.RequireTx(ctx, "settlement.UpdateBatch"); err != nil {
		return err
	}
	const query = `
		UPDATE settlement_batch SET
			status = $2, line_count = $3, matched_count = $4,
			total_amount = $5, matched_amount = $6, notes = $7,
			reconciled_by = $8,
			reconciled_at = CASE WHEN $2 = 'reconciled' THEN COALESCE(reconciled_at, now()) ELSE NULL END
		WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		b.ID, string(b.Status), b.LineCount, b.MatchedCount,
		b.TotalAmount.Int64(), b.MatchedAmount.Int64(), b.Notes, b.ReconciledBy).Scan(&id)
	return pg.WrapQuery("settlement.UpdateBatch", err)
}

// GetBatch returns one imported statement.
func (r *SettlementRepository) GetBatch(ctx context.Context, id shared.ID) (*settlement.Batch, error) {
	q := r.db.Conn(ctx)
	b, err := scanBatch(q.QueryRow(ctx, `SELECT`+batchColumns+` FROM settlement_batch WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("settlement.GetBatch", err)
	}
	return b, nil
}

// BatchByContent finds an already-imported statement by its digest.
func (r *SettlementRepository) BatchByContent(ctx context.Context, sha256 string) (*settlement.Batch, error) {
	const query = `SELECT` + batchColumns + `
		FROM settlement_batch WHERE content_sha256 = $1 AND status <> 'cancelled'`

	q := r.db.Conn(ctx)
	b, err := scanBatch(q.QueryRow(ctx, query, sha256))
	if err != nil {
		return nil, pg.WrapQuery("settlement.BatchByContent", err)
	}
	return b, nil
}

// ListBatches returns imported statements, newest first.
func (r *SettlementRepository) ListBatches(
	ctx context.Context, status *settlement.BatchStatus, limit, offset int,
) ([]*settlement.Batch, int, error) {
	const query = `
		SELECT` + batchColumns + `, count(*) OVER () AS total
		FROM settlement_batch
		WHERE ($1::text IS NULL OR status = $1::text)
		ORDER BY uploaded_at DESC
		LIMIT $2 OFFSET $3`

	var statusText *string
	if status != nil {
		value := string(*status)
		statusText = &value
	}

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, statusText, boundedLimit(limit, 50), max(offset, 0))
	if err != nil {
		return nil, 0, pg.WrapQuery("settlement.ListBatches", err)
	}

	var total int
	batches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*settlement.Batch, error) {
		var (
			b    settlement.Batch
			from *time.Time
			to   *time.Time
		)
		err := row.Scan(
			&b.ID, &b.SourceCode, &b.SourceName, &b.Filename, &b.ContentSHA256,
			&from, &to, &b.Status, &b.LineCount, &b.MatchedCount,
			&b.TotalAmount, &b.MatchedAmount, &b.Notes, &b.UploadedBy, &b.ReconciledBy, &total)
		b.StatementFrom = dateOrNil(from)
		b.StatementTo = dateOrNil(to)
		return &b, err
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("settlement.ListBatches", err)
	}
	return batches, total, nil
}

// CreateLines writes a parsed statement's rows.
func (r *SettlementRepository) CreateLines(ctx context.Context, lines []*settlement.Line) error {
	if err := r.db.RequireTx(ctx, "settlement.CreateLines"); err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}
	const query = `
		INSERT INTO settlement_line (
			id, batch_id, line_no, external_ref, amount, value_date, description, raw,
			match_status, matched_payment_id, variance)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	for _, line := range lines {
		raw, err := json.Marshal(line.Raw)
		if err != nil {
			return shared.Internal("settlement.raw_encode", err,
				"the parsed statement row could not be stored")
		}
		batch.Queue(query,
			line.ID, line.BatchID, line.LineNo, textOrNilString(line.ExternalRef),
			line.Amount.Int64(), timeOrNil(line.ValueDate), textOrNilString(line.Description),
			raw, string(line.Status), line.MatchedID, line.Variance.Int64())
	}
	return pg.WrapQuery("settlement.CreateLines", execBatch(ctx, q, batch))
}

const lineColumns = `
	id, batch_id, line_no, external_ref, amount, value_date, description, raw,
	match_status, matched_payment_id, variance, review_note, reviewed_by`

func scanLine(row pgx.Row) (*settlement.Line, error) {
	var (
		line        settlement.Line
		externalRef *string
		description *string
		valueDate   *time.Time
		raw         []byte
	)
	if err := row.Scan(
		&line.ID, &line.BatchID, &line.LineNo, &externalRef, &line.Amount,
		&valueDate, &description, &raw, &line.Status, &line.MatchedID,
		&line.Variance, &line.ReviewNote, &line.ReviewedBy,
	); err != nil {
		return nil, err
	}
	if externalRef != nil {
		line.ExternalRef = *externalRef
	}
	if description != nil {
		line.Description = *description
	}
	line.ValueDate = dateOrNil(valueDate)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &line.Raw)
	}
	return &line, nil
}

// UpdateLine writes back a line whose match or review changed.
func (r *SettlementRepository) UpdateLine(ctx context.Context, line *settlement.Line) error {
	if err := r.db.RequireTx(ctx, "settlement.UpdateLine"); err != nil {
		return err
	}
	const query = `
		UPDATE settlement_line SET
			match_status = $2, matched_payment_id = $3, variance = $4,
			review_note = $5, reviewed_by = $6,
			reviewed_at = CASE WHEN $6::uuid IS NULL THEN reviewed_at ELSE now() END
		WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		line.ID, string(line.Status), line.MatchedID, line.Variance.Int64(),
		line.ReviewNote, line.ReviewedBy).Scan(&id)
	return pg.WrapQuery("settlement.UpdateLine", err)
}

// GetLine returns one statement row.
func (r *SettlementRepository) GetLine(ctx context.Context, id shared.ID) (*settlement.Line, error) {
	q := r.db.Conn(ctx)
	line, err := scanLine(q.QueryRow(ctx, `SELECT`+lineColumns+` FROM settlement_line WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("settlement.GetLine", err)
	}
	return line, nil
}

// ListLines returns a batch's rows, optionally only those still open.
func (r *SettlementRepository) ListLines(ctx context.Context, batchID shared.ID, onlyOpen bool) ([]*settlement.Line, error) {
	const query = `
		SELECT` + lineColumns + `
		FROM settlement_line
		WHERE batch_id = $1
		  AND (NOT $2::boolean OR match_status IN ('unmatched', 'variance', 'duplicate'))
		ORDER BY line_no`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, batchID, onlyOpen)
	if err != nil {
		return nil, pg.WrapQuery("settlement.ListLines", err)
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*settlement.Line, error) {
		return scanLine(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("settlement.ListLines", err)
	}
	return lines, nil
}

// CandidatesForReference returns the payments carrying an external reference.
//
// Voided payments are included rather than filtered here: "the bank received
// this and we voided the receipt" is a different finding from "we never
// recorded it", and only the domain should decide which a line is.
func (r *SettlementRepository) CandidatesForReference(ctx context.Context, reference string) ([]settlement.Candidate, error) {
	const query = `
		SELECT id, amount, status = 'voided'
		FROM payment
		WHERE method_reference = $1 AND status IN ('posted', 'voided')
		ORDER BY posted_at`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, reference)
	if err != nil {
		return nil, pg.WrapQuery("settlement.CandidatesForReference", err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (settlement.Candidate, error) {
		var c settlement.Candidate
		err := row.Scan(&c.PaymentID, &c.Amount, &c.Voided)
		return c, err
	})
	if err != nil {
		return nil, pg.WrapQuery("settlement.CandidatesForReference", err)
	}
	return candidates, nil
}

// Exceptions lists every line still needing a person, across batches.
func (r *SettlementRepository) Exceptions(ctx context.Context, limit int) ([]port.SettlementException, error) {
	const query = `
		SELECT line_id, batch_id, source_code, filename, line_no,
		       external_ref, amount, value_date, match_status, variance, matched_payment_id
		FROM v_settlement_exceptions
		ORDER BY value_date DESC NULLS LAST, amount DESC
		LIMIT $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, boundedLimit(limit, 200))
	if err != nil {
		return nil, pg.WrapQuery("settlement.Exceptions", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.SettlementException, error) {
		var (
			e         port.SettlementException
			valueDate *time.Time
		)
		err := row.Scan(&e.LineID, &e.BatchID, &e.SourceCode, &e.Filename, &e.LineNo,
			&e.ExternalRef, &e.Amount, &valueDate, &e.Status, &e.Variance, &e.PaymentID)
		e.ValueDate = dateOrNil(valueDate)
		return e, err
	})
	if err != nil {
		return nil, pg.WrapQuery("settlement.Exceptions", err)
	}
	return out, nil
}

// UnconfirmedPayments lists posted non-cash collections no statement confirms.
//
// The `before` bound exists because a transfer posted an hour ago has not had
// time to appear on a statement; reporting it would make the register noise.
func (r *SettlementRepository) UnconfirmedPayments(ctx context.Context, before time.Time, limit int) ([]port.UnconfirmedPayment, error) {
	const query = `
		SELECT payment_id, receipt_no, account_id, student_id, amount,
		       method_reference, method_code, paid_at
		FROM v_unconfirmed_electronic_payments
		WHERE paid_at < $1
		ORDER BY paid_at
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, before, boundedLimit(limit, 200))
	if err != nil {
		return nil, pg.WrapQuery("settlement.UnconfirmedPayments", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.UnconfirmedPayment, error) {
		var p port.UnconfirmedPayment
		err := row.Scan(&p.PaymentID, &p.ReceiptNo, &p.AccountID, &p.StudentID,
			&p.Amount, &p.Reference, &p.MethodCode, &p.PaidAt)
		return p, err
	})
	if err != nil {
		return nil, pg.WrapQuery("settlement.UnconfirmedPayments", err)
	}
	return out, nil
}

// textOrNilString maps an empty string onto NULL, so an absent reference is
// absent rather than an empty string that a unique index would treat as a
// value.
func textOrNilString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
