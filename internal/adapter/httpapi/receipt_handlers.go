package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/platform/httpx"
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

	g.GET("/payments/:id/receipt", h.PaymentReceipt)
	g.GET("/refunds/:id/receipt", h.RefundReceipt)
}

// PaymentReceipt renders the receipt for a collection.
//
//	GET /payments/:id/receipt?format=pdf|html|text&download=true
//
// The default is PDF. `format=html` returns a page for a browser tab, and
// `format=text` returns
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

// receiptFormat reads the requested rendering.
//
// PDF is the default. It is the one a student files and a ministry accepts —
// the same page on every machine, with the Arabic font inside the file — and
// defaulting to it means the good document is what comes out when nobody
// chooses. HTML stays for a quick look in a browser tab, and text for the
// 80mm thermal roll most counters actually print to.
func receiptFormat(c *gin.Context) app.Format {
	switch c.Query("format") {
	case "text", "txt", "thermal":
		return app.FormatText
	case "html":
		return app.FormatHTML
	default:
		return app.FormatPDF
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
