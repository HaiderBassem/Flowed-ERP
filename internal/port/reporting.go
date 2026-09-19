package port

import (
	"context"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// Reporting read models.
//
// Two rules govern every figure declared below.
//
// Counts and money are drawn from deliberately different populations. A head
// count comes from the effective enrollments, where superseded rows have
// already been removed, so a student who changed department in March is one
// student and not two. Money comes from every non-cancelled account of the
// year, superseded ones included: the account behind a superseded enrollment
// may hold cash a student really handed over, and dropping it would leave the
// year's revenue disagreeing with the cashiers' drawers. Joining the two
// populations into one query is what produces either a doubled gross or a
// missing collection, which is why they stay separate all the way out to here.
//
// Anything an auditor would question is derived from transaction rows. The
// cached totals on financial_account exist so a list screen renders quickly;
// only the counting dashboards are allowed to read them.
//
// These structs carry JSON tags because they are the report itself — a read
// model shaped for the page it prints on, with no behaviour to hide. That is a
// serialisation detail of the model, not the transport concern this package
// otherwise keeps out, and a parallel set of view structs would only drift.
//
// This was revisited and left as it is. The consistent alternative — the one
// import_views.go takes, and the right one there — is a parallel view struct
// per type with a mapping function. Here that is thirty structs and some seven
// hundred lines of field-for-field copying whose only defect mode is a field
// somebody forgets to map, on a path that is read-only and has no behaviour to
// protect. The layering rule exists to stop storage and transport concerns
// leaking into the domain's vocabulary; a read model that exists solely to be
// serialised has no such vocabulary to protect, and enforcing the rule here
// would buy the appearance of consistency at the cost of the drift it was
// meant to prevent.
//
// The distinction against import_views.go is real rather than an excuse. Those
// port structs describe a staged import that the application also *acts* on —
// they are operated on, not just rendered — and their field names differ from
// what the API should expose. These are rendered and nothing else.

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

// SummaryFilter narrows the aggregate reports.
//
// AcademicYearID is required. It is what bounds the scan: without it a summary
// reads every account the university has ever opened, and the answer means
// nothing anyway because two years' prices are not comparable.
type SummaryFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	StudyTypeID    *shared.ID
	Stage          *int16
}

// InstallmentFilter narrows the installment schedule report.
type InstallmentFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	StudyTypeID    *shared.ID
	DueFrom        *shared.Date
	DueTo          *shared.Date
}

// DebtFilter narrows the outstanding-balance report.
//
// Either an academic year or PriorYearsOnly must be given. The two are the only
// bounds that keep the report from scanning every open balance in the
// institution's history.
type DebtFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	// PriorYearsOnly restricts the report to accounts of years whose books are
	// already shut, which is the collection list the finance office works from.
	PriorYearsOnly bool
	// MinimumAmount drops balances too small to chase.
	MinimumAmount money.Amount
	Limit         int
	Offset        int
}

// AgingFilter narrows the receivables ageing report.
//
// The academic year is optional here alone. Ageing exists to show how old a
// debt is, and its prior-year bucket is empty by construction once the report
// is pinned to a single year.
type AgingFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
}

// DiscountFilter narrows the discount cost report.
type DiscountFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	DefinitionID   *shared.ID
	Category       *string
}

// CashierDailyFilter narrows the cash movement report. The date range is
// required: it is what bounds a report whose rows have no academic year of
// their own, because a cashier's day belongs to a shift and not to a year.
type CashierDailyFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope         shared.ScopeFilter
	From          *shared.Date
	To            *shared.Date
	CashierUserID *shared.ID
}

// TrendFilter narrows the collection trend.
type TrendFilter struct {
	// Scope restricts the report to the caller's colleges and departments.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
}

// CashFlowFilter narrows the expected inflow projection.
type CashFlowFilter struct {
	// Scope restricts the report to the caller's colleges and departments.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	// Through caps the projection horizon; unset, every remaining due date is
	// projected.
	Through *shared.Date
}

