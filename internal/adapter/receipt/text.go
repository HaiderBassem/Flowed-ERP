package receipt

import (
	"strings"
	"time"
	"unicode"

	"github.com/swibit/flowed/internal/domain/money"
)

// receiptWidth is the character width of a standard 80mm thermal roll.
const receiptWidth = 42

// RenderText produces a receipt for a thermal or dot-matrix printer.
//
// Most Iraqi cashier desks print to an 80mm thermal roll, not to A5 through a
// browser. Those printers take bytes, not HTML, so the plain-text rendering is
// the one that will actually be used day to day — the HTML is for the office
// that files a copy.
//
// Two constraints shape it. The roll is 42 characters wide, so nothing may
// wrap unpredictably. And terminal fonts are monospaced, which Arabic is not
// designed for: the text is right-aligned by padding rather than by relying on
// the printer to handle bidirectional layout, because cheap thermal firmware
// frequently does not.
func RenderText(d Data, loc *time.Location) []byte {
	var b strings.Builder

	rule := strings.Repeat("=", receiptWidth)
	thin := strings.Repeat("-", receiptWidth)

	writeCentred(&b, d.Institution.UniversityNameAr)
	if d.Institution.CollegeNameAr != "" {
		writeCentred(&b, d.Institution.CollegeNameAr)
	}
	if d.Institution.Phone != "" {
		writeCentred(&b, d.Institution.Phone)
	}
	b.WriteString(rule + "\n")

	writeCentred(&b, d.Title())
	if d.IsCopy() {
		writeCentred(&b, "** "+d.CopyLabel()+" **")
	}
	if d.IsVoided() {
		writeCentred(&b, "** سند ملغى **")
		if d.VoidReason != "" {
			writeCentred(&b, d.VoidReason)
		}
	}
	b.WriteString(rule + "\n")

	writeField(&b, "الرقم", d.Number)
	writeField(&b, "التاريخ", FormatDateTime(d.IssuedAt, loc))
	writeField(&b, "العام", d.AcademicYear)
	b.WriteString(thin + "\n")

	writeField(&b, "الطالب", d.StudentName)
	writeField(&b, "الرقم الجامعي", d.StudentNumber)
	if d.MotherName != "" {
		writeField(&b, "اسم الأم", d.MotherName)
	}
	writeField(&b, "القسم", d.DepartmentName)
	writeField(&b, "المرحلة", d.StageLabel+" - "+d.StudyTypeName)
	if d.PayerName != "" {
		writeField(&b, "المسلِّم", d.PayerName)
	}
	b.WriteString(rule + "\n")

	// The figure and the words together. The words are what make the slip hard
	// to alter, so they get their own block rather than being squeezed onto a
	// line that might truncate.
	writeField(&b, d.AmountLabel(), money.FormatWesternDigits(d.Amount)+" د.ع")
	b.WriteString(thin + "\n")
	b.WriteString("المبلغ كتابةً:\n")
	writeWrapped(&b, d.AmountInWords)
	b.WriteString(rule + "\n")

	writeField(&b, "طريقة الدفع", d.PaymentMethod)
	if d.MethodReference != "" {
		writeField(&b, "رقم الإشعار", d.MethodReference)
	}

	if len(d.Lines) > 0 {
		b.WriteString(thin + "\n")
		for _, l := range d.Lines {
			label := l.Label
			if l.DueDate != nil {
				label += " (" + FormatDate(*l.DueDate) + ")"
			}
			writeField(&b, label, money.FormatWesternDigits(l.Amount))
		}
	}

	if d.Kind == KindPayment && d.NetFees.IsPositive() {
		b.WriteString(rule + "\n")
		writeField(&b, "مجموع الرسوم", money.FormatWesternDigits(d.TotalFees))
		writeField(&b, "الخصومات", money.FormatWesternDigits(d.TotalDiscount))
		writeField(&b, "الصافي", money.FormatWesternDigits(d.NetFees))
		writeField(&b, "المسدَّد", money.FormatWesternDigits(d.PaidToDate))
		b.WriteString(thin + "\n")
		writeField(&b, "المتبقي", money.FormatWesternDigits(d.Remaining))
	}

	if d.Notes != "" {
		b.WriteString(thin + "\n")
		writeWrapped(&b, "ملاحظات: "+d.Notes)
	}

	b.WriteString(rule + "\n")
	writeField(&b, "أمين الصندوق", d.CashierName)
	b.WriteString("\n\n")
	writeCentred(&b, "توقيع المستلم: ........................")
	b.WriteString("\n")
	writeCentred(&b, "طُبع "+FormatDateTime(d.PrintedAt, loc))
	if d.PrintedBy != "" {
		writeCentred(&b, "بواسطة "+d.PrintedBy)
	}
	// Thermal cutters need trailing feed or they slice through the last line.
	b.WriteString("\n\n\n")

	return []byte(b.String())
}

// writeField writes a right-aligned label with its value pushed to the left
// edge, which is how a right-to-left slip reads on a monospaced roll.
func writeField(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	line := label + ": " + value
	if displayWidth(line) <= receiptWidth {
		b.WriteString(padRight(line) + "\n")
		return
	}
	// Too long for one line: put the label on its own and wrap the value.
	b.WriteString(padRight(label+":") + "\n")
	writeWrapped(b, value)
}

func writeCentred(b *strings.Builder, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	width := displayWidth(s)
	if width >= receiptWidth {
		writeWrapped(b, s)
		return
	}
	pad := (receiptWidth - width) / 2
	b.WriteString(strings.Repeat(" ", pad) + s + "\n")
}

// writeWrapped breaks text on word boundaries so an Arabic phrase never splits
// mid-word, which renders as two unconnected fragments and reads as gibberish.
//
// It splits on ASCII spacing only, deliberately. strings.Fields would also
// split on the non-breaking space, and the one place this system uses one is
// the لا غير that closes a written amount — the marker exists to be an
// unmistakable end to the figure, and breaking it across two lines is exactly
// what it must not do.
func writeWrapped(b *strings.Builder, s string) {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(words) == 0 {
		return
	}
	line := ""
	for _, w := range words {
		candidate := w
		if line != "" {
			candidate = line + " " + w
		}
		if displayWidth(candidate) > receiptWidth && line != "" {
			b.WriteString(padRight(line) + "\n")
			line = w
			continue
		}
		line = candidate
	}
	if line != "" {
		b.WriteString(padRight(line) + "\n")
	}
}

func padRight(s string) string {
	// Right-to-left text is padded on the left so it sits against the right
	// edge of the roll.
	pad := receiptWidth - displayWidth(s)
	if pad <= 0 {
		return s
	}
	return strings.Repeat(" ", pad) + s
}

// displayWidth counts printed columns rather than bytes or runes.
//
// Arabic combining marks — the short vowels and the shadda — occupy no column
// of their own, so counting runes would make a vocalised word appear wider
// than it prints and the line would be padded short.
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		width++
	}
	return width
}
