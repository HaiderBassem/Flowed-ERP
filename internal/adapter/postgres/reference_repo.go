package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
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
