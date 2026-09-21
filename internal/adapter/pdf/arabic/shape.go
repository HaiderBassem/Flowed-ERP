// Package arabic turns logical Arabic text into the glyph sequence a PDF
// viewer will draw correctly.
//
// PDF has no text engine. A viewer draws the glyphs it is given, left to right,
// in the order given — so handing it the bytes of "محمد" produces four
// disconnected, backwards letters. Two things have to happen first, and neither
// is something the PDF library does:
//
//  1. **Shaping.** Arabic letters change form by position: ع alone, عـ at the
//     start of a word, ـعـ in the middle, ـع at the end. Unicode keeps those
//     four forms as separate codepoints in the presentation-forms block
//     (U+FE70–U+FEFF), which is what the embedded font actually has glyphs for.
//     Choosing between them is a question about a letter's neighbours.
//
//  2. **Ordering.** Arabic reads right to left. Since the viewer draws left to
//     right, the run has to be reversed before it is handed over — but only the
//     Arabic runs. A receipt number or an amount inside an Arabic sentence is
//     still read left to right, and reversing it would print 000917 as 719000.
//
// This is a deliberate subset of the Unicode bidirectional algorithm (UAX #9),
// not an implementation of it. It handles the case this system produces: Arabic
// prose with Western digits, Latin identifiers and punctuation embedded in it.
// Nested directional overrides, Hebrew and explicit bidi controls are not
// handled, because no receipt and no report here contains them.
package arabic

import "strings"

// form indexes the four shapes a letter can take.
type form int

const (
	isolated form = iota
	final
	initial
	medial
)

// letter describes one Arabic letter's presentation forms.
//
// linksNext says whether a letter joins the one that follows it. Six letters —
// ا د ذ ر ز و — do not: they connect to what came before and then break the
// run, which is why بـرـيـد is wrong and بريد is right.
type letter struct {
	forms     [4]rune
	linksNext bool
}

