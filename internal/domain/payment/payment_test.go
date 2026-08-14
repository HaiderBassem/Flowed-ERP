package payment_test

import (
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
)

func newPostedPayment(t *testing.T, amount money.Amount) *payment.Payment {
	t.Helper()
	p, err := payment.New(payment.NewParams{
		AccountID:       shared.NewID(),
		StudentID:       shared.NewID(),
		EnrollmentID:    shared.NewID(),
		PostingYearID:   shared.NewID(),
		Amount:          amount,
		PaymentMethodID: shared.NewID(),
		CashierUserID:   shared.NewID(),
		IdempotencyKey:  shared.NewID().String(),
		PayloadHash:     "hash",
	})
	if err != nil {
		t.Fatalf("building payment: %v", err)
	}
	if err := p.Post("2025-D01-000001", shared.NewID(), time.Now().UTC()); err != nil {
		t.Fatalf("posting payment: %v", err)
	}
	return p
}

// The rule that stops a double payout. Refund 400,000 of a million, then void
// the whole payment, and the university has returned 1,400,000 against a
// million received — the Σ-refunds check never fires, because a void is not a
// refund.
func TestVoidIsRefusedOnceAnyRefundIsPosted(t *testing.T) {
	p := newPostedPayment(t, 1_000_000)

	err := p.Void(shared.NewID(), "keyed against the wrong student", time.Now().UTC(), 1)
	if err == nil {
		t.Fatal("voiding a payment that already carries a refund must be refused")
	}
	if code := shared.CodeOf(err); code != "payment.has_refunds" {
		t.Errorf("error code = %q, want payment.has_refunds", code)
	}
	if p.Status != payment.StatusPosted {
		t.Errorf("a refused void must leave the payment posted, got %s", p.Status)
	}

	domainErr, ok := shared.AsDomain(err)
	if !ok {
		t.Fatal("expected a domain error")
	}
	if _, present := domainErr.Details["remedy"]; !present {
		t.Error("the refusal must point the cashier at the refund path")
	}
}

func TestVoidSucceedsWithNoRefunds(t *testing.T) {
	p := newPostedPayment(t, 1_000_000)
	if err := p.Void(shared.NewID(), "keyed against the wrong student", time.Now().UTC(), 0); err != nil {
		t.Fatal(err)
	}
	if p.Status != payment.StatusVoided {
		t.Errorf("status = %s, want voided", p.Status)
	}
	// The receipt number survives. An auditor scanning a receipt book should
	// find a cancelled receipt in place, not a gap to go and explain.
	if p.ReceiptNo == nil || *p.ReceiptNo != "2025-D01-000001" {
		t.Error("a voided payment keeps its receipt number")
	}
	if p.IsCounted() {
		t.Error("a voided payment must not count towards collections")
	}
}

func TestVoidRequiresAReason(t *testing.T) {
	p := newPostedPayment(t, 500_000)
	if err := p.Void(shared.NewID(), "", time.Now().UTC(), 0); err == nil {
		t.Error("voiding without a reason must be refused")
	}
}

