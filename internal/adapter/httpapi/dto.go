// Package httpapi is the HTTP boundary: request shapes, response shapes, and
// the routing that connects them to domain commands.
//
// Handlers here do three things and nothing else — decode, call one command,
// encode. No business rule lives in this package. A rule written in a handler
// would apply only to requests that arrive through that route, and the same
// command invoked by a bulk job or a scheduled task would skip it.
package httpapi

import (
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
)

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// LoginRequest authenticates an operator.
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	// CashierDeskID scopes a cashier to a physical window. Receipt series run
	// per year per desk, so a cashier without one cannot post cash.
	CashierDeskID *string `json:"cashier_desk_id"`
}

// RefreshRequest renews an access token.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// RegisterStudentRequest creates a student identity.
type RegisterStudentRequest struct {
	StudentNo  string  `json:"student_no" binding:"required"`
	FullName   string  `json:"full_name" binding:"required"`
	MotherName string  `json:"mother_name" binding:"required"`
	NationalID *string `json:"national_id"`
	BirthDate  *string `json:"birth_date"`
	Gender     *string `json:"gender" binding:"omitempty,oneof=male female"`
	Phone      *string `json:"phone"`
	Email      *string `json:"email" binding:"omitempty,email"`
	Address    *string `json:"address"`
	// AcknowledgeDuplicates proceeds past a probable-duplicate match. The
	// override is recorded, so a wrongly created second record for one person
	// can be traced to the decision that made it.
	AcknowledgeDuplicates bool `json:"acknowledge_duplicates"`
}

// UpdateContactRequest changes how a student is reached.
type UpdateContactRequest struct {
	Phone         *string `json:"phone"`
	PhoneAlt      *string `json:"phone_alt"`
	Email         *string `json:"email" binding:"omitempty,email"`
	Address       *string `json:"address"`
	GuardianName  *string `json:"guardian_name"`
	GuardianPhone *string `json:"guardian_phone"`
}

// EnrollStudentRequest registers a student for a year.
type EnrollStudentRequest struct {
	StudentID      string `json:"student_id" binding:"required,uuid"`
	AcademicYearID string `json:"academic_year_id" binding:"required,uuid"`
	DepartmentID   string `json:"department_id" binding:"required,uuid"`
	StudyTypeID    string `json:"study_type_id" binding:"required,uuid"`
	Stage          int16  `json:"stage" binding:"required,min=1,max=8"`
	// CategoryCode is derived when omitted: a second or later attempt at a
	// stage makes the student a repeat student, which usually prices
	// differently.
	CategoryCode         string  `json:"category_code"`
	Kind                 string  `json:"kind" binding:"omitempty,oneof=regular hosted_in transfer_in"`
	PreviousEnrollmentID *string `json:"previous_enrollment_id" binding:"omitempty,uuid"`
	OverrideDebtBlock    bool    `json:"override_debt_block"`
	OverrideReason       *string `json:"override_reason"`
}

// SupersedeEnrollmentRequest changes an enrollment's context mid-year.
type SupersedeEnrollmentRequest struct {
	NewDepartmentID *string `json:"new_department_id" binding:"omitempty,uuid"`
	NewStudyTypeID  *string `json:"new_study_type_id" binding:"omitempty,uuid"`
	NewStage        *int16  `json:"new_stage" binding:"omitempty,min=1,max=8"`
	NewCategoryCode *string `json:"new_category_code"`
	Reason          string  `json:"reason" binding:"required"`
	EffectiveDate   string  `json:"effective_date" binding:"required"`
}

// RecordResultRequest records an examination outcome.
type RecordResultRequest struct {
	Result     string `json:"result" binding:"required,oneof=passed_r1 passed_r2 failed"`
	ByDecision bool   `json:"by_decision"`
}

// ChangeStatusRequest applies a lifecycle transition.
type ChangeStatusRequest struct {
	Status   string  `json:"status" binding:"required"`
	OrderRef *string `json:"order_ref"`
	Reason   *string `json:"reason"`
	Result   string  `json:"result"`
}