// shapes maps each Arabic letter to its presentation forms.
//
// Written out rather than computed: the presentation-forms block is not
// contiguous with the Arabic block and has no arithmetic relationship to it.
// Every attempt to derive one produces a table with holes in exactly the
// letters nobody tests.
var shapes = map[rune]letter{
	0x0621: {[4]rune{0xFE80, 0xFE80, 0xFE80, 0xFE80}, false}, // ء hamza
	0x0622: {[4]rune{0xFE81, 0xFE82, 0xFE81, 0xFE82}, false}, // آ alef madda
	0x0623: {[4]rune{0xFE83, 0xFE84, 0xFE83, 0xFE84}, false}, // أ alef hamza above
	0x0624: {[4]rune{0xFE85, 0xFE86, 0xFE85, 0xFE86}, false}, // ؤ waw hamza
	0x0625: {[4]rune{0xFE87, 0xFE88, 0xFE87, 0xFE88}, false}, // إ alef hamza below
	0x0626: {[4]rune{0xFE89, 0xFE8A, 0xFE8B, 0xFE8C}, true},  // ئ yeh hamza
	0x0627: {[4]rune{0xFE8D, 0xFE8E, 0xFE8D, 0xFE8E}, false}, // ا alef
	0x0628: {[4]rune{0xFE8F, 0xFE90, 0xFE91, 0xFE92}, true},  // ب beh
	0x0629: {[4]rune{0xFE93, 0xFE94, 0xFE93, 0xFE94}, false}, // ة teh marbuta
	0x062A: {[4]rune{0xFE95, 0xFE96, 0xFE97, 0xFE98}, true},  // ت teh
	0x062B: {[4]rune{0xFE99, 0xFE9A, 0xFE9B, 0xFE9C}, true},  // ث theh
	0x062C: {[4]rune{0xFE9D, 0xFE9E, 0xFE9F, 0xFEA0}, true},  // ج jeem
	0x062D: {[4]rune{0xFEA1, 0xFEA2, 0xFEA3, 0xFEA4}, true},  // ح hah
	0x062E: {[4]rune{0xFEA5, 0xFEA6, 0xFEA7, 0xFEA8}, true},  // خ khah
	0x062F: {[4]rune{0xFEA9, 0xFEAA, 0xFEA9, 0xFEAA}, false}, // د dal
	0x0630: {[4]rune{0xFEAB, 0xFEAC, 0xFEAB, 0xFEAC}, false}, // ذ thal
	0x0631: {[4]rune{0xFEAD, 0xFEAE, 0xFEAD, 0xFEAE}, false}, // ر reh
	0x0632: {[4]rune{0xFEAF, 0xFEB0, 0xFEAF, 0xFEB0}, false}, // ز zain
	0x0633: {[4]rune{0xFEB1, 0xFEB2, 0xFEB3, 0xFEB4}, true},  // س seen
	0x0634: {[4]rune{0xFEB5, 0xFEB6, 0xFEB7, 0xFEB8}, true},  // ش sheen
	0x0635: {[4]rune{0xFEB9, 0xFEBA, 0xFEBB, 0xFEBC}, true},  // ص sad
	0x0636: {[4]rune{0xFEBD, 0xFEBE, 0xFEBF, 0xFEC0}, true},  // ض dad
	0x0637: {[4]rune{0xFEC1, 0xFEC2, 0xFEC3, 0xFEC4}, true},  // ط tah
	0x0638: {[4]rune{0xFEC5, 0xFEC6, 0xFEC7, 0xFEC8}, true},  // ظ zah
	0x0639: {[4]rune{0xFEC9, 0xFECA, 0xFECB, 0xFECC}, true},  // ع ain
	0x063A: {[4]rune{0xFECD, 0xFECE, 0xFECF, 0xFED0}, true},  // غ ghain
	0x0640: {[4]rune{0x0640, 0x0640, 0x0640, 0x0640}, true},  // ـ tatweel
	0x0641: {[4]rune{0xFED1, 0xFED2, 0xFED3, 0xFED4}, true},  // ف feh
	0x0642: {[4]rune{0xFED5, 0xFED6, 0xFED7, 0xFED8}, true},  // ق qaf
	0x0643: {[4]rune{0xFED9, 0xFEDA, 0xFEDB, 0xFEDC}, true},  // ك kaf
	0x0644: {[4]rune{0xFEDD, 0xFEDE, 0xFEDF, 0xFEE0}, true},  // ل lam
	0x0645: {[4]rune{0xFEE1, 0xFEE2, 0xFEE3, 0xFEE4}, true},  // م meem
	0x0646: {[4]rune{0xFEE5, 0xFEE6, 0xFEE7, 0xFEE8}, true},  // ن noon
	0x0647: {[4]rune{0xFEE9, 0xFEEA, 0xFEEB, 0xFEEC}, true},  // ه heh
	0x0648: {[4]rune{0xFEED, 0xFEEE, 0xFEED, 0xFEEE}, false}, // و waw
	0x0649: {[4]rune{0xFEEF, 0xFEF0, 0xFEEF, 0xFEF0}, false}, // ى alef maksura
	0x064A: {[4]rune{0xFEF1, 0xFEF2, 0xFEF3, 0xFEF4}, true},  // ي yeh
	0x0671: {[4]rune{0xFB50, 0xFB51, 0xFB50, 0xFB51}, false}, // ٱ alef wasla
	0x067E: {[4]rune{0xFB56, 0xFB57, 0xFB58, 0xFB59}, true},  // پ peh
	0x0686: {[4]rune{0xFB7A, 0xFB7B, 0xFB7C, 0xFB7D}, true},  // چ tcheh
	0x0698: {[4]rune{0xFB8A, 0xFB8B, 0xFB8A, 0xFB8B}, false}, // ژ jeh
	0x06A4: {[4]rune{0xFB6A, 0xFB6B, 0xFB6C, 0xFB6D}, true},  // ڤ veh
	0x06AF: {[4]rune{0xFB92, 0xFB93, 0xFB94, 0xFB95}, true},  // گ gaf
	0x06CC: {[4]rune{0xFBFC, 0xFBFD, 0xFBFE, 0xFBFF}, true},  // ی farsi yeh
}