func TestDraftPaymentCarriesNoReceiptNumber(t *testing.T) {
	p, err := payment.New(payment.NewParams{
		AccountID: shared.NewID(), StudentID: shared.NewID(), EnrollmentID: shared.NewID(),
		PostingYearID: shared.NewID(), Amount: 500_000, PaymentMethodID: shared.NewID(),
		CashierUserID: shared.NewID(), IdempotencyKey: "key-1", PayloadHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ReceiptNo != nil {
		t.Error("a draft must not hold a receipt number: an abandoned draft would burn one")
	}
	if err := p.Void(shared.NewID(), "reason", time.Now().UTC(), 0); err == nil {
		t.Error("voiding a draft must be refused; a draft is discarded, not voided")
	}
}

func TestPaymentRequiresAnIdempotencyKey(t *testing.T) {
	_, err := payment.New(payment.NewParams{
		AccountID: shared.NewID(), StudentID: shared.NewID(), EnrollmentID: shared.NewID(),
		PostingYearID: shared.NewID(), Amount: 500_000, PaymentMethodID: shared.NewID(),
		CashierUserID: shared.NewID(),
	})
	if err == nil {
		t.Error("a payment without an idempotency key must be refused: a network retry would collect twice")
	}
}

func TestPaymentRejectsNonPositiveAmounts(t *testing.T) {
	for _, amount := range []money.Amount{0, -1, -500_000} {
		_, err := payment.New(payment.NewParams{
			AccountID: shared.NewID(), StudentID: shared.NewID(), EnrollmentID: shared.NewID(),
			PostingYearID: shared.NewID(), Amount: amount, PaymentMethodID: shared.NewID(),
			CashierUserID: shared.NewID(), IdempotencyKey: "key", PayloadHash: "hash",
		})
		if err == nil {
			t.Errorf("a payment of %s must be refused", amount)
		}
	}
}

// Approval is a second person's act, in the domain as well as in the database.
func TestFourEyesIsEnforcedOnRefundsVoidsAndDrawers(t *testing.T) {
	requester := shared.NewID()
	now := time.Now().UTC()

	refund, err := payment.NewRefund(payment.NewRefundParams{
		PaymentID: shared.NewID(), AccountID: shared.NewID(), StudentID: shared.NewID(),
		PostingYearID: shared.NewID(), Amount: 100_000, PaymentMethodID: shared.NewID(),
		Reason: "student withdrew", RequestedBy: requester,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := refund.Approve(requester, now); err == nil {
		t.Error("a refund must not be approved by the person who requested it")
	}
	if err := refund.Approve(shared.NewID(), now); err != nil {
		t.Errorf("a different approver should succeed: %v", err)
	}

	voidRequest, err := payment.NewVoidRequest(shared.NewID(), "wrong student", requester)
	if err != nil {
		t.Fatal(err)
	}
	if err := voidRequest.Execute(requester, now); err == nil {
		t.Error("a void must not be executed by the cashier who requested it")
	}
	if err := voidRequest.Execute(shared.NewID(), now); err != nil {
		t.Errorf("a different executor should succeed: %v", err)
	}

	cashier := shared.NewID()
	session, err := payment.NewCashierSession(cashier, shared.NewID(), shared.NewID(), 100_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(500_000, 500_000, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := session.Approve(cashier, now); err == nil {
		t.Error("a cashier must not sign off their own drawer")
	}
	if err := session.Approve(shared.NewID(), now); err != nil {
		t.Errorf("a supervisor should be able to sign off: %v", err)
	}
}

func TestRefundLifecycleOrder(t *testing.T) {
	now := time.Now().UTC()
	refund, err := payment.NewRefund(payment.NewRefundParams{
		PaymentID: shared.NewID(), AccountID: shared.NewID(), StudentID: shared.NewID(),
		PostingYearID: shared.NewID(), Amount: 100_000, PaymentMethodID: shared.NewID(),
		Reason: "student withdrew", RequestedBy: shared.NewID(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Money cannot leave before somebody approves its leaving.
	if err := refund.Post("R-0001", shared.NewID(), shared.NewID(), now); err == nil {
		t.Error("posting an unapproved refund must be refused")
	}

	if err := refund.Approve(shared.NewID(), now); err != nil {
		t.Fatal(err)
	}
	if err := refund.Post("R-0001", shared.NewID(), shared.NewID(), now); err != nil {
		t.Fatal(err)
	}
	if refund.Status != payment.RefundPosted {
		t.Errorf("status = %s, want posted", refund.Status)
	}
	if err := refund.Approve(shared.NewID(), now); err == nil {
		t.Error("a posted refund must not be re-approved")
	}
}

// A drawer that does not balance needs an explanation before the shift can be
// signed off — that is the point of counting it.
func TestCashierSessionDemandsAnExplanationForAVariance(t *testing.T) {
	session, err := payment.NewCashierSession(shared.NewID(), shared.NewID(), shared.NewID(), 100_000)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	if err := session.Close(500_000, 480_000, nil, now); err == nil {
		t.Error("closing a drawer 20,000 short without a reason must be refused")
	}

	reason := "20,000 handed to the bursar against receipt 88"
	if err := session.Close(500_000, 480_000, &reason, now); err != nil {
		t.Fatal(err)
	}
	if session.Variance == nil || *session.Variance != -20_000 {
		t.Errorf("variance = %v, want -20,000", session.Variance)
	}
}
