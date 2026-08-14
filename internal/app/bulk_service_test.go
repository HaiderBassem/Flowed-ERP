package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
	"github.com/swibit/flowed/internal/port"
)

// ---------------------------------------------------------------------------
// Promotion
// ---------------------------------------------------------------------------

// A failed student is re-registered at the same stage, on the next attempt, in
// the repeat category. The category is what the repeat-fee policy resolves
// against, so getting it wrong charges the wrong tuition to everyone who
// failed.
func TestPromoteStudentsBulkRepeatsFailedStudent(t *testing.T) {
	w := newBulkWorld(t)
	person := w.addStudent("S001", "علي حسن كاظم", "زينب")
	source := w.addEnrollment(person, w.sourceYear, 2, academic.ResultFailed, academic.StatusActive)

	plan := w.promote(t, PromoteBulkInput{
		SourceYearID: w.sourceYear.ID,
		TargetYearID: w.targetYear.ID,
		DryRun:       true,
	})

	if len(plan.Rows) != 1 {
		t.Fatalf("expected one planned row, got %d", len(plan.Rows))
	}
	row := plan.Rows[0]
	if row.Action != ActionRepeat {
		t.Fatalf("expected the failed student to repeat, got %q (%s)", row.Action, row.Reason)
	}
	if row.ToStage != 2 {
		t.Errorf("a repeat stays at the same stage: want 2, got %d", row.ToStage)
	}
	if row.AttemptNumber != 2 {
		t.Errorf("a repeat is the next attempt: want 2, got %d", row.AttemptNumber)
	}
	if row.CategoryCode != academic.CategoryRepeat {
		t.Errorf("want category %s, got %q", academic.CategoryRepeat, row.CategoryCode)
	}
	if plan.Counts.Repeated != 1 || plan.Counts.Promoted != 0 {
		t.Errorf("unexpected plan counts: %+v", plan.Counts)
	}

	report := w.promote(t, PromoteBulkInput{
		SourceYearID:    w.sourceYear.ID,
		TargetYearID:    w.targetYear.ID,
		ApprovePlanHash: plan.PlanHash,
	})

	if report.Counts.Repeated != 1 {
		t.Fatalf("expected one repeat applied, got %+v (%s)", report.Counts, report.Rows[0].Reason)
	}
	created := w.store.enrollmentIn(w.targetYear.ID, person.ID)
	if created == nil {
		t.Fatal("no enrollment was created in the target year")
	}
	if created.Stage != 2 {
		t.Errorf("want stage 2, got %d", created.Stage)
	}
	if created.AttemptNumber != 2 {
		t.Errorf("want attempt 2, got %d", created.AttemptNumber)
	}
	if created.StudentCategoryID != w.repeatCategory.ID {
		t.Errorf("want the repeat category, got %s", created.StudentCategoryID)
	}
	if created.PreviousEnrollmentID == nil || *created.PreviousEnrollmentID != source.ID {
		t.Error("the new enrollment does not point back at the one it follows")
	}
}

// A pass at the department's last stage completes the enrollment. Promoting it
// would ask for a stage the programme does not run.
func TestPromoteStudentsBulkCompletesFinalStagePass(t *testing.T) {
	w := newBulkWorld(t)
	person := w.addStudent("S002", "مريم عبد الله", "سعاد")
	source := w.addEnrollment(person, w.sourceYear, w.department.StageCount, academic.ResultPassedR1, academic.StatusActive)

	plan := w.promote(t, PromoteBulkInput{
		SourceYearID: w.sourceYear.ID,
		TargetYearID: w.targetYear.ID,
		DryRun:       true,
	})
	if plan.Rows[0].Action != ActionComplete {
		t.Fatalf("a final-stage pass must complete, got %q", plan.Rows[0].Action)
	}
	if plan.Counts.Completed != 1 || plan.Counts.Promoted != 0 {
		t.Fatalf("unexpected plan counts: %+v", plan.Counts)
	}

	report := w.promote(t, PromoteBulkInput{
		SourceYearID:    w.sourceYear.ID,
		TargetYearID:    w.targetYear.ID,
		ApprovePlanHash: plan.PlanHash,
	})
	if report.Counts.Completed != 1 || report.Counts.Failed != 0 {
		t.Fatalf("unexpected report counts: %+v (%s)", report.Counts, report.Rows[0].Reason)
	}
	if got := w.store.enrollmentIn(w.targetYear.ID, person.ID); got != nil {
		t.Fatalf("a stage %d student was promoted into stage %d of a %d-stage programme",
			source.Stage, got.Stage, w.department.StageCount)
	}
	if source.Status != academic.StatusCompleted {
		t.Errorf("want the source enrollment completed, got %s", source.Status)
	}
	if person.Status != student.StatusGraduated {
		t.Errorf("a completed final stage is a graduation; student is %s", person.Status)
	}
}

// A commit without an approved plan is refused outright: the dry run is not
// advisory.
func TestPromoteStudentsBulkRequiresDryRunFirst(t *testing.T) {
	w := newBulkWorld(t)
	person := w.addStudent("S003", "حسين جبار", "ليلى")
	w.addEnrollment(person, w.sourceYear, 1, academic.ResultPassedR1, academic.StatusActive)

	_, err := w.bulk.PromoteStudentsBulk(context.Background(), w.actor, PromoteBulkInput{
		SourceYearID: w.sourceYear.ID,
		TargetYearID: w.targetYear.ID,
	})
	if err == nil {
		t.Fatal("a promotion committed without a dry run")
	}
	if code := shared.CodeOf(err); code != "promotion.dry_run_required" {
		t.Fatalf("want promotion.dry_run_required, got %s", code)
	}
}

// A cohort that changed after the officer approved it is refused rather than
// applied under numbers nobody read.
func TestPromoteStudentsBulkRefusesChangedPlan(t *testing.T) {
	w := newBulkWorld(t)
	first := w.addStudent("S004", "زيد قاسم", "هدى")
	w.addEnrollment(first, w.sourceYear, 1, academic.ResultPassedR1, academic.StatusActive)

	plan := w.promote(t, PromoteBulkInput{
		SourceYearID: w.sourceYear.ID,
		TargetYearID: w.targetYear.ID,
		DryRun:       true,
	})

	// A second student's result arrives between the preview and the commit.
	late := w.addStudent("S005", "نور صباح", "أمل")
	w.addEnrollment(late, w.sourceYear, 1, academic.ResultFailed, academic.StatusActive)

	_, err := w.bulk.PromoteStudentsBulk(context.Background(), w.actor, PromoteBulkInput{
		SourceYearID:    w.sourceYear.ID,
		TargetYearID:    w.targetYear.ID,
		ApprovePlanHash: plan.PlanHash,
	})
	if err == nil {
		t.Fatal("a promotion applied a plan that no longer describes the cohort")
	}
	if code := shared.CodeOf(err); code != "promotion.plan_changed_since_preview" {
		t.Fatalf("want promotion.plan_changed_since_preview, got %s", code)
	}
	if len(w.store.enrollments) != 2 {
		t.Errorf("nothing should have been created, found %d enrollments", len(w.store.enrollments))
	}
}

