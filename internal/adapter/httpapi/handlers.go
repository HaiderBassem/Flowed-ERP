package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/port"
)

// Handlers holds the services and repositories the routes reach.
//
// Commands go through services, which own transactions, authority checks and
// the audit trail. Reads go straight to repositories: wrapping a lookup in a
// command layer that adds nothing is ceremony, and the read paths have no
// invariants of their own to protect.
type Handlers struct {
	Students    *app.StudentService
	Enrollments *app.EnrollmentService
	Accounts    *app.AccountService
	Payments    *app.PaymentService
	Refunds     *app.RefundService
	Discounts   *app.DiscountService
	Years       *app.YearService

	StudentRepo     port.StudentRepository
	EnrollmentRepo  port.EnrollmentRepository
	AccountRepo     port.AccountRepository
	InstallmentRepo port.InstallmentRepository
	PaymentRepo     port.PaymentRepository
	RefundRepo      port.RefundRepository
	VoidRequestRepo port.VoidRequestRepository
	DiscountRepo    port.DiscountRepository
	YearRepo        port.AcademicYearRepository
	ReferenceRepo   port.ReferenceRepository
	AuditRepo       port.AuditRepository

	Clock shared.Clock
}

func (h *Handlers) now() time.Time {
	if h.Clock == nil {
		return time.Now().UTC()
	}
	return h.Clock.Now()
}

func (h *Handlers) today() shared.Date { return shared.DateFromTime(h.now()) }

// ---------------------------------------------------------------------------
// Students
// ---------------------------------------------------------------------------

// RegisterStudent creates a student identity.
func (h *Handlers) RegisterStudent(c *gin.Context) {
	var req RegisterStudentRequest
	if !bindJSON(c, &req) {
		return
	}

	in := app.RegisterStudentInput{
		StudentNo:             req.StudentNo,
		FullName:              req.FullName,
		MotherName:            req.MotherName,
		NationalID:            req.NationalID,
		Phone:                 req.Phone,
		Email:                 req.Email,
		Address:               req.Address,
		AcknowledgeDuplicates: req.AcknowledgeDuplicates,
	}
	if req.BirthDate != nil {
		birthDate, err := shared.ParseDate(*req.BirthDate)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.BirthDate = &birthDate
	}
	if req.Gender != nil {
		gender := student.Gender(*req.Gender)
		in.Gender = &gender
	}

	result, err := h.Students.RegisterStudent(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toStudentView(result.Student))
}

// GetStudent returns one student identity.
func (h *Handlers) GetStudent(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	found, err := h.StudentRepo.GetByID(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toStudentView(found))
}

// SearchStudents queries the student index.
//
// The query term is folded by the same Arabic normalisation applied to the
// stored columns, so a clerk typing فاطمه finds فاطمة.
func (h *Handlers) SearchStudents(c *gin.Context) {
	limit, offset := pagination(c)

	q := port.StudentSearch{
		Query:        c.Query("q"),
		OnlyWithDebt: c.Query("with_debt") == "true",
		Limit:        limit,
		Offset:       offset,
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		q.DepartmentID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		q.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "study_type_id"); ok {
		q.StudyTypeID = id
	}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		q.AcademicYearID = id
	}
	if stage, ok := optionalQueryInt16(c, "stage"); ok {
		q.Stage = stage
	}

	found, total, err := h.StudentRepo.Search(requestContext(c), q)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]StudentView, 0, len(found))
	for _, s := range found {
		views = append(views, toStudentView(s))
	}
	httpx.OKPage(c, views, total, limit, offset)
}

// UpdateStudentContact changes how a student is reached.
func (h *Handlers) UpdateStudentContact(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdateContactRequest
	if !bindJSON(c, &req) {
		return
	}

	updated, err := h.Students.UpdateContactDetails(requestContext(c), httpx.MustActor(c), app.UpdateContactInput{
		StudentID:     id,
		Phone:         req.Phone,
		PhoneAlt:      req.PhoneAlt,
		Email:         req.Email,
		Address:       req.Address,
		GuardianName:  req.GuardianName,
		GuardianPhone: req.GuardianPhone,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toStudentView(updated))
}

// StudentEnrollments returns the student's full academic history.
//
// Superseded rows are included. A client showing a student's record needs the
// lineage: "registered evening, moved to morning in January" is the answer to
// why two rows exist for one year, and hiding one of them makes the payment
// history unexplainable.
func (h *Handlers) StudentEnrollments(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	history, err := h.EnrollmentRepo.History(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]EnrollmentView, 0, len(history))
	for _, e := range history {
		views = append(views, toEnrollmentView(e))
	}
	httpx.OK(c, views)
}

