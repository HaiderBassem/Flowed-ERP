package app

import (
	"context"
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// AccountService generates and maintains financial accounts.
type AccountService struct {
	deps Deps
	// sponsors materialises third-party commitments at generation. Nil-safe:
	// a deployment with no sponsors, and every test that does not care about
	// them, wires it as nil and nothing here changes.
	sponsors *SponsorService
	auditor
}

// WithSponsors attaches the sponsorship service.
//
// Set after construction rather than taken as a parameter because the two
// services are mutually useful — sponsorship needs accounts to attach
// commitments to — and threading both through one constructor would make the
// wiring order load-bearing.
func (s *AccountService) WithSponsors(sponsors *SponsorService) *AccountService {
	s.sponsors = sponsors
	return s
}

// NewAccountService wires the account commands.
func NewAccountService(d Deps) *AccountService {
	return &AccountService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// GenerateAccountInput asks for an enrollment to be priced.
type GenerateAccountInput struct {
	EnrollmentID shared.ID
	// TemplateID overrides the resolved installment template, for the case
	// where an administration negotiates a specific plan with a student.
	TemplateID *shared.ID
	// DryRun computes everything and returns the result without writing. Bulk
	// generation runs this first so a finance manager approves real numbers.
	DryRun bool
}

// GenerateAccountResult is the priced outcome.
type GenerateAccountResult struct {
	Account      *billing.Account
	Snapshot     []*billing.SnapshotLine
	Applications []*discount.Application
	Installments []*billing.Installment
	// CarriedCredit is money the student already had on account — from an
	// overpayment or from a superseded enrollment — that was applied here.
	CarriedCredit money.Amount
	// PendingDiscounts counts grants that materialised but await the annual
	// eligibility re-confirmation, so the caller can show a worklist.
	PendingDiscounts int
	// SponsorCommitments are what third parties owe for this account, frozen
	// here exactly as the discount applications are.
	SponsorCommitments []*billing.Commitment
	DryRun             bool
}

// GenerateFinancialAccount prices an enrollment and freezes the result.
//
// This is where configuration becomes history. The fee policy is resolved
// once, its components are copied into the account as snapshot lines, and the
// discounts are computed and frozen with a reference to the exact definition
// version used. Everything after this point reads the snapshot; nothing
// recomputes from configuration. A price change next week, or a discount
// revised next year, cannot reach this account, because the account holds no
// pointer to "the current value" — only to rows that can no longer change.
func (s *AccountService) GenerateFinancialAccount(ctx context.Context, actor shared.Actor, in GenerateAccountInput) (*GenerateAccountResult, error) {
	if err := actor.RequireAnyRole("GenerateFinancialAccount",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var result *GenerateAccountResult
	run := func(ctx context.Context) error {
		var err error
		result, err = s.generate(ctx, actor, in)
		return err
	}

	// A dry run reads under a consistent snapshot but must not be able to
	// write, which the read-only transaction guarantees at the database level
	// rather than by the code remembering to behave.
	var err error
	if in.DryRun {
		err = s.deps.Tx.Read(ctx, run)
	} else {
		err = s.deps.Tx.Write(ctx, run)
	}
	if err != nil {
		return nil, err
	}

	// A dry run priced an account it did not create. Counting it would put
	// obligation on the dashboard that nobody owes, which is the same class of
	// error as counting a rolled-back payment.
	if !in.DryRun {
		s.deps.Metrics.AccountGenerated(ctx, result.Account.NetTotal)
		for _, application := range result.Applications {
			s.deps.Metrics.DiscountApplied(ctx,
				string(application.Status), application.AppliedAmount)
		}
	}

	return result, nil
}

func (s *AccountService) generate(ctx context.Context, actor shared.Actor, in GenerateAccountInput) (*GenerateAccountResult, error) {
	now := nowOr(s.deps.Clock)

	enrollment, err := s.deps.Enrollments.GetByID(ctx, in.EnrollmentID)
	if err != nil {
		return nil, err
	}
	if !enrollment.IsLive() {
		return nil, shared.PreconditionFailed("account.enrollment_not_live",
			"an account can only be generated for a live enrollment; this one is %s",
			enrollment.Status).
			WithDetail("enrollment_status", string(enrollment.Status))
	}

	if existing, err := s.deps.Accounts.GetByEnrollment(ctx, in.EnrollmentID); err == nil && existing != nil {
		return nil, shared.Conflict("account.already_exists",
			"this enrollment already has a financial account").
			WithDetail("account_id", existing.ID.String())
	}

	year, err := s.deps.Years.GetByID(ctx, enrollment.AcademicYearID)
	if err != nil {
		return nil, err
	}
	if !in.DryRun {
		locked, err := s.deps.Years.GetForUpdate(ctx, year.ID)
		if err != nil {
			return nil, err
		}
		year = locked
		if err := year.RequireFinancialPosting("generating a financial account"); err != nil {
			return nil, err
		}
	}

	// An incoming hosted student whose home university collects the tuition
	// gets no account here. Creating one would put a debt on our books that
	// belongs on somebody else's.
	if hosting, err := s.deps.Enrollments.GetHostingRecord(ctx, enrollment.ID); err == nil && hosting != nil {
		if !hosting.GeneratesLocalAccount() {
			return nil, shared.PreconditionFailed("account.hosted_elsewhere",
				"this hosted student is billed by their %s institution under the hosting agreement",
				hosting.FeeCollector).
				WithDetail("hosting_direction", string(hosting.Direction)).
				WithDetail("fee_collector", string(hosting.FeeCollector))
		}
	}

	scope := port.FeeScope{
		AcademicYearID:    enrollment.AcademicYearID,
		CollegeID:         enrollment.CollegeID,
		DepartmentID:      enrollment.DepartmentID,
		Stage:             enrollment.Stage,
		StudyTypeID:       enrollment.StudyTypeID,
		StudentCategoryID: enrollment.StudentCategoryID,
	}

	// No matching policy is a hard stop. Defaulting an unpriced enrollment to
	// zero would enroll a student who owes nothing and nobody would notice
	// until the year's revenue came up short.
	policy, err := s.deps.FeePolicies.Resolve(ctx, scope)
	if err != nil {
		return nil, err
	}

	account := &billing.Account{
		ID:                   shared.NewID(),
		EnrollmentID:         enrollment.ID,
		AcademicYearID:       enrollment.AcademicYearID,
		StudentID:            enrollment.StudentID,
		CollegeID:            enrollment.CollegeID,
		DepartmentID:         enrollment.DepartmentID,
		StudyTypeID:          enrollment.StudyTypeID,
		Stage:                enrollment.Stage,
		FeePolicyID:          &policy.ID,
		FeePolicySpecificity: &policy.SpecificityScore,
		Status:               billing.AccountPending,
		GeneratedAt:          now,
		GeneratedBy:          &actor.UserID,
	}

	snapshot := billing.SnapshotFromPolicy(account.ID, policy)

	candidates, pendingByAssignment, err := s.collectDiscountCandidates(ctx, enrollment.StudentID, year.Code)
	if err != nil {
		return nil, err
	}

	components := make([]discount.ComponentAmount, 0, len(snapshot))
	for _, line := range snapshot {
		components = append(components, discount.ComponentAmount{
			Code:         line.ComponentCode,
			Amount:       line.Amount,
			Discountable: line.IsDiscountable,
		})
	}

	computation, err := discount.Compute(discount.Input{
		Components:         components,
		Candidates:         candidates,
		MaxTotalDiscountBP: policy.MaxDiscountBP,
	})
	if err != nil {
		return nil, err
	}

	applications := make([]*discount.Application, 0, len(computation.Applications))
	appliedTotal := money.Zero
	pendingCount := 0
	for _, r := range computation.Applications {
		status := discount.ApplicationApplied
		if pendingByAssignment[r.Candidate.AssignmentID] {
			// The grant is approved, but the definition requires eligibility
			// to be re-confirmed each year. It materialises now so nobody
			// forgets it, and waits on an officer before it reduces anything.
			status = discount.ApplicationPending
			pendingCount++
		}
		application := discount.NewApplication(account.ID, r, status)
		if status == discount.ApplicationApplied {
			application.AppliedAt = &now
			application.AppliedBy = &actor.UserID
			appliedTotal = appliedTotal.MustAdd(r.AppliedAmount)
		}
		applications = append(applications, application)
	}

	account.GrossTotal = computation.GrossTotal
	account.DiscountableBase = computation.DiscountableBase
	account.DiscountTotal = appliedTotal
	net, err := computation.GrossTotal.Sub(appliedTotal)
	if err != nil {
		return nil, shared.Internal("account.net_arithmetic", err, "computing the net total")
	}
	account.NetTotal = net

	installments, templateID, err := s.buildPlan(ctx, account, scope, year, in.TemplateID)
	if err != nil {
		return nil, err
	}
	account.InstallmentTemplateID = templateID

	// A net of zero means a full exemption cleared the account. It settles
	// immediately with no plan: zero-value installments would appear on every
	// overdue report and in every cashier's worklist, chasing nothing.
	if account.NetTotal.IsZero() {
		account.Status = billing.AccountExempt
		account.SettledAt = &now
		installments = nil
	}

	result := &GenerateAccountResult{
		Account:          account,
		Snapshot:         snapshot,
		Applications:     applications,
		Installments:     installments,
		PendingDiscounts: pendingCount,
		DryRun:           in.DryRun,
	}
	if in.DryRun {
		return result, nil
	}

	if err := s.deps.Accounts.Create(ctx, account, snapshot); err != nil {
		return nil, err
	}
	for _, application := range applications {
		if err := s.deps.Discounts.CreateApplication(ctx, application); err != nil {
			return nil, err
		}
	}
	if len(installments) > 0 {
		if err := s.deps.Installments.CreatePlan(ctx, installments); err != nil {
			return nil, err
		}
		if err := account.Activate(now); err != nil {
			return nil, err
		}
	}

	// Sponsorships are materialised here, beside the discount applications and
	// for the same reason: a commitment computed later would answer with
	// today's agreement rather than the one the student was admitted under.
	// Under the covers_debt mode this posts an adjustment, so it must run
	// before the plan is reconciled against what is owed.
	if s.sponsors != nil {
		commitments, err := s.sponsors.MaterialiseCommitments(
			ctx, actor, account, year.Code, account.DiscountableBase)
		if err != nil {
			return nil, err
		}
		result.SponsorCommitments = commitments
		if len(commitments) > 0 && len(installments) > 0 {
			// The plan was built against the pre-sponsorship net. Any share a
			// sponsor took off the student's obligation has to come out of it,
			// or the student is asked for money a ministry has already agreed
			// to pay.
			var covered money.Amount
			for _, commitment := range commitments {
				if commitment.SettlementMode.ReducesStudentDebt() {
					covered = covered.MustAdd(commitment.CommittedAmount)
				}
			}
			if covered.IsPositive() {
				if err := reduceOpenPlan(ctx, s.deps.Installments, installments, covered); err != nil {
					return nil, err
				}
			}
		}
	}

	carried, err := s.applyCarriedCredit(ctx, actor, account, installments, now)
	if err != nil {
		return nil, err
	}
	result.CarriedCredit = carried

	if err := s.deps.Accounts.Update(ctx, account); err != nil {
		return nil, err
	}

	if err := s.record(ctx, port.AuditEntry{
		EntityType:     "financial_account",
		EntityID:       &account.ID,
		Action:         "account.generated",
		Actor:          actor,
		After:          snapshotOf(account),
		AcademicYearID: &account.AcademicYearID,
		StudentID:      &account.StudentID,
		AccountID:      &account.ID,
		Metadata: map[string]any{
			"fee_policy_id":          policy.ID.String(),
			"fee_policy_code":        policy.PolicyCode,
			"fee_policy_specificity": policy.SpecificityScore,
			"gross_total":            account.GrossTotal.Int64(),
			"discount_total":         account.DiscountTotal.Int64(),
			"net_total":              account.NetTotal.Int64(),
			"installment_count":      len(installments),
			"pending_discounts":      pendingCount,
			"carried_credit":         carried.Int64(),
		},
	}); err != nil {
		return nil, err
	}

	return result, nil
}

// applyCarriedCredit spends the student's unspent credit on the account just
// generated, oldest credit first.
//
// This is what completes a mid-year supersede. When a student changes
// department in March, their old account is retired and everything they paid
// becomes credit; without this step that money sits in a row nobody touches
// while the replacement account demands the full fee, and the student is asked
// to pay twice for one year.
//
// It also carries an ordinary overpayment forward: a student who overpaid last
// year starts this one with the excess already applied.
//
// Each credit row is locked before it is spent. Carrying credit forward and
// refunding it in cash are separate code paths locking different accounts, so
// without a lock on the credit itself both could read the same balance and
// both pay it out.
func (s *AccountService) applyCarriedCredit(
	ctx context.Context,
	actor shared.Actor,
	account *billing.Account,
	installments []*billing.Installment,
	now time.Time,
) (money.Amount, error) {
	owing := account.Remaining()
	if !owing.IsPositive() {
		return 0, nil
	}

	credits, err := s.deps.Accounts.ListOpenCredits(ctx, account.StudentID)
	if err != nil {
		return 0, err
	}

	var consumed money.Amount
	for _, entry := range credits {
		if !owing.IsPositive() {
			break
		}
		// Credit already sitting on this account is not "carried" — it is the
		// account's own overpayment and is already reflected in its balance.
		if entry.AccountID == account.ID {
			continue
		}

		locked, err := s.deps.Accounts.GetCreditForUpdate(ctx, entry.ID)
		if err != nil {
			return 0, err
		}
		take := money.Min(owing, locked.Available())
		if !take.IsPositive() {
			continue
		}

		if err := locked.Consume(take, now); err != nil {
			return 0, err
		}
		if err := s.deps.Accounts.UpdateCredit(ctx, locked); err != nil {
			return 0, err
		}

		// The credit belongs to the account it was created on — usually a
		// superseded enrollment's — and that account caches a credit balance
		// of its own. Spending the credit here without decrementing it there
		// leaves the two disagreeing, which the nightly reconciliation reports
		// as drift and which blocks the year from closing.
		if err := s.releaseSourceCredit(ctx, locked.AccountID, take); err != nil {
			return 0, err
		}

		if err := s.deps.Accounts.RecordCreditConsumption(ctx, &billing.CreditConsumption{
			ID:              shared.NewID(),
			CreditEntryID:   locked.ID,
			Amount:          take,
			Purpose:         billing.CreditForCarryForward,
			TargetAccountID: &account.ID,
			ConsumedAt:      now,
			ConsumedBy:      &actor.UserID,
		}); err != nil {
			return 0, err
		}

		// The credit reduces what is owed through an adjustment rather than by
		// rewriting the frozen net, so the account still shows the fee it was
		// charged beside a dated row explaining why less is due.
		adjustment, err := billing.NewAdjustment(
			account.ID, billing.AdjustmentTransferCreditIn, take.Neg(),
			"credit carried from a previous enrollment")
		if err != nil {
			return 0, err
		}
		adjustment.PostedBy = &actor.UserID
		adjustment.PostedAt = now
		adjustment.ReferenceType = ptr("credit_entry")
		adjustment.ReferenceID = &locked.ID
		adjustment.PostingYearID = &account.AcademicYearID

		if err := account.ApplyAdjustment(take.Neg(), now); err != nil {
			return 0, err
		}
		if err := s.deps.Accounts.CreateAdjustment(ctx, adjustment); err != nil {
			return 0, err
		}

		consumed = consumed.MustAdd(take)
		owing, err = owing.Sub(take)
		if err != nil {
			return 0, shared.Internal("account.credit_arithmetic", err, "reducing the outstanding balance")
		}
	}

	if !consumed.IsPositive() {
		return 0, nil
	}

	// The plan was built against the pre-credit net, so it now overstates what
	// is owed. Reshaping it from the newest installment backwards leaves the
	// nearest due dates intact, which is what a student expects when money
	// they already handed over is applied.
	if err := s.reducePlanBy(ctx, installments, consumed); err != nil {
		return 0, err
	}

	return consumed, nil
}

// releaseSourceCredit decrements the cached credit balance on the account a
// spent credit came from.
func (s *AccountService) releaseSourceCredit(ctx context.Context, sourceAccountID shared.ID, amount money.Amount) error {
	source, err := s.deps.Accounts.GetForUpdate(ctx, sourceAccountID)
	if err != nil {
		return err
	}
	if err := source.ConsumeCredit(amount); err != nil {
		return err
	}
	return s.deps.Accounts.Update(ctx, source)
}

// reducePlanBy shrinks an installment plan by an amount already covered,
// consuming the furthest-out installments first so that the obligations coming
// up soonest are the ones that disappear last.
func (s *AccountService) reducePlanBy(ctx context.Context, installments []*billing.Installment, amount money.Amount) error {
	remaining := amount
	for i := len(installments) - 1; i >= 0 && remaining.IsPositive(); i-- {
		inst := installments[i]
		take := money.Min(remaining, inst.Amount)

		if take == inst.Amount {
			// Fully covered: the obligation is met, not deleted, so it is
			// marked waived and stays visible on the statement.
			inst.Status = billing.InstallmentWaived
		} else {
			reduced, err := inst.Amount.Sub(take)
			if err != nil {
				return shared.Internal("account.plan_arithmetic", err, "reducing installment %d", inst.Number)
			}
			inst.Amount = reduced
		}

		if err := s.deps.Installments.Update(ctx, inst); err != nil {
			return err
		}
		var err error
		if remaining, err = remaining.Sub(take); err != nil {
			return shared.Internal("account.plan_arithmetic", err, "reducing the plan")
		}
	}
	return nil
}

// collectDiscountCandidates turns a student's approved grants into engine
// input, resolving each to the definition version currently published.
//
// The version identifier captured here is what the application freezes. Once
// stored it is never re-resolved, which is why a rate revised next year cannot
// reach back into this year's numbers.
func (s *AccountService) collectDiscountCandidates(ctx context.Context, studentID shared.ID, yearCode string) ([]discount.Candidate, map[shared.ID]bool, error) {
	assignments, err := s.deps.Discounts.ApprovedAssignmentsCovering(ctx, studentID, yearCode)
	if err != nil {
		return nil, nil, err
	}

	candidates := make([]discount.Candidate, 0, len(assignments))
	pending := make(map[shared.ID]bool, len(assignments))

	for _, assignment := range assignments {
		if !assignment.CoversYear(yearCode) {
			continue
		}
		definition, err := s.deps.Discounts.GetDefinition(ctx, assignment.DefinitionID)
		if err != nil {
			return nil, nil, err
		}
		if !definition.IsActive {
			continue
		}
		version, err := s.deps.Discounts.GetPublishedVersion(ctx, definition.ID)
		if err != nil {
			// A grant whose definition has no published version cannot be
			// priced. Skipping it silently would quietly overcharge the
			// student, so it stops the command.
			return nil, nil, shared.PreconditionFailed("discount.no_published_version",
				"discount %s is granted to this student but has no published version to compute from",
				definition.Code).
				WithDetail("definition_code", definition.Code).
				WithCause(err)
		}

		candidates = append(candidates, discount.Candidate{
			AssignmentID:        assignment.ID,
			DefinitionID:        definition.ID,
			DefinitionVersionID: version.ID,
			DefinitionCode:      definition.Code,
			ValueType:           version.ValueType,
			Rate:                version.Rate,
			FixedAmount:         version.FixedAmount,
			AppliesToComponents: version.AppliesToComponents,
			PerApplicationCap:   version.PerApplicationCap,
			Stackable:           version.Stackable,
			ExclusivityGroupID:  definition.ExclusivityGroupID,
			Priority:            version.Priority,
			IsFullExemption:     definition.IsFullExemption,
		})

		if definition.AnnualReconfirmation {
			pending[assignment.ID] = true
		}
	}

	return candidates, pending, nil
}

// buildPlan resolves an installment template and generates the schedule.
func (s *AccountService) buildPlan(
	ctx context.Context,
	account *billing.Account,
	scope port.FeeScope,
	year *academic.Year,
	override *shared.ID,
) ([]*billing.Installment, *shared.ID, error) {
	if account.NetTotal.IsZero() {
		return nil, nil, nil
	}

	var template *billing.InstallmentTemplate
	var err error
	if override != nil {
		template, err = s.deps.Templates.GetByID(ctx, *override)
	} else {
		template, err = s.deps.Templates.Resolve(ctx, scope)
	}
	if err != nil {
		return nil, nil, err
	}

	installments, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: account.ID,
		NetAmount: account.NetTotal,
		YearStart: year.StartDate,
		Lines:     template.Lines,
	})
	if err != nil {
		return nil, nil, err
	}
	return installments, &template.ID, nil
}

// PostAdjustmentInput describes a signed change to what an account owes.
type PostAdjustmentInput struct {
	AccountID shared.ID
	Type      billing.AdjustmentType
	Amount    money.Amount
	Reason    string
	Reference *shared.ID
}

// PostAdjustment records a change to an account's obligation after generation.
//
// The frozen net is never rewritten. A retroactive discount, a transfer from a
// superseded enrollment, a correction to a closed year — each is a signed row,
// and the amount owed is the frozen net plus their sum. That is what keeps
// "the fee was two million and here is every reason it is now one" answerable
// years later, instead of leaving a single number nobody can explain.
func (s *AccountService) PostAdjustment(ctx context.Context, actor shared.Actor, in PostAdjustmentInput) (*billing.Adjustment, error) {
	if err := actor.RequireAnyRole("PostAdjustment", shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var adjustment *billing.Adjustment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		account, err := s.deps.Accounts.GetForUpdate(ctx, in.AccountID)
		if err != nil {
			return err
		}

		adjustment, err = billing.NewAdjustment(account.ID, in.Type, in.Amount, in.Reason)
		if err != nil {
			return err
		}
		adjustment.PostedBy = &actor.UserID
		adjustment.PostedAt = now
		adjustment.ReferenceID = in.Reference

		// The kinds that reach into closed books need a named approver, and a
		// finance manager posting one signs it themselves.
		if adjustment.RequiresApproval() {
			adjustment.ApprovedBy = &actor.UserID
			adjustment.ApprovedAt = &now
		}

		// A correction to a closed year posts against the currently open one,
		// so it appears in this year's reconciliation while still attaching to
		// the account it corrects.
		postingYear, err := s.resolveAdjustmentYear(ctx, account)
		if err != nil {
			return err
		}
		adjustment.PostingYearID = &postingYear.ID

		if err := account.ApplyAdjustment(in.Amount, now); err != nil {
			return err
		}
		if err := s.deps.Accounts.CreateAdjustment(ctx, adjustment); err != nil {
			return err
		}
		if err := s.deps.Accounts.Update(ctx, account); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "account_adjustment",
			EntityID:       &adjustment.ID,
			Action:         "account.adjusted",
			Actor:          actor,
			After:          snapshotOf(adjustment),
			AcademicYearID: &postingYear.ID,
			StudentID:      &account.StudentID,
			AccountID:      &account.ID,
			Reason:         &in.Reason,
			Metadata: map[string]any{
				"adjustment_type": string(in.Type),
				"amount":          in.Amount.Int64(),
				"effective_net":   account.EffectiveNet().Int64(),
				"remaining":       account.Remaining().Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	s.deps.Metrics.AdjustmentPosted(ctx, adjustment.Amount)

	return adjustment, nil
}

func (s *AccountService) resolveAdjustmentYear(ctx context.Context, account *billing.Account) (*academic.Year, error) {
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
		return nil, shared.PreconditionFailed("adjustment.no_open_year",
			"academic year %s is %s and no year is open to post the adjustment against", year.Code, year.Status)
	}
	return s.deps.Years.GetForUpdate(ctx, open[len(open)-1].ID)
}
