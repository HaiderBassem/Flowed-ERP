package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/swibit/flowed/internal/domain/money"
)

// Attribute keys. Declared once so a typo cannot silently split one time
// series into two that never add up.
const (
	attrMethod    = attribute.Key("method_code")
	attrDirection = attribute.Key("direction")
	attrStatus    = attribute.Key("status")
	attrJob       = attribute.Key("job")
	attrOutcome   = attribute.Key("outcome")
	attrTier      = attribute.Key("tier")
	attrState     = attribute.Key("state")
)

// Metrics holds every instrument this process records against.
//
// A nil *Metrics is a working no-op. Recording is unconditional at the call
// site — there is no `if metrics != nil` anywhere in the services — because a
// guard that can be forgotten is a metric that silently stops being emitted,
// and the day anybody notices is the day they needed it.
//
// The money counters are all cumulative totals in whole dinars, recorded
// **after** the transaction commits. Incrementing inside the transaction would
// count money that a later rollback never took: the dashboard would then
// disagree with the ledger, and the ledger is the one that is right.
type Metrics struct {
	requestDuration metric.Float64Histogram
	requestsActive  metric.Int64UpDownCounter

	paymentsPosted metric.Int64Counter
	paymentAmount  metric.Int64Counter
	paymentsVoided metric.Int64Counter
	voidedAmount   metric.Int64Counter
	refundsPosted  metric.Int64Counter
	refundAmount   metric.Int64Counter

	accountsGenerated metric.Int64Counter
	accountNetTotal   metric.Int64Counter
	adjustments       metric.Int64Counter
	adjustmentAmount  metric.Int64Counter

	discountsApplied metric.Int64Counter
	discountAmount   metric.Int64Counter

	rateLimitDecisions metric.Int64Counter

	jobDuration metric.Float64Histogram
	jobRuns     metric.Int64Counter

	meter metric.Meter
}

func newMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{meter: meter}
	var err error

	int64Counter := func(name, unit, description string) metric.Int64Counter {
		if err != nil {
			return nil
		}
		var c metric.Int64Counter
		c, err = meter.Int64Counter(name,
			metric.WithUnit(unit), metric.WithDescription(description))
		return c
	}

	// http.server.request.duration is the OTel semantic-convention name and
	// unit. Keeping it rather than inventing our own means an off-the-shelf
	// dashboard works against this service without a translation layer.
	if m.requestDuration, err = meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time from accepting a request to writing its last byte."),
	); err != nil {
		return nil, fmt.Errorf("building request duration instrument: %w", err)
	}
	if m.requestsActive, err = meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Requests currently being served."),
	); err != nil {
		return nil, fmt.Errorf("building active requests instrument: %w", err)
	}

	m.paymentsPosted = int64Counter("flowed.payment.posted", "{payment}",
		"Payments posted, counted after their transaction committed.")
	m.paymentAmount = int64Counter("flowed.payment.amount", "IQD",
		"Gross dinars collected, counted after their transaction committed.")
	m.paymentsVoided = int64Counter("flowed.payment.voided", "{payment}",
		"Payments reversed by a void. A rate that climbs is the first sign of a cashier erasing receipts.")
	m.voidedAmount = int64Counter("flowed.payment.voided.amount", "IQD",
		"Dinars removed from collection by voids.")
	m.refundsPosted = int64Counter("flowed.refund.posted", "{refund}",
		"Refunds posted.")
	m.refundAmount = int64Counter("flowed.refund.amount", "IQD",
		"Dinars returned to students.")

	m.accountsGenerated = int64Counter("flowed.account.generated", "{account}",
		"Financial accounts generated, each freezing one enrollment's prices.")
	// "obligation" rather than "net_total": the Prometheus exporter strips a
	// trailing _total before appending its own, so flowed.account.net_total
	// would be scraped as flowed_account_net_IQD_total — a name that reads as
	// something else entirely.
	m.accountNetTotal = int64Counter("flowed.account.obligation", "IQD",
		"Dinars of obligation raised at account generation, before later adjustments.")
	m.adjustments = int64Counter("flowed.account.adjustment", "{adjustment}",
		"Signed adjustments posted against a frozen net.")
	m.adjustmentAmount = int64Counter("flowed.account.adjustment.amount", "IQD",
		"Absolute dinars of adjustment, split by direction; the frozen net itself never moves.")

	m.discountsApplied = int64Counter("flowed.discount.applied", "{application}",
		"Discount applications materialised onto an account.")
	m.discountAmount = int64Counter("flowed.discount.amount", "IQD",
		"Dinars given away by discounts, as applied rather than as computed.")

	m.rateLimitDecisions = int64Counter("flowed.ratelimit.decisions", "{decision}",
		"Rate limit decisions by outcome and by which tier decided.")

	if err == nil {
		m.jobDuration, err = meter.Float64Histogram("flowed.scheduler.job.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Wall time of one scheduled job pass."))
	}
	m.jobRuns = int64Counter("flowed.scheduler.job.runs", "{run}",
		"Scheduled job passes by outcome. A job whose runs stop is invisible in a log and obvious here.")

	if err != nil {
		return nil, fmt.Errorf("building instruments: %w", err)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// RequestStarted marks a request as in flight and returns the function that
// marks it finished. Returning the closure rather than exposing a matching
// RequestFinished is what makes the pair impossible to unbalance.
func (m *Metrics) RequestStarted(ctx context.Context, method string) func() {
	if m == nil || m.requestsActive == nil {
		return func() {}
	}
	set := attribute.NewSet(attrMethodKey(method))
	m.requestsActive.Add(ctx, 1, metric.WithAttributeSet(set))
	return func() { m.requestsActive.Add(ctx, -1, metric.WithAttributeSet(set)) }
}

// RequestCompleted records one finished request.
//
// route must be the matched route template — /students/:id — and never the
// request path. A path carries a student id, so a path-keyed histogram grows
// one time series per student and leaks the identifier into a store nobody
// audits. Both failure modes come from the same mistake.
func (m *Metrics) RequestCompleted(ctx context.Context, method, route string, status int, elapsed time.Duration) {
	if m == nil || m.requestDuration == nil {
		return
	}
	m.requestDuration.Record(ctx, elapsed.Seconds(), metric.WithAttributeSet(attrs(
		attrMethodKey(method),
		attribute.String("http.route", route),
		attribute.Int("http.response.status_code", status),
	)))
}

// attrMethodKey is the HTTP method attribute, kept distinct from the payment
// method attribute of the same English word.
func attrMethodKey(method string) attribute.KeyValue {
	return attribute.String("http.request.method", method)
}

// ---------------------------------------------------------------------------
// Money
//
// Every recorder below is called after the command's transaction committed.
// ---------------------------------------------------------------------------

// PaymentPosted records one collection.
func (m *Metrics) PaymentPosted(ctx context.Context, methodCode string, amount money.Amount) {
	if m == nil || m.paymentsPosted == nil {
		return
	}
	set := metric.WithAttributeSet(attrs(attrMethod.String(methodCode)))
	m.paymentsPosted.Add(ctx, 1, set)
	m.paymentAmount.Add(ctx, amount.Int64(), set)
}

// PaymentVoided records one reversal.
//
// Unlabelled by payment method, unlike a collection. The method is not on the
// payment row as a code, and resolving it would mean an extra query inside the
// void's transaction — while it holds the account lock — to decorate a
// counter. What matters here is the rate and the amount, and both are on this
// instrument already.
func (m *Metrics) PaymentVoided(ctx context.Context, amount money.Amount) {
	if m == nil || m.paymentsVoided == nil {
		return
	}
	m.paymentsVoided.Add(ctx, 1)
	m.voidedAmount.Add(ctx, amount.Int64())
}

// RefundPosted records money handed back.
func (m *Metrics) RefundPosted(ctx context.Context, amount money.Amount) {
	if m == nil || m.refundsPosted == nil {
		return
	}
	m.refundsPosted.Add(ctx, 1)
	m.refundAmount.Add(ctx, amount.Int64())
}

// AccountGenerated records one account and the obligation it froze.
func (m *Metrics) AccountGenerated(ctx context.Context, netTotal money.Amount) {
	if m == nil || m.accountsGenerated == nil {
		return
	}
	m.accountsGenerated.Add(ctx, 1)
	m.accountNetTotal.Add(ctx, netTotal.Int64())
}

// AdjustmentPosted records one signed change to what is owed.
//
// The amount is recorded as a magnitude with its direction as an attribute
// rather than as a signed number: a counter that can go down is not a counter,
// and Prometheus' rate() over one would report nonsense on every credit.
func (m *Metrics) AdjustmentPosted(ctx context.Context, amount money.Amount) {
	if m == nil || m.adjustments == nil {
		return
	}
	direction, magnitude := "increase", amount.Int64()
	if magnitude < 0 {
		direction, magnitude = "decrease", -magnitude
	}
	set := metric.WithAttributeSet(attrs(attrDirection.String(direction)))
	m.adjustments.Add(ctx, 1, set)
	m.adjustmentAmount.Add(ctx, magnitude, set)
}

// DiscountApplied records relief granted, as applied rather than as computed:
// a grant cut short by a cap or by the discountable floor cost the university
// the smaller figure, and the larger one was never money.
//
// Split by application status rather than by discount category. The category
// lives on the definition, two joins away from the application, and the
// question this instrument is asked in practice — how much relief is still
// provisional on an eligibility re-check nobody has done — is answered by the
// status and not by the category.
func (m *Metrics) DiscountApplied(ctx context.Context, status string, applied money.Amount) {
	if m == nil || m.discountsApplied == nil {
		return
	}
	set := metric.WithAttributeSet(attrs(attrStatus.String(status)))
	m.discountsApplied.Add(ctx, 1, set)
	m.discountAmount.Add(ctx, applied.Int64(), set)
}

// ---------------------------------------------------------------------------
// Infrastructure
// ---------------------------------------------------------------------------

// Rate limit tiers and outcomes, as recorded on flowed.ratelimit.decisions.
const (
	// TierLocal is the in-process bucket, which every replica keeps.
	TierLocal = "local"
	// TierShared is the cross-replica bucket in PostgreSQL.
	TierShared = "shared"
	// TierDegraded means the shared store could not be reached and the local
	// decision stood in for it. A rate that is anything but zero is the signal
	// that the configured limit is not the limit actually being enforced.
	TierDegraded = "degraded"
)

// RateLimitDecision records one admission decision.
func (m *Metrics) RateLimitDecision(ctx context.Context, tier string, allowed bool) {
	if m == nil || m.rateLimitDecisions == nil {
		return
	}
	outcome := "allowed"
	if !allowed {
		outcome = "denied"
	}
	m.rateLimitDecisions.Add(ctx, 1, metric.WithAttributeSet(attrs(
		attrTier.String(tier),
		attrOutcome.String(outcome),
	)))
}

// SchedulerJobFinished records one pass of a background job.
//
// outcome distinguishes a clean pass from one that found a defect and from one
// that failed outright, because reconciliation finding drift is not an error
// in the job — it is the job working.
func (m *Metrics) SchedulerJobFinished(ctx context.Context, job, outcome string, elapsed time.Duration) {
	if m == nil || m.jobRuns == nil {
		return
	}
	set := metric.WithAttributeSet(attrs(
		attrJob.String(job),
		attrOutcome.String(outcome),
	))
	m.jobRuns.Add(ctx, 1, set)
	m.jobDuration.Record(ctx, elapsed.Seconds(),
		metric.WithAttributeSet(attrs(attrJob.String(job))))
}

// PoolStats is the slice of connection-pool telemetry worth reporting.
//
// It is declared here rather than taken from the pg package so that neither
// package needs the other: observability stays free of a database dependency,
// and pg stays free of a telemetry one. The wiring in cmd/api bridges them,
// which is where every other concrete-to-interface join in this system lives.
type PoolStats struct {
	Acquired      int32
	Idle          int32
	Total         int32
	Max           int32
	AcquireCount  int64
	EmptyAcquires int64
	AcquireWait   time.Duration
}

// ObservePool reports pool utilisation on every scrape.
//
// Observed rather than pushed because these are gauges the pool already
// maintains: sampling them when somebody asks costs nothing between scrapes,
// where a ticker updating them would burn a wakeup a second for a number
// nobody was reading.
//
// EmptyAcquires is the one to alert on. It counts the times a caller wanted a
// connection and the pool had none — the shape of a cashier desk hanging while
// a report holds the pool.
func (p *Provider) ObservePool(stats func() PoolStats) error {
	if p == nil || p.metrics == nil || p.metrics.meter == nil {
		return nil
	}
	meter := p.metrics.meter

	connections, err := meter.Int64ObservableGauge("flowed.db.pool.connections",
		metric.WithUnit("{connection}"),
		metric.WithDescription("Pooled connections by state."))
	if err != nil {
		return fmt.Errorf("building pool gauge: %w", err)
	}
	maxConns, err := meter.Int64ObservableGauge("flowed.db.pool.connections.max",
		metric.WithUnit("{connection}"),
		metric.WithDescription("Configured pool ceiling."))
	if err != nil {
		return fmt.Errorf("building pool ceiling gauge: %w", err)
	}
	acquires, err := meter.Int64ObservableCounter("flowed.db.pool.acquires",
		metric.WithUnit("{acquire}"),
		metric.WithDescription("Connections acquired from the pool."))
	if err != nil {
		return fmt.Errorf("building pool acquire counter: %w", err)
	}
	empty, err := meter.Int64ObservableCounter("flowed.db.pool.acquires.empty",
		metric.WithUnit("{acquire}"),
		metric.WithDescription("Acquires that had to wait because the pool was exhausted."))
	if err != nil {
		return fmt.Errorf("building pool starvation counter: %w", err)
	}
	wait, err := meter.Float64ObservableCounter("flowed.db.pool.acquire.wait",
		metric.WithUnit("s"),
		metric.WithDescription("Cumulative time spent waiting for a pooled connection."))
	if err != nil {
		return fmt.Errorf("building pool wait counter: %w", err)
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			s := stats()
			o.ObserveInt64(connections, int64(s.Acquired),
				metric.WithAttributeSet(attrs(attrState.String("acquired"))))
			o.ObserveInt64(connections, int64(s.Idle),
				metric.WithAttributeSet(attrs(attrState.String("idle"))))
			o.ObserveInt64(maxConns, int64(s.Max))
			o.ObserveInt64(acquires, s.AcquireCount)
			o.ObserveInt64(empty, s.EmptyAcquires)
			o.ObserveFloat64(wait, s.AcquireWait.Seconds())
			return nil
		},
		connections, maxConns, acquires, empty, wait,
	)
	if err != nil {
		return fmt.Errorf("registering pool callback: %w", err)
	}
	return nil
}
