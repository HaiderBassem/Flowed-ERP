package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/adapter/export"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// exportRequested reports whether the caller asked for a file rather than JSON.
//
// A query parameter rather than content negotiation: the link a finance officer
// clicks is a link, and Accept headers are not something anyone can put in a
// bookmark.
func exportRequested(c *gin.Context) bool {
	return strings.TrimSpace(c.Query("format")) != ""
}

// writeExport renders a table and sends it as a download.
//
// The rows have already been produced under the caller's own authority and
// scope — this only changes the encoding, so an export can never widen what
// somebody may see. That is why every export path here takes rows rather than
// filters.
func writeExport(c *gin.Context, table export.Table) {
	format, err := export.Parse(c.Query("format"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	actor, _ := httpx.ActorFrom(c)
	table.GeneratedBy = actor.Username
	if table.GeneratedAt.IsZero() {
		table.GeneratedAt = time.Now().UTC()
	}

	c.Header("Content-Type", format.ContentType())
	// A printable page is meant to be opened and printed; a spreadsheet is
	// meant to be saved. Naming the disposition accordingly is the difference
	// between a working export and a browser showing XML.
	disposition := "attachment"
	if format == export.FormatPDF {
		disposition = "inline"
	}
	c.Header("Content-Disposition",
		fmt.Sprintf("%s; filename=%q", disposition, table.Filename(format)))

	c.Status(http.StatusOK)
	if err := table.Write(c.Writer, format); err != nil {
		// The status is already written; the best that can be done is to stop
		// and let the truncated download fail visibly rather than silently
		// producing half a report somebody files.
		//
		// It is also counted, because this is the one failure the caller sees
		// as a 200: a report that ends halfway through is filed as though it
		// were complete, and nothing else would ever say otherwise.
		httpx.InstrumentsFrom(c).ExportFailure(c.Request.Context(), string(format))
		_ = c.Error(err)
	}
}

// reportSubtitle states what a report covers, for the printed page.
//
// A page with no statement of its filters is a page that will be misread on
// somebody's desk in March, which is exactly when a debt report gets quoted at
// a student.
func reportSubtitle(c *gin.Context, extra ...string) string {
	parts := make([]string, 0, 6)
	for _, param := range []struct{ label, key string }{
		{"Year", "academic_year_id"},
		{"College", "college_id"},
		{"Department", "department_id"},
		{"From", "from"},
		{"To", "to"},
	} {
		if value := strings.TrimSpace(c.Query(param.key)); value != "" {
			parts = append(parts, param.label+" "+value)
		}
	}
	parts = append(parts, extra...)
	if len(parts) == 0 {
		return "All records"
	}
	return strings.Join(parts, " · ")
}

// debtTable renders the outstanding-balance report.
func debtTable(c *gin.Context, rows []port.DebtRow) export.Table {
	table := export.Table{
		Title:    "Debt report",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Student no"},
			{Header: "Student"},
			{Header: "Mother"},
			{Header: "Year"},
			{Header: "Department"},
			{Header: "Stage", Numeric: true},
			{Header: "Charged", Numeric: true},
			{Header: "Paid", Numeric: true},
			{Header: "Outstanding", Numeric: true},
			{Header: "Overdue", Numeric: true},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			row.StudentNo,
			row.FullName,
			// The discriminator a registrar uses when four-part names repeat.
			row.MotherName,
			row.AcademicYearCode,
			row.DepartmentName,
			fmt.Sprintf("%d", row.Stage),
			amountText(row.EffectiveNet),
			amountText(row.NetPaid),
			amountText(row.Remaining),
			amountText(row.OverdueAmount),
		})
	}
	return table
}

// agingTable renders the receivables-ageing report.
func agingTable(c *gin.Context, rows []port.AgingRow) export.Table {
	table := export.Table{
		Title:    "Receivables ageing",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Department"},
			{Header: "Not yet due", Numeric: true},
			{Header: "0-30 days", Numeric: true},
			{Header: "31-90 days", Numeric: true},
			{Header: "91-180 days", Numeric: true},
			{Header: "Over 180 days", Numeric: true},
			{Header: "Prior years", Numeric: true},
			{Header: "Total overdue", Numeric: true},
			{Header: "Total receivable", Numeric: true},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			row.DepartmentName,
			amountText(row.NotYetDue),
			amountText(row.Days0To30),
			amountText(row.Days31To90),
			amountText(row.Days91To180),
			amountText(row.Days180Plus),
			amountText(row.PriorYear),
			amountText(row.TotalOverdue),
			amountText(row.TotalReceivable),
		})
	}
	return table
}

