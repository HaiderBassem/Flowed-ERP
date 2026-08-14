package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryRouteHasAnAuthorityDecision stops an endpoint being added without
// anybody deciding who may call it.
//
// A route is acceptable in exactly three ways: it carries a role check, in its
// own middleware chain or in the group it is mounted on; it is deliberately
// public and says so in publicPaths; or its authority lives in the service,
// which is often the better place — a service check can see the row, and "a
// cashier may read their own shift" is not something a role check can express.
// Anything else reaches every signed-in operator, and for a debt register that
// is not the same thing as reaching a cashier.
//
// The router's own source is what it reads. Gin exposes only the last handler
// of a chain, so there is no way to ask the built engine which middleware a
// route carries; the registrations, however, are a small and consistent set of
// files, and the same technique already guards the configuration loader.
func TestEveryRouteHasAnAuthorityDecision(t *testing.T) {
	groups, routes := parseRegistrations(t)

	for _, route := range routes {
		if route.hasRoleCheck {
			continue
		}
		if groupRequiresRoles(groups, route.group) {
			continue
		}
		path := fullPath(groups, route.group, route.pathFragment)
		if serviceEnforcedRoutes[path] || publicRouteFragments[path] {
			continue
		}
		t.Errorf("%s %s (%s) has no authority decision: give it a role check, "+
			"declare it public, or record it as service-enforced with the reason",
			route.method, path, route.file)
	}
}

// serviceEnforcedRoutes are the paths whose authority is decided by the service
// behind them, with the reason each one belongs there rather than in a role
// check. The path fragment is the literal in the registration, not the full
// route, because that is what a reader comparing this list against the source
// will have in front of them.
var serviceEnforcedRoutes = map[string]bool{
	// Reads narrowed by organisational scope. The scope is applied to the query
	// rather than to its result: filtering afterwards would have read the other
	// colleges' rows first, which is the thing scope exists to prevent.
	"/students":                  true,
	"/students/:id":              true,
	"/students/:id/audit":        true,
	"/students/:id/accounts":     true,
	"/students/:id/enrollments":  true,
	"/students/:id/discounts":    true,
	"/students/:id/sponsorships": true,
	"/enrollments/:id":           true,
	"/accounts/:id":              true,
	"/payments/:id":              true,
	"/refunds/:id":               true,
	"/void-requests/:id":         true,

	// AccountService.PlanRevisions names the five roles that may read a
	// schedule history. The check is in the service because the same method
	// answers the portal, where the caller is a student reading their own.
	"/accounts/:id/plan-revisions": true,

	// A payment intent is read through the poll path, which is where a
	// student's claim to an account is checked against actor.StudentID and a
	// member of staff's against their scope. A role check here would have to
	// admit students, and would then be no check at all.
	"/payments/:id/poll":             true,
	"/payments/accounts/:account_id": true,

	// Reference data. Every signed-in operator reads it, none of it is about a
	// person, and all of it is printed on documents the university hands out.
	"/academic-years":        true,
	"/academic-years/:id":    true,
	"/colleges":              true,
	"/departments":           true,
	"/study-types":           true,
	"/student-categories":    true,
	"/payment-methods":       true,
	"/fee-components":        true,
	"/discount-definitions":  true,
	"/installment-templates": true,
	"/cashier-desks":         true,
	"/number-series":         true,

	// About the caller, and only the caller.
	"/api/v1/auth/me":                     true,
	"/api/v1/auth/logout":                 true,
	"/api/v1/auth/sessions":               true,
	"/api/v1/auth/sessions/revoke-others": true,
	"/api/v1/auth/change-password":        true,

	// Revoking one session is the interesting case: an operator may revoke
	// their own without holding any role, and somebody else's only as an
	// administrator. UserService.RevokeSession compares the session's owner to
	// the actor and requires admin when they differ — a decision that needs the
	// row, which is exactly what a role check cannot see.
	"/sessions/:id/revoke": true,
}

// publicRouteFragments are mounted outside authentication entirely. The list in
// openapi.go is the one that matters; this mirrors it in the form the scanner
// sees, and TestPublicRoutesAreDeclaredPublic asserts the other list separately.
var publicRouteFragments = map[string]bool{
	"/health":                      true,
	"/ready":                       true,
	"/api/v1/auth/login":           true,
	"/api/v1/auth/refresh":         true,
	"/webhooks/payments/:provider": true,
	"/verify/statement/:code":      true,
	"/api/v1/public/cashier-desks": true,
	"/app/*filepath":               true,
}

