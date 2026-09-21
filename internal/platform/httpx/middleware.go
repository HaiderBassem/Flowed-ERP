package httpx

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"flowed/internal/platform/config"
	"flowed/internal/platform/logger"
	"flowed/internal/platform/observability"
	"flowed/internal/port"
)

// RequestIDHeader is the header a correlation id is read from and echoed back
// on. Clients that already have a trace id pass theirs so that a cashier's
// complaint, the terminal's log, and the server's log all name the same thing.
const RequestIDHeader = "X-Request-ID"

// maxRequestIDLength bounds a client-supplied correlation id.
const maxRequestIDLength = 128

// ginRequestIDKey is where the correlation id lives in the gin context. Values
// are stored in both the gin context and the request context: gin only falls
// through to the request context when ContextWithFallback is enabled, and this
// package must not depend on how the engine happens to be configured.
const ginRequestIDKey = "flowed.request_id"

type requestIDContextKey struct{}

// RequestID installs a correlation id on the request, the response, and the
// logging context.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !wellFormedRequestID(id) {
			id = uuid.NewString()
		}

		c.Set(ginRequestIDKey, id)
		if c.Request != nil {
			c.Request = c.Request.WithContext(
				context.WithValue(c.Request.Context(), requestIDContextKey{}, id))
		}
		c.Header(RequestIDHeader, id)

		c.Next()
	}
}

// RequestIDFrom returns the correlation id for the request, or the empty
// string when the RequestID middleware did not run. It accepts either the
// *gin.Context or the request context.
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if gc, ok := ctx.(*gin.Context); ok {
		if value, exists := gc.Get(ginRequestIDKey); exists {
			if id, ok := value.(string); ok {
				return id
			}
		}
		if gc.Request == nil {
			return ""
		}
		ctx = gc.Request.Context()
	}
	if id, ok := ctx.Value(requestIDContextKey{}).(string); ok {
		return id
	}
	return ""
}

// wellFormedRequestID reports whether a client-supplied id may be adopted.
//
// The value is echoed into a response header and stamped onto every log record
// for the request. Accepting arbitrary bytes would let a caller inject newlines
// into the log stream — forging records that look like the server's own — and
// control characters into a header. Anything outside the conservative set below
// is discarded in favour of a generated id.
func wellFormedRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		ch := id[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == '.', ch == ':':
		default:
			return false
		}
	}
	return true
}

// Logger installs a request-scoped logger and writes one record per completed
// request.
//
// Neither the request body nor the query string is logged. Both carry student
// identifiers, names and amounts, and a log shipped off the box is a far
// weaker place to hold those than the database is.
func Logger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		fields := []any{
			slog.String("request_id", RequestIDFrom(c)),
			slog.String("method", requestMethod(c)),
			slog.String("path", requestPath(c)),
		}
		// Present only when Observe ran ahead of this middleware and tracing is
		// on. They are what lets an operator jump from a log line to the span
		// tree that explains it, so they go on the request logger rather than
		// on the completion record alone.
		if traceID, spanID := traceAttrs(c); traceID != "" {
			fields = append(fields,
				slog.String("trace_id", traceID),
				slog.String("span_id", spanID))
		}

		requestLogger := log.With(fields...)
		if c.Request != nil {
			c.Request = c.Request.WithContext(logger.Into(c.Request.Context(), requestLogger))
		}

		c.Next()

		status := c.Writer.Status()
		bytesWritten := c.Writer.Size()
		if bytesWritten < 0 {
			// gin reports -1 until something is written; a handler that answered
			// 204 wrote zero bytes, not minus one.
			bytesWritten = 0
		}

		attrs := []any{
			slog.Int("status", status),
			slog.Duration("latency", time.Since(start)),
			slog.Int("bytes", bytesWritten),
			slog.String("client_ip", c.ClientIP()),
		}

		ctx := requestContext(c)
		switch {
		case status >= http.StatusInternalServerError:
			requestLogger.ErrorContext(ctx, "request completed", attrs...)
		case status >= http.StatusBadRequest:
			requestLogger.WarnContext(ctx, "request completed", attrs...)
		default:
			requestLogger.InfoContext(ctx, "request completed", attrs...)
		}
	}
}

