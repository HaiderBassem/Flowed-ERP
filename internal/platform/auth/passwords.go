// Package auth holds the credential primitives the rest of the system is built
// on: password hashing and the issuing and verification of JSON Web Tokens.
//
// It deliberately stops short of a login flow. Whether a user is active,
// whether the login is recorded, and what the response looks like are
// application concerns. This package answers only two questions — "does this
// password match this hash" and "which actor does this token represent" — so
// that each has exactly one implementation and one place to audit.
package auth

import (
	"errors"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/config"
)

const (
	// MinPasswordRunes is the shortest password the system will store.
	//
	// The count is in characters rather than bytes because an Arabic password
	// costs two bytes per letter: a byte-based minimum would silently accept a
	// five-letter Arabic password while rejecting a nine-letter Latin one.
	MinPasswordRunes = 10

	// MaxPasswordBytes is the longest password bcrypt actually hashes.
	//
	// bcrypt ignores everything past the 72nd byte without complaining. A user
	// who sets a 90-character passphrase would therefore be protected by its
	// first 72 bytes only, and two long passphrases sharing a 72-byte prefix
	// would both open the same account. Refusing the input is the only honest
	// answer; truncating it quietly is a real security hole.
	MaxPasswordBytes = 72
)

// decoyPassword is hashed once per Hasher to give VerifyDummy something to
// compare against. Nothing needs to match it, so its value is irrelevant.
const decoyPassword = "timing-parity-decoy"

// Hasher hashes and verifies passwords at the configured bcrypt cost.
type Hasher struct {
	cost int

	// The decoy digest is derived lazily so that constructing a Hasher — which
	// happens during start-up wiring — does not pay for a bcrypt round that
	// most processes never need.
	decoyOnce sync.Once
	decoyHash []byte
}

// NewHasher builds a hasher from configuration.
func NewHasher(cfg config.Auth) *Hasher {
	return &Hasher{cost: normaliseCost(cfg.BcryptCost)}
}

// Cost reports the bcrypt cost this hasher applies to new passwords.
func (h *Hasher) Cost() int { return h.cost }

// Hash validates the password and returns its bcrypt digest.
func (h *Hasher) Hash(plain string) (string, error) {
	if err := ValidatePassword(plain); err != nil {
		return "", err
	}
	digest, err := bcrypt.GenerateFromPassword([]byte(plain), h.cost)
	if err != nil {
		return "", shared.Internal("auth.password_hash_failed", err, "the password could not be hashed")
	}
	return string(digest), nil
}

// Verify checks a plaintext password against a stored digest.
//
// The bcrypt comparison runs unconditionally, even on input Hash would have
// rejected outright: returning early for an empty or over-long guess would
// make those guesses measurably faster than plausible ones, which is a timing
// oracle over the shape of the password. bcrypt's own comparison is constant
// time for a given cost, so the work below is uniform.
func (h *Hasher) Verify(hash, plain string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return shared.Unauthorized("auth.invalid_credentials",
			"the username or password is incorrect").WithCause(err)
	default:
		// A digest bcrypt cannot even parse is a corrupted or half-migrated user
		// row, not a bad guess. Reporting it as a failed login would bury a data
		// problem under what looks like a user's typo.
		return shared.Internal("auth.password_hash_unreadable", err,
			"the stored password hash could not be read")
	}
}

// VerifyDummy burns one bcrypt comparison and returns the same error a real
// mismatch produces.
//
// A login attempt against a username that does not exist has no digest to
// compare with, so it would answer in microseconds where a real attempt takes
// tens of milliseconds. That difference is enough to enumerate every valid
// username in the system. Callers that find no user call this and return its
// error instead of answering early.
func (h *Hasher) VerifyDummy() error {
	h.decoyOnce.Do(func() {
		digest, err := bcrypt.GenerateFromPassword([]byte(decoyPassword), h.cost)
		if err != nil {
			// The cost is normalised in the constructor, so this is unreachable.
			// Leaving the digest empty costs the caller the timing parity but
			// never the correct answer, which the fallback below still gives.
			return
		}
		h.decoyHash = digest
	})

	if len(h.decoyHash) > 0 {
		if err := h.Verify(string(h.decoyHash), decoyPassword+"-mismatch"); err != nil &&
			shared.KindOf(err) == shared.KindUnauthorized {
			return err
		}
	}
	return shared.Unauthorized("auth.invalid_credentials", "the username or password is incorrect")
}

// ValidatePassword reports whether a password may be stored at all. Callers
// that only want to check a candidate — a password-change form, a user import
// — use this without paying for a hash.
func ValidatePassword(plain string) error {
	if plain == "" {
		return shared.Validation("auth.password_required", "a password is required")
	}
	if len(plain) > MaxPasswordBytes {
		return shared.Validation("auth.password_too_long",
			"a password may be at most %d bytes long (this one is %d); bcrypt ignores everything beyond that, so a longer password would only appear to protect the account",
			MaxPasswordBytes, len(plain)).
			WithDetail("max_bytes", MaxPasswordBytes).
			WithDetail("actual_bytes", len(plain))
	}
	if utf8.RuneCountInString(plain) < MinPasswordRunes {
		return shared.Validation("auth.password_too_short",
			"a password must be at least %d characters long", MinPasswordRunes).
			WithDetail("min_characters", MinPasswordRunes)
	}
	return nil
}

// normaliseCost keeps an out-of-range cost from weakening every password the
// process writes. Configuration already constrains it to 10..15, but a
// zero-valued config.Auth — a half-wired binary, a test fixture — would
// otherwise hash at bcrypt's floor without anyone noticing.
func normaliseCost(cost int) int {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return bcrypt.DefaultCost
	}
	return cost
}
