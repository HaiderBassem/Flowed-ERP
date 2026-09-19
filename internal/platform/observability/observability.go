// Package observability builds the metric and trace pipelines and hands out
// the instruments the rest of the process records against.
//
// Three decisions shape everything below.
//
// # Metrics are pulled, traces are pushed
//
// Metrics are exposed for a Prometheus scrape, which needs nothing running
// between this process and the dashboard — a university IT department can keep
// that alive. Traces are only legible with a collector in front of them, so
// tracing is off unless somebody has stood one up, and the trace pipeline
// degrades to a no-op rather than to an error when they have not.
//
// # Nothing that identifies a student may become a label
//
// A metric label is retained for as long as its time series lives, in a store
// with none of the access control the database has, and behind a dashboard
// nobody audits. A student number, a name or a receipt number on that surface
// is a disclosure that deleting the series afterwards does not undo. Every
// label set in this package is fixed and low-cardinality by construction —
// route templates, method codes, statuses, job names. Never an id, never a
// name, and never an amount as a label: amounts are what a counter *measures*,
// which aggregates, rather than what it is keyed by, which multiplies.
//
// # The metrics endpoint is not on the public listener
//
// It gets its own listener, bound to loopback by default. The scrape surface
// names every route the API has and how often each fails, which is a map of
// the system worth withholding from the internet, and it needs no
// authentication precisely because nothing outside the host can reach it. A
// containerised deployment that must be scraped from another pod overrides the
// bind address deliberately.
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"flowed/internal/platform/config"
)

// instrumentationScope names this codebase in every metric and span it emits,
// which is how a backend tells our instrumentation from a library's.
const instrumentationScope = "flowed"

// Provider owns the pipelines for the life of the process.
//
// A nil *Provider is usable: every method on it is a no-op, and Instruments
// returns the nil *Metrics, which is itself safe to record against. That is
// what lets a test, a one-shot CLI command, or a deployment with observability
// switched off run the same code paths as the server without a single guard at
// the call site.
type Provider struct {
	metrics *Metrics
	tracer  trace.Tracer

	registry       *prometheus.Registry
	meterProvider  *sdkmetric.MeterProvider
	tracerProvider *sdktrace.TracerProvider

	metricsPath string
	log         *slog.Logger
}

// Setup builds the pipelines described by cfg.
//
// It never fails because a collector is unreachable. The OTLP exporter
// connects lazily and retries in the background, so a collector that is down
// at boot costs a few dropped spans rather than a service that will not start:
// a cashier desk must not be blocked from taking money because a tracing
// backend is being upgraded.
func Setup(ctx context.Context, cfg config.Observability, app config.App, version string, log *slog.Logger) (*Provider, error) {
	log = log.With(slog.String("component", "observability"))

	p := &Provider{
		tracer:      noop.NewTracerProvider().Tracer(instrumentationScope),
		metricsPath: cfg.MetricsPath,
		log:         log,
	}

	// Anything the SDK cannot deliver — a rejected export, a malformed
	// attribute — arrives here. Without this it goes to stderr unstructured,
	// bypassing the very log pipeline that would surface it.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry pipeline error", slog.String("error", err.Error()))
	}))

	res, err := buildResource(app, version)
	if err != nil {
		return nil, err
	}

	if cfg.MetricsEnabled {
		if err := p.startMetrics(res); err != nil {
			return nil, err
		}
	}

	if cfg.TracingEnabled {
		if err := p.startTracing(ctx, cfg, res); err != nil {
			return nil, err
		}
	}

	log.Info("observability configured",
		slog.Bool("metrics", cfg.MetricsEnabled),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.Bool("tracing", cfg.TracingEnabled),
		slog.String("otlp_endpoint", cfg.OTLPEndpoint),
		slog.Float64("trace_sample_ratio", cfg.TraceSampleRatio))

	return p, nil
}

// buildResource describes this process to every backend it reports to.
func buildResource(app config.App, version string) (*resource.Resource, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(app.Name),
			semconv.ServiceVersion(version),
			semconv.DeploymentEnvironmentNameKey.String(app.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("building telemetry resource: %w", err)
	}
	return res, nil
}

