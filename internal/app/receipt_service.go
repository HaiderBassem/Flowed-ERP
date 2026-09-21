package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"flowed/internal/adapter/pdf"
	"flowed/internal/adapter/receipt"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// ReceiptService assembles the document a student walks away with.
//
// It reads only frozen rows — the payment, its allocations, the account's fee
// snapshot — so a receipt reprinted in three years reproduces the one printed
// today. Nothing here recomputes from configuration, because configuration
// moves and the paper in the student's hand does not.
type ReceiptService struct {
	deps        Deps
	institution receipt.Institution
	// settings supplies the letterhead when the office has set one. Nil-safe:
	// see WithSettings.
	settings *SettingsService
	location *time.Location
	auditor
}

// NewReceiptService wires receipt rendering.
//
// The institution details and the timezone are configuration: one binary
// should serve any university, and the receipt states Baghdad local time even
// though every stored timestamp is UTC.
func NewReceiptService(d Deps, institution receipt.Institution, loc *time.Location) *ReceiptService {
	if loc == nil {
		loc = time.UTC
	}
	return &ReceiptService{
		deps:        d,
		institution: institution,
		location:    loc,
		auditor:     newAuditor(d.Audit, d.Clock),
	}
}

// WithSettings makes the letterhead come from the settings table rather than
// from start-up configuration.
//
// Attached after construction because the settings service needs the same Deps
// this one does, and threading both through one constructor would make the
// wiring order load-bearing. Left unset — as every test leaves it — the
// configured institution is used, which is what receipts printed before the
// settings screen existed.
func (s *ReceiptService) WithSettings(settings *SettingsService) *ReceiptService {
	s.settings = settings
	return s
}

// letterhead is the institution a receipt prints, preferring what the office
// typed into the settings screen.
func (s *ReceiptService) letterhead(ctx context.Context) receipt.Institution {
	if s.settings == nil {
		return s.institution
	}
	return s.settings.Institution(ctx)
}

// Format selects the rendering.
type Format string

const (
	// FormatHTML is an A5 page for a browser or an office printer.
	FormatHTML Format = "html"
	// FormatPDF is the formal document — A4, in colour, signed and filed, and
	// the one a ministry accepts.
	FormatPDF Format = "pdf"
	// FormatThermal is the counter slip: 80mm of roll, one column, black on
	// white, cut where the content ends. A different document from the A4
	// receipt rather than the same one shrunk — see pdf.ThermalReceipt.
	FormatThermal Format = "thermal"
	// FormatText is an 80mm thermal roll, which is what most cashier desks
	// actually print to.
	FormatText Format = "text"
)

// Rendered is a receipt ready to send.
type Rendered struct {
	Body        []byte
	ContentType string
	Filename    string
	// CopyNumber is 0 for the original and counts up per reprint, so a caller
	// can tell the operator what they just produced.
	CopyNumber int
}

// PaymentReceipt renders the receipt for a collection.
//
// Every render is recorded. The first is the original; each one after it is
// stamped as a copy, and the audit trail carries who reprinted it and when.
// Reprints are legitimate — students lose paper — but a burst of them against
// one payment is exactly the pattern an investigation looks for, and that
// pattern only exists if each print is written down.
func (s *ReceiptService) PaymentReceipt(
	ctx context.Context, actor shared.Actor, paymentID shared.ID, format Format,
) (*Rendered, error) {
	var rendered *Rendered
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		target, err := s.deps.Payments.GetByID(ctx, paymentID)
		if err != nil {
			return err
		}
		// A draft has no receipt number and is not a document. Printing one
		// would put a slip in a student's hand for money the system does not
		// yet consider collected.
		if target.Status == payment.StatusDraft {
			return shared.PreconditionFailed("receipt.payment_not_posted",
				"this payment has not been posted and has no receipt number yet")
		}

		data, err := s.buildPaymentData(ctx, target)
		if err != nil {
			return err
		}

		copyNumber, err := s.recordPrint(ctx, actor, "payment", target.ID,
			target.StudentID, &target.AccountID, derefString(target.ReceiptNo))
		if err != nil {
			return err
		}
		data.CopyNumber = copyNumber
		data.PrintedAt = nowOr(s.deps.Clock)
		data.PrintedBy = s.printedBy(ctx, actor)

		rendered, err = s.render(data, format)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rendered, nil
}

