package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// ReferenceRepository stores the configuration tables the administration owns:
// colleges, departments, study types, student categories and payment methods.
// They live behind one repository because validating an enrollment's context
// reads all of them together.
type ReferenceRepository struct{ db *pg.DB }

// NewReferenceRepository builds the reference data store over a connection pool.
func NewReferenceRepository(db *pg.DB) *ReferenceRepository { return &ReferenceRepository{db: db} }

var _ port.ReferenceRepository = (*ReferenceRepository)(nil)

// ---------------------------------------------------------------------------
// Colleges
// ---------------------------------------------------------------------------

const collegeColumns = ` id, code, name_ar, name_en, is_active, created_at, updated_at`

func scanCollege(row pgx.Row) (*academic.College, error) {
	var c academic.College
	if err := row.Scan(&c.ID, &c.Code, &c.NameAr, &c.NameEn, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// ListColleges returns the faculties, optionally only the ones in service.
func (r *ReferenceRepository) ListColleges(ctx context.Context, activeOnly bool) ([]*academic.College, error) {
	const query = `
		SELECT` + collegeColumns + `
		FROM college
		WHERE NOT $1::boolean OR is_active
		ORDER BY code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("reference.ListColleges", err)
	}
	colleges, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.College, error) {
		return scanCollege(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("reference.ListColleges", err)
	}
	return colleges, nil
}

// GetCollege returns one faculty.
func (r *ReferenceRepository) GetCollege(ctx context.Context, id shared.ID) (*academic.College, error) {
	q := r.db.Conn(ctx)
	c, err := scanCollege(q.QueryRow(ctx, `SELECT`+collegeColumns+` FROM college WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetCollege", err)
	}
	return c, nil
}

// CreateCollege records a faculty.
func (r *ReferenceRepository) CreateCollege(ctx context.Context, c *academic.College) error {
	if err := r.db.RequireTx(ctx, "reference.CreateCollege"); err != nil {
		return err
	}
	const query = `
		INSERT INTO college (id, code, name_ar, name_en, is_active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, c.ID, c.Code, c.NameAr, c.NameEn, c.IsActive).Scan(&c.CreatedAt, &c.UpdatedAt)
	return pg.WrapQuery("reference.CreateCollege", err)
}

// ---------------------------------------------------------------------------
// Departments
// ---------------------------------------------------------------------------

const departmentColumns = ` id, college_id, code, name_ar, name_en, stage_count, is_active, created_at, updated_at`

func scanDepartment(row pgx.Row) (*academic.Department, error) {
	var d academic.Department
	if err := row.Scan(
		&d.ID, &d.CollegeID, &d.Code, &d.NameAr, &d.NameEn, &d.StageCount,
		&d.IsActive, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDepartments returns the programmes, narrowed to one college when asked.
func (r *ReferenceRepository) ListDepartments(
	ctx context.Context, collegeID *shared.ID, activeOnly bool,
) ([]*academic.Department, error) {
	const query = `
		SELECT` + departmentColumns + `
		FROM department
		WHERE ($1::uuid IS NULL OR college_id = $1)
		  AND (NOT $2::boolean OR is_active)
		ORDER BY code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, collegeID, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("reference.ListDepartments", err)
	}
	departments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.Department, error) {
		return scanDepartment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("reference.ListDepartments", err)
	}
	return departments, nil
}

