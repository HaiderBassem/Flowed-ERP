package billing_test

import (
	"testing"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

func openInstallment(number int16, day int, amount, paid money.Amount) billing.OpenInstallment {
	return billing.OpenInstallment{
		ID:        shared.NewID(),
		Number:    number,
		DueDate:   shared.NewDate(2025, 10, day),
		Amount:    amount,
		PaidSoFar: paid,
	}
}

// The worked example from the design: a plan of 400,000 / 300,000 / 300,000
// settled by two payments, the second of which spills across three
// installments.
func TestPaymentSpillsForwardAcrossInstallments(t *testing.T) {
	plan := []billing.OpenInstallment{
		openInstallment(1, 1, 400_000, 0),
		openInstallment(2, 15, 300_000, 0),
		openInstallment(3, 28, 300_000, 0),
	}

	first, err := billing.AllocatePayment(250_000, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Allocations) != 1 || first.Allocations[0].Amount != 250_000 {
		t.Fatalf("a partial payment should touch one installment: %+v", first.Allocations)
	}
	if first.CreditAmount != 0 {
		t.Errorf("credit = %s, want 0", first.CreditAmount)
	}

	// 150,000 finishes the first, 300,000 clears the second, 50,000 starts the third.
	plan[0].PaidSoFar = 250_000
	second, err := billing.AllocatePayment(500_000, plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []money.Amount{150_000, 300_000, 50_000}
	if len(second.Allocations) != 3 {
		t.Fatalf("expected the payment to spill across three installments, got %d", len(second.Allocations))
	}
	for i, allocation := range second.Allocations {
		if allocation.Amount != want[i] {
			t.Errorf("allocation %d = %s, want %s", i+1, allocation.Amount, want[i])
		}
	}
	if second.CreditAmount != 0 {
		t.Errorf("credit = %s, want 0", second.CreditAmount)
	}
}

// Money beyond every open installment becomes credit rather than a fabricated
// extra installment with a due date on money already handed over.
func TestOverpaymentBecomesCreditNotANewInstallment(t *testing.T) {
	plan := []billing.OpenInstallment{
		openInstallment(1, 1, 200_000, 0),
		openInstallment(2, 15, 300_000, 0),
	}

	result, err := billing.AllocatePayment(750_000, plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Allocated != 500_000 {
		t.Errorf("allocated = %s, want 500,000", result.Allocated)
	}
	if result.CreditAmount != 250_000 {
		t.Errorf("credit = %s, want 250,000", result.CreditAmount)
	}
	if len(result.Allocations) != 2 {
		t.Errorf("expected exactly two allocations, got %d", len(result.Allocations))
	}
}

// Oldest first, regardless of the order the installments arrive in. Paying the
// newest while an older one sits overdue would make a diligent student look
// delinquent and would corrupt every aging report.
func TestAllocationTakesOldestDueDateFirst(t *testing.T) {
	plan := []billing.OpenInstallment{
		openInstallment(3, 28, 300_000, 0),
		openInstallment(1, 1, 300_000, 0),
		openInstallment(2, 15, 300_000, 0),
	}

	result, err := billing.AllocatePayment(450_000, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Allocations) != 2 {
		t.Fatalf("expected two allocations, got %d", len(result.Allocations))
	}
	if result.Allocations[0].Number != 1 {
		t.Errorf("first allocation went to installment %d, want 1", result.Allocations[0].Number)
	}
	if result.Allocations[1].Number != 2 {
		t.Errorf("second allocation went to installment %d, want 2", result.Allocations[1].Number)
	}
	if result.Allocations[1].Amount != 150_000 {
		t.Errorf("spillover = %s, want 150,000", result.Allocations[1].Amount)
	}
}

func TestFullyPaidInstallmentsAreSkipped(t *testing.T) {
	plan := []billing.OpenInstallment{
		openInstallment(1, 1, 300_000, 300_000), // already settled
		openInstallment(2, 15, 300_000, 0),
	}

	result, err := billing.AllocatePayment(100_000, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Allocations) != 1 {
		t.Fatalf("expected one allocation, got %d", len(result.Allocations))
	}
	if result.Allocations[0].Number != 2 {
		t.Errorf("allocation went to installment %d, want 2", result.Allocations[0].Number)
	}
}

func TestAllocationRejectsNonPositiveAmounts(t *testing.T) {
	plan := []billing.OpenInstallment{openInstallment(1, 1, 300_000, 0)}
	for _, amount := range []money.Amount{0, -1, -500_000} {
		if _, err := billing.AllocatePayment(amount, plan); err == nil {
			t.Errorf("AllocatePayment accepted %s", amount)
		}
	}
}

func TestManualAllocationRefusesToExceedAnInstallment(t *testing.T) {
	plan := []billing.OpenInstallment{openInstallment(1, 1, 300_000, 100_000)}
	targets := []billing.Allocation{{InstallmentID: plan[0].ID, Amount: 250_000}}

	_, err := billing.AllocateToSpecific(500_000, targets, plan)
	if err == nil {
		t.Fatal("allocating more than an installment owes must be rejected")
	}
	if code := shared.CodeOf(err); code != "payment.allocation_exceeds_installment" {
		t.Errorf("error code = %q, want payment.allocation_exceeds_installment", code)
	}
}

func TestManualAllocationRefusesToExceedThePayment(t *testing.T) {
	first := openInstallment(1, 1, 300_000, 0)
	second := openInstallment(2, 15, 300_000, 0)
	plan := []billing.OpenInstallment{first, second}
	targets := []billing.Allocation{
		{InstallmentID: first.ID, Amount: 300_000},
		{InstallmentID: second.ID, Amount: 300_000},
	}

	if _, err := billing.AllocateToSpecific(400_000, targets, plan); err == nil {
		t.Fatal("allocating more than the payment carries must be rejected")
	}
}

// The scoping rule that prevents a double payout. A refund may only unwind
// allocations belonging to its own payment; the caller supplies exactly those,
// and nothing in the plan can reach another payment's funding.
func TestReversalTakesNewestDueDateFirstWithinItsOwnPayment(t *testing.T) {
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 200_000},
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 2,
			DueDate: shared.NewDate(2025, 11, 1), Amount: 200_000},
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 3,
			DueDate: shared.NewDate(2025, 12, 1), Amount: 200_000},
	}

	plan, err := billing.PlanReversal(400_000, 0, allocations)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Reversals) != 2 {
		t.Fatalf("expected two reversals, got %d", len(plan.Reversals))
	}
	if plan.Reversals[0].Number != 3 {
		t.Errorf("first reversal hit installment %d, want 3 (newest due date)", plan.Reversals[0].Number)
	}
	if plan.Reversals[1].Number != 2 {
		t.Errorf("second reversal hit installment %d, want 2", plan.Reversals[1].Number)
	}
}

