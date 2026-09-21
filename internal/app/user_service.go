package app

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/port"
)

// PasswordHasher is the subset of the platform hasher the user commands need.
//
// An interface rather than the concrete type so these commands can be tested
// without paying bcrypt's cost per case, and so the hashing algorithm can be
// replaced without touching the command logic.
type PasswordHasher interface {
	Hash(plain string) (string, error)
	Verify(hash, plain string) error
}

// UserService administers operator accounts.
//
// Everything here was previously a shell command or a psql session: creating a
// user, disabling a leaver, resetting a forgotten password, narrowing somebody
// after a transfer. In a university those are weekly events, so in practice
// they were performed by whoever held the database password — which is the
// opposite of the separation of duties the financial commands enforce so
// carefully. Each one is now an audited command with an authority check.
type UserService struct {
	deps    Deps
	hasher  PasswordHasher
	users   port.UserRepository
	session port.SessionRepository
	logins  port.LoginAttemptRepository
	auditor
}

// NewUserService wires the operator-administration commands.
func NewUserService(d Deps, hasher PasswordHasher, sessions port.SessionRepository, logins port.LoginAttemptRepository) *UserService {
	return &UserService{
		deps:    d,
		hasher:  hasher,
		users:   d.Users,
		session: sessions,
		logins:  logins,
		auditor: newAuditor(d.Audit, d.Clock),
	}
}

// CreateUserInput describes a new operator.
type CreateUserInput struct {
	Username string
	FullName string
	Email    *string
	Roles    []shared.Role
	// Password may be empty, in which case one is generated and returned once.
	// A generated password is preferable: an administrator inventing passwords
	// for a hall of cashiers invents the same one.
	Password string
	// ScopeMode and the two lists bound the account organisationally. An empty
	// mode means university-wide, which is what every account held before
	// scoping existed.
	ScopeMode   shared.ScopeMode
	Colleges    []shared.ID
	Departments []shared.ID
}

// CreateUserResult carries the account and, when one was generated, the
// temporary password. The password is returned exactly once and never stored in
// readable form.
type CreateUserResult struct {
	User *port.User
	// TemporaryPassword is set only when the service generated it.
	TemporaryPassword string
}

// CreateUser registers an operator.
func (s *UserService) CreateUser(ctx context.Context, actor shared.Actor, in CreateUserInput) (*CreateUserResult, error) {
	username := strings.ToLower(strings.TrimSpace(in.Username))
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.FullName) == "" {
		return nil, shared.Validation("user.full_name_required",
			"an operator record needs the person's name; the audit trail names them, not their login")
	}
	if err := validateRoles(in.Roles); err != nil {
		return nil, err
	}
	scopeMode, err := normaliseScope(in.ScopeMode, in.Colleges, in.Departments)
	if err != nil {
		return nil, err
	}

	password := in.Password
	generated := false
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return nil, err
		}
		generated = true
	}
	if err := auth.ValidatePasswordFor(password, username, in.FullName); err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return nil, err
	}

	now := nowOr(s.deps.Clock)
	user := &port.User{
		ID:           shared.NewID(),
		Username:     username,
		FullName:     strings.TrimSpace(in.FullName),
		PasswordHash: hash,
		Email:        in.Email,
		Roles:        in.Roles,
		IsActive:     true,
		// A password somebody else chose is a shared secret until its holder
		// replaces it. It authenticates and nothing else.
		MustChangePassword: true,
		PasswordChangedAt:  &now,
		ScopeMode:          scopeMode,
		Colleges:           in.Colleges,
		Departments:        in.Departments,
	}

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.users.Create(ctx, user); err != nil {
			return err
		}
		if err := s.users.SetScope(ctx, user.ID, scopeMode, in.Colleges, in.Departments, actor.UserID); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     "user.created",
			Actor:      actor,
			After:      redactUser(user),
			Metadata: map[string]any{
				"username":            user.Username,
				"roles":               user.Roles,
				"scope_mode":          string(scopeMode),
				"password_generated":  generated,
				"college_grants":      len(in.Colleges),
				"department_grants":   len(in.Departments),
				"must_change_on_next": true,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	result := &CreateUserResult{User: user}
	if generated {
		result.TemporaryPassword = password
	}
	return result, nil
}

