package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/adapter/export"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/port"
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

// sponsorReceivableTable renders the invoice list.
func sponsorReceivableTable(c *gin.Context, rows []port.SponsorReceivable) export.Table {
	table := export.Table{
		Title:    "Sponsor receivables",
		Subtitle: reportSubtitle(c),
		Columns: []export.Column{
			{Header: "Sponsor"},
			{Header: "Code"},
			{Header: "Students", Numeric: true},
			{Header: "Committed", Numeric: true},
			{Header: "Paid", Numeric: true},
			{Header: "Outstanding", Numeric: true},
		},
	}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{
			row.SponsorName,
			row.SponsorCode,
			fmt.Sprintf("%d", row.StudentCount),
			amountText(row.Committed),
			amountText(row.Paid),
			amountText(row.Outstanding),
		})
	}
	return table
}

// amountText renders a figure for a spreadsheet cell.
//
// Grouped for a person reading the printed page; the spreadsheet writer strips
// the separators again for a numeric column, so the cell is still a number that
// adds up.
func amountText(amount money.Amount) string { return amount.String() }

var _ = shared.NilID
