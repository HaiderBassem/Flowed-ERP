package app

import (
	"context"
	"log/slog"
	"time"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/port"
)

// TokenIssuer is the subset of the platform token service the login flow needs.
type TokenIssuer interface {
	IssuePair(u *port.User, deskID *shared.ID) (auth.Pair, error)
	RenewFromRefresh(u *port.User, deskID *shared.ID, sessionID string) (string, time.Time, error)
	ParseRefresh(token string) (*auth.Claims, error)
	RefreshTTL() time.Duration
}

// AuthService owns signing in, renewing and signing out.
//
// The logic lived in the HTTP handler, which meant the throttling decision, the
// session record and the audit entry were all transport concerns — and a second
// entry point (the CLI, a future portal) would have had to reimplement them or
// go without. It is a command like any other now: authority, transaction,
// audit.
type AuthService struct {
	deps     Deps
	tokens   TokenIssuer
	hasher   PasswordHasher
	sessions port.SessionRepository
	logins   port.LoginAttemptRepository
	policy   LockoutPolicy
	auditor
}

// NewAuthService wires the credential commands.
func NewAuthService(
	d Deps, tokens TokenIssuer, hasher PasswordHasher,
	sessions port.SessionRepository, logins port.LoginAttemptRepository, policy LockoutPolicy,
) *AuthService {
	if policy.MaxFailures < 0 {
		policy = DefaultLockoutPolicy()
	}
	return &AuthService{
		deps: d, tokens: tokens, hasher: hasher,
		sessions: sessions, logins: logins, policy: policy,
		auditor: newAuditor(d.Audit, d.Clock),
	}
}

// LoginInput is one sign-in attempt.
type LoginInput struct {
	Username      string
	Password      string
	CashierDeskID *shared.ID
	IPAddress     string
	UserAgent     string
}

// LoginResult carries the issued credentials and the account behind them.
type LoginResult struct {
	Pair auth.Pair
	User *port.User
}

// invalidCredentials is the single answer every failure gives.
//
// One message and one code for an unknown user, a wrong password and a
// disabled account. Distinguishing them tells an attacker which half to keep
// trying, and in a finance office knowing which accounts exist is most of the
// work.
func invalidCredentials() error {
	return shared.Unauthorized("auth.invalid_credentials", "the username or password is incorrect")
}

// Login authenticates an operator and opens a session.
func (s *AuthService) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	now := nowOr(s.deps.Clock)

	// Throttling is checked against the username before the password, and the
	// username need not exist. An attacker cycling through names would
	// otherwise meet no limit but the per-IP budget, which at a university is
	// shared by a whole hall of terminals and therefore sized far too high to
	// stop guessing one account.
	if s.policy.MaxFailures > 0 {
		failures, err := s.logins.CountRecentFailures(ctx, in.Username, now.Add(-s.policy.Window))
		if err != nil {
			// A throttling store that cannot be read must not stop the desks
			// working; the account-level counter below still applies.
			s.logf(ctx, "counting recent login failures", err)
		} else if failures >= s.policy.MaxFailures {
			s.recordAttempt(ctx, in, nil, false, "throttled", now)
			return nil, shared.TooManyRequests("auth.too_many_attempts",
				"too many failed sign-in attempts for this account; wait %s and try again",
				s.policy.LockFor).
				WithDetail("retry_after_seconds", int(s.policy.LockFor.Seconds()))
		}
	}

	user, err := s.deps.Users.GetByUsername(ctx, in.Username)
	if err != nil {
		// An unknown username still pays for a bcrypt comparison, so it cannot
		// be told from a known one by how long the answer took.
		if verifyErr := s.hasher.Verify(decoyHashForTiming, in.Password); verifyErr == nil {
			_ = verifyErr
		}
		s.recordAttempt(ctx, in, nil, false, "unknown_user", now)
		return nil, invalidCredentials()
	}

	if user.IsLocked(now) {
		s.recordAttempt(ctx, in, &user.ID, false, "locked", now)
		return nil, shared.TooManyRequests("auth.account_locked",
			"this account is locked after repeated failed attempts; it unlocks automatically").
			WithDetail("locked_until", user.LockedUntil.UTC().Format(time.RFC3339))
	}

	if err := s.hasher.Verify(user.PasswordHash, in.Password); err != nil {
		s.registerFailure(ctx, in, user, now)
		return nil, invalidCredentials()
	}

	// Checked after the password so that a disabled account cannot be told
	// apart from a wrong password by somebody holding neither.
	if !user.IsActive {
		s.recordAttempt(ctx, in, &user.ID, false, "disabled", now)
		return nil, invalidCredentials()
	}

	// A cashier without a desk cannot take cash: receipt series run per year
	// per desk, and a collection with no desk has no paper book to reconcile
	// against.
	if user.HasRole(shared.RoleCashier) && in.CashierDeskID == nil {
		s.recordAttempt(ctx, in, &user.ID, false, "desk_required", now)
		return nil, shared.Validation("auth.cashier_desk_required",
			"a cashier must sign in at a specific desk; receipt numbering is per desk").
			WithDetail("field", "cashier_desk_id")
	}

	pair, err := s.tokens.IssuePair(user, in.CashierDeskID)
	if err != nil {
		return nil, err
	}
	sessionID, err := shared.ParseID(pair.SessionID)
	if err != nil {
		return nil, shared.Internal("auth.session_id_invalid", err,
			"the issued session identifier is not a valid row key")
	}

	actor := shared.Actor{
		UserID:        user.ID,
		Username:      user.Username,
		Roles:         user.Roles,
		CashierDeskID: in.CashierDeskID,
		SessionID:     pair.SessionID,
		IPAddress:     in.IPAddress,
		Scope:         user.Scope(),
	}

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.sessions.Create(ctx, &port.Session{
			ID:            sessionID,
			UserID:        user.ID,
			IssuedAt:      now,
			ExpiresAt:     pair.RefreshExpiresAt,
			IPAddress:     textOrNil(in.IPAddress),
			UserAgent:     textOrNil(in.UserAgent),
			CashierDeskID: in.CashierDeskID,
		}); err != nil {
			return err
		}
		if err := s.deps.Users.ClearLoginFailures(ctx, user.ID); err != nil {
			return err
		}
		if err := s.deps.Users.RecordLogin(ctx, user.ID, now); err != nil {
			return err
		}
		if err := s.logins.Record(ctx, port.LoginAttempt{
			Username:   in.Username,
			UserID:     &user.ID,
			Succeeded:  true,
			IPAddress:  textOrNil(in.IPAddress),
			UserAgent:  textOrNil(in.UserAgent),
			OccurredAt: now,
		}); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "session",
			EntityID:   &sessionID,
			Action:     "session.opened",
			Actor:      actor,
			Metadata: map[string]any{
				"username":   user.Username,
				"roles":      user.Roles,
				"scope_mode": string(user.Scope().Mode),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	return &LoginResult{Pair: pair, User: user}, nil
}

