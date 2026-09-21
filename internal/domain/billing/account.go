package billing

import (
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// AccountStatus is the lifecycle state of a financial account.
type AccountStatus string

const (
	// AccountPending is generated but not yet activated: the plan may still be
	// reshaped freely.
	AccountPending AccountStatus = "pending"
	// AccountActive is in service and accepting payments.
	AccountActive AccountStatus = "active"
	// AccountSettled owes nothing. A refund can move it back to active.
	AccountSettled AccountStatus = "settled"
	// AccountCancelled belongs to a superseded or withdrawn enrollment. Its
	// payments stay where they are; the balance moves by transfer adjustment.
	AccountCancelled AccountStatus = "cancelled"
	// AccountExempt owes nothing because a full exemption cleared it, and has
	// no installment plan at all.
	AccountExempt AccountStatus = "exempt"
)

// AdjustmentType classifies a signed change to what an account owes.
type AdjustmentType string

const (
	AdjustmentTransferCreditOut   AdjustmentType = "transfer_credit_out"
	AdjustmentTransferCreditIn    AdjustmentType = "transfer_credit_in"
	AdjustmentRetroactiveDiscount AdjustmentType = "retroactive_discount"
	AdjustmentDiscountReversal    AdjustmentType = "discount_reversal"
	AdjustmentLateResult          AdjustmentType = "late_result_correction"
	AdjustmentClosedYear          AdjustmentType = "closed_year_correction"
	AdjustmentWaiver              AdjustmentType = "waiver"
	AdjustmentWriteOff            AdjustmentType = "write_off"
	AdjustmentCorrection          AdjustmentType = "correction"
	// AdjustmentSponsorship is a third party covering part of the fees under
	// an agreement whose settlement mode reduces the student's obligation.
	// Distinct from a discount: the university is still owed the money, by
	// somebody else.
	AdjustmentSponsorship AdjustmentType = "sponsorship"
)

// Account is a student's financial position for one enrollment.
//
// The frozen fields and the cached fields are deliberately different kinds of
// thing. GrossTotal, DiscountTotal and NetTotal are a snapshot: they record
// what was decided when the account was generated and nothing recomputes them
// ever again. PaidTotal, RefundedTotal and CreditBalance are caches of
// transaction rows, maintained inside the transaction that changes them and
// verified nightly.
type Account struct {
	ID           shared.ID
	EnrollmentID shared.ID

	AcademicYearID shared.ID
	StudentID      shared.ID
	CollegeID      shared.ID
	DepartmentID   shared.ID
	StudyTypeID    shared.ID
	Stage          int16

	FeePolicyID           *shared.ID
	FeePolicySpecificity  *int32
	InstallmentTemplateID *shared.ID

	// Frozen at generation.
	GrossTotal       money.Amount
	DiscountableBase money.Amount
	DiscountTotal    money.Amount
	NetTotal         money.Amount

	// Sum of every adjustment posted since. Cached; recomputable.
	AdjustmentTotal money.Amount

	// Caches over payments, refunds and credits.
	PaidTotal     money.Amount
	RefundedTotal money.Amount
	CreditBalance money.Amount

	Status                AccountStatus
	SupersededByAccountID *shared.ID

	GeneratedAt        time.Time
	GeneratedBy        *shared.ID
	ActivatedAt        *time.Time
	SettledAt          *time.Time
	CancelledAt        *time.Time
	CancellationReason *string
	LastReconciledAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EffectiveNet is what the student actually owes: the frozen net plus every
// adjustment posted since. This, not NetTotal, is what the installment plan
// must sum to and what the balance is measured against.
func (a *Account) EffectiveNet() money.Amount {
	net, err := a.NetTotal.Add(a.AdjustmentTotal)
	if err != nil {
		return a.NetTotal
	}
	return net.ClampNonNegative()
}

// NetPaid is money collected less money returned.
func (a *Account) NetPaid() money.Amount {
	paid, err := a.PaidTotal.Sub(a.RefundedTotal)
	if err != nil {
		return 0
	}
	return paid
}

// Remaining is the outstanding balance, floored at zero. A negative raw value
// means the student has overpaid, which shows up as credit rather than as a
// negative debt.
func (a *Account) Remaining() money.Amount {
	remaining, err := a.EffectiveNet().Sub(a.NetPaid())
	if err != nil {
		return 0
	}
	return remaining.ClampNonNegative()
}

// IsFullySettled reports whether nothing is owed.
func (a *Account) IsFullySettled() bool { return !a.Remaining().IsPositive() }

// AcceptsPayment reports whether the account can receive money.
//
// A cancelled account cannot: its enrollment was superseded, and money aimed
// at it belongs on the successor. A settled account can, because a refund may
// reopen it and because a student may overpay deliberately to clear next
// year's first installment.
func (a *Account) AcceptsPayment() bool {
	switch a.Status {
	case AccountPending, AccountActive, AccountSettled:
		return true
	default:
		return false
	}
}

// RequirePaymentAccepted returns a precondition error unless the account can
// receive money.
func (a *Account) RequirePaymentAccepted() error {
	if a.AcceptsPayment() {
		return nil
	}
	err := shared.PreconditionFailed("account.not_accepting_payment",
		"this financial account is %s and cannot receive payments", a.Status).
		WithDetail("account_status", string(a.Status))
	if a.Status == AccountCancelled && a.SupersededByAccountID != nil {
		err = err.WithDetail("pay_this_account_instead", a.SupersededByAccountID.String())
	}
	return err
}

// Activate puts a generated account into service.
func (a *Account) Activate(at time.Time) error {
	if a.Status != AccountPending {
		return shared.PreconditionFailed("account.not_pending",
			"only a pending account can be activated; this one is %s", a.Status)
	}
	a.Status = AccountActive
	a.ActivatedAt = &at
	return nil
}

// RecomputeStatus moves the account between active and settled after money
// changed. Called inside the same transaction as the payment or refund, under
// the account's row lock.
func (a *Account) RecomputeStatus(at time.Time) {
	switch a.Status {
	case AccountCancelled, AccountExempt:
		return
	}
	if a.IsFullySettled() {
		if a.Status != AccountSettled {
			a.Status = AccountSettled
			a.SettledAt = &at
		}
		return
	}
	if a.Status == AccountSettled || a.Status == AccountPending {
		a.Status = AccountActive
		a.SettledAt = nil
	}
}

// Cancel retires an account whose enrollment was superseded or withdrawn. The
// payments stay on it; the balance moves by transfer adjustment so both sides
// of the movement are visible.
func (a *Account) Cancel(reason string, at time.Time, successor *shared.ID) error {
	if reason == "" {
		return shared.Validation("account.cancellation_reason_required",
			"cancelling an account requires a reason")
	}
	if a.Status == AccountCancelled {
		return shared.PreconditionFailed("account.already_cancelled", "this account is already cancelled")
	}
	a.Status = AccountCancelled
	a.CancelledAt = &at
	a.CancellationReason = &reason
	a.SupersededByAccountID = successor
	return nil
}

// ApplyPayment updates the cached totals for a posted payment.
func (a *Account) ApplyPayment(amount money.Amount, at time.Time) error {
	paid, err := a.PaidTotal.Add(amount)
	if err != nil {
		return shared.Internal("account.paid_overflow", err, "adding a payment to the account total")
	}
	a.PaidTotal = paid
	a.RecomputeStatus(at)
	return nil
}

// ApplyRefund updates the cached totals for a posted refund.
func (a *Account) ApplyRefund(amount money.Amount, at time.Time) error {
	refunded, err := a.RefundedTotal.Add(amount)
	if err != nil {
		return shared.Internal("account.refund_overflow", err, "adding a refund to the account total")
	}
	if refunded > a.PaidTotal {
		return shared.InvariantViolation("account.refund_exceeds_paid",
			"refunds would total %s against %s collected", refunded, a.PaidTotal).
			WithDetail("refunded_after", refunded.Int64()).
			WithDetail("paid_total", a.PaidTotal.Int64())
	}
	a.RefundedTotal = refunded
	a.RecomputeStatus(at)
	return nil
}

// ReversePayment updates the cached totals when a payment is voided. A void is
// not a refund: it removes the collection from the record entirely rather than
// recording money returned against money kept.
func (a *Account) ReversePayment(amount money.Amount, at time.Time) error {
	paid, err := a.PaidTotal.Sub(amount)
	if err != nil {
		return shared.Internal("account.paid_underflow", err, "reversing a payment")
	}
	if paid.IsNegative() {
		return shared.InvariantViolation("account.void_exceeds_paid",
			"voiding %s would leave the account with negative collections", amount)
	}
	a.PaidTotal = paid
	a.RecomputeStatus(at)
	return nil
}

// ApplyAdjustment posts a signed change to what is owed.
func (a *Account) ApplyAdjustment(amount money.Amount, at time.Time) error {
	total, err := a.AdjustmentTotal.Add(amount)
	if err != nil {
		return shared.Internal("account.adjustment_overflow", err, "adding an adjustment")
	}
	next, err := a.NetTotal.Add(total)
	if err != nil {
		return shared.Internal("account.adjustment_overflow", err, "computing the adjusted net")
	}
	if next.IsNegative() {
		return shared.PreconditionFailed("account.adjustment_below_zero",
			"this adjustment of %s would take the amount owed below zero (net %s, adjustments %s)",
			amount, a.NetTotal, a.AdjustmentTotal).
			WithDetail("net_total", a.NetTotal.Int64()).
			WithDetail("current_adjustments", a.AdjustmentTotal.Int64()).
			WithDetail("requested", amount.Int64())
	}
	a.AdjustmentTotal = total
	a.RecomputeStatus(at)
	return nil
}

// AddCredit increases the account's unspent credit balance.
func (a *Account) AddCredit(amount money.Amount) error {
	credit, err := a.CreditBalance.Add(amount)
	if err != nil {
		return shared.Internal("account.credit_overflow", err, "adding credit")
	}
	a.CreditBalance = credit
	return nil
}

// ConsumeCredit reduces the credit balance, refusing to spend more than exists.
func (a *Account) ConsumeCredit(amount money.Amount) error {
	credit, err := a.CreditBalance.Sub(amount)
	if err != nil {
		return shared.Internal("account.credit_underflow", err, "consuming credit")
	}
	if credit.IsNegative() {
		return shared.PreconditionFailed("account.insufficient_credit",
			"the account holds %s in credit but %s was requested", a.CreditBalance, amount).
			WithDetail("available", a.CreditBalance.Int64()).
			WithDetail("requested", amount.Int64())
	}
	a.CreditBalance = credit
	return nil
}
