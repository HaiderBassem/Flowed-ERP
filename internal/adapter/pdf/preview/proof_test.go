package preview_test

import (
	"os"
	"testing"

	"flowed/internal/adapter/pdf"
)

// A proof sheet: every Arabic letter in every joining position, the ligatures,
// the digits, and the strings this system actually prints.
//
// Written when receipts came back reported as "letters missing". A proof sheet
// finds that in one look instead of by reprinting a receipt twenty times, and
// it is the only way to tell a glyph the font lacks from a glyph the shaper
// never asked for.
func TestProofSheet(t *testing.T) {
	out := os.Getenv("FLOWED_PDF_OUT")
	if out == "" {
		t.Skip("set FLOWED_PDF_OUT to write the proof sheet")
	}

	doc, err := pdf.New(pdf.A4)
	if err != nil {
		t.Fatal(err)
	}
	doc.NewPage()

	doc.Title("ورقة إثبات الحروف", "كل حرف في كل موضع")

	// Every letter, isolated and joined both ways. A letter that only breaks in
	// the middle of a word looks right in a heading and wrong in a name.
	const letters = "ابتثجحخدذرزسشصضطظعغفقكلمنهوي"
	doc.SectionTitle("الحروف في كل موضع")

	rows := [][]string{}
	for _, letter := range letters {
		l := string(letter)
		rows = append(rows, []string{
			l,               // isolated
			l + "ـ",         // initial, held open by a tatweel
			"ـ" + l + "ـ",   // medial
			"ـ" + l,         // final
			"سـ" + l + "ـم", // inside a word shape
		})
	}
	doc.Table([]pdf.Column{
		{Header: "منفرد", Width: 1},
		{Header: "في البداية", Width: 1},
		{Header: "في الوسط", Width: 1},
		{Header: "في النهاية", Width: 1},
		{Header: "داخل كلمة", Width: 1},
	}, rows)

	doc.SectionTitle("اللامات والهمزات")
	doc.Table([]pdf.Column{
		{Header: "الحالة", Width: 2},
		{Header: "النص", Width: 3},
	}, [][]string{
		{"لام ألف", "لا الله بلا كلا"},
		{"لام ألف بهمزة", "لأ لإ لآ"},
		{"همزات", "أحمد إبراهيم آمنة مؤمن رئيس شيء"},
		{"تاء مربوطة", "كلية جامعة مرحلة سنة دفعة"},
		{"ألف مقصورة", "مصطفى ليلى عيسى يحيى"},
		{"شدة وتشكيل", "المتبقّي المُستلَم الأوّل"},
	})

	doc.SectionTitle("ما يطبعه النظام فعلاً")
	doc.Table([]pdf.Column{
		{Header: "الحقل", Width: 2},
		{Header: "القيمة", Width: 3},
	}, [][]string{
		{"الجامعة", "جامعة الفراهيدي — كلية الهندسة"},
		{"الطالب", "علي محمد حسن الجبوري"},
		{"اسم الأم", "زينب عبد الله"},
		{"القسم", "هندسة الحاسوب"},
		{"نوع الدراسة", "استضافة من المسائي إلى الصباحي"},
		{"رقم السند", "R-2025-2026-000008"},
		{"المبلغ", "1,550,000 د.ع"},
		{"بالحروف", "فقط مليون وخمسمائة وخمسون ألف دينار عراقي لا غير"},
		{"التاريخ", "2026-09-22 14:35"},
		{"ملاحظة", "دفعة أولى عن الفصل الأول، والباقي في 2026-01-30."},
	})

	doc.SectionTitle("الأرقام والعلامات")
	doc.Paragraph("0123456789 ٠١٢٣٤٥٦٧٨٩ % ٪ ( ) [ ] — – - / : , .",
		pdf.TextStyle{Size: 11})

	doc.SectionTitle("فقرة طويلة لاختبار الالتفاف")
	doc.Paragraph(
		"هذا نص طويل غرضه أن يملأ أكثر من سطر واحد حتى نرى كيف يلتف الكلام عند "+
			"حافة الصفحة، وهل تبقى الحروف متصلة عبر الفاصل، وهل تحافظ الأرقام مثل "+
			"1,550,000 و2026-09-22 على ترتيبها داخل الجملة العربية أم تنقلب. "+
			"الجملة تحتوي كذلك على كلمات فيها لام ألف مثل: لا، إلا، كلا، الأولى.",
		pdf.TextStyle{Size: 10})

	rendered, err := doc.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+"/proof.pdf", rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s/proof.pdf (%d bytes)", out, len(rendered))
}
