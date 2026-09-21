package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/platform/config"
	"flowed/internal/platform/httpx"
	"flowed/internal/platform/observability"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
	"flowed/webui"
)

// RouterDeps is everything the router needs to wire itself.
//
// The handler groups after Auth each own their own routes and mount them
// through a Register method. Splitting them that way keeps one area's routing
// out of another's file, which matters when several people work on the API at
// once; the cost is this struct, which is a fair trade.
type RouterDeps struct {
	Config      *config.Config
	Log         *slog.Logger
	DB          *pg.DB
	Handlers    *Handlers
	Auth        *AuthHandlers
	Reports     *ReportHandlers
	ConfigAdmin *ConfigHandlers
	Bulk        *BulkHandlers
	Receipts    *ReceiptHandlers
	UserAdmin   *UserHandlers
	Lifecycle   *LifecycleHandlers
	MasterData  *MasterDataHandlers
	// AuditArchive is always mounted, even when no destination is configured:
	// the routes then answer with what to set rather than 404, so "is the
	// trail archived here" is an answerable question.
	AuditArchive *AuditArchiveHandlers
	// Reconciliation is the invariant queue: what the nightly checks found and
	// what was done about it.
	Reconciliation *ReconciliationHandlers
	// Settings is the institution's own details: the name on every receipt.
	Settings *SettingsHandlers
	// SettingsService backs the export letterhead middleware, so a report PDF
	// carries the same university name a receipt does.
	SettingsService *app.SettingsService
	// ReportTimezone is what a generated-at stamp prints in. A report stamped
	// in UTC and read in Baghdad is three hours wrong, which shows on a daily
	// cash sheet run near midnight.
	ReportTimezone *time.Location
	// Data is the one-button CSV export and import of the whole system, next
	// to but not the same as Backups: a dump rebuilds a broken database, this
	// is what the office opens in Excel and carries to another machine.
	Data *DataHandlers
	// Backups serves the Backup & Restore screen: creating, listing,
	// restoring, exporting, importing and scheduling backups.
	Backups *BackupHandlers
	// Maintenance is closed for the short window a restore swaps the live
	// database's pool. Never nil in a real server — BuildEngine always
	// constructs one — but every handler must still work if a test builds a
	// router without going through it, which is why the field itself may be
	// nil and Middleware treats that as "always open."
	Maintenance *httpx.MaintenanceGate
	// AuthService backs the session-revocation middleware as well as the
	// credential endpoints: a token whose session was revoked must stop
	// working on the next request, not at the end of its lifetime.
	AuthService *app.AuthService
	// Users is read by the password-change gate, which needs the stored flag
	// rather than the token's copy of it.
	Users       port.UserRepository
	Tokens      *auth.TokenService
	Idempotency port.IdempotencyRepository
	// RateLimiter is the cross-replica budget. Nil leaves the limiter
	// per process.
	RateLimiter   port.RateLimiter
	Observability *observability.Provider
	Version       string
}

// probeRoutes are exempt from rate limiting.
//
// They were not exempt while the limiter was per process, and leaving them in
// once it is shared would be a regression: every replica's readiness probe now
// draws on the same budget as the load balancer's own health check, all of it
// keyed to one address, and the desks behind that address would start being
// refused because the infrastructure was checking whether they were up.
var probeRoutes = []string{"/health", "/ready"}

