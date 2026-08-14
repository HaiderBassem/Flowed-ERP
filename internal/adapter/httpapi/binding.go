package httpapi

import (
	"context"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 500
)

// requestContext carries the request id into the domain so an audit entry
// written deep inside a command can be traced back to the HTTP call.
func requestContext(c *gin.Context) context.Context {
	return app.WithRequestID(c.Request.Context(), httpx.RequestIDFrom(c.Request.Context()))
}

// bindJSON decodes and validates a body, responding on failure.
//
// The returned bool rather than an error is deliberate: a handler that forgets
// to check an error would proceed with a zero-valued request, while forgetting
// to check the bool reads as obviously wrong at the call site.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		httpx.Respond(c, shared.Validation("invalid_request_body",
			"the request body is not valid: %s", err.Error()).WithCause(err))
		return false
	}
	return true
}

// pathID reads a UUID path parameter.
func pathID(c *gin.Context, name string) (shared.ID, bool) {
	id, err := shared.ParseID(c.Param(name))
	if err != nil {
		httpx.Respond(c, shared.Validation("invalid_path_parameter",
			"%q is not a valid identifier for %s", c.Param(name), name).WithCause(err))
		return shared.NilID, false
	}
	return id, true
}

// optionalQueryID reads a UUID query parameter, treating an unparseable value
// as absent rather than as an error: a stale bookmark should return unfiltered
// results, not a rejection a user cannot act on.
func optionalQueryID(c *gin.Context, name string) (*shared.ID, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, false
	}
	id, err := shared.ParseID(raw)
	if err != nil {
		return nil, false
	}
	return &id, true
}

func optionalQueryInt16(c *gin.Context, name string) (*int16, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 16)
	if err != nil {
		return nil, false
	}
	value := int16(parsed)
	return &value, true
}

// pagination reads limit and offset, clamped so one request cannot ask for the
// whole table.
func pagination(c *gin.Context) (limit, offset int) {
	limit = defaultPageLimit
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if raw := c.Query("offset"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	return limit, offset
}

// payloadHash returns the hash the idempotency middleware computed over the
// request body.
//
// It is persisted alongside the payment so a retry carrying the same key can
// be checked against the request that first used it. A client that reuses a
// key with a different amount then gets a hard error instead of a replayed
// receipt for a collection it never made.
//
// Empty means the route ran without the middleware, which is a wiring mistake
// on any money-moving endpoint; the service treats an empty hash as unverified
// rather than as a match.
func payloadHash(c *gin.Context) string {
	stored, exists := c.Get(httpx.PayloadHashContextKey)
	if !exists {
		return ""
	}
	hash, _ := stored.(string)
	return hash
}

// queryBool reads a boolean query parameter.
//
// An unparseable value is false rather than an error: these are filters, and a
// stale bookmark carrying "?active_only=yes" should return the unfiltered list
// rather than a rejection the reader cannot act on.
func queryBool(c *gin.Context, name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(c.Query(name)))
	return err == nil && value
}
