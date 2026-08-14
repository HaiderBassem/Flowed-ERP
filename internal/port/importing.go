package port

import (
	"context"
	"time"

	"github.com/swibit/flowed/internal/domain/shared"
)

// ---------------------------------------------------------------------------
// Staged imports
// ---------------------------------------------------------------------------

// ImportBatchStatus is where a staged import has reached.
//
// The values are exactly the ones the schema's CHECK constraint permits, and
// the order below is the workflow: a spreadsheet is uploaded, validated,
// reviewed by a human, confirmed, and only then does anything get created.
type ImportBatchStatus string

const (
	// BatchUploaded holds parsed rows that nothing has looked at yet.
	BatchUploaded ImportBatchStatus = "uploaded"
	// BatchValidating is the validation pass in flight.
	BatchValidating ImportBatchStatus = "validating"
	// BatchNeedsReview has per-row findings waiting for a human.
	BatchNeedsReview ImportBatchStatus = "needs_review"
	// BatchConfirmed is approved and queued. Reachable only with zero
	// unresolved error rows.
	BatchConfirmed ImportBatchStatus = "confirmed"
	// BatchImporting is the worker walking the rows.
	BatchImporting ImportBatchStatus = "importing"
	// BatchImported finished with every row processed.
	BatchImported ImportBatchStatus = "imported"
	// BatchCompletedWithSkips finished, but not every row was applied. It is a
	// distinct terminal state because a bare "done" over a batch that dropped
	// forty rows reads as "we covered everyone" when we did not.
	BatchCompletedWithSkips ImportBatchStatus = "completed_with_skips"
	// BatchFailed could not be run at all, or every row failed.
	BatchFailed ImportBatchStatus = "failed"
	// BatchCancelled was abandoned before execution.
	BatchCancelled ImportBatchStatus = "cancelled"
)

// ImportBatchType names what a batch loads. The schema constrains it to this
// set.
const (
	// BatchTypeStudents loads student identities together with one enrollment
	// each. It never creates financial accounts.
	BatchTypeStudents = "students"
	// BatchTypeEnrollments loads registrations for students already on file.
	BatchTypeEnrollments = "enrollments"
	// BatchTypeAccounts is reserved for a staged financial load.
	BatchTypeAccounts = "accounts"
	// BatchTypeDiscounts is reserved for a staged discount grant load.
	BatchTypeDiscounts = "discounts"
)

// ImportRowStatus is the validation and processing state of one row.
type ImportRowStatus string

const (
	// RowPending has not been validated yet.
	RowPending ImportRowStatus = "pending"
	// RowValid passed validation cleanly.
	RowValid ImportRowStatus = "valid"
	// RowWarning is importable but carries something a reviewer should read.
	RowWarning ImportRowStatus = "warning"
	// RowError cannot be imported as it stands.
	RowError ImportRowStatus = "error"
	// RowProcessed was applied.
	RowProcessed ImportRowStatus = "processed"
	// RowSkipped was deliberately not applied, and the reason is recorded.
	RowSkipped ImportRowStatus = "skipped"
	// RowFailed was attempted and the command refused it.
	RowFailed ImportRowStatus = "failed"
)

// ImportDisposition is what the import will do with a row: the decision a
// reviewer sees and may override.
type ImportDisposition string

const (
	// DispositionCreate creates a new record.
	DispositionCreate ImportDisposition = "create"
	// DispositionUpdate attaches to an existing record. For a student row this
	// means enrolling the person already on file, never rewriting their
	// identity: a name change is a documented command of its own.
	DispositionUpdate ImportDisposition = "update"
	// DispositionSkip leaves the row alone and says why.
	DispositionSkip ImportDisposition = "skip"
	// DispositionError blocks the batch until a reviewer resolves it.
	DispositionError ImportDisposition = "error"
)

