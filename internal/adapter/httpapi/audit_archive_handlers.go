package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
)

// AuditArchiveHandlers expose the off-host audit copy: ship now, and check
// whether the copy and the database still agree.
//
// Verification is the endpoint that matters. It is the one place an auditor can
// ask a question the database cannot answer about itself — whether anything has
// been removed from the trail since it was written.
type AuditArchiveHandlers struct {
	// Ship is nil when this deployment named no destination. The routes are
	// still mounted, and answer with a refusal that names the setting, rather
	// than 404 — an auditor asking "is the trail archived" deserves an answer
	// rather than a missing page.
	Ship *app.AuditShipService
}

// NewAuditArchiveHandlers wires the archive routes.
func NewAuditArchiveHandlers(ship *app.AuditShipService) *AuditArchiveHandlers {
	return &AuditArchiveHandlers{Ship: ship}
}

// Register mounts the archive routes.
func (h *AuditArchiveHandlers) Register(g *gin.RouterGroup) {
	archive := g.Group("/audit/archive")

	archive.POST("/ship",
		httpx.RequireRoles(shared.RoleAdmin),
		h.ShipNow)
	archive.GET("/verify",
		httpx.RequireRoles(shared.RoleAdmin, shared.RoleAuditor),
		h.Verify)
}

// ShipNow copies whatever has not been copied yet.
//
// Exposed as well as scheduled because the moment somebody wants it is the
// moment before they take a backup or start an investigation, and waiting a
// quarter of an hour for the timer is the wrong answer then.
func (h *AuditArchiveHandlers) ShipNow(c *gin.Context) {
	if h.Ship == nil {
		httpx.Respond(c, notArchived())
		return
	}
	result, err := h.Ship.Ship(requestContext(c), httpx.MustActor(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, result)
}

// Verify compares the archive against the database.
//
// A report with problems is still a 200: the request succeeded, and what it
// found is the answer. A 5xx here would be read as "the check failed" when what
// happened is "the check found something", and those must not look alike.
func (h *AuditArchiveHandlers) Verify(c *gin.Context) {
	if h.Ship == nil {
		httpx.Respond(c, notArchived())
		return
	}
	report, err := h.Ship.Verify(requestContext(c), httpx.MustActor(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":     report.OK(),
		"report": report,
	})
}

// notArchived is the refusal for a deployment with no archive configured. It
// names the setting, because "not configured" without "here is what to set" is
// a dead end for whoever reads it.
func notArchived() error {
	return shared.PreconditionFailed("audit.archive_not_configured",
		"this deployment does not copy its audit trail off-host").
		WithDetail("consequence",
			"the hash chain detects an altered entry but cannot detect a deleted one").
		WithDetail("remedy", "set AUDIT_ARCHIVE_DIR or AUDIT_ARCHIVE_URL and restart")
}
