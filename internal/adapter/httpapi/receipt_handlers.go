package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
)

// ReceiptHandlers serves printable receipts.
type ReceiptHandlers struct {
	receipts *app.ReceiptService
}

// NewReceiptHandlers wires the receipt routes.
func NewReceiptHandlers(receipts *app.ReceiptService) *ReceiptHandlers {
	return &ReceiptHandlers{receipts: receipts}
}

// Register mounts the receipt routes on an authenticated group.
//
// They hang off the payment and refund they belong to rather than under a
// /receipts collection, because a receipt is not an entity of its own — it is
// a rendering of a document that already exists.
func (h *ReceiptHandlers) Register(g *gin.RouterGroup) {
	readers := httpx.RequireRoles(
		shared.RoleCashier, shared.RoleFinanceManager, shared.RoleAdmin,
		shared.RoleAuditor, shared.RoleRegistrar,
	)

	g.GET("/payments/:id/receipt", readers, h.PaymentReceipt)
	g.GET("/refunds/:id/receipt", readers, h.RefundReceipt)
}

// PaymentReceipt renders the receipt for a collection.
//
//	GET /payments/:id/receipt?format=html|text&download=true
//
// The default is HTML, which prints from any browser. `format=text` returns
// the 80mm thermal rendering that most cashier desks actually feed to their
// roll printer.
func (h *ReceiptHandlers) PaymentReceipt(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	rendered, err := h.receipts.PaymentReceipt(
		requestContext(c), httpx.MustActor(c), id, receiptFormat(c))
	if err != nil {
		// A receipt that will not render is a student standing at a desk with
		// nothing to take away, which is a different kind of outage from a
		// slow report and deserves its own count.
		httpx.InstrumentsFrom(c).ExportFailure(c.Request.Context(), "payment_receipt")
		httpx.Respond(c, err)
		return
	}
	writeReceipt(c, rendered)
}

// RefundReceipt renders the voucher for money returned.
func (h *ReceiptHandlers) RefundReceipt(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	rendered, err := h.receipts.RefundReceipt(
		requestContext(c), httpx.MustActor(c), id, receiptFormat(c))
	if err != nil {
		httpx.InstrumentsFrom(c).ExportFailure(c.Request.Context(), "refund_receipt")
		httpx.Respond(c, err)
		return
	}
	writeReceipt(c, rendered)
}

func receiptFormat(c *gin.Context) app.Format {
	switch c.Query("format") {
	case "text", "txt", "thermal":
		return app.FormatText
	default:
		return app.FormatHTML
	}
}

// writeReceipt sends the rendered document.
//
// It answers with the document itself rather than a JSON envelope: the caller
// is a browser print dialog or a printer driver, and either would have to
// unwrap a JSON string before it could put ink on paper.
func writeReceipt(c *gin.Context, rendered *app.Rendered) {
	// The copy number travels in a header so a cashier's terminal can warn
	// that it is about to print a duplicate, without having to parse the body.
	c.Header("X-Receipt-Copy-Number", strconv.Itoa(rendered.CopyNumber))

	if c.Query("download") == "true" {
		c.Header("Content-Disposition", `attachment; filename="`+rendered.Filename+`"`)
	} else {
		c.Header("Content-Disposition", `inline; filename="`+rendered.Filename+`"`)
	}

	// A receipt is a financial document; a cached copy served later could show
	// a stale copy number or a balance that has since moved.
	c.Header("Cache-Control", "no-store, must-revalidate")

	c.Data(http.StatusOK, rendered.ContentType, rendered.Body)
}
