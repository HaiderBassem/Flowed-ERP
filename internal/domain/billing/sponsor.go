package billing

import (
	"regexp"
	"strings"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// SettlementMode says who carries the risk when a sponsor does not pay.
//
// The design document lists sponsorship as a future extension and does not
// settle this, so it is configured per agreement rather than chosen here. Both
// answers are real and a university uses both: a ministry-funded seat is one, a
// company reimbursing an employee's tuition is the other.
type SettlementMode string

const (
	// SettlementReceivable leaves the student's obligation exactly as it is.
	// The sponsor's share is an expected inflow, and the sponsor pays through
	// an ordinary payment allocated the ordinary way — nothing in the financial
	// core changes. If the sponsor never pays, the student still owes it.
	SettlementReceivable SettlementMode = "receivable"
	// SettlementCoversDebt reduces the student's obligation by an adjustment
	// when the account is generated. The university carries the loss if the
	// sponsor defaults, which is what a ministry-funded seat actually means:
	// the student is not chased, whatever passes between the university and
	// the ministry.
	SettlementCoversDebt SettlementMode = "covers_debt"
)

// Valid reports whether the mode is one the system recognises.
func (m SettlementMode) Valid() bool {
	return m == SettlementReceivable || m == SettlementCoversDebt
}

// ReducesStudentDebt reports whether this mode writes an adjustment against the
// student's account.
func (m SettlementMode) ReducesStudentDebt() bool { return m == SettlementCoversDebt }

// CoverageType is the shape of what a sponsor agreed to pay.
type CoverageType string

const (
	// CoveragePercentage is a share of the discountable base, which is the
	// same base a percentage discount computes against. Using the gross would
	// have a sponsor paying for the identity card, which no agreement means.
	CoveragePercentage CoverageType = "percentage"
	// CoverageFixed is a stated number of dinars per academic year.
	CoverageFixed CoverageType = "fixed_per_year"
	// CoverageFull is the whole obligation.
	CoverageFull CoverageType = "full"
)

// Valid reports whether the coverage type is one the system recognises.
func (c CoverageType) Valid() bool {
	switch c {
	case CoveragePercentage, CoverageFixed, CoverageFull:
		return true
	default:
		return false
	}
}

// SponsorshipStatus is the state of an agreement.
type SponsorshipStatus string

const (
	SponsorshipDraft     SponsorshipStatus = "draft"
	SponsorshipActive    SponsorshipStatus = "active"
	SponsorshipSuspended SponsorshipStatus = "suspended"
	SponsorshipRevoked   SponsorshipStatus = "revoked"
	SponsorshipExpired   SponsorshipStatus = "expired"
)

// Sponsor is a body that pays part of a student's fees.
type Sponsor struct {
	ID           shared.ID
	Code         string
	NameAr       string
	NameEn       *string
	SponsorType  string
	ContactName  *string
	ContactPhone *string
	ContactEmail *string
	Address      *string
	Notes        *string
	IsActive     bool
	CreatedAt    time.Time
	CreatedBy    *shared.ID
}

// NewSponsor builds a sponsoring body.
func NewSponsor(code, nameAr, sponsorType string) (*Sponsor, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !sponsorCodePattern.MatchString(code) {
		return nil, shared.Validation("sponsor.invalid_code",
			"a sponsor code is 2 to 32 upper-case letters, digits or underscores, got %q", code)
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("sponsor.name_required", "a sponsor needs a name")
	}
	if sponsorType == "" {
		sponsorType = "other"
	}
	if !validSponsorType(sponsorType) {
		return nil, shared.Validation("sponsor.invalid_type",
			"%q is not a sponsor type; use ministry, government, company, charity, individual or other",
			sponsorType)
	}
	return &Sponsor{
		ID:          shared.NewID(),
		Code:        code,
		NameAr:      strings.TrimSpace(nameAr),
		SponsorType: sponsorType,
		IsActive:    true,
	}, nil
}

func validSponsorType(t string) bool {
	switch t {
	case "ministry", "government", "company", "charity", "individual", "other":
		return true
	default:
		return false
	}
}

// Sponsorship is the agreement: who is sponsored, by whom, for how much.
type Sponsorship struct {
	ID        shared.ID
	SponsorID shared.ID
	StudentID shared.ID

	CoverageType   CoverageType
	CoverageBP     *money.BasisPoints
	CoverageAmount *money.Amount
	// AnnualCap bounds what the sponsor pays per year whatever the shape.
	// Ministry agreements are routinely "eighty per cent, up to two million".
	AnnualCap *money.Amount

	SettlementMode SettlementMode

	FromYearCode string
	ToYearCode   *string

	Status        SponsorshipStatus
	AgreementRef  *string
	Notes         *string
	ApprovedBy    *shared.ID
	ApprovedAt    *time.Time
	RevokedBy     *shared.ID
	RevokedAt     *time.Time
	RevokedReason *string
	CreatedAt     time.Time
	CreatedBy     *shared.ID
}

// NewSponsorshipParams describes an agreement being recorded.
type NewSponsorshipParams struct {
	SponsorID      shared.ID
	StudentID      shared.ID
	CoverageType   CoverageType
	CoverageBP     *money.BasisPoints
	CoverageAmount *money.Amount
	AnnualCap      *money.Amount
	SettlementMode SettlementMode
	FromYearCode   string
	ToYearCode     *string
	AgreementRef   *string
}

// NewSponsorship records an agreement.
//
// The settlement mode has no default on purpose. An agreement that does not say
// whether the student remains liable has not been negotiated yet, and choosing
// for the university would decide who receives a debt letter.
func NewSponsorship(p NewSponsorshipParams) (*Sponsorship, error) {
	if !p.CoverageType.Valid() {
		return nil, shared.Validation("sponsorship.invalid_coverage",
			"coverage must be percentage, fixed_per_year or full, got %q", p.CoverageType)
	}
	if !p.SettlementMode.Valid() {
		return nil, shared.Validation("sponsorship.settlement_mode_required",
			"the agreement must say whether the student remains liable (receivable) or the "+
				"sponsorship covers the debt (covers_debt)").
			WithDetail("remedy", "ask what happens if the sponsor does not pay; the answer is the mode")
	}
	if strings.TrimSpace(p.FromYearCode) == "" {
		return nil, shared.Validation("sponsorship.year_required",
			"an agreement must say which academic year it starts in")
	}
	if p.ToYearCode != nil && *p.ToYearCode < p.FromYearCode {
		return nil, shared.Validation("sponsorship.years_reversed",
			"the agreement ends (%s) before it starts (%s)", *p.ToYearCode, p.FromYearCode)
	}

	switch p.CoverageType {
	case CoveragePercentage:
		if p.CoverageBP == nil || *p.CoverageBP <= 0 || *p.CoverageBP > 10_000 {
			return nil, shared.Validation("sponsorship.invalid_percentage",
				"a percentage agreement needs a share between 1 and 10,000 basis points")
		}
		if p.CoverageAmount != nil {
			return nil, shared.Validation("sponsorship.conflicting_coverage",
				"a percentage agreement cannot also carry a fixed amount; one of them would "+
					"never be read and nobody could tell which")
		}
	case CoverageFixed:
		if p.CoverageAmount == nil || !p.CoverageAmount.IsPositive() {
			return nil, shared.Validation("sponsorship.invalid_amount",
				"a fixed agreement needs an amount greater than zero")
		}
		if p.CoverageBP != nil {
			return nil, shared.Validation("sponsorship.conflicting_coverage",
				"a fixed agreement cannot also carry a percentage")
		}
	case CoverageFull:
		if p.CoverageBP != nil || p.CoverageAmount != nil {
			return nil, shared.Validation("sponsorship.conflicting_coverage",
				"a full agreement covers everything and takes no figure")
		}
	}
	if p.AnnualCap != nil && !p.AnnualCap.IsPositive() {
		return nil, shared.Validation("sponsorship.invalid_cap",
			"an annual cap must be greater than zero")
	}

	return &Sponsorship{
		ID:             shared.NewID(),
		SponsorID:      p.SponsorID,
		StudentID:      p.StudentID,
		CoverageType:   p.CoverageType,
		CoverageBP:     p.CoverageBP,
		CoverageAmount: p.CoverageAmount,
		AnnualCap:      p.AnnualCap,
		SettlementMode: p.SettlementMode,
		FromYearCode:   strings.TrimSpace(p.FromYearCode),
		ToYearCode:     p.ToYearCode,
		Status:         SponsorshipDraft,
		AgreementRef:   p.AgreementRef,
		CreatedAt:      time.Now().UTC(),
	}, nil
}

// Approve activates an agreement.
//
// Four eyes, like every other decision not to collect money from somebody: the
// person who recorded the agreement cannot be the person who activates it.
func (s *Sponsorship) Approve(approver shared.ID, at time.Time) error {
	if s.Status != SponsorshipDraft {
		return shared.PreconditionFailed("sponsorship.not_draft",
			"this agreement is %s and cannot be approved", s.Status)
	}
	if s.CreatedBy != nil && *s.CreatedBy == approver {
		return shared.Forbidden("sponsorship.self_approval",
			"the person who recorded an agreement cannot approve it").
			WithDetail("remedy", "ask another finance manager to approve it")
	}
	s.Status = SponsorshipActive
	s.ApprovedBy = &approver
	s.ApprovedAt = &at
	return nil
}

// Revoke ends an agreement.
//
// Commitments already frozen against generated accounts are untouched: the
// sponsor agreed to those years and the university priced them accordingly.
// What revocation stops is new commitments.
func (s *Sponsorship) Revoke(by shared.ID, reason string, at time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return shared.Validation("sponsorship.revoke_reason_required",
			"revoking an agreement requires a reason")
	}
	if s.Status == SponsorshipRevoked {
		return nil
	}
	s.Status = SponsorshipRevoked
	s.RevokedBy = &by
	s.RevokedAt = &at
	s.RevokedReason = &reason
	return nil
}

