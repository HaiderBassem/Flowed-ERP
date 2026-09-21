package app

import (
	"context"
	"time"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// nearDuplicateWindow is how long after a payment an identical one is treated
// as a probable retry rather than a genuine second collection.
//
// This is the safety net beneath the idempotency key, for the case the key
// cannot cover: a terminal that restarted, generated a fresh key, and resent.
// It warns rather than blocks, because a student paying the same amount twice
// in five minutes is unusual but not impossible.
const nearDuplicateWindow = 5 * time.Minute

// PaymentService handles the money-in and money-out commands.
type PaymentService struct {
	deps Deps
	auditor
}

// NewPaymentService wires the payment commands.
func NewPaymentService(d Deps) *PaymentService {
	return &PaymentService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// RecordPaymentInput is what a cashier submits.
type RecordPaymentInput struct {
	AccountID       shared.ID
	Amount          money.Amount
	PaymentMethodID shared.ID
	MethodReference *string
	PayerName       *string
	Notes           *string
	IdempotencyKey  string
	PayloadHash     string
	// TargetInstallments lets a cashier direct the money at specific
	// installments. Empty means the default: oldest due date first.
	TargetInstallments []billing.Allocation
	// ConfirmedDistinct acknowledges the near-duplicate warning, recorded in
	// the audit trail so an override is traceable.
	ConfirmedDistinct bool
}

// RecordPaymentResult is what the cashier gets back.
type RecordPaymentResult struct {
	Payment      *payment.Payment
	Allocations  []*payment.Allocation
	CreditAmount money.Amount
	Remaining    money.Amount
	Duplicate    bool
}

// RecordPayment collects money against a financial account.
//
// This is the busiest command in the system and the one with the most ways to
// go wrong, so the ordering below is deliberate rather than incidental. Locks
// are taken account first, then year, then installments, then the receipt
// counter — the same order in every command, which is what makes deadlock
// between two cashiers impossible rather than merely unlikely.
func (s *PaymentService) RecordPayment(ctx context.Context, actor shared.Actor, in RecordPaymentInput) (*RecordPaymentResult, error) {
	if !in.Amount.IsPositive() {
		return nil, shared.Validation("payment.non_positive_amount",
			"a payment must be greater than zero, got %s", in.Amount)
	}
	if in.IdempotencyKey == "" {
		return nil, shared.Validation("payment.idempotency_key_required",
			"an idempotency key is required so a retried submission cannot collect twice")
	}

	// A key already used returns the original receipt rather than a second
	// collection. Checked outside the transaction as a fast path; the unique
	// index is what actually guarantees it.
	if existing, err := s.deps.Payments.GetByIdempotencyKey(ctx, in.IdempotencyKey); err == nil && existing != nil {
		return s.replayPayment(ctx, existing)
	}

	var (
		result *RecordPaymentResult
		// Hoisted out of the closure so the metric below can name the method
		// without re-reading it. It is only read after Write returned nil.
		methodCode string
	)
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		// 1. The account, locked. Everything else follows from this row, and
		//    holding it serialises two cashiers working on the same student.
		account, err := s.deps.Accounts.GetForUpdate(ctx, in.AccountID)
		if err != nil {
			return err
		}
		if err := account.RequirePaymentAccepted(); err != nil {
			return err
		}

		// 2. The posting year, locked.
		//
		//    Reading the year without a lock is a real bug, not a theoretical
		//    one: under read-committed, a payment can read "open", the close
		//    command can commit, and the payment can then commit into a year
		//    whose books were just shut. The lock serialises the two.
		//
		//    Note which year this is. Debt on a closed year is still
		//    collectible; the collection simply posts against the currently
		//    open year and allocates to the old year's installments.
		postingYear, err := s.resolvePostingYear(ctx, account)
		if err != nil {
			return err
		}
		if err := postingYear.RequireFinancialPosting("recording a payment"); err != nil {
			return err
		}

		method, err := s.deps.Reference.GetPaymentMethod(ctx, in.PaymentMethodID)
		if err != nil {
			return err
		}
		if method.RequiresReference && (in.MethodReference == nil || *in.MethodReference == "") {
			return shared.Validation("payment.reference_required",
				"paying by %s requires the bank or terminal reference number", method.NameAr).
				WithDetail("method", method.Code)
		}

		if err := s.guardNearDuplicate(ctx, in, method.ID); err != nil {
			return err
		}

		// 4. The open installments, locked in installment-number order.
		open, err := s.deps.Installments.ListOpenForUpdate(ctx, account.ID)
		if err != nil {
			return err
		}

		plan, err := buildAllocationPlan(in, open)
		if err != nil {
			return err
		}

		record, err := payment.New(payment.NewParams{
			AccountID:       account.ID,
			StudentID:       account.StudentID,
			EnrollmentID:    account.EnrollmentID,
			PostingYearID:   postingYear.ID,
			Amount:          in.Amount,
			PaymentMethodID: method.ID,
			MethodReference: in.MethodReference,
			CashierUserID:   actor.UserID,
			IdempotencyKey:  in.IdempotencyKey,
			PayloadHash:     in.PayloadHash,
			PayerName:       in.PayerName,
			Notes:           in.Notes,
			PaidAt:          now,
		})
		if err != nil {
			return err
		}

		// 5. The receipt number, taken last so a hot counter cannot deadlock
		//    against an account lock. Allocated here rather than at row
		//    creation: a rolled-back transaction returns the number, which is
		//    what keeps the printed sequence gapless.
		receiptNo, seriesID, err := s.nextReceiptNumber(ctx, payment.SeriesPayment, postingYear)
		if err != nil {
			return err
		}
		if err := record.Post(receiptNo, seriesID, now); err != nil {
			return err
		}

		allocations := make([]*payment.Allocation, 0, len(plan.Allocations))
		byID := make(map[shared.ID]*billing.Installment, len(open))
		for _, inst := range open {
			byID[inst.ID] = inst
		}
		for _, a := range plan.Allocations {
			allocation := payment.NewAllocation(record.ID, a.InstallmentID, a.Amount)
			allocation.CreatedBy = &actor.UserID
			allocations = append(allocations, allocation)

			inst := byID[a.InstallmentID]
			if inst == nil {
				return shared.InvariantViolation("payment.allocation_target_missing",
					"the allocation targets installment %s, which was not among the locked rows",
					a.InstallmentID)
			}
			if err := inst.ApplyAllocation(a.Amount); err != nil {
				return err
			}
			if err := s.deps.Installments.Update(ctx, inst); err != nil {
				return err
			}
		}

		if err := s.deps.Payments.Create(ctx, record, allocations); err != nil {
			return err
		}

		// 6. Anything the plan could not place becomes credit — a lockable row,
		//    not a number, so that carrying it forward and refunding it in cash
		//    cannot both spend it.
		if plan.CreditAmount.IsPositive() {
			credit, err := billing.NewCreditEntry(
				account.ID, account.StudentID, plan.CreditAmount, billing.CreditFromOverpayment)
			if err != nil {
				return err
			}
			credit.SourceReference = &record.ID
			credit.CreatedBy = &actor.UserID
			credit.Reason = ptr("payment exceeded the outstanding installments")
			if err := s.deps.Accounts.CreateCredit(ctx, credit); err != nil {
				return err
			}
			if err := account.AddCredit(plan.CreditAmount); err != nil {
				return err
			}
		}

		if err := account.ApplyPayment(in.Amount, now); err != nil {
			return err
		}
		if err := s.deps.Accounts.Update(ctx, account); err != nil {
			return err
		}

		if err := s.record(ctx, port.AuditEntry{
			EntityType:     "payment",
			EntityID:       &record.ID,
			Action:         "payment.recorded",
			Actor:          actor,
			After:          snapshotOf(record),
			AcademicYearID: &postingYear.ID,
			StudentID:      &account.StudentID,
			AccountID:      &account.ID,
			Metadata: map[string]any{
				"receipt_no":         receiptNo,
				"amount":             in.Amount.Int64(),
				"method":             method.Code,
				"allocation_count":   len(allocations),
				"credit_created":     plan.CreditAmount.Int64(),
				"confirmed_distinct": in.ConfirmedDistinct,
			},
		}); err != nil {
			return err
		}

		result = &RecordPaymentResult{
			Payment:      record,
			Allocations:  allocations,
			CreditAmount: plan.CreditAmount,
			Remaining:    account.Remaining(),
		}
		methodCode = method.Code
		return nil
	})
	if err != nil {
		// Counted by the refusal's machine code, never by student or account:
		// a desk stuck on one code all morning is a desk not collecting, and
		// the code says which rule is stopping it. A code the metric does not
		// recognise is a rule nobody expected to be hit.
		s.deps.Metrics.PaymentFailure(ctx, shared.CodeOf(err))
		return nil, err
	}

	// Counted here rather than inside the transaction. A counter incremented
	// before the commit counts money that a rollback never took, and a
	// dashboard that disagrees with the ledger is worse than no dashboard: the
	// ledger is the one that is right, and every hour spent reconciling the
	// two is an hour spent on an artefact of the instrumentation.
	s.deps.Metrics.PaymentPosted(ctx, methodCode, in.Amount)

	return result, nil
}

