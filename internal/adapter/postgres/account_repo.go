package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// AccountRepository stores financial accounts and everything frozen into them:
// the fee snapshot, the adjustments posted since, and the credits held.
type AccountRepository struct{ db *pg.DB }

// NewAccountRepository builds the financial account store over a connection pool.
func NewAccountRepository(db *pg.DB) *AccountRepository { return &AccountRepository{db: db} }

var _ port.AccountRepository = (*AccountRepository)(nil)

// Qualified with the fa alias because ListForStudent joins academic_year,
// which carries an id column of its own.
const accountColumns = `
	fa.id, fa.enrollment_id, fa.academic_year_id, fa.student_id,
	fa.college_id, fa.department_id, fa.study_type_id, fa.stage,
	fa.fee_policy_id, fa.fee_policy_specificity, fa.installment_template_id,
	fa.gross_total, fa.discountable_base, fa.discount_total, fa.net_total,
	fa.adjustment_total, fa.paid_total, fa.refunded_total, fa.credit_balance,
	fa.status, fa.superseded_by_account_id,
	fa.generated_at, fa.generated_by, fa.activated_at, fa.settled_at,
	fa.cancelled_at, fa.cancellation_reason, fa.last_reconciled_at,
	fa.created_at, fa.updated_at`

func scanAccount(row pgx.Row) (*billing.Account, error) {
	var a billing.Account
	if err := row.Scan(
		&a.ID, &a.EnrollmentID, &a.AcademicYearID, &a.StudentID,
		&a.CollegeID, &a.DepartmentID, &a.StudyTypeID, &a.Stage,
		&a.FeePolicyID, &a.FeePolicySpecificity, &a.InstallmentTemplateID,
		&a.GrossTotal, &a.DiscountableBase, &a.DiscountTotal, &a.NetTotal,
		&a.AdjustmentTotal, &a.PaidTotal, &a.RefundedTotal, &a.CreditBalance,
		&a.Status, &a.SupersededByAccountID,
		&a.GeneratedAt, &a.GeneratedBy, &a.ActivatedAt, &a.SettledAt,
		&a.CancelledAt, &a.CancellationReason, &a.LastReconciledAt,
		&a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &a, nil
}

// Create writes the account and its frozen fee snapshot together.
//
// The two belong to one act: an account whose snapshot lines failed to land
// would report a gross total no list of components explains, and the snapshot
// is what an audit reconstructs the charge from years later.
func (r *AccountRepository) Create(ctx context.Context, a *billing.Account, snapshot []*billing.SnapshotLine) error {
	if err := r.db.RequireTx(ctx, "account.Create"); err != nil {
		return err
	}
	const insertAccount = `
		INSERT INTO financial_account (
			id, enrollment_id, academic_year_id, student_id,
			college_id, department_id, study_type_id, stage,
			fee_policy_id, fee_policy_specificity, installment_template_id,
			gross_total, discountable_base, discount_total, net_total,
			adjustment_total, paid_total, refunded_total, credit_balance,
			status, superseded_by_account_id,
			generated_at, generated_by, activated_at, settled_at,
			cancelled_at, cancellation_reason, last_reconciled_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19,
			$20, $21,
			COALESCE($22, now()), $23, $24, $25,
			$26, $27, $28
		)
		RETURNING generated_at, created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, insertAccount,
		a.ID, a.EnrollmentID, a.AcademicYearID, a.StudentID,
		a.CollegeID, a.DepartmentID, a.StudyTypeID, a.Stage,
		a.FeePolicyID, a.FeePolicySpecificity, a.InstallmentTemplateID,
		a.GrossTotal, a.DiscountableBase, a.DiscountTotal, a.NetTotal,
		a.AdjustmentTotal, a.PaidTotal, a.RefundedTotal, a.CreditBalance,
		a.Status, a.SupersededByAccountID,
		instant(a.GeneratedAt), a.GeneratedBy, a.ActivatedAt, a.SettledAt,
		a.CancelledAt, a.CancellationReason, a.LastReconciledAt,
	).Scan(&a.GeneratedAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return pg.WrapQuery("account.Create", err)
	}

	if len(snapshot) == 0 {
		return nil
	}

	const insertLine = `
		INSERT INTO fee_snapshot_line (
			id, account_id, component_code, name_ar, amount,
			is_discountable, is_refundable, sort_order, source_component_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10, now()))`

	batch := &pgx.Batch{}
	for _, line := range snapshot {
		if shared.IsNil(line.ID) {
			line.ID = shared.NewID()
		}
		line.AccountID = a.ID
		batch.Queue(insertLine,
			line.ID, line.AccountID, line.ComponentCode, line.NameAr, line.Amount,
			line.IsDiscountable, line.IsRefundable, line.SortOrder, line.SourceComponentID,
			instant(line.CreatedAt),
		)
	}
	return pg.WrapQuery("account.Create.snapshot", execBatch(ctx, q, batch))
}

// Reassign points an account at another student record, for a merge.
//
// Only the owner column moves: no total, no payment, no receipt is touched, so
// a receipt printed under the old student number still reads the same. The
// database checks that a student_merge row justifies it.
func (r *AccountRepository) Reassign(ctx context.Context, accountID, toStudentID shared.ID) error {
	if err := r.db.RequireTx(ctx, "account.Reassign"); err != nil {
		return err
	}
	const query = `UPDATE financial_account SET student_id = $2 WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, accountID, toStudentID).Scan(&id)
	return pg.WrapQuery("account.Reassign", err)
}

