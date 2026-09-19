// Package messaging delivers the messages the notification service queues.
//
// Kept behind an interface and out of the financial core on purpose: whether a
// university has an SMS contract changes nothing about what a student owes, and
// a deployment with no gateway must still produce the worklist rather than
// failing to schedule anything.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"flowed/internal/domain/notify"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// Config is what an SMS gateway integration needs.
type Config struct {
	Enabled bool
	// BaseURL is the gateway's endpoint. Iraqi providers differ; the shape
	// below is the common one and the field names are configurable where they
	// are not.
	BaseURL string
	APIKey  string
	// Sender is the alphanumeric sender identity registered with the operator.
	// Messages from an unregistered sender are dropped silently by the
	// networks, which is the most confusing possible failure.
	Sender  string
	Timeout time.Duration
}

// SMSGateway delivers over HTTP to a bulk SMS provider.
type SMSGateway struct {
	cfg    Config
	client *http.Client
}

// NewSMSGateway builds the gateway, or returns nil when none is configured.
//
// Returning nil rather than a no-op is deliberate: the notification service
// checks for it and marks messages as worklist entries instead of queueing
// deliveries nothing will perform.
func NewSMSGateway(cfg Config) *SMSGateway {
	if !cfg.Enabled || strings.TrimSpace(cfg.BaseURL) == "" {
		return nil
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &SMSGateway{cfg: cfg, client: &http.Client{Timeout: timeout}}
}

var _ port.Deliverer = (*SMSGateway)(nil)

// Channel reports what this deliverer sends over.
func (g *SMSGateway) Channel() notify.Channel { return notify.ChannelSMS }

// Send delivers one message.
//
// Errors are returned rather than swallowed: the caller records the attempt and
// retries, and a gateway that is down should look like a gateway that is down
// rather than like a message nobody needed to send.
func (g *SMSGateway) Send(ctx context.Context, destination, body string) error {
	if strings.TrimSpace(destination) == "" {
		return shared.Validation("sms.no_destination", "there is no number to send to")
	}

	payload, err := json.Marshal(map[string]any{
		"to":      normaliseIraqiNumber(destination),
		"from":    g.cfg.Sender,
		"text":    body,
		"unicode": true, // Arabic bodies are not GSM-7.
	})
	if err != nil {
		return shared.Internal("sms.encode_failed", err, "the message could not be encoded")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.BaseURL, bytes.NewReader(payload))
	if err != nil {
		return shared.Internal("sms.request_failed", err, "the request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	if g.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return shared.Internal("sms.gateway_unreachable", err, "the SMS gateway could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return shared.Internal("sms.rejected", nil,
			"the gateway answered %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// normaliseIraqiNumber puts a number into the form gateways expect.
//
// The same folding the database applies to a stored phone: 07701234567,
// +964 770 123 4567 and ٠٧٧٠١٢٣٤٥٦٧ are one number, and a gateway that receives
// the wrong form drops the message without saying so.
func normaliseIraqiNumber(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r >= '٠' && r <= '٩':
			digits.WriteRune('0' + (r - '٠'))
		case r >= '۰' && r <= '۹':
			digits.WriteRune('0' + (r - '۰'))
		}
	}

	number := digits.String()
	switch {
	case strings.HasPrefix(number, "964"):
		return number
	case strings.HasPrefix(number, "0"):
		return "964" + strings.TrimPrefix(number, "0")
	default:
		return number
	}
}

var _ = fmt.Sprintf
