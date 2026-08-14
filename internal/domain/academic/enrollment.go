package academic

import (
	"time"

	"github.com/swibit/flowed/internal/domain/shared"
)

// EnrollmentStatus answers "is this student still with us in this year".
type EnrollmentStatus string

const (
	StatusDraft          EnrollmentStatus = "draft"
	StatusActive         EnrollmentStatus = "active"
	StatusDeferred       EnrollmentStatus = "deferred"
	StatusSuperseded     EnrollmentStatus = "superseded"
	StatusTransferredOut EnrollmentStatus = "transferred_out"
	StatusWithdrawn      EnrollmentStatus = "withdrawn"
	StatusDroppedOut     EnrollmentStatus = "dropped_out"
	StatusCompleted      EnrollmentStatus = "completed"
)

// AcademicResult answers "did they pass". Kept strictly separate from status:
// a student can be dropped_out and failed, or active and passed, and a single
// merged field cannot say either.
type AcademicResult string

const (
	ResultPending       AcademicResult = "pending"
	ResultPassedR1      AcademicResult = "passed_r1"
	ResultPassedR2      AcademicResult = "passed_r2"
	ResultFailed        AcademicResult = "failed"
	ResultNoResult      AcademicResult = "no_result"
	ResultNotApplicable AcademicResult = "not_applicable"
)

// Kind distinguishes an ordinary registration from one created by hosting or
// mid-year transfer.
type Kind string

const (
	KindRegular    Kind = "regular"
	KindHostedIn   Kind = "hosted_in"
	KindTransferIn Kind = "transfer_in"
)

// resultsByStatus is the legality matrix. It mirrors the CHECK constraint in
// the schema exactly; the database is the enforcement and this is the fast,
// message-bearing check that runs first.
var resultsByStatus = map[EnrollmentStatus][]AcademicResult{
	StatusDraft:          {ResultPending},
	StatusActive:         {ResultPending, ResultPassedR1, ResultPassedR2, ResultFailed},
	StatusDeferred:       {ResultNotApplicable},
	StatusSuperseded:     {ResultNotApplicable},
	StatusTransferredOut: {ResultNoResult, ResultNotApplicable},
	StatusWithdrawn:      {ResultNoResult},
	StatusDroppedOut:     {ResultNoResult, ResultFailed},
	StatusCompleted:      {ResultPassedR1, ResultPassedR2},
}

// allowedTransitions is the enrollment state machine.
var allowedTransitions = map[EnrollmentStatus][]EnrollmentStatus{
	StatusDraft: {StatusActive},
	StatusActive: {
		StatusSuperseded, StatusDeferred, StatusTransferredOut,
		StatusWithdrawn, StatusDroppedOut, StatusCompleted,
	},
	// A deferral can be revoked while the year is still open, and a student
	// who never returns from one ends the year deferred.
	StatusDeferred: {StatusActive, StatusWithdrawn, StatusDroppedOut},
	// The rest are terminal.
	StatusSuperseded:     {},
	StatusTransferredOut: {},
	StatusWithdrawn:      {},
	StatusDroppedOut:     {},
	StatusCompleted:      {},
}

