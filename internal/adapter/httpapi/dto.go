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

	"flowed/internal/domain/academic"
	"flowed/internal/domain/billing"
	"flowed/internal/domain/discount"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/domain/student"
	"flowed/internal/port"
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

// RegisterStudentWithPlacementRequest is identity registration extended with
// the academic placement — and, when the actor holds finance authority, the
// pricing that follows from it — in one submission.
type RegisterStudentWithPlacementRequest struct {
	RegisterStudentRequest

	AcademicYearID string `json:"academic_year_id" binding:"required,uuid"`
	DepartmentID   string `json:"department_id" binding:"required,uuid"`
	StudyTypeID    string `json:"study_type_id" binding:"required,uuid"`
	Stage          int16  `json:"stage" binding:"required,min=1,max=5"`
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
	Stage          int16  `json:"stage" binding:"required,min=1,max=5"`
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
	NewStage        *int16  `json:"new_stage" binding:"omitempty,min=1,max=5"`
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

	// FinancialTreatment is required for deferral, withdrawal, dropout and
	// transfer out. There is no default: charging a student who withdrew and
	// waiving what they owe are both defensible, and only the university can
	// choose. Omitting it is refused with the options named.
	FinancialTreatment string `json:"financial_treatment" binding:"omitempty,oneof=keep waive_unpaid waive_all partial"`
	// ChargeInstead is the amount to charge under the "partial" treatment.
	ChargeInstead *int64 `json:"charge_instead"`
	// GraduationOverrideReason clears a debtor whose year blocks graduation.
	// Recorded by name against the clearance decision.
	GraduationOverrideReason string `json:"graduation_override_reason"`
}

// ChangeStatusResponse is the enrollment plus what the change did to the money.
type ChangeStatusResponse struct {
	Enrollment EnrollmentView `json:"enrollment"`
	Treatment  *TreatmentView `json:"financial_treatment,omitempty"`
	Clearance  *ClearanceView `json:"graduation_clearance,omitempty"`
}

// TreatmentView says what a status change did to the account.
type TreatmentView struct {
	Treatment           string  `json:"treatment"`
	Applied             bool    `json:"applied"`
	Waived              int64   `json:"waived"`
	CreditRaised        int64   `json:"credit_raised"`
	RemainingObligation int64   `json:"remaining_obligation"`
	AccountID           *string `json:"account_id,omitempty"`
}

// ClearanceView is a graduation clearance decision (براءة الذمة).
type ClearanceView struct {
	Cleared        bool      `json:"cleared"`
	Policy         string    `json:"policy"`
	Outstanding    int64     `json:"outstanding"`
	OverrideReason *string   `json:"override_reason,omitempty"`
	DecidedAt      time.Time `json:"decided_at"`
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
	ID            string  `json:"id"`
	StudentNo     string  `json:"student_no"`
	FullName      string  `json:"full_name"`
	MotherName    string  `json:"mother_name"`
	BirthDate     *string `json:"birth_date,omitempty"`
	Gender        *string `json:"gender,omitempty"`
	Phone         *string `json:"phone,omitempty"`
	PhoneAlt      *string `json:"phone_alt,omitempty"`
	Email         *string `json:"email,omitempty"`
	Address       *string `json:"address,omitempty"`
	GuardianName  *string `json:"guardian_name,omitempty"`
	GuardianPhone *string `json:"guardian_phone,omitempty"`
	Status        string  `json:"status"`

	// Current* is a read-time convenience from the student's most recent
	// non-superseded enrollment, not a stored student attribute — study type
	// and stage still live only on enrollment (§ "the enrollment, not the
	// student"). Absent when the student has no non-superseded enrollment.
	CurrentStudyTypeID    *string `json:"current_study_type_id,omitempty"`
	CurrentStudyTypeCode  *string `json:"current_study_type_code,omitempty"`
	CurrentStage          *int16  `json:"current_stage,omitempty"`
	CurrentAcademicYearID *string `json:"current_academic_year_id,omitempty"`
}

