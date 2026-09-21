package pdf

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/signintech/gopdf"

	"flowed/internal/adapter/receipt"
	"flowed/internal/domain/money"
)

// Receipt renders a payment or refund receipt as an A5 PDF.
//
// A5 rather than A4: half a sheet is what an office guillotine produces from
// stock it already buys, and a receipt on a full page wastes three quarters of
// it. One column, top to bottom, because a receipt is read once at a counter
// and then filed — the reader wants the amount and the number, and everything
// else is there so they can find it again in a year.
func Receipt(data receipt.Data, loc *time.Location) ([]byte, error) {
	if loc == nil {
		loc = time.UTC
	}

	doc, err := New(A5)
	if err != nil {
		return nil, err
	}

	doc.WithHeader(func(d *Document) { receiptHead(d, data) })
	doc.WithFooter(func(d *Document, _ int) { receiptFoot(d, data, loc) })
	doc.NewPage()

	title := "سند قبض"
	signatures := []string{"توقيع المحاسب", "توقيع الطالب"}
	if data.Kind == receipt.KindRefund {
		title = "سند صرف"
		signatures = []string{"توقيع المحاسب", "توقيع المستلم"}
	}
	doc.Title(title, joinNonEmpty(" — ", data.AcademicYear, data.CollegeName))

	// A voided receipt still prints — an auditor asking about one needs to see
	// it — but it says so before anything else on the page. A reader who takes
	// in the amount first and the cancellation second has already misread it.
	if data.VoidedAt != nil {
		voidBanner(doc, data, loc)
	}

	doc.Fields(present(
		Field{Label: "رقم السند", Value: data.Number, Strong: true, LTR: true},
		Field{Label: "التاريخ", Value: data.IssuedAt.In(loc).Format("2006-01-02 15:04"), LTR: true},
		Field{Label: "اسم الطالب", Value: data.StudentName, Strong: true},
		Field{Label: "الرقم الجامعي", Value: data.StudentNumber, LTR: true},
		Field{Label: "اسم الأم", Value: data.MotherName},
		Field{Label: "السنة الدراسية", Value: data.AcademicYear, LTR: true},
		Field{Label: "الكلية", Value: data.CollegeName},
		Field{Label: "القسم", Value: data.DepartmentName},
		Field{Label: "نوع الدراسة", Value: data.StudyTypeName},
		Field{Label: "المرحلة", Value: data.StageLabel},
	), 2)

	doc.Space(6)
	doc.TotalBar(amountLabel(data.Kind), money.FormatWesternDigits(data.Amount)+" "+currency(data))

	// The amount in words is what makes a receipt hard to alter: a figure can
	// have a zero added to it, a sentence cannot.
	//
	// No closing لا غير is added here. SpellArabic already ends with it — that
	// phrase is part of the convention it implements — and appending a second
	// one printed "لا غير لا غير" on every receipt.
	if data.AmountInWords != "" {
		doc.Space(2)
		doc.Paragraph("فقط "+data.AmountInWords, TextStyle{Size: 9.5, Bold: true, Color: Ink()})
	}

	doc.Fields(present(
		Field{Label: "طريقة الدفع", Value: data.PaymentMethod},
		Field{Label: "المرجع", Value: data.MethodReference, LTR: true},
	), 2)

	if len(data.Lines) > 0 {
		doc.SectionTitle("توزيع المبلغ على الأقساط")
		rows := make([][]string, 0, len(data.Lines))
		for _, line := range data.Lines {
			due := ""
			if line.DueDate != nil {
				due = line.DueDate.String()
			}
			rows = append(rows, []string{line.Label, due, money.FormatWesternDigits(line.Amount)})
		}
		doc.Table([]Column{
			{Header: "القسط", Width: 3},
			{Header: "تاريخ الاستحقاق", Width: 2, Numeric: true},
			{Header: "المبلغ", Width: 2, Numeric: true, Strong: true},
		}, rows)
	}

	doc.SectionTitle("موقف الحساب عند الإصدار")
	doc.Fields([]Field{
		{Label: "مجموع الأجور", Value: money.FormatWesternDigits(data.TotalFees), LTR: true},
		{Label: "الخصومات", Value: money.FormatWesternDigits(data.TotalDiscount), LTR: true},
		{Label: "الصافي", Value: money.FormatWesternDigits(data.NetFees), LTR: true},
		{Label: "المدفوع لتاريخه", Value: money.FormatWesternDigits(data.PaidToDate), LTR: true},
		{Label: "المتبقّي", Value: money.FormatWesternDigits(data.Remaining), Strong: true, LTR: true},
	}, 2)

	// Frozen at issue, and the receipt says so. A student comparing the paper
	// in their hand against a screen a month later will find different numbers,
	// and the sentence explaining why belongs on the paper.
	doc.Space(2)
	doc.Note("الأرقام أعلاه هي موقف الحساب لحظة إصدار هذا السند، ولا تتغيّر بإعادة الطباعة.")

	if strings.TrimSpace(data.Notes) != "" {
		doc.SectionTitle("ملاحظات")
		doc.Paragraph(data.Notes, TextStyle{Size: 9, Color: Ink()})
	}

	doc.SignatureLine(signatures...)

	return doc.Bytes()
}