// buildAllocationPlan chooses between cashier-directed and default allocation.
func buildAllocationPlan(in RecordPaymentInput, open []*billing.Installment) (*billing.AllocationPlan, error) {
	candidates := make([]billing.OpenInstallment, 0, len(open))
	for _, inst := range open {
		candidates = append(candidates, billing.OpenInstallment{
			ID:        inst.ID,
			Number:    inst.Number,
			DueDate:   inst.DueDate,
			Amount:    inst.Amount,
			PaidSoFar: inst.PaidAmount,
		})
	}
	if len(in.TargetInstallments) > 0 {
		return billing.AllocateToSpecific(in.Amount, in.TargetInstallments, candidates)
	}
	return billing.AllocatePayment(in.Amount, candidates)
}

// guardNearDuplicate blocks a probable retry unless the cashier confirms the
// collection is genuinely a second one.
func (s *PaymentService) guardNearDuplicate(ctx context.Context, in RecordPaymentInput, methodID shared.ID) error {
	if in.ConfirmedDistinct {
		return nil
	}
	recent, err := s.deps.Payments.FindNearDuplicate(ctx, in.AccountID, in.Amount, methodID, nearDuplicateWindow)
	if err != nil || recent == nil {
		// A failure here must not block a legitimate collection; the
		// idempotency key remains the real guarantee.
		return nil
	}
	receipt := ""
	if recent.ReceiptNo != nil {
		receipt = *recent.ReceiptNo
	}
	return shared.Conflict("payment.probable_duplicate",
		"an identical payment of %s was recorded for this student minutes ago on receipt %s; "+
			"confirm this is a separate collection to proceed",
		in.Amount, receipt).
		WithDetail("existing_receipt_no", receipt).
		WithDetail("existing_payment_id", recent.ID.String()).
		WithDetail("existing_paid_at", recent.PaidAt).
		WithDetail("remedy", "resubmit with confirmed_distinct set to true")
}

