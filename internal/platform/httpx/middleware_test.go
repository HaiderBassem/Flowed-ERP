package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"flowed/internal/platform/config"
	"flowed/internal/platform/httpx"
	"flowed/internal/platform/logger"
)

func get(t *testing.T, router *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRequestIDAdoptsWellFormedClientValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())

	var fromContext string
	router.GET("/health", func(c *gin.Context) {
		fromContext = httpx.RequestIDFrom(c.Request.Context())
		httpx.NoContent(c)
	})

	const supplied = "trace-01HXYZ.desk-3"
	rec := get(t, router, "/health", map[string]string{httpx.RequestIDHeader: supplied})

	if got := rec.Header().Get(httpx.RequestIDHeader); got != supplied {
		t.Errorf("echoed header: got %q, want %q", got, supplied)
	}
	if fromContext != supplied {
		t.Errorf("request context: got %q, want %q", fromContext, supplied)
	}
}

// TestRequestIDRejectsForgeryAttempts covers log injection: the id is stamped
// on every record for the request, so a newline in it would let a caller write
// records that look like the server's own.
func TestRequestIDRejectsForgeryAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())
	router.GET("/health", func(c *gin.Context) { httpx.NoContent(c) })

	hostile := map[string]string{
		"newline":   "abc\ndef",
		"return":    "abc\r\nSet-Cookie: x=1",
		"space":     "abc def",
		"too long":  strings.Repeat("a", 200),
		"semicolon": "abc;def",
	}

	for name, supplied := range hostile {
		t.Run(name, func(t *testing.T) {
			rec := get(t, router, "/health", map[string]string{httpx.RequestIDHeader: supplied})
			got := rec.Header().Get(httpx.RequestIDHeader)
			if got == supplied {
				t.Fatalf("the hostile value %q was adopted verbatim", supplied)
			}
			if got == "" {
				t.Fatal("expected a generated request id to replace the rejected one")
			}
		})
	}
}

// TestRecoveryHidesThePanicValue keeps whatever the panic quoted — a row, a
// path, a nil map — out of the client's copy while keeping it in the log.
func TestRecoveryHidesThePanicValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())
	router.Use(httpx.Recovery(logger.NewNop()))
	router.GET("/boom", func(c *gin.Context) {
		panic("student 20240001 owes 1500000 IQD on account a1b2")
	})

	rec := get(t, router, "/boom", nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "20240001") {
		t.Errorf("the panic value reached the client: %s", rec.Body.String())
	}
	body := decodeError(t, rec)
	if body.Code != "internal_error" {
		t.Errorf("code: got %q, want internal_error", body.Code)
	}
	if body.RequestID == "" {
		t.Error("expected the request id so the panic can be found in the log")
	}
}

func TestRateLimitBlocksBeyondTheBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())
	router.Use(httpx.RateLimit(httpx.RateLimitConfig{PerMinute: 3}))
	router.GET("/health", func(c *gin.Context) { httpx.NoContent(c) })

	for attempt := 1; attempt <= 3; attempt++ {
		if rec := get(t, router, "/health", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: got %d, want 204 — the budget of 3 was not honoured", attempt, rec.Code)
		}
	}

	rec := get(t, router, "/health", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth attempt: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 should say when to come back")
	}
	if code := decodeError(t, rec).Code; code != "rate_limited" {
		t.Errorf("code: got %q, want rate_limited", code)
	}
}

func TestRateLimitDisabledAtZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RateLimit(httpx.RateLimitConfig{PerMinute: 0}))
	router.GET("/health", func(c *gin.Context) { httpx.NoContent(c) })

	for attempt := range 50 {
		if rec := get(t, router, "/health", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("attempt %d: got %d, want 204 with the limiter disabled", attempt, rec.Code)
		}
	}
}

func TestCORSHonoursTheAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.HTTP{CORSAllowedOrigins: []string{"https://desk.university.edu.iq"}}

	router := gin.New()
	router.Use(httpx.CORS(cfg))
	router.GET("/health", func(c *gin.Context) { httpx.NoContent(c) })

	allowed := get(t, router, "/health", map[string]string{"Origin": "https://desk.university.edu.iq"})
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://desk.university.edu.iq" {
		t.Errorf("allowed origin: got %q", got)
	}
	if got := allowed.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary: got %q, want it to include Origin", got)
	}

	refused := get(t, router, "/health", map[string]string{"Origin": "https://evil.example"})
	if got := refused.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was allowed: %q", got)
	}
	// Never credentialed: this API authenticates with a bearer header.
	if got := refused.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials should never be sent, got %q", got)
	}
}

func TestCORSWildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.CORS(config.HTTP{CORSAllowedOrigins: []string{"*"}}))
	router.GET("/health", func(c *gin.Context) { httpx.NoContent(c) })

	rec := get(t, router, "/health", map[string]string{"Origin": "http://localhost:5173"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("wildcard origin: got %q, want *", got)
	}
}

func TestCORSPreflightPermitsTheIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.CORS(config.HTTP{CORSAllowedOrigins: []string{"*"}}))
	router.POST("/payments", func(c *gin.Context) { httpx.Created(c, gin.H{}) })

	req := httptest.NewRequest(http.MethodOptions, "/payments", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status: got %d, want 204", rec.Code)
	}
	// A header the preflight does not permit is one the browser will not send,
	// which would make every money route fail its idempotency check.
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, httpx.IdempotencyKeyHeader) {
		t.Errorf("Access-Control-Allow-Headers %q does not permit %s", got, httpx.IdempotencyKeyHeader)
	}
}

func TestBodyLimitStopsAnOversizedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())
	router.Use(httpx.BodyLimit(64, nil))
	router.POST("/import", func(c *gin.Context) {
		if _, err := c.GetRawData(); err != nil {
			httpx.Respond(c, err)
			return
		}
		httpx.NoContent(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/import", strings.NewReader(strings.Repeat("x", 4096)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusNoContent {
		t.Fatal("the oversized body was accepted")
	}
}
