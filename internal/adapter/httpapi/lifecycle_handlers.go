package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
)

// LifecycleHandlers expose the commands that existed in the domain with no way
// to reach them: hosting, court-ordered identity changes, student merge and
// installment plan adjustment.
//
// Each was specified in the design and half-built in the schema. The missing
// entry point is what mattered: a registrar who cannot record a hosted student
// records them as an ordinary one, and a finance office that cannot move a due
// date moves it in a notebook.
type LifecycleHandlers struct {
	Enrollments *app.EnrollmentService
	Students    *app.StudentService
	Accounts    *app.AccountService
}

// NewLifecycleHandlers wires the endpoints.
func NewLifecycleHandlers(
	enrollments *app.EnrollmentService, students *app.StudentService, accounts *app.AccountService,
) *LifecycleHandlers {
	return &LifecycleHandlers{Enrollments: enrollments, Students: students, Accounts: accounts}
}

// Register mounts the routes.
func (h *LifecycleHandlers) Register(g *gin.RouterGroup) {
	hosting := g.Group("/hosting")
	hosting.GET("", h.ListHosting)
	hosting.POST("",
		httpx.RequireRoles(shared.RoleRegistrar, shared.RoleAdmin),
		h.RegisterHosting)
	hosting.PATCH("/:enrollment_id",
		httpx.RequireRoles(shared.RoleRegistrar, shared.RoleAdmin),
		h.UpdateHosting)

	students := g.Group("/students")
	students.POST("/:id/identity",
		httpx.RequireRoles(shared.RoleRegistrar, shared.RoleAdmin),
		h.RecordIdentityChange)
	students.GET("/:id/identity-history",
		httpx.RequireRoles(shared.RoleRegistrar, shared.RoleAdmin, shared.RoleAuditor),
		h.IdentityHistory)
	// The target of the merge is the path parameter: the surviving record is
	// the one the operator is looking at when they decide the other is a
	// duplicate of it.
	students.POST("/:id/merge",
		httpx.RequireRoles(shared.RoleRegistrar, shared.RoleAdmin),
		h.MergeStudents)

	accounts := g.Group("/accounts")
	accounts.POST("/:id/plan",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin),
		h.AdjustPlan)
	accounts.GET("/:id/plan-revisions", h.PlanRevisions)
}

