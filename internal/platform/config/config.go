// Package config loads and validates runtime configuration from the
// environment. Configuration is read once at start-up and passed explicitly to
// the components that need it; nothing in this system reads os.Getenv at
// request time, so a running process cannot change behaviour underneath itself.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/swibit/flowed/internal/platform/buildinfo"
)

// Config is the complete runtime configuration.
type Config struct {
	App           App
	HTTP          HTTP
	Database      Database
	Auth          Auth
	Receipt       Receipt
	Payments      Payments
	Notifications Notifications
	AuditArchive  AuditArchive
	Log           Log
	Observability Observability
}

// App holds process-wide identity and behaviour switches.
type App struct {
	// Name identifies the service in logs and health responses.
	Name string
	// Environment is one of development, staging, production. Production
	// tightens several defaults and refuses insecure settings outright.
	Environment string
	// Version is the build version, surfaced by the health endpoint.
	Version string
	// DefaultTimezone renders dates for reports and receipts. Storage is
	// always UTC; this only affects presentation and due-date evaluation.
	DefaultTimezone string
}

// IsProduction reports whether the process runs in production.
func (a App) IsProduction() bool { return a.Environment == "production" }

// HTTP holds server and transport settings.
type HTTP struct {
	Host string
	Port int
	// ReadTimeout bounds reading the request including its body.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing the response. Report endpoints that stream
	// large exports get their own longer deadline via request context.
	WriteTimeout time.Duration
	// IdleTimeout bounds keep-alive connections.
	IdleTimeout time.Duration
	// ShutdownTimeout bounds graceful drain on SIGTERM. In-flight financial
	// transactions must be allowed to commit or roll back cleanly.
	ShutdownTimeout time.Duration
	// MaxRequestBodyBytes caps request bodies, protecting the import endpoints.
	MaxRequestBodyBytes int64
	// TrustedProxies lists proxy CIDRs whose forwarded-for headers are honoured.
	// Empty means client IPs are taken from the socket, which is correct when
	// the service is exposed directly.
	TrustedProxies []string
	// CORSAllowedOrigins lists browser origins permitted to call the API.
	CORSAllowedOrigins []string
	// RateLimitPerMinute caps requests per client per minute; zero disables.
	//
	// The limit is enforced across every replica, not per process, so this is
	// the real figure rather than a figure to divide by the replica count. It
	// is also per client IP, which at a university means per campus NAT: a
	// cashier hall of thirty terminals shares one budget, and the default is
	// sized for that rather than for one operator.
	RateLimitPerMinute int
	// RateLimitShared enables the cross-replica limiter in PostgreSQL. Turning
	// it off falls back to the per-process bucket, which is one round trip
	// cheaper and correct only on a single replica.
	RateLimitShared bool
	// RateLimitIdleTTL is how long an untouched bucket survives before the
	// sweeper deletes it. A bucket refills fully within its window, so one idle
	// for several windows carries no information.
	RateLimitIdleTTL time.Duration
}

// Addr returns the listen address.
func (h HTTP) Addr() string { return fmt.Sprintf("%s:%d", h.Host, h.Port) }

// Database holds PostgreSQL connection and pool settings.
type Database struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
	// MaxConns bounds the pool. Sized against PostgreSQL's own max_connections
	// and the number of API replicas, not against expected concurrency: a
	// financial workload serialises on row locks long before it saturates a
	// large pool.
	MaxConns int32
	// MinConns keeps warm connections ready for the cashier desks.
	MinConns int32
	// MaxConnLifetime recycles connections so a long-lived process does not
	// pin a backend that PostgreSQL wants to retire.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime closes connections idle beyond this window.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod controls background liveness probing of pooled conns.
	HealthCheckPeriod time.Duration
	// ConnectTimeout bounds establishing a new connection.
	ConnectTimeout time.Duration
	// StatementTimeout bounds any single statement server-side. This is the
	// backstop against a runaway report holding locks a cashier needs.
	StatementTimeout time.Duration
	// LockTimeout bounds how long a statement waits for a row lock before
	// failing. Without it, two cashiers on one account can block indefinitely.
	LockTimeout time.Duration
	// IdleInTransactionTimeout kills abandoned open transactions, which would
	// otherwise hold account locks after a client disconnects.
	IdleInTransactionTimeout time.Duration
	// LogQueries emits every statement at debug level. Never enable in
	// production: statements carry student identifiers and amounts.
	LogQueries bool
}

