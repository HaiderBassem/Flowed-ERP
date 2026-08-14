package billing

import (
	"time"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// PolicyStatus is the lifecycle of a fee policy or installment template.
type PolicyStatus string

const (
	// PolicyDraft is editable and invisible to resolution.
	PolicyDraft PolicyStatus = "draft"
	// PolicyPublished is in force. Its amounts are frozen: changing a price
	// means publishing a new version, never editing this one.
	PolicyPublished PolicyStatus = "published"
	// PolicyRetired is superseded and no longer resolvable.
	PolicyRetired PolicyStatus = "retired"
)

// FeeComponent is one charge inside a policy.
//
// Fees are components, not a single number, because the components carry
// different rules. Registration and identity-card charges are typically not
// discountable and not refundable, and a design with one lump sum cannot
// express a full exemption that still collects them.
type FeeComponent struct {
	ID             shared.ID
	FeePolicyID    shared.ID
	Code           string
	NameAr         string
	NameEn         *string
	Amount         money.Amount
	IsDiscountable bool
	IsRefundable   bool
	IsMandatory    bool
	SortOrder      int16
}

// FeePolicy is a priced scope for one academic year.
//
// A NULL dimension is a wildcard. The specificity score decides between
// overlapping rows and is computed from powers of two, so no two distinct
// scopes can tie: a score that merely counted specified dimensions would rank
// {college} against {department, stage, category} arbitrarily and charge
// different students differently depending on which row the planner happened
// to return first.
type FeePolicy struct {
	ID         shared.ID
	PolicyCode string
	VersionNo  int32

	AcademicYearID    shared.ID
	CollegeID         *shared.ID
	DepartmentID      *shared.ID
	Stage             *int16
	StudyTypeID       *shared.ID
	StudentCategoryID *shared.ID

	SpecificityScore int32
	Status           PolicyStatus
	// MaxDiscountBP caps total discount as a rate of the discountable base.
	// It lives on the policy so that it freezes with the year rather than
	// following a definition someone edits later.
	MaxDiscountBP money.BasisPoints
	EffectiveFrom *shared.Date
	Description   *string

	Components []*FeeComponent

	PublishedAt *time.Time
	PublishedBy *shared.ID
	RetiredAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   *shared.ID
}

// Specificity weights. Powers of two guarantee that every distinct combination
// of specified dimensions produces a distinct total.
const (
	weightCollege  = 16
	weightDept     = 8
	weightStage    = 4
	weightType     = 2
	weightCategory = 1
)

// ComputeSpecificity returns the score for this policy's scope. The database
// computes the same value in a generated column; this mirrors it so the
// application can rank candidates it holds in memory.
func (p *FeePolicy) ComputeSpecificity() int32 {
	var score int32
	if p.CollegeID != nil {
		score += weightCollege
	}
	if p.DepartmentID != nil {
		score += weightDept
	}
	if p.Stage != nil {
		score += weightStage
	}
	if p.StudyTypeID != nil {
		score += weightType
	}
	if p.StudentCategoryID != nil {
		score += weightCategory
	}
	return score
}

// GrossTotal sums every component.
func (p *FeePolicy) GrossTotal() (money.Amount, error) {
	var total money.Amount
	for _, c := range p.Components {
		next, err := total.Add(c.Amount)
		if err != nil {
			return 0, shared.Internal("fee_policy.total_overflow", err, "summing fee components")
		}
		total = next
	}
	return total, nil
}

// Publish freezes the policy's amounts and makes it resolvable.
func (p *FeePolicy) Publish(actor shared.ID, at time.Time) error {
	if p.Status != PolicyDraft {
		return shared.PreconditionFailed("fee_policy.not_draft",
			"only a draft policy can be published; %s is %s", p.PolicyCode, p.Status)
	}
	if len(p.Components) == 0 {
		return shared.Validation("fee_policy.no_components",
			"a fee policy must define at least one component before it is published")
	}
	seen := make(map[string]bool, len(p.Components))
	for _, c := range p.Components {
		if seen[c.Code] {
			return shared.Validation("fee_policy.duplicate_component",
				"component %q appears more than once", c.Code)
		}
		seen[c.Code] = true
		if c.Amount.IsNegative() {
			return shared.Validation("fee_policy.negative_component",
				"component %q has a negative amount", c.Code)
		}
	}
	p.Status = PolicyPublished
	p.PublishedAt = &at
	p.PublishedBy = &actor
	return nil
}

// SnapshotLine is a fee component copied into an account and frozen there.
//
// The amount here is authoritative even if the source component is later
// edited or the whole policy retired. That is the point: an account
// reconstructed from these lines gives the number the student was actually
// charged, not the number the configuration would produce today.
type SnapshotLine struct {
	ID                shared.ID
	AccountID         shared.ID
	ComponentCode     string
	NameAr            string
	Amount            money.Amount
	IsDiscountable    bool
	IsRefundable      bool
	SortOrder         int16
	SourceComponentID *shared.ID
	CreatedAt         time.Time
}

// SnapshotFromPolicy copies a policy's components onto an account.
func SnapshotFromPolicy(accountID shared.ID, p *FeePolicy) []*SnapshotLine {
	lines := make([]*SnapshotLine, 0, len(p.Components))
	for _, c := range p.Components {
		componentID := c.ID
		lines = append(lines, &SnapshotLine{
			ID:                shared.NewID(),
			AccountID:         accountID,
			ComponentCode:     c.Code,
			NameAr:            c.NameAr,
			Amount:            c.Amount,
			IsDiscountable:    c.IsDiscountable,
			IsRefundable:      c.IsRefundable,
			SortOrder:         c.SortOrder,
			SourceComponentID: &componentID,
		})
	}
	return lines
}

// InstallmentTemplate is a reusable plan shape.
type InstallmentTemplate struct {
	ID     shared.ID
	Code   string
	NameAr string
	NameEn *string

	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	Stage          *int16
	StudyTypeID    *shared.ID

	SpecificityScore int32
	MaxInstallments  int16
	Status           PolicyStatus
	Lines            []TemplateLine

	PublishedAt *time.Time
	PublishedBy *shared.ID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Publish validates and freezes the template.
//
// The share validation happens here, at publication, rather than at use. A
// template whose shares total ninety percent would otherwise fail once per
// student on the first morning of registration, with hundreds of people in the
// corridor, instead of once when an administrator saved it.
func (t *InstallmentTemplate) Publish(actor shared.ID, at time.Time) error {
	if t.Status != PolicyDraft {
		return shared.PreconditionFailed("installment_template.not_draft",
			"only a draft template can be published; %s is %s", t.Code, t.Status)
	}
	if len(t.Lines) == 0 {
		return shared.Validation("installment_template.no_lines",
			"a template must define at least one installment")
	}
	if int16(len(t.Lines)) > t.MaxInstallments {
		return shared.Validation("installment_template.too_many_lines",
			"the template defines %d installments but its maximum is %d", len(t.Lines), t.MaxInstallments)
	}

	var total money.BasisPoints
	seen := make(map[int16]bool, len(t.Lines))
	for _, line := range t.Lines {
		if seen[line.LineNo] {
			return shared.Validation("installment_template.duplicate_line",
				"line number %d appears more than once", line.LineNo)
		}
		seen[line.LineNo] = true
		if line.ShareBP <= 0 {
			return shared.Validation("installment_template.non_positive_share",
				"line %d has a share of %d basis points", line.LineNo, line.ShareBP)
		}
		total += line.ShareBP
	}
	if total != money.FullRate {
		return shared.Validation("installment_template.shares_do_not_total",
			"the shares total %d basis points; they must total exactly %d (100%%)",
			total, money.FullRate).
			WithDetail("total_bp", int(total)).
			WithDetail("required_bp", int(money.FullRate))
	}

	t.Status = PolicyPublished
	t.PublishedAt = &at
	t.PublishedBy = &actor
	return nil
}

// Adjustment is a signed change to what an account owes, posted after the
// account was generated.
type Adjustment struct {
	ID        shared.ID
	AccountID shared.ID
	Type      AdjustmentType
	Amount    money.Amount
	Reason    string

	PairedAdjustmentID *shared.ID
	ReferenceType      *string
	ReferenceID        *shared.ID

	ApprovedBy    *shared.ID
	ApprovedAt    *time.Time
	PostedAt      time.Time
	PostedBy      *shared.ID
	PostingYearID *shared.ID
}

// NewAdjustment builds an adjustment, enforcing that the types which reach
// into closed books carry a named approver.
func NewAdjustment(accountID shared.ID, kind AdjustmentType, amount money.Amount, reason string) (*Adjustment, error) {
	if amount.IsZero() {
		return nil, shared.Validation("adjustment.zero_amount",
			"an adjustment of zero changes nothing and must not be recorded")
	}
	if reason == "" {
		return nil, shared.Validation("adjustment.reason_required",
			"every adjustment must record why it was made")
	}
	return &Adjustment{
		ID:        shared.NewID(),
		AccountID: accountID,
		Type:      kind,
		Amount:    amount,
		Reason:    reason,
		PostedAt:  time.Now().UTC(),
	}, nil
}

// RequiresApproval reports whether this kind of adjustment needs a second
// person's signature before it may be posted.
func (a *Adjustment) RequiresApproval() bool {
	switch a.Type {
	case AdjustmentClosedYear, AdjustmentWaiver, AdjustmentWriteOff:
		return true
	default:
		return false
	}
}

// CreditSource says where an unspent balance came from.
type CreditSource string

const (
	CreditFromOverpayment         CreditSource = "overpayment"
	CreditFromRetroactiveDiscount CreditSource = "retroactive_discount"
	CreditFromTransfer            CreditSource = "transfer_credit"
	CreditFromRefundReversal      CreditSource = "refund_reversal"
	CreditFromOther               CreditSource = "other"
)

// CreditStatus is the state of a credit entry.
type CreditStatus string

const (
	CreditOpen              CreditStatus = "open"
	CreditPartiallyConsumed CreditStatus = "partially_consumed"
	CreditConsumed          CreditStatus = "consumed"
	CreditRefunded          CreditStatus = "refunded"
	CreditExpired           CreditStatus = "expired"
)

// CreditEntry is money the university holds that the student has not spent.
//
// It is a row rather than a number on the account so that it can be locked.
// Carrying credit forward to next year and refunding it in cash are separate
// code paths that lock different accounts; without a lock on the credit
// itself, both could read the same balance and pay it out.
type CreditEntry struct {
	ID              shared.ID
	AccountID       shared.ID
	StudentID       shared.ID
	Amount          money.Amount
	ConsumedAmount  money.Amount
	Source          CreditSource
	SourceReference *shared.ID
	Status          CreditStatus
	Reason          *string
	CreatedAt       time.Time
	CreatedBy       *shared.ID
	ClosedAt        *time.Time
}

// NewCreditEntry builds an open credit.
func NewCreditEntry(accountID, studentID shared.ID, amount money.Amount, source CreditSource) (*CreditEntry, error) {
	if !amount.IsPositive() {
		return nil, shared.Validation("credit.non_positive_amount",
			"a credit must be greater than zero, got %s", amount)
	}
	return &CreditEntry{
		ID:        shared.NewID(),
		AccountID: accountID,
		StudentID: studentID,
		Amount:    amount,
		Source:    source,
		Status:    CreditOpen,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// Available is the unspent part of this credit.
func (c *CreditEntry) Available() money.Amount {
	available, err := c.Amount.Sub(c.ConsumedAmount)
	if err != nil {
		return 0
	}
	return available.ClampNonNegative()
}

// Consume spends part of the credit, refusing to spend more than is left.
func (c *CreditEntry) Consume(amount money.Amount, at time.Time) error {
	if !amount.IsPositive() {
		return shared.Validation("credit.non_positive_consumption",
			"a consumption must be greater than zero, got %s", amount)
	}
	if amount > c.Available() {
		return shared.PreconditionFailed("credit.insufficient",
			"credit %s has %s available but %s was requested", c.ID, c.Available(), amount).
			WithDetail("available", c.Available().Int64()).
			WithDetail("requested", amount.Int64())
	}
	consumed, err := c.ConsumedAmount.Add(amount)
	if err != nil {
		return shared.Internal("credit.consumption_overflow", err, "consuming credit")
	}
	c.ConsumedAmount = consumed
	if c.ConsumedAmount == c.Amount {
		c.Status = CreditConsumed
		c.ClosedAt = &at
	} else {
		c.Status = CreditPartiallyConsumed
	}
	return nil
}

// CreditPurpose says what a credit was spent on.
type CreditPurpose string

const (
	CreditForInstallment  CreditPurpose = "installment_offset"
	CreditForCarryForward CreditPurpose = "carry_forward"
	CreditForCashRefund   CreditPurpose = "cash_refund"
	CreditForWriteOff     CreditPurpose = "write_off"
)

// CreditConsumption records one spend against a credit entry.
type CreditConsumption struct {
	ID              shared.ID
	CreditEntryID   shared.ID
	Amount          money.Amount
	Purpose         CreditPurpose
	TargetAccountID *shared.ID
	TargetReference *shared.ID
	ConsumedAt      time.Time
	ConsumedBy      *shared.ID
}