// registerFailure increments the account counter and locks it if the policy
// says so.
//
// Deliberately in its own transaction. Folding it into the caller's would roll
// the increment back with the failed login, and the counter would never reach
// its threshold no matter how many attempts arrived.
func (s *AuthService) registerFailure(ctx context.Context, in LoginInput, user *port.User, now time.Time) {
	// Counted here as well as written to the attempt table: the table answers
	// "who", days later and under access control, and the counter answers
	// "is something happening right now" without naming anybody.
	s.deps.Metrics.AuthFailure(ctx, "bad_password")

	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		count, err := s.deps.Users.RecordFailedLogin(ctx, user.ID, now)
		if err != nil {
			return err
		}
		if s.policy.MaxFailures > 0 && count >= s.policy.MaxFailures {
			if err := s.deps.Users.LockAccount(ctx, user.ID, now.Add(s.policy.LockFor)); err != nil {
				return err
			}
		}
		return s.logins.Record(ctx, port.LoginAttempt{
			Username:    in.Username,
			UserID:      &user.ID,
			Succeeded:   false,
			FailureCode: ptr("bad_password"),
			IPAddress:   textOrNil(in.IPAddress),
			UserAgent:   textOrNil(in.UserAgent),
			OccurredAt:  now,
		})
	})
	if err != nil {
		s.logf(ctx, "recording a failed sign-in", err)
	}
}

// recordAttempt writes one attempt outside the caller's error path.
func (s *AuthService) recordAttempt(
	ctx context.Context, in LoginInput, userID *shared.ID, succeeded bool, code string, now time.Time,
) {
	if !succeeded {
		// The reason is a fixed vocabulary and the username is deliberately
		// absent: a metric label outlives the request in a store with none of
		// the database's access control, and the login history endpoint is
		// where the identifiable detail belongs.
		s.deps.Metrics.AuthFailure(ctx, code)
	}

	err := s.logins.Record(ctx, port.LoginAttempt{
		Username:    in.Username,
		UserID:      userID,
		Succeeded:   succeeded,
		FailureCode: &code,
		IPAddress:   textOrNil(in.IPAddress),
		UserAgent:   textOrNil(in.UserAgent),
		OccurredAt:  now,
	})
	if err != nil {
		s.logf(ctx, "recording a sign-in attempt", err)
	}
}

// RefreshInput renews an access token.
type RefreshInput struct {
	RefreshToken string
	IPAddress    string
}

// RefreshResult carries the new access token.
type RefreshResult struct {
	AccessToken string
	ExpiresAt   time.Time
	User        *port.User
	SessionID   string
}