type registration struct {
	file         string
	method       string
	pathFragment string
	group        string
	hasRoleCheck bool
}

var (
	routeCall = regexp.MustCompile(
		`(?m)^\s*(\w+)\.(GET|POST|PUT|PATCH|DELETE)\(\s*"([^"]*)"(.*)$`)
	groupCall = regexp.MustCompile(
		`(?m)^\s*(\w+)\s*:?=\s*(\w+)\.Group\(\s*"([^"]*)"(.*)$`)
	// Several handler groups hoist their role checks into a variable — `admin
	// := httpx.RequireRoles(shared.RoleAdmin)` — and then pass it to a dozen
	// routes. A scanner that only looked for the call would report every one of
	// those as undecided, which is how a sweep like this teaches people to
	// ignore it.
	guardVariable = regexp.MustCompile(`(?m)^\s*(\w+)\s*:=\s*httpx\.Require\w+\(`)
)

// parseRegistrations reads every route and group declaration in this package.
func parseRegistrations(t *testing.T) (map[string]groupInfo, []registration) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	groups := map[string]groupInfo{}
	var routes []registration

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		text := string(source)

		guards := map[string]bool{}
		for _, match := range guardVariable.FindAllStringSubmatch(text, -1) {
			guards[match[1]] = true
		}

		for _, match := range groupCall.FindAllStringSubmatch(text, -1) {
			variable, parent, tail := match[1], match[2], match[4]
			groups[variable] = groupInfo{
				parent:       parent,
				prefix:       match[3],
				hasRoleCheck: mentionsRoleCheck(tail, text, match[0]) || namesGuard(tail, guards),
			}
		}

		for _, match := range routeCall.FindAllStringSubmatch(text, -1) {
			receiver, method, path, tail := match[1], match[2], match[3], match[4]
			routes = append(routes, registration{
				file:         name,
				method:       method,
				pathFragment: path,
				group:        receiver,
				hasRoleCheck: mentionsRoleCheck(tail, text, match[0]) || namesGuard(tail, guards),
			})
		}
	}

	if len(routes) < 50 {
		t.Fatalf("only %d route registrations found; the scanner has stopped "+
			"matching and this test would pass for the wrong reason", len(routes))
	}
	return groups, routes
}

type groupInfo struct {
	parent       string
	prefix       string
	hasRoleCheck bool
}

// fullPath walks the group chain so the allowlists can be written as the paths
// a reader recognises rather than as the fragments a registration happens to
// use. Half the routes in this package are registered as "" or "/:id" inside a
// group, and an allowlist keyed on those would be unreadable and would match
// the wrong things.
func fullPath(groups map[string]groupInfo, group, fragment string) string {
	prefix := ""
	name := group
	for depth := 0; depth < 8; depth++ {
		info, ok := groups[name]
		if !ok {
			break
		}
		prefix = info.prefix + prefix
		name = info.parent
	}
	path := prefix + fragment
	if path == "" {
		path = "/"
	}
	return path
}

func groupRequiresRoles(groups map[string]groupInfo, name string) bool {
	// Bounded: a cycle would otherwise hang the test rather than fail it.
	for depth := 0; depth < 8; depth++ {
		info, ok := groups[name]
		if !ok {
			return false
		}
		if info.hasRoleCheck {
			return true
		}
		name = info.parent
	}
	return false
}

// namesGuard reports whether the call passes one of the hoisted role checks.
func namesGuard(tail string, guards map[string]bool) bool {
	for name := range guards {
		// Word boundaries: a guard called "read" must not match "readers".
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(tail) {
			return true
		}
	}
	return false
}

// mentionsRoleCheck looks at the rest of the call, and — because a registration
// often spans several lines — at the few lines following it.
func mentionsRoleCheck(tail, text, anchor string) bool {
	if strings.Contains(tail, "RequireRoles") || strings.Contains(tail, "RequireAnyRole") {
		return true
	}
	index := strings.Index(text, anchor)
	if index < 0 {
		return false
	}
	rest := text[index:]
	if len(rest) > 400 {
		rest = rest[:400]
	}
	// Only up to the end of this call: the next registration's role check is
	// not this one's.
	if end := strings.Index(rest, "\n\n"); end > 0 {
		rest = rest[:end]
	}
	return strings.Contains(rest, "RequireRoles") || strings.Contains(rest, "RequireAnyRole")
}
