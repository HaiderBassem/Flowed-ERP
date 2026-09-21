// Package academic holds the academic context: years, enrollments, and the
// rules governing how a student moves through them.
package academic

import (
	"regexp"
	"time"

	"flowed/internal/domain/shared"
)

// YearStatus is the lifecycle state of an academic year.
type YearStatus string

const (
	// YearDraft is a year being set up. Nothing may reference it yet.
	YearDraft YearStatus = "draft"
	// YearOpen is normal operation: registration, payments, results.
	YearOpen YearStatus = "open"
	// YearFinanciallyClosed freezes money while academic records stay
	// writable. This state exists because the Iraqi calendar demands it:
	// second-round results arrive weeks after the treasury closes its books,
	// and a system with a single "closed" flag forces one of the two to be
	// wrong.
	YearFinanciallyClosed YearStatus = "financially_closed"
	// YearClosed freezes everything.
	YearClosed YearStatus = "closed"
	// YearAdjustmentOpen is a time-boxed reopening for an audited correction.
	// Only a whitelist of commands runs in it.
	YearAdjustmentOpen YearStatus = "adjustment_open"
)

// DebtBlockPolicy decides what prior-year debt does to a new registration.
type DebtBlockPolicy string

const (
	// DebtIgnore registers the student regardless.
	DebtIgnore DebtBlockPolicy = "ignore"
	// DebtWarn registers the student but surfaces the debt to the registrar.
	DebtWarn DebtBlockPolicy = "warn"
	// DebtBlock refuses registration until the debt is settled or overridden.
	DebtBlock DebtBlockPolicy = "block"
)

var yearCodePattern = regexp.MustCompile(`^\d{4}-\d{4}$`)

