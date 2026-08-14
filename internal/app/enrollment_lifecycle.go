package app

import (
	"context"
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// TreatmentOutcome is what applying a financial treatment did.
type TreatmentOutcome struct {
	Treatment    academic.FinancialTreatment
	Applied      bool
	Waived       money.Amount
	CreditRaised money.Amount
	// RemainingObligation is what the student owes on this account afterwards.
	RemainingObligation money.Amount
	// AccountID is the account it was applied to, absent when the enrollment
	// was never priced — a legitimate case the design names explicitly.
	AccountID *shared.ID
}

// applyFinancialTreatment settles the money when an enrollment stops being
// active.
//
// The whole point of this function is that it is called with an explicit
// decision. Before it existed, deferring, withdrawing, dropping out and
// transferring out changed the academic status and left the account exactly as
// it was: full debt, full schedule, and a student on the aging report for a
// year they did not attend. That was the "keep" treatment applied silently to
// every case, including the ones nobody would have chosen it for.
//
// Lock order is the system's usual one — account, then academic year — because
// the adjustment may have to post against a different, currently open year when
// the enrollment's own year has shut its books.
func (s *EnrollmentService) applyFinancialTreatment(
	ctx context.Context,
	actor shared.Actor,
	enrollment *academic.Enrollment,
	treatment academic.FinancialTreatment,
	chargeInstead money.Amount,
	reason string,
	now time.Time,
) (TreatmentOutcome, error) {
	outcome := TreatmentOutcome{Treatment: treatment}

	account, err := s.deps.Accounts.GetByEnrollment(ctx, enrollment.ID)
	if err != nil {
		if shared.KindOf(err) == shared.KindNotFound {
			// Registered but never priced. The design calls this out as a
			// legitimate state, and there is nothing to settle.
			return outcome, nil
		}
		return outcome, err
	}
	outcome.AccountID = &account.ID

	locked, err := s.deps.Accounts.GetForUpdate(ctx, account.ID)
	if err != nil {
		return outcome, err
	}

	plan, err := academic.PlanTreatment(
		treatment, locked.EffectiveNet(), locked.NetPaid(), chargeInstead)
	if err != nil {
		return outcome, err
	}
	outcome.RemainingObligation = plan.RemainingObligation

	if !treatment.ChangesMoney() || plan.Waived.IsZero() {
		outcome.Applied = treatment.ChangesMoney()
		return outcome, nil
	}

	// The obligation falls by a signed adjustment. The frozen net is never
	// rewritten, so the account keeps showing the fee it was charged beside a
	// dated row saying why less is owed.
	adjustment, err := billing.NewAdjustment(
		locked.ID, treatmentAdjustmentType(treatment), plan.Waived.Neg(), reason)
	if err != nil {
		return outcome, err
	}
	adjustment.PostedBy = &actor.UserID
	adjustment.PostedAt = now
	adjustment.ReferenceType = ptr("enrollment")
	adjustment.ReferenceID = &enrollment.ID
	if adjustment.RequiresApproval() {
		adjustment.ApprovedBy = &actor.UserID
		adjustment.ApprovedAt = &now
	}

	postingYear, err := s.resolveTreatmentYear(ctx, locked)
	if err != nil {
		return outcome, err
	}
	adjustment.PostingYearID = &postingYear.ID

	if err := locked.ApplyAdjustment(plan.Waived.Neg(), now); err != nil {
		return outcome, err
	}
	if err := s.deps.Accounts.CreateAdjustment(ctx, adjustment); err != nil {
		return outcome, err
	}

	// The plan still asks for the old amount. Shrink it from the furthest-out
	// installment backwards, so the obligations coming up soonest are the last
	// to disappear — which is what a student expects when a waiver is granted.
	installments, err := s.deps.Installments.ListOpenForUpdate(ctx, locked.ID)
	if err != nil {
		return outcome, err
	}
	if err := reduceOpenPlan(ctx, s.deps.Installments, installments, plan.Waived); err != nil {
		return outcome, err
	}

	// Anything collected above the new obligation becomes a credit rather than
	// a negative payment: the receipt is printed and the money is in the
	// drawer, and only a refund can move it back out.
	if plan.CreditToStudent.IsPositive() && plan.CreditToStudent > locked.CreditBalance {
		raise, err := plan.CreditToStudent.Sub(locked.CreditBalance)
		if err != nil {
			return outcome, shared.Internal("treatment.arithmetic", err,
				"computing the credit to raise")
		}
		credit, err := billing.NewCreditEntry(locked.ID, locked.StudentID, raise, billing.CreditFromWaiver)
		if err != nil {
			return outcome, err
		}
		credit.Reason = ptr(reason)
		credit.CreatedAt = now
		credit.CreatedBy = &actor.UserID
		credit.SourceReference = &enrollment.ID
		if err := s.deps.Accounts.CreateCredit(ctx, credit); err != nil {
			return outcome, err
		}
		if err := locked.AddCredit(raise); err != nil {
			return outcome, err
		}
		outcome.CreditRaised = raise
	}

	if err := s.deps.Accounts.Update(ctx, locked); err != nil {
		return outcome, err
	}

	outcome.Applied = true
	outcome.Waived = plan.Waived
	return outcome, nil
}

// treatmentAdjustmentType maps a treatment onto the adjustment kind that
// records it, so the ledger says why an obligation fell rather than only that
// it did.
func treatmentAdjustmentType(t academic.FinancialTreatment) billing.AdjustmentType {
	switch t {
	case academic.TreatmentWaiveAll, academic.TreatmentWaiveUnpaid, academic.TreatmentPartial:
		return billing.AdjustmentWaiver
	default:
		return billing.AdjustmentCorrection
	}
}

// resolveTreatmentYear picks the year the adjustment posts against.
//
// The enrollment's own year may have shut its books, and closing a year does
// not stop the university correcting what a student owes for it — the
// correction posts against the year that is open, exactly as a late collection
// does.
func (s *EnrollmentService) resolveTreatmentYear(ctx context.Context, account *billing.Account) (*academic.Year, error) {
	year, err := s.deps.Years.GetForUpdate(ctx, account.AcademicYearID)
	if err != nil {
		return nil, err
	}
	if year.AcceptsFinancialPosting() {
		return year, nil
	}
	open, err := s.deps.Years.CurrentOpen(ctx)
	if err != nil {
		return nil, err
	}
	if len(open) == 0 {
		return nil, shared.PreconditionFailed("treatment.no_open_year",
			"academic year %s is %s and no year is open to post the waiver against",
			year.Code, year.Status).
			WithDetail("remedy", "open the current academic year, or reopen this one for adjustment")
	}
	return s.deps.Years.GetForUpdate(ctx, open[len(open)-1].ID)
}

// reduceOpenPlan shrinks a plan by an amount no longer owed, consuming the
// furthest-out installments first.
//
// Shared with the credit-carry path in AccountService, which does the same
// thing for the same reason. An installment fully covered is marked waived
// rather than deleted: it stays on the statement, which is what makes the
// student's copy and the system's agree.
func reduceOpenPlan(
	ctx context.Context, repo port.InstallmentRepository, installments []*billing.Installment, amount money.Amount,
) error {
	remaining := amount
	for i := len(installments) - 1; i >= 0 && remaining.IsPositive(); i-- {
		inst := installments[i]
		reducible, err := inst.Amount.Sub(inst.PaidAmount)
		if err != nil {
			return shared.Internal("plan.arithmetic", err, "measuring installment %d", inst.Number)
		}
		if !reducible.IsPositive() {
			continue
		}

		take := money.Min(remaining, reducible)
		if take == inst.Amount {
			inst.Status = billing.InstallmentWaived
		} else {
			reduced, err := inst.Amount.Sub(take)
			if err != nil {
				return shared.Internal("plan.arithmetic", err, "reducing installment %d", inst.Number)
			}
			inst.Amount = reduced
			if inst.PaidAmount >= inst.Amount {
				inst.Status = billing.InstallmentPaid
			}
		}

		if err := repo.Update(ctx, inst); err != nil {
			return err
		}
		remaining, err = remaining.Sub(take)
		if err != nil {
			return shared.Internal("plan.arithmetic", err, "tracking the remaining reduction")
		}
	}
	return nil
}
