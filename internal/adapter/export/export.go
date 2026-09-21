// Package export renders a report as a file somebody can open.
//
// Finance offices and ministry submissions run on spreadsheets. A report that
// exists only as JSON is a report somebody re-types into Excel, and a re-typed
// figure is a figure that can be wrong — which is the whole reason this system
// exists.
//
// Three formats, and the choice between them is deliberate:
//
//   - CSV, because everything opens it and a bulk upload to a ministry portal
//     usually wants it. Written with a byte-order mark so Excel opens Arabic
//     correctly instead of showing ÙØ­Ù…Ø¯.
//   - XLSX, because a clerk who has to sort and total wants a real spreadsheet,
//     and because numbers stay numbers rather than becoming text that will not
//     add up.
//   - A print-ready HTML page for PDF, rather than a hand-rolled PDF writer.
//     An Arabic PDF needs an embedded font with correct shaping and
//     bidirectional layout; the browser already has both, and a page it prints
//     is right where several hundred lines of PDF plumbing would be subtly
//     wrong. The existing receipts take the same approach for the same reason.
//
// Nothing here decides what is in a report. It takes rows that a repository
// already produced under the caller's own authority and writes them out, so an
// export can never widen what somebody may see.
package export

import (
	"archive/zip"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"
	"time"

	"flowed/internal/domain/shared"
)

// Format is how a report is rendered.
type Format string

const (
	FormatCSV  Format = "csv"
	FormatXLSX Format = "xlsx"
	// FormatPDF renders a print-ready page. See the package comment for why it
	// is HTML rather than a PDF byte stream.
	FormatPDF Format = "pdf"
)

// Parse reads a requested format, defaulting to CSV.
func Parse(raw string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "csv":
		return FormatCSV, nil
	case "xlsx", "excel":
		return FormatXLSX, nil
	case "pdf", "print":
		return FormatPDF, nil
	default:
		return "", shared.Validation("export.unknown_format",
			"%q is not a format; use csv, xlsx or pdf", raw)
	}
}

// ContentType is what the format should be served as.
func (f Format) ContentType() string {
	switch f {
	case FormatXLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case FormatPDF:
		return "text/html; charset=utf-8"
	default:
		return "text/csv; charset=utf-8"
	}
}

// Extension is the filename suffix.
func (f Format) Extension() string {
	switch f {
	case FormatXLSX:
		return "xlsx"
	case FormatPDF:
		return "html"
	default:
		return "csv"
	}
}

// Column describes one field of a report.
type Column struct {
	// Header is what the column is called, in the language of whoever reads
	// it. Arabic for a page a finance officer prints.
	Header string
	// Numeric marks a column that must stay a number in a spreadsheet. A
	// figure written as text does not add up, and a total that silently
	// excludes half the rows is worse than no total.
	Numeric bool
}

// Table is a rendered report.
type Table struct {
	// Title appears on the printed page and names the sheet.
	Title string
	// Subtitle carries the filter the report was run under — the academic
	// year, the college, the date range. A printed page with no statement of
	// what it covers is a page that will be misread.
	Subtitle string
	Columns  []Column
	Rows     [][]string
	// GeneratedAt and GeneratedBy appear in the footer, because a spreadsheet
	// on somebody's desk in March needs to say when it was true.
	GeneratedAt time.Time
	GeneratedBy string
	Institution string
}

// Write renders the table in the requested format.
func (t Table) Write(w io.Writer, format Format) error {
	switch format {
	case FormatXLSX:
		return t.writeXLSX(w)
	case FormatPDF:
		return t.writeHTML(w)
	default:
		return t.writeCSV(w)
	}
}

// Filename suggests a name for the download.
func (t Table) Filename(format Format) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		case r == ' ', r == '-', r == '_':
			return '-'
		default:
			return -1
		}
	}, t.Title)
	if slug == "" {
		slug = "report"
	}
	return fmt.Sprintf("%s-%s.%s", slug, t.GeneratedAt.Format("2006-01-02"), format.Extension())
}