// lamAlef maps the alef following a lam to the single ligature the two form.
//
// Not an optional flourish: لا written as two glyphs is wrong in Arabic the way
// "TH" for "þ" is wrong, and every Arabic font draws it as one. The ligature has
// only two forms, because the alef never joins what follows it.
var lamAlef = map[rune][2]rune{
	0x0622: {0xFEF5, 0xFEF6}, // لآ
	0x0623: {0xFEF7, 0xFEF8}, // لأ
	0x0625: {0xFEF9, 0xFEFA}, // لإ
	0x0627: {0xFEFB, 0xFEFC}, // لا
}

// Shape converts logical text into the glyph sequence a PDF viewer draws
// correctly: Arabic letters in their positional forms, Arabic runs reversed,
// embedded numbers and Latin left as they are.
//
// Safe on text with no Arabic in it, which is why callers pass everything
// through it rather than deciding per string — a caller that has to choose is a
// caller that will eventually choose wrong for one label.
func Shape(text string) string {
	if text == "" || !containsArabic(text) {
		return text
	}
	// The explicit bidi marks are removed rather than honoured: they exist to
	// steer an engine this does not have, and drawn as glyphs they would print
	// as empty boxes.
	cleaned := strings.NewReplacer("\u200e", "", "\u200f", "").Replace(text)
	return reorder(join(cleaned))
}

// join replaces each Arabic letter with the presentation form its neighbours
// call for, and fuses lam-alef pairs.
func join(text string) []rune {
	in := []rune(text)
	out := make([]rune, 0, len(in))

	for i := 0; i < len(in); i++ {
		current, isArabic := shapes[in[i]]
		if !isArabic {
			out = append(out, in[i])
			continue
		}

		// Diacritics sit above or below the letter they belong to and take no
		// part in joining. They are passed through untouched and skipped when
		// looking for a neighbour, or every vowelled word would break its
		// connections.
		prev, hasPrev := previousLetter(in, i)
		_, hasNext := nextLetter(in, i)

		// A lam followed by an alef is one glyph, not two. Consumed here, in
		// the lam's own iteration, so the alef is not emitted again.
		if in[i] == 0x0644 && hasNext {
			if pair, ok := lamAlef[in[nextIndex(in, i)]]; ok {
				shape := pair[0]
				if hasPrev && prev.linksNext {
					shape = pair[1]
				}
				out = append(out, shape)
				i = nextIndex(in, i)
				continue
			}
		}

		linkedBefore := hasPrev && prev.linksNext
		linkedAfter := hasNext && current.linksNext

		switch {
		case linkedBefore && linkedAfter:
			out = append(out, current.forms[medial])
		case linkedBefore:
			out = append(out, current.forms[final])
		case linkedAfter:
			out = append(out, current.forms[initial])
		default:
			out = append(out, current.forms[isolated])
		}
	}
	return out
}