// Rows that cannot move are reported with a reason, and the batch says so in
// its outcome rather than calling itself done.
func TestPromoteStudentsBulkReportsSkips(t *testing.T) {
	w := newBulkWorld(t)
	pending := w.addStudent("S006", "سيف علاء", "رجاء")
	w.addEnrollment(pending, w.sourceYear, 1, academic.ResultPending, academic.StatusActive)
	withdrawn := w.addStudent("S007", "رغد ثائر", "بشرى")
	w.addEnrollment(withdrawn, w.sourceYear, 1, academic.ResultNoResult, academic.StatusWithdrawn)

	plan := w.promote(t, PromoteBulkInput{
		SourceYearID: w.sourceYear.ID,
		TargetYearID: w.targetYear.ID,
		DryRun:       true,
	})

	if plan.Counts.Skipped != 2 {
		t.Fatalf("want two skipped rows, got %+v", plan.Counts)
	}
	if plan.Outcome != BulkCompletedWithSkips {
		t.Errorf("a plan that skips rows is %s, not %s", BulkCompletedWithSkips, plan.Outcome)
	}
	if plan.SkipReasons[SkipNoResult] != 1 || plan.SkipReasons[SkipNotActive] != 1 {
		t.Errorf("skip reasons are not stated: %+v", plan.SkipReasons)
	}
	for _, row := range plan.Rows {
		if row.Reason == "" {
			t.Errorf("row %s was skipped without a reason", row.SourceEnrollmentID)
		}
	}
}

// ---------------------------------------------------------------------------
// Bulk account generation
// ---------------------------------------------------------------------------

// The preview hash is the whole guarantee. A fee policy published between the
// preview and the commit must not be charged to a cohort under an approval
// given for different numbers.
func TestGenerateAccountsBulkSkipsRowChangedSincePreview(t *testing.T) {
	w := newBulkWorld(t)
	person := w.addStudent("S010", "أحمد وليد", "سناء")
	enrollment := w.addEnrollment(person, w.targetYear, 1, academic.ResultPending, academic.StatusActive)

	preview := w.priceCohort(t, GenerateAccountsBulkInput{
		AcademicYearID: w.targetYear.ID,
		DryRun:         true,
	})
	if len(preview.Rows) != 1 || preview.Rows[0].Outcome != AccountWillCreate {
		t.Fatalf("expected one priced row, got %+v", preview.Rows)
	}
	approvedHash := preview.Rows[0].PreviewHash
	if approvedHash == "" {
		t.Fatal("the preview produced no hash to approve")
	}
	if preview.Rows[0].Net != money.FromInt64(1_000_000) {
		t.Fatalf("want a net of 1,000,000, got %s", preview.Rows[0].Net)
	}
	if preview.Rows[0].FeePolicyCode != "FP-BASE" {
		t.Errorf("the resolved policy is shown to the reviewer: got %q", preview.Rows[0].FeePolicyCode)
	}

	// The tuition is revised after the manager approved the preview.
	w.store.policy = w.buildPolicy("FP-RAISED", 1_500_000)

	report := w.priceCohort(t, GenerateAccountsBulkInput{
		AcademicYearID: w.targetYear.ID,
		Approved:       []AccountApproval{{EnrollmentID: enrollment.ID, PreviewHash: approvedHash}},
	})

	if len(report.Rows) != 1 {
		t.Fatalf("expected one row, got %d", len(report.Rows))
	}
	row := report.Rows[0]
	if row.Outcome != AccountSkipped {
		t.Fatalf("want the row skipped, got %s (%s)", row.Outcome, row.Reason)
	}
	if row.Reason != SkipChangedSincePreview {
		t.Fatalf("want reason %q, got %q", SkipChangedSincePreview, row.Reason)
	}
	if len(w.store.accounts) != 0 {
		t.Fatal("an account was written for a row whose price changed after approval")
	}
	if report.Outcome != BulkCompletedWithSkips {
		t.Errorf("want %s, got %s", BulkCompletedWithSkips, report.Outcome)
	}
}

// The control for the test above: an unchanged row is priced, so the skip is
// the mechanism working rather than the mechanism refusing everything.
func TestGenerateAccountsBulkCreatesApprovedRow(t *testing.T) {
	w := newBulkWorld(t)
	person := w.addStudent("S011", "دعاء فاضل", "خديجة")
	enrollment := w.addEnrollment(person, w.targetYear, 1, academic.ResultPending, academic.StatusActive)

	preview := w.priceCohort(t, GenerateAccountsBulkInput{
		AcademicYearID: w.targetYear.ID,
		DryRun:         true,
	})
	report := w.priceCohort(t, GenerateAccountsBulkInput{
		AcademicYearID: w.targetYear.ID,
		Approved: []AccountApproval{{
			EnrollmentID: enrollment.ID,
			PreviewHash:  preview.Rows[0].PreviewHash,
		}},
	})

	if report.Counts.Created != 1 {
		t.Fatalf("want one account created, got %+v (%s)", report.Counts, report.Rows[0].Reason)
	}
	if len(w.store.accounts) != 1 {
		t.Fatalf("want one stored account, got %d", len(w.store.accounts))
	}
	if report.Outcome != BulkCompleted {
		t.Errorf("want %s, got %s", BulkCompleted, report.Outcome)
	}
	_ = person
}

// A row inside the scope that nobody previewed is reported, never priced on an
// authority that was not given.
func TestGenerateAccountsBulkSkipsRowNobodyPreviewed(t *testing.T) {
	w := newBulkWorld(t)
	approvedStudent := w.addStudent("S012", "عمر ياسين", "شيماء")
	approved := w.addEnrollment(approvedStudent, w.targetYear, 1, academic.ResultPending, academic.StatusActive)

	preview := w.priceCohort(t, GenerateAccountsBulkInput{
		AcademicYearID: w.targetYear.ID,
		DryRun:         true,
	})

	// Registered after the manager approved the list.
	latecomer := w.addStudent("S013", "تبارك رعد", "إيمان")
	w.addEnrollment(latecomer, w.targetYear, 1, academic.ResultPending, academic.StatusActive)

	report := w.priceCohort(t, GenerateAccountsBulkInput{
		AcademicYearID: w.targetYear.ID,
		Approved: []AccountApproval{{
			EnrollmentID: approved.ID,
			PreviewHash:  preview.Rows[0].PreviewHash,
		}},
	})

	if report.Counts.Created != 1 {
		t.Fatalf("the approved row should have been priced: %+v", report.Counts)
	}
	if report.Counts.Skipped != 1 {
		t.Fatalf("the unapproved row should have been skipped: %+v", report.Counts)
	}
	if report.SkipReasons[SkipNotPreviewed] != 1 {
		t.Errorf("want the skip reported as %q, got %+v", SkipNotPreviewed, report.SkipReasons)
	}
}

// ---------------------------------------------------------------------------
// Import validation: the duplicate-handling matrix
// ---------------------------------------------------------------------------

