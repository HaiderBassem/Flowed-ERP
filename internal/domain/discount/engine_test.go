package discount_test

import (
	"testing"

	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

func tuitionOnly(amount money.Amount) []discount.ComponentAmount {
	return []discount.ComponentAmount{
		{Code: "TUITION", Amount: amount, Discountable: true},
	}
}

func percentGrant(code string, bp int32, priority int16) discount.Candidate {
	return discount.Candidate{
		AssignmentID:        shared.NewID(),
		DefinitionID:        shared.NewID(),
		DefinitionVersionID: shared.NewID(),
		DefinitionCode:      code,
		ValueType:           discount.ValuePercentage,
		Rate:                money.BasisPoints(bp),
		Stackable:           true,
		Priority:            priority,
	}
}

func fixedGrant(code string, amount money.Amount, priority int16) discount.Candidate {
	return discount.Candidate{
		AssignmentID:        shared.NewID(),
		DefinitionID:        shared.NewID(),
		DefinitionVersionID: shared.NewID(),
		DefinitionCode:      code,
		ValueType:           discount.ValueFixed,
		FixedAmount:         amount,
		Stackable:           true,
		Priority:            priority,
	}
}

// The worked example from the domain design: a two-million dinar fee, a ten
// percent grant and a flat 100,000 grant. Percentages compute against the
// original base, so the answer is 1,700,000 whichever order the grants arrive
// in. Sequential application would give 1,710,000 one way and 1,700,000 the
// other, and a number that depends on evaluation order cannot be defended at a
// cashier's window.
func TestStackingIsOrderIndependent(t *testing.T) {
	components := tuitionOnly(2_000_000)
	a := percentGrant("TEACHERS_CHILD", 1000, 10)
	b := fixedGrant("SOCIAL", 100_000, 20)

	forward, err := discount.Compute(discount.Input{
		Components: components, Candidates: []discount.Candidate{a, b}, MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := discount.Compute(discount.Input{
		Components: components, Candidates: []discount.Candidate{b, a}, MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if forward.NetTotal != 1_700_000 {
		t.Errorf("net = %s, want 1,700,000", forward.NetTotal)
	}
	if forward.NetTotal != reverse.NetTotal {
		t.Errorf("order changed the outcome: %s forward, %s reverse", forward.NetTotal, reverse.NetTotal)
	}
	if forward.TotalDiscount != 300_000 {
		t.Errorf("total discount = %s, want 300,000", forward.TotalDiscount)
	}
}

// Two percentage grants must both compute against the original base. Applied
// sequentially, "fifteen percent" would silently become fifteen percent of
// what a previous grant left, which is not what any ministry circular means.
func TestTwoPercentagesBothComputeAgainstTheOriginalBase(t *testing.T) {
	result, err := discount.Compute(discount.Input{
		Components: tuitionOnly(2_000_000),
		Candidates: []discount.Candidate{
			percentGrant("A", 1000, 10), // 200,000
			percentGrant("B", 1500, 20), // 300,000
		},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalDiscount != 500_000 {
		t.Errorf("total discount = %s, want 500,000 (sequential would give 470,000)", result.TotalDiscount)
	}
	if result.NetTotal != 1_500_000 {
		t.Errorf("net = %s, want 1,500,000", result.NetTotal)
	}
}

// The floor is the non-discountable remainder, not zero. Registration and card
// fees are collected from everyone, including a fully exempt student.
func TestDiscountsCannotEatNonDiscountableComponents(t *testing.T) {
	components := []discount.ComponentAmount{
		{Code: "TUITION", Amount: 300_000, Discountable: true},
		{Code: "REGISTRATION", Amount: 150_000, Discountable: false},
		{Code: "ID_CARD", Amount: 50_000, Discountable: false},
	}

	result, err := discount.Compute(discount.Input{
		Components: components,
		// A flat grant far larger than the discountable base.
		Candidates:         []discount.Candidate{fixedGrant("BIG", 500_000, 10)},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.DiscountableBase != 300_000 {
		t.Fatalf("discountable base = %s, want 300,000", result.DiscountableBase)
	}
	if result.TotalDiscount != 300_000 {
		t.Errorf("discount = %s, want 300,000 — it must not reach the non-discountable fees", result.TotalDiscount)
	}
	if result.NetTotal != 200_000 {
		t.Errorf("net = %s, want 200,000 (the registration and card fees survive)", result.NetTotal)
	}
}

// A full exemption clears tuition and leaves the rest.
func TestFullExemptionLeavesNonDiscountableFeesPayable(t *testing.T) {
	components := []discount.ComponentAmount{
		{Code: "TUITION", Amount: 1_500_000, Discountable: true},
		{Code: "ID_CARD", Amount: 25_000, Discountable: false},
	}
	exemption := percentGrant("FULL_EXEMPTION", int32(money.FullRate), 1)
	exemption.IsFullExemption = true
	exemption.Stackable = false

	result, err := discount.Compute(discount.Input{
		Components:         components,
		Candidates:         []discount.Candidate{exemption},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalDiscount != 1_500_000 {
		t.Errorf("discount = %s, want 1,500,000", result.TotalDiscount)
	}
	if result.NetTotal != 25_000 {
		t.Errorf("net = %s, want 25,000 — the identity card fee is still collected", result.NetTotal)
	}
}

// Over-discounting truncates the grant that crosses the line and records why,
// rather than letting the total drift past the base.
func TestOverDiscountTruncatesAndRecordsTheReason(t *testing.T) {
	result, err := discount.Compute(discount.Input{
		Components: tuitionOnly(1_000_000),
		Candidates: []discount.Candidate{
			percentGrant("A", 6000, 10), // 600,000
			percentGrant("B", 5000, 20), // 500,000, but only 400,000 fits
		},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.TotalDiscount != 1_000_000 {
		t.Errorf("total discount = %s, want 1,000,000", result.TotalDiscount)
	}
	if result.NetTotal != 0 {
		t.Errorf("net = %s, want 0", result.NetTotal)
	}

	second := result.Applications[1]
	if second.ComputedAmount != 500_000 {
		t.Errorf("second grant computed %s, want 500,000 — the computed figure must survive truncation",
			second.ComputedAmount)
	}
	if second.AppliedAmount != 400_000 {
		t.Errorf("second grant applied %s, want 400,000", second.AppliedAmount)
	}
	if second.TruncationReason == nil {
		t.Error("a truncated grant must record why it was truncated")
	}
}

func TestPolicyCapLimitsTheTotal(t *testing.T) {
	result, err := discount.Compute(discount.Input{
		Components: tuitionOnly(2_000_000),
		Candidates: []discount.Candidate{
			percentGrant("A", 3000, 10), // 600,000
			percentGrant("B", 3000, 20), // 600,000, but the cap allows only 400,000 more
		},
		MaxTotalDiscountBP: 5000, // half the base, 1,000,000
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalDiscount != 1_000_000 {
		t.Errorf("total discount = %s, want 1,000,000 (capped at fifty percent)", result.TotalDiscount)
	}
	if reason := result.Applications[1].TruncationReason; reason == nil || *reason != discount.TruncatedByTotalCap {
		t.Errorf("second grant should record the total cap as its truncation reason, got %v", reason)
	}
}

func TestPerApplicationCapAppliesBeforeTheTotalCap(t *testing.T) {
	grant := percentGrant("CAPPED", 5000, 10) // would be 1,000,000
	capValue := money.Amount(250_000)
	grant.PerApplicationCap = &capValue

	result, err := discount.Compute(discount.Input{
		Components:         tuitionOnly(2_000_000),
		Candidates:         []discount.Candidate{grant},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalDiscount != 250_000 {
		t.Errorf("discount = %s, want 250,000", result.TotalDiscount)
	}
	if reason := result.Applications[0].TruncationReason; reason == nil || *reason != discount.TruncatedByPerApplicationCap {
		t.Errorf("expected a per-application cap truncation, got %v", reason)
	}
}

func TestNonStackableGrantRefusesToShare(t *testing.T) {
	exclusive := percentGrant("EXCLUSIVE", 5000, 10)
	exclusive.Stackable = false

	_, err := discount.Compute(discount.Input{
		Components:         tuitionOnly(1_000_000),
		Candidates:         []discount.Candidate{exclusive, percentGrant("OTHER", 1000, 20)},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err == nil {
		t.Fatal("a non-stackable grant alongside another must be rejected")
	}
	if code := shared.CodeOf(err); code != "discount.not_stackable" {
		t.Errorf("error code = %q, want discount.not_stackable", code)
	}
}

func TestExclusivityGroupAllowsOnlyOneMember(t *testing.T) {
	group := shared.NewID()
	a := percentGrant("SOCIAL_A", 1000, 10)
	a.ExclusivityGroupID = &group
	b := percentGrant("SOCIAL_B", 2000, 20)
	b.ExclusivityGroupID = &group

	_, err := discount.Compute(discount.Input{
		Components:         tuitionOnly(1_000_000),
		Candidates:         []discount.Candidate{a, b},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err == nil {
		t.Fatal("two grants in the same exclusivity group must be rejected")
	}
	if code := shared.CodeOf(err); code != "discount.exclusivity_conflict" {
		t.Errorf("error code = %q, want discount.exclusivity_conflict", code)
	}
}

func TestGrantTargetingSpecificComponentsUsesOnlyThoseAsItsBase(t *testing.T) {
	components := []discount.ComponentAmount{
		{Code: "TUITION", Amount: 1_000_000, Discountable: true},
		{Code: "LAB", Amount: 400_000, Discountable: true},
	}
	grant := percentGrant("TUITION_ONLY", 5000, 10)
	grant.AppliesToComponents = []string{"TUITION"}

	result, err := discount.Compute(discount.Input{
		Components:         components,
		Candidates:         []discount.Candidate{grant},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Applications[0].FrozenBase != 1_000_000 {
		t.Errorf("frozen base = %s, want 1,000,000 (the lab fee is out of scope)",
			result.Applications[0].FrozenBase)
	}
	if result.TotalDiscount != 500_000 {
		t.Errorf("discount = %s, want 500,000", result.TotalDiscount)
	}
	if result.NetTotal != 900_000 {
		t.Errorf("net = %s, want 900,000", result.NetTotal)
	}
}

func TestNoGrantsLeavesTheFeeIntact(t *testing.T) {
	result, err := discount.Compute(discount.Input{
		Components:         tuitionOnly(1_500_000),
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.NetTotal != 1_500_000 || result.TotalDiscount != 0 {
		t.Errorf("net = %s, discount = %s; want 1,500,000 and 0", result.NetTotal, result.TotalDiscount)
	}
}

// The historical-integrity guarantee, demonstrated. Last year's numbers are
// reproduced from last year's frozen inputs, and this year's revision of the
// same discount cannot reach them.
func TestFrozenInputsReproduceHistoricalAmounts(t *testing.T) {
	lastYear := percentGrant("TEACHERS_CHILD", 2000, 10) // v3: twenty percent
	thisYear := percentGrant("TEACHERS_CHILD", 2500, 10) // v4: twenty-five percent

	before, err := discount.Compute(discount.Input{
		Components: tuitionOnly(2_000_000), Candidates: []discount.Candidate{lastYear},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := discount.Compute(discount.Input{
		Components: tuitionOnly(2_000_000), Candidates: []discount.Candidate{thisYear},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if before.TotalDiscount != 400_000 {
		t.Errorf("last year's discount = %s, want 400,000", before.TotalDiscount)
	}
	if after.TotalDiscount != 500_000 {
		t.Errorf("this year's discount = %s, want 500,000", after.TotalDiscount)
	}

	// Recomputing last year with last year's version still gives last year's
	// answer: the engine has no notion of a "current" value to drift toward.
	recomputed, err := discount.Compute(discount.Input{
		Components: tuitionOnly(2_000_000), Candidates: []discount.Candidate{lastYear},
		MaxTotalDiscountBP: money.FullRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recomputed.TotalDiscount != before.TotalDiscount {
		t.Errorf("recomputing last year gave %s, originally %s", recomputed.TotalDiscount, before.TotalDiscount)
	}
}
