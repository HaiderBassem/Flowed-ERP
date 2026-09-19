package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// SessionRepository stores sign-ins so a credential can be withdrawn before it
// expires on its own.
type SessionRepository struct{ db *pg.DB }

// NewSessionRepository builds the session store over a connection pool.
func NewSessionRepository(db *pg.DB) *SessionRepository { return &SessionRepository{db: db} }

var _ port.SessionRepository = (*SessionRepository)(nil)

const sessionColumns = `
	id, user_id, issued_at, expires_at, last_seen_at,
	revoked_at, revoked_by, revoked_reason, ip_address, user_agent, cashier_desk_id`

func scanSession(row pgx.Row) (*port.Session, error) {
	var s port.Session
	if err := row.Scan(
		&s.ID, &s.UserID, &s.IssuedAt, &s.ExpiresAt, &s.LastSeenAt,
		&s.RevokedAt, &s.RevokedBy, &s.RevokedReason, &s.IPAddress, &s.UserAgent, &s.CashierDeskID,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// Create records a sign-in.
func (r *SessionRepository) Create(ctx context.Context, s *port.Session) error {
	const query = `
		INSERT INTO auth_session
			(id, user_id, issued_at, expires_at, last_seen_at, ip_address, user_agent, cashier_desk_id)
		VALUES ($1, $2, COALESCE($3, now()), $4, COALESCE($3, now()), $5, $6, $7)
		RETURNING issued_at, last_seen_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		s.ID, s.UserID, instant(s.IssuedAt), s.ExpiresAt, s.IPAddress, s.UserAgent, s.CashierDeskID,
	).Scan(&s.IssuedAt, &s.LastSeenAt)
	return pg.WrapQuery("session.Create", err)
}

// GetByID returns one session, revoked or not. The caller decides what a
// revoked session means; a repository that hid them could not answer "why was
// I signed out".
func (r *SessionRepository) GetByID(ctx context.Context, id shared.ID) (*port.Session, error) {
	const query = `SELECT` + sessionColumns + ` FROM auth_session WHERE id = $1`

	q := r.db.Conn(ctx)
	s, err := scanSession(q.QueryRow(ctx, query, id))
	if err != nil {
		return nil, pg.WrapQuery("session.GetByID", err)
	}
	return s, nil
}

// ListForUser returns a user's sessions, newest first.
func (r *SessionRepository) ListForUser(ctx context.Context, userID shared.ID, includeEnded bool) ([]*port.Session, error) {
	const query = `
		SELECT` + sessionColumns + `
		FROM auth_session
		WHERE user_id = $1
		  AND ($2::boolean OR (revoked_at IS NULL AND expires_at > now()))
		ORDER BY issued_at DESC
		LIMIT 200`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, userID, includeEnded)
	if err != nil {
		return nil, pg.WrapQuery("session.ListForUser", err)
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.Session, error) {
		return scanSession(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("session.ListForUser", err)
	}
	return sessions, nil
}

// Revoke ends one session.
//
// Idempotent by construction: the WHERE clause skips an already-revoked row and
// the statement still succeeds. A client retrying a logout after a lost
// response must not be told the logout failed, and the first revocation is the
// one worth keeping — it names who actually ended the session.
func (r *SessionRepository) Revoke(ctx context.Context, id shared.ID, by shared.ID, reason string, at time.Time) error {
	const query = `
		UPDATE auth_session
		SET revoked_at = COALESCE($4, now()), revoked_by = $2, revoked_reason = $3
		WHERE id = $1 AND revoked_at IS NULL`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, id, idOrNil(by), reason, instant(at))
	return pg.WrapQuery("session.Revoke", err)
}

// RevokeAllForUser ends every live session, optionally sparing one.
//
// The exception exists so "sign out my other sessions" does not sign the
// operator out of the session they issued it from — which would make the
// feature useless at a cashier desk mid-shift.
func (r *SessionRepository) RevokeAllForUser(
	ctx context.Context, userID shared.ID, except *shared.ID, by shared.ID, reason string, at time.Time,
) (int, error) {
	const query = `
		UPDATE auth_session
		SET revoked_at = COALESCE($5, now()), revoked_by = $3, revoked_reason = $4
		WHERE user_id = $1
		  AND revoked_at IS NULL
		  AND ($2::uuid IS NULL OR id <> $2::uuid)`

	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, query, userID, except, idOrNil(by), reason, instant(at))
	if err != nil {
		return 0, pg.WrapQuery("session.RevokeAllForUser", err)
	}
	return int(tag.RowsAffected()), nil
}

// Touch records that a session was seen.
func (r *SessionRepository) Touch(ctx context.Context, id shared.ID, at time.Time) error {
	const query = `UPDATE auth_session SET last_seen_at = COALESCE($2, now()) WHERE id = $1`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, id, instant(at))
	return pg.WrapQuery("session.Touch", err)
}

// PurgeExpired deletes sessions past their expiry.
//
// Sessions are not an audit trail — the audit log records the sign-in and the
// revocation — so keeping expired rows forever would grow a table that is read
// on every refresh for no benefit.
func (r *SessionRepository) PurgeExpired(ctx context.Context, before time.Time) (int64, error) {
	const query = `DELETE FROM auth_session WHERE expires_at < $1`

	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, query, before)
	if err != nil {
		return 0, pg.WrapQuery("session.PurgeExpired", err)
	}
	return tag.RowsAffected(), nil
}

// LoginAttemptRepository records sign-in attempts.
type LoginAttemptRepository struct{ db *pg.DB }

// NewLoginAttemptRepository builds the attempt store over a connection pool.
func NewLoginAttemptRepository(db *pg.DB) *LoginAttemptRepository {
	return &LoginAttemptRepository{db: db}
}

var _ port.LoginAttemptRepository = (*LoginAttemptRepository)(nil)

// Record appends one attempt.
func (r *LoginAttemptRepository) Record(ctx context.Context, a port.LoginAttempt) error {
	const query = `
		INSERT INTO auth_login_attempt
			(id, username, user_id, succeeded, failure_code, ip_address, user_agent, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, now()))`

	if a.ID == shared.NilID {
		a.ID = shared.NewID()
	}
	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query,
		a.ID, a.Username, a.UserID, a.Succeeded, a.FailureCode,
		a.IPAddress, a.UserAgent, instant(a.OccurredAt))
	return pg.WrapQuery("login_attempt.Record", err)
}

// CountRecentFailures counts failures against a username inside a window.
//
// Keyed by username rather than by user, because the attempts worth throttling
// hardest are the ones against names that do not exist: those have no user row
// to hold a counter, and an attacker enumerating names would otherwise never
// meet a limit other than the shared campus-address budget.
func (r *LoginAttemptRepository) CountRecentFailures(ctx context.Context, username string, since time.Time) (int, error) {
	const query = `
		SELECT count(*)
		FROM auth_login_attempt
		WHERE username = $1 AND NOT succeeded AND occurred_at >= $2`

	q := r.db.Conn(ctx)
	var count int
	if err := q.QueryRow(ctx, query, username, since).Scan(&count); err != nil {
		return 0, pg.WrapQuery("login_attempt.CountRecentFailures", err)
	}
	return count, nil
}

// ListForUser returns recent attempts against one account.
func (r *LoginAttemptRepository) ListForUser(ctx context.Context, userID shared.ID, limit int) ([]port.LoginAttempt, error) {
	const query = `
		SELECT id, username, user_id, succeeded, failure_code, ip_address, user_agent, occurred_at
		FROM auth_login_attempt
		WHERE user_id = $1
		ORDER BY occurred_at DESC
		LIMIT $2`

	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, userID, limit)
	if err != nil {
		return nil, pg.WrapQuery("login_attempt.ListForUser", err)
	}
	attempts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.LoginAttempt, error) {
		var a port.LoginAttempt
		err := row.Scan(&a.ID, &a.Username, &a.UserID, &a.Succeeded, &a.FailureCode,
			&a.IPAddress, &a.UserAgent, &a.OccurredAt)
		return a, err
	})
	if err != nil {
		return nil, pg.WrapQuery("login_attempt.ListForUser", err)
	}
	return attempts, nil
}

// PurgeBefore deletes attempts older than the retention window.
func (r *LoginAttemptRepository) PurgeBefore(ctx context.Context, before time.Time) (int64, error) {
	const query = `DELETE FROM auth_login_attempt WHERE occurred_at < $1`

	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, query, before)
	if err != nil {
		return 0, pg.WrapQuery("login_attempt.PurgeBefore", err)
	}
	return tag.RowsAffected(), nil
}
