package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/port"
)

// TokenType separates the two tokens this service issues. It travels in the
// payload as the private claim "typ".
type TokenType string

const (
	// TokenTypeAccess authorises API calls and lives for one cashier shift.
	TokenTypeAccess TokenType = "access"
	// TokenTypeRefresh buys a new access token and lives for weeks. It
	// authorises nothing on its own.
	TokenTypeRefresh TokenType = "refresh"
)

// signingMethod is the only algorithm this service will sign with or accept.
//
// Pinning it is the whole of the JWT security model in practice. A parser that
// trusts the token's own "alg" header accepts "alg: none" — a token with no
// signature at all — and will verify an RS256 token using the HMAC secret as
// though it were a shared key. The second case is the classic forgery: the
// verification key of an RS256 pair is public, so anyone holding it can mint
// an administrator token that a permissive parser accepts. Restricting the
// method on both the issuing and the parsing side removes both attacks.
var signingMethod = jwt.SigningMethodHS256

// acceptedMethods is the allowlist handed to the parser.
var acceptedMethods = []string{signingMethod.Alg()}

// Claims is the payload of every token this service issues.
type Claims struct {
	jwt.RegisteredClaims

	// Username is carried so that logs and the audit trail can name the actor
	// without a database read on every request.
	Username string `json:"username"`

	// Roles are the actor's authorities at the moment the token was issued.
	// They therefore lag a revocation by up to one access-token lifetime, which
	// is why the access TTL is a shift rather than a month.
	Roles []string `json:"roles,omitempty"`

	// CashierDeskID scopes a cashier to a physical desk. Receipt series are
	// allocated per (academic year, desk), so a cashier whose token carries no
	// desk cannot post cash payments at all.
	CashierDeskID *string `json:"cashier_desk_id,omitempty"`

	// SessionID mirrors the registered "jti". It is duplicated under a plain
	// name so that the audit trail's field matches shared.Actor.SessionID
	// without every reader having to know JOSE vocabulary.
	SessionID string `json:"sid"`

	// Type is "access" or "refresh". The name collides with the JOSE header
	// parameter of the same spelling, which is unrelated: this one lives in the
	// payload and is signed along with everything else.
	Type TokenType `json:"typ"`

	// StudentID is set on a student's credential. It is what every route a
	// student can reach compares the row in question against, so it travels
	// with the token like the roles do.
	StudentID string `json:"student_id,omitempty"`

	// ScopeMode and the two lists carry the actor's organisational reach.
	//
	// Carried in the token rather than read per request for the same reason
	// roles are: the alternative is a database round trip on every call, at
	// every cashier desk, for a value that changes a few times a year. It lags
	// a change by at most one access-token lifetime, and the refresh path
	// re-reads the user, so narrowing somebody takes effect within a shift —
	// and revoking their session makes it immediate.
	ScopeMode string `json:"scope_mode,omitempty"`
	// ScopeColleges and ScopeDepartments are omitted entirely for a
	// university-wide actor, which is most of them, so the common token does
	// not grow.
	ScopeColleges    []string `json:"scope_colleges,omitempty"`
	ScopeDepartments []string `json:"scope_departments,omitempty"`
}

// Pair is an access token together with the refresh token that renews it. Both
// carry the same session id, so rotating an access token does not break the
// audit trail's view of one login.
type Pair struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	SessionID        string
}

// TokenService issues and verifies the tokens that carry an actor's identity
// across the stateless HTTP boundary.
type TokenService struct {
	keys       *Keyring
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	clock      shared.Clock
	parser     *jwt.Parser
}

// NewTokenService builds a token service from configuration.
func NewTokenService(cfg config.Auth) *TokenService {
	return NewTokenServiceWithClock(cfg, shared.SystemClock{})
}

