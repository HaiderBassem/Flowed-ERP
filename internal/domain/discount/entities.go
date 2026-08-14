package discount

import (
	"strings"
	"time"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// Category groups discounts for reporting.
type Category string

const (
	CategorySocial    Category = "social"
	CategoryStaff     Category = "staff"
	CategoryMerit     Category = "merit"
	CategoryExemption Category = "exemption"
	CategorySibling   Category = "sibling"
	CategoryMartyr    Category = "martyr"
	CategoryOther     Category = "other"
)

// Definition is the stable identity of a discount. It deliberately holds no
// value: every number lives on a version, so a rate change is a new row rather
// than an edit that would reach backwards into computed history.
type Definition struct {
	ID                 shared.ID
	Code               string
	NameAr             string
	NameEn             *string
	Category           Category
	ExclusivityGroupID *shared.ID
	IsFullExemption    bool
	// AnnualReconfirmation decides whether eligibility must be re-verified
	// each year before the discount takes effect. A staff benefit runs on; a
	// hardship discount does not, because the hardship may have ended. This is
	// what makes an all-years grant safe.
	AnnualReconfirmation bool
	IsActive             bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
	CreatedBy            *shared.ID
}

// NewDefinition builds a discount definition.
func NewDefinition(code, nameAr string, category Category) (*Definition, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, shared.Validation("discount.code_required", "a discount code is required")
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("discount.name_required", "the Arabic name is required")
	}
	return &Definition{
		ID:                   shared.NewID(),
		Code:                 code,
		NameAr:               strings.TrimSpace(nameAr),
		Category:             category,
		AnnualReconfirmation: true,
		IsActive:             true,
	}, nil
}

// VersionStatus is the lifecycle of a definition version.
type VersionStatus string

const (
	VersionDraft     VersionStatus = "draft"
	VersionPublished VersionStatus = "published"
	VersionRetired   VersionStatus = "retired"
)

// DefinitionVersion is one frozen configuration of a discount.
//
// Once published it cannot be edited: the database refuses any change but the
// lifecycle columns. That immutability is the whole historical-integrity
// guarantee, because every application points at one of these rows.
type DefinitionVersion struct {
	ID           shared.ID
	DefinitionID shared.ID
	VersionNo    int32

	ValueType   ValueType
	Rate        money.BasisPoints
	FixedAmount money.Amount

	AppliesToComponents []string
	PerApplicationCap   *money.Amount
	Stackable           bool
	Priority            int16

	RequiresApproval  bool
	ApprovalRole      *shared.Role
	RequiredDocuments []string

	ValidFromYearID *shared.ID
	ValidToYearID   *shared.ID

	Status VersionStatus
	Notes  *string

	CreatedAt   time.Time
	CreatedBy   *shared.ID
	PublishedAt *time.Time
	PublishedBy *shared.ID
	RetiredAt   *time.Time
}

// NewPercentageVersion builds a rate-based version.
func NewPercentageVersion(definitionID shared.ID, versionNo int32, rate money.BasisPoints) (*DefinitionVersion, error) {
	if _, err := money.NewBasisPoints(rate.Int32()); err != nil {
		return nil, err
	}
	return &DefinitionVersion{
		ID:               shared.NewID(),
		DefinitionID:     definitionID,
		VersionNo:        versionNo,
		ValueType:        ValuePercentage,
		Rate:             rate,
		Stackable:        true,
		Priority:         100,
		RequiresApproval: true,
		Status:           VersionDraft,
	}, nil
}

// NewFixedVersion builds an amount-based version.
func NewFixedVersion(definitionID shared.ID, versionNo int32, amount money.Amount) (*DefinitionVersion, error) {
	if !amount.IsPositive() {
		return nil, shared.Validation("discount.non_positive_value",
			"a fixed discount must be greater than zero, got %s", amount)
	}
	return &DefinitionVersion{
		ID:               shared.NewID(),
		DefinitionID:     definitionID,
		VersionNo:        versionNo,
		ValueType:        ValueFixed,
		FixedAmount:      amount,
		Stackable:        true,
		Priority:         100,
		RequiresApproval: true,
		Status:           VersionDraft,
	}, nil
}

// Publish freezes the version.
func (v *DefinitionVersion) Publish(actor shared.ID, at time.Time) error {
	if v.Status != VersionDraft {
		return shared.PreconditionFailed("discount.version_not_draft",
			"only a draft version can be published; this one is %s", v.Status)
	}
	v.Status = VersionPublished
	v.PublishedAt = &at
	v.PublishedBy = &actor
	return nil
}

// ScopeType says which years a grant reaches.
type ScopeType string

const (
	// ScopeSingleYear covers exactly one academic year.
	ScopeSingleYear ScopeType = "single_year"
	// ScopeYearRange covers a closed range of years.
	ScopeYearRange ScopeType = "year_range"
	// ScopeAllYears covers every year, present and future.
	ScopeAllYears ScopeType = "all_years"
)

