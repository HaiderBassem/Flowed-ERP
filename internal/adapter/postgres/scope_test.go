// Integration tests for organisational scope.
//
// These go at the database on purpose. Scope is enforced by predicates the Go
// code assembles into SQL, and the failure mode that matters — a filter that
// silently matches everything because a clause was dropped — is invisible to a
// unit test of the same code. A scoped finance manager reading another
// college's debt report is a data-protection failure, not a cosmetic one.
package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// scopeFixture builds two colleges with a student enrolled in each, so a scope
// that leaks shows up as the other college's student appearing in a result.
type scopeFixture struct {
	*fixture
	otherCollegeID    shared.ID
	otherDepartmentID shared.ID
	otherStudentID    shared.ID
}

func withScopeFixture(t *testing.T, body func(sf *scopeFixture)) {
	t.Helper()
	withFixture(t, func(f *fixture) {
		suffix := time.Now().UnixNano()
		sf := &scopeFixture{fixture: f}

		sf.otherCollegeID = f.scan(`
			INSERT INTO college (id, code, name_ar) VALUES (gen_random_uuid(), $1, 'كلية أخرى')
			RETURNING id`, fmt.Sprintf("SC%d", suffix%1000000))
		sf.otherDepartmentID = f.scan(`
			INSERT INTO department (id, college_id, code, name_ar, stage_count)
			VALUES (gen_random_uuid(), $1, $2, 'قسم آخر', 4)
			RETURNING id`, sf.otherCollegeID, fmt.Sprintf("SD%d", suffix%1000000))
		sf.otherStudentID = f.scan(`
			INSERT INTO student (id, student_no, full_name, mother_name)
			VALUES (gen_random_uuid(), $1, 'سارة كريم جاسم', 'هدى')
			RETURNING id`, fmt.Sprintf("SS%d", suffix))

		// One enrollment in each college.
		f.enroll(1, 1, 1, f.morning, nil)
		f.exec(`
			INSERT INTO enrollment (
				id, student_id, academic_year_id, sequence_no,
				college_id, department_id, study_type_id, student_category_id,
				stage, attempt_number, enrollment_status, academic_result)
			VALUES (gen_random_uuid(), $1, $2, 1, $3, $4, $5, $6, 1, 1, 'active', 'pending')`,
			sf.otherStudentID, f.yearID, sf.otherCollegeID, sf.otherDepartmentID, f.morning, f.regular)

		body(sf)
	})
}

