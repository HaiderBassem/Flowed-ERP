package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/swibit/flowed/internal/adapter/payments"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// IntentService drives electronic collection: begin a payment at a provider,
// accept their confirmation, and turn it into an ordinary payment.
//
// The design rule that shapes everything here is that a provider's confirmation
// is evidence, and money is a payment row. Nothing about a student tapping
// "pay" in a wallet changes what they owe; the provider telling us, in a
// message we can verify, is what does. So this service ends where the existing
// payment command begins, and that command is unchanged: same receipt series,
// same allocation, same year lock, same idempotency.
type IntentService struct {
	deps      Deps
	repo      port.IntentRepository
	registry  *payments.Registry
	payments  *PaymentService
	accounts  port.AccountRepository
	students  port.StudentRepository
	methodFor func(ctx context.Context, providerCode string) (shared.ID, error)
	auditor
}

// NewIntentService wires the electronic collection commands.
func NewIntentService(
	d Deps, repo port.IntentRepository, registry *payments.Registry, paymentService *PaymentService,
) *IntentService {
	s := &IntentService{
		deps:     d,
		repo:     repo,
		registry: registry,
		payments: paymentService,
		accounts: d.Accounts,
		students: d.Students,
		auditor:  newAuditor(d.Audit, d.Clock),
	}
	s.methodFor = s.resolvePaymentMethod
	return s
}

// providerActor is the identity an accepted confirmation posts under.
//
// It holds the cashier role and nothing else, and it carries no desk: an
// electronic collection has no window, so its receipts run in the year's
// deskless series rather than borrowing a cashier's book. The name in the audit
// trail says which provider confirmed it, because "who took this money" has an
// answer even when nobody was standing there.
func providerActor(providerCode string) shared.Actor {
	return shared.Actor{
		Username: "provider:" + strings.ToLower(providerCode),
		Roles:    []shared.Role{shared.RoleCashier},
		Scope:    shared.UniversityScope(),
	}
}

// InitiateIntentInput begins a collection at a provider.
type InitiateIntentInput struct {
	AccountID    shared.ID
	ProviderCode string
	Amount       money.Amount
	// ReturnURL and CallbackURL are where the provider sends the student back
	// and where it posts its confirmation. Supplied by the caller because they
	// depend on how this deployment is published, not on the provider.
	ReturnURL   string
	CallbackURL string
}

// InitiateIntentResult is what the student needs in order to pay.
type InitiateIntentResult struct {
	Intent      *payment.Intent
	RedirectURL string
	Instruction string
}

