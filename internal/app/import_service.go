package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/shared"
	"flowed/internal/domain/student"
	"flowed/internal/port"
)

// ImportService runs the staged student import.
//
// The workflow is uploaded → validating → needs_review → confirmed →
// importing → imported | completed_with_skips, with cancelled available until
// the run starts. Every step exists because the alternative — reading a
// spreadsheet straight into the database — is how one mistyped department name
// becomes four hundred students in a department that does not exist.
//
// This service never generates a financial account. Pricing a student is a
// separate, explicit command with its own authority and its own dry run: an
// import that quietly created accounts would put half a million dinars of
// obligation on the books as a side effect of a file upload.
type ImportService struct {
	deps        Deps
	imports     port.ImportRepository
	students    *StudentService
	enrollments *EnrollmentService
	auditor
}

// NewImportService wires the staged import commands.
func NewImportService(
	d Deps, imports port.ImportRepository, students *StudentService, enrollments *EnrollmentService,
) *ImportService {
	return &ImportService{
		deps:        d,
		imports:     imports,
		students:    students,
		enrollments: enrollments,
		auditor:     newAuditor(d.Audit, d.Clock),
	}
}

// Columns a student import row carries. The HTTP layer maps a spreadsheet
// header onto these; nothing below reads a column by any other name.
const (
	ColStudentNo      = "student_no"
	ColFullName       = "full_name"
	ColMotherName     = "mother_name"
	ColPhone          = "phone"
	ColBirthDate      = "birth_date"
	ColGender         = "gender"
	ColDepartmentCode = "department_code"
	ColStudyTypeCode  = "study_type_code"
	ColStage          = "stage"
)

// StudentImportColumns is the expected header of a student spreadsheet, in
// order.
var StudentImportColumns = []string{
	ColStudentNo, ColFullName, ColMotherName, ColPhone,
	ColBirthDate, ColGender, ColDepartmentCode, ColStudyTypeCode, ColStage,
}

// Validation finding codes. They are stable strings because a review screen
// groups by them and a reviewer's override is justified against one.
const (
	FindingMissingField      = "missing_required_field"
	FindingDuplicateInFile   = "duplicate_student_no_in_file"
	FindingIdentityMismatch  = "identity_mismatch"
	FindingUnknownStudent    = "unknown_student"
	FindingUnknownDepartment = "unknown_department"
	FindingUnknownStudyType  = "unknown_study_type"
	FindingInvalidStage      = "invalid_stage"
	FindingInvalidBirthDate  = "invalid_birth_date"
	FindingInvalidGender     = "invalid_gender"
	FindingEnrollmentExists  = "enrollment_already_exists"
	FindingEnrollmentConflct = "enrollment_conflict"
	FindingEnrollOnly        = "existing_student_enroll_only"
	FindingProbableDuplicate = "probable_duplicate_person"
)

const (
	// importRunPageSize bounds how many queued rows are pulled at a time.
	importRunPageSize = 200
	// heartbeatEvery is how often the run announces it is still alive. Often
	// enough that the reaper's silence threshold can be short, rarely enough
	// that it is not a write per row.
	heartbeatEvery = 25
	// maxStagedRows refuses a file that is certainly a mistake rather than a
	// cohort.
	maxStagedRows = 50000
)

// ---------------------------------------------------------------------------
// Stage
// ---------------------------------------------------------------------------

// StageImportInput carries a parsed spreadsheet.
type StageImportInput struct {
	BatchType string
	Filename  string
	// AcademicYearID is the year the rows enroll into. Required for the batch
	// types that create registrations, which is all of the ones supported here.
	AcademicYearID *shared.ID
	Rows           []map[string]any
}

// StageImportResult reports what was staged.
type StageImportResult struct {
	Batch  *port.ImportBatch
	Staged int
	// DuplicateRowsDropped counts lines identical to one already in the batch.
	// It is returned rather than swallowed: "loaded 500 rows" over a file whose
	// last thirty lines were a copy-paste of the first thirty is a false
	// receipt.
	DuplicateRowsDropped int
}