// NewTokenServiceWithClock builds a token service that reads time from the
// supplied clock, for tests that need to mint or expire a token at a chosen
// instant. Both issuing and validation use the clock, so a fixed clock makes
// the whole lifecycle deterministic.
func NewTokenServiceWithClock(cfg config.Auth, clock shared.Clock) *TokenService {
	if clock == nil {
		clock = shared.SystemClock{}
	}
	// A malformed keyring cannot be returned from here without changing every
	// caller, and a service with no key would mint nothing anyway: fall back to
	// a ring holding just the configured secret, which is what the service did
	// before rotation existed. Configuration validation refuses an empty secret
	// long before this point.
	ring, err := NewKeyring(cfg.JWTSecret, cfg.RetiredJWTSecrets)
	if err != nil {
		ring = &Keyring{
			active:    keyFromSecret(cfg.JWTSecret),
			verifiers: map[string]Key{keyFromSecret(cfg.JWTSecret).ID: keyFromSecret(cfg.JWTSecret)},
		}
	}
	return &TokenService{
		keys:       ring,
		issuer:     cfg.Issuer,
		accessTTL:  cfg.AccessTokenTTL,
		refreshTTL: cfg.RefreshTokenTTL,
		clock:      clock,
		parser: jwt.NewParser(
			jwt.WithValidMethods(acceptedMethods),
			jwt.WithIssuer(cfg.Issuer),
			// A token without an expiry is a permanent credential. Require one
			// rather than trusting that every issuing path remembered to set it.
			jwt.WithExpirationRequired(),
			// Reject non-canonical base64. Two parsers disagreeing about how to
			// decode the same string is how signature checks get bypassed.
			jwt.WithStrictDecoding(),
			jwt.WithTimeFunc(clock.Now),
		),
	}
}

// AccessTTL reports how long an issued access token stays valid.
func (s *TokenService) AccessTTL() time.Duration { return s.accessTTL }

// RefreshTTL reports how long an issued refresh token stays valid.
func (s *TokenService) RefreshTTL() time.Duration { return s.refreshTTL }

// Issue mints an access token for the user under a fresh session id.
//
// A login flow normally wants IssuePair instead, so that the refresh token it
// hands out belongs to the same session as the access token.
func (s *TokenService) Issue(u *port.User, deskID *shared.ID) (string, time.Time, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return "", time.Time{}, err
	}
	return s.issue(u, deskID, TokenTypeAccess, s.accessTTL, sessionID)
}

// IssueRefresh mints a refresh token for the user under a fresh session id.
func (s *TokenService) IssueRefresh(u *port.User, deskID *shared.ID) (string, time.Time, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return "", time.Time{}, err
	}
	return s.issue(u, deskID, TokenTypeRefresh, s.refreshTTL, sessionID)
}

// IssuePair mints an access and a refresh token sharing one session id.
func (s *TokenService) IssuePair(u *port.User, deskID *shared.ID) (Pair, error) {
	sessionID, err := newSessionID()
	if err != nil {
		return Pair{}, err
	}
	access, accessExpiry, err := s.issue(u, deskID, TokenTypeAccess, s.accessTTL, sessionID)
	if err != nil {
		return Pair{}, err
	}
	refresh, refreshExpiry, err := s.issue(u, deskID, TokenTypeRefresh, s.refreshTTL, sessionID)
	if err != nil {
		return Pair{}, err
	}
	return Pair{
		AccessToken:      access,
		AccessExpiresAt:  accessExpiry,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExpiry,
		SessionID:        sessionID,
	}, nil
}

// RenewFromRefresh mints a fresh access token for a verified refresh token,
// keeping the session id so the whole login remains one thread in the audit
// trail. The caller is responsible for re-reading the user first: roles and
// the active flag may have changed since the refresh token was issued.
func (s *TokenService) RenewFromRefresh(u *port.User, deskID *shared.ID, sessionID string) (string, time.Time, error) {
	if sessionID == "" {
		return "", time.Time{}, shared.Internal("auth.token_issue_failed", nil,
			"cannot renew a token without the session it belongs to")
	}
	return s.issue(u, deskID, TokenTypeAccess, s.accessTTL, sessionID)
}

