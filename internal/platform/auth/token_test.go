package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
	"flowed/internal/platform/config"
	"flowed/internal/port"
)

const (
	testSecret = "test-secret-that-is-at-least-thirty-two-bytes"
	testIssuer = "flowed-test"
)

func testAuthConfig() config.Auth {
	return config.Auth{
		JWTSecret:       testSecret,
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: 24 * time.Hour,
		Issuer:          testIssuer,
		BcryptCost:      4,
	}
}

func testUser() *port.User {
	return &port.User{
		ID:       shared.NewID(),
		Username: "cashier.ali",
		Roles:    []shared.Role{shared.RoleAdmin},
		IsActive: true,
	}
}

// forgedClaims are what an attacker would put in a token they were able to
// mint: the two roles that between them can grant a discount, approve a void
// and close a year.
func forgedClaims(typ auth.TokenType) *auth.Claims {
	now := time.Now()
	return &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   shared.NewID().String(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			ID:        "forged-session",
		},
		Username:  "attacker",
		Roles:     []string{string(shared.RoleAdmin), string(shared.RoleAdmin)},
		SessionID: "forged-session",
		Type:      typ,
	}
}

func requireUnauthorized(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the token to be rejected, but parsing succeeded")
	}
	domainErr, ok := shared.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if domainErr.Kind != shared.KindUnauthorized {
		t.Fatalf("expected kind unauthorized, got %s: %v", domainErr.Kind, err)
	}
	if wantCode != "" && domainErr.Code != wantCode {
		t.Fatalf("expected code %q, got %q: %v", wantCode, domainErr.Code, err)
	}
}

// TestParseRejectsAlgNone covers the oldest JWT forgery there is: a token
// whose header claims no signature was needed. A parser that trusts the
// header's own "alg" accepts it and hands the attacker whatever roles the
// payload asks for.
func TestParseRejectsAlgNone(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, forgedClaims(auth.TokenTypeAccess)).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("building the unsigned token: %v", err)
	}

	_, err = svc.Parse(unsigned)
	requireUnauthorized(t, err, "auth.token_signature_invalid")
}

// TestParseRejectsRS256Token covers the algorithm-confusion forgery: an
// asymmetric token offered to a service that verifies with an HMAC secret. A
// parser that picks its algorithm from the token would try to verify this one
// with the secret as if it were a public key.
func TestParseRejectsRS256Token(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating an RSA key: %v", err)
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, forgedClaims(auth.TokenTypeAccess)).
		SignedString(key)
	if err != nil {
		t.Fatalf("signing the RS256 token: %v", err)
	}

	_, err = svc.Parse(signed)
	requireUnauthorized(t, err, "auth.token_signature_invalid")
}

// TestParseRejectsOtherHMACVariants is the direct test of the signing-method
// allowlist.
//
// Unlike the two cases above — which the library also refuses because a []byte
// is not a valid key for RS256 or for "none" — an HS384 token signed with our
// own secret verifies perfectly well against a []byte key. Only the explicit
// allowlist stops it. That makes this the case that actually fails if
// jwt.WithValidMethods is ever dropped, so it is the guard's regression test.
func TestParseRejectsOtherHMACVariants(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	for _, method := range []*jwt.SigningMethodHMAC{jwt.SigningMethodHS384, jwt.SigningMethodHS512} {
		t.Run(method.Alg(), func(t *testing.T) {
			signed, err := jwt.NewWithClaims(method, forgedClaims(auth.TokenTypeAccess)).
				SignedString([]byte(testSecret))
			if err != nil {
				t.Fatalf("signing with %s: %v", method.Alg(), err)
			}

			_, err = svc.Parse(signed)
			requireUnauthorized(t, err, "auth.token_signature_invalid")
		})
	}
}

// TestParseRejectsForeignSecret proves the signature is actually verified
// against our key rather than merely being well formed.
func TestParseRejectsForeignSecret(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, forgedClaims(auth.TokenTypeAccess)).
		SignedString([]byte("a-different-secret-of-a-respectable-length"))
	if err != nil {
		t.Fatalf("signing with the foreign secret: %v", err)
	}

	_, err = svc.Parse(signed)
	requireUnauthorized(t, err, "auth.token_signature_invalid")
}

// TestParseRejectsTamperedClaims proves the roles in a real token cannot be
// edited after issuance.
func TestParseRejectsTamperedClaims(t *testing.T) {
	cfg := testAuthConfig()
	svc := auth.NewTokenService(cfg)

	token, _, err := svc.Issue(testUser())
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected three token segments, got %d", len(parts))
	}
	// Re-sign the forged payload with a foreign key but keep our header: the
	// header still says HS256, so only the signature check can catch this.
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, forgedClaims(auth.TokenTypeAccess)).
		SignedString([]byte(testSecret + "x"))
	if err != nil {
		t.Fatalf("signing the forgery: %v", err)
	}
	forgedParts := strings.Split(forged, ".")
	spliced := parts[0] + "." + forgedParts[1] + "." + parts[2]

	_, err = svc.Parse(spliced)
	requireUnauthorized(t, err, "auth.token_signature_invalid")
}

