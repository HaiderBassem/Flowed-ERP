package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// SponsorRepository stores sponsoring bodies, agreements and commitments.
type SponsorRepository struct{ db *pg.DB }

// NewSponsorRepository builds the sponsor store over a connection pool.
func NewSponsorRepository(db *pg.DB) *SponsorRepository { return &SponsorRepository{db: db} }

var _ port.SponsorRepository = (*SponsorRepository)(nil)

const sponsorColumns = `
	id, code, name_ar, name_en, sponsor_type, contact_name, contact_phone,
	contact_email, address, notes, is_active, created_at, created_by`

func scanSponsor(row pgx.Row) (*billing.Sponsor, error) {
	var s billing.Sponsor
	if err := row.Scan(
		&s.ID, &s.Code, &s.NameAr, &s.NameEn, &s.SponsorType, &s.ContactName, &s.ContactPhone,
		&s.ContactEmail, &s.Address, &s.Notes, &s.IsActive, &s.CreatedAt, &s.CreatedBy,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateSponsor records a sponsoring body.
func (r *SponsorRepository) CreateSponsor(ctx context.Context, s *billing.Sponsor) error {
	if err := r.db.RequireTx(ctx, "sponsor.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO sponsor (id, code, name_ar, name_en, sponsor_type, contact_name,
			contact_phone, contact_email, address, notes, is_active, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, s.ID, s.Code, s.NameAr, s.NameEn, s.SponsorType,
		s.ContactName, s.ContactPhone, s.ContactEmail, s.Address, s.Notes, s.IsActive, s.CreatedBy,
	).Scan(&s.CreatedAt)
	return pg.WrapQuery("sponsor.Create", err)
}

// UpdateSponsor renames a body, updates its contacts or retires it.
func (r *SponsorRepository) UpdateSponsor(ctx context.Context, s *billing.Sponsor) error {
	if err := r.db.RequireTx(ctx, "sponsor.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE sponsor SET name_ar=$2, name_en=$3, sponsor_type=$4, contact_name=$5,
			contact_phone=$6, contact_email=$7, address=$8, notes=$9, is_active=$10
		WHERE id=$1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, s.ID, s.NameAr, s.NameEn, s.SponsorType, s.ContactName,
		s.ContactPhone, s.ContactEmail, s.Address, s.Notes, s.IsActive).Scan(&id)
	return pg.WrapQuery("sponsor.Update", err)
}

// GetSponsor returns one body.
func (r *SponsorRepository) GetSponsor(ctx context.Context, id shared.ID) (*billing.Sponsor, error) {
	q := r.db.Conn(ctx)
	s, err := scanSponsor(q.QueryRow(ctx, `SELECT`+sponsorColumns+` FROM sponsor WHERE id=$1`, id))
	if err != nil {
		return nil, pg.WrapQuery("sponsor.Get", err)
	}
	return s, nil
}

// ListSponsors returns the bodies, in code order.
func (r *SponsorRepository) ListSponsors(ctx context.Context, activeOnly bool) ([]*billing.Sponsor, error) {
	const query = `SELECT` + sponsorColumns + `
		FROM sponsor WHERE NOT $1::boolean OR is_active ORDER BY code`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("sponsor.List", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Sponsor, error) {
		return scanSponsor(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("sponsor.List", err)
	}
	return out, nil
}

const sponsorshipColumns = `
	id, sponsor_id, student_id, coverage_type, coverage_bp, coverage_amount, annual_cap,
	settlement_mode, from_year_code, to_year_code, status, agreement_ref, notes,
	approved_at, approved_by, revoked_at, revoked_by, revoked_reason, created_at, created_by`

func scanSponsorship(row pgx.Row) (*billing.Sponsorship, error) {
	var (
		s      billing.Sponsorship
		bp     *int32
		amount *int64
		cap_   *int64
	)
	if err := row.Scan(
		&s.ID, &s.SponsorID, &s.StudentID, &s.CoverageType, &bp, &amount, &cap_,
		&s.SettlementMode, &s.FromYearCode, &s.ToYearCode, &s.Status, &s.AgreementRef, &s.Notes,
		&s.ApprovedAt, &s.ApprovedBy, &s.RevokedAt, &s.RevokedBy, &s.RevokedReason,
		&s.CreatedAt, &s.CreatedBy,
	); err != nil {
		return nil, err
	}
	if bp != nil {
		value := money.BasisPoints(*bp)
		s.CoverageBP = &value
	}
	if amount != nil {
		value := money.Amount(*amount)
		s.CoverageAmount = &value
	}
	if cap_ != nil {
		value := money.Amount(*cap_)
		s.AnnualCap = &value
	}
	return &s, nil
}

// CreateSponsorship records an agreement.
func (r *SponsorRepository) CreateSponsorship(ctx context.Context, s *billing.Sponsorship) error {
	if err := r.db.RequireTx(ctx, "sponsorship.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO sponsorship (id, sponsor_id, student_id, coverage_type, coverage_bp,
			coverage_amount, annual_cap, settlement_mode, from_year_code, to_year_code,
			status, agreement_ref, notes, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, s.ID, s.SponsorID, s.StudentID, string(s.CoverageType),
		bpOrNil(s.CoverageBP), amountOrNil(s.CoverageAmount), amountOrNil(s.AnnualCap),
		string(s.SettlementMode), s.FromYearCode, s.ToYearCode, string(s.Status),
		s.AgreementRef, s.Notes, s.CreatedBy).Scan(&s.CreatedAt)
	return pg.WrapQuery("sponsorship.Create", err)
}

// UpdateSponsorship writes back an agreement whose state moved.
func (r *SponsorRepository) UpdateSponsorship(ctx context.Context, s *billing.Sponsorship) error {
	if err := r.db.RequireTx(ctx, "sponsorship.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE sponsorship SET status=$2, notes=$3, approved_at=$4, approved_by=$5,
			revoked_at=$6, revoked_by=$7, revoked_reason=$8
		WHERE id=$1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, s.ID, string(s.Status), s.Notes, s.ApprovedAt, s.ApprovedBy,
		s.RevokedAt, s.RevokedBy, s.RevokedReason).Scan(&id)
	return pg.WrapQuery("sponsorship.Update", err)
}

// GetSponsorship returns one agreement.
func (r *SponsorRepository) GetSponsorship(ctx context.Context, id shared.ID) (*billing.Sponsorship, error) {
	q := r.db.Conn(ctx)
	s, err := scanSponsorship(q.QueryRow(ctx, `SELECT`+sponsorshipColumns+` FROM sponsorship WHERE id=$1`, id))
	if err != nil {
		return nil, pg.WrapQuery("sponsorship.Get", err)
	}
	return s, nil
}

// ListSponsorshipsForStudent returns every agreement naming a student.
func (r *SponsorRepository) ListSponsorshipsForStudent(ctx context.Context, studentID shared.ID) ([]*billing.Sponsorship, error) {
	const query = `SELECT` + sponsorshipColumns + `
		FROM sponsorship WHERE student_id=$1 ORDER BY created_at DESC`
	return r.collectSponsorships(ctx, "sponsorship.ListForStudent", query, studentID)
}

// ActiveSponsorshipsCovering returns the agreements applying to a student in a
// year — what account generation materialises commitments from.
func (r *SponsorRepository) ActiveSponsorshipsCovering(
	ctx context.Context, studentID shared.ID, yearCode string,
) ([]*billing.Sponsorship, error) {
	const query = `
		SELECT` + sponsorshipColumns + `
		FROM sponsorship
		WHERE student_id = $1
		  AND status = 'active'
		  AND from_year_code <= $2
		  AND (to_year_code IS NULL OR to_year_code >= $2)
		ORDER BY created_at`
	return r.collectSponsorships(ctx, "sponsorship.ActiveCovering", query, studentID, yearCode)
}

func (r *SponsorRepository) collectSponsorships(
	ctx context.Context, op, query string, args ...any,
) ([]*billing.Sponsorship, error) {
	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, pg.WrapQuery(op, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Sponsorship, error) {
		return scanSponsorship(row)
	})
	if err != nil {
		return nil, pg.WrapQuery(op, err)
	}
	return out, nil
}

const commitmentColumns = `
	id, sponsorship_id, sponsor_id, account_id, student_id, academic_year_id,
	frozen_base, committed_amount, paid_amount, settlement_mode, status,
	adjustment_id, created_at, created_by`

func scanCommitment(row pgx.Row) (*billing.Commitment, error) {
	var c billing.Commitment
	if err := row.Scan(
		&c.ID, &c.SponsorshipID, &c.SponsorID, &c.AccountID, &c.StudentID, &c.AcademicYearID,
		&c.FrozenBase, &c.CommittedAmount, &c.PaidAmount, &c.SettlementMode, &c.Status,
		&c.AdjustmentID, &c.CreatedAt, &c.CreatedBy,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateCommitment freezes what an agreement owes for one account.
func (r *SponsorRepository) CreateCommitment(ctx context.Context, c *billing.Commitment) error {
	if err := r.db.RequireTx(ctx, "commitment.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO sponsor_commitment (id, sponsorship_id, sponsor_id, account_id, student_id,
			academic_year_id, frozen_base, committed_amount, paid_amount, settlement_mode,
			status, adjustment_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING created_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, c.ID, c.SponsorshipID, c.SponsorID, c.AccountID, c.StudentID,
		c.AcademicYearID, c.FrozenBase.Int64(), c.CommittedAmount.Int64(), c.PaidAmount.Int64(),
		string(c.SettlementMode), c.Status, c.AdjustmentID, c.CreatedBy).Scan(&c.CreatedAt)
	return pg.WrapQuery("commitment.Create", err)
}

// UpdateCommitment records money received against a commitment.
//
// Only the paid amount and the status may move; the trigger on the table
// refuses everything else, because the committed figure is frozen exactly as a
// discount application's is.
func (r *SponsorRepository) UpdateCommitment(ctx context.Context, c *billing.Commitment) error {
	if err := r.db.RequireTx(ctx, "commitment.Update"); err != nil {
		return err
	}
	const query = `UPDATE sponsor_commitment SET paid_amount=$2, status=$3 WHERE id=$1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, c.ID, c.PaidAmount.Int64(), c.Status).Scan(&id)
	return pg.WrapQuery("commitment.Update", err)
}

// GetCommitment returns one commitment.
func (r *SponsorRepository) GetCommitment(ctx context.Context, id shared.ID) (*billing.Commitment, error) {
	q := r.db.Conn(ctx)
	c, err := scanCommitment(q.QueryRow(ctx, `SELECT`+commitmentColumns+` FROM sponsor_commitment WHERE id=$1`, id))
	if err != nil {
		return nil, pg.WrapQuery("commitment.Get", err)
	}
	return c, nil
}

// ListCommitmentsForAccount returns what sponsors owe against one account.
func (r *SponsorRepository) ListCommitmentsForAccount(ctx context.Context, accountID shared.ID) ([]*billing.Commitment, error) {
	const query = `SELECT` + commitmentColumns + `
		FROM sponsor_commitment WHERE account_id=$1 ORDER BY created_at`
	return r.collectCommitments(ctx, "commitment.ListForAccount", query, accountID)
}

// ListCommitmentsForSponsor returns what one body owes, optionally for a year.
func (r *SponsorRepository) ListCommitmentsForSponsor(
	ctx context.Context, sponsorID shared.ID, yearID *shared.ID, openOnly bool,
) ([]*billing.Commitment, error) {
	const query = `
		SELECT` + commitmentColumns + `
		FROM sponsor_commitment
		WHERE sponsor_id = $1
		  AND ($2::uuid IS NULL OR academic_year_id = $2::uuid)
		  AND (NOT $3::boolean OR status = 'open')
		ORDER BY created_at DESC
		LIMIT 1000`
	return r.collectCommitments(ctx, "commitment.ListForSponsor", query, sponsorID, yearID, openOnly)
}

func (r *SponsorRepository) collectCommitments(
	ctx context.Context, op, query string, args ...any,
) ([]*billing.Commitment, error) {
	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, pg.WrapQuery(op, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*billing.Commitment, error) {
		return scanCommitment(row)
	})
	if err != nil {
		return nil, pg.WrapQuery(op, err)
	}
	return out, nil
}

// SponsorPaidForAccount totals what sponsors have actually paid against one
// account, which is what separates their money from the student's.
func (r *SponsorRepository) SponsorPaidForAccount(ctx context.Context, accountID shared.ID) (money.Amount, error) {
	const query = `
		SELECT coalesce(sum(amount), 0)::bigint
		FROM payment
		WHERE account_id = $1 AND status = 'posted' AND sponsor_id IS NOT NULL`

	q := r.db.Conn(ctx)
	var total money.Amount
	if err := q.QueryRow(ctx, query, accountID).Scan(&total); err != nil {
		return 0, pg.WrapQuery("commitment.SponsorPaid", err)
	}
	return total, nil
}

// Receivables is the invoice list.
func (r *SponsorRepository) Receivables(ctx context.Context, yearID *shared.ID) ([]port.SponsorReceivable, error) {
	const query = `
		SELECT sponsor_id, sponsor_code, sponsor_name, academic_year_id,
		       commitment_count, student_count, committed_total, paid_total, outstanding_total
		FROM v_sponsor_receivable
		WHERE $1::uuid IS NULL OR academic_year_id = $1::uuid
		ORDER BY outstanding_total DESC`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, yearID)
	if err != nil {
		return nil, pg.WrapQuery("sponsor.Receivables", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.SponsorReceivable, error) {
		var r port.SponsorReceivable
		err := row.Scan(&r.SponsorID, &r.SponsorCode, &r.SponsorName, &r.AcademicYearID,
			&r.CommitmentCount, &r.StudentCount, &r.Committed, &r.Paid, &r.Outstanding)
		return r, err
	})
	if err != nil {
		return nil, pg.WrapQuery("sponsor.Receivables", err)
	}
	return out, nil
}

func bpOrNil(bp *money.BasisPoints) *int32 {
	if bp == nil {
		return nil
	}
	value := int32(*bp)
	return &value
}

func amountOrNil(a *money.Amount) *int64 {
	if a == nil {
		return nil
	}
	value := a.Int64()
	return &value
}
