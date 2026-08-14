package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/settlement"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// SettlementService imports bank and card statements and reconciles them
// against what this system recorded collecting.
//
// The control it implements is the oldest one in accounting and the one this
// system was missing: two independent records of the same money, compared. The
// ledger says a receipt was issued; the statement says money arrived. Where
// they agree, nothing needs doing. Where they disagree, somebody has to look —
// and the whole design here is about making sure somebody does, rather than
// making the difference disappear.
type SettlementService struct {
	deps  Deps
	repo  port.SettlementRepository
	audit auditor
}

// NewSettlementService wires the reconciliation commands.
func NewSettlementService(d Deps, repo port.SettlementRepository) *SettlementService {
	return &SettlementService{deps: d, repo: repo, audit: newAuditor(d.Audit, d.Clock)}
}

// StatementRow is one parsed row of an uploaded statement.
type StatementRow struct {
	LineNo      int
	ExternalRef string
	Amount      money.Amount
	ValueDate   *shared.Date
	Description string
	Raw         map[string]any
}

// ImportStatementInput is an uploaded statement.
type ImportStatementInput struct {
	SourceCode string
	SourceName *string
	Filename   string
	// Content is the file as uploaded. Its digest is what recognises the same
	// statement uploaded twice, which is a routine mistake at a desk and would
	// otherwise reconcile one day's money twice.
	Content       []byte
	StatementFrom *shared.Date
	StatementTo   *shared.Date
	Rows          []StatementRow
	Notes         *string
}

// ImportStatementResult reports what the import found.
type ImportStatementResult struct {
	Batch *settlement.Batch
	Lines []*settlement.Line
	// Exceptions counts the lines a person still has to work.
	Exceptions int
}