// receiptHead draws the letterhead: the crest, the university, the college.
func receiptHead(d *Document, data receipt.Data) {
	top := d.Y()

	if logo := decodeDataURI(data.Institution.LogoDataURI); logo != nil {
		// Placed on the right, where a right-to-left reader's eye starts.
		d.Image(logo, d.Right()-46, top, 46, 46)
	}

	d.SetY(top + 2)
	d.Text(data.Institution.UniversityNameAr,
		TextStyle{Size: 13, Bold: true, Align: AlignCenter, Color: Accent()})

	if data.Institution.CollegeNameAr != "" {
		d.Space(-6)
		d.Text(data.Institution.CollegeNameAr, TextStyle{Size: 10, Align: AlignCenter, Color: Ink()})
	}

	if contact := joinNonEmpty(" · ", data.Institution.Address, data.Institution.Phone); contact != "" {
		d.Space(-6)
		d.Text(contact, TextStyle{Size: 7.5, Align: AlignCenter, Color: Muted()})
	}

	d.Space(2)
	d.Rule(Hairline(), 0.5)
}

// receiptFoot draws the bottom strip: the office's own line, and the print
// stamp that tells an auditor whether this sheet is a copy.
func receiptFoot(d *Document, data receipt.Data, loc *time.Location) {
	y := d.Height() - d.Margin() - 16
	d.RuleAt(y-6, Hairline(), 0.5)

	if footer := strings.TrimSpace(data.Institution.FooterAr); footer != "" {
		d.TextAt(footer, d.Left(), d.Right(), y+4,
			TextStyle{Size: 7.5, Color: Muted(), Align: AlignCenter})
		y += 10
	}

	stamp := fmt.Sprintf("طُبع في %s", data.PrintedAt.In(loc).Format("2006-01-02 15:04"))
	if data.PrintedBy != "" {
		stamp += " · بواسطة " + data.PrintedBy
	}
	d.TextAt(stamp, d.Left(), d.Right(), y+4, TextStyle{Size: 7, Color: Muted()})

	// A reprint is labelled, because two identical receipts for one payment is
	// how a student comes to believe they paid twice.
	if data.CopyNumber > 0 {
		d.TextAt(fmt.Sprintf("نسخة رقم %d", data.CopyNumber), d.Left(), d.Right(), y+4,
			TextStyle{Size: 7, Bold: true, Color: Muted(), Align: AlignLeft})
	}
}

// voidBanner states, before anything else, that this receipt was cancelled.
func voidBanner(d *Document, data receipt.Data, loc *time.Location) {
	const height = 34
	d.EnsureRoom(height + 6)

	red := gopdf.RGBColor{R: 153, G: 27, B: 27}
	pale := gopdf.RGBColor{R: 254, G: 242, B: 242}

	d.Box(d.Left(), d.Y(), d.ContentWidth(), height, pale)
	d.TextAt("سند ملغى — لا يُعتدّ به", d.Left()+8, d.Right()-8, d.Y()+14,
		TextStyle{Size: 11, Bold: true, Color: red})

	reason := data.VoidReason
	if reason == "" {
		reason = "بلا سبب مُسجّل"
	}
	d.TextAt(fmt.Sprintf("أُلغي في %s · %s", data.VoidedAt.In(loc).Format("2006-01-02"), reason),
		d.Left()+8, d.Right()-8, d.Y()+27, TextStyle{Size: 8, Color: red})

	d.Space(height + 8)
}

func amountLabel(kind receipt.Kind) string {
	if kind == receipt.KindRefund {
		return "المبلغ المصروف"
	}
	return "المبلغ المستلم"
}

// currency abbreviates the Iraqi dinar and spells anything else out. A receipt
// reads better with د.ع than with the full name repeated beside every figure.
func currency(data receipt.Data) string {
	if name := data.Institution.CurrencyNameAr; name != "" && name != "دينار عراقي" {
		return name
	}
	return "د.ع"
}

func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, sep)
}

// present drops the fields with nothing in them.
//
// A label with an empty value beside it reads as missing data rather than as
// data that does not apply — a cash receipt showing "المرجع:" and a blank looks
// like somebody forgot to type the reference, when cash has none.
func present(fields ...Field) []Field {
	kept := make([]Field, 0, len(fields))
	for _, field := range fields {
		if strings.TrimSpace(field.Value) != "" {
			kept = append(kept, field)
		}
	}
	return kept
}

// decodeDataURI turns an inline image into bytes, or nil.
//
// nil rather than an error: a malformed logo costs the page its crest and
// nothing else, and refusing to print over it would stop a student leaving with
// proof of a payment already taken.
func decodeDataURI(uri string) []byte {
	if uri == "" {
		return nil
	}
	_, encoded, found := strings.Cut(uri, ",")
	if !found {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	return decoded
}
