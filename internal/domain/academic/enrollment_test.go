package academic_test

import (
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/shared"
)

func newActiveEnrollment(t *testing.T, stage, attempt int16) *academic.Enrollment {
	t.Helper()
	e, err := academic.NewEnrollment(academic.NewEnrollmentParams{
		StudentID:            shared.NewID(),
		AcademicYearID:       shared.NewID(),
		CollegeID:            shared.NewID(),
		DepartmentID:         shared.NewID(),
		StudyTypeID:          shared.NewID(),
		StudentCategoryID:    shared.NewID(),
		Stage:                stage,
		AttemptNumber:        attempt,
		DepartmentStageCount: 4,
	})
	if err != nil {
		t.Fatalf("building enrollment: %v", err)
	}
	if err := e.Activate(); err != nil {
		t.Fatalf("activating enrollment: %v", err)
	}
	return e
}

// The three status dimensions must stay separate. Every combination outside
// the matrix is rejected, so no code path can leave an enrollment in a state
// the domain has no meaning for.
func TestStatusAndResultMatrixIsEnforced(t *testing.T) {
	actor := shared.NewID()
	now := time.Now().UTC()

	// Superseded carries no result: the real outcome belongs to the successor.
	e := newActiveEnrollment(t, 1, 1)
	if err := e.RecordResult(academic.ResultPassedR1, false, actor, now); err != nil {
		t.Fatal(err)
	}
	if err := e.Supersede("changed department", shared.NewDate(2026, 1, 15)); err != nil {
		t.Fatal(err)
	}
	if e.Result != academic.ResultNotApplicable {
		t.Errorf("a superseded enrollment carries result %s, want not_applicable", e.Result)
	}

	// Completing without a pass is refused.
	failed := newActiveEnrollment(t, 4, 1)
	if err := failed.RecordResult(academic.ResultFailed, false, actor, now); err != nil {
		t.Fatal(err)
	}
	if err := failed.Complete(); err == nil {
		t.Error("completing a failed enrollment must be refused")
	}
}

func TestIllegalTransitionsAreRefused(t *testing.T) {
	withdrawn := newActiveEnrollment(t, 1, 1)
	if err := withdrawn.Withdraw(); err != nil {
		t.Fatal(err)
	}
	if !withdrawn.IsTerminal() {
		t.Error("a withdrawn enrollment should be terminal")
	}
	if err := withdrawn.Defer("ORDER-1"); err == nil {
		t.Error("deferring a withdrawn enrollment must be refused")
	}

	deferred := newActiveEnrollment(t, 1, 1)
	if err := deferred.Defer("ORDER-2"); err != nil {
		t.Fatal(err)
	}
	if err := deferred.Complete(); err == nil {
		t.Error("completing a deferred enrollment must be refused")
	}
	// A deferral revoked while the year is still open returns the student to
	// active study.
	if err := deferred.ResumeFromDeferral(); err != nil {
		t.Errorf("resuming from deferral should be allowed: %v", err)
	}
}

// Whether a year counts against a student's attempts is decided by the result
// alone. An earlier design tried to exclude students who dropped out "before
// exams", which nothing records and no two clerks would judge alike.
func TestAttemptCountingFollowsTheResult(t *testing.T) {
	actor := shared.NewID()
	now := time.Now().UTC()

	cases := []struct {
		name   string
		build  func() *academic.Enrollment
		counts bool
		next   int16
	}{
		{
			name: "failed the year",
			build: func() *academic.Enrollment {
				e := newActiveEnrollment(t, 2, 1)
				_ = e.RecordResult(academic.ResultFailed, false, actor, now)
				return e
			},
			counts: true, next: 2,
		},
		{
			name: "passed in the second round",
			build: func() *academic.Enrollment {
				e := newActiveEnrollment(t, 2, 1)
				_ = e.RecordResult(academic.ResultPassedR2, false, actor, now)
				return e
			},
			counts: true, next: 2,
		},
		{
			name: "deferred the year",
			build: func() *academic.Enrollment {
				e := newActiveEnrollment(t, 2, 1)
				_ = e.Defer("ORDER-3")
				return e
			},
			counts: false, next: 1,
		},
		{
			name: "withdrew",
			build: func() *academic.Enrollment {
				e := newActiveEnrollment(t, 2, 1)
				_ = e.Withdraw()
				return e
			},
			counts: false, next: 1,
		},
		{
			name: "dropped out with no result",
			build: func() *academic.Enrollment {
				e := newActiveEnrollment(t, 2, 1)
				_ = e.MarkDroppedOut(academic.ResultNoResult)
				return e
			},
			counts: false, next: 1,
		},
		{
			name: "dropped out after failing",
			build: func() *academic.Enrollment {
				e := newActiveEnrollment(t, 2, 1)
				_ = e.RecordResult(academic.ResultFailed, false, actor, now)
				_ = e.MarkDroppedOut(academic.ResultFailed)
				return e
			},
			counts: true, next: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.build()
			if got := e.CountsAsAttempt(); got != tc.counts {
				t.Errorf("CountsAsAttempt() = %v, want %v (status %s, result %s)",
					got, tc.counts, e.Status, e.Result)
			}
			if got := e.NextAttemptNumber(); got != tc.next {
				t.Errorf("NextAttemptNumber() = %d, want %d", got, tc.next)
			}
		})
	}
}

