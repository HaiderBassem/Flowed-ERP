package httpapi

import (
	"encoding/csv"
	"errors"
	"io"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// BulkHandlers is the HTTP surface of the cohort-wide commands and the staged
// import.
//
// Like the rest of this package it decodes, calls one command, and encodes.
// The one piece of real work it does is parsing a spreadsheet, which belongs
// here rather than in the service: the service takes rows, and where those
// rows came from — CSV today, a spreadsheet upload widget tomorrow — is a
// transport concern.
type BulkHandlers struct {
	Bulk    *app.BulkService
	Imports *app.ImportService

	ImportRepo port.ImportRepository
}

// NewBulkHandlers wires the bulk and import routes to their services.
func NewBulkHandlers(bulk *app.BulkService, imports *app.ImportService, importRepo port.ImportRepository) *BulkHandlers {
	return &BulkHandlers{Bulk: bulk, Imports: imports, ImportRepo: importRepo}
}

// Register mounts /bulk and /imports on the group it is given.
//
// The authority split follows who owns the decision rather than who happens to
// operate the screen: an academic officer promotes a cohort, a finance manager
// prices one, and a registrar loads students. None of the three can do
// another's job here.
func (h *BulkHandlers) Register(g *gin.RouterGroup) {
	bulk := g.Group("/bulk")
	bulk.POST("/promotions",
		h.PromoteStudentsBulk)
	bulk.POST("/accounts",
		h.GenerateAccountsBulk)

	imports := g.Group("/imports")

	imports.GET("", h.ListImports)
	imports.GET("/:id", h.ReviewImport)
	imports.POST("/students", h.UploadStudentImport)
	imports.POST("/:id/validate", h.ValidateImport)
	imports.PATCH("/:id/rows/:row_no", h.SetRowDisposition)
	imports.POST("/:id/confirm", h.ConfirmImport)
	imports.POST("/:id/run", h.RunImport)
	imports.POST("/:id/resume", h.ResumeImport)
	imports.POST("/:id/cancel", h.CancelImport)
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// PromoteBulkRequest asks for a cohort to be rolled into the next year.
type PromoteBulkRequest struct {
	SourceYearID string  `json:"source_year_id" binding:"required,uuid"`
	TargetYearID string  `json:"target_year_id" binding:"required,uuid"`
	CollegeID    *string `json:"college_id" binding:"omitempty,uuid"`
	DepartmentID *string `json:"department_id" binding:"omitempty,uuid"`
	StudyTypeID  *string `json:"study_type_id" binding:"omitempty,uuid"`
	Stage        *int16  `json:"stage" binding:"omitempty,min=1,max=5"`
	// DryRun computes the plan and writes nothing. A commit is refused without
	// the plan hash a dry run returned.
	DryRun          bool   `json:"dry_run"`
	ApprovePlanHash string `json:"approve_plan_hash"`
}

// GenerateAccountsBulkRequest asks for a cohort to be priced.
type GenerateAccountsBulkRequest struct {
	AcademicYearID string  `json:"academic_year_id" binding:"required,uuid"`
	CollegeID      *string `json:"college_id" binding:"omitempty,uuid"`
	DepartmentID   *string `json:"department_id" binding:"omitempty,uuid"`
	StudyTypeID    *string `json:"study_type_id" binding:"omitempty,uuid"`
	Stage          *int16  `json:"stage" binding:"omitempty,min=1,max=5"`
	DryRun         bool    `json:"dry_run"`
	// Approved carries the dry run's rows back, each with the hash of the
	// outcome that was shown. A row whose numbers moved since is skipped rather
	// than charged under an approval given for different ones.
	Approved []ApprovedAccountRow `json:"approved"`
}

// ApprovedAccountRow is one previewed price, signed off.
type ApprovedAccountRow struct {
	EnrollmentID string `json:"enrollment_id" binding:"required,uuid"`
	PreviewHash  string `json:"preview_hash" binding:"required"`
}

// SetDispositionRequest is a reviewer's decision on one import row.
type SetDispositionRequest struct {
	Disposition string `json:"disposition" binding:"required,oneof=create update skip error"`
	// Reason is required when overriding a row validation rejected: it is the
	// audited confirmation that this really is the same student.
	Reason string `json:"reason"`
}

// CancelImportRequest abandons a batch.
type CancelImportRequest struct {
	Reason string `json:"reason"`
}

// ---------------------------------------------------------------------------
// Bulk commands
// ---------------------------------------------------------------------------

// PromoteStudentsBulk previews or applies a cohort promotion.
func (h *BulkHandlers) PromoteStudentsBulk(c *gin.Context) {
	var req PromoteBulkRequest
	if !bindJSON(c, &req) {
		return
	}

	sourceYearID, err := shared.ParseID(req.SourceYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	targetYearID, err := shared.ParseID(req.TargetYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	in := app.PromoteBulkInput{
		SourceYearID:    sourceYearID,
		TargetYearID:    targetYearID,
		Stage:           req.Stage,
		DryRun:          req.DryRun,
		ApprovePlanHash: req.ApprovePlanHash,
	}
	if in.CollegeID, err = optionalID(req.CollegeID); err != nil {
		httpx.Respond(c, err)
		return
	}
	if in.DepartmentID, err = optionalID(req.DepartmentID); err != nil {
		httpx.Respond(c, err)
		return
	}
	if in.StudyTypeID, err = optionalID(req.StudyTypeID); err != nil {
		httpx.Respond(c, err)
		return
	}

	result, err := h.Bulk.PromoteStudentsBulk(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, result)
}

// GenerateAccountsBulk previews or applies a cohort's pricing.
func (h *BulkHandlers) GenerateAccountsBulk(c *gin.Context) {
	var req GenerateAccountsBulkRequest
	if !bindJSON(c, &req) {
		return
	}

	yearID, err := shared.ParseID(req.AcademicYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	in := app.GenerateAccountsBulkInput{
		AcademicYearID: yearID,
		Stage:          req.Stage,
		DryRun:         req.DryRun,
	}
	if in.CollegeID, err = optionalID(req.CollegeID); err != nil {
		httpx.Respond(c, err)
		return
	}
	if in.DepartmentID, err = optionalID(req.DepartmentID); err != nil {
		httpx.Respond(c, err)
		return
	}
	if in.StudyTypeID, err = optionalID(req.StudyTypeID); err != nil {
		httpx.Respond(c, err)
		return
	}
	for _, approved := range req.Approved {
		enrollmentID, err := shared.ParseID(approved.EnrollmentID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.Approved = append(in.Approved, app.AccountApproval{
			EnrollmentID: enrollmentID,
			PreviewHash:  approved.PreviewHash,
		})
	}

	result, err := h.Bulk.GenerateFinancialAccountsBulk(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, result)
}

// ---------------------------------------------------------------------------
// Staged import
// ---------------------------------------------------------------------------

// UploadStudentImport accepts a CSV of students and stages it for review.
//
// Nothing is created by this call. The file becomes rows in a batch, and the
// batch has to be validated, reviewed and confirmed before a single student
// exists.
func (h *BulkHandlers) UploadStudentImport(c *gin.Context) {
	header, err := c.FormFile("file")
	if err != nil {
		httpx.Respond(c, shared.Validation("import.file_required",
			"attach the spreadsheet as a multipart field named \"file\"").WithCause(err))
		return
	}

	yearID, err := shared.ParseID(strings.TrimSpace(c.PostForm("academic_year_id")))
	if err != nil {
		httpx.Respond(c, shared.Validation("import.academic_year_required",
			"the form must carry academic_year_id: an import enrolls its rows into a specific year").WithCause(err))
		return
	}

	batchType := strings.TrimSpace(c.PostForm("batch_type"))
	if batchType == "" {
		batchType = port.BatchTypeStudents
	}

	rows, err := parseImportCSV(header)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	result, err := h.Imports.StageImport(requestContext(c), httpx.MustActor(c), app.StageImportInput{
		BatchType:      batchType,
		Filename:       header.Filename,
		AcademicYearID: &yearID,
		Rows:           rows,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	// The dropped count is part of the response, not a log line the uploader
	// will never see: a file whose last thirty lines repeat its first thirty
	// must not report five hundred rows loaded.
	httpx.Created(c, gin.H{
		"batch":                  toImportBatchView(result.Batch),
		"rows_staged":            result.Staged,
		"duplicate_rows_dropped": result.DuplicateRowsDropped,
		"next":                   "POST /imports/" + result.Batch.ID.String() + "/validate",
	})
}

// ValidateImport checks every staged row and records its findings.
func (h *BulkHandlers) ValidateImport(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	batch, err := h.Imports.ValidateImport(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toImportBatchView(batch))
}

// ReviewImport returns a batch with a page of its rows and their findings.
func (h *BulkHandlers) ReviewImport(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	limit, offset := pagination(c)

	filter := port.ImportRowFilter{
		OnlyProblems: c.Query("only_problems") == "true",
		Limit:        limit,
		Offset:       offset,
	}
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		for _, status := range strings.Split(raw, ",") {
			filter.Statuses = append(filter.Statuses, port.ImportRowStatus(strings.TrimSpace(status)))
		}
	}
	if raw := strings.TrimSpace(c.Query("disposition")); raw != "" {
		for _, disposition := range strings.Split(raw, ",") {
			filter.Dispositions = append(filter.Dispositions, port.ImportDisposition(strings.TrimSpace(disposition)))
		}
	}

	review, err := h.Imports.ReviewImport(requestContext(c), httpx.MustActor(c), id, filter)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"batch":  toImportBatchView(review.Batch),
		"counts": review.Counts,
		"rows":   toImportRowViews(review.Rows),
		"total":  review.Total,
		"limit":  limit,
		"offset": offset,
	})
}

// ListImports returns recent batches.
func (h *BulkHandlers) ListImports(c *gin.Context) {
	limit, offset := pagination(c)

	var status *port.ImportBatchStatus
	if raw := strings.TrimSpace(c.Query("status")); raw != "" {
		parsed := port.ImportBatchStatus(raw)
		status = &parsed
	}

	batches, total, err := h.ImportRepo.ListBatches(requestContext(c), status, limit, offset)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OKPage(c, toImportBatchViews(batches), total, limit, offset)
}

// SetRowDisposition records a reviewer's override of one row.
func (h *BulkHandlers) SetRowDisposition(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	rowNo, err := strconv.Atoi(c.Param("row_no"))
	if err != nil || rowNo < 1 {
		httpx.Respond(c, shared.Validation("import.invalid_row_no",
			"%q is not a row number", c.Param("row_no")))
		return
	}

	var req SetDispositionRequest
	if !bindJSON(c, &req) {
		return
	}

	row, err := h.Imports.SetRowDisposition(requestContext(c), httpx.MustActor(c), app.SetDispositionInput{
		BatchID:     id,
		RowNo:       rowNo,
		Disposition: port.ImportDisposition(req.Disposition),
		Reason:      req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toImportRowView(row))
}

// ConfirmImport approves a reviewed batch for execution.
func (h *BulkHandlers) ConfirmImport(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	batch, err := h.Imports.ConfirmImport(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toImportBatchView(batch))
}

// RunImport applies a confirmed batch.
func (h *BulkHandlers) RunImport(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	report, err := h.Imports.RunImport(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, report)
}

// ResumeImport picks up a batch whose worker stopped.
func (h *BulkHandlers) ResumeImport(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	report, err := h.Imports.ResumeImport(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, report)
}

// CancelImport abandons a batch before it runs.
func (h *BulkHandlers) CancelImport(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req CancelImportRequest
	if !bindJSON(c, &req) {
		return
	}
	batch, err := h.Imports.CancelImport(requestContext(c), httpx.MustActor(c), id, req.Reason)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toImportBatchView(batch))
}

// ---------------------------------------------------------------------------
// CSV parsing
// ---------------------------------------------------------------------------

// utf8BOM is what Excel writes at the start of a UTF-8 CSV. Left in place it
// becomes part of the first header name, and every row then reports a missing
// student number.
const utf8BOM = "\uFEFF"

// parseImportCSV turns an uploaded spreadsheet into rows.
//
// It is deliberately forgiving about what real files look like — a byte-order
// mark, padded headers, trailing blank lines, a ragged last column — and
// unforgiving about what would change the meaning: a missing required column
// is refused here rather than reported five hundred times by validation.
func parseImportCSV(header *multipart.FileHeader) ([]map[string]any, error) {
	file, err := header.Open()
	if err != nil {
		return nil, shared.Validation("import.unreadable_file",
			"the uploaded file could not be read").WithCause(err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	// A spreadsheet exported with a trailing comma on some lines is normal;
	// rejecting the whole file for it would send the registrar back to Excel
	// for a difference that changes nothing.
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	headerRow, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, shared.Validation("import.empty_file", "the uploaded file is empty")
		}
		return nil, shared.Validation("import.unparseable_csv",
			"the file is not readable as CSV: %s", err.Error()).WithCause(err)
	}

	columns := make([]string, len(headerRow))
	for i, name := range headerRow {
		if i == 0 {
			name = strings.TrimPrefix(name, utf8BOM)
		}
		columns[i] = strings.ToLower(strings.TrimSpace(name))
	}

	present := make(map[string]bool, len(columns))
	for _, name := range columns {
		present[name] = true
	}
	// student_no is not required. The system issues university numbers, so a
	// spreadsheet of new students has no numbers to carry yet — demanding the
	// column would make the office invent them, which is the collision the
	// generated sequence exists to prevent. The column is still read when it
	// is present, for a student who already has a number on a document.
	var missing []string
	for _, required := range []string{
		app.ColFullName, app.ColMotherName,
		app.ColDepartmentCode, app.ColStudyTypeCode, app.ColStage,
	} {
		if !present[required] {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return nil, shared.Validation("import.missing_columns",
			"the file is missing the column(s) %s", strings.Join(missing, ", ")).
			WithDetail("missing_columns", missing).
			WithDetail("expected_columns", app.StudentImportColumns)
	}

	var rows []map[string]any
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, shared.Validation("import.unparseable_csv",
				"line %d could not be read: %s", len(rows)+2, err.Error()).WithCause(err)
		}
		if isBlankRecord(record) {
			// Trailing empty lines are an artefact of the export, not data.
			continue
		}

		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if name == "" {
				continue
			}
			value := ""
			if i < len(record) {
				// Only the padding goes. The name and the phone number are
				// stored exactly as entered — the database derives the folded
				// and Arabic-Indic-normalised forms it searches on, and folding
				// them here would corrupt the record of what was submitted.
				value = strings.TrimSpace(record[i])
			}
			row[name] = value
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return nil, shared.Validation("import.no_rows", "the file has a header but no data rows")
	}
	return rows, nil
}

func isBlankRecord(record []string) bool {
	for _, field := range record {
		if strings.TrimSpace(field) != "" {
			return false
		}
	}
	return true
}

// optionalID parses an optional identifier from a request body.
func optionalID(raw *string) (*shared.ID, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	id, err := shared.ParseID(strings.TrimSpace(*raw))
	if err != nil {
		return nil, err
	}
	return &id, nil
}