// Initiate begins a collection at a provider.
func (s *IntentService) Initiate(ctx context.Context, actor shared.Actor, in InitiateIntentInput) (*InitiateIntentResult, error) {
	// A cashier may start one on a student's behalf at the desk; a student may
	// start their own through the portal, which authenticates them as
	// themselves and passes their own account.
	if err := actor.RequireAnyRole("InitiatePayment",
		shared.RoleCashier, shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleStudent); err != nil {
		return nil, err
	}

	provider, err := s.registry.Get(in.ProviderCode)
	if err != nil {
		return nil, err
	}

	var (
		intent      *payment.Intent
		studentNo   string
		studentName string
	)
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		account, err := s.accounts.GetByID(ctx, in.AccountID)
		if err != nil {
			return err
		}
		if err := actor.RequireScope("InitiatePayment", &account.CollegeID, &account.DepartmentID); err != nil {
			return err
		}
		// A student may only pay their own fees. The portal authenticates them
		// as a person, and this is where that becomes an authorisation.
		if actor.HasRole(shared.RoleStudent) && actor.StudentID != nil && *actor.StudentID != account.StudentID {
			return shared.Forbidden("intent.not_your_account",
				"this account belongs to another student")
		}

		person, err := s.students.GetByID(ctx, account.StudentID)
		if err != nil {
			return err
		}
		studentNo, studentName = person.StudentNo, person.FullName

		amount := in.Amount
		if !amount.IsPositive() {
			// Paying "the balance" is the common case at a portal, and making
			// the client compute it invites a stale figure.
			amount = account.Remaining()
		}
		if !amount.IsPositive() {
			return shared.PreconditionFailed("intent.nothing_owed",
				"this account has nothing outstanding")
		}

		intent, err = payment.NewIntent(
			provider.Code(), account.ID, account.StudentID, account.AcademicYearID, amount)
		if err != nil {
			return err
		}
		if !shared.IsNil(actor.UserID) {
			intent.CreatedBy = &actor.UserID
		}
		return s.repo.Create(ctx, intent)
	})
	if err != nil {
		return nil, err
	}

	// The provider is called outside the transaction. A network call inside one
	// holds a database connection for the length of somebody else's outage, and
	// at a cashier desk that is the difference between a slow payment and a
	// stalled hall.
	response, err := provider.Initiate(ctx, payments.InitiateRequest{
		IntentID:    intent.ID,
		ClientRef:   intent.ClientRef,
		Amount:      intent.Amount,
		StudentNo:   studentNo,
		StudentName: studentName,
		Description: fmt.Sprintf("Tuition — %s", studentNo),
		ReturnURL:   in.ReturnURL,
		CallbackURL: in.CallbackURL,
	})
	if err != nil {
		// The intent stays in 'created' rather than being deleted: an
		// initiation that failed at their end may still have created a charge,
		// and a row nobody can find is how that charge becomes unreconcilable.
		s.logf(ctx, "provider initiation failed", err)
		return nil, err
	}

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		locked, err := s.repo.GetForUpdate(ctx, intent.ID)
		if err != nil {
			return err
		}
		var redirect *string
		if response.RedirectURL != "" {
			redirect = &response.RedirectURL
		}
		if err := locked.MarkPending(response.ProviderRef, redirect, response.ExpiresAt); err != nil {
			return err
		}
		if err := s.repo.Update(ctx, locked); err != nil {
			return err
		}
		intent = locked

		return s.record(ctx, port.AuditEntry{
			EntityType:     "payment_intent",
			EntityID:       &intent.ID,
			Action:         "intent.initiated",
			Actor:          actor,
			AccountID:      &intent.AccountID,
			StudentID:      &intent.StudentID,
			AcademicYearID: &intent.AcademicYearID,
			Metadata: map[string]any{
				"provider":     intent.ProviderCode,
				"amount":       intent.Amount.Int64(),
				"provider_ref": response.ProviderRef,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	return &InitiateIntentResult{
		Intent:      intent,
		RedirectURL: response.RedirectURL,
		Instruction: response.Instruction,
	}, nil
}

// HandleCallbackInput is one delivered provider callback.
type HandleCallbackInput struct {
	ProviderCode string
	Envelope     payments.CallbackEnvelope
}

// HandleCallbackResult reports what the callback did.
type HandleCallbackResult struct {
	Intent *payment.Intent
	// Duplicate marks a delivery that had already been processed. The response
	// is still a success: a provider retrying must be told the outcome, not
	// given an error that makes them retry again.
	Duplicate bool
	// PaymentID is set when this callback posted a collection.
	PaymentID *shared.ID
}

// HandleCallback verifies a provider's confirmation and posts the collection.
//
// The order is the point:
//
//  1. verify the signature — an unverified callback is stored as evidence and
//     acted on never;
//  2. record the delivery, whose unique external event id makes a retry a
//     no-op rather than a second payment;
//  3. lock the intent, so a callback and a status poll racing each other
//     cannot both post;
//  4. check the amount against what was requested;
//  5. post through the ordinary payment command, under the intent's fixed
//     idempotency key.
func (s *IntentService) HandleCallback(ctx context.Context, in HandleCallbackInput) (*HandleCallbackResult, error) {
	provider, err := s.registry.Get(in.ProviderCode)
	if err != nil {
		return nil, err
	}

	parsed, parseErr := provider.ParseCallback(ctx, in.Envelope)
	if parseErr != nil {
		// A callback that failed verification is still recorded. A burst of
		// them is somebody probing the endpoint, and that is only visible if
		// the failures are kept.
		s.recordRejected(ctx, provider.Code(), in.Envelope, parseErr)
		return nil, parseErr
	}

	result := &HandleCallbackResult{}
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		intent, err := s.repo.FindByReference(ctx, provider.Code(), parsed.ClientRef, parsed.ProviderRef)
		if err != nil {
			if shared.KindOf(err) == shared.KindNotFound {
				// Recorded anyway: a confirmation for a request we have no
				// record of is either a provider mixing up merchants or money
				// arriving for something we never asked for. Both need seeing.
				event := &payment.ProviderEvent{
					ProviderCode:    provider.Code(),
					ExternalEventID: parsed.ExternalEventID,
					EventType:       parsed.EventType,
					SignatureOK:     true,
					Payload:         parsed.Payload,
				}
				if _, recordErr := s.repo.RecordEvent(ctx, event); recordErr != nil {
					return recordErr
				}
				return shared.NotFound("intent.unknown",
					"this confirmation names a payment request that does not exist").
					WithDetail("client_ref", parsed.ClientRef).
					WithDetail("provider_ref", parsed.ProviderRef)
			}
			return err
		}

		event := &payment.ProviderEvent{
			ProviderCode:    provider.Code(),
			IntentID:        &intent.ID,
			ExternalEventID: parsed.ExternalEventID,
			EventType:       parsed.EventType,
			SignatureOK:     true,
			Payload:         parsed.Payload,
		}
		duplicate, err := s.repo.RecordEvent(ctx, event)
		if err != nil {
			return err
		}
		if duplicate {
			// Already processed. Report the intent as it stands; posting again
			// is exactly what the unique index exists to prevent.
			result.Intent, result.Duplicate = intent, true
			result.PaymentID = intent.PaymentID
			return nil
		}

		locked, err := s.repo.GetForUpdate(ctx, intent.ID)
		if err != nil {
			return err
		}
		if err := parsed.ValidateAgainst(locked); err != nil {
			_ = s.repo.MarkEventProcessed(ctx, event.ID, "refused: "+shared.CodeOf(err), nowOr(s.deps.Clock))
			return err
		}

		outcome, paymentID, err := s.applyOutcome(ctx, locked, *parsed)
		if err != nil {
			return err
		}
		result.Intent, result.PaymentID = locked, paymentID

		if err := s.repo.MarkEventProcessed(ctx, event.ID, outcome, nowOr(s.deps.Clock)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// applyOutcome moves an intent to its confirmed state, posting a payment when
// the provider says the money moved.
func (s *IntentService) applyOutcome(
	ctx context.Context, intent *payment.Intent, parsed payment.CallbackResult,
) (outcome string, paymentID *shared.ID, err error) {
	actor := providerActor(intent.ProviderCode)
	now := nowOr(s.deps.Clock)

	switch parsed.Status {
	case payment.IntentSucceeded:
		if intent.Status == payment.IntentSucceeded && intent.PaymentID != nil {
			return "already confirmed", intent.PaymentID, nil
		}

		methodID, err := s.methodFor(ctx, intent.ProviderCode)
		if err != nil {
			return "", nil, err
		}

		reference := intent.ProviderRef
		if reference == nil || *reference == "" {
			reference = &parsed.ProviderRef
		}
		posted, err := s.payments.RecordPayment(ctx, actor, RecordPaymentInput{
			AccountID:       intent.AccountID,
			Amount:          intent.Amount,
			PaymentMethodID: methodID,
			MethodReference: reference,
			// The key was fixed when the intent was created, so two deliveries
			// of one confirmation reach the same payment rather than two.
			IdempotencyKey: intent.PaymentIdempotencyKey,
			Notes:          ptr("collected through " + intent.ProviderCode),
		})
		if err != nil {
			return "", nil, err
		}

		if err := intent.Succeed(posted.Payment.ID, now); err != nil {
			return "", nil, err
		}
		if err := s.repo.Update(ctx, intent); err != nil {
			return "", nil, err
		}

		if err := s.record(ctx, port.AuditEntry{
			EntityType:     "payment_intent",
			EntityID:       &intent.ID,
			Action:         "intent.confirmed",
			Actor:          actor,
			AccountID:      &intent.AccountID,
			StudentID:      &intent.StudentID,
			AcademicYearID: &intent.AcademicYearID,
			Metadata: map[string]any{
				"provider":   intent.ProviderCode,
				"amount":     intent.Amount.Int64(),
				"payment_id": posted.Payment.ID.String(),
				"receipt_no": valueOr(posted.Payment.ReceiptNo),
			},
		}); err != nil {
			return "", nil, err
		}
		return "payment posted", &posted.Payment.ID, nil

	case payment.IntentFailed:
		if err := intent.Fail(parsed.FailureCode, parsed.FailureReason, now); err != nil {
			return "", nil, err
		}
		if err := s.repo.Update(ctx, intent); err != nil {
			return "", nil, err
		}
		return "marked failed", nil, s.record(ctx, port.AuditEntry{
			EntityType: "payment_intent",
			EntityID:   &intent.ID,
			Action:     "intent.failed",
			Actor:      actor,
			AccountID:  &intent.AccountID,
			StudentID:  &intent.StudentID,
			Metadata: map[string]any{
				"provider": intent.ProviderCode,
				"code":     parsed.FailureCode,
			},
		})

	default:
		// Still pending. Nothing to do but keep the delivery on file: the
		// provider will send another, or the poll below will ask.
		return "no change: still pending", nil, nil
	}
}

// PollStatus asks a provider directly about an intent whose callback never
// arrived.
//
// The ordinary end of a student closing the browser at the wrong moment. The
// answer goes through exactly the same path a callback would, including the
// duplicate guard, so a poll and a late callback cannot post twice.
func (s *IntentService) PollStatus(ctx context.Context, actor shared.Actor, intentID shared.ID) (*HandleCallbackResult, error) {
	if err := actor.RequireAnyRole("PollPaymentStatus",
		shared.RoleCashier, shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleStudent); err != nil {
		return nil, err
	}

	intent, err := s.repo.GetByID(ctx, intentID)
	if err != nil {
		return nil, err
	}
	if intent.ProviderRef == nil {
		return nil, shared.PreconditionFailed("intent.not_started",
			"this payment was never handed to the provider, so there is nothing to ask about")
	}

	provider, err := s.registry.Get(intent.ProviderCode)
	if err != nil {
		return nil, err
	}
	parsed, err := provider.FetchStatus(ctx, *intent.ProviderRef)
	if err != nil {
		return nil, err
	}

	result := &HandleCallbackResult{}
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		event := &payment.ProviderEvent{
			ProviderCode:    provider.Code(),
			IntentID:        &intent.ID,
			ExternalEventID: parsed.ExternalEventID,
			EventType:       parsed.EventType,
			SignatureOK:     true,
			Payload:         parsed.Payload,
		}
		duplicate, err := s.repo.RecordEvent(ctx, event)
		if err != nil {
			return err
		}

		locked, err := s.repo.GetForUpdate(ctx, intent.ID)
		if err != nil {
			return err
		}
		result.Intent = locked
		if duplicate {
			result.Duplicate, result.PaymentID = true, locked.PaymentID
			return nil
		}
		if err := parsed.ValidateAgainst(locked); err != nil {
			return err
		}

		outcome, paymentID, err := s.applyOutcome(ctx, locked, *parsed)
		if err != nil {
			return err
		}
		result.PaymentID = paymentID
		return s.repo.MarkEventProcessed(ctx, event.ID, outcome, nowOr(s.deps.Clock))
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ExpireStale closes intents whose window passed with no answer.
//
// Expiry is not failure and the distinction is deliberate: nobody refused the
// payment, the answer simply never came, and the money may still move. An
// expired intent therefore stays visible to the settlement import, which is
// where a branch payment made on the last day of the window turns up.
func (s *IntentService) ExpireStale(ctx context.Context, limit int) (int, error) {
	now := nowOr(s.deps.Clock)

	stale, err := s.repo.ListStale(ctx, now, limit)
	if err != nil {
		return 0, err
	}

	expired := 0
	for _, intent := range stale {
		err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
			locked, err := s.repo.GetForUpdate(ctx, intent.ID)
			if err != nil {
				return err
			}
			if locked.Status.Terminal() || locked.Status == payment.IntentExpired {
				return nil
			}
			if err := locked.Expire(now); err != nil {
				return err
			}
			if err := s.repo.Update(ctx, locked); err != nil {
				return err
			}
			expired++
			return s.record(ctx, port.AuditEntry{
				EntityType: "payment_intent",
				EntityID:   &locked.ID,
				Action:     "intent.expired",
				Actor:      shared.SystemActor(),
				AccountID:  &locked.AccountID,
				StudentID:  &locked.StudentID,
				Metadata:   map[string]any{"provider": locked.ProviderCode},
			})
		})
		if err != nil {
			s.logf(ctx, "expiring a stale payment intent", err)
		}
	}
	return expired, nil
}

// ListForAccount returns an account's collection requests.
func (s *IntentService) ListForAccount(ctx context.Context, actor shared.Actor, accountID shared.ID) ([]*payment.Intent, error) {
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if actor.HasRole(shared.RoleStudent) {
		if actor.StudentID == nil || *actor.StudentID != account.StudentID {
			return nil, shared.Forbidden("intent.not_your_account",
				"this account belongs to another student")
		}
	} else if err := actor.RequireScope("ListPaymentIntents", &account.CollegeID, &account.DepartmentID); err != nil {
		return nil, err
	}
	return s.repo.ListForAccount(ctx, accountID)
}

// Providers lists the electronic channels this deployment offers.
func (s *IntentService) Providers() []payments.ProviderInfo { return s.registry.Describe() }

// resolvePaymentMethod maps a provider onto the payment method its collections
// are recorded under.
//
// Looked up by code rather than configured as an identifier, because the method
// rows are seeded by migration and an operator should not have to paste a UUID
// into configuration to switch a provider on. A provider with no matching
// method falls back to ONLINE, which every deployment has.
func (s *IntentService) resolvePaymentMethod(ctx context.Context, providerCode string) (shared.ID, error) {
	methods, err := s.deps.Reference.ListPaymentMethods(ctx, true)
	if err != nil {
		return shared.NilID, err
	}

	var fallback *shared.ID
	for _, method := range methods {
		switch strings.ToUpper(method.Code) {
		case strings.ToUpper(providerCode):
			return method.ID, nil
		case "ONLINE":
			id := method.ID
			fallback = &id
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return shared.NilID, shared.PreconditionFailed("intent.no_payment_method",
		"no payment method matches provider %q and there is no ONLINE method to fall back on",
		providerCode).
		WithDetail("remedy", "add a payment method with this provider's code")
}

// recordRejected stores a callback that failed verification.
func (s *IntentService) recordRejected(ctx context.Context, providerCode string, envelope payments.CallbackEnvelope, cause error) {
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		event := &payment.ProviderEvent{
			ProviderCode: providerCode,
			// No trustworthy identifier is available — the body did not
			// verify — so one is minted. Two rejected deliveries are two rows,
			// which is what makes a burst of them visible.
			ExternalEventID: "rejected:" + shared.NewID().String(),
			EventType:       "callback.rejected",
			SignatureOK:     false,
			Payload: map[string]any{
				"reason":     shared.CodeOf(cause),
				"body_bytes": len(envelope.Body),
			},
		}
		_, err := s.repo.RecordEvent(ctx, event)
		return err
	})
	if err != nil {
		s.logf(ctx, "recording a rejected provider callback", err)
	}
}

func (s *IntentService) logf(ctx context.Context, what string, err error) {
	if s.deps.Log == nil {
		return
	}
	s.deps.Log.WarnContext(ctx, what, slog.String("error", err.Error()))
}

var _ = time.Now
