package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// BulkService runs the commands that act on a whole cohort at once: rolling a
// year's students into the next one, and pricing a year's enrollments.
//
// Both commands share a shape, and it is the shape that makes them safe to run
// against five hundred students. Nothing happens without a dry run first; the
// dry run produces the real numbers and a fingerprint of them; the commit
// re-derives those numbers and refuses, or skips, anything that moved in
// between. And every row that is not applied is reported with a reason rather
// than dropped, because a report that lists 412 successes out of 500 without
// naming the other 88 reads as "we covered everyone" to the person signing it.
type BulkService struct {
	deps        Deps
	accounts    *AccountService
	enrollments *EnrollmentService
	auditor
}

// NewBulkService wires the cohort-wide commands.
//
// It takes the per-row services rather than reimplementing them. A promotion
// that built enrollments itself would be a second registration path with its
// own idea of attempt numbers and debt policy, and the two would drift.
func NewBulkService(d Deps, accounts *AccountService, enrollments *EnrollmentService) *BulkService {
	return &BulkService{
		deps:        d,
		accounts:    accounts,
		enrollments: enrollments,
		auditor:     newAuditor(d.Audit, d.Clock),
	}
}

const (
	// bulkPageSize bounds how many enrollments are held in memory at once while
	// walking a cohort.
	bulkPageSize = 200
	// maxBulkRows refuses a scope that is almost certainly a mistake. A cohort
	// larger than this is a whole university, not a department's intake, and
	// running it as one command would hold a transaction open for minutes.
	maxBulkRows = 20000
	// maxLoggedRows bounds how many identifiers a skip summary names
	// individually. The counts are always complete; only the list is trimmed.
	maxLoggedRows = 50
)

// BulkOutcome is how a cohort command finished.
type BulkOutcome string

const (
	// BulkCompleted applied every row in scope.
	BulkCompleted BulkOutcome = "completed"
	// BulkCompletedWithSkips finished, but some rows were not applied. It is a
	// separate outcome from "completed" on purpose: a bare success over a run
	// that dropped rows is a lie of omission.
	BulkCompletedWithSkips BulkOutcome = "completed_with_skips"
	// BulkFailed applied nothing.
	BulkFailed BulkOutcome = "failed"
)

// ---------------------------------------------------------------------------
// Promotion
// ---------------------------------------------------------------------------

// PromotionAction is what the plan intends to do with one enrollment.
type PromotionAction string

const (
	// ActionPromote registers the student in the next stage of the target year.
	ActionPromote PromotionAction = "promote"
	// ActionRepeat re-registers the student at the same stage on a further
	// attempt, in the repeat category — which is what prices repeat tuition.
	ActionRepeat PromotionAction = "repeat"
	// ActionComplete closes a final-stage pass. There is no stage above the
	// last one to promote into.
	ActionComplete PromotionAction = "complete"
	// ActionSkip leaves the enrollment alone and states why.
	ActionSkip PromotionAction = "skip"
)

// Skip reasons a promotion can state. Every skipped row carries one; none is
// dropped without a reason a registrar can act on.
const (
	SkipNotActive           = "not_active"
	SkipNoResult            = "no_result"
	SkipAlreadyInTargetYear = "already_enrolled_in_target"
	SkipPriorDebtBlocks     = "prior_debt_blocks"
)

// PromoteBulkInput selects a cohort and where it is going.
type PromoteBulkInput struct {
	SourceYearID shared.ID
	TargetYearID shared.ID

	// Optional narrowing. A promotion is usually run per college or per
	// department so that one faculty's officer approves their own numbers.
	CollegeID    *shared.ID
	DepartmentID *shared.ID
	StudyTypeID  *shared.ID
	Stage        *int16

	// DryRun computes the plan and writes nothing.
	DryRun bool
	// ApprovePlanHash is the PlanHash a dry run returned. A commit without it
	// is refused, and a commit whose recomputed plan disagrees with it is
	// refused too: those are the two halves of "an officer approved these exact
	// numbers".
	ApprovePlanHash string
}

// PromotionRow is one enrollment's place in the plan, and after a commit its
// outcome.
type PromotionRow struct {
	StudentID          shared.ID `json:"student_id"`
	SourceEnrollmentID shared.ID `json:"source_enrollment_id"`
	DepartmentID       shared.ID `json:"department_id"`
	// StudyTypeID carries the seat forward: an evening student stays in the
	// evening programme, which also decides what they are charged.
	StudyTypeID   shared.ID       `json:"study_type_id"`
	FromStage     int16           `json:"from_stage"`
	ToStage       int16           `json:"to_stage"`
	AttemptNumber int16           `json:"attempt_number"`
	CategoryCode  string          `json:"category_code"`
	Result        string          `json:"result"`
	Action        PromotionAction `json:"action"`
	// Reason is filled for every row the plan will not apply, and for every row
	// a commit could not apply.
	Reason string `json:"reason,omitempty"`
	// Applied is true once the row's command succeeded.
	Applied bool `json:"applied"`
	// NewEnrollmentID is what the row produced.
	NewEnrollmentID *shared.ID `json:"new_enrollment_id,omitempty"`
	// ErrorCode carries the stable code of the failure, so a client can group
	// eighty identical failures instead of showing eighty sentences.
	ErrorCode string `json:"error_code,omitempty"`
}

