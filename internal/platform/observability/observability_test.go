package observability_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/platform/config"
	"flowed/internal/platform/logger"
	"flowed/internal/platform/observability"
)

func newProvider(t *testing.T) *observability.Provider {
	t.Helper()
	p, err := observability.Setup(context.Background(),
		config.Observability{MetricsEnabled: true, MetricsPath: "/metrics"},
		config.App{Name: "flowed-tuition", Environment: "test"},
		"v-test", logger.NewNop())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}

func scrape(t *testing.T, p *observability.Provider) string {
	t.Helper()
	handler := p.MetricsHandler()
	if handler == nil {
		t.Fatal("no metrics handler; the provider was built with metrics enabled")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("scrape returned %d", rec.Code)
	}
	return rec.Body.String()
}

// requireSeries asserts that a series exists carrying every given label.
// Labels are matched individually because the exporter emits them in
// alphabetical order, with its own otel_scope_* labels interleaved.
func requireSeries(t *testing.T, body, name string, labels ...string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		missing := false
		for _, label := range labels {
			if !strings.Contains(line, label) {
				missing = true
				break
			}
		}
		if !missing {
			return
		}
	}
	t.Errorf("no series %q carrying %v in the scrape", name, labels)
}

// The money counters are the reason this package exists, so their names and
// their values are pinned. A renamed series is a silently broken dashboard.
func TestScrapeExposesTheMoneyCounters(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	m := p.Instruments()

	m.PaymentPosted(ctx, "CASH", money.FromInt64(250_000))
	m.PaymentPosted(ctx, "CASH", money.FromInt64(750_000))
	m.PaymentVoided(ctx, money.FromInt64(50_000))
	m.RefundPosted(ctx, money.FromInt64(30_000))
	m.AccountGenerated(ctx, money.FromInt64(1_200_000))
	m.DiscountApplied(ctx, "active", money.FromInt64(100_000))

	body := scrape(t, p)

	requireSeries(t, body, "flowed_payment_posted_total", `method_code="CASH"`)
	requireSeries(t, body, "flowed_payment_voided_total")
	requireSeries(t, body, "flowed_refund_posted_total")
	requireSeries(t, body, "flowed_account_generated_total")
	requireSeries(t, body, "flowed_account_obligation_IQD_total")
	requireSeries(t, body, "flowed_discount_applied_total", `status="active"`)

	// The currency is part of the series name, which is what stops a dinar
	// counter from ever being read as anything else.
	requireSeries(t, body, "flowed_payment_amount_IQD_total", `method_code="CASH"`)

	// Two payments of 250,000 and 750,000.
	if !strings.Contains(body, "} 1e+06") && !strings.Contains(body, "} 1000000") {
		t.Error("the collected total does not sum the two payments")
	}
}

// A signed adjustment is recorded as a magnitude with a direction, because a
// counter that can go down is not a counter and rate() over one reports
// nonsense on every credit.
func TestAdjustmentsAreCountedByDirectionNotSign(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()

	p.Instruments().AdjustmentPosted(ctx, money.FromInt64(-40_000))
	p.Instruments().AdjustmentPosted(ctx, money.FromInt64(15_000))

	body := scrape(t, p)
	requireSeries(t, body, "flowed_account_adjustment_amount_IQD_total", `direction="decrease"`)
	requireSeries(t, body, "flowed_account_adjustment_amount_IQD_total", `direction="increase"`)

	if strings.Contains(body, "-40000") {
		t.Error("a negative value reached a monotonic counter")
	}
}

// No label anywhere may carry something that identifies a student. A metric
// label outlives the request in a store with none of the database's access
// control, so this is a disclosure rule and not a cardinality preference.
func TestNoInstrumentCarriesAnIdentifier(t *testing.T) {
	p := newProvider(t)
	ctx := context.Background()
	m := p.Instruments()

	m.PaymentPosted(ctx, "CASH", money.FromInt64(1))
	m.RequestCompleted(ctx, "GET", "/api/v1/students/:id", 200, 5*time.Millisecond)
	m.RateLimitDecision(ctx, observability.TierShared, false)
	m.SchedulerJobFinished(ctx, "reconciliation", "ok", time.Second)

	body := scrape(t, p)

	// The route label must be the template. A path-keyed label would both
	// explode the series count and publish the id sitting in the path.
	requireSeries(t, body, "http_server_request_duration_seconds_bucket",
		`http_request_method="GET"`, `http_route="/api/v1/students/:id"`)
	requireSeries(t, body, "flowed_ratelimit_decisions_total",
		`tier="shared"`, `outcome="denied"`)
	requireSeries(t, body, "flowed_scheduler_job_runs_total",
		`job="reconciliation"`, `outcome="ok"`)
}

// A nil Provider is a working no-op, and so is the nil *Metrics it hands out.
// That property is what lets every service record unconditionally: a guard at
// the call site is a guard somebody eventually forgets, and the metric that
// then stops being emitted is discovered on the day it was needed.
func TestNilProviderIsUsable(t *testing.T) {
	var p *observability.Provider

	if p.TracingEnabled() {
		t.Error("a nil provider claims tracing is on")
	}
	if p.MetricsHandler() != nil {
		t.Error("a nil provider returned a metrics handler")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a nil provider: %v", err)
	}
	if err := p.ObservePool(func() observability.PoolStats { return observability.PoolStats{} }); err != nil {
		t.Errorf("ObservePool on a nil provider: %v", err)
	}

	// The tracer must be usable rather than nil, or every span site would need
	// its own check.
	_, span := p.Tracer().Start(context.Background(), "anything")
	span.End()

	ctx := context.Background()
	m := p.Instruments()
	m.PaymentPosted(ctx, "CASH", money.FromInt64(1))
	m.PaymentVoided(ctx, money.FromInt64(1))
	m.RefundPosted(ctx, money.FromInt64(1))
	m.AccountGenerated(ctx, money.FromInt64(1))
	m.AdjustmentPosted(ctx, money.FromInt64(-1))
	m.DiscountApplied(ctx, "active", money.FromInt64(1))
	m.RateLimitDecision(ctx, observability.TierLocal, true)
	m.SchedulerJobFinished(ctx, "job", "ok", time.Second)
	m.RequestCompleted(ctx, "GET", "/x", 200, time.Second)
	m.RequestStarted(ctx, "GET")()
}

// Metrics switched off must not leave a half-built pipeline behind.
func TestMetricsDisabledLeavesNoEndpoint(t *testing.T) {
	p, err := observability.Setup(context.Background(),
		config.Observability{MetricsEnabled: false},
		config.App{Name: "flowed-tuition", Environment: "test"},
		"v-test", logger.NewNop())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if p.MetricsHandler() != nil {
		t.Error("metrics are disabled but an endpoint was built")
	}
	// Recording still has to be safe; the services do not know it is off.
	p.Instruments().PaymentPosted(context.Background(), "CASH", money.FromInt64(1))
}
