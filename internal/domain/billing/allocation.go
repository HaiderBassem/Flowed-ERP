// Package billing holds the financial account, its installment plan, and the
// arithmetic that decides which installment a payment settles.
package billing

import (
	"sort"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// OpenInstallment is an installment with money still owing, as the allocation
// engine sees it.
type OpenInstallment struct {
	ID        shared.ID
	Number    int16
	DueDate   shared.Date
	Amount    money.Amount
	PaidSoFar money.Amount
}

// Remaining is what is still owed on this installment.
func (i OpenInstallment) Remaining() money.Amount {
	remaining, err := i.Amount.Sub(i.PaidSoFar)
	if err != nil {
		return 0
	}
	return remaining.ClampNonNegative()
}

// Allocation is one line of a payment's distribution across installments.
type Allocation struct {
	InstallmentID shared.ID
	Number        int16
	Amount        money.Amount
}

// AllocationPlan is how a payment will be distributed.
type AllocationPlan struct {
	Allocations []Allocation
	// CreditAmount is the part of the payment that no open installment could
	// absorb. It becomes a credit entry on the account rather than a new
	// installment: inventing an installment to hold an overpayment would put a
	// due date on money the student already handed over.
	CreditAmount money.Amount
	Allocated    money.Amount
}

// AllocatePayment distributes an amount across open installments, oldest due
// date first.
//
// Oldest-first is the only defensible default. Paying the newest installment
// while an older one sits overdue would let a student who pays every month
// still show as delinquent, and would make the aging report describe a debt
// nobody has.
//
// A payment larger than the installment it was aimed at spills forward into
// the next open ones rather than stopping. Whatever survives every open
// installment becomes credit on the account, consumable by a later installment
// or refundable on request.
func AllocatePayment(amount money.Amount, open []OpenInstallment) (*AllocationPlan, error) {
	if !amount.IsPositive() {
		return nil, shared.Validation("payment.non_positive_amount",
			"a payment must be greater than zero, got %s", amount)
	}

	ordered := make([]OpenInstallment, len(open))
	copy(ordered, open)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].DueDate != ordered[j].DueDate {
			return ordered[i].DueDate.Before(ordered[j].DueDate)
		}
		return ordered[i].Number < ordered[j].Number
	})

	plan := &AllocationPlan{Allocations: make([]Allocation, 0, len(ordered))}
	remaining := amount

	for _, inst := range ordered {
		if !remaining.IsPositive() {
			break
		}
		owing := inst.Remaining()
		if !owing.IsPositive() {
			continue
		}

		take := money.Min(remaining, owing)
		plan.Allocations = append(plan.Allocations, Allocation{
			InstallmentID: inst.ID,
			Number:        inst.Number,
			Amount:        take,
		})

		var err error
		if remaining, err = remaining.Sub(take); err != nil {
			return nil, shared.Internal("payment.allocation_arithmetic", err, "reducing the unallocated remainder")
		}
		if plan.Allocated, err = plan.Allocated.Add(take); err != nil {
			return nil, shared.Internal("payment.allocation_arithmetic", err, "accumulating allocated amount")
		}
	}

	plan.CreditAmount = remaining
	return plan, nil
}

// AllocateToSpecific distributes a payment across installments the cashier
// chose explicitly, used when a student insists on settling a particular
// instalment out of order.
func AllocateToSpecific(amount money.Amount, targets []Allocation, open []OpenInstallment) (*AllocationPlan, error) {
	if !amount.IsPositive() {
		return nil, shared.Validation("payment.non_positive_amount",
			"a payment must be greater than zero, got %s", amount)
	}

	byID := make(map[shared.ID]OpenInstallment, len(open))
	for _, inst := range open {
		byID[inst.ID] = inst
	}

	plan := &AllocationPlan{Allocations: make([]Allocation, 0, len(targets))}
	for _, target := range targets {
		inst, known := byID[target.InstallmentID]
		if !known {
			return nil, shared.Validation("payment.unknown_installment",
				"installment %s is not open on this account", target.InstallmentID).
				WithDetail("installment_id", target.InstallmentID.String())
		}
		if !target.Amount.IsPositive() {
			return nil, shared.Validation("payment.non_positive_allocation",
				"the allocation to installment %d must be greater than zero", inst.Number)
		}
		if target.Amount > inst.Remaining() {
			return nil, shared.Validation("payment.allocation_exceeds_installment",
				"allocating %s to installment %d exceeds the %s still owed on it",
				target.Amount, inst.Number, inst.Remaining()).
				WithDetail("installment_no", inst.Number).
				WithDetail("requested", target.Amount.Int64()).
				WithDetail("remaining", inst.Remaining().Int64())
		}

		plan.Allocations = append(plan.Allocations, Allocation{
			InstallmentID: inst.ID,
			Number:        inst.Number,
			Amount:        target.Amount,
		})
		var err error
		if plan.Allocated, err = plan.Allocated.Add(target.Amount); err != nil {
			return nil, shared.Internal("payment.allocation_arithmetic", err, "accumulating allocated amount")
		}
	}

	if plan.Allocated > amount {
		return nil, shared.Validation("payment.allocation_exceeds_payment",
			"the requested allocations total %s but the payment is only %s", plan.Allocated, amount).
			WithDetail("allocated", plan.Allocated.Int64()).
			WithDetail("payment_amount", amount.Int64())
	}

	credit, err := amount.Sub(plan.Allocated)
	if err != nil {
		return nil, shared.Internal("payment.allocation_arithmetic", err, "computing the credit remainder")
	}
	plan.CreditAmount = credit
	return plan, nil
}

