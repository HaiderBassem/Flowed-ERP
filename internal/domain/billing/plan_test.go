package billing_test

import (
	"testing"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

func quarterlyTemplate() []billing.TemplateLine {
	return []billing.TemplateLine{
		{LineNo: 1, ShareBP: 2500, DueOffsetDays: 0},
		{LineNo: 2, ShareBP: 2500, DueOffsetDays: 60},
		{LineNo: 3, ShareBP: 2500, DueOffsetDays: 120},
		{LineNo: 4, ShareBP: 2500, DueOffsetDays: 180},
	}
}

// An Iraqi plan is routinely uneven — a larger payment at registration, then
// smaller ones. The template expresses that directly rather than dividing by a
// count.
func unevenTemplate() []billing.TemplateLine {
	return []billing.TemplateLine{
		{LineNo: 1, ShareBP: 3334, DueOffsetDays: 0},
		{LineNo: 2, ShareBP: 2000, DueOffsetDays: 60},
		{LineNo: 3, ShareBP: 2666, DueOffsetDays: 120},
		{LineNo: 4, ShareBP: 2000, DueOffsetDays: 180},
	}
}

func TestGeneratedPlanAlwaysSumsToTheNet(t *testing.T) {
	yearStart := shared.NewDate(2025, 9, 1)
	nets := []money.Amount{1, 999, 1_000_000, 1_000_001, 1_500_000, 2_000_003, 987_654_321}

	for _, lines := range [][]billing.TemplateLine{quarterlyTemplate(), unevenTemplate()} {
		for _, net := range nets {
			plan, err := billing.GeneratePlan(billing.PlanSpec{
				AccountID: shared.NewID(),
				NetAmount: net,
				YearStart: yearStart,
				Lines:     lines,
			})
			if err != nil {
				t.Fatalf("GeneratePlan(net=%s) returned error: %v", net, err)
			}
			if err := billing.VerifyPlanSum(plan, net); err != nil {
				t.Errorf("net %s: %v", net, err)
			}
			for _, inst := range plan {
				if !inst.Amount.IsPositive() {
					t.Errorf("net %s: installment %d has a non-positive amount %s",
						net, inst.Number, inst.Amount)
				}
			}
		}
	}
}

func TestPlanRespectsUnevenShares(t *testing.T) {
	plan, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: 1_500_000,
		YearStart: shared.NewDate(2025, 9, 1),
		Lines:     unevenTemplate(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []money.Amount{500_100, 300_000, 399_900, 300_000}
	if len(plan) != len(want) {
		t.Fatalf("got %d installments, want %d", len(plan), len(want))
	}
	for i, inst := range plan {
		if inst.Amount != want[i] {
			t.Errorf("installment %d = %s, want %s", i+1, inst.Amount, want[i])
		}
	}
}

func TestPlanDueDatesFollowTheYearStart(t *testing.T) {
	plan, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: 1_000_000,
		YearStart: shared.NewDate(2025, 9, 1),
		Lines:     quarterlyTemplate(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan[0].DueDate.String(); got != "2025-09-01" {
		t.Errorf("first due date = %s, want 2025-09-01", got)
	}
	if got := plan[3].DueDate.String(); got != "2026-02-28" {
		t.Errorf("last due date = %s, want 2026-02-28 (180 days on)", got)
	}
}

// A fully exempt student gets no plan. Four zero-value installments would
// appear on every overdue report and in every cashier's worklist, chasing
// nothing.
func TestZeroNetGeneratesNoPlan(t *testing.T) {
	plan, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: 0,
		YearStart: shared.NewDate(2025, 9, 1),
		Lines:     quarterlyTemplate(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Errorf("a zero net produced %d installments, want none", len(plan))
	}
}

func TestPlanRejectsTemplateSharesThatDoNotTotal(t *testing.T) {
	_, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: 1_000_000,
		YearStart: shared.NewDate(2025, 9, 1),
		Lines: []billing.TemplateLine{
			{LineNo: 1, ShareBP: 5000, DueOffsetDays: 0},
			{LineNo: 2, ShareBP: 4000, DueOffsetDays: 60},
		},
	})
	if err == nil {
		t.Fatal("a template whose shares total ninety percent must be rejected")
	}
}

// A template is validated when it is published, not when it is used. Otherwise
// a misconfigured split fails once per student on the first morning of
// registration instead of once when an administrator saved it.
func TestTemplatePublishValidatesShares(t *testing.T) {
	base := func(lines []billing.TemplateLine) *billing.InstallmentTemplate {
		return &billing.InstallmentTemplate{
			ID:              shared.NewID(),
			Code:            "STD4",
			NameAr:          "أربعة أقساط",
			MaxInstallments: 4,
			Status:          billing.PolicyDraft,
			Lines:           lines,
		}
	}
	actor := shared.NewID()
	now := shared.SystemClock{}.Now()

	if err := base(quarterlyTemplate()).Publish(actor, now); err != nil {
		t.Errorf("a valid template failed to publish: %v", err)
	}

	short := base([]billing.TemplateLine{{LineNo: 1, ShareBP: 9000, DueOffsetDays: 0}})
	if err := short.Publish(actor, now); err == nil {
		t.Error("a template totalling ninety percent must not publish")
	}

	over := base([]billing.TemplateLine{
		{LineNo: 1, ShareBP: 6000, DueOffsetDays: 0},
		{LineNo: 2, ShareBP: 6000, DueOffsetDays: 60},
	})
	if err := over.Publish(actor, now); err == nil {
		t.Error("a template totalling a hundred and twenty percent must not publish")
	}

	tooMany := base(quarterlyTemplate())
	tooMany.MaxInstallments = 2
	if err := tooMany.Publish(actor, now); err == nil {
		t.Error("a template with more lines than its own maximum must not publish")
	}
}

// Retiring is what makes a published template's amount changeable at all:
// publishing a second version over the same scope is refused while the first
// still holds it (uq_installment_template_scope), so a changed figure is
// retire-then-publish, never an edit.
func TestInstallmentTemplateRetire(t *testing.T) {
	now := shared.SystemClock{}.Now()

	draft := &billing.InstallmentTemplate{
		Code: "STD4", NameAr: "أربعة أقساط", MaxInstallments: 4,
		Status: billing.PolicyDraft, Lines: quarterlyTemplate(),
	}
	if err := draft.Retire(now); err == nil {
		t.Error("a draft template must not be retirable — it was never in force")
	}

	published := &billing.InstallmentTemplate{
		Code: "STD4", NameAr: "أربعة أقساط", MaxInstallments: 4,
		Status: billing.PolicyPublished, Lines: quarterlyTemplate(),
	}
	if err := published.Retire(now); err != nil {
		t.Fatalf("retiring a published template: %v", err)
	}
	if published.Status != billing.PolicyRetired || published.RetiredAt == nil {
		t.Errorf("template = %+v, want retired with a timestamp", published)
	}

	if err := published.Retire(now); err == nil {
		t.Error("an already-retired template must not retire again")
	}
}

// A template authored in literal amounts must reproduce them exactly — that
// is the entire point of typing "400,000" instead of a percentage — and the
// four figures must still sum to the net, the same invariant a percentage
// template is held to.
func TestGeneratePlanPrefersLiteralAmountsWhenEveryLineHasOne(t *testing.T) {
	lines := []billing.TemplateLine{
		{LineNo: 1, ShareBP: 2667, DueOffsetDays: 0, Amount: amt(400_000)},
		{LineNo: 2, ShareBP: 2667, DueOffsetDays: 60, Amount: amt(400_000)},
		{LineNo: 3, ShareBP: 2333, DueOffsetDays: 120, Amount: amt(350_000)},
		{LineNo: 4, ShareBP: 2333, DueOffsetDays: 180, Amount: amt(350_000)},
	}

	plan, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: money.FromInt64(1_500_000),
		YearStart: shared.NewDate(2026, 9, 1),
		Lines:     lines,
	})
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	want := []money.Amount{400_000, 400_000, 350_000, 350_000}
	if len(plan) != len(want) {
		t.Fatalf("got %d installments, want %d", len(plan), len(want))
	}
	for i, inst := range plan {
		if inst.Amount != want[i] {
			t.Errorf("installment %d = %s, want %s (literal amounts must not be re-derived from share_bp)",
				i+1, inst.Amount, want[i])
		}
	}
}

