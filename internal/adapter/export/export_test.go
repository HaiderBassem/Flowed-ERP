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
	if err := table().Write(&buf, export.FormatPDF); err != nil {
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
