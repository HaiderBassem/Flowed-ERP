package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/discount"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// DiscountRepository stores discount configuration, grants and the
// applications frozen onto accounts.
type DiscountRepository struct{ db *pg.DB }

// NewDiscountRepository builds the discount store over a connection pool.
func NewDiscountRepository(db *pg.DB) *DiscountRepository { return &DiscountRepository{db: db} }

var _ port.DiscountRepository = (*DiscountRepository)(nil)

// ---------------------------------------------------------------------------
// Definitions
// ---------------------------------------------------------------------------

const definitionColumns = `
	id, code, name_ar, name_en, category, exclusivity_group_id,
	is_full_exemption, annual_reconfirmation, is_active,
	created_at, updated_at, created_by`

func scanDefinition(row pgx.Row) (*discount.Definition, error) {
	var d discount.Definition
	if err := row.Scan(
		&d.ID, &d.Code, &d.NameAr, &d.NameEn, &d.Category, &d.ExclusivityGroupID,
		&d.IsFullExemption, &d.AnnualReconfirmation, &d.IsActive,
		&d.CreatedAt, &d.UpdatedAt, &d.CreatedBy,
	); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateDefinition records the stable identity of a discount. It carries no
// value: every number lives on a version.
func (r *DiscountRepository) CreateDefinition(ctx context.Context, d *discount.Definition) error {
	if err := r.db.RequireTx(ctx, "discount.CreateDefinition"); err != nil {
		return err
	}
	const query = `
		INSERT INTO discount_definition (
			id, code, name_ar, name_en, category, exclusivity_group_id,
			is_full_exemption, annual_reconfirmation, is_active, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		d.ID, d.Code, d.NameAr, d.NameEn, d.Category, d.ExclusivityGroupID,
		d.IsFullExemption, d.AnnualReconfirmation, d.IsActive, d.CreatedBy,
	).Scan(&d.CreatedAt, &d.UpdatedAt)
	return pg.WrapQuery("discount.CreateDefinition", err)
}

// GetDefinition returns one discount definition.
func (r *DiscountRepository) GetDefinition(ctx context.Context, id shared.ID) (*discount.Definition, error) {
	q := r.db.Conn(ctx)
	d, err := scanDefinition(q.QueryRow(ctx, `SELECT`+definitionColumns+` FROM discount_definition WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("discount.GetDefinition", err)
	}
	return d, nil
}

// GetDefinitionByCode returns a definition by its stable code.
func (r *DiscountRepository) GetDefinitionByCode(ctx context.Context, code string) (*discount.Definition, error) {
	q := r.db.Conn(ctx)
	d, err := scanDefinition(q.QueryRow(ctx, `SELECT`+definitionColumns+` FROM discount_definition WHERE code = $1`, code))
	if err != nil {
		return nil, pg.WrapQuery("discount.GetDefinitionByCode", err)
	}
	return d, nil
}