// Every shape from the design's duplicate matrix, and the disposition each one
// must produce.
func TestValidateImportDuplicateMatrix(t *testing.T) {
	w := newBulkWorld(t)

	// Already on file and already enrolled this year, in ENG stage 1.
	enrolled := w.addStudent("E100", "سارة محمد جاسم", "فاطمة")
	w.addEnrollment(enrolled, w.targetYear, 1, academic.ResultPending, academic.StatusActive)
	// Also on file and enrolled, used for the conflicting-seat row.
	elsewhere := w.addStudent("E101", "ليث فراس", "أسماء")
	w.addEnrollment(elsewhere, w.targetYear, 1, academic.ResultPending, academic.StatusActive)
	// On file, and the row will disagree about the mother's name.
	mismatched := w.addStudent("E102", "حيدر منتظر", "رقية")
	_ = mismatched
	// On file with no enrollment this year.
	unenrolled := w.addStudent("E200", "كرار عادل", "نجاة")
	_ = unenrolled

	rows := []map[string]any{
		// 1: new person, everything resolves.
		w.importRow("N001", "مصطفى رياض", "وفاء", "ENG", "MORNING", "1"),
		// 2 and 3: the same university number twice inside one file.
		w.importRow("D001", "علياء حاتم", "بتول", "ENG", "MORNING", "1"),
		w.importRow("D001", "علياء حاتم", "بتول", "ENG", "MORNING", "2"),
		// 4: on file, name and mother match, not yet enrolled this year.
		w.importRow("E200", "كرار عادل", "نجاة", "ENG", "MORNING", "2"),
		// 5: on file, mother's name differs.
		w.importRow("E102", "حيدر منتظر", "زهراء", "ENG", "MORNING", "1"),
		// 6: enrollment already exists for this year, in the same seat.
		w.importRow("E100", "سارة محمد جاسم", "فاطمة", "ENG", "MORNING", "1"),
		// 7: enrolled this year, but the row puts them in another stage.
		w.importRow("E101", "ليث فراس", "أسماء", "ENG", "MORNING", "3"),
		// 8: unknown department.
		w.importRow("N002", "ياسر نزار", "سهام", "NOPE", "MORNING", "1"),
		// 9: unknown study type.
		w.importRow("N003", "بيداء صلاح", "غادة", "ENG", "NIGHTLY", "1"),
		// 10: a stage the programme does not run.
		w.importRow("N004", "أنس طارق", "منى", "ENG", "MORNING", "9"),
	}

	// Rows 2 and 3 differ by stage, so both survive staging as distinct lines
	// and the duplicate check sees the repeated number on each of them.
	batch := w.stageRows(rows)
	if _, err := w.imports.ValidateImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("validating the batch: %v", err)
	}

	cases := []struct {
		rowNo       int
		name        string
		disposition port.ImportDisposition
		status      port.ImportRowStatus
		finding     string
	}{
		{1, "a new student with valid references is created", port.DispositionCreate, port.RowValid, ""},
		{2, "a number duplicated inside the file is an error", port.DispositionError, port.RowError, FindingDuplicateInFile},
		{3, "the other half of the duplicate is an error too", port.DispositionError, port.RowError, FindingDuplicateInFile},
		{4, "an existing student with matching name and mother is enrolled only", port.DispositionUpdate, port.RowWarning, ""},
		{5, "the same number under a different mother needs a human", port.DispositionError, port.RowError, FindingIdentityMismatch},
		{6, "an identical enrollment is skipped", port.DispositionSkip, port.RowWarning, ""},
		{7, "a conflicting enrollment is an error, never an implicit supersede", port.DispositionError, port.RowError, FindingEnrollmentConflct},
		{8, "an unknown department is an error", port.DispositionError, port.RowError, FindingUnknownDepartment},
		{9, "an unknown study type is an error", port.DispositionError, port.RowError, FindingUnknownStudyType},
		{10, "a stage the programme does not run is an error", port.DispositionError, port.RowError, FindingInvalidStage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := w.store.importRows[tc.rowNo]
			if row == nil {
				t.Fatalf("row %d is missing from the batch", tc.rowNo)
			}
			if row.Disposition != tc.disposition {
				t.Errorf("row %d: want disposition %q, got %q (errors %+v)",
					tc.rowNo, tc.disposition, row.Disposition, row.Errors)
			}
			if row.ValidationStatus != tc.status {
				t.Errorf("row %d: want status %q, got %q", tc.rowNo, tc.status, row.ValidationStatus)
			}
			if tc.finding == "" {
				return
			}
			if !hasBulkFinding(row.Errors, tc.finding) {
				t.Errorf("row %d: want finding %q, got %+v", tc.rowNo, tc.finding, row.Errors)
			}
		})
	}
}

// Confirmation is refused while any error row is still nobody's decision, and
// allowed once a reviewer has dispositioned them.
func TestConfirmImportRequiresResolvedErrors(t *testing.T) {
	w := newBulkWorld(t)
	batch := w.stageRows([]map[string]any{
		w.importRow("N010", "زهراء منير", "سهاد", "ENG", "MORNING", "1"),
		w.importRow("N011", "باقر أمير", "رنا", "NOPE", "MORNING", "1"),
	})
	if _, err := w.imports.ValidateImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("validating: %v", err)
	}

	_, err := w.imports.ConfirmImport(context.Background(), w.actor, batch.ID)
	if err == nil {
		t.Fatal("a batch with an unresolved error row was confirmed")
	}
	if code := shared.CodeOf(err); code != "import.unresolved_errors" {
		t.Fatalf("want import.unresolved_errors, got %s", code)
	}

	// Overriding a rejected row requires a written reason.
	if _, err := w.imports.SetRowDisposition(context.Background(), w.actor, SetDispositionInput{
		BatchID:     batch.ID,
		RowNo:       2,
		Disposition: port.DispositionSkip,
	}); shared.CodeOf(err) != "import.override_reason_required" {
		t.Fatalf("want import.override_reason_required, got %v", err)
	}

	if _, err := w.imports.SetRowDisposition(context.Background(), w.actor, SetDispositionInput{
		BatchID:     batch.ID,
		RowNo:       2,
		Disposition: port.DispositionSkip,
		Reason:      "the department code was wrong in the source file; this student is handled separately",
	}); err != nil {
		t.Fatalf("dispositioning row 2: %v", err)
	}

	confirmed, err := w.imports.ConfirmImport(context.Background(), w.actor, batch.ID)
	if err != nil {
		t.Fatalf("confirming after the override: %v", err)
	}
	if confirmed.Status != port.BatchConfirmed {
		t.Fatalf("want %s, got %s", port.BatchConfirmed, confirmed.Status)
	}
	if confirmed.ConfirmedBy == nil || confirmed.ConfirmedAt == nil {
		t.Error("a confirmed batch must carry who confirmed it and when")
	}
}

// A poisoned row must not take the good ones with it, the batch must say it
// finished with skips, and no financial account may exist afterwards.
func TestRunImportCommitsPerRowAndNeverPricesAnyone(t *testing.T) {
	w := newBulkWorld(t)
	batch := w.stageRows([]map[string]any{
		w.importRow("N020", "حوراء عمار", "ابتسام", "ENG", "MORNING", "1"),
		w.importRow("N021", "جعفر سلام", "نادية", "ENG", "MORNING", "1"),
	})
	if _, err := w.imports.ValidateImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("validating: %v", err)
	}
	if _, err := w.imports.ConfirmImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("confirming: %v", err)
	}

	// The second row's registration fails, the way a number colliding with one
	// created a second ago would.
	w.store.failStudentNo = "N021"

	report, err := w.imports.RunImport(context.Background(), w.actor, batch.ID)
	if err != nil {
		t.Fatalf("running the import: %v", err)
	}

	if report.Batch.CreatedRows != 1 {
		t.Errorf("the good row should have been committed on its own: %+v", report.Counts)
	}
	if report.Batch.FailedRows != 1 {
		t.Errorf("the poisoned row should be reported as failed: %+v", report.Counts)
	}
	if report.Batch.Status != port.BatchCompletedWithSkips {
		t.Errorf("want %s, got %s", port.BatchCompletedWithSkips, report.Batch.Status)
	}
	if w.store.studentByNo("N020") == nil {
		t.Error("the good row was rolled back with the bad one")
	}
	if w.store.studentByNo("N021") != nil {
		t.Error("the failed row left a student behind")
	}
	if len(w.store.accounts) != 0 {
		t.Fatal("the import generated a financial account; pricing is a separate, explicit command")
	}
	if report.Batch.CompletedAt == nil {
		t.Error("a finished run must record when it finished")
	}
}