// RefundReceipt renders the voucher for money returned.
func (s *ReceiptService) RefundReceipt(
	ctx context.Context, actor shared.Actor, refundID shared.ID, format Format,
) (*Rendered, error) {
	var rendered *Rendered
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		target, err := s.deps.Refunds.GetByID(ctx, refundID)
		if err != nil {
			return err
		}
		if target.Status != payment.RefundPosted {
			return shared.PreconditionFailed("receipt.refund_not_posted",
				"this refund is %s; a voucher prints only once the money has actually gone out",
				target.Status).
				WithDetail("refund_status", string(target.Status))
		}

		data, err := s.buildRefundData(ctx, target)
		if err != nil {
			return err
		}

		copyNumber, err := s.recordPrint(ctx, actor, "refund", target.ID,
			target.StudentID, &target.AccountID, derefString(target.RefundNo))
		if err != nil {
			return err
		}
		data.CopyNumber = copyNumber
		data.PrintedAt = nowOr(s.deps.Clock)
		data.PrintedBy = s.printedBy(ctx, actor)

		rendered, err = s.render(data, format)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rendered, nil
}

func (s *ReceiptService) buildPaymentData(ctx context.Context, p *payment.Payment) (receipt.Data, error) {
	data := receipt.Data{
		Kind:            receipt.KindPayment,
		Institution:     s.letterhead(ctx),
		Number:          derefString(p.ReceiptNo),
		Amount:          p.Amount,
		AmountInWords:   money.SpellArabic(p.Amount),
		MethodReference: derefString(p.MethodReference),
		PayerName:       derefString(p.PayerName),
		Notes:           derefString(p.Notes),
		VoidedAt:        p.VoidedAt,
		VoidReason:      derefString(p.VoidReason),
	}
	data.IssuedAt = p.PaidAt
	if p.PostedAt != nil {
		data.IssuedAt = *p.PostedAt
	}

	if method, err := s.deps.Reference.GetPaymentMethod(ctx, p.PaymentMethodID); err == nil {
		data.PaymentMethod = method.NameAr
	}
	if cashier, err := s.deps.Users.GetByID(ctx, p.CashierUserID); err == nil {
		data.CashierName = cashier.FullName
	}

	if err := s.fillContext(ctx, &data, p.StudentID, p.AccountID); err != nil {
		return data, err
	}

	// Which installments the money settled. Read from the allocation rows, so
	// a reprint shows the same breakdown the original did even if later
	// payments have since moved the plan on.
	allocations, err := s.deps.Payments.LiveAllocations(ctx, p.ID)
	if err == nil {
		for _, a := range allocations {
			due := a.DueDate
			data.Lines = append(data.Lines, receipt.Line{
				Label:   "القسط " + strconv.Itoa(int(a.Number)),
				DueDate: &due,
				Amount:  a.Amount,
			})
		}
	}

	return data, nil
}

func (s *ReceiptService) buildRefundData(ctx context.Context, r *payment.Refund) (receipt.Data, error) {
	data := receipt.Data{
		Kind:            receipt.KindRefund,
		Institution:     s.letterhead(ctx),
		Number:          derefString(r.RefundNo),
		Amount:          r.Amount,
		AmountInWords:   money.SpellArabic(r.Amount),
		MethodReference: derefString(r.MethodReference),
		Notes:           r.Reason,
	}
	if r.PostedAt != nil {
		data.IssuedAt = *r.PostedAt
	} else {
		data.IssuedAt = r.RequestedAt
	}

	if method, err := s.deps.Reference.GetPaymentMethod(ctx, r.PaymentMethodID); err == nil {
		data.PaymentMethod = method.NameAr
	}
	if r.PostedBy != nil {
		if u, err := s.deps.Users.GetByID(ctx, *r.PostedBy); err == nil {
			data.CashierName = u.FullName
		}
	}

	if err := s.fillContext(ctx, &data, r.StudentID, r.AccountID); err != nil {
		return data, err
	}

	// The voucher names the receipt it reverses. Without it a refund is a
	// slip of paper with no stated origin, which is the last thing a finance
	// office wants in its file.
	if original, err := s.deps.Payments.GetByID(ctx, r.PaymentID); err == nil {
		data.Lines = append(data.Lines, receipt.Line{
			Label:  "استرجاع عن السند " + derefString(original.ReceiptNo),
			Amount: r.Amount,
		})
	}

	return data, nil
}

