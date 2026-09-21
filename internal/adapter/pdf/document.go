// Package pdf renders right-to-left Arabic documents: receipts a student
// carries away and reports an office files.
//
// It exists because the alternative was asking a browser to print. That works
// until the office has no browser on the machine by the counter, or prints from
// a different one and gets different margins, or nobody switches off the
// browser's own headers and every receipt carries "localhost:8090/app/…" across
// the top. A PDF is the same document on every machine, which is the property
// that matters for a paper a student presents to a ministry.
//
// Everything a caller draws goes through arabic.Shape first — see that package
// for why PDF cannot do it — and everything is positioned from the right edge,
// because that is where a right-to-left page begins.
package pdf

import (
	"bytes"
	_ "embed"

	"github.com/signintech/gopdf"

	"flowed/internal/adapter/pdf/arabic"
	"flowed/internal/domain/shared"
)

// The fonts are embedded rather than read from disk beside the binary. A
// receipt that depends on such a file prints as empty boxes the first time
// somebody copies the binary somewhere else, and nobody notices until a student
// is holding one.
//
//go:embed fonts/Amiri-Regular.ttf
var fontRegular []byte

//go:embed fonts/Amiri-Bold.ttf
var fontBold []byte

// Font weights, named so call sites read as typography rather than as strings.
//
// Amiri, a Naskh book face, and the choice is load-bearing rather than
// aesthetic. The first font tried — Noto Naskh Arabic from the noto-fonts
// repository — covers Arabic and nothing else, so a receipt number came out as
// "2025 2026 000008": the R and every hyphen had no glyph and were dropped
// silently, which is the worst way for a font to be wrong. Amiri carries
// Arabic, its presentation forms, the lam-alef ligatures, Latin, digits and
// punctuation in one file, so no run of text can fall through it.
const (
	regular = "amiri"
	bold    = "amiri-bold"
)

// Page geometry, in points. A4 for reports, A5 for receipts — an A5 receipt is
// half a sheet, which is what an office guillotine produces from stock it
// already buys.
const (
	a4Width  = 595.28
	a4Height = 841.89
	a5Width  = 419.53
	a5Height = 595.28
)

// The palette. Three greys and one accent, because a document using six colours
// to say six things ends up saying none of them — and because most of these
// print on a laser printer that renders every one of them as grey anyway.
var (
	ink       = gopdf.RGBColor{R: 24, G: 24, B: 27}
	muted     = gopdf.RGBColor{R: 113, G: 113, B: 122}
	hairline  = gopdf.RGBColor{R: 212, G: 212, B: 216}
	zebra     = gopdf.RGBColor{R: 246, G: 246, B: 247}
	accent    = gopdf.RGBColor{R: 13, G: 94, B: 74}
	accentPal = gopdf.RGBColor{R: 233, G: 243, B: 240}
)

// Document is a page under construction.
//
// It tracks the cursor itself rather than exposing gopdf's, because every
// primitive here has to know how much room is left before it draws anything: a
// table row that discovers it has run off the page after drawing half of itself
// is the "شي فوك وشي جوه" this package exists to prevent.
type Document struct {
	pdf    *gopdf.GoPdf
	width  float64
	height float64
	// margin is the same on all four sides. A document whose left margin
	// differs from its right looks wrong to a reader who cannot say why, and on
	// a right-to-left page the asymmetry is twice as visible.
	margin float64
	// y is the current baseline, measured down from the top of the page.
	y float64
	// header redraws the letterhead when content spills onto a new page, so
	// page four of a debt report still says which university it belongs to.
	header func(d *Document)
	// footer is drawn at the bottom of every page, and numbers it.
	footer func(d *Document, page int)
	pageNo int
	err    error
}

// Size selects the page geometry.
type Size int

const (
	// A4 is for reports: a filing cabinet, a ministry, a printer's default tray.
	A4 Size = iota
	// A5 is for receipts: half a sheet, which is what a guillotine produces.
	A5
	// A4Landscape is for a report too wide to read down a portrait page. Used
	// only when a table genuinely has more columns than portrait can hold —
	// turning every report sideways to save one would be worse.
	A4Landscape
)

