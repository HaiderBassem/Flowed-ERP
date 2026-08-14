package httpx_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
	"github.com/swibit/flowed/internal/platform/logger"
)

// respondTo runs Respond for one error and returns the response together with
// everything the handler logged.
func respondTo(t *testing.T, err error) (*httptest.ResponseRecorder, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var logs bytes.Buffer
	captured := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	router := gin.New()
	router.Use(httpx.RequestID())
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.Into(c.Request.Context(), captured))
		c.Next()
	})
	router.GET("/accounts/:id", func(c *gin.Context) { httpx.Respond(c, err) })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/accounts/42", nil))
	return rec, logs.String()
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) httpx.ErrorBody {
	t.Helper()
	var envelope httpx.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return envelope.Error
}

func TestStatusForKind(t *testing.T) {
	cases := map[shared.Kind]int{
		shared.KindValidation:         http.StatusBadRequest,
		shared.KindUnauthorized:       http.StatusUnauthorized,
		shared.KindForbidden:          http.StatusForbidden,
		shared.KindNotFound:           http.StatusNotFound,
		shared.KindConflict:           http.StatusConflict,
		shared.KindPreconditionFailed: http.StatusUnprocessableEntity,
		shared.KindInvariantViolation: http.StatusInternalServerError,
		shared.KindInternal:           http.StatusInternalServerError,
	}
	for kind, want := range cases {
		if got := httpx.StatusForKind(kind); got != want {
			t.Errorf("%s: got %d, want %d", kind, got, want)
		}
	}
}

// TestRespondHidesInternalDetail is the leak this mapping exists to prevent: a
// wrapped driver error quotes the statement and the row that broke.
func TestRespondHidesInternalDetail(t *testing.T) {
	cause := errors.New(`ERROR: duplicate key value violates unique constraint "uq_payment_receipt" (SQLSTATE 23505)`)
	err := shared.Internal("database_error", cause,
		"insert into payment (account_id, amount) values ($1, $2) failed").
		WithDetail("db_detail", "Key (receipt_no)=(R-000001) already exists.")

	rec, logs := respondTo(t, err)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}

	raw := rec.Body.String()
	for _, leak := range []string{"insert into", "SQLSTATE", "uq_payment_receipt", "R-000001"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the response leaks %q: %s", leak, raw)
		}
	}

	body := decodeBody(t, rec)
	if body.Details != nil {
		t.Errorf("expected no details on an internal error, got %v", body.Details)
	}
	if body.RequestID == "" {
		t.Error("expected the request id in the body so a user can quote it")
	}
	if body.Code != "database_error" {
		t.Errorf("expected the stable code to survive, got %q", body.Code)
	}

	// The operator's copy must keep everything the client's copy dropped.
	if !strings.Contains(logs, "SQLSTATE") {
		t.Errorf("the log record lost the real cause: %s", logs)
	}
	if !strings.Contains(logs, body.RequestID) {
		t.Error("the log record does not carry the request id shown to the client")
	}
}

// TestRespondMarksInvariantViolations gives the alert something to fire on.
func TestRespondMarksInvariantViolations(t *testing.T) {
	err := shared.InvariantViolation("account.totals_disagree",
		"allocations exceed the payment they belong to")

	rec, logs := respondTo(t, err)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if !strings.Contains(logs, `"invariant_violation":true`) {
		t.Errorf("the log record is missing the invariant marker: %s", logs)
	}
	if strings.Contains(rec.Body.String(), "allocations exceed") {
		t.Errorf("the internal message reached the client: %s", rec.Body.String())
	}
}

// TestRespondKeepsBusinessDetail proves the sanitising above is narrow: a
// cashier still gets the numbers they need on screen.
func TestRespondKeepsBusinessDetail(t *testing.T) {
	err := shared.PreconditionFailed("payment.exceeds_outstanding",
		"the payment exceeds what remains on the account").
		WithDetail("outstanding_iqd", 150000).
		WithDetail("db_detail", "Key (id)=(...) internal")

	rec, _ := respondTo(t, err)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", rec.Code)
	}
	body := decodeBody(t, rec)
	if body.Message != "the payment exceeds what remains on the account" {
		t.Errorf("the business message was altered: %q", body.Message)
	}
	if _, ok := body.Details["outstanding_iqd"]; !ok {
		t.Errorf("the business detail was dropped: %v", body.Details)
	}
	if _, ok := body.Details["db_detail"]; ok {
		t.Errorf("the raw driver detail reached the client: %v", body.Details)
	}
}

// TestRespondTreatsForeignErrorsAsInternal keeps an unclassified error from
// being answered with an accidental 200 or a leaked message.
func TestRespondTreatsForeignErrorsAsInternal(t *testing.T) {
	rec, _ := respondTo(t, errors.New("pq: connection to 10.0.0.5:5432 refused"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("the response leaks infrastructure detail: %s", rec.Body.String())
	}
}
