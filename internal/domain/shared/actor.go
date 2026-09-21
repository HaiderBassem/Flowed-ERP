package shared

import "slices"

// Role names what a user may do.
//
// There is exactly one. The system was built with a separation-of-duties
// matrix — cashier, finance manager, registrar, academic officer, auditor —
// and the university that runs it works one desk with one operator, for whom
// every one of those separations was a door they had to walk through twice.
// The role survives as a single value rather than being deleted outright so
// that the audit trail keeps recording an authority alongside every entry, and
// so that reintroducing a second role later is an addition rather than a
// retrofit.
type Role string

// RoleAdmin is the only role: it may do everything the system can do.
const RoleAdmin Role = "admin"

// AllRoles lists every role the system recognises.
var AllRoles = []Role{RoleAdmin}

// StaffRoles are the roles that may be granted to an account. Identical to
// AllRoles, and kept separate because the two answered different questions
// when there was more than one role.
var StaffRoles = []Role{RoleAdmin}

// IsStaff reports whether the role belongs to an operator of the system.
func (r Role) IsStaff() bool { return slices.Contains(StaffRoles, r) }

// Valid reports whether the role is one the system recognises.
func (r Role) Valid() bool { return slices.Contains(AllRoles, r) }

// Actor is the authenticated user on whose authority a command runs. It
// travels with every command so that the audit log can answer "who" without
// the domain layer reaching into HTTP context.
type Actor struct {
	UserID   ID
	Username string
	Roles    []Role
	// SessionID is the HTTP session or token identifier, recorded in the audit
	// trail so a suspicious sequence can be traced to one login.
	SessionID string
	// IPAddress is the request origin, recorded for the audit trail.
	IPAddress string
	// Scope is the actor's organisational reach. The zero value is
	// university-wide, so an Actor built without thinking about scope behaves
	// exactly as actors did before scoping existed — a missing field is then a
	// visible mistake rather than a silent denial of service.
	Scope Scope
}

// HasRole reports whether the actor holds the given role.
func (a Actor) HasRole(role Role) bool { return slices.Contains(a.Roles, role) }

// HasAnyRole reports whether the actor holds at least one of the given roles.
func (a Actor) HasAnyRole(roles ...Role) bool {
	for _, role := range roles {
		if a.HasRole(role) {
			return true
		}
	}
	return false
}

// IsSystem reports whether this is the internal system actor used by scheduled
// jobs such as nightly reconciliation.
func (a Actor) IsSystem() bool { return a.Username == SystemActorUsername }

// SystemActorUsername names the internal actor for background work.
const SystemActorUsername = "system"

// SystemActor builds the actor used by scheduled jobs.
func SystemActor() Actor {
	return Actor{
		UserID:   NilID,
		Username: SystemActorUsername,
		Roles:    []Role{RoleAdmin},
	}
}
