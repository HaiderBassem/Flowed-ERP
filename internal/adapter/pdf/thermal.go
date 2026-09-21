package pdf

import (
	"fmt"
	"strings"
	"time"

	"flowed/internal/adapter/receipt"
	"flowed/internal/domain/money"
)

// ThermalReceipt renders the counter slip: 80mm of roll, one column, no colour.
//
// A different document from the A4 receipt rather than the same one shrunk.
// Seventy-two printable millimetres is about thirty Arabic characters, so
// anything in two columns collides, and a thermal head prints one ink at one
// density — a green band comes out as a black band with the text lost inside
// it. Everything here is black on white, ruled with lines rather than fills,
// and ordered the way somebody reads a slip they are being handed: what was
// paid, by whom, against what.
//
// The A4 receipt stays the formal document — the one that is filed, signed and
// presented. This is the one the student walks away with.
func ThermalReceipt(data receipt.Data, loc *time.Location) ([]byte, error) {
	if loc == nil {
		loc = time.UTC
	}

	// Measured, then drawn. See NewRoll: the page size is fixed when the
	// document is created, so the only way to end the paper where the content
	// ends is to lay it out once and ask how tall it was.
	measure, err := NewRoll(0)
	if err != nil {
		return nil, err
	}
	drawThermal(measure, data, loc)

	// The measured height plus a margin, and a little tail so the cutter does
	// not shave the last rule.
	doc, err := NewRoll(measure.Y() + measure.Margin() + 6)
	if err != nil {
		return nil, err
	}
	drawThermal(doc, data, loc)
	return doc.Bytes()
}

// drawThermal lays the slip out. Run twice — once to measure, once to keep.
func drawThermal(doc *Document, data receipt.Data, loc *time.Location) {
	doc.NewPage()

	centre := func(text string, size float64, bold bool) {
		if strings.TrimSpace(text) == "" {
			return
		}
		doc.Paragraph(text, TextStyle{Size: size, Bold: bold, Align: AlignCenter, Color: ink})
	}

	// The crest is centred and small. A logo on a thermal roll is a block of
	// heat: wider than about half the roll it prints as a grey smear, and the
	// name below it is what identifies the university anyway.
	if logo := decodeDataURI(data.Institution.LogoDataURI); logo != nil {
		const h = 34
		doc.Image(logo, doc.Left()+(doc.ContentWidth()-h*3/2)/2, doc.Y(), h*3/2, h)
		doc.Space(h + 4)
	}

	centre(data.Institution.UniversityNameAr, 10, true)
	centre(data.Institution.CollegeNameAr, 8.5, false)
	centre(joinNonEmpty(" · ", data.Institution.Address, data.Institution.Phone), 7, false)

	doc.Space(2)
	doc.Rule(ink, 0.8)

	title := "سند قبض"
	if data.Kind == receipt.KindRefund {
		title = "سند صرف"
	}
	centre(title, 12, true)
	doc.Space(2)

	if data.VoidedAt != nil {
		// No colour to say it with, so it is said in words and ruled off.
		doc.Rule(ink, 0.8)
		centre("سند ملغى — لا يُعتدّ به", 10, true)
		centre(data.VoidReason, 7.5, false)
		doc.Rule(ink, 0.8)
	}

	// Label above value rather than beside it. Thirty characters is not enough
	// for two columns, and a label that wraps onto its own line beside a value
	// is worse than one that was always going to be there.
	line := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		doc.EnsureRoom(16)
		// The baseline clears the largest text on the line. Set any tighter and
		// the value's ascenders climb above the rule drawn before it, which is
		// how a horizontal line ends up struck through "نقداً".
		baseline := doc.Y() + 10
		doc.TextAt(label, doc.Left(), doc.Right(), baseline, TextStyle{Size: 7, Color: muted})
		doc.TextAt(value, doc.Left(), doc.Right(), baseline,
			TextStyle{Size: 8.5, Color: ink, Align: AlignLeft})
		doc.Space(14)
	}

	line("رقم السند", data.Number)
	line("التاريخ", data.IssuedAt.In(loc).Format("2006-01-02 15:04"))
	line("الطالب", data.StudentName)
	line("الرقم الجامعي", data.StudentNumber)
	line("القسم", data.DepartmentName)
	line("السنة الدراسية", data.AcademicYear)

	doc.Space(2)
	doc.Rule(ink, 0.8)

	// The amount, as large as the roll allows. This is the line the student
	// checks before they leave the counter.
	label := "المبلغ المستلم"
	if data.Kind == receipt.KindRefund {
		label = "المبلغ المصروف"
	}
	centre(label, 8, false)
	centre(money.FormatWesternDigits(data.Amount)+" "+currency(data), 15, true)
	centre(data.AmountInWords, 7.5, false)
	doc.Rule(ink, 0.8)

	line("طريقة الدفع", data.PaymentMethod)
	line("المرجع", data.MethodReference)

	if len(data.Lines) > 0 {
		doc.Space(2)
		centre("الأقساط", 8, true)
		for _, l := range data.Lines {
			doc.EnsureRoom(13)
			baseline := doc.Y() + 9
			style := TextStyle{Size: 7.5, Color: ink}
			doc.TextAt(doc.Ellipsise(l.Label, doc.ContentWidth()*0.6, style),
				doc.Left(), doc.Right(), baseline, style)
			doc.TextAt(money.FormatWesternDigits(l.Amount), doc.Left(), doc.Right(), baseline,
				TextStyle{Size: 7.5, Color: ink, Align: AlignLeft})
			doc.Space(11)
		}
	}

	doc.Space(2)
	doc.Rule(hairline, 0.5)
	line("الصافي", money.FormatWesternDigits(data.NetFees))
	line("المدفوع لتاريخه", money.FormatWesternDigits(data.PaidToDate))
	line("المتبقّي", money.FormatWesternDigits(data.Remaining))

	doc.Space(4)
	doc.Rule(ink, 0.8)
	centre("الأرقام أعلاه لحظة إصدار السند", 6.5, false)
	centre(data.Institution.FooterAr, 6.5, false)

	stamp := data.PrintedAt.In(loc).Format("2006-01-02 15:04")
	if data.PrintedBy != "" {
		stamp += " · " + data.PrintedBy
	}
	if data.CopyNumber > 0 {
		stamp += fmt.Sprintf(" · نسخة %d", data.CopyNumber)
	}
	centre(stamp, 6.5, false)

}
