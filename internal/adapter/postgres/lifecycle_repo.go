package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// LifecycleRepository stores the decisions that end or reshape a student's
// relationship with the university: graduation clearance, installment plan
// revisions and identity merges.
//
// All three tables are append-only at the database level. None is on a hot
// path, and each exists so that a decision taken today can be explained years
// from now — which is exactly what the audit log gives too, but these are
// shaped for the question rather than for the search.
type LifecycleRepository struct{ db *pg.DB }

// NewLifecycleRepository builds the lifecycle store over a connection pool.
func NewLifecycleRepository(db *pg.DB) *LifecycleRepository { return &LifecycleRepository{db: db} }

var _ port.LifecycleRepository = (*LifecycleRepository)(nil)

const clearanceColumns = `
	id, student_id, enrollment_id, academic_year_id, outstanding_total,
	policy, cleared, override_reason, override_by, decided_at, decided_by`

func scanClearance(row pgx.Row) (*port.GraduationClearance, error) {
	var c port.GraduationClearance
	if err := row.Scan(
		&c.ID, &c.StudentID, &c.EnrollmentID, &c.AcademicYearID, &c.Outstanding,
		&c.Policy, &c.Cleared, &c.OverrideReason, &c.OverrideBy, &c.DecidedAt, &c.DecidedBy,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

// RecordClearance appends a clearance decision.
func (r *LifecycleRepository) RecordClearance(ctx context.Context, c *port.GraduationClearance) error {
	if err := r.db.RequireTx(ctx, "clearance.Record"); err != nil {
		return err
	}
	const query = `
		INSERT INTO graduation_clearance (
			id, student_id, enrollment_id, academic_year_id, outstanding_total,
			policy, cleared, override_reason, override_by, decided_at, decided_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10, now()), $11)
		RETURNING decided_at`

	if c.ID == shared.NilID {
		c.ID = shared.NewID()
	}
	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		c.ID, c.StudentID, c.EnrollmentID, c.AcademicYearID, c.Outstanding.Int64(),
		string(c.Policy), c.Cleared, c.OverrideReason, c.OverrideBy,
		instant(c.DecidedAt), c.DecidedBy,
	).Scan(&c.DecidedAt)
	return pg.WrapQuery("clearance.Record", err)
}

// ListClearances returns a student's clearance decisions, newest first.
func (r *LifecycleRepository) ListClearances(ctx context.Context, studentID shared.ID) ([]*port.GraduationClearance, error) {
	const query = `SELECT` + clearanceColumns + `
		FROM graduation_clearance WHERE student_id = $1 ORDER BY decided_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("clearance.ListForStudent", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.GraduationClearance, error) {
		return scanClearance(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("clearance.ListForStudent", err)
	}
	return out, nil
}

// LatestClearance returns the most recent decision for an enrollment.
func (r *LifecycleRepository) LatestClearance(ctx context.Context, enrollmentID shared.ID) (*port.GraduationClearance, error) {
	const query = `SELECT` + clearanceColumns + `
		FROM graduation_clearance WHERE enrollment_id = $1 ORDER BY decided_at DESC LIMIT 1`

	q := r.db.Conn(ctx)
	c, err := scanClearance(q.QueryRow(ctx, query, enrollmentID))
	if err != nil {
		return nil, pg.WrapQuery("clearance.Latest", err)
	}
	return c, nil
}

// RecordPlanRevision appends the record of an installment plan change.
func (r *LifecycleRepository) RecordPlanRevision(ctx context.Context, rev *port.PlanRevision) error {
	if err := r.db.RequireTx(ctx, "plan_revision.Record"); err != nil {
		return err
	}
	const query = `
		INSERT INTO installment_plan_revision (
			id, account_id, plan_version, kind, reason,
			installments_before, installments_after, unpaid_before, unpaid_after,
			approved_by, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at`

	if rev.ID == shared.NilID {
		rev.ID = shared.NewID()
	}
	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		rev.ID, rev.AccountID, rev.PlanVersion, rev.Kind, rev.Reason,
		rev.InstallmentsBefore, rev.InstallmentsAfter,
		rev.UnpaidBefore.Int64(), rev.UnpaidAfter.Int64(),
		rev.ApprovedBy, rev.CreatedBy,
	).Scan(&rev.CreatedAt)
	return pg.WrapQuery("plan_revision.Record", err)
}

// ListPlanRevisions returns an account's plan history, newest first.
func (r *LifecycleRepository) ListPlanRevisions(ctx context.Context, accountID shared.ID) ([]*port.PlanRevision, error) {
	const query = `
		SELECT id, account_id, plan_version, kind, reason,
		       installments_before, installments_after, unpaid_before, unpaid_after,
		       approved_by, created_at, created_by
		FROM installment_plan_revision
		WHERE account_id = $1
		ORDER BY created_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("plan_revision.List", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.PlanRevision, error) {
		var rev port.PlanRevision
		err := row.Scan(&rev.ID, &rev.AccountID, &rev.PlanVersion, &rev.Kind, &rev.Reason,
			&rev.InstallmentsBefore, &rev.InstallmentsAfter, &rev.UnpaidBefore, &rev.UnpaidAfter,
			&rev.ApprovedBy, &rev.CreatedAt, &rev.CreatedBy)
		return &rev, err
	})
	if err != nil {
		return nil, pg.WrapQuery("plan_revision.List", err)
	}
	return out, nil
}

// RecordMerge appends the record of one duplicate folded into another.
func (r *LifecycleRepository) RecordMerge(ctx context.Context, m *port.StudentMerge) error {
	if err := r.db.RequireTx(ctx, "student_merge.Record"); err != nil {
		return err
	}
	const query = `
		INSERT INTO student_merge (
			id, source_id, target_id, reason,
			enrollments_moved, accounts_moved, discounts_moved, merged_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING merged_at`

	if m.ID == shared.NilID {
		m.ID = shared.NewID()
	}
	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		m.ID, m.SourceID, m.TargetID, m.Reason,
		m.EnrollmentsMoved, m.AccountsMoved, m.DiscountsMoved, m.MergedBy,
	).Scan(&m.MergedAt)
	return pg.WrapQuery("student_merge.Record", err)
}