// RegisterStudentWithPlacementView is the outcome of combined student intake:
// the identity, the enrollment it was placed into, and — when pricing
// happened — the account it was priced onto.
type RegisterStudentWithPlacementView struct {
	Student    StudentView    `json:"student"`
	Enrollment EnrollmentView `json:"enrollment"`
	Account    *AccountView   `json:"account,omitempty"`
	// PricingPending and PricingNote explain a nil Account: the actor may
	// lack finance authority, or no fee policy matched this scope. See
	// app.RegisterStudentWithPlacementResult.
	PricingPending bool   `json:"pricing_pending"`
	PricingNote    string `json:"pricing_note,omitempty"`
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
		ID:            s.ID.String(),
		StudentNo:     s.StudentNo,
		FullName:      s.FullName,
		MotherName:    s.MotherName,
		Phone:         s.Phone,
		PhoneAlt:      s.PhoneAlt,
		Email:         s.Email,
		Address:       s.Address,
		GuardianName:  s.GuardianName,
		GuardianPhone: s.GuardianPhone,
		Status:        string(s.Status),
	}
	if s.BirthDate != nil {
		v.BirthDate = ptrString(s.BirthDate.String())
	}
	if s.Gender != nil {
		v.Gender = ptrString(string(*s.Gender))
	}
	return v
}

