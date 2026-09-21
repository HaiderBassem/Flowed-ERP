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
	"encoding/binary"
	"fmt"
	"math"
	"strings"

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
	ink      = gopdf.RGBColor{R: 24, G: 24, B: 27}
	muted    = gopdf.RGBColor{R: 113, G: 113, B: 122}
	hairline = gopdf.RGBColor{R: 212, G: 212, B: 216}
	zebra    = gopdf.RGBColor{R: 246, G: 246, B: 247}
	accent   = gopdf.RGBColor{R: 13, G: 94, B: 74}
	// paper is the page itself. See NewPage for why it is painted rather than
	// left to the viewer.
	paper     = gopdf.RGBColor{R: 255, G: 255, B: 255}
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
	// size is the geometry the document was created with. Kept so TrimHeight
	// can refuse to shorten a sheet, which is a roll-only operation.
	size Size
	// y is the current baseline, measured down from the top of the page.
	y float64
	// header redraws the letterhead when content spills onto a new page, so
	// page four of a debt report still says which university it belongs to.
	header func(d *Document)
	// footer is drawn at the bottom of every page, and numbers it.
	footer func(d *Document, page int)
	pageNo int
	err    error
	// missing collects every rune the font could not supply, in the order
	// first seen. See the OnGlyphNotFound hook in New.
	missing     []rune
	missingSeen map[rune]bool
}

// noteMissingGlyph records a character the font has no glyph for.
func (d *Document) noteMissingGlyph(r rune) {
	if d.missingSeen == nil {
		d.missingSeen = map[rune]bool{}
	}
	if d.missingSeen[r] {
		return
	}
	d.missingSeen[r] = true
	d.missing = append(d.missing, r)
}

// MissingGlyphs lists the characters the font could not draw.
func (d *Document) MissingGlyphs() []rune { return d.missing }

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
	// POS is an 80mm thermal roll: the printer that actually sits on a
	// counter. 80mm of paper is 72mm of printable width once the feed margins
	// are taken off, which is 204 points — narrow enough that everything has
	// to be one column, and that is the point. A receipt is read once, in a
	// queue, standing up.
	POS
)

// Roll geometry, in points. The height is a starting length, not a page: a
// thermal roll is continuous, and the driver cuts where the content ends.
const (
	posWidth  = 204
	posHeight = 1400
)