// Update writes back the account's cached totals and lifecycle stamps. The
// frozen gross, discount and net are included because a regeneration may
// legitimately restate them before any money has moved.
func (r *AccountRepository) Update(ctx context.Context, a *billing.Account) error {
	if err := r.db.RequireTx(ctx, "account.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE financial_account SET
			fee_policy_id            = $2,
			fee_policy_specificity   = $3,
			installment_template_id  = $4,
			gross_total              = $5,
			discountable_base        = $6,
			discount_total           = $7,
			net_total                = $8,
			adjustment_total         = $9,
			paid_total               = $10,
			refunded_total           = $11,
			credit_balance           = $12,
			status                   = $13,
			superseded_by_account_id = $14,
			activated_at             = $15,
			settled_at               = $16,
			cancelled_at             = $17,
			cancellation_reason      = $18,
			last_reconciled_at       = $19
		WHERE id = $1
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		a.ID, a.FeePolicyID, a.FeePolicySpecificity, a.InstallmentTemplateID,
		a.GrossTotal, a.DiscountableBase, a.DiscountTotal, a.NetTotal,
		a.AdjustmentTotal, a.PaidTotal, a.RefundedTotal, a.CreditBalance,
		a.Status, a.SupersededByAccountID,
		a.ActivatedAt, a.SettledAt, a.CancelledAt, a.CancellationReason, a.LastReconciledAt,
	).Scan(&a.UpdatedAt)
	return pg.WrapQuery("account.Update", err)
}

