package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/port"
)

const (
	activeSecret  = "an-active-signing-secret-of-sufficient-length"
	retiredSecret = "the-previous-signing-secret-of-sufficient-length"
	otherSecret   = "a-secret-this-deployment-never-issued-tokens-with"
)

func rotationUser() *port.User {
	return &port.User{
		ID:       shared.NewID(),
		Username: "finance.one",
		Roles:    []shared.Role{shared.RoleFinanceManager},
		IsActive: true,
	}
}

func serviceWith(active string, retired []string) *auth.TokenService {
	cfg := testAuthConfig()
	cfg.JWTSecret = active
	cfg.RetiredJWTSecrets = retired
	return auth.NewTokenService(cfg)
}

// The whole point of the keyring: a token minted before the rotation keeps
// working afterwards. Without it, rotating the secret signs every cashier out
// mid-shift, which is why in practice the secret never got rotated at all.
func TestTokenSignedWithRetiredKeyStillVerifies(t *testing.T) {
	before := serviceWith(retiredSecret, nil)
	token, _, err := before.Issue(rotationUser(), nil)
	if err != nil {
		t.Fatalf("issuing before rotation: %v", err)
	}

	after := serviceWith(activeSecret, []string{retiredSecret})
	actor, err := after.Parse(token)
	if err != nil {
		t.Fatalf("a token signed with the retired key must still verify: %v", err)
	}
	if actor.Username != "finance.one" {
		t.Errorf("username = %q", actor.Username)
	}
}

// The other half: once a key is dropped from the ring, tokens it signed stop
// working. A ring that kept verifying every secret it had ever seen would make
// retiring a leaked key impossible.
func TestTokenSignedWithDroppedKeyIsRefused(t *testing.T) {
	before := serviceWith(otherSecret, nil)
	token, _, err := before.Issue(rotationUser(), nil)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	after := serviceWith(activeSecret, []string{retiredSecret})
	if _, err := after.Parse(token); err == nil {
		t.Fatal("a token signed with a key no longer in the ring must be refused")
	} else if code := shared.CodeOf(err); code != "auth.token_signature_invalid" {
		t.Errorf("code = %q, want auth.token_signature_invalid", code)
	}
}

// New tokens must be signed with the active key, not with whichever key
// happens to be first in a map.
func TestNewTokensAreSignedWithTheActiveKey(t *testing.T) {
	svc := serviceWith(activeSecret, []string{retiredSecret})
	token, _, err := svc.Issue(rotationUser(), nil)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	kid := kidOf(t, token)
	if kid == "" {
		t.Fatal("every issued token must carry a kid header")
	}
	if want := svc.KeyIDs()[0]; kid != want {
		t.Errorf("kid = %q, want the active key %q", kid, want)
	}

	// A service holding only the retired key must refuse it, which proves the
	// token really was signed with the active one.
	onlyRetired := serviceWith(retiredSecret, nil)
	if _, err := onlyRetired.Parse(token); err == nil {
		t.Error("a token signed with the active key must not verify under the retired key alone")
	}
}

// A kid naming a key this deployment does not hold must be refused outright.
// Falling back to the active key would verify tokens signed with a secret that
// was deliberately retired — the one thing retiring a key must stop.
func TestUnknownKeyIDIsRefused(t *testing.T) {
	ring, err := auth.NewKeyring(activeSecret, nil)
	if err != nil {
		t.Fatalf("building keyring: %v", err)
	}
	if _, ok := ring.Lookup("0000000000000000"); ok {
		t.Fatal("an unknown kid must not resolve to a key")
	}
	// A token with no kid at all resolves to the active key, so credentials
	// issued before this package stamped one keep working across the deploy
	// that introduced it.
	if _, ok := ring.Lookup(""); !ok {
		t.Fatal("a token with no kid must fall back to the active key")
	}
}

func TestKeyringRejectsAnEmptyActiveSecret(t *testing.T) {
	if _, err := auth.NewKeyring("   ", []string{retiredSecret}); err == nil {
		t.Fatal("a keyring with no active secret must be refused")
	}
}