// A scoped operator's student search must not return the other college's
// students. Filtering the page afterwards would already have read them.
func TestStudentSearchIsBoundedByScope(t *testing.T) {
	withScopeFixture(t, func(sf *scopeFixture) {
		repo := NewStudentRepository(reportDB)

		unrestricted, _, err := repo.Search(sf.ctx, port.StudentSearch{
			AcademicYearID: &sf.yearID,
			Scope:          shared.ScopeFilter{},
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("unrestricted search: %v", err)
		}
		if len(unrestricted) < 2 {
			t.Fatalf("the fixture should give an unrestricted search both students, got %d", len(unrestricted))
		}

		scoped, _, err := repo.Search(sf.ctx, port.StudentSearch{
			AcademicYearID: &sf.yearID,
			Scope:          shared.LimitedTo([]shared.ID{sf.collegeID}, nil),
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("scoped search: %v", err)
		}
		for _, s := range scoped {
			if s.ID == sf.otherStudentID {
				t.Fatal("a scoped search returned a student from a college the caller does not hold")
			}
		}
		if len(scoped) == 0 {
			t.Fatal("the scoped search should still find the caller's own student")
		}
	})
}

// The direction that matters most: a scoped caller with no grants must see
// nothing, not everything. A filter that fails open is not a filter.
func TestEmptyScopeMatchesNothing(t *testing.T) {
	withScopeFixture(t, func(sf *scopeFixture) {
		students, total, err := NewStudentRepository(reportDB).Search(sf.ctx, port.StudentSearch{
			AcademicYearID: &sf.yearID,
			// Scoped, no grants: exactly what a newly narrowed account looks
			// like before an administrator assigns it a college.
			Scope: shared.LimitedTo(nil, nil),
			Limit: 100,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(students) != 0 || total != 0 {
			t.Fatalf("a scoped caller with no grants saw %d students (total %d); it must see none",
				len(students), total)
		}
	})
}

func TestEnrollmentListIsBoundedByScope(t *testing.T) {
	withScopeFixture(t, func(sf *scopeFixture) {
		repo := NewEnrollmentRepository(reportDB)

		all, _, err := repo.List(sf.ctx, port.EnrollmentFilter{
			AcademicYearID: &sf.yearID,
			Scope:          shared.ScopeFilter{},
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("unrestricted list: %v", err)
		}
		if len(all) < 2 {
			t.Fatalf("expected both enrollments unrestricted, got %d", len(all))
		}

		scoped, _, err := repo.List(sf.ctx, port.EnrollmentFilter{
			AcademicYearID: &sf.yearID,
			Scope:          shared.LimitedTo([]shared.ID{sf.collegeID}, nil),
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("scoped list: %v", err)
		}
		for _, e := range scoped {
			if e.CollegeID != sf.collegeID {
				t.Fatalf("a scoped list returned an enrollment from college %s", e.CollegeID)
			}
		}
		if len(scoped) == 0 {
			t.Fatal("the scoped list should still return the caller's own enrollment")
		}
	})
}

// A department grant is narrower than a college grant, and the query must
// honour the difference rather than treating any grant as "the whole college".
func TestDepartmentGrantDoesNotLeakTheCollege(t *testing.T) {
	withScopeFixture(t, func(sf *scopeFixture) {
		scoped, _, err := NewEnrollmentRepository(reportDB).List(sf.ctx, port.EnrollmentFilter{
			AcademicYearID: &sf.yearID,
			Scope:          shared.LimitedTo(nil, []shared.ID{sf.otherDepartmentID}),
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("scoped list: %v", err)
		}
		for _, e := range scoped {
			if e.DepartmentID != sf.otherDepartmentID {
				t.Fatalf("a department grant returned an enrollment from department %s", e.DepartmentID)
			}
		}
		if len(scoped) == 0 {
			t.Fatal("the granted department's enrollment should be visible")
		}
	})
}

// Reports are the widest read surface in the system, and the one a scoped
// report viewer is most likely to be pointed at.
func TestDebtReportIsBoundedByScope(t *testing.T) {
	withScopeFixture(t, func(sf *scopeFixture) {
		repo := NewReportRepository(reportDB)

		rows, _, err := repo.DebtReport(sf.ctx, port.DebtFilter{
			AcademicYearID: &sf.yearID,
			Scope:          shared.LimitedTo([]shared.ID{sf.collegeID}, nil),
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("scoped debt report: %v", err)
		}
		for _, row := range rows {
			if row.StudentID == sf.otherStudentID {
				t.Fatal("the debt report leaked a student from another college")
			}
		}

		// And with no grants at all, nothing.
		empty, total, err := repo.DebtReport(sf.ctx, port.DebtFilter{
			AcademicYearID: &sf.yearID,
			Scope:          shared.LimitedTo(nil, nil),
			Limit:          100,
		})
		if err != nil {
			t.Fatalf("empty-scope debt report: %v", err)
		}
		if len(empty) != 0 || total != 0 {
			t.Fatalf("a scoped caller with no grants got %d debt rows (total %d)", len(empty), total)
		}
	})
}

func TestSummaryReportIsBoundedByScope(t *testing.T) {
	withScopeFixture(t, func(sf *scopeFixture) {
		summaries, err := NewReportRepository(reportDB).DepartmentSummary(sf.ctx, port.SummaryFilter{
			AcademicYearID: &sf.yearID,
			Scope:          shared.LimitedTo([]shared.ID{sf.collegeID}, nil),
		})
		if err != nil {
			t.Fatalf("scoped summary: %v", err)
		}
		for _, college := range summaries {
			if college.CollegeID == sf.otherCollegeID {
				t.Fatal("the department summary leaked another college")
			}
		}
	})
}
