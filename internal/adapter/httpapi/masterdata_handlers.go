package httpapi

import (
	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
)

// MasterDataHandlers administer the reference tables.
//
// Creating a college and a department was already possible; everything else —
// renaming one, retiring one, adding a payment method, opening a cashier desk,
// adding the student category a new pricing rule needs — required a database
// session. A university that has to open psql to let a new cashier start work
// is a university whose database password is widely known.
type MasterDataHandlers struct {
	Master *app.MasterDataService
}

// NewMasterDataHandlers wires the reference-data endpoints.
func NewMasterDataHandlers(master *app.MasterDataService) *MasterDataHandlers {
	return &MasterDataHandlers{Master: master}
}

// Register mounts the routes.
//
// Reads are open to anyone signed in — a cashier's screen needs the desk list,
// a registrar's needs the departments — and every write is the administrator's.
func (h *MasterDataHandlers) Register(g *gin.RouterGroup) {
	admin := httpx.RequireRoles(shared.RoleAdmin)

	g.PATCH("/colleges/:id", admin, h.UpdateCollege)
	g.PATCH("/departments/:id", admin, h.UpdateDepartment)
	g.PATCH("/study-types/:id", admin, h.UpdateStudyType)

	g.GET("/student-categories", h.ListStudentCategories)
	g.POST("/student-categories", admin, h.CreateStudentCategory)
	g.PATCH("/student-categories/:id", admin, h.UpdateStudentCategory)

	g.POST("/payment-methods", admin, h.CreatePaymentMethod)
	g.PATCH("/payment-methods/:id", admin, h.UpdatePaymentMethod)

	g.POST("/cashier-desks", admin, h.CreateCashierDesk)
	g.PATCH("/cashier-desks/:id", admin, h.UpdateCashierDesk)
}

