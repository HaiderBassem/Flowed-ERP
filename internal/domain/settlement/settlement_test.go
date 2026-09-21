package settlement_test

import (
	"testing"

	"flowed/internal/domain/money"
	"flowed/internal/domain/settlement"
	"flowed/internal/domain/shared"
)

func line(ref string, amount money.Amount) settlement.Line {
	return settlement.Line{ExternalRef: ref, Amount: amount}
}

func TestExactlyOneLivePaymentAtTheSameAmountMatches(t *testing.T) {
	paymentID := shared.NewID()
	decision := settlement.Match(line("REF-8814772", 500_000), []settlement.Candidate{
		{PaymentID: paymentID, Amount: 500_000},
	})

	if decision.Status != settlement.StatusMatched {
		t.Fatalf("status = %q, want matched", decision.Status)
	}
	if decision.PaymentID == nil || *decision.PaymentID != paymentID {
		t.Error("a matched line must name the payment it matched")
	}
	if !decision.Variance.IsZero() {
		t.Errorf("variance = %s, want zero", decision.Variance)
	}
}

// Money the bank says arrived that the system never recorded. The student is
// still being chased for it, which is why this is the most important finding
// the reconciliation produces.
func TestALineWithNoPaymentIsUnmatched(t *testing.T) {
	decision := settlement.Match(line("REF-NOBODY", 250_000), nil)
	if decision.Status != settlement.StatusUnmatched {
		t.Fatalf("status = %q, want unmatched", decision.Status)
	}
	if decision.PaymentID != nil {
		t.Error("an unmatched line must not name a payment")
	}
}

// A voided receipt against a real bank credit is a different problem from a
// credit nobody recorded, and the note has to say which.
func TestOnlyVoidedPaymentsIsReportedDistinctly(t *testing.T) {
	decision := settlement.Match(line("REF-VOIDED", 300_000), []settlement.Candidate{
		{PaymentID: shared.NewID(), Amount: 300_000, Voided: true},
	})
	if decision.Status != settlement.StatusUnmatched {
		t.Fatalf("status = %q, want unmatched", decision.Status)
	}
	if decision.Note == "" || decision.Note == "no payment carries this reference" {
		t.Errorf("the note should say the payments were voided, got %q", decision.Note)
	}
}

// One bank slip entered twice. This is the single most likely way for a
// university to record more money than it received.
func TestTwoLivePaymentsOnOneReferenceIsADuplicate(t *testing.T) {
	decision := settlement.Match(line("REF-TWICE", 400_000), []settlement.Candidate{
		{PaymentID: shared.NewID(), Amount: 400_000},
		{PaymentID: shared.NewID(), Amount: 400_000},
	})
	if decision.Status != settlement.StatusDuplicate {
		t.Fatalf("status = %q, want duplicate", decision.Status)
	}
	if decision.PaymentID != nil {
		t.Error("a duplicate must not silently pick one of the payments")
	}
}

func TestDifferentAmountsProduceASignedVariance(t *testing.T) {
	paymentID := shared.NewID()

	// The bank received more than the receipt says.
	over := settlement.Match(line("REF-OVER", 520_000), []settlement.Candidate{
		{PaymentID: paymentID, Amount: 500_000},
	})
	if over.Status != settlement.StatusVariance || over.Variance != 20_000 {
		t.Errorf("over: status %q variance %s, want variance +20,000", over.Status, over.Variance)
	}

	// And less: a bank charge deducted in transit looks exactly like this.
	under := settlement.Match(line("REF-UNDER", 495_000), []settlement.Candidate{
		{PaymentID: paymentID, Amount: 500_000},
	})
	if under.Status != settlement.StatusVariance || under.Variance != -5_000 {
		t.Errorf("under: status %q variance %s, want variance -5,000", under.Status, under.Variance)
	}
}

func TestALineWithNoReferenceCannotMatch(t *testing.T) {
	decision := settlement.Match(line("   ", 100_000), []settlement.Candidate{
		{PaymentID: shared.NewID(), Amount: 100_000},
	})
	if decision.Status != settlement.StatusUnmatched {
		t.Fatalf("status = %q, want unmatched: matching on amount alone would pair a line "+
			"with whichever payment happened to be the same size", decision.Status)
	}
}

func TestBatchIsReconciledOnlyWhenNothingNeedsAPerson(t *testing.T) {
	clean := []settlement.Line{
		{Status: settlement.StatusMatched},
		{Status: settlement.StatusIgnored},
	}
	if got := settlement.StatusAfterMatching(clean); got != settlement.BatchReconciled {
		t.Errorf("status = %q, want reconciled", got)
	}

	for _, open := range []settlement.MatchStatus{
		settlement.StatusUnmatched, settlement.StatusVariance, settlement.StatusDuplicate,
	} {
		lines := []settlement.Line{{Status: settlement.StatusMatched}, {Status: open}}
		if got := settlement.StatusAfterMatching(lines); got != settlement.BatchNeedsReview {
			t.Errorf("with one %q line, status = %q, want needs_review", open, got)
		}
	}
}

func TestSummariseCountsMatchedSeparately(t *testing.T) {
	lines := []settlement.Line{
		{Amount: 500_000, Status: settlement.StatusMatched},
		{Amount: 300_000, Status: settlement.StatusVariance},
		{Amount: 200_000, Status: settlement.StatusUnmatched},
	}
	count, matched, total, matchedTotal := settlement.Summarise(lines)

	if count != 3 || matched != 2 {
		t.Errorf("count = %d, matched = %d; want 3 and 2", count, matched)
	}
	if total != 1_000_000 || matchedTotal != 800_000 {
		t.Errorf("total = %s, matched total = %s; want 1,000,000 and 800,000", total, matchedTotal)
	}
}

func TestResolutionRules(t *testing.T) {
	paymentID := shared.NewID()

	if err := settlement.ValidateResolution(settlement.StatusIgnored, nil, ""); err == nil {
		t.Error("setting a line aside must require a reason")
	}
	if err := settlement.ValidateResolution(settlement.StatusIgnored, nil, "bank charge"); err != nil {
		t.Errorf("a reasoned dismissal should be accepted: %v", err)
	}
	// The rule that matters: "matched" without naming a payment is
	// indistinguishable from clearing the queue.
	if err := settlement.ValidateResolution(settlement.StatusMatched, nil, "done"); err == nil {
		t.Error("matching by hand must name the payment")
	}
	if err := settlement.ValidateResolution(settlement.StatusMatched, &paymentID, ""); err != nil {
		t.Errorf("a hand match naming its payment should be accepted: %v", err)
	}
	if err := settlement.ValidateResolution(settlement.StatusUnmatched, nil, "x"); err == nil {
		t.Error("unmatched is a finding, not a resolution")
	}
}
