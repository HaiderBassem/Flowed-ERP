package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// UserRepository stores application users and the roles they hold.
type UserRepository struct{ db *pg.DB }

// NewUserRepository builds the user store over a connection pool.
func NewUserRepository(db *pg.DB) *UserRepository { return &UserRepository{db: db} }

var _ port.UserRepository = (*UserRepository)(nil)

// Roles live in their own table, so every read aggregates them back into the
// user rather than issuing a second query per row.
const userColumns = `
	u.id, u.username, u.full_name, u.password_hash, u.email,
	u.is_active, u.last_login_at, u.created_at, u.updated_at,
	COALESCE(array_agg(r.role ORDER BY r.role) FILTER (WHERE r.role IS NOT NULL), '{}') AS roles`

const userFrom = `
	FROM app_user u
	LEFT JOIN app_user_role r ON r.user_id = u.id`

func scanUser(row pgx.Row) (*port.User, error) {
	var (
		u     port.User
		roles []string
	)
	if err := row.Scan(
		&u.ID, &u.Username, &u.FullName, &u.PasswordHash, &u.Email,
		&u.IsActive, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt, &roles,
	); err != nil {
		return nil, err
	}
	u.Roles = fromStrings[shared.Role](roles)
	return &u, nil
}

// Create records an operator together with any roles they were granted. A user
// with no roles can sign in and do nothing, which is the correct default.
func (r *UserRepository) Create(ctx context.Context, u *port.User) error {
	if err := r.db.RequireTx(ctx, "user.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO app_user (id, username, full_name, password_hash, email, is_active, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		u.ID, u.Username, u.FullName, u.PasswordHash, u.Email, u.IsActive, u.LastLoginAt,
	).Scan(&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return pg.WrapQuery("user.Create", err)
	}
	if len(u.Roles) == 0 {
		return nil
	}
	return r.replaceRoles(ctx, "user.Create.roles", u.ID, u.Roles, nil)
}

// Update writes back the user's details. Roles move through SetRoles, which
// records who granted them.
func (r *UserRepository) Update(ctx context.Context, u *port.User) error {
	if err := r.db.RequireTx(ctx, "user.Update"); err != nil {
		return err
	}
	const query = `
		UPDATE app_user SET
			username      = $2,
			full_name     = $3,
			password_hash = $4,
			email         = $5,
			is_active     = $6
		WHERE id = $1
		RETURNING updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query, u.ID, u.Username, u.FullName, u.PasswordHash, u.Email, u.IsActive).
		Scan(&u.UpdatedAt)
	return pg.WrapQuery("user.Update", err)
}

// GetByID returns one user with their roles.
func (r *UserRepository) GetByID(ctx context.Context, id shared.ID) (*port.User, error) {
	const query = `SELECT` + userColumns + userFrom + ` WHERE u.id = $1 GROUP BY u.id`

	q := r.db.Conn(ctx)
	u, err := scanUser(q.QueryRow(ctx, query, id))
	if err != nil {
		return nil, pg.WrapQuery("user.GetByID", err)
	}
	return u, nil
}

// GetByUsername returns the user behind a login name.
func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*port.User, error) {
	const query = `SELECT` + userColumns + userFrom + ` WHERE u.username = $1 GROUP BY u.id`

	q := r.db.Conn(ctx)
	u, err := scanUser(q.QueryRow(ctx, query, username))
	if err != nil {
		return nil, pg.WrapQuery("user.GetByUsername", err)
	}
	return u, nil
}

// List returns the operators, optionally only those still able to sign in.
func (r *UserRepository) List(ctx context.Context, activeOnly bool) ([]*port.User, error) {
	const query = `
		SELECT` + userColumns + userFrom + `
		WHERE NOT $1::boolean OR u.is_active
		GROUP BY u.id
		ORDER BY u.username`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, activeOnly)
	if err != nil {
		return nil, pg.WrapQuery("user.List", err)
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.User, error) {
		return scanUser(row)
	})
	if err != nil {
		return nil, pg.WrapQuery("user.List", err)
	}
	return users, nil
}

// SetRoles replaces a user's roles wholesale.
//
// Replacing rather than merging is what makes the separation-of-duties matrix
// enforceable: an administrator revoking the cashier role expects it gone, not
// left behind because the new list simply did not mention it.
func (r *UserRepository) SetRoles(ctx context.Context, userID shared.ID, roles []shared.Role, grantedBy shared.ID) error {
	if err := r.db.RequireTx(ctx, "user.SetRoles"); err != nil {
		return err
	}
	return r.replaceRoles(ctx, "user.SetRoles", userID, roles, idOrNil(grantedBy))
}

func (r *UserRepository) replaceRoles(
	ctx context.Context, operation string, userID shared.ID, roles []shared.Role, grantedBy *shared.ID,
) error {
	const (
		clear  = `DELETE FROM app_user_role WHERE user_id = $1`
		insert = `INSERT INTO app_user_role (user_id, role, granted_by) VALUES ($1, $2, $3)`
	)

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	batch.Queue(clear, userID)
	for _, role := range roles {
		batch.Queue(insert, userID, string(role), grantedBy)
	}
	return pg.WrapQuery(operation, execBatch(ctx, q, batch))
}

// RecordLogin stamps a successful sign-in.
func (r *UserRepository) RecordLogin(ctx context.Context, userID shared.ID, at time.Time) error {
	if err := r.db.RequireTx(ctx, "user.RecordLogin"); err != nil {
		return err
	}
	const query = `UPDATE app_user SET last_login_at = COALESCE($2, now()) WHERE id = $1 RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, userID, instant(at)).Scan(&id)
	return pg.WrapQuery("user.RecordLogin", err)
}