// The whole reason VerifyPlanSum runs after amountsFor: literal amounts that
// do not sum to the actual net must be refused, exactly like a bad
// percentage template — never silently trimmed or padded.
func TestGeneratePlanRefusesLiteralAmountsThatDoNotSumToNet(t *testing.T) {
	lines := []billing.TemplateLine{
		{LineNo: 1, ShareBP: 5000, DueOffsetDays: 0, Amount: amt(400_000)},
		{LineNo: 2, ShareBP: 5000, DueOffsetDays: 60, Amount: amt(400_000)},
	}
	_, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: money.FromInt64(1_500_000), // the lines only sum to 800,000
		YearStart: shared.NewDate(2026, 9, 1),
		Lines:     lines,
	})
	if err == nil {
		t.Fatal("literal amounts that fall short of the net must be refused, not padded onto the first line")
	}
}

// A template with only some lines carrying a literal amount is not a
// coherent request — the application layer refuses that combination before
// publication — but GeneratePlan must still behave predictably rather than
// silently mixing the two: it falls through to the percentage split.
func TestGeneratePlanTreatsAPartialAmountSetAsNone(t *testing.T) {
	lines := []billing.TemplateLine{
		{LineNo: 1, ShareBP: 5000, DueOffsetDays: 0, Amount: amt(999)},
		{LineNo: 2, ShareBP: 5000, DueOffsetDays: 60},
	}
	plan, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: money.FromInt64(1_000_000),
		YearStart: shared.NewDate(2026, 9, 1),
		Lines:     lines,
	})
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	if err := billing.VerifyPlanSum(plan, money.FromInt64(1_000_000)); err != nil {
		t.Errorf("a partial amount set should fall through to the percentage split, which must still sum: %v", err)
	}
	for _, inst := range plan {
		if inst.Amount == 999 {
			t.Error("the lone literal amount must not have been used once the set was incomplete")
		}
	}
}

