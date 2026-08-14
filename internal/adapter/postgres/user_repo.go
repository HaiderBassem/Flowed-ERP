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

// Roles and scope grants live in their own tables, so every read aggregates
// them back into the user rather than issuing further queries per row. The
// scope arrays are aggregated in subqueries rather than joined: two LEFT JOINs
// against one row would multiply the role rows by the scope rows, and the
// aggregate would then count each role once per college.
const userColumns = `
	u.id, u.username, u.full_name, u.password_hash, u.email,
	u.is_active, u.last_login_at, u.created_at, u.updated_at,
	u.must_change_password, u.password_changed_at,
	u.failed_login_count, u.last_failed_login_at, u.locked_until,
	u.disabled_at, u.disabled_by, u.disabled_reason, u.scope_mode,
	COALESCE(array_agg(r.role ORDER BY r.role) FILTER (WHERE r.role IS NOT NULL), '{}') AS roles,
	COALESCE((SELECT array_agg(sc.college_id)
	          FROM app_user_scope sc
	          WHERE sc.user_id = u.id AND sc.college_id IS NOT NULL), '{}') AS colleges,
	COALESCE((SELECT array_agg(sd.department_id)
	          FROM app_user_scope sd
	          WHERE sd.user_id = u.id AND sd.department_id IS NOT NULL), '{}') AS departments`

const userFrom = `
	FROM app_user u
	LEFT JOIN app_user_role r ON r.user_id = u.id`

func scanUser(row pgx.Row) (*port.User, error) {
	var (
		u           port.User
		roles       []string
		scopeMode   string
		colleges    []shared.ID
		departments []shared.ID
	)
	if err := row.Scan(
		&u.ID, &u.Username, &u.FullName, &u.PasswordHash, &u.Email,
		&u.IsActive, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt,
		&u.MustChangePassword, &u.PasswordChangedAt,
		&u.FailedLoginCount, &u.LastFailedLogin, &u.LockedUntil,
		&u.DisabledAt, &u.DisabledBy, &u.DisabledReason, &scopeMode,
		&roles, &colleges, &departments,
	); err != nil {
		return nil, err
	}
	u.Roles = fromStrings[shared.Role](roles)
	u.ScopeMode = shared.ScopeMode(scopeMode)
	u.Colleges = colleges
	u.Departments = departments
	return &u, nil
}