// UpdateCollege renames or retires a college.
func (h *MasterDataHandlers) UpdateCollege(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdateMasterRequest
	if !bindJSON(c, &req) {
		return
	}

	college, err := h.Master.UpdateCollege(requestContext(c), httpx.MustActor(c), app.UpdateCollegeInput{
		ID: id, NameAr: req.NameAr, NameEn: req.NameEn, IsActive: req.IsActive, Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toCollegeView(college))
}

// UpdateDepartment renames a department, changes its length or retires it.
func (h *MasterDataHandlers) UpdateDepartment(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdateDepartmentRequest
	if !bindJSON(c, &req) {
		return
	}

	department, err := h.Master.UpdateDepartment(requestContext(c), httpx.MustActor(c), app.UpdateDepartmentInput{
		ID: id, NameAr: req.NameAr, NameEn: req.NameEn,
		StageCount: req.StageCount, IsActive: req.IsActive, Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toDepartmentView(department))
}

// UpdateStudyType renames, reorders or retires a study type.
func (h *MasterDataHandlers) UpdateStudyType(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdateStudyTypeRequest
	if !bindJSON(c, &req) {
		return
	}

	studyType, err := h.Master.UpdateStudyType(requestContext(c), httpx.MustActor(c), app.UpdateStudyTypeInput{
		ID: id, NameAr: req.NameAr, NameEn: req.NameEn,
		SortOrder: req.SortOrder, IsActive: req.IsActive, Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toStudyTypeView(studyType))
}

// ListStudentCategories returns the categories fee policy resolves against.
func (h *MasterDataHandlers) ListStudentCategories(c *gin.Context) {
	categories, err := h.Master.ListStudentCategories(
		requestContext(c), httpx.MustActor(c), !queryBool(c, "include_inactive"))
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	views := make([]StudentCategoryView, 0, len(categories))
	for _, category := range categories {
		views = append(views, StudentCategoryView{
			ID: category.ID.String(), Code: category.Code,
			NameAr: category.NameAr, IsActive: category.IsActive,
		})
	}
	httpx.OK(c, views)
}

// CreateStudentCategory adds a category.
func (h *MasterDataHandlers) CreateStudentCategory(c *gin.Context) {
	var req CreateStudentCategoryRequest
	if !bindJSON(c, &req) {
		return
	}

	category, err := h.Master.CreateStudentCategory(requestContext(c), httpx.MustActor(c), app.CreateStudentCategoryInput{
		Code: req.Code, NameAr: req.NameAr, NameEn: req.NameEn,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, StudentCategoryView{
		ID: category.ID.String(), Code: category.Code,
		NameAr: category.NameAr, IsActive: category.IsActive,
	})
}

// UpdateStudentCategory renames or retires a category.
func (h *MasterDataHandlers) UpdateStudentCategory(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdateMasterRequest
	if !bindJSON(c, &req) {
		return
	}

	category, err := h.Master.UpdateStudentCategory(requestContext(c), httpx.MustActor(c), app.UpdateStudentCategoryInput{
		ID: id, NameAr: req.NameAr, NameEn: req.NameEn, IsActive: req.IsActive, Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, StudentCategoryView{
		ID: category.ID.String(), Code: category.Code,
		NameAr: category.NameAr, IsActive: category.IsActive,
	})
}

// CreatePaymentMethod adds a way of paying.
func (h *MasterDataHandlers) CreatePaymentMethod(c *gin.Context) {
	var req CreatePaymentMethodRequest
	if !bindJSON(c, &req) {
		return
	}

	method, err := h.Master.CreatePaymentMethod(requestContext(c), httpx.MustActor(c), app.CreatePaymentMethodInput{
		Code: req.Code, NameAr: req.NameAr,
		IsCash: req.IsCash, RequiresReference: req.RequiresReference, SortOrder: req.SortOrder,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toPaymentMethodView(method))
}

// UpdatePaymentMethod renames a method, changes its rules or retires it.
func (h *MasterDataHandlers) UpdatePaymentMethod(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdatePaymentMethodRequest
	if !bindJSON(c, &req) {
		return
	}

	method, err := h.Master.UpdatePaymentMethod(requestContext(c), httpx.MustActor(c), app.UpdatePaymentMethodInput{
		ID: id, NameAr: req.NameAr, IsCash: req.IsCash,
		RequiresReference: req.RequiresReference, IsActive: req.IsActive,
		SortOrder: req.SortOrder, Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toPaymentMethodView(method))
}

// CreateCashierDesk opens a window money can be taken at.
func (h *MasterDataHandlers) CreateCashierDesk(c *gin.Context) {
	var req CreateCashierDeskRequest
	if !bindJSON(c, &req) {
		return
	}
	collegeID, ok := respondingID(c, req.CollegeID)
	if !ok {
		return
	}

	desk, err := h.Master.CreateCashierDesk(requestContext(c), httpx.MustActor(c), app.CreateCashierDeskInput{
		Code: req.Code, NameAr: req.NameAr, CollegeID: collegeID,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toDeskView(desk))
}

// UpdateCashierDesk renames a desk or closes it.
func (h *MasterDataHandlers) UpdateCashierDesk(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req UpdateCashierDeskRequest
	if !bindJSON(c, &req) {
		return
	}
	collegeID, ok := respondingID(c, req.CollegeID)
	if !ok {
		return
	}

	desk, err := h.Master.UpdateCashierDesk(requestContext(c), httpx.MustActor(c), app.UpdateCashierDeskInput{
		ID: id, NameAr: req.NameAr, CollegeID: collegeID, IsActive: req.IsActive, Reason: req.Reason,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toDeskView(desk))
}

func toPaymentMethodView(m *payment.Method) PaymentMethodView {
	return PaymentMethodView{
		ID: m.ID.String(), Code: m.Code, NameAr: m.NameAr,
		IsCash: m.IsCash, RequiresReference: m.RequiresReference, IsActive: m.IsActive,
	}
}

func toDeskView(d *payment.CashierDesk) CashierDeskView {
	view := CashierDeskView{
		ID: d.ID.String(), Code: d.Code, NameAr: d.NameAr, IsActive: d.IsActive,
	}
	if d.CollegeID != nil {
		view.CollegeID = ptr(d.CollegeID.String())
	}
	return view
}
