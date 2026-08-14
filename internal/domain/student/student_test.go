package student_test

import (
	"testing"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
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