const mergeColumns = `
	id, source_id, target_id, reason,
	enrollments_moved, accounts_moved, discounts_moved, merged_at, merged_by`

func scanMerge(row pgx.Row) (*port.StudentMerge, error) {
	var m port.StudentMerge
	if err := row.Scan(&m.ID, &m.SourceID, &m.TargetID, &m.Reason,
		&m.EnrollmentsMoved, &m.AccountsMoved, &m.DiscountsMoved, &m.MergedAt, &m.MergedBy); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMerges returns everything folded into one record.
func (r *LifecycleRepository) ListMerges(ctx context.Context, targetID shared.ID) ([]*port.StudentMerge, error) {
	const query = `SELECT` + mergeColumns + ` FROM student_merge WHERE target_id = $1 ORDER BY merged_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, targetID)
	if err != nil {
		return nil, pg.WrapQuery("student_merge.List", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.StudentMerge, error) {
		return scanMerge(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("student_merge.List", err)
	}
	return out, nil
}

// MergeOf returns where a record went, if it was merged away.
func (r *LifecycleRepository) MergeOf(ctx context.Context, sourceID shared.ID) (*port.StudentMerge, error) {
	const query = `SELECT` + mergeColumns + ` FROM student_merge WHERE source_id = $1`

	q := r.db.Conn(ctx)
	m, err := scanMerge(q.QueryRow(ctx, query, sourceID))
	if err != nil {
		return nil, pg.WrapQuery("student_merge.MergeOf", err)
	}
	return m, nil
}
