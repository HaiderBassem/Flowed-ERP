package preview_test

import (
	"os"
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
			UniversityNameAr: "جامعة بغداد",
			CollegeNameAr:    "كلية الهندسة",
			Address:          "بغداد — الجادرية",
			Phone:            "07701234567",
			FooterAr:         "يُرجى الاحتفاظ بهذا السند لمراجعة الحسابات",
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

	rendered, err := pdf.Receipt(data, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+"/receipt.pdf", rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s/receipt.pdf (%d bytes)", out, len(rendered))
}