// CoversYear reports whether the agreement applies to an academic year.
func (s *Sponsorship) CoversYear(yearCode string) bool {
	if yearCode < s.FromYearCode {
		return false
	}
	if s.ToYearCode != nil && yearCode > *s.ToYearCode {
		return false
	}
	return true
}

// IsLive reports whether new commitments may be created from this agreement.
func (s *Sponsorship) IsLive() bool { return s.Status == SponsorshipActive }

// Commitment is what a sponsorship worked out to for one account, frozen when
// the account was generated.
//
// The same rule as a discount application, for the same reason: a commitment
// recomputed from the agreement in three years would answer with today's terms
// rather than the ones the student was admitted under.
type Commitment struct {
	ID             shared.ID
	SponsorshipID  shared.ID
	SponsorID      shared.ID
	AccountID      shared.ID
	StudentID      shared.ID
	AcademicYearID shared.ID

	FrozenBase      money.Amount
	CommittedAmount money.Amount
	PaidAmount      money.Amount

	SettlementMode SettlementMode
	Status         string
	AdjustmentID   *shared.ID
	CreatedAt      time.Time
	CreatedBy      *shared.ID
}

// Outstanding is what the sponsor still owes on this commitment.
func (c *Commitment) Outstanding() money.Amount {
	remaining, err := c.CommittedAmount.Sub(c.PaidAmount)
	if err != nil {
		return 0
	}
	return remaining.ClampNonNegative()
}

