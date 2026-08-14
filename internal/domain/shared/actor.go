package shared

import "slices"

// Role names the job a user performs. Roles come straight from the domain
// design's separation-of-duties matrix: the cashier who takes money can never
// be the person who approves writing it off, and the administrator who
// configures the system deliberately cannot post payments.
type Role string

const (
	// RoleAdmin configures the system and manages the academic year lifecycle.
	// Deliberately excluded from posting payments.
	RoleAdmin Role = "admin"
	// RoleFinanceManager owns fee policy, approves discounts, executes voids
	// and refunds, and co-signs year close.
	RoleFinanceManager Role = "finance_manager"
	// RoleCashier records payments and prints receipts, and may request — but
	// never execute — a void.
	RoleCashier Role = "cashier"
	// RoleRegistrar owns student identity and enrollment lifecycle.
	RoleRegistrar Role = "registrar"
	// RoleAcademicOfficer records results and runs promotions.
	RoleAcademicOfficer Role = "academic_officer"
	// RoleReportViewer reads reports within its scope and nothing else.
	RoleReportViewer Role = "report_viewer"
	// RoleAuditor reads everything, including the audit log, and writes nothing.
	RoleAuditor Role = "auditor"
)

// AllRoles lists every role the system recognises.
var AllRoles = []Role{
	RoleAdmin, RoleFinanceManager, RoleCashier, RoleRegistrar,
	RoleAcademicOfficer, RoleReportViewer, RoleAuditor,
}

// Valid reports whether the role is one the system recognises.
func (r Role) Valid() bool { return slices.Contains(AllRoles, r) }

// Actor is the authenticated user on whose authority a command runs. It
// travels with every command so that the audit log can answer "who" without
// the domain layer reaching into HTTP context.
type Actor struct {
	UserID   ID
	Username string
	Roles    []Role
	// CashierDeskID scopes a cashier to a physical desk. Receipt series are
	// allocated per (academic year, desk), so a cashier without a desk cannot
	// post cash payments.
	CashierDeskID *ID
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

// RequireAnyRole returns a Forbidden error unless the actor holds one of the
// listed roles. Command handlers call this as their first act, so authority is
// checked before any state is read or locked.
func (a Actor) RequireAnyRole(operation string, roles ...Role) error {
	if a.HasAnyRole(roles...) {
		return nil
	}
	return Forbidden("insufficient_role",
		"%s requires one of the roles %v; actor %q holds %v", operation, roles, a.Username, a.Roles).
		WithDetail("operation", operation).
		WithDetail("required_roles", roles).
		WithDetail("actor_roles", a.Roles)
}

// IsSystem reports whether this is the internal system actor used by scheduled
// jobs such as nightly reconciliation.
func (a Actor) IsSystem() bool { return a.Username == SystemActorUsername }

// SystemActorUsername names the internal actor for background work.
const SystemActorUsername = "system"

// SystemActor builds the actor used by scheduled jobs. It holds no interactive
// roles: background work runs specific whitelisted routines, never arbitrary
// commands, so granting it broad authority would be a standing back door.
func SystemActor() Actor {
	return Actor{
		UserID:   NilID,
		Username: SystemActorUsername,
		Roles:    nil,
	}
}