// ListDefinitions returns the discount catalogue.
func (r *DiscountRepository) ListDefinitions(ctx context.Context, activeOnly bool) ([]*discount.Definition, error) {
	const query = `
		SELECT` + definitionColumns + `
		FROM discount_definition
		WHERE NOT $1::boolean OR is_active
		ORDER BY category, code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("discount.ListDefinitions", err)
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*discount.Definition, error) {
		return scanDefinition(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("discount.ListDefinitions", err)
	}
	return definitions, nil
}

// ---------------------------------------------------------------------------
// Definition versions
// ---------------------------------------------------------------------------

const versionColumns = `
	id, definition_id, version_no, value_type, value_bp, value_amount,
	applies_to_components, per_application_cap, stackable, priority,
	requires_approval, approval_role, required_documents,
	valid_from_year_id, valid_to_year_id, status, notes,
	created_at, created_by, published_at, published_by, retired_at`

func scanDefinitionVersion(row pgx.Row) (*discount.DefinitionVersion, error) {
	var (
		v            discount.DefinitionVersion
		valueBP      *money.BasisPoints
		valueAmount  *money.Amount
		components   []string
		documents    []string
		approvalRole *string
	)
	if err := row.Scan(
		&v.ID, &v.DefinitionID, &v.VersionNo, &v.ValueType, &valueBP, &valueAmount,
		&components, &v.PerApplicationCap, &v.Stackable, &v.Priority,
		&v.RequiresApproval, &approvalRole, &documents,
		&v.ValidFromYearID, &v.ValidToYearID, &v.Status, &v.Notes,
		&v.CreatedAt, &v.CreatedBy, &v.PublishedAt, &v.PublishedBy, &v.RetiredAt,
	); err != nil {
		return nil, err
	}
	if valueBP != nil {
		v.Rate = *valueBP
	}
	if valueAmount != nil {
		v.FixedAmount = *valueAmount
	}
	v.AppliesToComponents = components
	v.RequiredDocuments = documents
	v.ApprovalRole = enumPtr[shared.Role](approvalRole)
	return &v, nil
}

// CreateVersion records a draft configuration of a discount.
//
// The value columns are mutually exclusive by CHECK constraint: a percentage
// version stores basis points and no amount, a fixed one the reverse. Writing
// both would let a later reader pick the wrong one.
func (r *DiscountRepository) CreateVersion(ctx context.Context, v *discount.DefinitionVersion) error {
	if err := r.db.RequireTx(ctx, "discount.CreateVersion"); err != nil {
		return err
	}
	var (
		valueBP     *money.BasisPoints
		valueAmount *money.Amount
	)
	switch v.ValueType {
	case discount.ValuePercentage:
		rate := v.Rate
		valueBP = &rate
	case discount.ValueFixed:
		amount := v.FixedAmount
		valueAmount = &amount
	default:
		return shared.Validation("discount.invalid_value_type",
			"a discount version must be percentage or fixed, got %q", v.ValueType)
	}

	const query = `
		INSERT INTO discount_definition_version (
			id, definition_id, version_no, value_type, value_bp, value_amount,
			applies_to_components, per_application_cap, stackable, priority,
			requires_approval, approval_role, required_documents,
			valid_from_year_id, valid_to_year_id, status, notes, created_by
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13,
			$14, $15, $16, $17, $18
		)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		v.ID, v.DefinitionID, v.VersionNo, v.ValueType, valueBP, valueAmount,
		v.AppliesToComponents, v.PerApplicationCap, v.Stackable, v.Priority,
		v.RequiresApproval, enumValue(v.ApprovalRole), v.RequiredDocuments,
		v.ValidFromYearID, v.ValidToYearID, v.Status, v.Notes, v.CreatedBy,
	).Scan(&v.CreatedAt)
	return pg.WrapQuery("discount.CreateVersion", err)
}