// Recovery turns a panic into a logged stack trace and a generic 500.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			ctx := requestContext(c)
			requestID := RequestIDFrom(c)

			if isBrokenPipe(recovered) {
				// The client vanished mid-write. There is nobody left to answer and
				// nothing wrong with the server, so note it and stop.
				log.WarnContext(ctx, "client disconnected mid-response",
					slog.String("request_id", requestID),
					slog.String("path", requestPath(c)))
				c.Abort()
				return
			}

			log.ErrorContext(ctx, "panic recovered",
				slog.Bool("panic", true),
				slog.String("request_id", requestID),
				slog.String("method", requestMethod(c)),
				slog.String("path", requestPath(c)),
				slog.String("client_ip", c.ClientIP()),
				slog.Any("panic_value", recovered),
				slog.String("stack", string(debug.Stack())),
			)

			// The panic value routinely quotes internal state — a nil map, a row
			// from a query, a file path. It belongs in the record above and
			// nowhere near the client.
			writeError(c, http.StatusInternalServerError, ErrorBody{
				Code:      "internal_error",
				Message:   genericInternalMessage,
				RequestID: requestID,
			})
		}()

		c.Next()
	}
}

// isBrokenPipe reports whether a panic is really just a client hanging up.
func isBrokenPipe(recovered any) bool {
	err, ok := recovered.(error)
	if !ok {
		return false
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	var sysErr *os.SyscallError
	if !errors.As(opErr, &sysErr) {
		return false
	}
	message := strings.ToLower(sysErr.Error())
	return strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "connection reset by peer")
}

// corsAllowedMethods and corsAllowedHeaders describe what a browser client may
// send. Idempotency-Key is on the list because the money-moving routes require
// it, and a header the preflight does not permit is a header the browser will
// not send.
const (
	corsAllowedMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowedHeaders = "Origin, Content-Type, Accept, Authorization, Idempotency-Key, X-Request-ID"
	// Response headers a browser client is allowed to read. Without this the
	// request id is invisible to the very page that would show it to the user,
	// and the budget headers are invisible to the page that should be pacing
	// itself against them.
	corsExposedHeaders = RequestIDHeader + ", " + IdempotentReplayHeader +
		", " + rateLimitLimitHeader + ", " + rateLimitRemainingHeader + ", Retry-After"
	corsPreflightMaxAge = "600"
)

// CORS applies the configured browser origin allowlist.
//
// A wildcard is honoured when it is configured, which configuration permits
// outside production only. No Access-Control-Allow-Credentials is ever sent:
// this API authenticates with a bearer token in a header, not with a cookie,
// so credentialed CORS buys nothing and would forbid the wildcard that
// development relies on.
func CORS(cfg config.HTTP) gin.HandlerFunc {
	allowAll := false
	allowed := make(map[string]struct{}, len(cfg.CORSAllowedOrigins))
	for _, origin := range cfg.CORSAllowedOrigins {
		origin = strings.TrimSpace(origin)
		switch {
		case origin == "":
		case origin == "*":
			allowAll = true
		default:
			allowed[strings.ToLower(origin)] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			// Not a browser cross-origin call: a cashier terminal, a report job,
			// curl. CORS headers would be noise.
			c.Next()
			return
		}

		if allowAll {
			c.Header("Access-Control-Allow-Origin", "*")
		} else {
			// The answer now varies by origin, so a shared cache must not hand one
			// origin's response to another. Added rather than set, because another
			// layer may already vary on something else.
			c.Writer.Header().Add("Vary", "Origin")
			if _, ok := allowed[strings.ToLower(origin)]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
			}
			// An origin that is not on the list simply gets no allow header, and
			// the browser refuses the response on the caller's behalf. Answering
			// 403 here would be a lie: the request itself is perfectly legitimate
			// when it comes from a terminal rather than a page.
		}

		if c.Request != nil && c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", corsAllowedMethods)
			c.Header("Access-Control-Allow-Headers", corsAllowedHeaders)
			c.Header("Access-Control-Max-Age", corsPreflightMaxAge)
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Header("Access-Control-Expose-Headers", corsExposedHeaders)
		c.Next()
	}
}