// GetByID returns one account.
func (r *AccountRepository) GetByID(ctx context.Context, id shared.ID) (*billing.Account, error) {
	q := r.db.Conn(ctx)
	a, err := scanAccount(q.QueryRow(ctx, `SELECT`+accountColumns+` FROM financial_account fa WHERE fa.id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("account.GetByID", err)
	}
	return a, nil
}

// GetByEnrollment returns the live account of an enrollment.
//
// Cancelled accounts are excluded, which is also how the unique index is
// scoped: an account generated against the wrong policy is cancelled and
// regenerated, and both rows then exist against the same enrollment.
func (r *AccountRepository) GetByEnrollment(ctx context.Context, enrollmentID shared.ID) (*billing.Account, error) {
	const query = `
		SELECT` + accountColumns + `
		FROM financial_account fa
		WHERE fa.enrollment_id = $1 AND fa.status <> 'cancelled'`

	q := r.db.Conn(ctx)
	a, err := scanAccount(q.QueryRow(ctx, query, enrollmentID))
	if err != nil {
		return nil, pg.WrapQuery("account.GetByEnrollment", err)
	}
	return a, nil
}

// GetForUpdate locks the account row for the rest of the transaction.
//
// Every command that moves money takes this lock first. It serialises two
// cashiers working on the same student and is what makes the cached totals
// safe to maintain in-transaction rather than recomputed on every read. The
// lock order is fixed system-wide — account, then year, then number series —
// so two concurrent commands cannot deadlock against each other.
func (r *AccountRepository) GetForUpdate(ctx context.Context, id shared.ID) (*billing.Account, error) {
	if err := r.db.RequireTx(ctx, "account.GetForUpdate"); err != nil {
		return nil, err
	}
	q := r.db.Conn(ctx)
	a, err := scanAccount(q.QueryRow(ctx, `SELECT`+accountColumns+` FROM financial_account fa WHERE fa.id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, pg.WrapQuery("account.GetForUpdate", err)
	}
	return a, nil
}

// ListForStudent returns every account a student holds, newest year first.
func (r *AccountRepository) ListForStudent(ctx context.Context, studentID shared.ID) ([]*billing.Account, error) {
	const query = `
		SELECT` + accountColumns + `
		FROM financial_account fa
		JOIN academic_year y ON y.id = fa.academic_year_id
		WHERE fa.student_id = $1
		ORDER BY y.start_date DESC, fa.generated_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("account.ListForStudent", err)
	}
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Account, error) {
		return scanAccount(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("account.ListForStudent", err)
	}
	return accounts, nil
}

// Snapshot returns the fee components frozen onto an account.
func (r *AccountRepository) Snapshot(ctx context.Context, accountID shared.ID) ([]*billing.SnapshotLine, error) {
	const query = `
		SELECT id, account_id, component_code, name_ar, amount,
		       is_discountable, is_refundable, sort_order, source_component_id, created_at
		FROM fee_snapshot_line
		WHERE account_id = $1
		ORDER BY sort_order, component_code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("account.Snapshot", err)
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.SnapshotLine, error) {
		var line billing.SnapshotLine
		if err := row.Scan(
			&line.ID, &line.AccountID, &line.ComponentCode, &line.NameAr, &line.Amount,
			&line.IsDiscountable, &line.IsRefundable, &line.SortOrder, &line.SourceComponentID, &line.CreatedAt,
		); err != nil {
			return nil, err
		}
		return &line, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("account.Snapshot", err)
	}
	return lines, nil
}

// ---------------------------------------------------------------------------
// Adjustments
// ---------------------------------------------------------------------------

const adjustmentColumns = `
	id, account_id, adjustment_type, amount, reason,
	paired_adjustment_id, reference_type, reference_id,
	approved_by, approved_at, posted_at, posted_by, posting_year_id`

func scanAdjustment(row pgx.Row) (*billing.Adjustment, error) {
	var adj billing.Adjustment
	if err := row.Scan(
		&adj.ID, &adj.AccountID, &adj.Type, &adj.Amount, &adj.Reason,
		&adj.PairedAdjustmentID, &adj.ReferenceType, &adj.ReferenceID,
		&adj.ApprovedBy, &adj.ApprovedAt, &adj.PostedAt, &adj.PostedBy, &adj.PostingYearID,
	); err != nil {
		return nil, err
	}
	return &adj, nil
}