func (p *Provider) startMetrics(res *resource.Resource) error {
	registry := prometheus.NewRegistry()

	// Go runtime and process collectors come from client_golang rather than
	// from the OTel SDK: they are the ones a Go engineer already has
	// dashboards and alert rules for, and they cost nothing to register.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		// The target_info series repeats the resource attributes on every
		// scrape. They are already on this process's job labels in any sane
		// Prometheus configuration, so it is pure duplication.
		otelprom.WithoutTargetInfo(),
	)
	if err != nil {
		return fmt.Errorf("building prometheus exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(exporter),
		// Latency buckets are set here rather than left to the SDK default,
		// which spans milliseconds to a hundred seconds. Every request this
		// API serves that is not broken finishes inside two seconds, and the
		// default buckets put almost all of them in one bar.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			}},
		)),
	)

	otel.SetMeterProvider(provider)

	metrics, err := newMetrics(provider.Meter(instrumentationScope))
	if err != nil {
		return err
	}

	p.registry = registry
	p.meterProvider = provider
	p.metrics = metrics
	return nil
}

func (p *Provider) startTracing(ctx context.Context, cfg config.Observability, res *resource.Resource) error {
	endpoint, err := traceEndpoint(cfg.OTLPEndpoint)
	if err != nil {
		return err
	}

	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return fmt.Errorf("building OTLP trace exporter: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
		// Parent-based so a sampling decision taken at the edge holds for the
		// whole trace. Sampling each span independently produces traces with
		// holes in them, which are worse than no trace at all: an operator
		// reads a missing span as work that did not happen.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio))),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	p.tracerProvider = provider
	p.tracer = provider.Tracer(instrumentationScope)
	return nil
}

// Instruments returns the recorders. Safe on a nil Provider, and the *Metrics
// it returns is safe to record against whether or not metrics are enabled.
func (p *Provider) Instruments() *Metrics {
	if p == nil {
		return nil
	}
	return p.metrics
}

// Tracer returns the tracer, which is a no-op tracer when tracing is off.
func (p *Provider) Tracer() trace.Tracer {
	if p == nil || p.tracer == nil {
		return noop.NewTracerProvider().Tracer(instrumentationScope)
	}
	return p.tracer
}

// TracingEnabled reports whether spans are actually exported. Callers use it
// to decide whether to install instrumentation that costs something even when
// nothing is sampled, such as the per-query database tracer.
func (p *Provider) TracingEnabled() bool {
	return p != nil && p.tracerProvider != nil
}

// MetricsHandler serves the scrape endpoint, or nil when metrics are off.
func (p *Provider) MetricsHandler() http.Handler {
	if p == nil || p.registry == nil {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle(p.metricsPath, promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{
		// A scrape that fails should say so in the response and in our own
		// log, not panic the handler and take the listener with it.
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      slog.NewLogLogger(p.log.Handler(), slog.LevelWarn),
		// One scrape at a time. Two concurrent collections of the same
		// registry double the work for an identical answer.
		MaxRequestsInFlight: 1,
		Timeout:             10 * time.Second,
	}))
	return mux
}

// Shutdown flushes both pipelines.
//
// It runs after the HTTP server has drained, so the spans and samples produced
// by the last requests in flight are exported rather than dropped — the tail
// of a shutdown is exactly where the interesting latency lives.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}

	var errs []error
	if p.tracerProvider != nil {
		if err := p.tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("flushing traces: %w", err))
		}
	}
	if p.meterProvider != nil {
		if err := p.meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("flushing metrics: %w", err))
		}
	}
	return errors.Join(errs...)
}

// otlpTracePath is where the OTLP/HTTP specification puts the trace endpoint.
const otlpTracePath = "/v1/traces"

// traceEndpoint completes a collector base URL.
//
// The exporter takes the URL exactly as given, path included, so a bare
// http://collector:4318 posts to "/" and every export is rejected by a
// collector that is running perfectly well. The failure is quiet — spans
// simply never arrive — so the signal path is completed here and an endpoint
// that already names one is left alone.
func traceEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing OTLP endpoint %q: %w", raw, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("OTLP endpoint %q must be an absolute URL, for example http://collector:4318", raw)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = otlpTracePath
	}
	return parsed.String(), nil
}

// attrs is shorthand for the option every instrument call needs.
func attrs(kv ...attribute.KeyValue) attribute.Set { return attribute.NewSet(kv...) }
