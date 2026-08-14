// Command api serves the tuition management HTTP API.
//
//	api                 start the server
//	api version         print the build identity
//	api healthcheck     probe a running server (used by the container health check)
//	api seed            create the first administrator, for development
//	api create-user     add an operator with the given roles
//	api demo            load an exploration dataset (development only)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/swibit/flowed/internal/adapter/httpapi"
	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/adapter/receipt"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/buildinfo"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/migrate"
	"github.com/swibit/flowed/internal/platform/observability"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
	"github.com/swibit/flowed/migrations"
)

// version is the resolved build identity: the release stamp when the pipeline
// built this binary, the embedded VCS revision when a developer did, and
// buildinfo.Unknown when neither identifies it. Production refuses to serve on
// the last case — see config validation.
var version = buildinfo.Version()

func main() {
	command := ""
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	var err error
	switch command {
	case "", "serve":
		err = serve()
	case "version":
		fmt.Println(buildinfo.Get().Describe())
	case "healthcheck":
		err = healthcheck()
	case "seed":
		err = seed()
	case "create-user":
		err = createUser()
	case "demo":
		err = demo()
	default:
		fmt.Fprintf(os.Stderr,
			"unknown command %q; expected serve, version, healthcheck, seed, create-user or demo\n",
			command)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.Log, cfg.App.Name, version, cfg.App.Environment)
	slog.SetDefault(log)

	// SIGTERM cancels this context, which begins the graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before the pool, because the pool takes the query tracer from it.
	obs, err := observability.Setup(ctx, cfg.Observability, cfg.App, version, log)
	if err != nil {
		return err
	}

	db, err := pg.Connect(ctx, cfg.Database, log, observability.NewQueryTracer(obs))
	if err != nil {
		return err
	}
	defer db.Close()

	if err := obs.ObservePool(func() observability.PoolStats {
		s := db.Stats()
		return observability.PoolStats{
			Acquired:      s.AcquiredConns,
			Idle:          s.IdleConns,
			Total:         s.TotalConns,
			Max:           s.MaxConns,
			AcquireCount:  s.AcquireCount,
			EmptyAcquires: s.EmptyAcquires,
			AcquireWait:   s.AcquireDuration,
		}
	}); err != nil {
		return err
	}

	// A binary whose migrations do not match the database must refuse to
	// serve. Running queries against a schema this build does not understand
	// is how a rolling deploy corrupts data quietly: the old replica writes
	// rows the new one cannot read, or worse, the reverse.
	runner, err := migrate.New(db.Pool(), migrations.FS, migrations.Dir, log)
	if err != nil {
		return err
	}
	if err := runner.Validate(ctx); err != nil {
		return fmt.Errorf("schema check failed, refusing to serve: %w", err)
	}

	engine, scheduler := buildEngine(cfg, log, db, obs)

	// Background work starts before the listener. A replica that begins
	// serving while its reaper is not yet running would leave an expired
	// adjustment window open for the length of that gap.
	if err := scheduler.Start(ctx); err != nil {
		return fmt.Errorf("starting background jobs: %w", err)
	}
	defer scheduler.Stop()

	metricsServer := startMetricsListener(cfg.Observability, obs, log)

	server := &http.Server{
		Addr:              cfg.HTTP.Addr(),
		Handler:           engine,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening",
			slog.String("addr", cfg.HTTP.Addr()),
			slog.String("env", cfg.App.Environment),
			slog.String("version", version))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received, draining",
			slog.Duration("grace_period", cfg.HTTP.ShutdownTimeout))
	}

	// The drain window matters more here than in most services: a payment
	// transaction cut off mid-flight would leave a cashier holding cash with
	// no receipt, and the client has no way to tell that from a lost response.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown timed out; some in-flight requests were cut off",
			slog.String("error", err.Error()))
		return err
	}

	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Warn("metrics listener did not shut down cleanly",
				slog.String("error", err.Error()))
		}
	}

	// Last, and after the API has drained. The spans and samples produced by
	// the requests that were still in flight are exactly the ones worth
	// keeping — a shutdown is where the interesting latency lives — and
	// flushing before the drain would discard them.
	flushCtx, cancelFlush := context.WithTimeout(
		context.Background(), cfg.Observability.ShutdownTimeout)
	defer cancelFlush()
	if err := obs.Shutdown(flushCtx); err != nil {
		log.Warn("telemetry did not flush cleanly", slog.String("error", err.Error()))
	}

	log.Info("shutdown complete")
	return nil
}

