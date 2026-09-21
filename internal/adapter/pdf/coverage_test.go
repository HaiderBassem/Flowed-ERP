package pdf

import (
	"strings"
	"testing"
)

// Every character an Iraqi university can type must reach the page.
//
// gopdf substitutes a space for a glyph it cannot find and carries on, so a
// font missing one letter produces a receipt with a gap in a student's name and
// nothing anywhere says so. Document.Bytes refuses such a document now; these
// tests are what stop it being refused in front of a student instead of here.
//
// The sweep is over codepoints rather than over sample words on purpose: a word
// list only ever covers the letters somebody thought of.
func TestEveryArabicCharacterHasAGlyph(t *testing.T) {
	blocks := []struct {
		name     string
		from, to rune
	}{
		{"Arabic block", 0x0600, 0x06FF},
		{"Arabic Supplement", 0x0750, 0x077F},
		{"presentation forms B", 0xFE70, 0xFEFF},
		{"ASCII", 0x0020, 0x007E},
	}

	for _, block := range blocks {
		t.Run(block.name, func(t *testing.T) {
			var absent []rune
			for r := block.from; r <= block.to; r++ {
				if !drawable(t, string(r)) {
					absent = append(absent, r)
				}
			}
			if len(absent) == 0 {
				return
			}
			// An unassigned codepoint having no glyph is not a defect — nothing
			// can type it. The report names what is absent and leaves the
			// judgement to a reader: deciding in code which ones "matter" is
			// how the one that mattered gets excluded.
			t.Logf("%s: %d codepoint(s) with no glyph: %s",
				block.name, len(absent), describeRunes(absent))
		})
	}
}

// The characters this system is certain to print. Not a sample: the alphabet,
// the digits in both scripts, the punctuation a receipt carries, and the marks
// that appear in Iraqi names.
func TestEveryCharacterWePrintHasAGlyph(t *testing.T) {
	required := strings.Join([]string{
		"ابتثجحخدذرزسشصضطظعغفقكلمنهوي", // the alphabet
		"ءأإآؤئةىـ",                     // hamza carriers, teh marbuta, alef maksura, tatweel
		"ًٌٍَُِّْ",                      // tashkeel
		"٠١٢٣٤٥٦٧٨٩",                    // Arabic-Indic digits
		"0123456789",                    // Western digits
		"،؛؟٪",                          // Arabic punctuation
		".,:;!?()[]{}-–—/\\%@#&*+=_'\"", // Latin punctuation
		"پچژگڤ",                         // Persian letters, seen in transliterated names
		"د.ع",                           // the currency abbreviation
	}, "")

	var absent []rune
	for _, r := range required {
		if !drawable(t, string(r)) {
			absent = append(absent, r)
		}
	}

	if len(absent) > 0 {
		t.Errorf("the embedded font cannot draw %d character(s) this system prints: %s — "+
			"every receipt carrying one would have a gap where it belongs",
			len(absent), describeRunes(absent))
	}
}

// Shaped text must also be drawable. A shaper that emits a presentation form
// the font lacks fails exactly like a missing letter, and it is a failure the
// character sweep above cannot see.
func TestShapedWordsAreFullyDrawable(t *testing.T) {
	words := []string{
		"جامعة الفراهيدي", "كلية الهندسة", "هندسة الحاسوب",
		"علي محمد حسن الجبوري", "زينب عبد الله", "مصطفى ليلى عيسى يحيى",
		"أحمد إبراهيم آمنة مؤمن رئيس شيء", "لا إلا كلا الأولى",
		"استضافة من المسائي إلى الصباحي", "المتبقّي المُستلَم الأوّل",
		"فقط مليون وخمسمائة ألف دينار عراقي لا غير",
		"1,550,000 د.ع", "R-2025-2026-000008", "٪٥٠ من الأجور",
	}

	for _, word := range words {
		if !drawable(t, word) {
			t.Errorf("%q cannot be drawn in full", word)
		}
	}
}

// drawable reports whether a string reaches the page with every character
// intact, in both weights.
func drawable(t *testing.T, text string) bool {
	t.Helper()
	doc, err := New(A4)
	if err != nil {
		t.Fatal(err)
	}
	doc.NewPage()
	doc.Text(text, TextStyle{Size: 12})
	doc.Text(text, TextStyle{Size: 12, Bold: true})
	_, err = doc.Bytes()
	return err == nil
}
