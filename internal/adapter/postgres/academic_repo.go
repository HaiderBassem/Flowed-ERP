package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// ---------------------------------------------------------------------------
// Academic years
// ---------------------------------------------------------------------------

// AcademicYearRepository stores academic years.
type AcademicYearRepository struct{ db *pg.DB }

// NewAcademicYearRepository builds the academic year store over a connection pool.
func NewAcademicYearRepository(db *pg.DB) *AcademicYearRepository {
	return &AcademicYearRepository{db: db}
}

var _ port.AcademicYearRepository = (*AcademicYearRepository)(nil)

const yearColumns = `
	id, code, start_date, end_date, status, registration_deadline, debt_block_policy,
	financially_closed_at, financially_closed_by, closed_at, closed_by,
	adjustment_window_ends, adjustment_reason, created_at, updated_at`

func scanYear(row pgx.Row) (*academic.Year, error) {
	var (
		y        academic.Year
		start    time.Time
		end      time.Time
		deadline *time.Time
	)
	if err := row.Scan(
		&y.ID, &y.Code, &start, &end, &y.Status, &deadline, &y.DebtBlockPolicy,
		&y.FinanciallyClosedAt, &y.FinanciallyClosedBy, &y.ClosedAt, &y.ClosedBy,
		&y.AdjustmentWindowEnds, &y.AdjustmentReason, &y.CreatedAt, &y.UpdatedAt,
	); err != nil {
		return nil, err
	}
	y.StartDate = shared.DateFromTime(start)
	y.EndDate = shared.DateFromTime(end)
	y.RegistrationDeadline = dateOrNil(deadline)
	return &y, nil
}

// Create records a new academic year.
func (r *AcademicYearRepository) Create(ctx context.Context, y *academic.Year) error {
	if err := r.db.RequireTx(ctx, "academic_year.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO academic_year (
			id, code, start_date, end_date, status, registration_deadline, debt_block_policy,
			financially_closed_at, financially_closed_by, closed_at, closed_by,
			adjustment_window_ends, adjustment_reason
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		y.ID, y.Code, y.StartDate.Time(), y.EndDate.Time(), y.Status,
		timeOrNil(y.RegistrationDeadline), y.DebtBlockPolicy,
		y.FinanciallyClosedAt, y.FinanciallyClosedBy, y.ClosedAt, y.ClosedBy,
		y.AdjustmentWindowEnds, y.AdjustmentReason,
	).Scan(&y.CreatedAt, &y.UpdatedAt)
	return pg.WrapQuery("academic_year.Create", err)
}

// Update writes back a year whose lifecycle moved.
func (r *AcademicYearRepository) Update(ctx context.Context, y *academic.Year) error {
	if err := r.db.RequireTx(ctx, "academic_year.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE academic_year SET
			code                   = $2,
			start_date             = $3,
			end_date               = $4,
			status                 = $5,
			registration_deadline  = $6,
			debt_block_policy      = $7,
			financially_closed_at  = $8,
			financially_closed_by  = $9,
			closed_at              = $10,
			closed_by              = $11,
			adjustment_window_ends = $12,
			adjustment_reason      = $13
		WHERE id = $1
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		y.ID, y.Code, y.StartDate.Time(), y.EndDate.Time(), y.Status,
		timeOrNil(y.RegistrationDeadline), y.DebtBlockPolicy,
		y.FinanciallyClosedAt, y.FinanciallyClosedBy, y.ClosedAt, y.ClosedBy,
		y.AdjustmentWindowEnds, y.AdjustmentReason,
	).Scan(&y.UpdatedAt)
	return pg.WrapQuery("academic_year.Update", err)
}