// PublishVersion puts a draft version in force. Once published, a trigger
// freezes every column but the lifecycle ones, which is what lets an
// application point at it as an unchangeable record of the numbers used.
func (r *DiscountRepository) PublishVersion(ctx context.Context, versionID, actor shared.ID, at time.Time) error {
	if err := r.db.RequireTx(ctx, "discount.PublishVersion"); err != nil {
		return err
	}
	const query = `
		UPDATE discount_definition_version
		SET status = 'published', published_at = COALESCE($2, now()), published_by = $3
		WHERE id = $1 AND status = 'draft'
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, versionID, instant(at), idOrNil(actor)).Scan(&id)
	if pg.IsNotFound(err) {
		return shared.PreconditionFailed("discount.version_not_publishable",
			"discount version %s is either unknown or no longer a draft", versionID).
			WithDetail("version_id", versionID.String())
	}
	return pg.WrapQuery("discount.PublishVersion", err)
}

// GetVersion returns one definition version.
func (r *DiscountRepository) GetVersion(ctx context.Context, id shared.ID) (*discount.DefinitionVersion, error) {
	q := r.db.Conn(ctx)
	v, err := scanDefinitionVersion(
		q.QueryRow(ctx, `SELECT`+versionColumns+` FROM discount_definition_version WHERE id = $1`, id),
	)
	if err != nil {
		return nil, pg.WrapQuery("discount.GetVersion", err)
	}
	return v, nil
}

// GetPublishedVersion returns the version of a definition currently in force.
// A partial unique index guarantees there is at most one.
func (r *DiscountRepository) GetPublishedVersion(ctx context.Context, definitionID shared.ID) (*discount.DefinitionVersion, error) {
	const query = `
		SELECT` + versionColumns + `
		FROM discount_definition_version
		WHERE definition_id = $1 AND status = 'published'`

	q := r.db.Conn(ctx)
	v, err := scanDefinitionVersion(q.QueryRow(ctx, query, definitionID))
	if err != nil {
		return nil, pg.WrapQuery("discount.GetPublishedVersion", err)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Assignments
// ---------------------------------------------------------------------------

const assignmentColumns = `
	id, student_id, definition_id, scope_type,
	scope_year_from_id, scope_year_to_id, scope_year_from_code, scope_year_to_code,
	status, justification, document_refs,
	requested_by, requested_at, approved_by, approved_at, rejection_reason,
	revoked_by, revoked_at, revocation_reason, revocation_effect,
	created_at, updated_at`

func scanAssignment(row pgx.Row) (*discount.Assignment, error) {
	var (
		a         discount.Assignment
		documents []string
		effect    *string
	)
	if err := row.Scan(
		&a.ID, &a.StudentID, &a.DefinitionID, &a.ScopeType,
		&a.ScopeYearFrom, &a.ScopeYearTo, &a.ScopeCodeFrom, &a.ScopeCodeTo,
		&a.Status, &a.Justification, &documents,
		&a.RequestedBy, &a.RequestedAt, &a.ApprovedBy, &a.ApprovedAt, &a.RejectionReason,
		&a.RevokedBy, &a.RevokedAt, &a.RevocationReason, &effect,
		&a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.DocumentRefs = documents
	a.RevocationEffect = enumPtr[discount.RevocationEffect](effect)
	return &a, nil
}

// CreateAssignment records a grant to a student.
func (r *DiscountRepository) CreateAssignment(ctx context.Context, a *discount.Assignment) error {
	if err := r.db.RequireTx(ctx, "discount.CreateAssignment"); err != nil {
		return err
	}
	const query = `
		INSERT INTO discount_assignment (
			id, student_id, definition_id, scope_type,
			scope_year_from_id, scope_year_to_id, scope_year_from_code, scope_year_to_code,
			status, justification, document_refs,
			requested_by, requested_at, approved_by, approved_at, rejection_reason,
			revoked_by, revoked_at, revocation_reason, revocation_effect
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11,
			$12, COALESCE($13, now()), $14, $15, $16,
			$17, $18, $19, $20
		)
		RETURNING requested_at, created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		a.ID, a.StudentID, a.DefinitionID, a.ScopeType,
		a.ScopeYearFrom, a.ScopeYearTo, a.ScopeCodeFrom, a.ScopeCodeTo,
		a.Status, a.Justification, a.DocumentRefs,
		a.RequestedBy, instant(a.RequestedAt), a.ApprovedBy, a.ApprovedAt, a.RejectionReason,
		a.RevokedBy, a.RevokedAt, a.RevocationReason, enumValue(a.RevocationEffect),
	).Scan(&a.RequestedAt, &a.CreatedAt, &a.UpdatedAt)
	return pg.WrapQuery("discount.CreateAssignment", err)
}

