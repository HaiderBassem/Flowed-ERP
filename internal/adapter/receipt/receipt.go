// Package receipt renders printable Arabic receipts for payments and refunds.
//
// A receipt is the only part of this system a student physically holds, and it
// is the document they bring back when something is disputed. Three properties
// follow from that and shape everything here.
//
// It must reproduce exactly. Reprinting a receipt a year later has to produce
// the same document, so the renderer reads only frozen rows — the payment, its
// allocations, the account's snapshot — and never recomputes anything from
// configuration that may have moved since.
//
// It must be hard to alter. The amount appears twice, in figures and written
// out in Arabic, and the written form is closed with لا غير so nothing can be
// appended to it.
//
// A reprint must announce itself. The first print is the original; every
// subsequent one is stamped as a copy, because two documents both claiming to
// be the original receipt for one payment is precisely the ambiguity a
// forger needs.
package receipt

import (
	"strconv"
	"time"

	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
)

// Kind distinguishes a collection from a repayment.
type Kind string

const (
	// KindPayment is money received — سند قبض.
	KindPayment Kind = "payment"
	// KindRefund is money returned — سند صرف.
	KindRefund Kind = "refund"
)

// Institution is the letterhead. It is configuration rather than constants so
// that one deployment can serve a university whose name is not hardcoded into
// somebody else's binary.
type Institution struct {
	UniversityNameAr string
	CollegeNameAr    string
	Address          string
	Phone            string
	// LogoDataURI is an optional inline image. Inlined rather than linked
	// because a receipt must print identically from a machine with no network.
	LogoDataURI string
	// FooterAr is a line the office chooses — an opening-hours note, a refund
	// policy, a thank-you. Printed below the total and above the signature.
	FooterAr string
	// CurrencyNameAr names the currency in words, for the amount-in-words line
	// (التفقيط). Configurable because the receipt says "دينار عراقي" and a
	// hard-coded currency is the one thing a second deployment cannot change.
	CurrencyNameAr string
}

// Line is one row of the receipt's itemisation: which installment the money
// went to, and how much of it.
type Line struct {
	Label   string
	DueDate *shared.Date
	Amount  money.Amount
}

// Data is everything a receipt prints. It is assembled once from frozen rows
// and then rendered; the renderer performs no lookups of its own.
type Data struct {
	Kind        Kind
	Institution Institution

	// Number is the receipt or refund number from the per-year, per-desk
	// series — the figure a paper reconciliation matches against.
	Number   string
	IssuedAt time.Time

	StudentName    string
	StudentNumber  string
	MotherName     string
	CollegeName    string
	DepartmentName string
	StageLabel     string
	StudyTypeName  string
	AcademicYear   string

	Amount money.Amount
	// AmountInWords is the same figure written out. Rendered by the caller via
	// money.SpellArabic so the receipt and any other document that spells an
	// amount agree word for word.
	AmountInWords   string
	PaymentMethod   string
	MethodReference string

	Lines []Line

	// The account position at the moment of issue, so a student can see what
	// remains without asking. Frozen into the receipt: recomputing it on a
	// reprint would show a different balance than the paper the student holds.
	TotalFees     money.Amount
	TotalDiscount money.Amount
	NetFees       money.Amount
	PaidToDate    money.Amount
	Remaining     money.Amount

	CashierName string
	PayerName   string
	Notes       string

	// VoidedAt marks a receipt whose payment was later reversed. A voided
	// receipt still prints — an auditor asking about it needs to see it — but
	// it prints struck through and labelled, never as a live collection.
	VoidedAt   *time.Time
	VoidReason string

	// CopyNumber is 0 for the original and counts up for each reprint.
	CopyNumber int
	PrintedAt  time.Time
	PrintedBy  string
}

// IsCopy reports whether this is a reprint rather than the original.
func (d Data) IsCopy() bool { return d.CopyNumber > 0 }

// IsVoided reports whether the underlying payment was reversed.
func (d Data) IsVoided() bool { return d.VoidedAt != nil }

// Title is the document's Arabic heading.
//
// Iraqi practice names the two documents differently: money in is a سند قبض,
// money out a سند صرف. Printing the wrong heading on a refund would make it
// look like a second collection.
func (d Data) Title() string {
	if d.Kind == KindRefund {
		return "سند صرف"
	}
	return "سند قبض"
}

// AmountLabel names what the figure represents.
func (d Data) AmountLabel() string {
	if d.Kind == KindRefund {
		return "المبلغ المصروف"
	}
	return "المبلغ المستلم"
}

// CopyLabel is the stamp a reprint carries.
func (d Data) CopyLabel() string {
	switch {
	case d.CopyNumber == 1:
		return "نسخة طبق الأصل"
	case d.CopyNumber > 1:
		return "نسخة طبق الأصل (" + strconv.Itoa(d.CopyNumber) + ")"
	default:
		return ""
	}
}

// FormatDateTime renders an instant for the receipt in Baghdad local time.
//
// Storage is UTC everywhere in this system, but a receipt states the time the
// cashier and the student were both standing there. Printing 21:40 UTC on a
// slip handed over at midnight in Baghdad invites a dispute that is entirely
// avoidable.
func FormatDateTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02  15:04")
}

// FormatDate renders a calendar date.
func FormatDate(d shared.Date) string { return d.String() }