// New starts a document.
func New(size Size) (*Document, error) {
	doc := &Document{margin: 36}
	switch size {
	case A5:
		doc.width, doc.height = a5Width, a5Height
		doc.margin = 28
	case A4Landscape:
		doc.width, doc.height = a4Height, a4Width
	default:
		doc.width, doc.height = a4Width, a4Height
	}

	doc.pdf = &gopdf.GoPdf{}
	doc.pdf.Start(gopdf.Config{
		PageSize: gopdf.Rect{W: doc.width, H: doc.height},
		Unit:     gopdf.UnitPT,
	})

	if err := doc.pdf.AddTTFFontData(regular, fontRegular); err != nil {
		return nil, shared.Internal("pdf.font", err, "loading the regular font")
	}
	if err := doc.pdf.AddTTFFontData(bold, fontBold); err != nil {
		return nil, shared.Internal("pdf.font", err, "loading the bold font")
	}
	return doc, nil
}

// WithHeader sets the letterhead, redrawn at the top of every page.
func (d *Document) WithHeader(header func(*Document)) *Document {
	d.header = header
	return d
}

// WithFooter sets the strip drawn at the bottom of every page.
func (d *Document) WithFooter(footer func(*Document, int)) *Document {
	d.footer = footer
	return d
}

// NewPage starts a page and draws the letterhead on it.
func (d *Document) NewPage() {
	if d.pageNo > 0 && d.footer != nil {
		d.footer(d, d.pageNo)
	}
	d.pdf.AddPage()
	d.pageNo++
	d.y = d.margin
	if d.header != nil {
		d.header(d)
	}
}

// Pages reports how many pages have been started.
func (d *Document) Pages() int { return d.pageNo }

// Bytes finishes the document and returns it.
func (d *Document) Bytes() ([]byte, error) {
	if d.err != nil {
		return nil, d.err
	}
	if d.pageNo > 0 && d.footer != nil {
		d.footer(d, d.pageNo)
	}
	var buf bytes.Buffer
	if _, err := d.pdf.WriteTo(&buf); err != nil {
		return nil, shared.Internal("pdf.write", err, "writing the document")
	}
	return buf.Bytes(), nil
}

// Right is the x coordinate a right-to-left line starts at.
func (d *Document) Right() float64 { return d.width - d.margin }

// Left is the x coordinate a right-to-left line ends at.
func (d *Document) Left() float64 { return d.margin }

// ContentWidth is the usable width between the margins.
func (d *Document) ContentWidth() float64 { return d.width - 2*d.margin }

// Height is the page height.
func (d *Document) Height() float64 { return d.height }

// Margin is the space kept clear on all four sides.
func (d *Document) Margin() float64 { return d.margin }

// Y is the current vertical position.
func (d *Document) Y() float64 { return d.y }

// SetY moves the cursor.
func (d *Document) SetY(y float64) { d.y = y }

// Space advances the cursor without drawing.
func (d *Document) Space(points float64) { d.y += points }

// footerReserve is the strip kept clear at the bottom for the page footer.
const footerReserve = 26

// Room reports whether the given height fits before the bottom margin, leaving
// space for the footer.
func (d *Document) Room(height float64) bool {
	return d.y+height <= d.height-d.margin-footerReserve
}

// EnsureRoom starts a new page if the given height would not fit.
//
// Called by every primitive before it draws anything, which is the whole
// discipline: a row that checks after drawing its first cell is a row split
// across two pages.
func (d *Document) EnsureRoom(height float64) {
	if d.pageNo == 0 || !d.Room(height) {
		d.NewPage()
	}
}

// Align says which edge a piece of text is placed against.
type Align int

const (
	// AlignRight is the default for Arabic prose.
	AlignRight Align = iota
	// AlignLeft is for a Latin identifier or a figure in a left-hand column.
	AlignLeft
	// AlignCenter is for a title.
	AlignCenter
)

// TextStyle describes one run of text.
type TextStyle struct {
	Size  float64
	Bold  bool
	Color gopdf.RGBColor
	Align Align
}

// Text draws one line and advances the cursor by its height.
func (d *Document) Text(text string, style TextStyle) {
	height := style.Size * 1.6
	d.EnsureRoom(height)
	d.TextAt(text, d.Left(), d.Right(), d.y+style.Size, style)
	d.y += height
}

// TextAt draws one line between two x coordinates without moving the cursor.
//
// The baseline is given rather than derived, because a caller placing a label
// and a value on one line needs both to sit on the same baseline — deriving it
// twice from two different sizes is how they end up a point apart, which is
// visible and looks like a mistake because it is one.
func (d *Document) TextAt(text string, left, right, baseline float64, style TextStyle) {
	if text == "" {
		return
	}
	d.setFont(style)

	shaped := arabic.Shape(text)
	width, err := d.pdf.MeasureTextWidth(shaped)
	if err != nil {
		d.fail(err, "measuring text")
		return
	}

	x := right - width
	switch style.Align {
	case AlignLeft:
		x = left
	case AlignCenter:
		x = left + (right-left-width)/2
	}

	d.pdf.SetXY(x, baseline-style.Size)
	if err := d.pdf.Text(shaped); err != nil {
		d.fail(err, "drawing text")
	}
}

