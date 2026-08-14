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

// EnrollmentService handles the academic lifecycle commands.
type EnrollmentService struct {
	deps Deps
	auditor
}

// NewEnrollmentService wires the enrollment commands.
func NewEnrollmentService(d Deps) *EnrollmentService {
	return &EnrollmentService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// EnrollStudentInput registers a student for a year.
type EnrollStudentInput struct {
	StudentID      shared.ID
	AcademicYearID shared.ID
	DepartmentID   shared.ID
	StudyTypeID    shared.ID
	Stage          int16
	// CategoryCode selects the fee category. Left empty, it is derived: a
	// student on a second or later attempt at a stage is a repeat student, and
	// repeat tuition is usually different.
	CategoryCode string
	Kind         academic.Kind
	// PreviousEnrollmentID links across years, which is what makes lineage,
	// attempt validation and return-after-dropout answerable.
	PreviousEnrollmentID *shared.ID
	// OverrideDebtBlock proceeds despite unsettled prior-year debt. Requires
	// finance authority and is recorded.
	OverrideDebtBlock bool
	OverrideReason    *string
}

// EnrollStudentResult carries the new registration and any debt warning.
type EnrollStudentResult struct {
	Enrollment *academic.Enrollment
	// PriorDebt is what the student still owes from earlier years. Surfaced
	// even when policy allows the registration, because a registrar who cannot
	// see it cannot mention it.
	PriorDebt  money.Amount
	DebtWarned bool
}

// EnrollStudent registers a student for an academic year.
func (s *EnrollmentService) EnrollStudent(ctx context.Context, actor shared.Actor, in EnrollStudentInput) (*EnrollStudentResult, error) {
	if err := actor.RequireAnyRole("EnrollStudent",
		shared.RoleRegistrar, shared.RoleAcademicOfficer, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var result *EnrollStudentResult
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		person, err := s.deps.Students.GetByID(ctx, in.StudentID)
		if err != nil {
			return err
		}
		if err := person.RequireEnrollable(); err != nil {
			return err
		}

		year, err := s.deps.Years.GetByID(ctx, in.AcademicYearID)
		if err != nil {
			return err
		}
		if !year.AcceptsEnrollment() {
			return shared.PreconditionFailed("enrollment.year_not_open",
				"academic year %s is %s and is not accepting registrations", year.Code, year.Status).
				WithDetail("academic_year", year.Code).
				WithDetail("status", string(year.Status))
		}

		department, err := s.deps.Reference.GetDepartment(ctx, in.DepartmentID)
		if err != nil {
			return err
		}

		priorDebt, debtWarned, err := s.checkPriorDebt(ctx, actor, person.ID, year, in)
		if err != nil {
			return err
		}

		// The attempt number is derived from history and validated, never
		// taken from the caller. It drives the repeat-fee policy row, so a
		// wrong value silently charges the wrong tuition.
		priorAttempts, err := s.deps.Enrollments.CountAttempts(ctx, person.ID, department.ID, in.Stage)
		if err != nil {
			return err
		}
		attemptNumber := int16(priorAttempts + 1)

		categoryCode := in.CategoryCode
		if categoryCode == "" {
			categoryCode = academic.CategoryRegular
			if attemptNumber > 1 {
				categoryCode = academic.CategoryRepeat
			}
		}
		category, err := s.deps.Reference.GetStudentCategoryByCode(ctx, categoryCode)
		if err != nil {
			return err
		}

		sequenceNo, err := s.deps.Enrollments.NextSequenceNo(ctx, person.ID, year.ID)
		if err != nil {
			return err
		}

		enrollment, err := academic.NewEnrollment(academic.NewEnrollmentParams{
			StudentID:            person.ID,
			AcademicYearID:       year.ID,
			CollegeID:            department.CollegeID,
			DepartmentID:         department.ID,
			StudyTypeID:          in.StudyTypeID,
			StudentCategoryID:    category.ID,
			Stage:                in.Stage,
			AttemptNumber:        attemptNumber,
			Kind:                 in.Kind,
			SequenceNo:           sequenceNo,
			PreviousEnrollmentID: in.PreviousEnrollmentID,
			RegisteredBy:         &actor.UserID,
			DepartmentStageCount: department.StageCount,
		})
		if err != nil {
			return err
		}
		if err := enrollment.Activate(); err != nil {
			return err
		}

		if err := s.deps.Enrollments.Create(ctx, enrollment); err != nil {
			return err
		}

		// A student returning after dropping out comes back onto the register.
		if person.Status == "separated" {
			if err := person.MarkReturned(); err != nil {
				return err
			}
			if err := s.deps.Students.Update(ctx, person); err != nil {
				return err
			}
		}

		if err := s.record(ctx, port.AuditEntry{
			EntityType:     "enrollment",
			EntityID:       &enrollment.ID,
			Action:         "enrollment.created",
			Actor:          actor,
			After:          snapshotOf(enrollment),
			AcademicYearID: &year.ID,
			StudentID:      &person.ID,
			Metadata: map[string]any{
				"stage":               in.Stage,
				"attempt_number":      attemptNumber,
				"category":            categoryCode,
				"prior_debt":          priorDebt.Int64(),
				"debt_block_override": in.OverrideDebtBlock,
			},
		}); err != nil {
			return err
		}

		result = &EnrollStudentResult{Enrollment: enrollment, PriorDebt: priorDebt, DebtWarned: debtWarned}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// checkPriorDebt applies the year's debt policy to a new registration.
//
// Debt stays on the account of the year that incurred it — it is never rolled
// forward into a running balance, because that loses which year owes what.
// What carries forward is the consequence, and the university decides by
// circular whether that consequence is a block, a warning, or nothing. So it
// is a per-year setting rather than a constant.
func (s *EnrollmentService) checkPriorDebt(
	ctx context.Context,
	actor shared.Actor,
	studentID shared.ID,
	year *academic.Year,
	in EnrollStudentInput,
) (money.Amount, bool, error) {
	debt, err := s.deps.Accounts.OutstandingForStudent(ctx, studentID, &year.ID)
	if err != nil {
		return 0, false, err
	}
	if !debt.IsPositive() {
		return 0, false, nil
	}

	switch year.DebtBlockPolicy {
	case academic.DebtIgnore:
		return debt, false, nil
	case academic.DebtWarn:
		return debt, true, nil
	case academic.DebtBlock:
		if !in.OverrideDebtBlock {
			return debt, true, shared.PreconditionFailed("enrollment.prior_debt_blocks",
				"this student owes %s from earlier years and %s blocks registration until it is settled",
				debt.Format(), year.Code).
				WithDetail("prior_debt", debt.Int64()).
				WithDetail("policy", string(year.DebtBlockPolicy)).
				WithDetail("remedy", "settle the debt, or re-submit with an override if you hold finance authority")
		}
		// Overriding a block is a financial decision, not a clerical one.
		if err := actor.RequireAnyRole("OverrideDebtBlock", shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
			return debt, true, err
		}
		if in.OverrideReason == nil || *in.OverrideReason == "" {
			return debt, true, shared.Validation("enrollment.override_reason_required",
				"overriding the debt block requires a written reason")
		}
		return debt, true, nil
	default:
		return debt, false, nil
	}
}

// SupersedeInput changes an enrollment's context mid-year.
type SupersedeInput struct {
	EnrollmentID shared.ID
	// Any of these may be set; unset fields carry over unchanged.
	NewDepartmentID *shared.ID
	NewStudyTypeID  *shared.ID
	NewStage        *int16
	NewCategoryCode *string
	Reason          string
	EffectiveDate   shared.Date
}

// SupersedeResult carries both halves of the change.
type SupersedeResult struct {
	Superseded  *academic.Enrollment
	Replacement *academic.Enrollment
	// TransferredCredit is what moved from the old account to the new one.
	TransferredCredit money.Amount
}

// SupersedeEnrollment replaces an enrollment with a corrected one, preserving
// the original.
//
// Two things about this command are worth stating plainly.
//
// The write order is forced. The old row must be marked superseded before the
// replacement can be inserted, because a partial unique index permits only one
// live enrollment per student per year. A deferred constraint then verifies at
// commit that the replacement really was created — so an enrollment cannot be
// retired with nothing put in its place, which would silently erase a
// registration somebody may already have paid into.
//
// The money does not follow the student directly. The old account is cancelled
// with its payments untouched, and the balance moves as a visible pair of
// transfer adjustments. Re-pointing the payments would falsify receipts that
// are already printed and in students' hands, and would make the old account
// stop reconciling against the drawer it was collected into.
func (s *EnrollmentService) SupersedeEnrollment(ctx context.Context, actor shared.Actor, in SupersedeInput) (*SupersedeResult, error) {
	if err := actor.RequireAnyRole("SupersedeEnrollment", shared.RoleRegistrar, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if in.Reason == "" {
		return nil, shared.Validation("enrollment.supersede_reason_required",
			"changing an enrollment mid-year requires a reason")
	}

	var result *SupersedeResult
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		original, err := s.deps.Enrollments.GetByID(ctx, in.EnrollmentID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetForUpdate(ctx, original.AcademicYearID)
		if err != nil {
			return err
		}
		if !year.AcceptsEnrollment() {
			return shared.PreconditionFailed("enrollment.year_not_open",
				"academic year %s is %s; an enrollment can only be superseded while the year is open",
				year.Code, year.Status)
		}

		departmentID := original.DepartmentID
		if in.NewDepartmentID != nil {
			departmentID = *in.NewDepartmentID
		}
		department, err := s.deps.Reference.GetDepartment(ctx, departmentID)
		if err != nil {
			return err
		}

		studyTypeID := original.StudyTypeID
		if in.NewStudyTypeID != nil {
			studyTypeID = *in.NewStudyTypeID
		}
		stage := original.Stage
		if in.NewStage != nil {
			stage = *in.NewStage
		}
		categoryID := original.StudentCategoryID
		if in.NewCategoryCode != nil {
			category, err := s.deps.Reference.GetStudentCategoryByCode(ctx, *in.NewCategoryCode)
			if err != nil {
				return err
			}
			categoryID = category.ID
		}

		if departmentID == original.DepartmentID &&
			studyTypeID == original.StudyTypeID &&
			stage == original.Stage &&
			categoryID == original.StudentCategoryID {
			return shared.Validation("enrollment.supersede_no_change",
				"superseding must change something; every field matches the current enrollment")
		}

		sequenceNo, err := s.deps.Enrollments.NextSequenceNo(ctx, original.StudentID, year.ID)
		if err != nil {
			return err
		}

		// Step one: free the year's slot.
		if err := original.Supersede(in.Reason, in.EffectiveDate); err != nil {
			return err
		}
		if err := s.deps.Enrollments.Update(ctx, original); err != nil {
			return err
		}

		// Step two: the replacement, pointing back at what it replaces.
		replacement, err := academic.NewEnrollment(academic.NewEnrollmentParams{
			StudentID:            original.StudentID,
			AcademicYearID:       year.ID,
			CollegeID:            department.CollegeID,
			DepartmentID:         department.ID,
			StudyTypeID:          studyTypeID,
			StudentCategoryID:    categoryID,
			Stage:                stage,
			AttemptNumber:        original.AttemptNumber,
			Kind:                 original.Kind,
			SequenceNo:           sequenceNo,
			PreviousEnrollmentID: original.PreviousEnrollmentID,
			SupersedesID:         &original.ID,
			RegisteredBy:         &actor.UserID,
			DepartmentStageCount: department.StageCount,
		})
		if err != nil {
			return err
		}
		if err := replacement.Activate(); err != nil {
			return err
		}
		if err := s.deps.Enrollments.Create(ctx, replacement); err != nil {
			return err
		}

		transferred, err := s.transferAccount(ctx, actor, original, replacement, in.Reason, now)
		if err != nil {
			return err
		}

		if err := s.record(ctx, port.AuditEntry{
			EntityType:     "enrollment",
			EntityID:       &original.ID,
			Action:         "enrollment.superseded",
			Actor:          actor,
			Before:         snapshotOf(original),
			After:          snapshotOf(replacement),
			AcademicYearID: &year.ID,
			StudentID:      &original.StudentID,
			Reason:         &in.Reason,
			Metadata: map[string]any{
				"replacement_enrollment_id": replacement.ID.String(),
				"transferred_credit":        transferred.Int64(),
				"effective_date":            in.EffectiveDate.String(),
			},
		}); err != nil {
			return err
		}

		result = &SupersedeResult{
			Superseded:        original,
			Replacement:       replacement,
			TransferredCredit: transferred,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// transferAccount retires the superseded enrollment's account and turns
// everything the student paid into credit for the replacement.
//
// The old obligation is written off to zero rather than down to what was
// collected. The student never occupied that seat for the year — the
// replacement enrollment carries the real obligation, priced from its own
// context — so leaving any charge behind would bill them twice for one year.
//
// The money itself does not move. Payments stay on the account they were
// collected into, which is what keeps their printed receipts truthful and
// keeps that account reconciling against the drawer that took the cash. What
// moves is the entitlement: a credit the student can spend on the replacement,
// matched by a signed adjustment so the write-off is visible rather than
// inferred from a total that quietly changed.
func (s *EnrollmentService) transferAccount(
	ctx context.Context,
	actor shared.Actor,
	original, replacement *academic.Enrollment,
	reason string,
	timestamp time.Time,
) (money.Amount, error) {
	oldAccount, err := s.deps.Accounts.GetByEnrollment(ctx, original.ID)
	if err != nil || oldAccount == nil {
		// No account was generated yet, the common case for a change made
		// during registration week. Nothing to move.
		return 0, nil
	}

	locked, err := s.deps.Accounts.GetForUpdate(ctx, oldAccount.ID)
	if err != nil {
		return 0, err
	}

	netPaid := locked.NetPaid()
	obligation := locked.EffectiveNet()

	if err := locked.Cancel("enrollment superseded: "+reason, timestamp, nil); err != nil {
		return 0, err
	}

	if obligation.IsPositive() {
		writeOff := obligation.Neg()
		out, err := billing.NewAdjustment(locked.ID, billing.AdjustmentTransferCreditOut, writeOff, reason)
		if err != nil {
			return 0, err
		}
		out.PostedBy = &actor.UserID
		out.ApprovedBy = &actor.UserID
		out.ApprovedAt = &timestamp
		out.ReferenceID = &replacement.ID
		out.ReferenceType = ptr("enrollment")
		if err := locked.ApplyAdjustment(writeOff, timestamp); err != nil {
			return 0, err
		}
		if err := s.deps.Accounts.CreateAdjustment(ctx, out); err != nil {
			return 0, err
		}
	}

	// With the obligation gone, the part of the payments that had been consumed
	// by installments is unspent too, and joins whatever was already sitting as
	// credit. Only the newly freed part becomes a new credit row — the existing
	// entries are still there and still count.
	freed, err := netPaid.Sub(locked.CreditBalance)
	if err != nil {
		return 0, shared.Internal("supersede.arithmetic", err, "computing the credit to carry forward")
	}
	if freed.IsPositive() {
		credit, err := billing.NewCreditEntry(locked.ID, locked.StudentID, freed, billing.CreditFromTransfer)
		if err != nil {
			return 0, err
		}
		credit.CreatedBy = &actor.UserID
		credit.CreatedAt = timestamp
		credit.Reason = ptr("carried from superseded enrollment " + original.ID.String())
		credit.SourceReference = &replacement.ID
		if err := s.deps.Accounts.CreateCredit(ctx, credit); err != nil {
			return 0, err
		}
		if err := locked.AddCredit(freed); err != nil {
			return 0, err
		}
	}

	// Written after the adjustment and the credit, so the cached totals the row
	// carries match what was just recorded against it.
	if err := s.deps.Accounts.Update(ctx, locked); err != nil {
		return 0, err
	}

	return netPaid, nil
}

// RecordResultInput records an examination outcome.
type RecordResultInput struct {
	EnrollmentID shared.ID
	Result       academic.AcademicResult
	ByDecision   bool
}

// RecordAcademicResult sets a student's outcome for the year.
//
// Permitted right through the financial close. Second-round results arrive in
// September and October, weeks after the treasury shuts its books, and a
// system that froze both at once would force the registrar to either falsify a
// date or leave the result unrecorded.
func (s *EnrollmentService) RecordAcademicResult(ctx context.Context, actor shared.Actor, in RecordResultInput) (*academic.Enrollment, error) {
	if err := actor.RequireAnyRole("RecordAcademicResult",
		shared.RoleAcademicOfficer, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var enrollment *academic.Enrollment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		enrollment, err = s.deps.Enrollments.GetByID(ctx, in.EnrollmentID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetByID(ctx, enrollment.AcademicYearID)
		if err != nil {
			return err
		}
		if err := year.RequireAcademicRecording("recording an examination result"); err != nil {
			return err
		}

		before := snapshotOf(enrollment)
		if err := enrollment.RecordResult(in.Result, in.ByDecision, actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Enrollments.Update(ctx, enrollment); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "enrollment",
			EntityID:       &enrollment.ID,
			Action:         "enrollment.result_recorded",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(enrollment),
			AcademicYearID: &year.ID,
			StudentID:      &enrollment.StudentID,
			Metadata: map[string]any{
				"result":      string(in.Result),
				"by_decision": in.ByDecision,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return enrollment, nil
}

// ChangeStatusInput applies a lifecycle transition.
type ChangeStatusInput struct {
	EnrollmentID shared.ID
	Target       academic.EnrollmentStatus
	OrderRef     *string
	Reason       *string
	Result       academic.AcademicResult
}

// ChangeEnrollmentStatus defers, withdraws, transfers out, marks a dropout, or
// completes an enrollment.
//
// Note what none of these do: they never clear a debt. A student who stops
// attending still owes what they owed, and the account stays exactly where it
// is so the debt report can still find it.
func (s *EnrollmentService) ChangeEnrollmentStatus(ctx context.Context, actor shared.Actor, in ChangeStatusInput) (*academic.Enrollment, error) {
	if err := actor.RequireAnyRole("ChangeEnrollmentStatus",
		shared.RoleRegistrar, shared.RoleAcademicOfficer, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var enrollment *academic.Enrollment
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		enrollment, err = s.deps.Enrollments.GetByID(ctx, in.EnrollmentID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetByID(ctx, enrollment.AcademicYearID)
		if err != nil {
			return err
		}
		if err := year.RequireAcademicRecording("changing an enrollment's status"); err != nil {
			return err
		}

		before := snapshotOf(enrollment)

		switch in.Target {
		case academic.StatusDeferred:
			if in.OrderRef == nil {
				return shared.Validation("enrollment.deferral_order_required",
					"deferral requires the reference of the official order")
			}
			err = enrollment.Defer(*in.OrderRef)
		case academic.StatusActive:
			err = enrollment.ResumeFromDeferral()
		case academic.StatusWithdrawn:
			err = enrollment.Withdraw()
		case academic.StatusDroppedOut:
			err = enrollment.MarkDroppedOut(in.Result)
		case academic.StatusTransferredOut:
			if in.OrderRef == nil {
				return shared.Validation("enrollment.transfer_order_required",
					"transferring out requires the reference of the official order")
			}
			err = enrollment.TransferOut(*in.OrderRef)
		case academic.StatusCompleted:
			err = enrollment.Complete()
		default:
			return shared.Validation("enrollment.unsupported_status_change",
				"%q is not a status this command can apply; superseding has its own command", in.Target)
		}
		if err != nil {
			return err
		}

		if err := s.deps.Enrollments.Update(ctx, enrollment); err != nil {
			return err
		}

		if err := s.syncStudentStanding(ctx, enrollment); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "enrollment",
			EntityID:       &enrollment.ID,
			Action:         "enrollment.status_changed",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(enrollment),
			AcademicYearID: &year.ID,
			StudentID:      &enrollment.StudentID,
			Reason:         in.Reason,
			Metadata:       map[string]any{"new_status": string(in.Target)},
		})
	})
	if err != nil {
		return nil, err
	}
	return enrollment, nil
}

// syncStudentStanding derives the person's standing from what just happened to
// their enrollment. Graduation is never a flag somebody sets by hand.
func (s *EnrollmentService) syncStudentStanding(ctx context.Context, e *academic.Enrollment) error {
	person, err := s.deps.Students.GetByID(ctx, e.StudentID)
	if err != nil {
		return err
	}

	switch e.Status {
	case academic.StatusWithdrawn, academic.StatusDroppedOut:
		person.MarkSeparated()
	case academic.StatusTransferredOut:
		person.MarkTransferredOut()
	case academic.StatusCompleted:
		department, err := s.deps.Reference.GetDepartment(ctx, e.DepartmentID)
		if err != nil {
			return err
		}
		if e.Stage < department.StageCount {
			// Completing a non-final stage means promotion, not graduation.
			return nil
		}
		person.MarkGraduated()
	default:
		return nil
	}

	return s.deps.Students.Update(ctx, person)
}
