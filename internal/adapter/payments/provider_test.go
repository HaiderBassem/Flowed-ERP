package payments

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
)

const testSecret = "a-callback-secret-shared-with-the-provider"

func signedEnvelope(t *testing.T, body map[string]any, header, secret string) CallbackEnvelope {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding body: %v", err)
	}
	return CallbackEnvelope{
		Body:       raw,
		Headers:    map[string]string{header: signPayload(secret, raw)},
		ReceivedAt: time.Now().UTC(),
	}
}

// The rule the whole boundary rests on: an unsigned or wrongly signed callback
// is an error, never a payment. Anyone who can reach the endpoint could
// otherwise mark any student's fees paid.
func TestUnverifiedCallbackIsRefused(t *testing.T) {
	provider := NewZainCash(Config{CallbackSecret: testSecret})

	body := map[string]any{
		"event_id": "evt-1", "order_id": "order-1", "transaction_id": "zc-1",
		"status": "completed", "amount": "500000",
	}

	// Signed with the wrong secret.
	forged := signedEnvelope(t, body, "X-ZainCash-Signature", "not-the-secret")
	if _, err := provider.ParseCallback(context.Background(), forged); err == nil {
		t.Fatal("a callback signed with the wrong secret must be refused")
	} else if code := shared.CodeOf(err); code != "provider.signature_invalid" {
		t.Errorf("code = %q", code)
	}

	// Not signed at all.
	raw, _ := json.Marshal(body)
	unsigned := CallbackEnvelope{Body: raw, ReceivedAt: time.Now()}
	if _, err := provider.ParseCallback(context.Background(), unsigned); err == nil {
		t.Fatal("an unsigned callback must be refused")
	}
}

func TestVerifiedCallbackMapsToTheDomainVocabulary(t *testing.T) {
	provider := NewZainCash(Config{CallbackSecret: testSecret})

	envelope := signedEnvelope(t, map[string]any{
		"event_id": "evt-77", "order_id": "order-77", "transaction_id": "zc-77",
		"status": "completed", "amount": "750000",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}, "X-ZainCash-Signature", testSecret)

	result, err := provider.ParseCallback(context.Background(), envelope)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if result.Status != payment.IntentSucceeded {
		t.Errorf("status = %q, want succeeded", result.Status)
	}
	if result.Amount != 750_000 {
		t.Errorf("amount = %s, want 750,000", result.Amount)
	}
	if result.ExternalEventID != "evt-77" || result.ClientRef != "order-77" || result.ProviderRef != "zc-77" {
		t.Errorf("references did not survive: %+v", result)
	}
}

// A status this build has never seen must not be read as a failure. Declaring
// a collection failed when the provider has not said so invites the student to
// pay a second time.
func TestUnknownStatusIsPendingNotFailed(t *testing.T) {
	provider := NewQiCard(Config{CallbackSecret: testSecret})

	envelope := signedEnvelope(t, map[string]any{
		"event_id": "evt-1", "transaction_id": "qi-1",
		"status": "under_review", "amount": "100000",
	}, "X-Qi-Signature", testSecret)

	result, err := provider.ParseCallback(context.Background(), envelope)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if result.Status != payment.IntentPending {
		t.Errorf("status = %q, want pending", result.Status)
	}
}

// A signature proves who sent a message, not when. Without a window a captured
// "succeeded" can be presented forever.
func TestStaleCallbackIsRefused(t *testing.T) {
	provider := NewFastPay(Config{CallbackSecret: testSecret})

	envelope := signedEnvelope(t, map[string]any{
		"event_id": "evt-old", "order_ref": "fp-1", "status": "paid", "amount": "50000",
		"timestamp": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}, "X-FastPay-Signature", testSecret)

	if _, err := provider.ParseCallback(context.Background(), envelope); err == nil {
		t.Fatal("a callback timestamped two hours ago must be refused")
	} else if code := shared.CodeOf(err); code != "provider.callback_stale" {
		t.Errorf("code = %q", code)
	}
}

// A fractional amount is an error, not something to round. Every figure in this
// system is a whole dinar.
func TestFractionalAmountIsRefused(t *testing.T) {
	provider := NewZainCash(Config{CallbackSecret: testSecret})

	envelope := signedEnvelope(t, map[string]any{
		"event_id": "evt-frac", "transaction_id": "zc-frac",
		"status": "completed", "amount": "500000.75",
	}, "X-ZainCash-Signature", testSecret)

	if _, err := provider.ParseCallback(context.Background(), envelope); err == nil {
		t.Fatal("a fractional dinar amount must be refused")
	}
	// A zero fraction is an Excel-ism and is accepted.
	ok := signedEnvelope(t, map[string]any{
		"event_id": "evt-whole", "transaction_id": "zc-whole",
		"status": "completed", "amount": "500000.00",
	}, "X-ZainCash-Signature", testSecret)
	result, err := provider.ParseCallback(context.Background(), ok)
	if err != nil {
		t.Fatalf("a whole amount written with a zero fraction should parse: %v", err)
	}
	if result.Amount != 500_000 {
		t.Errorf("amount = %s", result.Amount)
	}
}