// GetByID returns one academic year.
func (r *AcademicYearRepository) GetByID(ctx context.Context, id shared.ID) (*academic.Year, error) {
	q := r.db.Conn(ctx)
	y, err := scanYear(q.QueryRow(ctx, `SELECT`+yearColumns+` FROM academic_year WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("academic_year.GetByID", err)
	}
	return y, nil
}

// GetByCode returns the year with a code such as "2025-2026".
func (r *AcademicYearRepository) GetByCode(ctx context.Context, code string) (*academic.Year, error) {
	q := r.db.Conn(ctx)
	y, err := scanYear(q.QueryRow(ctx, `SELECT`+yearColumns+` FROM academic_year WHERE code = $1`, code))
	if err != nil {
		return nil, pg.WrapQuery("academic_year.GetByCode", err)
	}
	return y, nil
}

// List returns every academic year, most recent first.
func (r *AcademicYearRepository) List(ctx context.Context) ([]*academic.Year, error) {
	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, `SELECT`+yearColumns+` FROM academic_year ORDER BY start_date DESC`)
	if err != nil {
		return nil, pg.WrapQuery("academic_year.List", err)
	}
	years, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.Year, error) {
		return scanYear(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("academic_year.List", err)
	}
	return years, nil
}

// GetForUpdate locks the year row for the rest of the transaction.
//
// Every money command takes this lock before posting. Without it a payment that
// read "open" under MVCC could commit into a year that closed a millisecond
// later, and the closing transaction's own totals would already be written.
func (r *AcademicYearRepository) GetForUpdate(ctx context.Context, id shared.ID) (*academic.Year, error) {
	if err := r.db.RequireTx(ctx, "academic_year.GetForUpdate"); err != nil {
		return nil, err
	}
	q := r.db.Conn(ctx)
	y, err := scanYear(q.QueryRow(ctx, `SELECT`+yearColumns+` FROM academic_year WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, pg.WrapQuery("academic_year.GetForUpdate", err)
	}
	return y, nil
}

// CurrentOpen returns the years money may move in. Two are legitimately open
// each autumn, while second-round results close one and registration opens the
// next.
func (r *AcademicYearRepository) CurrentOpen(ctx context.Context) ([]*academic.Year, error) {
	const query = `
		SELECT` + yearColumns + `
		FROM academic_year
		WHERE status IN ('open', 'adjustment_open')
		ORDER BY start_date`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("academic_year.CurrentOpen", err)
	}
	years, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.Year, error) {
		return scanYear(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("academic_year.CurrentOpen", err)
	}
	return years, nil
}

// ---------------------------------------------------------------------------
// Enrollments
// ---------------------------------------------------------------------------

// EnrollmentRepository stores enrollments and their hosting overlays.
type EnrollmentRepository struct{ db *pg.DB }

// NewEnrollmentRepository builds the enrollment store over a connection pool.
func NewEnrollmentRepository(db *pg.DB) *EnrollmentRepository {
	return &EnrollmentRepository{db: db}
}

var _ port.EnrollmentRepository = (*EnrollmentRepository)(nil)

const enrollmentColumns = `
	e.id, e.student_id, e.academic_year_id, e.sequence_no,
	e.college_id, e.department_id, e.study_type_id, e.student_category_id,
	e.stage, e.attempt_number, e.enrollment_kind,
	e.enrollment_status, e.academic_result, e.result_by_decision,
	e.result_recorded_at, e.result_recorded_by,
	e.previous_enrollment_id, e.supersedes_id, e.supersede_reason, e.supersede_date,
	e.deferral_order_ref, e.transfer_order_ref, e.return_order_ref, e.notes,
	e.registered_at, e.registered_by, e.created_at, e.updated_at`

// scanEnrollment reads one enrollment row. Extra destinations serve the queries
// that select a computed column alongside the entity.
func scanEnrollment(row pgx.Row, extra ...any) (*academic.Enrollment, error) {
	var (
		e             academic.Enrollment
		supersedeDate *time.Time
	)
	dest := []any{
		&e.ID, &e.StudentID, &e.AcademicYearID, &e.SequenceNo,
		&e.CollegeID, &e.DepartmentID, &e.StudyTypeID, &e.StudentCategoryID,
		&e.Stage, &e.AttemptNumber, &e.Kind,
		&e.Status, &e.Result, &e.ResultByDecision,
		&e.ResultRecordedAt, &e.ResultRecordedBy,
		&e.PreviousEnrollmentID, &e.SupersedesID, &e.SupersedeReason, &supersedeDate,
		&e.DeferralOrderRef, &e.TransferOrderRef, &e.ReturnOrderRef, &e.Notes,
		&e.RegisteredAt, &e.RegisteredBy, &e.CreatedAt, &e.UpdatedAt,
	}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	e.SupersedeDate = dateOrNil(supersedeDate)
	return &e, nil
}