func (s *TokenService) issue(
	u *port.User,
	deskID *shared.ID,
	typ TokenType,
	ttl time.Duration,
	sessionID string,
) (string, time.Time, error) {
	if u == nil {
		return "", time.Time{}, shared.Internal("auth.token_issue_failed", nil,
			"cannot issue a token without a user")
	}
	if ttl <= 0 {
		// A zero TTL would mint a token that is already expired, which surfaces
		// later as an unexplained 401 storm rather than as the misconfiguration
		// it is.
		return "", time.Time{}, shared.Internal("auth.token_issue_failed", nil,
			"the configured %s token lifetime is not positive", typ)
	}

	now := s.clock.Now()
	expiresAt := now.Add(ttl)

	roles := make([]string, 0, len(u.Roles))
	for _, role := range u.Roles {
		roles = append(roles, string(role))
	}

	var desk *string
	if deskID != nil && !shared.IsNil(*deskID) {
		text := deskID.String()
		desk = &text
	}

	scope := u.Scope()
	var scopeMode string
	var scopeColleges, scopeDepartments []string
	if !scope.IsUniversityWide() {
		scopeMode = string(shared.ScopeLimited)
		for _, id := range scope.Colleges {
			scopeColleges = append(scopeColleges, id.String())
		}
		for _, id := range scope.Departments {
			scopeDepartments = append(scopeDepartments, id.String())
		}
	}

	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   u.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        sessionID,
		},
		Username:         u.Username,
		Roles:            roles,
		StudentID:        studentIDClaim(u),
		CashierDeskID:    desk,
		SessionID:        sessionID,
		Type:             typ,
		ScopeMode:        scopeMode,
		ScopeColleges:    scopeColleges,
		ScopeDepartments: scopeDepartments,
	}

	token := jwt.NewWithClaims(signingMethod, claims)
	// The kid names which key verifies this token, so rotation can retire a
	// secret without invalidating tokens already in browsers.
	token.Header["kid"] = s.keys.Active().ID

	signed, err := token.SignedString(s.keys.Active().Secret)
	if err != nil {
		return "", time.Time{}, shared.Internal("auth.token_issue_failed", err, "the token could not be signed")
	}
	return signed, expiresAt, nil
}

// Parse verifies an access token and returns the actor it represents.
//
// It rejects a refresh token. A refresh token lives for weeks; honouring one
// here would stretch a stolen credential's reach from a single shift to a
// month, which is precisely the exposure the two lifetimes exist to separate.
// The returned actor carries no IP address: only the transport knows that.
func (s *TokenService) Parse(token string) (*shared.Actor, error) {
	claims, err := s.parse(token, TokenTypeAccess)
	if err != nil {
		return nil, err
	}
	return actorFromClaims(claims)
}

// ParseRefresh verifies a refresh token and returns its claims. It rejects an
// access token, so the renewal endpoint cannot be driven with the short-lived
// token a browser hands to every API call.
func (s *TokenService) ParseRefresh(token string) (*Claims, error) {
	return s.parse(token, TokenTypeRefresh)
}

func (s *TokenService) parse(token string, want TokenType) (*Claims, error) {
	if strings.TrimSpace(token) == "" {
		return nil, shared.Unauthorized("auth.token_missing", "no token was supplied")
	}

	claims := &Claims{}
	if _, err := s.parser.ParseWithClaims(token, claims, s.keyFunc); err != nil {
		return nil, translateParseError(err)
	}

	if claims.Type != want {
		return nil, shared.Unauthorized("auth.token_wrong_type",
			"a %s token is required here", want).
			WithDetail("expected_type", string(want)).
			WithDetail("actual_type", string(claims.Type))
	}
	return claims, nil
}

// keyFunc supplies the verification key, after checking the algorithm a second
// time.
//
// jwt.WithValidMethods has already rejected anything but HS256 by this point.
// The check is repeated deliberately: the parser option is one line away from
// being dropped in a refactor, and the price of losing it is that anyone can
// mint a finance-manager token.
func (s *TokenService) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("%w: unexpected signing method %q", jwt.ErrTokenSignatureInvalid, token.Method.Alg())
	}

	// The kid selects the key. An unknown kid is refused rather than falling
	// back to the active key: falling back would verify a token signed with a
	// secret this deployment has deliberately retired, which is the one thing
	// retiring a key is supposed to stop.
	kid, _ := token.Header["kid"].(string)
	key, ok := s.keys.Lookup(kid)
	if !ok {
		return nil, fmt.Errorf("%w: no signing key with id %q", jwt.ErrTokenSignatureInvalid, kid)
	}
	return key.Secret, nil
}

// KeyIDs lists the signing keys this service accepts, active first. Used by the
// start-up log so an operator can confirm a rotation reached every replica.
func (s *TokenService) KeyIDs() []string { return s.keys.KeyIDs() }

