package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/port"
)

const (
	// IdempotencyKeyHeader carries the client's key for a money-moving command.
	IdempotencyKeyHeader = "Idempotency-Key"

	// IdempotentReplayHeader marks a response served from a stored record
	// rather than freshly computed. A terminal that prints a receipt can use it
	// to say "duplicate" on the paper.
	IdempotentReplayHeader = "Idempotent-Replay"

	// PayloadHashContextKey holds the hash this middleware computed over the
	// request body.
	//
	// The handler persists it alongside the payment so that a later replay can
	// prove the retry is the same request. Recomputing it downstream would not
	// do: the body is a one-shot stream that the handler's binder has already
	// consumed by then, and re-marshalling the decoded struct produces
	// different bytes than the client actually sent.
	PayloadHashContextKey = "flowed.idempotency.payload_hash"

	minIdempotencyKeyLength = 8
	maxIdempotencyKeyLength = 255

	// maxRecordedResponseBytes bounds what is buffered for replay. The
	// money-moving commands answer with a payment and its allocations, a few
	// kilobytes at most; the cap only exists so that a route wired here by
	// mistake cannot buffer an export into memory.
	maxRecordedResponseBytes = 1 << 20

	// completionTimeout bounds the write that records the command's outcome.
	completionTimeout = 5 * time.Second
)

// Record statuses, mirroring the CHECK constraint on idempotency_record.
const (
	idempotencyInProgress = "in_progress"
	idempotencySucceeded  = "succeeded"
	idempotencyFailed     = "failed"
)

// Idempotency makes a POST route safe to retry.
//
// It is meant for the commands that move money. A cashier's terminal that
// loses its answer to a timeout will press the button again, and the second
// press must return the first receipt rather than take the money twice. The
// middleware claims the key before the handler runs, replays a stored outcome
// when the same key comes back, and refuses a duplicate that is still in
// flight.
//
// commandName scopes the key: the same key used for a payment and for a refund
// are two different records, because the uniqueness constraint is on the pair.
func Idempotency(repo port.IdempotencyRepository, commandName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader(IdempotencyKeyHeader))
		if err := validateIdempotencyKey(key); err != nil {
			Respond(c, err)
			return
		}

		payload, err := bufferRequestBody(c)
		if err != nil {
			Respond(c, err)
			return
		}
		digest := sha256.Sum256(payload)
		payloadHash := hex.EncodeToString(digest[:])
		c.Set(PayloadHashContextKey, payloadHash)

		var actorID *shared.ID
		if actor, ok := ActorFrom(c); ok {
			id := actor.UserID
			actorID = &id
		}

		existing, err := repo.Begin(requestContext(c), key, commandName, payloadHash, actorID)
		if err != nil {
			Respond(c, err)
			return
		}
		if existing != nil {
			replayOrReject(c, existing, payloadHash)
			return
		}

		recorder := &recordingWriter{ResponseWriter: c.Writer, limit: maxRecordedResponseBytes}
		c.Writer = recorder

		// A panic unwinds straight past the call below, which would leave the key
		// claimed but never settled — and an in_progress record refuses every
		// retry until it expires, locking the cashier out of a payment for as
		// long as the record lives. The deferred check releases it.
		//
		// It deliberately does not recover: Recovery owns the response and the
		// stack trace, and swallowing the panic here would cost both.
		settled := false
		defer func() {
			if settled {
				return
			}
			recordFailure(c, repo, key, commandName, "command.panicked")
		}()

		c.Next()

		recordOutcome(c, repo, key, commandName, recorder)
		settled = true
	}
}

// bufferRequestBody reads the body so it can be hashed, then puts it back.
//
// The handler binds its command from this same body once the middleware chain
// has run. Reading it here consumes the only copy of a one-shot stream, so
// without the restore below every idempotent route would see an empty payload
// — and the failure would be silent, because an empty body binds cleanly to a
// struct full of zero values and a payment for zero dinars is a valid-looking
// request.
func bufferRequestBody(c *gin.Context) ([]byte, error) {
	if c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}

	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, shared.Validation("request.body_too_large",
				"the request body exceeds the %d byte limit", tooLarge.Limit).
				WithDetail("max_bytes", tooLarge.Limit).
				WithCause(err)
		}
		return nil, shared.Validation("request.body_unreadable",
			"the request body could not be read").WithCause(err)
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(payload))
	return payload, nil
}

// replayOrReject answers a request whose key has been seen before.
func replayOrReject(c *gin.Context, existing *port.IdempotencyRecord, payloadHash string) {
	if existing.PayloadHash != "" && existing.PayloadHash != payloadHash {
		// Same key, different request. Replaying the first outcome would answer a
		// question this client did not ask — a receipt for another amount, or for
		// another student — so the reuse fails loudly instead.
		Respond(c, shared.Conflict("command.idempotency_key_reused",
			"this idempotency key was already used for a different request payload").
			WithDetail("idempotency_key", existing.Key))
		return
	}

	switch existing.Status {
	case idempotencySucceeded:
		status := existing.ResponseCode
		if status == 0 {
			status = http.StatusOK
		}
		c.Header(IdempotentReplayHeader, "true")
		c.Abort()
		if len(existing.ResponseBody) == 0 {
			// The original response was too large to store (see recordingWriter).
			// The command certainly ran, so the client must not retry it; it has
			// to re-read the resource instead.
			c.Status(status)
			return
		}
		// The stored body is written verbatim. That is what makes a replay
		// genuinely identical to the first answer, receipt number included,
		// rather than a freshly rendered approximation of it.
		c.Data(status, "application/json; charset=utf-8", existing.ResponseBody)

	case idempotencyInProgress:
		Respond(c, shared.Conflict("command.in_progress",
			"a request with this idempotency key is still running; wait for its answer instead of retrying").
			WithDetail("idempotency_key", existing.Key))

	case idempotencyFailed:
		Respond(c, shared.Conflict("command.previous_attempt_failed",
			"the previous request with this idempotency key failed; retry with a new key").
			WithDetail("idempotency_key", existing.Key).
			WithDetail("previous_error_code", existing.ErrorCode))

	default:
		Respond(c, shared.Conflict("command.idempotency_state_unknown",
			"this idempotency key is in an unrecognised state").
			WithDetail("idempotency_key", existing.Key).
			WithDetail("status", existing.Status))
	}
}