// CreateAdjustment posts a signed change to what an account owes.
func (r *AccountRepository) CreateAdjustment(ctx context.Context, adj *billing.Adjustment) error {
	if err := r.db.RequireTx(ctx, "account.CreateAdjustment"); err != nil {
		return err
	}
	const query = `
		INSERT INTO account_adjustment (
			id, account_id, adjustment_type, amount, reason,
			paired_adjustment_id, reference_type, reference_id,
			approved_by, approved_at, posted_at, posted_by, posting_year_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, now()), $12, $13)
		RETURNING posted_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		adj.ID, adj.AccountID, adj.Type, adj.Amount, adj.Reason,
		adj.PairedAdjustmentID, adj.ReferenceType, adj.ReferenceID,
		adj.ApprovedBy, adj.ApprovedAt, instant(adj.PostedAt), adj.PostedBy, adj.PostingYearID,
	).Scan(&adj.PostedAt)
	return pg.WrapQuery("account.CreateAdjustment", err)
}

// ListAdjustments returns every change posted against an account since it was
// generated, oldest first.
func (r *AccountRepository) ListAdjustments(ctx context.Context, accountID shared.ID) ([]*billing.Adjustment, error) {
	const query = `
		SELECT` + adjustmentColumns + `
		FROM account_adjustment
		WHERE account_id = $1
		ORDER BY posted_at, id`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("account.ListAdjustments", err)
	}
	adjustments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Adjustment, error) {
		return scanAdjustment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("account.ListAdjustments", err)
	}
	return adjustments, nil
}

// ---------------------------------------------------------------------------
// Credits
// ---------------------------------------------------------------------------

const creditColumns = `
	id, account_id, student_id, amount, consumed_amount,
	source, source_reference_id, status, reason,
	created_at, created_by, closed_at`

func scanCredit(row pgx.Row) (*billing.CreditEntry, error) {
	var c billing.CreditEntry
	if err := row.Scan(
		&c.ID, &c.AccountID, &c.StudentID, &c.Amount, &c.ConsumedAmount,
		&c.Source, &c.SourceReference, &c.Status, &c.Reason,
		&c.CreatedAt, &c.CreatedBy, &c.ClosedAt,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateCredit records money the university holds that the student has not
// spent.
func (r *AccountRepository) CreateCredit(ctx context.Context, c *billing.CreditEntry) error {
	if err := r.db.RequireTx(ctx, "account.CreateCredit"); err != nil {
		return err
	}
	const query = `
		INSERT INTO credit_entry (
			id, account_id, student_id, amount, consumed_amount,
			source, source_reference_id, status, reason, created_by, closed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		c.ID, c.AccountID, c.StudentID, c.Amount, c.ConsumedAmount,
		c.Source, c.SourceReference, c.Status, c.Reason, c.CreatedBy, c.ClosedAt,
	).Scan(&c.CreatedAt)
	return pg.WrapQuery("account.CreateCredit", err)
}

