package money

import "strings"

// Arabic number spelling — التفقيط.
//
// Every Iraqi financial document carries the amount twice: once in figures and
// once written out. The written form is what makes a receipt hard to alter,
// because changing 500,000 to 5,000,000 in figures is a stroke of a pen while
// changing "خمسمائة ألف" to "خمسة ملايين" is not. A receipt without it is not
// a document a finance office will accept.
//
// Arabic makes this harder than English. A counted noun agrees with its number
// in a way that changes the noun's form three times as the count grows: two
// thousand is a dual (ألفان), three to ten thousand takes a broken plural
// (آلاف), and eleven upwards reverts to the singular in the accusative
// (ألفاً). Getting that wrong is the sort of thing an accountant notices
// immediately, so the rules are spelled out below rather than approximated.

var arabicOnes = [...]string{
	"", "واحد", "اثنان", "ثلاثة", "أربعة", "خمسة",
	"ستة", "سبعة", "ثمانية", "تسعة",
}

// Ten through nineteen are irregular enough to list rather than compose.
var arabicTeens = [...]string{
	"عشرة", "أحد عشر", "اثنا عشر", "ثلاثة عشر", "أربعة عشر",
	"خمسة عشر", "ستة عشر", "سبعة عشر", "ثمانية عشر", "تسعة عشر",
}

var arabicTens = [...]string{
	"", "", "عشرون", "ثلاثون", "أربعون", "خمسون",
	"ستون", "سبعون", "ثمانون", "تسعون",
}

// Hundreds are written as single words, and several are not the concatenation
// a reader might expect: three hundred is ثلاثمائة, not ثلاثة مائة.
var arabicHundreds = [...]string{
	"", "مائة", "مائتان", "ثلاثمائة", "أربعمائة", "خمسمائة",
	"ستمائة", "سبعمائة", "ثمانمائة", "تسعمائة",
}

// scale is one power-of-thousand step with the forms its noun takes.
type scale struct {
	// singular is used for a count of one, where the count itself is not
	// spoken: one thousand is ألف, never واحد ألف.
	singular string
	// dual is used for exactly two, again without speaking the count.
	dual string
	// dualConstruct is the dual with its nūn dropped, used when the dual is
	// itself followed by what it counts (المثنى المضاف): two thousand dinars
	// is ألفا دينار, not ألفان دينار.
	dualConstruct string
	// plural is used for three through ten.
	plural string
	// accusative is used from eleven upwards, where the noun returns to the
	// singular and takes tanween.
	accusative string
}

var arabicScales = []scale{
	{}, // units: no scale word
	{singular: "ألف", dual: "ألفان", dualConstruct: "ألفا", plural: "آلاف", accusative: "ألفاً"},
	{singular: "مليون", dual: "مليونان", dualConstruct: "مليونا", plural: "ملايين", accusative: "مليوناً"},
	{singular: "مليار", dual: "ملياران", dualConstruct: "مليارا", plural: "مليارات", accusative: "ملياراً"},
	{singular: "ترليون", dual: "ترليونان", dualConstruct: "ترليونا", plural: "ترليونات", accusative: "ترليوناً"},
}

// A word governing the noun that follows it (المضاف) sheds its ending.
//
// Two forms are affected, and both show up on a real receipt. A dual loses its
// final nūn: two hundred standing alone is مائتان, but two hundred thousand is
// مائتا ألف. And a scale word in the accusative loses its tanween: twenty
// thousand alone is عشرون ألفاً, but twenty thousand dinars is عشرون ألف
// دينار. Writing either the long way is the kind of thing an Arabic reader
// stumbles over on the first line.
var arabicConstructForms = map[string]string{
	// Duals.
	"مائتان":   "مائتا",
	"ألفان":    "ألفا",
	"مليونان":  "مليونا",
	"ملياران":  "مليارا",
	"ترليونان": "ترليونا",
	"اثنان":    "اثنا",
	// Scale words in the accusative.
	"ألفاً":    "ألف",
	"مليوناً":  "مليون",
	"ملياراً":  "مليار",
	"ترليوناً": "ترليون",
}