// ExistingAllocation is a posted allocation that a refund or void may unwind.
type ExistingAllocation struct {
	ID            shared.ID
	InstallmentID shared.ID
	Number        int16
	DueDate       shared.Date
	Amount        money.Amount
	// ReversedAmount is how much of this allocation earlier refunds already
	// took back. Partial reversals are permitted, so this is a running total
	// rather than a flag.
	ReversedAmount money.Amount
	// Reversed marks an allocation already fully unwound.
	Reversed bool
}

// Available is how much of this allocation may still be unwound.
func (a ExistingAllocation) Available() money.Amount {
	if a.Reversed {
		return 0
	}
	available, err := a.Amount.Sub(a.ReversedAmount)
	if err != nil {
		return 0
	}
	return available.ClampNonNegative()
}

// Reversal is one line of an unwinding.
type Reversal struct {
	AllocationID  shared.ID
	InstallmentID shared.ID
	Number        int16
	Amount        money.Amount
}

// ReversalPlan is how a refund or void will be unwound.
type ReversalPlan struct {
	Reversals []Reversal
	// CreditConsumed is the part taken from credit balance rather than from
	// money that reached an installment.
	CreditConsumed money.Amount
	Total          money.Amount
}

// PlanReversal decides which allocations a refund unwinds.
//
// The critical constraint is the one this signature enforces by construction:
// the caller passes only allocations belonging to the payment being refunded,
// and the plan touches nothing else.
//
// An earlier design reversed "the latest installments" across the account.
// That is subtly catastrophic. Suppose payment A funded installments one and
// two, and payment B funded installment three. Refunding part of payment A
// under a latest-first rule strips installment three — funding A never
// provided — leaving A's allocations overstated and B's understated. Voiding
// B afterwards then reverses installment three a second time and hands back
// cash for money that was already returned. Scoping every reversal to its own
// payment's allocations removes the possibility.
//
// Within that scope the order is newest due date first: refunding should
// reopen the obligation furthest in the future, leaving the student's oldest
// commitments settled.
func PlanReversal(amount money.Amount, availableCredit money.Amount, allocations []ExistingAllocation) (*ReversalPlan, error) {
	if !amount.IsPositive() {
		return nil, shared.Validation("refund.non_positive_amount",
			"a refund must be greater than zero, got %s", amount)
	}

	plan := &ReversalPlan{Reversals: make([]Reversal, 0, len(allocations))}
	remaining := amount

	// Credit first: money that never reached an installment is the cleanest
	// thing to hand back, and doing so leaves the payment schedule untouched.
	if availableCredit.IsPositive() {
		take := money.Min(remaining, availableCredit)
		plan.CreditConsumed = take
		var err error
		if remaining, err = remaining.Sub(take); err != nil {
			return nil, shared.Internal("refund.arithmetic", err, "consuming credit balance")
		}
	}

	live := make([]ExistingAllocation, 0, len(allocations))
	for _, a := range allocations {
		if a.Available().IsPositive() {
			live = append(live, a)
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].DueDate != live[j].DueDate {
			return live[i].DueDate.After(live[j].DueDate)
		}
		return live[i].Number > live[j].Number
	})

	for _, alloc := range live {
		if !remaining.IsPositive() {
			break
		}
		// An allocation may be unwound in part.
		//
		// An earlier version reversed whole allocations only, which reads as
		// safer but makes ordinary refunds impossible: a student who paid
		// 1,100,000 and wants 300,000 back cannot be served if every
		// allocation happens to be larger than what is left to unwind. The
		// protection against two refunds unwinding the same money is the lock
		// on the account, which serialises every refund touching it, plus the
		// check that reversals against one allocation never exceed it.
		take := money.Min(remaining, alloc.Available())
		if !take.IsPositive() {
			continue
		}

		plan.Reversals = append(plan.Reversals, Reversal{
			AllocationID:  alloc.ID,
			InstallmentID: alloc.InstallmentID,
			Number:        alloc.Number,
			Amount:        take,
		})
		var err error
		if remaining, err = remaining.Sub(take); err != nil {
			return nil, shared.Internal("refund.arithmetic", err, "reducing the unreversed remainder")
		}
	}

	if remaining.IsPositive() {
		return nil, shared.PreconditionFailed("refund.exceeds_allocated",
			"cannot unwind %s: only %s of this payment remains attached to installments or credit",
			amount, amount.MustAdd(-remaining)).
			WithDetail("requested", amount.Int64()).
			WithDetail("unmatched", remaining.Int64())
	}

	total, err := amount.Sub(0)
	if err != nil {
		return nil, shared.Internal("refund.arithmetic", err, "computing the reversal total")
	}
	plan.Total = total
	return plan, nil
}

// PlanFullReversal unwinds every live allocation of a payment, used by void.
func PlanFullReversal(allocations []ExistingAllocation, creditToRelease money.Amount) (*ReversalPlan, error) {
	plan := &ReversalPlan{
		Reversals:      make([]Reversal, 0, len(allocations)),
		CreditConsumed: creditToRelease,
	}
	total := creditToRelease

	for _, alloc := range allocations {
		available := alloc.Available()
		if !available.IsPositive() {
			continue
		}
		plan.Reversals = append(plan.Reversals, Reversal{
			AllocationID:  alloc.ID,
			InstallmentID: alloc.InstallmentID,
			Number:        alloc.Number,
			Amount:        available,
		})
		next, err := total.Add(available)
		if err != nil {
			return nil, shared.Internal("void.arithmetic", err, "totalling the full reversal")
		}
		total = next
	}

	plan.Total = total
	return plan, nil
}
