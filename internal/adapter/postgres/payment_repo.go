package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// PaymentRepository stores payments and their allocations.
type PaymentRepository struct{ db *pg.DB }

// NewPaymentRepository builds the payment store over a connection pool.
func NewPaymentRepository(db *pg.DB) *PaymentRepository { return &PaymentRepository{db: db} }

var _ port.PaymentRepository = (*PaymentRepository)(nil)

const paymentColumns = `
	id, receipt_no, number_series_id,
	account_id, student_id, enrollment_id, posting_year_id,
	amount, payment_method_id, method_reference,
	cashier_user_id, cashier_session_id,
	paid_at, posted_at, status,
	idempotency_key, payload_hash, payer_name, notes,
	voided_at, voided_by, void_reason, void_request_id, created_at`

func scanPayment(row pgx.Row) (*payment.Payment, error) {
	var p payment.Payment
	if err := row.Scan(
		&p.ID, &p.ReceiptNo, &p.NumberSeriesID,
		&p.AccountID, &p.StudentID, &p.EnrollmentID, &p.PostingYearID,
		&p.Amount, &p.PaymentMethodID, &p.MethodReference,
		&p.CashierUserID, &p.CashierSessionID,
		&p.PaidAt, &p.PostedAt, &p.Status,
		&p.IdempotencyKey, &p.PayloadHash, &p.PayerName, &p.Notes,
		&p.VoidedAt, &p.VoidedBy, &p.VoidReason, &p.VoidRequestID, &p.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// Create records a collection together with the allocations it funds.
func (r *PaymentRepository) Create(ctx context.Context, p *payment.Payment, allocations []*payment.Allocation) error {
	if err := r.db.RequireTx(ctx, "payment.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO payment (
			id, receipt_no, number_series_id,
			account_id, student_id, enrollment_id, posting_year_id,
			amount, payment_method_id, method_reference,
			cashier_user_id, cashier_session_id,
			paid_at, posted_at, status,
			idempotency_key, payload_hash, payer_name, notes,
			voided_at, voided_by, void_reason, void_request_id
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7,
			$8, $9, $10,
			$11, $12,
			COALESCE($13, now()), $14, $15,
			$16, $17, $18, $19,
			$20, $21, $22, $23
		)
		RETURNING paid_at, created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		p.ID, p.ReceiptNo, p.NumberSeriesID,
		p.AccountID, p.StudentID, p.EnrollmentID, p.PostingYearID,
		p.Amount, p.PaymentMethodID, p.MethodReference,
		p.CashierUserID, p.CashierSessionID,
		instant(p.PaidAt), p.PostedAt, p.Status,
		p.IdempotencyKey, p.PayloadHash, p.PayerName, p.Notes,
		p.VoidedAt, p.VoidedBy, p.VoidReason, p.VoidRequestID,
	).Scan(&p.PaidAt, &p.CreatedAt)
	if err != nil {
		return pg.WrapQuery("payment.Create", err)
	}
	return r.insertAllocations(ctx, "payment.Create.allocations", allocations)
}

// Update writes back a payment's lifecycle.
//
// Only the columns a trigger permits to move are written. Everything carrying
// money or identity is frozen the moment the row exists, so a posted payment
// can become voided and nothing else.
func (r *PaymentRepository) Update(ctx context.Context, p *payment.Payment) error {
	if err := r.db.RequireTx(ctx, "payment.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE payment SET
			status           = $2,
			receipt_no       = $3,
			number_series_id = $4,
			posted_at        = $5,
			voided_at        = $6,
			voided_by        = $7,
			void_reason      = $8,
			void_request_id  = $9,
			notes            = $10
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		p.ID, p.Status, p.ReceiptNo, p.NumberSeriesID, p.PostedAt,
		p.VoidedAt, p.VoidedBy, p.VoidReason, p.VoidRequestID, p.Notes,
	).Scan(&id)
	return pg.WrapQuery("payment.Update", err)
}

// GetByID returns one payment.
func (r *PaymentRepository) GetByID(ctx context.Context, id shared.ID) (*payment.Payment, error) {
	q := r.db.Conn(ctx)
	p, err := scanPayment(q.QueryRow(ctx, `SELECT`+paymentColumns+` FROM payment WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("payment.GetByID", err)
	}
	return p, nil
}

// GetByIdempotencyKey returns the payment a terminal already recorded under a
// key, so a retry replays the original receipt instead of collecting twice.
func (r *PaymentRepository) GetByIdempotencyKey(ctx context.Context, key string) (*payment.Payment, error) {
	q := r.db.Conn(ctx)
	p, err := scanPayment(q.QueryRow(ctx, `SELECT`+paymentColumns+` FROM payment WHERE idempotency_key = $1`, key))
	if err != nil {
		return nil, pg.WrapQuery("payment.GetByIdempotencyKey", err)
	}
	return p, nil
}

// GetByReceiptNo returns the payment holding a receipt number in a series.
func (r *PaymentRepository) GetByReceiptNo(ctx context.Context, seriesID shared.ID, receiptNo string) (*payment.Payment, error) {
	const query = `
		SELECT` + paymentColumns + `
		FROM payment
		WHERE number_series_id = $1 AND receipt_no = $2`

	q := r.db.Conn(ctx)
	p, err := scanPayment(q.QueryRow(ctx, query, seriesID, receiptNo))
	if err != nil {
		return nil, pg.WrapQuery("payment.GetByReceiptNo", err)
	}
	return p, nil
}

// ListForAccount returns an account's payments, newest first.
func (r *PaymentRepository) ListForAccount(ctx context.Context, accountID shared.ID) ([]*payment.Payment, error) {
	const query = `
		SELECT` + paymentColumns + `
		FROM payment
		WHERE account_id = $1
		ORDER BY paid_at DESC, id DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("payment.ListForAccount", err)
	}
	payments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Payment, error) {
		return scanPayment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("payment.ListForAccount", err)
	}
	return payments, nil
}

// ListForStudent returns every payment a student made, across all years.
func (r *PaymentRepository) ListForStudent(ctx context.Context, studentID shared.ID) ([]*payment.Payment, error) {
	const query = `
		SELECT` + paymentColumns + `
		FROM payment
		WHERE student_id = $1
		ORDER BY paid_at DESC, id DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("payment.ListForStudent", err)
	}
	payments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Payment, error) {
		return scanPayment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("payment.ListForStudent", err)
	}
	return payments, nil
}

// LiveAllocations returns the allocations of one payment that have not been
// unwound.
//
// Two conditions carry the whole safety property. Only forward allocations are
// considered, and only those with no reversal row pointing back at them: a
// reversal is a row rather than a flag, and a unique index allows at most one
// per allocation. Scoping to a single payment is what stops a refund of payment
// A from unwinding the funding payment B provided, which would leave A's
// allocations overstated and hand back cash twice when B was later voided.
func (r *PaymentRepository) LiveAllocations(ctx context.Context, paymentID shared.ID) ([]*billing.ExistingAllocation, error) {
	// An allocation may be unwound in parts, so what matters is how much of it
	// earlier refunds already took back rather than whether any reversal row
	// exists. Rows whose reversals already sum to the full amount drop out.
	const query = `
		SELECT pa.id, pa.installment_id, i.installment_no, i.due_date, pa.amount,
		       coalesce(rev.reversed, 0) AS reversed_amount
		FROM payment_allocation pa
		JOIN installment i ON i.id = pa.installment_id
		LEFT JOIN LATERAL (
		    SELECT sum(r.amount) AS reversed
		    FROM payment_allocation r
		    WHERE r.reverses_allocation_id = pa.id
		) rev ON true
		WHERE pa.payment_id = $1
		  AND pa.entry_type = 'allocation'
		  AND pa.amount > coalesce(rev.reversed, 0)
		ORDER BY i.due_date, i.installment_no`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, paymentID)
	if err != nil {
		return nil, pg.WrapQuery("payment.LiveAllocations", err)
	}
	allocations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.ExistingAllocation, error) {
		var (
			a       billing.ExistingAllocation
			dueDate time.Time
		)
		if err := row.Scan(&a.ID, &a.InstallmentID, &a.Number, &dueDate, &a.Amount, &a.ReversedAmount); err != nil {
			return nil, err
		}
		a.DueDate = shared.DateFromTime(dueDate)
		return &a, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("payment.LiveAllocations", err)
	}
	return allocations, nil
}

// CreateAllocations appends allocation rows, whether forward or reversing.
func (r *PaymentRepository) CreateAllocations(ctx context.Context, allocations []*payment.Allocation) error {
	if err := r.db.RequireTx(ctx, "payment.CreateAllocations"); err != nil {
		return err
	}
	return r.insertAllocations(ctx, "payment.CreateAllocations", allocations)
}

func (r *PaymentRepository) insertAllocations(ctx context.Context, operation string, allocations []*payment.Allocation) error {
	if len(allocations) == 0 {
		return nil
	}
	const query = `
		INSERT INTO payment_allocation (
			id, payment_id, installment_id, amount, entry_type,
			reverses_allocation_id, caused_by_type, caused_by_id, created_at, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()), $10)`

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	for _, a := range allocations {
		if shared.IsNil(a.ID) {
			a.ID = shared.NewID()
		}
		batch.Queue(query,
			a.ID, a.PaymentID, a.InstallmentID, a.Amount, a.EntryType,
			a.ReversesAllocationID, a.CausedByType, a.CausedByID, instant(a.CreatedAt), a.CreatedBy,
		)
	}
	return pg.WrapQuery(operation, execBatch(ctx, q, batch))
}

// FindNearDuplicate looks for a collection that looks like the one about to be
// recorded: same account, same amount, same method, moments ago.
//
// It catches a terminal that lost its idempotency key across a restart and
// resubmitted. Finding nothing is the normal outcome and is reported as a nil
// payment rather than an error.
func (r *PaymentRepository) FindNearDuplicate(
	ctx context.Context, accountID shared.ID, amount money.Amount, methodID shared.ID, within time.Duration,
) (*payment.Payment, error) {
	const query = `
		SELECT` + paymentColumns + `
		FROM payment
		WHERE account_id = $1
		  AND amount = $2
		  AND payment_method_id = $3
		  AND status IN ('draft', 'posted')
		  AND paid_at >= now() - make_interval(secs => $4)
		ORDER BY paid_at DESC
		LIMIT 1`

	q := r.db.Conn(ctx)
	p, err := scanPayment(q.QueryRow(ctx, query, accountID, amount, methodID, within.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, pg.WrapQuery("payment.FindNearDuplicate", err)
	}
	return p, nil
}

// PostedRefundTotal is what has already been returned against a payment.
func (r *PaymentRepository) PostedRefundTotal(ctx context.Context, paymentID shared.ID) (money.Amount, error) {
	const query = `SELECT COALESCE(sum(amount), 0)::bigint FROM refund WHERE payment_id = $1 AND status = 'posted'`

	q := r.db.Conn(ctx)
	var total money.Amount
	if err := q.QueryRow(ctx, query, paymentID).Scan(&total); err != nil {
		return 0, pg.WrapQuery("payment.PostedRefundTotal", err)
	}
	return total, nil
}

// CountPostedRefunds reports how many refunds a payment already carries, which
// is what stops it also being voided: doing both would return more money than
// was collected.
func (r *PaymentRepository) CountPostedRefunds(ctx context.Context, paymentID shared.ID) (int, error) {
	const query = `SELECT count(*) FROM refund WHERE payment_id = $1 AND status = 'posted'`

	q := r.db.Conn(ctx)
	var count int64
	if err := q.QueryRow(ctx, query, paymentID).Scan(&count); err != nil {
		return 0, pg.WrapQuery("payment.CountPostedRefunds", err)
	}
	return int(count), nil
}

// SummariesForAccount lists an account's collections as a statement shows them.
//
// Deliberately narrow: a receipt number, an amount, a method and a date, plus
// what has been refunded against it. Who took the money is an operator's
// business and does not belong on a page a student hands to a third party.
func (r *PaymentRepository) SummariesForAccount(ctx context.Context, accountID shared.ID) ([]port.PaymentSummary, error) {
	const query = `
		SELECT p.id, p.receipt_no, p.amount, pm.code, p.paid_at, p.status,
		       coalesce((SELECT sum(rf.amount) FROM refund rf
		                 WHERE rf.payment_id = p.id AND rf.status = 'posted'), 0)::bigint
		FROM payment p
		JOIN payment_method pm ON pm.id = p.payment_method_id
		WHERE p.account_id = $1 AND p.status IN ('posted', 'voided')
		ORDER BY p.paid_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("payment.SummariesForAccount", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.PaymentSummary, error) {
		var s port.PaymentSummary
		err := row.Scan(&s.PaymentID, &s.ReceiptNo, &s.Amount, &s.MethodCode,
			&s.PaidAt, &s.Status, &s.RefundedTotal)
		return s, err
	})
	if err != nil {
		return nil, pg.WrapQuery("payment.SummariesForAccount", err)
	}
	return out, nil
}