// installmentTable renders expected against collected by due month.
func installmentTable(c *gin.Context, rows []port.InstallmentMonth) export.Table {
	table := export.Table{
		Title:    "Installments by month",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Month"},
			{Header: "Installments", Numeric: true},
			{Header: "Expected", Numeric: true},
			{Header: "Collected", Numeric: true},
			{Header: "Remaining", Numeric: true},
			{Header: "Overdue", Numeric: true},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			row.Month,
			fmt.Sprintf("%d", row.InstallmentCount),
			amountText(row.Expected),
			amountText(row.Paid),
			amountText(row.Remaining),
			amountText(row.OverdueAmount),
		})
	}
	return table
}

// departmentTable renders the year's aggregate, college then department.
func departmentTable(c *gin.Context, colleges []port.CollegeSummary) export.Table {
	table := export.Table{
		Title:    "Departments summary",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "College"},
			{Header: "Department"},
			{Header: "Students", Numeric: true},
			{Header: "Charged", Numeric: true},
			{Header: "Discount", Numeric: true},
			{Header: "Collected", Numeric: true},
			{Header: "Outstanding", Numeric: true},
		},
	}
	for _, college := range colleges {
		for _, department := range college.Departments {
			table.Rows = append(table.Rows, []string{
				college.CollegeName,
				department.DepartmentName,
				fmt.Sprintf("%d", department.StudentCount),
				amountText(department.EffectiveNet),
				amountText(department.DiscountTotal),
				amountText(department.PaidTotal),
				amountText(department.Remaining),
			})
		}
	}
	return table
}

// studyTypeTable renders the year's aggregate regrouped by mode of study.
func studyTypeTable(c *gin.Context, rows []port.StudyTypeSummary) export.Table {
	table := export.Table{
		Title:    "Study types summary",
		Subtitle: reportSubtitle(c),
		Columns: append([]export.Column{{Header: "Study type"}, {Header: "Code"}},
			summaryColumns()...),
	}
	for _, row := range rows {
		table.Rows = append(table.Rows,
			append([]string{row.StudyTypeName, row.StudyTypeCode}, summaryCells(row.SummaryTotals)...))
	}
	return table
}

// stageTable renders the aggregate by year of study.
//
// The repeat columns are carried rather than folded in, for the reason the port
// type gives: repeat students are priced differently, and a stage total that
// blends them explains neither the head count nor the revenue.
func stageTable(c *gin.Context, rows []port.StageSummary) export.Table {
	table := export.Table{
		Title:    "Stages summary",
		Subtitle: reportSubtitle(c),
		Columns: append(append([]export.Column{{Header: "Stage", Numeric: true}},
			summaryColumns()...),
			export.Column{Header: "Repeat students", Numeric: true},
			export.Column{Header: "Repeat charged", Numeric: true},
			export.Column{Header: "Repeat paid", Numeric: true}),
	}
	for _, row := range rows {
		cells := append([]string{fmt.Sprintf("%d", row.Stage)}, summaryCells(row.SummaryTotals)...)
		table.Rows = append(table.Rows, append(cells,
			countText(row.RepeatStudentCount),
			amountText(row.RepeatEffectiveNet),
			amountText(row.RepeatPaid)))
	}
	return table
}