// SetRolesInput changes what an operator may do.
type SetRolesInput struct {
	UserID shared.ID
	Roles  []shared.Role
	Reason string
}

// SetRoles replaces an operator's roles.
//
// An actor cannot change their own. Self-elevation is the failure this guards:
// an administrator is deliberately barred from posting payments, and without
// this rule that separation lasts exactly as long as it takes them to grant
// themselves the cashier role. Somebody else has to do it, and the audit trail
// then names two people.
func (s *UserService) SetRoles(ctx context.Context, actor shared.Actor, in SetRolesInput) (*port.User, error) {
	if in.UserID == actor.UserID {
		return nil, shared.Forbidden("user.self_role_change",
			"an operator cannot change their own roles").
			WithDetail("remedy", "ask another administrator to make this change")
	}
	if err := validateRoles(in.Roles); err != nil {
		return nil, err
	}

	var user *port.User
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		user, err = s.users.GetByID(ctx, in.UserID)
		if err != nil {
			return err
		}
		before := redactUser(user)

		// The last administrator cannot be stripped of the role. Nobody would
		// then be able to restore it, and the recovery is a database session —
		// exactly the out-of-band access this service exists to remove.
		if user.HasRole(shared.RoleAdmin) && !containsRole(in.Roles, shared.RoleAdmin) {
			if err := s.requireAnotherAdmin(ctx, user.ID); err != nil {
				return err
			}
		}

		if err := s.users.SetRoles(ctx, user.ID, in.Roles, actor.UserID); err != nil {
			return err
		}
		user.Roles = in.Roles

		// Roles travel in the token, so a narrowed operator would keep the old
		// set until it expired. Ending their sessions makes the change take
		// effect on their next request rather than at the end of their shift.
		if _, err := s.session.RevokeAllForUser(ctx, user.ID, nil, actor.UserID,
			"roles changed", nowOr(s.deps.Clock)); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     "user.roles_changed",
			Actor:      actor,
			Before:     before,
			After:      redactUser(user),
			Reason:     reasonOrNil(in.Reason),
			Metadata:   map[string]any{"roles": in.Roles},
		})
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// SetScopeInput narrows or widens an operator's organisational reach.
type SetScopeInput struct {
	UserID      shared.ID
	Mode        shared.ScopeMode
	Colleges    []shared.ID
	Departments []shared.ID
	Reason      string
}