// A resumed run re-attempts only what is still queued, so calling it again
// cannot enroll anybody twice.
func TestResumeImportIsIdempotent(t *testing.T) {
	w := newBulkWorld(t)
	batch := w.stageRows([]map[string]any{
		w.importRow("N030", "آية سعد", "بلقيس", "ENG", "MORNING", "1"),
	})
	if _, err := w.imports.ValidateImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("validating: %v", err)
	}
	if _, err := w.imports.ConfirmImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("confirming: %v", err)
	}
	if _, err := w.imports.RunImport(context.Background(), w.actor, batch.ID); err != nil {
		t.Fatalf("running: %v", err)
	}

	// The worker is presumed dead mid-batch and the reaper hands it back.
	w.store.batch.Status = port.BatchImporting
	report, err := w.imports.ResumeImport(context.Background(), w.actor, batch.ID)
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if report.Remaining != 0 {
		t.Errorf("nothing was left to do, got %d queued", report.Remaining)
	}
	if got := len(w.store.enrollments); got != 1 {
		t.Fatalf("a resume enrolled the same row twice: %d enrollments", got)
	}
	if report.Batch.Status != port.BatchImported {
		t.Errorf("want %s, got %s", port.BatchImported, report.Batch.Status)
	}
}

// ---------------------------------------------------------------------------
// Test bulkWorld
// ---------------------------------------------------------------------------

type bulkWorld struct {
	t     *testing.T
	store *bulkStore
	deps  Deps
	actor shared.Actor

	bulk    *BulkService
	imports *ImportService

	sourceYear     *academic.Year
	targetYear     *academic.Year
	department     *academic.Department
	studyType      *academic.StudyType
	repeatCategory *academic.StudentCategory
	regularCateg   *academic.StudentCategory
}

func newBulkWorld(t *testing.T) *bulkWorld {
	t.Helper()

	w := &bulkWorld{t: t, store: newBulkStore()}
	w.sourceYear = &academic.Year{
		ID: shared.NewID(), Code: "2024-2025", Status: academic.YearOpen,
		StartDate: shared.NewDate(2024, time.September, 1),
		EndDate:   shared.NewDate(2025, time.June, 30),
	}
	w.targetYear = &academic.Year{
		ID: shared.NewID(), Code: "2025-2026", Status: academic.YearOpen,
		DebtBlockPolicy: academic.DebtWarn,
		StartDate:       shared.NewDate(2025, time.September, 1),
		EndDate:         shared.NewDate(2026, time.June, 30),
	}
	w.department = &academic.Department{
		ID: shared.NewID(), CollegeID: shared.NewID(), Code: "ENG",
		NameAr: "الهندسة", StageCount: 4, IsActive: true,
	}
	w.studyType = &academic.StudyType{ID: shared.NewID(), Code: "MORNING", NameAr: "صباحي", IsActive: true}
	w.repeatCategory = &academic.StudentCategory{ID: shared.NewID(), Code: academic.CategoryRepeat, IsActive: true}
	w.regularCateg = &academic.StudentCategory{ID: shared.NewID(), Code: academic.CategoryRegular, IsActive: true}

	w.store.years[w.sourceYear.ID] = w.sourceYear
	w.store.years[w.targetYear.ID] = w.targetYear
	w.store.departments[w.department.ID] = w.department
	w.store.studyTypes[w.studyType.Code] = w.studyType
	w.store.categories[w.repeatCategory.Code] = w.repeatCategory
	w.store.categories[w.regularCateg.Code] = w.regularCateg
	w.store.policy = w.buildPolicy("FP-BASE", 1_000_000)
	w.store.template = &billing.InstallmentTemplate{
		ID: shared.NewID(), Code: "T-2", Status: billing.PolicyPublished,
		Lines: []billing.TemplateLine{
			{LineNo: 1, ShareBP: 5_000, DueOffsetDays: 30},
			{LineNo: 2, ShareBP: 5_000, DueOffsetDays: 120},
		},
	}

	tx := &bulkTx{store: w.store}
	w.deps = Deps{
		Tx:           tx,
		Students:     &bulkStudents{store: w.store},
		Years:        &bulkYears{store: w.store},
		Enrollments:  &bulkEnrollments{store: w.store},
		Reference:    &bulkReference{store: w.store},
		FeePolicies:  &bulkPolicies{store: w.store},
		Templates:    &bulkTemplates{store: w.store},
		Discounts:    &bulkDiscounts{},
		Accounts:     &bulkAccounts{store: w.store},
		Installments: &bulkInstallments{store: w.store},
		Audit:        &bulkAudit{store: w.store},
		Clock:        shared.FixedClock{Instant: time.Date(2025, time.August, 1, 9, 0, 0, 0, time.UTC)},
	}
	w.actor = shared.Actor{
		UserID:   shared.NewID(),
		Username: "officer",
		Roles: []shared.Role{
			shared.RoleAcademicOfficer, shared.RoleFinanceManager, shared.RoleRegistrar, shared.RoleAdmin,
		},
	}

	w.bulk = NewBulkService(w.deps, NewAccountService(w.deps), NewEnrollmentService(w.deps))
	w.imports = NewImportService(
		w.deps, &bulkImports{store: w.store}, NewStudentService(w.deps), NewEnrollmentService(w.deps))
	return w
}

func (w *bulkWorld) buildPolicy(code string, tuition int64) *billing.FeePolicy {
	policyID := shared.NewID()
	return &billing.FeePolicy{
		ID: policyID, PolicyCode: code, VersionNo: 1,
		AcademicYearID: w.targetYear.ID, SpecificityScore: 8, Status: billing.PolicyPublished,
		Components: []*billing.FeeComponent{{
			ID: shared.NewID(), FeePolicyID: policyID, Code: "TUITION", NameAr: "قسط",
			Amount: money.FromInt64(tuition), IsDiscountable: true, IsRefundable: true, IsMandatory: true,
		}},
	}
}

func (w *bulkWorld) addStudent(studentNo, fullName, motherName string) *student.Student {
	w.t.Helper()
	person := &student.Student{
		ID: shared.NewID(), StudentNo: studentNo,
		FullName: fullName, MotherName: motherName, Status: student.StatusActive,
	}
	w.store.students[person.ID] = person
	return person
}

func (w *bulkWorld) addEnrollment(
	person *student.Student, year *academic.Year, stage int16,
	result academic.AcademicResult, status academic.EnrollmentStatus,
) *academic.Enrollment {
	w.t.Helper()
	e := &academic.Enrollment{
		ID: shared.NewID(), StudentID: person.ID, AcademicYearID: year.ID, SequenceNo: 1,
		CollegeID: w.department.CollegeID, DepartmentID: w.department.ID,
		StudyTypeID: w.studyType.ID, StudentCategoryID: w.regularCateg.ID,
		Stage: stage, AttemptNumber: 1, Kind: academic.KindRegular,
		Status: status, Result: result,
	}
	w.store.enrollments[e.ID] = e
	return e
}

