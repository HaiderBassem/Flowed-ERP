package httpx_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/port"
)

const testKey = "idem-key-0001"

type completion struct {
	key    string
	status int
	body   []byte
}

type failure struct {
	key       string
	errorCode string
}

// fakeIdempotencyRepo stands in for the PostgreSQL adapter.
type fakeIdempotencyRepo struct {
	existing    *port.IdempotencyRecord
	beginErr    error
	completions []completion
	failures    []failure
}

func (r *fakeIdempotencyRepo) Begin(
	_ context.Context, _, _, _ string, _ *shared.ID,
) (*port.IdempotencyRecord, error) {
	return r.existing, r.beginErr
}

func (r *fakeIdempotencyRepo) Complete(_ context.Context, key, _ string, status int, body []byte) error {
	r.completions = append(r.completions, completion{key: key, status: status, body: body})
	return nil
}

func (r *fakeIdempotencyRepo) Fail(_ context.Context, key, _, errorCode string) error {
	r.failures = append(r.failures, failure{key: key, errorCode: errorCode})
	return nil
}

func (r *fakeIdempotencyRepo) PurgeExpired(context.Context, time.Time) (int64, error) { return 0, nil }

func newRouter(repo port.IdempotencyRepository, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(httpx.RequestID())
	router.POST("/payments", httpx.Idempotency(repo, "payment.record"), handler)
	return router
}

func post(t *testing.T, router *gin.Engine, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(httpx.IdempotencyKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorBody {
	t.Helper()
	var envelope httpx.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding the error envelope from %q: %v", rec.Body.String(), err)
	}
	return envelope.Error
}

// TestIdempotencyRestoresRequestBody is the one that would fail silently. The
// middleware reads the body to hash it; if it does not put it back, every
// handler downstream binds an empty payload — and an empty payload is a
// payment of zero dinars, not a parse error.
func TestIdempotencyRestoresRequestBody(t *testing.T) {
	const payload = `{"account_id":"a","amount":250000}`
	repo := &fakeIdempotencyRepo{}

	var seenByHandler string
	router := newRouter(repo, func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("the handler could not read the body: %v", err)
		}
		seenByHandler = string(body)
		httpx.Created(c, gin.H{"receipt_no": "R-000001"})
	})

	rec := post(t, router, payload, testKey)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if seenByHandler != payload {
		t.Fatalf("the handler saw %q, want %q", seenByHandler, payload)
	}
	if len(repo.completions) != 1 {
		t.Fatalf("expected one completion, got %d", len(repo.completions))
	}
	if got := repo.completions[0].status; got != http.StatusCreated {
		t.Errorf("recorded status: got %d, want 201", got)
	}
	if !strings.Contains(string(repo.completions[0].body), "R-000001") {
		t.Errorf("the recorded body does not carry the receipt: %s", repo.completions[0].body)
	}
}

// TestIdempotencyReplaysCompletedRecord proves a retry returns the original
// receipt rather than printing a second one.
func TestIdempotencyReplaysCompletedRecord(t *testing.T) {
	stored := []byte(`{"receipt_no":"R-000001"}`)
	repo := &fakeIdempotencyRepo{existing: &port.IdempotencyRecord{
		Key:          testKey,
		CommandName:  "payment.record",
		PayloadHash:  "", // an empty stored hash must not trip the reuse check
		Status:       "succeeded",
		ResponseCode: http.StatusCreated,
		ResponseBody: stored,
	}}

	handlerRan := false
	router := newRouter(repo, func(c *gin.Context) {
		handlerRan = true
		httpx.Created(c, gin.H{"receipt_no": "R-000002"})
	})

	rec := post(t, router, `{"amount":1}`, testKey)

	if handlerRan {
		t.Fatal("the handler ran on a replay; the command was performed twice")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status: got %d, want the stored 201", rec.Code)
	}
	if got := rec.Body.String(); got != string(stored) {
		t.Errorf("body: got %q, want the stored %q", got, stored)
	}
	if got := rec.Header().Get(httpx.IdempotentReplayHeader); got != "true" {
		t.Errorf("%s: got %q, want \"true\"", httpx.IdempotentReplayHeader, got)
	}
}

