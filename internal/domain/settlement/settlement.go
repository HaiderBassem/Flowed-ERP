// Package settlement holds the rules for reconciling what a bank or a card
// terminal says it received against what this system recorded collecting.
//
// The two sides are evidence and ledger. The ledger is authoritative about what
// the university charged and what receipt it issued; the statement is
// authoritative about what money actually moved. Where they agree there is
// nothing to do. Where they disagree, one of four things happened, and naming
// which is the whole job:
//
//   - the bank received money the system never recorded — a collection nobody
//     credited to a student, and the student is still being chased for it;
//   - the system recorded money the bank never received — a receipt issued for
//     a transfer that failed, which is the shape of a fraud and also the shape
//     of an honest typo;
//   - both sides recorded it, for different amounts;
//   - both sides recorded it, but the system recorded it twice.
//
// Nothing in this package writes anything. It decides what a line means; the
// application posts the consequences through the ordinary commands, so a
// correction discovered here is a void or an adjustment like any other.
package settlement

import (
	"strings"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// MatchStatus is what became of one statement line.
type MatchStatus string

const (
	// StatusUnmatched means no payment carries this reference. The line is
	// money the bank says arrived that the system has no record of.
	StatusUnmatched MatchStatus = "unmatched"
	// StatusMatched means exactly one payment carries this reference, for this
	// amount.
	StatusMatched MatchStatus = "matched"
	// StatusVariance means one payment carries this reference for a different
	// amount. Never resolved automatically: the difference is either a bank
	// charge, a partial transfer or a mistake, and only a person can say.
	StatusVariance MatchStatus = "variance"
	// StatusDuplicate means more than one payment carries this reference,
	// which is the shape of one bank slip entered twice.
	StatusDuplicate MatchStatus = "duplicate"
	// StatusIgnored means somebody set the line aside with a written reason —
	// a bank charge, an inter-account transfer, a line that belongs to another
	// system entirely.
	StatusIgnored MatchStatus = "ignored"
)

// NeedsReview reports whether a line still requires a human.
func (s MatchStatus) NeedsReview() bool {
	switch s {
	case StatusUnmatched, StatusVariance, StatusDuplicate:
		return true
	default:
		return false
	}
}

// Line is one row of an imported statement.
type Line struct {
	ID          shared.ID
	BatchID     shared.ID
	LineNo      int
	ExternalRef string
	Amount      money.Amount
	ValueDate   *shared.Date
	Description string
	Raw         map[string]any

	Status     MatchStatus
	MatchedID  *shared.ID
	Variance   money.Amount
	ReviewNote *string
	ReviewedBy *shared.ID
}

// Candidate is a payment a line might belong to, reduced to what matching
// needs. The application supplies these from the ledger; this package does not
// know how they were found.
type Candidate struct {
	PaymentID shared.ID
	Amount    money.Amount
	// Voided candidates are carried rather than filtered out by the caller so
	// that a line matching only voided payments can say so: "the bank received
	// this and we voided the receipt" is a different problem from "we never
	// saw it".
	Voided bool
}

// Decision is what matching concluded about one line.
type Decision struct {
	Status    MatchStatus
	PaymentID *shared.ID
	Variance  money.Amount
	// Note explains a decision a reader would otherwise have to reconstruct.
	Note string
}

// Match decides what a statement line means against the payments that carry
// its reference.
//
// Deliberately conservative. It will mark a line matched only when exactly one
// live payment carries the reference and the amounts agree to the dinar;
// everything else is handed to a person. An automatic matcher that guessed
// would be right most of the time, and the times it was wrong would be
// invisible — which is the opposite of what a reconciliation is for.
func Match(line Line, candidates []Candidate) Decision {
	if strings.TrimSpace(line.ExternalRef) == "" {
		return Decision{
			Status: StatusUnmatched,
			Note:   "the statement line carries no reference to match on",
		}
	}

	var live []Candidate
	voided := 0
	for _, c := range candidates {
		if c.Voided {
			voided++
			continue
		}
		live = append(live, c)
	}

	switch {
	case len(live) == 0 && voided > 0:
		return Decision{
			Status: StatusUnmatched,
			Note:   "every payment carrying this reference was voided; the money may still have arrived",
		}
	case len(live) == 0:
		return Decision{
			Status: StatusUnmatched,
			Note:   "no payment carries this reference",
		}
	case len(live) > 1:
		return Decision{
			Status: StatusDuplicate,
			Note:   "more than one live payment carries this reference",
		}
	}

	payment := live[0]
	if payment.Amount == line.Amount {
		return Decision{Status: StatusMatched, PaymentID: &payment.PaymentID}
	}

	// Positive when the bank received more than the receipt says.
	variance, err := line.Amount.Sub(payment.Amount)
	if err != nil {
		return Decision{
			Status: StatusVariance,
			Note:   "the amounts differ by more than can be represented",
		}
	}
	return Decision{
		Status:    StatusVariance,
		PaymentID: &payment.PaymentID,
		Variance:  variance,
		Note:      "the statement and the receipt disagree about the amount",
	}
}

// BatchStatus is the state of one imported statement.
type BatchStatus string

const (
	BatchUploaded    BatchStatus = "uploaded"
	BatchMatching    BatchStatus = "matching"
	BatchNeedsReview BatchStatus = "needs_review"
	BatchReconciled  BatchStatus = "reconciled"
	BatchFailed      BatchStatus = "failed"
	BatchCancelled   BatchStatus = "cancelled"
)

// Batch is one imported statement.
type Batch struct {
	ID            shared.ID
	SourceCode    string
	SourceName    *string
	Filename      string
	ContentSHA256 string
	StatementFrom *shared.Date
	StatementTo   *shared.Date
	Status        BatchStatus
	LineCount     int
	MatchedCount  int
	TotalAmount   money.Amount
	MatchedAmount money.Amount
	Notes         *string
	UploadedBy    *shared.ID
	ReconciledBy  *shared.ID
}

// StatusAfterMatching is the batch state implied by its lines.
//
// A batch is reconciled only when nothing is left needing a person. That is a
// deliberately high bar: a batch marked reconciled with three unmatched lines
// would be a reconciliation that reconciles nothing, and the register of
// exceptions is the only part of this whose emptiness means anything.
func StatusAfterMatching(lines []Line) BatchStatus {
	for _, line := range lines {
		if line.Status.NeedsReview() {
			return BatchNeedsReview
		}
	}
	return BatchReconciled
}

// Summarise totals a batch's lines for its header.
func Summarise(lines []Line) (count, matched int, total, matchedTotal money.Amount) {
	for _, line := range lines {
		count++
		total = total.MustAdd(line.Amount)
		if line.Status == StatusMatched || line.Status == StatusVariance {
			matched++
			matchedTotal = matchedTotal.MustAdd(line.Amount)
		}
	}
	return count, matched, total, matchedTotal
}

// ValidateResolution checks a human's decision about a line.
//
// Setting a line aside requires a reason, and matching one by hand requires the
// payment. The rule that matters is the last one: a line can never be marked
// simply "matched" by hand without naming what it matched, because that is
// indistinguishable from marking it done to clear the queue.
func ValidateResolution(status MatchStatus, paymentID *shared.ID, note string) error {
	switch status {
	case StatusIgnored:
		if strings.TrimSpace(note) == "" {
			return shared.Validation("settlement.reason_required",
				"setting a statement line aside requires a written reason").
				WithDetail("remedy", "say what this line is: a bank charge, an internal transfer, "+
					"a line belonging to another system")
		}
		return nil
	case StatusMatched, StatusVariance:
		if paymentID == nil {
			return shared.Validation("settlement.payment_required",
				"matching a line by hand requires the payment it matches")
		}
		return nil
	case StatusUnmatched, StatusDuplicate:
		return shared.Validation("settlement.not_a_resolution",
			"%q is a finding, not a resolution; a line is resolved by matching it or setting it aside",
			status)
	default:
		return shared.Validation("settlement.unknown_status",
			"%q is not a match status", status)
	}
}