// yearSummaryTable renders one academic year on a page.
//
// The year summary is four aggregates and a standing line rather than one list,
// so the export carries a section column and keeps them in one sheet. Splitting
// them into four downloads would be four files nobody keeps together, and the
// prior-year line is exactly the one that must not be read apart from the
// year's own figures.
func yearSummaryTable(c *gin.Context, summary *port.YearSummary) export.Table {
	table := export.Table{
		Title: "Year summary",
		Subtitle: reportSubtitle(c, "Year "+summary.AcademicYearCode,
			"Status "+summary.Status),
		Columns: append([]export.Column{{Header: "Section"}, {Header: "Item"}},
			summaryColumns()...),
	}

	add := func(section, item string, totals port.SummaryTotals) {
		table.Rows = append(table.Rows, append([]string{section, item}, summaryCells(totals)...))
	}

	add("Year", summary.AcademicYearCode, summary.Totals)
	for _, college := range summary.Colleges {
		add("College", college.CollegeName, college.SummaryTotals)
		for _, department := range college.Departments {
			add("Department", department.DepartmentName, department.SummaryTotals)
		}
	}
	for _, studyType := range summary.StudyTypes {
		add("Study type", studyType.StudyTypeName, studyType.SummaryTotals)
	}
	for _, stage := range summary.Stages {
		add("Stage", fmt.Sprintf("%d", stage.Stage), stage.SummaryTotals)
	}

	// Never folded into the year's own net — it is cash that arrived this year
	// against an obligation an earlier year raised. It gets its own row with
	// only the two columns it honestly has; the count goes in the label rather
	// than into a head-count column it is not, because a figure under the wrong
	// heading is how this line ends up added to the year's collection.
	prior := summary.PriorYearCollection
	table.Rows = append(table.Rows, []string{
		"Prior-year collection",
		fmt.Sprintf("Against earlier years — %s payments, net %s",
			countText(prior.PaymentCount), amountText(prior.NetCollected)),
		"", "", "", "", "",
		amountText(prior.Collected), amountText(prior.Refunded), "", "",
	})

	return table
}

// discountUsageTable renders what each discount version actually cost.
func discountUsageTable(c *gin.Context, rows []port.DiscountUsageRow) export.Table {
	table := export.Table{
		Title:    "Discount usage",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Code"},
			{Header: "Discount"},
			{Header: "Category"},
			{Header: "Version", Numeric: true},
			{Header: "Value type"},
			{Header: "Value"},
			{Header: "Students", Numeric: true},
			{Header: "Applications", Numeric: true},
			{Header: "Total given up", Numeric: true},
			{Header: "Truncated", Numeric: true},
			{Header: "% of gross", Numeric: true},
		},
	}
	for _, row := range rows {
		value := ""
		switch {
		case row.ValueAmount != nil:
			value = amountText(*row.ValueAmount)
		case row.ValueBP != nil:
			// Basis points as written, not as a percentage: the stored figure is
			// what the discount was computed from, and rounding it here would
			// print a number the recomputation never used.
			value = fmt.Sprintf("%d bp", *row.ValueBP)
		}
		table.Rows = append(table.Rows, []string{
			row.DefinitionCode,
			row.DefinitionName,
			row.Category,
			fmt.Sprintf("%d", row.VersionNo),
			row.ValueType,
			value,
			countText(row.StudentCount),
			countText(row.ApplicationCount),
			amountText(row.TotalDiscount),
			countText(row.TruncatedCount),
			percentText(row.PctOfGross),
		})
	}
	return table
}

// exemptionTable renders the students relieved of the whole charge, and who
// signed for it.
func exemptionTable(c *gin.Context, rows []port.ExemptionRow) export.Table {
	table := export.Table{
		Title:    "Exemption register",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Student no"},
			{Header: "Student"},
			{Header: "Mother"},
			{Header: "Year"},
			{Header: "Department"},
			{Header: "Discount code"},
			{Header: "Discount"},
			{Header: "Category"},
			{Header: "Full exemption"},
			{Header: "Version", Numeric: true},
			{Header: "Discountable base", Numeric: true},
			{Header: "Applied", Numeric: true},
			{Header: "Approved by"},
			{Header: "Approved at"},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			row.StudentNo,
			row.FullName,
			row.MotherName,
			row.AcademicYearCode,
			row.DepartmentName,
			row.DefinitionCode,
			row.DefinitionName,
			row.Category,
			yesNo(row.IsFullExemption),
			fmt.Sprintf("%d", row.VersionNo),
			amountText(row.DiscountableBase),
			amountText(row.AppliedAmount),
			optionalText(row.ApproverName),
			optionalTimeText(row.ApprovedAt),
		})
	}
	return table
}