// SetScope replaces an operator's organisational grants.
func (s *UserService) SetScope(ctx context.Context, actor shared.Actor, in SetScopeInput) (*port.User, error) {
	if in.UserID == actor.UserID {
		return nil, shared.Forbidden("user.self_scope_change",
			"an operator cannot widen their own scope").
			WithDetail("remedy", "ask another administrator to make this change")
	}
	mode, err := normaliseScope(in.Mode, in.Colleges, in.Departments)
	if err != nil {
		return nil, err
	}

	var user *port.User
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		user, err = s.users.GetByID(ctx, in.UserID)
		if err != nil {
			return err
		}
		before := redactUser(user)

		// Every referenced college and department must exist. A grant naming a
		// row that was never created is a scope that silently covers nothing,
		// and the operator would report "I cannot see my college" while the
		// grant looks correct in the administration screen.
		for _, id := range in.Colleges {
			if _, err := s.deps.Reference.GetCollege(ctx, id); err != nil {
				return err
			}
		}
		for _, id := range in.Departments {
			if _, err := s.deps.Reference.GetDepartment(ctx, id); err != nil {
				return err
			}
		}

		if err := s.users.SetScope(ctx, user.ID, mode, in.Colleges, in.Departments, actor.UserID); err != nil {
			return err
		}
		user.ScopeMode = mode
		user.Colleges = in.Colleges
		user.Departments = in.Departments

		if _, err := s.session.RevokeAllForUser(ctx, user.ID, nil, actor.UserID,
			"scope changed", nowOr(s.deps.Clock)); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     "user.scope_changed",
			Actor:      actor,
			Before:     before,
			After:      redactUser(user),
			Reason:     reasonOrNil(in.Reason),
			Metadata: map[string]any{
				"scope_mode":  string(mode),
				"colleges":    idStrings(in.Colleges),
				"departments": idStrings(in.Departments),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// SetActiveInput enables or disables an account.
type SetActiveInput struct {
	UserID shared.ID
	Active bool
	Reason string
}

// SetActive disables a leaver or restores an account.
//
// Disabling revokes every live session in the same transaction. Without that
// the account is refused at the next sign-in while the token already in a
// browser keeps working for the rest of its lifetime — which is precisely the
// window somebody who has just been dismissed would use.
func (s *UserService) SetActive(ctx context.Context, actor shared.Actor, in SetActiveInput) (*port.User, error) {
	if in.UserID == actor.UserID && !in.Active {
		return nil, shared.Forbidden("user.self_disable",
			"an operator cannot disable their own account").
			WithDetail("remedy", "ask another administrator, or sign out instead")
	}
	if !in.Active && strings.TrimSpace(in.Reason) == "" {
		return nil, shared.Validation("user.disable_reason_required",
			"disabling an account requires a reason; it is the first thing anyone asks afterwards")
	}

	var user *port.User
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		user, err = s.users.GetByID(ctx, in.UserID)
		if err != nil {
			return err
		}
		before := redactUser(user)

		if !in.Active && user.HasRole(shared.RoleAdmin) {
			if err := s.requireAnotherAdmin(ctx, user.ID); err != nil {
				return err
			}
		}

		now := nowOr(s.deps.Clock)
		if err := s.users.SetActive(ctx, user.ID, in.Active, actor.UserID, reasonOrNil(in.Reason), now); err != nil {
			return err
		}
		user.IsActive = in.Active

		revoked := 0
		if !in.Active {
			revoked, err = s.session.RevokeAllForUser(ctx, user.ID, nil, actor.UserID, "account disabled", now)
			if err != nil {
				return err
			}
		}

		action := "user.enabled"
		if !in.Active {
			action = "user.disabled"
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     action,
			Actor:      actor,
			Before:     before,
			After:      redactUser(user),
			Reason:     reasonOrNil(in.Reason),
			Metadata:   map[string]any{"sessions_revoked": revoked},
		})
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// ResetPasswordInput issues a new credential for somebody who cannot sign in.
type ResetPasswordInput struct {
	UserID shared.ID
	// Password may be empty, in which case one is generated and returned once.
	Password string
	Reason   string
}

// ResetPasswordResult carries the temporary password when one was generated.
type ResetPasswordResult struct {
	User              *port.User
	TemporaryPassword string
}

// ResetPassword replaces an operator's password on their behalf.
//
// The new credential is always marked must-change. An administrator who knows
// a working password for a cashier's account can act as that cashier, and the
// receipt would name the cashier — so the window in which that is possible is
// closed at the holder's next sign-in rather than left open indefinitely.
func (s *UserService) ResetPassword(ctx context.Context, actor shared.Actor, in ResetPasswordInput) (*ResetPasswordResult, error) {
	var (
		user      *port.User
		password  = in.Password
		generated bool
	)
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		user, err = s.users.GetByID(ctx, in.UserID)
		if err != nil {
			return err
		}
		if password == "" {
			password, err = generatePassword()
			if err != nil {
				return err
			}
			generated = true
		}
		if err := auth.ValidatePasswordFor(password, user.Username, user.FullName); err != nil {
			return err
		}
		hash, err := s.hasher.Hash(password)
		if err != nil {
			return err
		}

		now := nowOr(s.deps.Clock)
		if err := s.users.SetPassword(ctx, user.ID, hash, true, now); err != nil {
			return err
		}
		// Every session issued under the old password ends. A password reset
		// that left the previous sessions alive would not recover an account
		// somebody else is holding open.
		revoked, err := s.session.RevokeAllForUser(ctx, user.ID, nil, actor.UserID, "password reset", now)
		if err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     "user.password_reset",
			Actor:      actor,
			Reason:     reasonOrNil(in.Reason),
			Metadata: map[string]any{
				"username":           user.Username,
				"password_generated": generated,
				"sessions_revoked":   revoked,
			},
		})
	})
	if err != nil {
		return nil, err
	}

	result := &ResetPasswordResult{User: user}
	if generated {
		result.TemporaryPassword = password
	}
	return result, nil
}