// GenerateAccountRequest prices an enrollment.
type GenerateAccountRequest struct {
	EnrollmentID string  `json:"enrollment_id" binding:"required,uuid"`
	TemplateID   *string `json:"installment_template_id" binding:"omitempty,uuid"`
	// DryRun computes everything and writes nothing, so a finance manager
	// approves real numbers rather than a promise.
	DryRun bool `json:"dry_run"`
}

// RecordPaymentRequest collects money.
type RecordPaymentRequest struct {
	AccountID       string  `json:"account_id" binding:"required,uuid"`
	Amount          int64   `json:"amount" binding:"required,gt=0"`
	PaymentMethodID string  `json:"payment_method_id" binding:"required,uuid"`
	MethodReference *string `json:"method_reference"`
	PayerName       *string `json:"payer_name"`
	Notes           *string `json:"notes"`
	// TargetInstallments directs the money at specific installments. Omitted,
	// the default is oldest due date first.
	TargetInstallments []TargetAllocation `json:"target_installments"`
	// ConfirmedDistinct acknowledges the near-duplicate warning.
	ConfirmedDistinct bool `json:"confirmed_distinct"`
}

// TargetAllocation directs part of a payment at one installment.
type TargetAllocation struct {
	InstallmentID string `json:"installment_id" binding:"required,uuid"`
	Amount        int64  `json:"amount" binding:"required,gt=0"`
}

// VoidRequestBody asks for a payment to be reversed.
type VoidRequestBody struct {
	PaymentID string `json:"payment_id" binding:"required,uuid"`
	Reason    string `json:"reason" binding:"required"`
}

// RefundRequestBody asks for money to be returned.
type RefundRequestBody struct {
	PaymentID       string `json:"payment_id" binding:"required,uuid"`
	Amount          int64  `json:"amount" binding:"required,gt=0"`
	PaymentMethodID string `json:"payment_method_id" binding:"required,uuid"`
	Reason          string `json:"reason" binding:"required"`
}

// RejectRequestBody declines a pending request.
type RejectRequestBody struct {
	Reason string `json:"reason" binding:"required"`
}

// AssignDiscountRequest grants a discount.
type AssignDiscountRequest struct {
	StudentID     string   `json:"student_id" binding:"required,uuid"`
	DefinitionID  string   `json:"definition_id" binding:"required,uuid"`
	Scope         string   `json:"scope" binding:"required,oneof=single_year year_range all_years"`
	YearFromID    *string  `json:"year_from_id" binding:"omitempty,uuid"`
	YearToID      *string  `json:"year_to_id" binding:"omitempty,uuid"`
	Justification *string  `json:"justification"`
	DocumentRefs  []string `json:"document_refs"`
}

// RevokeDiscountRequest withdraws a grant.
type RevokeDiscountRequest struct {
	Reason string `json:"reason" binding:"required"`
	Effect string `json:"effect" binding:"omitempty,oneof=prospective_only include_current_year"`
}

// ConfirmApplicationRequest resolves a discount awaiting re-confirmation.
type ConfirmApplicationRequest struct {
	Confirm bool    `json:"confirm"`
	Reason  *string `json:"reason"`
}

// PostAdjustmentRequest records a signed change to what an account owes.
type PostAdjustmentRequest struct {
	AccountID string `json:"account_id" binding:"required,uuid"`
	Type      string `json:"type" binding:"required"`
	Amount    int64  `json:"amount" binding:"required"`
	Reason    string `json:"reason" binding:"required"`
}

// CreateYearRequest defines an academic year.
type CreateYearRequest struct {
	Code            string `json:"code" binding:"required"`
	StartDate       string `json:"start_date" binding:"required"`
	EndDate         string `json:"end_date" binding:"required"`
	DebtBlockPolicy string `json:"debt_block_policy" binding:"omitempty,oneof=ignore warn block"`
}

