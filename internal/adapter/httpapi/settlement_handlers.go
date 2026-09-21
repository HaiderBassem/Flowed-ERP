package httpapi

import (
	"encoding/csv"
	"errors"
	"io"
	"mime/multipart"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/money"
	"flowed/internal/domain/settlement"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// SettlementHandlers import bank and card statements and expose what did not
// reconcile.
type SettlementHandlers struct {
	Settlement *app.SettlementService
}

// NewSettlementHandlers wires the reconciliation endpoints.
func NewSettlementHandlers(s *app.SettlementService) *SettlementHandlers {
	return &SettlementHandlers{Settlement: s}
}

// Register mounts the routes. Reconciliation is finance work, readable by the
// auditor whose job is to check that it was done.
func (h *SettlementHandlers) Register(g *gin.RouterGroup) {
	settlements := g.Group("/settlements")
	settlements.Use(httpx.RequireRoles(
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor))

	settlements.GET("", h.ListBatches)
	settlements.GET("/exceptions", h.Exceptions)
	settlements.GET("/unconfirmed", h.Unconfirmed)
	settlements.GET("/:id", h.GetBatch)

	write := httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin)
	settlements.POST("/import", write, h.Import)
	settlements.POST("/:id/rematch", write, h.Rematch)
	settlements.POST("/lines/:line_id/resolve", write, h.ResolveLine)
}

// Import uploads a statement and matches it.
//
// CSV because that is what every Iraqi bank's portal exports and what a
// treasury clerk can produce from whatever they were sent. The parser is
// deliberately forgiving about column naming and strict about amounts: a
// misread header costs a re-upload, and a misread amount would reconcile the
// wrong figure.
func (h *SettlementHandlers) Import(c *gin.Context) {
	header, err := c.FormFile("file")
	if err != nil {
		httpx.Respond(c, shared.Validation("settlement.file_required",
			"upload the statement as a CSV in the \"file\" field").WithCause(err))
		return
	}

	content, rows, err := parseStatementCSV(header)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	from, ok := optionalDate(c, formValue(c, "statement_from"), "statement_from")
	if !ok {
		return
	}
	to, ok := optionalDate(c, formValue(c, "statement_to"), "statement_to")
	if !ok {
		return
	}

	result, err := h.Settlement.ImportStatement(requestContext(c), httpx.MustActor(c), app.ImportStatementInput{
		SourceCode:    c.PostForm("source_code"),
		SourceName:    formValue(c, "source_name"),
		Filename:      header.Filename,
		Content:       content,
		StatementFrom: from,
		StatementTo:   to,
		Rows:          rows,
		Notes:         formValue(c, "notes"),
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toSettlementResult(result))
}

// Rematch runs matching again over a batch's open lines.
func (h *SettlementHandlers) Rematch(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	result, err := h.Settlement.RematchBatch(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toSettlementResult(result))
}

// ListBatches returns imported statements.
func (h *SettlementHandlers) ListBatches(c *gin.Context) {
	limit, offset := pagination(c)

	var status *settlement.BatchStatus
	if raw := c.Query("status"); raw != "" {
		value := settlement.BatchStatus(raw)
		status = &value
	}

	batches, total, err := h.Settlement.ListBatches(requestContext(c), httpx.MustActor(c), status, limit, offset)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]SettlementBatchView, 0, len(batches))
	for _, b := range batches {
		views = append(views, toBatchView(b))
	}
	httpx.OKPage(c, views, total, limit, offset)
}

// GetBatch returns one statement with its lines.
func (h *SettlementHandlers) GetBatch(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}

	batch, lines, err := h.Settlement.GetBatch(
		requestContext(c), httpx.MustActor(c), id, queryBool(c, "only_open"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, SettlementBatchDetailView{
		Batch: toBatchView(batch),
		Lines: toLineViews(lines),
	})
}

// Exceptions lists every line still needing a person, across batches.
func (h *SettlementHandlers) Exceptions(c *gin.Context) {
	limit, _ := pagination(c)

	exceptions, err := h.Settlement.Exceptions(requestContext(c), httpx.MustActor(c), limit)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]SettlementExceptionView, 0, len(exceptions))
	for _, e := range exceptions {
		view := SettlementExceptionView{
			LineID:      e.LineID.String(),
			BatchID:     e.BatchID.String(),
			SourceCode:  e.SourceCode,
			Filename:    e.Filename,
			LineNo:      e.LineNo,
			ExternalRef: e.ExternalRef,
			Amount:      e.Amount,
			Status:      string(e.Status),
			Variance:    e.Variance,
		}
		if e.ValueDate != nil {
			view.ValueDate = ptr(e.ValueDate.String())
		}
		if e.PaymentID != nil {
			view.PaymentID = ptr(e.PaymentID.String())
		}
		views = append(views, view)
	}
	httpx.OK(c, views)
}

