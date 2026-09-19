package billing_test

import (
	"testing"
	"time"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

func bp(v money.BasisPoints) *money.BasisPoints { return &v }
func amt(v money.Amount) *money.Amount          { return &v }

func sponsorshipParams() billing.NewSponsorshipParams {
	return billing.NewSponsorshipParams{
		SponsorID:      shared.NewID(),
		StudentID:      shared.NewID(),
		CoverageType:   billing.CoveragePercentage,
		CoverageBP:     bp(8000),
		SettlementMode: billing.SettlementReceivable,
		FromYearCode:   "2025-2026",
	}
}

// The decision the whole model turns on: who is chased when the sponsor does
// not pay. An agreement that does not say has not been negotiated, and
// choosing for the university would decide who receives a debt letter.
func TestSettlementModeIsRequired(t *testing.T) {
	params := sponsorshipParams()
	params.SettlementMode = ""

	_, err := billing.NewSponsorship(params)
	if err == nil {
		t.Fatal("an agreement with no settlement mode must be refused")
	}
	if code := shared.CodeOf(err); code != "sponsorship.settlement_mode_required" {
		t.Errorf("code = %q", code)
	}
}

func TestCoverageShapeMustCarryExactlyItsOwnFigure(t *testing.T) {
	cases := map[string]func(p *billing.NewSponsorshipParams){
		"percentage with no share": func(p *billing.NewSponsorshipParams) { p.CoverageBP = nil },
		"percentage over 100%":     func(p *billing.NewSponsorshipParams) { p.CoverageBP = bp(10_001) },
		"percentage with an amount too": func(p *billing.NewSponsorshipParams) {
			p.CoverageAmount = amt(500_000)
		},
		"fixed with no amount": func(p *billing.NewSponsorshipParams) {
			p.CoverageType = billing.CoverageFixed
			p.CoverageBP = nil
		},
		"fixed with a percentage too": func(p *billing.NewSponsorshipParams) {
			p.CoverageType = billing.CoverageFixed
			p.CoverageAmount = amt(500_000)
		},
		"full with a figure": func(p *billing.NewSponsorshipParams) {
			p.CoverageType = billing.CoverageFull
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			params := sponsorshipParams()
			mutate(&params)
			if _, err := billing.NewSponsorship(params); err == nil {
				t.Fatal("expected the agreement to be refused")
			}
		})
	}

	// The three well-formed shapes.
	valid := []billing.NewSponsorshipParams{
		sponsorshipParams(),
		func() billing.NewSponsorshipParams {
			p := sponsorshipParams()
			p.CoverageType, p.CoverageBP, p.CoverageAmount = billing.CoverageFixed, nil, amt(1_500_000)
			return p
		}(),
		func() billing.NewSponsorshipParams {
			p := sponsorshipParams()
			p.CoverageType, p.CoverageBP = billing.CoverageFull, nil
			return p
		}(),
	}
	for _, params := range valid {
		if _, err := billing.NewSponsorship(params); err != nil {
			t.Errorf("%s agreement refused: %v", params.CoverageType, err)
		}
	}
}

// "Eighty per cent of the fees" means eighty per cent of tuition, not of the
// identity card — the same base a percentage discount computes against.
func TestPercentageCoverageUsesTheDiscountableBase(t *testing.T) {
	agreement, err := billing.NewSponsorship(sponsorshipParams())
	if err != nil {
		t.Fatal(err)
	}

	// Base of 2,000,000 discountable; the account owes 2,100,000 including
	// 100,000 of non-discountable registration and card fees.
	committed, err := billing.ComputeCommitment(agreement, 2_000_000, 2_100_000)
	if err != nil {
		t.Fatal(err)
	}
	if committed != 1_600_000 {
		t.Errorf("committed %s, want 1,600,000 — eighty per cent of the discountable base", committed)
	}
}

func TestAnnualCapBoundsTheCommitment(t *testing.T) {
	params := sponsorshipParams()
	params.AnnualCap = amt(1_000_000)
	agreement, err := billing.NewSponsorship(params)
	if err != nil {
		t.Fatal(err)
	}

	committed, err := billing.ComputeCommitment(agreement, 2_000_000, 2_100_000)
	if err != nil {
		t.Fatal(err)
	}
	if committed != 1_000_000 {
		t.Errorf("committed %s, want the 1,000,000 cap", committed)
	}
}

// A sponsor cannot commit more than the account owes. The surplus would be
// money the university is owed twice.
func TestCommitmentNeverExceedsWhatIsOwed(t *testing.T) {
	params := sponsorshipParams()
	params.CoverageType, params.CoverageBP, params.CoverageAmount =
		billing.CoverageFixed, nil, amt(5_000_000)
	agreement, err := billing.NewSponsorship(params)
	if err != nil {
		t.Fatal(err)
	}

	committed, err := billing.ComputeCommitment(agreement, 2_000_000, 2_100_000)
	if err != nil {
		t.Fatal(err)
	}
	if committed != 2_100_000 {
		t.Errorf("committed %s, want the 2,100,000 the account owes", committed)
	}
}