// BodyLimit caps how much of a request body the server will read, so that an
// import endpoint cannot be used to exhaust memory. Reads past the limit fail
// at the point of binding rather than after the whole payload is buffered.
// BodyLimit caps request bodies at maxBytes, except the paths named in
// overrides, which get their own limit — a backup upload is legitimately far
// larger than anything else this API accepts, and the alternative to naming
// it here is raising the global cap for every route to accommodate the one
// that needs it.
func BodyLimit(maxBytes int64, overrides map[string]int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := maxBytes
		if o, ok := overrides[c.Request.URL.Path]; ok {
			limit = o
		}
		if limit > 0 && c.Request != nil && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

// Timeout gives the request context a deadline.
//
// It bounds the work the handler is permitted to keep doing — the database
// driver cancels its statement when the context expires — but it deliberately
// does not race the handler to write a 504. Telling a cashier that a payment
// timed out while its transaction is still committing is worse than making
// them wait: they would post it again.
func Timeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d <= 0 || c.Request == nil {
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

const (
	// bucketSweepInterval and bucketIdleTTL control how quickly the local
	// limiter forgets a client. A bucket refills fully within its window, so
	// anything idle for several windows carries no information worth keeping.
	bucketSweepInterval = 5 * time.Minute
	bucketIdleTTL       = 10 * time.Minute

	// degradedLogInterval throttles the warning raised when the shared limiter
	// is unreachable. The condition that produces it — the database being down
	// — produces it on every request, and a log line per request would bury
	// the outage's actual cause under its own symptom.
	degradedLogInterval = 30 * time.Second
)

// Headers describing the budget. A client that can see what is left can pace
// itself; one that cannot has to discover the limit by being refused.
const (
	rateLimitLimitHeader     = "X-RateLimit-Limit"
	rateLimitRemainingHeader = "X-RateLimit-Remaining"
)

// RateLimitConfig wires the limiter.
type RateLimitConfig struct {
	// PerMinute is the budget for one client. Zero or less disables the
	// middleware entirely.
	PerMinute int
	// Shared is the cross-replica bucket. Nil keeps the limiter per process,
	// which is correct on a single replica and in tests.
	Shared port.RateLimiter
	// ExemptRoutes lists route templates the limiter does not apply to. The
	// caller names them because this package must not know the API's routes.
	ExemptRoutes []string
	Metrics      *observability.Metrics
	Log          *slog.Logger
}

// RateLimit caps how often one client address may call the API.
//
// Two tiers, in this order.
//
// The local tier is an in-memory token bucket, the same one this middleware
// has always had. It sees only this replica's traffic, so it can never admit
// what the shared budget would refuse — it can only refuse sooner. That makes
// it a free pre-filter: a terminal stuck in a retry loop is turned away
// without a database round trip.
//
// The shared tier is the budget itself, held in PostgreSQL and therefore the
// same budget for every replica. This is the tier the configured number
// actually means.
//
// When the shared tier cannot be reached the request is admitted on the local
// decision alone. Failing open is deliberate: a limiter exists to protect
// availability, and one that turns a database blip into a total outage has
// inverted its own purpose. The degradation is counted and logged, because a
// limit that has quietly reverted to per-process is a thing an operator must
// be able to discover rather than infer.
//
// The budget is keyed by client address, which at a university means keyed by
// campus NAT: a hall of thirty cashier terminals shares one bucket. That is
// why the default is 600 a minute rather than a per-operator figure, and why
// raising it for a large site is a configuration change and not a redesign.
func RateLimit(cfg RateLimitConfig) gin.HandlerFunc {
	if cfg.PerMinute <= 0 {
		return func(c *gin.Context) { c.Next() }
	}

	local := newIPRateLimiter(float64(cfg.PerMinute), time.Minute)
	limitHeader := strconv.Itoa(cfg.PerMinute)

	exempt := make(map[string]struct{}, len(cfg.ExemptRoutes))
	for _, route := range cfg.ExemptRoutes {
		exempt[route] = struct{}{}
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	degraded := &throttledLogger{every: degradedLogInterval}

	return func(c *gin.Context) {
		if _, skip := exempt[c.FullPath()]; skip {
			c.Next()
			return
		}

		ctx := requestContext(c)
		key := c.ClientIP()

		allowed, remaining, retryAfter := local.allow(key, time.Now())
		if !allowed {
			cfg.Metrics.RateLimitDecision(ctx, observability.TierLocal, false)
			refuse(c, limitHeader, retryAfter)
			return
		}

		tier := observability.TierLocal
		if cfg.Shared != nil {
			decision, err := cfg.Shared.Take(ctx, key, cfg.PerMinute, time.Minute)
			switch {
			case err != nil:
				tier = observability.TierDegraded
				degraded.warn(ctx, log,
					"shared rate limiter unreachable; falling back to the per-process budget",
					slog.String("error", err.Error()),
					slog.String("remedy", "the configured limit is now enforced per replica, not across the deployment"))
			case !decision.Allowed:
				cfg.Metrics.RateLimitDecision(ctx, observability.TierShared, false)
				c.Header(rateLimitRemainingHeader, "0")
				refuse(c, limitHeader, decision.RetryAfter)
				return
			default:
				tier = observability.TierShared
				remaining = decision.Remaining
			}
		}

		cfg.Metrics.RateLimitDecision(ctx, tier, true)
		c.Header(rateLimitLimitHeader, limitHeader)
		c.Header(rateLimitRemainingHeader, strconv.Itoa(remaining))
		c.Next()
	}
}

// refuse writes the 429.
func refuse(c *gin.Context, limitHeader string, retryAfter time.Duration) {
	c.Header(rateLimitLimitHeader, limitHeader)
	c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	// 429 has no domain kind of its own: exhausting a rate limit is a
	// property of the transport rather than of any business rule, so the
	// response is written directly instead of through StatusForKind.
	writeError(c, http.StatusTooManyRequests, ErrorBody{
		Code:      "rate_limited",
		Message:   "too many requests; slow down and retry",
		RequestID: RequestIDFrom(c),
	})
}

// throttledLogger emits at most one record per interval and says how many it
// swallowed, so a condition that fires on every request stays visible without
// becoming the log.
type throttledLogger struct {
	every time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func (t *throttledLogger) warn(ctx context.Context, log *slog.Logger, msg string, attrs ...any) {
	t.mu.Lock()
	now := time.Now()
	if !t.last.IsZero() && now.Sub(t.last) < t.every {
		t.suppressed++
		t.mu.Unlock()
		return
	}
	suppressed := t.suppressed
	t.suppressed = 0
	t.last = now
	t.mu.Unlock()

	if suppressed > 0 {
		attrs = append(attrs, slog.Int("suppressed_since_last", suppressed))
	}
	log.WarnContext(ctx, msg, attrs...)
}

// ipRateLimiter is a token bucket per client IP. The map is the only mutable
// state this package keeps between requests, and every access takes the mutex.
type ipRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	capacity float64
	refill   float64 // tokens per second
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(perWindow float64, window time.Duration) *ipRateLimiter {
	limiter := &ipRateLimiter{
		buckets:  make(map[string]*tokenBucket),
		capacity: perWindow,
		refill:   perWindow / window.Seconds(),
	}
	// The sweeper runs for the life of the process. That is intentional and
	// bounded: RateLimit is called once when the router is assembled, so there
	// is exactly one limiter and one goroutine.
	go limiter.sweep(bucketSweepInterval, bucketIdleTTL)
	return limiter
}

// allow spends one token for the key, reporting how many whole tokens are left
// and — when the bucket is empty — how long the caller should wait.
func (l *ipRateLimiter) allow(key string, now time.Time) (allowed bool, remaining int, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, known := l.buckets[key]
	if !known {
		bucket = &tokenBucket{tokens: l.capacity, last: now}
		l.buckets[key] = bucket
	} else if elapsed := now.Sub(bucket.last).Seconds(); elapsed > 0 {
		bucket.tokens = math.Min(l.capacity, bucket.tokens+elapsed*l.refill)
		bucket.last = now
	}

	if bucket.tokens < 1 {
		// A refused request does not spend. Charging for it would put a client
		// that retries hard into a deficit, so the rate it actually gets would
		// be lower than the configured one and unexplainable from it.
		return false, 0, time.Duration((1 - bucket.tokens) / l.refill * float64(time.Second))
	}
	bucket.tokens--
	return true, int(bucket.tokens), 0
}

// sweep discards buckets nobody has touched recently. Without it the map grows
// by one entry per distinct client IP for the life of the process, which a port
// scan or a large NAT range turns into a slow leak.
func (l *ipRateLimiter) sweep(every, idleFor time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for now := range ticker.C {
		l.mu.Lock()
		for key, bucket := range l.buckets {
			if now.Sub(bucket.last) > idleFor {
				delete(l.buckets, key)
			}
		}
		l.mu.Unlock()
	}
}