// Enrollment is a student's registration for one academic year: the unit that
// carries academic context and, through its financial account, money.
type Enrollment struct {
	ID             shared.ID
	StudentID      shared.ID
	AcademicYearID shared.ID
	SequenceNo     int16

	CollegeID         shared.ID
	DepartmentID      shared.ID
	StudyTypeID       shared.ID
	StudentCategoryID shared.ID
	Stage             int16
	AttemptNumber     int16
	Kind              Kind

	Status           EnrollmentStatus
	Result           AcademicResult
	ResultByDecision bool
	ResultRecordedAt *time.Time
	ResultRecordedBy *shared.ID

	PreviousEnrollmentID *shared.ID
	SupersedesID         *shared.ID
	SupersedeReason      *string
	SupersedeDate        *shared.Date

	DeferralOrderRef *string
	TransferOrderRef *string
	ReturnOrderRef   *string
	Notes            *string

	// FinancialTreatment records what was decided about the money when this
	// enrollment stopped being active. Nil while it is still active; never
	// inferred from the status, because "keep charging" and "nobody thought
	// about it" are different answers and only one is defensible.
	FinancialTreatment   *FinancialTreatment
	FinancialTreatmentAt *time.Time
	FinancialTreatmentBy *shared.ID

	RegisteredAt time.Time
	RegisteredBy *shared.ID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewEnrollmentParams carries the context a registration needs.
type NewEnrollmentParams struct {
	StudentID            shared.ID
	AcademicYearID       shared.ID
	CollegeID            shared.ID
	DepartmentID         shared.ID
	StudyTypeID          shared.ID
	StudentCategoryID    shared.ID
	Stage                int16
	AttemptNumber        int16
	Kind                 Kind
	SequenceNo           int16
	PreviousEnrollmentID *shared.ID
	SupersedesID         *shared.ID
	RegisteredBy         *shared.ID
	DepartmentStageCount int16
}

// NewEnrollment builds a draft enrollment after validating its context.
func NewEnrollment(p NewEnrollmentParams) (*Enrollment, error) {
	if p.Stage < 1 {
		return nil, shared.Validation("enrollment.invalid_stage", "stage must be at least 1, got %d", p.Stage)
	}
	if p.DepartmentStageCount > 0 && p.Stage > p.DepartmentStageCount {
		return nil, shared.Validation("enrollment.stage_out_of_range",
			"stage %d does not exist in this department, which runs %d stages",
			p.Stage, p.DepartmentStageCount).
			WithDetail("stage", p.Stage).
			WithDetail("department_stage_count", p.DepartmentStageCount)
	}
	if p.AttemptNumber < 1 {
		return nil, shared.Validation("enrollment.invalid_attempt",
			"attempt number must be at least 1, got %d", p.AttemptNumber)
	}
	if p.SequenceNo < 1 {
		p.SequenceNo = 1
	}
	if p.Kind == "" {
		p.Kind = KindRegular
	}

	return &Enrollment{
		ID:                   shared.NewID(),
		StudentID:            p.StudentID,
		AcademicYearID:       p.AcademicYearID,
		SequenceNo:           p.SequenceNo,
		CollegeID:            p.CollegeID,
		DepartmentID:         p.DepartmentID,
		StudyTypeID:          p.StudyTypeID,
		StudentCategoryID:    p.StudentCategoryID,
		Stage:                p.Stage,
		AttemptNumber:        p.AttemptNumber,
		Kind:                 p.Kind,
		Status:               StatusDraft,
		Result:               ResultPending,
		PreviousEnrollmentID: p.PreviousEnrollmentID,
		SupersedesID:         p.SupersedesID,
		RegisteredBy:         p.RegisteredBy,
		RegisteredAt:         time.Now().UTC(),
	}, nil
}

// IsLive reports whether the enrollment occupies the student's slot for the
// year. Exactly one live enrollment per (student, year) is permitted, and the
// database enforces it with a partial unique index.
func (e *Enrollment) IsLive() bool {
	switch e.Status {
	case StatusDraft, StatusActive, StatusDeferred:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether the enrollment can no longer change state.
func (e *Enrollment) IsTerminal() bool { return len(allowedTransitions[e.Status]) == 0 }

// CountsAsAttempt reports whether this registration consumes one of the
// student's attempts at the stage.
//
// The rule is derived from the result alone. An earlier draft of this design
// tried to exclude students who dropped out "before exams", which nothing in
// the data records and no two clerks would judge alike. A failed result counts
// against the student; anything that produced no result does not.
func (e *Enrollment) CountsAsAttempt() bool {
	switch e.Result {
	case ResultFailed, ResultPassedR1, ResultPassedR2:
		return true
	default:
		return false
	}
}

// Passed reports whether the student cleared the year in either round.
func (e *Enrollment) Passed() bool {
	return e.Result == ResultPassedR1 || e.Result == ResultPassedR2
}

// CanTransitionTo reports whether a status change is legal.
func (e *Enrollment) CanTransitionTo(target EnrollmentStatus) bool {
	for _, allowed := range allowedTransitions[e.Status] {
		if allowed == target {
			return true
		}
	}
	return false
}

// transitionTo applies a status change together with the result it implies,
// rejecting both illegal transitions and illegal status/result pairs.
func (e *Enrollment) transitionTo(target EnrollmentStatus, result AcademicResult) error {
	if !e.CanTransitionTo(target) {
		return shared.PreconditionFailed("enrollment.illegal_transition",
			"an enrollment cannot move from %s to %s", e.Status, target).
			WithDetail("from", string(e.Status)).
			WithDetail("to", string(target)).
			WithDetail("allowed", allowedTransitions[e.Status])
	}
	if !resultAllowed(target, result) {
		return shared.InvariantViolation("enrollment.illegal_status_result",
			"status %s cannot carry result %s", target, result).
			WithDetail("status", string(target)).
			WithDetail("result", string(result)).
			WithDetail("allowed_results", resultsByStatus[target])
	}
	e.Status = target
	e.Result = result
	return nil
}

func resultAllowed(status EnrollmentStatus, result AcademicResult) bool {
	for _, allowed := range resultsByStatus[status] {
		if allowed == result {
			return true
		}
	}
	return false
}

// Activate moves a draft registration into effect.
func (e *Enrollment) Activate() error {
	return e.transitionTo(StatusActive, ResultPending)
}

// RecordResult sets the academic outcome on an active enrollment.
//
// Recording a result does not end the enrollment. A student who failed is
// still enrolled until the year closes, still owes what they owe, and may
// still have a second-round result recorded over the first.
func (e *Enrollment) RecordResult(result AcademicResult, byDecision bool, actor shared.ID, at time.Time) error {
	if e.Status != StatusActive {
		return shared.PreconditionFailed("enrollment.not_active",
			"results can only be recorded on an active enrollment; this one is %s", e.Status)
	}
	switch result {
	case ResultPassedR1, ResultPassedR2, ResultFailed:
	default:
		return shared.Validation("enrollment.invalid_result",
			"%q is not a recordable examination result", result)
	}
	// A second-round result may replace a first-round failure — that is what
	// the second round is for — but a recorded pass is not quietly overwritten.
	if e.Passed() && result == ResultFailed {
		return shared.PreconditionFailed("enrollment.result_already_passed",
			"this enrollment already carries a pass; reversing it needs an explicit correction")
	}
	e.Result = result
	e.ResultByDecision = byDecision
	e.ResultRecordedAt = &at
	e.ResultRecordedBy = &actor
	return nil
}

// Supersede marks this enrollment as replaced. The caller creates the
// replacement in the same transaction; a deferred constraint in the database
// refuses the commit if it does not.
func (e *Enrollment) Supersede(reason string, on shared.Date) error {
	if reason == "" {
		return shared.Validation("enrollment.supersede_reason_required",
			"superseding an enrollment requires a reason")
	}
	if err := e.transitionTo(StatusSuperseded, ResultNotApplicable); err != nil {
		return err
	}
	e.SupersedeReason = &reason
	e.SupersedeDate = &on
	return nil
}

// Defer suspends the year without consuming an attempt.
func (e *Enrollment) Defer(orderRef string) error {
	if orderRef == "" {
		return shared.Validation("enrollment.deferral_order_required",
			"deferral requires the reference of the official order")
	}
	if err := e.transitionTo(StatusDeferred, ResultNotApplicable); err != nil {
		return err
	}
	e.DeferralOrderRef = &orderRef
	return nil
}

// ResumeFromDeferral returns a deferred student to active study in the same
// year.
func (e *Enrollment) ResumeFromDeferral() error {
	return e.transitionTo(StatusActive, ResultPending)
}

// Withdraw ends the registration at the student's request.
func (e *Enrollment) Withdraw() error {
	return e.transitionTo(StatusWithdrawn, ResultNoResult)
}

// MarkDroppedOut ends the registration through absence. Any debt survives on
// the account; a student who stops attending does not stop owing.
func (e *Enrollment) MarkDroppedOut(result AcademicResult) error {
	if result == "" {
		result = ResultNoResult
	}
	return e.transitionTo(StatusDroppedOut, result)
}

// TransferOut ends the registration because the student moved institution.
func (e *Enrollment) TransferOut(orderRef string) error {
	if orderRef == "" {
		return shared.Validation("enrollment.transfer_order_required",
			"transfer out requires the reference of the official order")
	}
	if err := e.transitionTo(StatusTransferredOut, ResultNoResult); err != nil {
		return err
	}
	e.TransferOrderRef = &orderRef
	return nil
}

// Complete closes a passed enrollment at year end.
func (e *Enrollment) Complete() error {
	if !e.Passed() {
		return shared.PreconditionFailed("enrollment.not_passed",
			"an enrollment can only be completed with a passing result; this one is %s", e.Result)
	}
	return e.transitionTo(StatusCompleted, e.Result)
}

// NextAttemptNumber returns the attempt number for the student's next
// registration at the same stage, given this one's outcome. A counted attempt
// increments it; a deferral or withdrawal leaves it alone.
func (e *Enrollment) NextAttemptNumber() int16 {
	if e.CountsAsAttempt() {
		return e.AttemptNumber + 1
	}
	return e.AttemptNumber
}
