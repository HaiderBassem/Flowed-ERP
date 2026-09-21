// Package bootstrap wires the whole system together.
//
// This is the only place in the process where concrete implementations meet
// the interfaces they satisfy, and it lives in its own package so that the
// server and the end-to-end tests build the same engine. When the wiring lived
// in main, a test could either reimplement it — and then test a system nobody
// runs — or not exist. Both were what the end-to-end gap looked like.
package bootstrap

import (
	"log/slog"
	"net/http"
	"time"

	"flowed/internal/adapter/auditship"
	"flowed/internal/adapter/backup"
	"flowed/internal/adapter/httpapi"
	"flowed/internal/adapter/postgres"
	"flowed/internal/adapter/receipt"
	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/platform/config"
	"flowed/internal/platform/httpx"
	"flowed/internal/platform/observability"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// buildEngine wires every layer. This is the only place in the process where
// concrete implementations meet the interfaces they satisfy.
func BuildEngine(
	cfg *config.Config, log *slog.Logger, db *pg.DB, obs *observability.Provider, buildVersion string,
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
	audit := postgres.NewAuditRepository(db)
	lifecycle := postgres.NewLifecycleRepository(db)
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
		Audit:        audit,
		Users:        users,
		Lifecycle:    lifecycle,
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
	studentService := app.NewStudentService(deps).WithPlacement(enrollmentService, accountService)
	paymentService := app.NewPaymentService(deps)

	handlers := &Handlers{
		Students:        studentService,
		Enrollments:     enrollmentService,
		Accounts:        accountService,
		Payments:        paymentService,
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
	masterDataService := app.NewMasterDataService(deps)

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

	// The audit archive is off unless a destination was named. A university
	// that has not set one keeps a hash chain that detects editing and cannot
	// detect deletion, and it should hear that at every start-up rather than
	// find out during an investigation.
	auditShipper := BuildAuditShipper(cfg, deps, db, log)

	reconciliation := app.NewReconciliationService(deps,
		postgres.NewReconciliationRepository(db), app.ReconciliationConfig{})

	maintenance := httpx.NewMaintenanceGate()
	backupRunner := backup.NewRunner(cfg.Database, cfg.Backup.MaintenanceDatabase, cfg.Backup.Timeout)
	backupService := app.NewBackupService(deps, postgres.NewBackupRepository(db), backupRunner, db, maintenance,
		app.BackupServiceConfig{
			Dir:              cfg.Backup.Dir,
			LiveDatabase:     cfg.Database.Name,
			RetentionMinKeep: cfg.Backup.RetentionMinKeep,
		})

	// Built from the defaults rather than from a bare literal, and the
	// difference is not cosmetic. withDefaults can fill an interval that
	// arrives zero, but it cannot fill a bool: the literal that used to be here
	// left RunOnStart false, so every job waited a full interval before its
	// first pass — and the daily ones, reconciliation among them, never ran at
	// all on a service redeployed more often than once a day. The system had no
	// opinion about its own correctness and nothing said so.
	schedulerCfg := app.DefaultSchedulerConfig()
	schedulerCfg.RateLimitIdleTTL = cfg.HTTP.RateLimitIdleTTL
	schedulerCfg.Sessions = authSessions
	schedulerCfg.LoginAttempts = loginAttempts
	schedulerCfg.Reconcile = reconciliation
	schedulerCfg.AuditShip = auditShipper
	schedulerCfg.AuditShipInterval = cfg.AuditArchive.Interval
	schedulerCfg.Backup = backupService

	scheduler := app.NewScheduler(deps, db, idempotency, rateLimiter, schedulerCfg)

	engine := httpapi.NewRouter(httpapi.RouterDeps{
		Config:         cfg,
		Log:            log,
		DB:             db,
		Handlers:       handlers,
		Auth:           httpapi.NewAuthHandlers(authService, userService, log),
		UserAdmin:      httpapi.NewUserHandlers(userService),
		Lifecycle:      httpapi.NewLifecycleHandlers(enrollmentService, studentService, accountService),
		MasterData:     httpapi.NewMasterDataHandlers(masterDataService),
		AuthService:    authService,
		Users:          users,
		Reports:        httpapi.NewReportHandlers(reports),
		ConfigAdmin:    httpapi.NewConfigHandlers(app.NewConfigService(deps)),
		Bulk:           httpapi.NewBulkHandlers(bulkService, importService, imports),
		AuditArchive:   httpapi.NewAuditArchiveHandlers(auditShipper),
		Reconciliation: httpapi.NewReconciliationHandlers(reconciliation),
		Backups:        httpapi.NewBackupHandlers(backupService),
		Maintenance:    maintenance,
		Receipts:       httpapi.NewReceiptHandlers(receiptService),
		Tokens:         tokens,
		Idempotency:    idempotency,
		RateLimiter:    rateLimiter,
		Observability:  obs,
		Version:        buildVersion,
	})

	return engine, scheduler
}

// BuildAuditShipper builds the off-host audit archive, or nothing.
//
// Nothing is a legitimate configuration — a small deployment may genuinely have
// nowhere to ship to — but it is a weaker system than one with an archive, and
// the log says which one is running. The service is nil in that case rather
// than a stub that silently succeeds: a shipper that reports success while
// writing nowhere is the worst of the three states.
func BuildAuditShipper(cfg *config.Config, deps app.Deps, db *pg.DB, log *slog.Logger) *app.AuditShipService {
	if !cfg.AuditArchive.Enabled() {
		log.Warn("the audit trail is not copied off this host",
			slog.String("consequence",
				"the hash chain detects an altered entry but not a deleted one"),
			slog.String("remedy", "set AUDIT_ARCHIVE_DIR or AUDIT_ARCHIVE_URL"))
		return nil
	}

	var (
		sink auditship.Sink
		err  error
	)
	if cfg.AuditArchive.Dir != "" {
		sink, err = auditship.NewDirSink(cfg.AuditArchive.Dir)
	} else {
		sink, err = auditship.NewHTTPSink(
			cfg.AuditArchive.Endpoint, cfg.AuditArchive.Secret, cfg.AuditArchive.Timeout)
	}
	if err != nil {
		// Not fatal: a university whose archive host is misconfigured should
		// still be able to take money this morning. It is loud, and the
		// shipping metric stays at zero, which is what an alert watches.
		log.Error("the audit archive is configured but unusable",
			slog.String("error", err.Error()))
		return nil
	}

	log.Info("audit trail is copied off-host", slog.String("destination", sink.Name()))
	return app.NewAuditShipService(deps, postgres.NewAuditShipmentRepository(db), sink,
		app.AuditShipConfig{Batch: cfg.AuditArchive.Batch})
}

// Handlers is an alias so buildEngine reads without repeating the package
// qualifier on every field.
type Handlers = httpapi.Handlers