// fillContext adds the student and account details every receipt carries.
func (s *ReceiptService) fillContext(ctx context.Context, data *receipt.Data, studentID, accountID shared.ID) error {
	person, err := s.deps.Students.GetByID(ctx, studentID)
	if err != nil {
		return err
	}
	data.StudentName = person.FullName
	data.StudentNumber = person.StudentNo
	data.MotherName = person.MotherName

	account, err := s.deps.Accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	data.TotalFees = account.GrossTotal
	data.TotalDiscount = account.DiscountTotal
	data.NetFees = account.EffectiveNet()
	data.PaidToDate = account.NetPaid()
	data.Remaining = account.Remaining()
	// The number alone: the field is already labelled المرحلة, and "المرحلة
	// المرحلة 1" is what printing the word twice produces.
	data.StageLabel = strconv.Itoa(int(account.Stage))

	if year, err := s.deps.Years.GetByID(ctx, account.AcademicYearID); err == nil {
		data.AcademicYear = year.Code
	}
	if college, err := s.deps.Reference.GetCollege(ctx, account.CollegeID); err == nil {
		data.CollegeName = college.NameAr
		if data.Institution.CollegeNameAr == "" {
			data.Institution.CollegeNameAr = college.NameAr
		}
	}
	if department, err := s.deps.Reference.GetDepartment(ctx, account.DepartmentID); err == nil {
		data.DepartmentName = department.NameAr
	}
	if studyType, err := s.deps.Reference.GetStudyType(ctx, account.StudyTypeID); err == nil {
		data.StudyTypeName = studyType.NameAr
	}

	return nil
}

// recordPrint appends the print to the audit trail and returns how many times
// this document had been printed before.
//
// The count comes from the trail itself rather than from a column on the
// payment, so it cannot drift from the record: if the audit says a receipt was
// printed four times, the fifth is stamped as the fourth copy.
func (s *ReceiptService) recordPrint(
	ctx context.Context, actor shared.Actor, entityType string,
	entityID, studentID shared.ID, accountID *shared.ID, number string,
) (int, error) {
	previous, err := s.deps.Audit.List(ctx, entityType+"_receipt", entityID, 200)
	if err != nil {
		return 0, err
	}
	copyNumber := len(previous)

	action := "receipt.printed"
	if copyNumber > 0 {
		action = "receipt.reprinted"
	}

	return copyNumber, s.record(ctx, port.AuditEntry{
		EntityType: entityType + "_receipt",
		EntityID:   &entityID,
		Action:     action,
		Actor:      actor,
		StudentID:  &studentID,
		AccountID:  accountID,
		Metadata: map[string]any{
			"document_number": number,
			"copy_number":     copyNumber,
		},
	})
}

func (s *ReceiptService) render(data receipt.Data, format Format) (*Rendered, error) {
	name := "receipt-" + sanitiseFilename(data.Number)

	switch format {
	case FormatPDF:
		body, err := pdf.Receipt(data, s.location)
		if err != nil {
			return nil, err
		}
		return &Rendered{
			Body:        body,
			ContentType: "application/pdf",
			Filename:    name + ".pdf",
			CopyNumber:  data.CopyNumber,
		}, nil
	case FormatThermal:
		body, err := pdf.ThermalReceipt(data, s.location)
		if err != nil {
			return nil, err
		}
		return &Rendered{
			Body:        body,
			ContentType: "application/pdf",
			Filename:    name + "-thermal.pdf",
			CopyNumber:  data.CopyNumber,
		}, nil
	case FormatText:
		return &Rendered{
			Body:        receipt.RenderText(data, s.location),
			ContentType: "text/plain; charset=utf-8",
			Filename:    name + ".txt",
			CopyNumber:  data.CopyNumber,
		}, nil
	case FormatHTML, "":
		body, err := receipt.RenderHTML(data, s.location)
		if err != nil {
			return nil, shared.Internal("receipt.render_failed", err, "rendering the receipt")
		}
		return &Rendered{
			Body:        body,
			ContentType: "text/html; charset=utf-8",
			Filename:    name + ".html",
			CopyNumber:  data.CopyNumber,
		}, nil
	default:
		return nil, shared.Validation("receipt.unknown_format",
			"%q is not a supported receipt format; use pdf, thermal, html or text", format)
	}
}

// sanitiseFilename keeps a receipt number safe to put in a Content-Disposition
// header, where a stray quote or newline would let the value break out of it.
func sanitiseFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "document"
	}
	return string(out)
}

// printedBy is the name that goes on the printed slip.
//
// The operator's full name rather than their username: "بواسطة ahmed" tells a
// student nothing, and the settings screen promises that the name typed there
// is what appears here. Falls back to the username when the account cannot be
// read, because a receipt with an imperfect footer still beats no receipt for
// money already taken.
func (s *ReceiptService) printedBy(ctx context.Context, actor shared.Actor) string {
	if shared.IsNil(actor.UserID) {
		return actor.Username
	}
	user, err := s.deps.Users.GetByID(ctx, actor.UserID)
	if err != nil || user == nil || strings.TrimSpace(user.FullName) == "" {
		return actor.Username
	}
	return user.FullName
}