// writeCSV writes a spreadsheet-friendly CSV.
func (t Table) writeCSV(w io.Writer) error {
	// Excel reads a UTF-8 CSV as the local code page unless it finds a
	// byte-order mark, and an Arabic name then arrives as mojibake. The mark
	// costs three bytes and saves the export being unusable.
	if _, err := io.WriteString(w, "\ufeff"); err != nil {
		return err
	}

	writer := csv.NewWriter(w)
	headers := make([]string, len(t.Columns))
	for i, column := range t.Columns {
		headers[i] = column.Header
	}
	if err := writer.Write(headers); err != nil {
		return err
	}
	for _, row := range t.Rows {
		if err := writer.Write(row); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

// writeXLSX writes a minimal but valid spreadsheet.
//
// Hand-built rather than pulled from a dependency: what an export needs is one
// sheet of strings and numbers, and the format for that is a zip of four small
// XML parts. A library would bring styling, charts, formulas and a supply-chain
// surface for a file this system writes and never reads.
func (t Table) writeXLSX(w io.Writer) error {
	archive := zip.NewWriter(w)

	parts := []struct {
		name    string
		content string
	}{
		{"[Content_Types].xml", contentTypesXML},
		{"_rels/.rels", rootRelsXML},
		{"xl/workbook.xml", workbookXML(t.Title)},
		{"xl/_rels/workbook.xml.rels", workbookRelsXML},
		{"xl/worksheets/sheet1.xml", t.sheetXML()},
	}

	for _, part := range parts {
		writer, err := archive.Create(part.name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(writer, part.content); err != nil {
			return err
		}
	}
	return archive.Close()
}

// sheetXML renders the rows.
//
// Numbers are written as numbers and everything else as an inline string.
// Inline rather than through a shared-strings table because a report is written
// once and read once: the table would save space in a file nobody keeps and
// would add a part that has to stay consistent with the sheet.
func (t Table) sheetXML() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)

	rowIndex := 1
	writeRow := func(values []string, numeric []bool) {
		fmt.Fprintf(&b, `<row r="%d">`, rowIndex)
		for i, value := range values {
			cell := cellReference(i, rowIndex)
			isNumeric := numeric != nil && i < len(numeric) && numeric[i]
			if isNumeric && looksNumeric(value) {
				fmt.Fprintf(&b, `<c r="%s"><v>%s</v></c>`, cell, escapeXML(stripSeparators(value)))
				continue
			}
			fmt.Fprintf(&b, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`,
				cell, escapeXML(value))
		}
		b.WriteString(`</row>`)
		rowIndex++
	}

	headers := make([]string, len(t.Columns))
	numeric := make([]bool, len(t.Columns))
	for i, column := range t.Columns {
		headers[i], numeric[i] = column.Header, column.Numeric
	}

	if t.Subtitle != "" {
		writeRow([]string{t.Title, t.Subtitle}, nil)
		writeRow(nil, nil)
	}
	writeRow(headers, nil)
	for _, row := range t.Rows {
		writeRow(row, numeric)
	}

	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// writeHTML renders a print-ready A4 page.
//
// Right-to-left, self-contained, and styled for paper rather than for a screen.
// A finance office prints this and files it; a page that needed a stylesheet
// from somewhere else would print as a column of unformatted text, which is the
// same reasoning the receipts already follow.
func (t Table) writeHTML(w io.Writer) error {
	var b strings.Builder
	b.WriteString(`<!doctype html><html dir="rtl" lang="ar"><head><meta charset="utf-8">`)
	fmt.Fprintf(&b, `<title>%s</title>`, html.EscapeString(t.Title))
	b.WriteString(`<style>
@page { size: A4 landscape; margin: 12mm; }
body { font-family: "Segoe UI", Tahoma, "Noto Naskh Arabic", sans-serif; color: #111; margin: 0; }
h1 { font-size: 16pt; margin: 0 0 2mm; }
.subtitle { font-size: 10pt; color: #444; margin-bottom: 4mm; }
table { border-collapse: collapse; width: 100%; font-size: 9pt; }
th, td { border: 1px solid #999; padding: 2mm 3mm; text-align: right; }
th { background: #eee; font-weight: 600; }
/* Figures read as columns of digits, which is what makes an error visible. */
td.num { font-variant-numeric: tabular-nums; direction: ltr; text-align: left; }
tbody tr:nth-child(even) { background: #fafafa; }
footer { margin-top: 4mm; font-size: 8pt; color: #555; display: flex; justify-content: space-between; }
@media print { footer { position: fixed; bottom: 0; left: 0; right: 0; } }
</style></head><body>`)

	if t.Institution != "" {
		fmt.Fprintf(&b, `<div class="subtitle">%s</div>`, html.EscapeString(t.Institution))
	}
	fmt.Fprintf(&b, `<h1>%s</h1>`, html.EscapeString(t.Title))
	if t.Subtitle != "" {
		fmt.Fprintf(&b, `<div class="subtitle">%s</div>`, html.EscapeString(t.Subtitle))
	}

	b.WriteString(`<table><thead><tr>`)
	for _, column := range t.Columns {
		fmt.Fprintf(&b, `<th>%s</th>`, html.EscapeString(column.Header))
	}
	b.WriteString(`</tr></thead><tbody>`)

	for _, row := range t.Rows {
		b.WriteString(`<tr>`)
		for i, value := range row {
			class := ""
			if i < len(t.Columns) && t.Columns[i].Numeric {
				class = ` class="num"`
			}
			fmt.Fprintf(&b, `<td%s>%s</td>`, class, html.EscapeString(value))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)

	fmt.Fprintf(&b, `<footer><span>%s</span><span>%d row(s)</span></footer>`,
		html.EscapeString(t.footerText()), len(t.Rows))
	b.WriteString(`</body></html>`)

	_, err := io.WriteString(w, b.String())
	return err
}

func (t Table) footerText() string {
	stamp := t.GeneratedAt.Format("2006-01-02 15:04")
	if t.GeneratedBy == "" {
		return "Generated " + stamp
	}
	return fmt.Sprintf("Generated %s by %s", stamp, t.GeneratedBy)
}

// cellReference renders A1, B1, ... AA1 for a zero-based column.
func cellReference(column, row int) string {
	name := ""
	for column >= 0 {
		name = string(rune('A'+(column%26))) + name
		column = column/26 - 1
	}
	return name + strconv.Itoa(row)
}

// looksNumeric reports whether a value can be written as a spreadsheet number.
func looksNumeric(value string) bool {
	trimmed := stripSeparators(value)
	if trimmed == "" {
		return false
	}
	_, err := strconv.ParseFloat(trimmed, 64)
	return err == nil
}

// stripSeparators removes the grouping commas a formatted amount carries.
func stripSeparators(value string) string {
	return strings.NewReplacer(",", "", " ", "", " ", "").Replace(strings.TrimSpace(value))
}

func escapeXML(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

const workbookRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`

// workbookXML names the single sheet.
//
// The name is trimmed and sanitised because Excel refuses a sheet name over 31
// characters or containing []:*?/\ — and refuses the whole file, not the name.
func workbookXML(title string) string {
	name := strings.Map(func(r rune) rune {
		switch r {
		case '[', ']', ':', '*', '?', '/', '\\':
			return '-'
		default:
			return r
		}
	}, strings.TrimSpace(title))
	if name == "" {
		name = "Report"
	}
	if runes := []rune(name); len(runes) > 31 {
		name = string(runes[:31])
	}

	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
 xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="` + escapeXML(name) + `" sheetId="1" r:id="rId1"/></sheets>
</workbook>`
}
