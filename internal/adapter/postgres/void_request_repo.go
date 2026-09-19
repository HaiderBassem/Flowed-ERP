package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// VoidRequestRepository stores void requests, the document that makes voiding
// a two-person act rather than a permission flag.
type VoidRequestRepository struct{ db *pg.DB }

// NewVoidRequestRepository builds the void request store over a connection pool.
func NewVoidRequestRepository(db *pg.DB) *VoidRequestRepository {
	return &VoidRequestRepository{db: db}
}

var _ port.VoidRequestRepository = (*VoidRequestRepository)(nil)

const voidRequestColumns = `
	id, payment_id, reason, status,
	requested_by, requested_at, executed_by, executed_at,
	rejected_by, rejected_at, rejection_reason`

func scanVoidRequest(row pgx.Row) (*payment.VoidRequest, error) {
	var v payment.VoidRequest
	if err := row.Scan(
		&v.ID, &v.PaymentID, &v.Reason, &v.Status,
		&v.RequestedBy, &v.RequestedAt, &v.ExecutedBy, &v.ExecutedAt,
		&v.RejectedBy, &v.RejectedAt, &v.RejectionReason,
	); err != nil {
		return nil, err
	}
	return &v, nil
}

// Create raises a request to void a payment. A partial unique index allows one
// live request per payment.
func (r *VoidRequestRepository) Create(ctx context.Context, v *payment.VoidRequest) error {
	if err := r.db.RequireTx(ctx, "void_request.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO void_request (
			id, payment_id, reason, status,
			requested_by, requested_at, executed_by, executed_at,
			rejected_by, rejected_at, rejection_reason
		) VALUES ($1, $2, $3, $4, $5, COALESCE($6, now()), $7, $8, $9, $10, $11)
		RETURNING requested_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		v.ID, v.PaymentID, v.Reason, v.Status,
		v.RequestedBy, instant(v.RequestedAt), v.ExecutedBy, v.ExecutedAt,
		v.RejectedBy, v.RejectedAt, v.RejectionReason,
	).Scan(&v.RequestedAt)
	return pg.WrapQuery("void_request.Create", err)
}

// Update records the second signature: the execution or the rejection.
func (r *VoidRequestRepository) Update(ctx context.Context, v *payment.VoidRequest) error {
	if err := r.db.RequireTx(ctx, "void_request.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE void_request SET
			status           = $2,
			executed_by      = $3,
			executed_at      = $4,
			rejected_by      = $5,
			rejected_at      = $6,
			rejection_reason = $7
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		v.ID, v.Status, v.ExecutedBy, v.ExecutedAt, v.RejectedBy, v.RejectedAt, v.RejectionReason,
	).Scan(&id)
	return pg.WrapQuery("void_request.Update", err)
}

// GetByID returns one void request.
func (r *VoidRequestRepository) GetByID(ctx context.Context, id shared.ID) (*payment.VoidRequest, error) {
	q := r.db.Conn(ctx)
	v, err := scanVoidRequest(q.QueryRow(ctx, `SELECT`+voidRequestColumns+` FROM void_request WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("void_request.GetByID", err)
	}
	return v, nil
}

// ListPending returns the requests still awaiting a second signature. This is
// what the void register reads from, the most useful fraud-detection report in
// the system.
func (r *VoidRequestRepository) ListPending(ctx context.Context) ([]*payment.VoidRequest, error) {
	const query = `
		SELECT` + voidRequestColumns + `
		FROM void_request
		WHERE status = 'requested'
		ORDER BY requested_at`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("void_request.ListPending", err)
	}
	requests, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.VoidRequest, error) {
		return scanVoidRequest(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("void_request.ListPending", err)
	}
	return requests, nil
}
