// Package payment holds the cash-facing documents: payments, refunds, voids,
// and the cashier session that frames them.
//
// Every type here is append-only in spirit and enforced as such in the
// database. A posted payment is never edited and never deleted. The receipt in
// the student's hand must always correspond to a row that still says what it
// said when it printed, and every correction is a new document that references
// the original.
package payment

import (
	"regexp"
	"strings"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// Status is the lifecycle of a payment.
type Status string

const (
	// StatusDraft has no receipt number and may be discarded. Desk flow
	// usually posts directly and never creates one.
	StatusDraft Status = "draft"
	// StatusPosted is money received. Immutable.
	StatusPosted Status = "posted"
	// StatusVoided was reversed in full. The receipt number is retained and
	// reported as voided rather than vanishing from the sequence.
	StatusVoided Status = "voided"
)

// SeriesKind distinguishes receipt series.
type SeriesKind string

const (
	SeriesPayment SeriesKind = "payment"
	SeriesRefund  SeriesKind = "refund"
)

// Method is a way of paying. Master data, so a university that adds a mobile
// wallet next year adds a row.
type Method struct {
	ID                shared.ID
	Code              string
	NameAr            string
	IsCash            bool
	RequiresReference bool
	IsActive          bool
	SortOrder         int16
}

// NewMethod builds a way of paying.
//
// Both flags are decisions about how the money will be handled rather than
// labels. IsCash puts the collection in a drawer that gets counted at shift
// close; RequiresReference demands the bank or terminal reference without
// which the collection cannot be matched to a statement.
func NewMethod(code, nameAr string, isCash, requiresReference bool) (*Method, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !methodCodePattern.MatchString(code) {
		return nil, shared.Validation("payment_method.invalid_code",
			"a method code is 2 to 32 upper-case letters, digits or underscores, got %q", code)
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("payment_method.name_required", "the Arabic name is required")
	}
	if isCash && requiresReference {
		return nil, shared.Validation("payment_method.cash_needs_no_reference",
			"cash has no external reference to record; requiring one would leave the cashier "+
				"inventing a number to get past the form")
	}
	return &Method{
		ID:                shared.NewID(),
		Code:              code,
		NameAr:            strings.TrimSpace(nameAr),
		IsCash:            isCash,
		RequiresReference: requiresReference,
		IsActive:          true,
	}, nil
}

// methodCodePattern mirrors the schema's check constraint.
var methodCodePattern = regexp.MustCompile(`^[A-Z0-9_]{2,32}$`)

// Well-known method codes, seeded by migration.
const (
	MethodCash     = "CASH"
	MethodBank     = "BANK"
	MethodPOS      = "POS"
	MethodOnline   = "ONLINE"
	MethodTransfer = "TRANSFER"
	MethodOther    = "OTHER"
)

// Payment is one collection of money.
type Payment struct {
	ID             shared.ID
	ReceiptNo      *string
	NumberSeriesID *shared.ID

	AccountID    shared.ID
	StudentID    shared.ID
	EnrollmentID shared.ID
	// PostingYearID is the year whose books this collection belongs to.
	//
	// It is not necessarily the account's year. A student settling a 2023-2024
	// debt in 2026 posts against the current open year and allocates to the old
	// year's installments: closing a year stops the university rewriting its
	// records, not collecting what it is owed.
	PostingYearID shared.ID

	Amount          money.Amount
	PaymentMethodID shared.ID
	MethodReference *string

	CashierUserID shared.ID

	PaidAt   time.Time
	PostedAt *time.Time
	Status   Status

	IdempotencyKey *string
	PayloadHash    *string

	PayerName *string
	Notes     *string

	VoidedAt      *time.Time
	VoidedBy      *shared.ID
	VoidReason    *string
	VoidRequestID *shared.ID

	CreatedAt time.Time
}

// NewParams carries what a cashier supplies to record a payment.
type NewParams struct {
	AccountID       shared.ID
	StudentID       shared.ID
	EnrollmentID    shared.ID
	PostingYearID   shared.ID
	Amount          money.Amount
	PaymentMethodID shared.ID
	MethodReference *string
	CashierUserID   shared.ID
	IdempotencyKey  string
	PayloadHash     string
	PayerName       *string
	Notes           *string
	PaidAt          time.Time
}

// New builds a payment in draft.
func New(p NewParams) (*Payment, error) {
	if !p.Amount.IsPositive() {
		return nil, shared.Validation("payment.non_positive_amount",
			"a payment must be greater than zero, got %s", p.Amount)
	}
	if p.IdempotencyKey == "" {
		return nil, shared.Validation("payment.idempotency_key_required",
			"an idempotency key is required so a retried submission cannot collect twice")
	}
	paidAt := p.PaidAt
	if paidAt.IsZero() {
		paidAt = time.Now().UTC()
	}
	return &Payment{
		ID:              shared.NewID(),
		AccountID:       p.AccountID,
		StudentID:       p.StudentID,
		EnrollmentID:    p.EnrollmentID,
		PostingYearID:   p.PostingYearID,
		Amount:          p.Amount,
		PaymentMethodID: p.PaymentMethodID,
		MethodReference: p.MethodReference,
		CashierUserID:   p.CashierUserID,
		PaidAt:          paidAt,
		Status:          StatusDraft,
		IdempotencyKey:  &p.IdempotencyKey,
		PayloadHash:     &p.PayloadHash,
		PayerName:       p.PayerName,
		Notes:           p.Notes,
		CreatedAt:       time.Now().UTC(),
	}, nil
}

// Post assigns the receipt number and marks the money received.
//
// The number is allocated here, at posting, not when the row was created. A
// draft that is abandoned must not burn a number, and a transaction that rolls
// back must return the one it took — which is what makes the printed sequence
// gapless.
func (p *Payment) Post(receiptNo string, seriesID shared.ID, at time.Time) error {
	if p.Status != StatusDraft {
		return shared.PreconditionFailed("payment.not_draft",
			"only a draft payment can be posted; this one is %s", p.Status)
	}
	if receiptNo == "" {
		return shared.InvariantViolation("payment.receipt_required",
			"a posted payment must carry a receipt number")
	}
	p.Status = StatusPosted
	p.ReceiptNo = &receiptNo
	p.NumberSeriesID = &seriesID
	p.PostedAt = &at
	return nil
}

// Void reverses the payment in full.
//
// The precondition is the important part. A payment that already carries a
// posted refund cannot be voided, because the two together would return more
// money than was collected: refund 400,000 of a 1,000,000 payment, then void
// the whole 1,000,000, and the university has paid out 1,400,000 against a
// million received. The refund path handles whatever remains.
func (p *Payment) Void(actor shared.ID, reason string, at time.Time, postedRefundCount int) error {
	if p.Status != StatusPosted {
		return shared.PreconditionFailed("payment.not_posted",
			"only a posted payment can be voided; this one is %s", p.Status)
	}
	if reason == "" {
		return shared.Validation("payment.void_reason_required", "a void must state its reason")
	}
	if postedRefundCount > 0 {
		return shared.PreconditionFailed("payment.has_refunds",
			"this payment already carries %d posted refund(s) and cannot be voided; "+
				"voiding it as well would return more money than was collected",
			postedRefundCount).
			WithDetail("posted_refunds", postedRefundCount).
			WithDetail("remedy", "refund the remaining amount instead of voiding")
	}
	p.Status = StatusVoided
	p.VoidedAt = &at
	p.VoidedBy = &actor
	p.VoidReason = &reason
	return nil
}

// IsCounted reports whether this payment contributes to collections.
func (p *Payment) IsCounted() bool { return p.Status == StatusPosted }

// Allocation is one line of a payment's distribution across installments.
//
// Reversals are rows here, not flags, so the same (payment, installment) pair
// legitimately appears more than once: an allocation, and later its reversal.
// A unique constraint on the pair would force an in-place update and destroy
// the append-only property the whole design rests on.
type Allocation struct {
	ID                   shared.ID
	PaymentID            shared.ID
	InstallmentID        shared.ID
	Amount               money.Amount
	EntryType            AllocationType
	ReversesAllocationID *shared.ID
	CausedByType         *string
	CausedByID           *shared.ID
	CreatedAt            time.Time
	CreatedBy            *shared.ID
}

// AllocationType distinguishes money arriving from money being unwound.
type AllocationType string

const (
	AllocationApplied  AllocationType = "allocation"
	AllocationReversed AllocationType = "reversal"
)

// NewAllocation builds a forward allocation.
func NewAllocation(paymentID, installmentID shared.ID, amount money.Amount) *Allocation {
	return &Allocation{
		ID:            shared.NewID(),
		PaymentID:     paymentID,
		InstallmentID: installmentID,
		Amount:        amount,
		EntryType:     AllocationApplied,
		CreatedAt:     time.Now().UTC(),
	}
}

// NewReversal builds the row that unwinds an allocation.
func NewReversal(paymentID, installmentID, reversesID shared.ID, amount money.Amount, causedByType string, causedByID shared.ID) *Allocation {
	return &Allocation{
		ID:                   shared.NewID(),
		PaymentID:            paymentID,
		InstallmentID:        installmentID,
		Amount:               amount,
		EntryType:            AllocationReversed,
		ReversesAllocationID: &reversesID,
		CausedByType:         &causedByType,
		CausedByID:           &causedByID,
		CreatedAt:            time.Now().UTC(),
	}
}

// RefundStatus is the lifecycle of a refund.
type RefundStatus string

const (
	RefundRequested RefundStatus = "requested"
	RefundApproved  RefundStatus = "approved"
	RefundRejected  RefundStatus = "rejected"
	RefundCancelled RefundStatus = "cancelled"
	RefundPosted    RefundStatus = "posted"
)

// Refund is money returned against a payment.
//
// The original payment keeps its amount. A 500,000 payment with a 100,000
// refund is not a 400,000 payment; it is two facts and a computed net. Editing
// the payment down would destroy the record of what was collected and what was
// given back, which is precisely what an auditor comes to check.
type Refund struct {
	ID             shared.ID
	RefundNo       *string
	NumberSeriesID *shared.ID

	PaymentID     shared.ID
	AccountID     shared.ID
	StudentID     shared.ID
	PostingYearID shared.ID

	Amount          money.Amount
	PaymentMethodID shared.ID
	MethodReference *string
	Reason          string

	Status          RefundStatus
	RequestedBy     shared.ID
	RequestedAt     time.Time
	ApprovedBy      *shared.ID
	ApprovedAt      *time.Time
	RejectedBy      *shared.ID
	RejectedAt      *time.Time
	RejectionReason *string
	PostedAt        *time.Time
	PostedBy        *shared.ID

	IdempotencyKey *string
	CreatedAt      time.Time
}

// NewRefundParams carries a refund request.
type NewRefundParams struct {
	PaymentID       shared.ID
	AccountID       shared.ID
	StudentID       shared.ID
	PostingYearID   shared.ID
	Amount          money.Amount
	PaymentMethodID shared.ID
	Reason          string
	RequestedBy     shared.ID
	IdempotencyKey  *string
}

// NewRefund builds a refund request.
func NewRefund(p NewRefundParams) (*Refund, error) {
	if !p.Amount.IsPositive() {
		return nil, shared.Validation("refund.non_positive_amount",
			"a refund must be greater than zero, got %s", p.Amount)
	}
	if p.Reason == "" {
		return nil, shared.Validation("refund.reason_required", "a refund must state its reason")
	}
	return &Refund{
		ID:              shared.NewID(),
		PaymentID:       p.PaymentID,
		AccountID:       p.AccountID,
		StudentID:       p.StudentID,
		PostingYearID:   p.PostingYearID,
		Amount:          p.Amount,
		PaymentMethodID: p.PaymentMethodID,
		Reason:          p.Reason,
		Status:          RefundRequested,
		RequestedBy:     p.RequestedBy,
		RequestedAt:     time.Now().UTC(),
		IdempotencyKey:  p.IdempotencyKey,
		CreatedAt:       time.Now().UTC(),
	}, nil
}

// Approve accepts a refund request.
//
// The approver may be the requester. With one operator account they always
// are, and a rule that refused it would not slow a fraud down — it would stop
// a refund being issued at all.
func (r *Refund) Approve(approver shared.ID, at time.Time) error {
	if r.Status != RefundRequested {
		return shared.PreconditionFailed("refund.not_requested",
			"only a requested refund can be approved; this one is %s", r.Status)
	}
	r.Status = RefundApproved
	r.ApprovedBy = &approver
	r.ApprovedAt = &at
	return nil
}

// Reject declines a refund request.
func (r *Refund) Reject(actor shared.ID, reason string, at time.Time) error {
	if r.Status != RefundRequested {
		return shared.PreconditionFailed("refund.not_requested",
			"only a requested refund can be rejected; this one is %s", r.Status)
	}
	if reason == "" {
		return shared.Validation("refund.rejection_reason_required", "a rejection must state its reason")
	}
	r.Status = RefundRejected
	r.RejectedBy = &actor
	r.RejectedAt = &at
	r.RejectionReason = &reason
	return nil
}

// Post pays the money out and assigns the refund number.
func (r *Refund) Post(refundNo string, seriesID shared.ID, actor shared.ID, at time.Time) error {
	if r.Status != RefundApproved {
		return shared.PreconditionFailed("refund.not_approved",
			"only an approved refund can be posted; this one is %s", r.Status)
	}
	r.Status = RefundPosted
	r.RefundNo = &refundNo
	r.NumberSeriesID = &seriesID
	r.PostedAt = &at
	r.PostedBy = &actor
	return nil
}

// RefundAllocation records which installment a refund pulled money back out
// of, or which credit it drew on.
type RefundAllocation struct {
	ID                   shared.ID
	RefundID             shared.ID
	InstallmentID        *shared.ID
	ReversesAllocationID *shared.ID
	CreditEntryID        *shared.ID
	Amount               money.Amount
	CreatedAt            time.Time
}

// VoidRequestStatus is the lifecycle of a void request.
type VoidRequestStatus string

const (
	VoidRequested VoidRequestStatus = "requested"
	VoidExecuted  VoidRequestStatus = "executed"
	VoidRejected  VoidRequestStatus = "rejected"
	VoidCancelled VoidRequestStatus = "cancelled"
)

// VoidRequest is the document that makes voiding a two-person act.
//
// A cashier who mis-keys a payment raises one; a finance manager executes it.
// Modelling this as a document rather than as a permission check means the
// second signature is recorded and reportable, which is what the void register
// — the single most useful fraud-detection report in the system — reads from.
type VoidRequest struct {
	ID              shared.ID
	PaymentID       shared.ID
	Reason          string
	Status          VoidRequestStatus
	RequestedBy     shared.ID
	RequestedAt     time.Time
	ExecutedBy      *shared.ID
	ExecutedAt      *time.Time
	RejectedBy      *shared.ID
	RejectedAt      *time.Time
	RejectionReason *string
}

// NewVoidRequest builds a request to void a payment.
func NewVoidRequest(paymentID shared.ID, reason string, requestedBy shared.ID) (*VoidRequest, error) {
	if reason == "" {
		return nil, shared.Validation("void_request.reason_required",
			"a void request must state its reason")
	}
	return &VoidRequest{
		ID:          shared.NewID(),
		PaymentID:   paymentID,
		Reason:      reason,
		Status:      VoidRequested,
		RequestedBy: requestedBy,
		RequestedAt: time.Now().UTC(),
	}, nil
}

// Execute carries out the void.
//
// The executor may be the requester: see Refund.Approve for why the four-eyes
// rule left with the second operator it depended on.
func (v *VoidRequest) Execute(actor shared.ID, at time.Time) error {
	if v.Status != VoidRequested {
		return shared.PreconditionFailed("void_request.not_requested",
			"only an open request can be executed; this one is %s", v.Status)
	}
	v.Status = VoidExecuted
	v.ExecutedBy = &actor
	v.ExecutedAt = &at
	return nil
}

// Reject declines a void request.
func (v *VoidRequest) Reject(actor shared.ID, reason string, at time.Time) error {
	if v.Status != VoidRequested {
		return shared.PreconditionFailed("void_request.not_requested",
			"only an open request can be rejected; this one is %s", v.Status)
	}
	v.Status = VoidRejected
	v.RejectedBy = &actor
	v.RejectedAt = &at
	v.RejectionReason = &reason
	return nil
}
