package httpapi

import (
	"github.com/gin-gonic/gin"

	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/httpx"
)

// SponsorHandlers administer sponsoring bodies and their agreements.
type SponsorHandlers struct {
	Sponsors *app.SponsorService
}

// NewSponsorHandlers wires the sponsorship endpoints.
func NewSponsorHandlers(s *app.SponsorService) *SponsorHandlers { return &SponsorHandlers{Sponsors: s} }

// Register mounts the routes.
func (h *SponsorHandlers) Register(g *gin.RouterGroup) {
	sponsors := g.Group("/sponsors")

	sponsors.GET("", h.ListSponsors)
	sponsors.GET("/receivables", h.Receivables)
	sponsors.POST("",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin),
		h.CreateSponsor)

	agreements := g.Group("/sponsorships")
	agreements.POST("",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleRegistrar),
		h.CreateSponsorship)
	agreements.POST("/:id/approve",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin),
		h.ApproveSponsorship)
	agreements.POST("/:id/revoke",
		httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin),
		h.RevokeSponsorship)

	g.GET("/students/:id/sponsorships", h.StudentSponsorships)
}

// ListSponsors returns the bodies.
func (h *SponsorHandlers) ListSponsors(c *gin.Context) {
	sponsors, err := h.Sponsors.ListSponsors(
		requestContext(c), httpx.MustActor(c), !queryBool(c, "include_inactive"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]SponsorView, 0, len(sponsors))
	for _, sponsor := range sponsors {
		views = append(views, toSponsorView(sponsor))
	}
	httpx.OK(c, views)
}

// CreateSponsor registers a body that pays students' fees.
func (h *SponsorHandlers) CreateSponsor(c *gin.Context) {
	var req CreateSponsorRequest
	if !bindJSON(c, &req) {
		return
	}

	sponsor, err := h.Sponsors.CreateSponsor(requestContext(c), httpx.MustActor(c), app.CreateSponsorInput{
		Code: req.Code, NameAr: req.NameAr, NameEn: req.NameEn, SponsorType: req.SponsorType,
		ContactName: req.ContactName, ContactPhone: req.ContactPhone,
		ContactEmail: req.ContactEmail, Address: req.Address, Notes: req.Notes,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toSponsorView(sponsor))
}

// CreateSponsorship records an agreement, in draft.
func (h *SponsorHandlers) CreateSponsorship(c *gin.Context) {
	var req CreateSponsorshipRequest
	if !bindJSON(c, &req) {
		return
	}

	sponsorID, err := shared.ParseID(req.SponsorID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	studentID, err := shared.ParseID(req.StudentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	input := app.CreateSponsorshipInput{
		SponsorID:      sponsorID,
		StudentID:      studentID,
		CoverageType:   billing.CoverageType(req.CoverageType),
		SettlementMode: billing.SettlementMode(req.SettlementMode),
		FromYearCode:   req.FromYearCode,
		ToYearCode:     req.ToYearCode,
		AgreementRef:   req.AgreementRef,
		Notes:          req.Notes,
	}
	if req.CoverageBP != nil {
		value := money.BasisPoints(*req.CoverageBP)
		input.CoverageBP = &value
	}
	if req.CoverageAmount != nil {
		value := money.Amount(*req.CoverageAmount)
		input.CoverageAmount = &value
	}
	if req.AnnualCap != nil {
		value := money.Amount(*req.AnnualCap)
		input.AnnualCap = &value
	}

	agreement, err := h.Sponsors.CreateSponsorship(requestContext(c), httpx.MustActor(c), input)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toSponsorshipView(agreement))
}

// ApproveSponsorship activates an agreement.
func (h *SponsorHandlers) ApproveSponsorship(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	agreement, err := h.Sponsors.ApproveSponsorship(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toSponsorshipView(agreement))
}

// RevokeSponsorship ends an agreement.
func (h *SponsorHandlers) RevokeSponsorship(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req ReasonRequest
	if !bindJSON(c, &req) {
		return
	}

	agreement, err := h.Sponsors.RevokeSponsorship(requestContext(c), httpx.MustActor(c), id, req.Reason)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toSponsorshipView(agreement))
}

// StudentSponsorships returns every agreement naming a student.
func (h *SponsorHandlers) StudentSponsorships(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}

	agreements, err := h.Sponsors.SponsorshipsForStudent(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]SponsorshipView, 0, len(agreements))
	for _, agreement := range agreements {
		views = append(views, toSponsorshipView(agreement))
	}
	httpx.OK(c, views)
}

// Receivables is the invoice list: what each sponsor owes.
func (h *SponsorHandlers) Receivables(c *gin.Context) {
	yearID, _ := optionalQueryID(c, "academic_year_id")

	receivables, err := h.Sponsors.Receivables(requestContext(c), httpx.MustActor(c), yearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]SponsorReceivableView, 0, len(receivables))
	for _, r := range receivables {
		views = append(views, SponsorReceivableView{
			SponsorID:       r.SponsorID.String(),
			SponsorCode:     r.SponsorCode,
			SponsorName:     r.SponsorName,
			AcademicYearID:  r.AcademicYearID.String(),
			CommitmentCount: r.CommitmentCount,
			StudentCount:    r.StudentCount,
			Committed:       r.Committed,
			Paid:            r.Paid,
			Outstanding:     r.Outstanding,
		})
	}
	httpx.OK(c, views)
}

func toSponsorView(s *billing.Sponsor) SponsorView {
	return SponsorView{
		ID: s.ID.String(), Code: s.Code, NameAr: s.NameAr, NameEn: s.NameEn,
		SponsorType: s.SponsorType, ContactName: s.ContactName,
		ContactPhone: s.ContactPhone, IsActive: s.IsActive,
	}
}

func toSponsorshipView(s *billing.Sponsorship) SponsorshipView {
	view := SponsorshipView{
		ID: s.ID.String(), SponsorID: s.SponsorID.String(), StudentID: s.StudentID.String(),
		CoverageType: string(s.CoverageType), SettlementMode: string(s.SettlementMode),
		FromYearCode: s.FromYearCode, ToYearCode: s.ToYearCode,
		Status: string(s.Status), AgreementRef: s.AgreementRef,
	}
	if s.CoverageBP != nil {
		value := int32(*s.CoverageBP)
		view.CoverageBP = &value
	}
	if s.CoverageAmount != nil {
		value := s.CoverageAmount.Int64()
		view.CoverageAmount = &value
	}
	if s.AnnualCap != nil {
		value := s.AnnualCap.Int64()
		view.AnnualCap = &value
	}
	return view
}