// ChangeOwnPasswordInput replaces the caller's own password.
type ChangeOwnPasswordInput struct {
	CurrentPassword string
	NewPassword     string
	// KeepOtherSessions leaves other sessions alive. The default ends them,
	// because the usual reason for changing a password is suspecting somebody
	// else has it.
	KeepOtherSessions bool
}

// ChangeOwnPassword lets an operator replace their own credential.
//
// Available to every role including one holding no roles at all: an account
// that must change its password before doing anything else has to be able to
// do that much. The current password is required even though the caller is
// already authenticated — an unattended terminal is otherwise a permanent
// account takeover.
func (s *UserService) ChangeOwnPassword(ctx context.Context, actor shared.Actor, in ChangeOwnPasswordInput) error {
	if shared.IsNil(actor.UserID) {
		return shared.Unauthorized("user.unauthenticated", "this command requires a signed-in operator")
	}

	return s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		user, err := s.users.GetByID(ctx, actor.UserID)
		if err != nil {
			return err
		}
		if err := s.hasher.Verify(user.PasswordHash, in.CurrentPassword); err != nil {
			return shared.Unauthorized("user.current_password_incorrect",
				"the current password is incorrect")
		}
		if in.NewPassword == in.CurrentPassword {
			return shared.Validation("user.password_unchanged",
				"the new password must differ from the current one")
		}
		if err := auth.ValidatePasswordFor(in.NewPassword, user.Username, user.FullName); err != nil {
			return err
		}
		hash, err := s.hasher.Hash(in.NewPassword)
		if err != nil {
			return err
		}

		now := nowOr(s.deps.Clock)
		if err := s.users.SetPassword(ctx, user.ID, hash, false, now); err != nil {
			return err
		}

		revoked := 0
		if !in.KeepOtherSessions {
			var except *shared.ID
			if sessionID, err := shared.ParseID(actor.SessionID); err == nil {
				// Spare the session the request arrived on: signing the
				// operator out of the change they just made would look like
				// the change failed.
				except = &sessionID
			}
			revoked, err = s.session.RevokeAllForUser(ctx, user.ID, except, actor.UserID, "password changed", now)
			if err != nil {
				return err
			}
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &user.ID,
			Action:     "user.password_changed",
			Actor:      actor,
			Metadata:   map[string]any{"sessions_revoked": revoked},
		})
	})
}

// ListUsers returns the operators.
func (s *UserService) ListUsers(ctx context.Context, actor shared.Actor, activeOnly bool) ([]*port.User, error) {
	return s.users.List(ctx, activeOnly)
}

// GetUser returns one operator.
func (s *UserService) GetUser(ctx context.Context, actor shared.Actor, id shared.ID) (*port.User, error) {
	return s.users.GetByID(ctx, id)
}

// ListSessions returns an operator's sign-ins.
func (s *UserService) ListSessions(ctx context.Context, actor shared.Actor, userID shared.ID, includeEnded bool) ([]*port.Session, error) {
	return s.session.ListForUser(ctx, userID, includeEnded)
}

// RevokeSession ends one session.
func (s *UserService) RevokeSession(ctx context.Context, actor shared.Actor, sessionID shared.ID, reason string) error {
	return s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		session, err := s.session.GetByID(ctx, sessionID)
		if err != nil {
			return err
		}
		if reason == "" {
			reason = "revoked by operator"
		}
		if err := s.session.Revoke(ctx, sessionID, actor.UserID, reason, nowOr(s.deps.Clock)); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "session",
			EntityID:   &sessionID,
			Action:     "session.revoked",
			Actor:      actor,
			Reason:     reasonOrNil(reason),
			Metadata:   map[string]any{"session_user_id": session.UserID.String()},
		})
	})
}