// ImportStatement records a statement and matches its lines.
//
// Matching runs inside the import rather than as a separate step because a
// statement imported and not matched is worse than one not imported: it looks
// done. What it produces is a finding per line, never a change to a payment —
// correcting the ledger is a void, a refund or an adjustment, through the
// commands that already exist for those.
func (s *SettlementService) ImportStatement(ctx context.Context, actor shared.Actor, in ImportStatementInput) (*ImportStatementResult, error) {
	if err := actor.RequireAnyRole("ImportStatement",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.SourceCode) == "" {
		return nil, shared.Validation("settlement.source_required",
			"say which bank or terminal produced this statement")
	}
	if len(in.Rows) == 0 {
		return nil, shared.Validation("settlement.empty_statement",
			"the statement has no rows to reconcile")
	}

	digest := sha256.Sum256(in.Content)
	contentHash := hex.EncodeToString(digest[:])

	result := &ImportStatementResult{}
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if existing, err := s.repo.BatchByContent(ctx, contentHash); err == nil && existing != nil {
			return shared.Conflict("settlement.already_imported",
				"this statement was already imported on %s as %s",
				existing.Filename, existing.ID).
				WithDetail("batch_id", existing.ID.String()).
				WithDetail("remedy", "open the existing import; reconciling one statement twice "+
					"would confirm the same money against two sets of lines")
		} else if err != nil && shared.KindOf(err) != shared.KindNotFound {
			return err
		}

		batch := &settlement.Batch{
			ID:            shared.NewID(),
			SourceCode:    strings.ToUpper(strings.TrimSpace(in.SourceCode)),
			SourceName:    in.SourceName,
			Filename:      in.Filename,
			ContentSHA256: contentHash,
			StatementFrom: in.StatementFrom,
			StatementTo:   in.StatementTo,
			Status:        settlement.BatchMatching,
			Notes:         in.Notes,
			UploadedBy:    &actor.UserID,
		}
		if err := s.repo.CreateBatch(ctx, batch); err != nil {
			return err
		}

		lines := make([]*settlement.Line, 0, len(in.Rows))
		for i, row := range in.Rows {
			line := &settlement.Line{
				ID:          shared.NewID(),
				BatchID:     batch.ID,
				LineNo:      row.LineNo,
				ExternalRef: strings.TrimSpace(row.ExternalRef),
				Amount:      row.Amount,
				ValueDate:   row.ValueDate,
				Description: row.Description,
				Raw:         row.Raw,
				Status:      settlement.StatusUnmatched,
			}
			if line.LineNo == 0 {
				line.LineNo = i + 1
			}

			decision, err := s.decide(ctx, *line)
			if err != nil {
				return err
			}
			line.Status = decision.Status
			line.MatchedID = decision.PaymentID
			line.Variance = decision.Variance
			if decision.Note != "" {
				line.ReviewNote = ptr(decision.Note)
			}
			lines = append(lines, line)
		}

		if err := s.repo.CreateLines(ctx, lines); err != nil {
			return err
		}

		value := make([]settlement.Line, 0, len(lines))
		for _, line := range lines {
			value = append(value, *line)
		}
		count, matched, total, matchedTotal := settlement.Summarise(value)
		batch.LineCount, batch.MatchedCount = count, matched
		batch.TotalAmount, batch.MatchedAmount = total, matchedTotal
		batch.Status = settlement.StatusAfterMatching(value)
		if batch.Status == settlement.BatchReconciled {
			batch.ReconciledBy = &actor.UserID
		}
		if err := s.repo.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		result.Batch = batch
		result.Lines = lines
		for _, line := range value {
			if line.Status.NeedsReview() {
				result.Exceptions++
			}
		}

		return s.audit.record(ctx, port.AuditEntry{
			EntityType: "settlement_batch",
			EntityID:   &batch.ID,
			Action:     "settlement.imported",
			Actor:      actor,
			After:      snapshotOf(batch),
			Metadata: map[string]any{
				"source":         batch.SourceCode,
				"filename":       batch.Filename,
				"lines":          count,
				"matched":        matched,
				"exceptions":     result.Exceptions,
				"total_amount":   total.Int64(),
				"matched_amount": matchedTotal.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// decide runs one line through the matcher.
func (s *SettlementService) decide(ctx context.Context, line settlement.Line) (settlement.Decision, error) {
	if strings.TrimSpace(line.ExternalRef) == "" {
		return settlement.Match(line, nil), nil
	}
	candidates, err := s.repo.CandidatesForReference(ctx, line.ExternalRef)
	if err != nil {
		return settlement.Decision{}, err
	}
	return settlement.Match(line, candidates), nil
}

// RematchBatch runs matching again over a batch's open lines.
//
// The case this exists for: a statement imported in the morning contains a
// transfer the cashier only enters at noon. Nothing was wrong with either; the
// two simply arrived in the wrong order, and re-running matching pairs them
// without anyone editing a line by hand.
func (s *SettlementService) RematchBatch(ctx context.Context, actor shared.Actor, batchID shared.ID) (*ImportStatementResult, error) {
	if err := actor.RequireAnyRole("RematchBatch",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	result := &ImportStatementResult{}
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		batch, err := s.repo.GetBatch(ctx, batchID)
		if err != nil {
			return err
		}
		if batch.Status == settlement.BatchCancelled {
			return shared.PreconditionFailed("settlement.batch_cancelled",
				"this statement was discarded and is not matched again")
		}

		lines, err := s.repo.ListLines(ctx, batchID, false)
		if err != nil {
			return err
		}

		changed := 0
		for _, line := range lines {
			// A line somebody resolved by hand is left alone. Re-deciding it
			// would discard a human judgement in favour of an automatic one,
			// which is the wrong way round.
			if !line.Status.NeedsReview() {
				continue
			}
			decision, err := s.decide(ctx, *line)
			if err != nil {
				return err
			}
			if decision.Status == line.Status && decision.PaymentID == nil {
				continue
			}
			line.Status = decision.Status
			line.MatchedID = decision.PaymentID
			line.Variance = decision.Variance
			if decision.Note != "" {
				line.ReviewNote = ptr(decision.Note)
			}
			if err := s.repo.UpdateLine(ctx, line); err != nil {
				return err
			}
			changed++
		}

		value := make([]settlement.Line, 0, len(lines))
		for _, line := range lines {
			value = append(value, *line)
		}
		count, matched, total, matchedTotal := settlement.Summarise(value)
		batch.LineCount, batch.MatchedCount = count, matched
		batch.TotalAmount, batch.MatchedAmount = total, matchedTotal
		batch.Status = settlement.StatusAfterMatching(value)
		if batch.Status == settlement.BatchReconciled {
			batch.ReconciledBy = &actor.UserID
		}
		if err := s.repo.UpdateBatch(ctx, batch); err != nil {
			return err
		}

		result.Batch = batch
		result.Lines = lines
		for _, line := range value {
			if line.Status.NeedsReview() {
				result.Exceptions++
			}
		}

		return s.audit.record(ctx, port.AuditEntry{
			EntityType: "settlement_batch",
			EntityID:   &batch.ID,
			Action:     "settlement.rematched",
			Actor:      actor,
			Metadata: map[string]any{
				"lines_changed": changed,
				"exceptions":    result.Exceptions,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ResolveLineInput is a person's decision about a statement line.
type ResolveLineInput struct {
	LineID    shared.ID
	Status    settlement.MatchStatus
	PaymentID *shared.ID
	Note      string
}

// ResolveLine records what a person decided about a line.
//
// Two things it deliberately does not do. It does not create a payment: money
// the bank received that the system never recorded is collected through
// RecordPayment like any other, and this line then matches it on the next run.
// And it does not adjust an amount: a variance is settled by a refund or an
// adjustment against the account, not by editing the reconciliation until the
// two sides agree.
func (s *SettlementService) ResolveLine(ctx context.Context, actor shared.Actor, in ResolveLineInput) (*settlement.Line, error) {
	if err := actor.RequireAnyRole("ResolveSettlementLine",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if err := settlement.ValidateResolution(in.Status, in.PaymentID, in.Note); err != nil {
		return nil, err
	}

	var line *settlement.Line
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		line, err = s.repo.GetLine(ctx, in.LineID)
		if err != nil {
			return err
		}
		before := snapshotOf(line)

		if in.PaymentID != nil {
			paid, err := s.deps.Payments.GetByID(ctx, *in.PaymentID)
			if err != nil {
				return err
			}
			// Matching a line to a voided receipt would record that money
			// arrived against a collection the university has disowned.
			if paid.Status == "voided" {
				return shared.PreconditionFailed("settlement.payment_voided",
					"receipt %s was voided and cannot settle a statement line",
					valueOr(paid.ReceiptNo)).
					WithDetail("remedy", "record the collection again, then match this line to it")
			}
			variance, err := line.Amount.Sub(paid.Amount)
			if err != nil {
				return err
			}
			line.Variance = variance
			if variance.IsZero() {
				line.Status = settlement.StatusMatched
			} else {
				line.Status = settlement.StatusVariance
			}
			line.MatchedID = in.PaymentID
		} else {
			line.Status = in.Status
			line.MatchedID = nil
			line.Variance = 0
		}

		if in.Note != "" {
			line.ReviewNote = ptr(in.Note)
		}
		line.ReviewedBy = &actor.UserID

		if err := s.repo.UpdateLine(ctx, line); err != nil {
			return err
		}
		if err := s.refreshBatch(ctx, actor, line.BatchID); err != nil {
			return err
		}

		return s.audit.record(ctx, port.AuditEntry{
			EntityType: "settlement_line",
			EntityID:   &line.ID,
			Action:     "settlement.line_resolved",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(line),
			Reason:     reasonOrNil(in.Note),
			Metadata: map[string]any{
				"status":   string(line.Status),
				"variance": line.Variance.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return line, nil
}

// refreshBatch recomputes a batch header after one of its lines changed.
func (s *SettlementService) refreshBatch(ctx context.Context, actor shared.Actor, batchID shared.ID) error {
	batch, err := s.repo.GetBatch(ctx, batchID)
	if err != nil {
		return err
	}
	lines, err := s.repo.ListLines(ctx, batchID, false)
	if err != nil {
		return err
	}

	value := make([]settlement.Line, 0, len(lines))
	for _, line := range lines {
		value = append(value, *line)
	}
	count, matched, total, matchedTotal := settlement.Summarise(value)
	batch.LineCount, batch.MatchedCount = count, matched
	batch.TotalAmount, batch.MatchedAmount = total, matchedTotal
	batch.Status = settlement.StatusAfterMatching(value)
	if batch.Status == settlement.BatchReconciled {
		batch.ReconciledBy = &actor.UserID
	}
	return s.repo.UpdateBatch(ctx, batch)
}

// ListBatches returns imported statements.
func (s *SettlementService) ListBatches(
	ctx context.Context, actor shared.Actor, status *settlement.BatchStatus, limit, offset int,
) ([]*settlement.Batch, int, error) {
	if err := actor.RequireAnyRole("ListSettlementBatches",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor); err != nil {
		return nil, 0, err
	}
	return s.repo.ListBatches(ctx, status, limit, offset)
}

// GetBatch returns one imported statement with its lines.
func (s *SettlementService) GetBatch(
	ctx context.Context, actor shared.Actor, id shared.ID, onlyOpen bool,
) (*settlement.Batch, []*settlement.Line, error) {
	if err := actor.RequireAnyRole("GetSettlementBatch",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor); err != nil {
		return nil, nil, err
	}
	batch, err := s.repo.GetBatch(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	lines, err := s.repo.ListLines(ctx, id, onlyOpen)
	if err != nil {
		return nil, nil, err
	}
	return batch, lines, nil
}

// Exceptions lists every line still needing a person, across batches.
func (s *SettlementService) Exceptions(ctx context.Context, actor shared.Actor, limit int) ([]port.SettlementException, error) {
	if err := actor.RequireAnyRole("SettlementExceptions",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor); err != nil {
		return nil, err
	}
	return s.repo.Exceptions(ctx, limit)
}

// UnconfirmedPayments lists posted non-cash collections no statement confirms.
//
// The other direction of the same control, and the one that catches a receipt
// issued for money that never arrived. The grace period keeps a transfer posted
// this morning out of the list: it has not had time to appear on a statement,
// and a register full of noise is a register nobody reads.
func (s *SettlementService) UnconfirmedPayments(
	ctx context.Context, actor shared.Actor, grace time.Duration, limit int,
) ([]port.UnconfirmedPayment, error) {
	if err := actor.RequireAnyRole("UnconfirmedPayments",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor); err != nil {
		return nil, err
	}
	if grace <= 0 {
		grace = 72 * time.Hour
	}
	return s.repo.UnconfirmedPayments(ctx, nowOr(s.deps.Clock).Add(-grace), limit)
}

func valueOr(s *string) string {
	if s == nil {
		return "(unnumbered)"
	}
	return *s
}
