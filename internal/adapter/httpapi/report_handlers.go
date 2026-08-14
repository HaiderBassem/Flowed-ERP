package httpapi

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/port"
)

// ReportHandlers serves the reporting endpoints.
//
// They read straight from the repository. A service layer between the two
// would have nothing to do: a report takes no lock, writes no row, and has no
// invariant of its own to protect, so the only thing a command layer could add
// here is a hop.
type ReportHandlers struct {
	Reports port.ReportRepository
}

// NewReportHandlers builds the reporting endpoints over the report store.
func NewReportHandlers(reports port.ReportRepository) *ReportHandlers {
	return &ReportHandlers{Reports: reports}
}

// Register mounts the report routes under /reports.
//
// Three authority tiers, and the line between them is accountability rather
// than seniority. The financial reports are aggregates that anyone answerable
// for the money may read. The student statement additionally reaches the desks,
// because a cashier who cannot see what a student owes cannot take the payment.
// The three registers stay with finance, administration and audit alone: they
// name individual operators beside the reversals they signed, and that is
// oversight material, not a dashboard.
func (h *ReportHandlers) Register(g *gin.RouterGroup) {
	reports := g.Group("/reports")

	reports.GET("/students/:id/statement",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor,
			shared.RoleReportViewer, shared.RoleRegistrar, shared.RoleCashier),
		h.StudentStatement)

	financial := reports.Group("",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin,
			shared.RoleAuditor, shared.RoleReportViewer))
	financial.GET("/departments", h.DepartmentSummary)
	financial.GET("/study-types", h.StudyTypeSummary)
	financial.GET("/stages", h.StageSummary)
	financial.GET("/years/:id", h.YearSummary)
	financial.GET("/installments", h.InstallmentReport)
	financial.GET("/debt", h.DebtReport)
	financial.GET("/aging", h.AgingReport)
	financial.GET("/discounts", h.DiscountReport)
	financial.GET("/collection-trend", h.CollectionTrend)
	financial.GET("/cash-flow", h.ExpectedCashFlow)

	// A cashier is added here and nowhere else in this block, because the sheet
	// they are allowed to read is their own; the handler enforces that.
	reports.GET("/cashier-daily",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor,
			shared.RoleReportViewer, shared.RoleCashier),
		h.CashierDaily)

	oversight := reports.Group("",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor))
	oversight.GET("/voids", h.VoidRegister)
	oversight.GET("/refunds", h.RefundRegister)
	oversight.GET("/exemptions", h.ExemptionRegister)
}

// ---------------------------------------------------------------------------
// Student statement
// ---------------------------------------------------------------------------

// StudentStatement returns one student's whole file, every year of it.
func (h *ReportHandlers) StudentStatement(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var yearID *shared.ID
	if parsed, ok := optionalQueryID(c, "academic_year_id"); ok {
		yearID = parsed
	}

	statement, err := h.Reports.StudentStatement(requestContext(c), id, yearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, statement)
}

// ---------------------------------------------------------------------------
// Aggregates
// ---------------------------------------------------------------------------