// ImportFinding is one validation remark against a row.
//
// Findings are structured rather than a flat message so that a review screen
// can group by code and point at the offending column, and so that a
// reviewer's override can be justified against a specific finding.
type ImportFinding struct {
	Field   string `json:"field,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImportBatch is one staged spreadsheet.
type ImportBatch struct {
	ID             shared.ID
	BatchType      string
	SourceFilename *string
	Status         ImportBatchStatus
	AcademicYearID *shared.ID

	TotalRows   int
	ValidRows   int
	ErrorRows   int
	CreatedRows int
	UpdatedRows int
	SkippedRows int
	FailedRows  int

	// HeartbeatAt is bumped as the worker progresses. A batch whose heartbeat
	// stopped is a stalled worker, which is what makes resumption possible
	// rather than leaving the batch stuck in "importing" with nobody to notice.
	HeartbeatAt  *time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	ErrorSummary *string

	CreatedAt   time.Time
	CreatedBy   *shared.ID
	ConfirmedBy *shared.ID
	ConfirmedAt *time.Time
}

// ImportRow is one line of the spreadsheet with everything the workflow has
// learned about it.
type ImportRow struct {
	ID      shared.ID
	BatchID shared.ID
	RowNo   int
	// RawData is the parsed line exactly as it arrived. Arabic names and
	// Arabic-Indic phone digits are stored unfolded: the database derives its
	// searchable forms in generated columns, so nothing here is normalised and
	// the original stays reproducible.
	RawData   map[string]any
	DedupHash string

	ValidationStatus ImportRowStatus
	Disposition      ImportDisposition
	Errors           []ImportFinding
	Warnings         []ImportFinding

	// PreviewHash records what the preview showed. The commit pass recomputes
	// and compares, so a row whose outcome moved after a human approved it is
	// skipped rather than silently applied under new configuration.
	PreviewHash *string
	// MatchedEntityID is the record the row resolved to — for a student row,
	// the person, whether found or created.
	MatchedEntityID *shared.ID
	// CreatedEntityID is what the row produced — for a student row, the
	// enrollment.
	CreatedEntityID *shared.ID
	ProcessedAt     *time.Time
	ErrorMessage    *string
}

// HasErrors reports whether validation rejected the row.
func (r *ImportRow) HasErrors() bool { return len(r.Errors) > 0 }

// IsUnprocessed reports whether the worker still owes this row an attempt.
// Both statuses are the ones the schema's partial index covers, so the queue
// query and this predicate cannot drift apart.
func (r *ImportRow) IsUnprocessed() bool {
	return r.ValidationStatus == RowValid || r.ValidationStatus == RowWarning
}

// ImportRowFilter narrows a query over a batch's rows.
type ImportRowFilter struct {
	Statuses     []ImportRowStatus
	Dispositions []ImportDisposition
	// OnlyProblems restricts to rows a reviewer must act on, which is what a
	// review screen opens with.
	OnlyProblems bool
	Limit        int
	Offset       int
}

// ImportRowCounts is the tally a batch reports.
//
// It is recomputed from the rows rather than accumulated in memory, because a
// batch resumed after a worker died must report every row's outcome, not only
// the ones the surviving pass happened to touch.
type ImportRowCounts struct {
	Total     int
	Pending   int
	Valid     int
	Warning   int
	Error     int
	Processed int
	Skipped   int
	Failed    int
	// Created and Updated split the processed rows by what they did.
	Created int
	Updated int
	// UnresolvedErrors counts error rows a reviewer has not dispositioned away.
	// Confirmation is gated on this reaching zero.
	UnresolvedErrors int
}

// ImportRepository stores staged imports and their rows.
//
// It is a separate port from the rest because an import batch is not domain
// state: it is the paperwork of getting domain state in. Nothing in billing or
// academic reads it.
type ImportRepository interface {
	CreateBatch(ctx context.Context, b *ImportBatch) error
	UpdateBatch(ctx context.Context, b *ImportBatch) error
	GetBatch(ctx context.Context, id shared.ID) (*ImportBatch, error)
	// GetBatchForUpdate locks the batch row, which serialises two workers that
	// both believe they should be running it.
	GetBatchForUpdate(ctx context.Context, id shared.ID) (*ImportBatch, error)
	ListBatches(ctx context.Context, status *ImportBatchStatus, limit, offset int) ([]*ImportBatch, int, error)
	// Heartbeat records that the worker is still alive, without rewriting the
	// rest of the batch row.
	Heartbeat(ctx context.Context, batchID shared.ID, at time.Time) error
	// StalledBatches lists importing batches whose heartbeat stopped, for the
	// reaper that hands them to ResumeImport.
	StalledBatches(ctx context.Context, silentSince time.Time, limit int) ([]*ImportBatch, error)

	// InsertRows stages parsed rows. Rows colliding on the batch's dedup hash
	// are dropped and counted, so re-staging the same file does not duplicate
	// work — and the count is returned rather than swallowed, because a silent
	// drop reads as a successful load of rows that were never loaded.
	InsertRows(ctx context.Context, rows []*ImportRow) (inserted int, err error)
	UpdateRow(ctx context.Context, r *ImportRow) error
	GetRow(ctx context.Context, batchID shared.ID, rowNo int) (*ImportRow, error)
	ListRows(ctx context.Context, batchID shared.ID, f ImportRowFilter) ([]*ImportRow, int, error)
	// UnprocessedRows returns the worker's queue in file order: exactly the
	// rows a resumed run still owes an attempt.
	UnprocessedRows(ctx context.Context, batchID shared.ID, limit int) ([]*ImportRow, error)
	CountRows(ctx context.Context, batchID shared.ID) (ImportRowCounts, error)
	// StudentNumberOccurrences counts how many times each student number
	// appears in a batch, which is how a duplicate inside one file is caught
	// before any of its rows are applied.
	StudentNumberOccurrences(ctx context.Context, batchID shared.ID) (map[string]int, error)
}
