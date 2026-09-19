package app

import (
	"context"
	"strings"
	"time"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// SponsorService administers sponsoring bodies and their agreements, and
// materialises what those agreements commit them to.
//
// The distinction it exists to preserve: a discount is money the university
// decided not to charge, a sponsorship is money somebody else agreed to pay.
// Recording the second as the first erases the receivable, and a university
// with no list of who owes it what cannot invoice, cannot chase, and cannot
// notice a ministry that stopped paying two years ago.
type SponsorService struct {
	deps Deps
	repo port.SponsorRepository
	auditor
}

// NewSponsorService wires the sponsorship commands.
func NewSponsorService(d Deps, repo port.SponsorRepository) *SponsorService {
	return &SponsorService{deps: d, repo: repo, auditor: newAuditor(d.Audit, d.Clock)}
}

// CreateSponsorInput registers a sponsoring body.
type CreateSponsorInput struct {
	Code         string
	NameAr       string
	NameEn       *string
	SponsorType  string
	ContactName  *string
	ContactPhone *string
	ContactEmail *string
	Address      *string
	Notes        *string
}

// CreateSponsor registers a body that pays students' fees.
func (s *SponsorService) CreateSponsor(ctx context.Context, actor shared.Actor, in CreateSponsorInput) (*billing.Sponsor, error) {
	if err := actor.RequireAnyRole("CreateSponsor", shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	sponsor, err := billing.NewSponsor(in.Code, in.NameAr, in.SponsorType)
	if err != nil {
		return nil, err
	}
	sponsor.NameEn = in.NameEn
	sponsor.ContactName = in.ContactName
	sponsor.ContactPhone = in.ContactPhone
	sponsor.ContactEmail = in.ContactEmail
	sponsor.Address = in.Address
	sponsor.Notes = in.Notes
	sponsor.CreatedBy = &actor.UserID

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.repo.CreateSponsor(ctx, sponsor); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "sponsor",
			EntityID:   &sponsor.ID,
			Action:     "sponsor.created",
			Actor:      actor,
			After:      snapshotOf(sponsor),
			Metadata:   map[string]any{"code": sponsor.Code, "type": sponsor.SponsorType},
		})
	})
	if err != nil {
		return nil, err
	}
	return sponsor, nil
}

// ListSponsors returns the bodies.
func (s *SponsorService) ListSponsors(ctx context.Context, actor shared.Actor, activeOnly bool) ([]*billing.Sponsor, error) {
	if err := actor.RequireAnyRole("ListSponsors",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor,
		shared.RoleRegistrar, shared.RoleReportViewer); err != nil {
		return nil, err
	}
	return s.repo.ListSponsors(ctx, activeOnly)
}

// CreateSponsorshipInput records an agreement.
type CreateSponsorshipInput struct {
	SponsorID      shared.ID
	StudentID      shared.ID
	CoverageType   billing.CoverageType
	CoverageBP     *money.BasisPoints
	CoverageAmount *money.Amount
	AnnualCap      *money.Amount
	SettlementMode billing.SettlementMode
	FromYearCode   string
	ToYearCode     *string
	AgreementRef   *string
	Notes          *string
}

// CreateSponsorship records an agreement, in draft.
//
// It takes effect on approval, and approval is somebody else's: an agreement
// is a decision not to collect from a student, which is the same shape as a
// discount and gets the same four eyes.
func (s *SponsorService) CreateSponsorship(ctx context.Context, actor shared.Actor, in CreateSponsorshipInput) (*billing.Sponsorship, error) {
	if err := actor.RequireAnyRole("CreateSponsorship",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleRegistrar); err != nil {
		return nil, err
	}

	agreement, err := billing.NewSponsorship(billing.NewSponsorshipParams{
		SponsorID:      in.SponsorID,
		StudentID:      in.StudentID,
		CoverageType:   in.CoverageType,
		CoverageBP:     in.CoverageBP,
		CoverageAmount: in.CoverageAmount,
		AnnualCap:      in.AnnualCap,
		SettlementMode: in.SettlementMode,
		FromYearCode:   in.FromYearCode,
		ToYearCode:     in.ToYearCode,
		AgreementRef:   in.AgreementRef,
	})
	if err != nil {
		return nil, err
	}
	agreement.Notes = in.Notes
	agreement.CreatedBy = &actor.UserID

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if _, err := s.repo.GetSponsor(ctx, in.SponsorID); err != nil {
			return err
		}
		if _, err := s.deps.Students.GetByID(ctx, in.StudentID); err != nil {
			return err
		}
		if err := s.repo.CreateSponsorship(ctx, agreement); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "sponsorship",
			EntityID:   &agreement.ID,
			Action:     "sponsorship.recorded",
			Actor:      actor,
			After:      snapshotOf(agreement),
			StudentID:  &agreement.StudentID,
			Metadata: map[string]any{
				"sponsor_id":      agreement.SponsorID.String(),
				"coverage":        string(agreement.CoverageType),
				"settlement_mode": string(agreement.SettlementMode),
				"from_year":       agreement.FromYearCode,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return agreement, nil
}

// ApproveSponsorship activates an agreement.
func (s *SponsorService) ApproveSponsorship(ctx context.Context, actor shared.Actor, id shared.ID) (*billing.Sponsorship, error) {
	if err := actor.RequireAnyRole("ApproveSponsorship", shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var agreement *billing.Sponsorship
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		agreement, err = s.repo.GetSponsorship(ctx, id)
		if err != nil {
			return err
		}
		before := snapshotOf(agreement)
		if err := agreement.Approve(actor.UserID, nowOr(s.deps.Clock)); err != nil {
			return err
		}
		if err := s.repo.UpdateSponsorship(ctx, agreement); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "sponsorship",
			EntityID:   &agreement.ID,
			Action:     "sponsorship.approved",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(agreement),
			StudentID:  &agreement.StudentID,
		})
	})
	if err != nil {
		return nil, err
	}
	return agreement, nil
}