// DSN renders the connection string for pgx.
func (d Database) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s&connect_timeout=%d",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
		int(d.ConnectTimeout.Seconds()),
	)
}

// RedactedDSN renders the connection string with the password masked, for logs.
func (d Database) RedactedDSN() string {
	return fmt.Sprintf("postgres://%s:****@%s:%d/%s?sslmode=%s", d.User, d.Host, d.Port, d.Name, d.SSLMode)
}

// Auth holds token issuance and verification settings.
type Auth struct {
	// JWTSecret signs access tokens. Must be at least 32 bytes in production.
	JWTSecret string
	// RetiredJWTSecrets still verify tokens but sign nothing. A rotation moves
	// the old value here for one refresh-token lifetime, so tokens already in
	// browsers keep working while new ones are signed with the new key. Without
	// it, rotating the secret signs every cashier out mid-shift, which is why
	// in practice it never got rotated at all.
	RetiredJWTSecrets []string
	// AccessTokenTTL bounds how long an access token stays valid. Cashier
	// shifts are long, but a stolen token should expire within the shift.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL bounds the refresh window.
	RefreshTokenTTL time.Duration
	// Issuer identifies this service in issued tokens.
	Issuer string
	// BcryptCost sets password hashing cost.
	BcryptCost int

	// MaxLoginFailures locks an account after this many failures inside
	// LoginFailureWindow. Zero disables per-account lockout, which leaves only
	// the per-IP limiter — and at a university that is a campus NAT shared by
	// a hall of terminals, so it is sized far too high to stop somebody
	// guessing one cashier's password.
	MaxLoginFailures int
	// LoginFailureWindow is how far back failures are counted.
	LoginFailureWindow time.Duration
	// LockoutDuration is how long a locked account stays locked. It expires on
	// its own: an account that needs an administrator to unlock it turns every
	// mistyped password into a support call, and the support call is answered
	// by unlocking it.
	LockoutDuration time.Duration
	// StrictSessionCheck verifies on every request that the session behind the
	// token has not been revoked. On, revocation is immediate at the cost of
	// one indexed lookup per request; off, it lags by up to one access-token
	// lifetime. The trade-off is named here rather than hidden in code.
	StrictSessionCheck bool
	// RequireHSTS adds Strict-Transport-Security to responses that arrived over
	// TLS. Off by default so a development server on plain HTTP cannot pin a
	// browser to a scheme it does not serve.
	RequireHSTS bool
}

// Payments holds the electronic collection channels this deployment offers.
//
// Every provider is off unless configured. A university that collects only at
// the desk configures none of them, and the API then says so plainly rather
// than offering a channel that fails when a student tries it.
type Payments struct {
	// PublicBaseURL is where this deployment is reachable from the internet.
	// Provider callbacks are built from it rather than from the incoming
	// request, because a request through a misconfigured proxy would otherwise
	// send the provider to an address only that proxy can reach.
	PublicBaseURL string
	ZainCash      PaymentProvider
	QiCard        PaymentProvider
	FastPay       PaymentProvider
	Branch        PaymentProvider
}

// PaymentProvider is one provider's settings.
type PaymentProvider struct {
	Enabled  bool
	BaseURL  string
	Merchant string
	APIKey   string
	// CallbackSecret verifies their callbacks to us. Deliberately separate
	// from the API key we send them: a leak of the outbound credential must
	// not let anyone forge a confirmation.
	CallbackSecret string
	Timeout        time.Duration
}