// Unconfirmed lists posted non-cash collections no statement has confirmed.
func (h *SettlementHandlers) Unconfirmed(c *gin.Context) {
	limit, _ := pagination(c)

	grace := 72 * time.Hour
	if raw := c.Query("grace_hours"); raw != "" {
		if hours, err := strconv.Atoi(raw); err == nil && hours >= 0 {
			grace = time.Duration(hours) * time.Hour
		}
	}

	payments, err := h.Settlement.UnconfirmedPayments(requestContext(c), httpx.MustActor(c), grace, limit)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]UnconfirmedPaymentView, 0, len(payments))
	for _, p := range payments {
		views = append(views, UnconfirmedPaymentView{
			PaymentID:  p.PaymentID.String(),
			ReceiptNo:  p.ReceiptNo,
			StudentID:  p.StudentID.String(),
			Amount:     p.Amount,
			Reference:  p.Reference,
			MethodCode: p.MethodCode,
			PaidAt:     p.PaidAt,
		})
	}
	httpx.OK(c, views)
}

// ResolveLine records what a person decided about a line.
func (h *SettlementHandlers) ResolveLine(c *gin.Context) {
	id, ok := pathID(c, "line_id")
	if !ok {
		return
	}
	var req ResolveSettlementLineRequest
	if !bindJSON(c, &req) {
		return
	}

	paymentID, ok := respondingID(c, req.PaymentID)
	if !ok {
		return
	}

	line, err := h.Settlement.ResolveLine(requestContext(c), httpx.MustActor(c), app.ResolveLineInput{
		LineID:    id,
		Status:    settlement.MatchStatus(req.Status),
		PaymentID: paymentID,
		Note:      req.Note,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toLineView(line))
}

// ---------------------------------------------------------------------------
// CSV parsing
// ---------------------------------------------------------------------------

// statementColumns maps the header names banks actually use onto the four
// things a line needs. Matching is case-insensitive and ignores punctuation,
// because "Value Date", "value_date" and "VALUE-DATE" are the same column and
// refusing the file over that sends a clerk back to Excel.
var statementColumns = map[string][]string{
	"reference":   {"reference", "ref", "externalref", "transactionref", "narrative1", "utr"},
	"amount":      {"amount", "credit", "creditamount", "value", "amountiqd"},
	"valuedate":   {"valuedate", "date", "transactiondate", "postingdate"},
	"description": {"description", "details", "narrative", "particulars", "remarks"},
}

func parseStatementCSV(header *multipart.FileHeader) ([]byte, []app.StatementRow, error) {
	file, err := header.Open()
	if err != nil {
		return nil, nil, shared.Validation("settlement.unreadable_upload",
			"the uploaded file could not be read").WithCause(err)
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, shared.Validation("settlement.unreadable_upload",
			"the uploaded file could not be read").WithCause(err)
	}

	// Excel writes a byte-order mark at the start of a UTF-8 CSV; left in
	// place it becomes part of the first header name and no column matches.
	body := strings.TrimPrefix(string(content), "\ufeff")
	reader := csv.NewReader(strings.NewReader(body))
	// Bank exports pad rows unevenly; refusing the file over a trailing comma
	// would send the clerk back to the spreadsheet for nothing.
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	headerRow, err := reader.Read()
	if err != nil {
		return nil, nil, shared.Validation("settlement.unparseable_csv",
			"the statement is not readable as CSV: %s", err.Error()).WithCause(err)
	}

	index := map[string]int{}
	for position, name := range headerRow {
		normalised := normaliseHeader(name)
		for field, aliases := range statementColumns {
			for _, alias := range aliases {
				if normalised == alias {
					if _, taken := index[field]; !taken {
						index[field] = position
					}
				}
			}
		}
	}
	if _, ok := index["amount"]; !ok {
		return nil, nil, shared.Validation("settlement.no_amount_column",
			"the statement has no column this parser recognises as an amount").
			WithDetail("looked_for", statementColumns["amount"]).
			WithDetail("found_columns", headerRow)
	}

	var rows []app.StatementRow
	for lineNo := 1; ; lineNo++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, shared.Validation("settlement.unparseable_csv",
				"row %d is not readable: %s", lineNo, err.Error()).WithCause(err)
		}

		amountText := columnValue(record, index, "amount")
		if strings.TrimSpace(amountText) == "" {
			// A blank amount is a subtotal or a spacer row, which bank exports
			// are full of. Skipping is right; failing would make the common
			// export unusable.
			continue
		}
		amount, err := money.Parse(amountText)
		if err != nil {
			return nil, nil, shared.Validation("settlement.unreadable_amount",
				"row %d has an amount this system cannot read as whole dinars: %q", lineNo, amountText).
				WithCause(err)
		}
		if amount.IsZero() {
			continue
		}

		row := app.StatementRow{
			LineNo:      lineNo,
			ExternalRef: strings.TrimSpace(columnValue(record, index, "reference")),
			Amount:      amount,
			Description: strings.TrimSpace(columnValue(record, index, "description")),
			Raw:         rawRow(headerRow, record),
		}
		if raw := strings.TrimSpace(columnValue(record, index, "valuedate")); raw != "" {
			if parsed, err := parseStatementDate(raw); err == nil {
				row.ValueDate = &parsed
			}
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return nil, nil, shared.Validation("settlement.empty_statement",
			"the statement has no rows carrying an amount")
	}
	return content, rows, nil
}