func TestReversalConsumesCreditBeforeTouchingInstallments(t *testing.T) {
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 300_000},
	}

	plan, err := billing.PlanReversal(150_000, 200_000, allocations)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CreditConsumed != 150_000 {
		t.Errorf("credit consumed = %s, want 150,000", plan.CreditConsumed)
	}
	if len(plan.Reversals) != 0 {
		t.Errorf("credit should have covered the refund; %d installment reversals were planned", len(plan.Reversals))
	}
}

func TestReversalSkipsAllocationsAlreadyUnwound(t *testing.T) {
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 2,
			DueDate: shared.NewDate(2025, 11, 1), Amount: 200_000, Reversed: true},
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 200_000},
	}

	plan, err := billing.PlanReversal(200_000, 0, allocations)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Reversals) != 1 {
		t.Fatalf("expected one reversal, got %d", len(plan.Reversals))
	}
	if plan.Reversals[0].Number != 1 {
		t.Errorf("reversal hit installment %d; the already-reversed one must be skipped", plan.Reversals[0].Number)
	}
}

// A refund smaller than the allocation it lands on unwinds part of it.
//
// Whole-allocation-only reversal reads as safer but makes ordinary refunds
// impossible: a student who paid 1,100,000 and wants 300,000 back cannot be
// served if every allocation happens to be larger than what is left to unwind.
// Two refunds cannot race each other over the same allocation because both
// hold the account's row lock.
func TestReversalMayUnwindPartOfAnAllocation(t *testing.T) {
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 200_000},
	}

	plan, err := billing.PlanReversal(150_000, 0, allocations)
	if err != nil {
		t.Fatalf("a partial reversal must be permitted: %v", err)
	}
	if len(plan.Reversals) != 1 {
		t.Fatalf("expected one reversal, got %d", len(plan.Reversals))
	}
	if plan.Reversals[0].Amount != 150_000 {
		t.Errorf("reversal = %s, want 150,000", plan.Reversals[0].Amount)
	}
}

func TestReversalRespectsWhatEarlierRefundsAlreadyTookBack(t *testing.T) {
	// 120,000 of this 200,000 allocation is already gone, so only 80,000 is
	// left to unwind.
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 200_000, ReversedAmount: 120_000},
	}

	plan, err := billing.PlanReversal(80_000, 0, allocations)
	if err != nil {
		t.Fatalf("unwinding the remaining 80,000 must be permitted: %v", err)
	}
	if plan.Reversals[0].Amount != 80_000 {
		t.Errorf("reversal = %s, want 80,000", plan.Reversals[0].Amount)
	}

	if _, err := billing.PlanReversal(80_001, 0, allocations); err == nil {
		t.Fatal("unwinding more than remains against the allocation must be refused")
	} else if code := shared.CodeOf(err); code != "refund.exceeds_allocated" {
		t.Errorf("error code = %q, want refund.exceeds_allocated", code)
	}
}

func TestReversalRefusesMoreThanThePaymentPlaced(t *testing.T) {
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 200_000},
	}

	_, err := billing.PlanReversal(500_000, 0, allocations)
	if err == nil {
		t.Fatal("unwinding more than the payment placed must be refused")
	}
	if code := shared.CodeOf(err); code != "refund.exceeds_allocated" {
		t.Errorf("error code = %q, want refund.exceeds_allocated", code)
	}
}

func TestFullReversalUnwindsEverythingLive(t *testing.T) {
	allocations := []billing.ExistingAllocation{
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 1,
			DueDate: shared.NewDate(2025, 10, 1), Amount: 200_000},
		{ID: shared.NewID(), InstallmentID: shared.NewID(), Number: 2,
			DueDate: shared.NewDate(2025, 11, 1), Amount: 300_000},
	}

	plan, err := billing.PlanFullReversal(allocations, 50_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Reversals) != 2 {
		t.Fatalf("expected two reversals, got %d", len(plan.Reversals))
	}
	if plan.Total != 550_000 {
		t.Errorf("total unwound = %s, want 550,000 (both allocations plus released credit)", plan.Total)
	}
}