// replayPayment returns a previously recorded payment for a repeated
// idempotency key.
//
// The status check matters. If the original was voided in the meantime,
// replaying it as a success would have the terminal print a receipt for a
// collection that no longer exists — so a replay of a non-posted payment is an
// error the client must surface rather than a receipt it should print.
func (s *PaymentService) replayPayment(ctx context.Context, existing *payment.Payment) (*RecordPaymentResult, error) {
	if existing.Status != payment.StatusPosted {
		return nil, shared.Conflict("payment.replay_not_posted",
			"a payment with this idempotency key exists but is %s, not posted; do not issue a receipt",
			existing.Status).
			WithDetail("payment_id", existing.ID.String()).
			WithDetail("status", string(existing.Status))
	}
	account, err := s.deps.Accounts.GetByID(ctx, existing.AccountID)
	if err != nil {
		return nil, err
	}
	return &RecordPaymentResult{
		Payment:   existing,
		Remaining: account.Remaining(),
		Duplicate: true,
	}, nil
}

// resolvePostingYear decides which year's books a collection belongs to.
//
// Normally the account's own year. When that year has closed its books, the
// collection posts against the current open year instead — the university is
// still owed the money and must still be able to take it. Refusing would push
// cashiers into working around the system, which is worse than the accounting
// impurity this avoids.
func (s *PaymentService) resolvePostingYear(ctx context.Context, account *billing.Account) (*academic.Year, error) {
	year, err := s.deps.Years.GetForUpdate(ctx, account.AcademicYearID)
	if err != nil {
		return nil, err
	}
	if year.AcceptsFinancialPosting() {
		return year, nil
	}

	openYears, err := s.deps.Years.CurrentOpen(ctx)
	if err != nil {
		return nil, err
	}
	if len(openYears) == 0 {
		return nil, shared.PreconditionFailed("payment.no_open_year",
			"academic year %s is %s and no other year is open to post the collection against",
			year.Code, year.Status).
			WithDetail("account_year", year.Code)
	}

	// The most recent open year, locked for the same reason as above.
	target := openYears[len(openYears)-1]
	return s.deps.Years.GetForUpdate(ctx, target.ID)
}