// GetDepartment returns one programme.
func (r *ReferenceRepository) GetDepartment(ctx context.Context, id shared.ID) (*academic.Department, error) {
	q := r.db.Conn(ctx)
	d, err := scanDepartment(q.QueryRow(ctx, `SELECT`+departmentColumns+` FROM department WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetDepartment", err)
	}
	return d, nil
}

// CreateDepartment records a programme.
func (r *ReferenceRepository) CreateDepartment(ctx context.Context, d *academic.Department) error {
	if err := r.db.RequireTx(ctx, "reference.CreateDepartment"); err != nil {
		return err
	}
	const query = `
		INSERT INTO department (id, college_id, code, name_ar, name_en, stage_count, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		d.ID, d.CollegeID, d.Code, d.NameAr, d.NameEn, d.StageCount, d.IsActive,
	).Scan(&d.CreatedAt, &d.UpdatedAt)
	return pg.WrapQuery("reference.CreateDepartment", err)
}

// ---------------------------------------------------------------------------
// Study types
// ---------------------------------------------------------------------------

const studyTypeColumns = ` id, code, name_ar, name_en, sort_order, is_active, created_at, updated_at`

func scanStudyType(row pgx.Row) (*academic.StudyType, error) {
	var s academic.StudyType
	if err := row.Scan(
		&s.ID, &s.Code, &s.NameAr, &s.NameEn, &s.SortOrder, &s.IsActive, &s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListStudyTypes returns the modes of study in display order.
func (r *ReferenceRepository) ListStudyTypes(ctx context.Context, activeOnly bool) ([]*academic.StudyType, error) {
	const query = `
		SELECT` + studyTypeColumns + `
		FROM study_type
		WHERE NOT $1::boolean OR is_active
		ORDER BY sort_order, code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("reference.ListStudyTypes", err)
	}
	types, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.StudyType, error) {
		return scanStudyType(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("reference.ListStudyTypes", err)
	}
	return types, nil
}

// GetStudyType returns one mode of study.
func (r *ReferenceRepository) GetStudyType(ctx context.Context, id shared.ID) (*academic.StudyType, error) {
	q := r.db.Conn(ctx)
	s, err := scanStudyType(q.QueryRow(ctx, `SELECT`+studyTypeColumns+` FROM study_type WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetStudyType", err)
	}
	return s, nil
}

// GetStudyTypeByCode returns a mode of study by its stable code.
func (r *ReferenceRepository) GetStudyTypeByCode(ctx context.Context, code string) (*academic.StudyType, error) {
	q := r.db.Conn(ctx)
	s, err := scanStudyType(q.QueryRow(ctx, `SELECT`+studyTypeColumns+` FROM study_type WHERE code = $1`, code))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetStudyTypeByCode", err)
	}
	return s, nil
}

// CreateStudyType records a mode of study.
func (r *ReferenceRepository) CreateStudyType(ctx context.Context, s *academic.StudyType) error {
	if err := r.db.RequireTx(ctx, "reference.CreateStudyType"); err != nil {
		return err
	}
	const query = `
		INSERT INTO study_type (id, code, name_ar, name_en, sort_order, is_active)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		s.ID, s.Code, s.NameAr, s.NameEn, s.SortOrder, s.IsActive,
	).Scan(&s.CreatedAt, &s.UpdatedAt)
	return pg.WrapQuery("reference.CreateStudyType", err)
}

// ---------------------------------------------------------------------------
// Student categories
// ---------------------------------------------------------------------------

const studentCategoryColumns = ` id, code, name_ar, name_en, is_active, created_at, updated_at`

func scanStudentCategory(row pgx.Row) (*academic.StudentCategory, error) {
	var c academic.StudentCategory
	if err := row.Scan(&c.ID, &c.Code, &c.NameAr, &c.NameEn, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// ListStudentCategories returns the categories that feed fee resolution.
func (r *ReferenceRepository) ListStudentCategories(ctx context.Context, activeOnly bool) ([]*academic.StudentCategory, error) {
	const query = `
		SELECT` + studentCategoryColumns + `
		FROM student_category
		WHERE NOT $1::boolean OR is_active
		ORDER BY code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("reference.ListStudentCategories", err)
	}
	categories, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*academic.StudentCategory, error) {
		return scanStudentCategory(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("reference.ListStudentCategories", err)
	}
	return categories, nil
}

// GetStudentCategoryByCode returns a category by its stable code.
func (r *ReferenceRepository) GetStudentCategoryByCode(ctx context.Context, code string) (*academic.StudentCategory, error) {
	q := r.db.Conn(ctx)
	c, err := scanStudentCategory(
		q.QueryRow(ctx, `SELECT`+studentCategoryColumns+` FROM student_category WHERE code = $1`, code),
	)
	if err != nil {
		return nil, pg.WrapQuery("reference.GetStudentCategoryByCode", err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Payment methods
// ---------------------------------------------------------------------------

const paymentMethodColumns = ` id, code, name_ar, is_cash, requires_reference, is_active, sort_order`

func scanPaymentMethod(row pgx.Row) (*payment.Method, error) {
	var m payment.Method
	if err := row.Scan(
		&m.ID, &m.Code, &m.NameAr, &m.IsCash, &m.RequiresReference, &m.IsActive, &m.SortOrder,
	); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListPaymentMethods returns the ways of paying, in display order.
func (r *ReferenceRepository) ListPaymentMethods(ctx context.Context, activeOnly bool) ([]*payment.Method, error) {
	const query = `
		SELECT` + paymentMethodColumns + `
		FROM payment_method
		WHERE NOT $1::boolean OR is_active
		ORDER BY sort_order, code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("reference.ListPaymentMethods", err)
	}
	methods, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.Method, error) {
		return scanPaymentMethod(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("reference.ListPaymentMethods", err)
	}
	return methods, nil
}

// GetPaymentMethod returns one way of paying.
func (r *ReferenceRepository) GetPaymentMethod(ctx context.Context, id shared.ID) (*payment.Method, error) {
	q := r.db.Conn(ctx)
	m, err := scanPaymentMethod(q.QueryRow(ctx, `SELECT`+paymentMethodColumns+` FROM payment_method WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetPaymentMethod", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------
//
// Master data is edited in place and never deleted. A college renamed is the
// same college, and every enrollment, account and receipt that references it
// keeps referencing the right row; a college deleted would orphan a decade of
// them. Taking one out of use is what is_active does.
//
// Codes are not updatable here on purpose. They travel into receipt numbers,
// fee-policy scopes and ministry returns, and a code that changes meaning is
// worse than one that is merely ugly.

// UpdateCollege renames a college or retires it.
func (r *ReferenceRepository) UpdateCollege(ctx context.Context, c *academic.College) error {
	if err := r.db.RequireTx(ctx, "reference.UpdateCollege"); err != nil {
		return err
	}
	const query = `
		UPDATE college SET name_ar = $2, name_en = $3, is_active = $4
		WHERE id = $1 RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, c.ID, c.NameAr, c.NameEn, c.IsActive).Scan(&c.UpdatedAt)
	return pg.WrapQuery("reference.UpdateCollege", err)
}

// UpdateDepartment renames a department, changes its length or retires it.
func (r *ReferenceRepository) UpdateDepartment(ctx context.Context, d *academic.Department) error {
	if err := r.db.RequireTx(ctx, "reference.UpdateDepartment"); err != nil {
		return err
	}
	const query = `
		UPDATE department SET name_ar = $2, name_en = $3, stage_count = $4, is_active = $5
		WHERE id = $1 RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, d.ID, d.NameAr, d.NameEn, d.StageCount, d.IsActive).Scan(&d.UpdatedAt)
	return pg.WrapQuery("reference.UpdateDepartment", err)
}

// UpdateStudyType renames a study type, reorders it or retires it.
func (r *ReferenceRepository) UpdateStudyType(ctx context.Context, s *academic.StudyType) error {
	if err := r.db.RequireTx(ctx, "reference.UpdateStudyType"); err != nil {
		return err
	}
	const query = `
		UPDATE study_type SET name_ar = $2, name_en = $3, sort_order = $4, is_active = $5
		WHERE id = $1 RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, s.ID, s.NameAr, s.NameEn, s.SortOrder, s.IsActive).Scan(&s.UpdatedAt)
	return pg.WrapQuery("reference.UpdateStudyType", err)
}

// CreateStudentCategory adds a category that fee policy can resolve against.
func (r *ReferenceRepository) CreateStudentCategory(ctx context.Context, c *academic.StudentCategory) error {
	if err := r.db.RequireTx(ctx, "reference.CreateStudentCategory"); err != nil {
		return err
	}
	const query = `
		INSERT INTO student_category (id, code, name_ar, name_en, is_active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, c.ID, c.Code, c.NameAr, c.NameEn, c.IsActive).
		Scan(&c.CreatedAt, &c.UpdatedAt)
	return pg.WrapQuery("reference.CreateStudentCategory", err)
}

// GetStudentCategory returns one category.
func (r *ReferenceRepository) GetStudentCategory(ctx context.Context, id shared.ID) (*academic.StudentCategory, error) {
	q := r.db.Conn(ctx)
	c, err := scanStudentCategory(
		q.QueryRow(ctx, `SELECT`+studentCategoryColumns+` FROM student_category WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetStudentCategory", err)
	}
	return c, nil
}

// UpdateStudentCategory renames a category or retires it.
func (r *ReferenceRepository) UpdateStudentCategory(ctx context.Context, c *academic.StudentCategory) error {
	if err := r.db.RequireTx(ctx, "reference.UpdateStudentCategory"); err != nil {
		return err
	}
	const query = `
		UPDATE student_category SET name_ar = $2, name_en = $3, is_active = $4
		WHERE id = $1 RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, c.ID, c.NameAr, c.NameEn, c.IsActive).Scan(&c.UpdatedAt)
	return pg.WrapQuery("reference.UpdateStudentCategory", err)
}

// CreatePaymentMethod adds a way of paying.
func (r *ReferenceRepository) CreatePaymentMethod(ctx context.Context, m *payment.Method) error {
	if err := r.db.RequireTx(ctx, "reference.CreatePaymentMethod"); err != nil {
		return err
	}
	const query = `
		INSERT INTO payment_method (id, code, name_ar, is_cash, requires_reference, is_active, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, m.ID, m.Code, m.NameAr, m.IsCash, m.RequiresReference, m.IsActive, m.SortOrder)
	return pg.WrapQuery("reference.CreatePaymentMethod", err)
}

// UpdatePaymentMethod renames a method, changes its rules or retires it.
//
// is_cash is writable here, but the service refuses to change it once payments
// exist: the flag decides what a cashier's drawer is expected to hold, and
// flipping it retroactively would move money between "counted in the drawer"
// and "arrived at the bank" for collections that already happened.
func (r *ReferenceRepository) UpdatePaymentMethod(ctx context.Context, m *payment.Method) error {
	if err := r.db.RequireTx(ctx, "reference.UpdatePaymentMethod"); err != nil {
		return err
	}
	const query = `
		UPDATE payment_method
		SET name_ar = $2, is_cash = $3, requires_reference = $4, is_active = $5, sort_order = $6
		WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, m.ID, m.NameAr, m.IsCash, m.RequiresReference, m.IsActive, m.SortOrder).Scan(&id)
	return pg.WrapQuery("reference.UpdatePaymentMethod", err)
}

const cashierDeskColumns = ` id, code, name_ar, college_id, is_active, created_at`

func scanCashierDesk(row pgx.Row) (*payment.CashierDesk, error) {
	var d payment.CashierDesk
	if err := row.Scan(&d.ID, &d.Code, &d.NameAr, &d.CollegeID, &d.IsActive, &d.CreatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListCashierDesks returns the windows money can be taken at.
func (r *ReferenceRepository) ListCashierDesks(ctx context.Context, activeOnly bool) ([]*payment.CashierDesk, error) {
	const query = `
		SELECT` + cashierDeskColumns + `
		FROM cashier_desk
		WHERE NOT $1::boolean OR is_active
		ORDER BY code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("reference.ListCashierDesks", err)
	}
	desks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*payment.CashierDesk, error) {
		return scanCashierDesk(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("reference.ListCashierDesks", err)
	}
	return desks, nil
}

// GetCashierDesk returns one desk.
func (r *ReferenceRepository) GetCashierDesk(ctx context.Context, id shared.ID) (*payment.CashierDesk, error) {
	q := r.db.Conn(ctx)
	d, err := scanCashierDesk(q.QueryRow(ctx, `SELECT`+cashierDeskColumns+` FROM cashier_desk WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("reference.GetCashierDesk", err)
	}
	return d, nil
}

// CreateCashierDesk opens a new window.
func (r *ReferenceRepository) CreateCashierDesk(ctx context.Context, d *payment.CashierDesk) error {
	if err := r.db.RequireTx(ctx, "reference.CreateCashierDesk"); err != nil {
		return err
	}
	const query = `
		INSERT INTO cashier_desk (id, code, name_ar, college_id, is_active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, d.ID, d.Code, d.NameAr, d.CollegeID, d.IsActive).Scan(&d.CreatedAt)
	return pg.WrapQuery("reference.CreateCashierDesk", err)
}

// UpdateCashierDesk renames a desk or closes it.
//
// The code is not updatable: it is embedded in every receipt number the desk
// has issued (2025-D03-000917), and changing it would make one desk's series
// look like two.
func (r *ReferenceRepository) UpdateCashierDesk(ctx context.Context, d *payment.CashierDesk) error {
	if err := r.db.RequireTx(ctx, "reference.UpdateCashierDesk"); err != nil {
		return err
	}
	const query = `
		UPDATE cashier_desk SET name_ar = $2, college_id = $3, is_active = $4
		WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, d.ID, d.NameAr, d.CollegeID, d.IsActive).Scan(&id)
	return pg.WrapQuery("reference.UpdateCashierDesk", err)
}

// usageQueries counts the rows that depend on a piece of master data.
//
// Each query names the reference that actually matters. A study type is "in
// use" when an enrollment or a fee policy names it, not when some join happens
// to touch it, because the question being answered is "would changing this
// rewrite something that already happened".
var usageQueries = map[port.MasterDataKind]string{
	port.MasterCollege:    `SELECT count(*) FROM enrollment WHERE college_id = $1`,
	port.MasterDepartment: `SELECT count(*) FROM enrollment WHERE department_id = $1`,
	port.MasterStudyType: `
		SELECT (SELECT count(*) FROM enrollment WHERE study_type_id = $1)
		     + (SELECT count(*) FROM fee_policy_version WHERE study_type_id = $1)`,
	port.MasterStudentCategory: `
		SELECT (SELECT count(*) FROM enrollment WHERE student_category_id = $1)
		     + (SELECT count(*) FROM fee_policy_version WHERE student_category_id = $1)`,
	port.MasterPaymentMethod: `
		SELECT (SELECT count(*) FROM payment WHERE payment_method_id = $1)
		     + (SELECT count(*) FROM refund WHERE payment_method_id = $1)`,
	port.MasterCashierDesk: `
		SELECT (SELECT count(*) FROM cashier_session WHERE cashier_desk_id = $1)
		     + (SELECT count(*) FROM number_series WHERE cashier_desk_id = $1)`,
}

// UsageCount reports how many rows depend on a piece of master data.
func (r *ReferenceRepository) UsageCount(ctx context.Context, kind port.MasterDataKind, id shared.ID) (int, error) {
	query, ok := usageQueries[kind]
	if !ok {
		return 0, shared.Internal("reference.unknown_master_kind", nil,
			"no usage query is defined for %q", kind)
	}

	q := r.db.Conn(ctx)
	var count int
	if err := q.QueryRow(ctx, query, id).Scan(&count); err != nil {
		return 0, pg.WrapQuery("reference.UsageCount", err)
	}
	return count, nil
}

// HighestStageInUse returns the furthest stage any enrollment in a department
// has reached, which is the floor a stage count may be reduced to.
func (r *ReferenceRepository) HighestStageInUse(ctx context.Context, departmentID shared.ID) (int16, error) {
	const query = `SELECT COALESCE(max(stage), 0) FROM enrollment WHERE department_id = $1`

	q := r.db.Conn(ctx)
	var stage int16
	if err := q.QueryRow(ctx, query, departmentID).Scan(&stage); err != nil {
		return 0, pg.WrapQuery("reference.HighestStageInUse", err)
	}
	return stage, nil
}
