package money_test

import (
	"strings"
	"testing"

	"github.com/swibit/flowed/internal/domain/money"
)

// The written amount is what makes a receipt hard to alter, so it has to be
// right in the ways an Iraqi accountant would check: the dual forms, the
// three-to-ten plural, and the shift back to the singular at eleven.
func TestSpellArabicPlain(t *testing.T) {
	cases := []struct {
		amount money.Amount
		want   string
	}{
		{0, "صفر"},
		{1, "واحد"},
		{2, "اثنان"},
		{3, "ثلاثة"},
		{10, "عشرة"},
		{11, "أحد عشر"},
		{12, "اثنا عشر"},
		{19, "تسعة عشر"},
		{20, "عشرون"},
		// The unit is spoken before the ten.
		{21, "واحد وعشرون"},
		{99, "تسعة وتسعون"},
		{100, "مائة"},
		{101, "مائة وواحد"},
		{200, "مائتان"},
		// Not "ثلاثة مائة": the hundreds are single words.
		{300, "ثلاثمائة"},
		{999, "تسعمائة وتسعة وتسعون"},

		// One thousand is ألف, never واحد ألف.
		{1_000, "ألف"},
		{2_000, "ألفان"},
		{3_000, "ثلاثة آلاف"},
		{10_000, "عشرة آلاف"},
		// At eleven the noun returns to the singular accusative.
		{11_000, "أحد عشر ألفاً"},
		{20_000, "عشرون ألفاً"},
		{100_000, "مائة ألف"},
		{500_000, "خمسمائة ألف"},

		{1_000_000, "مليون"},
		{2_000_000, "مليونان"},
		{3_000_000, "ثلاثة ملايين"},
		{11_000_000, "أحد عشر مليوناً"},

		// The amounts a real receipt carries.
		{1_500_000, "مليون وخمسمائة ألف"},
		{2_750_000, "مليونان وسبعمائة وخمسون ألفاً"},
		{320_000, "ثلاثمائة وعشرون ألفاً"},
		{1_600_000, "مليون وستمائة ألف"},
		{640_000, "ستمائة وأربعون ألفاً"},
		{75_000, "خمسة وسبعون ألفاً"},
		{25_000, "خمسة وعشرون ألفاً"},

		// A group of zeroes in the middle must be skipped, not spoken.
		{1_000_500, "مليون وخمسمائة"},
		{2_000_001, "مليونان وواحد"},

		{1_000_000_000, "مليار"},
		{2_000_000_000, "ملياران"},

		// A dual drops its nūn when it governs what follows: 200,000 is
		// مائتا ألف, and writing مائتان ألف reads as an error at a glance.
		{200_000, "مائتا ألف"},
		{200_000_000, "مائتا مليون"},
		{2_200_000, "مليونان ومائتا ألف"},
	}

	for _, tc := range cases {
		if got := money.SpellArabicPlain(tc.amount); got != tc.want {
			t.Errorf("SpellArabicPlain(%s) = %q, want %q", tc.amount, got, tc.want)
		}
	}
}

// The currency word agrees with the count too, and the agreement follows the
// last three digits rather than the whole number.
func TestSpellArabicCurrencyAgreement(t *testing.T) {
	cases := []struct {
		amount money.Amount
		want   string
	}{
		{1, "دينار عراقي لا\u00a0غير"},
		{2, "ديناران عراقيان لا\u00a0غير"},
		{3, "ثلاثة دنانير عراقية لا\u00a0غير"},
		{10, "عشرة دنانير عراقية لا\u00a0غير"},
		{11, "أحد عشر ديناراً عراقياً لا\u00a0غير"},
		{100, "مائة دينار عراقي لا\u00a0غير"},
		// A round thousand ends in three zeroes, so the noun is singular.
		{1_000, "ألف دينار عراقي لا\u00a0غير"},
		{1_500_000, "مليون وخمسمائة ألف دينار عراقي لا\u00a0غير"},
		// The scale word governs the currency, so it sheds its tanween:
		// عشرون ألف دينار, not عشرون ألفاً دينار.
		{320_000, "ثلاثمائة وعشرون ألف دينار عراقي لا\u00a0غير"},
		{200_000, "مائتا ألف دينار عراقي لا\u00a0غير"},
	}

	for _, tc := range cases {
		if got := money.SpellArabic(tc.amount); got != tc.want {
			t.Errorf("SpellArabic(%s) = %q, want %q", tc.amount, got, tc.want)
		}
	}
}