// RequestVoid asks for a payment to be reversed.
//
// A cashier cannot void alone. Splitting the request from the execution makes
// the correction a two-person act and leaves a document behind, which is what
// the void register — the most useful fraud-detection report in the system —
// reads from.
func (s *PaymentService) RequestVoid(ctx context.Context, actor shared.Actor, paymentID shared.ID, reason string) (*payment.VoidRequest, error) {
	var request *payment.VoidRequest
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		target, err := s.deps.Payments.GetByID(ctx, paymentID)
		if err != nil {
			return err
		}
		if target.Status != payment.StatusPosted {
			return shared.PreconditionFailed("payment.not_posted",
				"only a posted payment can be voided; this one is %s", target.Status)
		}

		// Checked here as well as at execution so a cashier learns immediately
		// that the refund path is the right one.
		refundCount, err := s.deps.Payments.CountPostedRefunds(ctx, paymentID)
		if err != nil {
			return err
		}
		if refundCount > 0 {
			return shared.PreconditionFailed("payment.has_refunds",
				"this payment already carries %d posted refund(s); refund the remainder instead of voiding",
				refundCount).
				WithDetail("posted_refunds", refundCount)
		}

		request, err = payment.NewVoidRequest(paymentID, reason, actor.UserID)
		if err != nil {
			return err
		}
		if err := s.deps.VoidRequests.Create(ctx, request); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "void_request",
			EntityID:   &request.ID,
			Action:     "void.requested",
			Actor:      actor,
			After:      snapshotOf(request),
			StudentID:  &target.StudentID,
			AccountID:  &target.AccountID,
			Reason:     &reason,
			Metadata:   map[string]any{"payment_id": paymentID.String(), "amount": target.Amount.Int64()},
		})
	})
	if err != nil {
		return nil, err
	}
	return request, nil
}

