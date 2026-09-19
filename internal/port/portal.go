package port

import (
	"context"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// StatementVerification is a printed statement a third party can check.
//
// The figures are frozen at printing. A verification that recomputed would
// disagree with the paper the moment the student paid anything, and the office
// holding it would conclude the paper was forged.
type StatementVerification struct {
	ID             shared.ID
	Code           string
	StudentID      shared.ID
	AcademicYearID *shared.ID
	TotalCharged   money.Amount
	TotalPaid      money.Amount
	Outstanding    money.Amount
	ContentSHA256  string
	IssuedAt       time.Time
	IssuedBy       *shared.ID
	ExpiresAt      time.Time
	RevokedAt      *time.Time
	RevokedReason  *string
}

// VerificationRepository stores issued statement verifications.
type VerificationRepository interface {
	Create(ctx context.Context, v *StatementVerification) error
	GetByCode(ctx context.Context, code string) (*StatementVerification, error)
	ListForStudent(ctx context.Context, studentID shared.ID) ([]*StatementVerification, error)
	Revoke(ctx context.Context, id shared.ID, reason string, at time.Time) error
}

// PaymentSummary is a collection as a statement shows it: enough to recognise
// the receipt in your hand, and nothing about who took it.
type PaymentSummary struct {
	PaymentID  shared.ID
	ReceiptNo  *string
	Amount     money.Amount
	MethodCode string
	PaidAt     time.Time
	Status     string
	// RefundedTotal lets a statement show a partially refunded collection as
	// what it is rather than as its original figure.
	RefundedTotal money.Amount
}