// ComputeCommitment works out what a sponsorship owes for one account.
//
// discountableBase is the same base a percentage discount computes against —
// the discountable snapshot lines — so a sponsor paying "eighty per cent of the
// fees" pays eighty per cent of tuition and not of the identity card.
// effectiveNet bounds the result: a sponsor cannot commit more than the account
// owes, whatever the agreement says, because the surplus would be money the
// university is owed twice.
func ComputeCommitment(s *Sponsorship, discountableBase, effectiveNet money.Amount) (money.Amount, error) {
	if s == nil {
		return 0, shared.InvariantViolation("sponsorship.missing",
			"a commitment cannot be computed without an agreement")
	}

	var committed money.Amount
	switch s.CoverageType {
	case CoveragePercentage:
		if s.CoverageBP == nil {
			return 0, shared.InvariantViolation("sponsorship.missing_percentage",
				"a percentage agreement with no share reached commitment computation")
		}
		applied, err := money.ApplyRate(discountableBase, *s.CoverageBP)
		if err != nil {
			return 0, shared.Internal("sponsorship.arithmetic", err,
				"computing the sponsor's share")
		}
		committed = applied
	case CoverageFixed:
		if s.CoverageAmount == nil {
			return 0, shared.InvariantViolation("sponsorship.missing_amount",
				"a fixed agreement with no amount reached commitment computation")
		}
		committed = *s.CoverageAmount
	case CoverageFull:
		committed = effectiveNet
	default:
		return 0, shared.Validation("sponsorship.invalid_coverage",
			"%q is not a coverage type", s.CoverageType)
	}

	if s.AnnualCap != nil {
		committed = money.Min(committed, *s.AnnualCap)
	}
	// Never more than the account owes.
	committed = money.Min(committed, effectiveNet)
	return committed.ClampNonNegative(), nil
}

