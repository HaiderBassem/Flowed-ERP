package port

import (
	"context"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/settlement"
	"flowed/internal/domain/shared"
)

// SettlementRepository stores imported bank and card statements and what their
// lines matched.
type SettlementRepository interface {
	CreateBatch(ctx context.Context, b *settlement.Batch) error
	UpdateBatch(ctx context.Context, b *settlement.Batch) error
	GetBatch(ctx context.Context, id shared.ID) (*settlement.Batch, error)
	// BatchByContent finds an already-imported statement by its digest, so the
	// same file uploaded twice is recognised rather than reconciled twice.
	BatchByContent(ctx context.Context, sha256 string) (*settlement.Batch, error)
	ListBatches(ctx context.Context, status *settlement.BatchStatus, limit, offset int) ([]*settlement.Batch, int, error)

	CreateLines(ctx context.Context, lines []*settlement.Line) error
	UpdateLine(ctx context.Context, line *settlement.Line) error
	GetLine(ctx context.Context, id shared.ID) (*settlement.Line, error)
	ListLines(ctx context.Context, batchID shared.ID, onlyOpen bool) ([]*settlement.Line, error)

	// CandidatesForReference returns the payments carrying an external
	// reference, which is what matching decides against.
	CandidatesForReference(ctx context.Context, reference string) ([]settlement.Candidate, error)
	// Exceptions lists every line still needing a person, across batches. The
	// register a finance office works from.
	Exceptions(ctx context.Context, limit int) ([]SettlementException, error)
	// UnconfirmedPayments lists posted non-cash collections no statement has
	// confirmed: a receipt was printed and the bank has not said the money
	// arrived.
	UnconfirmedPayments(ctx context.Context, before time.Time, limit int) ([]UnconfirmedPayment, error)
}

// SettlementException is one statement line that did not settle cleanly.
type SettlementException struct {
	LineID      shared.ID
	BatchID     shared.ID
	SourceCode  string
	Filename    string
	LineNo      int
	ExternalRef *string
	Amount      money.Amount
	ValueDate   *shared.Date
	Status      settlement.MatchStatus
	Variance    money.Amount
	PaymentID   *shared.ID
}

// UnconfirmedPayment is a posted non-cash collection with no statement line.
type UnconfirmedPayment struct {
	PaymentID  shared.ID
	ReceiptNo  *string
	AccountID  shared.ID
	StudentID  shared.ID
	Amount     money.Amount
	Reference  *string
	MethodCode string
	PaidAt     time.Time
}

// IntentRepository stores collections begun at an external provider.
type IntentRepository interface {
	Create(ctx context.Context, i *payment.Intent) error
	Update(ctx context.Context, i *payment.Intent) error
	GetByID(ctx context.Context, id shared.ID) (*payment.Intent, error)
	// GetForUpdate locks the intent row. A callback and a status poll can
	// arrive at the same instant, and without the lock both would see an
	// unconfirmed intent and both would post a payment.
	GetForUpdate(ctx context.Context, id shared.ID) (*payment.Intent, error)
	// FindByReference resolves whichever reference the provider echoed back.
	FindByReference(ctx context.Context, providerCode, clientRef, providerRef string) (*payment.Intent, error)
	ListForAccount(ctx context.Context, accountID shared.ID) ([]*payment.Intent, error)
	// ListStale returns open intents past their window, for the job that
	// expires them.
	ListStale(ctx context.Context, before time.Time, limit int) ([]*payment.Intent, error)

	// RecordEvent stores a callback delivery. It returns the stored event and
	// whether this delivery had been seen before: a provider retrying must not
	// produce a second payment, and the uniqueness of the external event id is
	// what makes that decidable.
	RecordEvent(ctx context.Context, e *payment.ProviderEvent) (duplicate bool, err error)
	MarkEventProcessed(ctx context.Context, id shared.ID, outcome string, at time.Time) error
	ListEvents(ctx context.Context, intentID shared.ID) ([]*payment.ProviderEvent, error)
}
