package payment_test

import (
	"testing"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
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

// The four-eyes rule left with the second operator it depended on: this system
// is run from one account, and a rule refusing self-approval refused every
// approval. The lifecycle order is what still binds, and it is what stops a
// refund being approved twice or a void being executed twice — each of which
// would pay the same money out again.
func TestRefundAndVoidLifecyclesBindTheirOrder(t *testing.T) {
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
	if err := refund.Approve(requester, now); err != nil {
		t.Errorf("the requester must be able to approve with one account: %v", err)
	}
	if refund.ApprovedBy == nil || *refund.ApprovedBy != requester {
		t.Error("an approved refund must record who approved it")
	}
	if err := refund.Approve(shared.NewID(), now); err == nil {
		t.Error("an approved refund must not be approved a second time")
	}

	voidRequest, err := payment.NewVoidRequest(shared.NewID(), "wrong student", requester)
	if err != nil {
		t.Fatal(err)
	}
	// The requester may now execute their own void: the four-eyes rule left
	// with the second operator it depended on. What still holds is the order —
	// a request must exist before it can be executed, and an executed one
	// cannot be executed twice.
	if err := voidRequest.Execute(requester, now); err != nil {
		t.Errorf("the requester should be able to execute with one account: %v", err)
	}
	if err := voidRequest.Execute(shared.NewID(), now); err == nil {
		t.Error("an executed void must not be executed a second time")
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
