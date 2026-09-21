package pdf

import (
	"fmt"
	"time"

	"flowed/internal/adapter/receipt"
)

// ReportDoc is a report ready to be drawn.
//
// It mirrors export.Table rather than importing it, because the dependency has
// to point this way: export renders to several formats and delegates one of
// them here. A shared struct in a third package would be a package existing
// only to hold six fields.
type ReportDoc struct {
	Title    string
	Subtitle string
	Columns  []ReportColumn
	Rows     [][]string

	Letterhead  receipt.Institution
	GeneratedAt time.Time
	GeneratedBy string
}

// ReportColumn is one column of a report.
type ReportColumn struct {
	Header string
	// Numeric left-aligns the cell so a column of figures lines up on its last
	// digit — the only way amounts can be compared down a page.
	Numeric bool
}

// Report renders a report as an A4 PDF.
//
// Portrait until the columns will not fit, then landscape. The decision is made
// here, from the column count, rather than asked of the caller: an operator
// pressing "PDF" has no way to know the debt report has nine columns and the
// refund register five, and a portrait page carrying nine is exactly the
// "شي فوك وشي جوه" this is meant to avoid.
func Report(doc ReportDoc, loc *time.Location) ([]byte, error) {
	if loc == nil {
		loc = time.UTC
	}

	size := A4
	if len(doc.Columns) > 6 {
		size = A4Landscape
	}

	d, err := New(size)
	if err != nil {
		return nil, err
	}

	d.WithHeader(func(d *Document) { reportHead(d, doc) })
	d.WithFooter(func(d *Document, page int) { reportFoot(d, doc, page, loc) })
	d.NewPage()

	d.Title(doc.Title, doc.Subtitle)

	if len(doc.Rows) == 0 {
		// An empty report says so. A page with a heading and nothing under it
		// reads as a report that failed rather than as a question with no
		// answer, and the difference matters when the question is "who owes
		// money".
		d.Space(20)
		d.Text("لا توجد بيانات ضمن هذا النطاق.",
			TextStyle{Size: 11, Align: AlignCenter, Color: Muted()})
		return d.Bytes()
	}

	d.Table(reportColumns(doc.Columns), doc.Rows)

	d.Space(2)
	d.Note(fmt.Sprintf("عدد السطور: %d", len(doc.Rows)))

	return d.Bytes()
}

// reportColumns gives every column a share of the width.
//
// Numeric columns get less: an amount is eight characters and a department name
// is thirty, and giving them the same width wastes a third of the page on
// whitespace no figure needs.
func reportColumns(in []ReportColumn) []Column {
	out := make([]Column, 0, len(in))
	for _, column := range in {
		width := 2.0
		if column.Numeric {
			width = 1.3
		}
		out = append(out, Column{Header: column.Header, Width: width, Numeric: column.Numeric})
	}
	return out
}

// reportHead draws the letterhead on every page.
//
// Compact, and on one line where it fits: a report's first page should be
// mostly report. The receipt's letterhead is three centred lines because a
// receipt is a document in its own right; a report is a working paper.
func reportHead(d *Document, doc ReportDoc) {
	top := d.Y()

	if logo := decodeDataURI(doc.Letterhead.LogoDataURI); logo != nil {
		d.Image(logo, d.Right()-34, top, 34, 34)
	}

	d.SetY(top + 2)
	name := doc.Letterhead.UniversityNameAr
	if doc.Letterhead.CollegeNameAr != "" {
		name += " — " + doc.Letterhead.CollegeNameAr
	}
	d.Text(name, TextStyle{Size: 11, Bold: true, Color: Accent()})

	if contact := joinNonEmpty(" · ", doc.Letterhead.Address, doc.Letterhead.Phone); contact != "" {
		d.Space(-8)
		d.Text(contact, TextStyle{Size: 7.5, Color: Muted()})
	}

	d.Space(-2)
	d.Rule(Hairline(), 0.5)
}

// reportFoot numbers the page and stamps when the figures were true.
//
// The stamp is not decoration. A debt report on somebody's desk in March is a
// picture of a database in January, and a page that does not say so gets read
// as current.
func reportFoot(d *Document, doc ReportDoc, page int, loc *time.Location) {
	y := d.Height() - d.Margin() - 10
	d.RuleAt(y-8, Hairline(), 0.5)

	stamp := fmt.Sprintf("صدر في %s", doc.GeneratedAt.In(loc).Format("2006-01-02 15:04"))
	if doc.GeneratedBy != "" {
		stamp += " · بواسطة " + doc.GeneratedBy
	}
	d.TextAt(stamp, d.Left(), d.Right(), y, TextStyle{Size: 7.5, Color: Muted()})

	d.TextAt(fmt.Sprintf("صفحة %d", page), d.Left(), d.Right(), y,
		TextStyle{Size: 7.5, Color: Muted(), Align: AlignLeft})
}

// StatementDoc is a student's full account.
//
// Separate from ReportDoc because a statement is not a table: it is a document
// about one person with several tables inside it, and the paper a student takes
// to a ministry to prove what they owe.
type StatementDoc struct {
	Letterhead receipt.Institution

	StudentName   string
	StudentNumber string
	MotherName    string

	Totals      []Field
	Sections    []StatementSection
	GeneratedAt time.Time
	GeneratedBy string
}

// StatementSection is one academic year within a statement.
type StatementSection struct {
	Title   string
	Fields  []Field
	Columns []ReportColumn
	Rows    [][]string
}

// Statement renders the per-student account document.
func Statement(doc StatementDoc, loc *time.Location) ([]byte, error) {
	if loc == nil {
		loc = time.UTC
	}

	d, err := New(A4)
	if err != nil {
		return nil, err
	}

	// The letterhead and footer are the report's, so a statement and a report
	// printed the same morning look like they came from the same office.
	chrome := ReportDoc{
		Letterhead:  doc.Letterhead,
		GeneratedAt: doc.GeneratedAt,
		GeneratedBy: doc.GeneratedBy,
	}
	d.WithHeader(func(d *Document) { reportHead(d, chrome) })
	d.WithFooter(func(d *Document, page int) { reportFoot(d, chrome, page, loc) })
	d.NewPage()

	d.Title("كشف حساب الطالب", doc.StudentName)

	d.Fields(present(
		Field{Label: "اسم الطالب", Value: doc.StudentName, Strong: true},
		Field{Label: "الرقم الجامعي", Value: doc.StudentNumber, LTR: true},
		Field{Label: "اسم الأم", Value: doc.MotherName},
	), 2)

	if len(doc.Totals) > 0 {
		d.SectionTitle("الإجمالي عبر كل السنوات")
		d.Fields(doc.Totals, 2)
	}

	for _, section := range doc.Sections {
		d.SectionTitle(section.Title)
		if len(section.Fields) > 0 {
			d.Fields(present(section.Fields...), 2)
		}
		if len(section.Rows) > 0 {
			d.Table(reportColumns(section.Columns), section.Rows)
		}
	}

	d.Space(6)
	d.Note("هذا الكشف صورة عن الحساب لحظة إصداره. للاستفسار يُراجع قسم الحسابات.")

	return d.Bytes()
}
