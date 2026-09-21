package arabic_test

import (
	"strings"
	"testing"

	"flowed/internal/adapter/pdf/arabic"
)

// The shaper is the difference between a receipt a student can read and one
// that prints four disconnected letters backwards. There is no visual test —
// nobody eyeballs a PDF on every build — so these assert glyph codepoints.

func TestLettersTakeTheFormTheirNeighboursCallFor(t *testing.T) {
	// بحر: beh starts, hah is medial, reh ends.
	got := []rune(arabic.Shape("بحر"))

	// Reversed for drawing, so the final reh comes out first.
	want := []rune{0xFEAE, 0xFEA4, 0xFE91}
	if string(got) != string(want) {
		t.Errorf("بحر shaped as %U, want %U", got, want)
	}
}

func TestANonJoiningLetterBreaksTheRun(t *testing.T) {
	// بريد: reh does not join what follows, so the yeh after it must be
	// initial and not medial. Getting this wrong produces بـرـيـد.
	got := []rune(arabic.Shape("بريد"))

	// Drawing order: dal(final) yeh(initial) reh(final) beh(initial)
	want := []rune{0xFEAA, 0xFEF3, 0xFEAE, 0xFE91}
	if string(got) != string(want) {
		t.Errorf("بريد shaped as %U, want %U", got, want)
	}
}

func TestLamAlefBecomesOneLigature(t *testing.T) {
	// لا is one glyph in every Arabic font. Two glyphs is as wrong as writing
	// "TH" where a text wants "þ".
	got := []rune(arabic.Shape("لا"))
	if len(got) != 1 {
		t.Fatalf("لا produced %d glyphs, want the single ligature: %U", len(got), got)
	}
	if got[0] != 0xFEFB {
		t.Errorf("لا shaped as %U, want U+FEFB", got[0])
	}

	// After a joining letter the ligature takes its final form.
	connected := []rune(arabic.Shape("بلا"))
	if connected[0] != 0xFEFC {
		t.Errorf("بلا: ligature shaped as %U, want the final form U+FEFC", connected[0])
	}
}

// A receipt is Arabic prose with a number in the middle of it. Reversing the
// number along with the words prints 500,000 as 000,005 — the one error on a
// receipt that costs money rather than credibility.
func TestEmbeddedNumbersKeepTheirOrder(t *testing.T) {
	got := arabic.Shape("المبلغ 500,000 دينار")

	if !strings.Contains(got, "500,000") {
		t.Errorf("the amount did not survive shaping: %q", got)
	}
	if strings.Contains(got, "000,005") {
		t.Error("the amount was reversed with the Arabic — the receipt would state the wrong sum")
	}
}

func TestLatinIdentifiersKeepTheirOrder(t *testing.T) {
	// A receipt number gets read out over a telephone. Reversed, it is a
	// different receipt.
	got := arabic.Shape("وصل رقم R-2025-2026-000008")

	if !strings.Contains(got, "R-2025-2026-000008") {
		t.Errorf("the receipt number did not survive shaping: %q", got)
	}
}

func TestDiacriticsDoNotBreakJoining(t *testing.T) {
	// A fatha between two letters must not stop them connecting: the bare word
	// and the vowelled word have to produce the same letter forms.
	bare := []rune(arabic.Shape("بحر"))
	vowelled := []rune(arabic.Shape("بَحْر"))

	var letters []rune
	for _, r := range string(vowelled) {
		if (r >= 0x064B && r <= 0x065F) || r == 0x0670 {
			continue
		}
		letters = append(letters, r)
	}
	if string(letters) != string(bare) {
		t.Errorf("vowelled %U produced different letter forms from bare %U", letters, bare)
	}
}

func TestTextWithNoArabicIsUntouched(t *testing.T) {
	// Every caller pipes everything through Shape rather than deciding per
	// string, so it has to be a no-op on Latin — otherwise a column of receipt
	// numbers comes out reversed.
	for _, input := range []string{"", "R-2025-000008", "Total: 1,250,000", "2026-09-21"} {
		if got := arabic.Shape(input); got != input {
			t.Errorf("Shape(%q) = %q, want it unchanged", input, got)
		}
	}
}

func TestBracketsAreMirrored(t *testing.T) {
	// After the reversal an opening bracket sits in the right place facing the
	// wrong way. A reader cannot say why the page looks broken, only that it
	// does.
	got := arabic.Shape("الطالب (مستضاف)")

	opening := strings.Index(got, "(")
	closing := strings.Index(got, ")")
	if opening < 0 || closing < 0 {
		t.Fatalf("brackets vanished: %q", got)
	}
	if opening > closing {
		t.Errorf("brackets are backwards in the drawing order: %q", got)
	}
}

// Every glyph the shaper emits has to exist in the embedded font, or the viewer
// draws an empty box. This asserts the shaper only ever produces codepoints
// from the blocks the font was verified against.
func TestEveryEmittedGlyphIsInAPresentationBlock(t *testing.T) {
	samples := []string{
		"جامعة بغداد", "كلية الهندسة", "قسم الحاسوب",
		"استضافة من المسائي إلى الصباحي", "وصل قبض", "إيصال استرجاع",
		"المبلغ المستلم", "التوقيع", "أجور الدراسة", "لا يوجد",
	}

	for _, sample := range samples {
		for _, r := range arabic.Shape(sample) {
			switch {
			case r < 0x0600: // Latin, digits, punctuation, space
			case r >= 0xFB50 && r <= 0xFEFF: // presentation forms
			case r >= 0x064B && r <= 0x0670: // diacritics, drawn as marks
			case r == 0x0640: // tatweel has no separate presentation form
			default:
				t.Errorf("%q emitted U+%04X, outside the presentation blocks the font was "+
					"verified for — it will draw as an empty box", sample, r)
			}
		}
	}
}
