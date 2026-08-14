package app

import (
	"context"
	"strings"

	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// AdjustPlanKind is what kind of change is being made to an installment plan.
type AdjustPlanKind string

const (
	// AdjustPlanReschedule moves due dates without changing any amount. The
	// common case: a family asks for the second installment after the harvest.
	AdjustPlanReschedule AdjustPlanKind = "reschedule"
	// AdjustPlanResplit redistributes the unpaid remainder into new shares.
	AdjustPlanResplit AdjustPlanKind = "resplit"
)

// AdjustInstallmentPlanInput describes a change to an account's schedule.
type AdjustInstallmentPlanInput struct {
	AccountID shared.ID
	Kind      AdjustPlanKind
	Reason    string

	// Reschedule: new due dates, keyed by installment id. Installments not
	// named keep their dates.
	NewDueDates map[shared.ID]shared.Date

	// Resplit: the shares the unpaid remainder is divided into. Either
	// ShareBP (basis points, summing to 10,000) or explicit due offsets from
	// the year's start. Paid and partly paid installments are never touched.
	Lines []billing.TemplateLine
	// TemplateID resplits using a published template instead of explicit
	// lines, which is how a cohort-wide change is applied to one account
	// without retyping its shares.
	TemplateID *shared.ID
}

// AdjustInstallmentPlanResult reports what the change did.
type AdjustInstallmentPlanResult struct {
	Revision     *port.PlanRevision
	Installments []*billing.Installment
}

// AdjustInstallmentPlan reschedules or re-splits the unpaid part of a plan.
//
// The design specified this command and the domain has carried the arithmetic
// for it since the first version; what was missing was the command itself, so a
// student who negotiated new dates could not be accommodated at all. That is
// precisely the sort of gap staff work around — by taking the money and writing
// the date in a notebook — and a workaround at a cashier's window is how the
// ledger stops being the record.
//
// Three rules hold, and each is enforced rather than documented:
//
//   - Money already paid is untouchable. An installment carrying an allocation
//     keeps its amount and its number; only the unpaid rows are replaced.
//   - The plan still sums to what is owed afterwards. The domain asserts it as
//     a post-condition, and a failure aborts the transaction.
//   - Nothing is deleted. Replaced installments are marked superseded and stay
//     readable, so a student's copy of an old schedule and the system's history
//     of it agree.
func (s *AccountService) AdjustInstallmentPlan(
	ctx context.Context, actor shared.Actor, in AdjustInstallmentPlanInput,
) (*AdjustInstallmentPlanResult, error) {
	// A supervisor's authority, not a cashier's: moving a due date decides when
	// the university is owed money, and re-splitting decides how much is due
	// when. Both are finance decisions.
	if err := actor.RequireAnyRole("AdjustInstallmentPlan",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, shared.Validation("plan.reason_required",
			"changing a schedule requires a reason; the student was told something, and this is where it is recorded")
	}
	switch in.Kind {
	case AdjustPlanReschedule, AdjustPlanResplit:
	default:
		return nil, shared.Validation("plan.unknown_adjustment",
			"%q is not a plan adjustment; use reschedule or resplit", in.Kind)
	}

	result := &AdjustInstallmentPlanResult{}
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		account, err := s.deps.Accounts.GetForUpdate(ctx, in.AccountID)
		if err != nil {
			return err
		}
		if err := actor.RequireScope("AdjustInstallmentPlan", &account.CollegeID, &account.DepartmentID); err != nil {
			return err
		}

		// The year lock, in the system's usual order after the account. A plan
		// change is a change to what is owed in that year, and a year closing
		// underneath it would leave the schedule disagreeing with shut books.
		year, err := s.deps.Years.GetForUpdate(ctx, account.AcademicYearID)
		if err != nil {
			return err
		}
		if err := year.RequireFinancialPosting("changing an installment plan"); err != nil {
			return err
		}

		existing, err := s.deps.Installments.ListForAccount(ctx, account.ID)
		if err != nil {
			return err
		}
		if len(existing) == 0 {
			return shared.PreconditionFailed("plan.no_plan",
				"this account has no installment plan to adjust").
				WithDetail("remedy", "a fully exempt account has no plan by design; nothing is owed")
		}

		before := planSummary(existing)

		var (
			replaced []*billing.Installment
			fresh    []*billing.Installment
			kept     []*billing.Installment
		)

		switch in.Kind {
		case AdjustPlanReschedule:
			kept, replaced, fresh, err = s.reschedulePlan(existing, in.NewDueDates)
		case AdjustPlanResplit:
			kept, replaced, fresh, err = s.resplitPlan(ctx, account, year.StartDate, existing, in)
		}
		if err != nil {
			return err
		}

		// Write the replacements first, then point the superseded rows at them.
		// The partial unique index on (account, number) excludes superseded
		// rows, so the old row has to stop being live before its replacement
		// can take its number — which is the same ordering the enrollment
		// supersede pair uses, for the same reason.
		replacedBy := make(map[shared.ID]shared.ID, len(replaced))
		for i, old := range replaced {
			old.Status = billing.InstallmentSuperseded
			if err := s.deps.Installments.Update(ctx, old); err != nil {
				return err
			}
			// Every replaced row is pointed at a replacement, and when the new
			// plan has fewer rows than the old one the extras point at the
			// last of them. Leaving them unmapped would break the chain a
			// reader follows forward from an old installment to the money it
			// became. The one case with no replacement at all — a credit that
			// retires the open rows outright — is carried by the plan
			// revision instead; trg_installment_supersede_pair checks that at
			// commit.
			if len(fresh) > 0 {
				replacedBy[old.ID] = fresh[min(i, len(fresh)-1)].ID
			}
		}
		if len(fresh) > 0 {
			if err := s.deps.Installments.CreatePlan(ctx, fresh); err != nil {
				return err
			}
		}
		if len(replacedBy) > 0 {
			if err := s.deps.Installments.SupersedePlan(ctx, account.ID, replacedBy); err != nil {
				return err
			}
		}

		live := append(append([]*billing.Installment{}, kept...), fresh...)
		// The invariant that matters: the live plan sums to what is owed. A
		// plan that does not add up asks the student for the wrong amount, and
		// no report downstream would notice.
		if err := billing.VerifyPlanSum(live, account.EffectiveNet()); err != nil {
			return err
		}
		result.Installments = live

		revision := &port.PlanRevision{
			ID:                 shared.NewID(),
			AccountID:          account.ID,
			PlanVersion:        nextPlanVersion(existing),
			Kind:               string(in.Kind),
			Reason:             in.Reason,
			InstallmentsBefore: int16(before.count),
			InstallmentsAfter:  int16(len(live)),
			UnpaidBefore:       before.unpaid,
			UnpaidAfter:        planSummary(live).unpaid,
			ApprovedBy:         &actor.UserID,
			CreatedBy:          &actor.UserID,
		}
		if s.deps.Lifecycle != nil {
			if err := s.deps.Lifecycle.RecordPlanRevision(ctx, revision); err != nil {
				return err
			}
		}
		result.Revision = revision

		return s.record(ctx, port.AuditEntry{
			EntityType:     "installment_plan",
			EntityID:       &account.ID,
			Action:         "plan.adjusted",
			Actor:          actor,
			Before:         snapshotOf(existing),
			After:          snapshotOf(live),
			AcademicYearID: &account.AcademicYearID,
			StudentID:      &account.StudentID,
			AccountID:      &account.ID,
			Reason:         &in.Reason,
			Metadata: map[string]any{
				"kind":                string(in.Kind),
				"installments_before": before.count,
				"installments_after":  len(live),
				"unpaid_before":       before.unpaid.Int64(),
				"unpaid_after":        revision.UnpaidAfter.Int64(),
				"superseded":          len(replaced),
				"plan_version":        revision.PlanVersion,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// reschedulePlan moves due dates, leaving every amount alone.
//
// A moved date is a real change to the schedule and is written as a new row
// rather than an update: the previous dates are what the student was told, and
// an overdue report run against last month's data must keep agreeing with
// itself. A paid installment cannot be moved — its date is when the money
// actually arrived, and nothing about the future changes that.
func (s *AccountService) reschedulePlan(
	existing []*billing.Installment, dates map[shared.ID]shared.Date,
) (kept, replaced, fresh []*billing.Installment, err error) {
	if len(dates) == 0 {
		return nil, nil, nil, shared.Validation("plan.no_dates",
			"a reschedule must name at least one installment and its new due date")
	}

	byID := make(map[shared.ID]*billing.Installment, len(existing))
	for _, inst := range existing {
		byID[inst.ID] = inst
	}

	for id := range dates {
		inst, ok := byID[id]
		if !ok {
			return nil, nil, nil, shared.NotFound("plan.unknown_installment",
				"installment %s does not belong to this account", id)
		}
		if inst.Status == billing.InstallmentSuperseded {
			return nil, nil, nil, shared.Validation("plan.superseded_installment",
				"installment %d has already been replaced and cannot be rescheduled", inst.Number)
		}
		if inst.Status == billing.InstallmentPaid {
			return nil, nil, nil, shared.PreconditionFailed("plan.installment_settled",
				"installment %d is paid; its date records when the money arrived", inst.Number).
				WithDetail("remedy", "reschedule the installments that are still open")
		}
	}

	version := nextPlanVersion(existing)
	for _, inst := range existing {
		if inst.Status == billing.InstallmentSuperseded {
			continue
		}
		newDate, moving := dates[inst.ID]
		if !moving || newDate == inst.DueDate {
			kept = append(kept, inst)
			continue
		}

		replacement := *inst
		replacement.ID = shared.NewID()
		replacement.DueDate = newDate
		replacement.PlanVersion = version
		// The paid amount travels with the row: a partly paid installment
		// whose date moves keeps the money already allocated to it.
		replaced = append(replaced, inst)
		fresh = append(fresh, &replacement)
	}

	if len(fresh) == 0 {
		return nil, nil, nil, shared.Validation("plan.no_change",
			"every named installment already carries the date given")
	}
	return kept, replaced, fresh, nil
}

// resplitPlan redistributes the unpaid remainder into new shares.
func (s *AccountService) resplitPlan(
	ctx context.Context,
	account *billing.Account,
	yearStart shared.Date,
	existing []*billing.Installment,
	in AdjustInstallmentPlanInput,
) (kept, replaced, fresh []*billing.Installment, err error) {
	lines := in.Lines
	if in.TemplateID != nil {
		template, err := s.deps.Templates.GetByID(ctx, *in.TemplateID)
		if err != nil {
			return nil, nil, nil, err
		}
		lines = template.Lines
	}
	if len(lines) == 0 {
		return nil, nil, nil, shared.Validation("plan.no_shares",
			"a resplit must give the shares to divide the remainder into, or a template to take them from")
	}

	outcome, err := billing.ResplitUnpaid(billing.ResplitSpec{
		Existing:     existing,
		NewNetAmount: account.EffectiveNet(),
		Lines:        lines,
		YearStart:    yearStart,
		PlanVersion:  nextPlanVersion(existing),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return outcome.Keep, outcome.Supersede, outcome.Fresh, nil
}

// planSummary counts the live installments and what is still unpaid on them.
type summary struct {
	count  int
	unpaid money.Amount
}

func planSummary(installments []*billing.Installment) summary {
	var s summary
	for _, inst := range installments {
		if inst.Status == billing.InstallmentSuperseded {
			continue
		}
		s.count++
		s.unpaid = s.unpaid.MustAdd(inst.Remaining())
	}
	return s
}

// nextPlanVersion is one past the highest version any row carries, so a
// revision is identifiable in the installment rows themselves and not only in
// the revision table.
func nextPlanVersion(installments []*billing.Installment) int16 {
	highest := int16(1)
	for _, inst := range installments {
		if inst.PlanVersion > highest {
			highest = inst.PlanVersion
		}
	}
	return highest + 1
}

// PlanRevisions returns an account's schedule history.
func (s *AccountService) PlanRevisions(ctx context.Context, actor shared.Actor, accountID shared.ID) ([]*port.PlanRevision, error) {
	if s.deps.Lifecycle == nil {
		return nil, nil
	}
	if err := actor.RequireAnyRole("PlanRevisions",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor,
		shared.RoleCashier, shared.RoleReportViewer); err != nil {
		return nil, err
	}
	return s.deps.Lifecycle.ListPlanRevisions(ctx, accountID)
}