// TestParseRejectsExpiredToken proves an old token stops working. Without it a
// stolen token would be a permanent credential.
func TestParseRejectsExpiredToken(t *testing.T) {
	cfg := testAuthConfig()

	// Issue as though it were two hours ago, against a one-hour access lifetime.
	past := shared.FixedClock{Instant: time.Now().UTC().Add(-2 * time.Hour)}
	issuer := auth.NewTokenServiceWithClock(cfg, past)

	token, expiresAt, err := issuer.Issue(testUser())
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}
	if !expiresAt.Before(time.Now()) {
		t.Fatalf("expected the token to have expired already, expiry is %s", expiresAt)
	}

	_, err = auth.NewTokenService(cfg).Parse(token)
	requireUnauthorized(t, err, "auth.token_expired")
}

// TestParseRejectsRefreshToken is the important half of the type separation. A
// refresh token lives thirty days; honouring one as an access token would turn
// a shift-long exposure into a month-long one.
func TestParseRejectsRefreshToken(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	refresh, _, err := svc.IssueRefresh(testUser())
	if err != nil {
		t.Fatalf("issuing the refresh token: %v", err)
	}

	_, err = svc.Parse(refresh)
	requireUnauthorized(t, err, "auth.token_wrong_type")
}

// TestParseRefreshRejectsAccessToken is the other half: the renewal endpoint
// cannot be driven with the token every API call already carries.
func TestParseRefreshRejectsAccessToken(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	access, _, err := svc.Issue(testUser())
	if err != nil {
		t.Fatalf("issuing the access token: %v", err)
	}

	_, err = svc.ParseRefresh(access)
	requireUnauthorized(t, err, "auth.token_wrong_type")
}

// TestParseRejectsForeignIssuer proves a validly signed token from another
// service sharing the secret — a staging deployment, say — is still refused.
func TestParseRejectsForeignIssuer(t *testing.T) {
	cfg := testAuthConfig()
	other := cfg
	other.Issuer = "somebody-else"

	token, _, err := auth.NewTokenService(other).Issue(testUser())
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	_, err = auth.NewTokenService(cfg).Parse(token)
	requireUnauthorized(t, err, "auth.token_invalid_issuer")
}

func TestParseRejectsGarbage(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	for name, token := range map[string]string{
		"empty":         "",
		"whitespace":    "   ",
		"not a jwt":     "this-is-not-a-token",
		"two segments":  "aGVhZGVy.cGF5bG9hZA",
		"empty payload": "..",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Parse(token)
			requireUnauthorized(t, err, "")
		})
	}
}

func TestIssueAndParseRoundTrip(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	user := testUser()
	user.Roles = []shared.Role{shared.RoleAdmin}

	token, expiresAt, err := svc.Issue(user)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}
	if time.Until(expiresAt) <= 0 {
		t.Fatalf("expected a future expiry, got %s", expiresAt)
	}

	actor, err := svc.Parse(token)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if actor.UserID != user.ID {
		t.Errorf("user id: got %s, want %s", actor.UserID, user.ID)
	}
	if actor.Username != user.Username {
		t.Errorf("username: got %q, want %q", actor.Username, user.Username)
	}
	if !actor.HasRole(shared.RoleAdmin) {
		t.Errorf("roles: got %v, want admin", actor.Roles)
	}
	if actor.SessionID == "" {
		t.Error("expected a session id on the actor")
	}
	if actor.IPAddress != "" {
		t.Errorf("expected no IP address from the token, got %q", actor.IPAddress)
	}
}

// TestIssuePairSharesOneSession keeps the audit trail able to see a renewal
// and the login it came from as one thread.
func TestIssuePairSharesOneSession(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	pair, err := svc.IssuePair(testUser())
	if err != nil {
		t.Fatalf("issuing the pair: %v", err)
	}

	access, err := svc.Parse(pair.AccessToken)
	if err != nil {
		t.Fatalf("parsing the access token: %v", err)
	}
	refresh, err := svc.ParseRefresh(pair.RefreshToken)
	if err != nil {
		t.Fatalf("parsing the refresh token: %v", err)
	}
	if access.SessionID != refresh.SessionID {
		t.Errorf("session ids differ: access %q, refresh %q", access.SessionID, refresh.SessionID)
	}
	if access.SessionID != pair.SessionID {
		t.Errorf("pair session id %q does not match the token's %q", pair.SessionID, access.SessionID)
	}
	if !pair.RefreshExpiresAt.After(pair.AccessExpiresAt) {
		t.Errorf("expected the refresh token to outlive the access token: %s vs %s",
			pair.RefreshExpiresAt, pair.AccessExpiresAt)
	}
}

// TestSessionIDsAreUnique guards the audit trail's ability to separate two
// logins by the same user.
func TestSessionIDsAreUnique(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())
	user := testUser()

	seen := make(map[string]struct{}, 32)
	for range 32 {
		token, _, err := svc.Issue(user)
		if err != nil {
			t.Fatalf("issuing: %v", err)
		}
		actor, err := svc.Parse(token)
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		if _, duplicate := seen[actor.SessionID]; duplicate {
			t.Fatalf("session id %q was issued twice", actor.SessionID)
		}
		seen[actor.SessionID] = struct{}{}
	}
}

// TestUnknownRolesAreDropped proves an unrecognised role grants nothing, so
// removing a role from the code cannot be undone by an old token.
func TestUnknownRolesAreDropped(t *testing.T) {
	svc := auth.NewTokenService(testAuthConfig())

	claims := forgedClaims(auth.TokenTypeAccess)
	claims.Roles = []string{"superuser", string(shared.RoleAdmin), "root"}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	actor, err := svc.Parse(signed)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(actor.Roles) != 1 || actor.Roles[0] != shared.RoleAdmin {
		t.Errorf("expected only the cashier role to survive, got %v", actor.Roles)
	}
}