// Notifications holds the delivery channel for reminders.
//
// Off by default. A university with no gateway still gets the worklist, which
// is what the office was working from anyway; queueing messages nothing will
// deliver would be worse than not queueing them.
type Notifications struct {
	SMSEnabled bool
	SMSBaseURL string
	SMSAPIKey  string
	// SMSSender is the alphanumeric identity registered with the operator.
	// Messages from an unregistered sender are dropped silently by the Iraqi
	// networks, which is the most confusing possible failure.
	SMSSender  string
	SMSTimeout time.Duration
}

// AuditArchive is where the audit trail is copied so that whoever can edit the
// database cannot edit the copy.
//
// Off by default, and that is a deliberate default rather than a safe one: an
// archive misconfigured to a directory on the same disk is worse than none,
// because it looks like a control. The deployment has to name a destination
// the application's own host cannot rewrite.
type AuditArchive struct {
	// Dir is a directory — in practice a mount of one on another machine.
	Dir string
	// Endpoint is an append-only HTTP destination, used when Dir is empty.
	Endpoint string
	// Secret signs what is posted to Endpoint. Required with it: an unsigned
	// append endpoint accepts entries from anyone who finds the URL.
	Secret string
	// Batch is how many entries go in one block.
	Batch int
	// Interval is how often the shipping job runs. Frequent: the window
	// between an entry being written and being witnessed is the window in
	// which it can be removed without trace.
	Interval time.Duration
	Timeout  time.Duration
}

// Enabled reports whether a destination was configured.
func (a AuditArchive) Enabled() bool { return a.Dir != "" || a.Endpoint != "" }

// Receipt holds what a printed receipt says about the institution issuing it.
//
// Configuration rather than constants: one binary should serve any university,
// and the name on the letterhead is not something to compile in.
type Receipt struct {
	UniversityNameAr string
	CollegeNameAr    string
	Address          string
	Phone            string
	// LogoDataURI inlines a logo. Inlined rather than linked because a receipt
	// must print identically from a desk with no network.
	LogoDataURI string
}

// Observability holds the metric and trace pipeline settings.
//
// Metrics default to on and traces default to off, which matches what the two
// cost to operate. A scrape endpoint needs nothing running to be useful; a
// trace exporter with no collector in front of it produces retry noise and
// nothing else.
type Observability struct {
	// MetricsEnabled exposes the Prometheus scrape endpoint.
	MetricsEnabled bool
	// MetricsAddr is the listener for that endpoint. Loopback by default: the
	// scrape surface names every route and its error rate, which is a map of
	// the system that does not belong on the public interface. A deployment
	// scraped from another host sets this deliberately.
	MetricsAddr string
	// MetricsPath is the path on that listener.
	MetricsPath string
	// TracingEnabled turns on span export. Off unless a collector exists.
	TracingEnabled bool
	// OTLPEndpoint is the collector's base URL. HTTP rather than gRPC: it goes
	// through an ordinary reverse proxy, which is what a university network
	// actually has between the application host and anything else.
	OTLPEndpoint string
	// TraceSampleRatio is the fraction of traces kept, applied at the root and
	// inherited by children so no trace is ever half-recorded.
	TraceSampleRatio float64
	// ShutdownTimeout bounds the final flush of both pipelines.
	ShutdownTimeout time.Duration
}

// Log holds logging settings.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is json or text.
	Format string
	// IncludeSource attaches file and line to each record.
	IncludeSource bool
}