func (w *bulkWorld) promote(t *testing.T, in PromoteBulkInput) *PromoteBulkResult {
	t.Helper()
	result, err := w.bulk.PromoteStudentsBulk(context.Background(), w.actor, in)
	if err != nil {
		t.Fatalf("promoting (dry_run=%t): %v", in.DryRun, err)
	}
	return result
}

func (w *bulkWorld) priceCohort(t *testing.T, in GenerateAccountsBulkInput) *GenerateAccountsBulkResult {
	t.Helper()
	result, err := w.bulk.GenerateFinancialAccountsBulk(context.Background(), w.actor, in)
	if err != nil {
		t.Fatalf("pricing the cohort (dry_run=%t): %v", in.DryRun, err)
	}
	return result
}

func (w *bulkWorld) importRow(studentNo, fullName, motherName, department, studyType, stage string) map[string]any {
	return map[string]any{
		ColStudentNo:      studentNo,
		ColFullName:       fullName,
		ColMotherName:     motherName,
		ColDepartmentCode: department,
		ColStudyTypeCode:  studyType,
		ColStage:          stage,
	}
}

// stageRows puts rows straight into the fake store, standing in for an upload.
func (w *bulkWorld) stageRows(rows []map[string]any) *port.ImportBatch {
	w.t.Helper()
	result, err := w.imports.StageImport(context.Background(), w.actor, StageImportInput{
		BatchType:      port.BatchTypeStudents,
		Filename:       "students.csv",
		AcademicYearID: &w.targetYear.ID,
		Rows:           rows,
	})
	if err != nil {
		w.t.Fatalf("staging rows: %v", err)
	}
	return result.Batch
}

