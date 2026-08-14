package shared

import "slices"

// ScopeMode says how far an actor's authority reaches organisationally.
//
// Roles answer "what may this person do"; scope answers "to whose data". The
// two are orthogonal, and conflating them is what let a finance manager
// appointed for engineering void a receipt in medicine: the role was correct
// and there was nothing else to check.
type ScopeMode string

const (
	// ScopeUniversity is authority over every college. It is what every user
	// held before scoping existed, and it stays the default so that adding
	// scope changed nobody's access on the day it shipped.
	ScopeUniversity ScopeMode = "university"
	// ScopeLimited restricts the actor to the colleges and departments granted
	// to them. A limited actor with no grants reaches nothing — the safe
	// direction for the absence of a rule to point.
	ScopeLimited ScopeMode = "scoped"
)

// Valid reports whether the mode is one the system recognises.
func (m ScopeMode) Valid() bool { return m == ScopeUniversity || m == ScopeLimited }

// Scope is the organisational reach of an actor.
//
// Departments are listed separately from colleges rather than expanded into
// them: a grant on a college must keep covering a department created next year,
// and expanding at grant time would silently fail to.
type Scope struct {
	Mode ScopeMode
	// Colleges the actor may act on. A college grant covers every department
	// within it.
	Colleges []ID
	// Departments the actor may act on directly, for an officer who runs one
	// department rather than a whole college.
	Departments []ID
}

// UniversityScope returns unrestricted organisational reach.
func UniversityScope() Scope { return Scope{Mode: ScopeUniversity} }

// IsUniversityWide reports whether the scope imposes no organisational limit.
//
// An empty mode counts as university-wide. That matters because Actor values
// are constructed in several places — background jobs, CLI commands, tests —
// and a zero value that silently denied everything would turn a missing field
// into an outage rather than into a visible mistake.
func (s Scope) IsUniversityWide() bool { return s.Mode != ScopeLimited }

// Covers reports whether the scope reaches a given college and department.
//
// Either identifier may be nil, which means "not applicable to this object".
// An object that names neither — a fee policy for the whole university, say —
// is reachable by any scope; the guard is about student-bearing rows.
func (s Scope) Covers(collegeID, departmentID *ID) bool {
	if s.IsUniversityWide() {
		return true
	}
	if collegeID == nil && departmentID == nil {
		// The object belongs to no college: a payment method, an installment
		// template for the whole university. There is nothing here for scope to
		// restrict, and refusing would block configuration that carries
		// nobody's data.
		return true
	}
	if collegeID != nil && slices.Contains(s.Colleges, *collegeID) {
		return true
	}
	if departmentID != nil && slices.Contains(s.Departments, *departmentID) {
		return true
	}
	return false
}

// CoversCollege reports whether the scope reaches a college outright.
func (s Scope) CoversCollege(collegeID ID) bool {
	if s.IsUniversityWide() {
		return true
	}
	return slices.Contains(s.Colleges, collegeID)
}

// RequireScope returns a Forbidden error unless the actor reaches the given
// college and department.
//
// Called after the row is read and before it is changed, because the check
// needs the row's own college and department — which is exactly why this is not
// something a route-level middleware can do for every command.
func (a Actor) RequireScope(operation string, collegeID, departmentID *ID) error {
	if a.Scope.Covers(collegeID, departmentID) {
		return nil
	}
	err := Forbidden("outside_scope",
		"%s is outside %q's organisational scope", operation, a.Username).
		WithDetail("operation", operation).
		WithDetail("remedy", "ask an administrator to widen this account's scope, "+
			"or hand the task to somebody appointed for that college")
	if collegeID != nil {
		err = err.WithDetail("college_id", collegeID.String())
	}
	if departmentID != nil {
		err = err.WithDetail("department_id", departmentID.String())
	}
	return err
}

// ScopeFilter describes the organisational restriction a query must apply.
//
// Read paths cannot use RequireScope, because the row is not read yet — the
// point is to not read it. A repository takes this and adds a WHERE clause, so
// a scoped user's report contains their colleges rather than everyone's with
// the others filtered out afterwards, which would still have read them.
//
// The zero value is unrestricted, matching the zero Scope on an actor. The two
// defaults have to agree: they did not at first, and the result was that a
// repository call which simply omitted the field returned nothing at all —
// silently, and only in the paths nobody had converted yet. One rule, stated
// once: an unset scope is university-wide, and narrowing is deliberate.
type ScopeFilter struct {
	// Mode is ScopeLimited when the lists below bound the query. Anything else,
	// including the zero value, means no organisational restriction.
	Mode        ScopeMode
	Colleges    []ID
	Departments []ID
}

// Unrestricted reports whether the filter imposes no organisational limit.
func (f ScopeFilter) Unrestricted() bool { return f.Mode != ScopeLimited }

// LimitedTo builds a filter restricted to the given colleges and departments.
// Used by tests and by any caller assembling a scope by hand; ordinary code
// takes the actor's own through QueryScope.
func LimitedTo(colleges, departments []ID) ScopeFilter {
	return ScopeFilter{Mode: ScopeLimited, Colleges: colleges, Departments: departments}
}

// QueryScope renders the actor's scope as a filter for repositories.
func (a Actor) QueryScope() ScopeFilter {
	if a.Scope.IsUniversityWide() {
		return ScopeFilter{}
	}
	return ScopeFilter{
		Mode:        ScopeLimited,
		Colleges:    a.Scope.Colleges,
		Departments: a.Scope.Departments,
	}
}

// Empty reports whether the filter would match nothing. A scoped actor holding
// no grants is in exactly this position, and a query must return nothing rather
// than everything — the difference between failing closed and failing open.
func (f ScopeFilter) Empty() bool {
	return f.Mode == ScopeLimited && len(f.Colleges) == 0 && len(f.Departments) == 0
}
