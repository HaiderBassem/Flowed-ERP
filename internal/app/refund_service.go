package app

import (
	"context"

	"flowed/internal/domain/academic"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// RefundService handles returning money to a student.
//
// A refund never edits the payment it reverses. A 500,000 collection with a
// 100,000 refund is two documents and a computed net of 400,000, not a
// rewritten 400,000 payment — the record of what was taken and what was given
// back is exactly what an auditor comes to see.
type RefundService struct {
	deps Deps
	auditor
}

// NewRefundService wires the refund commands.
func NewRefundService(d Deps) *RefundService {
	return &RefundService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// RequestRefundInput is a request to return money.
type RequestRefundInput struct {
	PaymentID       shared.ID
	Amount          money.Amount
	PaymentMethodID shared.ID
	Reason          string
	IdempotencyKey  *string
}

// RequestRefund opens a refund for approval.
//
// The cap is checked here and again at posting: the total returned against one
// payment may never exceed what that payment collected.
func (s *RefundService) RequestRefund(ctx context.Context, actor shared.Actor, in RequestRefundInput) (*payment.Refund, error) {
	var refund *payment.Refund
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		target, err := s.deps.Payments.GetByID(ctx, in.PaymentID)
		if err != nil {
			return err
		}
		if target.Status != payment.StatusPosted {
			return shared.PreconditionFailed("refund.payment_not_posted",
				"only a posted payment can be refunded; this one is %s", target.Status).
				WithDetail("payment_status", string(target.Status))
		}

		account, err := s.deps.Accounts.GetByID(ctx, target.AccountID)
		if err != nil {
			return err
		}

		// A payment sitting on a cancelled account belongs to a superseded
		// enrollment whose balance was already moved to a successor by
		// transfer adjustment. Refunding it here would return money the
		// successor account still counts as received, so the correction
		// belongs on that successor instead.
		if account.Status == billing.AccountCancelled {
			err := shared.PreconditionFailed("refund.superseded_account",
				"this payment belongs to a superseded enrollment whose balance was transferred; "+
					"post the correction against the replacement account instead")
			if account.SupersededByAccountID != nil {
				err = err.WithDetail("replacement_account_id", account.SupersededByAccountID.String())
			}
			return err
		}

		alreadyRefunded, err := s.deps.Payments.PostedRefundTotal(ctx, target.ID)
		if err != nil {
			return err
		}
		available, err := target.Amount.Sub(alreadyRefunded)
		if err != nil {
			return shared.Internal("refund.arithmetic", err, "computing the refundable remainder")
		}
		if in.Amount > available {
			return shared.PreconditionFailed("refund.exceeds_payment",
				"refunding %s would exceed the payment: %s was collected and %s has already been returned",
				in.Amount, target.Amount, alreadyRefunded).
				WithDetail("payment_amount", target.Amount.Int64()).
				WithDetail("already_refunded", alreadyRefunded.Int64()).
				WithDetail("available", available.Int64())
		}

		refund, err = payment.NewRefund(payment.NewRefundParams{
			PaymentID:       target.ID,
			AccountID:       target.AccountID,
			StudentID:       target.StudentID,
			PostingYearID:   target.PostingYearID,
			Amount:          in.Amount,
			PaymentMethodID: in.PaymentMethodID,
			Reason:          in.Reason,
			RequestedBy:     actor.UserID,
			IdempotencyKey:  in.IdempotencyKey,
		})
		if err != nil {
			return err
		}
		if err := s.deps.Refunds.Create(ctx, refund); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "refund",
			EntityID:   &refund.ID,
			Action:     "refund.requested",
			Actor:      actor,
			After:      snapshotOf(refund),
			StudentID:  &target.StudentID,
			AccountID:  &target.AccountID,
			Reason:     &in.Reason,
			Metadata: map[string]any{
				"payment_id": target.ID.String(),
				"receipt_no": derefString(target.ReceiptNo),
				"amount":     in.Amount.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return refund, nil
}

// ApproveRefund accepts a refund request. The approver may not be the
// requester; the domain, this layer, and a database constraint all say so.
func (s *RefundService) ApproveRefund(ctx context.Context, actor shared.Actor, refundID shared.ID) (*payment.Refund, error) {
	var refund *payment.Refund
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)
		var err error
		refund, err = s.deps.Refunds.GetForUpdate(ctx, refundID)
		if err != nil {
			return err
		}
		if err := refund.Approve(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Refunds.Update(ctx, refund); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "refund",
			EntityID:   &refund.ID,
			Action:     "refund.approved",
			Actor:      actor,
			After:      snapshotOf(refund),
			StudentID:  &refund.StudentID,
			AccountID:  &refund.AccountID,
			Metadata:   map[string]any{"amount": refund.Amount.Int64()},
		})
	})
	if err != nil {
		return nil, err
	}
	return refund, nil
}

