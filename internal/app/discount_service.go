package app

import (
	"context"
	"time"

	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// DiscountService handles granting, approving and revoking discounts.
type DiscountService struct {
	deps Deps
	auditor
}

// NewDiscountService wires the discount commands.
func NewDiscountService(d Deps) *DiscountService {
	return &DiscountService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// AssignDiscountInput grants a discount to a student.
type AssignDiscountInput struct {
	StudentID    shared.ID
	DefinitionID shared.ID
	Scope        discount.ScopeType
	// YearFromID and YearToID bound the scope. Both are ignored for an
	// all-years grant.
	YearFromID    *shared.ID
	YearToID      *shared.ID
	Justification *string
	DocumentRefs  []string
}

// AssignDiscount grants a discount, pending approval.
//
// The grant attaches to the student, not to an enrollment, because eligibility
// is a fact about the person: a lecturer's child is a lecturer's child in every
// year they study. What the grant does not do is reach into an account that
// already exists. It materialises when next year's account is generated, and
// applying it to the current one is a separate, separately approved
// adjustment.
func (s *DiscountService) AssignDiscount(ctx context.Context, actor shared.Actor, in AssignDiscountInput) (*discount.Assignment, error) {
	if err := actor.RequireAnyRole("AssignDiscount",
		shared.RoleRegistrar, shared.RoleAcademicOfficer, shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var assignment *discount.Assignment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if _, err := s.deps.Students.GetByID(ctx, in.StudentID); err != nil {
			return err
		}
		definition, err := s.deps.Discounts.GetDefinition(ctx, in.DefinitionID)
		if err != nil {
			return err
		}
		if !definition.IsActive {
			return shared.PreconditionFailed("discount.definition_retired",
				"discount %s is no longer active and cannot be granted", definition.Code)
		}

		// A definition with no published version cannot price anything. Better
		// to refuse the grant than to create one that silently computes to
		// nothing when the account is generated.
		if _, err := s.deps.Discounts.GetPublishedVersion(ctx, definition.ID); err != nil {
			return shared.PreconditionFailed("discount.no_published_version",
				"discount %s has no published version and cannot be granted yet", definition.Code).
				WithCause(err)
		}

		assignment, err = discount.NewAssignment(in.StudentID, in.DefinitionID, in.Scope, actor.UserID)
		if err != nil {
			return err
		}
		assignment.Justification = in.Justification
		assignment.DocumentRefs = in.DocumentRefs

		if err := s.resolveScopeYears(ctx, assignment, in); err != nil {
			return err
		}

		// Two live grants of the same discount over intersecting years would
		// apply it twice to one account.
		overlapping, err := s.deps.Discounts.HasOverlappingAssignment(
			ctx, in.StudentID, in.DefinitionID,
			derefString(assignment.ScopeCodeFrom), derefString(assignment.ScopeCodeTo), nil)
		if err != nil {
			return err
		}
		if overlapping {
			return shared.Conflict("discount.overlapping_assignment",
				"this student already holds %s over an overlapping range of years", definition.Code).
				WithDetail("definition_code", definition.Code)
		}

		if err := assignment.Submit(); err != nil {
			return err
		}
		if err := s.deps.Discounts.CreateAssignment(ctx, assignment); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "discount_assignment",
			EntityID:   &assignment.ID,
			Action:     "discount.assigned",
			Actor:      actor,
			After:      snapshotOf(assignment),
			StudentID:  &in.StudentID,
			Metadata: map[string]any{
				"definition_code": definition.Code,
				"scope":           string(in.Scope),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return assignment, nil
}

func (s *DiscountService) resolveScopeYears(ctx context.Context, a *discount.Assignment, in AssignDiscountInput) error {
	switch in.Scope {
	case discount.ScopeAllYears:
		return nil
	case discount.ScopeSingleYear:
		if in.YearFromID == nil {
			return shared.Validation("discount.year_required",
				"a single-year grant must name the year it covers")
		}
		year, err := s.deps.Years.GetByID(ctx, *in.YearFromID)
		if err != nil {
			return err
		}
		a.ScopeYearFrom = &year.ID
		a.ScopeCodeFrom = &year.Code
		return nil
	case discount.ScopeYearRange:
		if in.YearFromID == nil || in.YearToID == nil {
			return shared.Validation("discount.year_range_required",
				"a range grant must name both its first and last year")
		}
		from, err := s.deps.Years.GetByID(ctx, *in.YearFromID)
		if err != nil {
			return err
		}
		to, err := s.deps.Years.GetByID(ctx, *in.YearToID)
		if err != nil {
			return err
		}
		if to.Code < from.Code {
			return shared.Validation("discount.invalid_year_range",
				"the range ends (%s) before it starts (%s)", to.Code, from.Code)
		}
		a.ScopeYearFrom, a.ScopeCodeFrom = &from.ID, &from.Code
		a.ScopeYearTo, a.ScopeCodeTo = &to.ID, &to.Code
		return nil
	default:
		return shared.Validation("discount.invalid_scope", "unknown scope %q", in.Scope)
	}
}

// ApproveDiscountAssignment accepts a grant.
//
// The approver may not be the requester. A discount is money the university
// chooses not to collect, and one person should not be able to decide that
// alone — which is why the rule is enforced in the domain, here, and by a
// database constraint rather than trusted to any single layer.
func (s *DiscountService) ApproveDiscountAssignment(ctx context.Context, actor shared.Actor, assignmentID shared.ID) (*discount.Assignment, error) {
	if err := actor.RequireAnyRole("ApproveDiscountAssignment",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var assignment *discount.Assignment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		assignment, err = s.deps.Discounts.GetAssignment(ctx, assignmentID)
		if err != nil {
			return err
		}
		before := snapshotOf(assignment)
		if err := assignment.Approve(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Discounts.UpdateAssignment(ctx, assignment); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "discount_assignment",
			EntityID:   &assignment.ID,
			Action:     "discount.approved",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(assignment),
			StudentID:  &assignment.StudentID,
		})
	})
	if err != nil {
		return nil, err
	}
	return assignment, nil
}

// RevokeDiscountInput withdraws a grant.
type RevokeDiscountInput struct {
	AssignmentID shared.ID
	Reason       string
	Effect       discount.RevocationEffect
}

// RevokeDiscountAssignment withdraws an approved grant.
//
// The effect is the caller's choice, and the default is the kinder one.
// Prospective-only leaves this year's discount standing and stops future ones,
// because a student who arranged their finances around a discount should not
// find it withdrawn halfway through the year. Including the current year
// reverses the application through an audited adjustment, which raises what
// they owe — appropriate when the grant was obtained wrongly, not when
// eligibility simply lapsed.
func (s *DiscountService) RevokeDiscountAssignment(ctx context.Context, actor shared.Actor, in RevokeDiscountInput) (*discount.Assignment, error) {
	if err := actor.RequireAnyRole("RevokeDiscountAssignment",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var assignment *discount.Assignment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		assignment, err = s.deps.Discounts.GetAssignment(ctx, in.AssignmentID)
		if err != nil {
			return err
		}
		before := snapshotOf(assignment)

		if err := assignment.Revoke(actor.UserID, in.Reason, in.Effect, now); err != nil {
			return err
		}
		if err := s.deps.Discounts.UpdateAssignment(ctx, assignment); err != nil {
			return err
		}

		reversed := 0
		if in.Effect == discount.RevokeIncludeCurrentYear {
			reversed, err = s.reverseCurrentApplications(ctx, actor, assignment, in.Reason)
			if err != nil {
				return err
			}
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "discount_assignment",
			EntityID:   &assignment.ID,
			Action:     "discount.revoked",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(assignment),
			StudentID:  &assignment.StudentID,
			Reason:     &in.Reason,
			Metadata: map[string]any{
				"effect":                string(in.Effect),
				"applications_reversed": reversed,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return assignment, nil
}

// reverseCurrentApplications unwinds live applications of a revoked grant.
//
// The discount amount is restored to the account by a positive adjustment
// rather than by rewriting the frozen net. The account then shows what it
// always showed, plus a dated, reasoned row explaining why the obligation
// went back up — which is what somebody looking at it in two years needs.
func (s *DiscountService) reverseCurrentApplications(
	ctx context.Context,
	actor shared.Actor,
	assignment *discount.Assignment,
	reason string,
) (int, error) {
	now := nowOr(s.deps.Clock)

	accounts, err := s.deps.Accounts.ListForStudent(ctx, assignment.StudentID)
	if err != nil {
		return 0, err
	}

	reversed := 0
	for _, account := range accounts {
		if account.Status == billing.AccountCancelled {
			continue
		}
		year, err := s.deps.Years.GetByID(ctx, account.AcademicYearID)
		if err != nil {
			return reversed, err
		}
		// A closed year's applications are left exactly as they are. Reaching
		// back into settled books to raise an old obligation is what the
		// adjustment-window command exists for, deliberately and separately.
		if !year.AcceptsFinancialPosting() {
			continue
		}

		applications, err := s.deps.Discounts.ListApplications(ctx, account.ID)
		if err != nil {
			return reversed, err
		}

		for _, application := range applications {
			if application.AssignmentID != assignment.ID || application.Status != discount.ApplicationApplied {
				continue
			}

			locked, err := s.deps.Accounts.GetForUpdate(ctx, account.ID)
			if err != nil {
				return reversed, err
			}

			restored := application.AppliedAmount
			if err := application.Reverse(actor.UserID, reason, now); err != nil {
				return reversed, err
			}
			if err := s.deps.Discounts.UpdateApplication(ctx, application); err != nil {
				return reversed, err
			}

			adjustment, err := billing.NewAdjustment(
				locked.ID, billing.AdjustmentDiscountReversal, restored,
				"discount revoked: "+reason)
			if err != nil {
				return reversed, err
			}
			adjustment.PostedBy = &actor.UserID
			adjustment.ApprovedBy = &actor.UserID
			adjustment.ApprovedAt = &now
			adjustment.ReferenceType = ptr("discount_application")
			adjustment.ReferenceID = &application.ID
			adjustment.PostingYearID = &year.ID

			if err := locked.ApplyAdjustment(restored, now); err != nil {
				return reversed, err
			}
			if err := s.deps.Accounts.CreateAdjustment(ctx, adjustment); err != nil {
				return reversed, err
			}
			if err := s.deps.Accounts.Update(ctx, locked); err != nil {
				return reversed, err
			}
			reversed++
		}
	}

	return reversed, nil
}

// ConfirmApplicationInput confirms or declines a pending discount for a year.
type ConfirmApplicationInput struct {
	ApplicationID shared.ID
	Confirm       bool
	Reason        *string
}

// ConfirmDiscountApplication resolves a discount awaiting annual
// re-confirmation.
//
// Grants on definitions that require yearly verification materialise as
// pending: they appear on the account, so nobody forgets them, but they reduce
// nothing until an officer confirms the student is still eligible. This is
// what makes an all-years grant safe to give — the hardship a discount was
// awarded for may have ended, and silently applying it forever is both a
// budget leak and an audit finding.
func (s *DiscountService) ConfirmDiscountApplication(ctx context.Context, actor shared.Actor, in ConfirmApplicationInput) (*discount.Application, error) {
	if err := actor.RequireAnyRole("ConfirmDiscountApplication",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var application *discount.Application
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var account *billing.Account
		var err error
		application, account, err = s.loadApplication(ctx, in.ApplicationID)
		if err != nil {
			return err
		}
		before := snapshotOf(application)

		locked, err := s.deps.Accounts.GetForUpdate(ctx, account.ID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetForUpdate(ctx, locked.AcademicYearID)
		if err != nil {
			return err
		}
		if err := year.RequireFinancialPosting("confirming a discount"); err != nil {
			return err
		}

		action := "discount.application_declined"
		if in.Confirm {
			action = "discount.application_confirmed"
			if err := application.Confirm(actor.UserID, now); err != nil {
				return err
			}
			// Confirming after the account was generated lowers what is owed,
			// and the frozen net does not move: the reduction is a negative
			// adjustment, visible beside the original figure.
			reduction := application.AppliedAmount.Neg()
			adjustment, err := billing.NewAdjustment(
				locked.ID, billing.AdjustmentRetroactiveDiscount, reduction,
				"discount confirmed for the year")
			if err != nil {
				return err
			}
			adjustment.PostedBy = &actor.UserID
			adjustment.ReferenceType = ptr("discount_application")
			adjustment.ReferenceID = &application.ID
			adjustment.PostingYearID = &year.ID

			if err := locked.ApplyAdjustment(reduction, now); err != nil {
				return err
			}
			if err := s.deps.Accounts.CreateAdjustment(ctx, adjustment); err != nil {
				return err
			}
			if err := s.handleOverpayment(ctx, actor, locked, now); err != nil {
				return err
			}
			if err := s.deps.Accounts.Update(ctx, locked); err != nil {
				return err
			}
		} else {
			if err := application.Decline(actor.UserID, now); err != nil {
				return err
			}
		}

		if err := s.deps.Discounts.UpdateApplication(ctx, application); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "discount_application",
			EntityID:       &application.ID,
			Action:         action,
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(application),
			AcademicYearID: &locked.AcademicYearID,
			StudentID:      &locked.StudentID,
			AccountID:      &locked.ID,
			Reason:         in.Reason,
			Metadata:       map[string]any{"amount": application.AppliedAmount.Int64()},
		})
	})
	if err != nil {
		return nil, err
	}
	return application, nil
}

// handleOverpayment converts an over-collection into credit.
//
// A student who paid 1,500,000 against a two-million fee and is then granted
// half off now owes one million and has handed over five hundred thousand too
// much. The money is not refunded automatically: it becomes credit, which can
// offset what is left, carry forward to next year, or be returned in cash on
// an explicit request. Automatically pushing cash back out of the drawer on a
// clerical confirmation is not a decision this command should make.
func (s *DiscountService) handleOverpayment(ctx context.Context, actor shared.Actor, account *billing.Account, now time.Time) error {
	overpaid, err := account.NetPaid().Sub(account.EffectiveNet())
	if err != nil {
		return shared.Internal("discount.overpayment_arithmetic", err, "computing the overpayment")
	}
	if !overpaid.IsPositive() {
		return nil
	}

	credit, err := billing.NewCreditEntry(
		account.ID, account.StudentID, overpaid, billing.CreditFromRetroactiveDiscount)
	if err != nil {
		return err
	}
	credit.CreatedBy = &actor.UserID
	credit.Reason = ptr("discount applied after payment; the excess is held as credit")
	credit.CreatedAt = now

	if err := s.deps.Accounts.CreateCredit(ctx, credit); err != nil {
		return err
	}
	return account.AddCredit(overpaid)
}

// loadApplication fetches an application together with the account that owns
// it, since nothing may be decided about one without the other.
func (s *DiscountService) loadApplication(ctx context.Context, applicationID shared.ID) (*discount.Application, *billing.Account, error) {
	application, err := s.deps.Discounts.GetApplication(ctx, applicationID)
	if err != nil {
		return nil, nil, err
	}
	account, err := s.deps.Accounts.GetByID(ctx, application.AccountID)
	if err != nil {
		return nil, nil, err
	}
	return application, account, nil
}