// UpdateAssignment writes back a grant whose lifecycle moved.
func (r *DiscountRepository) UpdateAssignment(ctx context.Context, a *discount.Assignment) error {
	if err := r.db.RequireTx(ctx, "discount.UpdateAssignment"); err != nil {
		return err
	}
	const query = `
		UPDATE discount_assignment SET
			scope_type           = $2,
			scope_year_from_id   = $3,
			scope_year_to_id     = $4,
			scope_year_from_code = $5,
			scope_year_to_code   = $6,
			status               = $7,
			justification        = $8,
			document_refs        = $9,
			approved_by          = $10,
			approved_at          = $11,
			rejection_reason     = $12,
			revoked_by           = $13,
			revoked_at           = $14,
			revocation_reason    = $15,
			revocation_effect    = $16
		WHERE id = $1
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		a.ID, a.ScopeType, a.ScopeYearFrom, a.ScopeYearTo, a.ScopeCodeFrom, a.ScopeCodeTo,
		a.Status, a.Justification, a.DocumentRefs,
		a.ApprovedBy, a.ApprovedAt, a.RejectionReason,
		a.RevokedBy, a.RevokedAt, a.RevocationReason, enumValue(a.RevocationEffect),
	).Scan(&a.UpdatedAt)
	return pg.WrapQuery("discount.UpdateAssignment", err)
}

// GetAssignment returns one grant.
func (r *DiscountRepository) GetAssignment(ctx context.Context, id shared.ID) (*discount.Assignment, error) {
	q := r.db.Conn(ctx)
	a, err := scanAssignment(q.QueryRow(ctx, `SELECT`+assignmentColumns+` FROM discount_assignment WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("discount.GetAssignment", err)
	}
	return a, nil
}

