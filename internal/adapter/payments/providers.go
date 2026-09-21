package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
)

// Config is what one provider integration needs from the environment.
//
// Every field is configuration rather than code because the values differ per
// university: each has its own merchant account, its own secret, and — during
// the months an integration is being commissioned — its own sandbox endpoint.
type Config struct {
	// Enabled leaves the provider out of the registry entirely when false.
	Enabled bool
	// BaseURL is the provider's API root. A sandbox and a production
	// deployment differ only in this.
	BaseURL string
	// MerchantID identifies the university to the provider.
	MerchantID string
	// APIKey authenticates our requests to them.
	APIKey string
	// CallbackSecret verifies their callbacks to us. Distinct from the API key
	// on purpose: one is sent outward and one is only ever compared, so a leak
	// of the first does not let anyone forge a confirmation.
	CallbackSecret string
	// Timeout bounds a call to the provider. Short: this runs while a student
	// stands at a desk.
	Timeout time.Duration
}

// httpProvider carries what the three HTTP-speaking providers share.
type httpProvider struct {
	code        string
	displayName string
	cfg         Config
	client      *http.Client
}

func newHTTPProvider(code, displayName string, cfg Config) httpProvider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return httpProvider{
		code:        code,
		displayName: displayName,
		cfg:         cfg,
		client:      &http.Client{Timeout: timeout},
	}
}

func (p httpProvider) Code() string        { return p.code }
func (p httpProvider) DisplayName() string { return p.displayName }

