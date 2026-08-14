package payment_test

import (
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
)

func newTestIntent(t *testing.T) *payment.Intent {
	t.Helper()
	intent, err := payment.NewIntent("ZAINCASH", shared.NewID(), shared.NewID(), shared.NewID(), 750_000)
	if err != nil {
		t.Fatalf("building intent: %v", err)
	}
	return intent
}

// The rule the whole boundary rests on: an intent cannot report success
// without a payment row. "The app said it paid" is not a collection.
func TestIntentCannotSucceedWithoutAPayment(t *testing.T) {
	intent := newTestIntent(t)
	if err := intent.Succeed(shared.NilID, time.Now()); err == nil {
		t.Fatal("an intent must not succeed without the payment it produced")
	}
	if intent.Status == payment.IntentSucceeded {
		t.Error("the intent must not be left marked succeeded")
	}
}

// Providers retry. A retried delivery must find the same outcome rather than
// an error that makes them retry again — and must never post a second payment.
func TestConfirmingTwiceWithTheSamePaymentIsIdempotent(t *testing.T) {
	intent := newTestIntent(t)
	paymentID := shared.NewID()
	now := time.Now()

	if err := intent.Succeed(paymentID, now); err != nil {
		t.Fatalf("first confirmation: %v", err)
	}
	if err := intent.Succeed(paymentID, now); err != nil {
		t.Fatalf("a repeated confirmation of the same payment must be accepted: %v", err)
	}
	if intent.PaymentID == nil || *intent.PaymentID != paymentID {
		t.Error("the intent should still name the original payment")
	}
}

func TestConfirmingAgainstADifferentPaymentIsRefused(t *testing.T) {
	intent := newTestIntent(t)
	if err := intent.Succeed(shared.NewID(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := intent.Succeed(shared.NewID(), time.Now()); err == nil {
		t.Fatal("a second, different payment against one intent must be refused")
	}
}

// A failure arriving after a success is a provider bug or a replay. The money
// is in the ledger and only a refund moves it back out.
func TestFailureAfterSuccessIsRefused(t *testing.T) {
	intent := newTestIntent(t)
	if err := intent.Succeed(shared.NewID(), time.Now()); err != nil {
		t.Fatal(err)
	}
	err := intent.Fail("cancelled", "user cancelled", time.Now())
	if err == nil {
		t.Fatal("a failure cannot unwind a confirmed collection")
	}
	if intent.Status != payment.IntentSucceeded {
		t.Errorf("status = %q, want it left succeeded", intent.Status)
	}
}

func TestPendingRequiresTheProvidersOwnReference(t *testing.T) {
	intent := newTestIntent(t)
	if err := intent.MarkPending("  ", nil, nil); err == nil {
		t.Fatal("without the provider's reference their confirmation cannot be matched back")
	}
	if err := intent.MarkPending("ZC-99213", nil, nil); err != nil {
		t.Fatalf("marking pending: %v", err)
	}
	if intent.Status != payment.IntentPending || intent.ProviderRef == nil {
		t.Error("the intent should be pending and carry the provider reference")
	}
}

// An amount that does not match is not evidence of this collection. Crediting
// it would let a provider — or somebody replaying their callback — decide what
// a student paid.
func TestCallbackForADifferentAmountIsRefused(t *testing.T) {
	intent := newTestIntent(t)

	result := payment.CallbackResult{
		EventType: "payment.succeeded",
		Status:    payment.IntentSucceeded,
		Amount:    100_000,
	}
	err := result.ValidateAgainst(intent)
	if err == nil {
		t.Fatal("a confirmation for a different amount must be refused")
	}
	if code := shared.CodeOf(err); code != "intent.amount_mismatch" {
		t.Errorf("code = %q", code)
	}

	result.Amount = intent.Amount
	if err := result.ValidateAgainst(intent); err != nil {
		t.Errorf("a matching confirmation should validate: %v", err)
	}
}

func TestExpiryIsDistinctFromFailure(t *testing.T) {
	intent := newTestIntent(t)
	past := time.Now().Add(-time.Hour)
	if err := intent.MarkPending("ZC-1", nil, &past); err != nil {
		t.Fatal(err)
	}

	if !intent.IsExpired(time.Now()) {
		t.Fatal("an intent past its window should report as expired")
	}
	if err := intent.Expire(time.Now()); err != nil {
		t.Fatal(err)
	}
	// Expired is not terminal: nobody said no, and the money may still move,
	// which is why an expired intent is reconciled rather than forgotten.
	if intent.Status.Terminal() {
		t.Error("expiry must not be terminal; the answer may still arrive")
	}

	// And a confirmation arriving late is still accepted.
	if err := intent.Succeed(shared.NewID(), time.Now()); err != nil {
		t.Errorf("a late confirmation must still be accepted: %v", err)
	}
}

func TestCancellingACollectedPaymentIsRefused(t *testing.T) {
	intent := newTestIntent(t)
	if err := intent.Succeed(shared.NewID(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := intent.Cancel(); err == nil {
		t.Fatal("a collected payment cannot be cancelled; it is voided or refunded")
	}
}

// The key is fixed at creation because both deliveries of a retried callback
// must agree on it, and only this row is common to both.
func TestIdempotencyKeyIsStableAndDerivedFromTheIntent(t *testing.T) {
	intent := newTestIntent(t)
	if intent.PaymentIdempotencyKey == "" {
		t.Fatal("an intent must carry the key its payment will be posted under")
	}
	if intent.ClientRef != intent.ID.String() {
		t.Error("the client reference should be the intent's own identifier")
	}

	other := newTestIntent(t)
	if other.PaymentIdempotencyKey == intent.PaymentIdempotencyKey {
		t.Error("two intents must not share an idempotency key")
	}
}

func TestIntentRejectsNonPositiveAmounts(t *testing.T) {
	if _, err := payment.NewIntent("QI", shared.NewID(), shared.NewID(), shared.NewID(), 0); err == nil {
		t.Fatal("a zero payment must be refused")
	}
	if _, err := payment.NewIntent("", shared.NewID(), shared.NewID(), shared.NewID(), 1000); err == nil {
		t.Fatal("an intent must name its provider")
	}
}