// NewRouter builds the HTTP engine.
//
// The middleware order below is load-bearing rather than stylistic:
//
//   - RequestID first, because every layer after it — the logger, the error
//     responder, the panic recovery — puts the id in its output.
//   - Observe next, so the span exists before the request logger is built and
//     every log record of the request carries the trace id, and so the latency
//     it measures includes everything the server does rather than everything
//     after the middleware that happens to be cheapest to instrument.
//   - Logger outside Recovery, so a recovered panic still produces exactly one
//     completion record, at error level.
//   - CORS before authentication, so a browser preflight, which carries no
//     Authorization header, is answered rather than rejected as unauthorised.
//   - Rate limiting before authentication, so a flood is dropped before it
//     costs a signature verification each.
//   - Idempotency innermost, on individual money-moving routes only. It must
//     sit inside authentication because it stamps the actor on the record, and
//     inside the role check because a request that will be refused for lack of
//     authority must not claim a key.
func NewRouter(deps RouterDeps) *gin.Engine {
	if deps.Config.App.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()

	// Client IP feeds both the rate limiter and the audit trail, so which
	// proxies may rewrite it is a configuration decision, never a default.
	if err := engine.SetTrustedProxies(deps.Config.HTTP.TrustedProxies); err != nil {
		deps.Log.Error("configuring trusted proxies", slog.String("error", err.Error()))
	}

	engine.Use(
		httpx.RequestID(),
		// Ahead of everything else that touches the database: a restore's
		// live-swap window closes this gate while the pool is briefly gone,
		// and every request behind it must get a clean, retryable 503 rather
		// than hang on a pool that does not exist yet.
		deps.Maintenance.Middleware(probeRoutes...),
		httpx.Observe(deps.Observability),
		httpx.Logger(deps.Log),
		httpx.Recovery(deps.Log),
		httpx.SecurityHeaders(deps.Config.Auth.RequireHSTS),
		httpx.CORS(deps.Config.HTTP),
		httpx.BodyLimit(deps.Config.HTTP.MaxRequestBodyBytes, map[string]int64{
			"/api/v1/backups/import": deps.Config.HTTP.MaxBackupUploadBytes,
		}),
		// Comfortably inside the server's write timeout, so a handler that runs
		// long still gets to write its error before the socket is torn down.
		httpx.Timeout(deps.Config.HTTP.WriteTimeout-2*time.Second),
		httpx.RateLimit(httpx.RateLimitConfig{
			PerMinute:    deps.Config.HTTP.RateLimitPerMinute,
			Shared:       deps.RateLimiter,
			ExemptRoutes: probeRoutes,
			Metrics:      deps.Observability.Instruments(),
			Log:          deps.Log,
		}),
	)

	engine.NoRoute(func(c *gin.Context) {
		httpx.Respond(c, shared.NotFound("route_not_found",
			"no route matches %s %s", c.Request.Method, c.Request.URL.Path))
	})

	registerHealth(engine, deps)
	registerUI(engine)

	v1 := engine.Group("/api/v1")

	// Unauthenticated: obtaining credentials.
	v1.POST("/auth/login", deps.Auth.Login)
	v1.POST("/auth/refresh", deps.Auth.Refresh)

	authenticated := v1.Group("")
	authenticated.Use(httpx.Authenticate(deps.Tokens))

	// Session revocation is checked here rather than inside Authenticate,
	// because the token is valid — it is the session behind it that was
	// withdrawn, and the two failures deserve different codes.
	if deps.Config.Auth.StrictSessionCheck && deps.AuthService != nil {
		authenticated.Use(httpx.RequireLiveSession(deps.AuthService, deps.Log))
	}

	// Routes an operator holding a password somebody else set may still reach.
	// Everything else is refused until they replace it: an administrator who
	// reset the password knows it, and at a cashier desk the receipt would
	// carry the cashier's name.
	authenticated.GET("/auth/me", deps.Auth.Me)
	authenticated.POST("/auth/logout", deps.Auth.Logout)
	authenticated.POST("/auth/change-password", deps.Auth.ChangePassword)
	authenticated.PATCH("/auth/me", deps.Auth.UpdateProfile)
	authenticated.GET("/auth/sessions", deps.Auth.MySessions)
	authenticated.POST("/auth/sessions/revoke-others", deps.Auth.RevokeMyOtherSessions)

	secured := authenticated.Group("")
	if deps.Users != nil {
		secured.Use(httpx.PasswordChangeGate(mustChangePassword(deps.Users), nil))
	}

	registerReference(secured, deps.Handlers)
	registerStudents(secured, deps.Handlers)
	registerEnrollments(secured, deps.Handlers)
	registerAccounts(secured, deps.Handlers, deps.Idempotency)
	registerPayments(secured, deps.Handlers, deps.Idempotency)
	registerRefunds(secured, deps.Handlers, deps.Idempotency)
	registerDiscounts(secured, deps.Handlers)
	registerYears(secured, deps.Handlers)
	registerOversight(secured, deps.Handlers)

	// Areas that own their own routing. Each mounts under the same
	// authenticated group and applies its own role checks per route.

	// Every export wears the institution's letterhead. Installed here, once,
	// rather than in each of the fifteen report handlers.
	//
	// The repositories are read behind a nil check because the specification
	// generator builds this router with typed-nil handler groups — it reads
	// the route table and calls nothing — and reaching into one for a field
	// panics before a single route is registered.
	var (
		exportYears     port.AcademicYearRepository
		exportReference port.ReferenceRepository
	)
	if deps.Handlers != nil {
		exportYears, exportReference = deps.Handlers.YearRepo, deps.Handlers.ReferenceRepo
	}
	secured.Use(WithExportChrome(
		deps.SettingsService, deps.ReportTimezone, exportYears, exportReference))

	deps.Reports.Register(secured)
	deps.ConfigAdmin.Register(secured)
	deps.Bulk.Register(secured)
	deps.Receipts.Register(secured)
	if deps.UserAdmin != nil {
		deps.UserAdmin.Register(secured)
	}
	if deps.Lifecycle != nil {
		deps.Lifecycle.Register(secured)
	}
	if deps.MasterData != nil {
		deps.MasterData.Register(secured)
	}
	if deps.AuditArchive != nil {
		deps.AuditArchive.Register(secured)
	}
	if deps.Reconciliation != nil {
		deps.Reconciliation.Register(secured)
	}
	if deps.Backups != nil {
		deps.Backups.Register(secured)
	}
	if deps.Data != nil {
		deps.Data.Register(secured)
	}
	if deps.Settings != nil {
		deps.Settings.Register(secured)
	}

	return engine
}