// Year is one academic year.
type Year struct {
	ID                   shared.ID
	Code                 string
	StartDate            shared.Date
	EndDate              shared.Date
	Status               YearStatus
	RegistrationDeadline *shared.Date
	DebtBlockPolicy      DebtBlockPolicy
	// GraduationClearancePolicy decides what an outstanding balance does to a
	// graduation. Per year, because a university changes the rule and every
	// historical graduation must keep showing the rule that applied to it.
	GraduationClearancePolicy ClearancePolicy

	FinanciallyClosedAt  *time.Time
	FinanciallyClosedBy  *shared.ID
	ClosedAt             *time.Time
	ClosedBy             *shared.ID
	AdjustmentWindowEnds *time.Time
	AdjustmentReason     *string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewYear builds a year in draft.
func NewYear(code string, start, end shared.Date) (*Year, error) {
	if !yearCodePattern.MatchString(code) {
		return nil, shared.Validation("academic_year.invalid_code",
			"academic year code must look like 2025-2026, got %q", code)
	}
	if !start.Before(end) {
		return nil, shared.Validation("academic_year.invalid_dates",
			"the year must start before it ends (%s to %s)", start, end)
	}
	// The code's two halves should match the dates, or reports grouped by code
	// will disagree with reports filtered by date.
	if got := code[:4]; got != itoa4(start.Year) {
		return nil, shared.Validation("academic_year.code_date_mismatch",
			"code %q starts with %s but the year starts in %d", code, got, start.Year)
	}
	return &Year{
		ID:                        shared.NewID(),
		Code:                      code,
		StartDate:                 start,
		EndDate:                   end,
		Status:                    YearDraft,
		DebtBlockPolicy:           DebtWarn,
		GraduationClearancePolicy: ClearanceWarn,
	}, nil
}

// AcceptsFinancialPosting reports whether money may move in this year.
//
// This is the gate every payment, refund, discount application and account
// generation passes through. Note what it does not decide: whether debt from
// this year can still be collected. A payment settling a 2023-2024 debt in
// 2026 posts against the *current* year and allocates to the old year's
// installments, so closing a year stops the university rewriting its records,
// not collecting what it is owed.
func (y *Year) AcceptsFinancialPosting() bool {
	return y.Status == YearOpen || y.Status == YearAdjustmentOpen
}

// AcceptsAcademicRecording reports whether results and statuses may still be
// written. True right through financial close, which is the whole point of
// having two close states.
func (y *Year) AcceptsAcademicRecording() bool {
	switch y.Status {
	case YearOpen, YearFinanciallyClosed, YearAdjustmentOpen:
		return true
	default:
		return false
	}
}

// AcceptsEnrollment reports whether new registrations may be created.
func (y *Year) AcceptsEnrollment() bool { return y.Status == YearOpen }

// RequireFinancialPosting returns a precondition error unless money may move.
func (y *Year) RequireFinancialPosting(operation string) error {
	if y.AcceptsFinancialPosting() {
		return nil
	}
	err := shared.PreconditionFailed("academic_year.financially_closed",
		"%s is not allowed: academic year %s is %s", operation, y.Code, y.Status).
		WithDetail("academic_year", y.Code).
		WithDetail("status", string(y.Status))
	if y.Status == YearFinanciallyClosed || y.Status == YearClosed {
		err = err.WithDetail("remedy",
			"debt on a closed year is still collectible: post the payment against the current open year")
	}
	return err
}

// RequireAcademicRecording returns a precondition error unless results may be
// written.
func (y *Year) RequireAcademicRecording(operation string) error {
	if y.AcceptsAcademicRecording() {
		return nil
	}
	return shared.PreconditionFailed("academic_year.closed",
		"%s is not allowed: academic year %s is %s", operation, y.Code, y.Status).
		WithDetail("academic_year", y.Code).
		WithDetail("status", string(y.Status))
}

// Open moves a draft year into service.
func (y *Year) Open() error {
	if y.Status != YearDraft {
		return shared.PreconditionFailed("academic_year.not_draft",
			"only a draft year can be opened; %s is %s", y.Code, y.Status)
	}
	y.Status = YearOpen
	return nil
}

// CloseFinancially freezes money for the year. The caller has already verified
// there are no draft payments and that cached totals reconcile.
func (y *Year) CloseFinancially(actor shared.ID, at time.Time) error {
	if y.Status != YearOpen {
		return shared.PreconditionFailed("academic_year.not_open",
			"only an open year can be financially closed; %s is %s", y.Code, y.Status)
	}
	y.Status = YearFinanciallyClosed
	y.FinanciallyClosedAt = &at
	y.FinanciallyClosedBy = &actor
	return nil
}

// Close freezes the year completely. The caller has already verified every
// enrollment carries a recorded result.
func (y *Year) Close(actor shared.ID, at time.Time) error {
	if y.Status != YearFinanciallyClosed {
		return shared.PreconditionFailed("academic_year.not_financially_closed",
			"a year must be financially closed before it is closed; %s is %s", y.Code, y.Status)
	}
	y.Status = YearClosed
	y.ClosedAt = &at
	y.ClosedBy = &actor
	return nil
}

// OpenForAdjustment reopens a closed year for a bounded correction window.
func (y *Year) OpenForAdjustment(reason string, until time.Time) error {
	if y.Status != YearClosed && y.Status != YearFinanciallyClosed {
		return shared.PreconditionFailed("academic_year.not_closed",
			"only a closed year can be reopened for adjustment; %s is %s", y.Code, y.Status)
	}
	if reason == "" {
		return shared.Validation("academic_year.adjustment_reason_required",
			"reopening a closed year requires a written reason")
	}
	y.Status = YearAdjustmentOpen
	y.AdjustmentReason = &reason
	y.AdjustmentWindowEnds = &until
	return nil
}

// CloseAdjustmentWindow returns an adjustment-open year to closed.
func (y *Year) CloseAdjustmentWindow() error {
	if y.Status != YearAdjustmentOpen {
		return shared.PreconditionFailed("academic_year.not_adjustment_open",
			"year %s is not in an adjustment window", y.Code)
	}
	y.Status = YearClosed
	y.AdjustmentWindowEnds = nil
	return nil
}

// AdjustmentWindowExpired reports whether a reopened year has run past its
// deadline and should be closed again.
func (y *Year) AdjustmentWindowExpired(now time.Time) bool {
	return y.Status == YearAdjustmentOpen &&
		y.AdjustmentWindowEnds != nil &&
		now.After(*y.AdjustmentWindowEnds)
}

func itoa4(v int) string {
	const digits = "0123456789"
	return string([]byte{
		digits[(v/1000)%10],
		digits[(v/100)%10],
		digits[(v/10)%10],
		digits[v%10],
	})
}