// GetCreditForUpdate locks a credit row before it is spent.
//
// Carrying credit forward to next year and refunding it in cash are separate
// paths that lock different accounts. Without a lock on the credit itself both
// could read the same available balance and pay it out twice.
func (r *AccountRepository) GetCreditForUpdate(ctx context.Context, id shared.ID) (*billing.CreditEntry, error) {
	if err := r.db.RequireTx(ctx, "account.GetCreditForUpdate"); err != nil {
		return nil, err
	}
	q := r.db.Conn(ctx)
	c, err := scanCredit(q.QueryRow(ctx, `SELECT`+creditColumns+` FROM credit_entry WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, pg.WrapQuery("account.GetCreditForUpdate", err)
	}
	return c, nil
}

// ListOpenCredits returns a student's unspent credit across every year.
func (r *AccountRepository) ListOpenCredits(ctx context.Context, studentID shared.ID) ([]*billing.CreditEntry, error) {
	const query = `
		SELECT` + creditColumns + `
		FROM credit_entry
		WHERE student_id = $1 AND status IN ('open', 'partially_consumed')
		ORDER BY created_at`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("account.ListOpenCredits", err)
	}
	credits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.CreditEntry, error) {
		return scanCredit(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("account.ListOpenCredits", err)
	}
	return credits, nil
}

// UpdateCredit writes back a credit whose consumed amount or status changed.
func (r *AccountRepository) UpdateCredit(ctx context.Context, c *billing.CreditEntry) error {
	if err := r.db.RequireTx(ctx, "account.UpdateCredit"); err != nil {
		return err
	}
	const query = `
		UPDATE credit_entry SET
			consumed_amount = $2,
			status          = $3,
			reason          = $4,
			closed_at       = $5
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, c.ID, c.ConsumedAmount, c.Status, c.Reason, c.ClosedAt).Scan(&id)
	return pg.WrapQuery("account.UpdateCredit", err)
}

// RecordCreditConsumption appends one spend against a credit entry. The rows
// are append-only, so the history of a carried-forward balance is a list of
// facts rather than a decrementing number.
func (r *AccountRepository) RecordCreditConsumption(ctx context.Context, c *billing.CreditConsumption) error {
	if err := r.db.RequireTx(ctx, "account.RecordCreditConsumption"); err != nil {
		return err
	}
	const query = `
		INSERT INTO credit_consumption (
			id, credit_entry_id, amount, consumed_for,
			target_account_id, target_reference_id, consumed_at, consumed_by
		) VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), $8)
		RETURNING consumed_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		c.ID, c.CreditEntryID, c.Amount, c.Purpose,
		c.TargetAccountID, c.TargetReference, instant(c.ConsumedAt), c.ConsumedBy,
	).Scan(&c.ConsumedAt)
	return pg.WrapQuery("account.RecordCreditConsumption", err)
}

// ---------------------------------------------------------------------------
// Aggregates
// ---------------------------------------------------------------------------

// OutstandingForStudent totals what a student still owes across every year,
// optionally ignoring the year being registered.
//
// Each account is floored at zero before summing. An overpaid year must not
// silently cancel a debt on another one: the money is held as credit against
// the account that received it, and offsetting it here would let the debt
// check pass while the debt still exists.
func (r *AccountRepository) OutstandingForStudent(
	ctx context.Context, studentID shared.ID, excludingYear *shared.ID,
) (money.Amount, error) {
	const query = `
		SELECT COALESCE(sum(
			GREATEST((net_total + adjustment_total) - (paid_total - refunded_total), 0)
		), 0)::bigint
		FROM financial_account
		WHERE student_id = $1
		  AND status IN ('pending', 'active')
		  AND ($2::uuid IS NULL OR academic_year_id <> $2)`

	q := r.db.Conn(ctx)
	var outstanding money.Amount
	if err := q.QueryRow(ctx, query, studentID, excludingYear).Scan(&outstanding); err != nil {
		return 0, pg.WrapQuery("account.OutstandingForStudent", err)
	}
	return outstanding, nil
}

// DraftPaymentCount reports how many collections in a year are still
// unfinished, which gates the financial close.
func (r *AccountRepository) DraftPaymentCount(ctx context.Context, yearID shared.ID) (int, error) {
	const query = `SELECT count(*) FROM payment WHERE posting_year_id = $1 AND status = 'draft'`

	q := r.db.Conn(ctx)
	var count int64
	if err := q.QueryRow(ctx, query, yearID).Scan(&count); err != nil {
		return 0, pg.WrapQuery("account.DraftPaymentCount", err)
	}
	return int(count), nil
}

// ReconciliationDrift lists accounts whose cached totals disagree with their
// transaction rows. The view already contains only the disagreeing rows, so an
// empty result is a clean night; anything here is a bug, not rounding.
func (r *AccountRepository) ReconciliationDrift(ctx context.Context, limit int) ([]port.ReconciliationRow, error) {
	const query = `
		SELECT account_id,
		       cached_paid,
		       computed_paid::bigint,
		       cached_refunded,
		       computed_refunded::bigint,
		       cached_adjustment,
		       computed_adjustment::bigint,
		       cached_credit,
		       computed_credit::bigint
		FROM v_account_reconciliation
		ORDER BY account_id
		LIMIT $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, boundedLimit(limit, 100))
	if err != nil {
		return nil, pg.WrapQuery("account.ReconciliationDrift", err)
	}
	drift, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.ReconciliationRow, error) {
		var d port.ReconciliationRow
		if err := row.Scan(
			&d.AccountID,
			&d.CachedPaid, &d.ComputedPaid,
			&d.CachedRefunded, &d.ComputedRefunded,
			&d.CachedAdjustment, &d.ComputedAdjust,
			&d.CachedCredit, &d.ComputedCredit,
		); err != nil {
			return port.ReconciliationRow{}, err
		}
		return d, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("account.ReconciliationDrift", err)
	}
	return drift, nil
}