// ReopenYearRequest reopens closed books for a bounded correction.
type ReopenYearRequest struct {
	Reason      string `json:"reason" binding:"required"`
	WindowHours int    `json:"window_hours" binding:"omitempty,min=1,max=168"`
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// TokenResponse carries issued credentials.
type TokenResponse struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitempty"`
	TokenType        string    `json:"token_type"`
	User             UserView  `json:"user"`
}

// UserView is an operator as the client sees them. It deliberately has no
// password field of any kind, hashed or otherwise.
type UserView struct {
	ID       string   `json:"id"`
	Username string   `json:"username"`
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
	// MustChangePassword tells a client to send the operator straight to the
	// password form: every other route will refuse them until they do.
	MustChangePassword bool `json:"must_change_password"`
	// ScopeMode lets a client label a scoped operator's screens with the
	// colleges they can actually act on.
	ScopeMode string `json:"scope_mode,omitempty"`
}

// StudentView is a student identity.
type StudentView struct {
	ID         string  `json:"id"`
	StudentNo  string  `json:"student_no"`
	FullName   string  `json:"full_name"`
	MotherName string  `json:"mother_name"`
	NationalID *string `json:"national_id,omitempty"`
	BirthDate  *string `json:"birth_date,omitempty"`
	Gender     *string `json:"gender,omitempty"`
	Phone      *string `json:"phone,omitempty"`
	Email      *string `json:"email,omitempty"`
	Status     string  `json:"status"`
}

// EnrollmentView is a registration for one year.
type EnrollmentView struct {
	ID               string  `json:"id"`
	StudentID        string  `json:"student_id"`
	AcademicYearID   string  `json:"academic_year_id"`
	SequenceNo       int16   `json:"sequence_no"`
	CollegeID        string  `json:"college_id"`
	DepartmentID     string  `json:"department_id"`
	StudyTypeID      string  `json:"study_type_id"`
	Stage            int16   `json:"stage"`
	AttemptNumber    int16   `json:"attempt_number"`
	Kind             string  `json:"kind"`
	Status           string  `json:"status"`
	Result           string  `json:"result"`
	ResultByDecision bool    `json:"result_by_decision"`
	SupersedesID     *string `json:"supersedes_id,omitempty"`
	SupersedeReason  *string `json:"supersede_reason,omitempty"`
	IsRepeat         bool    `json:"is_repeat"`
}

// AccountView is a financial position.
//
// Both the frozen net and the effective net are exposed. A client showing only
// one cannot explain the difference, and the difference — the adjustments — is
// exactly what a student querying their balance is asking about.
type AccountView struct {
	ID               string       `json:"id"`
	EnrollmentID     string       `json:"enrollment_id"`
	StudentID        string       `json:"student_id"`
	AcademicYearID   string       `json:"academic_year_id"`
	Stage            int16        `json:"stage"`
	Status           string       `json:"status"`
	GrossTotal       money.Amount `json:"gross_total"`
	DiscountableBase money.Amount `json:"discountable_base"`
	DiscountTotal    money.Amount `json:"discount_total"`
	NetSnapshot      money.Amount `json:"net_snapshot"`
	AdjustmentTotal  money.Amount `json:"adjustment_total"`
	EffectiveNet     money.Amount `json:"effective_net"`
	PaidTotal        money.Amount `json:"paid_total"`
	RefundedTotal    money.Amount `json:"refunded_total"`
	NetPaid          money.Amount `json:"net_paid"`
	CreditBalance    money.Amount `json:"credit_balance"`
	Remaining        money.Amount `json:"remaining"`
}

// SnapshotLineView is one frozen fee component.
type SnapshotLineView struct {
	ComponentCode  string       `json:"component_code"`
	NameAr         string       `json:"name_ar"`
	Amount         money.Amount `json:"amount"`
	IsDiscountable bool         `json:"is_discountable"`
	IsRefundable   bool         `json:"is_refundable"`
}

