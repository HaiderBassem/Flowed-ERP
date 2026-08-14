package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// ---------------------------------------------------------------------------
// Fee policies
// ---------------------------------------------------------------------------

// FeePolicyRepository stores priced scopes and their components.
type FeePolicyRepository struct{ db *pg.DB }

// NewFeePolicyRepository builds the fee policy store over a connection pool.
func NewFeePolicyRepository(db *pg.DB) *FeePolicyRepository { return &FeePolicyRepository{db: db} }

var _ port.FeePolicyRepository = (*FeePolicyRepository)(nil)

const feePolicyColumns = `
	id, policy_code, version_no, academic_year_id,
	college_id, department_id, stage, study_type_id, student_category_id,
	specificity_score, status, max_discount_bp, effective_from, description,
	published_at, published_by, retired_at, created_at, updated_at, created_by`

func scanFeePolicy(row pgx.Row) (*billing.FeePolicy, error) {
	var (
		p             billing.FeePolicy
		effectiveFrom *time.Time
	)
	if err := row.Scan(
		&p.ID, &p.PolicyCode, &p.VersionNo, &p.AcademicYearID,
		&p.CollegeID, &p.DepartmentID, &p.Stage, &p.StudyTypeID, &p.StudentCategoryID,
		&p.SpecificityScore, &p.Status, &p.MaxDiscountBP, &effectiveFrom, &p.Description,
		&p.PublishedAt, &p.PublishedBy, &p.RetiredAt, &p.CreatedAt, &p.UpdatedAt, &p.CreatedBy,
	); err != nil {
		return nil, err
	}
	p.EffectiveFrom = dateOrNil(effectiveFrom)
	return &p, nil
}

