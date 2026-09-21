// Package postgres implements the repository interfaces declared in
// internal/port against PostgreSQL, through pgx natively.
//
// Four conventions hold across every repository in this package.
//
// Each method resolves its executor with db.Conn(ctx), which hands back the
// transaction open in the context when there is one and the pool otherwise. A
// service can therefore compose several repository calls into one atomic
// command without any repository knowing it happened.
//
// Every mutating method opens with db.RequireTx. A money path that lost its
// transaction boundary then fails immediately and loudly instead of writing
// half its rows and looking fine until reconciliation night.
//
// Every driver error leaves through pg.WrapQuery, so a caller sees a domain
// error carrying a stable code and the operation that produced it, never a raw
// pgx error and never a SQLSTATE.
//
// Timestamps split two ways. The created_at and updated_at columns belong to
// the database: they are never written and are read back through RETURNING, so
// one clock stamps them. Every other instant comes from the domain and is
// written as COALESCE($n, now()), which keeps a caller that left the field
// unset from storing the Go zero time — a value that reads as the year 1 in
// every report downstream.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
)

// dateOrNil projects a nullable DATE column onto a calendar date. shared.Date
// deliberately implements neither sql.Scanner nor driver.Valuer, so the
// conversion is explicit at every boundary.
func dateOrNil(t *time.Time) *shared.Date {
	if t == nil {
		return nil
	}
	d := shared.DateFromTime(*t)
	return &d
}

// timeOrNil projects a calendar date onto a nullable DATE parameter.
func timeOrNil(d *shared.Date) *time.Time {
	if d == nil {
		return nil
	}
	t := d.Time()
	return &t
}

// instant prepares a domain-supplied timestamp for a COALESCE($n, now())
// parameter: an unset instant becomes NULL and the database supplies the time.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// idOrNil turns the zero identifier into a NULL parameter. Several columns
// carrying an actor reference a real user row, and the system actor holds no
// identifier at all.
func idOrNil(id shared.ID) *shared.ID {
	if shared.IsNil(id) {
		return nil
	}
	return &id
}

// enumPtr projects a nullable text column onto a domain string type.
func enumPtr[T ~string](s *string) *T {
	if s == nil {
		return nil
	}
	v := T(*s)
	return &v
}

// enumValue projects a nullable domain string type onto a text parameter.
func enumValue[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	s := string(*v)
	return &s
}

// toStrings converts a slice of domain string types for a TEXT[] parameter.
func toStrings[T ~string](in []T) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

// fromStrings converts a scanned TEXT[] column into domain string types.
func fromStrings[T ~string](in []string) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	for i, v := range in {
		out[i] = T(v)
	}
	return out
}

// jsonOrNil marshals a value for a JSONB column, mapping a missing value onto
// SQL NULL rather than onto the JSON literal null. The two are different facts:
// "nothing was recorded" against "null was recorded".
func jsonOrNil(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, shared.Internal("json_encode", err, "encoding a JSONB column value")
	}
	return encoded, nil
}

// jsonInto decodes a JSONB column, leaving the target untouched when the
// column was NULL.
func jsonInto(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return shared.Internal("json_decode", err, "decoding a JSONB column value")
	}
	return nil
}

// execBatch runs every queued statement in one round trip and returns the
// first failure. The results are always closed, so a partial failure cannot
// leave the connection holding an unread pipeline.
func execBatch(ctx context.Context, q pg.Executor, batch *pgx.Batch) error {
	results := q.SendBatch(ctx, batch)
	var firstErr error
	for range batch.Len() {
		if _, err := results.Exec(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := results.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// argList accumulates the positional parameters of a dynamically built filter.
// Values never reach the SQL text; only their placeholders do.
type argList struct{ values []any }

// next records a value and returns the placeholder that refers to it.
func (a *argList) next(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

// all returns the accumulated parameters in placeholder order.
func (a *argList) all() []any { return a.values }

// likeEscape neutralises the LIKE metacharacters in a user-supplied term.
// Without it a clerk searching for a student number containing an underscore
// would match every number with any character in that position.
func likeEscape(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := range len(s) {
		switch c := s[i]; c {
		case '\\', '%', '_':
			out = append(out, '\\', c)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// boundedLimit applies a default to an unset page size.
func boundedLimit(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	return limit
}

// formatSeriesNumber renders a receipt number from its series' prefix and
// zero-padding, e.g. prefix "RC-2025-" and padding 6 give "RC-2025-000042".
func formatSeriesNumber(prefix string, number int64, padding int16) string {
	return fmt.Sprintf("%s%0*d", prefix, int(padding), number)
}