func TestFullCoverageTakesTheWholeObligation(t *testing.T) {
	params := sponsorshipParams()
	params.CoverageType, params.CoverageBP = billing.CoverageFull, nil
	agreement, err := billing.NewSponsorship(params)
	if err != nil {
		t.Fatal(err)
	}

	committed, err := billing.ComputeCommitment(agreement, 2_000_000, 2_100_000)
	if err != nil {
		t.Fatal(err)
	}
	if committed != 2_100_000 {
		t.Errorf("committed %s, want the whole 2,100,000", committed)
	}
}

// The same rule every other decision not to collect money follows.
func TestApprovalCannotBeSelfApproval(t *testing.T) {
	agreement, err := billing.NewSponsorship(sponsorshipParams())
	if err != nil {
		t.Fatal(err)
	}
	recorder := shared.NewID()
	agreement.CreatedBy = &recorder

	if err := agreement.Approve(recorder, time.Now()); err == nil {
		t.Fatal("the person who recorded an agreement must not approve it")
	}
	if err := agreement.Approve(shared.NewID(), time.Now()); err != nil {
		t.Fatalf("another approver should be accepted: %v", err)
	}
	if !agreement.IsLive() {
		t.Error("an approved agreement should be live")
	}
}

func TestRevocationRequiresAReason(t *testing.T) {
	agreement, err := billing.NewSponsorship(sponsorshipParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := agreement.Revoke(shared.NewID(), "  ", time.Now()); err == nil {
		t.Fatal("revoking an agreement must require a reason")
	}
	if err := agreement.Revoke(shared.NewID(), "sponsor withdrew funding", time.Now()); err != nil {
		t.Fatal(err)
	}
	if agreement.IsLive() {
		t.Error("a revoked agreement must not be live")
	}
}

func TestCoversYearRespectsTheWindow(t *testing.T) {
	params := sponsorshipParams()
	params.FromYearCode = "2024-2025"
	to := "2026-2027"
	params.ToYearCode = &to
	agreement, err := billing.NewSponsorship(params)
	if err != nil {
		t.Fatal(err)
	}

	for _, covered := range []string{"2024-2025", "2025-2026", "2026-2027"} {
		if !agreement.CoversYear(covered) {
			t.Errorf("%s should be covered", covered)
		}
	}
	for _, outside := range []string{"2023-2024", "2027-2028"} {
		if agreement.CoversYear(outside) {
			t.Errorf("%s should be outside the agreement", outside)
		}
	}

	// An open-ended agreement covers every later year.
	params.ToYearCode = nil
	open, err := billing.NewSponsorship(params)
	if err != nil {
		t.Fatal(err)
	}
	if !open.CoversYear("2030-2031") {
		t.Error("an open-ended agreement should cover a later year")
	}
}

// The question the sponsor model exists to answer, and the one that had no
// answer while every reduction looked like a discount.
func TestFundingBreakdownSeparatesWhoPaid(t *testing.T) {
	account := &billing.Account{
		GrossTotal:    2_100_000,
		DiscountTotal: 100_000,
		NetTotal:      2_000_000,
		PaidTotal:     1_200_000,
	}

	commitments := []*billing.Commitment{
		{
			SettlementMode:  billing.SettlementReceivable,
			CommittedAmount: 800_000,
			PaidAmount:      500_000,
			Status:          "open",
		},
		{
			SettlementMode:  billing.SettlementCoversDebt,
			CommittedAmount: 300_000,
			Status:          "open",
		},
	}

	funding := billing.BuildFunding(account, commitments, 500_000)

	if funding.SponsorReceivable != 300_000 {
		t.Errorf("receivable = %s, want the 300,000 still owed under the receivable agreement",
			funding.SponsorReceivable)
	}
	if funding.SponsorCovered != 300_000 {
		t.Errorf("covered = %s, want 300,000", funding.SponsorCovered)
	}
	if funding.SponsorPaid != 500_000 {
		t.Errorf("sponsor paid = %s, want 500,000", funding.SponsorPaid)
	}
	if funding.StudentPaid != 700_000 {
		t.Errorf("student paid = %s, want the 700,000 of the 1,200,000 that was not a sponsor's",
			funding.StudentPaid)
	}
}

func TestCommitmentOutstanding(t *testing.T) {
	commitment := &billing.Commitment{CommittedAmount: 800_000, PaidAmount: 300_000}
	if got := commitment.Outstanding(); got != 500_000 {
		t.Errorf("outstanding = %s, want 500,000", got)
	}

	overpaid := &billing.Commitment{CommittedAmount: 800_000, PaidAmount: 900_000}
	if got := overpaid.Outstanding(); !got.IsZero() {
		t.Errorf("an overpaid commitment owes nothing, got %s", got)
	}
}