// InstallmentView is one scheduled obligation. The overdue flag is computed
// for the request, never read from a column.
type InstallmentView struct {
	ID         string       `json:"id"`
	Number     int16        `json:"number"`
	DueDate    string       `json:"due_date"`
	Amount     money.Amount `json:"amount"`
	PaidAmount money.Amount `json:"paid_amount"`
	Remaining  money.Amount `json:"remaining"`
	Status     string       `json:"status"`
	IsOverdue  bool         `json:"is_overdue"`
}

// PaymentView is a collection.
type PaymentView struct {
	ID              string       `json:"id"`
	ReceiptNo       *string      `json:"receipt_no,omitempty"`
	AccountID       string       `json:"account_id"`
	StudentID       string       `json:"student_id"`
	Amount          money.Amount `json:"amount"`
	PaymentMethodID string       `json:"payment_method_id"`
	MethodReference *string      `json:"method_reference,omitempty"`
	Status          string       `json:"status"`
	PaidAt          time.Time    `json:"paid_at"`
	PostedAt        *time.Time   `json:"posted_at,omitempty"`
	VoidedAt        *time.Time   `json:"voided_at,omitempty"`
	VoidReason      *string      `json:"void_reason,omitempty"`
	PayerName       *string      `json:"payer_name,omitempty"`
}

// RecordPaymentResponse is what a cashier's terminal prints from.
type RecordPaymentResponse struct {
	Payment      PaymentView      `json:"payment"`
	Allocations  []AllocationView `json:"allocations"`
	CreditAmount money.Amount     `json:"credit_amount"`
	Remaining    money.Amount     `json:"remaining"`
	// Duplicate marks a replayed idempotency key. The terminal has already
	// printed this receipt once and must not print it again.
	Duplicate bool `json:"duplicate"`
}

// AllocationView is one line of a payment's distribution.
type AllocationView struct {
	InstallmentID string       `json:"installment_id"`
	Amount        money.Amount `json:"amount"`
}

// RefundView is money returned.
type RefundView struct {
	ID          string       `json:"id"`
	RefundNo    *string      `json:"refund_no,omitempty"`
	PaymentID   string       `json:"payment_id"`
	AccountID   string       `json:"account_id"`
	Amount      money.Amount `json:"amount"`
	Reason      string       `json:"reason"`
	Status      string       `json:"status"`
	RequestedAt time.Time    `json:"requested_at"`
	PostedAt    *time.Time   `json:"posted_at,omitempty"`
}

// VoidRequestView is a pending or executed void.
type VoidRequestView struct {
	ID          string     `json:"id"`
	PaymentID   string     `json:"payment_id"`
	Reason      string     `json:"reason"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	ExecutedAt  *time.Time `json:"executed_at,omitempty"`
}

// DiscountApplicationView is a discount materialised on an account, carrying
// both what it computed to and what was actually applied. A truncated grant
// keeps both figures and names the reason, so nothing is reduced silently.
type DiscountApplicationView struct {
	ID               string       `json:"id"`
	AssignmentID     string       `json:"assignment_id"`
	FrozenBase       money.Amount `json:"frozen_base"`
	ComputedAmount   money.Amount `json:"computed_amount"`
	AppliedAmount    money.Amount `json:"applied_amount"`
	TruncationReason *string      `json:"truncation_reason,omitempty"`
	Status           string       `json:"status"`
}

// AssignmentView is a discount granted to a student.
type AssignmentView struct {
	ID            string  `json:"id"`
	StudentID     string  `json:"student_id"`
	DefinitionID  string  `json:"definition_id"`
	ScopeType     string  `json:"scope_type"`
	ScopeCodeFrom *string `json:"scope_year_from,omitempty"`
	ScopeCodeTo   *string `json:"scope_year_to,omitempty"`
	Status        string  `json:"status"`
}

// YearView is an academic year.
type YearView struct {
	Code            string     `json:"code"`
	ID              string     `json:"id"`
	StartDate       string     `json:"start_date"`
	EndDate         string     `json:"end_date"`
	Status          string     `json:"status"`
	DebtBlockPolicy string     `json:"debt_block_policy"`
	AcceptsMoney    bool       `json:"accepts_financial_posting"`
	AcceptsResults  bool       `json:"accepts_academic_recording"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
}