// RegisterFilter narrows the void, refund and exemption registers. All three
// are bounded by posting year, and optionally by a range over the instant the
// event was recorded.
type RegisterFilter struct {
	// Scope restricts the report to the colleges and departments the
	// caller may see. Applied inside the query: a scoped operator's
	// report contains their colleges rather than everyone's with the
	// others filtered out after the rows were already read.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	From           *shared.Date
	To             *shared.Date
	Limit          int
	Offset         int
}

// ---------------------------------------------------------------------------
// Student statement
// ---------------------------------------------------------------------------

// StudentStatement is everything one student's file contains, across every
// year they have been enrolled.
//
// Every figure in it is read from transaction rows rather than from a cached
// total, because this is the document handed to a student who disputes a
// balance and to an auditor who asks how it was arrived at.
type StudentStatement struct {
	Student  StatementStudent   `json:"student"`
	Accounts []StatementAccount `json:"accounts"`
	Totals   StatementTotals    `json:"totals"`
}

// StatementStudent identifies the person the statement belongs to. The
// mother's name is present because in Iraq it is the discriminator between two
// students who share a name.
type StatementStudent struct {
	ID         shared.ID `json:"id"`
	StudentNo  string    `json:"student_no"`
	FullName   string    `json:"full_name"`
	MotherName string    `json:"mother_name"`
	Phone      *string   `json:"phone,omitempty"`
	Status     string    `json:"status"`
}

// StatementAccount is one year's financial position with everything that
// explains it.
//
// Cancelled accounts are included and flagged rather than hidden: a cancelled
// account is usually why a second one exists for the same enrollment, and a
// statement that omits it cannot explain the pair.
type StatementAccount struct {
	AccountID        shared.ID `json:"account_id"`
	EnrollmentID     shared.ID `json:"enrollment_id"`
	AcademicYearID   shared.ID `json:"academic_year_id"`
	AcademicYearCode string    `json:"academic_year_code"`
	CollegeName      string    `json:"college_name"`
	DepartmentName   string    `json:"department_name"`
	StudyTypeName    string    `json:"study_type_name"`
	Stage            int16     `json:"stage"`
	AttemptNumber    int16     `json:"attempt_number"`
	EnrollmentKind   string    `json:"enrollment_kind"`
	EnrollmentStatus string    `json:"enrollment_status"`
	AccountStatus    string    `json:"account_status"`

	GrossTotal       money.Amount `json:"gross_total"`
	DiscountableBase money.Amount `json:"discountable_base"`
	DiscountTotal    money.Amount `json:"discount_total"`
	NetSnapshot      money.Amount `json:"net_snapshot"`
	AdjustmentTotal  money.Amount `json:"adjustment_total"`
	EffectiveNet     money.Amount `json:"effective_net"`
	PaidGross        money.Amount `json:"paid_gross"`
	RefundedTotal    money.Amount `json:"refunded_total"`
	NetPaid          money.Amount `json:"net_paid"`
	CreditBalance    money.Amount `json:"credit_balance"`
	Remaining        money.Amount `json:"remaining"`

	FeeComponents []StatementFeeComponent `json:"fee_components"`
	Discounts     []StatementDiscount     `json:"discounts"`
	Adjustments   []StatementAdjustment   `json:"adjustments"`
	Installments  []StatementInstallment  `json:"installments"`
	Payments      []StatementPayment      `json:"payments"`
	Refunds       []StatementRefund       `json:"refunds"`
}

// StatementFeeComponent is one line of the fee frozen onto the account when it
// was generated. Later price changes never reach it.
type StatementFeeComponent struct {
	ComponentCode  string       `json:"component_code"`
	NameAr         string       `json:"name_ar"`
	Amount         money.Amount `json:"amount"`
	IsDiscountable bool         `json:"is_discountable"`
	IsRefundable   bool         `json:"is_refundable"`
}