// translateParseError turns a library error into a domain error with a stable
// code. The distinctions matter to a client: an expired token means "refresh
// and retry", while a bad signature means "stop and re-authenticate".
func translateParseError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return shared.Unauthorized("auth.token_expired",
			"the session has expired; sign in again").WithCause(err)
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return shared.Unauthorized("auth.token_not_yet_valid",
			"the token is not valid yet").WithCause(err)
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return shared.Unauthorized("auth.token_signature_invalid",
			"the token signature does not verify").WithCause(err)
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return shared.Unauthorized("auth.token_invalid_issuer",
			"the token was not issued by this service").WithCause(err)
	case errors.Is(err, jwt.ErrTokenMalformed), errors.Is(err, jwt.ErrTokenUnverifiable):
		return shared.Unauthorized("auth.token_malformed",
			"the token is not a readable JWT").WithCause(err)
	default:
		return shared.Unauthorized("auth.token_invalid", "the token is not valid").WithCause(err)
	}
}

func actorFromClaims(claims *Claims) (*shared.Actor, error) {
	userID, err := shared.ParseID(claims.Subject)
	if err != nil {
		return nil, shared.Unauthorized("auth.token_invalid_subject",
			"the token subject is not a valid user identifier").WithCause(err)
	}

	roles := make([]shared.Role, 0, len(claims.Roles))
	for _, name := range claims.Roles {
		role := shared.Role(name)
		// A role this build does not recognise grants nothing. Dropping it fails
		// closed: passing it through would let a role deleted from the code keep
		// authorising work through tokens issued before the deletion.
		if role.Valid() {
			roles = append(roles, role)
		}
	}

	var deskID *shared.ID
	if claims.CashierDeskID != nil {
		parsed, err := shared.ParseID(*claims.CashierDeskID)
		if err != nil {
			return nil, shared.Unauthorized("auth.token_invalid_desk",
				"the token carries an invalid cashier desk identifier").WithCause(err)
		}
		deskID = &parsed
	}

	sessionID := claims.SessionID
	if sessionID == "" {
		sessionID = claims.ID
	}

	scope, err := scopeFromClaims(claims)
	if err != nil {
		return nil, err
	}

	actor := &shared.Actor{
		UserID:        userID,
		Username:      claims.Username,
		Roles:         roles,
		CashierDeskID: deskID,
		SessionID:     sessionID,
		Scope:         scope,
	}
	if claims.StudentID != "" {
		studentID, err := shared.ParseID(claims.StudentID)
		if err != nil {
			return nil, shared.Unauthorized("auth.token_invalid_student",
				"the token carries an unreadable student identifier").WithCause(err)
		}
		actor.StudentID = &studentID
	}
	return actor, nil
}

// studentIDClaim renders the student link for a token.
func studentIDClaim(u *port.User) string {
	if u.StudentID == nil {
		return ""
	}
	return u.StudentID.String()
}

// scopeFromClaims rebuilds the organisational reach carried in a token.
//
// An unparseable identifier is an error rather than a value to drop. Dropping
// it would silently widen the actor — a scoped finance manager whose one
// college failed to parse would become a scoped actor with no restrictions
// left to apply — and widening on malformed input is the wrong direction for
// an authorisation decision to fail.
func scopeFromClaims(claims *Claims) (shared.Scope, error) {
	if shared.ScopeMode(claims.ScopeMode) != shared.ScopeLimited {
		return shared.UniversityScope(), nil
	}

	scope := shared.Scope{Mode: shared.ScopeLimited}
	for _, raw := range claims.ScopeColleges {
		id, err := shared.ParseID(raw)
		if err != nil {
			return shared.Scope{}, shared.Unauthorized("auth.token_invalid_scope",
				"the token carries an unreadable college scope").WithCause(err)
		}
		scope.Colleges = append(scope.Colleges, id)
	}
	for _, raw := range claims.ScopeDepartments {
		id, err := shared.ParseID(raw)
		if err != nil {
			return shared.Scope{}, shared.Unauthorized("auth.token_invalid_scope",
				"the token carries an unreadable department scope").WithCause(err)
		}
		scope.Departments = append(scope.Departments, id)
	}
	return scope, nil
}

// newSessionID mints the identifier that ties every action taken during one
// login together in the audit trail.
//
// It is random rather than derived from the user or the clock: an id that can
// be guessed or that repeats across replicas would let two logins collapse
// into one thread when someone later reconstructs what a cashier did.
//
// A UUID rather than raw hex, because the session is now also a row — revoking
// it is what makes logout and "sign out my other sessions" work — and the row's
// primary key is a uuid.
func newSessionID() (string, error) {
	return shared.NewID().String(), nil
}