// startMetricsListener serves the scrape endpoint on its own listener, or
// returns nil when metrics are switched off.
//
// Separate from the API's listener on purpose. The scrape surface enumerates
// every route and its error rate, which is a map of the system, and it carries
// no authentication — the bind address is the access control, which is why it
// defaults to loopback and why putting it on the public listener would be a
// different decision rather than a simpler one.
//
// A failure to bind is logged and not fatal. Losing the dashboard is a
// nuisance; refusing to collect tuition because the dashboard would not start
// is an outage.
func startMetricsListener(cfg config.Observability, obs *observability.Provider, log *slog.Logger) *http.Server {
	handler := obs.MetricsHandler()
	if handler == nil {
		return nil
	}

	server := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	go func() {
		log.Info("metrics listening",
			slog.String("addr", cfg.MetricsAddr),
			slog.String("path", cfg.MetricsPath))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped",
				slog.String("addr", cfg.MetricsAddr),
				slog.String("error", err.Error()))
		}
	}()

	return server
}

// buildEngine wires every layer. This is the only place in the process where
// concrete implementations meet the interfaces they satisfy.
func buildEngine(
	cfg *config.Config, log *slog.Logger, db *pg.DB, obs *observability.Provider,
) (http.Handler, *app.Scheduler) {
	clock := shared.SystemClock{}

	txManager := postgres.NewTxManager(db)
	students := postgres.NewStudentRepository(db)
	years := postgres.NewAcademicYearRepository(db)
	enrollments := postgres.NewEnrollmentRepository(db)
	reference := postgres.NewReferenceRepository(db)
	feePolicies := postgres.NewFeePolicyRepository(db)
	templates := postgres.NewInstallmentTemplateRepository(db)
	discounts := postgres.NewDiscountRepository(db)
	accounts := postgres.NewAccountRepository(db)
	installments := postgres.NewInstallmentRepository(db)
	payments := postgres.NewPaymentRepository(db)
	refunds := postgres.NewRefundRepository(db)
	voidRequests := postgres.NewVoidRequestRepository(db)
	series := postgres.NewNumberSeriesRepository(db)
	sessions := postgres.NewCashierSessionRepository(db)
	audit := postgres.NewAuditRepository(db)
	idempotency := postgres.NewIdempotencyRepository(db)
	users := postgres.NewUserRepository(db)
	authSessions := postgres.NewSessionRepository(db)
	loginAttempts := postgres.NewLoginAttemptRepository(db)
	reports := postgres.NewReportRepository(db)
	imports := postgres.NewImportRepository(db)

	// Left nil when the shared limiter is switched off, which is what makes
	// the middleware fall back to its in-process bucket and the scheduler skip
	// the sweep of a table nothing is writing to.
	var rateLimiter port.RateLimiter
	if cfg.HTTP.RateLimitShared {
		rateLimiter = postgres.NewRateLimitRepository(db)
	}

	deps := app.Deps{
		Tx:           txManager,
		Students:     students,
		Years:        years,
		Enrollments:  enrollments,
		Reference:    reference,
		FeePolicies:  feePolicies,
		Templates:    templates,
		Discounts:    discounts,
		Accounts:     accounts,
		Installments: installments,
		Payments:     payments,
		Refunds:      refunds,
		VoidRequests: voidRequests,
		Series:       series,
		Sessions:     sessions,
		Audit:        audit,
		Users:        users,
		Clock:        clock,
		Log:          log,
		Metrics:      obs.Instruments(),
	}

	tokens := auth.NewTokenService(cfg.Auth)
	hasher := auth.NewHasher(cfg.Auth)

	userService := app.NewUserService(deps, hasher, authSessions, loginAttempts)
	authService := app.NewAuthService(deps, tokens, hasher, authSessions, loginAttempts, app.LockoutPolicy{
		MaxFailures: cfg.Auth.MaxLoginFailures,
		Window:      cfg.Auth.LoginFailureWindow,
		LockFor:     cfg.Auth.LockoutDuration,
	})

	accountService := app.NewAccountService(deps)
	enrollmentService := app.NewEnrollmentService(deps)
	studentService := app.NewStudentService(deps)

	handlers := &Handlers{
		Students:        studentService,
		Enrollments:     enrollmentService,
		Accounts:        accountService,
		Payments:        app.NewPaymentService(deps),
		Refunds:         app.NewRefundService(deps),
		Discounts:       app.NewDiscountService(deps),
		Years:           app.NewYearService(deps),
		StudentRepo:     students,
		EnrollmentRepo:  enrollments,
		AccountRepo:     accounts,
		InstallmentRepo: installments,
		PaymentRepo:     payments,
		RefundRepo:      refunds,
		VoidRequestRepo: voidRequests,
		DiscountRepo:    discounts,
		YearRepo:        years,
		ReferenceRepo:   reference,
		AuditRepo:       audit,
		Clock:           clock,
	}

	bulkService := app.NewBulkService(deps, accountService, enrollmentService)
	importService := app.NewImportService(deps, imports, studentService, enrollmentService)
	cashierService := app.NewCashierService(deps)

	// A receipt states Baghdad local time even though every stored timestamp is
	// UTC: it records the moment the cashier and the student were both standing
	// there, and printing a UTC hour on a slip handed over near midnight
	// invites a dispute that is entirely avoidable.
	location, err := time.LoadLocation(cfg.App.DefaultTimezone)
	if err != nil {
		log.Warn("unknown timezone, receipts will print UTC",
			slog.String("timezone", cfg.App.DefaultTimezone))
		location = time.UTC
	}
	receiptService := app.NewReceiptService(deps, receipt.Institution{
		UniversityNameAr: cfg.Receipt.UniversityNameAr,
		CollegeNameAr:    cfg.Receipt.CollegeNameAr,
		Address:          cfg.Receipt.Address,
		Phone:            cfg.Receipt.Phone,
		LogoDataURI:      cfg.Receipt.LogoDataURI,
	}, location)

	// Defaults come from the scheduler itself; only a deployment with a reason
	// to differ overrides them.
	scheduler := app.NewScheduler(deps, db, idempotency, rateLimiter, app.SchedulerConfig{
		RateLimitIdleTTL: cfg.HTTP.RateLimitIdleTTL,
		Sessions:         authSessions,
		LoginAttempts:    loginAttempts,
	})

	engine := httpapi.NewRouter(httpapi.RouterDeps{
		Config:        cfg,
		Log:           log,
		DB:            db,
		Handlers:      handlers,
		Auth:          httpapi.NewAuthHandlers(authService, userService, log),
		UserAdmin:     httpapi.NewUserHandlers(userService),
		AuthService:   authService,
		Users:         users,
		Reports:       httpapi.NewReportHandlers(reports),
		ConfigAdmin:   httpapi.NewConfigHandlers(app.NewConfigService(deps)),
		Bulk:          httpapi.NewBulkHandlers(bulkService, importService, imports),
		Cashier:       httpapi.NewCashierHandlers(cashierService, db),
		Receipts:      httpapi.NewReceiptHandlers(receiptService),
		Tokens:        tokens,
		Idempotency:   idempotency,
		RateLimiter:   rateLimiter,
		Observability: obs,
		Version:       version,
	})

	return engine, scheduler
}

// Handlers is an alias so buildEngine reads without repeating the package
// qualifier on every field.
type Handlers = httpapi.Handlers

// healthcheck probes a running server. Used by the container health check,
// which has no shell tools available to it.
func healthcheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/health", cfg.HTTP.Port)
	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("probing %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}
	return nil
}