// StatementDiscount is one discount materialised onto the account.
//
// Computed and applied are both shown. Where they differ a cap or the
// discountable floor cut the grant short, and the reason is named rather than
// left as an unexplained shortfall between the grant and the relief.
type StatementDiscount struct {
	ApplicationID    shared.ID    `json:"application_id"`
	DefinitionCode   string       `json:"definition_code"`
	DefinitionName   string       `json:"definition_name"`
	Category         string       `json:"category"`
	VersionNo        int          `json:"version_no"`
	ValueType        string       `json:"value_type"`
	FrozenBase       money.Amount `json:"frozen_base"`
	ComputedAmount   money.Amount `json:"computed_amount"`
	AppliedAmount    money.Amount `json:"applied_amount"`
	TruncationReason *string      `json:"truncation_reason,omitempty"`
	Status           string       `json:"status"`
	AppliedAt        *time.Time   `json:"applied_at,omitempty"`
}

// StatementAdjustment is one signed change posted against the account since it
// was generated. The list is what makes the gap between the frozen net and the
// effective net readable.
type StatementAdjustment struct {
	ID       shared.ID    `json:"id"`
	Type     string       `json:"type"`
	Amount   money.Amount `json:"amount"`
	Reason   string       `json:"reason"`
	PostedAt time.Time    `json:"posted_at"`
}

// StatementInstallment is one scheduled obligation with its overdue state
// derived for today rather than read from a stored flag.
type StatementInstallment struct {
	InstallmentID shared.ID    `json:"installment_id"`
	Number        int16        `json:"number"`
	DueDate       shared.Date  `json:"due_date"`
	Amount        money.Amount `json:"amount"`
	AllocatedPaid money.Amount `json:"allocated_paid"`
	Remaining     money.Amount `json:"remaining"`
	Status        string       `json:"status"`
	IsOverdue     bool         `json:"is_overdue"`
	DaysOverdue   int          `json:"days_overdue"`
}

// StatementPayment is one collection. Voided payments are listed with their
// void stamp: a receipt the student holds must be explainable even after it has
// been reversed.
type StatementPayment struct {
	PaymentID   shared.ID    `json:"payment_id"`
	ReceiptNo   *string      `json:"receipt_no,omitempty"`
	Amount      money.Amount `json:"amount"`
	MethodCode  string       `json:"method_code"`
	Status      string       `json:"status"`
	PaidAt      time.Time    `json:"paid_at"`
	PostedAt    *time.Time   `json:"posted_at,omitempty"`
	VoidedAt    *time.Time   `json:"voided_at,omitempty"`
	VoidReason  *string      `json:"void_reason,omitempty"`
	CashierName string       `json:"cashier_name"`
	PayerName   *string      `json:"payer_name,omitempty"`
}

// StatementRefund is money returned, at whatever stage the request reached.
type StatementRefund struct {
	RefundID    shared.ID    `json:"refund_id"`
	RefundNo    *string      `json:"refund_no,omitempty"`
	ReceiptNo   *string      `json:"original_receipt_no,omitempty"`
	Amount      money.Amount `json:"amount"`
	Reason      string       `json:"reason"`
	Status      string       `json:"status"`
	RequestedAt time.Time    `json:"requested_at"`
	PostedAt    *time.Time   `json:"posted_at,omitempty"`
}

// StatementTotals is the student's position across every account on the
// statement.
type StatementTotals struct {
	AccountCount  int          `json:"account_count"`
	GrossTotal    money.Amount `json:"gross_total"`
	DiscountTotal money.Amount `json:"discount_total"`
	EffectiveNet  money.Amount `json:"effective_net"`
	NetPaid       money.Amount `json:"net_paid"`
	Remaining     money.Amount `json:"remaining"`
	// CreditBalance is money the university holds that the student has not
	// spent. It is reported beside the debt and never netted against it: an
	// overpaid year does not settle another year's balance.
	CreditBalance money.Amount `json:"credit_balance"`
}

// ---------------------------------------------------------------------------
// Aggregates
// ---------------------------------------------------------------------------

// SummaryTotals is the money block every aggregate report shares.
//
// StudentCount and AccountCount come from different populations on purpose,
// and the gap between them is informative: it is the supersede and
// regeneration population, the accounts that carry money for a seat counted
// somewhere else.
type SummaryTotals struct {
	StudentCount  int64        `json:"student_count"`
	AccountCount  int64        `json:"account_count"`
	GrossTotal    money.Amount `json:"gross_total"`
	DiscountTotal money.Amount `json:"discount_total"`
	EffectiveNet  money.Amount `json:"effective_net"`
	PaidTotal     money.Amount `json:"paid_total"`
	Refunded      money.Amount `json:"refunded_total"`
	Remaining     money.Amount `json:"remaining"`
	// CollectionRatePct is null where nothing is owed, which is not the same
	// fact as nothing having been collected.
	CollectionRatePct *float64 `json:"collection_rate_pct"`
}

