package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/billing"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
)

// PortalHandlers expose a student's own financial position.
type PortalHandlers struct {
	Portal *app.PortalService
}

// NewPortalHandlers wires the student-facing endpoints.
func NewPortalHandlers(portal *app.PortalService) *PortalHandlers {
	return &PortalHandlers{Portal: portal}
}

// Register mounts the authenticated portal routes.
//
// A student reaches only their own record, which the service enforces by
// comparing every row against the student their credential belongs to. Staff
// reach the same routes through the authority they already hold, so the
// finance window and the portal show one answer rather than two.
func (h *PortalHandlers) Register(g *gin.RouterGroup) {
	portal := g.Group("/portal")

	portal.GET("/me/statement", h.MyStatement)
	portal.POST("/me/statement/verification", h.IssueMyVerification)
	portal.GET("/students/:id/statement", h.StudentStatement)
	portal.POST("/students/:id/statement/verification", h.IssueVerification)
	portal.POST("/students/:id/credential",
		httpx.RequireRoles(shared.RoleRegistrar, shared.RoleAdmin),
		h.IssueCredential)
}

// RegisterPublic mounts the verification endpoint outside authentication.
//
// An office checking a document a student handed them has no account here and
// should not need one. What protects the student is that the code is
// unguessable and the answer carries only what the paper already says.
func (h *PortalHandlers) RegisterPublic(engine gin.IRoutes) {
	engine.GET("/verify/statement/:code", h.VerifyStatement)
}

// MyStatement returns the signed-in student's own position.
func (h *PortalHandlers) MyStatement(c *gin.Context) {
	actor := httpx.MustActor(c)
	if actor.StudentID == nil {
		httpx.Respond(c, shared.Validation("portal.not_a_student",
			"this credential is not a student's; ask for a specific student instead"))
		return
	}
	h.respondStatement(c, *actor.StudentID)
}

// StudentStatement returns one student's position, for staff.
func (h *PortalHandlers) StudentStatement(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	h.respondStatement(c, id)
}

func (h *PortalHandlers) respondStatement(c *gin.Context, studentID shared.ID) {
	statement, err := h.Portal.StudentStatement(requestContext(c), httpx.MustActor(c), studentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toStatementView(statement))
}

// IssueMyVerification mints a verification code for the signed-in student.
func (h *PortalHandlers) IssueMyVerification(c *gin.Context) {
	actor := httpx.MustActor(c)
	if actor.StudentID == nil {
		httpx.Respond(c, shared.Validation("portal.not_a_student",
			"this credential is not a student's"))
		return
	}
	h.issueVerification(c, *actor.StudentID)
}

// IssueVerification mints a verification code for one student, for staff.
func (h *PortalHandlers) IssueVerification(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	h.issueVerification(c, id)
}

func (h *PortalHandlers) issueVerification(c *gin.Context, studentID shared.ID) {
	var req IssueVerificationRequest
	if c.Request.ContentLength > 0 && !bindJSON(c, &req) {
		return
	}

	validFor := time.Duration(req.ValidForDays) * 24 * time.Hour
	yearID, _ := optionalQueryID(c, "academic_year_id")

	result, err := h.Portal.IssueStatementVerification(
		requestContext(c), httpx.MustActor(c), studentID, yearID, validFor)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, StatementVerificationView{
		Code:        result.Code,
		ExpiresAt:   result.ExpiresAt,
		Outstanding: result.Outstanding,
	})
}

// IssueCredential creates a student's portal login.
func (h *PortalHandlers) IssueCredential(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}

	result, err := h.Portal.IssueStudentCredential(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, gin.H{
		"username": result.Username,
		// Shown once. The student must replace it before the portal answers
		// them anything else.
		"temporary_password": result.TemporaryPassword,
	})
}

// VerifyStatement answers whether a printed statement is genuine.
func (h *PortalHandlers) VerifyStatement(c *gin.Context) {
	answer, err := h.Portal.VerifyStatement(requestContext(c), c.Param("code"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, StatementVerificationAnswer{
		Valid:        answer.Valid,
		StudentNo:    answer.StudentNo,
		FullName:     answer.FullName,
		TotalCharged: answer.TotalCharged,
		TotalPaid:    answer.TotalPaid,
		Outstanding:  answer.Outstanding,
		IssuedAt:     answer.IssuedAt,
		ExpiresAt:    answer.ExpiresAt,
		Reason:       answer.Reason,
	})
}

func toStatementView(s *app.Statement) StudentStatementView {
	view := StudentStatementView{
		StudentID:    s.StudentID.String(),
		StudentNo:    s.StudentNo,
		FullName:     s.FullName,
		TotalCharged: s.TotalCharged,
		TotalPaid:    s.TotalPaid,
		Outstanding:  s.Outstanding,
		GeneratedAt:  s.GeneratedAt,
	}
	if s.NextDue != nil {
		view.NextDue = &NextDueView{
			DueDate:   s.NextDue.DueDate.String(),
			Amount:    s.NextDue.Amount,
			Remaining: s.NextDue.Remaining(),
		}
	}

	today := shared.DateFromTime(time.Now().UTC())
	for _, line := range s.Lines {
		accountView := StatementAccountView{
			AccountID:    line.Account.ID.String(),
			YearCode:     line.YearCode,
			Gross:        line.Account.GrossTotal,
			Discount:     line.Account.DiscountTotal,
			EffectiveNet: line.Account.EffectiveNet(),
			Paid:         line.Account.NetPaid(),
			Outstanding:  line.Account.Remaining(),
			Credit:       line.Account.CreditBalance,
			Status:       string(line.Account.Status),
			Installments: toInstallmentViews(line.Installments, today),
		}
		for _, payment := range line.Payments {
			accountView.Payments = append(accountView.Payments, StatementPaymentView{
				PaymentID: payment.PaymentID.String(),
				ReceiptNo: payment.ReceiptNo,
				Amount:    payment.Amount,
				Method:    payment.MethodCode,
				PaidAt:    payment.PaidAt,
				Status:    payment.Status,
				Refunded:  payment.RefundedTotal,
			})
		}
		if line.Funding != nil {
			accountView.Funding = &FundingView{
				Gross:              line.Funding.Gross,
				Discount:           line.Funding.Discount,
				SponsorCovered:     line.Funding.SponsorCovered,
				SponsorReceivable:  line.Funding.SponsorReceivable,
				SponsorPaid:        line.Funding.SponsorPaid,
				StudentPaid:        line.Funding.StudentPaid,
				StudentOutstanding: line.Funding.StudentOutstanding,
			}
		}
		view.Accounts = append(view.Accounts, accountView)
	}
	return view
}

var _ = billing.Account{}
var _ = money.Amount(0)