// A callback with no event identifier still has to be de-duplicable, or a
// retrying provider posts two payments.
func TestCallbackWithoutAnEventIDStillDeduplicates(t *testing.T) {
	provider := NewZainCash(Config{CallbackSecret: testSecret})

	body := map[string]any{
		"transaction_id": "zc-42", "order_id": "order-42",
		"status": "completed", "amount": "200000",
	}
	first, err := provider.ParseCallback(context.Background(), signedEnvelope(t, body, "X-ZainCash-Signature", testSecret))
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.ParseCallback(context.Background(), signedEnvelope(t, body, "X-ZainCash-Signature", testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if first.ExternalEventID == "" || first.ExternalEventID != second.ExternalEventID {
		t.Errorf("two deliveries of one event must share an id, got %q and %q",
			first.ExternalEventID, second.ExternalEventID)
	}
}

func TestInitiateSendsWhatTheProviderNeedsAndNothingElse(t *testing.T) {
	var received map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"transaction_id":"zc-900","redirect_url":"https://pay.example/zc-900"}`))
	}))
	defer server.Close()

	provider := NewZainCash(Config{BaseURL: server.URL, MerchantID: "uni-1", APIKey: "k"})
	response, err := provider.Initiate(context.Background(), InitiateRequest{
		ClientRef:   "intent-1",
		Amount:      1_000_000,
		StudentNo:   "2025001",
		StudentName: "علي محمد",
		Description: "Tuition 2025-2026",
	})
	if err != nil {
		t.Fatalf("initiating: %v", err)
	}
	if response.ProviderRef != "zc-900" || response.RedirectURL == "" {
		t.Errorf("response = %+v", response)
	}

	// A payment provider has no business holding a national identifier or a
	// mother's name, so neither is in the request shape at all.
	encoded, _ := json.Marshal(received)
	for _, forbidden := range []string{"national_id", "mother", "birth"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Errorf("the initiation request carries %q; it must not", forbidden)
		}
	}
}

func TestInitiateRefusesAResponseWithNoReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"redirect_url":"https://pay.example/x"}`))
	}))
	defer server.Close()

	provider := NewZainCash(Config{BaseURL: server.URL})
	if _, err := provider.Initiate(context.Background(), InitiateRequest{Amount: 1000}); err == nil {
		t.Fatal("without the provider's reference their confirmation could never be matched back")
	}
}

// Branch collection deliberately has no callback: the bank statement is the
// confirmation, and a branch payment confirmable by an HTTP call would be a
// branch payment anyone could confirm.
func TestBranchCollectionHasNoCallbackPath(t *testing.T) {
	provider := NewBranchCollection(Config{CallbackSecret: testSecret})

	response, err := provider.Initiate(context.Background(), InitiateRequest{
		ClientRef: "intent-77", Amount: 250_000, StudentNo: "2025001",
	})
	if err != nil {
		t.Fatalf("initiating: %v", err)
	}
	if response.ProviderRef == "" || response.Instruction == "" {
		t.Error("a branch payment must give the student a reference and what to do with it")
	}
	if response.ExpiresAt == nil {
		t.Error("a quoted reference must expire")
	}

	if _, err := provider.ParseCallback(context.Background(), CallbackEnvelope{}); err == nil {
		t.Error("branch collection must refuse callbacks")
	}
	if _, err := provider.FetchStatus(context.Background(), "x"); err == nil {
		t.Error("branch collection must refuse status polling")
	}

	// The reference is deterministic, so a student who lost the slip can be
	// told it again without a second intent being opened.
	again, err := provider.Initiate(context.Background(), InitiateRequest{
		ClientRef: "intent-77", Amount: 250_000, StudentNo: "2025001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ProviderRef != response.ProviderRef {
		t.Errorf("the reference should be stable, got %q then %q", response.ProviderRef, again.ProviderRef)
	}
}

func TestRegistryOffersOnlyWhatIsConfigured(t *testing.T) {
	registry := NewRegistry(NewZainCash(Config{}), NewBranchCollection(Config{}))

	if _, err := registry.Get("ZAINCASH"); err != nil {
		t.Errorf("a configured provider must resolve: %v", err)
	}
	if _, err := registry.Get("zaincash"); err != nil {
		t.Errorf("provider codes are case-insensitive: %v", err)
	}
	_, err := registry.Get("FASTPAY")
	if err == nil {
		t.Fatal("an unconfigured provider must not resolve")
	}
	if code := shared.CodeOf(err); code != "provider.unknown" {
		t.Errorf("code = %q", code)
	}

	if codes := registry.Codes(); len(codes) != 2 {
		t.Errorf("Codes() = %v", codes)
	}
	empty := NewRegistry()
	if _, err := empty.Get("ZAINCASH"); err == nil {
		t.Error("a deployment with no providers must say so rather than failing obscurely")
	}
}

func TestStatusPollAsksTheProviderDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "zc-55") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"transaction_id":"zc-55","order_id":"order-55","status":"completed","amount":"300000"}`))
	}))
	defer server.Close()

	provider := NewZainCash(Config{BaseURL: server.URL})
	result, err := provider.FetchStatus(context.Background(), "zc-55")
	if err != nil {
		t.Fatalf("polling: %v", err)
	}
	if result.Status != payment.IntentSucceeded || result.Amount != 300_000 {
		t.Errorf("result = %+v", result)
	}
	// A poll is idempotent against repeated polling of the same outcome.
	if !strings.HasPrefix(result.ExternalEventID, "poll:") {
		t.Errorf("a poll should be distinguishable from a delivery, got %q", result.ExternalEventID)
	}
}