// TextWidth measures a string as it will be drawn.
func (d *Document) TextWidth(text string, style TextStyle) float64 {
	d.setFont(style)
	width, err := d.pdf.MeasureTextWidth(arabic.Shape(text))
	if err != nil {
		d.fail(err, "measuring text")
		return 0
	}
	return width
}

// Paragraph draws text wrapped to the content width.
//
// Wrapping is done on measured width rather than on a character count: Arabic
// glyphs vary enough in width that counting characters produces one line that
// overflows and one that is half empty.
func (d *Document) Paragraph(text string, style TextStyle) {
	for _, line := range d.Wrap(text, d.ContentWidth(), style) {
		d.Text(line, style)
	}
}

// Wrap breaks text into lines that fit a width.
func (d *Document) Wrap(text string, width float64, style TextStyle) []string {
	words := splitWords(text)
	if len(words) == 0 {
		return nil
	}

	var lines []string
	current := words[0]
	for _, word := range words[1:] {
		candidate := current + " " + word
		if d.TextWidth(candidate, style) <= width {
			current = candidate
			continue
		}
		lines = append(lines, current)
		current = word
	}
	return append(lines, current)
}

func splitWords(text string) []string {
	var words []string
	var current []rune
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' {
			if len(current) > 0 {
				words = append(words, string(current))
				current = nil
			}
			continue
		}
		current = append(current, r)
	}
	if len(current) > 0 {
		words = append(words, string(current))
	}
	return words
}

// Rule draws a horizontal hairline across the content width.
func (d *Document) Rule(color gopdf.RGBColor, thickness float64) {
	d.EnsureRoom(thickness + 6)
	d.pdf.SetLineWidth(thickness)
	d.pdf.SetStrokeColor(color.R, color.G, color.B)
	d.pdf.Line(d.Left(), d.y, d.Right(), d.y)
	d.y += thickness + 6
}

// RuleAt draws a hairline at a given y without moving the cursor.
func (d *Document) RuleAt(y float64, color gopdf.RGBColor, thickness float64) {
	d.pdf.SetLineWidth(thickness)
	d.pdf.SetStrokeColor(color.R, color.G, color.B)
	d.pdf.Line(d.Left(), y, d.Right(), y)
}

// Box fills a rectangle.
func (d *Document) Box(x, y, w, h float64, fill gopdf.RGBColor) {
	d.pdf.SetFillColor(fill.R, fill.G, fill.B)
	if err := d.pdf.Rectangle(x, y, x+w, y+h, "F", 0, 0); err != nil {
		d.fail(err, "drawing a box")
	}
}

// Image places an inline image, scaled into a box.
//
// A broken logo must not stop a receipt: the rest of the letterhead still
// identifies the university, and a student waiting at the counter is worse
// served by an error page than by one with no crest on it.
func (d *Document) Image(data []byte, x, y, maxW, maxH float64) {
	holder, err := gopdf.ImageHolderByBytes(data)
	if err != nil {
		return
	}
	_ = d.pdf.ImageByHolder(holder, x, y, &gopdf.Rect{W: maxW, H: maxH})
}

// setFont selects the weight, size and colour for the next draw.
func (d *Document) setFont(style TextStyle) {
	family := regular
	if style.Bold {
		family = bold
	}
	size := style.Size
	if size == 0 {
		size = 10
	}
	if err := d.pdf.SetFont(family, "", size); err != nil {
		d.fail(err, "selecting a font")
		return
	}
	color := style.Color
	if color == (gopdf.RGBColor{}) {
		color = ink
	}
	d.pdf.SetTextColor(color.R, color.G, color.B)
}

// fail records the first error and lets later calls no-op.
//
// The alternative is an error return on every draw, and a renderer where nine
// tenths of the lines handle a failure that cannot happen once the font has
// loaded. The first error wins, because the ones after it are consequences.
func (d *Document) fail(err error, what string) {
	if d.err == nil {
		d.err = shared.Internal("pdf.render", err, "%s", what)
	}
}

// The palette, exported so the receipt and report renderers share one set of
// greys rather than each inventing its own.
func Ink() gopdf.RGBColor        { return ink }
func Muted() gopdf.RGBColor      { return muted }
func Hairline() gopdf.RGBColor   { return hairline }
func Zebra() gopdf.RGBColor      { return zebra }
func Accent() gopdf.RGBColor     { return accent }
func AccentPale() gopdf.RGBColor { return accentPal }