// RejectRefund declines a refund request.
func (s *RefundService) RejectRefund(ctx context.Context, actor shared.Actor, refundID shared.ID, reason string) (*payment.Refund, error) {
	var refund *payment.Refund
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)
		var err error
		refund, err = s.deps.Refunds.GetForUpdate(ctx, refundID)
		if err != nil {
			return err
		}
		if err := refund.Reject(actor.UserID, reason, now); err != nil {
			return err
		}
		if err := s.deps.Refunds.Update(ctx, refund); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "refund",
			EntityID:   &refund.ID,
			Action:     "refund.rejected",
			Actor:      actor,
			After:      snapshotOf(refund),
			StudentID:  &refund.StudentID,
			AccountID:  &refund.AccountID,
			Reason:     &reason,
		})
	})
	if err != nil {
		return nil, err
	}
	return refund, nil
}

// PostRefund pays the money out.
//
// The reversal is scoped to the refunded payment's own allocations, and that
// scoping is the point. Unwinding "the newest installments" across the account
// would let a refund of payment A strip funding that payment B provided,
// leaving A's allocations overstated and B's understated — and a later void of
// B would then reverse the same money a second time and pay it out twice.
func (s *RefundService) PostRefund(ctx context.Context, actor shared.Actor, refundID shared.ID) (*payment.Refund, error) {
	var refund *payment.Refund
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		refund, err = s.deps.Refunds.GetForUpdate(ctx, refundID)
		if err != nil {
			return err
		}

		account, err := s.deps.Accounts.GetForUpdate(ctx, refund.AccountID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetForUpdate(ctx, refund.PostingYearID)
		if err != nil {
			return err
		}
		if err := year.RequireFinancialPosting("posting a refund"); err != nil {
			return err
		}

		source, err := s.deps.Payments.GetByID(ctx, refund.PaymentID)
		if err != nil {
			return err
		}

		// Re-checked under the lock: another refund may have been posted
		// against the same payment since this one was approved.
		alreadyRefunded, err := s.deps.Payments.PostedRefundTotal(ctx, source.ID)
		if err != nil {
			return err
		}
		projected, err := alreadyRefunded.Add(refund.Amount)
		if err != nil {
			return shared.Internal("refund.arithmetic", err, "totalling refunds against the payment")
		}
		if projected > source.Amount {
			return shared.PreconditionFailed("refund.exceeds_payment",
				"posting this refund would return %s against a payment of %s", projected, source.Amount).
				WithDetail("payment_amount", source.Amount.Int64()).
				WithDetail("would_total", projected.Int64())
		}

		live, err := s.deps.Payments.LiveAllocations(ctx, source.ID)
		if err != nil {
			return err
		}
		allocations := make([]billing.ExistingAllocation, 0, len(live))
		for _, a := range live {
			allocations = append(allocations, *a)
		}

		creditAvailable, creditRows, err := s.lockCreditsFromPayment(ctx, account.StudentID, source.ID, account.ID)
		if err != nil {
			return err
		}

		plan, err := billing.PlanReversal(refund.Amount, creditAvailable, allocations)
		if err != nil {
			return err
		}

		refundAllocations := make([]*payment.RefundAllocation, 0, len(plan.Reversals)+1)
		reversals := make([]*payment.Allocation, 0, len(plan.Reversals))

		if plan.CreditConsumed.IsPositive() {
			remaining := plan.CreditConsumed
			for _, credit := range creditRows {
				if !remaining.IsPositive() {
					break
				}
				take := money.Min(remaining, credit.Available())
				if !take.IsPositive() {
					continue
				}
				if err := credit.Consume(take, now); err != nil {
					return err
				}
				if err := s.deps.Accounts.UpdateCredit(ctx, credit); err != nil {
					return err
				}
				if err := s.deps.Accounts.RecordCreditConsumption(ctx, &billing.CreditConsumption{
					ID:              shared.NewID(),
					CreditEntryID:   credit.ID,
					Amount:          take,
					Purpose:         billing.CreditForCashRefund,
					TargetAccountID: &account.ID,
					TargetReference: &refund.ID,
					ConsumedAt:      now,
					ConsumedBy:      &actor.UserID,
				}); err != nil {
					return err
				}
				refundAllocations = append(refundAllocations, &payment.RefundAllocation{
					ID:            shared.NewID(),
					RefundID:      refund.ID,
					CreditEntryID: &credit.ID,
					Amount:        take,
					CreatedAt:     now,
				})
				remaining, err = remaining.Sub(take)
				if err != nil {
					return shared.Internal("refund.arithmetic", err, "consuming credit for the refund")
				}
			}
			if err := account.ConsumeCredit(plan.CreditConsumed); err != nil {
				return err
			}
		}

		for _, r := range plan.Reversals {
			reversal := payment.NewReversal(
				source.ID, r.InstallmentID, r.AllocationID, r.Amount, "refund", refund.ID)
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

			allocationID := r.AllocationID
			installmentID := r.InstallmentID
			refundAllocations = append(refundAllocations, &payment.RefundAllocation{
				ID:                   shared.NewID(),
				RefundID:             refund.ID,
				InstallmentID:        &installmentID,
				ReversesAllocationID: &allocationID,
				Amount:               r.Amount,
				CreatedAt:            now,
			})
		}

		if len(reversals) > 0 {
			if err := s.deps.Payments.CreateAllocations(ctx, reversals); err != nil {
				return err
			}
		}
		if len(refundAllocations) > 0 {
			if err := s.deps.Refunds.CreateAllocations(ctx, refundAllocations); err != nil {
				return err
			}
		}

		refundNo, seriesID, err := s.nextRefundNumber(ctx, year)
		if err != nil {
			return err
		}
		if err := refund.Post(refundNo, seriesID, actor.UserID, now); err != nil {
			return err
		}

		if err := account.ApplyRefund(refund.Amount, now); err != nil {
			return err
		}
		if err := s.deps.Accounts.Update(ctx, account); err != nil {
			return err
		}
		if err := s.deps.Refunds.Update(ctx, refund); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "refund",
			EntityID:       &refund.ID,
			Action:         "refund.posted",
			Actor:          actor,
			After:          snapshotOf(refund),
			AcademicYearID: &refund.PostingYearID,
			StudentID:      &refund.StudentID,
			AccountID:      &refund.AccountID,
			Reason:         &refund.Reason,
			Metadata: map[string]any{
				"refund_no":        refundNo,
				"amount":           refund.Amount.Int64(),
				"original_receipt": derefString(source.ReceiptNo),
				"reversals":        len(reversals),
				"credit_consumed":  plan.CreditConsumed.Int64(),
			},
		})
	})
	if err != nil {
		s.deps.Metrics.PaymentFailure(ctx, shared.CodeOf(err))
		return nil, err
	}

	// Only the posting is counted, not the request or the approval. The other
	// two move a document; this is the one that moves money.
	s.deps.Metrics.RefundPosted(ctx, refund.Amount)

	return refund, nil
}

