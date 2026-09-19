package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/adapter/export"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// TestEveryExportTableIsRectangular is the guard the spreadsheet writers do not
// have.
//
// A row with fewer cells than the table has columns produces a CSV that still
// parses and an XLSX that still opens — with every figure after the short row
// shifted one column left. Nothing refuses it, and what a finance officer then
// reads as "collected" is the discount total. Each builder is exercised with
// rows that carry the awkward cases the real data has: an absent receipt
// number, a null collection rate, a voided payment, a truncated discount.
func TestEveryExportTableIsRectangular(t *testing.T) {
	c := exportContext(t)

	tables := map[string]export.Table{
		"debt":         debtTable(c, []port.DebtRow{sampleDebtRow()}),
		"aging":        agingTable(c, []port.AgingRow{{DepartmentName: "هندسة البرمجيات"}}),
		"installments": installmentTable(c, []port.InstallmentMonth{{Month: "2025-10"}}),
		"departments":  departmentTable(c, sampleColleges()),
		"study types":  studyTypeTable(c, []port.StudyTypeSummary{{StudyTypeName: "صباحي"}}),
		"stages":       stageTable(c, []port.StageSummary{{Stage: 3}}),
		"year":         yearSummaryTable(c, sampleYearSummary()),
		"discounts":    discountUsageTable(c, sampleDiscountRows()),
		"exemptions":   exemptionTable(c, []port.ExemptionRow{{StudentNo: "2024001"}}),
		"cashier":      cashierDailyTable(c, []port.CashierDayRow{{CashierName: "علي"}}),
		"trend":        collectionTrendTable(c, sampleTrend()),
		"cash flow":    cashFlowTable(c, sampleCashFlow()),
		"voids":        voidTable(c, []port.VoidRow{{StudentNo: "2024001", GapHours: 51.5}}),
		"refunds":      refundTable(c, []port.RefundRow{{StudentNo: "2024001"}}),
		"statement":    statementTable(c, sampleStatement()),
	}

	for name, table := range tables {
		if len(table.Columns) == 0 {
			t.Errorf("%s: the table declares no columns", name)
		}
		if len(table.Rows) == 0 {
			t.Errorf("%s: the sample produced no rows, so nothing here was checked", name)
		}
		for i, row := range table.Rows {
			if len(row) != len(table.Columns) {
				t.Errorf("%s: row %d has %d cells against %d columns — every figure after it "+
					"lands under the wrong heading", name, i, len(row), len(table.Columns))
			}
		}
		if strings.TrimSpace(table.Title) == "" {
			t.Errorf("%s: the table has no title; it names the sheet and the downloaded file", name)
		}
	}
}

// TestStatementExportCarriesWhatExplainsTheBalance: the statement is the
// document handed to a student who disputes a figure, so the export has to
// carry the lines that produce it rather than the figure alone — including the
// voided receipt the student is still holding, which is usually the reason the
// paper and the balance disagree.
func TestStatementExportCarriesWhatExplainsTheBalance(t *testing.T) {
	table := statementTable(exportContext(t), sampleStatement())

	var joined strings.Builder
	for _, row := range table.Rows {
		joined.WriteString(strings.Join(row, "|"))
		joined.WriteString("\n")
	}
	rendered := joined.String()

	for _, want := range []string{
		"Fee",         // the frozen components
		"Discount",    // with the reason it was cut short
		"Adjustment",  // what moved the frozen net
		"Installment", // the schedule
		"Payment",     // every collection
		"Refund",
		"TUITION",
		"cap reached",
		"voided",
		"R-1001",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the statement export omits %q", want)
		}
	}

	// The totals belong in the subtitle, not in a trailing row: a total row in
	// a spreadsheet is a row somebody's SUM counts twice.
	if !strings.Contains(table.Subtitle, "Outstanding") {
		t.Errorf("the statement subtitle does not state the position: %q", table.Subtitle)
	}
	for _, row := range table.Rows {
		if strings.EqualFold(row[2], "total") {
			t.Error("the statement export carries a total row, which double-counts under SUM")
		}
	}
}

// TestPercentTextDistinguishesNothingOwedFromNothingCollected: a null
// collection rate means nothing was owed, and a zero in that cell is read as
// the second — a department that collected none of what it charged.
func TestPercentTextDistinguishesNothingOwedFromNothingCollected(t *testing.T) {
	if got := percentText(nil); got != "" {
		t.Errorf("a null rate rendered as %q; it must stay empty", got)
	}
	zero := 0.0
	if got := percentText(&zero); got != "0.00" {
		t.Errorf("a zero rate rendered as %q", got)
	}
}

// exportContext builds a request context the table builders can read their
// filters from. They only ever read the query string.
func exportContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/reports/debt?academic_year_id=2025", nil)
	return c
}