// A discount granted after two installments were paid must leave those exactly
// as they are — the receipts are printed and the money is in the drawer — and
// redistribute only what is left.
func TestResplitLeavesPaidInstallmentsUntouched(t *testing.T) {
	accountID := shared.NewID()
	existing := []*billing.Installment{
		{ID: shared.NewID(), AccountID: accountID, Number: 1, Amount: 500_000,
			PaidAmount: 500_000, Status: billing.InstallmentPaid, DueDate: shared.NewDate(2025, 9, 1)},
		{ID: shared.NewID(), AccountID: accountID, Number: 2, Amount: 500_000,
			PaidAmount: 200_000, Status: billing.InstallmentPartiallyPaid, DueDate: shared.NewDate(2025, 11, 1)},
		{ID: shared.NewID(), AccountID: accountID, Number: 3, Amount: 500_000,
			Status: billing.InstallmentPending, DueDate: shared.NewDate(2026, 1, 1)},
		{ID: shared.NewID(), AccountID: accountID, Number: 4, Amount: 500_000,
			Status: billing.InstallmentPending, DueDate: shared.NewDate(2026, 3, 1)},
	}

	// Net drops from 2,000,000 to 1,500,000. The two installments carrying
	// money total 1,000,000 and stay; the remaining 500,000 is re-spread.
	result, err := billing.ResplitUnpaid(billing.ResplitSpec{
		Existing:     existing,
		NewNetAmount: 1_500_000,
		YearStart:    shared.NewDate(2025, 9, 1),
		Lines: []billing.TemplateLine{
			{LineNo: 1, ShareBP: 5000, DueOffsetDays: 120},
			{LineNo: 2, ShareBP: 5000, DueOffsetDays: 180},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Keep) != 2 {
		t.Fatalf("kept %d installments, want the 2 carrying money", len(result.Keep))
	}
	for _, inst := range result.Keep {
		if !inst.PaidAmount.IsPositive() {
			t.Errorf("installment %d was kept but carries no money", inst.Number)
		}
	}

	var replacedTotal money.Amount
	for _, inst := range result.Fresh {
		replacedTotal = replacedTotal.MustAdd(inst.Amount)
	}
	if replacedTotal != 500_000 {
		t.Errorf("replacement installments total %s, want 500,000", replacedTotal)
	}

	// The two unpaid rows the new shares replace must come back so the caller
	// can retire them. Leaving them live beside their replacements would make
	// the plan sum to more than the account owes.
	if len(result.Supersede) != 2 {
		t.Fatalf("returned %d rows to supersede, want the 2 unpaid ones", len(result.Supersede))
	}
	for _, inst := range result.Supersede {
		if inst.PaidAmount.IsPositive() {
			t.Errorf("installment %d carries money and must never be superseded", inst.Number)
		}
	}
}

// Reducing the net below what settled installments already committed cannot be
// solved by reshaping the plan; the excess is a credit question.
func TestResplitRefusesToGoBelowSettledInstallments(t *testing.T) {
	accountID := shared.NewID()
	existing := []*billing.Installment{
		{ID: shared.NewID(), AccountID: accountID, Number: 1, Amount: 1_000_000,
			PaidAmount: 1_000_000, Status: billing.InstallmentPaid, DueDate: shared.NewDate(2025, 9, 1)},
	}

	_, err := billing.ResplitUnpaid(billing.ResplitSpec{
		Existing:     existing,
		NewNetAmount: 600_000,
		YearStart:    shared.NewDate(2025, 9, 1),
		Lines:        []billing.TemplateLine{{LineNo: 1, ShareBP: 10000, DueOffsetDays: 60}},
	})
	if err == nil {
		t.Fatal("re-splitting below the settled total must be refused")
	}
	if code := shared.CodeOf(err); code != "resplit.below_settled_amount" {
		t.Errorf("error code = %q, want resplit.below_settled_amount", code)
	}
}

func TestInstallmentStatusTracksAllocations(t *testing.T) {
	inst := &billing.Installment{
		ID: shared.NewID(), Number: 1, Amount: 500_000,
		Status: billing.InstallmentPending, DueDate: shared.NewDate(2025, 9, 1),
	}

	if err := inst.ApplyAllocation(200_000); err != nil {
		t.Fatal(err)
	}
	if inst.Status != billing.InstallmentPartiallyPaid {
		t.Errorf("status = %s, want partially_paid", inst.Status)
	}

	if err := inst.ApplyAllocation(300_000); err != nil {
		t.Fatal(err)
	}
	if inst.Status != billing.InstallmentPaid {
		t.Errorf("status = %s, want paid", inst.Status)
	}

	if err := inst.ApplyAllocation(1); err == nil {
		t.Error("allocating beyond the installment amount must be refused")
	}

	// A refund walks the status back rather than leaving a paid installment
	// that no longer has money behind it.
	if err := inst.ReverseAllocation(300_000); err != nil {
		t.Fatal(err)
	}
	if inst.Status != billing.InstallmentPartiallyPaid {
		t.Errorf("after reversal status = %s, want partially_paid", inst.Status)
	}
}

// Overdue is derived from today and the due date, never stored — a flag would
// need a nightly sweep whose failure silently misreports the debt.
func TestOverdueIsDerived(t *testing.T) {
	today := shared.NewDate(2026, 1, 15)

	past := &billing.Installment{Amount: 500_000, Status: billing.InstallmentPending,
		DueDate: shared.NewDate(2025, 12, 1)}
	if !past.IsOverdue(today) {
		t.Error("an unpaid installment past its due date is overdue")
	}

	future := &billing.Installment{Amount: 500_000, Status: billing.InstallmentPending,
		DueDate: shared.NewDate(2026, 3, 1)}
	if future.IsOverdue(today) {
		t.Error("an installment not yet due is not overdue")
	}

	settled := &billing.Installment{Amount: 500_000, PaidAmount: 500_000,
		Status: billing.InstallmentPaid, DueDate: shared.NewDate(2025, 12, 1)}
	if settled.IsOverdue(today) {
		t.Error("a paid installment is never overdue, however old")
	}

	waived := &billing.Installment{Amount: 500_000, Status: billing.InstallmentWaived,
		DueDate: shared.NewDate(2025, 12, 1)}
	if waived.IsOverdue(today) {
		t.Error("a waived installment is not overdue")
	}
}