// RegisterHosting records a hosting agreement.
func (h *LifecycleHandlers) RegisterHosting(c *gin.Context) {
	var req RegisterHostingRequest
	if !bindJSON(c, &req) {
		return
	}

	enrollmentID, err := shared.ParseID(req.EnrollmentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	homeStudyType, ok := respondingID(c, req.HomeStudyTypeID)
	if !ok {
		return
	}
	hostStudyType, ok := respondingID(c, req.HostStudyTypeID)
	if !ok {
		return
	}
	from, ok := optionalDate(c, req.PeriodFrom, "period_from")
	if !ok {
		return
	}
	to, ok := optionalDate(c, req.PeriodTo, "period_to")
	if !ok {
		return
	}

	record, err := h.Enrollments.RegisterHosting(requestContext(c), httpx.MustActor(c), app.RegisterHostingInput{
		EnrollmentID:    enrollmentID,
		Direction:       academic.HostingDirection(req.Direction),
		HomeUniversity:  req.HomeUniversity,
		HomeCollege:     req.HomeCollege,
		HomeDepartment:  req.HomeDepartment,
		HomeStudyTypeID: homeStudyType,
		HostUniversity:  req.HostUniversity,
		HostCollege:     req.HostCollege,
		HostDepartment:  req.HostDepartment,
		HostStudyTypeID: hostStudyType,
		FeeCollector:    academic.FeeCollector(req.FeeCollector),
		PeriodFrom:      from,
		PeriodTo:        to,
		AgreementRef:    req.AgreementRef,
		Notes:           req.Notes,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toHostingView(record))
}

// UpdateHosting amends an agreement.
func (h *LifecycleHandlers) UpdateHosting(c *gin.Context) {
	enrollmentID, ok := pathID(c, "enrollment_id")
	if !ok {
		return
	}
	var req UpdateHostingRequest
	if !bindJSON(c, &req) {
		return
	}
	to, ok := optionalDate(c, req.PeriodTo, "period_to")
	if !ok {
		return
	}

	var collector *academic.FeeCollector
	if req.FeeCollector != nil {
		value := academic.FeeCollector(*req.FeeCollector)
		collector = &value
	}

	record, err := h.Enrollments.UpdateHosting(requestContext(c), httpx.MustActor(c), app.UpdateHostingInput{
		EnrollmentID: enrollmentID,
		FeeCollector: collector,
		PeriodTo:     to,
		AgreementRef: req.AgreementRef,
		Notes:        req.Notes,
		Reason:       req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toHostingView(record))
}

// ListHosting returns hosting agreements.
func (h *LifecycleHandlers) ListHosting(c *gin.Context) {
	yearID, _ := optionalQueryID(c, "academic_year_id")

	var direction *academic.HostingDirection
	if raw := c.Query("direction"); raw != "" {
		value := academic.HostingDirection(raw)
		direction = &value
	}

	records, err := h.Enrollments.ListHosting(requestContext(c), httpx.MustActor(c), yearID, direction)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]HostingView, 0, len(records))
	for _, r := range records {
		views = append(views, toHostingView(r))
	}
	httpx.OK(c, views)
}

// RecordIdentityChange registers a court-ordered change to a person's legal
// identity.
//
// The previous identity becomes a numbered version rather than being
// overwritten: a certificate issued last year must keep naming the person as
// they were named when it was printed.
func (h *LifecycleHandlers) RecordIdentityChange(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req RecordIdentityChangeRequest
	if !bindJSON(c, &req) {
		return
	}

	decisionDate, ok := requiredDate(c, req.CourtDecisionDate, "court_decision_date")
	if !ok {
		return
	}
	effective := decisionDate
	if req.EffectiveFrom != "" {
		parsed, ok := requiredDate(c, req.EffectiveFrom, "effective_from")
		if !ok {
			return
		}
		effective = parsed
	}
	birthDate, ok := optionalDate(c, req.BirthDate, "birth_date")
	if !ok {
		return
	}

	person, err := h.Students.RecordIdentityChange(requestContext(c), httpx.MustActor(c), app.RecordIdentityChangeInput{
		StudentID:         id,
		FullName:          req.FullName,
		MotherName:        req.MotherName,
		NationalID:        req.NationalID,
		BirthDate:         birthDate,
		CourtDecisionNo:   req.CourtDecisionNo,
		CourtDecisionDate: decisionDate,
		EffectiveFrom:     effective,
		DocumentRef:       req.DocumentRef,
		Reason:            req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toStudentView(person))
}

// IdentityHistory returns every version of a person's legal identity.
func (h *LifecycleHandlers) IdentityHistory(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}

	versions, err := h.Students.IdentityHistory(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]IdentityVersionView, 0, len(versions))
	for _, v := range versions {
		view := IdentityVersionView{
			VersionNo:       v.VersionNo,
			FullName:        v.FullName,
			MotherName:      v.MotherName,
			NationalID:      v.NationalID,
			EffectiveFrom:   ptr(v.EffectiveFrom.String()),
			CourtDecisionNo: v.CourtDecisionNo,
			Reason:          v.ChangeReason,
			RecordedAt:      v.RecordedAt,
		}
		if v.CourtDecisionDate != nil {
			view.CourtDecisionDate = ptr(v.CourtDecisionDate.String())
		}
		views = append(views, view)
	}
	httpx.OK(c, views)
}