// PromotionCounts is the tally. In a dry run these are projections; after a
// commit they are what happened.
type PromotionCounts struct {
	Total     int `json:"total"`
	Promoted  int `json:"promoted"`
	Repeated  int `json:"repeated"`
	Completed int `json:"completed"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
}

// PromoteBulkResult is the plan, or the report.
type PromoteBulkResult struct {
	DryRun       bool            `json:"dry_run"`
	SourceYearID shared.ID       `json:"source_year_id"`
	TargetYearID shared.ID       `json:"target_year_id"`
	Outcome      BulkOutcome     `json:"outcome"`
	Counts       PromotionCounts `json:"counts"`
	// PlanHash fingerprints the plan. It is what a commit must present, and
	// what a commit re-derives before applying anything.
	PlanHash string `json:"plan_hash"`
	// SkipReasons groups the skipped rows so the summary is readable without
	// scrolling the row list.
	SkipReasons map[string]int `json:"skip_reasons,omitempty"`
	Rows        []PromotionRow `json:"rows"`
}

// PromoteStudentsBulk rolls a cohort from one academic year into the next.
//
// A pass moves the student up a stage on their first attempt at it. A failure
// re-registers them at the same stage with the attempt count incremented and
// the repeat category set, which is the input the repeat-fee policy resolves
// against — the single most consequential field in this command, and the
// reason it is derived from recorded results rather than typed by a clerk.
//
// A pass at the department's final stage completes the enrollment instead of
// promoting it. There is no stage seven in a six-year programme, and inventing
// one would put a student in a seat that does not exist and price it from a
// policy that was never written.
//
// Rows the plan cannot apply are skipped with a stated reason and reported.
// One student with unsettled debt must not abort the other four hundred, which
// is why each row runs in its own savepoint: its failure rolls back that row
// and nothing else.
func (s *BulkService) PromoteStudentsBulk(
	ctx context.Context, actor shared.Actor, in PromoteBulkInput,
) (*PromoteBulkResult, error) {
	if err := actor.RequireAnyRole("PromoteStudentsBulk",
		shared.RoleAcademicOfficer, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if in.SourceYearID == in.TargetYearID {
		return nil, shared.Validation("promotion.same_year",
			"a promotion must move students between two different academic years")
	}
	if !in.DryRun && in.ApprovePlanHash == "" {
		return nil, shared.PreconditionFailed("promotion.dry_run_required",
			"run this promotion as a dry run first and submit the plan hash it returns; "+
				"a cohort is not moved on numbers nobody has read").
			WithDetail("remedy", "POST the same scope with dry_run=true, review the counts, then resubmit with approve_plan_hash")
	}

	if in.DryRun {
		var result *PromoteBulkResult
		err := s.deps.Tx.Read(ctx, func(ctx context.Context) error {
			plan, err := s.buildPromotionPlan(ctx, in)
			if err != nil {
				return err
			}
			result = plan
			return nil
		})
		if err != nil {
			return nil, err
		}
		// The preview is audited in its own transaction: the read-only one it
		// was computed in cannot write, and the record of who saw which numbers
		// is exactly what a later question about the commit will ask for.
		if err := s.recordPreview(ctx, actor, "promotion.bulk_previewed", &in.SourceYearID, map[string]any{
			"source_year_id": in.SourceYearID.String(),
			"target_year_id": in.TargetYearID.String(),
			"plan_hash":      result.PlanHash,
			"counts":         snapshotOf(result.Counts),
			"skip_reasons":   result.SkipReasons,
		}); err != nil {
			return nil, err
		}
		s.logSkipped(ctx, "bulk promotion preview", result.SkipReasons, skippedPromotionIDs(result.Rows, false))
		return result, nil
	}

	var result *PromoteBulkResult
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		plan, err := s.buildPromotionPlan(ctx, in)
		if err != nil {
			return err
		}
		// Re-derived under the transaction that will apply it. If a result was
		// recorded, a student withdrew, or a department's length changed since
		// the officer looked, the plan they approved no longer describes what
		// would happen — so nothing happens.
		if plan.PlanHash != in.ApprovePlanHash {
			return shared.Conflict("promotion.plan_changed_since_preview",
				"the cohort changed since the dry run was approved; review the new plan before promoting").
				WithDetail("approved_plan_hash", in.ApprovePlanHash).
				WithDetail("current_plan_hash", plan.PlanHash).
				WithDetail("current_counts", snapshotOf(plan.Counts))
		}

		s.applyPromotionPlan(ctx, actor, plan)
		plan.DryRun = false
		result = plan

		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &in.TargetYearID,
			Action:         "promotion.bulk_executed",
			Actor:          actor,
			After:          snapshotOf(plan.Counts),
			AcademicYearID: &in.TargetYearID,
			Metadata: map[string]any{
				"source_year_id": in.SourceYearID.String(),
				"target_year_id": in.TargetYearID.String(),
				"plan_hash":      plan.PlanHash,
				"outcome":        string(plan.Outcome),
				"skip_reasons":   plan.SkipReasons,
				"promoted":       plan.Counts.Promoted,
				"repeated":       plan.Counts.Repeated,
				"completed":      plan.Counts.Completed,
				"skipped":        plan.Counts.Skipped,
				"failed":         plan.Counts.Failed,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	s.logSkipped(ctx, "bulk promotion did not apply every row", result.SkipReasons, skippedPromotionIDs(result.Rows, true))
	return result, nil
}

// buildPromotionPlan decides, for every enrollment in scope, what would happen
// to it. It writes nothing, and it is the same code the commit re-runs.
func (s *BulkService) buildPromotionPlan(ctx context.Context, in PromoteBulkInput) (*PromoteBulkResult, error) {
	// The source year is read for its own sake: a mistyped identifier would
	// otherwise match no enrollments and produce an empty plan, which reads
	// exactly like a cohort that has already been promoted.
	if _, err := s.deps.Years.GetByID(ctx, in.SourceYearID); err != nil {
		return nil, err
	}
	targetYear, err := s.deps.Years.GetByID(ctx, in.TargetYearID)
	if err != nil {
		return nil, err
	}
	// Checked once, up front. Five hundred identical per-row failures saying
	// "the year is closed" is not a report anybody can use.
	if !targetYear.AcceptsEnrollment() {
		return nil, shared.PreconditionFailed("promotion.target_year_not_open",
			"academic year %s is %s and is not accepting registrations", targetYear.Code, targetYear.Status).
			WithDetail("target_year", targetYear.Code).
			WithDetail("status", string(targetYear.Status))
	}

	filter := port.EnrollmentFilter{
		AcademicYearID:    &in.SourceYearID,
		CollegeID:         in.CollegeID,
		DepartmentID:      in.DepartmentID,
		StudyTypeID:       in.StudyTypeID,
		Stage:             in.Stage,
		ExcludeSuperseded: true,
	}

	result := &PromoteBulkResult{
		DryRun:       true,
		SourceYearID: in.SourceYearID,
		TargetYearID: in.TargetYearID,
		SkipReasons:  map[string]int{},
	}
	departments := map[shared.ID]*academic.Department{}

	err = s.eachEnrollment(ctx, filter, func(e *academic.Enrollment) error {
		row, err := s.planPromotionRow(ctx, e, targetYear, departments)
		if err != nil {
			return err
		}
		result.Rows = append(result.Rows, row)
		return nil
	})
	if err != nil {
		return nil, err
	}

	result.Counts = countPromotionPlan(result.Rows, result.SkipReasons)
	result.PlanHash = promotionPlanHash(in, result.Rows)
	result.Outcome = BulkCompleted
	if result.Counts.Skipped > 0 {
		result.Outcome = BulkCompletedWithSkips
	}
	return result, nil
}

// planPromotionRow decides one enrollment's fate.
func (s *BulkService) planPromotionRow(
	ctx context.Context,
	e *academic.Enrollment,
	targetYear *academic.Year,
	departments map[shared.ID]*academic.Department,
) (PromotionRow, error) {
	row := PromotionRow{
		StudentID:          e.StudentID,
		SourceEnrollmentID: e.ID,
		DepartmentID:       e.DepartmentID,
		StudyTypeID:        e.StudyTypeID,
		FromStage:          e.Stage,
		ToStage:            e.Stage,
		Result:             string(e.Result),
		Action:             ActionSkip,
	}

	if e.Status != academic.StatusActive {
		row.Reason = SkipNotActive + ": the enrollment is " + string(e.Status)
		return row, nil
	}
	if !e.Passed() && e.Result != academic.ResultFailed {
		row.Reason = SkipNoResult + ": the year's result is " + string(e.Result)
		return row, nil
	}

	// Already registered for the target year — by hand, or by an earlier run of
	// this same command. Re-registering would breach the one-live-enrollment
	// rule, and treating that as a failure would make a re-run look broken when
	// it is merely idempotent.
	existing, err := s.deps.Enrollments.GetLive(ctx, e.StudentID, targetYear.ID)
	switch {
	case err == nil && existing != nil:
		row.Reason = SkipAlreadyInTargetYear + ": enrollment " + existing.ID.String()
		return row, nil
	case err != nil && !missingRecord(err):
		return row, err
	}

	department, ok := departments[e.DepartmentID]
	if !ok {
		department, err = s.deps.Reference.GetDepartment(ctx, e.DepartmentID)
		if err != nil {
			return row, err
		}
		departments[e.DepartmentID] = department
	}

	switch {
	case e.Passed() && e.Stage >= department.StageCount:
		// The last stage of the programme. Completing it is graduation, which
		// the enrollment command derives; promoting it would ask for a stage the
		// department does not run.
		row.Action = ActionComplete
		row.ToStage = e.Stage
		row.AttemptNumber = e.AttemptNumber
		row.CategoryCode = ""
		return row, nil
	case e.Passed():
		row.Action = ActionPromote
		row.ToStage = e.Stage + 1
		row.CategoryCode = academic.CategoryRegular
	default:
		row.Action = ActionRepeat
		row.ToStage = e.Stage
		row.CategoryCode = academic.CategoryRepeat
	}

	// The attempt number is counted from recorded history, exactly as a
	// single registration would count it, so the plan shows the number the
	// commit will actually store rather than a guess.
	priorAttempts, err := s.deps.Enrollments.CountAttempts(ctx, e.StudentID, e.DepartmentID, row.ToStage)
	if err != nil {
		return row, err
	}
	row.AttemptNumber = int16(priorAttempts + 1)

	// Surfaced in the preview rather than discovered as four hundred identical
	// failures at commit: when the target year blocks on debt, a student who
	// owes cannot be rolled forward until finance clears them.
	if targetYear.DebtBlockPolicy == academic.DebtBlock {
		debt, err := s.deps.Accounts.OutstandingForStudent(ctx, e.StudentID, &targetYear.ID)
		if err != nil {
			return row, err
		}
		if debt.IsPositive() {
			row.Action = ActionSkip
			row.ToStage = e.Stage
			row.Reason = SkipPriorDebtBlocks + ": " + debt.Format() + " outstanding from earlier years"
		}
	}

	return row, nil
}

// applyPromotionPlan executes an approved plan, one savepoint per row.
//
// Errors are captured onto the row instead of returned. A promotion that
// aborted on the first refusal would leave the cohort half-moved with no
// record of where it stopped; this way every row is attempted and the report
// names each one that was not applied.
func (s *BulkService) applyPromotionPlan(ctx context.Context, actor shared.Actor, plan *PromoteBulkResult) {
	for i := range plan.Rows {
		row := &plan.Rows[i]
		if row.Action == ActionSkip {
			continue
		}

		// Each row runs inside its own savepoint. WithTx nests, so a refusal
		// here unwinds this student's writes and leaves the rest of the batch
		// intact and still committing.
		err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
			switch row.Action {
			case ActionComplete:
				// No NewEnrollmentID: a completion produces nothing new. The
				// student's own enrollment closes and, at the final stage, the
				// enrollment command derives their graduation from it.
				_, err := s.enrollments.ChangeEnrollmentStatus(ctx, actor, ChangeStatusInput{
					EnrollmentID: row.SourceEnrollmentID,
					Target:       academic.StatusCompleted,
					Reason:       ptr("final-stage pass recorded by cohort promotion"),
				})
				return err
			default:
				created, err := s.enrollments.EnrollStudent(ctx, actor, EnrollStudentInput{
					StudentID:            row.StudentID,
					AcademicYearID:       plan.TargetYearID,
					DepartmentID:         row.DepartmentID,
					StudyTypeID:          row.StudyTypeID,
					Stage:                row.ToStage,
					CategoryCode:         row.CategoryCode,
					Kind:                 academic.KindRegular,
					PreviousEnrollmentID: &row.SourceEnrollmentID,
				})
				if err != nil {
					return err
				}
				row.NewEnrollmentID = &created.Enrollment.ID
				// The plan promised an attempt number; if the commit derived a
				// different one the report must say so rather than keep the
				// projection.
				row.AttemptNumber = created.Enrollment.AttemptNumber
				return nil
			}
		})
		if err != nil {
			row.Applied = false
			row.NewEnrollmentID = nil
			row.ErrorCode = shared.CodeOf(err)
			row.Reason = err.Error()
			continue
		}
		row.Applied = true
	}

	plan.Counts = countPromotionOutcome(plan.Rows, plan.SkipReasons)
	applied := plan.Counts.Promoted + plan.Counts.Repeated + plan.Counts.Completed
	switch {
	// Failed is for a run that could not do its job, not for one that
	// deliberately skipped every row: a cohort where nobody was eligible was
	// correctly processed, and reporting it as a failure would send somebody
	// looking for a fault that is not there.
	case applied == 0 && plan.Counts.Failed > 0:
		plan.Outcome = BulkFailed
	case plan.Counts.Skipped > 0 || plan.Counts.Failed > 0:
		plan.Outcome = BulkCompletedWithSkips
	default:
		plan.Outcome = BulkCompleted
	}
}

func countPromotionPlan(rows []PromotionRow, reasons map[string]int) PromotionCounts {
	counts := PromotionCounts{Total: len(rows)}
	for _, row := range rows {
		switch row.Action {
		case ActionPromote:
			counts.Promoted++
		case ActionRepeat:
			counts.Repeated++
		case ActionComplete:
			counts.Completed++
		default:
			counts.Skipped++
			reasons[reasonKey(row.Reason)]++
		}
	}
	return counts
}

func countPromotionOutcome(rows []PromotionRow, reasons map[string]int) PromotionCounts {
	counts := PromotionCounts{Total: len(rows)}
	clear(reasons)
	for _, row := range rows {
		switch {
		case row.Action == ActionSkip:
			counts.Skipped++
			reasons[reasonKey(row.Reason)]++
		case !row.Applied:
			counts.Failed++
			reasons[reasonKey(row.ErrorCode)]++
		case row.Action == ActionPromote:
			counts.Promoted++
		case row.Action == ActionRepeat:
			counts.Repeated++
		case row.Action == ActionComplete:
			counts.Completed++
		}
	}
	return counts
}

// promotionPlanHash fingerprints exactly the decisions an officer approves.
//
// The identifiers of the source enrollments are included, so a cohort that
// gained or lost a student changes the hash; so are the derived stage, attempt
// and category, so a result recorded after the preview changes it too.
func promotionPlanHash(in PromoteBulkInput, rows []PromotionRow) string {
	h := sha256.New()
	writeHashField(h, "source", in.SourceYearID.String())
	writeHashField(h, "target", in.TargetYearID.String())
	for _, row := range rows {
		writeHashField(h, "row", strings.Join([]string{
			row.SourceEnrollmentID.String(),
			string(row.Action),
			row.StudyTypeID.String(),
			strconv.Itoa(int(row.ToStage)),
			strconv.Itoa(int(row.AttemptNumber)),
			row.CategoryCode,
			reasonKey(row.Reason),
		}, "|"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// skippedPromotionIDs names the rows a run did not apply.
//
// A preview counts only the deliberate skips: nothing has been applied yet, so
// treating every unapplied row as a casualty would report a clean plan of five
// hundred promotions as five hundred problems.
func skippedPromotionIDs(rows []PromotionRow, includeFailures bool) []string {
	var ids []string
	for _, row := range rows {
		if row.Action == ActionSkip || (includeFailures && !row.Applied) {
			ids = append(ids, row.SourceEnrollmentID.String())
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// Bulk account generation
// ---------------------------------------------------------------------------

// AccountBulkOutcome is what happened to one enrollment in a pricing run.
type AccountBulkOutcome string

const (
	// AccountWillCreate is a dry-run row that would be priced.
	AccountWillCreate AccountBulkOutcome = "will_create"
	// AccountCreated was priced and frozen.
	AccountCreated AccountBulkOutcome = "created"
	// AccountAlreadyExists already had an account; nothing was done.
	AccountAlreadyExists AccountBulkOutcome = "already_has_account"
	// AccountBlocked could not be priced, and the reason says why — most often
	// no fee policy resolves for the scope.
	AccountBlocked AccountBulkOutcome = "blocked"
	// AccountSkipped was deliberately not applied.
	AccountSkipped AccountBulkOutcome = "skipped"
	// AccountFailed was attempted and the command refused it.
	AccountFailed AccountBulkOutcome = "failed"
)

// Skip reasons a pricing run can state.
const (
	// SkipChangedSincePreview is the whole point of the preview hash: the
	// outcome this row would produce now is not the outcome that was approved.
	SkipChangedSincePreview = "changed_since_preview"
	// SkipNotPreviewed is a row inside the scope that the approval did not
	// cover, which happens when the cohort grew between preview and commit.
	SkipNotPreviewed = "not_previewed"
)

// errPreviewChanged unwinds a row whose recomputed outcome disagrees with what
// was approved. It travels as an error because that is what rolls the row's
// savepoint back — the account is generated, compared, and discarded.
var errPreviewChanged = errors.New("account preview outcome changed since approval")

// AccountApproval is one row of a dry run, signed off.
type AccountApproval struct {
	EnrollmentID shared.ID
	PreviewHash  string
}

// GenerateAccountsBulkInput selects the enrollments to price.
type GenerateAccountsBulkInput struct {
	AcademicYearID shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	StudyTypeID    *shared.ID
	Stage          *int16

	// DryRun resolves every price and writes nothing.
	DryRun bool
	// Approved carries the dry run's per-row hashes back. A commit without any
	// is refused; a row missing from it is skipped rather than priced on an
	// authority nobody gave.
	Approved []AccountApproval
}

// AccountPlanInstallment is one line of a previewed payment schedule.
type AccountPlanInstallment struct {
	Number  int16        `json:"number"`
	DueDate string       `json:"due_date"`
	Amount  money.Amount `json:"amount"`
}

// AccountPlanRow is one enrollment's price, previewed or applied.
type AccountPlanRow struct {
	EnrollmentID shared.ID          `json:"enrollment_id"`
	StudentID    shared.ID          `json:"student_id"`
	Stage        int16              `json:"stage"`
	Outcome      AccountBulkOutcome `json:"outcome"`
	Reason       string             `json:"reason,omitempty"`
	ErrorCode    string             `json:"error_code,omitempty"`

	// The resolved policy is shown, never chosen: resolution is the source of
	// truth and a reviewer's job is to notice when it picked the wrong row.
	FeePolicyCode        string `json:"fee_policy_code,omitempty"`
	FeePolicySpecificity int32  `json:"fee_policy_specificity,omitempty"`

	Gross        money.Amount             `json:"gross_total"`
	Discount     money.Amount             `json:"discount_total"`
	Net          money.Amount             `json:"net_total"`
	Installments []AccountPlanInstallment `json:"installments,omitempty"`

	// PreviewHash fingerprints everything above. The commit recomputes it.
	PreviewHash string     `json:"preview_hash,omitempty"`
	AccountID   *shared.ID `json:"account_id,omitempty"`
}

// AccountBulkCounts is the tally the design asks for by name.
type AccountBulkCounts struct {
	Total             int `json:"total"`
	WillCreate        int `json:"will_create"`
	Created           int `json:"created"`
	AlreadyHasAccount int `json:"already_has_account"`
	Blocked           int `json:"blocked"`
	Skipped           int `json:"skipped"`
	Failed            int `json:"failed"`
}

// GenerateAccountsBulkResult is the preview, or the report.
type GenerateAccountsBulkResult struct {
	DryRun         bool              `json:"dry_run"`
	AcademicYearID shared.ID         `json:"academic_year_id"`
	Outcome        BulkOutcome       `json:"outcome"`
	Counts         AccountBulkCounts `json:"counts"`
	SkipReasons    map[string]int    `json:"skip_reasons,omitempty"`
	Rows           []AccountPlanRow  `json:"rows"`
}

// GenerateFinancialAccountsBulk prices a whole cohort.
//
// The dry run resolves each enrollment's fee policy, discounts, net and
// installment split, and fingerprints that outcome per row. The commit
// regenerates each row and compares the fingerprint before letting it stand: a
// row whose numbers moved since the preview is discarded with the reason
// changed_since_preview. Without that comparison the review is theatre — a fee
// policy published between the preview and the commit would be applied to five
// hundred students under an approval given for different numbers.
//
// Each row commits on its own. Pricing a cohort inside one transaction would
// hold the academic year's row lock for the length of the run, and every
// cashier at every window would block behind it.
func (s *BulkService) GenerateFinancialAccountsBulk(
	ctx context.Context, actor shared.Actor, in GenerateAccountsBulkInput,
) (*GenerateAccountsBulkResult, error) {
	if err := actor.RequireAnyRole("GenerateFinancialAccountsBulk",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if !in.DryRun && len(in.Approved) == 0 {
		return nil, shared.PreconditionFailed("account.bulk_dry_run_required",
			"run this generation as a dry run first and submit the per-row preview hashes it returns").
			WithDetail("remedy", "POST the same scope with dry_run=true, review will_create/already_has/blocked, then resubmit with the approved rows")
	}

	result := &GenerateAccountsBulkResult{
		DryRun:         in.DryRun,
		AcademicYearID: in.AcademicYearID,
		SkipReasons:    map[string]int{},
	}

	if in.DryRun {
		if err := s.deps.Tx.Read(ctx, func(ctx context.Context) error {
			rows, err := s.previewAccounts(ctx, actor, in)
			if err != nil {
				return err
			}
			result.Rows = rows
			return nil
		}); err != nil {
			return nil, err
		}
		result.Counts = countAccountRows(result.Rows, result.SkipReasons)
		result.Outcome = accountOutcome(result.Counts)
		if err := s.recordPreview(ctx, actor, "account.bulk_previewed", &in.AcademicYearID, map[string]any{
			"academic_year_id": in.AcademicYearID.String(),
			"counts":           snapshotOf(result.Counts),
			"skip_reasons":     result.SkipReasons,
		}); err != nil {
			return nil, err
		}
		s.logSkipped(ctx, "bulk account preview", result.SkipReasons, blockedAccountIDs(result.Rows))
		return result, nil
	}

	approved := make(map[shared.ID]string, len(in.Approved))
	for _, a := range in.Approved {
		approved[a.EnrollmentID] = a.PreviewHash
	}

	targets, err := s.collectAccountTargets(ctx, in)
	if err != nil {
		return nil, err
	}
	policyCodes := map[shared.ID]string{}
	for _, target := range targets {
		result.Rows = append(result.Rows, s.commitAccountRow(ctx, actor, target, approved, policyCodes))
	}

	result.Counts = countAccountRows(result.Rows, result.SkipReasons)
	result.Outcome = accountOutcome(result.Counts)

	if err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &in.AcademicYearID,
			Action:         "account.bulk_generated",
			Actor:          actor,
			After:          snapshotOf(result.Counts),
			AcademicYearID: &in.AcademicYearID,
			Metadata: map[string]any{
				"academic_year_id":    in.AcademicYearID.String(),
				"outcome":             string(result.Outcome),
				"created":             result.Counts.Created,
				"already_has_account": result.Counts.AlreadyHasAccount,
				"blocked":             result.Counts.Blocked,
				"skipped":             result.Counts.Skipped,
				"failed":              result.Counts.Failed,
				"skip_reasons":        result.SkipReasons,
			},
		})
	}); err != nil {
		return nil, err
	}

	s.logSkipped(ctx, "bulk account generation did not price every row", result.SkipReasons, blockedAccountIDs(result.Rows))
	return result, nil
}

// accountTarget is one enrollment in scope, with what the scan already knows
// about it.
type accountTarget struct {
	EnrollmentID shared.ID
	StudentID    shared.ID
	Stage        int16
	HasAccount   bool
}

// collectAccountTargets walks the scope once.
//
// The scope is deliberately the active cohort. A deferred student's charge is
// a decision taken when the deferral is recorded, not a side effect of pricing
// the year, and a withdrawn one has no seat to price.
func (s *BulkService) collectAccountTargets(
	ctx context.Context, in GenerateAccountsBulkInput,
) ([]accountTarget, error) {
	active := academic.StatusActive
	filter := port.EnrollmentFilter{
		AcademicYearID:    &in.AcademicYearID,
		CollegeID:         in.CollegeID,
		DepartmentID:      in.DepartmentID,
		StudyTypeID:       in.StudyTypeID,
		Stage:             in.Stage,
		Status:            &active,
		ExcludeSuperseded: true,
	}

	var targets []accountTarget
	err := s.eachEnrollment(ctx, filter, func(e *academic.Enrollment) error {
		target := accountTarget{EnrollmentID: e.ID, StudentID: e.StudentID, Stage: e.Stage}
		existing, err := s.deps.Accounts.GetByEnrollment(ctx, e.ID)
		switch {
		case err == nil && existing != nil:
			target.HasAccount = true
		case err != nil && !missingRecord(err):
			return err
		}
		targets = append(targets, target)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return targets, nil
}

// previewAccounts resolves each enrollment's price without writing.
func (s *BulkService) previewAccounts(
	ctx context.Context, actor shared.Actor, in GenerateAccountsBulkInput,
) ([]AccountPlanRow, error) {
	targets, err := s.collectAccountTargets(ctx, in)
	if err != nil {
		return nil, err
	}

	rows := make([]AccountPlanRow, 0, len(targets))
	policyCodes := map[shared.ID]string{}
	for _, target := range targets {
		row := AccountPlanRow{
			EnrollmentID: target.EnrollmentID,
			StudentID:    target.StudentID,
			Stage:        target.Stage,
		}
		if target.HasAccount {
			row.Outcome = AccountAlreadyExists
			row.Reason = "this enrollment was already priced"
			rows = append(rows, row)
			continue
		}

		// The dry run of the single-enrollment command is the pricing engine.
		// Recomputing it here would be a second implementation of fee
		// resolution, and the two would eventually disagree.
		preview, err := s.accounts.GenerateFinancialAccount(ctx, actor, GenerateAccountInput{
			EnrollmentID: target.EnrollmentID,
			DryRun:       true,
		})
		if err != nil {
			row.Outcome = AccountBlocked
			row.ErrorCode = shared.CodeOf(err)
			row.Reason = err.Error()
			rows = append(rows, row)
			continue
		}

		s.fillAccountRow(ctx, &row, preview, policyCodes)
		row.Outcome = AccountWillCreate
		rows = append(rows, row)
	}
	return rows, nil
}

// commitAccountRow prices one enrollment in its own transaction.
func (s *BulkService) commitAccountRow(
	ctx context.Context,
	actor shared.Actor,
	target accountTarget,
	approved map[shared.ID]string,
	policyCodes map[shared.ID]string,
) AccountPlanRow {
	row := AccountPlanRow{
		EnrollmentID: target.EnrollmentID,
		StudentID:    target.StudentID,
		Stage:        target.Stage,
	}

	if target.HasAccount {
		row.Outcome = AccountAlreadyExists
		row.Reason = "this enrollment was already priced"
		return row
	}

	approvedHash, ok := approved[target.EnrollmentID]
	if !ok {
		// In scope, but nobody approved it. Registered after the preview, most
		// likely — reported rather than priced, because the approval covers
		// what was shown and nothing else.
		row.Outcome = AccountSkipped
		row.Reason = SkipNotPreviewed
		return row
	}

	var (
		generated *GenerateAccountResult
		actual    string
	)
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		result, err := s.accounts.GenerateFinancialAccount(ctx, actor, GenerateAccountInput{
			EnrollmentID: target.EnrollmentID,
		})
		if err != nil {
			return err
		}
		actual = accountPreviewHash(target.EnrollmentID, result)
		if actual != approvedHash {
			// Generated, compared, and thrown away. Rolling the row back is
			// what keeps an unapproved price out of the ledger.
			return errPreviewChanged
		}
		generated = result
		return nil
	})

	switch {
	case errors.Is(err, errPreviewChanged):
		row.Outcome = AccountSkipped
		row.Reason = SkipChangedSincePreview
		row.PreviewHash = actual
		return row
	case err != nil:
		row.Outcome = AccountFailed
		row.ErrorCode = shared.CodeOf(err)
		row.Reason = err.Error()
		return row
	}

	s.fillAccountRow(ctx, &row, generated, policyCodes)
	row.Outcome = AccountCreated
	row.AccountID = &generated.Account.ID
	return row
}

// fillAccountRow copies a priced outcome onto a report row and fingerprints it.
//
// The policy's code is shown alongside its specificity because that pair is
// what a reviewer checks: a whole department priced from a university-wide
// wildcard row usually means the specific policy was never published.
func (s *BulkService) fillAccountRow(
	ctx context.Context, row *AccountPlanRow, result *GenerateAccountResult, policyCodes map[shared.ID]string,
) {
	row.Gross = result.Account.GrossTotal
	row.Discount = result.Account.DiscountTotal
	row.Net = result.Account.NetTotal
	if result.Account.FeePolicySpecificity != nil {
		row.FeePolicySpecificity = *result.Account.FeePolicySpecificity
	}
	if result.Account.FeePolicyID != nil {
		row.FeePolicyCode = s.policyCode(ctx, *result.Account.FeePolicyID, policyCodes)
	}
	for _, installment := range result.Installments {
		row.Installments = append(row.Installments, AccountPlanInstallment{
			Number:  installment.Number,
			DueDate: installment.DueDate.String(),
			Amount:  installment.Amount,
		})
	}
	row.PreviewHash = accountPreviewHash(row.EnrollmentID, result)
}

// policyCode resolves a policy's human-readable code once per run.
//
// A cohort resolves to a handful of distinct policies, so caching turns five
// hundred lookups into three. A lookup failure yields an empty code rather
// than an error: the price is already computed and correct, and a missing
// label is not a reason to refuse the row.
func (s *BulkService) policyCode(ctx context.Context, policyID shared.ID, cache map[shared.ID]string) string {
	if code, ok := cache[policyID]; ok {
		return code
	}
	policy, err := s.deps.FeePolicies.GetByID(ctx, policyID)
	if err != nil {
		cache[policyID] = ""
		return ""
	}
	cache[policyID] = policy.PolicyCode
	return policy.PolicyCode
}

// accountPreviewHash fingerprints the outcome a reviewer approves.
//
// Identifiers minted at generation — the account's own id, each installment's
// id — are deliberately excluded: they differ between a preview and a commit
// by design, and including them would make every row look changed. What is
// included is everything a student would notice: the policy, the gross, the
// discounts, the net, and each due date and amount.
func accountPreviewHash(enrollmentID shared.ID, result *GenerateAccountResult) string {
	h := sha256.New()
	writeHashField(h, "enrollment", enrollmentID.String())
	if result.Account.FeePolicyID != nil {
		writeHashField(h, "policy", result.Account.FeePolicyID.String())
	}
	if result.Account.FeePolicySpecificity != nil {
		writeHashField(h, "specificity", strconv.Itoa(int(*result.Account.FeePolicySpecificity)))
	}
	writeHashField(h, "gross", result.Account.GrossTotal.String())
	writeHashField(h, "discountable", result.Account.DiscountableBase.String())
	writeHashField(h, "discount", result.Account.DiscountTotal.String())
	writeHashField(h, "net", result.Account.NetTotal.String())
	for _, line := range result.Snapshot {
		writeHashField(h, "component", line.ComponentCode+"="+line.Amount.String())
	}
	for _, installment := range result.Installments {
		writeHashField(h, "installment", strconv.Itoa(int(installment.Number))+
			"@"+installment.DueDate.String()+"="+installment.Amount.String())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func countAccountRows(rows []AccountPlanRow, reasons map[string]int) AccountBulkCounts {
	counts := AccountBulkCounts{Total: len(rows)}
	for _, row := range rows {
		switch row.Outcome {
		case AccountWillCreate:
			counts.WillCreate++
		case AccountCreated:
			counts.Created++
		case AccountAlreadyExists:
			counts.AlreadyHasAccount++
		case AccountBlocked:
			counts.Blocked++
			reasons[reasonKey(row.ErrorCode)]++
		case AccountSkipped:
			counts.Skipped++
			reasons[reasonKey(row.Reason)]++
		case AccountFailed:
			counts.Failed++
			reasons[reasonKey(row.ErrorCode)]++
		}
	}
	return counts
}

func accountOutcome(counts AccountBulkCounts) BulkOutcome {
	priced := counts.Created + counts.WillCreate + counts.AlreadyHasAccount
	switch {
	case counts.Total == 0:
		return BulkCompleted
	// Only an error condition on every row is a failure. A run that skipped
	// each row for a stated reason — nothing approved, every price changed —
	// did exactly what it was asked to; calling that "failed" would hide the
	// reasons behind a word that invites a retry instead of a look.
	case priced == 0 && counts.Skipped == 0:
		return BulkFailed
	case counts.Blocked > 0 || counts.Skipped > 0 || counts.Failed > 0:
		return BulkCompletedWithSkips
	default:
		return BulkCompleted
	}
}

func blockedAccountIDs(rows []AccountPlanRow) []string {
	var ids []string
	for _, row := range rows {
		switch row.Outcome {
		case AccountBlocked, AccountSkipped, AccountFailed:
			ids = append(ids, row.EnrollmentID.String())
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// eachEnrollment walks a filtered cohort page by page.
func (s *BulkService) eachEnrollment(
	ctx context.Context, filter port.EnrollmentFilter, fn func(*academic.Enrollment) error,
) error {
	offset := 0
	for {
		filter.Limit = bulkPageSize
		filter.Offset = offset

		page, total, err := s.deps.Enrollments.List(ctx, filter)
		if err != nil {
			return err
		}
		if total > maxBulkRows {
			return shared.Validation("bulk.scope_too_large",
				"this scope matches %d enrollments, more than the %d a single run accepts; narrow it by college or department",
				total, maxBulkRows).
				WithDetail("matched", total).
				WithDetail("limit", maxBulkRows)
		}
		for _, e := range page {
			if err := fn(e); err != nil {
				return err
			}
		}

		offset += len(page)
		if len(page) == 0 || offset >= total {
			return nil
		}
	}
}

// recordPreview audits a dry run in a transaction of its own.
//
// The preview itself runs read-only, so it cannot write its own audit entry —
// and the entry is worth having: it is the record of which numbers a manager
// was shown before they approved them.
func (s *BulkService) recordPreview(
	ctx context.Context, actor shared.Actor, action string, yearID *shared.ID, metadata map[string]any,
) error {
	return s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       yearID,
			Action:         action,
			Actor:          actor,
			AcademicYearID: yearID,
			Metadata:       metadata,
		})
	})
}

// logSkipped writes what a run did not do.
//
// Counts alone are not enough to act on: an operator chasing eleven missing
// students needs their identifiers, and a log line that says only "11 skipped"
// sends them back to the database to work out which eleven.
func (s *BulkService) logSkipped(ctx context.Context, message string, reasons map[string]int, ids []string) {
	if len(reasons) == 0 && len(ids) == 0 {
		return
	}
	named := ids
	truncated := 0
	if len(named) > maxLoggedRows {
		truncated = len(named) - maxLoggedRows
		named = named[:maxLoggedRows]
	}
	s.logger().WarnContext(ctx, message,
		slog.Any("reasons", reasons),
		slog.Int("affected_rows", len(ids)),
		slog.Any("affected", named),
		slog.Int("not_listed", truncated),
	)
}

func (s *BulkService) logger() *slog.Logger {
	if s.deps.Log == nil {
		return slog.Default()
	}
	return s.deps.Log
}

// reasonKey trims a reason down to its stable prefix, so a summary groups
// "prior_debt_blocks: 250,000 outstanding" with "prior_debt_blocks: 90,000"
// instead of listing every amount as its own category.
func reasonKey(reason string) string {
	if reason == "" {
		return "unspecified"
	}
	if idx := strings.Index(reason, ":"); idx > 0 {
		return reason[:idx]
	}
	return reason
}

// writeHashField feeds one delimited field into a hash. The separators are
// control characters that cannot occur in an identifier or an amount, so two
// different field splits cannot collide into the same digest.
func writeHashField(w io.Writer, name, value string) {
	_, _ = io.WriteString(w, name)
	_, _ = w.Write([]byte{0x1f})
	_, _ = io.WriteString(w, value)
	_, _ = w.Write([]byte{0x1e})
}

// missingRecord reports whether an error means "no such row", the outcome
// several lookups here treat as ordinary rather than exceptional.
func missingRecord(err error) bool {
	return err != nil && shared.KindOf(err) == shared.KindNotFound
}