// lockCreditsFromPayment locks the credit this refund may hand back and reports
// how much of it is still available.
//
// Locking is what stops the same credit being spent twice. Carrying it forward
// onto next year's account and refunding it in cash are different code paths
// locking different accounts; without a lock on the credit row itself, both
// would read the same balance and both would pay it out.
//
// Two kinds of credit qualify, and the second is here because its absence paid
// the same dinar out twice.
//
//  1. Credit this payment itself raised — an overpayment. Scoped to the payment
//     because unwinding another payment's credit would let a refund of A strip
//     the funding B provided.
//  2. Credit raised by waiving this account's obligation. A withdrawal treated
//     as waive_all reverses what is owed and turns everything already collected
//     into credit, but the paid installments and their allocations stay exactly
//     as they were — so the same money is represented twice, once as
//     allocations and once as credit. A refund that could not see the waiver
//     credit unwound the allocations instead and left the credit open: the
//     student took the cash and the university still owed it, and every
//     reconciliation check read clean because the refund never exceeded the
//     payment. Consuming it first collapses the two representations back into
//     one. Scoped to this payment's own account, so a waiver raised on another
//     enrollment stays out of reach.
func (s *RefundService) lockCreditsFromPayment(
	ctx context.Context, studentID, paymentID, accountID shared.ID,
) (money.Amount, []*billing.CreditEntry, error) {
	credits, err := s.deps.Accounts.ListOpenCredits(ctx, studentID)
	if err != nil {
		return 0, nil, err
	}

	var available money.Amount
	locked := make([]*billing.CreditEntry, 0, len(credits))
	for _, credit := range credits {
		fromThisPayment := credit.SourceReference != nil && *credit.SourceReference == paymentID
		waiverOnThisAccount := credit.Source == billing.CreditFromWaiver && credit.AccountID == accountID
		if !fromThisPayment && !waiverOnThisAccount {
			continue
		}
		row, err := s.deps.Accounts.GetCreditForUpdate(ctx, credit.ID)
		if err != nil {
			return 0, nil, err
		}
		if !row.Available().IsPositive() {
			continue
		}
		locked = append(locked, row)
		available, err = available.Add(row.Available())
		if err != nil {
			return 0, nil, shared.Internal("refund.arithmetic", err, "totalling available credit")
		}
	}
	return available, locked, nil
}

