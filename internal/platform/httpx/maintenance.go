package httpx

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// MaintenanceGate is the switch a live database restore throws while it
// swaps the pool underneath the running process.
//
// The window it guards is short but real: between pg.DB.Drain and pg.DB.Reopen
// nothing in the process holds a working connection, and a request that
// reached a handler in that window would not fail cleanly — it would hang
// until the pool reopens or time out with a confusing error. This turns that
// window into an explicit, retryable 503 instead, mounted ahead of every other
// middleware so it answers before routing, authentication, or rate limiting
// even run.
type MaintenanceGate struct {
	active atomic.Bool
	reason atomic.Pointer[string]
}

// NewMaintenanceGate builds a gate that starts open — the ordinary state,
// requests flowing — since a system that starts closed and nothing ever opens
// it is indistinguishable from one that never starts.
func NewMaintenanceGate() *MaintenanceGate { return &MaintenanceGate{} }

// Enter closes the gate. Every request other than the exempted probes is
// refused until Exit.
func (g *MaintenanceGate) Enter(reason string) {
	g.reason.Store(&reason)
	g.active.Store(true)
}

// Exit reopens the gate.
func (g *MaintenanceGate) Exit() { g.active.Store(false) }

// Active reports whether the gate is currently closed. Nil-safe, and reports
// open: a router built without a gate — every test that constructs one
// directly rather than through bootstrap — must behave as if maintenance mode
// does not exist, not panic on the first request.
func (g *MaintenanceGate) Active() bool { return g != nil && g.active.Load() }

// Middleware refuses every request while the gate is closed, except the paths
// named in exempt — the health and readiness probes, so an orchestrator
// watching them sees the process as alive rather than restarting it mid-restore.
func (g *MaintenanceGate) Middleware(exempt ...string) gin.HandlerFunc {
	skip := make(map[string]bool, len(exempt))
	for _, p := range exempt {
		skip[p] = true
	}
	return func(c *gin.Context) {
		if !g.Active() || skip[c.Request.URL.Path] {
			c.Next()
			return
		}
		reason := "the server is restoring a backup"
		if r := g.reason.Load(); r != nil && *r != "" {
			reason = *r
		}
		c.Header("Retry-After", "5")
		// 503 has no domain kind of its own, the same reasoning the rate
		// limiter's 429 uses: this is a property of the process right now,
		// not a business rule any command would refuse on its own.
		writeError(c, http.StatusServiceUnavailable, ErrorBody{
			Code:      "maintenance_mode",
			Message:   reason + "; retry shortly",
			RequestID: RequestIDFrom(c),
		})
	}
}
