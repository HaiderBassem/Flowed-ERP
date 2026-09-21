package pdf

import "github.com/signintech/gopdf"

// The composite pieces every document in this system is built from: a title, a
// block of label/value pairs, a table, a total bar. They live here rather than
// in the two renderers so that a receipt and a report look like documents from
// the same office — which is most of what "professional" means on paper.

// Title draws a document's name, centred, with a rule beneath it.
func (d *Document) Title(text, subtitle string) {
	d.EnsureRoom(52)
	d.Text(text, TextStyle{Size: 16, Bold: true, Align: AlignCenter, Color: ink})
	if subtitle != "" {
		d.Space(-4)
		d.Text(subtitle, TextStyle{Size: 9, Align: AlignCenter, Color: muted})
	}
	d.Space(2)
	d.Rule(accent, 1.2)
}

// SectionTitle labels a block within a document.
func (d *Document) SectionTitle(text string) {
	d.EnsureRoom(28)
	d.Space(6)
	d.Text(text, TextStyle{Size: 11, Bold: true, Color: accent})
	d.Space(-4)
	d.Rule(hairline, 0.5)
}

// Field is one label and its value.
type Field struct {
	Label string
	Value string
	// Strong renders the value in bold — for the one figure on the page a
	// reader is actually looking for.
	Strong bool
	// LTR draws the value left-aligned in its half, for a receipt number or a
	// date that reads left to right even on a right-to-left page.
	LTR bool
}

// Fields draws label/value pairs on a grid with the labels in a column.
//
// The labels are right-aligned against a fixed gutter and the values all start
// at the same place, so the eye runs down one line instead of hunting. The
// earlier version measured each label and placed its value beside it, which put
// every value at a different x — a page of that reads as text going up and down
// even though every baseline is level.
//
// A value too wide for its cell is shortened rather than allowed to run into
// the column beside it. Overflow is what actually produces overlapping text,
// and a long department name is not rare enough to leave to chance.
func (d *Document) Fields(fields []Field, columns int) {
	if len(fields) == 0 {
		return
	}
	if columns < 1 {
		columns = 1
	}

	const (
		rowHeight = 20
		gutter    = 8
	)

	cell := d.ContentWidth() / float64(columns)

	// One label column width for the whole block, from the widest label in it.
	// Sizing each row to its own label is what made the values ragged.
	labelStyle := TextStyle{Size: 8.5, Color: muted}
	labelWidth := 0.0
	for _, field := range fields {
		if w := d.TextWidth(field.Label+":", labelStyle); w > labelWidth {
			labelWidth = w
		}
	}
	// Never more than half the cell: a long label must not squeeze its value
	// into nothing.
	if max := cell * 0.45; labelWidth > max {
		labelWidth = max
	}

	for i := 0; i < len(fields); i += columns {
		d.EnsureRoom(rowHeight)
		baseline := d.y + 13

		for c := 0; c < columns && i+c < len(fields); c++ {
			field := fields[i+c]
			// Right to left: the first column is the rightmost one.
			right := d.Right() - float64(c)*cell
			left := right - cell + gutter

			d.TextAt(d.Ellipsise(field.Label+":", labelWidth, labelStyle),
				right-labelWidth, right, baseline, labelStyle)

			valueRight := right - labelWidth - gutter
			valueStyle := TextStyle{Size: 10, Bold: field.Strong, Color: ink}
			if field.LTR {
				valueStyle.Align = AlignLeft
			}
			d.TextAt(d.Ellipsise(field.Value, valueRight-left, valueStyle),
				left, valueRight, baseline, valueStyle)
		}
		d.y += rowHeight
	}
}

// Column describes one column of a table.
type Column struct {
	Header string
	// Width is a share of the content width, not an absolute. Shares are
	// normalised, so a caller writes 3, 2, 1 without working out points and the
	// table still fills the page exactly.
	Width float64
	// Numeric left-aligns the cell, which on a right-to-left page is what makes
	// a column of figures line up on its last digit — the only way amounts can
	// be compared down a page.
	Numeric bool
	// Strong renders the column in bold.
	Strong bool
}

