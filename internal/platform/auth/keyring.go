// Package auth issues and verifies the credentials that carry an operator's
// identity across the stateless HTTP boundary.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"flowed/internal/domain/shared"
)

// Keyring holds the signing keys this service will accept.
//
// Rotation without it is a choice between two bad outcomes: replace the secret
// and every operator is signed out mid-shift — at a cashier desk that means a
// queue and a part-finished collection — or never rotate, which is how a secret
// pasted into a chat window in 2026 is still signing tokens in 2031.
//
// A keyring makes rotation a two-step operation instead. The new key is added
// and becomes active, so new tokens are signed with it; the previous key stays
// listed and still verifies, so tokens already in browsers keep working until
// they expire. One refresh-token lifetime later the old key is dropped.
//
// Every issued token carries a "kid" header naming its key, so verification is
// a lookup rather than a trial of every key in turn. Trying each in turn would
// also work, but it turns an invalid signature into N hash computations, which
// is a free amplifier for anyone posting garbage tokens at the login endpoint.
type Keyring struct {
	// active is the key new tokens are signed with.
	active Key
	// verifiers includes the active key and every retired key still inside its
	// grace window, indexed by kid.
	verifiers map[string]Key
}

// Key is one signing secret and the identifier that names it.
type Key struct {
	// ID is the "kid" header value. Derived from the secret rather than chosen
	// by an operator: a kid that is a name ("prod-2026") invites reusing the
	// name for a different secret, and then two keys with one identifier are
	// indistinguishable in a log.
	ID string
	// Secret is the HMAC key.
	Secret []byte
}

// NewKeyring builds a keyring from an active secret and any number of retired
// secrets that must still verify.
//
// The retired list is ordinary configuration, so a rotation is a configuration
// change and a restart rather than a code change.
func NewKeyring(active string, retired []string) (*Keyring, error) {
	if strings.TrimSpace(active) == "" {
		return nil, shared.Internal("auth.keyring_empty", nil,
			"no active signing secret is configured")
	}

	activeKey := keyFromSecret(active)
	ring := &Keyring{
		active:    activeKey,
		verifiers: map[string]Key{activeKey.ID: activeKey},
	}
	for _, secret := range retired {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		key := keyFromSecret(secret)
		if key.ID == activeKey.ID {
			// The same secret listed twice is a copy-paste in configuration, not
			// an error worth refusing to start over.
			continue
		}
		ring.verifiers[key.ID] = key
	}
	return ring, nil
}

// keyFromSecret derives a stable, non-secret identifier for a signing key.
//
// The kid is a truncated SHA-256 of the secret. It is not reversible, it is
// identical on every replica without coordination, and it changes exactly when
// the secret changes — which is the property that makes "two replicas disagree
// about which key is active" visible in a log line rather than as intermittent
// 401s.
func keyFromSecret(secret string) Key {
	sum := sha256.Sum256([]byte(secret))
	return Key{ID: hex.EncodeToString(sum[:8]), Secret: []byte(secret)}
}

// Active returns the key new tokens are signed with.
func (k *Keyring) Active() Key { return k.active }

// Lookup returns the key with the given kid.
//
// A token with no kid is verified with the active key: tokens issued before
// this package learned to stamp one are still in browsers, and refusing them
// would sign everybody out at deploy time — the exact event rotation support
// exists to avoid.
func (k *Keyring) Lookup(kid string) (Key, bool) {
	if kid == "" {
		return k.active, true
	}
	key, ok := k.verifiers[kid]
	return key, ok
}

// KeyIDs lists every accepted kid, active first, for the start-up log and the
// health endpoint. Secrets never leave this package.
func (k *Keyring) KeyIDs() []string {
	ids := make([]string, 0, len(k.verifiers))
	for id := range k.verifiers {
		if id == k.active.ID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return append([]string{k.active.ID}, ids...)
}

// Describe renders the ring for a log line: the active kid and how many
// retired keys still verify.
func (k *Keyring) Describe() string {
	return fmt.Sprintf("active=%s accepted=%d", k.active.ID, len(k.verifiers))
}