// RevokeOtherSessions signs the caller out everywhere but here.
func (s *UserService) RevokeOtherSessions(ctx context.Context, actor shared.Actor) (int, error) {
	var revoked int
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var except *shared.ID
		if sessionID, err := shared.ParseID(actor.SessionID); err == nil {
			except = &sessionID
		}
		var err error
		revoked, err = s.session.RevokeAllForUser(ctx, actor.UserID, except, actor.UserID,
			"signed out other sessions", nowOr(s.deps.Clock))
		if err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "user",
			EntityID:   &actor.UserID,
			Action:     "user.other_sessions_revoked",
			Actor:      actor,
			Metadata:   map[string]any{"sessions_revoked": revoked},
		})
	})
	return revoked, err
}

// LoginHistory returns recent sign-in attempts against an account.
func (s *UserService) LoginHistory(ctx context.Context, actor shared.Actor, userID shared.ID, limit int) ([]port.LoginAttempt, error) {
	return s.logins.ListForUser(ctx, userID, limit)
}

// requireAnotherAdmin refuses an operation that would leave no administrator.
func (s *UserService) requireAnotherAdmin(ctx context.Context, excluding shared.ID) error {
	users, err := s.users.List(ctx, true)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID != excluding && u.HasRole(shared.RoleAdmin) {
			return nil
		}
	}
	return shared.PreconditionFailed("user.last_administrator",
		"this is the only active administrator; removing it would leave the system with no way "+
			"to grant anyone else that role").
		WithDetail("remedy", "appoint another administrator first")
}

// validateUsername enforces the shape the database check constraint requires,
// so the failure arrives as a readable message rather than as a constraint
// violation.
func validateUsername(username string) error {
	if len(username) < 3 || len(username) > 64 {
		return shared.Validation("user.username_length",
			"a username must be between 3 and 64 characters")
	}
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return shared.Validation("user.username_charset",
				"a username may contain lower-case letters, digits, dot, underscore and hyphen only").
				WithDetail("invalid_character", string(r))
		}
	}
	return nil
}

func validateRoles(roles []shared.Role) error {
	for _, role := range roles {
		if !role.Valid() {
			return shared.Validation("user.unknown_role",
				"%q is not a role this system recognises", role).
				WithDetail("known_roles", shared.StaffRoles)
		}
		// A student credential authenticates a person to see their own fees.
		// Granting it to an operator would create an actor that is both, and
		// every ownership check in the system would then have to decide which
		// half it was talking to.
		if !role.IsStaff() {
			return shared.Validation("user.not_a_staff_role",
				"%q is not a role an operator account may hold", role).
				WithDetail("known_roles", shared.StaffRoles)
		}
	}
	return nil
}

func containsRole(roles []shared.Role, want shared.Role) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

// normaliseScope validates the mode against the grants supplied with it.
func normaliseScope(mode shared.ScopeMode, colleges, departments []shared.ID) (shared.ScopeMode, error) {
	if mode == "" {
		mode = shared.ScopeUniversity
	}
	if !mode.Valid() {
		return "", shared.Validation("user.unknown_scope_mode",
			"%q is not a scope mode; use %q or %q", mode, shared.ScopeUniversity, shared.ScopeLimited)
	}
	if mode == shared.ScopeUniversity && (len(colleges) > 0 || len(departments) > 0) {
		return "", shared.Validation("user.scope_grants_ignored",
			"a university-wide account already reaches every college; grants would say otherwise "+
				"while changing nothing").
			WithDetail("remedy", "set scope_mode to \"scoped\" to make the grants meaningful")
	}
	if mode == shared.ScopeLimited && len(colleges) == 0 && len(departments) == 0 {
		return "", shared.Validation("user.scope_without_grants",
			"a scoped account with no grants can reach nothing at all").
			WithDetail("remedy", "name at least one college or department, or use university scope")
	}
	return mode, nil
}