// asConstruct puts a trailing governing word into its construct form, leaving
// anything else untouched.
func asConstruct(phrase string) string {
	if phrase == "" {
		return phrase
	}
	cut := strings.LastIndex(phrase, " ")
	head, last := "", phrase
	if cut >= 0 {
		head, last = phrase[:cut+1], phrase[cut+1:]
	}
	if construct, ok := arabicConstructForms[last]; ok {
		return head + construct
	}
	return phrase
}

// asConstructDualOnly applies the dual rule but not the accusative one.
//
// Inside a number, a count like عشرون is followed by its scale word and takes
// the accusative legitimately — عشرون ألفاً is correct standing alone. Only the
// scale word at the very end, which then governs the currency noun, sheds its
// tanween.
func asConstructDualOnly(phrase string) string {
	cut := strings.LastIndex(phrase, " ")
	last := phrase
	if cut >= 0 {
		last = phrase[cut+1:]
	}
	if strings.HasSuffix(last, "اً") {
		return phrase
	}
	return asConstruct(phrase)
}

// SpellArabic writes an amount of dinars in Arabic words, in the form an Iraqi
// receipt carries it.
//
//	SpellArabic(1_500_000) == "مليون وخمسمائة ألف دينار عراقي لا غير"
//
// The closing لا غير ("and no more") is part of the convention: it marks the
// end of the written amount so nothing can be appended to it.
func SpellArabic(a Amount) string {
	if a.IsNegative() {
		return "سالب " + SpellArabic(a.Abs())
	}
	if a.IsZero() {
		return "صفر دينار عراقي"
	}
	// The number governs the currency noun, so a dual at the end of it takes
	// the construct form: 2,000 dinars is ألفا دينار عراقي.
	//
	// The closing marker is joined with a non-breaking space. Split across two
	// lines on a narrow thermal roll it stops reading as one phrase, and the
	// whole point of it is to be an unmistakable end to the amount.
	// One and two are not counted out loud. The singular and the dual of the
	// noun already carry the number — دينار عراقي is one dinar and ديناران
	// عراقيان is two — so saying واحد or اثنان in front of them is the
	// redundancy of a translation rather than Arabic.
	switch a {
	case 1, 2:
		return arabicDinars(uint64(a)) + " لا\u00a0غير"
	}

	spelled := asConstruct(spellArabicNumber(uint64(a)))
	return spelled + " " + arabicDinars(uint64(a)) + " لا\u00a0غير"
}

// SpellArabicPlain writes the number in words without the currency or the
// closing marker, for callers that place it in their own sentence.
func SpellArabicPlain(a Amount) string {
	if a.IsNegative() {
		return "سالب " + SpellArabicPlain(a.Abs())
	}
	if a.IsZero() {
		return "صفر"
	}
	return spellArabicNumber(uint64(a))
}

// arabicDinars picks the form of "dinar" that agrees with the count.
//
// The agreement follows the final group of three digits, because that is what
// the noun actually counts. A round thousand or million ends in zeroes, so the
// noun takes the plain singular: one and a half million dinars is
// مليون وخمسمائة ألف دينار عراقي.
func arabicDinars(n uint64) string {
	group := n % 1000
	if group == 0 {
		return "دينار عراقي"
	}
	switch formFor(group) {
	case formSingular:
		return "دينار عراقي"
	case formDual:
		return "ديناران عراقيان"
	case formPlural:
		return "دنانير عراقية"
	default:
		return "ديناراً عراقياً"
	}
}

