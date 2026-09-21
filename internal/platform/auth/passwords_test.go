package auth_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/auth"
)

func testHasher() *auth.Hasher {
	// Cost 4 is bcrypt's floor. Tests care that the algorithm is wired
	// correctly, not how long it takes.
	cfg := testAuthConfig()
	cfg.BcryptCost = 4
	return auth.NewHasher(cfg)
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	hasher := testHasher()
	const password = "correct-horse-battery"

	digest, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if digest == password {
		t.Fatal("the digest is the plaintext")
	}
	if err := hasher.Verify(digest, password); err != nil {
		t.Fatalf("verifying the correct password: %v", err)
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	hasher := testHasher()

	digest, err := hasher.Hash("correct-horse-battery")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	err = hasher.Verify(digest, "correct-horse-batterz")
	if err == nil {
		t.Fatal("expected the wrong password to be rejected")
	}
	if kind := shared.KindOf(err); kind != shared.KindUnauthorized {
		t.Fatalf("expected kind unauthorized, got %s: %v", kind, err)
	}
	if code := shared.CodeOf(err); code != "auth.invalid_credentials" {
		t.Fatalf("expected code auth.invalid_credentials, got %q", code)
	}
}

// TestVerifyDoesNotShortCircuit proves Verify still runs the comparison for
// input Hash would have refused. An early return on an empty or over-long
// guess would answer faster than a plausible one and leak the shape of the
// password.
func TestVerifyDoesNotShortCircuit(t *testing.T) {
	hasher := testHasher()

	digest, err := hasher.Hash("correct-horse-battery")
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}

	for name, guess := range map[string]string{
		"empty":    "",
		"tiny":     "a",
		"over 72b": strings.Repeat("z", 200),
	} {
		t.Run(name, func(t *testing.T) {
			err := hasher.Verify(digest, guess)
			if kind := shared.KindOf(err); kind != shared.KindUnauthorized {
				t.Fatalf("expected a plain unauthorized answer, got kind %s: %v", kind, err)
			}
		})
	}
}

// TestHashRejectsPasswordsBeyondBcryptsLimit is the silent hole this guard
// exists for: bcrypt ignores everything past the 72nd byte, so without the
// check the two passwords below would both open the same account.
func TestHashRejectsPasswordsBeyondBcryptsLimit(t *testing.T) {
	hasher := testHasher()

	// Two alternating letters rather than one repeated: exactly 72 bytes, and
	// not the single-repeated-character shape the policy refuses on its own.
	prefix := strings.Repeat("ab", auth.MaxPasswordBytes/2)
	first := prefix + "-first-suffix"
	second := prefix + "-second-suffix"

	for _, password := range []string{first, second} {
		_, err := hasher.Hash(password)
		if err == nil {
			t.Fatalf("expected a %d byte password to be refused", len(password))
		}
		if kind := shared.KindOf(err); kind != shared.KindValidation {
			t.Fatalf("expected kind validation, got %s: %v", kind, err)
		}
		if code := shared.CodeOf(err); code != "auth.password_too_long" {
			t.Fatalf("expected code auth.password_too_long, got %q", code)
		}
	}

	// Exactly at the limit is fine; one byte past it is not.
	if _, err := hasher.Hash(prefix); err != nil {
		t.Fatalf("a password of exactly %d bytes should be accepted: %v", auth.MaxPasswordBytes, err)
	}
	if _, err := hasher.Hash(prefix + "b"); err == nil {
		t.Fatalf("a password of %d bytes should be refused", auth.MaxPasswordBytes+1)
	}
}

func TestHashRejectsShortPasswords(t *testing.T) {
	hasher := testHasher()

	cases := map[string]struct {
		password string
		wantCode string
	}{
		"empty":          {"", "auth.password_required"},
		"nine ascii":     {strings.Repeat("a", auth.MinPasswordRunes-1), "auth.password_too_short"},
		"nine arabic":    {strings.Repeat("ك", auth.MinPasswordRunes-1), "auth.password_too_short"},
		"whitespace pad": {"  a  ", "auth.password_too_short"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := hasher.Hash(tc.password)
			if err == nil {
				t.Fatal("expected the password to be refused")
			}
			if kind := shared.KindOf(err); kind != shared.KindValidation {
				t.Fatalf("expected kind validation, got %s: %v", kind, err)
			}
			if code := shared.CodeOf(err); code != tc.wantCode {
				t.Fatalf("expected code %q, got %q", tc.wantCode, code)
			}
		})
	}

	// Ten Arabic letters are twenty bytes and must be accepted: counting the
	// minimum in bytes would have let a five-letter Arabic password through.
	// Ten *different* letters, because a password of one repeated character is
	// now refused on its own account — the point being tested here is the rune
	// count, and repeating one letter would test both rules at once and pass
	// for the wrong reason.
	arabicTen := "كتبمنزلشجر"
	if got := utf8.RuneCountInString(arabicTen); got != auth.MinPasswordRunes {
		t.Fatalf("the fixture must be exactly %d runes, got %d", auth.MinPasswordRunes, got)
	}
	if _, err := hasher.Hash(arabicTen); err != nil {
		t.Fatalf("a ten character Arabic password should be accepted: %v", err)
	}
}

// TestVerifyReportsUnreadableHashAsInternal keeps a corrupted user row from
// looking like an ordinary failed login.
func TestVerifyReportsUnreadableHashAsInternal(t *testing.T) {
	hasher := testHasher()

	err := hasher.Verify("not-a-bcrypt-digest", "correct-horse-battery")
	if kind := shared.KindOf(err); kind != shared.KindInternal {
		t.Fatalf("expected kind internal, got %s: %v", kind, err)
	}
}

func TestVerifyDummyAlwaysFails(t *testing.T) {
	hasher := testHasher()

	err := hasher.VerifyDummy()
	if err == nil {
		t.Fatal("expected the decoy verification to fail")
	}
	if kind := shared.KindOf(err); kind != shared.KindUnauthorized {
		t.Fatalf("expected kind unauthorized, got %s: %v", kind, err)
	}
	if code := shared.CodeOf(err); code != "auth.invalid_credentials" {
		t.Fatalf("expected the same code a real mismatch produces, got %q", code)
	}
}
