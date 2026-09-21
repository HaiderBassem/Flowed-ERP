package student_test

import (
	"testing"

	"flowed/internal/domain/shared"
	"flowed/internal/domain/student"
)

func newStudent(t *testing.T, no, name, mother string) *student.Student {
	t.Helper()
	s, err := student.New(student.NewParams{StudentNo: no, FullName: name, MotherName: mother})
	if err != nil {
		t.Fatalf("building student: %v", err)
	}
	return s
}

// A merged record is kept, not deleted. Every receipt, ministry return and
// audit entry that names the old student number must still resolve to a person
// years later; what the tombstone stops is the record being used again.
func TestMergeIntoLeavesATombstone(t *testing.T) {
	source := newStudent(t, "2024001", "محمد علي حسن", "زينب")
	target := newStudent(t, "2024002", "محمد علي حسن", "زينب")

	if err := source.MergeInto(target.ID); err != nil {
		t.Fatalf("merging: %v", err)
	}
	if source.Status != student.StatusMerged {
		t.Errorf("status = %q, want merged", source.Status)
	}
	if source.MergedIntoID == nil || *source.MergedIntoID != target.ID {
		t.Error("a merged record must point at where the person went")
	}
	if source.StudentNo != "2024001" {
		t.Error("the student number must survive the merge; documents reference it")
	}
}

func TestMergeIntoRefusesASecondMerge(t *testing.T) {
	source := newStudent(t, "2024001", "محمد", "زينب")
	first := newStudent(t, "2024002", "محمد", "زينب")
	second := newStudent(t, "2024003", "محمد", "زينب")

	if err := source.MergeInto(first.ID); err != nil {
		t.Fatal(err)
	}
	// Two targets both claiming the same history is worse than the duplicate
	// the merge was fixing.
	if err := source.MergeInto(second.ID); err == nil {
		t.Fatal("merging an already-merged record must be refused")
	}
}

func TestMergeIntoRefusesItself(t *testing.T) {
	source := newStudent(t, "2024001", "محمد", "زينب")
	if err := source.MergeInto(source.ID); err == nil {
		t.Fatal("a record must not merge into itself")
	}
	if err := source.MergeInto(shared.NilID); err == nil {
		t.Fatal("a record must not merge into nothing")
	}
}

// The folding decides whether two records look like one person, so it has to
// treat the orthographic variation two clerks produce as identical — which is
// exactly how the duplicate arose in the first place.
func TestFoldArabicMatchesTheDatabaseRules(t *testing.T) {
	cases := []struct{ a, b string }{
		{"أحمد", "احمد"},        // hamza above alef
		{"إبراهيم", "ابراهيم"},  // hamza below alef
		{"آمنة", "امنه"},        // madda, and ta marbuta to ha
		{"فاطمة", "فاطمه"},      // ta marbuta
		{"مصطفى", "مصطفي"},      // alef maqsura to ya
		{"مُحَمَّد", "محمد"},    // tashkeel
		{"محـــمد", "محمد"},     // tatweel
		{"علي  حسن", "علي حسن"}, // collapsed spaces
		{" حسين ", "حسين"},      // trimmed
		{"مؤمن", "مومن"},        // hamza on waw
		{"رئيس", "رييس"},        // hamza on ya
	}

	for _, tc := range cases {
		if got, want := student.FoldArabic(tc.a), student.FoldArabic(tc.b); got != want {
			t.Errorf("FoldArabic(%q) = %q, FoldArabic(%q) = %q — these must fold alike",
				tc.a, got, tc.b, want)
		}
	}

	// Different names must stay different: a folding that collapses everything
	// would merge two people.
	if student.FoldArabic("محمد") == student.FoldArabic("أحمد") {
		t.Error("two different names must not fold to the same value")
	}
}

// Every shape a clerk might type for the same Iraqi mobile number must
// canonicalise to the same stored value — otherwise the same subscriber ends
// up on file under several different-looking strings.
func TestNormalizeIraqiPhoneAcceptsEveryShapeOfTheSameNumber(t *testing.T) {
	const want = "07701234567"
	shapes := []string{
		"07701234567",
		"+964 770 123 4567",
		"00964770123 4567",
		"9647701234567",
		"0770-123-4567",
		"٠٧٧٠١٢٣٤٥٦٧", // Arabic-Indic digits
	}

	for _, raw := range shapes {
		raw := raw
		got, err := student.NormalizeIraqiPhone(&raw)
		if err != nil {
			t.Fatalf("NormalizeIraqiPhone(%q): %v", raw, err)
		}
		if got == nil || *got != want {
			t.Errorf("NormalizeIraqiPhone(%q) = %v, want %q", raw, got, want)
		}
	}
}

func TestNormalizeIraqiPhoneRefusesWhatIsNotAnIraqiMobileNumber(t *testing.T) {
	cases := []string{
		"12345",           // too short
		"07701234567890",  // too many digits
		"01234567890",     // not a mobile prefix (7xx)
		"+1 555 123 4567", // a foreign number
	}
	for _, raw := range cases {
		raw := raw
		if _, err := student.NormalizeIraqiPhone(&raw); err == nil {
			t.Errorf("NormalizeIraqiPhone(%q) should have been refused", raw)
		}
	}
}

func TestNormalizeIraqiPhoneTreatsBlankAsAbsent(t *testing.T) {
	blank := "   "
	got, err := student.NormalizeIraqiPhone(&blank)
	if err != nil {
		t.Fatalf("blank phone should not error: %v", err)
	}
	if got != nil {
		t.Errorf("blank phone should clear the field, got %q", *got)
	}
	if got, err := student.NormalizeIraqiPhone(nil); err != nil || got != nil {
		t.Errorf("nil phone should pass through as nil, got (%v, %v)", got, err)
	}
}