// CollegeSummary is one faculty's block of the department summary.
type CollegeSummary struct {
	CollegeID   shared.ID `json:"college_id"`
	CollegeCode string    `json:"college_code"`
	CollegeName string    `json:"college_name"`
	SummaryTotals
	// Departments is empty in the year summary, where the college breakdown is
	// a header and the department detail is its own report.
	Departments []DepartmentSummary `json:"departments,omitempty"`
}

// DepartmentSummary is one programme's line.
type DepartmentSummary struct {
	DepartmentID   shared.ID `json:"department_id"`
	DepartmentCode string    `json:"department_code"`
	DepartmentName string    `json:"department_name"`
	SummaryTotals
}

// StudyTypeSummary is the same aggregate regrouped by mode of study, where the
// prices differ most.
type StudyTypeSummary struct {
	StudyTypeID   shared.ID `json:"study_type_id"`
	StudyTypeCode string    `json:"study_type_code"`
	StudyTypeName string    `json:"study_type_name"`
	SummaryTotals
}

// StageSummary is the aggregate regrouped by year of study.
//
// Repeat students are broken out because they are priced differently: a stage
// total that blends them explains neither the head count nor the revenue.
type StageSummary struct {
	Stage int16 `json:"stage"`
	SummaryTotals
	RepeatStudentCount int64        `json:"repeat_student_count"`
	RepeatEffectiveNet money.Amount `json:"repeat_effective_net"`
	RepeatPaid         money.Amount `json:"repeat_paid"`
}

// YearSummary is one academic year on a page.
type YearSummary struct {
	AcademicYearID   shared.ID          `json:"academic_year_id"`
	AcademicYearCode string             `json:"academic_year_code"`
	Status           string             `json:"status"`
	Totals           SummaryTotals      `json:"totals"`
	Colleges         []CollegeSummary   `json:"colleges"`
	StudyTypes       []StudyTypeSummary `json:"study_types"`
	Stages           []StageSummary     `json:"stages"`
	// PriorYearCollection is reported on its own line and is never folded into
	// the year's net. It is cash that arrived this year against an obligation
	// another year raised; adding it to this year's collection would overstate
	// the collection rate and understate the debt still standing.
	PriorYearCollection PriorYearCollection `json:"prior_year_collection"`
}

// PriorYearCollection is money taken during a year against accounts belonging
// to earlier ones.
type PriorYearCollection struct {
	PaymentCount int64        `json:"payment_count"`
	Collected    money.Amount `json:"collected"`
	Refunded     money.Amount `json:"refunded"`
	NetCollected money.Amount `json:"net_collected"`
}

// ---------------------------------------------------------------------------
// Installments, debt and ageing
// ---------------------------------------------------------------------------

// InstallmentTotals is what one due period is expected to bring in and what it
// has brought in so far.
type InstallmentTotals struct {
	InstallmentCount int64        `json:"installment_count"`
	Expected         money.Amount `json:"expected"`
	Paid             money.Amount `json:"paid"`
	Remaining        money.Amount `json:"remaining"`
	OverdueCount     int64        `json:"overdue_count"`
	OverdueAmount    money.Amount `json:"overdue_amount"`
	PctCollected     *float64     `json:"pct_collected"`
}

// InstallmentMonth is one due month with its departmental split.
type InstallmentMonth struct {
	Month     string      `json:"month"`
	MonthDate shared.Date `json:"month_start"`
	InstallmentTotals
	Departments []InstallmentDepartment `json:"departments"`
}

// InstallmentDepartment is one programme's share of a due month.
type InstallmentDepartment struct {
	DepartmentID   shared.ID `json:"department_id"`
	DepartmentName string    `json:"department_name"`
	InstallmentTotals
}

