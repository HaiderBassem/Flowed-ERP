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

	"github.com/swibit/flowed/internal/bootstrap"
	"github.com/swibit/flowed/internal/platform/buildinfo"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/migrate"
	"github.com/swibit/flowed/internal/platform/observability"
	"github.com/swibit/flowed/internal/platform/pg"
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

	engine, scheduler := bootstrap.BuildEngine(cfg, log, db, obs, version)

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