// mustChangePassword adapts the user store to the gate's narrow question.
//
// A read per request, which is why the gate answers from the row rather than
// from the token: the token's copy is up to one access lifetime stale, and a
// reset password that stayed usable for that long would defeat the point of
// marking it at all.
func mustChangePassword(users port.UserRepository) func(ctx context.Context, userID shared.ID) (bool, error) {
	return func(ctx context.Context, userID shared.ID) (bool, error) {
		if shared.IsNil(userID) {
			return false, nil
		}
		user, err := users.GetByID(ctx, userID)
		if err != nil {
			return false, err
		}
		return user.MustChangePassword, nil
	}
}

// registerHealth mounts the probes outside authentication: a load balancer has
// no credentials, and a readiness check that needs a token cannot report that
// authentication itself is broken.
// registerUI serves the operator interface from the binary.
//
// Mounted before the API group and under its own prefix, so an interface asset
// can never shadow an endpoint. It is served without authentication because it
// is a static page: everything it can do it does through the API, which
// authenticates every call. Serving the shell to an anonymous browser is what
// lets that browser render a sign-in form.
// uiContentSecurityPolicy is what the interface is allowed to load: its own
// assets and nothing else. It is also why the interface bundles its fonts
// rather than linking them — a window with no route to the public internet has
// to look exactly as it does on a connected machine.
const uiContentSecurityPolicy = "default-src 'self'; img-src 'self' data:; style-src 'self'; " +
	"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; " +
	"form-action 'self'"