// DebtRow is one student's outstanding balance on one account.
//
// The contact details travel with the row because the report is worked from a
// phone: a collection list without the mother's name and a number is a list
// nobody can act on.
type DebtRow struct {
	AccountID        shared.ID    `json:"account_id"`
	StudentID        shared.ID    `json:"student_id"`
	StudentNo        string       `json:"student_no"`
	FullName         string       `json:"full_name"`
	MotherName       string       `json:"mother_name"`
	Phone            *string      `json:"phone,omitempty"`
	AcademicYearID   shared.ID    `json:"academic_year_id"`
	AcademicYearCode string       `json:"academic_year_code"`
	DepartmentID     shared.ID    `json:"department_id"`
	DepartmentName   string       `json:"department_name"`
	Stage            int16        `json:"stage"`
	EffectiveNet     money.Amount `json:"effective_net"`
	NetPaid          money.Amount `json:"net_paid"`
	Remaining        money.Amount `json:"remaining"`
	IsPriorYear      bool         `json:"is_prior_year"`
	OldestOverdueDue *shared.Date `json:"oldest_overdue_due_date,omitempty"`
	OverdueAmount    money.Amount `json:"overdue_amount"`
	OverdueCount     int64        `json:"overdue_installments"`
}

// AgingRow is one department's receivables sorted by how long they have been
// owed.
//
// A debt on a closed year lands in PriorYear whatever its due date: once the
// books are shut the age of the individual installment stops being the useful
// fact, and "last year's money" is the line the finance office chases.
type AgingRow struct {
	DepartmentID    shared.ID    `json:"department_id"`
	DepartmentName  string       `json:"department_name"`
	NotYetDue       money.Amount `json:"not_yet_due"`
	Days0To30       money.Amount `json:"days_0_30"`
	Days31To90      money.Amount `json:"days_31_90"`
	Days91To180     money.Amount `json:"days_91_180"`
	Days180Plus     money.Amount `json:"days_180_plus"`
	PriorYear       money.Amount `json:"prior_year"`
	TotalOverdue    money.Amount `json:"total_overdue"`
	TotalReceivable money.Amount `json:"total_receivable"`
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

// DiscountUsageRow is what one version of one discount actually cost.
//
// Read from the applications frozen onto accounts, never recomputed from the
// definition: a definition may have been revised three times since, and the
// frozen amount is the money that was actually forgone.
type DiscountUsageRow struct {
	DefinitionID     shared.ID     `json:"definition_id"`
	DefinitionCode   string        `json:"definition_code"`
	DefinitionName   string        `json:"definition_name"`
	Category         string        `json:"category"`
	VersionNo        int           `json:"version_no"`
	ValueType        string        `json:"value_type"`
	ValueBP          *int          `json:"value_bp,omitempty"`
	ValueAmount      *money.Amount `json:"value_amount,omitempty"`
	StudentCount     int64         `json:"student_count"`
	ApplicationCount int64         `json:"application_count"`
	TotalDiscount    money.Amount  `json:"total_discount"`
	TruncatedCount   int64         `json:"truncated_count"`
	// PctOfGross is the share of the year's gross fees this discount gave away.
	PctOfGross *float64 `json:"pct_of_gross"`
}

// ExemptionRow is one application that relieved a student of the whole charge.
//
// Either the definition is a declared full exemption, or the applied amount
// consumed the entire discountable base. Both are the same fact to the
// ministry, which asks how many students paid nothing and on whose signature.
type ExemptionRow struct {
	ApplicationID    shared.ID    `json:"application_id"`
	StudentID        shared.ID    `json:"student_id"`
	StudentNo        string       `json:"student_no"`
	FullName         string       `json:"full_name"`
	MotherName       string       `json:"mother_name"`
	AcademicYearID   shared.ID    `json:"academic_year_id"`
	AcademicYearCode string       `json:"academic_year_code"`
	DepartmentName   string       `json:"department_name"`
	DefinitionCode   string       `json:"definition_code"`
	DefinitionName   string       `json:"definition_name"`
	Category         string       `json:"category"`
	IsFullExemption  bool         `json:"is_full_exemption"`
	VersionNo        int          `json:"version_no"`
	DiscountableBase money.Amount `json:"discountable_base"`
	AppliedAmount    money.Amount `json:"applied_amount"`
	ApproverName     *string      `json:"approver_name,omitempty"`
	ApprovedAt       *time.Time   `json:"approved_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Cash
// ---------------------------------------------------------------------------

// CashierDayRow is one cashier's movement in one method on one day.
type CashierDayRow struct {
	CashierUserID shared.ID   `json:"cashier_user_id"`
	CashierName   string      `json:"cashier_name"`
	Date          shared.Date `json:"date"`
	MethodCode    string      `json:"method_code"`
	IsCash        bool        `json:"is_cash"`
	PaymentCount  int64       `json:"payment_count"`
	// PaymentTotal already excludes voided receipts, which carry their own
	// status and are reported separately below.
	PaymentTotal money.Amount `json:"payment_total"`
	VoidCount    int64        `json:"void_count"`
	VoidTotal    money.Amount `json:"void_total"`
	// ExpectedCash is what the drawer should hold for this method on this day,
	// which is zero for anything that never became cash.
	ExpectedCash money.Amount `json:"expected_cash"`
}

// CollectionTrend is how a year's money arrived, over time and by programme.
type CollectionTrend struct {
	AcademicYearID shared.ID              `json:"academic_year_id"`
	EffectiveNet   money.Amount           `json:"effective_net"`
	Months         []CollectionMonth      `json:"months"`
	Departments    []CollectionDepartment `json:"departments"`
}

// CollectionMonth is one month of movement against the year's obligation.
type CollectionMonth struct {
	Month         string       `json:"month"`
	MonthDate     shared.Date  `json:"month_start"`
	PaymentCount  int64        `json:"payment_count"`
	Collected     money.Amount `json:"collected"`
	Refunded      money.Amount `json:"refunded"`
	NetCollected  money.Amount `json:"net_collected"`
	CumulativeNet money.Amount `json:"cumulative_net"`
	PctOfNet      *float64     `json:"pct_of_net"`
}

// CollectionDepartment is one programme's share of the year's collection.
type CollectionDepartment struct {
	DepartmentID   shared.ID    `json:"department_id"`
	DepartmentName string       `json:"department_name"`
	EffectiveNet   money.Amount `json:"effective_net"`
	Paid           money.Amount `json:"paid"`
	Remaining      money.Amount `json:"remaining"`
	PctCollected   *float64     `json:"pct_collected"`
}

// CashFlowMonth is the money a future month is scheduled to bring in.
type CashFlowMonth struct {
	Month            string               `json:"month"`
	MonthDate        shared.Date          `json:"month_start"`
	InstallmentCount int64                `json:"installment_count"`
	Expected         money.Amount         `json:"expected"`
	Departments      []CashFlowDepartment `json:"departments"`
}

// CashFlowDepartment is one programme's share of a projected month.
type CashFlowDepartment struct {
	DepartmentID     shared.ID    `json:"department_id"`
	DepartmentName   string       `json:"department_name"`
	InstallmentCount int64        `json:"installment_count"`
	Expected         money.Amount `json:"expected"`
}

// ---------------------------------------------------------------------------
// Registers
// ---------------------------------------------------------------------------

// VoidRow is one reversed collection.
//
// GapHours — the time between posting the receipt and reversing it — is the
// column this report exists for. A void raised within minutes is a cashier
// fixing a typo; one raised days later, against a receipt the student is
// holding, is the shape of a cashier pocketing cash and erasing the record.
// CrossedDay marks the reversals that should have been refunds, because a
// drawer that has already been counted cannot give money back through a void.
type VoidRow struct {
	PaymentID     shared.ID    `json:"payment_id"`
	ReceiptNo     *string      `json:"receipt_no,omitempty"`
	StudentID     shared.ID    `json:"student_id"`
	StudentNo     string       `json:"student_no"`
	FullName      string       `json:"full_name"`
	Amount        money.Amount `json:"amount"`
	MethodCode    string       `json:"method_code"`
	PostedAt      time.Time    `json:"posted_at"`
	VoidedAt      time.Time    `json:"voided_at"`
	GapHours      float64      `json:"gap_hours"`
	CrossedDay    bool         `json:"crossed_day"`
	Reason        string       `json:"reason"`
	CashierName   string       `json:"cashier_name"`
	RequestedBy   *string      `json:"requested_by,omitempty"`
	RequestedAt   *time.Time   `json:"requested_at,omitempty"`
	ExecutedBy    *string      `json:"executed_by,omitempty"`
	VoidRequestID *shared.ID   `json:"void_request_id,omitempty"`
}

// RefundRow is one payment returned to a student.
type RefundRow struct {
	RefundID        shared.ID    `json:"refund_id"`
	RefundNo        *string      `json:"refund_no,omitempty"`
	PostedAt        time.Time    `json:"posted_at"`
	OriginalReceipt *string      `json:"original_receipt_no,omitempty"`
	StudentID       shared.ID    `json:"student_id"`
	StudentNo       string       `json:"student_no"`
	FullName        string       `json:"full_name"`
	Amount          money.Amount `json:"amount"`
	MethodCode      string       `json:"method_code"`
	Reason          string       `json:"reason"`
	RequestedBy     string       `json:"requested_by"`
	ApprovedBy      *string      `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time   `json:"approved_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// ReportRepository reads the reporting model.
//
// Every method is a read. None of them takes a lock, none of them writes, and
// none of them opens a transaction of its own: a caller that needs several
// reports to agree on one snapshot wraps them in a read transaction, and a
// caller printing one report does not pay for a boundary it does not need.
type ReportRepository interface {
	// StudentStatement assembles one student's whole file from transaction
	// rows. Passing a year narrows it to that year's accounts.
	StudentStatement(ctx context.Context, studentID shared.ID, yearID *shared.ID) (*StudentStatement, error)

	// DepartmentSummary aggregates a year, grouped college then department.
	DepartmentSummary(ctx context.Context, f SummaryFilter) ([]CollegeSummary, error)
	// StudyTypeSummary regroups the same aggregate by mode of study.
	StudyTypeSummary(ctx context.Context, f SummaryFilter) ([]StudyTypeSummary, error)
	// StageSummary regroups the same aggregate by stage, with repeat students
	// broken out.
	StageSummary(ctx context.Context, f SummaryFilter) ([]StageSummary, error)
	// YearSummary is the year's header with its three breakdowns and the debt
	// of earlier years collected during it.
	YearSummary(ctx context.Context, yearID shared.ID) (*YearSummary, error)

	// InstallmentReport reports expected against collected by due month.
	InstallmentReport(ctx context.Context, f InstallmentFilter) ([]InstallmentMonth, error)
	// DebtReport lists outstanding balances, newest debt first, with the total
	// matching the filter.
	DebtReport(ctx context.Context, f DebtFilter) ([]DebtRow, int, error)
	// AgingReport buckets receivables by how long they have been owed.
	AgingReport(ctx context.Context, f AgingFilter) ([]AgingRow, error)

	// DiscountReport reports what discounts cost, per definition and version.
	DiscountReport(ctx context.Context, f DiscountFilter) ([]DiscountUsageRow, error)
	// ExemptionRegister lists the students who were relieved of the whole
	// charge, and who signed for it.
	ExemptionRegister(ctx context.Context, f RegisterFilter) ([]ExemptionRow, int, error)

	// CashierDaily reports cash movement per cashier, day and method.
	CashierDaily(ctx context.Context, f CashierDailyFilter) ([]CashierDayRow, error)
	// CollectionTrend reports collection against obligation over the months of
	// a year and across its departments.
	CollectionTrend(ctx context.Context, f TrendFilter) (*CollectionTrend, error)
	// ExpectedCashFlow projects the inflow the remaining due dates imply.
	ExpectedCashFlow(ctx context.Context, f CashFlowFilter) ([]CashFlowMonth, error)

	// VoidRegister lists every reversed collection, widest time gap first.
	VoidRegister(ctx context.Context, f RegisterFilter) ([]VoidRow, int, error)
	// RefundRegister lists every posted refund with its two signatures.
	RefundRegister(ctx context.Context, f RegisterFilter) ([]RefundRow, int, error)
}