// spellArabicNumber writes a whole number in words.
func spellArabicNumber(n uint64) string {
	if n == 0 {
		return "صفر"
	}

	// Split into groups of three, least significant first, so that each group
	// can be paired with its scale word.
	var groups []uint64
	for n > 0 {
		groups = append(groups, n%1000)
		n /= 1000
	}
	if len(groups) > len(arabicScales) {
		// Beyond a trillion the scale words run out. No tuition reaches here,
		// and silently producing a wrong word on a financial document would be
		// worse than admitting the gap.
		return "مبلغ يتجاوز الحد القابل للكتابة"
	}

	// Spell from the largest group down.
	parts := make([]string, 0, len(groups))
	for i := len(groups) - 1; i >= 0; i-- {
		group := groups[i]
		if group == 0 {
			continue
		}
		parts = append(parts, spellArabicGroup(group, i))
	}

	return strings.Join(parts, " و")
}

// countedForm names which form of a counted noun a number takes.
type countedForm int

const (
	formSingular countedForm = iota
	formDual
	formPlural
	formAccusative
)

// formFor picks the form of the counted noun (التمييز) for a count.
//
// The rule turns on the LAST TWO DIGITS, not on the size of the number, which
// is the part that is easy to get wrong. Eleven through ninety-nine take the
// singular accusative — أحد عشر ألفاً — but a round hundred goes back to the
// plain singular: five hundred thousand is خمسمائة ألف, never خمسمائة ألفاً.
// An implementation that simply tests "eleven or more" produces the second,
// and an accountant reads it as a mistake on the first pass.
func formFor(count uint64) countedForm {
	switch {
	case count == 1:
		return formSingular
	case count == 2:
		return formDual
	}

	switch lastTwo := count % 100; {
	case lastTwo == 0:
		// A round hundred, or a round thousand within its own group.
		return formSingular
	case lastTwo == 1:
		return formSingular
	case lastTwo == 2:
		return formDual
	case lastTwo <= 10:
		return formPlural
	default:
		return formAccusative
	}
}

// spellArabicGroup writes one group of three digits together with its scale
// word, applying the agreement the count demands.
func spellArabicGroup(group uint64, scaleIndex int) string {
	if scaleIndex == 0 {
		return spellArabicUnder1000(group)
	}

	s := arabicScales[scaleIndex]

	// One and two do not speak their count: one thousand is ألف, not واحد
	// ألف, and two thousand is ألفان. Saying the number is the mistake an
	// English-speaking implementation makes.
	switch group {
	case 1:
		return s.singular
	case 2:
		return s.dual
	}

	// The count governs the scale word that follows it, so a trailing dual in
	// the count drops its nūn: 200,000 is مائتا ألف, not مائتان ألف.
	spelled := asConstructDualOnly(spellArabicUnder1000(group))
	switch formFor(group) {
	case formSingular:
		return spelled + " " + s.singular
	case formDual:
		return spelled + " " + s.dual
	case formPlural:
		return spelled + " " + s.plural
	default:
		return spelled + " " + s.accusative
	}
}

// spellArabicUnder1000 writes a number below one thousand.
func spellArabicUnder1000(n uint64) string {
	if n == 0 {
		return ""
	}

	parts := make([]string, 0, 3)

	if hundreds := n / 100; hundreds > 0 {
		parts = append(parts, arabicHundreds[hundreds])
	}

	remainder := n % 100
	switch {
	case remainder == 0:
		// Nothing below the hundreds.
	case remainder < 10:
		parts = append(parts, arabicOnes[remainder])
	case remainder < 20:
		parts = append(parts, arabicTeens[remainder-10])
	default:
		ones := remainder % 10
		tens := remainder / 10
		if ones == 0 {
			parts = append(parts, arabicTens[tens])
		} else {
			// Arabic states the unit before the ten: twenty-one is
			// "one and twenty".
			parts = append(parts, arabicOnes[ones]+" و"+arabicTens[tens])
		}
	}

	return strings.Join(parts, " و")
}

// FormatWesternDigits renders an amount with thousands separators in Western
// digits.
//
// Printed receipts use Western digits even in an otherwise Arabic document.
// Arabic-Indic ٠ and Western 0 are easy to confuse at a glance on a poorly
// printed slip, and a figure that can be misread is a figure that can be
// disputed.
func FormatWesternDigits(a Amount) string { return a.Format() }