// postJSON performs one call to the provider.
func (p httpProvider) postJSON(ctx context.Context, path string, body any, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return providerError(p.code, "encode its request", err)
	}

	endpoint, err := url.JoinPath(p.cfg.BaseURL, path)
	if err != nil {
		return providerError(p.code, "build its endpoint", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return providerError(p.code, "build its request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return providerError(p.code, "be reached", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read: a provider returning an unbounded body must not exhaust
	// this process's memory while a cashier waits.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return providerError(p.code, "return a readable response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return shared.Internal("provider.request_failed", nil,
			"%s answered %d: %s", p.code, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return providerError(p.code, "return readable JSON", err)
	}
	return nil
}

func (p httpProvider) getJSON(ctx context.Context, path string, out any) error {
	endpoint, err := url.JoinPath(p.cfg.BaseURL, path)
	if err != nil {
		return providerError(p.code, "build its endpoint", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return providerError(p.code, "build its request", err)
	}
	req.Header.Set("Accept", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return providerError(p.code, "be reached", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return providerError(p.code, "return a readable response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return shared.Internal("provider.request_failed", nil,
			"%s answered %d: %s", p.code, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return json.Unmarshal(payload, out)
}

// genericCallback is the shape all three HTTP providers' callbacks reduce to
// once their field names are mapped.
type genericCallback struct {
	EventID   string         `json:"event_id"`
	EventType string         `json:"event_type"`
	OrderID   string         `json:"order_id"`
	TxnID     string         `json:"transaction_id"`
	Status    string         `json:"status"`
	Amount    string         `json:"amount"`
	Timestamp string         `json:"timestamp"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Raw       map[string]any `json:"-"`
}

// parseSignedCallback is the body of every HTTP provider's ParseCallback:
// verify, check freshness, map the vocabulary.
//
// The order matters. Nothing is parsed as meaningful until the signature has
// verified, so a forged body cannot reach the amount comparison or the intent
// lookup.
func (p httpProvider) parseSignedCallback(
	raw CallbackEnvelope, signatureHeader string, succeeded, failed []string,
) (*payment.CallbackResult, error) {
	signature := headerValue(raw.Headers, signatureHeader)
	if !verifyHMAC(p.cfg.CallbackSecret, raw.Body, signature) {
		return nil, rejectUnverified(p.code)
	}

	var body genericCallback
	if err := json.Unmarshal(raw.Body, &body); err != nil {
		return nil, shared.Validation("provider.callback_unreadable",
			"the %s callback is not readable JSON", p.code).WithCause(err)
	}
	_ = json.Unmarshal(raw.Body, &body.Raw)

	if stated := parseProviderTime(body.Timestamp); !stated.IsZero() {
		if err := checkFreshness(stated, raw.ReceivedAt, p.code); err != nil {
			return nil, err
		}
	}

	// A callback with no event identifier cannot be de-duplicated, and a
	// provider that retries would then post twice. The transaction reference
	// stands in for it, which is stable across retries of the same event.
	eventID := body.EventID
	if eventID == "" {
		eventID = body.TxnID + ":" + body.Status
	}

	result := &payment.CallbackResult{
		ExternalEventID: eventID,
		EventType:       orDefault(body.EventType, "payment.update"),
		ClientRef:       body.OrderID,
		ProviderRef:     body.TxnID,
		Status:          mapStatus(body.Status, succeeded, failed),
		FailureCode:     body.Code,
		FailureReason:   body.Message,
		Payload:         body.Raw,
	}
	if body.Amount != "" {
		amount, err := amountFromString(body.Amount)
		if err != nil {
			return nil, err
		}
		result.Amount = amount
	}
	return result, nil
}

func parseProviderTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// ---------------------------------------------------------------------------
// ZainCash
// ---------------------------------------------------------------------------

// ZainCash is Iraq's most widely used mobile wallet.
//
// The flow is a redirect: the student is sent to ZainCash's page, pays from
// their wallet balance, and is returned. The confirmation arrives twice over —
// once as a redirect the student's browser makes, and once as a server-to-
// server callback. Only the second is trusted, because the first is under the
// student's control.
type ZainCash struct{ httpProvider }

// NewZainCash builds the ZainCash adapter.
func NewZainCash(cfg Config) *ZainCash {
	return &ZainCash{newHTTPProvider("ZAINCASH", "زين كاش", cfg)}
}

// Initiate opens a ZainCash transaction.
func (p *ZainCash) Initiate(ctx context.Context, req InitiateRequest) (*InitiateResponse, error) {
	var response struct {
		TransactionID string `json:"transaction_id"`
		RedirectURL   string `json:"redirect_url"`
		ExpiresAt     string `json:"expires_at"`
	}

	err := p.postJSON(ctx, "/transaction/init", map[string]any{
		"merchant_id":  p.cfg.MerchantID,
		"amount":       req.Amount.Int64(),
		"order_id":     req.ClientRef,
		"service_type": "tuition",
		"msisdn":       "",
		"redirect_url": req.ReturnURL,
		"callback_url": req.CallbackURL,
		"description":  req.Description,
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.TransactionID == "" {
		return nil, shared.Internal("provider.no_reference", nil,
			"ZainCash accepted the request without returning a transaction reference")
	}

	out := &InitiateResponse{
		ProviderRef: response.TransactionID,
		RedirectURL: response.RedirectURL,
	}
	if expires := parseProviderTime(response.ExpiresAt); !expires.IsZero() {
		out.ExpiresAt = &expires
	}
	return out, nil
}

// ParseCallback verifies and maps a ZainCash confirmation.
func (p *ZainCash) ParseCallback(_ context.Context, raw CallbackEnvelope) (*payment.CallbackResult, error) {
	return p.parseSignedCallback(raw, "X-ZainCash-Signature",
		[]string{"completed", "success", "paid"},
		[]string{"failed", "cancelled", "declined", "rejected"})
}

// FetchStatus asks ZainCash directly about a transaction.
func (p *ZainCash) FetchStatus(ctx context.Context, providerRef string) (*payment.CallbackResult, error) {
	var response struct {
		TransactionID string `json:"transaction_id"`
		OrderID       string `json:"order_id"`
		Status        string `json:"status"`
		Amount        string `json:"amount"`
	}
	if err := p.getJSON(ctx, "/transaction/"+url.PathEscape(providerRef), &response); err != nil {
		return nil, err
	}

	result := &payment.CallbackResult{
		// A poll is not a delivery, so it carries no provider event id of its
		// own. The reference and status make it idempotent against repeated
		// polling of the same outcome.
		ExternalEventID: "poll:" + response.TransactionID + ":" + response.Status,
		EventType:       "payment.status_polled",
		ClientRef:       response.OrderID,
		ProviderRef:     response.TransactionID,
		Status: mapStatus(response.Status,
			[]string{"completed", "success", "paid"},
			[]string{"failed", "cancelled", "declined", "rejected"}),
	}
	if response.Amount != "" {
		amount, err := amountFromString(response.Amount)
		if err != nil {
			return nil, err
		}
		result.Amount = amount
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Qi Card
// ---------------------------------------------------------------------------

// QiCard is the card network most Iraqi salaries are paid onto, which makes it
// the channel a parent paying from a salary account will reach for.
type QiCard struct{ httpProvider }

// NewQiCard builds the Qi Card adapter.
func NewQiCard(cfg Config) *QiCard {
	return &QiCard{newHTTPProvider("QI", "كي كارد", cfg)}
}

// Initiate opens a Qi Card payment session.
func (p *QiCard) Initiate(ctx context.Context, req InitiateRequest) (*InitiateResponse, error) {
	var response struct {
		PaymentID   string `json:"payment_id"`
		PaymentURL  string `json:"payment_url"`
		ExpiresAt   string `json:"expires_at"`
		Instruction string `json:"instruction"`
	}

	err := p.postJSON(ctx, "/payments", map[string]any{
		"merchant":     p.cfg.MerchantID,
		"amount":       req.Amount.Int64(),
		"currency":     "IQD",
		"reference":    req.ClientRef,
		"customer_ref": req.StudentNo,
		"customer":     req.StudentName,
		"description":  req.Description,
		"success_url":  req.ReturnURL,
		"webhook_url":  req.CallbackURL,
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.PaymentID == "" {
		return nil, shared.Internal("provider.no_reference", nil,
			"Qi Card accepted the request without returning a payment reference")
	}

	out := &InitiateResponse{
		ProviderRef: response.PaymentID,
		RedirectURL: response.PaymentURL,
		Instruction: response.Instruction,
	}
	if expires := parseProviderTime(response.ExpiresAt); !expires.IsZero() {
		out.ExpiresAt = &expires
	}
	return out, nil
}

// ParseCallback verifies and maps a Qi Card confirmation.
func (p *QiCard) ParseCallback(_ context.Context, raw CallbackEnvelope) (*payment.CallbackResult, error) {
	return p.parseSignedCallback(raw, "X-Qi-Signature",
		[]string{"captured", "settled", "success"},
		[]string{"declined", "failed", "voided", "expired"})
}

// FetchStatus asks Qi Card directly about a payment.
func (p *QiCard) FetchStatus(ctx context.Context, providerRef string) (*payment.CallbackResult, error) {
	var response struct {
		PaymentID string `json:"payment_id"`
		Reference string `json:"reference"`
		Status    string `json:"status"`
		Amount    string `json:"amount"`
	}
	if err := p.getJSON(ctx, "/payments/"+url.PathEscape(providerRef), &response); err != nil {
		return nil, err
	}

	result := &payment.CallbackResult{
		ExternalEventID: "poll:" + response.PaymentID + ":" + response.Status,
		EventType:       "payment.status_polled",
		ClientRef:       response.Reference,
		ProviderRef:     response.PaymentID,
		Status: mapStatus(response.Status,
			[]string{"captured", "settled", "success"},
			[]string{"declined", "failed", "voided", "expired"}),
	}
	if response.Amount != "" {
		amount, err := amountFromString(response.Amount)
		if err != nil {
			return nil, err
		}
		result.Amount = amount
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// FastPay
// ---------------------------------------------------------------------------

// FastPay is a wallet with wide agent coverage outside the cities, which is
// what makes it the channel for a family with no card.
type FastPay struct{ httpProvider }

// NewFastPay builds the FastPay adapter.
func NewFastPay(cfg Config) *FastPay {
	return &FastPay{newHTTPProvider("FASTPAY", "فاست باي", cfg)}
}

// Initiate opens a FastPay order.
func (p *FastPay) Initiate(ctx context.Context, req InitiateRequest) (*InitiateResponse, error) {
	var response struct {
		OrderRef  string `json:"order_ref"`
		PayURL    string `json:"pay_url"`
		ExpiresAt string `json:"expires_at"`
	}

	err := p.postJSON(ctx, "/orders", map[string]any{
		"store_id":     p.cfg.MerchantID,
		"amount":       req.Amount.Int64(),
		"currency":     "IQD",
		"external_ref": req.ClientRef,
		"note":         req.Description,
		"return_url":   req.ReturnURL,
		"notify_url":   req.CallbackURL,
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.OrderRef == "" {
		return nil, shared.Internal("provider.no_reference", nil,
			"FastPay accepted the request without returning an order reference")
	}

	out := &InitiateResponse{ProviderRef: response.OrderRef, RedirectURL: response.PayURL}
	if expires := parseProviderTime(response.ExpiresAt); !expires.IsZero() {
		out.ExpiresAt = &expires
	}
	return out, nil
}

// ParseCallback verifies and maps a FastPay notification.
func (p *FastPay) ParseCallback(_ context.Context, raw CallbackEnvelope) (*payment.CallbackResult, error) {
	return p.parseSignedCallback(raw, "X-FastPay-Signature",
		[]string{"paid", "completed"},
		[]string{"expired", "cancelled", "failed"})
}

// FetchStatus asks FastPay directly about an order.
func (p *FastPay) FetchStatus(ctx context.Context, providerRef string) (*payment.CallbackResult, error) {
	var response struct {
		OrderRef    string `json:"order_ref"`
		ExternalRef string `json:"external_ref"`
		Status      string `json:"status"`
		Amount      string `json:"amount"`
	}
	if err := p.getJSON(ctx, "/orders/"+url.PathEscape(providerRef), &response); err != nil {
		return nil, err
	}

	result := &payment.CallbackResult{
		ExternalEventID: "poll:" + response.OrderRef + ":" + response.Status,
		EventType:       "payment.status_polled",
		ClientRef:       response.ExternalRef,
		ProviderRef:     response.OrderRef,
		Status: mapStatus(response.Status,
			[]string{"paid", "completed"},
			[]string{"expired", "cancelled", "failed"}),
	}
	if response.Amount != "" {
		amount, err := amountFromString(response.Amount)
		if err != nil {
			return nil, err
		}
		result.Amount = amount
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Branch collection
// ---------------------------------------------------------------------------

// BranchCollection is payment at a bank counter against a quoted reference.
//
// It has no API and needs none: the student is given a reference, takes it to
// Rafidain or Rasheed, and the bank's statement is the confirmation. This
// adapter exists so that channel goes through the same door as the others —
// the intent is created, the reference is quoted, and the settlement import
// confirms it. Without it, branch collection would be a cashier typing a
// reference by hand, which is exactly the unreconciled path the settlement
// work was written to close.
type BranchCollection struct {
	cfg Config
	// window is how long a quoted reference stays valid. Long, because a
	// student may travel to a branch in another governorate.
	window time.Duration
}

// NewBranchCollection builds the branch adapter.
func NewBranchCollection(cfg Config) *BranchCollection {
	return &BranchCollection{cfg: cfg, window: 14 * 24 * time.Hour}
}

func (p *BranchCollection) Code() string        { return "BRANCH" }
func (p *BranchCollection) DisplayName() string { return "الدفع في فرع المصرف" }

// Initiate mints the reference the student quotes at the counter.
//
// The reference is deterministic and short enough to read down a telephone. It
// is derived from the intent rather than random so that a student who lost the
// slip can be told it again without a second intent being created.
func (p *BranchCollection) Initiate(_ context.Context, req InitiateRequest) (*InitiateResponse, error) {
	digest := signPayload(p.cfg.CallbackSecret, []byte(req.ClientRef))
	reference := strings.ToUpper(fmt.Sprintf("%s-%s", firstSegment(req.StudentNo), digest[:8]))
	expires := time.Now().UTC().Add(p.window)

	return &InitiateResponse{
		ProviderRef: reference,
		Instruction: fmt.Sprintf(
			"ادفع مبلغ %s في أي فرع، مع ذكر الرمز %s", req.Amount, reference),
		ExpiresAt: &expires,
	}, nil
}

// ParseCallback refuses: there is no callback for a bank counter.
//
// The confirmation for this channel is the statement import, which is a
// deliberate design rather than a gap. A branch payment that could be confirmed
// by an HTTP call would be a branch payment anyone could confirm.
func (p *BranchCollection) ParseCallback(context.Context, CallbackEnvelope) (*payment.CallbackResult, error) {
	return nil, shared.PreconditionFailed("provider.no_callback",
		"branch collection is confirmed by importing the bank statement, not by a callback").
		WithDetail("remedy", "import the day's statement; the reference on the line matches the intent")
}

// FetchStatus refuses for the same reason.
func (p *BranchCollection) FetchStatus(context.Context, string) (*payment.CallbackResult, error) {
	return nil, shared.PreconditionFailed("provider.no_status_api",
		"a bank counter has no status to poll; the statement is the confirmation")
}

func firstSegment(studentNo string) string {
	if studentNo == "" {
		return "STU"
	}
	if len(studentNo) > 6 {
		return studentNo[len(studentNo)-6:]
	}
	return studentNo
}

var _ = money.Amount(0)