// AccountDetailView is the full financial picture for one enrollment.
type AccountDetailView struct {
	Account      AccountView               `json:"account"`
	Snapshot     []SnapshotLineView        `json:"fee_components"`
	Discounts    []DiscountApplicationView `json:"discounts"`
	Installments []InstallmentView         `json:"installments"`
	Payments     []PaymentView             `json:"payments"`
	Refunds      []RefundView              `json:"refunds"`
}

// ---------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------

func toStudentView(s *student.Student) StudentView {
	v := StudentView{
		ID:         s.ID.String(),
		StudentNo:  s.StudentNo,
		FullName:   s.FullName,
		MotherName: s.MotherName,
		NationalID: s.NationalID,
		Phone:      s.Phone,
		Email:      s.Email,
		Status:     string(s.Status),
	}
	if s.BirthDate != nil {
		v.BirthDate = ptrString(s.BirthDate.String())
	}
	if s.Gender != nil {
		v.Gender = ptrString(string(*s.Gender))
	}
	return v
}

func toEnrollmentView(e *academic.Enrollment) EnrollmentView {
	v := EnrollmentView{
		ID:               e.ID.String(),
		StudentID:        e.StudentID.String(),
		AcademicYearID:   e.AcademicYearID.String(),
		SequenceNo:       e.SequenceNo,
		CollegeID:        e.CollegeID.String(),
		DepartmentID:     e.DepartmentID.String(),
		StudyTypeID:      e.StudyTypeID.String(),
		Stage:            e.Stage,
		AttemptNumber:    e.AttemptNumber,
		Kind:             string(e.Kind),
		Status:           string(e.Status),
		Result:           string(e.Result),
		ResultByDecision: e.ResultByDecision,
		SupersedeReason:  e.SupersedeReason,
		IsRepeat:         e.AttemptNumber > 1,
	}
	if e.SupersedesID != nil {
		v.SupersedesID = ptrString(e.SupersedesID.String())
	}
	return v
}

func toAccountView(a *billing.Account) AccountView {
	return AccountView{
		ID:               a.ID.String(),
		EnrollmentID:     a.EnrollmentID.String(),
		StudentID:        a.StudentID.String(),
		AcademicYearID:   a.AcademicYearID.String(),
		Stage:            a.Stage,
		Status:           string(a.Status),
		GrossTotal:       a.GrossTotal,
		DiscountableBase: a.DiscountableBase,
		DiscountTotal:    a.DiscountTotal,
		NetSnapshot:      a.NetTotal,
		AdjustmentTotal:  a.AdjustmentTotal,
		EffectiveNet:     a.EffectiveNet(),
		PaidTotal:        a.PaidTotal,
		RefundedTotal:    a.RefundedTotal,
		NetPaid:          a.NetPaid(),
		CreditBalance:    a.CreditBalance,
		Remaining:        a.Remaining(),
	}
}

func toSnapshotViews(lines []*billing.SnapshotLine) []SnapshotLineView {
	out := make([]SnapshotLineView, 0, len(lines))
	for _, l := range lines {
		out = append(out, SnapshotLineView{
			ComponentCode:  l.ComponentCode,
			NameAr:         l.NameAr,
			Amount:         l.Amount,
			IsDiscountable: l.IsDiscountable,
			IsRefundable:   l.IsRefundable,
		})
	}
	return out
}