// ExecuteVoid reverses a payment in full.
//
// The whole collection is unwound: every allocation is reversed, the credit it
// created is released, and the account's totals are reduced. The payment row
// stays exactly as it was, marked voided, and keeps its receipt number — an
// auditor scanning a receipt book should find a cancelled receipt in place,
// not a gap they have to go and explain.
func (s *PaymentService) ExecuteVoid(ctx context.Context, actor shared.Actor, requestID shared.ID) (*payment.Payment, error) {
	var voided *payment.Payment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		request, err := s.deps.VoidRequests.GetByID(ctx, requestID)
		if err != nil {
			return err
		}
		target, err := s.deps.Payments.GetByID(ctx, request.PaymentID)
		if err != nil {
			return err
		}

		account, err := s.deps.Accounts.GetForUpdate(ctx, target.AccountID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetForUpdate(ctx, target.PostingYearID)
		if err != nil {
			return err
		}
		if err := year.RequireFinancialPosting("voiding a payment"); err != nil {
			return err
		}

		// Re-checked under the lock. A refund could have been posted between
		// the request and its execution, and voiding on top of one returns
		// more money than was ever collected.
		refundCount, err := s.deps.Payments.CountPostedRefunds(ctx, target.ID)
		if err != nil {
			return err
		}

		if err := request.Execute(actor.UserID, now); err != nil {
			return err
		}
		if err := target.Void(actor.UserID, request.Reason, now, refundCount); err != nil {
			return err
		}
		target.VoidRequestID = &request.ID

		live, err := s.deps.Payments.LiveAllocations(ctx, target.ID)
		if err != nil {
			return err
		}
		allocations := make([]billing.ExistingAllocation, 0, len(live))
		for _, a := range live {
			allocations = append(allocations, *a)
		}

		// Credit this payment created is released along with it.
		creditToRelease := money.Zero
		credits, err := s.deps.Accounts.ListOpenCredits(ctx, account.StudentID)
		if err != nil {
			return err
		}
		for _, credit := range credits {
			if credit.SourceReference == nil || *credit.SourceReference != target.ID {
				continue
			}
			locked, err := s.deps.Accounts.GetCreditForUpdate(ctx, credit.ID)
			if err != nil {
				return err
			}
			available := locked.Available()
			if !available.IsPositive() {
				continue
			}
			if err := locked.Consume(available, now); err != nil {
				return err
			}
			locked.Status = billing.CreditExpired
			if err := s.deps.Accounts.UpdateCredit(ctx, locked); err != nil {
				return err
			}
			if err := s.deps.Accounts.RecordCreditConsumption(ctx, &billing.CreditConsumption{
				ID:            shared.NewID(),
				CreditEntryID: locked.ID,
				Amount:        available,
				Purpose:       billing.CreditForWriteOff,
				ConsumedAt:    now,
				ConsumedBy:    &actor.UserID,
			}); err != nil {
				return err
			}
			creditToRelease = creditToRelease.MustAdd(available)
		}

		plan, err := billing.PlanFullReversal(allocations, creditToRelease)
		if err != nil {
			return err
		}

		reversals := make([]*payment.Allocation, 0, len(plan.Reversals))
		for _, r := range plan.Reversals {
			reversal := payment.NewReversal(
				target.ID, r.InstallmentID, r.AllocationID, r.Amount, "void_request", request.ID)
			reversal.CreatedBy = &actor.UserID
			reversals = append(reversals, reversal)

			inst, err := s.deps.Installments.GetByID(ctx, r.InstallmentID)
			if err != nil {
				return err
			}
			if err := inst.ReverseAllocation(r.Amount); err != nil {
				return err
			}
			if err := s.deps.Installments.Update(ctx, inst); err != nil {
				return err
			}
		}
		if len(reversals) > 0 {
			if err := s.deps.Payments.CreateAllocations(ctx, reversals); err != nil {
				return err
			}
		}

		if err := account.ReversePayment(target.Amount, now); err != nil {
			return err
		}
		if creditToRelease.IsPositive() {
			if err := account.ConsumeCredit(creditToRelease); err != nil {
				return err
			}
		}

		if err := s.deps.Accounts.Update(ctx, account); err != nil {
			return err
		}
		if err := s.deps.Payments.Update(ctx, target); err != nil {
			return err
		}
		if err := s.deps.VoidRequests.Update(ctx, request); err != nil {
			return err
		}

		voided = target
		return s.record(ctx, port.AuditEntry{
			EntityType:     "payment",
			EntityID:       &target.ID,
			Action:         "payment.voided",
			Actor:          actor,
			After:          snapshotOf(target),
			AcademicYearID: &target.PostingYearID,
			StudentID:      &target.StudentID,
			AccountID:      &target.AccountID,
			Reason:         &request.Reason,
			Metadata: map[string]any{
				"receipt_no":      derefString(target.ReceiptNo),
				"amount":          target.Amount.Int64(),
				"reversals":       len(reversals),
				"credit_released": creditToRelease.Int64(),
				"requested_by":    request.RequestedBy.String(),
				"void_request_id": request.ID.String(),
			},
		})
	})
	if err != nil {
		s.deps.Metrics.PaymentFailure(ctx, shared.CodeOf(err))
		return nil, err
	}

	// Recorded after the commit, like every other money counter here. A void
	// rate that climbs is the earliest visible sign of a cashier reversing
	// receipts a student is still holding, and the void register is where that
	// suspicion is then confirmed.
	s.deps.Metrics.PaymentVoided(ctx, voided.Amount)

	return voided, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nextReceiptNumber takes the next number from the year's series, creating the