// Create records a registration.
func (r *EnrollmentRepository) Create(ctx context.Context, e *academic.Enrollment) error {
	if err := r.db.RequireTx(ctx, "enrollment.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO enrollment (
			id, student_id, academic_year_id, sequence_no,
			college_id, department_id, study_type_id, student_category_id,
			stage, attempt_number, enrollment_kind,
			enrollment_status, academic_result, result_by_decision,
			result_recorded_at, result_recorded_by,
			previous_enrollment_id, supersedes_id, supersede_reason, supersede_date,
			deferral_order_ref, transfer_order_ref, return_order_ref, notes,
			registered_at, registered_by
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14,
			$15, $16,
			$17, $18, $19, $20,
			$21, $22, $23, $24,
			COALESCE($25, now()), $26
		)
		RETURNING registered_at, created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		e.ID, e.StudentID, e.AcademicYearID, e.SequenceNo,
		e.CollegeID, e.DepartmentID, e.StudyTypeID, e.StudentCategoryID,
		e.Stage, e.AttemptNumber, e.Kind,
		e.Status, e.Result, e.ResultByDecision,
		e.ResultRecordedAt, e.ResultRecordedBy,
		e.PreviousEnrollmentID, e.SupersedesID, e.SupersedeReason, timeOrNil(e.SupersedeDate),
		e.DeferralOrderRef, e.TransferOrderRef, e.ReturnOrderRef, e.Notes,
		instant(e.RegisteredAt), e.RegisteredBy,
	).Scan(&e.RegisteredAt, &e.CreatedAt, &e.UpdatedAt)
	return pg.WrapQuery("enrollment.Create", err)
}

// Update writes back an enrollment whose status, result or lineage changed.
func (r *EnrollmentRepository) Update(ctx context.Context, e *academic.Enrollment) error {
	if err := r.db.RequireTx(ctx, "enrollment.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE enrollment SET
			college_id             = $2,
			department_id          = $3,
			study_type_id          = $4,
			student_category_id    = $5,
			stage                  = $6,
			attempt_number         = $7,
			enrollment_kind        = $8,
			enrollment_status      = $9,
			academic_result        = $10,
			result_by_decision     = $11,
			result_recorded_at     = $12,
			result_recorded_by     = $13,
			previous_enrollment_id = $14,
			supersedes_id          = $15,
			supersede_reason       = $16,
			supersede_date         = $17,
			deferral_order_ref     = $18,
			transfer_order_ref     = $19,
			return_order_ref       = $20,
			notes                  = $21
		WHERE id = $1
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		e.ID,
		e.CollegeID, e.DepartmentID, e.StudyTypeID, e.StudentCategoryID,
		e.Stage, e.AttemptNumber, e.Kind,
		e.Status, e.Result, e.ResultByDecision,
		e.ResultRecordedAt, e.ResultRecordedBy,
		e.PreviousEnrollmentID, e.SupersedesID, e.SupersedeReason, timeOrNil(e.SupersedeDate),
		e.DeferralOrderRef, e.TransferOrderRef, e.ReturnOrderRef, e.Notes,
	).Scan(&e.UpdatedAt)
	return pg.WrapQuery("enrollment.Update", err)
}

