package observability

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// maxRecordedSQL bounds the statement text on a span. The long ones here are
// the reporting queries, and their first two kilobytes already name every
// table and join that matters.
const maxRecordedSQL = 2048

// NewQueryTracer returns a pgx tracer that opens one span per statement, or
// nil when tracing is off — pgx treats a nil tracer as no tracing at all, so
// there is no cost to pay on the query path a deployment did not ask for.
//
// This is what makes a slow request legible. The HTTP span says a payment took
// 900ms; only the child spans say that 850ms of it was one lock wait on the
// account row, which is the difference between a database to tune and a
// contention problem to design out.
func NewQueryTracer(p *Provider) pgx.QueryTracer {
	if !p.TracingEnabled() {
		return nil
	}
	return &queryTracer{tracer: p.Tracer()}
}

type queryTracer struct{ tracer trace.Tracer }

type queryTracerSpanKey struct{}

// TraceQueryStart opens the span.
//
// The statement text is recorded; the arguments never are. Every parameter in
// this system is a student id, a name, an amount or a receipt number, and a
// trace backend is a searchable copy of whatever it is given, held outside the
// database's access control and usually outside its retention policy too. The
// SQL alone says what the query did; the arguments would say who it was done
// to.
func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := data.SQL
	if len(sql) > maxRecordedSQL {
		sql = sql[:maxRecordedSQL]
	}

	operation := operationOf(data.SQL)

	ctx, span := t.tracer.Start(ctx, "postgres "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationName(operation),
			semconv.DBQueryText(sql),
		),
	)
	return context.WithValue(ctx, queryTracerSpanKey{}, span)
}

// TraceQueryEnd closes it.
func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(queryTracerSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	defer span.End()

	if data.Err != nil {
		recordQueryError(span, data.Err)
		return
	}

	if tag := data.CommandTag; tag.String() != "" {
		span.SetAttributes(semconv.DBResponseReturnedRows(int(tag.RowsAffected())))
	}
}

// recordQueryError marks a failed statement without copying the driver's
// message onto the span.
//
// span.RecordError would be the obvious call and is the wrong one here. A
// PostgreSQL error carries a DETAIL line, and for the constraint violations
// this schema raises deliberately that line quotes the offending values —
// "Key (student_no)=(2024001) already exists". Recording the error verbatim
// would publish a student number to the trace backend from the one code path
// most likely to fire in production. The SQLSTATE and the constraint name say
// which rule was broken, which is all a trace needs; who broke it is in the
// database, behind its access control, where it belongs.
func recordQueryError(span trace.Span, err error) {
	// pgx surfaces a cancelled context as a query error. The client hung up or
	// the deadline passed: nothing about the database is wrong, and marking it
	// as an error turns every abandoned request into a red span.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		span.SetAttributes(attribute.String("db.cancellation", err.Error()))
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// A miss on a lookup is an ordinary answer, not a failure.
		span.SetAttributes(attribute.Bool("db.no_rows", true))
		return
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		span.SetStatus(codes.Error, "database error")
		return
	}

	span.SetAttributes(
		semconv.ErrorTypeKey.String("postgres."+pgErr.Code),
		attribute.String("db.response.status_code", pgErr.Code),
	)
	if pgErr.ConstraintName != "" {
		span.SetAttributes(attribute.String("db.constraint", pgErr.ConstraintName))
	}
	span.SetStatus(codes.Error, "SQLSTATE "+pgErr.Code)
}

// operationOf names the span from the statement's leading keyword.
//
// Deliberately the keyword and not the table. A span name becomes a metric
// dimension in most backends, so it has to stay bounded; the full statement is
// already on the span for anybody who needs to know which table it hit.
func operationOf(sql string) string {
	trimmed := strings.TrimLeft(sql, " \t\r\n(")

	// Repositories put their leading comment on its own line; skipping those
	// is what keeps every commented query from being named "--".
	for strings.HasPrefix(trimmed, "--") {
		if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
			trimmed = strings.TrimLeft(trimmed[idx+1:], " \t\r\n(")
			continue
		}
		return "unknown"
	}

	end := strings.IndexAny(trimmed, " \t\r\n(")
	if end < 0 {
		end = len(trimmed)
	}
	keyword := strings.ToUpper(trimmed[:end])

	switch keyword {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "WITH", "BEGIN", "COMMIT",
		"ROLLBACK", "SET", "CREATE", "DROP", "ALTER", "TRUNCATE", "COPY", "VALUES":
		return keyword
	default:
		// An unrecognised leading token is far more likely to be a typo or a
		// generated statement than a new kind of query, and echoing it into a
		// span name is how an unbounded dimension gets in.
		return "unknown"
	}
}