// series on first use.
//
// Creating it lazily rather than when the year opens is deliberate: a series
// that had to exist in advance would force somebody to remember a setup step
// at the worst possible moment, with a student waiting. The fast path stays a
// single statement — the series is only created when taking a number finds
// none.
func (s *PaymentService) nextReceiptNumber(
	ctx context.Context,
	kind payment.SeriesKind,
	year *academic.Year,
) (string, shared.ID, error) {
	receiptNo, seriesID, err := s.deps.Series.NextNumber(ctx, kind, year.ID)
	if err == nil {
		return receiptNo, seriesID, nil
	}
	if shared.CodeOf(err) != "number_series.missing" {
		return "", shared.NilID, err
	}

	// The prefix embeds the year, so a printed receipt says on its face which
	// book it came from — which is what a paper reconciliation needs.
	prefix := receiptPrefix(kind, year.Code)
	if _, err := s.deps.Series.EnsureSeries(ctx, kind, year.ID, prefix); err != nil {
		return "", shared.NilID, err
	}
	return s.deps.Series.NextNumber(ctx, kind, year.ID)
}

// receiptPrefix builds a series prefix, such as "R-2025-2026-".
func receiptPrefix(kind payment.SeriesKind, yearCode string) string {
	letter := "R"
	if kind == payment.SeriesRefund {
		letter = "RF"
	}
	return letter + "-" + yearCode + "-"
}

// VoidPayment raises and executes a void in one command.
//
// The two-step shape was a separation of duties: a cashier could ask for a
// void and only a finance manager could carry it out. With one operator
// account there is nobody on the other side, and what remained was a request
// state that existed only to be passed through — and a payment left in it is
// worse than one never voided, because the student has been told the receipt
// is cancelled while the ledger still counts the money.
//
// Both steps still run, and in order, because the guard that matters lives in
// the second one: a void is refused while any refund has been posted against
// the payment. Refunding 400,000 of a million and then voiding the whole
// receipt would pay out 1,400,000 against a million received, and that check
// is made again under the account lock, where it cannot be raced.
func (s *PaymentService) VoidPayment(
	ctx context.Context, actor shared.Actor, paymentID shared.ID, reason string,
) (*payment.Payment, error) {
	request, err := s.RequestVoid(ctx, actor, paymentID, reason)
	if err != nil {
		return nil, err
	}
	voided, err := s.ExecuteVoid(ctx, actor, request.ID)
	if err != nil {
		if domainErr, ok := shared.AsDomain(err); ok {
			// The request survives a failed execution and must be findable:
			// an operator who cannot see it raises a second one, and the
			// payment then carries two open void requests.
			return nil, domainErr.WithDetail("void_request_id", request.ID.String()).
				WithDetail("remedy", "this void was requested but not executed; finish or reject it")
		}
		return nil, err
	}
	return voided, nil
}