// ListAssignmentsForStudent returns every grant a student holds.
func (r *DiscountRepository) ListAssignmentsForStudent(ctx context.Context, studentID shared.ID) ([]*discount.Assignment, error) {
	const query = `
		SELECT` + assignmentColumns + `
		FROM discount_assignment
		WHERE student_id = $1
		ORDER BY requested_at DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("discount.ListAssignmentsForStudent", err)
	}
	assignments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*discount.Assignment, error) {
		return scanAssignment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("discount.ListAssignmentsForStudent", err)
	}
	return assignments, nil
}

// ApprovedAssignmentsCovering returns the grants whose scope reaches a year.
//
// The comparison runs on the denormalised year codes rather than through a
// join, which is safe because every code has the form 2025-2026 and therefore
// sorts chronologically as text. The predicate mirrors Assignment.CoversYear
// exactly; the two must agree or a grant would materialise in memory and not
// in the query, or the reverse.
func (r *DiscountRepository) ApprovedAssignmentsCovering(
	ctx context.Context, studentID shared.ID, yearCode string,
) ([]*discount.Assignment, error) {
	const query = `
		SELECT` + assignmentColumns + `
		FROM discount_assignment
		WHERE student_id = $1
		  AND status = 'approved'
		  AND (
		        scope_type = 'all_years'
		     OR (scope_type = 'single_year' AND scope_year_from_code = $2)
		     OR (scope_type = 'year_range'
		         AND scope_year_from_code <= $2
		         AND scope_year_to_code   >= $2)
		  )
		ORDER BY created_at`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID, yearCode)
	if err != nil {
		return nil, pg.WrapQuery("discount.ApprovedAssignmentsCovering", err)
	}
	assignments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*discount.Assignment, error) {
		return scanAssignment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("discount.ApprovedAssignmentsCovering", err)
	}
	return assignments, nil
}

// HasOverlappingAssignment reports whether a live grant of the same discount
// already covers any of the requested years.
//
// The test is ordinary interval overlap on the year codes. An empty bound is
// open-ended on that side, and an existing all-years grant overlaps every
// request. Terminal grants — rejected, revoked, expired, cancelled — are
// ignored, so revoking a grant genuinely frees the years it held.
func (r *DiscountRepository) HasOverlappingAssignment(
	ctx context.Context, studentID, definitionID shared.ID, fromCode, toCode string, excluding *shared.ID,
) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1
			FROM discount_assignment
			WHERE student_id = $1
			  AND definition_id = $2
			  AND status IN ('draft', 'submitted', 'approved')
			  AND ($5::uuid IS NULL OR id <> $5)
			  AND (
			        scope_type = 'all_years'
			     OR (
			            ($3 = '' OR COALESCE(scope_year_to_code, scope_year_from_code) >= $3)
			        AND ($4 = '' OR scope_year_from_code <= $4)
			        )
			  )
		)`

	q := r.db.Conn(ctx)
	var overlaps bool
	if err := q.QueryRow(ctx, query, studentID, definitionID, fromCode, toCode, excluding).Scan(&overlaps); err != nil {
		return false, pg.WrapQuery("discount.HasOverlappingAssignment", err)
	}
	return overlaps, nil
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

const applicationColumns = `
	id, account_id, assignment_id, definition_version_id,
	frozen_base_amount, computed_amount, applied_amount, truncation_reason,
	application_sequence, status, applied_at, applied_by,
	reversed_at, reversed_by, reversal_reason, created_at`

func scanApplication(row pgx.Row) (*discount.Application, error) {
	var (
		a          discount.Application
		truncation *string
	)
	if err := row.Scan(
		&a.ID, &a.AccountID, &a.AssignmentID, &a.DefinitionVersionID,
		&a.FrozenBase, &a.ComputedAmount, &a.AppliedAmount, &truncation,
		&a.Sequence, &a.Status, &a.AppliedAt, &a.AppliedBy,
		&a.ReversedAt, &a.ReversedBy, &a.ReversalReason, &a.CreatedAt,
	); err != nil {
		return nil, err
	}
	a.TruncationReason = enumPtr[discount.TruncationReason](truncation)
	return &a, nil
}

// CreateApplication materialises a discount onto an account with its amount
// frozen at the moment it was computed.
func (r *DiscountRepository) CreateApplication(ctx context.Context, a *discount.Application) error {
	if err := r.db.RequireTx(ctx, "discount.CreateApplication"); err != nil {
		return err
	}
	const query = `
		INSERT INTO discount_application (
			id, account_id, assignment_id, definition_version_id,
			frozen_base_amount, computed_amount, applied_amount, truncation_reason,
			application_sequence, status, applied_at, applied_by,
			reversed_at, reversed_by, reversal_reason
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15
		)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		a.ID, a.AccountID, a.AssignmentID, a.DefinitionVersionID,
		a.FrozenBase, a.ComputedAmount, a.AppliedAmount, enumValue(a.TruncationReason),
		a.Sequence, a.Status, a.AppliedAt, a.AppliedBy,
		a.ReversedAt, a.ReversedBy, a.ReversalReason,
	).Scan(&a.CreatedAt)
	return pg.WrapQuery("discount.CreateApplication", err)
}

// UpdateApplication moves an application through its lifecycle.
//
// Only the lifecycle columns are written. The frozen base, the computed amount
// and the version the calculation came from are immutable by trigger, because
// an account has to remain reconstructable from these rows alone.
func (r *DiscountRepository) UpdateApplication(ctx context.Context, a *discount.Application) error {
	if err := r.db.RequireTx(ctx, "discount.UpdateApplication"); err != nil {
		return err
	}
	const query = `
		UPDATE discount_application SET
			status          = $2,
			applied_at      = $3,
			applied_by      = $4,
			reversed_at     = $5,
			reversed_by     = $6,
			reversal_reason = $7
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query,
		a.ID, a.Status, a.AppliedAt, a.AppliedBy, a.ReversedAt, a.ReversedBy, a.ReversalReason,
	).Scan(&id)
	return pg.WrapQuery("discount.UpdateApplication", err)
}

// GetApplication returns one materialised discount.
func (r *DiscountRepository) GetApplication(ctx context.Context, id shared.ID) (*discount.Application, error) {
	q := r.db.Conn(ctx)
	a, err := scanApplication(q.QueryRow(ctx, `SELECT`+applicationColumns+` FROM discount_application WHERE id = $1`, id))
	if err != nil {
		return nil, pg.WrapQuery("discount.GetApplication", err)
	}
	return a, nil
}

// ListApplications returns every discount materialised onto an account,
// including the reversed ones, in the order they were computed.
func (r *DiscountRepository) ListApplications(ctx context.Context, accountID shared.ID) ([]*discount.Application, error) {
	const query = `
		SELECT` + applicationColumns + `
		FROM discount_application
		WHERE account_id = $1
		ORDER BY application_sequence, created_at`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, accountID)
	if err != nil {
		return nil, pg.WrapQuery("discount.ListApplications", err)
	}
	applications, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*discount.Application, error) {
		return scanApplication(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("discount.ListApplications", err)
	}
	return applications, nil
}