// Create records a draft policy together with its components.
//
// specificity_score is a generated column: the database derives it from the
// scope so the two cannot disagree, and it is read back rather than written.
func (r *FeePolicyRepository) Create(ctx context.Context, p *billing.FeePolicy) error {
	if err := r.db.RequireTx(ctx, "fee_policy.Create"); err != nil {
		return err
	}
	const insertPolicy = `
		INSERT INTO fee_policy_version (
			id, policy_code, version_no, academic_year_id,
			college_id, department_id, stage, study_type_id, student_category_id,
			status, max_discount_bp, effective_from, description,
			published_at, published_by, retired_at, created_by
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9,
			$10, $11, $12, $13,
			$14, $15, $16, $17
		)
		RETURNING specificity_score, created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, insertPolicy,
		p.ID, p.PolicyCode, p.VersionNo, p.AcademicYearID,
		p.CollegeID, p.DepartmentID, p.Stage, p.StudyTypeID, p.StudentCategoryID,
		p.Status, p.MaxDiscountBP, timeOrNil(p.EffectiveFrom), p.Description,
		p.PublishedAt, p.PublishedBy, p.RetiredAt, p.CreatedBy,
	).Scan(&p.SpecificityScore, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return pg.WrapQuery("fee_policy.Create", err)
	}

	if len(p.Components) == 0 {
		return nil
	}

	const insertComponent = `
		INSERT INTO fee_component (
			id, fee_policy_id, component_code, name_ar, name_en,
			amount, is_discountable, is_refundable, is_mandatory, sort_order
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	batch := &pgx.Batch{}
	for _, c := range p.Components {
		if shared.IsNil(c.ID) {
			c.ID = shared.NewID()
		}
		c.FeePolicyID = p.ID
		batch.Queue(insertComponent,
			c.ID, c.FeePolicyID, c.Code, c.NameAr, c.NameEn,
			c.Amount, c.IsDiscountable, c.IsRefundable, c.IsMandatory, c.SortOrder,
		)
	}
	return pg.WrapQuery("fee_policy.Create.components", execBatch(ctx, q, batch))
}

// Publish makes a draft policy resolvable and freezes its amounts.
func (r *FeePolicyRepository) Publish(ctx context.Context, policyID shared.ID, actor shared.ID, at time.Time) error {
	if err := r.db.RequireTx(ctx, "fee_policy.Publish"); err != nil {
		return err
	}
	const query = `
		UPDATE fee_policy_version
		SET status = 'published', published_at = COALESCE($2, now()), published_by = $3
		WHERE id = $1 AND status = 'draft'
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, policyID, instant(at), idOrNil(actor)).Scan(&id)
	if pg.IsNotFound(err) {
		return shared.PreconditionFailed("fee_policy.not_publishable",
			"fee policy %s is either unknown or no longer a draft", policyID).
			WithDetail("fee_policy_id", policyID.String())
	}
	return pg.WrapQuery("fee_policy.Publish", err)
}

// GetByID returns one policy with its components.
func (r *FeePolicyRepository) GetByID(ctx context.Context, id shared.ID) (*billing.FeePolicy, error) {
	q := r.db.Conn(ctx)
	p, err := scanFeePolicy(q.QueryRow(ctx, `SELECT`+feePolicyColumns+` FROM fee_policy_version WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("fee_policy.GetByID", err)
	}
	if err := r.attachComponents(ctx, []*billing.FeePolicy{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// List returns every policy defined for a year, with components.
func (r *FeePolicyRepository) List(ctx context.Context, yearID shared.ID) ([]*billing.FeePolicy, error) {
	const query = `
		SELECT` + feePolicyColumns + `
		FROM fee_policy_version
		WHERE academic_year_id = $1
		ORDER BY specificity_score DESC, policy_code, version_no`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, yearID)
	if err != nil {
		return nil, pg.WrapQuery("fee_policy.List", err)
	}
	policies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.FeePolicy, error) {
		return scanFeePolicy(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("fee_policy.List", err)
	}
	if err := r.attachComponents(ctx, policies); err != nil {
		return nil, err
	}
	return policies, nil
}

// Resolve returns the single published policy that best fits a scope.
//
// A NULL dimension on a policy is a wildcard, so every candidate matches the
// request on each dimension it names. The highest specificity score wins, and
// because the weights are powers of two no two distinct scopes can tie.
//
// Finding nothing is an error, never a zero-fee default: an enrollment nobody
// priced must stop the operation and reach a human, not quietly bill nothing.
func (r *FeePolicyRepository) Resolve(ctx context.Context, scope port.FeeScope) (*billing.FeePolicy, error) {
	const query = `
		SELECT` + feePolicyColumns + `
		FROM fee_policy_version
		WHERE status = 'published'
		  AND academic_year_id = $1
		  AND (college_id          IS NULL OR college_id          = $2)
		  AND (department_id       IS NULL OR department_id       = $3)
		  AND (stage               IS NULL OR stage               = $4)
		  AND (study_type_id       IS NULL OR study_type_id       = $5)
		  AND (student_category_id IS NULL OR student_category_id = $6)
		ORDER BY specificity_score DESC
		LIMIT 1`

	q := r.db.Conn(ctx)
	p, err := scanFeePolicy(q.QueryRow(ctx, query,
		scope.AcademicYearID, scope.CollegeID, scope.DepartmentID,
		scope.Stage, scope.StudyTypeID, scope.StudentCategoryID,
	))
	if pg.IsNotFound(err) {
		return nil, shared.NotFound("fee_policy.unresolved",
			"no published fee policy covers this enrollment; it cannot be priced").
			WithDetail("academic_year_id", scope.AcademicYearID.String()).
			WithDetail("college_id", scope.CollegeID.String()).
			WithDetail("department_id", scope.DepartmentID.String()).
			WithDetail("stage", scope.Stage).
			WithDetail("study_type_id", scope.StudyTypeID.String()).
			WithDetail("student_category_id", scope.StudentCategoryID.String()).
			WithDetail("remedy", "publish a fee policy covering this scope before generating the account").
			WithCause(err)
	}
	if err != nil {
		return nil, pg.WrapQuery("fee_policy.Resolve", err)
	}
	if err := r.attachComponents(ctx, []*billing.FeePolicy{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// attachComponents loads the components of every supplied policy in one query,
// so listing a year's policies does not fan out into one query per row.
func (r *FeePolicyRepository) attachComponents(ctx context.Context, policies []*billing.FeePolicy) error {
	if len(policies) == 0 {
		return nil
	}
	byID := make(map[shared.ID]*billing.FeePolicy, len(policies))
	ids := make([]shared.ID, 0, len(policies))
	for _, p := range policies {
		byID[p.ID] = p
		ids = append(ids, p.ID)
	}

	const query = `
		SELECT id, fee_policy_id, component_code, name_ar, name_en,
		       amount, is_discountable, is_refundable, is_mandatory, sort_order
		FROM fee_component
		WHERE fee_policy_id = ANY($1)
		ORDER BY fee_policy_id, sort_order, component_code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, ids)
	if err != nil {
		return pg.WrapQuery("fee_policy.components", err)
	}
	components, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.FeeComponent, error) {
		var c billing.FeeComponent
		if err := row.Scan(
			&c.ID, &c.FeePolicyID, &c.Code, &c.NameAr, &c.NameEn,
			&c.Amount, &c.IsDiscountable, &c.IsRefundable, &c.IsMandatory, &c.SortOrder,
		); err != nil {
			return nil, err
		}
		return &c, nil
	})
	if err != nil {
		return pg.WrapQuery("fee_policy.components", err)
	}
	for _, c := range components {
		if p, ok := byID[c.FeePolicyID]; ok {
			p.Components = append(p.Components, c)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Installment templates
// ---------------------------------------------------------------------------

// InstallmentTemplateRepository stores reusable plan shapes.
type InstallmentTemplateRepository struct{ db *pg.DB }

// NewInstallmentTemplateRepository builds the template store over a connection pool.
func NewInstallmentTemplateRepository(db *pg.DB) *InstallmentTemplateRepository {
	return &InstallmentTemplateRepository{db: db}
}

var _ port.InstallmentTemplateRepository = (*InstallmentTemplateRepository)(nil)

const templateColumns = `
	id, code, name_ar, name_en,
	academic_year_id, college_id, department_id, stage, study_type_id,
	specificity_score, max_installments, status,
	published_at, published_by, created_at, updated_at`

func scanTemplate(row pgx.Row) (*billing.InstallmentTemplate, error) {
	var t billing.InstallmentTemplate
	if err := row.Scan(
		&t.ID, &t.Code, &t.NameAr, &t.NameEn,
		&t.AcademicYearID, &t.CollegeID, &t.DepartmentID, &t.Stage, &t.StudyTypeID,
		&t.SpecificityScore, &t.MaxInstallments, &t.Status,
		&t.PublishedAt, &t.PublishedBy, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &t, nil
}

// Create records a draft template together with its lines.
func (r *InstallmentTemplateRepository) Create(ctx context.Context, t *billing.InstallmentTemplate) error {
	if err := r.db.RequireTx(ctx, "installment_template.Create"); err != nil {
		return err
	}
	const insertTemplate = `
		INSERT INTO installment_template (
			id, code, name_ar, name_en,
			academic_year_id, college_id, department_id, stage, study_type_id,
			max_installments, status, published_at, published_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING specificity_score, created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, insertTemplate,
		t.ID, t.Code, t.NameAr, t.NameEn,
		t.AcademicYearID, t.CollegeID, t.DepartmentID, t.Stage, t.StudyTypeID,
		t.MaxInstallments, t.Status, t.PublishedAt, t.PublishedBy,
	).Scan(&t.SpecificityScore, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return pg.WrapQuery("installment_template.Create", err)
	}

	if len(t.Lines) == 0 {
		return nil
	}

	const insertLine = `
		INSERT INTO installment_template_line (id, template_id, line_no, share_bp, due_offset_days, label_ar)
		VALUES ($1, $2, $3, $4, $5, $6)`

	batch := &pgx.Batch{}
	for _, line := range t.Lines {
		batch.Queue(insertLine, shared.NewID(), t.ID, line.LineNo, line.ShareBP, line.DueOffsetDays, line.Label)
	}
	return pg.WrapQuery("installment_template.Create.lines", execBatch(ctx, q, batch))
}

// Publish makes a draft template resolvable.
func (r *InstallmentTemplateRepository) Publish(ctx context.Context, templateID shared.ID, actor shared.ID, at time.Time) error {
	if err := r.db.RequireTx(ctx, "installment_template.Publish"); err != nil {
		return err
	}
	const query = `
		UPDATE installment_template
		SET status = 'published', published_at = COALESCE($2, now()), published_by = $3
		WHERE id = $1 AND status = 'draft'
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, templateID, instant(at), idOrNil(actor)).Scan(&id)
	if pg.IsNotFound(err) {
		return shared.PreconditionFailed("installment_template.not_publishable",
			"installment template %s is either unknown or no longer a draft", templateID).
			WithDetail("template_id", templateID.String())
	}
	return pg.WrapQuery("installment_template.Publish", err)
}

// GetByID returns one template with its lines.
func (r *InstallmentTemplateRepository) GetByID(ctx context.Context, id shared.ID) (*billing.InstallmentTemplate, error) {
	q := r.db.Conn(ctx)
	t, err := scanTemplate(q.QueryRow(ctx, `SELECT`+templateColumns+` FROM installment_template WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("installment_template.GetByID", err)
	}
	if err := r.attachLines(ctx, []*billing.InstallmentTemplate{t}); err != nil {
		return nil, err
	}
	return t, nil
}

// List returns the templates applicable to a year, with their lines. A
// template with no year is global and appears for every year; passing nil
// lists everything.
func (r *InstallmentTemplateRepository) List(ctx context.Context, yearID *shared.ID) ([]*billing.InstallmentTemplate, error) {
	const query = `
		SELECT` + templateColumns + `
		FROM installment_template
		WHERE $1::uuid IS NULL OR academic_year_id IS NULL OR academic_year_id = $1
		ORDER BY specificity_score DESC, code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, yearID)
	if err != nil {
		return nil, pg.WrapQuery("installment_template.List", err)
	}
	templates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.InstallmentTemplate, error) {
		return scanTemplate(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("installment_template.List", err)
	}
	if err := r.attachLines(ctx, templates); err != nil {
		return nil, err
	}
	return templates, nil
}

// Resolve returns the published template that best fits a scope.
//
// Unlike a fee policy, a template's academic year is itself a wildcard
// dimension, which is what lets one three-installment shape serve every year
// until somebody overrides it. Finding nothing is an error rather than an
// implicit "one installment due today".
func (r *InstallmentTemplateRepository) Resolve(ctx context.Context, scope port.FeeScope) (*billing.InstallmentTemplate, error) {
	const query = `
		SELECT` + templateColumns + `
		FROM installment_template
		WHERE status = 'published'
		  AND (academic_year_id IS NULL OR academic_year_id = $1)
		  AND (college_id       IS NULL OR college_id       = $2)
		  AND (department_id    IS NULL OR department_id    = $3)
		  AND (stage            IS NULL OR stage            = $4)
		  AND (study_type_id    IS NULL OR study_type_id    = $5)
		ORDER BY specificity_score DESC
		LIMIT 1`

	q := r.db.Conn(ctx)
	t, err := scanTemplate(q.QueryRow(ctx, query,
		scope.AcademicYearID, scope.CollegeID, scope.DepartmentID, scope.Stage, scope.StudyTypeID,
	))
	if pg.IsNotFound(err) {
		return nil, shared.NotFound("installment_template.unresolved",
			"no published installment template covers this enrollment; its plan cannot be shaped").
			WithDetail("academic_year_id", scope.AcademicYearID.String()).
			WithDetail("college_id", scope.CollegeID.String()).
			WithDetail("department_id", scope.DepartmentID.String()).
			WithDetail("stage", scope.Stage).
			WithDetail("study_type_id", scope.StudyTypeID.String()).
			WithDetail("remedy", "publish an installment template covering this scope, or a global one").
			WithCause(err)
	}
	if err != nil {
		return nil, pg.WrapQuery("installment_template.Resolve", err)
	}
	if err := r.attachLines(ctx, []*billing.InstallmentTemplate{t}); err != nil {
		return nil, err
	}
	return t, nil
}

// attachLines loads the lines of every supplied template in one query.
func (r *InstallmentTemplateRepository) attachLines(ctx context.Context, templates []*billing.InstallmentTemplate) error {
	if len(templates) == 0 {
		return nil
	}
	byID := make(map[shared.ID]*billing.InstallmentTemplate, len(templates))
	ids := make([]shared.ID, 0, len(templates))
	for _, t := range templates {
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}

	const query = `
		SELECT template_id, line_no, share_bp, due_offset_days, label_ar
		FROM installment_template_line
		WHERE template_id = ANY($1)
		ORDER BY template_id, line_no`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, ids)
	if err != nil {
		return pg.WrapQuery("installment_template.lines", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			templateID shared.ID
			line       billing.TemplateLine
		)
		if err := rows.Scan(&templateID, &line.LineNo, &line.ShareBP, &line.DueOffsetDays, &line.Label); err != nil {
			return pg.WrapQuery("installment_template.lines", err)
		}
		if t, ok := byID[templateID]; ok {
			t.Lines = append(t.Lines, line)
		}
	}
	return pg.WrapQuery("installment_template.lines", rows.Err())
}
