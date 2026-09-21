package preview_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"flowed/internal/adapter/pdf"
	"flowed/internal/adapter/receipt"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// TestRenderSamples writes one of each document to FLOWED_PDF_OUT so a person
// can look at them. Skipped unless that is set: a build must not litter, and
// nobody reviews a PDF on every commit.
func TestRenderSamples(t *testing.T) {
	out := os.Getenv("FLOWED_PDF_OUT")
	if out == "" {
		t.Skip("set FLOWED_PDF_OUT to write sample documents")
	}

	due := shared.NewDate(2026, time.March, 1)
	loc, _ := time.LoadLocation("Asia/Baghdad")
	data := receipt.Data{
		Kind: receipt.KindPayment,
		Institution: receipt.Institution{
			// The real installation's values: a long name and a 16:9 logo,
			// which is what put the crest on top of the text.
			UniversityNameAr: "الجامعة التكنولوجية - العراق",
			CollegeNameAr:    "كلية هندسة الحاسوب",
			Address:          "بغداد - حي الوحدة",
			Phone:            "07709099732",
			FooterAr:         "يُرجى الاحتفاظ بهذا السند لمراجعة الحسابات",
			LogoDataURI:      testLogo(),
		},
		Number: "R-2025-2026-000008", IssuedAt: time.Now(),
		StudentName: "علي محمد حسن الجبوري", StudentNumber: "2026-0001",
		MotherName: "زينب عبد الله", CollegeName: "كلية الهندسة",
		DepartmentName: "هندسة الحاسوب", StageLabel: "الأولى",
		StudyTypeName: "صباحي", AcademicYear: "2025-2026",
		Amount: 1_500_000, AmountInWords: money.SpellArabic(1_500_000),
		PaymentMethod: "نقداً", MethodReference: "",
		Lines: []receipt.Line{
			{Label: "القسط الأول", DueDate: &due, Amount: 900_000},
			{Label: "القسط الثاني", DueDate: &due, Amount: 600_000},
		},
		TotalFees: 3_300_000, TotalDiscount: 300_000, NetFees: 3_000_000,
		PaidToDate: 1_500_000, Remaining: 1_500_000,
		CashierName: "مدير النظام", Notes: "دفعة أولى عن الفصل الأول.",
		PrintedAt: time.Now(), PrintedBy: "admin",
	}

	thermal, err := pdf.ThermalReceipt(data, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+"/thermal.pdf", thermal, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s/thermal.pdf (%d bytes)", out, len(thermal))

	rendered, err := pdf.Receipt(data, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+"/receipt.pdf", rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s/receipt.pdf (%d bytes)", out, len(rendered))

	// A report long enough to spill, because page two is where a generated
	// document goes wrong: the letterhead vanishes, the column headings do
	// not repeat, and a wall of unlabelled figures is what somebody files.
	rows := make([][]string, 0, 60)
	names := []string{
		"علي محمد حسن الجبوري", "زينب عبد الله كريم", "مصطفى وليد الساعدي",
		"نور عماد شاكر", "فاطمة أحمد كاظم الحسيني", "عبد الله ماجد حميد",
	}
	depts := []string{"هندسة الحاسوب", "الهندسة المدنية", "الهندسة الكهربائية"}
	for i := 0; i < 60; i++ {
		rows = append(rows, []string{
			fmt.Sprintf("CPE-2025-%03d", i+1),
			names[i%len(names)],
			"زينب عبد الله",
			"2025-2026",
			depts[i%len(depts)],
			fmt.Sprintf("%d", i%4+1),
			money.FormatWesternDigits(money.Amount(3_100_000)),
			money.FormatWesternDigits(money.Amount(int64(i%5) * 400_000)),
			money.FormatWesternDigits(money.Amount(3_100_000 - int64(i%5)*400_000)),
		})
	}

	report, err := pdf.Report(pdf.ReportDoc{
		Title:    "تقرير الديون",
		Subtitle: "السنة 2025-2026 · الكلية كلية الهندسة",
		Columns: []pdf.ReportColumn{
			{Header: "الرقم الجامعي"}, {Header: "الطالب"}, {Header: "اسم الأم"},
			{Header: "السنة"}, {Header: "القسم"}, {Header: "المرحلة", Numeric: true},
			{Header: "المفروض", Numeric: true}, {Header: "المدفوع", Numeric: true},
			{Header: "المتبقّي", Numeric: true},
		},
		Rows:        rows,
		Letterhead:  data.Institution,
		GeneratedAt: time.Now(),
		GeneratedBy: "مدير الحسابات",
	}, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+"/report.pdf", report, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s/report.pdf (%d bytes)", out, len(report))
}

// testLogo reads the crest the running installation uses, when it is there.
//
// A logo changes the letterhead's whole geometry — it is what the text has to
// be kept clear of — so a preview without one proves nothing about the case
// that was broken.
func testLogo() string {
	raw, err := os.ReadFile(os.Getenv("FLOWED_PDF_OUT") + "/logo.txt")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