// DepartmentSummary returns a year's money grouped college then department.
func (h *ReportHandlers) DepartmentSummary(c *gin.Context) {
	rows, err := h.Reports.DepartmentSummary(requestContext(c), reportSummaryFilter(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	if exportRequested(c) {
		writeExport(c, departmentTable(c, rows))
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// StudyTypeSummary regroups the same aggregate by mode of study.
func (h *ReportHandlers) StudyTypeSummary(c *gin.Context) {
	rows, err := h.Reports.StudyTypeSummary(requestContext(c), reportSummaryFilter(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// StageSummary regroups the aggregate by stage, with repeat students broken
// out because they are priced differently.
func (h *ReportHandlers) StageSummary(c *gin.Context) {
	rows, err := h.Reports.StageSummary(requestContext(c), reportSummaryFilter(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// YearSummary returns one academic year's header, its breakdowns, and the
// earlier years' debt collected during it.
func (h *ReportHandlers) YearSummary(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	summary, err := h.Reports.YearSummary(requestContext(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, summary)
}

// ---------------------------------------------------------------------------
// Installments, debt and ageing
// ---------------------------------------------------------------------------

// InstallmentReport returns expected against collected by due month.
func (h *ReportHandlers) InstallmentReport(c *gin.Context) {
	f := port.InstallmentFilter{Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		f.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		f.DepartmentID = id
	}
	if id, ok := optionalQueryID(c, "study_type_id"); ok {
		f.StudyTypeID = id
	}
	from, err := reportDateParam(c, "due_from")
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	to, err := reportDateParam(c, "due_to")
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	f.DueFrom, f.DueTo = from, to

	rows, err := h.Reports.InstallmentReport(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	if exportRequested(c) {
		writeExport(c, installmentTable(c, rows))
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// DebtReport returns outstanding balances, largest first.
func (h *ReportHandlers) DebtReport(c *gin.Context) {
	limit, offset := pagination(c)
	f := port.DebtFilter{
		Scope:          httpx.MustActor(c).QueryScope(),
		PriorYearsOnly: c.Query("prior_years_only") == "true",
		Limit:          limit,
		Offset:         offset,
	}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		f.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		f.DepartmentID = id
	}
	minimum, err := reportAmountParam(c, "minimum_amount")
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	f.MinimumAmount = minimum

	// An export is the whole result, not one page of it: a finance officer
	// exporting the debt list wants the list, and a spreadsheet of the first
	// fifty rows is worse than no spreadsheet because it looks complete.
	if exportRequested(c) {
		f.Limit, f.Offset = maxExportRows, 0
	}

	rows, total, err := h.Reports.DebtReport(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	if exportRequested(c) {
		writeExport(c, debtTable(c, rows))
		return
	}
	httpx.OKPage(c, rows, total, limit, offset)
}

// AgingReport buckets receivables by how long they have been owed.
func (h *ReportHandlers) AgingReport(c *gin.Context) {
	f := port.AgingFilter{Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		f.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		f.DepartmentID = id
	}

	rows, err := h.Reports.AgingReport(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	if exportRequested(c) {
		writeExport(c, agingTable(c, rows))
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// ---------------------------------------------------------------------------
// Discounts and exemptions
// ---------------------------------------------------------------------------

// DiscountReport returns what discounts cost, per definition and version.
func (h *ReportHandlers) DiscountReport(c *gin.Context) {
	f := port.DiscountFilter{Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "definition_id"); ok {
		f.DefinitionID = id
	}
	if category := strings.TrimSpace(c.Query("category")); category != "" {
		f.Category = &category
	}

	rows, err := h.Reports.DiscountReport(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// ExemptionRegister lists the students who were relieved of the whole charge.
func (h *ReportHandlers) ExemptionRegister(c *gin.Context) {
	f, limit, offset, err := reportRegisterFilter(c)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	rows, total, err := h.Reports.ExemptionRegister(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OKPage(c, rows, total, limit, offset)
}

// ---------------------------------------------------------------------------
// Cash
// ---------------------------------------------------------------------------

// CashierDaily returns cash movement per cashier, day and method.
//
// A cashier reads their own sheet and nobody else's. The report is what a
// shift is reconciled against, and a cashier who can see a colleague's takings
// can also see which discrepancies went unnoticed. Finance, administration,
// audit and the report viewers see every desk.
func (h *ReportHandlers) CashierDaily(c *gin.Context) {
	actor := httpx.MustActor(c)

	f := port.CashierDailyFilter{Scope: httpx.MustActor(c).QueryScope()}
	from, err := reportDateParam(c, "from")
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	to, err := reportDateParam(c, "to")
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	f.From, f.To = from, to
	if id, ok := optionalQueryID(c, "cashier_user_id"); ok {
		f.CashierUserID = id
	}

	if !actor.HasAnyRole(shared.RoleFinanceManager, shared.RoleAdmin,
		shared.RoleAuditor, shared.RoleReportViewer) {
		self := actor.UserID
		f.CashierUserID = &self
	}

	rows, err := h.Reports.CashierDaily(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// CollectionTrend returns how a year's money arrived, by month and by
// department.
func (h *ReportHandlers) CollectionTrend(c *gin.Context) {
	f := port.TrendFilter{Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		f.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		f.DepartmentID = id
	}

	trend, err := h.Reports.CollectionTrend(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, trend)
}

// ExpectedCashFlow projects the inflow the remaining due dates imply.
func (h *ReportHandlers) ExpectedCashFlow(c *gin.Context) {
	f := port.CashFlowFilter{Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		f.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		f.DepartmentID = id
	}
	through, err := reportDateParam(c, "through")
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	f.Through = through

	rows, err := h.Reports.ExpectedCashFlow(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, emptyIfNil(rows))
}

// ---------------------------------------------------------------------------
// Registers
// ---------------------------------------------------------------------------

// VoidRegister lists every reversed collection, widest gap between posting and
// reversal first.
func (h *ReportHandlers) VoidRegister(c *gin.Context) {
	f, limit, offset, err := reportRegisterFilter(c)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	rows, total, err := h.Reports.VoidRegister(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OKPage(c, rows, total, limit, offset)
}

// RefundRegister lists every posted refund with both signatures on it.
func (h *ReportHandlers) RefundRegister(c *gin.Context) {
	f, limit, offset, err := reportRegisterFilter(c)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	rows, total, err := h.Reports.RefundRegister(requestContext(c), f)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OKPage(c, rows, total, limit, offset)
}

// ---------------------------------------------------------------------------
// Parameters
// ---------------------------------------------------------------------------

// reportSummaryFilter reads the dimensions every aggregate report shares. The
// academic year is left to the repository to insist on, so that one refusal
// message serves every route rather than each handler writing its own.
func reportSummaryFilter(c *gin.Context) port.SummaryFilter {
	f := port.SummaryFilter{Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	if id, ok := optionalQueryID(c, "college_id"); ok {
		f.CollegeID = id
	}
	if id, ok := optionalQueryID(c, "department_id"); ok {
		f.DepartmentID = id
	}
	if id, ok := optionalQueryID(c, "study_type_id"); ok {
		f.StudyTypeID = id
	}
	if stage, ok := optionalQueryInt16(c, "stage"); ok {
		f.Stage = stage
	}
	return f
}

// reportRegisterFilter reads the filter the three registers share, along with
// the page it was asked for.
func reportRegisterFilter(c *gin.Context) (port.RegisterFilter, int, int, error) {
	limit, offset := pagination(c)
	f := port.RegisterFilter{Limit: limit, Offset: offset, Scope: httpx.MustActor(c).QueryScope()}
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		f.AcademicYearID = id
	}
	from, err := reportDateParam(c, "from")
	if err != nil {
		return port.RegisterFilter{}, 0, 0, err
	}
	to, err := reportDateParam(c, "to")
	if err != nil {
		return port.RegisterFilter{}, 0, 0, err
	}
	f.From, f.To = from, to
	return f, limit, offset, nil
}

// reportDateParam reads an optional ISO date bound.
//
// Unlike the identifier parameters, an unparseable date is refused rather than
// ignored. A stale bookmark carrying a bad id can safely widen a search, but a
// report that quietly drops "from=2026-13-01" prints a different period from
// the one it was asked for, under a heading nobody rereads.
func reportDateParam(c *gin.Context, name string) (*shared.Date, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, nil
	}
	parsed, err := shared.ParseDate(raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// reportAmountParam reads an optional whole-dinar threshold.
func reportAmountParam(c *gin.Context, name string) (money.Amount, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return money.Zero, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return money.Zero, shared.Validation("invalid_query_parameter",
			"%q is not a whole number of dinars for %s", raw, name).WithCause(err)
	}
	return money.FromInt64(parsed), nil
}

// emptyIfNil keeps a report with no rows rendering as [] rather than null, for
// the reason the page envelope gives: a client that iterates without a nil
// check is common, and an empty result is not an error.
func emptyIfNil[T any](rows []T) []T {
	if rows == nil {
		return []T{}
	}
	return rows
}

// maxExportRows bounds a single export.
//
// An export is the whole result rather than one page, but "the whole result"
// still has to fit in a response somebody's browser will accept and in the
// memory of a process serving cashier desks at the same time. Fifty thousand
// rows is a year of a large university's debt list; beyond it the answer is a
// narrower filter, and the limit is stated rather than silently truncating.
const maxExportRows = 50_000