// StudentAccounts returns every financial account the student holds.
//
// One per enrollment, each with its own year's prices and its own debt. The
// total a student owes is the sum across these, computed by the client from
// what it can see rather than by a running balance that loses which year owes
// what.
func (h *Handlers) StudentAccounts(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	accounts, err := h.AccountRepo.ListForStudent(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]AccountView, 0, len(accounts))
	for _, a := range accounts {
		views = append(views, toAccountView(a))
	}
	httpx.OK(c, views)
}

// ---------------------------------------------------------------------------
// Enrollments
// ---------------------------------------------------------------------------

// EnrollStudent registers a student for a year.
func (h *Handlers) EnrollStudent(c *gin.Context) {
	var req EnrollStudentRequest
	if !bindJSON(c, &req) {
		return
	}

	studentID, err := shared.ParseID(req.StudentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	yearID, err := shared.ParseID(req.AcademicYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	departmentID, err := shared.ParseID(req.DepartmentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	studyTypeID, err := shared.ParseID(req.StudyTypeID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	in := app.EnrollStudentInput{
		StudentID:         studentID,
		AcademicYearID:    yearID,
		DepartmentID:      departmentID,
		StudyTypeID:       studyTypeID,
		Stage:             req.Stage,
		CategoryCode:      req.CategoryCode,
		Kind:              academic.Kind(req.Kind),
		OverrideDebtBlock: req.OverrideDebtBlock,
		OverrideReason:    req.OverrideReason,
	}
	if req.PreviousEnrollmentID != nil {
		previous, err := shared.ParseID(*req.PreviousEnrollmentID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.PreviousEnrollmentID = &previous
	}

	result, err := h.Enrollments.EnrollStudent(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	// The debt is surfaced even when policy allowed the registration. A
	// registrar who cannot see it cannot mention it to the student.
	httpx.Created(c, gin.H{
		"enrollment":  toEnrollmentView(result.Enrollment),
		"prior_debt":  result.PriorDebt,
		"debt_warned": result.DebtWarned,
	})
}

// GetEnrollment returns one registration.
func (h *Handlers) GetEnrollment(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	found, err := h.EnrollmentRepo.GetByID(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toEnrollmentView(found))
}

// SupersedeEnrollment replaces an enrollment with a corrected one.
func (h *Handlers) SupersedeEnrollment(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req SupersedeEnrollmentRequest
	if !bindJSON(c, &req) {
		return
	}

	effectiveDate, err := shared.ParseDate(req.EffectiveDate)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	in := app.SupersedeInput{
		EnrollmentID:    id,
		NewStage:        req.NewStage,
		NewCategoryCode: req.NewCategoryCode,
		Reason:          req.Reason,
		EffectiveDate:   effectiveDate,
	}
	if req.NewDepartmentID != nil {
		departmentID, err := shared.ParseID(*req.NewDepartmentID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.NewDepartmentID = &departmentID
	}
	if req.NewStudyTypeID != nil {
		studyTypeID, err := shared.ParseID(*req.NewStudyTypeID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.NewStudyTypeID = &studyTypeID
	}

	result, err := h.Enrollments.SupersedeEnrollment(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"superseded":         toEnrollmentView(result.Superseded),
		"replacement":        toEnrollmentView(result.Replacement),
		"transferred_credit": result.TransferredCredit,
	})
}

// RecordResult sets an enrollment's examination outcome.
func (h *Handlers) RecordResult(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req RecordResultRequest
	if !bindJSON(c, &req) {
		return
	}

	updated, err := h.Enrollments.RecordAcademicResult(requestContext(c), httpx.MustActor(c), app.RecordResultInput{
		EnrollmentID: id,
		Result:       academic.AcademicResult(req.Result),
		ByDecision:   req.ByDecision,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toEnrollmentView(updated))
}

// ChangeEnrollmentStatus defers, withdraws, transfers out or completes.
func (h *Handlers) ChangeEnrollmentStatus(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ChangeStatusRequest
	if !bindJSON(c, &req) {
		return
	}

	var charge money.Amount
	if req.ChargeInstead != nil {
		charge = money.Amount(*req.ChargeInstead)
	}

	result, err := h.Enrollments.ChangeEnrollmentStatus(requestContext(c), httpx.MustActor(c), app.ChangeStatusInput{
		EnrollmentID:             id,
		Target:                   academic.EnrollmentStatus(req.Status),
		OrderRef:                 req.OrderRef,
		Reason:                   req.Reason,
		Result:                   academic.AcademicResult(req.Result),
		FinancialTreatment:       academic.FinancialTreatment(req.FinancialTreatment),
		ChargeInstead:            charge,
		GraduationOverrideReason: req.GraduationOverrideReason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	response := ChangeStatusResponse{Enrollment: toEnrollmentView(result.Enrollment)}
	if result.Treatment.Treatment != "" {
		response.Treatment = &TreatmentView{
			Treatment:           string(result.Treatment.Treatment),
			Applied:             result.Treatment.Applied,
			Waived:              result.Treatment.Waived.Int64(),
			CreditRaised:        result.Treatment.CreditRaised.Int64(),
			RemainingObligation: result.Treatment.RemainingObligation.Int64(),
		}
		if result.Treatment.AccountID != nil {
			response.Treatment.AccountID = ptr(result.Treatment.AccountID.String())
		}
	}
	if result.Clearance != nil {
		response.Clearance = &ClearanceView{
			Cleared:        result.Clearance.Cleared,
			Policy:         string(result.Clearance.Policy),
			Outstanding:    result.Clearance.Outstanding.Int64(),
			OverrideReason: result.Clearance.OverrideReason,
			DecidedAt:      result.Clearance.DecidedAt,
		}
	}
	httpx.OK(c, response)
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

// GenerateAccount prices an enrollment and freezes the result.
func (h *Handlers) GenerateAccount(c *gin.Context) {
	var req GenerateAccountRequest
	if !bindJSON(c, &req) {
		return
	}

	enrollmentID, err := shared.ParseID(req.EnrollmentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	in := app.GenerateAccountInput{EnrollmentID: enrollmentID, DryRun: req.DryRun}
	if req.TemplateID != nil {
		templateID, err := shared.ParseID(*req.TemplateID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.TemplateID = &templateID
	}

	result, err := h.Accounts.GenerateFinancialAccount(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	payload := gin.H{
		"account":           toAccountView(result.Account),
		"fee_components":    toSnapshotViews(result.Snapshot),
		"discounts":         toApplicationViews(result.Applications),
		"installments":      toInstallmentViews(result.Installments, h.today()),
		"pending_discounts": result.PendingDiscounts,
		"dry_run":           result.DryRun,
	}
	if result.DryRun {
		httpx.OK(c, payload)
		return
	}
	httpx.Created(c, payload)
}

// GetAccount returns the full financial picture for one account.
func (h *Handlers) GetAccount(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	ctx := requestContext(c)

	account, err := h.AccountRepo.GetByID(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	snapshot, err := h.AccountRepo.Snapshot(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	applications, err := h.DiscountRepo.ListApplications(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	installments, err := h.InstallmentRepo.ListForAccount(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	payments, err := h.PaymentRepo.ListForAccount(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	refunds, err := h.RefundRepo.ListForAccount(ctx, id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	httpx.OK(c, AccountDetailView{
		Account:      toAccountView(account),
		Snapshot:     toSnapshotViews(snapshot),
		Discounts:    toApplicationViews(applications),
		Installments: toInstallmentViews(installments, h.today()),
		Payments:     toPaymentViews(payments),
		Refunds:      toRefundViews(refunds),
	})
}

// PostAdjustment records a signed change to what an account owes.
func (h *Handlers) PostAdjustment(c *gin.Context) {
	var req PostAdjustmentRequest
	if !bindJSON(c, &req) {
		return
	}

	accountID, err := shared.ParseID(req.AccountID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	adjustment, err := h.Accounts.PostAdjustment(requestContext(c), httpx.MustActor(c), app.PostAdjustmentInput{
		AccountID: accountID,
		Type:      billing.AdjustmentType(req.Type),
		Amount:    money.FromInt64(req.Amount),
		Reason:    req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, gin.H{
		"id":     adjustment.ID.String(),
		"type":   string(adjustment.Type),
		"amount": adjustment.Amount,
		"reason": adjustment.Reason,
	})
}

// ---------------------------------------------------------------------------
// Payments
// ---------------------------------------------------------------------------

// RecordPayment collects money against an account.
func (h *Handlers) RecordPayment(c *gin.Context) {
	var req RecordPaymentRequest
	if !bindJSON(c, &req) {
		return
	}

	accountID, err := shared.ParseID(req.AccountID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	methodID, err := shared.ParseID(req.PaymentMethodID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	targets := make([]billing.Allocation, 0, len(req.TargetInstallments))
	for _, t := range req.TargetInstallments {
		installmentID, err := shared.ParseID(t.InstallmentID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		targets = append(targets, billing.Allocation{
			InstallmentID: installmentID,
			Amount:        money.FromInt64(t.Amount),
		})
	}

	// The idempotency middleware has already claimed the key and hashed the
	// body; the service stores both on the payment so a replay can prove it is
	// the same request rather than a different one reusing a key.
	result, err := h.Payments.RecordPayment(requestContext(c), httpx.MustActor(c), app.RecordPaymentInput{
		AccountID:          accountID,
		Amount:             money.FromInt64(req.Amount),
		PaymentMethodID:    methodID,
		MethodReference:    req.MethodReference,
		PayerName:          req.PayerName,
		Notes:              req.Notes,
		IdempotencyKey:     c.GetHeader(httpx.IdempotencyKeyHeader),
		PayloadHash:        payloadHash(c),
		TargetInstallments: targets,
		ConfirmedDistinct:  req.ConfirmedDistinct,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	allocations := make([]AllocationView, 0, len(result.Allocations))
	for _, a := range result.Allocations {
		allocations = append(allocations, AllocationView{
			InstallmentID: a.InstallmentID.String(),
			Amount:        a.Amount,
		})
	}

	httpx.Created(c, RecordPaymentResponse{
		Payment:      toPaymentView(result.Payment),
		Allocations:  allocations,
		CreditAmount: result.CreditAmount,
		Remaining:    result.Remaining,
		Duplicate:    result.Duplicate,
	})
}

// GetPayment returns one collection.
func (h *Handlers) GetPayment(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	found, err := h.PaymentRepo.GetByID(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toPaymentView(found))
}

// RequestVoid asks for a payment to be reversed.
func (h *Handlers) RequestVoid(c *gin.Context) {
	var req VoidRequestBody
	if !bindJSON(c, &req) {
		return
	}
	paymentID, err := shared.ParseID(req.PaymentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	request, err := h.Payments.RequestVoid(requestContext(c), httpx.MustActor(c), paymentID, req.Reason)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toVoidRequestView(request))
}

// ExecuteVoid reverses a payment in full. A different person from the one who
// requested it, which the domain enforces.
func (h *Handlers) ExecuteVoid(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	voided, err := h.Payments.ExecuteVoid(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toPaymentView(voided))
}

// ListPendingVoids returns void requests awaiting execution.
func (h *Handlers) ListPendingVoids(c *gin.Context) {
	requests, err := h.VoidRequestRepo.ListPending(requestContext(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]VoidRequestView, 0, len(requests))
	for _, r := range requests {
		views = append(views, toVoidRequestView(r))
	}
	httpx.OK(c, views)
}

// ---------------------------------------------------------------------------
// Refunds
// ---------------------------------------------------------------------------

// RequestRefund opens a refund for approval.
func (h *Handlers) RequestRefund(c *gin.Context) {
	var req RefundRequestBody
	if !bindJSON(c, &req) {
		return
	}
	paymentID, err := shared.ParseID(req.PaymentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	methodID, err := shared.ParseID(req.PaymentMethodID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	refund, err := h.Refunds.RequestRefund(requestContext(c), httpx.MustActor(c), app.RequestRefundInput{
		PaymentID:       paymentID,
		Amount:          money.FromInt64(req.Amount),
		PaymentMethodID: methodID,
		Reason:          req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toRefundView(refund))
}

// ApproveRefund accepts a refund request.
func (h *Handlers) ApproveRefund(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	refund, err := h.Refunds.ApproveRefund(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toRefundView(refund))
}

// RejectRefund declines a refund request.
func (h *Handlers) RejectRefund(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req RejectRequestBody
	if !bindJSON(c, &req) {
		return
	}
	refund, err := h.Refunds.RejectRefund(requestContext(c), httpx.MustActor(c), id, req.Reason)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toRefundView(refund))
}

// PostRefund pays the money out.
func (h *Handlers) PostRefund(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	refund, err := h.Refunds.PostRefund(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toRefundView(refund))
}

// ListPendingRefunds returns refunds awaiting approval.
func (h *Handlers) ListPendingRefunds(c *gin.Context) {
	refunds, err := h.RefundRepo.ListPending(requestContext(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toRefundViews(refunds))
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

// AssignDiscount grants a discount to a student.
func (h *Handlers) AssignDiscount(c *gin.Context) {
	var req AssignDiscountRequest
	if !bindJSON(c, &req) {
		return
	}

	studentID, err := shared.ParseID(req.StudentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	definitionID, err := shared.ParseID(req.DefinitionID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	in := app.AssignDiscountInput{
		StudentID:     studentID,
		DefinitionID:  definitionID,
		Scope:         discount.ScopeType(req.Scope),
		Justification: req.Justification,
		DocumentRefs:  req.DocumentRefs,
	}
	if req.YearFromID != nil {
		from, err := shared.ParseID(*req.YearFromID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.YearFromID = &from
	}
	if req.YearToID != nil {
		to, err := shared.ParseID(*req.YearToID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.YearToID = &to
	}

	assignment, err := h.Discounts.AssignDiscount(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toAssignmentView(assignment))
}

// ApproveDiscount accepts a grant. The approver may not be the requester.
func (h *Handlers) ApproveDiscount(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	assignment, err := h.Discounts.ApproveDiscountAssignment(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toAssignmentView(assignment))
}

// RevokeDiscount withdraws a grant.
func (h *Handlers) RevokeDiscount(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req RevokeDiscountRequest
	if !bindJSON(c, &req) {
		return
	}

	effect := discount.RevocationEffect(req.Effect)
	if effect == "" {
		effect = discount.RevokeProspectiveOnly
	}

	assignment, err := h.Discounts.RevokeDiscountAssignment(requestContext(c), httpx.MustActor(c), app.RevokeDiscountInput{
		AssignmentID: id,
		Reason:       req.Reason,
		Effect:       effect,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toAssignmentView(assignment))
}

// ConfirmDiscountApplication resolves a discount awaiting annual
// re-confirmation.
func (h *Handlers) ConfirmDiscountApplication(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ConfirmApplicationRequest
	if !bindJSON(c, &req) {
		return
	}

	application, err := h.Discounts.ConfirmDiscountApplication(requestContext(c), httpx.MustActor(c), app.ConfirmApplicationInput{
		ApplicationID: id,
		Confirm:       req.Confirm,
		Reason:        req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toApplicationViews([]*discount.Application{application})[0])
}

// StudentDiscounts lists a student's grants.
func (h *Handlers) StudentDiscounts(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	assignments, err := h.DiscountRepo.ListAssignmentsForStudent(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]AssignmentView, 0, len(assignments))
	for _, a := range assignments {
		views = append(views, toAssignmentView(a))
	}
	httpx.OK(c, views)
}

// ---------------------------------------------------------------------------
// Academic years
// ---------------------------------------------------------------------------

// CreateYear defines an academic year in draft.
func (h *Handlers) CreateYear(c *gin.Context) {
	var req CreateYearRequest
	if !bindJSON(c, &req) {
		return
	}
	startDate, err := shared.ParseDate(req.StartDate)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	endDate, err := shared.ParseDate(req.EndDate)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	year, err := h.Years.CreateAcademicYear(requestContext(c), httpx.MustActor(c), app.CreateYearInput{
		Code:            req.Code,
		StartDate:       startDate,
		EndDate:         endDate,
		DebtBlockPolicy: academic.DebtBlockPolicy(req.DebtBlockPolicy),
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toYearView(year))
}

// ListYears returns every academic year.
func (h *Handlers) ListYears(c *gin.Context) {
	years, err := h.YearRepo.List(requestContext(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]YearView, 0, len(years))
	for _, y := range years {
		views = append(views, toYearView(y))
	}
	httpx.OK(c, views)
}

// OpenYear puts a draft year into service.
func (h *Handlers) OpenYear(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	year, err := h.Years.OpenAcademicYear(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toYearView(year))
}

// CloseYearFinancially freezes money while leaving results writable.
func (h *Handlers) CloseYearFinancially(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	result, err := h.Years.CloseYearFinancially(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"year":              toYearView(result.Year),
		"drifting_accounts": result.DriftingAccounts,
	})
}

// CloseYear freezes a year completely.
func (h *Handlers) CloseYear(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	year, err := h.Years.CloseAcademicYear(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toYearView(year))
}

// ReopenYear opens closed books for a bounded, audited correction.
func (h *Handlers) ReopenYear(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ReopenYearRequest
	if !bindJSON(c, &req) {
		return
	}
	window := time.Duration(req.WindowHours) * time.Hour

	year, err := h.Years.ReopenYearForAdjustment(requestContext(c), httpx.MustActor(c), app.ReopenForAdjustmentInput{
		YearID: id,
		Reason: req.Reason,
		Window: window,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toYearView(year))
}

// ---------------------------------------------------------------------------
// Reference data and audit
// ---------------------------------------------------------------------------

// ListStudyTypes returns the configured modes of study.
func (h *Handlers) ListStudyTypes(c *gin.Context) {
	types, err := h.ReferenceRepo.ListStudyTypes(requestContext(c), true)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(types))
	for _, t := range types {
		out = append(out, gin.H{"id": t.ID.String(), "code": t.Code, "name_ar": t.NameAr})
	}
	httpx.OK(c, out)
}

// ListColleges returns the faculties.
func (h *Handlers) ListColleges(c *gin.Context) {
	colleges, err := h.ReferenceRepo.ListColleges(requestContext(c), true)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(colleges))
	for _, col := range colleges {
		out = append(out, gin.H{"id": col.ID.String(), "code": col.Code, "name_ar": col.NameAr})
	}
	httpx.OK(c, out)
}

// ListDepartments returns programmes, optionally filtered by college.
func (h *Handlers) ListDepartments(c *gin.Context) {
	var collegeID *shared.ID
	if id, ok := optionalQueryID(c, "college_id"); ok {
		collegeID = id
	}
	departments, err := h.ReferenceRepo.ListDepartments(requestContext(c), collegeID, true)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(departments))
	for _, d := range departments {
		out = append(out, gin.H{
			"id": d.ID.String(), "college_id": d.CollegeID.String(),
			"code": d.Code, "name_ar": d.NameAr, "stage_count": d.StageCount,
		})
	}
	httpx.OK(c, out)
}

// ListPaymentMethods returns the ways money may be taken.
func (h *Handlers) ListPaymentMethods(c *gin.Context) {
	methods, err := h.ReferenceRepo.ListPaymentMethods(requestContext(c), true)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(methods))
	for _, m := range methods {
		out = append(out, gin.H{
			"id": m.ID.String(), "code": m.Code, "name_ar": m.NameAr,
			"is_cash": m.IsCash, "requires_reference": m.RequiresReference,
		})
	}
	httpx.OK(c, out)
}

// StudentAudit returns the audit trail for one student.
func (h *Handlers) StudentAudit(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	limit, _ := pagination(c)
	entries, err := h.AuditRepo.ListForStudent(requestContext(c), id, limit)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		out = append(out, gin.H{
			"action":      e.Action,
			"entity_type": e.EntityType,
			"actor":       e.Actor.Username,
			"occurred_at": e.OccurredAt,
			"reason":      e.Reason,
			"metadata":    e.Metadata,
		})
	}
	httpx.OK(c, out)
}

// VerifyAuditChain reports any audit entry altered after it was written.
//
// An empty result is the expected outcome. The chain does not prevent
// tampering — the application owns its tables — but it makes tampering
// visible and names where it happened.
func (h *Handlers) VerifyAuditChain(c *gin.Context) {
	problems, err := h.AuditRepo.VerifyChain(requestContext(c), 0)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(problems))
	for _, p := range problems {
		out = append(out, gin.H{
			"sequence_no": p.SequenceNo,
			"entry_id":    p.EntryID.String(),
			"occurred_at": p.OccurredAt,
			"problem":     p.Problem,
		})
	}
	httpx.OK(c, gin.H{"intact": len(problems) == 0, "problems": out})
}

// ReconciliationReport lists accounts whose cached totals disagree with their
// transaction rows. Expected to be empty; every row is a defect.
func (h *Handlers) ReconciliationReport(c *gin.Context) {
	limit, _ := pagination(c)
	rows, err := h.AccountRepo.ReconciliationDrift(requestContext(c), limit)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"account_id":        r.AccountID.String(),
			"cached_paid":       r.CachedPaid,
			"computed_paid":     r.ComputedPaid,
			"cached_refunded":   r.CachedRefunded,
			"computed_refunded": r.ComputedRefunded,
			"cached_credit":     r.CachedCredit,
			"computed_credit":   r.ComputedCredit,
		})
	}
	httpx.OK(c, gin.H{"clean": len(rows) == 0, "drifting_accounts": out})
}
