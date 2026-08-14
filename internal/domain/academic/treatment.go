package academic

import (
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// FinancialTreatment says what happens to the money when an enrollment stops
// being active.
//
// It is a required parameter of every status change that ends an enrollment,
// never a consequence inferred from the status. The design says so in as many
// words, and the reason is that both answers are defensible: a university that
// charges a student who withdrew in June has consumed the year's teaching, and
// one that waives the unpaid remainder is being humane about a family that
// could not continue. What is not defensible is that nobody chose, and the
// student finds out which policy applied when a debt appears three years later.
//
// Before this existed, changing a status did nothing financial at all — which
// is the "keep" treatment applied silently to every case, including the ones
// nobody would have chosen it for.
type FinancialTreatment string

const (
	// TreatmentKeep leaves the obligation and the schedule exactly as they are.
	TreatmentKeep FinancialTreatment = "keep"
	// TreatmentWaiveUnpaid writes off what has not been paid. What was paid
	// stays paid: the receipts are printed and the money is in the drawer.
	TreatmentWaiveUnpaid FinancialTreatment = "waive_unpaid"
	// TreatmentWaiveAll reverses the whole obligation. Anything already paid
	// becomes a credit, which the student may carry forward or reclaim in cash
	// through the ordinary refund route.
	TreatmentWaiveAll FinancialTreatment = "waive_all"
	// TreatmentPartial charges a stated amount and waives the rest, which is
	// what a pro-rata withdrawal rule amounts to in practice. The amount is
	// given explicitly rather than as a percentage of something: a percentage
	// stored beside the figure it produced is a second source of truth, and the
	// two disagree the first time a rounding rule changes.
	TreatmentPartial FinancialTreatment = "partial"
)

// AllTreatments lists every treatment, for validation and for a UI to offer.
var AllTreatments = []FinancialTreatment{
	TreatmentKeep, TreatmentWaiveUnpaid, TreatmentWaiveAll, TreatmentPartial,
}

// Valid reports whether the treatment is one the system recognises.
func (t FinancialTreatment) Valid() bool {
	for _, known := range AllTreatments {
		if t == known {
			return true
		}
	}
	return false
}

// ChangesMoney reports whether applying this treatment writes an adjustment.
func (t FinancialTreatment) ChangesMoney() bool { return t != TreatmentKeep && t != "" }

// StatusesRequiringTreatment are the transitions that end an enrollment and
// therefore have to say what happens to its account.
//
// Superseding is deliberately absent: it has its own command, which already
// moves the balance as a visible pair of transfer adjustments, and offering a
// second mechanism there would let one event be settled twice.
var StatusesRequiringTreatment = []EnrollmentStatus{
	StatusDeferred, StatusWithdrawn, StatusDroppedOut, StatusTransferredOut,
}

// RequiresFinancialTreatment reports whether a transition to this status must
// carry a treatment.
func RequiresFinancialTreatment(target EnrollmentStatus) bool {
	for _, status := range StatusesRequiringTreatment {
		if status == target {
			return true
		}
	}
	return false
}

// TreatmentPlan is what a treatment works out to for one account.
//
// The domain computes it; the application writes the adjustment and reshapes
// the plan. Keeping the arithmetic here means it can be tested without a
// database, and it is the arithmetic — not the plumbing — that decides whether
// a student is owed money or owes it.
type TreatmentPlan struct {
	// Waived is the amount to remove from what is owed, as a positive number.
	// The adjustment written from it is negative; the sign lives in one place.
	Waived money.Amount
	// RemainingObligation is what the student still owes after the treatment.
	RemainingObligation money.Amount
	// CreditToStudent is what they have overpaid once the obligation shrinks:
	// money already collected above the new obligation.
	CreditToStudent money.Amount
}

// PlanTreatment computes the effect of a treatment on an account.
//
//   - effectiveNet is what is currently owed (net plus adjustments so far).
//   - paid is what has actually been collected and not refunded.
//   - chargeInstead applies to TreatmentPartial only: the amount the student is
//     to be charged in place of the full obligation.
//
// Two properties matter. Nothing here ever reduces what was paid — the payment
// rows are untouchable and the money is in the drawer, so an obligation that
// falls below the amount collected produces a credit rather than a negative
// payment. And the waiver is expressed as an amount rather than as a new net,
// because account.net_total is frozen for the life of the account and every
// later change to what is owed is a signed adjustment beside it.
func PlanTreatment(t FinancialTreatment, effectiveNet, paid, chargeInstead money.Amount) (TreatmentPlan, error) {
	if !t.Valid() {
		return TreatmentPlan{}, shared.Validation("treatment.unknown",
			"%q is not a financial treatment; use one of %v", t, AllTreatments)
	}
	if effectiveNet.IsNegative() {
		return TreatmentPlan{}, shared.InvariantViolation("treatment.negative_obligation",
			"an account cannot owe a negative amount (%s)", effectiveNet)
	}

	var target money.Amount
	switch t {
	case TreatmentKeep:
		target = effectiveNet
	case TreatmentWaiveUnpaid:
		// What was paid becomes the whole obligation. A student who paid two of
		// four installments and withdrew owes exactly what they have paid, so
		// the account settles rather than carrying a debt nobody will collect
		// and every aging report will keep listing.
		target = money.Min(paid, effectiveNet)
	case TreatmentWaiveAll:
		target = 0
	case TreatmentPartial:
		if chargeInstead.IsNegative() {
			return TreatmentPlan{}, shared.Validation("treatment.negative_charge",
				"the amount to charge cannot be negative (%s)", chargeInstead)
		}
		if chargeInstead > effectiveNet {
			return TreatmentPlan{}, shared.Validation("treatment.charge_exceeds_obligation",
				"charging %s is more than the %s currently owed; raising an obligation is an "+
					"adjustment in its own right, not a withdrawal treatment",
				chargeInstead, effectiveNet).
				WithDetail("charge", chargeInstead.Int64()).
				WithDetail("currently_owed", effectiveNet.Int64())
		}
		target = chargeInstead
	}

	waived, err := effectiveNet.Sub(target)
	if err != nil {
		return TreatmentPlan{}, shared.Internal("treatment.arithmetic", err,
			"computing the amount to waive")
	}

	var credit money.Amount
	if paid > target {
		credit, err = paid.Sub(target)
		if err != nil {
			return TreatmentPlan{}, shared.Internal("treatment.arithmetic", err,
				"computing the credit left after the waiver")
		}
	}

	return TreatmentPlan{
		Waived:              waived.ClampNonNegative(),
		RemainingObligation: target,
		CreditToStudent:     credit,
	}, nil
}

// ClearancePolicy says what an outstanding balance does to a graduation.
//
// Configurable per academic year rather than decided here, because the design
// names it as a policy the university sets and universities change it. A rule
// hard-coded in Go would also make every historical graduation report the rule
// of today rather than the rule that applied to it.
type ClearancePolicy string

const (
	// ClearanceIgnore does not consult the balance at all.
	ClearanceIgnore ClearancePolicy = "ignore"
	// ClearanceWarn records the outstanding amount and allows the graduation.
	ClearanceWarn ClearancePolicy = "warn"
	// ClearanceBlock refuses to complete an enrollment while money is owed,
	// unless somebody with the authority to do so overrides it in writing.
	ClearanceBlock ClearancePolicy = "block"
)

// AllClearancePolicies lists every policy, for validation and for a UI.
var AllClearancePolicies = []ClearancePolicy{ClearanceIgnore, ClearanceWarn, ClearanceBlock}

// Valid reports whether the policy is one the system recognises.
func (p ClearancePolicy) Valid() bool {
	for _, known := range AllClearancePolicies {
		if p == known {
			return true
		}
	}
	return false
}

// ClearanceDecision is the outcome of a graduation clearance check.
type ClearanceDecision struct {
	Policy      ClearancePolicy
	Outstanding money.Amount
	// Cleared reports whether graduation may proceed.
	Cleared bool
	// RequiresOverride reports that the policy blocks and money is owed, so
	// proceeding needs a named authority and a written reason.
	RequiresOverride bool
}

// DecideClearance applies a year's policy to a student's outstanding balance.
func DecideClearance(policy ClearancePolicy, outstanding money.Amount, overridden bool) (ClearanceDecision, error) {
	if policy == "" {
		policy = ClearanceWarn
	}
	if !policy.Valid() {
		return ClearanceDecision{}, shared.Validation("clearance.unknown_policy",
			"%q is not a clearance policy; use one of %v", policy, AllClearancePolicies)
	}

	decision := ClearanceDecision{Policy: policy, Outstanding: outstanding.ClampNonNegative()}
	switch policy {
	case ClearanceIgnore, ClearanceWarn:
		decision.Cleared = true
	case ClearanceBlock:
		if decision.Outstanding.IsZero() {
			decision.Cleared = true
		} else {
			decision.RequiresOverride = true
			decision.Cleared = overridden
		}
	}
	return decision, nil
}