func registerUI(engine *gin.Engine) {
	// Content types are set explicitly rather than sniffed. The API sets
	// X-Content-Type-Options: nosniff for good reasons, and a stylesheet
	// served as text/plain under that header is a stylesheet the browser
	// refuses.
	contentTypes := map[string]string{
		".html":  "text/html; charset=utf-8",
		".css":   "text/css; charset=utf-8",
		".js":    "text/javascript; charset=utf-8",
		".json":  "application/json; charset=utf-8",
		".svg":   "image/svg+xml",
		".ico":   "image/x-icon",
		".png":   "image/png",
		".woff2": "font/woff2",
	}

	assets := webui.Assets()
	built := webui.Built()

	engine.GET("/app/*filepath", func(c *gin.Context) {
		// A binary built before `make ui-build` says so, rather than answering
		// every asset with a 404 that reads like a broken deployment.
		if !built {
			c.Header("Content-Security-Policy", uiContentSecurityPolicy)
			c.Data(http.StatusServiceUnavailable, "text/html; charset=utf-8",
				[]byte(webui.NotBuiltNotice))
			return
		}

		name := strings.TrimPrefix(c.Param("filepath"), "/")
		if name == "" {
			name = "index.html"
		}

		content, err := fs.ReadFile(assets, name)
		if err != nil {
			// The interface is a history-router: every screen is a real path
			// under /app/, so an unknown path is a deep link or a refresh
			// rather than a missing file. Serving the shell is what makes a
			// refreshed page land where the operator was.
			//
			// A missing *asset* must not be answered this way: handing
			// index.html to a request for a .js file gives the browser HTML
			// where it expected a module, which fails with a MIME error that
			// names nothing useful. Those 404 honestly.
			if ext := filepath.Ext(name); ext != "" && ext != ".html" {
				c.Status(http.StatusNotFound)
				return
			}
			content, err = fs.ReadFile(assets, "index.html")
			if err != nil {
				c.Status(http.StatusNotFound)
				return
			}
			name = "index.html"
		}

		// The document-wide policy is written for JSON responses, which load
		// nothing. The interface loads its own stylesheet and modules and
		// nothing else, which is exactly what 'self' expresses.
		c.Header("Content-Security-Policy", uiContentSecurityPolicy)

		// Fingerprinted assets are immutable; the shell never is, or an
		// operator keeps yesterday's interface against today's API.
		//
		// no-store rather than no-cache for the shell, and the difference
		// matters here: no-cache permits a cached copy to be reused once
		// revalidated, and this response carries no ETag or Last-Modified to
		// revalidate against — so a browser may keep serving an old shell that
		// points at asset hashes this binary no longer contains. The shell is
		// about a kilobyte; re-fetching it is cheaper than a screen of 404s.
		if strings.HasPrefix(name, "assets/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			c.Header("Cache-Control", "no-store")
		}

		c.Data(http.StatusOK, contentTypes[filepath.Ext(name)], content)
	})
}

func registerHealth(engine *gin.Engine, deps RouterDeps) {
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"service": deps.Config.App.Name,
			"version": deps.Version,
		})
	})

	// Readiness reports the database, because an API that cannot reach
	// PostgreSQL can serve nothing useful and should be taken out of rotation
	// rather than left to fail every request.
	engine.GET("/ready", func(c *gin.Context) {
		ctx, cancel := c.Request.Context(), func() {}
		defer cancel()

		if err := deps.DB.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "unavailable",
				"reason": "database unreachable",
			})
			return
		}
		stats := deps.DB.Stats()
		c.JSON(http.StatusOK, gin.H{
			"status": "ready",
			"database": gin.H{
				"acquired_conns": stats.AcquiredConns,
				"idle_conns":     stats.IdleConns,
				"total_conns":    stats.TotalConns,
				"max_conns":      stats.MaxConns,
			},
		})
	})
}

func registerReference(g *gin.RouterGroup, h *Handlers) {
	// Readable by anyone signed in: a cashier's screen needs the list of
	// payment methods, and a registrar's needs the departments.
	g.GET("/study-types", h.ListStudyTypes)
	g.GET("/colleges", h.ListColleges)
	g.GET("/departments", h.ListDepartments)
	g.GET("/payment-methods", h.ListPaymentMethods)
}

func registerStudents(g *gin.RouterGroup, h *Handlers) {
	students := g.Group("/students")

	students.GET("", h.SearchStudents)
	students.GET("/:id", h.GetStudent)
	students.GET("/:id/enrollments", h.StudentEnrollments)
	students.GET("/:id/accounts", h.StudentAccounts)
	students.GET("/:id/discounts", h.StudentDiscounts)

	students.POST("",
		h.RegisterStudent)
	// Identity, academic placement, and — for an actor who also holds finance
	// authority — initial pricing, in one request. The role gate here is the
	// base authority to attempt it at all; RegisterStudentWithPlacement checks
	// each of the three composed commands' own authority again before running
	// it, and prices only when the actor qualifies for that too.
	students.POST("/intake",
		h.RegisterStudentWithPlacement)
	students.PATCH("/:id/contact",
		h.UpdateStudentContact)

	// The audit trail is the auditor's, and an administrator's. A cashier who
	// could read it could also learn which of their corrections were noticed.
	students.GET("/:id/audit",
		h.StudentAudit)
}