// applyCurrentEnrollment attaches the read-time enrollment summary to a
// student view. Kept separate from toStudentView because the summary comes
// from a second, batched query — see port.CurrentEnrollmentSummary.
func applyCurrentEnrollment(v *StudentView, summary port.CurrentEnrollmentSummary) {
	v.CurrentStudyTypeID = ptrString(summary.StudyTypeID.String())
	v.CurrentStudyTypeCode = ptrString(summary.StudyTypeCode)
	stage := summary.Stage
	v.CurrentStage = &stage
	v.CurrentAcademicYearID = ptrString(summary.AcademicYearID.String())
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

func ptrInt64(v int64) *int64 { return &v }

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

// ---------------------------------------------------------------------------
// Hosting, identity, merge and plan adjustment
// ---------------------------------------------------------------------------

// RegisterHostingRequest records a hosting agreement over an enrollment.
type RegisterHostingRequest struct {
	EnrollmentID string `json:"enrollment_id" binding:"required,uuid"`
	Direction    string `json:"direction" binding:"required,oneof=incoming outgoing"`

	HomeUniversity  *string `json:"home_university"`
	HomeCollege     *string `json:"home_college"`
	HomeDepartment  *string `json:"home_department"`
	HomeStudyTypeID *string `json:"home_study_type_id" binding:"omitempty,uuid"`

	HostUniversity  *string `json:"host_university"`
	HostCollege     *string `json:"host_college"`
	HostDepartment  *string `json:"host_department"`
	HostStudyTypeID *string `json:"host_study_type_id" binding:"omitempty,uuid"`

	// FeeCollector is which institution collects tuition under this agreement.
	// Deliberately per-agreement: Iraqi practice varies, and the design refuses
	// to assume one answer in code.
	FeeCollector string  `json:"fee_collector" binding:"omitempty,oneof=home host split"`
	PeriodFrom   *string `json:"period_from"`
	PeriodTo     *string `json:"period_to"`
	AgreementRef *string `json:"agreement_ref"`
	Notes        *string `json:"notes"`
}

// UpdateHostingRequest amends an agreement.
type UpdateHostingRequest struct {
	FeeCollector *string `json:"fee_collector" binding:"omitempty,oneof=home host split"`
	PeriodTo     *string `json:"period_to"`
	AgreementRef *string `json:"agreement_ref"`
	Notes        *string `json:"notes"`
	Reason       string  `json:"reason" binding:"required"`
}

// HostingView is a hosting agreement.
type HostingView struct {
	ID                    string  `json:"id"`
	EnrollmentID          string  `json:"enrollment_id"`
	Direction             string  `json:"direction"`
	HomeUniversity        *string `json:"home_university,omitempty"`
	HomeCollege           *string `json:"home_college,omitempty"`
	HomeDepartment        *string `json:"home_department,omitempty"`
	HostUniversity        *string `json:"host_university,omitempty"`
	HostCollege           *string `json:"host_college,omitempty"`
	HostDepartment        *string `json:"host_department,omitempty"`
	FeeCollector          string  `json:"fee_collector"`
	GeneratesLocalAccount bool    `json:"generates_local_account"`
	PeriodFrom            *string `json:"period_from,omitempty"`
	PeriodTo              *string `json:"period_to,omitempty"`
	AgreementRef          *string `json:"agreement_ref,omitempty"`
}

// RecordIdentityChangeRequest documents a court-ordered identity change.
type RecordIdentityChangeRequest struct {
	FullName          *string `json:"full_name"`
	MotherName        *string `json:"mother_name"`
	BirthDate         *string `json:"birth_date"`
	CourtDecisionNo   string  `json:"court_decision_no" binding:"required"`
	CourtDecisionDate string  `json:"court_decision_date" binding:"required"`
	EffectiveFrom     string  `json:"effective_from"`
	DocumentRef       *string `json:"document_ref"`
	Reason            string  `json:"reason" binding:"required"`
}

// MergeStudentsRequest folds one duplicate record into another.
type MergeStudentsRequest struct {
	SourceStudentID string `json:"source_student_id" binding:"required,uuid"`
	Reason          string `json:"reason" binding:"required"`
	// AcknowledgeDifferentIdentity confirms a merge whose two records disagree
	// on name or mother's name. Merging two people is far worse than leaving
	// two records for one, so it must be said explicitly.
	AcknowledgeDifferentIdentity bool `json:"acknowledge_different_identity"`
}

// MergeStudentsResponse reports what moved.
type MergeStudentsResponse struct {
	SourceStudentID  string `json:"source_student_id"`
	TargetStudentID  string `json:"target_student_id"`
	EnrollmentsMoved int16  `json:"enrollments_moved"`
	AccountsMoved    int16  `json:"accounts_moved"`
	DiscountsMoved   int16  `json:"discounts_moved"`
}

// AdjustPlanRequest reschedules or re-splits an installment plan.
type AdjustPlanRequest struct {
	Kind   string `json:"kind" binding:"required,oneof=reschedule resplit"`
	Reason string `json:"reason" binding:"required"`
	// DueDates maps installment id to its new due date, for a reschedule.
	DueDates map[string]string `json:"due_dates"`
	// Shares divides the unpaid remainder, for a resplit.
	Shares     []PlanShareRequest `json:"shares"`
	TemplateID *string            `json:"installment_template_id" binding:"omitempty,uuid"`
}

// PlanShareRequest is one share of a resplit remainder.
type PlanShareRequest struct {
	ShareBP       int32   `json:"share_bp" binding:"required"`
	DueOffsetDays int     `json:"due_offset_days"`
	Label         *string `json:"label"`
}

// AdjustPlanResponse is the plan after the change.
type AdjustPlanResponse struct {
	Installments []InstallmentView `json:"installments"`
	Revision     *PlanRevisionView `json:"revision,omitempty"`
}

// PlanRevisionView is one recorded change to a schedule.
type PlanRevisionView struct {
	ID                 string    `json:"id"`
	Kind               string    `json:"kind"`
	Reason             string    `json:"reason"`
	PlanVersion        int16     `json:"plan_version"`
	InstallmentsBefore int16     `json:"installments_before"`
	InstallmentsAfter  int16     `json:"installments_after"`
	UnpaidBefore       int64     `json:"unpaid_before"`
	UnpaidAfter        int64     `json:"unpaid_after"`
	CreatedAt          time.Time `json:"created_at"`
}

// IdentityVersionView is one frozen version of a person's legal identity.
//
// Kept because Iraqi courts change names and civil-registry details, and a
// certificate issued before the change must keep naming the person as it named
// them. Nothing here is editable.
type IdentityVersionView struct {
	VersionNo         int32     `json:"version_no"`
	FullName          string    `json:"full_name"`
	MotherName        string    `json:"mother_name"`
	EffectiveFrom     *string   `json:"effective_from,omitempty"`
	CourtDecisionNo   *string   `json:"court_decision_no,omitempty"`
	CourtDecisionDate *string   `json:"court_decision_date,omitempty"`
	Reason            string    `json:"reason"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// ---------------------------------------------------------------------------
// Master data administration
// ---------------------------------------------------------------------------

// UpdateMasterRequest renames a reference row or retires it.
//
// Codes are absent on purpose. They travel into receipt numbers, fee-policy
// scopes and ministry returns, so a code that changes meaning is worse than one
// that is merely ugly: retire the row and create a new one instead.
type UpdateMasterRequest struct {
	NameAr   *string `json:"name_ar"`
	NameEn   *string `json:"name_en"`
	IsActive *bool   `json:"is_active"`
	Reason   string  `json:"reason"`
}

// UpdateDepartmentRequest additionally changes how many years a programme runs.
type UpdateDepartmentRequest struct {
	NameAr *string `json:"name_ar"`
	NameEn *string `json:"name_en"`
	// StageCount cannot be reduced below a stage students are registered in:
	// the final stage decides who is a graduate.
	StageCount *int16 `json:"stage_count"`
	IsActive   *bool  `json:"is_active"`
	Reason     string `json:"reason"`
}

// UpdateStudyTypeRequest renames, reorders or retires a study type.
type UpdateStudyTypeRequest struct {
	NameAr    *string `json:"name_ar"`
	NameEn    *string `json:"name_en"`
	SortOrder *int16  `json:"sort_order"`
	IsActive  *bool   `json:"is_active"`
	Reason    string  `json:"reason"`
}

// CreateStudentCategoryRequest adds a category fee policy can resolve against.
type CreateStudentCategoryRequest struct {
	Code   string  `json:"code" binding:"required"`
	NameAr string  `json:"name_ar" binding:"required"`
	NameEn *string `json:"name_en"`
}

// StudentCategoryView is a category.
type StudentCategoryView struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	NameAr   string `json:"name_ar"`
	IsActive bool   `json:"is_active"`
}

// CreatePaymentMethodRequest adds a way of paying.
type CreatePaymentMethodRequest struct {
	Code   string `json:"code" binding:"required"`
	NameAr string `json:"name_ar" binding:"required"`
	// IsCash puts the collection in a drawer that is counted at shift close.
	IsCash bool `json:"is_cash"`
	// RequiresReference demands the bank or terminal reference without which
	// the collection cannot be matched against a statement.
	RequiresReference bool  `json:"requires_reference"`
	SortOrder         int16 `json:"sort_order"`
}

// UpdatePaymentMethodRequest renames a method, changes its rules or retires it.
type UpdatePaymentMethodRequest struct {
	NameAr *string `json:"name_ar"`
	// IsCash is refused once payments exist: it decides what a drawer was
	// expected to hold, and changing it would unbalance a shift that balanced.
	IsCash            *bool  `json:"is_cash"`
	RequiresReference *bool  `json:"requires_reference"`
	IsActive          *bool  `json:"is_active"`
	SortOrder         *int16 `json:"sort_order"`
	Reason            string `json:"reason"`
}

// PaymentMethodView is a way of paying.
type PaymentMethodView struct {
	ID                string `json:"id"`
	Code              string `json:"code"`
	NameAr            string `json:"name_ar"`
	IsCash            bool   `json:"is_cash"`
	RequiresReference bool   `json:"requires_reference"`
	IsActive          bool   `json:"is_active"`
}

// CreateCashierDeskRequest opens a window money can be taken at.
type CreateCashierDeskRequest struct {
	// Code appears inside every receipt number the desk issues
	// (2025-D03-000917), so it is short and fixed for the desk's life.
	Code      string  `json:"code" binding:"required"`
	NameAr    string  `json:"name_ar" binding:"required"`
	CollegeID *string `json:"college_id" binding:"omitempty,uuid"`
}

// UpdateCashierDeskRequest renames a desk or closes it.
type UpdateCashierDeskRequest struct {
	NameAr    *string `json:"name_ar"`
	CollegeID *string `json:"college_id" binding:"omitempty,uuid"`
	IsActive  *bool   `json:"is_active"`
	Reason    string  `json:"reason"`
}

// ---------------------------------------------------------------------------
// Settlement reconciliation
// ---------------------------------------------------------------------------

// SettlementImportView is the outcome of importing a statement.
type SettlementImportView struct {
	Batch SettlementBatchView  `json:"batch"`
	Lines []SettlementLineView `json:"lines"`
	// Exceptions counts the lines a person still has to work. Zero is what a
	// clean reconciliation looks like.
	Exceptions int `json:"exceptions"`
}

// SettlementBatchView is one imported statement.
type SettlementBatchView struct {
	ID            string       `json:"id"`
	SourceCode    string       `json:"source_code"`
	SourceName    *string      `json:"source_name,omitempty"`
	Filename      string       `json:"filename"`
	StatementFrom *string      `json:"statement_from,omitempty"`
	StatementTo   *string      `json:"statement_to,omitempty"`
	Status        string       `json:"status"`
	LineCount     int          `json:"line_count"`
	MatchedCount  int          `json:"matched_count"`
	TotalAmount   money.Amount `json:"total_amount"`
	MatchedAmount money.Amount `json:"matched_amount"`
}

// SettlementBatchDetailView is a statement with its lines.
type SettlementBatchDetailView struct {
	Batch SettlementBatchView  `json:"batch"`
	Lines []SettlementLineView `json:"lines"`
}

// SettlementLineView is one row of a statement and what it matched.
type SettlementLineView struct {
	ID               string       `json:"id"`
	LineNo           int          `json:"line_no"`
	ExternalRef      string       `json:"external_ref"`
	Amount           money.Amount `json:"amount"`
	ValueDate        *string      `json:"value_date,omitempty"`
	Description      string       `json:"description,omitempty"`
	Status           string       `json:"match_status"`
	MatchedPaymentID *string      `json:"matched_payment_id,omitempty"`
	// Variance is positive when the bank received more than the receipt says.
	Variance   money.Amount `json:"variance"`
	ReviewNote *string      `json:"review_note,omitempty"`
}

// SettlementExceptionView is a line that did not settle cleanly.
type SettlementExceptionView struct {
	LineID      string       `json:"line_id"`
	BatchID     string       `json:"batch_id"`
	SourceCode  string       `json:"source_code"`
	Filename    string       `json:"filename"`
	LineNo      int          `json:"line_no"`
	ExternalRef *string      `json:"external_ref,omitempty"`
	Amount      money.Amount `json:"amount"`
	ValueDate   *string      `json:"value_date,omitempty"`
	Status      string       `json:"match_status"`
	Variance    money.Amount `json:"variance"`
	PaymentID   *string      `json:"payment_id,omitempty"`
}

// UnconfirmedPaymentView is a posted non-cash collection no statement confirms.
type UnconfirmedPaymentView struct {
	PaymentID  string       `json:"payment_id"`
	ReceiptNo  *string      `json:"receipt_no,omitempty"`
	StudentID  string       `json:"student_id"`
	Amount     money.Amount `json:"amount"`
	Reference  *string      `json:"method_reference,omitempty"`
	MethodCode string       `json:"method_code"`
	PaidAt     time.Time    `json:"paid_at"`
}

// ResolveSettlementLineRequest is a person's decision about a line.
type ResolveSettlementLineRequest struct {
	// Status is "matched" or "ignored". A finding — unmatched, duplicate — is
	// not a resolution and is refused.
	Status string `json:"status" binding:"required,oneof=matched ignored"`
	// PaymentID is required when matching by hand. Without it, "matched" is
	// indistinguishable from clearing the queue.
	PaymentID *string `json:"payment_id" binding:"omitempty,uuid"`
	Note      string  `json:"note"`
}

// ---------------------------------------------------------------------------
// Electronic collection
// ---------------------------------------------------------------------------

// InitiatePaymentRequest begins a collection at a provider.
type InitiatePaymentRequest struct {
	AccountID string `json:"account_id" binding:"required,uuid"`
	Provider  string `json:"provider" binding:"required"`
	// Amount may be omitted, which pays the outstanding balance. Making the
	// client compute it invites a stale figure.
	Amount *int64 `json:"amount"`
	// ReturnURL is where the provider sends the payer's browser afterwards.
	ReturnURL string `json:"return_url"`
}

// PaymentIntentView is a collection begun at a provider.
type PaymentIntentView struct {
	ID          string       `json:"id"`
	Provider    string       `json:"provider"`
	AccountID   string       `json:"account_id"`
	Amount      money.Amount `json:"amount"`
	Status      string       `json:"status"`
	ProviderRef *string      `json:"provider_ref,omitempty"`
	RedirectURL *string      `json:"redirect_url,omitempty"`
	// Instruction is what to tell the payer when there is no redirect: the
	// reference to quote at a bank counter.
	Instruction string     `json:"instruction,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	PaymentID   *string    `json:"payment_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// PaymentProviderView is a channel this deployment offers.
type PaymentProviderView struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
}

// ---------------------------------------------------------------------------
// Sponsors and scholarships
// ---------------------------------------------------------------------------

// CreateSponsorRequest registers a body that pays students' fees.
type CreateSponsorRequest struct {
	Code        string  `json:"code" binding:"required"`
	NameAr      string  `json:"name_ar" binding:"required"`
	NameEn      *string `json:"name_en"`
	SponsorType string  `json:"sponsor_type" binding:"omitempty,oneof=ministry government company charity individual other"`

	ContactName  *string `json:"contact_name"`
	ContactPhone *string `json:"contact_phone"`
	ContactEmail *string `json:"contact_email" binding:"omitempty,email"`
	Address      *string `json:"address"`
	Notes        *string `json:"notes"`
}

// SponsorView is a sponsoring body.
type SponsorView struct {
	ID           string  `json:"id"`
	Code         string  `json:"code"`
	NameAr       string  `json:"name_ar"`
	NameEn       *string `json:"name_en,omitempty"`
	SponsorType  string  `json:"sponsor_type"`
	ContactName  *string `json:"contact_name,omitempty"`
	ContactPhone *string `json:"contact_phone,omitempty"`
	IsActive     bool    `json:"is_active"`
}

// CreateSponsorshipRequest records an agreement.
type CreateSponsorshipRequest struct {
	SponsorID string `json:"sponsor_id" binding:"required,uuid"`
	StudentID string `json:"student_id" binding:"required,uuid"`

	CoverageType string `json:"coverage_type" binding:"required,oneof=percentage fixed_per_year full"`
	// CoverageBP is basis points of the discountable base — the same base a
	// percentage discount uses, so "eighty per cent of the fees" does not
	// include the identity card.
	CoverageBP     *int32 `json:"coverage_bp"`
	CoverageAmount *int64 `json:"coverage_amount"`
	AnnualCap      *int64 `json:"annual_cap"`

	// SettlementMode has no default. "receivable" leaves the student liable
	// and treats the sponsor's share as an expected inflow; "covers_debt"
	// reduces what the student owes and leaves the university carrying the
	// loss if the sponsor defaults. The choice decides who gets a debt letter.
	SettlementMode string `json:"settlement_mode" binding:"required,oneof=receivable covers_debt"`

	FromYearCode string  `json:"from_year_code" binding:"required"`
	ToYearCode   *string `json:"to_year_code"`
	AgreementRef *string `json:"agreement_ref"`
	Notes        *string `json:"notes"`
}

// SponsorshipView is an agreement.
type SponsorshipView struct {
	ID             string  `json:"id"`
	SponsorID      string  `json:"sponsor_id"`
	StudentID      string  `json:"student_id"`
	CoverageType   string  `json:"coverage_type"`
	CoverageBP     *int32  `json:"coverage_bp,omitempty"`
	CoverageAmount *int64  `json:"coverage_amount,omitempty"`
	AnnualCap      *int64  `json:"annual_cap,omitempty"`
	SettlementMode string  `json:"settlement_mode"`
	FromYearCode   string  `json:"from_year_code"`
	ToYearCode     *string `json:"to_year_code,omitempty"`
	Status         string  `json:"status"`
	AgreementRef   *string `json:"agreement_ref,omitempty"`
}

// SponsorReceivableView is what one sponsor owes for one year.
type SponsorReceivableView struct {
	SponsorID       string       `json:"sponsor_id"`
	SponsorCode     string       `json:"sponsor_code"`
	SponsorName     string       `json:"sponsor_name"`
	AcademicYearID  string       `json:"academic_year_id"`
	CommitmentCount int          `json:"commitment_count"`
	StudentCount    int          `json:"student_count"`
	Committed       money.Amount `json:"committed"`
	Paid            money.Amount `json:"paid"`
	Outstanding     money.Amount `json:"outstanding"`
}

// FundingView is who is paying for one account.
type FundingView struct {
	Gross              money.Amount `json:"gross"`
	Discount           money.Amount `json:"discount"`
	SponsorCovered     money.Amount `json:"sponsor_covered"`
	SponsorReceivable  money.Amount `json:"sponsor_receivable"`
	SponsorPaid        money.Amount `json:"sponsor_paid"`
	StudentPaid        money.Amount `json:"student_paid"`
	StudentOutstanding money.Amount `json:"student_outstanding"`
}

// ---------------------------------------------------------------------------
// Student portal
// ---------------------------------------------------------------------------

// StudentStatementView is a student's whole financial position.
type StudentStatementView struct {
	StudentID    string                 `json:"student_id"`
	StudentNo    string                 `json:"student_no"`
	FullName     string                 `json:"full_name"`
	Accounts     []StatementAccountView `json:"accounts"`
	TotalCharged money.Amount           `json:"total_charged"`
	TotalPaid    money.Amount           `json:"total_paid"`
	Outstanding  money.Amount           `json:"outstanding"`
	// NextDue is the soonest unpaid installment across every year, which is
	// the one figure a student actually came to ask about.
	NextDue     *NextDueView `json:"next_due,omitempty"`
	GeneratedAt time.Time    `json:"generated_at"`
}

// NextDueView is the soonest obligation.
type NextDueView struct {
	DueDate   string       `json:"due_date"`
	Amount    money.Amount `json:"amount"`
	Remaining money.Amount `json:"remaining"`
}

// StatementAccountView is one year of a student's fees.
type StatementAccountView struct {
	AccountID    string                 `json:"account_id"`
	YearCode     string                 `json:"academic_year"`
	Gross        money.Amount           `json:"gross"`
	Discount     money.Amount           `json:"discount"`
	EffectiveNet money.Amount           `json:"effective_net"`
	Paid         money.Amount           `json:"paid"`
	Outstanding  money.Amount           `json:"outstanding"`
	Credit       money.Amount           `json:"credit"`
	Status       string                 `json:"status"`
	Installments []InstallmentView      `json:"installments"`
	Payments     []StatementPaymentView `json:"payments"`
	Funding      *FundingView           `json:"funding,omitempty"`
}

// StatementPaymentView is a collection as a student's statement shows it.
type StatementPaymentView struct {
	PaymentID string       `json:"payment_id"`
	ReceiptNo *string      `json:"receipt_no,omitempty"`
	Amount    money.Amount `json:"amount"`
	Method    string       `json:"method"`
	PaidAt    time.Time    `json:"paid_at"`
	Status    string       `json:"status"`
	Refunded  money.Amount `json:"refunded"`
}

// IssueVerificationRequest mints a code for a printed statement.
type IssueVerificationRequest struct {
	// ValidForDays defaults to thirty. A verification that never expired would
	// let a student present a two-year-old clearance as current.
	ValidForDays int `json:"valid_for_days"`
}

// StatementVerificationView is a freshly minted verification code.
type StatementVerificationView struct {
	Code        string       `json:"code"`
	ExpiresAt   time.Time    `json:"expires_at"`
	Outstanding money.Amount `json:"outstanding"`
}

// StatementVerificationAnswer is what a third party learns from a code.
//
// Deliberately thin: it confirms that this university issued a statement for a
// person with this name and number showing these figures. Nothing else — the
// holder of a printed page has no business learning a national identifier from
// an unauthenticated endpoint.
type StatementVerificationAnswer struct {
	Valid        bool         `json:"valid"`
	StudentNo    string       `json:"student_no,omitempty"`
	FullName     string       `json:"full_name,omitempty"`
	TotalCharged money.Amount `json:"total_charged"`
	TotalPaid    money.Amount `json:"total_paid"`
	Outstanding  money.Amount `json:"outstanding"`
	IssuedAt     time.Time    `json:"issued_at,omitempty"`
	ExpiresAt    time.Time    `json:"expires_at,omitempty"`
	Reason       string       `json:"reason,omitempty"`
}