// Refresh renews an access token from a refresh token.
//
// Three checks, and each has caught something real in systems that skipped it:
// the session must still exist and not be revoked (this is what makes logout
// and disablement immediate); the user is re-read rather than trusted from the
// token (roles and the active flag may have changed); and the renewed token
// carries the user's current scope rather than the scope frozen at sign-in.
func (s *AuthService) Refresh(ctx context.Context, in RefreshInput) (*RefreshResult, error) {
	claims, err := s.tokens.ParseRefresh(in.RefreshToken)
	if err != nil {
		return nil, err
	}

	userID, err := shared.ParseID(claims.Subject)
	if err != nil {
		return nil, shared.Unauthorized("auth.invalid_token", "the refresh token is malformed")
	}
	sessionID, err := shared.ParseID(claims.SessionID)
	if err != nil {
		return nil, shared.Unauthorized("auth.invalid_token", "the refresh token names no session")
	}

	now := nowOr(s.deps.Clock)
	session, err := s.sessions.GetByID(ctx, sessionID)
	if err != nil {
		// A session row that is gone means it expired and was purged, or the
		// token predates session tracking. Either way it cannot be renewed.
		return nil, shared.Unauthorized("auth.session_unknown",
			"this session is no longer valid; sign in again")
	}
	if !session.Active(now) {
		reason := "expired"
		if session.RevokedAt != nil {
			reason = "revoked"
		}
		s.deps.Metrics.AuthFailure(ctx, "session_revoked")
		return nil, shared.Unauthorized("auth.session_revoked",
			"this session was ended; sign in again").
			WithDetail("reason", reason)
	}

	user, err := s.deps.Users.GetByID(ctx, userID)
	if err != nil {
		return nil, shared.Unauthorized("auth.invalid_token", "the refresh token is no longer valid")
	}
	if !user.IsActive {
		s.deps.Metrics.AuthFailure(ctx, "account_disabled")
		return nil, shared.Unauthorized("auth.account_disabled", "this account is disabled")
	}
	if user.IsLocked(now) {
		return nil, shared.Unauthorized("auth.account_locked", "this account is locked")
	}

	var deskID *shared.ID
	if claims.CashierDeskID != nil {
		parsed, err := shared.ParseID(*claims.CashierDeskID)
		if err != nil {
			return nil, shared.Unauthorized("auth.invalid_token", "the token carries an invalid desk")
		}
		deskID = &parsed
	}

	access, expiresAt, err := s.tokens.RenewFromRefresh(user, deskID, claims.SessionID)
	if err != nil {
		return nil, err
	}

	if err := s.sessions.Touch(ctx, sessionID, now); err != nil {
		// Losing the last-seen stamp costs an operator's session list some
		// precision. It must not cost them their shift.
		s.logf(ctx, "touching a session", err)
	}

	return &RefreshResult{
		AccessToken: access,
		ExpiresAt:   expiresAt,
		User:        user,
		SessionID:   claims.SessionID,
	}, nil
}

// Logout ends the session the request arrived on.
//
// Idempotent: a client retrying after a lost response, or one that had already
// been signed out by an administrator, gets a success. A logout that can fail
// is a logout people stop trusting and work around by closing the tab.
func (s *AuthService) Logout(ctx context.Context, actor shared.Actor) error {
	sessionID, err := shared.ParseID(actor.SessionID)
	if err != nil {
		// A token predating session tracking has nothing to revoke. The client
		// discards it either way.
		return nil
	}

	return s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.sessions.Revoke(ctx, sessionID, actor.UserID, "signed out", nowOr(s.deps.Clock)); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "session",
			EntityID:   &sessionID,
			Action:     "session.closed",
			Actor:      actor,
		})
	})
}

// SessionActive reports whether a session may still be used.
//
// Called by the authentication middleware when strict session checking is on:
// it turns an access token into a credential that can be withdrawn within
// seconds rather than at the end of its lifetime, at the cost of one indexed
// primary-key lookup per request.
func (s *AuthService) SessionActive(ctx context.Context, sessionID shared.ID) (bool, error) {
	session, err := s.sessions.GetByID(ctx, sessionID)
	if err != nil {
		// A session that is not there is not an error to the caller: it was
		// purged after expiry, or it belongs to a token issued before session
		// tracking. Either way the answer is "not active".
		if shared.KindOf(err) == shared.KindNotFound {
			return false, nil
		}
		return false, err
	}
	return session.Active(nowOr(s.deps.Clock)), nil
}

func (s *AuthService) logf(ctx context.Context, what string, err error) {
	if s.deps.Log == nil {
		return
	}
	s.deps.Log.WarnContext(ctx, what, slog.String("error", err.Error()))
}

func textOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// decoyHashForTiming is a bcrypt digest of a value nothing matches. Verifying
// against it costs the same as verifying a real user's hash, which is what
// keeps an unknown username from answering measurably faster than a known one.
const decoyHashForTiming = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