// StageImport persists parsed rows for review. Nothing is created yet.
func (s *ImportService) StageImport(
	ctx context.Context, actor shared.Actor, in StageImportInput,
) (*StageImportResult, error) {
	if err := supportedBatchType(in.BatchType); err != nil {
		return nil, err
	}
	if len(in.Rows) == 0 {
		return nil, shared.Validation("import.no_rows", "the file contains no data rows")
	}
	if len(in.Rows) > maxStagedRows {
		return nil, shared.Validation("import.too_many_rows",
			"this file has %d rows, more than the %d a single batch accepts; split it",
			len(in.Rows), maxStagedRows)
	}
	if in.AcademicYearID == nil {
		return nil, shared.Validation("import.academic_year_required",
			"a %s import must name the academic year its rows enroll into", in.BatchType)
	}

	result := &StageImportResult{}
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		year, err := s.deps.Years.GetByID(ctx, *in.AcademicYearID)
		if err != nil {
			return err
		}
		if !year.AcceptsEnrollment() {
			return shared.PreconditionFailed("import.year_not_open",
				"academic year %s is %s and is not accepting registrations", year.Code, year.Status)
		}

		batch := &port.ImportBatch{
			ID:             shared.NewID(),
			BatchType:      in.BatchType,
			SourceFilename: nullableText(in.Filename),
			Status:         port.BatchUploaded,
			AcademicYearID: in.AcademicYearID,
			TotalRows:      len(in.Rows),
			CreatedBy:      idOrNilActor(actor),
		}
		if err := s.imports.CreateBatch(ctx, batch); err != nil {
			return err
		}

		rows := make([]*port.ImportRow, 0, len(in.Rows))
		for i, raw := range in.Rows {
			rows = append(rows, &port.ImportRow{
				ID:               shared.NewID(),
				BatchID:          batch.ID,
				RowNo:            i + 1,
				RawData:          raw,
				DedupHash:        studentRowDedupHash(raw),
				ValidationStatus: port.RowPending,
				Disposition:      port.DispositionCreate,
			})
		}

		inserted, err := s.imports.InsertRows(ctx, rows)
		if err != nil {
			return err
		}

		// total_rows records what was actually staged, so the review screen's
		// denominator matches the rows it can show.
		batch.TotalRows = inserted
		if err := s.imports.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		result.Batch = batch
		result.Staged = inserted
		result.DuplicateRowsDropped = len(in.Rows) - inserted

		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_batch",
			EntityID:       &batch.ID,
			Action:         "import.staged",
			Actor:          actor,
			After:          snapshotOf(batch),
			AcademicYearID: in.AcademicYearID,
			Metadata: map[string]any{
				"batch_type":      in.BatchType,
				"source_filename": in.Filename,
				"rows_submitted":  len(in.Rows),
				"rows_staged":     inserted,
				"rows_duplicate":  len(in.Rows) - inserted,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	if result.DuplicateRowsDropped > 0 {
		s.logger().WarnContext(ctx, "import dropped rows identical to ones already staged",
			slog.String("batch_id", result.Batch.ID.String()),
			slog.Int("dropped", result.DuplicateRowsDropped),
			slog.Int("staged", result.Staged))
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

// ValidateImport checks every row and writes its findings and disposition.
//
// The disposition it writes is a default, not a verdict. A reviewer may
// override any of them, which is the whole reason validation and import are
// separate steps.
func (s *ImportService) ValidateImport(
	ctx context.Context, actor shared.Actor, batchID shared.ID,
) (*port.ImportBatch, error) {
	var batch *port.ImportBatch
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		batch, err = s.imports.GetBatchForUpdate(ctx, batchID)
		if err != nil {
			return err
		}
		switch batch.Status {
		case port.BatchUploaded, port.BatchValidating, port.BatchNeedsReview:
		default:
			return importStateError("validating", batch,
				port.BatchUploaded, port.BatchValidating, port.BatchNeedsReview)
		}
		if batch.AcademicYearID == nil {
			return shared.InvariantViolation("import.batch_without_year",
				"batch %s has no academic year and cannot be validated", batch.ID)
		}

		batch.Status = port.BatchValidating
		if err := s.imports.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		vc, err := s.loadValidationContext(ctx, batch)
		if err != nil {
			return err
		}

		offset := 0
		for {
			rows, total, err := s.imports.ListRows(ctx, batch.ID, port.ImportRowFilter{
				Limit:  importRunPageSize,
				Offset: offset,
			})
			if err != nil {
				return err
			}
			for _, row := range rows {
				s.validateRow(ctx, batch, row, vc)
				if err := s.imports.UpdateRow(ctx, row); err != nil {
					return err
				}
			}
			offset += len(rows)
			if len(rows) == 0 || offset >= total {
				break
			}
		}

		counts, err := s.imports.CountRows(ctx, batch.ID)
		if err != nil {
			return err
		}
		batch.TotalRows = counts.Total
		batch.ValidRows = counts.Valid + counts.Warning
		batch.ErrorRows = counts.Error
		batch.Status = port.BatchNeedsReview
		if err := s.imports.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_batch",
			EntityID:       &batch.ID,
			Action:         "import.validated",
			Actor:          actor,
			After:          snapshotOf(batch),
			AcademicYearID: batch.AcademicYearID,
			Metadata: map[string]any{
				"total_rows":        counts.Total,
				"valid_rows":        counts.Valid,
				"warning_rows":      counts.Warning,
				"error_rows":        counts.Error,
				"unresolved_errors": counts.UnresolvedErrors,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// validationContext is what every row is checked against, loaded once per
// pass rather than per row.
type validationContext struct {
	yearID shared.ID
	seats  *seatIndex
	// occurrences counts each student number across the whole file, which is
	// how a number duplicated inside one spreadsheet is caught before any of
	// its rows are applied.
	occurrences map[string]int
}

func (s *ImportService) loadValidationContext(
	ctx context.Context, batch *port.ImportBatch,
) (*validationContext, error) {
	seats, err := s.loadSeatIndex(ctx)
	if err != nil {
		return nil, err
	}
	occurrences, err := s.imports.StudentNumberOccurrences(ctx, batch.ID)
	if err != nil {
		return nil, err
	}
	return &validationContext{
		yearID:      *batch.AcademicYearID,
		seats:       seats,
		occurrences: occurrences,
	}, nil
}

// validateRow applies the duplicate-handling matrix from the domain design.
//
//	duplicate student_no within the file          → error
//	student_no exists, name and mother match      → update: enroll only
//	student_no exists, name or mother differ      → error, needs a confirmation
//	unknown department / study type / stage       → error
//	enrollment already exists for (student, year) → skip
//	enrollment exists but in a different seat     → error
//
// The last line is the one worth defending: an import that reconciled a
// conflicting enrollment would be performing a supersede, and a supersede
// moves money between accounts. That is a deliberate command a registrar
// issues with a reason attached, never a side effect of a spreadsheet.
func (s *ImportService) validateRow(
	ctx context.Context, batch *port.ImportBatch, row *port.ImportRow, vc *validationContext,
) {
	row.Errors = nil
	row.Warnings = nil
	row.Disposition = port.DispositionCreate

	studentNo := rowText(row.RawData, ColStudentNo)
	fullName := collapseSpaces(rowText(row.RawData, ColFullName))
	motherName := collapseSpaces(rowText(row.RawData, ColMotherName))

	if studentNo == "" {
		addError(row, ColStudentNo, FindingMissingField, "the university number is required")
	}
	if fullName == "" {
		addError(row, ColFullName, FindingMissingField, "the student's full name is required")
	}
	if motherName == "" {
		// Not a nicety: it is the field that tells two students with identical
		// four-part names apart at the cashier's window.
		addError(row, ColMotherName, FindingMissingField, "the mother's name is required")
	}

	if studentNo != "" && vc.occurrences[studentNo] > 1 {
		addError(row, ColStudentNo, FindingDuplicateInFile,
			fmt.Sprintf("university number %s appears %d times in this file", studentNo, vc.occurrences[studentNo]))
	}

	department := vc.seats.departments[strings.ToUpper(rowText(row.RawData, ColDepartmentCode))]
	if department == nil {
		addError(row, ColDepartmentCode, FindingUnknownDepartment,
			fmt.Sprintf("no active department has the code %q", rowText(row.RawData, ColDepartmentCode)))
	}
	if vc.seats.studyTypes[strings.ToUpper(rowText(row.RawData, ColStudyTypeCode))] == nil {
		addError(row, ColStudyTypeCode, FindingUnknownStudyType,
			fmt.Sprintf("no active study type has the code %q", rowText(row.RawData, ColStudyTypeCode)))
	}

	stage, stageErr := parseStage(rowText(row.RawData, ColStage))
	switch {
	case stageErr != nil:
		addError(row, ColStage, FindingInvalidStage, stageErr.Error())
	case department != nil && stage > department.StageCount:
		addError(row, ColStage, FindingInvalidStage,
			fmt.Sprintf("stage %d does not exist in %s, which runs %d stages",
				stage, department.Code, department.StageCount))
	}

	if raw := rowText(row.RawData, ColBirthDate); raw != "" {
		if _, err := shared.ParseDate(raw); err != nil {
			addError(row, ColBirthDate, FindingInvalidBirthDate,
				fmt.Sprintf("%q is not a date in YYYY-MM-DD form", raw))
		}
	}
	if raw := rowText(row.RawData, ColGender); raw != "" {
		if g := student.Gender(strings.ToLower(raw)); g != student.GenderMale && g != student.GenderFemale {
			addError(row, ColGender, FindingInvalidGender, fmt.Sprintf("%q is neither male nor female", raw))
		}
	}

	if studentNo != "" {
		s.matchExistingPerson(ctx, batch, row, vc, studentNo, fullName, motherName, department, stage)
	}

	switch {
	case row.HasErrors():
		row.ValidationStatus = port.RowError
		row.Disposition = port.DispositionError
	case len(row.Warnings) > 0:
		row.ValidationStatus = port.RowWarning
	default:
		row.ValidationStatus = port.RowValid
	}
}

// matchExistingPerson resolves the row against who is already on file.
func (s *ImportService) matchExistingPerson(
	ctx context.Context,
	batch *port.ImportBatch,
	row *port.ImportRow,
	vc *validationContext,
	studentNo, fullName, motherName string,
	department *academic.Department,
	stage int16,
) {
	existing, err := s.deps.Students.GetByStudentNo(ctx, studentNo)
	if err != nil && !missingRecord(err) {
		addError(row, ColStudentNo, "lookup_failed", err.Error())
		return
	}

	if existing == nil {
		if batch.BatchType == port.BatchTypeEnrollments {
			// An enrollment load registers people who are already on file. It
			// must not invent a person from a row that was meant to match one.
			addError(row, ColStudentNo, FindingUnknownStudent,
				fmt.Sprintf("no student holds the number %s; a %s batch does not create people",
					studentNo, batch.BatchType))
			return
		}
		row.Disposition = port.DispositionCreate
		// A different number but the same name and mother is the shape of a
		// person entered twice. It is a warning rather than a block: the
		// reviewer sees it, and confirming the batch is the acknowledgement.
		if fullName != "" && motherName != "" {
			duplicates, err := s.deps.Students.FindPossibleDuplicates(ctx, fullName, motherName, nil)
			if err == nil && len(duplicates) > 0 {
				numbers := make([]string, 0, len(duplicates))
				for _, d := range duplicates {
					numbers = append(numbers, d.StudentNo)
				}
				addWarning(row, ColFullName, FindingProbableDuplicate,
					fmt.Sprintf("%d student(s) already share this name and mother's name: %s",
						len(duplicates), strings.Join(numbers, ", ")))
			}
		}
		return
	}

	row.MatchedEntityID = &existing.ID

	// The comparison is literal on purpose. A spelling variant — فاطمه against
	// فاطمة — lands in the error bucket where a human decides, rather than
	// being silently treated as the same person and enrolled against someone
	// else's record.
	if !strings.EqualFold(collapseSpaces(existing.FullName), fullName) ||
		!strings.EqualFold(collapseSpaces(existing.MotherName), motherName) {
		addError(row, ColFullName, FindingIdentityMismatch,
			fmt.Sprintf("number %s belongs to %s (mother %s); confirm this is the same person or issue a new number",
				studentNo, existing.FullName, existing.MotherName))
		return
	}

	// Same person, already on file. The row enrolls them and leaves their
	// identity alone: a legal name change is its own documented command.
	row.Disposition = port.DispositionUpdate
	addWarning(row, ColStudentNo, FindingEnrollOnly,
		fmt.Sprintf("student %s already exists; this row will only add the enrollment", studentNo))

	live, err := s.deps.Enrollments.GetLive(ctx, existing.ID, vc.yearID)
	if err != nil && !missingRecord(err) {
		addError(row, "", "lookup_failed", err.Error())
		return
	}
	if live == nil {
		return
	}

	sameSeat := department != nil && live.DepartmentID == department.ID && live.Stage == stage
	if sameSeat {
		row.Disposition = port.DispositionSkip
		addWarning(row, "", FindingEnrollmentExists,
			fmt.Sprintf("student %s is already enrolled for this year in the same seat", studentNo))
		return
	}
	addError(row, "", FindingEnrollmentConflct,
		fmt.Sprintf("student %s is already enrolled this year in a different department or stage; "+
			"moving them is a supersede, which an import must not perform", studentNo))
}

// ---------------------------------------------------------------------------
// Review and reviewer overrides
// ---------------------------------------------------------------------------

// ImportReview is a batch with the rows a human has to look at.
type ImportReview struct {
	Batch  *port.ImportBatch    `json:"batch"`
	Counts port.ImportRowCounts `json:"counts"`
	Rows   []*port.ImportRow    `json:"rows"`
	Total  int                  `json:"total"`
}

// ReviewImport returns a batch and a page of its rows with their findings.
func (s *ImportService) ReviewImport(
	ctx context.Context, actor shared.Actor, batchID shared.ID, filter port.ImportRowFilter,
) (*ImportReview, error) {
	review := &ImportReview{}
	// One read transaction, so the counts and the rows on screen describe the
	// same instant rather than two states a validation pass apart.
	err := s.deps.Tx.Read(ctx, func(ctx context.Context) error {
		batch, err := s.imports.GetBatch(ctx, batchID)
		if err != nil {
			return err
		}
		counts, err := s.imports.CountRows(ctx, batchID)
		if err != nil {
			return err
		}
		rows, total, err := s.imports.ListRows(ctx, batchID, filter)
		if err != nil {
			return err
		}
		review.Batch = batch
		review.Counts = counts
		review.Rows = rows
		review.Total = total
		return nil
	})
	if err != nil {
		return nil, err
	}
	return review, nil
}

// SetDispositionInput is a reviewer's decision on one row.
type SetDispositionInput struct {
	BatchID     shared.ID
	RowNo       int
	Disposition port.ImportDisposition
	Reason      string
}

// SetRowDisposition records a reviewer's override of one row.
//
// Overriding a row that validation rejected requires a written reason. That is
// the audited confirm-same-person the duplicate matrix calls for: somebody puts
// their name against "this really is the same student" before the import acts
// on it.
func (s *ImportService) SetRowDisposition(
	ctx context.Context, actor shared.Actor, in SetDispositionInput,
) (*port.ImportRow, error) {
	switch in.Disposition {
	case port.DispositionCreate, port.DispositionUpdate, port.DispositionSkip, port.DispositionError:
	default:
		return nil, shared.Validation("import.invalid_disposition",
			"%q is not a disposition; use create, update, skip or error", in.Disposition)
	}

	var row *port.ImportRow
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		batch, err := s.imports.GetBatchForUpdate(ctx, in.BatchID)
		if err != nil {
			return err
		}
		if batch.Status != port.BatchNeedsReview {
			return importStateError("changing a row's disposition", batch, port.BatchNeedsReview)
		}

		row, err = s.imports.GetRow(ctx, in.BatchID, in.RowNo)
		if err != nil {
			return err
		}
		if row.HasErrors() && in.Disposition != port.DispositionError && strings.TrimSpace(in.Reason) == "" {
			return shared.Validation("import.override_reason_required",
				"row %d was rejected by validation; overriding it requires a written reason", in.RowNo).
				WithDetail("row_no", in.RowNo).
				WithDetail("findings", row.Errors)
		}

		before := snapshotOf(row)
		row.Disposition = in.Disposition
		if in.Reason != "" {
			row.ErrorMessage = ptr("reviewer override: " + in.Reason)
		}
		if err := s.imports.UpdateRow(ctx, row); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_row",
			EntityID:       &row.ID,
			Action:         "import.row_dispositioned",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(row),
			AcademicYearID: batch.AcademicYearID,
			Reason:         nullableText(in.Reason),
			Metadata: map[string]any{
				"batch_id":    batch.ID.String(),
				"row_no":      row.RowNo,
				"disposition": string(in.Disposition),
				"had_errors":  row.HasErrors(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return row, nil
}

// ---------------------------------------------------------------------------
// Confirm and cancel
// ---------------------------------------------------------------------------

// ConfirmImport approves a reviewed batch for execution.
//
// It is reachable only with zero unresolved error rows. An error row is
// resolved by giving it a workable disposition or by skipping it — either way
// a human decided, which is the point of the gate.
func (s *ImportService) ConfirmImport(
	ctx context.Context, actor shared.Actor, batchID shared.ID,
) (*port.ImportBatch, error) {
	var batch *port.ImportBatch
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		batch, err = s.imports.GetBatchForUpdate(ctx, batchID)
		if err != nil {
			return err
		}
		if batch.Status != port.BatchNeedsReview {
			return importStateError("confirming", batch, port.BatchNeedsReview)
		}

		counts, err := s.imports.CountRows(ctx, batchID)
		if err != nil {
			return err
		}
		if counts.UnresolvedErrors > 0 {
			return shared.PreconditionFailed("import.unresolved_errors",
				"%d row(s) still carry validation errors nobody has dispositioned; resolve or skip them first",
				counts.UnresolvedErrors).
				WithDetail("unresolved_errors", counts.UnresolvedErrors).
				WithDetail("remedy", "set each failing row's disposition to skip, or fix the source data and re-validate")
		}

		now := nowOr(s.deps.Clock)
		batch.Status = port.BatchConfirmed
		batch.ConfirmedBy = idOrNilActor(actor)
		batch.ConfirmedAt = &now
		batch.ValidRows = counts.Valid + counts.Warning
		batch.ErrorRows = counts.Error
		if err := s.imports.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_batch",
			EntityID:       &batch.ID,
			Action:         "import.confirmed",
			Actor:          actor,
			After:          snapshotOf(batch),
			AcademicYearID: batch.AcademicYearID,
			Metadata: map[string]any{
				"rows_to_apply": counts.Valid + counts.Warning,
				"rows_error":    counts.Error,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// CancelImport abandons a batch before it runs.
func (s *ImportService) CancelImport(
	ctx context.Context, actor shared.Actor, batchID shared.ID, reason string,
) (*port.ImportBatch, error) {
	var batch *port.ImportBatch
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		batch, err = s.imports.GetBatchForUpdate(ctx, batchID)
		if err != nil {
			return err
		}
		switch batch.Status {
		case port.BatchUploaded, port.BatchValidating, port.BatchNeedsReview, port.BatchConfirmed:
		default:
			// A running or finished batch is not cancellable: rows it already
			// applied are real students, and pretending otherwise would leave
			// them orphaned from any record of how they arrived.
			return importStateError("cancelling", batch,
				port.BatchUploaded, port.BatchValidating, port.BatchNeedsReview, port.BatchConfirmed)
		}

		now := nowOr(s.deps.Clock)
		batch.Status = port.BatchCancelled
		batch.CompletedAt = &now
		batch.ErrorSummary = nullableText(reason)
		if err := s.imports.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_batch",
			EntityID:       &batch.ID,
			Action:         "import.cancelled",
			Actor:          actor,
			After:          snapshotOf(batch),
			AcademicYearID: batch.AcademicYearID,
			Reason:         nullableText(reason),
		})
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// ---------------------------------------------------------------------------
// Run and resume
// ---------------------------------------------------------------------------

// ImportRunReport is what a run did.
type ImportRunReport struct {
	Batch  *port.ImportBatch    `json:"batch"`
	Counts port.ImportRowCounts `json:"counts"`
	// Remaining is non-zero when the run stopped before the queue emptied — a
	// cancelled context, usually. The batch stays in importing so a resume can
	// finish it.
	Remaining int `json:"remaining"`
	// FailureReasons groups the rows that were not applied, so a report reads
	// as "31 rows: 30 unknown department, 1 duplicate number" rather than as a
	// number with no explanation.
	FailureReasons map[string]int `json:"failure_reasons,omitempty"`
}

// RunImport applies a confirmed batch.
func (s *ImportService) RunImport(
	ctx context.Context, actor shared.Actor, batchID shared.ID,
) (*ImportRunReport, error) {
	return s.runQueue(ctx, actor, batchID, false)
}

// ResumeImport picks up a batch whose worker died.
//
// Only the rows still queued are attempted, and the queue is defined by the
// rows' own status, so a row already applied cannot be applied twice however
// often this is called. Without it a killed worker strands its batch in
// importing forever, with two hundred students created and three hundred
// waiting for a run nobody will ever start.
func (s *ImportService) ResumeImport(
	ctx context.Context, actor shared.Actor, batchID shared.ID,
) (*ImportRunReport, error) {
	return s.runQueue(ctx, actor, batchID, true)
}

// StalledImports lists batches whose worker stopped reporting, for a reaper.
func (s *ImportService) StalledImports(
	ctx context.Context, actor shared.Actor, silentFor time.Duration, limit int,
) ([]*port.ImportBatch, error) {
	if silentFor <= 0 {
		silentFor = 5 * time.Minute
	}
	return s.imports.StalledBatches(ctx, nowOr(s.deps.Clock).Add(-silentFor), limit)
}

// runQueue is the body of both RunImport and ResumeImport.
func (s *ImportService) runQueue(
	ctx context.Context, actor shared.Actor, batchID shared.ID, resuming bool,
) (*ImportRunReport, error) {
	batch, err := s.claimBatch(ctx, actor, batchID, resuming)
	if err != nil {
		return nil, err
	}

	// Reference data is read once for the run rather than per row: a cohort
	// resolves against the same handful of departments, and five hundred
	// identical lookups would dominate the import's cost.
	seats, err := s.loadSeatIndex(ctx)
	if err != nil {
		return nil, err
	}

	processed := 0
	attempted := map[int]bool{}
	for {
		// The queue is re-read each pass rather than paged with an offset: rows
		// leave the queue as they are applied, so a fixed offset would step over
		// the ones that failed and stayed behind. Rows already attempted in this
		// pass are remembered so a row that could not even be marked failed does
		// not spin the loop forever.
		rows, err := s.imports.UnprocessedRows(ctx, batch.ID, importRunPageSize)
		if err != nil {
			return nil, err
		}

		fresh := 0
		for _, row := range rows {
			if attempted[row.RowNo] {
				continue
			}
			if ctx.Err() != nil {
				// The caller went away. Everything committed so far stands, and
				// the batch is left in importing for a resume to finish.
				//
				// The finalisation runs on a detached context: the request's
				// cancellation must not also cost us the record of how far the
				// run got, which is the only thing telling an operator whether
				// two hundred students were created or none.
				return s.finaliseRun(context.WithoutCancel(ctx), actor, batch)
			}
			attempted[row.RowNo] = true
			fresh++
			s.processRow(ctx, actor, batch, row, seats)
			processed++
			if processed%heartbeatEvery == 0 {
				s.beat(ctx, batch.ID)
			}
		}
		if fresh == 0 {
			break
		}
	}

	return s.finaliseRun(ctx, actor, batch)
}

// seatIndex is the reference data a run resolves its rows against.
type seatIndex struct {
	departments map[string]*academic.Department
	studyTypes  map[string]*academic.StudyType
}

func (s *ImportService) loadSeatIndex(ctx context.Context) (*seatIndex, error) {
	departments, err := s.deps.Reference.ListDepartments(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	studyTypes, err := s.deps.Reference.ListStudyTypes(ctx, true)
	if err != nil {
		return nil, err
	}

	index := &seatIndex{
		departments: make(map[string]*academic.Department, len(departments)),
		studyTypes:  make(map[string]*academic.StudyType, len(studyTypes)),
	}
	for _, d := range departments {
		index.departments[strings.ToUpper(d.Code)] = d
	}
	for _, t := range studyTypes {
		index.studyTypes[strings.ToUpper(t.Code)] = t
	}
	return index, nil
}

// claimBatch moves a batch into importing under a row lock, which is what
// stops a reaper and a woken worker from running the same queue at once.
func (s *ImportService) claimBatch(
	ctx context.Context, actor shared.Actor, batchID shared.ID, resuming bool,
) (*port.ImportBatch, error) {
	var batch *port.ImportBatch
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		batch, err = s.imports.GetBatchForUpdate(ctx, batchID)
		if err != nil {
			return err
		}

		switch {
		case batch.Status == port.BatchConfirmed:
		case resuming && batch.Status == port.BatchImporting:
		default:
			if resuming {
				return importStateError("resuming", batch, port.BatchConfirmed, port.BatchImporting)
			}
			return importStateError("running", batch, port.BatchConfirmed)
		}

		now := nowOr(s.deps.Clock)
		batch.Status = port.BatchImporting
		if batch.StartedAt == nil {
			batch.StartedAt = &now
		}
		batch.HeartbeatAt = &now
		if err := s.imports.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		action := "import.run_started"
		if resuming {
			action = "import.run_resumed"
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_batch",
			EntityID:       &batch.ID,
			Action:         action,
			Actor:          actor,
			After:          snapshotOf(batch),
			AcademicYearID: batch.AcademicYearID,
		})
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// processRow applies one row in a transaction of its own.
//
// Per row, not per chunk. A poisoned row — a department retired between
// validation and import, a number that collided with one created a second ago
// — rolls back only itself; the 199 good rows around it are already committed
// and stay committed. Chunked commits would throw away up to a chunk's worth
// of correct work for one bad line, and the operator would have no way to know
// which of the discarded rows had actually been fine.
//
// The row's own status is written in the same transaction as the student and
// the enrollment it creates. That is what makes a resume safe: a crash between
// the two would otherwise leave a created student behind a row still marked
// queued, and the resume would create them again.
func (s *ImportService) processRow(
	ctx context.Context, actor shared.Actor, batch *port.ImportBatch, row *port.ImportRow, seats *seatIndex,
) {
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return s.applyRow(ctx, actor, batch, row, seats)
	})
	if err == nil {
		return
	}

	// The row's transaction rolled back, so its failure has to be recorded by
	// a second one. Written outside the failed unit of work on purpose: the
	// point of the record is to survive the rollback.
	markErr := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		fresh, getErr := s.imports.GetRow(ctx, batch.ID, row.RowNo)
		if getErr != nil {
			return getErr
		}
		now := nowOr(s.deps.Clock)
		fresh.ValidationStatus = port.RowFailed
		fresh.ProcessedAt = &now
		fresh.ErrorMessage = ptr(shared.CodeOf(err) + ": " + err.Error())
		return s.imports.UpdateRow(ctx, fresh)
	})
	if markErr != nil {
		// If even that fails the row stays queued, which a resume will retry.
		// Logging it is the only thing left that keeps the failure visible.
		s.logger().ErrorContext(ctx, "import row failed and could not be marked failed",
			slog.String("batch_id", batch.ID.String()),
			slog.Int("row_no", row.RowNo),
			slog.String("row_error", err.Error()),
			slog.String("mark_error", markErr.Error()))
	}
}

// applyRow creates what one row describes.
//
// It never generates a financial account. Pricing is a separate command with
// its own authority, its own dry run and its own approval; an import that
// created accounts would commit the university to collecting money as a side
// effect of a file upload, under nobody's signature.
func (s *ImportService) applyRow(
	ctx context.Context, actor shared.Actor, batch *port.ImportBatch, row *port.ImportRow, seats *seatIndex,
) error {
	now := nowOr(s.deps.Clock)
	row.ProcessedAt = &now

	if row.Disposition == port.DispositionSkip || row.Disposition == port.DispositionError {
		row.ValidationStatus = port.RowSkipped
		if row.ErrorMessage == nil {
			row.ErrorMessage = ptr("row disposition is " + string(row.Disposition))
		}
		return s.imports.UpdateRow(ctx, row)
	}
	if batch.AcademicYearID == nil {
		return shared.InvariantViolation("import.batch_without_year",
			"batch %s has no academic year", batch.ID)
	}

	studentNo := rowText(row.RawData, ColStudentNo)
	person, err := s.deps.Students.GetByStudentNo(ctx, studentNo)
	if err != nil && !missingRecord(err) {
		return err
	}

	if person == nil {
		if row.Disposition == port.DispositionUpdate {
			return shared.Conflict("import.matched_student_vanished",
				"row %d was reviewed as an update but student %s no longer exists", row.RowNo, studentNo)
		}
		person, err = s.createPerson(ctx, actor, row, studentNo)
		if err != nil {
			return err
		}
	}
	row.MatchedEntityID = &person.ID

	department, studyType, stage, err := resolveSeat(row, seats)
	if err != nil {
		return err
	}

	enrollment, err := s.enrollments.EnrollStudent(ctx, actor, EnrollStudentInput{
		StudentID:      person.ID,
		AcademicYearID: *batch.AcademicYearID,
		DepartmentID:   department.ID,
		StudyTypeID:    studyType.ID,
		Stage:          stage,
		Kind:           academic.KindRegular,
	})
	if err != nil {
		return err
	}

	row.ValidationStatus = port.RowProcessed
	row.CreatedEntityID = &enrollment.Enrollment.ID
	row.ErrorMessage = nil
	return s.imports.UpdateRow(ctx, row)
}

// createPerson registers the identity a row describes.
//
// Duplicates are acknowledged here because the acknowledgement already
// happened: validation raised a probable-duplicate warning, a reviewer read it,
// and confirming the batch is the signature. Re-asking per row inside a
// five-hundred-row run would stop the import with nobody there to answer.
func (s *ImportService) createPerson(
	ctx context.Context, actor shared.Actor, row *port.ImportRow, studentNo string,
) (*student.Student, error) {
	in := RegisterStudentInput{
		StudentNo:  studentNo,
		FullName:   rowText(row.RawData, ColFullName),
		MotherName: rowText(row.RawData, ColMotherName),
		// Phone goes in exactly as typed. Arabic-Indic digits are folded by
		// the database's generated columns, so touching it here would only
		// risk corrupting what the registrar entered.
		Phone:                 nullableText(rowText(row.RawData, ColPhone)),
		AcknowledgeDuplicates: true,
	}
	if raw := rowText(row.RawData, ColBirthDate); raw != "" {
		birthDate, err := shared.ParseDate(raw)
		if err != nil {
			return nil, err
		}
		in.BirthDate = &birthDate
	}
	if raw := rowText(row.RawData, ColGender); raw != "" {
		gender := student.Gender(strings.ToLower(raw))
		in.Gender = &gender
	}

	created, err := s.students.RegisterStudent(ctx, actor, in)
	if err != nil {
		return nil, err
	}
	return created.Student, nil
}

// resolveSeat turns the row's codes into the reference records an enrollment
// needs.
//
// Validation checked these already. They are checked again because a
// disposition set by a reviewer yesterday can name a department retired this
// morning, and enrolling into a programme that is no longer offered is worse
// than failing the row.
func resolveSeat(
	row *port.ImportRow, seats *seatIndex,
) (*academic.Department, *academic.StudyType, int16, error) {
	departmentCode := strings.ToUpper(rowText(row.RawData, ColDepartmentCode))
	department := seats.departments[departmentCode]
	if department == nil {
		return nil, nil, 0, shared.NotFound("import.unknown_department",
			"no active department has the code %q", departmentCode)
	}

	studyTypeCode := strings.ToUpper(rowText(row.RawData, ColStudyTypeCode))
	studyType := seats.studyTypes[studyTypeCode]
	if studyType == nil {
		return nil, nil, 0, shared.NotFound("import.unknown_study_type",
			"no active study type has the code %q", studyTypeCode)
	}

	stage, err := parseStage(rowText(row.RawData, ColStage))
	if err != nil {
		return nil, nil, 0, shared.Validation("import.invalid_stage", "%s", err.Error())
	}
	if stage > department.StageCount {
		return nil, nil, 0, shared.Validation("import.invalid_stage",
			"stage %d does not exist in %s, which runs %d stages", stage, department.Code, department.StageCount)
	}
	return department, studyType, stage, nil
}

// beat records progress. A failed heartbeat is logged and ignored: it is
// monitoring, and losing it must not abort an import that is otherwise fine.
func (s *ImportService) beat(ctx context.Context, batchID shared.ID) {
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return s.imports.Heartbeat(ctx, batchID, nowOr(s.deps.Clock))
	})
	if err != nil {
		s.logger().WarnContext(ctx, "import heartbeat failed",
			slog.String("batch_id", batchID.String()), slog.String("error", err.Error()))
	}
}

// finaliseRun writes the tallies and the terminal status.
func (s *ImportService) finaliseRun(
	ctx context.Context, actor shared.Actor, batch *port.ImportBatch,
) (*ImportRunReport, error) {
	report := &ImportRunReport{FailureReasons: map[string]int{}}

	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		locked, err := s.imports.GetBatchForUpdate(ctx, batch.ID)
		if err != nil {
			return err
		}
		counts, err := s.imports.CountRows(ctx, batch.ID)
		if err != nil {
			return err
		}

		now := nowOr(s.deps.Clock)
		locked.CreatedRows = counts.Created
		locked.UpdatedRows = counts.Updated
		// Rows a reviewer left in error were never queued, so they are part of
		// what the run did not apply and are counted as skipped rather than
		// quietly forgotten.
		locked.SkippedRows = counts.Skipped + counts.Error
		locked.FailedRows = counts.Failed
		locked.HeartbeatAt = &now

		remaining := counts.Valid + counts.Warning
		report.Remaining = remaining

		switch {
		case remaining > 0:
			// Left in importing on purpose. A queue that is not empty means the
			// run stopped early — a cancelled request, a dead worker — and a
			// terminal status here would tell an operator the batch was handled
			// when three hundred of its rows were never attempted.
			locked.Status = port.BatchImporting
		case counts.Total > 0 && counts.Created+counts.Updated == 0:
			locked.Status = port.BatchFailed
			locked.CompletedAt = &now
			locked.ErrorSummary = ptr("no row was applied")
		case locked.SkippedRows > 0 || locked.FailedRows > 0:
			// Never a bare "done" over a run that dropped rows.
			locked.Status = port.BatchCompletedWithSkips
			locked.CompletedAt = &now
			locked.ErrorSummary = ptr(fmt.Sprintf("%d row(s) skipped, %d row(s) failed",
				locked.SkippedRows, locked.FailedRows))
		default:
			locked.Status = port.BatchImported
			locked.CompletedAt = &now
			locked.ErrorSummary = nil
		}

		if err := s.imports.UpdateBatch(ctx, locked); err != nil {
			return err
		}

		report.Batch = locked
		report.Counts = counts

		return s.record(ctx, port.AuditEntry{
			EntityType:     "import_batch",
			EntityID:       &locked.ID,
			Action:         "import.run_finished",
			Actor:          actor,
			After:          snapshotOf(locked),
			AcademicYearID: locked.AcademicYearID,
			Metadata: map[string]any{
				"status":       string(locked.Status),
				"created_rows": locked.CreatedRows,
				"updated_rows": locked.UpdatedRows,
				"skipped_rows": locked.SkippedRows,
				"failed_rows":  locked.FailedRows,
				"remaining":    remaining,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	if err := s.collectFailureReasons(ctx, report); err != nil {
		return nil, err
	}
	if report.Batch.SkippedRows > 0 || report.Batch.FailedRows > 0 || report.Remaining > 0 {
		s.logger().WarnContext(ctx, "import did not apply every row",
			slog.String("batch_id", report.Batch.ID.String()),
			slog.String("status", string(report.Batch.Status)),
			slog.Int("created", report.Batch.CreatedRows),
			slog.Int("updated", report.Batch.UpdatedRows),
			slog.Int("skipped", report.Batch.SkippedRows),
			slog.Int("failed", report.Batch.FailedRows),
			slog.Int("remaining", report.Remaining),
			slog.Any("reasons", report.FailureReasons))
	}
	return report, nil
}

// collectFailureReasons groups the rows that were not applied.
func (s *ImportService) collectFailureReasons(ctx context.Context, report *ImportRunReport) error {
	rows, _, err := s.imports.ListRows(ctx, report.Batch.ID, port.ImportRowFilter{
		Statuses: []port.ImportRowStatus{port.RowFailed, port.RowSkipped, port.RowError},
		Limit:    maxStagedRows,
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		switch {
		case len(row.Errors) > 0:
			report.FailureReasons[row.Errors[0].Code]++
		case row.ErrorMessage != nil:
			report.FailureReasons[reasonKey(*row.ErrorMessage)]++
		default:
			report.FailureReasons[string(row.ValidationStatus)]++
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *ImportService) logger() *slog.Logger {
	if s.deps.Log == nil {
		return slog.Default()
	}
	return s.deps.Log
}

// supportedBatchType refuses the batch types this service does not implement.
func supportedBatchType(batchType string) error {
	switch batchType {
	case port.BatchTypeStudents, port.BatchTypeEnrollments:
		return nil
	case port.BatchTypeAccounts:
		return shared.Validation("import.unsupported_batch_type",
			"financial accounts are not imported; generate them with the bulk pricing command, "+
				"which previews every price before anything is charged")
	default:
		return shared.Validation("import.unsupported_batch_type",
			"%q is not an import type this system loads", batchType)
	}
}

// importStateError explains a workflow step attempted out of order.
func importStateError(operation string, batch *port.ImportBatch, allowed ...port.ImportBatchStatus) error {
	names := make([]string, 0, len(allowed))
	for _, status := range allowed {
		names = append(names, string(status))
	}
	return shared.PreconditionFailed("import.wrong_state",
		"%s requires the batch to be %s; it is %s", operation, strings.Join(names, " or "), batch.Status).
		WithDetail("status", string(batch.Status)).
		WithDetail("allowed", names)
}

// studentRowDedupHash fingerprints the fields that make a row meaningful, so
// the same line staged twice in one batch is stored once.
func studentRowDedupHash(raw map[string]any) string {
	h := sha256.New()
	for _, column := range StudentImportColumns {
		writeHashField(h, column, rowText(raw, column))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rowText reads a column as trimmed text.
//
// Values arrive as strings from a spreadsheet and may arrive as numbers from a
// JSON client, so both are accepted. Only surrounding whitespace is removed:
// an Arabic name and an Arabic-Indic phone number are stored exactly as they
// were entered, and the database derives the folded forms it searches on.
func rowText(raw map[string]any, key string) string {
	value, ok := raw[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

// collapseSpaces folds runs of whitespace into single spaces, matching what
// the student domain does when it stores a name — so a comparison here is
// against the form that would actually be written.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func parseStage(raw string) (int16, error) {
	if raw == "" {
		return 0, fmt.Errorf("the stage is required")
	}
	parsed, err := strconv.ParseInt(raw, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%q is not a stage number", raw)
	}
	if parsed < 1 {
		return 0, fmt.Errorf("stage must be at least 1, got %d", parsed)
	}
	return int16(parsed), nil
}

func addError(row *port.ImportRow, field, code, message string) {
	row.Errors = append(row.Errors, port.ImportFinding{Field: field, Code: code, Message: message})
}

func addWarning(row *port.ImportRow, field, code, message string) {
	row.Warnings = append(row.Warnings, port.ImportFinding{Field: field, Code: code, Message: message})
}

func nullableText(s string) *string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// idOrNilActor maps the system actor, which holds no user row, onto a NULL
// foreign key.
func idOrNilActor(actor shared.Actor) *shared.ID {
	if shared.IsNil(actor.UserID) {
		return nil
	}
	id := actor.UserID
	return &id
}