// generatePassword mints a temporary credential.
//
// Base32 without padding: unambiguous when read aloud down a phone line or
// copied off a note, which is how a temporary password actually travels, and
// long enough that guessing it is not a strategy.
func generatePassword() (string, error) {
	buf := make([]byte, 15)
	if _, err := rand.Read(buf); err != nil {
		return "", shared.Internal("user.password_generation_failed", err,
			"the system entropy source is unavailable")
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

// redactUser renders a user for the audit trail without its password hash.
//
// The hash is not a secret in the sense the password is, but a trail that
// carries every historical hash is a trail that rewards stealing the trail.
func redactUser(u *port.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"id":                   u.ID.String(),
		"username":             u.Username,
		"full_name":            u.FullName,
		"email":                u.Email,
		"roles":                u.Roles,
		"is_active":            u.IsActive,
		"scope_mode":           string(u.ScopeMode),
		"colleges":             idStrings(u.Colleges),
		"departments":          idStrings(u.Departments),
		"must_change_password": u.MustChangePassword,
	}
}

func idStrings(ids []shared.ID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func reasonOrNil(reason string) *string {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// LockoutPolicy decides when repeated failures stop being typos.
//
// Both numbers are configuration rather than constants: a hall of shared
// terminals and a back office of individual logins want different answers, and
// the design's rule is that a policy the domain does not settle is configured
// rather than guessed.
type LockoutPolicy struct {
	// MaxFailures before the account is locked. Zero disables lockout.
	MaxFailures int
	// Window over which failures are counted.
	Window time.Duration
	// LockFor is how long the account stays locked.
	LockFor time.Duration
}

// DefaultLockoutPolicy is sized for a university desk: enough attempts that a
// cashier mistyping in Arabic layout is not locked out mid-shift, short enough
// that an unattended guess is not worth running.
func DefaultLockoutPolicy() LockoutPolicy {
	return LockoutPolicy{MaxFailures: 8, Window: 15 * time.Minute, LockFor: 15 * time.Minute}
}

// UpdateOwnProfileInput changes the caller's own name or username.
type UpdateOwnProfileInput struct {
	// FullName is what appears on a receipt beside "بواسطة" and in the audit
	// trail. Empty means leave it alone.
	FullName string
	// Username is what they sign in with. Empty means leave it alone.
	Username string
	// Email is optional and may be cleared by sending a single space, which is
	// distinguishable from "not supplied" in a way an empty string is not.
	Email *string
}

// UpdateOwnProfile lets an operator rename themselves.
//
// The name matters because it is printed: every receipt carries "بواسطة
// <name>", and an installation whose only account is called "System
// Administrator" hands the student a slip signed by nobody.
//
// Changing the username is allowed and is not cosmetic either — the first
// account is created by a script, and an office that has to keep signing in as
// "admin" because the system will not let them rename it is an office that
// shares one credential forever. Existing sessions survive: the token carries
// the user's identifier, not their name.
func (s *UserService) UpdateOwnProfile(
	ctx context.Context, actor shared.Actor, in UpdateOwnProfileInput,
) (*port.User, error) {
	if shared.IsNil(actor.UserID) {
		return nil, shared.Unauthorized("user.unauthenticated",
			"this command requires a signed-in operator")
	}

	var updated *port.User
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		user, err := s.users.GetByID(ctx, actor.UserID)
		if err != nil {
			return err
		}
		before := snapshotOf(user)

		if name := strings.TrimSpace(in.FullName); name != "" {
			user.FullName = name
		}
		if username := strings.ToLower(strings.TrimSpace(in.Username)); username != "" &&
			username != user.Username {
			if err := validateUsername(username); err != nil {
				return err
			}
			// Checked here as well as by the unique index, so the refusal
			// names the problem instead of arriving as a constraint violation
			// somebody has to decode.
			if existing, err := s.users.GetByUsername(ctx, username); err == nil && existing != nil {
				return shared.Conflict("user.username_taken",
					"another account already uses the username %q", username)
			}
			user.Username = username
		}
		if in.Email != nil {
			email := strings.TrimSpace(*in.Email)
			if email == "" {
				user.Email = nil
			} else {
				user.Email = &email
			}
		}

		if err := s.users.Update(ctx, user); err != nil {
			return err
		}
		updated = user

		return s.record(ctx, port.AuditEntry{
			EntityType: "app_user",
			EntityID:   &user.ID,
			Action:     "user.profile_updated",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(user),
			OccurredAt: nowOr(s.deps.Clock),
		})
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}