// RevokeSponsorship ends an agreement.
//
// Commitments already frozen against generated accounts are untouched: the
// sponsor agreed to those years and the university priced them accordingly.
// Revocation stops new commitments, exactly as revoking a discount assignment
// leaves the current year's application alone.
func (s *SponsorService) RevokeSponsorship(ctx context.Context, actor shared.Actor, id shared.ID, reason string) (*billing.Sponsorship, error) {
	if err := actor.RequireAnyRole("RevokeSponsorship", shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var agreement *billing.Sponsorship
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		agreement, err = s.repo.GetSponsorship(ctx, id)
		if err != nil {
			return err
		}
		before := snapshotOf(agreement)
		if err := agreement.Revoke(actor.UserID, reason, nowOr(s.deps.Clock)); err != nil {
			return err
		}
		if err := s.repo.UpdateSponsorship(ctx, agreement); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "sponsorship",
			EntityID:   &agreement.ID,
			Action:     "sponsorship.revoked",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(agreement),
			StudentID:  &agreement.StudentID,
			Reason:     &reason,
		})
	})
	if err != nil {
		return nil, err
	}
	return agreement, nil
}

// MaterialiseCommitments freezes what every live agreement owes for an account.
//
// Called from account generation, inside its transaction, exactly where
// discount applications are materialised — and for the same reason. A
// commitment computed later would answer with today's agreement rather than the
// one the student was admitted under.
//
// Under covers_debt the student's obligation falls here by a signed adjustment.
// Under receivable nothing about the student changes: the sponsor's share is an
// expected inflow, and it arrives as an ordinary payment.
func (s *SponsorService) MaterialiseCommitments(
	ctx context.Context, actor shared.Actor, account *billing.Account, yearCode string,
	discountableBase money.Amount,
) ([]*billing.Commitment, error) {
	if s.repo == nil {
		return nil, nil
	}

	agreements, err := s.repo.ActiveSponsorshipsCovering(ctx, account.StudentID, yearCode)
	if err != nil {
		return nil, err
	}
	if len(agreements) == 0 {
		return nil, nil
	}

	now := nowOr(s.deps.Clock)
	commitments := make([]*billing.Commitment, 0, len(agreements))

	for _, agreement := range agreements {
		committed, err := billing.ComputeCommitment(agreement, discountableBase, account.EffectiveNet())
		if err != nil {
			return nil, err
		}
		if !committed.IsPositive() {
			continue
		}

		commitment := &billing.Commitment{
			ID:              shared.NewID(),
			SponsorshipID:   agreement.ID,
			SponsorID:       agreement.SponsorID,
			AccountID:       account.ID,
			StudentID:       account.StudentID,
			AcademicYearID:  account.AcademicYearID,
			FrozenBase:      discountableBase,
			CommittedAmount: committed,
			SettlementMode:  agreement.SettlementMode,
			Status:          "open",
			CreatedAt:       now,
			CreatedBy:       &actor.UserID,
		}

		if agreement.SettlementMode.ReducesStudentDebt() {
			adjustment, err := billing.NewAdjustment(account.ID, billing.AdjustmentSponsorship,
				committed.Neg(), "sponsorship covers this share of the fees")
			if err != nil {
				return nil, err
			}
			adjustment.PostedBy = &actor.UserID
			adjustment.PostedAt = now
			adjustment.ReferenceType = ptr("sponsorship")
			adjustment.ReferenceID = &agreement.ID
			adjustment.PostingYearID = &account.AcademicYearID
			if adjustment.RequiresApproval() {
				adjustment.ApprovedBy = &actor.UserID
				adjustment.ApprovedAt = &now
			}

			if err := account.ApplyAdjustment(committed.Neg(), now); err != nil {
				return nil, err
			}
			if err := s.deps.Accounts.CreateAdjustment(ctx, adjustment); err != nil {
				return nil, err
			}
			commitment.AdjustmentID = &adjustment.ID
		}

		if err := s.repo.CreateCommitment(ctx, commitment); err != nil {
			return nil, err
		}
		commitments = append(commitments, commitment)

		if err := s.record(ctx, port.AuditEntry{
			EntityType:     "sponsor_commitment",
			EntityID:       &commitment.ID,
			Action:         "sponsorship.committed",
			Actor:          actor,
			After:          snapshotOf(commitment),
			AccountID:      &account.ID,
			StudentID:      &account.StudentID,
			AcademicYearID: &account.AcademicYearID,
			Metadata: map[string]any{
				"sponsor_id":           commitment.SponsorID.String(),
				"committed":            committed.Int64(),
				"settlement_mode":      string(commitment.SettlementMode),
				"reduced_student_debt": agreement.SettlementMode.ReducesStudentDebt(),
			},
		}); err != nil {
			return nil, err
		}
	}
	return commitments, nil
}

