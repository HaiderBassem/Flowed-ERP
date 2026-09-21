package payment

import (
	"strings"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// IntentStatus is the state of a collection begun at an external provider.
type IntentStatus string

const (
	// IntentCreated has been recorded here and not yet handed to the provider.
	IntentCreated IntentStatus = "created"
	// IntentPending is with the provider: the student is at a payment page, a
	// wallet prompt or a bank counter.
	IntentPending IntentStatus = "pending"
	// IntentSucceeded has been confirmed by the provider and posted to the
	// ledger. This state and a payment row always arrive together.
	IntentSucceeded IntentStatus = "succeeded"
	// IntentFailed was refused by the provider or by the student's bank.
	IntentFailed IntentStatus = "failed"
	// IntentExpired ran out of time without an answer. Distinct from failed:
	// nobody said no, the answer never came, and the money may yet move —
	// which is why an expired intent is reconciled rather than forgotten.
	IntentExpired IntentStatus = "expired"
	// IntentCancelled was abandoned deliberately.
	IntentCancelled IntentStatus = "cancelled"
)

// Terminal reports whether the intent can still change.
func (s IntentStatus) Terminal() bool {
	switch s {
	case IntentSucceeded, IntentFailed, IntentCancelled:
		return true
	default:
		return false
	}
}

// Intent is a collection begun at an external provider and confirmed later.
//
// It is deliberately not money. A student who taps "pay" in a wallet has not
// paid the university anything; the provider saying so afterwards is what makes
// it a payment, and that confirmation creates an ordinary payment row through
// the ordinary command. Everything the rest of the system knows about
// collection — receipt numbering, allocation, the year lock, idempotency — then
// applies unchanged, because there is nothing special about where the money
// came from.
type Intent struct {
	ID             shared.ID
	ProviderCode   string
	AccountID      shared.ID
	StudentID      shared.ID
	AcademicYearID shared.ID
	Amount         money.Amount
	Status         IntentStatus

	// ProviderRef is what the provider calls this transaction. Their
	// confirmation arrives by it.
	ProviderRef *string
	// ClientRef is what we told them to call it, sent on initiation so a
	// retried initiation cannot open a second charge at their end.
	ClientRef string
	// PaymentIdempotencyKey is the key the resulting payment is posted under,
	// so a confirmation delivered twice posts one payment. It is fixed when the
	// intent is created rather than derived at confirmation time, because the
	// two deliveries must agree on it and only this row is common to both.
	PaymentIdempotencyKey string

	RedirectURL    *string
	ExpiresAt      *time.Time
	FailureCode    *string
	FailureMessage *string
	PaymentID      *shared.ID

	CreatedAt   time.Time
	CreatedBy   *shared.ID
	ConfirmedAt *time.Time
}

// NewIntent builds a collection request.
func NewIntent(providerCode string, accountID, studentID, yearID shared.ID, amount money.Amount) (*Intent, error) {
	code := strings.ToUpper(strings.TrimSpace(providerCode))
	if code == "" {
		return nil, shared.Validation("intent.provider_required",
			"a payment intent must name the provider it is being made through")
	}
	if !amount.IsPositive() {
		return nil, shared.Validation("intent.non_positive_amount",
			"a payment must be greater than zero, got %s", amount)
	}

	id := shared.NewID()
	return &Intent{
		ID:             id,
		ProviderCode:   code,
		AccountID:      accountID,
		StudentID:      studentID,
		AcademicYearID: yearID,
		Amount:         amount,
		Status:         IntentCreated,
		// The intent's own identifier is the client reference and the seed of
		// the idempotency key. Derived rather than random so that a provider
		// echoing the reference back is enough to find the intent, and so the
		// key cannot drift from the row it belongs to.
		ClientRef:             id.String(),
		PaymentIdempotencyKey: "intent-" + id.String(),
		CreatedAt:             time.Now().UTC(),
	}, nil
}

// MarkPending records that the provider accepted the request.
func (i *Intent) MarkPending(providerRef string, redirectURL *string, expiresAt *time.Time) error {
	if i.Status != IntentCreated && i.Status != IntentPending {
		return shared.PreconditionFailed("intent.not_open",
			"this payment is %s and cannot be handed to the provider again", i.Status)
	}
	if strings.TrimSpace(providerRef) == "" {
		return shared.Validation("intent.provider_ref_required",
			"the provider must return its own reference for the transaction; "+
				"without one their confirmation cannot be matched to this request")
	}
	ref := strings.TrimSpace(providerRef)
	i.ProviderRef = &ref
	i.RedirectURL = redirectURL
	i.ExpiresAt = expiresAt
	i.Status = IntentPending
	return nil
}

// Succeed records a confirmed collection.
//
// The payment must already exist: an intent cannot succeed without a row in the
// ledger, which is the difference between "the app said it paid" and "the
// university received money".
func (i *Intent) Succeed(paymentID shared.ID, at time.Time) error {
	if i.Status == IntentSucceeded {
		// Idempotent. A provider retrying a delivery must find the same
		// outcome, not an error that makes them retry again.
		if i.PaymentID != nil && *i.PaymentID == paymentID {
			return nil
		}
		return shared.Conflict("intent.already_settled",
			"this payment was already confirmed against a different collection").
			WithDetail("existing_payment_id", i.PaymentID.String())
	}
	if i.Status.Terminal() {
		return shared.PreconditionFailed("intent.terminal",
			"this payment is %s and cannot be confirmed", i.Status)
	}
	if shared.IsNil(paymentID) {
		return shared.InvariantViolation("intent.no_payment",
			"an intent cannot succeed without the payment it produced")
	}

	i.Status = IntentSucceeded
	i.PaymentID = &paymentID
	i.ConfirmedAt = &at
	return nil
}

// Fail records a refusal.
func (i *Intent) Fail(code, message string, at time.Time) error {
	if i.Status == IntentSucceeded {
		// A failure arriving after a success is a provider bug or a replayed
		// callback. The money is in the ledger; refusing to unwind it here is
		// the same rule that makes a posted payment immutable.
		return shared.Conflict("intent.already_succeeded",
			"this payment was already confirmed; a later failure cannot unwind it").
			WithDetail("remedy", "if the money was returned, post a refund")
	}
	if i.Status.Terminal() {
		return nil
	}
	i.Status = IntentFailed
	i.FailureCode = &code
	i.FailureMessage = &message
	i.ConfirmedAt = &at
	return nil
}

// Expire marks an intent whose answer never came.
func (i *Intent) Expire(at time.Time) error {
	if i.Status.Terminal() || i.Status == IntentExpired {
		return nil
	}
	i.Status = IntentExpired
	i.ConfirmedAt = &at
	return nil
}

// Cancel abandons an intent deliberately.
func (i *Intent) Cancel() error {
	if i.Status == IntentSucceeded {
		return shared.PreconditionFailed("intent.already_succeeded",
			"this payment has been collected and cannot be cancelled").
			WithDetail("remedy", "void or refund the collection instead")
	}
	i.Status = IntentCancelled
	return nil
}

// IsExpired reports whether the provider's window has passed.
func (i *Intent) IsExpired(now time.Time) bool {
	return i.ExpiresAt != nil && now.After(*i.ExpiresAt) && !i.Status.Terminal()
}

// ProviderEvent is one callback delivery, verified or not.
//
// Stored whatever it says, including the ones whose signature failed: a stream
// of unsigned callbacks is somebody probing the endpoint, and that is worth
// being able to see.
type ProviderEvent struct {
	ID              shared.ID
	ProviderCode    string
	IntentID        *shared.ID
	ExternalEventID string
	EventType       string
	SignatureOK     bool
	Payload         map[string]any
	ReceivedAt      time.Time
	ProcessedAt     *time.Time
	Outcome         *string
}

// CallbackResult is what a provider's callback says happened.
type CallbackResult struct {
	// ExternalEventID identifies this delivery. Two deliveries of the same
	// event carry the same value, which is what makes the duplicate guard
	// possible.
	ExternalEventID string
	EventType       string
	// ClientRef or ProviderRef identifies the intent. Providers differ about
	// which they echo, so both are accepted.
	ClientRef   string
	ProviderRef string
	// Status is the provider's verdict, already mapped onto this domain's
	// vocabulary by the adapter.
	Status IntentStatus
	// Amount as the provider states it. Compared against the intent: a
	// confirmation for a different amount is refused rather than posted,
	// because the amount the student was charged is the amount that must reach
	// the ledger.
	Amount        money.Amount
	FailureCode   string
	FailureReason string
	Payload       map[string]any
}

// ValidateAgainst checks a callback against the intent it claims to confirm.
func (r CallbackResult) ValidateAgainst(intent *Intent) error {
	if intent == nil {
		return shared.NotFound("intent.unknown",
			"this callback names a payment request that does not exist")
	}
	if !strings.EqualFold(r.EventType, "") && r.Status == "" {
		return shared.Validation("intent.unmapped_status",
			"the provider's callback carries no recognisable outcome")
	}
	if r.Status == IntentSucceeded && r.Amount != intent.Amount {
		return shared.PreconditionFailed("intent.amount_mismatch",
			"the provider confirmed %s against a request for %s", r.Amount, intent.Amount).
			WithDetail("confirmed", r.Amount.Int64()).
			WithDetail("requested", intent.Amount.Int64()).
			WithDetail("remedy", "investigate with the provider before crediting the student; "+
				"a confirmation for a different amount is not evidence of this collection")
	}
	return nil
}
