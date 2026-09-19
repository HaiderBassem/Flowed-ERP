// Package httpx is the HTTP edge of the system: the one place a domain error
// becomes a status code and a response body, and the middleware every route
// runs through.
//
// Handlers in the layers above build domain errors and hand them to Respond;
// they never choose a status code themselves. That is what keeps the same
// failure looking identical on every endpoint, and what keeps an internal
// message — which routinely quotes SQL, a constraint name, or a row — from
// reaching an API consumer by accident.
package httpx

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/logger"
)

// genericInternalMessage is what a client is told when the real message is not
// safe to show. The request id is the handle they quote to support.
const genericInternalMessage = "an internal error occurred; quote the request id when reporting it"

// ErrorResponse is the envelope every failure is returned in.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries the machine-readable code clients branch on, a human
// message, the structured details a cashier needs on screen, and the request
// id that ties the response to the server's logs.
type ErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
}

// StatusForKind maps a domain error kind onto the status that represents it.
//
// KindPreconditionFailed maps to 422 rather than 412: 412 belongs to
// conditional-request headers, while a business precondition — paying into a
// closed year, voiding a payment that already carries a refund — is a
// well-formed request the server understands and refuses.
func StatusForKind(kind shared.Kind) int {
	switch kind {
	case shared.KindValidation:
		return http.StatusBadRequest
	case shared.KindUnauthorized:
		return http.StatusUnauthorized
	case shared.KindForbidden:
		return http.StatusForbidden
	case shared.KindNotFound:
		return http.StatusNotFound
	case shared.KindConflict:
		return http.StatusConflict
	case shared.KindPreconditionFailed:
		return http.StatusUnprocessableEntity
	case shared.KindRateLimited:
		return http.StatusTooManyRequests
	case shared.KindInvariantViolation, shared.KindInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// Respond writes an error to the client and aborts the middleware chain.
//
// Anything that is not a domain error is treated as internal: a foreign error
// reaching the transport means some layer forgot to classify it, and guessing
// a friendlier status would hide that.
func Respond(c *gin.Context, err error) {
	if err == nil {
		return
	}

	requestID := RequestIDFrom(c)
	domainErr, ok := shared.AsDomain(err)
	if !ok {
		domainErr = shared.Internal("internal_error", err, "an unclassified error reached the HTTP layer")
	}

	body := ErrorBody{
		Code:      domainErr.Code,
		Message:   domainErr.Message,
		Details:   sanitiseDetails(domainErr.Details),
		RequestID: requestID,
	}

	if domainErr.Kind == shared.KindInternal || domainErr.Kind == shared.KindInvariantViolation {
		logInternalFailure(c, domainErr, err, requestID)
		// The message and details of an internal failure are written for an
		// operator reading logs. They quote statements, constraint names and row
		// contents, so the client gets the code, the request id, and nothing else.
		body.Message = genericInternalMessage
		body.Details = nil
	}

	writeError(c, StatusForKind(domainErr.Kind), body)
}

func logInternalFailure(c *gin.Context, domainErr *shared.Error, original error, requestID string) {
	ctx := requestContext(c)

	attrs := []any{
		slog.String("request_id", requestID),
		slog.String("code", domainErr.Code),
		slog.String("kind", domainErr.Kind.String()),
		slog.String("method", requestMethod(c)),
		slog.String("path", requestPath(c)),
		// The full chain, cause included: this record is the only place the real
		// reason survives, because the client's copy is deliberately generic.
		slog.String("error", original.Error()),
	}

	if domainErr.Kind == shared.KindInvariantViolation {
		// An invariant violation is not an ordinary fault. It means the system
		// caught itself about to persist inconsistent state — installments that
		// do not sum to the net, allocations exceeding their payment — and the
		// marker exists so an alert can fire on it alone.
		attrs = append(attrs, slog.Bool("invariant_violation", true))
	}

	logger.From(ctx).ErrorContext(ctx, "request failed", attrs...)
}

// detailKeysNeverReturned lists detail keys that carry raw infrastructure text
// rather than something a client can act on. The PostgreSQL adapter attaches
// "db_detail" to conflict errors, and its content is a driver string quoting
// the offending column and value — useful in a log, not on the wire.
var detailKeysNeverReturned = map[string]struct{}{
	"db_detail": {},
	"sql":       {},
	"query":     {},
	"stack":     {},
}

func sanitiseDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	out := make(map[string]any, len(details))
	for key, value := range details {
		if _, blocked := detailKeysNeverReturned[key]; blocked {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// writeError is the single point where an error envelope reaches the wire, so
// that the shape stays identical whether it came from Respond, the panic
// recovery, or the rate limiter.
func writeError(c *gin.Context, status int, body ErrorBody) {
	if c.Writer.Written() {
		// A handler already committed a status and part of a body. Appending an
		// error object would produce a document the client cannot parse, so stop
		// the chain and leave the truncated response as the only evidence — the
		// log record written by the caller carries the real story.
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(status, ErrorResponse{Error: body})
}

// requestContext returns the request's context, tolerating a gin.Context built
// by hand in a test.
func requestContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}

func requestMethod(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return c.Request.Method
}

// requestPath prefers the route pattern over the concrete path, so that logs
// group by endpoint instead of scattering across every student id.
func requestPath(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if pattern := c.FullPath(); pattern != "" {
		return pattern
	}
	if c.Request == nil || c.Request.URL == nil {
		return ""
	}
	return c.Request.URL.Path
}