// reorder reverses the right-to-left runs and leaves the left-to-right ones
// alone.
//
// The whole string is reversed first, which puts the Arabic the right way
// round, and then each run of digits or Latin is reversed back — so
// "استلمنا 500,000 دينار" keeps its amount readable instead of printing
// "000,005". Punctuation between two Arabic words travels with the Arabic;
// punctuation inside a number stays with the number, which is why a dot or a
// comma counts as neutral here rather than as a break.
func reorder(in []rune) string {
	reversed := make([]rune, len(in))
	for i, r := range in {
		reversed[len(in)-1-i] = r
	}

	var out []rune
	for i := 0; i < len(reversed); {
		if !isLeftToRight(reversed[i]) {
			out = append(out, mirror(reversed[i]))
			i++
			continue
		}
		// A left-to-right run, plus the neutral characters inside it. Trailing
		// neutrals are left out, so a full stop ending an Arabic sentence is
		// not dragged into the number before it.
		end := i
		lastStrong := i
		for end < len(reversed) && (isLeftToRight(reversed[end]) || isNeutral(reversed[end])) {
			if isLeftToRight(reversed[end]) {
				lastStrong = end
			}
			end++
		}
		run := reversed[i : lastStrong+1]
		for j := len(run) - 1; j >= 0; j-- {
			out = append(out, run[j])
		}
		i = lastStrong + 1
	}
	return string(out)
}

// mirror flips the brackets that face the other way in right-to-left text.
//
// An opening parenthesis in Arabic points the opposite way to one in English.
// After the reversal above it is in the right position and the wrong shape, and
// a receipt with backwards brackets looks broken to a reader who cannot say why.
func mirror(r rune) rune {
	switch r {
	case '(':
		return ')'
	case ')':
		return '('
	case '[':
		return ']'
	case ']':
		return '['
	case '{':
		return '}'
	case '}':
		return '{'
	case '<':
		return '>'
	case '>':
		return '<'
	}
	return r
}

// previousLetter finds the letter before position i, skipping diacritics.
func previousLetter(in []rune, i int) (letter, bool) {
	for j := i - 1; j >= 0; j-- {
		if isDiacritic(in[j]) {
			continue
		}
		found, ok := shapes[in[j]]
		return found, ok
	}
	return letter{}, false
}

// nextLetter finds the letter after position i, skipping diacritics.
func nextLetter(in []rune, i int) (letter, bool) {
	j := nextIndex(in, i)
	if j < 0 {
		return letter{}, false
	}
	found, ok := shapes[in[j]]
	return found, ok
}

// nextIndex is the position of the next non-diacritic rune, or -1.
func nextIndex(in []rune, i int) int {
	for j := i + 1; j < len(in); j++ {
		if isDiacritic(in[j]) {
			continue
		}
		return j
	}
	return -1
}

// isDiacritic reports whether a rune is a mark that sits on a letter rather
// than beside it: tashkeel, the superscript alef, and the Quranic marks.
func isDiacritic(r rune) bool {
	return (r >= 0x064B && r <= 0x065F) || r == 0x0670 || (r >= 0x06D6 && r <= 0x06ED)
}

// isLeftToRight reports whether a rune belongs to a run that must keep its
// original order: digits, Latin letters, and the signs that travel with them.
//
// Arabic-Indic digits count. They are written right-to-left as *glyphs* but
// left-to-right as a *number* — ٢٠٢٦ is two thousand and twenty-six, not six
// thousand two hundred and two — so reversing them with the surrounding Arabic
// turns ٠١٢٣ into ٣٢١٠. This system prints Western digits by policy, but a
// student's name or an office's note can carry either.
func isLeftToRight(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r >= 0x0660 && r <= 0x0669: // Arabic-Indic ٠..٩
		return true
	case r >= 0x06F0 && r <= 0x06F9: // Extended Arabic-Indic, used in Persian
		return true
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		return true
	case r == '%', r == '$', r == '@', r == '#':
		return true
	}
	return false
}

// isNeutral reports whether a rune takes its direction from its neighbours —
// the separators inside a number, and the space between a number and its unit.
func isNeutral(r rune) bool {
	switch r {
	case ' ', '.', ',', ':', '-', '/', '+', '_', '\'':
		return true
	}
	return false
}

// containsArabic reports whether shaping has anything to do.
func containsArabic(text string) bool {
	for _, r := range text {
		if (r >= 0x0600 && r <= 0x06FF) || (r >= 0xFB50 && r <= 0xFEFF) {
			return true
		}
	}
	return false
}