// TestIdempotencyRejectsConcurrentDuplicate keeps a double-click from running
// the command a second time while the first is still committing.
func TestIdempotencyRejectsConcurrentDuplicate(t *testing.T) {
	repo := &fakeIdempotencyRepo{existing: &port.IdempotencyRecord{
		Key:    testKey,
		Status: "in_progress",
	}}

	handlerRan := false
	router := newRouter(repo, func(c *gin.Context) {
		handlerRan = true
		httpx.Created(c, gin.H{})
	})

	rec := post(t, router, `{"amount":1}`, testKey)

	if handlerRan {
		t.Fatal("the handler ran alongside an in-flight duplicate")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d, want 409", rec.Code)
	}
	if code := decodeError(t, rec).Code; code != "command.in_progress" {
		t.Errorf("code: got %q, want command.in_progress", code)
	}
}

// TestIdempotencyRejectsKeyReusedForAnotherPayload stops a client bug from
// being answered with somebody else's receipt.
func TestIdempotencyRejectsKeyReusedForAnotherPayload(t *testing.T) {
	repo := &fakeIdempotencyRepo{existing: &port.IdempotencyRecord{
		Key:          testKey,
		PayloadHash:  "a-hash-of-some-entirely-different-request",
		Status:       "succeeded",
		ResponseCode: http.StatusCreated,
		ResponseBody: []byte(`{"receipt_no":"R-000001"}`),
	}}

	router := newRouter(repo, func(c *gin.Context) { httpx.Created(c, gin.H{}) })
	rec := post(t, router, `{"amount":999}`, testKey)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).Code; code != "command.idempotency_key_reused" {
		t.Errorf("code: got %q, want command.idempotency_key_reused", code)
	}
}

func TestIdempotencyRequiresKey(t *testing.T) {
	repo := &fakeIdempotencyRepo{}
	router := newRouter(repo, func(c *gin.Context) { httpx.Created(c, gin.H{}) })

	rec := post(t, router, `{"amount":1}`, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Code != "command.idempotency_key_required" {
		t.Errorf("code: got %q, want command.idempotency_key_required", body.Code)
	}
	if body.RequestID == "" {
		t.Error("expected a request id in the error body")
	}
}

// TestIdempotencyReleasesTheKeyOnPanic covers the worst way to get this wrong.
// A panic unwinds past the settling code, and an in_progress record refuses
// every retry until it expires — so a crashed handler would lock the cashier
// out of that payment for the life of the record.
func TestIdempotencyReleasesTheKeyOnPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &fakeIdempotencyRepo{}

	router := gin.New()
	router.Use(httpx.RequestID())
	router.Use(httpx.Recovery(logger.NewNop()))
	router.POST("/payments", httpx.Idempotency(repo, "payment.record"), func(c *gin.Context) {
		panic("allocation planner dereferenced a nil installment")
	})

	rec := post(t, router, `{"amount":1}`, testKey)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if len(repo.completions) != 0 {
		t.Errorf("a panicking command must not be recorded as complete: %+v", repo.completions)
	}
	if len(repo.failures) != 1 {
		t.Fatalf("the key was never released; a retry would be refused forever")
	}
	if got := repo.failures[0].errorCode; got != "command.panicked" {
		t.Errorf("recorded error code: got %q, want command.panicked", got)
	}
}

// TestIdempotencyRecordsFailure keeps a failed command from occupying its key
// as though it had succeeded.
func TestIdempotencyRecordsFailure(t *testing.T) {
	repo := &fakeIdempotencyRepo{}
	router := newRouter(repo, func(c *gin.Context) {
		httpx.Respond(c, shared.PreconditionFailed("academic_year.closed",
			"the academic year is financially closed"))
	})

	rec := post(t, router, `{"amount":1}`, testKey)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
	if len(repo.completions) != 0 {
		t.Errorf("a failed command must not be recorded as complete: %+v", repo.completions)
	}
	if len(repo.failures) != 1 {
		t.Fatalf("expected one recorded failure, got %d", len(repo.failures))
	}
	if got := repo.failures[0].errorCode; got != "academic_year.closed" {
		t.Errorf("recorded error code: got %q, want academic_year.closed", got)
	}
}
