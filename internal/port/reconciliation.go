package port

import (
	"context"
	"time"

	"flowed/internal/domain/shared"
)

// ReconciliationKind names one invariant check.
//
// Separate kinds rather than one pass: the account caches and the audit chain
// fail for unrelated reasons, at unrelated frequencies, and a single combined
// status would hide whichever failed less often.
type ReconciliationKind string

const (
	// ReconcileAccounts compares each account's cached totals against its
	// transaction rows.
	ReconcileAccounts ReconciliationKind = "accounts"
	// ReconcileInstallments compares each installment's cached paid amount
	// against its allocations.
	ReconcileInstallments ReconciliationKind = "installments"
	// ReconcileRefunds looks for a payment refunded beyond what it took.
	ReconcileRefunds ReconciliationKind = "refunds"
	// ReconcileAuditChain re-walks the audit hash chain.
	ReconcileAuditChain ReconciliationKind = "audit_chain"
)

// AllReconciliationKinds is what a full pass covers.
var AllReconciliationKinds = []ReconciliationKind{
	ReconcileAccounts, ReconcileInstallments, ReconcileRefunds, ReconcileAuditChain,
}

// ReconciliationRun is one pass of one check.
type ReconciliationRun struct {
	ID          shared.ID
	Kind        ReconciliationKind
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	RowsChecked int64
	Findings    int
	NewFindings int
	Error       *string
	TriggeredBy *shared.ID
}

// ReconciliationFinding is one invariant violation, tracked from the night it
// appeared to the day somebody closed it.
type ReconciliationFinding struct {
	ID          shared.ID
	FirstRunID  shared.ID
	LastRunID   shared.ID
	Kind        ReconciliationKind
	SubjectType string
	SubjectID   shared.ID
	Detail      map[string]any

	Severity  string
	State     string
	SeenCount int

	FirstSeenAt time.Time
	LastSeenAt  time.Time

	AcknowledgedAt     *time.Time
	AcknowledgedBy     *shared.ID
	AcknowledgedReason *string
	ResolvedAt         *time.Time
	ResolvedBy         *shared.ID
	Resolution         *string
}

// ObservedFinding is one violation as a check just saw it, before it is
// matched against whatever is already open for that subject.
type ObservedFinding struct {
	Kind        ReconciliationKind
	SubjectType string
	SubjectID   shared.ID
	Detail      map[string]any
}

// OpenCount is how many findings of one kind are open at one severity.
type OpenCount struct {
	Severity string
	Kind     ReconciliationKind
	Count    int
}

// ReconciliationRepository stores runs and findings, and reads the invariant
// views the checks are built on.
type ReconciliationRepository interface {
	StartRun(ctx context.Context, run *ReconciliationRun) error
	FinishRun(ctx context.Context, run *ReconciliationRun) error

	// RecordFinding either opens a new finding or records another sighting of
	// one already open for the same subject. It returns true when the finding
	// is new, which is what distinguishes "the same problem, still there" from
	// "another account has started drifting".
	RecordFinding(ctx context.Context, runID shared.ID, observed ObservedFinding,
		escalateAfter int, at time.Time) (isNew bool, err error)

	// ResolveMissing closes findings of this kind that the latest run no
	// longer sees. Drift that stops is drift that was fixed — by a correcting
	// command, or by the deployment that was writing it being replaced — and
	// leaving it open forever would train operators to ignore the queue.
	ResolveMissing(ctx context.Context, kind ReconciliationKind, runID shared.ID,
		stillPresent []shared.ID, at time.Time) (int, error)

	ListRuns(ctx context.Context, kind string, limit int) ([]*ReconciliationRun, error)
	ListFindings(ctx context.Context, state string, limit int) ([]*ReconciliationFinding, error)
	GetFinding(ctx context.Context, id shared.ID) (*ReconciliationFinding, error)
	UpdateFinding(ctx context.Context, finding *ReconciliationFinding) error

	// OpenCounts is how many findings are open, by severity and by check. The
	// check matters to an alert: an open refunds finding means money left the
	// university that never entered it, which is a different night from an
	// account cache that is out by a dinar.
	OpenCounts(ctx context.Context) ([]OpenCount, error)

	// The checks themselves. Each returns what it saw plus how many rows it
	// looked at, because "nothing wrong in 40,000 accounts" and "nothing wrong
	// because the query matched nothing" have to be distinguishable.
	CheckAccounts(ctx context.Context, limit int) ([]ObservedFinding, int64, error)
	CheckInstallments(ctx context.Context, limit int) ([]ObservedFinding, int64, error)
	CheckRefunds(ctx context.Context, limit int) ([]ObservedFinding, int64, error)
	CheckAuditChain(ctx context.Context, limit int) ([]ObservedFinding, int64, error)
}