// nextRefundNumber takes the next number from the refund series, creating it
// on first use for the same reason payments do: a refund raised before the
// year's first collection must not need a separate setup step.
func (s *RefundService) nextRefundNumber(
	ctx context.Context, year *academic.Year,
) (string, shared.ID, error) {
	refundNo, seriesID, err := s.deps.Series.NextNumber(ctx, payment.SeriesRefund, year.ID)
	if err == nil {
		return refundNo, seriesID, nil
	}
	if shared.CodeOf(err) != "number_series.missing" {
		return "", shared.NilID, err
	}
	prefix := receiptPrefix(payment.SeriesRefund, year.Code)
	if _, err := s.deps.Series.EnsureSeries(
		ctx, payment.SeriesRefund, year.ID, prefix); err != nil {
		return "", shared.NilID, err
	}
	return s.deps.Series.NextNumber(ctx, payment.SeriesRefund, year.ID)
}

// IssueRefund raises, approves and pays out a refund in one command.
//
// The three-step shape — request, approve, post — was a separation of duties
// between a cashier who asks and a finance manager who agrees. With one
// operator account there is nobody on the other side of it, and what was left
// was three buttons the same person pressed in a row, with two intermediate
// states that existed only to be passed through. A refund abandoned between
// step one and step three is worse than no refund: the student is told the
// money is coming and the record says it was never paid.
//
// The steps themselves are unchanged and still run in order, because each one
// holds a check the next relies on — that the payment is posted, that nothing
// has been refunded past it, that the account is not superseded. They run in
// separate transactions, as they always did: RequestRefund takes the account
// lock and releases it, and wrapping all three in one outer transaction would
// hold that lock across three commands for no benefit.
func (s *RefundService) IssueRefund(
	ctx context.Context, actor shared.Actor, in RequestRefundInput,
) (*payment.Refund, error) {
	requested, err := s.RequestRefund(ctx, actor, in)
	if err != nil {
		return nil, err
	}
	if _, err := s.ApproveRefund(ctx, actor, requested.ID); err != nil {
		// Reported with the refund's own identifier, so an operator who sees
		// this can finish or reject the request by hand rather than raise a
		// second one against the same payment.
		return nil, withRefundID(err, requested.ID)
	}
	posted, err := s.PostRefund(ctx, actor, requested.ID)
	if err != nil {
		return nil, withRefundID(err, requested.ID)
	}
	return posted, nil
}

// withRefundID names the half-finished refund on an error from a later step.
//
// Without it an operator sees "this account is closed" and has no way to find
// the requested-but-unpaid refund the first step left behind, so they raise a
// second one against the same payment.
func withRefundID(err error, refundID shared.ID) error {
	if domainErr, ok := shared.AsDomain(err); ok {
		return domainErr.WithDetail("refund_id", refundID.String()).
			WithDetail("remedy", "this refund was raised but not paid out; finish or reject it")
	}
	return err
}
