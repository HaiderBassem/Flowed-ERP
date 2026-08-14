package academic_test

import (
	"testing"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// A student who paid two of four installments and then withdrew owes exactly
// what they have paid. Anything else leaves a debt on an aging report that
// nobody will ever collect, which is how a debt report stops being read.
func TestWaiveUnpaidSettlesTheAccountAtWhatWasPaid(t *testing.T) {
	plan, err := academic.PlanTreatment(academic.TreatmentWaiveUnpaid, 2_000_000, 1_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Waived != 1_000_000 {
		t.Errorf("waived %s, want 1,000,000", plan.Waived)
	}
	if plan.RemainingObligation != 1_000_000 {
		t.Errorf("remaining %s, want the 1,000,000 already paid", plan.RemainingObligation)
	}
	if !plan.CreditToStudent.IsZero() {
		t.Errorf("credit %s, want none: nothing was overpaid", plan.CreditToStudent)
	}
}

// The money is in the drawer and the receipt is printed. Waiving everything
// cannot un-collect it, so the overpayment becomes a credit the student may
// carry forward or reclaim through the ordinary refund route.
func TestWaiveAllTurnsWhatWasPaidIntoCredit(t *testing.T) {
	plan, err := academic.PlanTreatment(academic.TreatmentWaiveAll, 2_000_000, 750_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Waived != 2_000_000 {
		t.Errorf("waived %s, want the whole obligation", plan.Waived)
	}
	if !plan.RemainingObligation.IsZero() {
		t.Errorf("remaining %s, want zero", plan.RemainingObligation)
	}
	if plan.CreditToStudent != 750_000 {
		t.Errorf("credit %s, want the 750,000 already collected", plan.CreditToStudent)
	}
}

func TestKeepChangesNothing(t *testing.T) {
	plan, err := academic.PlanTreatment(academic.TreatmentKeep, 2_000_000, 500_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Waived.IsZero() || plan.RemainingObligation != 2_000_000 || !plan.CreditToStudent.IsZero() {
		t.Errorf("keep must change nothing, got %+v", plan)
	}
	if academic.TreatmentKeep.ChangesMoney() {
		t.Error("keep must not be reported as changing money")
	}
}

// A pro-rata withdrawal rule: charge for the part of the year consumed.
func TestPartialChargesTheStatedAmount(t *testing.T) {
	plan, err := academic.PlanTreatment(academic.TreatmentPartial, 2_000_000, 1_500_000, 800_000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Waived != 1_200_000 {
		t.Errorf("waived %s, want 1,200,000", plan.Waived)
	}
	if plan.RemainingObligation != 800_000 {
		t.Errorf("remaining %s, want the 800,000 charged", plan.RemainingObligation)
	}
	// Paid 1,500,000 against a new obligation of 800,000.
	if plan.CreditToStudent != 700_000 {
		t.Errorf("credit %s, want 700,000", plan.CreditToStudent)
	}
}

// Raising an obligation is an adjustment in its own right, with its own
// approval. Letting a withdrawal do it would give a registrar a way to charge
// somebody more without anyone signing for it.
func TestPartialCannotChargeMoreThanIsOwed(t *testing.T) {
	_, err := academic.PlanTreatment(academic.TreatmentPartial, 1_000_000, 0, 1_500_000)
	if err == nil {
		t.Fatal("charging more than is owed must be refused")
	}
	if code := shared.CodeOf(err); code != "treatment.charge_exceeds_obligation" {
		t.Errorf("code = %q", code)
	}
}

func TestUnknownTreatmentIsRefused(t *testing.T) {
	if _, err := academic.PlanTreatment("forgive", 1_000_000, 0, 0); err == nil {
		t.Fatal("an unrecognised treatment must be refused")
	}
	if academic.FinancialTreatment("forgive").Valid() {
		t.Error("an unrecognised treatment must not validate")
	}
}

// A waiver on an account that has already been reduced below what was paid
// must not produce a negative waiver, which would read as an increase.
func TestWaiveUnpaidOnAnOverpaidAccountWaivesNothing(t *testing.T) {
	plan, err := academic.PlanTreatment(academic.TreatmentWaiveUnpaid, 500_000, 900_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Waived.IsZero() {
		t.Errorf("waived %s, want zero: more was paid than was owed", plan.Waived)
	}
	if plan.RemainingObligation != 500_000 {
		t.Errorf("remaining %s, want the obligation unchanged", plan.RemainingObligation)
	}
	if plan.CreditToStudent != 400_000 {
		t.Errorf("credit %s, want the 400,000 overpayment", plan.CreditToStudent)
	}
}

func TestStatusesThatRequireATreatment(t *testing.T) {
	for _, status := range []academic.EnrollmentStatus{
		academic.StatusDeferred, academic.StatusWithdrawn,
		academic.StatusDroppedOut, academic.StatusTransferredOut,
	} {
		if !academic.RequiresFinancialTreatment(status) {
			t.Errorf("%s ends an enrollment and must require a treatment", status)
		}
	}
	// Superseding moves the balance through its own transfer-adjustment pair.
	// Offering a treatment there would let one event be settled twice.
	for _, status := range []academic.EnrollmentStatus{
		academic.StatusSuperseded, academic.StatusCompleted, academic.StatusActive,
	} {
		if academic.RequiresFinancialTreatment(status) {
			t.Errorf("%s must not require a treatment", status)
		}
	}
}

// ---------------------------------------------------------------------------
// Graduation clearance
// ---------------------------------------------------------------------------

func TestClearanceBlocksADebtorAndClearsEveryoneElse(t *testing.T) {
	cases := []struct {
		name         string
		policy       academic.ClearancePolicy
		outstanding  money.Amount
		overridden   bool
		wantCleared  bool
		wantOverride bool
	}{
		{"ignore with debt", academic.ClearanceIgnore, 500_000, false, true, false},
		{"warn with debt", academic.ClearanceWarn, 500_000, false, true, false},
		{"block with debt", academic.ClearanceBlock, 500_000, false, false, true},
		{"block, debt, overridden", academic.ClearanceBlock, 500_000, true, true, true},
		{"block, nothing owed", academic.ClearanceBlock, 0, false, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := academic.DecideClearance(tc.policy, tc.outstanding, tc.overridden)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Cleared != tc.wantCleared {
				t.Errorf("cleared = %t, want %t", decision.Cleared, tc.wantCleared)
			}
			if decision.RequiresOverride != tc.wantOverride {
				t.Errorf("requires override = %t, want %t", decision.RequiresOverride, tc.wantOverride)
			}
			if decision.Outstanding != tc.outstanding {
				t.Errorf("outstanding = %s, want %s", decision.Outstanding, tc.outstanding)
			}
		})
	}
}

// An unset policy behaves as "warn": it records the debt and lets the
// graduation through. Defaulting to block would refuse every graduation in a
// university that never configured the field, and defaulting to ignore would
// silently discard the check the design asked for.
func TestUnsetClearancePolicyWarns(t *testing.T) {
	decision, err := academic.DecideClearance("", 500_000, false)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Cleared || decision.Policy != academic.ClearanceWarn {
		t.Errorf("decision = %+v, want a cleared warn", decision)
	}
}

func TestUnknownClearancePolicyIsRefused(t *testing.T) {
	if _, err := academic.DecideClearance("maybe", 0, false); err == nil {
		t.Fatal("an unrecognised clearance policy must be refused")
	}
}