// cashierDailyTable renders cash movement per cashier, day and method.
//
// This is the sheet a drawer is counted against, so the expected-cash column is
// the one it exists for and voids are carried beside it rather than netted into
// the takings.
func cashierDailyTable(c *gin.Context, rows []port.CashierDayRow) export.Table {
	table := export.Table{
		Title:    "Cashier daily",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Date"},
			{Header: "Cashier"},
			{Header: "Method"},
			{Header: "Cash"},
			{Header: "Payments", Numeric: true},
			{Header: "Collected", Numeric: true},
			{Header: "Voids", Numeric: true},
			{Header: "Voided", Numeric: true},
			{Header: "Expected in drawer", Numeric: true},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			row.Date.String(),
			row.CashierName,
			row.MethodCode,
			yesNo(row.IsCash),
			countText(row.PaymentCount),
			amountText(row.PaymentTotal),
			countText(row.VoidCount),
			amountText(row.VoidTotal),
			amountText(row.ExpectedCash),
		})
	}
	return table
}

// collectionTrendTable renders how a year's money arrived.
//
// Months and departments are two views of one figure, so they share a sheet
// under a section column: the month rows are cumulative against the year's
// obligation and the department rows are that same obligation split, and a
// reader who has both can check one against the other.
func collectionTrendTable(c *gin.Context, trend *port.CollectionTrend) export.Table {
	table := export.Table{
		Title:    "Collection trend",
		Subtitle: reportSubtitle(c, "Obligation "+amountText(trend.EffectiveNet)),
		Columns: []export.Column{
			{Header: "Section"},
			{Header: "Item"},
			{Header: "Payments", Numeric: true},
			{Header: "Collected", Numeric: true},
			{Header: "Refunded", Numeric: true},
			{Header: "Net collected", Numeric: true},
			{Header: "Cumulative net", Numeric: true},
			{Header: "Outstanding", Numeric: true},
			{Header: "% of net", Numeric: true},
		},
	}
	for _, month := range trend.Months {
		table.Rows = append(table.Rows, []string{
			"Month",
			month.Month,
			countText(month.PaymentCount),
			amountText(month.Collected),
			amountText(month.Refunded),
			amountText(month.NetCollected),
			amountText(month.CumulativeNet),
			"",
			percentText(month.PctOfNet),
		})
	}
	for _, department := range trend.Departments {
		table.Rows = append(table.Rows, []string{
			"Department",
			department.DepartmentName,
			"",
			amountText(department.Paid),
			"",
			"",
			"",
			amountText(department.Remaining),
			percentText(department.PctCollected),
		})
	}
	return table
}

// cashFlowTable renders the inflow the remaining due dates imply.
func cashFlowTable(c *gin.Context, rows []port.CashFlowMonth) export.Table {
	table := export.Table{
		Title:    "Expected cash flow",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Month"},
			{Header: "Department"},
			{Header: "Installments", Numeric: true},
			{Header: "Expected", Numeric: true},
		},
	}
	for _, month := range rows {
		table.Rows = append(table.Rows, []string{
			month.Month,
			"All departments",
			countText(month.InstallmentCount),
			amountText(month.Expected),
		})
		for _, department := range month.Departments {
			table.Rows = append(table.Rows, []string{
				month.Month,
				department.DepartmentName,
				countText(department.InstallmentCount),
				amountText(department.Expected),
			})
		}
	}
	return table
}

// voidTable renders the void register.
//
// The gap between posting and reversal leads the columns because it is the
// column the report exists for: minutes is a typo being fixed, days is a
// receipt the student is still holding.
func voidTable(c *gin.Context, rows []port.VoidRow) export.Table {
	table := export.Table{
		Title:    "Void register",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Gap hours", Numeric: true},
			{Header: "Crossed day"},
			{Header: "Receipt no"},
			{Header: "Student no"},
			{Header: "Student"},
			{Header: "Amount", Numeric: true},
			{Header: "Method"},
			{Header: "Posted at"},
			{Header: "Voided at"},
			{Header: "Reason"},
			{Header: "Cashier"},
			{Header: "Requested by"},
			{Header: "Executed by"},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			fmt.Sprintf("%.1f", row.GapHours),
			yesNo(row.CrossedDay),
			optionalText(row.ReceiptNo),
			row.StudentNo,
			row.FullName,
			amountText(row.Amount),
			row.MethodCode,
			timeText(row.PostedAt),
			timeText(row.VoidedAt),
			row.Reason,
			row.CashierName,
			optionalText(row.RequestedBy),
			optionalText(row.ExecutedBy),
		})
	}
	return table
}