// Table draws a header row and the rows beneath it, paginating as it goes.
//
// The header is redrawn at the top of every page the table spills onto. A table
// whose second page is a wall of unlabelled figures is a table nobody can read,
// and it is the most common way a generated report goes wrong.
func (d *Document) Table(columns []Column, rows [][]string) {
	if len(columns) == 0 {
		return
	}

	var total float64
	for _, column := range columns {
		total += column.Width
	}
	if total == 0 {
		for i := range columns {
			columns[i].Width = 1
		}
		total = float64(len(columns))
	}

	widths := make([]float64, len(columns))
	for i, column := range columns {
		widths[i] = d.ContentWidth() * column.Width / total
	}

	const (
		headerHeight = 22
		rowHeight    = 18
		padding      = 6
	)

	drawHeader := func() {
		d.Box(d.Left(), d.y, d.ContentWidth(), headerHeight, accentPal)
		baseline := d.y + 15
		right := d.Right()
		for i, column := range columns {
			left := right - widths[i]
			style := TextStyle{Size: 8.5, Bold: true, Color: accent}
			if column.Numeric {
				style.Align = AlignLeft
			}
			d.TextAt(column.Header, left+padding, right-padding, baseline, style)
			right = left
		}
		d.y += headerHeight
	}

	d.EnsureRoom(headerHeight + rowHeight)
	drawHeader()

	for index, row := range rows {
		if !d.Room(rowHeight) {
			d.NewPage()
			drawHeader()
		}
		// Alternating bands rather than a grid of lines. Ruling every cell
		// makes a page look like a spreadsheet nobody meant to print; a band on
		// every other row does the same job — holding the eye on one line
		// across the page — without the ink.
		if index%2 == 1 {
			d.Box(d.Left(), d.y, d.ContentWidth(), rowHeight, zebra)
		}

		baseline := d.y + 12.5
		right := d.Right()
		for i, column := range columns {
			left := right - widths[i]
			if i < len(row) {
				style := TextStyle{Size: 9, Bold: column.Strong, Color: ink}
				if column.Numeric {
					style.Align = AlignLeft
				}
				text := d.Ellipsise(row[i], widths[i]-2*padding, style)
				d.TextAt(text, left+padding, right-padding, baseline, style)
			}
			right = left
		}
		d.y += rowHeight
	}

	d.RuleAt(d.y, hairline, 0.5)
	d.y += 8
}

// Ellipsise shortens text that will not fit, rather than letting it run into
// the next column.
//
// Overflow is the failure that makes a generated table look broken: one long
// department name pushes into the column beside it, and every figure on that
// row then reads as belonging to the wrong heading.
func (d *Document) Ellipsise(text string, width float64, style TextStyle) string {
	if text == "" || d.TextWidth(text, style) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		candidate := string(runes) + "…"
		if d.TextWidth(candidate, style) <= width {
			return candidate
		}
	}
	return string(runes)
}

// TotalBar draws the figure a reader is looking for, in a filled band.
//
// A total that is merely the last row of a table is a total somebody has to
// find. This is the only filled band on the page, which is what makes it
// findable at arm's length.
func (d *Document) TotalBar(label, value string) {
	const height = 30
	d.EnsureRoom(height + 8)
	d.Space(4)

	d.Box(d.Left(), d.y, d.ContentWidth(), height, accent)
	baseline := d.y + 20

	// The value takes the width it needs and the label takes what is left.
	// Drawing both across the full band let a long label run under the figure,
	// which on the one line of a receipt anybody actually reads is the worst
	// place on the page for text to collide.
	white := gopdf.RGBColor{R: 255, G: 255, B: 255}
	valueStyle := TextStyle{Size: 13, Bold: true, Color: white, Align: AlignLeft}
	valueWidth := d.TextWidth(value, valueStyle)

	d.TextAt(value, d.Left()+10, d.Right()-10, baseline, valueStyle)

	labelStyle := TextStyle{Size: 10, Color: white}
	labelRight := d.Right() - 10
	labelLeft := d.Left() + 10 + valueWidth + 12
	d.TextAt(d.Ellipsise(label, labelRight-labelLeft, labelStyle),
		labelLeft, labelRight, baseline, labelStyle)

	d.y += height + 6
}

// Note draws a small muted line — a caveat, a generated-at stamp, a remark the
// reader needs but should not read first.
func (d *Document) Note(text string) {
	d.Paragraph(text, TextStyle{Size: 8, Color: muted})
}

// SignatureLine draws the ruled blanks a paper document is signed on.
//
// Two of them side by side, because an Iraqi receipt is signed by the cashier
// and by the student — and a receipt with one line is one somebody has to
// explain at the counter.
func (d *Document) SignatureLine(labels ...string) {
	if len(labels) == 0 {
		return
	}
	const height = 46
	d.EnsureRoom(height)
	d.Space(14)

	cell := d.ContentWidth() / float64(len(labels))
	lineY := d.y + 20

	for i, label := range labels {
		right := d.Right() - float64(i)*cell
		left := right - cell + 16

		d.pdf.SetLineWidth(0.6)
		d.pdf.SetStrokeColor(hairline.R, hairline.G, hairline.B)
		d.pdf.Line(left, lineY, right, lineY)

		d.TextAt(label, left, right, lineY+12,
			TextStyle{Size: 8, Color: muted, Align: AlignCenter})
	}
	d.y += height
}