func TestKeyIDsAreDerivedAndStable(t *testing.T) {
	first, err := auth.NewKeyring(activeSecret, []string{retiredSecret})
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.NewKeyring(activeSecret, []string{retiredSecret})
	if err != nil {
		t.Fatal(err)
	}

	// Derived from the secret, so every replica computes the same identifier
	// without coordinating, and the identifier changes exactly when the secret
	// does.
	if first.Active().ID != second.Active().ID {
		t.Error("the same secret must produce the same key id on every replica")
	}
	if len(first.KeyIDs()) != 2 {
		t.Errorf("KeyIDs() = %v, want the active key and one retired", first.KeyIDs())
	}
	if strings.Contains(first.Describe(), activeSecret) {
		t.Fatal("the secret must never appear in a description meant for logs")
	}

	// Listing the same secret as both active and retired is a configuration
	// copy-paste, not a reason to refuse to start.
	duplicated, err := auth.NewKeyring(activeSecret, []string{activeSecret})
	if err != nil {
		t.Fatal(err)
	}
	if len(duplicated.KeyIDs()) != 1 {
		t.Errorf("a secret listed twice must collapse to one key, got %v", duplicated.KeyIDs())
	}
}

// Scope travels in the token, so a scoped operator's reach survives the
// stateless boundary without a database read on every request.
func TestScopeSurvivesTheTokenRoundTrip(t *testing.T) {
	college, department := shared.NewID(), shared.NewID()
	user := rotationUser()
	user.ScopeMode = shared.ScopeLimited
	user.Colleges = []shared.ID{college}
	user.Departments = []shared.ID{department}

	svc := serviceWith(activeSecret, nil)
	token, _, err := svc.Issue(user, nil)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	actor, err := svc.Parse(token)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if actor.Scope.IsUniversityWide() {
		t.Fatal("a scoped user's token must not produce a university-wide actor")
	}
	if !actor.Scope.CoversCollege(college) {
		t.Error("the granted college did not survive the round trip")
	}
	if actor.Scope.CoversCollege(shared.NewID()) {
		t.Error("an ungranted college must not be covered")
	}
	if err := actor.RequireScope("op", nil, &department); err != nil {
		t.Errorf("the granted department did not survive: %v", err)
	}
}

func TestUniversityScopeKeepsTheTokenSmall(t *testing.T) {
	svc := serviceWith(activeSecret, nil)
	token, _, err := svc.Issue(rotationUser(), nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := payloadOf(t, token)
	for _, key := range []string{"scope_mode", "scope_colleges", "scope_departments"} {
		if _, present := payload[key]; present {
			t.Errorf("a university-wide actor should carry no %q claim", key)
		}
	}

	actor, err := svc.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if !actor.Scope.IsUniversityWide() {
		t.Fatal("a token with no scope claim must produce a university-wide actor")
	}
}

func TestPasswordPolicyRefusesTheAccountsOwnDetails(t *testing.T) {
	cases := map[string]struct {
		password string
		username string
		fullName string
		wantCode string
	}{
		"contains username": {"hala.cashier-2026", "hala.cashier", "Hala Kareem", "auth.password_contains_username"},
		"contains name":     {"kareem-is-here-now", "hala.c", "Hala Kareem", "auth.password_contains_name"},
		"common":            {"password123", "hala.c", "Hala Kareem", "auth.password_too_common"},
		"one character":     {"aaaaaaaaaaaa", "hala.c", "Hala Kareem", "auth.password_too_simple"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := auth.ValidatePasswordFor(tc.password, tc.username, tc.fullName)
			if err == nil {
				t.Fatal("expected the password to be refused")
			}
			if code := shared.CodeOf(err); code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if kind := shared.KindOf(err); kind != shared.KindValidation {
				t.Errorf("kind = %v, want validation", kind)
			}
		})
	}

	// A phrase unrelated to the account passes. Composition rules — a digit, a
	// capital, a symbol — are deliberately not imposed.
	if err := auth.ValidatePasswordFor("marsh warbler thicket", "hala.c", "Hala Kareem"); err != nil {
		t.Errorf("an unrelated passphrase was refused: %v", err)
	}
}

// kidOf extracts the kid header without verifying anything.
func kidOf(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected three token segments, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding header: %v", err)
	}
	var header struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatalf("parsing header: %v", err)
	}
	return header.Kid
}

// payloadOf decodes the claim set without verifying anything.
func payloadOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected three token segments, got %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}
	return claims
}

var _ = config.Auth{}
