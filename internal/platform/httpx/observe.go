package httpx

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/swibit/flowed/internal/platform/observability"
)

// unmatchedRoute stands in for the route template of a request that matched no
// route.
//
// Without it the label would be the raw path, and a port scanner walking a
// thousand URLs would create a thousand time series that never appear again —
// the classic way a metrics backend is filled up from outside. One label for
// every miss says the same thing and costs one series.
const unmatchedRoute = "/{unmatched}"

// Observe opens a span and records one latency sample per request.
//
// It sits immediately after RequestID and before Logger, which is deliberate
// in both directions. After RequestID, so the id can be stamped on the span
// and a cashier quoting a failed request leads straight to its trace. Before
// Logger, so the trace id exists by the time the request logger is built and
// every record of the request carries it — logs that cannot be joined to the
// trace they belong to are the usual reason a trace backend goes unused.
func Observe(p *observability.Provider) gin.HandlerFunc {
	tracer := p.Tracer()
	metrics := p.Instruments()

	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		method := requestMethod(c)
		start := time.Now()

		// A trace that started at the gateway continues here rather than
		// beginning again, which is the only way the hop that was slow can be
		// told from the hop that was waiting.
		ctx := otel.GetTextMapPropagator().Extract(
			c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))

		// The route template is unknown until gin has matched, so the span
		// opens under the method alone and is renamed below.
		ctx, span := tracer.Start(ctx, method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(method),
				semconv.URLScheme(requestScheme(c)),
				semconv.ClientAddress(c.ClientIP()),
				attribute.String("flowed.request_id", RequestIDFrom(c)),
			),
		)
		defer span.End()

		c.Request = c.Request.WithContext(ctx)

		done := metrics.RequestStarted(ctx, method)
		defer done()

		c.Next()

		route := c.FullPath()
		if route == "" {
			route = unmatchedRoute
		}
		status := c.Writer.Status()

		span.SetName(method + " " + route)
		span.SetAttributes(
			semconv.HTTPRoute(route),
			semconv.HTTPResponseStatusCode(status),
		)
		// Only server faults mark the span as failed. A 404 or a 409 is this
		// API refusing correctly, and colouring those red trains everyone to
		// ignore the colour.
		if status >= 500 {
			span.SetStatus(codes.Error, "")
		}

		metrics.RequestCompleted(ctx, method, route, status, time.Since(start))
	}
}

// requestScheme reports how the client reached us, honouring the forwarded
// header only from a proxy gin has been configured to trust.
func requestScheme(c *gin.Context) string {
	if c.Request.TLS != nil {
		return "https"
	}
	if forwarded := c.GetHeader("X-Forwarded-Proto"); forwarded != "" && c.RemoteIP() != c.ClientIP() {
		return forwarded
	}
	return "http"
}

// traceAttrs returns the identifiers that join a log record to its trace, or
// nothing when tracing is off.
func traceAttrs(c *gin.Context) (traceID, spanID string) {
	if c.Request == nil {
		return "", ""
	}
	sc := trace.SpanContextFromContext(c.Request.Context())
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}