func toInstallmentViews(installments []*billing.Installment, today shared.Date) []InstallmentView {
	out := make([]InstallmentView, 0, len(installments))
	for _, i := range installments {
		out = append(out, InstallmentView{
			ID:         i.ID.String(),
			Number:     i.Number,
			DueDate:    i.DueDate.String(),
			Amount:     i.Amount,
			PaidAmount: i.PaidAmount,
			Remaining:  i.Remaining(),
			Status:     string(i.Status),
			IsOverdue:  i.IsOverdue(today),
		})
	}
	return out
}

func toPaymentView(p *payment.Payment) PaymentView {
	return PaymentView{
		ID:              p.ID.String(),
		ReceiptNo:       p.ReceiptNo,
		AccountID:       p.AccountID.String(),
		StudentID:       p.StudentID.String(),
		Amount:          p.Amount,
		PaymentMethodID: p.PaymentMethodID.String(),
		MethodReference: p.MethodReference,
		Status:          string(p.Status),
		PaidAt:          p.PaidAt,
		PostedAt:        p.PostedAt,
		VoidedAt:        p.VoidedAt,
		VoidReason:      p.VoidReason,
		PayerName:       p.PayerName,
	}
}

func toPaymentViews(payments []*payment.Payment) []PaymentView {
	out := make([]PaymentView, 0, len(payments))
	for _, p := range payments {
		out = append(out, toPaymentView(p))
	}
	return out
}

func toRefundView(r *payment.Refund) RefundView {
	return RefundView{
		ID:          r.ID.String(),
		RefundNo:    r.RefundNo,
		PaymentID:   r.PaymentID.String(),
		AccountID:   r.AccountID.String(),
		Amount:      r.Amount,
		Reason:      r.Reason,
		Status:      string(r.Status),
		RequestedAt: r.RequestedAt,
		PostedAt:    r.PostedAt,
	}
}

func toRefundViews(refunds []*payment.Refund) []RefundView {
	out := make([]RefundView, 0, len(refunds))
	for _, r := range refunds {
		out = append(out, toRefundView(r))
	}
	return out
}

func toVoidRequestView(v *payment.VoidRequest) VoidRequestView {
	return VoidRequestView{
		ID:          v.ID.String(),
		PaymentID:   v.PaymentID.String(),
		Reason:      v.Reason,
		Status:      string(v.Status),
		RequestedAt: v.RequestedAt,
		ExecutedAt:  v.ExecutedAt,
	}
}

func toApplicationViews(applications []*discount.Application) []DiscountApplicationView {
	out := make([]DiscountApplicationView, 0, len(applications))
	for _, a := range applications {
		view := DiscountApplicationView{
			ID:             a.ID.String(),
			AssignmentID:   a.AssignmentID.String(),
			FrozenBase:     a.FrozenBase,
			ComputedAmount: a.ComputedAmount,
			AppliedAmount:  a.AppliedAmount,
			Status:         string(a.Status),
		}
		if a.TruncationReason != nil {
			view.TruncationReason = ptrString(string(*a.TruncationReason))
		}
		out = append(out, view)
	}
	return out
}

func toAssignmentView(a *discount.Assignment) AssignmentView {
	return AssignmentView{
		ID:            a.ID.String(),
		StudentID:     a.StudentID.String(),
		DefinitionID:  a.DefinitionID.String(),
		ScopeType:     string(a.ScopeType),
		ScopeCodeFrom: a.ScopeCodeFrom,
		ScopeCodeTo:   a.ScopeCodeTo,
		Status:        string(a.Status),
	}
}

func toYearView(y *academic.Year) YearView {
	return YearView{
		ID:              y.ID.String(),
		Code:            y.Code,
		StartDate:       y.StartDate.String(),
		EndDate:         y.EndDate.String(),
		Status:          string(y.Status),
		DebtBlockPolicy: string(y.DebtBlockPolicy),
		AcceptsMoney:    y.AcceptsFinancialPosting(),
		AcceptsResults:  y.AcceptsAcademicRecording(),
		ClosedAt:        y.ClosedAt,
	}
}