// Create records an operator together with any roles they were granted. A user
// with no roles can sign in and do nothing, which is the correct default.
func (r *UserRepository) Create(ctx context.Context, u *port.User) error {
	if err := r.db.RequireTx(ctx, "user.Create"); err != nil {
		return err
	}
	const query = `
		INSERT INTO app_user (
			id, username, full_name, password_hash, email, is_active, last_login_at,
			must_change_password, password_changed_at, scope_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, now()), COALESCE($10, 'university'))
		RETURNING created_at, updated_at`

	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, query,
		u.ID, u.Username, u.FullName, u.PasswordHash, u.Email, u.IsActive, u.LastLoginAt,
		u.MustChangePassword, u.PasswordChangedAt, nullableScope(u.ScopeMode),
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

// SetScope replaces a user's organisational grants.
//
// Mode and grants move together in one statement pair inside the caller's
// transaction. Written separately, a crash between them could leave a user
// marked 'scoped' with the previous owner's colleges still attached, or
// 'university' with grants that no longer mean anything.
func (r *UserRepository) SetScope(
	ctx context.Context, userID shared.ID, mode shared.ScopeMode,
	colleges, departments []shared.ID, grantedBy shared.ID,
) error {
	if err := r.db.RequireTx(ctx, "user.SetScope"); err != nil {
		return err
	}
	const (
		setMode = `UPDATE app_user SET scope_mode = $2 WHERE id = $1`
		clear   = `DELETE FROM app_user_scope WHERE user_id = $1`
		addCol  = `INSERT INTO app_user_scope (id, user_id, college_id, granted_by) VALUES ($1, $2, $3, $4)`
		addDept = `INSERT INTO app_user_scope (id, user_id, department_id, granted_by) VALUES ($1, $2, $3, $4)`
	)

	q := r.db.Conn(ctx)
	batch := &pgx.Batch{}
	batch.Queue(setMode, userID, string(mode))
	batch.Queue(clear, userID)
	for _, id := range colleges {
		batch.Queue(addCol, shared.NewID(), userID, id, idOrNil(grantedBy))
	}
	for _, id := range departments {
		batch.Queue(addDept, shared.NewID(), userID, id, idOrNil(grantedBy))
	}
	return pg.WrapQuery("user.SetScope", execBatch(ctx, q, batch))
}

// RecordFailedLogin increments the counter and returns the value after it.
//
// The increment and the read are one statement so that two attempts arriving
// together cannot both read the same count and both decide they are the fifth.
// No transaction is required: this runs on the login path, which has none, and
// the row lock the UPDATE takes is released on its own commit.
func (r *UserRepository) RecordFailedLogin(ctx context.Context, userID shared.ID, at time.Time) (int, error) {
	const query = `
		UPDATE app_user
		SET failed_login_count = failed_login_count + 1,
		    last_failed_login_at = COALESCE($2, now())
		WHERE id = $1
		RETURNING failed_login_count`

	q := r.db.Conn(ctx)
	var count int
	err := q.QueryRow(ctx, query, userID, instant(at)).Scan(&count)
	if err != nil {
		return 0, pg.WrapQuery("user.RecordFailedLogin", err)
	}
	return count, nil
}

// ClearLoginFailures resets throttling after a successful sign-in.
func (r *UserRepository) ClearLoginFailures(ctx context.Context, userID shared.ID) error {
	const query = `
		UPDATE app_user
		SET failed_login_count = 0, locked_until = NULL
		WHERE id = $1 AND (failed_login_count <> 0 OR locked_until IS NOT NULL)`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, userID)
	return pg.WrapQuery("user.ClearLoginFailures", err)
}

// LockAccount refuses sign-in until the given moment.
func (r *UserRepository) LockAccount(ctx context.Context, userID shared.ID, until time.Time) error {
	const query = `UPDATE app_user SET locked_until = $2 WHERE id = $1`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query, userID, until)
	return pg.WrapQuery("user.LockAccount", err)
}

// SetPassword stores a new hash.
//
// The failure counter is cleared in the same statement: a password that has
// just been replaced cannot sensibly still be locked out for failures against
// the old one, and leaving the lock would strand an operator who did exactly
// what the reset told them to.
func (r *UserRepository) SetPassword(
	ctx context.Context, userID shared.ID, hash string, mustChange bool, at time.Time,
) error {
	if err := r.db.RequireTx(ctx, "user.SetPassword"); err != nil {
		return err
	}
	const query = `
		UPDATE app_user
		SET password_hash        = $2,
		    must_change_password = $3,
		    password_changed_at  = COALESCE($4, now()),
		    failed_login_count   = 0,
		    locked_until         = NULL
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, userID, hash, mustChange, instant(at)).Scan(&id)
	return pg.WrapQuery("user.SetPassword", err)
}

// SetActive enables or disables an account, recording who and why.
func (r *UserRepository) SetActive(
	ctx context.Context, userID shared.ID, active bool, by shared.ID, reason *string, at time.Time,
) error {
	if err := r.db.RequireTx(ctx, "user.SetActive"); err != nil {
		return err
	}
	const query = `
		UPDATE app_user
		SET is_active       = $2,
		    disabled_at     = CASE WHEN $2 THEN NULL ELSE COALESCE($5, now()) END,
		    disabled_by     = CASE WHEN $2 THEN NULL ELSE $3 END,
		    disabled_reason = CASE WHEN $2 THEN NULL ELSE $4 END
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, userID, active, idOrNil(by), reason, instant(at)).Scan(&id)
	return pg.WrapQuery("user.SetActive", err)
}

// nullableScope lets the column default apply when a caller left the mode
// unset, rather than writing an empty string the check constraint refuses.
func nullableScope(mode shared.ScopeMode) *string {
	if mode == "" {
		return nil
	}
	value := string(mode)
	return &value
}
