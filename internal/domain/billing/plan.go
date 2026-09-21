package billing

import (
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// InstallmentStatus is the state of one scheduled payment.
//
// There is no "overdue" here on purpose. Overdue is a statement about today,
// derived from the due date and what remains, and storing it would require a
// nightly sweep whose failure leaves the debt misreported.
type InstallmentStatus string

const (
	InstallmentPending       InstallmentStatus = "pending"
	InstallmentPartiallyPaid InstallmentStatus = "partially_paid"
	InstallmentPaid          InstallmentStatus = "paid"
	InstallmentWaived        InstallmentStatus = "waived"
	InstallmentSuperseded    InstallmentStatus = "superseded"
)

// Installment is one scheduled obligation on an account.
type Installment struct {
	ID             shared.ID
	AccountID      shared.ID
	Number         int16
	DueDate        shared.Date
	Amount         money.Amount
	PaidAmount     money.Amount
	Status         InstallmentStatus
	Label          *string
	SupersededByID *shared.ID
	PlanVersion    int16
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Remaining is what is still owed on this installment.
func (i *Installment) Remaining() money.Amount {
	remaining, err := i.Amount.Sub(i.PaidAmount)
	if err != nil {
		return 0
	}
	return remaining.ClampNonNegative()
}

// IsOpen reports whether the installment still expects money.
func (i *Installment) IsOpen() bool {
	switch i.Status {
	case InstallmentPending, InstallmentPartiallyPaid:
		return true
	default:
		return false
	}
}

// IsOverdue reports whether the installment is open and past its due date.
func (i *Installment) IsOverdue(today shared.Date) bool {
	return i.IsOpen() && i.DueDate.Before(today)
}

// ApplyAllocation records money arriving against this installment.
func (i *Installment) ApplyAllocation(amount money.Amount) error {
	paid, err := i.PaidAmount.Add(amount)
	if err != nil {
		return shared.Internal("installment.paid_overflow", err, "allocating to installment %d", i.Number)
	}
	if paid > i.Amount {
		return shared.InvariantViolation("installment.over_allocated",
			"allocating %s to installment %d would take it to %s against an amount of %s",
			amount, i.Number, paid, i.Amount).
			WithDetail("installment_no", i.Number).
			WithDetail("amount", i.Amount.Int64()).
			WithDetail("would_be_paid", paid.Int64())
	}
	i.PaidAmount = paid
	i.recomputeStatus()
	return nil
}

// ReverseAllocation removes money from this installment when a payment is
// refunded or voided.
func (i *Installment) ReverseAllocation(amount money.Amount) error {
	paid, err := i.PaidAmount.Sub(amount)
	if err != nil {
		return shared.Internal("installment.paid_underflow", err, "reversing on installment %d", i.Number)
	}
	if paid.IsNegative() {
		return shared.InvariantViolation("installment.over_reversed",
			"reversing %s from installment %d would take its paid amount below zero", amount, i.Number)
	}
	i.PaidAmount = paid
	i.recomputeStatus()
	return nil
}

func (i *Installment) recomputeStatus() {
	switch i.Status {
	case InstallmentWaived, InstallmentSuperseded:
		return
	}
	switch {
	case i.PaidAmount.IsZero():
		i.Status = InstallmentPending
	case i.PaidAmount >= i.Amount:
		i.Status = InstallmentPaid
	default:
		i.Status = InstallmentPartiallyPaid
	}
}

// TemplateLine is one share of a published installment template.
type TemplateLine struct {
	LineNo        int16
	ShareBP       money.BasisPoints
	DueOffsetDays int
	Label         *string
	// Amount is set when the template was authored in literal amounts rather
	// than percentages. ShareBP is still populated (derived at definition
	// time) so a mid-year re-split, which only ever knows a share of a
	// remainder, keeps working. GeneratePlan prefers Amount over ShareBP when
	// every line on the template carries one.
	Amount *money.Amount
}

// PlanSpec is the input to plan generation.
type PlanSpec struct {
	AccountID   shared.ID
	NetAmount   money.Amount
	YearStart   shared.Date
	Lines       []TemplateLine
	PlanVersion int16
}

// GeneratePlan turns a template and a net amount into concrete installments.
//
// Two properties matter more than anything else here. The shares need not be
// equal — 500,000 / 300,000 / 400,000 / 300,000 is an ordinary Iraqi plan and
// the template expresses it directly. And the parts must sum to exactly the
// net: rounding four shares of a number not divisible by four leaves a few
// dinars unaccounted for, and a plan that does not add up is a reconciliation
// failure waiting for the end of the year.
//
// A net of zero produces no plan at all. A fully exempt student with four
// zero-value installments would appear on every overdue report and in every
// cashier's worklist, chasing nothing.
func GeneratePlan(spec PlanSpec) ([]*Installment, error) {
	if spec.NetAmount.IsNegative() {
		return nil, shared.Validation("plan.negative_net",
			"cannot generate a plan for a negative amount (%s)", spec.NetAmount)
	}
	if spec.NetAmount.IsZero() {
		return nil, nil
	}
	if len(spec.Lines) == 0 {
		return nil, shared.Validation("plan.no_template_lines",
			"the installment template has no lines")
	}

	amounts, err := amountsFor(spec.Lines, spec.NetAmount)
	if err != nil {
		return nil, err
	}

	version := spec.PlanVersion
	if version < 1 {
		version = 1
	}

	installments := make([]*Installment, 0, len(spec.Lines))
	for i, line := range spec.Lines {
		if amounts[i].IsZero() {
			// A share that rounds to nothing is dropped rather than stored as a
			// zero-value obligation.
			continue
		}
		installments = append(installments, &Installment{
			ID:          shared.NewID(),
			AccountID:   spec.AccountID,
			Number:      int16(len(installments) + 1),
			DueDate:     spec.YearStart.AddDays(line.DueOffsetDays),
			Amount:      amounts[i],
			Status:      InstallmentPending,
			Label:       line.Label,
			PlanVersion: version,
		})
	}

	if err := VerifyPlanSum(installments, spec.NetAmount); err != nil {
		return nil, err
	}
	return installments, nil
}

// amountsFor computes each line's amount against a given net.
//
// Literal amounts win when every line on the template carries one — that is
// what "1,500,000 as 400,000 / 400,000 / 350,000 / 350,000" means, and the
// figures an administrator typed must survive untouched rather than being
// re-derived through a percentage. They still have to sum to exactly net;
// VerifyPlanSum is what catches a mismatch, immediately after this returns,
// with the same refusal a bad percentage template would get.
//
// A template with only some lines carrying an amount cannot reach here in
// practice — the application layer refuses that combination before a
// template is ever published — so a partial set is treated as none and falls
// through to the percentage split, which is always well-defined.
func amountsFor(lines []TemplateLine, net money.Amount) ([]money.Amount, error) {
	literal := true
	for _, line := range lines {
		if line.Amount == nil {
			literal = false
			break
		}
	}
	if literal {
		amounts := make([]money.Amount, len(lines))
		for i, line := range lines {
			amounts[i] = *line.Amount
		}
		return amounts, nil
	}

	weights := make([]money.BasisPoints, len(lines))
	for i, line := range lines {
		weights[i] = line.ShareBP
	}
	// The rounding residual lands on the first share. At generation time every
	// installment is open, so the first is a safe place; a later re-split
	// passes the index of the first still-open row instead.
	amounts, err := money.SplitByRates(net, weights, 0)
	if err != nil {
		return nil, shared.Validation("plan.invalid_template",
			"the template's shares do not form a valid split: %s", err.Error()).WithCause(err)
	}
	return amounts, nil
}

// VerifyPlanSum asserts that the live installments add up to what is owed.
//
// Called as a post-condition of every plan generation and every plan edit. If
// it ever fails, the transaction must abort: a plan that does not sum to the
// net means the student is being asked for the wrong amount, and no downstream
// report can be trusted to notice.
func VerifyPlanSum(installments []*Installment, expected money.Amount) error {
	var total money.Amount
	for _, inst := range installments {
		if inst.Status == InstallmentSuperseded {
			continue
		}
		next, err := total.Add(inst.Amount)
		if err != nil {
			return shared.Internal("plan.sum_overflow", err, "summing the installment plan")
		}
		total = next
	}
	if total != expected {
		difference, _ := total.Sub(expected)
		return shared.InvariantViolation("plan.sum_mismatch",
			"the installment plan totals %s but the amount owed is %s (difference %s)",
			total, expected, difference).
			WithDetail("plan_total", total.Int64()).
			WithDetail("expected", expected.Int64()).
			WithDetail("difference", difference.Int64())
	}
	return nil
}

// ResplitSpec describes a change to the unpaid part of an existing plan.
type ResplitSpec struct {
	Existing     []*Installment
	NewNetAmount money.Amount
	Lines        []TemplateLine
	YearStart    shared.Date
	PlanVersion  int16
}

// ResplitResult is the three-way outcome of re-splitting a plan.
//
// All three lists matter to the caller and the earlier two-value signature
// could not express that: the rows to supersede were computed and then
// discarded, so a caller had no way to retire them and the old unpaid rows
// would have survived beside their replacements — the plan would then sum to
// more than the account owes, which is the one invariant a plan has.
type ResplitResult struct {
	// Keep are installments carrying money. They are returned unchanged and
	// must not be written back: their receipts are printed and their
	// allocations point at these exact rows.
	Keep []*Installment
	// Supersede are the unpaid installments the new shares replace. The caller
	// marks these superseded rather than deleting them, so the plan's history
	// stays readable.
	Supersede []*Installment
	// Fresh are the newly computed installments to insert.
	Fresh []*Installment
}

// ResplitUnpaid rebuilds the open part of a plan while leaving settled
// installments untouched.
//
// A student who has paid two of four installments and then receives a discount
// keeps those two exactly as they are — the receipts are printed and the money
// is in the drawer. Only the remainder is redistributed, and the new shares
// must sum to what is left after the paid amounts.
func ResplitUnpaid(spec ResplitSpec) (ResplitResult, error) {
	var keep, replace []*Installment
	var alreadyPaid money.Amount
	for _, inst := range spec.Existing {
		if inst.Status == InstallmentSuperseded {
			continue
		}
		if inst.PaidAmount.IsPositive() {
			// An installment with money against it is kept whole. Reducing it
			// below what has been allocated would leave the allocation
			// pointing at more than the installment is worth.
			keep = append(keep, inst)
			next, addErr := alreadyPaid.Add(inst.Amount)
			if addErr != nil {
				return ResplitResult{}, shared.Internal("resplit.sum_overflow", addErr, "summing settled installments")
			}
			alreadyPaid = next
			continue
		}
		replace = append(replace, inst)
	}

	remainder, err := spec.NewNetAmount.Sub(alreadyPaid)
	if err != nil {
		return ResplitResult{}, shared.Internal("resplit.arithmetic", err, "computing the redistributable remainder")
	}
	if remainder.IsNegative() {
		return ResplitResult{}, shared.PreconditionFailed("resplit.below_settled_amount",
			"the new amount owed (%s) is less than the %s already committed to settled installments; "+
				"the difference must be handled as a credit rather than by reshaping the plan",
			spec.NewNetAmount, alreadyPaid).
			WithDetail("new_net", spec.NewNetAmount.Int64()).
			WithDetail("settled_installment_total", alreadyPaid.Int64())
	}

	if remainder.IsZero() {
		// Nothing left to spread: every open installment is retired and no
		// replacement takes its place. The account is settled by its paid rows.
		return ResplitResult{Keep: keep, Supersede: replace}, nil
	}

	weights := make([]money.BasisPoints, len(spec.Lines))
	for i, line := range spec.Lines {
		weights[i] = line.ShareBP
	}
	amounts, err := money.SplitByRates(remainder, weights, 0)
	if err != nil {
		return ResplitResult{}, shared.Validation("resplit.invalid_template",
			"the replacement shares do not form a valid split: %s", err.Error()).WithCause(err)
	}

	version := spec.PlanVersion
	if version < 1 {
		version = 1
	}

	fresh := make([]*Installment, 0, len(spec.Lines))
	nextNumber := int16(len(keep) + 1)
	for i, line := range spec.Lines {
		if amounts[i].IsZero() {
			continue
		}
		var accountID shared.ID
		if len(spec.Existing) > 0 {
			accountID = spec.Existing[0].AccountID
		}
		fresh = append(fresh, &Installment{
			ID:          shared.NewID(),
			AccountID:   accountID,
			Number:      nextNumber,
			DueDate:     spec.YearStart.AddDays(line.DueOffsetDays),
			Amount:      amounts[i],
			Status:      InstallmentPending,
			Label:       line.Label,
			PlanVersion: version,
		})
		nextNumber++
	}

	combined := append(append([]*Installment{}, keep...), fresh...)
	if err := VerifyPlanSum(combined, spec.NewNetAmount); err != nil {
		return ResplitResult{}, err
	}
	return ResplitResult{Keep: keep, Supersede: replace, Fresh: fresh}, nil
}
