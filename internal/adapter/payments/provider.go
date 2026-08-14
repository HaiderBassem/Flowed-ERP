// Package payments holds the adapters for the electronic collection channels
// an Iraqi university actually uses: Qi Card, ZainCash, FastPay, and collection
// at a bank branch.
//
// The financial core knows nothing about any of them. A provider's job is to
// take a request for an amount against an account, hand it to whatever the
// provider's protocol is, and turn the eventual confirmation back into one
// vocabulary: succeeded, failed, or still pending. Everything after that — the
// receipt number, the allocation, the year lock, the idempotency key — is the
// ordinary payment path, because there is nothing financially special about
// where money came from.
//
// Two properties are non-negotiable and are enforced here rather than left to
// each integration:
//
//   - The provider's confirmation is authoritative. A client saying "the app
//     said it worked" posts nothing. Every provider below either verifies a
//     signed callback or asks the provider directly.
//   - A confirmation delivered twice posts one payment. Providers retry, and
//     the honest ones retry a lot.
package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
)

// Provider is one electronic collection channel.
type Provider interface {
	// Code identifies the provider in configuration, in the intent row and in
	// the callback URL: QI, ZAINCASH, FASTPAY, BRANCH.
	Code() string
	// DisplayName is what a student sees offered.
	DisplayName() string
	// Initiate hands a request to the provider and returns what the student
	// needs in order to pay: a redirect, a deep link, or a reference to quote
	// at a counter.
	Initiate(ctx context.Context, req InitiateRequest) (*InitiateResponse, error)
	// ParseCallback turns a delivered callback into this domain's vocabulary,
	// verifying its authenticity first. A callback that does not verify is an
	// error, never a payment.
	ParseCallback(ctx context.Context, raw CallbackEnvelope) (*payment.CallbackResult, error)
	// FetchStatus asks the provider directly. Used to settle an intent whose
	// callback never arrived, which is the ordinary outcome of a student
	// closing the browser at the wrong moment.
	FetchStatus(ctx context.Context, providerRef string) (*payment.CallbackResult, error)
}

// InitiateRequest is a collection about to be handed to a provider.
type InitiateRequest struct {
	IntentID shared.ID
	// ClientRef is our reference for the transaction, echoed back by the
	// provider. It is the intent's identifier.
	ClientRef string
	Amount    money.Amount
	// StudentNo and StudentName appear on the provider's own screen so the
	// person paying can see who they are paying for. Nothing else about the
	// student is sent: a payment provider has no business holding a national
	// identifier or a mother's name.
	StudentNo   string
	StudentName string
	Description string
	// ReturnURL is where the provider sends the student's browser afterwards.
	ReturnURL string
	// CallbackURL is where the provider posts its confirmation.
	CallbackURL string
}

// InitiateResponse is what the provider gave back.
type InitiateResponse struct {
	// ProviderRef is the provider's own identifier for the transaction.
	ProviderRef string
	// RedirectURL sends the student to the provider. Empty for a channel that
	// works by quoting a reference at a counter.
	RedirectURL string
	// Instruction is what to tell the student when there is no redirect: the
	// reference to quote and where to quote it.
	Instruction string
	ExpiresAt   *time.Time
}

// CallbackEnvelope is a delivered callback, before any provider has looked at
// it.
type CallbackEnvelope struct {
	// Body as received, byte for byte. Signatures are computed over the raw
	// bytes; re-encoding a parsed body changes them.
	Body []byte
	// Headers carries the signature, whichever header the provider uses.
	Headers map[string]string
	// Query carries the parameters of a redirect-style confirmation.
	Query map[string]string
	// ReceivedAt is when it arrived, for replay windows.
	ReceivedAt time.Time
}

// Registry holds the providers this deployment has configured.
//
// A university with only ZainCash configures only ZainCash; the others are not
// half-present, they are absent, and asking for one returns an error naming
// what is available. That matters at a desk: "FastPay is not configured" is
// actionable, and a silent failure is not.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a registry from the providers a deployment configured.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p == nil {
			continue
		}
		r.providers[strings.ToUpper(p.Code())] = p
	}
	return r
}

// Get returns a configured provider.
func (r *Registry) Get(code string) (Provider, error) {
	if r == nil || len(r.providers) == 0 {
		return nil, shared.PreconditionFailed("provider.none_configured",
			"no electronic payment provider is configured in this deployment").
			WithDetail("remedy", "collect at the cashier desk, or configure a provider")
	}
	provider, ok := r.providers[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return nil, shared.NotFound("provider.unknown",
			"%q is not a payment provider this deployment offers", code).
			WithDetail("available", r.Codes())
	}
	return provider, nil
}

