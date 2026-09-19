package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// VerificationRepository stores issued statement verifications.
type VerificationRepository struct{ db *pg.DB }

// NewVerificationRepository builds the verification store over a pool.
func NewVerificationRepository(db *pg.DB) *VerificationRepository {
	return &VerificationRepository{db: db}
}

var _ port.VerificationRepository = (*VerificationRepository)(nil)

const verificationColumns = `
	id, code, student_id, academic_year_id, total_charged, total_paid, outstanding,
	content_sha256, issued_at, issued_by, expires_at, revoked_at, revoked_reason`

func scanVerification(row pgx.Row) (*port.StatementVerification, error) {
	var v port.StatementVerification
	if err := row.Scan(
		&v.ID, &v.Code, &v.StudentID, &v.AcademicYearID, &v.TotalCharged, &v.TotalPaid,
		&v.Outstanding, &v.ContentSHA256, &v.IssuedAt, &v.IssuedBy, &v.ExpiresAt,
		&v.RevokedAt, &v.RevokedReason,
	); err != nil {
		return nil, err
	}
	return &v, nil
}

// Create records an issued statement.
func (r *VerificationRepository) Create(ctx context.Context, v *port.StatementVerification) error {
	if err := r.db.RequireTx(ctx, "verification.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO statement_verification (id, code, student_id, academic_year_id,
			total_charged, total_paid, outstanding, content_sha256, issued_at, issued_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9, now()),$10,$11)
		RETURNING issued_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, v.ID, v.Code, v.StudentID, v.AcademicYearID,
		v.TotalCharged.Int64(), v.TotalPaid.Int64(), v.Outstanding.Int64(),
		v.ContentSHA256, instant(v.IssuedAt), v.IssuedBy, v.ExpiresAt).Scan(&v.IssuedAt)
	return pg.WrapQuery("verification.Create", err)
}

// GetByCode resolves a code presented by whoever holds the paper.
func (r *VerificationRepository) GetByCode(ctx context.Context, code string) (*port.StatementVerification, error) {
	q := r.db.Conn(ctx)
	v, err := scanVerification(q.QueryRow(ctx,
		`SELECT`+verificationColumns+` FROM statement_verification WHERE code = $1`, code))
	if err != nil {
		return nil, pg.WrapQuery("verification.GetByCode", err)
	}
	return v, nil
}

// ListForStudent returns the statements issued for a student.
func (r *VerificationRepository) ListForStudent(ctx context.Context, studentID shared.ID) ([]*port.StatementVerification, error) {
	const query = `SELECT` + verificationColumns + `
		FROM statement_verification WHERE student_id = $1 ORDER BY issued_at DESC LIMIT 100`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID)
	if err != nil {
		return nil, pg.WrapQuery("verification.ListForStudent", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.StatementVerification, error) {
		return scanVerification(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("verification.ListForStudent", err)
	}
	return out, nil
}

// Revoke withdraws an issued statement.
func (r *VerificationRepository) Revoke(ctx context.Context, id shared.ID, reason string, at time.Time) error {
	if err := r.db.RequireTx(ctx, "verification.Revoke"); err != nil {
		return err
	}
	const query = `
		UPDATE statement_verification
		SET revoked_at = COALESCE($3, now()), revoked_reason = $2
		WHERE id = $1 AND revoked_at IS NULL`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, id, reason, instant(at))
	return pg.WrapQuery("verification.Revoke", err)
}
