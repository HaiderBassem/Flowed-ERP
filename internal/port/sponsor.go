package port

import (
	"context"

	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// SponsorRepository stores sponsoring bodies, their agreements and what those
// agreements committed them to.
type SponsorRepository interface {
	CreateSponsor(ctx context.Context, s *billing.Sponsor) error
	UpdateSponsor(ctx context.Context, s *billing.Sponsor) error
	GetSponsor(ctx context.Context, id shared.ID) (*billing.Sponsor, error)
	ListSponsors(ctx context.Context, activeOnly bool) ([]*billing.Sponsor, error)

	CreateSponsorship(ctx context.Context, s *billing.Sponsorship) error
	UpdateSponsorship(ctx context.Context, s *billing.Sponsorship) error
	GetSponsorship(ctx context.Context, id shared.ID) (*billing.Sponsorship, error)
	ListSponsorshipsForStudent(ctx context.Context, studentID shared.ID) ([]*billing.Sponsorship, error)
	// ActiveSponsorshipsCovering returns the agreements that apply to a student
	// in a year, which is what account generation materialises commitments
	// from — the same shape as an all-years discount grant.
	ActiveSponsorshipsCovering(ctx context.Context, studentID shared.ID, yearCode string) ([]*billing.Sponsorship, error)

	CreateCommitment(ctx context.Context, c *billing.Commitment) error
	UpdateCommitment(ctx context.Context, c *billing.Commitment) error
	GetCommitment(ctx context.Context, id shared.ID) (*billing.Commitment, error)
	ListCommitmentsForAccount(ctx context.Context, accountID shared.ID) ([]*billing.Commitment, error)
	ListCommitmentsForSponsor(ctx context.Context, sponsorID shared.ID, yearID *shared.ID, openOnly bool) ([]*billing.Commitment, error)
	// SponsorPaidForAccount totals what sponsors have actually paid against
	// one account, which separates their money from the student's in the
	// funding breakdown.
	SponsorPaidForAccount(ctx context.Context, accountID shared.ID) (money.Amount, error)
	// Receivables is the invoice list: what each sponsor has committed, paid
	// and still owes, per year.
	Receivables(ctx context.Context, yearID *shared.ID) ([]SponsorReceivable, error)
}

// SponsorReceivable is what one sponsor owes for one academic year.
type SponsorReceivable struct {
	SponsorID       shared.ID
	SponsorCode     string
	SponsorName     string
	AcademicYearID  shared.ID
	CommitmentCount int
	StudentCount    int
	Committed       money.Amount
	Paid            money.Amount
	Outstanding     money.Amount
}
