package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// InstallmentRepository stores installment plans.
type InstallmentRepository struct{ db *pg.DB }

// NewInstallmentRepository builds the installment store over a connection pool.
func NewInstallmentRepository(db *pg.DB) *InstallmentRepository {
	return &InstallmentRepository{db: db}
}

var _ port.InstallmentRepository = (*InstallmentRepository)(nil)

const installmentColumns = `
	id, account_id, installment_no, due_date, amount, paid_amount,
	status, label_ar, superseded_by_id, plan_version, created_at, updated_at`

func scanInstallment(row pgx.Row) (*billing.Installment, error) {
	var (
		i       billing.Installment
		dueDate time.Time
	)
	if err := row.Scan(
		&i.ID, &i.AccountID, &i.Number, &dueDate, &i.Amount, &i.PaidAmount,
		&i.Status, &i.Label, &i.SupersededByID, &i.PlanVersion, &i.CreatedAt, &i.UpdatedAt,
	); err != nil {
		return nil, err
	}
	i.DueDate = shared.DateFromTime(dueDate)
	return &i, nil
}

// CreatePlan writes a whole installment plan in one round trip. The plan is
// generated as a unit and must land as one: a partially written plan would not
// sum to what the student owes.
func (r *InstallmentRepository) CreatePlan(ctx context.Context, installments []*billing.Installment) error {
	if err := r.db.RequireTx(ctx, "installment.CreatePlan"); err != nil {
		return err
	}
	if len(installments) == 0 {
		return nil
	}

	const query = `
		INSERT INTO installment (
			id, account_id, installment_no, due_date, amount, paid_amount,
			status, label_ar, superseded_by_id, plan_version, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, now()))`

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	for _, i := range installments {
		if shared.IsNil(i.ID) {
			i.ID = shared.NewID()
		}
		batch.Queue(query,
			i.ID, i.AccountID, i.Number, i.DueDate.Time(), i.Amount, i.PaidAmount,
			i.Status, i.Label, i.SupersededByID, i.PlanVersion, instant(i.CreatedAt),
		)
	}
	return pg.WrapQuery("installment.CreatePlan", execBatch(ctx, q, batch))
}

// Update writes back an installment whose paid amount or status changed.
func (r *InstallmentRepository) Update(ctx context.Context, i *billing.Installment) error {
	if err := r.db.RequireTx(ctx, "installment.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE installment SET
			installment_no   = $2,
			due_date         = $3,
			amount           = $4,
			paid_amount      = $5,
			status           = $6,
			label_ar         = $7,
			superseded_by_id = $8,
			plan_version     = $9
		WHERE id = $1
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		i.ID, i.Number, i.DueDate.Time(), i.Amount, i.PaidAmount,
		i.Status, i.Label, i.SupersededByID, i.PlanVersion,
	).Scan(&i.UpdatedAt)
	return pg.WrapQuery("installment.Update", err)
}

// GetByID returns one installment.
func (r *InstallmentRepository) GetByID(ctx context.Context, id shared.ID) (*billing.Installment, error) {
	q := r.db.Conn(ctx)
	i, err := scanInstallment(q.QueryRow(ctx, `SELECT`+installmentColumns+` FROM installment WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("installment.GetByID", err)
	}
	return i, nil
}

// ListForAccount returns an account's whole plan, superseded rows included, so
// the schedule the student agreed to in October is still readable in March.
func (r *InstallmentRepository) ListForAccount(ctx context.Context, accountID shared.ID) ([]*billing.Installment, error) {
	const query = `
		SELECT` + installmentColumns + `
		FROM installment
		WHERE account_id = $1
		ORDER BY installment_no, plan_version`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("installment.ListForAccount", err)
	}
	installments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Installment, error) {
		return scanInstallment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("installment.ListForAccount", err)
	}
	return installments, nil
}

// ListOpenForUpdate returns the account's unsettled installments, locked.
//
// The ORDER BY is the point. PostgreSQL locks rows at the plan node above the
// sort, so the locks are taken in installment number order — the same order for
// every caller. Two cashiers taking payments against the same student therefore
// queue behind one another instead of each holding a row the other needs.
func (r *InstallmentRepository) ListOpenForUpdate(ctx context.Context, accountID shared.ID) ([]*billing.Installment, error) {
	if err := r.db.RequireTx(ctx, "installment.ListOpenForUpdate"); err != nil {
		return nil, err
	}
	const query = `
		SELECT` + installmentColumns + `
		FROM installment
		WHERE account_id = $1 AND status IN ('pending', 'partially_paid')
		ORDER BY installment_no
		FOR UPDATE`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("installment.ListOpenForUpdate", err)
	}
	installments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Installment, error) {
		return scanInstallment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("installment.ListOpenForUpdate", err)
	}
	return installments, nil
}

// SupersedePlan retires the installments a re-split replaced, pointing each at
// its successor. The old rows stay: editing an amount in place would destroy
// the record of the schedule the student actually agreed to.
//
// The updates run in a fixed identifier order for the same reason
// ListOpenForUpdate sorts: a Go map iterates unpredictably, and two concurrent
// re-splits touching overlapping rows in opposite orders would deadlock.
func (r *InstallmentRepository) SupersedePlan(
	ctx context.Context, accountID shared.ID, replacedBy map[shared.ID]shared.ID,
) error {
	if err := r.db.RequireTx(ctx, "installment.SupersedePlan"); err != nil {
		return err
	}
	if len(replacedBy) == 0 {
		return nil
	}

	oldIDs := make([]shared.ID, 0, len(replacedBy))
	for oldID := range replacedBy {
		oldIDs = append(oldIDs, oldID)
	}
	slices.SortFunc(oldIDs, func(a, b shared.ID) int { return slices.Compare(a[:], b[:]) })

	const query = `
		UPDATE installment
		SET status = 'superseded', superseded_by_id = $2
		WHERE id = $1 AND account_id = $3`

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	for _, oldID := range oldIDs {
		batch.Queue(query, oldID, replacedBy[oldID], accountID)
	}
	return pg.WrapQuery("installment.SupersedePlan", execBatch(ctx, q, batch))
}
