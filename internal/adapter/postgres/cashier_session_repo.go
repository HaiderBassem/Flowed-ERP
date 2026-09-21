package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// CashierSessionRepository stores cashier shifts.
type CashierSessionRepository struct{ db *pg.DB }

// NewCashierSessionRepository builds the cashier session store over a connection pool.
func NewCashierSessionRepository(db *pg.DB) *CashierSessionRepository {
	return &CashierSessionRepository{db: db}
}

var _ port.CashierSessionRepository = (*CashierSessionRepository)(nil)

const cashierSessionColumns = `
	id, cashier_user_id, cashier_desk_id, academic_year_id,
	opened_at, opening_float, closed_at,
	expected_cash, counted_cash, variance, variance_reason,
	status, approved_by, approved_at, notes`

func scanCashierSession(row pgx.Row) (*payment.CashierSession, error) {
	var s payment.CashierSession
	if err := row.Scan(
		&s.ID, &s.CashierUserID, &s.CashierDeskID, &s.AcademicYearID,
		&s.OpenedAt, &s.OpeningFloat, &s.ClosedAt,
		&s.ExpectedCash, &s.CountedCash, &s.Variance, &s.VarianceReason,
		&s.Status, &s.ApprovedBy, &s.ApprovedAt, &s.Notes,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// Open starts a shift. A partial unique index allows one open drawer per
// cashier, so a second Open collides rather than splitting the takings.
func (r *CashierSessionRepository) Open(ctx context.Context, s *payment.CashierSession) error {
	if err := r.db.RequireTx(ctx, "cashier_session.Open"); err != nil {
		return err
	}
	const query = `
		INSERT INTO cashier_session (
			id, cashier_user_id, cashier_desk_id, academic_year_id,
			opened_at, opening_float, status, notes
		) VALUES ($1, $2, $3, $4, COALESCE($5, now()), $6, $7, $8)
		RETURNING opened_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		s.ID, s.CashierUserID, s.CashierDeskID, s.AcademicYearID,
		instant(s.OpenedAt), s.OpeningFloat, s.Status, s.Notes,
	).Scan(&s.OpenedAt)
	return pg.WrapQuery("cashier_session.Open", err)
}

// Close records the count against the expectation and ends the shift.
func (r *CashierSessionRepository) Close(ctx context.Context, s *payment.CashierSession) error {
	if err := r.db.RequireTx(ctx, "cashier_session.Close"); err != nil {
		return err
	}
	const query = `
		UPDATE cashier_session SET
			status          = $2,
			closed_at       = $3,
			expected_cash   = $4,
			counted_cash    = $5,
			variance        = $6,
			variance_reason = $7,
			notes           = $8
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		s.ID, s.Status, s.ClosedAt, s.ExpectedCash, s.CountedCash, s.Variance, s.VarianceReason, s.Notes,
	).Scan(&id)
	return pg.WrapQuery("cashier_session.Close", err)
}

// Update writes back a session whose state changed, such as a supervisor
// signing off a closed drawer.
func (r *CashierSessionRepository) Update(ctx context.Context, s *payment.CashierSession) error {
	if err := r.db.RequireTx(ctx, "cashier_session.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE cashier_session SET
			opening_float   = $2,
			closed_at       = $3,
			expected_cash   = $4,
			counted_cash    = $5,
			variance        = $6,
			variance_reason = $7,
			status          = $8,
			approved_by     = $9,
			approved_at     = $10,
			notes           = $11
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		s.ID, s.OpeningFloat, s.ClosedAt, s.ExpectedCash, s.CountedCash,
		s.Variance, s.VarianceReason, s.Status, s.ApprovedBy, s.ApprovedAt, s.Notes,
	).Scan(&id)
	return pg.WrapQuery("cashier_session.Update", err)
}

// GetByID returns one shift.
func (r *CashierSessionRepository) GetByID(ctx context.Context, id shared.ID) (*payment.CashierSession, error) {
	q := r.db.Conn(ctx)
	s, err := scanCashierSession(q.QueryRow(ctx, `SELECT`+cashierSessionColumns+` FROM cashier_session WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("cashier_session.GetByID", err)
	}
	return s, nil
}

// GetOpenForUser returns the cashier's open drawer, if they have one.
func (r *CashierSessionRepository) GetOpenForUser(ctx context.Context, userID shared.ID) (*payment.CashierSession, error) {
	const query = `
		SELECT` + cashierSessionColumns + `
		FROM cashier_session
		WHERE cashier_user_id = $1 AND status = 'open'`

	q := r.db.Conn(ctx)
	s, err := scanCashierSession(q.QueryRow(ctx, query, userID))
	if err != nil {
		return nil, pg.WrapQuery("cashier_session.GetOpenForUser", err)
	}
	return s, nil
}

// ExpectedCash totals what the drawer should hold.
//
// Opening float, plus the cash taken during the shift, less cash refunded and
// less cash handed back on voids. A voided payment still carries posted_at and
// its receipt number, so the cash-in term counts it and the void term takes it
// out again — the pair nets to zero, which is exactly right for the same-shift
// mistake the void path exists to correct. Only a same-shift void can be
// attributed here, because a payment records the session that took it and not
// the session that reversed it; the domain confines voids to that window for
// this reason, and anything later is a refund.
//
// Non-cash methods are excluded through payment_method.is_cash: a bank transfer
// never touched the drawer.
func (r *CashierSessionRepository) ExpectedCash(ctx context.Context, sessionID shared.ID) (money.Amount, error) {
	const query = `
		SELECT (
			s.opening_float
			+ COALESCE((
				SELECT sum(p.amount) FROM payment p
				JOIN payment_method pm ON pm.id = p.payment_method_id
				WHERE p.cashier_session_id = s.id
				  AND pm.is_cash
				  AND p.status IN ('posted', 'voided')
			), 0)
			- COALESCE((
				SELECT sum(f.amount) FROM refund f
				JOIN payment_method pm ON pm.id = f.payment_method_id
				WHERE f.cashier_session_id = s.id
				  AND pm.is_cash
				  AND f.status = 'posted'
			), 0)
			- COALESCE((
				SELECT sum(p.amount) FROM payment p
				JOIN payment_method pm ON pm.id = p.payment_method_id
				WHERE p.cashier_session_id = s.id
				  AND pm.is_cash
				  AND p.status = 'voided'
			), 0)
		)::bigint
		FROM cashier_session s
		WHERE s.id = $1`

	q := r.db.Conn(ctx)
	var expected money.Amount
	if err := q.QueryRow(ctx, query, sessionID).Scan(&expected); err != nil {
		return 0, pg.WrapQuery("cashier_session.ExpectedCash", err)
	}
	return expected, nil
}
