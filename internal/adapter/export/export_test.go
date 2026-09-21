package export_test

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"flowed/internal/adapter/export"
)

func table() export.Table {
	return export.Table{
		Title:    "Debt report",
		Subtitle: "2025-2026 — كلية الهندسة",
		Columns: []export.Column{
			{Header: "رقم الطالب"},
			{Header: "الاسم"},
			{Header: "المتبقي", Numeric: true},
		},
		Rows: [][]string{
			{"2025001", "علي محمد حسن", "1,500,000"},
			{"2025002", "فاطمة كريم", "250,000"},
		},
		GeneratedAt: time.Date(2026, 3, 15, 9, 30, 0, 0, time.UTC),
		GeneratedBy: "finance.one",
		Institution: "جامعة الاختبار",
	}
}

// Excel reads a UTF-8 CSV as the local code page unless it finds a byte-order
// mark, and an Arabic name then arrives as mojibake — which makes the export
// useless for the office that asked for it.
func TestCSVCarriesAByteOrderMark(t *testing.T) {
	var buf bytes.Buffer
	if err := table().Write(&buf, export.FormatCSV); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if !bytes.HasPrefix(buf.Bytes(), []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("the CSV must start with a UTF-8 byte-order mark")
	}
	body := buf.String()
	for _, want := range []string{"رقم الطالب", "علي محمد حسن", "1,500,000"} {
		if !strings.Contains(body, want) {
			t.Errorf("the CSV is missing %q", want)
		}
	}
}

// A figure written as text does not add up, and a total that silently excludes
// half the rows is worse than no total.
func TestXLSXWritesNumericColumnsAsNumbers(t *testing.T) {
	var buf bytes.Buffer
	if err := table().Write(&buf, export.FormatXLSX); err != nil {
		t.Fatalf("writing: %v", err)
	}

	sheet := readZipEntry(t, buf.Bytes(), "xl/worksheets/sheet1.xml")

	// The amount arrives as a bare value, with its grouping commas removed.
	if !strings.Contains(sheet, "<v>1500000</v>") {
		t.Errorf("the amount should be a number, not text:\n%s", sheet)
	}
	// The name stays an inline string.
	if !strings.Contains(sheet, "علي محمد حسن") {
		t.Error("the student's name did not survive into the sheet")
	}
	if strings.Contains(sheet, "<v>2025001</v>") {
		t.Error("a student number is an identifier, not a quantity; it must stay text " +
			"or a spreadsheet will strip its leading zeros")
	}
}

func TestXLSXIsAValidArchiveWithEveryRequiredPart(t *testing.T) {
	var buf bytes.Buffer
	if err := table().Write(&buf, export.FormatXLSX); err != nil {
		t.Fatalf("writing: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("the file is not a readable archive: %v", err)
	}

	present := map[string]bool{}
	for _, file := range reader.File {
		present[file.Name] = true
	}
	for _, required := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml",
	} {
		if !present[required] {
			t.Errorf("the workbook is missing %s; Excel will refuse the whole file", required)
		}
	}
}

// Excel refuses a workbook whose sheet name is too long or carries a forbidden
// character — and refuses the file, not the name.
func TestSheetNameIsSanitised(t *testing.T) {
	report := table()
	report.Title = "Collections 2025/2026 [engineering]: a very long title indeed"

	var buf bytes.Buffer
	if err := report.Write(&buf, export.FormatXLSX); err != nil {
		t.Fatalf("writing: %v", err)
	}

	workbook := readZipEntry(t, buf.Bytes(), "xl/workbook.xml")
	for _, forbidden := range []string{"[", "]", ":", "/"} {
		if strings.Contains(sheetName(workbook), forbidden) {
			t.Errorf("the sheet name still contains %q", forbidden)
		}
	}
	if runes := []rune(sheetName(workbook)); len(runes) > 31 {
		t.Errorf("the sheet name is %d characters; Excel allows 31", len(runes))
	}
}

func TestPrintablePageIsSelfContainedAndRightToLeft(t *testing.T) {
	var buf bytes.Buffer
	if err := table().Write(&buf, export.FormatHTML); err != nil {
		t.Fatalf("writing: %v", err)
	}

	page := buf.String()
	if !strings.Contains(page, `dir="rtl"`) {
		t.Error("an Arabic report must render right to left")
	}
	// A page that fetched a stylesheet would print as unformatted text at a
	// desk with no network — the same reason receipts inline everything.
	for _, external := range []string{"<link", "src=\"http", "@import"} {
		if strings.Contains(page, external) {
			t.Errorf("the printable page references something external: %q", external)
		}
	}
	if !strings.Contains(page, "@page") {
		t.Error("the page should carry print geometry")
	}
	// The filter the report was run under has to be on the paper, or the page
	// will be misread on somebody's desk in March.
	if !strings.Contains(page, "2025-2026") {
		t.Error("the printed page must state what it covers")
	}
	if !strings.Contains(page, "finance.one") {
		t.Error("the printed page must say who produced it and when")
	}
}