// recordOutcome stores what the handler answered, so the next retry can replay
// it.
func recordOutcome(
	c *gin.Context,
	repo port.IdempotencyRepository,
	key, commandName string,
	recorder *recordingWriter,
) {
	status := c.Writer.Status()
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		recordFailure(c, repo, key, commandName, recorder.errorCode())
		return
	}

	ctx, cancel := settlementContext(c)
	defer cancel()

	if err := repo.Complete(ctx, key, commandName, status, recorder.recorded()); err != nil {
		logSettlementFailure(ctx, c, commandName, key, status, err)
	}
}

// recordFailure releases the key for a command that did not succeed.
func recordFailure(c *gin.Context, repo port.IdempotencyRepository, key, commandName, errorCode string) {
	ctx, cancel := settlementContext(c)
	defer cancel()

	if err := repo.Fail(ctx, key, commandName, errorCode); err != nil {
		logSettlementFailure(ctx, c, commandName, key, c.Writer.Status(), err)
	}
}

// settlementContext detaches the bookkeeping write from the request.
//
// By this point the command has committed and its answer is already on the
// wire. A client that hung up, or a request that hit its deadline while the
// transaction was committing, would otherwise cancel this write too and leave
// the key stuck at in_progress.
func settlementContext(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		context.WithoutCancel(requestContext(c)), completionTimeout)
}

// logSettlementFailure records that the outcome could not be stored.
//
// Nothing can be done about the response — it is written. Losing the record
// costs only the duplicate-detection window for one key, which is a far
// smaller harm than turning a payment the database has already accepted into a
// 500 the cashier will react to by paying again.
func logSettlementFailure(
	ctx context.Context,
	c *gin.Context,
	commandName, key string,
	status int,
	cause error,
) {
	logger.From(requestContext(c)).ErrorContext(ctx, "could not record idempotent command outcome",
		slog.String("command", commandName),
		slog.String("idempotency_key", key),
		slog.String("request_id", RequestIDFrom(c)),
		slog.Int("status", status),
		slog.String("error", cause.Error()))
}

// recordingWriter tees the response into a buffer so that a later retry with
// the same key can be answered with the identical bytes.
type recordingWriter struct {
	gin.ResponseWriter
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.capture(b)
	return w.ResponseWriter.Write(b)
}

func (w *recordingWriter) WriteString(s string) (int, error) {
	w.capture([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

func (w *recordingWriter) capture(b []byte) {
	if w.overflowed {
		return
	}
	if w.buf.Len()+len(b) > w.limit {
		// A stored fragment of a JSON document is worse than no record at all: a
		// replay would hand the client a truncated body it cannot parse. Drop what
		// was captured and remember the response is unreplayable.
		w.overflowed = true
		w.buf.Reset()
		return
	}
	// bytes.Buffer.Write never reports an error; it panics if it cannot grow.
	w.buf.Write(b)
}

// recorded returns the captured body, or nil when there is nothing safe to
// replay.
func (w *recordingWriter) recorded() []byte {
	if w.overflowed || w.buf.Len() == 0 {
		return nil
	}
	return slices.Clone(w.buf.Bytes())
}

// errorCode digs the domain code out of a recorded failure, so the stored
// record says why the command failed rather than merely that it did.
func (w *recordingWriter) errorCode() string {
	var envelope ErrorResponse
	if err := json.Unmarshal(w.buf.Bytes(), &envelope); err != nil || envelope.Error.Code == "" {
		return "unknown_error"
	}
	return envelope.Error.Code
}

func validateIdempotencyKey(key string) error {
	if key == "" {
		return shared.Validation("command.idempotency_key_required",
			"this endpoint moves money and requires an %s header; generate one per logical operation and send the same one when retrying",
			IdempotencyKeyHeader).
			WithDetail("header", IdempotencyKeyHeader)
	}
	if len(key) < minIdempotencyKeyLength || len(key) > maxIdempotencyKeyLength {
		return shared.Validation("command.idempotency_key_invalid",
			"an %s must be between %d and %d characters long",
			IdempotencyKeyHeader, minIdempotencyKeyLength, maxIdempotencyKeyLength).
			WithDetail("min_length", minIdempotencyKeyLength).
			WithDetail("max_length", maxIdempotencyKeyLength)
	}
	for i := 0; i < len(key); i++ {
		// Printable ASCII without spaces. The key is stored, logged and compared;
		// control characters in it would forge log records exactly as they would
		// in a request id.
		if key[i] < 0x21 || key[i] > 0x7e {
			return shared.Validation("command.idempotency_key_invalid",
				"an %s may contain printable ASCII characters only, with no spaces",
				IdempotencyKeyHeader)
		}
	}
	return nil
}