// MergeStudents folds a duplicate record into this one.
func (h *LifecycleHandlers) MergeStudents(c *gin.Context) {
	targetID, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req MergeStudentsRequest
	if !bindJSON(c, &req) {
		return
	}
	sourceID, err := shared.ParseID(req.SourceStudentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	result, err := h.Students.MergeStudents(requestContext(c), httpx.MustActor(c), app.MergeStudentsInput{
		SourceID:                     sourceID,
		TargetID:                     targetID,
		Reason:                       req.Reason,
		AcknowledgeDifferentIdentity: req.AcknowledgeDifferentIdentity,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	response := MergeStudentsResponse{
		SourceStudentID: sourceID.String(),
		TargetStudentID: targetID.String(),
	}
	if result.Merge != nil {
		response.EnrollmentsMoved = result.Merge.EnrollmentsMoved
		response.AccountsMoved = result.Merge.AccountsMoved
		response.DiscountsMoved = result.Merge.DiscountsMoved
	}
	httpx.OK(c, response)
}

// AdjustPlan reschedules or re-splits an installment plan.
func (h *LifecycleHandlers) AdjustPlan(c *gin.Context) {
	accountID, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req AdjustPlanRequest
	if !bindJSON(c, &req) {
		return
	}

	input := app.AdjustInstallmentPlanInput{
		AccountID: accountID,
		Kind:      app.AdjustPlanKind(req.Kind),
		Reason:    req.Reason,
	}

	if len(req.DueDates) > 0 {
		input.NewDueDates = make(map[shared.ID]shared.Date, len(req.DueDates))
		for rawID, rawDate := range req.DueDates {
			id, err := shared.ParseID(rawID)
			if err != nil {
				httpx.Respond(c, shared.Validation("invalid_installment_id",
					"%q is not a valid installment identifier", rawID).WithCause(err))
				return
			}
			date, ok := requiredDate(c, rawDate, "due_dates")
			if !ok {
				return
			}
			input.NewDueDates[id] = date
		}
	}

	for _, share := range req.Shares {
		input.Lines = append(input.Lines, billing.TemplateLine{
			LineNo:        int16(len(input.Lines) + 1),
			ShareBP:       money.BasisPoints(share.ShareBP),
			DueOffsetDays: share.DueOffsetDays,
			Label:         share.Label,
		})
	}
	if req.TemplateID != nil {
		id, err := shared.ParseID(*req.TemplateID)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		input.TemplateID = &id
	}

	result, err := h.Accounts.AdjustInstallmentPlan(requestContext(c), httpx.MustActor(c), input)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	response := AdjustPlanResponse{Installments: toInstallmentViews(result.Installments, shared.DateFromTime(time.Now().UTC()))}
	if result.Revision != nil {
		response.Revision = &PlanRevisionView{
			ID:                 result.Revision.ID.String(),
			Kind:               result.Revision.Kind,
			Reason:             result.Revision.Reason,
			PlanVersion:        result.Revision.PlanVersion,
			InstallmentsBefore: result.Revision.InstallmentsBefore,
			InstallmentsAfter:  result.Revision.InstallmentsAfter,
			UnpaidBefore:       result.Revision.UnpaidBefore.Int64(),
			UnpaidAfter:        result.Revision.UnpaidAfter.Int64(),
			CreatedAt:          result.Revision.CreatedAt,
		}
	}
	httpx.OK(c, response)
}

// PlanRevisions returns an account's schedule history.
func (h *LifecycleHandlers) PlanRevisions(c *gin.Context) {
	accountID, ok := pathID(c, "id")
	if !ok {
		return
	}

	revisions, err := h.Accounts.PlanRevisions(requestContext(c), httpx.MustActor(c), accountID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]PlanRevisionView, 0, len(revisions))
	for _, r := range revisions {
		views = append(views, PlanRevisionView{
			ID:                 r.ID.String(),
			Kind:               r.Kind,
			Reason:             r.Reason,
			PlanVersion:        r.PlanVersion,
			InstallmentsBefore: r.InstallmentsBefore,
			InstallmentsAfter:  r.InstallmentsAfter,
			UnpaidBefore:       r.UnpaidBefore.Int64(),
			UnpaidAfter:        r.UnpaidAfter.Int64(),
			CreatedAt:          r.CreatedAt,
		})
	}
	httpx.OK(c, views)
}

func toHostingView(r *academic.HostingRecord) HostingView {
	view := HostingView{
		ID:                    r.ID.String(),
		EnrollmentID:          r.EnrollmentID.String(),
		Direction:             string(r.Direction),
		HomeUniversity:        r.HomeUniversity,
		HomeCollege:           r.HomeCollege,
		HomeDepartment:        r.HomeDepartment,
		HostUniversity:        r.HostUniversity,
		HostCollege:           r.HostCollege,
		HostDepartment:        r.HostDepartment,
		FeeCollector:          string(r.FeeCollector),
		GeneratesLocalAccount: r.GeneratesLocalAccount(),
		AgreementRef:          r.AgreementRef,
	}
	if r.PeriodFrom != nil {
		view.PeriodFrom = ptr(r.PeriodFrom.String())
	}
	if r.PeriodTo != nil {
		view.PeriodTo = ptr(r.PeriodTo.String())
	}
	return view
}

// respondingID parses an optional identifier, responding on a malformed one.
//
// The bare optionalID in bulk_handlers returns an error for a caller that
// aggregates several; this one answers the request directly, which is what a
// handler with a single bad field wants.
func respondingID(c *gin.Context, raw *string) (*shared.ID, bool) {
	id, err := optionalID(raw)
	if err != nil {
		httpx.Respond(c, err)
		return nil, false
	}
	return id, true
}

// optionalDate parses an optional YYYY-MM-DD field, responding on a bad one.
func optionalDate(c *gin.Context, raw *string, field string) (*shared.Date, bool) {
	if raw == nil || *raw == "" {
		return nil, true
	}
	parsed, err := shared.ParseDate(*raw)
	if err != nil {
		httpx.Respond(c, shared.Validation("invalid_date",
			"%s must be a date in YYYY-MM-DD form, got %q", field, *raw).WithCause(err))
		return nil, false
	}
	return &parsed, true
}

// requiredDate parses a mandatory YYYY-MM-DD field.
func requiredDate(c *gin.Context, raw, field string) (shared.Date, bool) {
	parsed, err := shared.ParseDate(raw)
	if err != nil {
		httpx.Respond(c, shared.Validation("invalid_date",
			"%s must be a date in YYYY-MM-DD form, got %q", field, raw).WithCause(err))
		return shared.Date{}, false
	}
	return parsed, true
}