// GetByID returns one enrollment.
func (r *EnrollmentRepository) GetByID(ctx context.Context, id shared.ID) (*academic.Enrollment, error) {
	q := r.db.Conn(ctx)
	e, err := scanEnrollment(q.QueryRow(ctx, `SELECT`+enrollmentColumns+` FROM enrollment e WHERE e.id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("enrollment.GetByID", err)
	}
	return e, nil
}

// GetLive returns the enrollment occupying the student's slot in a year. At
// most one can exist; a partial unique index enforces it.
func (r *EnrollmentRepository) GetLive(ctx context.Context, studentID, yearID shared.ID) (*academic.Enrollment, error) {
	const query = `
		SELECT` + enrollmentColumns + `
		FROM enrollment e
		WHERE e.student_id = $1
		  AND e.academic_year_id = $2
		  AND e.enrollment_status IN ('draft', 'active', 'deferred')`

	q := r.db.Conn(ctx)
	e, err := scanEnrollment(q.QueryRow(ctx, query, studentID, yearID))
	if err != nil {
		return nil, pg.WrapQuery("enrollment.GetLive", err)
	}
	return e, nil
}

// History returns every enrollment a student has held, oldest year first,
// including superseded rows so the whole lineage is visible.
func (r *EnrollmentRepository) History(ctx context.Context, studentID shared.ID) ([]*academic.Enrollment, error) {
	const query = `
		SELECT` + enrollmentColumns + `
		FROM enrollment e
		JOIN academic_year y ON y.id = e.academic_year_id
		WHERE e.student_id = $1
		ORDER BY y.start_date, e.sequence_no`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("enrollment.History", err)
	}
	history, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.Enrollment, error) {
		return scanEnrollment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("enrollment.History", err)
	}
	return history, nil
}

// List returns a filtered page of enrollments and the full match count.
func (r *EnrollmentRepository) List(ctx context.Context, f port.EnrollmentFilter) ([]*academic.Enrollment, int, error) {
	args := &argList{}
	where := enrollmentPredicates(f, args)

	limit := boundedLimit(f.Limit, 50)
	offset := max(f.Offset, 0)

	query := `
		SELECT` + enrollmentColumns + `, count(*) OVER () AS total_count
		FROM enrollment e
		WHERE ` + strings.Join(where, "\n\t\t  AND ") + `
		ORDER BY e.registered_at DESC, e.id DESC
		LIMIT ` + args.next(limit) + ` OFFSET ` + args.next(offset)

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args.all()...)
	if err != nil {
		return nil, 0, pg.WrapQuery("enrollment.List", err)
	}

	var total int64
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.Enrollment, error) {
		return scanEnrollment(row, &total)
	})
	if err != nil {
		return nil, 0, pg.WrapQuery("enrollment.List", err)
	}

	// A page past the end carries no window count, and reporting zero matches
	// there would make a paginator believe the filter found nothing.
	if len(found) == 0 && offset > 0 {
		countArgs := &argList{}
		countWhere := enrollmentPredicates(f, countArgs)
		countQuery := `SELECT count(*) FROM enrollment e WHERE ` + strings.Join(countWhere, "\n\t\t  AND ")
		if err := q.QueryRow(ctx, countQuery, countArgs.all()...).Scan(&total); err != nil {
			return nil, 0, pg.WrapQuery("enrollment.List.count", err)
		}
	}

	return found, int(total), nil
}

func enrollmentPredicates(f port.EnrollmentFilter, args *argList) []string {
	where := []string{"true"}
	if f.AcademicYearID != nil {
		where = append(where, "e.academic_year_id = "+args.next(*f.AcademicYearID))
	}
	if f.StudentID != nil {
		where = append(where, "e.student_id = "+args.next(*f.StudentID))
	}
	if f.CollegeID != nil {
		where = append(where, "e.college_id = "+args.next(*f.CollegeID))
	}
	if f.DepartmentID != nil {
		where = append(where, "e.department_id = "+args.next(*f.DepartmentID))
	}
	if f.StudyTypeID != nil {
		where = append(where, "e.study_type_id = "+args.next(*f.StudyTypeID))
	}
	if f.Stage != nil {
		where = append(where, "e.stage = "+args.next(*f.Stage))
	}
	if f.Status != nil {
		where = append(where, "e.enrollment_status = "+args.next(string(*f.Status)))
	}
	if f.Result != nil {
		where = append(where, "e.academic_result = "+args.next(string(*f.Result)))
	}
	if f.ExcludeSuperseded {
		where = append(where, "e.enrollment_status <> 'superseded'")
	}
	return where
}

// CountAttempts returns how many of a student's registrations at a stage in a
// department consumed an attempt.
//
// The result alone decides. An earlier design tried to exclude students who
// dropped out "before exams", which nothing in the data records and no two
// clerks would judge alike.
func (r *EnrollmentRepository) CountAttempts(
	ctx context.Context, studentID, departmentID shared.ID, stage int16,
) (int, error) {
	const query = `
		SELECT count(*)
		FROM enrollment
		WHERE student_id = $1
		  AND department_id = $2
		  AND stage = $3
		  AND academic_result IN ('failed', 'passed_r1', 'passed_r2')`

	q := r.db.Conn(ctx)
	var count int64
	if err := q.QueryRow(ctx, query, studentID, departmentID, stage).Scan(&count); err != nil {
		return 0, pg.WrapQuery("enrollment.CountAttempts", err)
	}
	return int(count), nil
}

// NextSequenceNo returns the next position in a student's supersede chain for
// a year.
func (r *EnrollmentRepository) NextSequenceNo(ctx context.Context, studentID, yearID shared.ID) (int16, error) {
	const query = `
		SELECT (COALESCE(max(sequence_no), 0) + 1)::smallint
		FROM enrollment
		WHERE student_id = $1 AND academic_year_id = $2`

	q := r.db.Conn(ctx)
	var next int16
	if err := q.QueryRow(ctx, query, studentID, yearID).Scan(&next); err != nil {
		return 0, pg.WrapQuery("enrollment.NextSequenceNo", err)
	}
	return next, nil
}

// PendingResults counts the enrollments in a year still awaiting an outcome,
// which gates the academic close. Only a draft or active enrollment can carry
// a pending result; every other status implies a recorded one.
func (r *EnrollmentRepository) PendingResults(ctx context.Context, yearID shared.ID) (int, error) {
	const query = `
		SELECT count(*)
		FROM enrollment
		WHERE academic_year_id = $1 AND academic_result = 'pending'`

	q := r.db.Conn(ctx)
	var count int64
	if err := q.QueryRow(ctx, query, yearID).Scan(&count); err != nil {
		return 0, pg.WrapQuery("enrollment.PendingResults", err)
	}
	return int(count), nil
}

const hostingColumns = `
	id, enrollment_id, direction,
	home_university, home_college, home_department, home_study_type_id,
	host_university, host_college, host_department, host_study_type_id,
	fee_collector, period_from, period_to, agreement_ref, notes,
	created_at, updated_at, created_by`

// CreateHostingRecord attaches a hosting overlay to an enrollment.
func (r *EnrollmentRepository) CreateHostingRecord(ctx context.Context, h *academic.HostingRecord) error {
	if err := r.db.RequireTx(ctx, "enrollment.CreateHostingRecord"); err != nil {
		return err
	}
	const query = `
		INSERT INTO hosting_record (
			id, enrollment_id, direction,
			home_university, home_college, home_department, home_study_type_id,
			host_university, host_college, host_department, host_study_type_id,
			fee_collector, period_from, period_to, agreement_ref, notes, created_by
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7,
			$8, $9, $10, $11,
			$12, $13, $14, $15, $16, $17
		)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		h.ID, h.EnrollmentID, h.Direction,
		h.HomeUniversity, h.HomeCollege, h.HomeDepartment, h.HomeStudyTypeID,
		h.HostUniversity, h.HostCollege, h.HostDepartment, h.HostStudyTypeID,
		h.FeeCollector, timeOrNil(h.PeriodFrom), timeOrNil(h.PeriodTo), h.AgreementRef, h.Notes, h.CreatedBy,
	).Scan(&h.CreatedAt, &h.UpdatedAt)
	return pg.WrapQuery("enrollment.CreateHostingRecord", err)
}

// GetHostingRecord returns an enrollment's hosting overlay.
func (r *EnrollmentRepository) GetHostingRecord(ctx context.Context, enrollmentID shared.ID) (*academic.HostingRecord, error) {
	q := r.db.Conn(ctx)
	var (
		h    academic.HostingRecord
		from *time.Time
		to   *time.Time
	)
	err := q.QueryRow(ctx, `SELECT`+hostingColumns+` FROM hosting_record WHERE enrollment_id = $1`, enrollmentID).Scan(
		&h.ID, &h.EnrollmentID, &h.Direction,
		&h.HomeUniversity, &h.HomeCollege, &h.HomeDepartment, &h.HomeStudyTypeID,
		&h.HostUniversity, &h.HostCollege, &h.HostDepartment, &h.HostStudyTypeID,
		&h.FeeCollector, &from, &to, &h.AgreementRef, &h.Notes,
		&h.CreatedAt, &h.UpdatedAt, &h.CreatedBy,
	)
	if err != nil {
		return nil, pg.WrapQuery("enrollment.GetHostingRecord", err)
	}
	h.PeriodFrom = dateOrNil(from)
	h.PeriodTo = dateOrNil(to)
	return &h, nil
}