// refundTable renders the refund register, both signatures on every row.
func refundTable(c *gin.Context, rows []port.RefundRow) export.Table {
	table := export.Table{
		Title:    "Refund register",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Refund no"},
			{Header: "Posted at"},
			{Header: "Original receipt"},
			{Header: "Student no"},
			{Header: "Student"},
			{Header: "Amount", Numeric: true},
			{Header: "Method"},
			{Header: "Reason"},
			{Header: "Requested by"},
			{Header: "Approved by"},
			{Header: "Approved at"},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			optionalText(row.RefundNo),
			timeText(row.PostedAt),
			optionalText(row.OriginalReceipt),
			row.StudentNo,
			row.FullName,
			amountText(row.Amount),
			row.MethodCode,
			row.Reason,
			row.RequestedBy,
			optionalText(row.ApprovedBy),
			optionalTimeText(row.ApprovedAt),
		})
	}
	return table
}

// statementTable renders one student's whole file as a ledger.
//
// This is the document handed to a student who disputes a balance, so it is
// every line that explains the figure rather than the figure itself: the frozen
// fee components, the discounts with computed beside applied, every adjustment,
// the schedule, and every payment and refund including the reversed ones. A
// statement that showed only the totals would be a statement nobody could check
// — and a voided receipt the student is holding has to appear, with its stamp,
// or the page cannot explain why the balance disagrees with the paper.
//
// The totals go in the subtitle rather than into a trailing row: a total row in
// a spreadsheet is a row somebody's SUM counts twice.
func statementTable(c *gin.Context, statement *port.StudentStatement) export.Table {
	totals := statement.Totals
	table := export.Table{
		Title: "Student statement",
		Subtitle: reportSubtitle(c,
			statement.Student.StudentNo+" — "+statement.Student.FullName,
			"Charged "+amountText(totals.EffectiveNet),
			"Paid "+amountText(totals.NetPaid),
			"Outstanding "+amountText(totals.Remaining),
			"Credit "+amountText(totals.CreditBalance)),
		Columns: []export.Column{
			{Header: "Year"},
			{Header: "Department"},
			{Header: "Line"},
			{Header: "Reference"},
			{Header: "Detail"},
			{Header: "Date"},
			{Header: "Amount", Numeric: true},
			{Header: "Paid", Numeric: true},
			{Header: "Remaining", Numeric: true},
			{Header: "Status"},
		},
	}

	for _, account := range statement.Accounts {
		year, department := account.AcademicYearCode, account.DepartmentName
		line := func(kind, reference, detail, date string, amount, paid, remaining, status string) {
			table.Rows = append(table.Rows, []string{
				year, department, kind, reference, detail, date, amount, paid, remaining, status,
			})
		}

		// The account's own position first, so a reader who wants one year's
		// answer has it before the lines that produce it.
		line("Account", "",
			fmt.Sprintf("Stage %d · attempt %d · %s", account.Stage, account.AttemptNumber,
				account.StudyTypeName),
			"",
			amountText(account.EffectiveNet), amountText(account.NetPaid),
			amountText(account.Remaining), account.AccountStatus)

		for _, component := range account.FeeComponents {
			line("Fee", component.ComponentCode, component.NameAr, "",
				amountText(component.Amount), "", "", "")
		}
		for _, discount := range account.Discounts {
			detail := discount.DefinitionName
			// Where computed and applied differ, a cap or the discountable
			// floor cut the grant short. Naming it here is what stops the
			// shortfall reading as an error on the page.
			if discount.TruncationReason != nil {
				detail += " (" + *discount.TruncationReason + ")"
			}
			line("Discount", discount.DefinitionCode, detail,
				optionalTimeText(discount.AppliedAt),
				amountText(discount.AppliedAmount), "", "", discount.Status)
		}
		for _, adjustment := range account.Adjustments {
			line("Adjustment", adjustment.Type, adjustment.Reason,
				timeText(adjustment.PostedAt),
				amountText(adjustment.Amount), "", "", "")
		}
		for _, installment := range account.Installments {
			// The count, not the word again: the schedule's own status already
			// says "overdue", and "overdue · overdue 349 days" is the kind of
			// line that makes a reader distrust the rest of the sheet.
			status := installment.Status
			if installment.IsOverdue {
				status = fmt.Sprintf("%s · %d days late", status, installment.DaysOverdue)
			}
			line("Installment", fmt.Sprintf("%d", installment.Number), "",
				installment.DueDate.String(),
				amountText(installment.Amount), amountText(installment.AllocatedPaid),
				amountText(installment.Remaining), status)
		}
		for _, payment := range account.Payments {
			detail := payment.MethodCode
			if payment.CashierName != "" {
				detail += " · " + payment.CashierName
			}
			status := payment.Status
			if payment.VoidedAt != nil {
				status += " · voided " + timeText(*payment.VoidedAt)
				if payment.VoidReason != nil {
					status += " (" + *payment.VoidReason + ")"
				}
			}
			line("Payment", optionalText(payment.ReceiptNo), detail,
				timeText(payment.PaidAt), amountText(payment.Amount), "", "", status)
		}
		for _, refund := range account.Refunds {
			line("Refund", optionalText(refund.RefundNo), refund.Reason,
				optionalTimeText(refund.PostedAt), amountText(refund.Amount), "", "",
				refund.Status)
		}
	}

	return table
}

