// Package app holds the application services: one method per domain command.
//
// These are deliberately not CRUD. `PUT /payment/{id}` is an invitation to
// rewrite history; `RecordPayment` is an intention with preconditions, an
// authority check, a transaction boundary and an audit entry. Every write to
// an enrollment, an account, an installment, a payment or a discount goes
// through a named command here. Reads are plain queries and may go straight to
// a repository.
//
// Each command follows the same shape, and the order matters:
//
//  1. Check the actor's authority, before anything is read or locked.
//  2. Open the transaction.
//  3. Take row locks in the fixed order: account, then academic year, then
//     number series. Every command uses this order, so no two can deadlock.
//  4. Re-read state under the lock and re-check the preconditions. Anything
//     checked before the lock was a courtesy; this is the check that counts.
//  5. Mutate through domain methods, which carry the invariants.
//  6. Append the audit entry.
package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/observability"
	"github.com/swibit/flowed/internal/port"
)

// Deps is everything the services need, wired once at start-up.
type Deps struct {
	Tx           port.TxManager
	Students     port.StudentRepository
	Years        port.AcademicYearRepository
	Enrollments  port.EnrollmentRepository
	Reference    port.ReferenceRepository
	FeePolicies  port.FeePolicyRepository
	Templates    port.InstallmentTemplateRepository
	Discounts    port.DiscountRepository
	Accounts     port.AccountRepository
	Installments port.InstallmentRepository
	Payments     port.PaymentRepository
	Refunds      port.RefundRepository
	VoidRequests port.VoidRequestRepository
	Series       port.NumberSeriesRepository
	Sessions     port.CashierSessionRepository
	Audit        port.AuditRepository
	Users        port.UserRepository
	// Lifecycle stores the decisions that end or reshape a student's
	// relationship with the university: graduation clearance, installment plan
	// revisions and identity merges. Nil-safe at every call site, so a service
	// wired without it — a test, a one-shot CLI command — still works, and the
	// records it would have written are simply not written.
	Lifecycle port.LifecycleRepository
	Clock     shared.Clock
	Log       *slog.Logger
	// Metrics records what the commands did. A nil value is a working no-op,
	// so tests and one-shot CLI commands leave it unset and every service
	// records unconditionally — a recording guarded by a nil check at the call
	// site is one that can be forgotten, and a metric that silently stopped
	// being emitted is discovered on the day somebody needed it.
	Metrics *observability.Metrics
}

// auditor writes the trail. Every service embeds it so recording an event is a
// single call rather than a repeated block of field assembly.
type auditor struct {
	repo  port.AuditRepository
	clock shared.Clock
}

func newAuditor(repo port.AuditRepository, clock shared.Clock) auditor {
	return auditor{repo: repo, clock: clock}
}

// record appends an audit entry inside the caller's transaction.
//
// It runs in the same transaction as the change it describes, so the trail can
// never disagree with the data: either both land or neither does. That is also
// why a failure here fails the command — an unaudited financial change is not
// an acceptable outcome.
func (a auditor) record(ctx context.Context, e port.AuditEntry) error {
	if e.ID == shared.NilID {
		e.ID = shared.NewID()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = a.clock.Now()
	}
	if e.RequestID == "" {
		e.RequestID = RequestIDFrom(ctx)
	}
	return a.repo.Append(ctx, e)
}

type requestIDKey struct{}

// WithRequestID threads the request identifier into the context so audit
// entries written deep in a command can be correlated with the HTTP request
// that caused them.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom reads the request identifier, returning "" when absent (a
// background job, for instance).
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// snapshotOf renders an entity for the audit trail's before/after columns.
//
// Errors are swallowed on purpose: a value that will not marshal must not
// abort a financial command. The trail records the failure inline instead, so
// the gap is visible rather than silent.
func snapshotOf(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]string{"_marshal_error": err.Error()}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]string{"_unmarshal_error": err.Error()}
	}
	return out
}

// ptr returns a pointer to a value, for the many optional fields in this
// domain.
func ptr[T any](v T) *T { return &v }

// nowOr returns the clock's time, falling back to the wall clock when a
// service was constructed without one.
func nowOr(c shared.Clock) time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	return c.Now()
}