func sampleDebtRow() port.DebtRow {
	return port.DebtRow{
		StudentNo:        "2024001",
		FullName:         "محمد علي حسن",
		MotherName:       "زينب",
		AcademicYearCode: "2025-2026",
		DepartmentName:   "هندسة البرمجيات",
		Stage:            2,
		EffectiveNet:     money.FromInt64(3_000_000),
		NetPaid:          money.FromInt64(1_000_000),
		Remaining:        money.FromInt64(2_000_000),
	}
}

func sampleColleges() []port.CollegeSummary {
	return []port.CollegeSummary{{
		CollegeName: "كلية الهندسة",
		Departments: []port.DepartmentSummary{{DepartmentName: "هندسة البرمجيات"}},
	}}
}

func sampleYearSummary() *port.YearSummary {
	rate := 62.5
	return &port.YearSummary{
		AcademicYearCode: "2025-2026",
		Status:           "open",
		Totals:           port.SummaryTotals{StudentCount: 20, CollectionRatePct: &rate},
		Colleges:         sampleColleges(),
		StudyTypes:       []port.StudyTypeSummary{{StudyTypeName: "صباحي"}},
		Stages:           []port.StageSummary{{Stage: 1}},
		PriorYearCollection: port.PriorYearCollection{
			PaymentCount: 4,
			Collected:    money.FromInt64(500_000),
			NetCollected: money.FromInt64(500_000),
		},
	}
}

func sampleDiscountRows() []port.DiscountUsageRow {
	bp := 2500
	fixed := money.FromInt64(250_000)
	return []port.DiscountUsageRow{
		{DefinitionCode: "SIBLING", ValueType: "percentage", ValueBP: &bp},
		{DefinitionCode: "STAFF", ValueType: "fixed", ValueAmount: &fixed},
		// Neither: a definition whose value lives only in its versions. The
		// cell stays empty rather than printing a zero somebody quotes.
		{DefinitionCode: "TOP_STUDENT", ValueType: "full_exemption"},
	}
}

func sampleTrend() *port.CollectionTrend {
	return &port.CollectionTrend{
		EffectiveNet: money.FromInt64(9_000_000),
		Months:       []port.CollectionMonth{{Month: "2025-10"}},
		Departments:  []port.CollectionDepartment{{DepartmentName: "هندسة البرمجيات"}},
	}
}

func sampleCashFlow() []port.CashFlowMonth {
	return []port.CashFlowMonth{{
		Month:       "2026-01",
		Departments: []port.CashFlowDepartment{{DepartmentName: "هندسة البرمجيات"}},
	}}
}

func sampleStatement() *port.StudentStatement {
	voidedAt := time.Date(2025, 11, 2, 9, 30, 0, 0, time.UTC)
	reason := "cap reached"
	receipt := "R-1001"
	refundNo := "RF-7"
	return &port.StudentStatement{
		Student: port.StatementStudent{StudentNo: "2024001", FullName: "محمد علي حسن"},
		Accounts: []port.StatementAccount{{
			AcademicYearCode: "2025-2026",
			DepartmentName:   "هندسة البرمجيات",
			StudyTypeName:    "صباحي",
			Stage:            2,
			AttemptNumber:    1,
			AccountStatus:    "active",
			FeeComponents: []port.StatementFeeComponent{
				{ComponentCode: "TUITION", NameAr: "القسط الدراسي", Amount: money.FromInt64(3_000_000)},
			},
			Discounts: []port.StatementDiscount{{
				DefinitionCode:   "SIBLING",
				DefinitionName:   "خصم الأشقاء",
				ComputedAmount:   money.FromInt64(500_000),
				AppliedAmount:    money.FromInt64(300_000),
				TruncationReason: &reason,
				Status:           "applied",
			}},
			Adjustments: []port.StatementAdjustment{{
				Type: "correction", Amount: money.FromInt64(-50_000), Reason: "restated fee",
			}},
			Installments: []port.StatementInstallment{{
				Number: 1, DueDate: shared.NewDate(2025, time.November, 1),
				Amount: money.FromInt64(1_000_000), Status: "partial",
				IsOverdue: true, DaysOverdue: 12,
			}},
			Payments: []port.StatementPayment{{
				ReceiptNo:   &receipt,
				Amount:      money.FromInt64(1_000_000),
				MethodCode:  "CASH",
				Status:      "voided",
				CashierName: "علي",
				VoidedAt:    &voidedAt,
				VoidReason:  &reason,
			}},
			Refunds: []port.StatementRefund{{
				RefundNo: &refundNo, Amount: money.FromInt64(200_000),
				Reason: "withdrawal", Status: "posted",
			}},
		}},
		Totals: port.StatementTotals{
			EffectiveNet: money.FromInt64(2_650_000),
			NetPaid:      money.FromInt64(800_000),
			Remaining:    money.FromInt64(1_850_000),
		},
	}
}