// summaryColumns is the money block every aggregate report shares.
func summaryColumns() []export.Column {
	return []export.Column{
		{Header: "Students", Numeric: true},
		{Header: "Accounts", Numeric: true},
		{Header: "Gross", Numeric: true},
		{Header: "Discount", Numeric: true},
		{Header: "Charged", Numeric: true},
		{Header: "Collected", Numeric: true},
		{Header: "Refunded", Numeric: true},
		{Header: "Outstanding", Numeric: true},
		{Header: "Collection %", Numeric: true},
	}
}

// summaryCells renders that block for one row.
func summaryCells(totals port.SummaryTotals) []string {
	return []string{
		countText(totals.StudentCount),
		countText(totals.AccountCount),
		amountText(totals.GrossTotal),
		amountText(totals.DiscountTotal),
		amountText(totals.EffectiveNet),
		amountText(totals.PaidTotal),
		amountText(totals.Refunded),
		amountText(totals.Remaining),
		percentText(totals.CollectionRatePct),
	}
}

// amountText renders a figure for a spreadsheet cell.
//
// Grouped for a person reading the printed page; the spreadsheet writer strips
// the separators again for a numeric column, so the cell is still a number that
// adds up.
func amountText(amount money.Amount) string { return amount.String() }

// countText renders a head count or a row count.
func countText(count int64) string { return strconv.FormatInt(count, 10) }

// percentText renders a rate, leaving the cell empty where there is none.
//
// Empty rather than "0", because a null collection rate means nothing was owed
// — which is not the same fact as nothing having been collected, and a zero in
// that cell is read as the second.
func percentText(pct *float64) string {
	if pct == nil {
		return ""
	}
	return strconv.FormatFloat(*pct, 'f', 2, 64)
}

// timeText renders an instant for a cell.
//
// UTC and to the minute: these columns are read to answer "how long between
// these two events", and a local rendering makes that question unanswerable on
// a sheet mailed to somebody in another office.
func timeText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// optionalTimeText renders an instant that may not have happened.
func optionalTimeText(t *time.Time) string {
	if t == nil {
		return ""
	}
	return timeText(*t)
}

// optionalText renders a pointer to a string, empty where it is absent.
func optionalText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// yesNo renders a flag for a person reading a printed column.
func yesNo(flag bool) string {
	if flag {
		return "yes"
	}
	return "no"
}

var _ = shared.NilID