func registerEnrollments(g *gin.RouterGroup, h *Handlers) {
	enrollments := g.Group("/enrollments")

	enrollments.GET("/:id", h.GetEnrollment)

	enrollments.POST("",
		h.EnrollStudent)
	enrollments.POST("/:id/supersede",
		h.SupersedeEnrollment)
	enrollments.POST("/:id/result",
		h.RecordResult)
	enrollments.POST("/:id/status",
		h.ChangeEnrollmentStatus)
}

func registerAccounts(g *gin.RouterGroup, h *Handlers, idem port.IdempotencyRepository) {
	accounts := g.Group("/accounts")

	accounts.GET("/:id", h.GetAccount)

	// Account generation is idempotent-keyed as well as payments: a retried
	// generation that created a second account would give one enrollment two
	// sets of prices.
	accounts.POST("",
		httpx.Idempotency(idem, "GenerateFinancialAccount"),
		h.GenerateAccount)

	accounts.POST("/adjustments",
		httpx.Idempotency(idem, "PostAdjustment"),
		h.PostAdjustment)
}

func registerPayments(g *gin.RouterGroup, h *Handlers, idem port.IdempotencyRepository) {
	payments := g.Group("/payments")

	payments.GET("/:id", h.GetPayment)

	payments.POST("",
		httpx.Idempotency(idem, "RecordPayment"),
		h.RecordPayment)

	// Voiding was split in two so a cashier raised the request and a finance
	// manager executed it. With one account the split leaves a pending state
	// nobody is waiting on, so POST /voids/execute does both — and the void
	// request row is still written, which is what the void register reads
	// from, the most useful fraud-detection report in the system.
	//
	// The two-step routes stay mounted for a void that must be left pending on
	// purpose, and because the register's document is the same row either way.
	voids := g.Group("/voids")
	voids.GET("/pending",
		h.ListPendingVoids)
	voids.POST("/execute",
		httpx.Idempotency(idem, "VoidPayment"),
		h.VoidPayment)
	voids.POST("",
		h.RequestVoid)
	voids.POST("/:id/execute",
		httpx.Idempotency(idem, "ExecuteVoid"),
		h.ExecuteVoid)
}

func registerRefunds(g *gin.RouterGroup, h *Handlers, idem port.IdempotencyRepository) {
	refunds := g.Group("/refunds")

	refunds.GET("/pending",
		h.ListPendingRefunds)

	// One call raises, approves and pays out. See RefundService.IssueRefund —
	// a refund abandoned between the request and the payout tells the student
	// their money is coming and records that it never was.
	refunds.POST("/issue",
		httpx.Idempotency(idem, "IssueRefund"),
		h.IssueRefund)

	refunds.POST("",
		h.RequestRefund)
	refunds.POST("/:id/approve",
		h.ApproveRefund)
	refunds.POST("/:id/reject",
		h.RejectRefund)
	refunds.POST("/:id/post",
		httpx.Idempotency(idem, "PostRefund"),
		h.PostRefund)
}

func registerDiscounts(g *gin.RouterGroup, h *Handlers) {
	discounts := g.Group("/discounts")

	// One call assigns and approves. Where money has already been collected
	// the excess becomes credit on the account, which POST /refunds/issue is
	// what turns back into cash.
	discounts.POST("/grants",
		h.GrantDiscount)

	discounts.POST("/assignments",
		h.AssignDiscount)

	discounts.POST("/assignments/:id/approve",
		h.ApproveDiscount)
	discounts.POST("/assignments/:id/revoke",
		h.RevokeDiscount)
	discounts.POST("/applications/:id/confirm",
		h.ConfirmDiscountApplication)
}

func registerYears(g *gin.RouterGroup, h *Handlers) {
	years := g.Group("/academic-years")

	years.GET("", h.ListYears)

	years.POST("", h.CreateYear)
	years.POST("/:id/open", h.OpenYear)

	// Shutting the books is finance's call; declaring the academic year over
	// is not, so the two closes carry different authority.
	years.POST("/:id/close-financially",
		h.CloseYearFinancially)
	years.POST("/:id/close", h.CloseYear)
	years.POST("/:id/reopen", h.ReopenYear)
}

func registerOversight(g *gin.RouterGroup, h *Handlers) {
	oversight := g.Group("/oversight")

	oversight.GET("/audit/verify", h.VerifyAuditChain)
	oversight.GET("/reconciliation", h.ReconciliationReport)
}