// The PDF is a real PDF, not an HTML page with a misleading content type. A
// viewer opens it, a printer prints it, and it looks the same on both — which
// is the whole reason it stopped being HTML.
func TestPDFIsAPDFWithItsFontEmbedded(t *testing.T) {
	var buf bytes.Buffer
	if err := table().Write(&buf, export.FormatPDF); err != nil {
		t.Fatalf("writing: %v", err)
	}

	out := buf.Bytes()
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatalf("the output is not a PDF; it starts %q", firstBytes(out, 16))
	}
	if !bytes.Contains(out, []byte("%%EOF")) {
		t.Error("the PDF has no end-of-file marker — a viewer will call it damaged")
	}

	// The font has to travel inside the file. A PDF that names a font the
	// reader's machine does not have renders Arabic as empty boxes, which is
	// the failure this whole path exists to avoid.
	if !bytes.Contains(out, []byte("FontFile2")) {
		t.Error("no embedded font programme: Arabic will render as empty boxes " +
			"on any machine without Amiri installed")
	}
	if !bytes.Contains(out, []byte("amiri")) {
		t.Error("the embedded font is not the one this system ships")
	}

	if len(out) < 20_000 {
		t.Errorf("the PDF is %d bytes, too small to carry a font and a page", len(out))
	}
}

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func TestFormatParsing(t *testing.T) {
	cases := map[string]export.Format{
		"":      export.FormatCSV,
		"csv":   export.FormatCSV,
		"XLSX":  export.FormatXLSX,
		"excel": export.FormatXLSX,
		"pdf":   export.FormatPDF,
	}
	for input, want := range cases {
		got, err := export.Parse(input)
		if err != nil {
			t.Errorf("Parse(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := export.Parse("docx"); err == nil {
		t.Error("an unsupported format must be refused rather than silently becoming CSV")
	}
}

func TestFilenameCarriesTheDate(t *testing.T) {
	name := table().Filename(export.FormatCSV)
	if !strings.Contains(name, "2026-03-15") || !strings.HasSuffix(name, ".csv") {
		t.Errorf("filename = %q", name)
	}
}

// Every report title in this system is Arabic. A filename rule that keeps only
// ASCII therefore names them all the same thing, and an officer who exports
// three registers cannot tell the files apart without opening each. The test
// that existed asserted only that the date was present, which "--2026-03-15"
// satisfies — so it passed throughout.
func TestArabicTitlesProduceDistinctFilenames(t *testing.T) {
	at := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	titles := []string{"تقرير الديون", "سجل الإلغاءات", "أعمار الذمم"}

	seen := make(map[string]string, len(titles))
	for _, title := range titles {
		name := export.Table{Title: title, GeneratedAt: at}.Filename(export.FormatCSV)

		if !strings.Contains(name, title) {
			t.Errorf("filename for %q is %q; the title should survive into the name", title, name)
		}
		if earlier, clash := seen[name]; clash {
			t.Errorf("%q and %q both download as %q", earlier, title, name)
		}
		seen[name] = title
	}
}

// The ASCII fallback rides in filename=, which cannot carry anything else. It
// may collapse to a constant for Arabic titles — filename*= is what
// distinguishes them — but it must never be empty or a bare run of hyphens,
// because that is what a client reading only this parameter would save.
func TestASCIIFallbackIsAlwaysUsable(t *testing.T) {
	at := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	for _, title := range []string{"تقرير الديون", "   ", "///", "Year summary"} {
		name := export.Table{Title: title, GeneratedAt: at}.ASCIIFilename(export.FormatCSV)
		if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
			t.Errorf("ASCII fallback for %q is %q", title, name)
		}
		if !strings.HasSuffix(name, ".csv") {
			t.Errorf("ASCII fallback for %q lost its extension: %q", title, name)
		}
	}
}

// A title is data and a filename is a header value: a separator or a newline
// in one must not become part of a path or a second header line.
func TestFilenameStripsWhatAPathCannotCarry(t *testing.T) {
	at := time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	name := export.Table{
		Title:       "تقرير/الديون\nSet-Cookie: x",
		GeneratedAt: at,
	}.Filename(export.FormatCSV)

	for _, forbidden := range []string{"/", "\n", "\r"} {
		if strings.Contains(name, forbidden) {
			t.Errorf("filename %q still contains %q", name, forbidden)
		}
	}
}

func readZipEntry(t *testing.T, data []byte, name string) string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", name, err)
		}
		defer func() { _ = rc.Close() }()
		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(content)
	}
	t.Fatalf("%s is not in the archive", name)
	return ""
}

// sheetName pulls the name attribute out of the workbook part.
func sheetName(workbook string) string {
	const marker = `name="`
	start := strings.Index(workbook, marker)
	if start < 0 {
		return ""
	}
	rest := workbook[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