// NewRoll starts an 80mm document of a given length.
//
// gopdf fixes the page size when the document is created and offers no way to
// change it afterwards, so a receipt that should end where its content ends has
// to be measured before it is drawn. ThermalReceipt renders once to find the
// length and once to keep — cheap, because the expensive part is embedding the
// font and the measuring pass throws its output away.
//
// Without this every slip is followed by a hand's length of blank paper, which
// on a busy counter is most of a roll by the end of the day.
func NewRoll(height float64) (*Document, error) {
	doc, err := New(POS)
	if err != nil {
		return nil, err
	}
	if height > 0 {
		doc.height = height
		doc.pdf = &gopdf.GoPdf{}
		doc.pdf.Start(gopdf.Config{
			PageSize: gopdf.Rect{W: doc.width, H: height},
			Unit:     gopdf.UnitPT,
		})
		if err := doc.loadFonts(); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// New starts a document.
func New(size Size) (*Document, error) {
	doc := &Document{margin: 36, size: size}
	switch size {
	case A5:
		doc.width, doc.height = a5Width, a5Height
		doc.margin = 28
	case A4Landscape:
		doc.width, doc.height = a4Height, a4Width
	case POS:
		doc.width, doc.height = posWidth, posHeight
		// Four points of margin, because a thermal printer has no hardware
		// margin worth the name and every point of an 80mm roll is wanted.
		doc.margin = 8
	default:
		doc.width, doc.height = a4Width, a4Height
	}

	doc.pdf = &gopdf.GoPdf{}
	doc.pdf.Start(gopdf.Config{
		PageSize: gopdf.Rect{W: doc.width, H: doc.height},
		Unit:     gopdf.UnitPT,
	})

	if err := doc.loadFonts(); err != nil {
		return nil, err
	}
	return doc, nil
}

// loadFonts embeds both weights and wires the missing-glyph hook.
//
// Every glyph the font cannot supply is recorded. gopdf's default is to
// substitute a space and carry on, which is how a receipt loses a letter
// without anything anywhere saying so — the exact failure this hook exists to
// make impossible. A document that dropped a character is refused in Bytes
// rather than handed over looking almost right.
func (d *Document) loadFonts() error {
	option := gopdf.TtfOption{
		OnGlyphNotFound: func(r rune) { d.noteMissingGlyph(r) },
	}
	if err := d.pdf.AddTTFFontDataWithOption(regular, fontRegular, option); err != nil {
		return shared.Internal("pdf.font", err, "loading the regular font")
	}
	// No Style flag on the bold face. gopdf keys a font by family *and* style,
	// so registering it as Bold would mean SetFont(bold, "", size) finds
	// nothing — and the nil font it then measures against is a segfault, not a
	// refusal. The family names already tell the two apart.
	if err := d.pdf.AddTTFFontDataWithOption(bold, fontBold, option); err != nil {
		return shared.Internal("pdf.font", err, "loading the bold font")
	}
	return nil
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

	// Paint the sheet white before anything else.
	//
	// A PDF page has no background of its own. Most viewers paint one, and the
	// ones that do not — some print pipelines, some preview panes, anything
	// compositing onto a dark surface — render the page transparent, so every
	// unbanded row of a table comes out black with black text on it. The
	// document then looks like letters are missing, because they are: they are
	// there, drawn in ink on ink.
	//
	// One rectangle per page is the cheapest possible insurance against a
	// class of failure that is invisible on the machine that generated it.
	d.Box(0, 0, d.width, d.height, paper)

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
	// A document that lost a character is refused rather than handed over.
	//
	// gopdf substitutes a space for a glyph it cannot find, so the failure
	// arrives as a receipt with a gap where a letter should be — and a student
	// holding it has no way to know, and neither does the office. Naming the
	// characters is the whole point: "the font has no glyph for ٱ" is fixable,
	// "the printout looks wrong" is not.
	if len(d.missing) > 0 {
		return nil, shared.Internal("pdf.missing_glyphs", nil,
			"the embedded font has no glyph for %s; the document would print with "+
				"gaps where those characters belong", describeRunes(d.missing))
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
//
// A roll has no footer and no second page: it is cut where the content ends, so
// reserving a strip at the bottom only pushed the last few lines onto a page
// that should not exist. That is what produced a two-page thermal receipt with
// the total on the second.
func (d *Document) Room(height float64) bool {
	reserve := footerReserve
	if d.size == POS {
		reserve = 0
	}
	return d.y+height <= d.height-d.margin-float64(reserve)
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

	// The y gopdf is given *is* the baseline: cacheContentText.calY returns
	// pageHeight - y for text, and PDF's Td places the baseline there.
	//
	// This subtracted the font size first, which drew every string one size
	// too high — and by a different amount per size, so a 8pt label and a 10pt
	// value sharing a baseline sat two points apart. A page of that is text
	// visibly rising and falling along each line, and it is what put the total
	// band's figure half outside the band and ran a rule through the line
	// under it.
	d.pdf.SetXY(x, baseline)
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

// Image places an inline image inside a box, keeping its proportions, and
// returns the width it actually used.
//
// The proportions are the point. gopdf stretches an image to whatever rectangle
// it is given, so a 1920×1080 crest handed a square box comes out squashed to
// half its width — and the caller, having reserved a square, then draws text
// across the part of the box the image did not fill. Returning the true width
// is what lets a letterhead keep its text clear of the crest.
//
// A broken image must not stop a receipt: the letterhead still identifies the
// university, and a student waiting at a counter is worse served by an error
// than by a page with no crest on it. Zero is returned, and the caller lays out
// as though there were no image.
func (d *Document) Image(data []byte, x, y, maxW, maxH float64) float64 {
	holder, err := gopdf.ImageHolderByBytes(data)
	if err != nil {
		return 0
	}

	width, height := maxW, maxH
	if w, h, ok := imageSize(data); ok && w > 0 && h > 0 {
		scale := math.Min(maxW/float64(w), maxH/float64(h))
		width, height = float64(w)*scale, float64(h)*scale
	}

	// Right-aligned within the box and vertically centred, because the box is
	// reserved from the right edge of a right-to-left page.
	offsetX := x + (maxW - width)
	offsetY := y + (maxH-height)/2

	if err := d.pdf.ImageByHolder(holder, offsetX, offsetY, &gopdf.Rect{W: width, H: height}); err != nil {
		return 0
	}
	return width
}

// imageSize reads the pixel dimensions of a PNG or JPEG.
//
// Only the header is parsed. Decoding the whole image to learn its shape would
// cost a megabyte of allocation per receipt to answer a question the first
// twenty bytes contain.
func imageSize(data []byte) (width, height int, ok bool) {
	// PNG: an 8-byte signature, then an IHDR chunk whose first eight bytes of
	// payload are the dimensions.
	if len(data) >= 24 && bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) {
		w := binary.BigEndian.Uint32(data[16:20])
		h := binary.BigEndian.Uint32(data[20:24])
		return int(w), int(h), true
	}

	// JPEG: walk the segment markers to the start-of-frame, which carries the
	// dimensions. Anything else is skipped by its own declared length.
	if len(data) >= 4 && data[0] == 0xFF && data[1] == 0xD8 {
		for i := 2; i+9 < len(data); {
			if data[i] != 0xFF {
				i++
				continue
			}
			marker := data[i+1]
			switch {
			case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
				i += 2
				continue
			case marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC:
				h := binary.BigEndian.Uint16(data[i+5 : i+7])
				w := binary.BigEndian.Uint16(data[i+7 : i+9])
				return int(w), int(h), true
			}
			length := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
			if length < 2 {
				return 0, 0, false
			}
			i += 2 + length
		}
	}
	return 0, 0, false
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

// describeRunes names the characters a font could not draw, for an error a
// person can act on.
func describeRunes(runes []rune) string {
	parts := make([]string, 0, len(runes))
	for i, r := range runes {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("and %d more", len(runes)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%q (U+%04X)", r, r))
	}
	return strings.Join(parts, ", ")
}