// AssignmentStatus is the lifecycle of a grant.
type AssignmentStatus string

const (
	AssignmentDraft     AssignmentStatus = "draft"
	AssignmentSubmitted AssignmentStatus = "submitted"
	AssignmentApproved  AssignmentStatus = "approved"
	AssignmentRejected  AssignmentStatus = "rejected"
	AssignmentRevoked   AssignmentStatus = "revoked"
	AssignmentExpired   AssignmentStatus = "expired"
	AssignmentCancelled AssignmentStatus = "cancelled"
)

// RevocationEffect decides what revoking does to the year already in progress.
type RevocationEffect string

const (
	// RevokeProspectiveOnly leaves the current year's application standing and
	// stops future ones. The default, because a student who budgeted around a
	// discount should not find it withdrawn mid-year.
	RevokeProspectiveOnly RevocationEffect = "prospective_only"
	// RevokeIncludeCurrentYear also reverses this year's application through
	// an audited adjustment.
	RevokeIncludeCurrentYear RevocationEffect = "include_current_year"
)

// Assignment is a discount granted to a student for a span of years.
//
// It attaches to the student, not to an enrollment, because eligibility is a
// fact about the person. It never reaches into an account that already exists:
// a grant approved in December materialises on next year's account, and
// applying it to the current one is a separate, approved adjustment.
type Assignment struct {
	ID           shared.ID
	StudentID    shared.ID
	DefinitionID shared.ID

	ScopeType     ScopeType
	ScopeYearFrom *shared.ID
	ScopeYearTo   *shared.ID
	ScopeCodeFrom *string
	ScopeCodeTo   *string

	Status        AssignmentStatus
	Justification *string
	DocumentRefs  []string

	RequestedBy      *shared.ID
	RequestedAt      time.Time
	ApprovedBy       *shared.ID
	ApprovedAt       *time.Time
	RejectionReason  *string
	RevokedBy        *shared.ID
	RevokedAt        *time.Time
	RevocationReason *string
	RevocationEffect *RevocationEffect

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAssignment builds a draft grant.
func NewAssignment(studentID, definitionID shared.ID, scope ScopeType, requestedBy shared.ID) (*Assignment, error) {
	switch scope {
	case ScopeSingleYear, ScopeYearRange, ScopeAllYears:
	default:
		return nil, shared.Validation("discount.invalid_scope",
			"scope must be single_year, year_range or all_years, got %q", scope)
	}
	return &Assignment{
		ID:           shared.NewID(),
		StudentID:    studentID,
		DefinitionID: definitionID,
		ScopeType:    scope,
		Status:       AssignmentDraft,
		RequestedBy:  &requestedBy,
		RequestedAt:  time.Now().UTC(),
	}, nil
}

// CoversYear reports whether the grant's scope includes an academic year,
// compared on the year code so the test is a string comparison rather than a
// join. Codes sort correctly because they are all of the form 2025-2026.
func (a *Assignment) CoversYear(yearCode string) bool {
	if a.Status != AssignmentApproved {
		return false
	}
	switch a.ScopeType {
	case ScopeAllYears:
		return true
	case ScopeSingleYear:
		return a.ScopeCodeFrom != nil && *a.ScopeCodeFrom == yearCode
	case ScopeYearRange:
		if a.ScopeCodeFrom == nil || a.ScopeCodeTo == nil {
			return false
		}
		return yearCode >= *a.ScopeCodeFrom && yearCode <= *a.ScopeCodeTo
	default:
		return false
	}
}

// Submit sends a draft grant for approval.
func (a *Assignment) Submit() error {
	if a.Status != AssignmentDraft {
		return shared.PreconditionFailed("discount.assignment_not_draft",
			"only a draft grant can be submitted; this one is %s", a.Status)
	}
	a.Status = AssignmentSubmitted
	return nil
}

// Approve accepts a submitted grant.
//
// The approver may not be the person who requested it. This is the four-eyes
// rule, and it is checked here, in the permission layer, and by a database
// constraint — a discount is money leaving the university, and one person
// should not be able to grant it alone.
func (a *Assignment) Approve(approver shared.ID, at time.Time) error {
	if a.Status != AssignmentSubmitted {
		return shared.PreconditionFailed("discount.assignment_not_submitted",
			"only a submitted grant can be approved; this one is %s", a.Status)
	}
	if a.RequestedBy != nil && *a.RequestedBy == approver {
		return shared.Forbidden("discount.self_approval",
			"a discount cannot be approved by the person who requested it")
	}
	a.Status = AssignmentApproved
	a.ApprovedBy = &approver
	a.ApprovedAt = &at
	return nil
}

// Reject declines a submitted grant.
func (a *Assignment) Reject(approver shared.ID, reason string, at time.Time) error {
	if a.Status != AssignmentSubmitted {
		return shared.PreconditionFailed("discount.assignment_not_submitted",
			"only a submitted grant can be rejected; this one is %s", a.Status)
	}
	if reason == "" {
		return shared.Validation("discount.rejection_reason_required", "a rejection must state its reason")
	}
	a.Status = AssignmentRejected
	a.ApprovedBy = &approver
	a.RejectionReason = &reason
	return nil
}

// Revoke withdraws an approved grant.
func (a *Assignment) Revoke(actor shared.ID, reason string, effect RevocationEffect, at time.Time) error {
	if a.Status != AssignmentApproved {
		return shared.PreconditionFailed("discount.assignment_not_approved",
			"only an approved grant can be revoked; this one is %s", a.Status)
	}
	if reason == "" {
		return shared.Validation("discount.revocation_reason_required", "a revocation must state its reason")
	}
	switch effect {
	case RevokeProspectiveOnly, RevokeIncludeCurrentYear:
	case "":
		effect = RevokeProspectiveOnly
	default:
		return shared.Validation("discount.invalid_revocation_effect",
			"revocation effect must be prospective_only or include_current_year, got %q", effect)
	}
	a.Status = AssignmentRevoked
	a.RevokedBy = &actor
	a.RevokedAt = &at
	a.RevocationReason = &reason
	a.RevocationEffect = &effect
	return nil
}

// ApplicationStatus is the state of a materialised discount on one account.
type ApplicationStatus string

const (
	// ApplicationPending is materialised but not yet in force, waiting on the
	// annual eligibility re-confirmation.
	ApplicationPending ApplicationStatus = "pending"
	// ApplicationApplied is reducing the account's net.
	ApplicationApplied ApplicationStatus = "applied"
	// ApplicationDeclined was not confirmed for this year. The grant itself is
	// untouched and may apply again next year.
	ApplicationDeclined ApplicationStatus = "declined"
	// ApplicationReversed was in force and has been unwound by an adjustment.
	ApplicationReversed ApplicationStatus = "reversed"
)

// Application is a discount materialised onto one account, with its amount
// frozen at the moment it was computed.
type Application struct {
	ID                  shared.ID
	AccountID           shared.ID
	AssignmentID        shared.ID
	DefinitionVersionID shared.ID

	FrozenBase       money.Amount
	ComputedAmount   money.Amount
	AppliedAmount    money.Amount
	TruncationReason *TruncationReason
	Sequence         int16

	Status         ApplicationStatus
	AppliedAt      *time.Time
	AppliedBy      *shared.ID
	ReversedAt     *time.Time
	ReversedBy     *shared.ID
	ReversalReason *string
	CreatedAt      time.Time
}

// NewApplication turns an engine result into a persistable application.
func NewApplication(accountID shared.ID, r Result, status ApplicationStatus) *Application {
	return &Application{
		ID:                  shared.NewID(),
		AccountID:           accountID,
		AssignmentID:        r.Candidate.AssignmentID,
		DefinitionVersionID: r.Candidate.DefinitionVersionID,
		FrozenBase:          r.FrozenBase,
		ComputedAmount:      r.ComputedAmount,
		AppliedAmount:       r.AppliedAmount,
		TruncationReason:    r.TruncationReason,
		Sequence:            r.Sequence,
		Status:              status,
		CreatedAt:           time.Now().UTC(),
	}
}

// EffectiveAmount is what this application actually takes off the account.
// Only an applied row reduces anything; a pending or declined one is recorded
// but inert.
func (a *Application) EffectiveAmount() money.Amount {
	if a.Status != ApplicationApplied {
		return 0
	}
	return a.AppliedAmount
}

// Confirm moves a pending application into force.
func (a *Application) Confirm(actor shared.ID, at time.Time) error {
	if a.Status != ApplicationPending {
		return shared.PreconditionFailed("discount.application_not_pending",
			"only a pending application can be confirmed; this one is %s", a.Status)
	}
	a.Status = ApplicationApplied
	a.AppliedAt = &at
	a.AppliedBy = &actor
	return nil
}

// Decline records that eligibility was not confirmed for this year.
func (a *Application) Decline(actor shared.ID, at time.Time) error {
	if a.Status != ApplicationPending {
		return shared.PreconditionFailed("discount.application_not_pending",
			"only a pending application can be declined; this one is %s", a.Status)
	}
	a.Status = ApplicationDeclined
	a.ReversedBy = &actor
	a.ReversedAt = &at
	return nil
}

// Reverse unwinds an applied discount. The row stays; a compensating
// adjustment restores the amount to the account.
func (a *Application) Reverse(actor shared.ID, reason string, at time.Time) error {
	if a.Status != ApplicationApplied {
		return shared.PreconditionFailed("discount.application_not_applied",
			"only an applied discount can be reversed; this one is %s", a.Status)
	}
	if reason == "" {
		return shared.Validation("discount.reversal_reason_required", "a reversal must state its reason")
	}
	a.Status = ApplicationReversed
	a.ReversedBy = &actor
	a.ReversedAt = &at
	a.ReversalReason = &reason
	return nil
}
