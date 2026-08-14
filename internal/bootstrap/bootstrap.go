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

	"github.com/swibit/flowed/internal/adapter/auditship"
	"github.com/swibit/flowed/internal/adapter/httpapi"
	"github.com/swibit/flowed/internal/adapter/messaging"
	"github.com/swibit/flowed/internal/adapter/payments"
	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/adapter/receipt"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/observability"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
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
	sessions := postgres.NewCashierSessionRepository(db)
	audit := postgres.NewAuditRepository(db)
	lifecycle := postgres.NewLifecycleRepository(db)
	settlements := postgres.NewSettlementRepository(db)
	sponsors := postgres.NewSponsorRepository(db)
	verifications := postgres.NewVerificationRepository(db)
	intents := postgres.NewIntentRepository(db)
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
	studentService := app.NewStudentService(deps)
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
	cashierService := app.NewCashierService(deps)
	masterDataService := app.NewMasterDataService(deps)
	settlementService := app.NewSettlementService(deps, settlements)
	sponsorService := app.NewSponsorService(deps, sponsors)
	accountService.WithSponsors(sponsorService)
	portalService := app.NewPortalService(deps, hasher, verifications, sponsorService)

	// A deployment with no SMS gateway gets nil here, and the notification
	// service then produces a worklist instead of queueing deliveries nothing
	// will perform.
	var deliverer port.Deliverer
	if gateway := messaging.NewSMSGateway(messaging.Config{
		Enabled: cfg.Notifications.SMSEnabled,
		BaseURL: cfg.Notifications.SMSBaseURL,
		APIKey:  cfg.Notifications.SMSAPIKey,
		Sender:  cfg.Notifications.SMSSender,
		Timeout: cfg.Notifications.SMSTimeout,
	}); gateway != nil {
		deliverer = gateway
	}
	notifyService := app.NewNotifyService(
		deps, postgres.NewNotificationRepository(db), deliverer, cfg.Receipt.UniversityNameAr)

	// Only the providers this deployment configured are registered. A channel
	// that is off is absent rather than half-present, so the API can say "not
	// offered here" instead of failing when a student tries it.
	intentService := app.NewIntentService(deps, intents, buildProviderRegistry(cfg.Payments), paymentService)

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

	// Defaults come from the scheduler itself; only a deployment with a reason
	// to differ overrides them.
	scheduler := app.NewScheduler(deps, db, idempotency, rateLimiter, app.SchedulerConfig{
		RateLimitIdleTTL:  cfg.HTTP.RateLimitIdleTTL,
		Sessions:          authSessions,
		LoginAttempts:     loginAttempts,
		Notify:            notifyService,
		Intents:           intentService,
		AuditShip:         auditShipper,
		AuditShipInterval: cfg.AuditArchive.Interval,
	})

	engine := httpapi.NewRouter(httpapi.RouterDeps{
		Config:        cfg,
		Log:           log,
		DB:            db,
		Handlers:      handlers,
		Auth:          httpapi.NewAuthHandlers(authService, userService, log),
		UserAdmin:     httpapi.NewUserHandlers(userService),
		Lifecycle:     httpapi.NewLifecycleHandlers(enrollmentService, studentService, accountService),
		MasterData:    httpapi.NewMasterDataHandlers(masterDataService),
		Settlement:    httpapi.NewSettlementHandlers(settlementService),
		Sponsors:      httpapi.NewSponsorHandlers(sponsorService),
		Portal:        httpapi.NewPortalHandlers(portalService),
		Intents:       httpapi.NewIntentHandlers(intentService, cfg.Payments.PublicBaseURL, log),
		AuthService:   authService,
		Users:         users,
		Reports:       httpapi.NewReportHandlers(reports),
		ConfigAdmin:   httpapi.NewConfigHandlers(app.NewConfigService(deps)),
		Bulk:          httpapi.NewBulkHandlers(bulkService, importService, imports),
		Cashier:       httpapi.NewCashierHandlers(cashierService, masterDataService, db),
		AuditArchive:  httpapi.NewAuditArchiveHandlers(auditShipper),
		Receipts:      httpapi.NewReceiptHandlers(receiptService),
		Tokens:        tokens,
		Idempotency:   idempotency,
		RateLimiter:   rateLimiter,
		Observability: obs,
		Version:       buildVersion,
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

// buildProviderRegistry assembles the electronic collection channels this
// deployment has switched on.
//
// Branch collection is registered whenever a callback secret exists, because it
// needs nothing else: the reference it mints is derived from that secret and
// the bank statement is its confirmation.
func buildProviderRegistry(cfg config.Payments) *payments.Registry {
	var providers []payments.Provider

	if cfg.ZainCash.Enabled {
		providers = append(providers, payments.NewZainCash(toProviderConfig(cfg.ZainCash)))
	}
	if cfg.QiCard.Enabled {
		providers = append(providers, payments.NewQiCard(toProviderConfig(cfg.QiCard)))
	}
	if cfg.FastPay.Enabled {
		providers = append(providers, payments.NewFastPay(toProviderConfig(cfg.FastPay)))
	}
	if cfg.Branch.Enabled {
		providers = append(providers, payments.NewBranchCollection(toProviderConfig(cfg.Branch)))
	}
	return payments.NewRegistry(providers...)
}

func toProviderConfig(p config.PaymentProvider) payments.Config {
	return payments.Config{
		Enabled:        p.Enabled,
		BaseURL:        p.BaseURL,
		MerchantID:     p.Merchant,
		APIKey:         p.APIKey,
		CallbackSecret: p.CallbackSecret,
		Timeout:        p.Timeout,
	}
}

// Handlers is an alias so buildEngine reads without repeating the package
// qualifier on every field.
type Handlers = httpapi.Handlers
