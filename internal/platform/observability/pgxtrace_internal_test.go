package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordOneQuery runs a statement through the tracer and returns the span it
// produced.
func recordOneQuery(t *testing.T, sql string, args []any, endErr error) tracetest.SpanStub {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := &queryTracer{tracer: provider.Tracer("test")}

	ctx := tracer.TraceQueryStart(context.Background(), nil,
		pgx.TraceQueryStartData{SQL: sql, Args: args})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: endErr})

	spans := tracetest.SpanStubsFromReadOnlySpans(recorder.Ended())
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	return spans[0]
}

// The one that matters. A PostgreSQL constraint violation carries a DETAIL
// line quoting the offending values, and this schema raises those deliberately
// — uq_student_number fires with the student's university number in it.
// Recording the driver error verbatim would publish that number to a trace
// backend from the code path most likely to fire in production.
func TestQueryTracerNeverRecordsTheDriverDetail(t *testing.T) {
	const studentNo = "2024001"

	span := recordOneQuery(t,
		"INSERT INTO student (student_no) VALUES ($1)",
		[]any{studentNo},
		&pgconn.PgError{
			Code:           "23505",
			Message:        `duplicate key value violates unique constraint "uq_student_number"`,
			Detail:         "Key (student_no)=(" + studentNo + ") already exists.",
			ConstraintName: "uq_student_number",
		})

	haystack := spanText(span)
	if strings.Contains(haystack, studentNo) {
		t.Errorf("the student number reached the span:\n%s", haystack)
	}
	if strings.Contains(haystack, "already exists") {
		t.Errorf("the driver's DETAIL line reached the span:\n%s", haystack)
	}
	if len(span.Events) != 0 {
		t.Errorf("the span carries %d events; RecordError would attach the raw message", len(span.Events))
	}

	// What must survive is the part that says which rule was broken.
	if !strings.Contains(haystack, "23505") {
		t.Error("the SQLSTATE is missing; the span cannot say what failed")
	}
	if !strings.Contains(haystack, "uq_student_number") {
		t.Error("the constraint name is missing; the span cannot say which rule fired")
	}
}

// Arguments are never attributes, whatever the statement did.
func TestQueryTracerNeverRecordsArguments(t *testing.T) {
	span := recordOneQuery(t,
		"SELECT * FROM payment WHERE student_id = $1 AND amount = $2",
		[]any{"3f2a...-student", int64(750_000)},
		nil)

	haystack := spanText(span)
	if strings.Contains(haystack, "3f2a") || strings.Contains(haystack, "750000") {
		t.Errorf("query arguments reached the span:\n%s", haystack)
	}
	if !strings.Contains(haystack, "FROM payment") {
		t.Error("the statement text is missing; the span cannot say what ran")
	}
}

// A cancelled context is the client hanging up, not the database failing. Red
// spans for those train everyone to ignore the colour.
func TestQueryTracerDoesNotBlameTheDatabaseForACancellation(t *testing.T) {
	span := recordOneQuery(t, "SELECT 1", nil, context.Canceled)
	if span.Status.Code.String() == "Error" {
		t.Error("a cancelled context was recorded as a database error")
	}
}

// Span names become a dimension in most backends, so the set of them has to be
// finite whatever a repository puts in front of its SQL.
func TestOperationOfKeepsSpanNamesBounded(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"plain select", "SELECT 1", "SELECT"},
		{"lowercase", "insert into student values (1)", "INSERT"},
		{"leading whitespace", "\n\t  UPDATE account SET x = 1", "UPDATE"},
		{"cte", "WITH x AS (SELECT 1) SELECT * FROM x", "WITH"},
		{"parenthesised", "(SELECT 1) UNION (SELECT 2)", "SELECT"},
		{"leading comment", "-- lock the account row\nSELECT 1 FOR UPDATE", "SELECT"},
		{"several comments", "-- one\n-- two\nDELETE FROM x", "DELETE"},
		{"comment only", "-- nothing follows", "unknown"},
		{"empty", "", "unknown"},
		{"unrecognised leading token", "EXPLAIN ANALYZE SELECT 1", "unknown"},
		{"garbage stays bounded", "SELECT_FROM_STUDENT_2024001", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operationOf(tc.sql); got != tc.want {
				t.Errorf("operationOf(%q) = %q, want %q", tc.sql, got, tc.want)
			}
		})
	}
}

// spanText flattens a span into something a leak can be searched for.
func spanText(span tracetest.SpanStub) string {
	var b strings.Builder
	b.WriteString(span.Name)
	b.WriteString("\n")
	b.WriteString(span.Status.Description)
	b.WriteString("\n")
	for _, attr := range span.Attributes {
		b.WriteString(string(attr.Key))
		b.WriteString("=")
		b.WriteString(attr.Value.String())
		b.WriteString("\n")
	}
	for _, event := range span.Events {
		b.WriteString(event.Name)
		b.WriteString("\n")
		for _, attr := range event.Attributes {
			b.WriteString(string(attr.Key))
			b.WriteString("=")
			b.WriteString(attr.Value.String())
			b.WriteString("\n")
		}
	}
	return b.String()
}
