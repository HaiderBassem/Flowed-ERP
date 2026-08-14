package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/port"
)

// fakeSharedLimiter stands in for the PostgreSQL bucket. It counts calls,
// because the point of several tests below is not what it answered but whether
// it was consulted at all.
type fakeSharedLimiter struct {
	mu sync.Mutex

	budget int
	err    error
	calls  int
	keys   []string
}

func (f *fakeSharedLimiter) Take(
	_ context.Context, key string, _ int, _ time.Duration,
) (port.RateLimitDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.keys = append(f.keys, key)

	if f.err != nil {
		return port.RateLimitDecision{}, f.err
	}
	if f.budget <= 0 {
		return port.RateLimitDecision{Allowed: false, RetryAfter: 2 * time.Second}, nil
	}
	f.budget--
	return port.RateLimitDecision{Allowed: true, Remaining: f.budget}, nil
}

func (f *fakeSharedLimiter) PurgeIdle(context.Context, time.Duration) (int64, error) { return 0, nil }

func (f *fakeSharedLimiter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func limitedRouter(cfg httpx.RateLimitConfig) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())
	router.Use(httpx.RateLimit(cfg))
	router.GET("/health", func(c *gin.Context) { httpx.NoContent(c) })
	router.GET("/api/v1/students", func(c *gin.Context) { httpx.NoContent(c) })
	return router
}

// The shared budget is the one that binds. The local bucket is deliberately
// generous here so that only the shared tier can refuse, which is the
// arrangement a real deployment has when a single replica is well under the
// limit but the fleet as a whole is not.
func TestRateLimitSharedBudgetRefusesWhatLocalWouldAdmit(t *testing.T) {
	shared := &fakeSharedLimiter{budget: 2}
	router := limitedRouter(httpx.RateLimitConfig{
		PerMinute: 1000,
		Shared:    shared,
		Log:       logger.NewNop(),
	})

	for attempt := 1; attempt <= 2; attempt++ {
		if rec := get(t, router, "/api/v1/students", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: got %d, want 204 while the shared budget had tokens", attempt, rec.Code)
		}
	}

	rec := get(t, router, "/api/v1/students", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third attempt: got %d, want 429 — the local bucket admitted what the shared budget had spent", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 from the shared tier should still say when to come back")
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("remaining header on refusal: got %q, want %q", got, "0")
	}
}

// A request the local bucket has already refused must not cost a database
// round trip. That is the whole reason the local tier is still there.
func TestRateLimitLocalRefusalSkipsTheSharedStore(t *testing.T) {
	shared := &fakeSharedLimiter{budget: 1000}
	router := limitedRouter(httpx.RateLimitConfig{
		PerMinute: 2,
		Shared:    shared,
		Log:       logger.NewNop(),
	})

	for attempt := 1; attempt <= 2; attempt++ {
		if rec := get(t, router, "/api/v1/students", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: got %d, want 204", attempt, rec.Code)
		}
	}
	admitted := shared.callCount()

	if rec := get(t, router, "/api/v1/students", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third attempt: got %d, want 429 from the local bucket", rec.Code)
	}

	if got := shared.callCount(); got != admitted {
		t.Errorf("shared store consulted %d times after a local refusal, want %d: "+
			"a locally refused request must not reach the database", got, admitted)
	}
}

// The limiter exists to protect availability. One that turns a database outage
// into a total outage has inverted its own purpose, so an unreachable shared
// store must degrade to the local decision rather than refuse.
func TestRateLimitFailsOpenWhenTheSharedStoreIsDown(t *testing.T) {
	shared := &fakeSharedLimiter{err: errors.New("connection refused")}
	router := limitedRouter(httpx.RateLimitConfig{
		PerMinute: 5,
		Shared:    shared,
		Log:       logger.NewNop(),
	})

	for attempt := 1; attempt <= 5; attempt++ {
		if rec := get(t, router, "/api/v1/students", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: got %d, want 204 — an unreachable limiter must not refuse traffic", attempt, rec.Code)
		}
	}

	// The local bucket still binds while degraded, which is what keeps a
	// runaway client from being unlimited for the length of the outage.
	if rec := get(t, router, "/api/v1/students", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth attempt: got %d, want 429 — the local budget should still apply while degraded", rec.Code)
	}
}

// Probes are exempt. With a shared bucket every replica's readiness check
// draws on one budget keyed to the load balancer's address, and the desks
// behind that address would start being refused because the infrastructure was
// checking whether they were up.
func TestRateLimitExemptsProbeRoutes(t *testing.T) {
	shared := &fakeSharedLimiter{budget: 0}
	router := limitedRouter(httpx.RateLimitConfig{
		PerMinute:    1,
		Shared:       shared,
		ExemptRoutes: []string{"/health"},
		Log:          logger.NewNop(),
	})

	for attempt := 1; attempt <= 20; attempt++ {
		if rec := get(t, router, "/health", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("probe %d: got %d, want 204 — probes must not consume the budget", attempt, rec.Code)
		}
	}
	if shared.callCount() != 0 {
		t.Errorf("shared store consulted %d times for exempt routes, want 0", shared.callCount())
	}

	// The exemption is per route and must not leak to anything else.
	if rec := get(t, router, "/api/v1/students", nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a non-exempt route got %d, want 429 with an exhausted shared budget", rec.Code)
	}
}

// A client that can read what is left can pace itself. One that cannot has to
// discover the limit by being refused.
func TestRateLimitReportsTheRemainingBudget(t *testing.T) {
	router := limitedRouter(httpx.RateLimitConfig{PerMinute: 4, Log: logger.NewNop()})

	rec := get(t, router, "/api/v1/students", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "4" {
		t.Errorf("limit header: got %q, want %q", got, "4")
	}
	remaining, err := strconv.Atoi(rec.Header().Get("X-RateLimit-Remaining"))
	if err != nil {
		t.Fatalf("remaining header %q is not a number: %v",
			rec.Header().Get("X-RateLimit-Remaining"), err)
	}
	if remaining != 3 {
		t.Errorf("remaining after one of four: got %d, want 3", remaining)
	}
}