func ptrString(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Operator administration
// ---------------------------------------------------------------------------

// CreateUserRequest registers an operator.
type CreateUserRequest struct {
	Username string   `json:"username" binding:"required"`
	FullName string   `json:"full_name" binding:"required"`
	Email    *string  `json:"email" binding:"omitempty,email"`
	Roles    []string `json:"roles"`
	// Password may be omitted, in which case the server generates one and
	// returns it exactly once. Preferable: an administrator inventing
	// passwords for a hall of cashiers invents the same one.
	Password string `json:"password"`
	// ScopeMode is "university" (the default) or "scoped".
	ScopeMode   string   `json:"scope_mode" binding:"omitempty,oneof=university scoped"`
	Colleges    []string `json:"colleges"`
	Departments []string `json:"departments"`
}

// SetRolesRequest replaces an operator's roles.
type SetRolesRequest struct {
	Roles  []string `json:"roles"`
	Reason string   `json:"reason"`
}

// SetScopeRequest replaces an operator's organisational grants.
type SetScopeRequest struct {
	ScopeMode   string   `json:"scope_mode" binding:"required,oneof=university scoped"`
	Colleges    []string `json:"colleges"`
	Departments []string `json:"departments"`
	Reason      string   `json:"reason"`
}

// ResetPasswordRequest issues a new credential on somebody's behalf.
type ResetPasswordRequest struct {
	// Password may be omitted for a generated one.
	Password string `json:"password"`
	Reason   string `json:"reason"`
}

// ChangePasswordRequest replaces the caller's own password.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required"`
	// KeepOtherSessions leaves other sessions alive. The default ends them,
	// because the usual reason for changing a password is suspecting somebody
	// else has it.
	KeepOtherSessions bool `json:"keep_other_sessions"`
}

// ReasonRequest is the body of a command whose only input is why.
type ReasonRequest struct {
	Reason string `json:"reason"`
}

// CreateUserResponse carries the account and, when the server generated it,
// the temporary password.
type CreateUserResponse struct {
	User UserDetailView `json:"user"`
	// TemporaryPassword appears only when the server generated it, and only in
	// this one response. It is never readable again.
	TemporaryPassword string `json:"temporary_password,omitempty"`
}

// UserDetailView is an operator as an administrator sees them. No password
// field of any kind, hashed or otherwise.
type UserDetailView struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	FullName           string     `json:"full_name"`
	Email              *string    `json:"email,omitempty"`
	Roles              []string   `json:"roles"`
	IsActive           bool       `json:"is_active"`
	MustChangePassword bool       `json:"must_change_password"`
	ScopeMode          string     `json:"scope_mode"`
	Colleges           []string   `json:"colleges,omitempty"`
	Departments        []string   `json:"departments,omitempty"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	LockedUntil        *time.Time `json:"locked_until,omitempty"`
	DisabledReason     *string    `json:"disabled_reason,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	// CashierDeskID and SessionID are set only on /auth/me, where the client
	// needs to know which desk it signed in at and which session it holds.
	CashierDeskID *string `json:"cashier_desk_id,omitempty"`
	SessionID     string  `json:"session_id,omitempty"`
}

// SessionView is one sign-in.
type SessionView struct {
	ID            string     `json:"id"`
	IssuedAt      time.Time  `json:"issued_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokedReason *string    `json:"revoked_reason,omitempty"`
	IPAddress     *string    `json:"ip_address,omitempty"`
	UserAgent     *string    `json:"user_agent,omitempty"`
	// Current marks the session the request arrived on, so a client can label
	// it rather than inviting somebody to revoke the session they are using.
	Current bool `json:"current"`
}

// LoginAttemptView is one sign-in attempt. It never carries the password, its
// length, or which half of the credential was wrong.
type LoginAttemptView struct {
	Username    string    `json:"username"`
	Succeeded   bool      `json:"succeeded"`
	FailureCode *string   `json:"failure_code,omitempty"`
	IPAddress   *string   `json:"ip_address,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}