// Load reads configuration from the environment, applies defaults, and
// validates the result. It returns every validation problem at once rather
// than failing on the first, so a misconfigured deployment is fixed in one pass.
func Load() (*Config, error) {
	cfg := &Config{
		App: App{
			Name:        env("APP_NAME", "flowed-tuition"),
			Environment: env("APP_ENV", "development"),
			// The linked-in build identity is the default. APP_VERSION exists
			// only to let a container image label override it; it is not a way
			// to invent a version for a binary that has none, because the
			// production check below inspects the resolved value either way.
			Version:         env("APP_VERSION", buildinfo.Version()),
			DefaultTimezone: env("APP_TIMEZONE", "Asia/Baghdad"),
		},
		HTTP: HTTP{
			Host:                env("HTTP_HOST", "0.0.0.0"),
			Port:                envInt("HTTP_PORT", 8080),
			ReadTimeout:         envDuration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:        envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:         envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout:     envDuration("HTTP_SHUTDOWN_TIMEOUT", 30*time.Second),
			MaxRequestBodyBytes: int64(envInt("HTTP_MAX_BODY_BYTES", 32<<20)),
			TrustedProxies:      envList("HTTP_TRUSTED_PROXIES", nil),
			CORSAllowedOrigins:  envList("HTTP_CORS_ORIGINS", []string{"*"}),
			RateLimitPerMinute:  envInt("HTTP_RATE_LIMIT_PER_MINUTE", 600),
			RateLimitShared:     envBool("HTTP_RATE_LIMIT_SHARED", true),
			RateLimitIdleTTL:    envDuration("HTTP_RATE_LIMIT_IDLE_TTL", 10*time.Minute),
		},
		Database: Database{
			Host:                     env("DB_HOST", "localhost"),
			Port:                     envInt("DB_PORT", 5432),
			User:                     env("DB_USER", "postgres"),
			Password:                 env("DB_PASSWORD", "postgres"),
			Name:                     env("DB_NAME", "flowed"),
			SSLMode:                  env("DB_SSLMODE", "disable"),
			MaxConns:                 int32(envInt("DB_MAX_CONNS", 25)),
			MinConns:                 int32(envInt("DB_MIN_CONNS", 5)),
			MaxConnLifetime:          envDuration("DB_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime:          envDuration("DB_MAX_CONN_IDLE_TIME", 30*time.Minute),
			HealthCheckPeriod:        envDuration("DB_HEALTH_CHECK_PERIOD", time.Minute),
			ConnectTimeout:           envDuration("DB_CONNECT_TIMEOUT", 10*time.Second),
			StatementTimeout:         envDuration("DB_STATEMENT_TIMEOUT", 30*time.Second),
			LockTimeout:              envDuration("DB_LOCK_TIMEOUT", 10*time.Second),
			IdleInTransactionTimeout: envDuration("DB_IDLE_IN_TX_TIMEOUT", 60*time.Second),
			LogQueries:               envBool("DB_LOG_QUERIES", false),
		},
		Auth: Auth{
			JWTSecret:          env("AUTH_JWT_SECRET", "development-secret-change-me-in-production"),
			RetiredJWTSecrets:  envList("AUTH_JWT_RETIRED_SECRETS", nil),
			AccessTokenTTL:     envDuration("AUTH_ACCESS_TOKEN_TTL", 12*time.Hour),
			RefreshTokenTTL:    envDuration("AUTH_REFRESH_TOKEN_TTL", 30*24*time.Hour),
			Issuer:             env("AUTH_ISSUER", "flowed-tuition"),
			BcryptCost:         envInt("AUTH_BCRYPT_COST", 12),
			MaxLoginFailures:   envInt("AUTH_MAX_LOGIN_FAILURES", 8),
			LoginFailureWindow: envDuration("AUTH_LOGIN_FAILURE_WINDOW", 15*time.Minute),
			LockoutDuration:    envDuration("AUTH_LOCKOUT_DURATION", 15*time.Minute),
			StrictSessionCheck: envBool("AUTH_STRICT_SESSION_CHECK", true),
			RequireHSTS:        envBool("AUTH_REQUIRE_HSTS", false),
		},
		Payments: Payments{
			PublicBaseURL: env("PUBLIC_BASE_URL", "http://localhost:8080"),
			ZainCash:      providerConfig("ZAINCASH"),
			QiCard:        providerConfig("QI"),
			FastPay:       providerConfig("FASTPAY"),
			Branch:        providerConfig("BRANCH"),
		},
		Notifications: Notifications{
			SMSEnabled: envBool("NOTIFY_SMS_ENABLED", false),
			SMSBaseURL: env("NOTIFY_SMS_BASE_URL", ""),
			SMSAPIKey:  env("NOTIFY_SMS_API_KEY", ""),
			SMSSender:  env("NOTIFY_SMS_SENDER", ""),
			SMSTimeout: envDuration("NOTIFY_SMS_TIMEOUT", 15*time.Second),
		},
		AuditArchive: AuditArchive{
			Dir:      env("AUDIT_ARCHIVE_DIR", ""),
			Endpoint: env("AUDIT_ARCHIVE_URL", ""),
			Secret:   env("AUDIT_ARCHIVE_SECRET", ""),
			Batch:    envInt("AUDIT_ARCHIVE_BATCH", 500),
			Interval: envDuration("AUDIT_ARCHIVE_INTERVAL", 15*time.Minute),
			Timeout:  envDuration("AUDIT_ARCHIVE_TIMEOUT", 30*time.Second),
		},
		Receipt: Receipt{
			UniversityNameAr: env("RECEIPT_UNIVERSITY_NAME", "الجامعة"),
			CollegeNameAr:    env("RECEIPT_COLLEGE_NAME", ""),
			Address:          env("RECEIPT_ADDRESS", ""),
			Phone:            env("RECEIPT_PHONE", ""),
			LogoDataURI:      env("RECEIPT_LOGO_DATA_URI", ""),
		},
		Log: Log{
			Level:         env("LOG_LEVEL", "info"),
			Format:        env("LOG_FORMAT", "json"),
			IncludeSource: envBool("LOG_INCLUDE_SOURCE", false),
		},
		Observability: Observability{
			MetricsEnabled: envBool("OBS_METRICS_ENABLED", true),
			// 9464 is the port the OpenTelemetry Prometheus exporter is
			// conventionally scraped on, which saves an argument later.
			MetricsAddr:      env("OBS_METRICS_ADDR", "127.0.0.1:9464"),
			MetricsPath:      env("OBS_METRICS_PATH", "/metrics"),
			TracingEnabled:   envBool("OBS_TRACING_ENABLED", false),
			OTLPEndpoint:     env("OBS_OTLP_ENDPOINT", "http://localhost:4318"),
			TraceSampleRatio: envFloat("OBS_TRACE_SAMPLE_RATIO", 0.05),
			ShutdownTimeout:  envDuration("OBS_SHUTDOWN_TIMEOUT", 5*time.Second),
		},
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the configuration for internal consistency and for settings
// that are merely unwise in development but unacceptable in production.
func (c *Config) Validate() error {
	var problems []string

	switch c.App.Environment {
	case "development", "staging", "production":
	default:
		problems = append(problems, fmt.Sprintf("APP_ENV must be development, staging or production (got %q)", c.App.Environment))
	}
	if _, err := time.LoadLocation(c.App.DefaultTimezone); err != nil {
		problems = append(problems, fmt.Sprintf("APP_TIMEZONE %q is not a known IANA zone", c.App.DefaultTimezone))
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		problems = append(problems, fmt.Sprintf("HTTP_PORT %d is out of range", c.HTTP.Port))
	}
	if c.Database.Name == "" {
		problems = append(problems, "DB_NAME is required")
	}
	if c.Database.MaxConns < 1 {
		problems = append(problems, "DB_MAX_CONNS must be at least 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		problems = append(problems, fmt.Sprintf("DB_MIN_CONNS (%d) must be between 0 and DB_MAX_CONNS (%d)", c.Database.MinConns, c.Database.MaxConns))
	}
	if c.Database.LockTimeout >= c.Database.StatementTimeout {
		problems = append(problems, "DB_LOCK_TIMEOUT must be shorter than DB_STATEMENT_TIMEOUT so lock waits fail with a precise error")
	}
	if c.Auth.BcryptCost < 10 || c.Auth.BcryptCost > 15 {
		problems = append(problems, fmt.Sprintf("AUTH_BCRYPT_COST %d is outside the sane range 10..15", c.Auth.BcryptCost))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("LOG_LEVEL must be debug, info, warn or error (got %q)", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		problems = append(problems, fmt.Sprintf("LOG_FORMAT must be json or text (got %q)", c.Log.Format))
	}

	if c.Observability.TraceSampleRatio < 0 || c.Observability.TraceSampleRatio > 1 {
		problems = append(problems, fmt.Sprintf(
			"OBS_TRACE_SAMPLE_RATIO must be between 0 and 1 (got %v)", c.Observability.TraceSampleRatio))
	}
	if c.Observability.TracingEnabled && c.Observability.OTLPEndpoint == "" {
		problems = append(problems, "OBS_OTLP_ENDPOINT is required when OBS_TRACING_ENABLED is set")
	}
	// pgx accepts one tracer per connection, so the query logger and the span
	// tracer cannot both be installed. Refusing here is better than silently
	// dropping one: an operator who turned on query logging and saw no queries
	// would spend the afternoon looking at the wrong thing.
	if c.Observability.TracingEnabled && c.Database.LogQueries {
		problems = append(problems,
			"DB_LOG_QUERIES and OBS_TRACING_ENABLED cannot both be set: pgx takes a single tracer, "+
				"and query logging is the development substitute for the spans tracing already gives you")
	}
	if c.Observability.MetricsEnabled && c.Observability.MetricsAddr == c.HTTP.Addr() {
		problems = append(problems,
			"OBS_METRICS_ADDR must not be the API's own listen address; the scrape endpoint gets its own listener")
	}

	if c.App.IsProduction() {
		// A production binary that cannot say which build it is cannot be
		// rolled back to a known-good predecessor, and every log line, metric
		// and audit entry it writes carries a version that means nothing. The
		// release build stamps this; a binary reaching production without it
		// was not built by the pipeline.
		if !(buildinfo.Info{Version: c.App.Version}).IsIdentified() {
			problems = append(problems, fmt.Sprintf(
				"this build reports version %q, which identifies nothing: build with `make build` "+
					"(which stamps VERSION and the commit) or set APP_VERSION to the released version",
				c.App.Version))
		}
		if len(c.Auth.JWTSecret) < 32 {
			problems = append(problems, "AUTH_JWT_SECRET must be at least 32 bytes in production")
		}
		if strings.Contains(c.Auth.JWTSecret, "development") {
			problems = append(problems, "AUTH_JWT_SECRET still holds the development default")
		}
		if c.Database.SSLMode == "disable" {
			problems = append(problems, "DB_SSLMODE must not be disable in production")
		}
		if c.Database.LogQueries {
			problems = append(problems, "DB_LOG_QUERIES must be off in production: statements carry student identifiers and amounts")
		}
		if len(c.HTTP.CORSAllowedOrigins) == 1 && c.HTTP.CORSAllowedOrigins[0] == "*" {
			problems = append(problems, "HTTP_CORS_ORIGINS must list explicit origins in production")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// providerConfig reads one payment provider's settings.
//
// Prefixed by the provider's own code so a deployment adding a second provider
// adds variables rather than editing the ones the first is using.
func providerConfig(code string) PaymentProvider {
	return PaymentProvider{
		Enabled:        envBool("PAY_"+code+"_ENABLED", false),
		BaseURL:        env("PAY_"+code+"_BASE_URL", ""),
		Merchant:       env("PAY_"+code+"_MERCHANT_ID", ""),
		APIKey:         env("PAY_"+code+"_API_KEY", ""),
		CallbackSecret: env("PAY_"+code+"_CALLBACK_SECRET", ""),
		Timeout:        envDuration("PAY_"+code+"_TIMEOUT", 20*time.Second),
	}
}

// ErrMissing reports a required variable with no value and no default.
var ErrMissing = errors.New("required environment variable is not set")

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(key string, fallback float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