// Codes lists the configured providers, for a client to offer.
func (r *Registry) Codes() []string {
	if r == nil {
		return nil
	}
	codes := make([]string, 0, len(r.providers))
	for code := range r.providers {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// Describe lists the configured providers with their display names.
func (r *Registry) Describe() []ProviderInfo {
	if r == nil {
		return nil
	}
	out := make([]ProviderInfo, 0, len(r.providers))
	for _, code := range r.Codes() {
		p := r.providers[code]
		out = append(out, ProviderInfo{Code: p.Code(), DisplayName: p.DisplayName()})
	}
	return out
}

// ProviderInfo is a configured provider as a client sees it.
type ProviderInfo struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
}

// ---------------------------------------------------------------------------
// Shared signature verification
// ---------------------------------------------------------------------------

// verifyHMAC checks an HMAC-SHA256 signature over the raw body.
//
// Constant-time comparison, because a byte-by-byte one leaks how much of a
// forged signature was right and that is enough to construct the rest. The
// signature is computed over the bytes as received: re-encoding a parsed body
// changes whitespace and key order, and the signature then never matches for
// reasons that take a day to find.
func verifyHMAC(secret string, body []byte, provided string) bool {
	if secret == "" || provided == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Some providers send the digest upper-case, some prefix it with the
	// algorithm. Normalising here is not laxity: the comparison below is still
	// exact and constant-time.
	provided = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(provided)), "sha256=")
	return hmac.Equal([]byte(expected), []byte(provided))
}

// signPayload produces the signature a provider would send, used by the tests
// and by the branch adapter's own receipts.
func signPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// headerValue reads a header case-insensitively. Go's http.Header canonicalises
// names; a map assembled elsewhere may not have.
func headerValue(headers map[string]string, name string) string {
	if value, ok := headers[name]; ok {
		return value
	}
	lower := strings.ToLower(name)
	for key, value := range headers {
		if strings.ToLower(key) == lower {
			return value
		}
	}
	return ""
}

// rejectUnverified is the single refusal every provider uses, so an operator
// reading a log sees one message whichever channel it came from.
func rejectUnverified(providerCode string) error {
	return shared.Unauthorized("provider.signature_invalid",
		"the %s callback did not verify and has not been acted on", providerCode).
		WithDetail("remedy", "check the shared secret in configuration; "+
			"a burst of these is somebody probing the endpoint")
}

// replayWindow bounds how old a signed callback may be.
//
// A signature proves who sent a message, not when. Without a window, a callback
// captured once can be replayed forever — and while the duplicate-event guard
// would stop it posting twice, an unbounded window means a captured "succeeded"
// can be presented against a later intent that reused a reference.
const replayWindow = 15 * time.Minute

// checkFreshness refuses a callback whose stated time is outside the window.
// A provider that sends no timestamp is trusted on its signature alone, because
// refusing would make the integration impossible rather than more secure.
func checkFreshness(stated time.Time, received time.Time, providerCode string) error {
	if stated.IsZero() {
		return nil
	}
	drift := received.Sub(stated)
	if drift < 0 {
		drift = -drift
	}
	if drift > replayWindow {
		return shared.Unauthorized("provider.callback_stale",
			"this %s callback is timestamped %s away from now and has not been acted on",
			providerCode, drift.Round(time.Second)).
			WithDetail("remedy", "check clock skew between the provider and this server")
	}
	return nil
}

// mapStatus turns a provider's vocabulary into this domain's.
//
// Anything unrecognised becomes pending rather than failed. A provider adding a
// state we have not seen must not cause the system to declare a collection
// failed and let the student pay again; leaving it pending sends it to the
// status poll, which asks the provider directly.
func mapStatus(raw string, succeeded, failed []string) payment.IntentStatus {
	value := strings.ToLower(strings.TrimSpace(raw))
	for _, s := range succeeded {
		if value == s {
			return payment.IntentSucceeded
		}
	}
	for _, f := range failed {
		if value == f {
			return payment.IntentFailed
		}
	}
	return payment.IntentPending
}

// amountFromMinorUnits converts a provider's smallest-unit figure to dinars.
//
// The Iraqi dinar has no circulating subunit, and every provider here quotes
// whole dinars — but they disagree about whether to send "500000" or
// "500000.00", and a float would make the second one 499999.99999. The
// conversion is integer throughout, and a fractional dinar is an error rather
// than something to round.
func amountFromString(raw string) (money.Amount, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, shared.Validation("provider.amount_missing",
			"the callback carries no amount")
	}
	amount, err := money.Parse(trimmed)
	if err != nil {
		return 0, shared.Validation("provider.amount_unreadable",
			"the callback amount %q is not a whole number of dinars", raw).WithCause(err)
	}
	return amount, nil
}

// providerError wraps a transport or protocol failure with the provider's name,
// so an operator reading it knows which integration to look at.
func providerError(code, operation string, err error) error {
	return shared.Internal("provider.request_failed", err,
		"the %s provider could not %s", code, operation)
}

var _ = fmt.Sprintf