// RecordSponsorPayment attributes a collection to a sponsor's commitment.
//
// The payment itself is an ordinary collection against the student's account,
// allocated the ordinary way. This only decides which commitment it settles, so
// that "how much has this ministry paid us" has an answer that does not involve
// reading payer names.
func (s *SponsorService) RecordSponsorPayment(
	ctx context.Context, actor shared.Actor, accountID, sponsorID shared.ID, amount money.Amount,
) error {
	commitments, err := s.repo.ListCommitmentsForAccount(ctx, accountID)
	if err != nil {
		return err
	}

	remaining := amount
	for _, commitment := range commitments {
		if !remaining.IsPositive() {
			break
		}
		if commitment.SponsorID != sponsorID || commitment.Status != "open" {
			continue
		}

		take := money.Min(remaining, commitment.Outstanding())
		if !take.IsPositive() {
			continue
		}
		paid, err := commitment.PaidAmount.Add(take)
		if err != nil {
			return shared.Internal("commitment.arithmetic", err, "recording a sponsor payment")
		}
		commitment.PaidAmount = paid
		if commitment.Outstanding().IsZero() {
			commitment.Status = "settled"
		}
		if err := s.repo.UpdateCommitment(ctx, commitment); err != nil {
			return err
		}
		remaining, err = remaining.Sub(take)
		if err != nil {
			return shared.Internal("commitment.arithmetic", err, "tracking a sponsor payment")
		}
	}
	// A surplus is not an error: a sponsor may pay more than one year's
	// commitment in one transfer, and the excess is on the student's account as
	// an ordinary overpayment, which becomes credit exactly as it would from
	// any other payer.
	return nil
}

// Funding returns who is paying for one account.
func (s *SponsorService) Funding(ctx context.Context, actor shared.Actor, account *billing.Account) (billing.FundingBreakdown, []*billing.Commitment, error) {
	commitments, err := s.repo.ListCommitmentsForAccount(ctx, account.ID)
	if err != nil {
		return billing.FundingBreakdown{}, nil, err
	}
	sponsorPaid, err := s.repo.SponsorPaidForAccount(ctx, account.ID)
	if err != nil {
		return billing.FundingBreakdown{}, nil, err
	}
	return billing.BuildFunding(account, commitments, sponsorPaid), commitments, nil
}

// Receivables is the invoice list: what each sponsor owes.
func (s *SponsorService) Receivables(ctx context.Context, actor shared.Actor, yearID *shared.ID) ([]port.SponsorReceivable, error) {
	if err := actor.RequireAnyRole("SponsorReceivables",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor, shared.RoleReportViewer); err != nil {
		return nil, err
	}
	return s.repo.Receivables(ctx, yearID)
}

// SponsorshipsForStudent returns every agreement naming a student.
func (s *SponsorService) SponsorshipsForStudent(ctx context.Context, actor shared.Actor, studentID shared.ID) ([]*billing.Sponsorship, error) {
	if actor.HasRole(shared.RoleStudent) {
		if actor.StudentID == nil || *actor.StudentID != studentID {
			return nil, shared.Forbidden("sponsorship.not_yours", "this record belongs to another student")
		}
		return s.repo.ListSponsorshipsForStudent(ctx, studentID)
	}
	if err := actor.RequireAnyRole("SponsorshipsForStudent",
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor,
		shared.RoleRegistrar, shared.RoleCashier, shared.RoleReportViewer); err != nil {
		return nil, err
	}
	return s.repo.ListSponsorshipsForStudent(ctx, studentID)
}

var _ = strings.TrimSpace
var _ = time.Now