// A second-round result may replace a first-round failure — that is what the
// second round is for — but a recorded pass is not quietly overwritten.
func TestSecondRoundReplacesAFailureButNotAPass(t *testing.T) {
	actor := shared.NewID()
	now := time.Now().UTC()

	e := newActiveEnrollment(t, 1, 1)
	if err := e.RecordResult(academic.ResultFailed, false, actor, now); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordResult(academic.ResultPassedR2, false, actor, now); err != nil {
		t.Errorf("a second-round pass must be able to replace a first-round failure: %v", err)
	}
	if err := e.RecordResult(academic.ResultFailed, false, actor, now); err == nil {
		t.Error("overwriting a recorded pass with a failure must require an explicit correction")
	}
}

func TestPassedByCommitteeDecisionIsFlaggedNotHidden(t *testing.T) {
	e := newActiveEnrollment(t, 3, 2)
	if err := e.RecordResult(academic.ResultPassedR1, true, shared.NewID(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if !e.Passed() {
		t.Error("a decision pass is still a pass")
	}
	if !e.ResultByDecision {
		t.Error("passing by committee decision must be recorded as such, not folded into an ordinary pass")
	}
}

func TestStageIsValidatedAgainstTheProgrammeLength(t *testing.T) {
	_, err := academic.NewEnrollment(academic.NewEnrollmentParams{
		StudentID:            shared.NewID(),
		AcademicYearID:       shared.NewID(),
		CollegeID:            shared.NewID(),
		DepartmentID:         shared.NewID(),
		StudyTypeID:          shared.NewID(),
		StudentCategoryID:    shared.NewID(),
		Stage:                5,
		AttemptNumber:        1,
		DepartmentStageCount: 4,
	})
	if err == nil {
		t.Fatal("stage 5 in a four-year programme must be refused")
	}
	if code := shared.CodeOf(err); code != "enrollment.stage_out_of_range" {
		t.Errorf("error code = %q, want enrollment.stage_out_of_range", code)
	}

	// Six-year programmes exist; the limit is the department's, not a constant.
	if _, err := academic.NewEnrollment(academic.NewEnrollmentParams{
		StudentID:            shared.NewID(),
		AcademicYearID:       shared.NewID(),
		CollegeID:            shared.NewID(),
		DepartmentID:         shared.NewID(),
		StudyTypeID:          shared.NewID(),
		StudentCategoryID:    shared.NewID(),
		Stage:                6,
		AttemptNumber:        1,
		DepartmentStageCount: 6,
	}); err != nil {
		t.Errorf("stage 6 in a six-year programme should be accepted: %v", err)
	}
}

func TestSupersedeRequiresAReason(t *testing.T) {
	e := newActiveEnrollment(t, 1, 1)
	if err := e.Supersede("", shared.NewDate(2026, 1, 1)); err == nil {
		t.Error("superseding without a reason must be refused")
	}
	if e.Status != academic.StatusActive {
		t.Errorf("a rejected supersede must leave the status alone, got %s", e.Status)
	}
}

func TestLiveEnrollmentStatesOccupyTheYearSlot(t *testing.T) {
	// These three occupy the student's slot for the year, which is what the
	// database's partial unique index keys on.
	live := []func() *academic.Enrollment{
		func() *academic.Enrollment {
			e, _ := academic.NewEnrollment(academic.NewEnrollmentParams{
				StudentID: shared.NewID(), AcademicYearID: shared.NewID(),
				CollegeID: shared.NewID(), DepartmentID: shared.NewID(),
				StudyTypeID: shared.NewID(), StudentCategoryID: shared.NewID(),
				Stage: 1, AttemptNumber: 1, DepartmentStageCount: 4,
			})
			return e
		},
		func() *academic.Enrollment { return newActiveEnrollment(t, 1, 1) },
		func() *academic.Enrollment {
			e := newActiveEnrollment(t, 1, 1)
			_ = e.Defer("ORDER-4")
			return e
		},
	}
	for _, build := range live {
		if e := build(); !e.IsLive() {
			t.Errorf("status %s should occupy the year's slot", e.Status)
		}
	}

	terminal := []func() *academic.Enrollment{
		func() *academic.Enrollment {
			e := newActiveEnrollment(t, 1, 1)
			_ = e.Supersede("changed study type", shared.NewDate(2026, 1, 1))
			return e
		},
		func() *academic.Enrollment {
			e := newActiveEnrollment(t, 1, 1)
			_ = e.Withdraw()
			return e
		},
		func() *academic.Enrollment {
			e := newActiveEnrollment(t, 1, 1)
			_ = e.TransferOut("ORDER-5")
			return e
		},
	}
	for _, build := range terminal {
		if e := build(); e.IsLive() {
			t.Errorf("status %s should not occupy the year's slot", e.Status)
		}
	}
}