func normaliseHeader(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func columnValue(record []string, index map[string]int, field string) string {
	position, ok := index[field]
	if !ok || position >= len(record) {
		return ""
	}
	return record[position]
}

// rawRow keeps the row as the file had it, so a mismatch can be investigated
// against what the bank actually sent rather than against what was extracted.
func rawRow(header, record []string) map[string]any {
	raw := make(map[string]any, len(record))
	for i, value := range record {
		name := "column_" + strconv.Itoa(i+1)
		if i < len(header) && strings.TrimSpace(header[i]) != "" {
			name = strings.TrimSpace(header[i])
		}
		raw[name] = value
	}
	return raw
}

// parseStatementDate accepts the formats Iraqi bank exports use.
func parseStatementDate(raw string) (shared.Date, error) {
	for _, layout := range []string{"2006-01-02", "02/01/2006", "02-01-2006", "2006/01/02", "01/02/2006"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return shared.DateFromTime(parsed), nil
		}
	}
	return shared.Date{}, shared.Validation("settlement.unreadable_date",
		"%q is not a date this parser recognises", raw)
}

func formValue(c *gin.Context, field string) *string {
	value := strings.TrimSpace(c.PostForm(field))
	if value == "" {
		return nil
	}
	return &value
}

func toSettlementResult(result *app.ImportStatementResult) SettlementImportView {
	return SettlementImportView{
		Batch:      toBatchView(result.Batch),
		Lines:      toLineViews(result.Lines),
		Exceptions: result.Exceptions,
	}
}

func toBatchView(b *settlement.Batch) SettlementBatchView {
	view := SettlementBatchView{
		ID:            b.ID.String(),
		SourceCode:    b.SourceCode,
		SourceName:    b.SourceName,
		Filename:      b.Filename,
		Status:        string(b.Status),
		LineCount:     b.LineCount,
		MatchedCount:  b.MatchedCount,
		TotalAmount:   b.TotalAmount,
		MatchedAmount: b.MatchedAmount,
	}
	if b.StatementFrom != nil {
		view.StatementFrom = ptr(b.StatementFrom.String())
	}
	if b.StatementTo != nil {
		view.StatementTo = ptr(b.StatementTo.String())
	}
	return view
}

func toLineViews(lines []*settlement.Line) []SettlementLineView {
	views := make([]SettlementLineView, 0, len(lines))
	for _, line := range lines {
		views = append(views, toLineView(line))
	}
	return views
}

func toLineView(line *settlement.Line) SettlementLineView {
	view := SettlementLineView{
		ID:          line.ID.String(),
		LineNo:      line.LineNo,
		ExternalRef: line.ExternalRef,
		Amount:      line.Amount,
		Description: line.Description,
		Status:      string(line.Status),
		Variance:    line.Variance,
		ReviewNote:  line.ReviewNote,
	}
	if line.ValueDate != nil {
		view.ValueDate = ptr(line.ValueDate.String())
	}
	if line.MatchedID != nil {
		view.MatchedPaymentID = ptr(line.MatchedID.String())
	}
	return view
}

var _ = port.SettlementException{}
