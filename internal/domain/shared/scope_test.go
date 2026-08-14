package shared_test

import (
	"testing"

	"github.com/swibit/flowed/internal/domain/shared"
)

func TestZeroScopeIsUniversityWide(t *testing.T) {
	// Actors are built in background jobs, CLI commands and tests. A zero
	// value that denied everything would turn a forgotten field into an
	// outage; a zero value that permits everything makes the same mistake
	// visible in a scope test instead.
	var actor shared.Actor
	if !actor.Scope.IsUniversityWide() {
		t.Fatal("the zero scope must be university-wide")
	}
	if err := actor.RequireScope("anything", ptrID(shared.NewID()), nil); err != nil {
		t.Fatalf("university-wide scope refused a college: %v", err)
	}
}

func TestLimitedScopeCoversGrantedCollege(t *testing.T) {
	engineering, medicine := shared.NewID(), shared.NewID()
	actor := shared.Actor{
		Username: "finance.eng",
		Scope:    shared.Scope{Mode: shared.ScopeLimited, Colleges: []shared.ID{engineering}},
	}

	if err := actor.RequireScope("RecordPayment", &engineering, nil); err != nil {
		t.Fatalf("granted college refused: %v", err)
	}
	err := actor.RequireScope("RecordPayment", &medicine, nil)
	if err == nil {
		t.Fatal("a college outside the grant must be refused")
	}
	var domainErr *shared.Error
	if !asError(err, &domainErr) {
		t.Fatalf("expected a domain error, got %T", err)
	}
	if domainErr.Code != "outside_scope" {
		t.Errorf("code = %q, want outside_scope", domainErr.Code)
	}
	if domainErr.Kind != shared.KindForbidden {
		t.Errorf("kind = %v, want forbidden", domainErr.Kind)
	}
}

func TestCollegeGrantCoversItsDepartments(t *testing.T) {
	engineering := shared.NewID()
	civil := shared.NewID()
	actor := shared.Actor{Scope: shared.Scope{Mode: shared.ScopeLimited, Colleges: []shared.ID{engineering}}}

	// The row names both, and the college half is granted. Department grants
	// are not expanded from colleges at grant time, because a department
	// created next year must still be covered by last year's college grant.
	if err := actor.RequireScope("op", &engineering, &civil); err != nil {
		t.Fatalf("a department inside a granted college was refused: %v", err)
	}
}

func TestDepartmentGrantDoesNotWidenToTheCollege(t *testing.T) {
	engineering, civil := shared.NewID(), shared.NewID()
	actor := shared.Actor{Scope: shared.Scope{Mode: shared.ScopeLimited, Departments: []shared.ID{civil}}}

	if err := actor.RequireScope("op", nil, &civil); err != nil {
		t.Fatalf("granted department refused: %v", err)
	}
	// A row naming only the college is not covered by a department grant.
	if err := actor.RequireScope("op", &engineering, nil); err == nil {
		t.Fatal("a department grant must not confer authority over the whole college")
	}
}

func TestScopedActorWithNoGrantsReachesNothing(t *testing.T) {
	actor := shared.Actor{Scope: shared.Scope{Mode: shared.ScopeLimited}}
	if err := actor.RequireScope("op", ptrID(shared.NewID()), nil); err == nil {
		t.Fatal("a scoped actor with no grants must reach nothing")
	}
	if !actor.QueryScope().Empty() {
		t.Fatal("the query filter for a scoped actor with no grants must match nothing")
	}
}

func TestObjectsWithNoOrganisationAreReachable(t *testing.T) {
	// A fee policy for the whole university names neither a college nor a
	// department. Scope guards student-bearing rows; refusing these would
	// block configuration reads that carry nobody's data.
	actor := shared.Actor{Scope: shared.Scope{Mode: shared.ScopeLimited, Colleges: []shared.ID{shared.NewID()}}}
	if err := actor.RequireScope("op", nil, nil); err != nil {
		t.Fatalf("a row naming no organisation was refused: %v", err)
	}
}

func TestQueryScopeMirrorsTheActor(t *testing.T) {
	college := shared.NewID()
	unrestricted := shared.Actor{}.QueryScope()
	if !unrestricted.Unrestricted || unrestricted.Empty() {
		t.Error("a university-wide actor must produce an unrestricted filter")
	}

	limited := shared.Actor{Scope: shared.Scope{Mode: shared.ScopeLimited, Colleges: []shared.ID{college}}}.QueryScope()
	if limited.Unrestricted {
		t.Error("a scoped actor must not produce an unrestricted filter")
	}
	if len(limited.Colleges) != 1 || limited.Colleges[0] != college {
		t.Errorf("filter colleges = %v, want [%v]", limited.Colleges, college)
	}
}

func TestScopeModeValidity(t *testing.T) {
	if !shared.ScopeUniversity.Valid() || !shared.ScopeLimited.Valid() {
		t.Error("both recognised modes must validate")
	}
	if shared.ScopeMode("faculty").Valid() {
		t.Error("an unrecognised mode must not validate")
	}
}

func ptrID(id shared.ID) *shared.ID { return &id }

// asError unwraps into a *shared.Error without pulling errors into every test.
func asError(err error, target **shared.Error) bool {
	e, ok := err.(*shared.Error)
	if ok {
		*target = e
	}
	return ok
}