// FundingBreakdown is who is paying for one account.
//
// The answer the sponsor model exists to make possible: before it, every
// reduction looked like a discount and "who funded this student" had no
// answer beyond "the university did not charge it".
type FundingBreakdown struct {
	// Gross is what the account was charged before anything was taken off.
	Gross money.Amount
	// Discount is what the university chose not to charge.
	Discount money.Amount
	// SponsorCovered is what a sponsor's agreement removed from the student's
	// obligation — the covers_debt mode only.
	SponsorCovered money.Amount
	// SponsorReceivable is what a sponsor owes while the student remains
	// liable — the receivable mode.
	SponsorReceivable money.Amount
	// SponsorPaid is what sponsors have actually paid.
	SponsorPaid money.Amount
	// StudentPaid is what the student has paid themselves.
	StudentPaid money.Amount
	// StudentOutstanding is what the student still owes.
	StudentOutstanding money.Amount
}

// BuildFunding assembles the breakdown from an account and its commitments.
func BuildFunding(account *Account, commitments []*Commitment, sponsorPaid money.Amount) FundingBreakdown {
	breakdown := FundingBreakdown{
		Gross:              account.GrossTotal,
		Discount:           account.DiscountTotal,
		SponsorPaid:        sponsorPaid,
		StudentOutstanding: account.Remaining(),
	}

	for _, commitment := range commitments {
		if commitment.Status != "open" && commitment.Status != "settled" {
			continue
		}
		if commitment.SettlementMode.ReducesStudentDebt() {
			breakdown.SponsorCovered = breakdown.SponsorCovered.MustAdd(commitment.CommittedAmount)
		} else {
			breakdown.SponsorReceivable = breakdown.SponsorReceivable.MustAdd(commitment.Outstanding())
		}
	}

	studentPaid, err := account.NetPaid().Sub(sponsorPaid)
	if err == nil {
		breakdown.StudentPaid = studentPaid.ClampNonNegative()
	}
	return breakdown
}

// sponsorCodePattern mirrors the schema's check constraint, so a bad code is
// refused with a readable message rather than a constraint violation.
var sponsorCodePattern = regexp.MustCompile(`^[A-Z0-9_]{2,32}$`)