// The closing marker is not decoration: it terminates the written amount so
// nothing can be appended to it after printing.
//
// It is joined with a non-breaking space on purpose. A thermal roll is 42
// characters wide and wraps on spaces, and "لا" left dangling at the end of one
// line with "غير" alone on the next stops reading as one phrase — which defeats
// the only job the marker has.
const closingMarker = "لا\u00a0غير"

func TestSpellArabicClosesTheAmount(t *testing.T) {
	for _, amount := range []money.Amount{1, 1_000, 1_500_000, 987_654_321} {
		got := money.SpellArabic(amount)
		if !strings.HasSuffix(got, closingMarker) {
			t.Errorf("SpellArabic(%s) = %q, which does not close the amount", amount, got)
		}
		if strings.HasSuffix(got, "لا غير") {
			t.Errorf("SpellArabic(%s) closes with a breaking space, so the marker can wrap", amount)
		}
	}
	if got := money.SpellArabic(0); strings.Contains(got, "غير") {
		t.Errorf("a zero amount should not be closed as if it were a payment: %q", got)
	}
}

// Every amount the system can hold must produce words. A receipt that renders
// an empty line where the amount belongs is worse than one that renders an
// awkward phrase.
func TestSpellArabicNeverReturnsEmpty(t *testing.T) {
	amounts := []money.Amount{
		1, 7, 13, 40, 99, 100, 999, 1_000, 1_001, 9_999, 10_000, 99_999,
		100_000, 999_999, 1_000_000, 12_345_678, 999_999_999,
		1_000_000_000, 123_456_789_012,
	}
	for _, amount := range amounts {
		got := money.SpellArabic(amount)
		if strings.TrimSpace(got) == "" {
			t.Errorf("SpellArabic(%s) produced nothing", amount)
		}
		if strings.Contains(got, "  ") {
			t.Errorf("SpellArabic(%s) = %q has a doubled space", amount, got)
		}
		if strings.Contains(got, " و ") {
			t.Errorf("SpellArabic(%s) = %q separates the conjunction from its word", amount, got)
		}
	}
}

// Two different amounts must never spell the same, or the written form stops
// being a check on the figures.
func TestSpellArabicIsDistinctPerAmount(t *testing.T) {
	seen := make(map[string]money.Amount)
	for amount := money.Amount(1); amount <= 2000; amount++ {
		words := money.SpellArabicPlain(amount)
		if previous, clash := seen[words]; clash {
			t.Fatalf("%s and %s both spell as %q", previous, amount, words)
		}
		seen[words] = amount
	}
	// And across the scales, where a collision would be most damaging.
	for _, amount := range []money.Amount{
		500_000, 5_000_000, 50_000, 1_500_000, 15_000_000,
	} {
		words := money.SpellArabicPlain(amount)
		if previous, clash := seen[words]; clash {
			t.Fatalf("%s and %s both spell as %q", previous, amount, words)
		}
		seen[words] = amount
	}
}

func TestSpellArabicHandlesNegatives(t *testing.T) {
	// Not expected on a receipt, but a refund register or an adjustment line
	// can carry one, and silently dropping the sign would invert the meaning.
	got := money.SpellArabicPlain(-500_000)
	if !strings.HasPrefix(got, "سالب") {
		t.Errorf("SpellArabicPlain(-500,000) = %q, which does not mark the sign", got)
	}
}

func TestWesternDigitsOnReceipts(t *testing.T) {
	// Arabic-Indic ٠ and Western 0 are easy to confuse on a poorly printed
	// slip, and a figure that can be misread is a figure that can be disputed.
	got := money.FormatWesternDigits(1_500_000)
	if got != "1,500,000" {
		t.Errorf("FormatWesternDigits = %q, want 1,500,000", got)
	}
	for _, r := range got {
		if r >= '٠' && r <= '٩' {
			t.Errorf("receipt figure %q contains Arabic-Indic digits", got)
			break
		}
	}
}