func hasBulkFinding(findings []port.ImportFinding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Fake store
// ---------------------------------------------------------------------------

// bulkStore is the whole bulkWorld's state in maps, so the fakes below stay to one
// line each and a rollback is a matter of putting the containers back.
type bulkStore struct {
	students    map[shared.ID]*student.Student
	years       map[shared.ID]*academic.Year
	enrollments map[shared.ID]*academic.Enrollment
	departments map[shared.ID]*academic.Department
	studyTypes  map[string]*academic.StudyType
	categories  map[string]*academic.StudentCategory
	accounts    map[shared.ID]*billing.Account
	audits      []port.AuditEntry

	policy   *billing.FeePolicy
	template *billing.InstallmentTemplate

	batch      *port.ImportBatch
	importRows map[int]*port.ImportRow

	// failStudentNo makes one row's registration fail, standing in for the
	// unique-number collision a real run hits.
	failStudentNo string
}

func newBulkStore() *bulkStore {
	return &bulkStore{
		students:    map[shared.ID]*student.Student{},
		years:       map[shared.ID]*academic.Year{},
		enrollments: map[shared.ID]*academic.Enrollment{},
		departments: map[shared.ID]*academic.Department{},
		studyTypes:  map[string]*academic.StudyType{},
		categories:  map[string]*academic.StudentCategory{},
		accounts:    map[shared.ID]*billing.Account{},
		importRows:  map[int]*port.ImportRow{},
	}
}

func (s *bulkStore) studentByNo(studentNo string) *student.Student {
	for _, person := range s.students {
		if person.StudentNo == studentNo {
			return person
		}
	}
	return nil
}

func (s *bulkStore) enrollmentIn(yearID, studentID shared.ID) *academic.Enrollment {
	for _, e := range s.enrollments {
		if e.AcademicYearID == yearID && e.StudentID == studentID && e.IsLive() {
			return e
		}
	}
	return nil
}

func (s *bulkStore) accountFor(enrollmentID shared.ID) *billing.Account {
	for _, a := range s.accounts {
		if a.EnrollmentID == enrollmentID {
			return a
		}
	}
	return nil
}

// snapshot copies the containers and returns the undo, which is what lets the
// fake transaction reproduce the savepoint behaviour the real one relies on:
// a closure that returns an error leaves nothing behind.
func (s *bulkStore) snapshot() func() {
	students := copyBulkMap(s.students)
	enrollments := copyBulkMap(s.enrollments)
	accounts := copyBulkMap(s.accounts)
	rows := make(map[int]port.ImportRow, len(s.importRows))
	for no, row := range s.importRows {
		rows[no] = *row
	}
	audits := append([]port.AuditEntry(nil), s.audits...)
	batch := *s.batchOrZero()

	return func() {
		s.students = students
		s.enrollments = enrollments
		s.accounts = accounts
		for no, row := range rows {
			restored := row
			s.importRows[no] = &restored
		}
		s.audits = audits
		if s.batch != nil {
			*s.batch = batch
		}
	}
}

func (s *bulkStore) batchOrZero() *port.ImportBatch {
	if s.batch == nil {
		return &port.ImportBatch{}
	}
	return s.batch
}

func copyBulkMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// bulkTx runs closures directly and undoes their writes on failure, which is
// the property the per-row isolation of both bulk commands depends on.
type bulkTx struct{ store *bulkStore }

func (f *bulkTx) Write(ctx context.Context, fn func(context.Context) error) error {
	undo := f.store.snapshot()
	if err := fn(ctx); err != nil {
		undo()
		return err
	}
	return nil
}

func (f *bulkTx) Read(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (f *bulkTx) RequireTx(context.Context, string) error { return nil }

func bulkNotFound(what string) error {
	return shared.NotFound("not_found", "%s does not exist", what)
}

// ---------------------------------------------------------------------------
// Fake repositories
// ---------------------------------------------------------------------------

type bulkStudents struct{ store *bulkStore }

func (f *bulkStudents) Create(_ context.Context, s *student.Student) error {
	if f.store.failStudentNo != "" && s.StudentNo == f.store.failStudentNo {
		return shared.Conflict("student.duplicate_number", "a student with this number already exists")
	}
	f.store.students[s.ID] = s
	return nil
}

func (f *bulkStudents) Update(_ context.Context, s *student.Student) error {
	f.store.students[s.ID] = s
	return nil
}

func (f *bulkStudents) GetByID(_ context.Context, id shared.ID) (*student.Student, error) {
	if s, ok := f.store.students[id]; ok {
		return s, nil
	}
	return nil, bulkNotFound("student")
}

func (f *bulkStudents) GetByStudentNo(_ context.Context, studentNo string) (*student.Student, error) {
	if s := f.store.studentByNo(studentNo); s != nil {
		return s, nil
	}
	return nil, bulkNotFound("student")
}

func (f *bulkStudents) FindPossibleDuplicates(
	_ context.Context, fullName, motherName string, _ *shared.Date,
) ([]*student.Student, error) {
	var found []*student.Student
	for _, s := range f.store.students {
		if strings.EqualFold(s.FullName, fullName) && strings.EqualFold(s.MotherName, motherName) {
			found = append(found, s)
		}
	}
	return found, nil
}

func (f *bulkStudents) Search(context.Context, port.StudentSearch) ([]*student.Student, int, error) {
	return nil, 0, nil
}
func (f *bulkStudents) AppendIdentityVersion(context.Context, *student.IdentityVersion) error {
	return nil
}
func (f *bulkStudents) IdentityHistory(context.Context, shared.ID) ([]*student.IdentityVersion, error) {
	return nil, nil
}

type bulkYears struct{ store *bulkStore }

func (f *bulkYears) Create(context.Context, *academic.Year) error { return nil }
func (f *bulkYears) Update(context.Context, *academic.Year) error { return nil }
func (f *bulkYears) GetByID(_ context.Context, id shared.ID) (*academic.Year, error) {
	if y, ok := f.store.years[id]; ok {
		return y, nil
	}
	return nil, bulkNotFound("academic year")
}
func (f *bulkYears) GetByCode(context.Context, string) (*academic.Year, error) {
	return nil, bulkNotFound("academic year")
}
func (f *bulkYears) List(context.Context) ([]*academic.Year, error) { return nil, nil }
func (f *bulkYears) GetForUpdate(ctx context.Context, id shared.ID) (*academic.Year, error) {
	return f.GetByID(ctx, id)
}
func (f *bulkYears) CurrentOpen(context.Context) ([]*academic.Year, error) { return nil, nil }

type bulkEnrollments struct{ store *bulkStore }

func (f *bulkEnrollments) Create(_ context.Context, e *academic.Enrollment) error {
	f.store.enrollments[e.ID] = e
	return nil
}
func (f *bulkEnrollments) Update(_ context.Context, e *academic.Enrollment) error {
	f.store.enrollments[e.ID] = e
	return nil
}
func (f *bulkEnrollments) GetByID(_ context.Context, id shared.ID) (*academic.Enrollment, error) {
	if e, ok := f.store.enrollments[id]; ok {
		return e, nil
	}
	return nil, bulkNotFound("enrollment")
}
func (f *bulkEnrollments) GetLive(_ context.Context, studentID, yearID shared.ID) (*academic.Enrollment, error) {
	if e := f.store.enrollmentIn(yearID, studentID); e != nil {
		return e, nil
	}
	return nil, bulkNotFound("live enrollment")
}
func (f *bulkEnrollments) History(context.Context, shared.ID) ([]*academic.Enrollment, error) {
	return nil, nil
}

func (f *bulkEnrollments) List(
	_ context.Context, filter port.EnrollmentFilter,
) ([]*academic.Enrollment, int, error) {
	var matched []*academic.Enrollment
	for _, e := range f.store.enrollments {
		switch {
		case filter.AcademicYearID != nil && e.AcademicYearID != *filter.AcademicYearID:
			continue
		case filter.DepartmentID != nil && e.DepartmentID != *filter.DepartmentID:
			continue
		case filter.Stage != nil && e.Stage != *filter.Stage:
			continue
		case filter.Status != nil && e.Status != *filter.Status:
			continue
		case filter.ExcludeSuperseded && e.Status == academic.StatusSuperseded:
			continue
		}
		matched = append(matched, e)
	}
	// Stable order, so a plan hash computed twice over the same data matches.
	sortBulkEnrollments(matched)

	total := len(matched)
	if filter.Offset >= total {
		return nil, total, nil
	}
	end := min(filter.Offset+max(filter.Limit, 1), total)
	return matched[filter.Offset:end], total, nil
}

func (f *bulkEnrollments) CountAttempts(
	_ context.Context, studentID, departmentID shared.ID, stage int16,
) (int, error) {
	count := 0
	for _, e := range f.store.enrollments {
		if e.StudentID == studentID && e.DepartmentID == departmentID && e.Stage == stage && e.CountsAsAttempt() {
			count++
		}
	}
	return count, nil
}

func (f *bulkEnrollments) NextSequenceNo(context.Context, shared.ID, shared.ID) (int16, error) {
	return 1, nil
}
func (f *bulkEnrollments) PendingResults(context.Context, shared.ID) (int, error) { return 0, nil }
func (f *bulkEnrollments) CreateHostingRecord(context.Context, *academic.HostingRecord) error {
	return nil
}
func (f *bulkEnrollments) GetHostingRecord(context.Context, shared.ID) (*academic.HostingRecord, error) {
	return nil, bulkNotFound("hosting record")
}
func (f *bulkEnrollments) UpdateHostingRecord(context.Context, *academic.HostingRecord) error {
	return nil
}
func (f *bulkEnrollments) ListHostingRecords(
	context.Context, *shared.ID, *academic.HostingDirection,
) ([]*academic.HostingRecord, error) {
	return nil, nil
}
func (f *bulkEnrollments) Reassign(context.Context, shared.ID, shared.ID) error { return nil }

func sortBulkEnrollments(list []*academic.Enrollment) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ID.String() < list[j-1].ID.String(); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

type bulkReference struct{ store *bulkStore }

func (f *bulkReference) ListColleges(context.Context, bool) ([]*academic.College, error) {
	return nil, nil
}
func (f *bulkReference) GetCollege(context.Context, shared.ID) (*academic.College, error) {
	return nil, bulkNotFound("college")
}
func (f *bulkReference) CreateCollege(context.Context, *academic.College) error { return nil }

func (f *bulkReference) ListDepartments(
	context.Context, *shared.ID, bool,
) ([]*academic.Department, error) {
	out := make([]*academic.Department, 0, len(f.store.departments))
	for _, d := range f.store.departments {
		out = append(out, d)
	}
	return out, nil
}

func (f *bulkReference) GetDepartment(_ context.Context, id shared.ID) (*academic.Department, error) {
	if d, ok := f.store.departments[id]; ok {
		return d, nil
	}
	return nil, bulkNotFound("department")
}
func (f *bulkReference) CreateDepartment(context.Context, *academic.Department) error { return nil }

func (f *bulkReference) ListStudyTypes(context.Context, bool) ([]*academic.StudyType, error) {
	out := make([]*academic.StudyType, 0, len(f.store.studyTypes))
	for _, s := range f.store.studyTypes {
		out = append(out, s)
	}
	return out, nil
}

func (f *bulkReference) GetStudyType(context.Context, shared.ID) (*academic.StudyType, error) {
	return nil, bulkNotFound("study type")
}
func (f *bulkReference) GetStudyTypeByCode(_ context.Context, code string) (*academic.StudyType, error) {
	if s, ok := f.store.studyTypes[strings.ToUpper(code)]; ok {
		return s, nil
	}
	return nil, bulkNotFound("study type")
}
func (f *bulkReference) CreateStudyType(context.Context, *academic.StudyType) error { return nil }

// The administration half of the reference repository. The bulk commands do
// not touch master data, so these are stubs — but the interface is one
// interface, and a stub that lies about succeeding is safer here than a panic
// in a test that never calls it.
func (f *bulkReference) UpdateCollege(context.Context, *academic.College) error       { return nil }
func (f *bulkReference) UpdateDepartment(context.Context, *academic.Department) error { return nil }
func (f *bulkReference) UpdateStudyType(context.Context, *academic.StudyType) error   { return nil }
func (f *bulkReference) CreateStudentCategory(context.Context, *academic.StudentCategory) error {
	return nil
}
func (f *bulkReference) GetStudentCategory(context.Context, shared.ID) (*academic.StudentCategory, error) {
	return nil, bulkNotFound("student category")
}
func (f *bulkReference) UpdateStudentCategory(context.Context, *academic.StudentCategory) error {
	return nil
}
func (f *bulkReference) CreatePaymentMethod(context.Context, *payment.Method) error { return nil }
func (f *bulkReference) UpdatePaymentMethod(context.Context, *payment.Method) error { return nil }
func (f *bulkReference) ListCashierDesks(context.Context, bool) ([]*payment.CashierDesk, error) {
	return nil, nil
}
func (f *bulkReference) GetCashierDesk(context.Context, shared.ID) (*payment.CashierDesk, error) {
	return nil, bulkNotFound("cashier desk")
}
func (f *bulkReference) CreateCashierDesk(context.Context, *payment.CashierDesk) error { return nil }
func (f *bulkReference) UpdateCashierDesk(context.Context, *payment.CashierDesk) error { return nil }
func (f *bulkReference) UsageCount(context.Context, port.MasterDataKind, shared.ID) (int, error) {
	return 0, nil
}
func (f *bulkReference) HighestStageInUse(context.Context, shared.ID) (int16, error) { return 0, nil }

func (f *bulkReference) ListStudentCategories(context.Context, bool) ([]*academic.StudentCategory, error) {
	return nil, nil
}
func (f *bulkReference) GetStudentCategoryByCode(
	_ context.Context, code string,
) (*academic.StudentCategory, error) {
	if c, ok := f.store.categories[strings.ToUpper(code)]; ok {
		return c, nil
	}
	return nil, bulkNotFound("student category")
}
func (f *bulkReference) ListPaymentMethods(context.Context, bool) ([]*payment.Method, error) {
	return nil, nil
}
func (f *bulkReference) GetPaymentMethod(context.Context, shared.ID) (*payment.Method, error) {
	return nil, bulkNotFound("payment method")
}

type bulkPolicies struct{ store *bulkStore }

func (f *bulkPolicies) Create(context.Context, *billing.FeePolicy) error { return nil }
func (f *bulkPolicies) Publish(context.Context, shared.ID, shared.ID, time.Time) error {
	return nil
}
func (f *bulkPolicies) GetByID(_ context.Context, id shared.ID) (*billing.FeePolicy, error) {
	if f.store.policy != nil && f.store.policy.ID == id {
		return f.store.policy, nil
	}
	return nil, bulkNotFound("fee policy")
}
func (f *bulkPolicies) List(context.Context, shared.ID) ([]*billing.FeePolicy, error) {
	return nil, nil
}
func (f *bulkPolicies) Resolve(context.Context, port.FeeScope) (*billing.FeePolicy, error) {
	if f.store.policy == nil {
		return nil, bulkNotFound("fee policy")
	}
	return f.store.policy, nil
}

type bulkTemplates struct{ store *bulkStore }

func (f *bulkTemplates) Create(context.Context, *billing.InstallmentTemplate) error { return nil }
func (f *bulkTemplates) Publish(context.Context, shared.ID, shared.ID, time.Time) error {
	return nil
}
func (f *bulkTemplates) GetByID(context.Context, shared.ID) (*billing.InstallmentTemplate, error) {
	return f.store.template, nil
}
func (f *bulkTemplates) List(context.Context, *shared.ID) ([]*billing.InstallmentTemplate, error) {
	return nil, nil
}
func (f *bulkTemplates) Resolve(context.Context, port.FeeScope) (*billing.InstallmentTemplate, error) {
	if f.store.template == nil {
		return nil, bulkNotFound("installment template")
	}
	return f.store.template, nil
}

type bulkDiscounts struct{}

func (f *bulkDiscounts) CreateDefinition(context.Context, *discount.Definition) error { return nil }
func (f *bulkDiscounts) GetDefinition(context.Context, shared.ID) (*discount.Definition, error) {
	return nil, bulkNotFound("discount definition")
}
func (f *bulkDiscounts) GetDefinitionByCode(context.Context, string) (*discount.Definition, error) {
	return nil, bulkNotFound("discount definition")
}
func (f *bulkDiscounts) ListDefinitions(context.Context, bool) ([]*discount.Definition, error) {
	return nil, nil
}
func (f *bulkDiscounts) CreateVersion(context.Context, *discount.DefinitionVersion) error { return nil }
func (f *bulkDiscounts) PublishVersion(context.Context, shared.ID, shared.ID, time.Time) error {
	return nil
}
func (f *bulkDiscounts) GetVersion(context.Context, shared.ID) (*discount.DefinitionVersion, error) {
	return nil, bulkNotFound("discount version")
}
func (f *bulkDiscounts) GetPublishedVersion(context.Context, shared.ID) (*discount.DefinitionVersion, error) {
	return nil, bulkNotFound("published version")
}
func (f *bulkDiscounts) CreateAssignment(context.Context, *discount.Assignment) error { return nil }
func (f *bulkDiscounts) UpdateAssignment(context.Context, *discount.Assignment) error { return nil }
func (f *bulkDiscounts) GetAssignment(context.Context, shared.ID) (*discount.Assignment, error) {
	return nil, bulkNotFound("assignment")
}
func (f *bulkDiscounts) ListAssignmentsForStudent(context.Context, shared.ID) ([]*discount.Assignment, error) {
	return nil, nil
}
func (f *bulkDiscounts) ApprovedAssignmentsCovering(
	context.Context, shared.ID, string,
) ([]*discount.Assignment, error) {
	return nil, nil
}
func (f *bulkDiscounts) HasOverlappingAssignment(
	context.Context, shared.ID, shared.ID, string, string, *shared.ID,
) (bool, error) {
	return false, nil
}
func (f *bulkDiscounts) CreateApplication(context.Context, *discount.Application) error { return nil }
func (f *bulkDiscounts) UpdateApplication(context.Context, *discount.Application) error { return nil }
func (f *bulkDiscounts) GetApplication(context.Context, shared.ID) (*discount.Application, error) {
	return nil, bulkNotFound("application")
}
func (f *bulkDiscounts) ListApplications(context.Context, shared.ID) ([]*discount.Application, error) {
	return nil, nil
}

type bulkAccounts struct{ store *bulkStore }

func (f *bulkAccounts) Create(_ context.Context, a *billing.Account, _ []*billing.SnapshotLine) error {
	f.store.accounts[a.ID] = a
	return nil
}
func (f *bulkAccounts) Update(_ context.Context, a *billing.Account) error {
	f.store.accounts[a.ID] = a
	return nil
}
func (f *bulkAccounts) GetByID(_ context.Context, id shared.ID) (*billing.Account, error) {
	if a, ok := f.store.accounts[id]; ok {
		return a, nil
	}
	return nil, bulkNotFound("account")
}
func (f *bulkAccounts) GetByEnrollment(_ context.Context, enrollmentID shared.ID) (*billing.Account, error) {
	if a := f.store.accountFor(enrollmentID); a != nil {
		return a, nil
	}
	return nil, bulkNotFound("account")
}
func (f *bulkAccounts) GetForUpdate(ctx context.Context, id shared.ID) (*billing.Account, error) {
	return f.GetByID(ctx, id)
}
func (f *bulkAccounts) ListForStudent(context.Context, shared.ID) ([]*billing.Account, error) {
	return nil, nil
}
func (f *bulkAccounts) Reassign(_ context.Context, accountID, toStudentID shared.ID) error {
	if account, ok := f.store.accounts[accountID]; ok {
		account.StudentID = toStudentID
	}
	return nil
}
func (f *bulkAccounts) Snapshot(context.Context, shared.ID) ([]*billing.SnapshotLine, error) {
	return nil, nil
}
func (f *bulkAccounts) CreateAdjustment(context.Context, *billing.Adjustment) error { return nil }
func (f *bulkAccounts) ListAdjustments(context.Context, shared.ID) ([]*billing.Adjustment, error) {
	return nil, nil
}
func (f *bulkAccounts) CreateCredit(context.Context, *billing.CreditEntry) error { return nil }
func (f *bulkAccounts) GetCreditForUpdate(context.Context, shared.ID) (*billing.CreditEntry, error) {
	return nil, bulkNotFound("credit")
}
func (f *bulkAccounts) ListOpenCredits(context.Context, shared.ID) ([]*billing.CreditEntry, error) {
	return nil, nil
}
func (f *bulkAccounts) UpdateCredit(context.Context, *billing.CreditEntry) error { return nil }
func (f *bulkAccounts) RecordCreditConsumption(context.Context, *billing.CreditConsumption) error {
	return nil
}
func (f *bulkAccounts) OutstandingForStudent(
	context.Context, shared.ID, *shared.ID,
) (money.Amount, error) {
	return money.Zero, nil
}
func (f *bulkAccounts) DraftPaymentCount(context.Context, shared.ID) (int, error) { return 0, nil }
func (f *bulkAccounts) ReconciliationDrift(context.Context, int) ([]port.ReconciliationRow, error) {
	return nil, nil
}

type bulkInstallments struct{ store *bulkStore }

func (f *bulkInstallments) CreatePlan(context.Context, []*billing.Installment) error { return nil }
func (f *bulkInstallments) Update(context.Context, *billing.Installment) error       { return nil }
func (f *bulkInstallments) GetByID(context.Context, shared.ID) (*billing.Installment, error) {
	return nil, bulkNotFound("installment")
}
func (f *bulkInstallments) ListForAccount(context.Context, shared.ID) ([]*billing.Installment, error) {
	return nil, nil
}
func (f *bulkInstallments) ListOpenForUpdate(context.Context, shared.ID) ([]*billing.Installment, error) {
	return nil, nil
}
func (f *bulkInstallments) SupersedePlan(context.Context, shared.ID, map[shared.ID]shared.ID) error {
	return nil
}

type bulkAudit struct{ store *bulkStore }

func (f *bulkAudit) Append(_ context.Context, entry port.AuditEntry) error {
	f.store.audits = append(f.store.audits, entry)
	return nil
}
func (f *bulkAudit) List(context.Context, string, shared.ID, int) ([]port.AuditEntry, error) {
	return nil, nil
}
func (f *bulkAudit) ListForStudent(context.Context, shared.ID, int) ([]port.AuditEntry, error) {
	return nil, nil
}
func (f *bulkAudit) VerifyChain(context.Context, int64) ([]port.ChainProblem, error) {
	return nil, nil
}

type bulkImports struct{ store *bulkStore }

func (f *bulkImports) CreateBatch(_ context.Context, b *port.ImportBatch) error {
	f.store.batch = b
	return nil
}
func (f *bulkImports) UpdateBatch(_ context.Context, b *port.ImportBatch) error {
	f.store.batch = b
	return nil
}
func (f *bulkImports) GetBatch(_ context.Context, id shared.ID) (*port.ImportBatch, error) {
	if f.store.batch != nil && f.store.batch.ID == id {
		return f.store.batch, nil
	}
	return nil, bulkNotFound("import batch")
}
func (f *bulkImports) GetBatchForUpdate(ctx context.Context, id shared.ID) (*port.ImportBatch, error) {
	return f.GetBatch(ctx, id)
}
func (f *bulkImports) ListBatches(
	context.Context, *port.ImportBatchStatus, int, int,
) ([]*port.ImportBatch, int, error) {
	return nil, 0, nil
}
func (f *bulkImports) Heartbeat(context.Context, shared.ID, time.Time) error { return nil }
func (f *bulkImports) StalledBatches(context.Context, time.Time, int) ([]*port.ImportBatch, error) {
	return nil, nil
}

func (f *bulkImports) InsertRows(_ context.Context, rows []*port.ImportRow) (int, error) {
	seen := map[string]bool{}
	for _, row := range f.store.importRows {
		seen[row.DedupHash] = true
	}
	inserted := 0
	for _, row := range rows {
		if seen[row.DedupHash] {
			continue
		}
		seen[row.DedupHash] = true
		stored := *row
		f.store.importRows[row.RowNo] = &stored
		inserted++
	}
	return inserted, nil
}

func (f *bulkImports) UpdateRow(_ context.Context, r *port.ImportRow) error {
	stored := *r
	f.store.importRows[r.RowNo] = &stored
	return nil
}

func (f *bulkImports) GetRow(_ context.Context, _ shared.ID, rowNo int) (*port.ImportRow, error) {
	if row, ok := f.store.importRows[rowNo]; ok {
		copied := *row
		return &copied, nil
	}
	return nil, bulkNotFound("import row")
}

func (f *bulkImports) ListRows(
	_ context.Context, _ shared.ID, filter port.ImportRowFilter,
) ([]*port.ImportRow, int, error) {
	var out []*port.ImportRow
	for no := 1; no <= len(f.store.importRows); no++ {
		row, ok := f.store.importRows[no]
		if !ok {
			continue
		}
		if len(filter.Statuses) > 0 && !containsBulkStatus(filter.Statuses, row.ValidationStatus) {
			continue
		}
		copied := *row
		out = append(out, &copied)
	}
	total := len(out)
	if filter.Offset >= total {
		return nil, total, nil
	}
	end := min(filter.Offset+max(filter.Limit, 1), total)
	return out[filter.Offset:end], total, nil
}

func (f *bulkImports) UnprocessedRows(
	_ context.Context, _ shared.ID, _ int,
) ([]*port.ImportRow, error) {
	var out []*port.ImportRow
	for no := 1; no <= len(f.store.importRows); no++ {
		row, ok := f.store.importRows[no]
		if ok && row.IsUnprocessed() {
			copied := *row
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (f *bulkImports) CountRows(context.Context, shared.ID) (port.ImportRowCounts, error) {
	var c port.ImportRowCounts
	for _, row := range f.store.importRows {
		c.Total++
		switch row.ValidationStatus {
		case port.RowPending:
			c.Pending++
		case port.RowValid:
			c.Valid++
		case port.RowWarning:
			c.Warning++
		case port.RowError:
			c.Error++
			if row.Disposition == port.DispositionError {
				c.UnresolvedErrors++
			}
		case port.RowProcessed:
			c.Processed++
			if row.Disposition == port.DispositionUpdate {
				c.Updated++
			} else {
				c.Created++
			}
		case port.RowSkipped:
			c.Skipped++
		case port.RowFailed:
			c.Failed++
		}
	}
	return c, nil
}

func (f *bulkImports) StudentNumberOccurrences(context.Context, shared.ID) (map[string]int, error) {
	counts := map[string]int{}
	for _, row := range f.store.importRows {
		if studentNo := rowText(row.RawData, ColStudentNo); studentNo != "" {
			counts[studentNo]++
		}
	}
	return counts, nil
}

func containsBulkStatus(statuses []port.ImportRowStatus, want port.ImportRowStatus) bool {
	for _, s := range statuses {
		if s == want {
			return true
		}
	}
	return false
}
