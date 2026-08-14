package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// RefundRepository stores refunds and the allocations they unwind.
type RefundRepository struct{ db *pg.DB }

// NewRefundRepository builds the refund store over a connection pool.
func NewRefundRepository(db *pg.DB) *RefundRepository { return &RefundRepository{db: db} }

var _ port.RefundRepository = (*RefundRepository)(nil)

const refundColumns = `
	id, refund_no, number_series_id,
	payment_id, account_id, student_id, posting_year_id,
	amount, payment_method_id, method_reference, reason,
	status, requested_by, requested_at, approved_by, approved_at,
	rejected_by, rejected_at, rejection_reason,
	posted_at, posted_by, cashier_session_id, idempotency_key, created_at`

func scanRefund(row pgx.Row) (*payment.Refund, error) {
	var f payment.Refund
	if err := row.Scan(
		&f.ID, &f.RefundNo, &f.NumberSeriesID,
		&f.PaymentID, &f.AccountID, &f.StudentID, &f.PostingYearID,
		&f.Amount, &f.PaymentMethodID, &f.MethodReference, &f.Reason,
		&f.Status, &f.RequestedBy, &f.RequestedAt, &f.ApprovedBy, &f.ApprovedAt,
		&f.RejectedBy, &f.RejectedAt, &f.RejectionReason,
		&f.PostedAt, &f.PostedBy, &f.CashierSessionID, &f.IdempotencyKey, &f.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &f, nil
}

// Create records a refund request. The original payment keeps its amount: a
// refund is a second fact, not an edit to the first.
func (r *RefundRepository) Create(ctx context.Context, f *payment.Refund) error {
	if err := r.db.RequireTx(ctx, "refund.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO refund (
			id, refund_no, number_series_id,
			payment_id, account_id, student_id, posting_year_id,
			amount, payment_method_id, method_reference, reason,
			status, requested_by, requested_at, approved_by, approved_at,
			rejected_by, rejected_at, rejection_reason,
			posted_at, posted_by, cashier_session_id, idempotency_key
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7,
			$8, $9, $10, $11,
			$12, $13, COALESCE($14, now()), $15, $16,
			$17, $18, $19,
			$20, $21, $22, $23
		)
		RETURNING requested_at, created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		f.ID, f.RefundNo, f.NumberSeriesID,
		f.PaymentID, f.AccountID, f.StudentID, f.PostingYearID,
		f.Amount, f.PaymentMethodID, f.MethodReference, f.Reason,
		f.Status, f.RequestedBy, instant(f.RequestedAt), f.ApprovedBy, f.ApprovedAt,
		f.RejectedBy, f.RejectedAt, f.RejectionReason,
		f.PostedAt, f.PostedBy, f.CashierSessionID, f.IdempotencyKey,
	).Scan(&f.RequestedAt, &f.CreatedAt)
	return pg.WrapQuery("refund.Create", err)
}

// Update moves a refund through its lifecycle. Only the columns a trigger
// permits to change are written; the amount, the payment and the reason are
// frozen from the moment the request exists.
func (r *RefundRepository) Update(ctx context.Context, f *payment.Refund) error {
	if err := r.db.RequireTx(ctx, "refund.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE refund SET
			status             = $2,
			refund_no          = $3,
			number_series_id   = $4,
			approved_by        = $5,
			approved_at        = $6,
			rejected_by        = $7,
			rejected_at        = $8,
			rejection_reason   = $9,
			posted_at          = $10,
			posted_by          = $11,
			cashier_session_id = $12
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		f.ID, f.Status, f.RefundNo, f.NumberSeriesID,
		f.ApprovedBy, f.ApprovedAt, f.RejectedBy, f.RejectedAt, f.RejectionReason,
		f.PostedAt, f.PostedBy, f.CashierSessionID,
	).Scan(&id)
	return pg.WrapQuery("refund.Update", err)
}

// GetByID returns one refund.
func (r *RefundRepository) GetByID(ctx context.Context, id shared.ID) (*payment.Refund, error) {
	q := r.db.Conn(ctx)
	f, err := scanRefund(q.QueryRow(ctx, `SELECT`+refundColumns+` FROM refund WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("refund.GetByID", err)
	}
	return f, nil
}

// GetForUpdate locks the refund row before it is approved or paid out, so an
// approval and a payout racing on the same request cannot both proceed from the
// same stale status.
func (r *RefundRepository) GetForUpdate(ctx context.Context, id shared.ID) (*payment.Refund, error) {
	if err := r.db.RequireTx(ctx, "refund.GetForUpdate"); err != nil {
		return nil, err
	}
	q := r.db.Conn(ctx)
	f, err := scanRefund(q.QueryRow(ctx, `SELECT`+refundColumns+` FROM refund WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, pg.WrapQuery("refund.GetForUpdate", err)
	}
	return f, nil
}

// ListForPayment returns every refund raised against a payment.
func (r *RefundRepository) ListForPayment(ctx context.Context, paymentID shared.ID) ([]*payment.Refund, error) {
	const query = `
		SELECT` + refundColumns + `
		FROM refund
		WHERE payment_id = $1
		ORDER BY requested_at DESC, id DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, paymentID)
	if err != nil {
		return nil, pg.WrapQuery("refund.ListForPayment", err)
	}
	refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Refund, error) {
		return scanRefund(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("refund.ListForPayment", err)
	}
	return refunds, nil
}

// ListForAccount returns every refund against an account.
func (r *RefundRepository) ListForAccount(ctx context.Context, accountID shared.ID) ([]*payment.Refund, error) {
	const query = `
		SELECT` + refundColumns + `
		FROM refund
		WHERE account_id = $1
		ORDER BY requested_at DESC, id DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("refund.ListForAccount", err)
	}
	refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Refund, error) {
		return scanRefund(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("refund.ListForAccount", err)
	}
	return refunds, nil
}

// ListPending returns the refunds still waiting on someone: requested ones
// awaiting a decision and approved ones awaiting payout. Both sit on a
// finance manager's worklist, which is what this feeds.
func (r *RefundRepository) ListPending(ctx context.Context) ([]*payment.Refund, error) {
	const query = `
		SELECT` + refundColumns + `
		FROM refund
		WHERE status IN ('requested', 'approved')
		ORDER BY requested_at`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("refund.ListPending", err)
	}
	refunds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Refund, error) {
		return scanRefund(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("refund.ListPending", err)
	}
	return refunds, nil
}

// CreateAllocations records which allocations a refund unwound, or which
// credit it drew on. The rows are append-only.
func (r *RefundRepository) CreateAllocations(ctx context.Context, allocations []*payment.RefundAllocation) error {
	if err := r.db.RequireTx(ctx, "refund.CreateAllocations"); err != nil {
		return err
	}
	if len(allocations) == 0 {
		return nil
	}
	const query = `
		INSERT INTO refund_allocation (
			id, refund_id, installment_id, reverses_allocation_id, credit_entry_id, amount, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()))`

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	for _, a := range allocations {
		if shared.IsNil(a.ID) {
			a.ID = shared.NewID()
		}
		batch.Queue(query,
			a.ID, a.RefundID, a.InstallmentID, a.ReversesAllocationID, a.CreditEntryID,
			a.Amount, instant(a.CreatedAt),
		)
	}
	return pg.WrapQuery("refund.CreateAllocations", execBatch(ctx, q, batch))
}
