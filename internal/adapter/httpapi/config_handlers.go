package httpapi

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/academic"
	"flowed/internal/domain/billing"
	"flowed/internal/domain/discount"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
)

// ConfigHandlers exposes the configuration administration: fee policies,
// installment templates, discounts, and reference data.
//
// Every route here goes through ConfigService rather than touching a
// repository, reads included. That is a departure from the read paths
// elsewhere in this package, and it is deliberate: configuration reads are
// role-scoped — a cashier has no business browsing the fee book — and putting
// the check in the service keeps it identical whether the caller is this
// router or a future reporting job.
type ConfigHandlers struct {
	Config *app.ConfigService
}

// NewConfigHandlers wires the configuration endpoints.
func NewConfigHandlers(config *app.ConfigService) *ConfigHandlers {
	return &ConfigHandlers{Config: config}
}

// Register mounts the configuration routes on an authenticated group.
//
// The authority split follows the separation-of-duties matrix. Prices,
// installment shapes and discount values are the finance manager's, with the
// administrator as the standing second pair of hands — and publishing a
// discount version additionally requires that the two are different people,
// which the service enforces. Reference data is structural rather than
// financial, so it belongs to the administrator alone. Reads reach further
// than writes: an auditor tracing why a student was charged, and a registrar
// answering the same question at the counter, both need to see the
// configuration and neither can change it.
func (h *ConfigHandlers) Register(g *gin.RouterGroup) {
	write := httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin)
	read := httpx.RequireRoles(shared.RoleFinanceManager, shared.RoleAdmin,
		shared.RoleAuditor, shared.RoleRegistrar)
	admin := httpx.RequireRoles(shared.RoleAdmin)

	policies := g.Group("/fee-policies")
	policies.POST("", write, h.DefineFeePolicy)
	policies.GET("", read, h.ListFeePolicies)
	policies.GET("/:id", read, h.GetFeePolicy)
	policies.POST("/:id/publish", write, h.PublishFeePolicy)
	policies.POST("/:id/retire", write, h.RetireFeePolicy)
	// A preview writes nothing; it is a POST because the scope it is asked
	// about is six fields, and because the same shape is what account
	// generation's dry run already uses.
	policies.POST("/preview-resolution", read, h.PreviewFeeResolution)
	// The one-call configuration surface for a study type's default debt on
	// creation — same authority as defining any other policy, since that is
	// exactly what this does under one name.
	policies.POST("/study-type-defaults", write, h.SetStudyTypeInitialDebt)

	templates := g.Group("/installment-templates")
	templates.POST("", write, h.DefineInstallmentTemplate)
	templates.GET("", read, h.ListInstallmentTemplates)
	templates.GET("/:id", read, h.GetInstallmentTemplate)
	templates.POST("/:id/publish", write, h.PublishInstallmentTemplate)
	templates.POST("/:id/retire", write, h.RetireInstallmentTemplate)
	// The one-call configuration surface for a study type's default
	// installment plan — same authority as defining any other template.
	templates.POST("/study-type-defaults", write, h.SetStudyTypeInstallmentPlan)

	discounts := g.Group("/discounts")
	discounts.POST("/definitions", write, h.DefineDiscount)
	discounts.GET("/definitions", read, h.ListDiscounts)
	discounts.GET("/definitions/:id", read, h.GetDiscount)
	discounts.POST("/definitions/:id/versions", write, h.AddDiscountVersion)
	discounts.GET("/versions/:id", read, h.GetDiscountVersion)
	discounts.POST("/versions/:id/publish", write, h.PublishDiscountVersion)

	// Reference data is created where it is already listed, so a client
	// discovers both halves at one path.
	g.POST("/colleges", admin, h.CreateCollege)
	g.POST("/departments", admin, h.CreateDepartment)
	g.POST("/study-types", admin, h.CreateStudyType)
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// FeeComponentRequest is one charge inside a policy.
type FeeComponentRequest struct {
	Code   string  `json:"code" binding:"required"`
	NameAr string  `json:"name_ar" binding:"required"`
	NameEn *string `json:"name_en"`
	Amount int64   `json:"amount" binding:"min=0"`
	// The three flags are pointers because their default is true. An omitted
	// field must not quietly make a registration charge optional, or a tuition
	// charge immune to the exemption that was supposed to cover it.
	IsDiscountable *bool `json:"is_discountable"`
	IsRefundable   *bool `json:"is_refundable"`
	IsMandatory    *bool `json:"is_mandatory"`
	SortOrder      int16 `json:"sort_order"`
}

// DefineFeePolicyRequest defines a draft fee policy for one scope.
type DefineFeePolicyRequest struct {
	PolicyCode string `json:"policy_code" binding:"required"`
	VersionNo  int32  `json:"version_no" binding:"omitempty,min=1"`

	// An omitted dimension is a wildcard. The academic year never is.
	AcademicYearID    string  `json:"academic_year_id" binding:"required,uuid"`
	CollegeID         *string `json:"college_id" binding:"omitempty,uuid"`
	DepartmentID      *string `json:"department_id" binding:"omitempty,uuid"`
	Stage             *int16  `json:"stage" binding:"omitempty,min=1,max=5"`
	StudyTypeID       *string `json:"study_type_id" binding:"omitempty,uuid"`
	StudentCategoryID *string `json:"student_category_id" binding:"omitempty,uuid"`

	// MaxDiscountBP is omitted for the full rate. Zero is a real value — no
	// discount may apply under this policy — so it must be stated.
	MaxDiscountBP *int32  `json:"max_discount_bp" binding:"omitempty,min=0,max=10000"`
	EffectiveFrom *string `json:"effective_from"`
	Description   *string `json:"description"`

	Components []FeeComponentRequest `json:"components" binding:"required,min=1,dive"`
}

// PreviewFeeResolutionRequest asks which policy would price an enrollment.
type PreviewFeeResolutionRequest struct {
	AcademicYearID string `json:"academic_year_id" binding:"required,uuid"`
	CollegeID      string `json:"college_id" binding:"required,uuid"`
	DepartmentID   string `json:"department_id" binding:"required,uuid"`
	Stage          int16  `json:"stage" binding:"required,min=1,max=5"`
	StudyTypeID    string `json:"study_type_id" binding:"required,uuid"`
	// StudentCategoryCode defaults to REGULAR, matching what enrollment
	// derives for a first attempt at a stage.
	StudentCategoryCode string `json:"student_category_code"`
}

// TemplateLineRequest is one installment of a plan, given either as a
// percentage share or a literal amount — never both, and every line on a
// template must agree on which.
type TemplateLineRequest struct {
	// LineNo may be omitted, in which case the lines are numbered in order.
	LineNo int16 `json:"line_no" binding:"omitempty,min=1"`
	// ShareBP is required unless every line instead carries Amount.
	ShareBP int32 `json:"share_bp" binding:"omitempty,min=1,max=10000"`
	// Amount is a literal installment figure ("400,000"), for a plan
	// authored in amounts rather than percentages.
	Amount *int64 `json:"amount" binding:"omitempty,min=0"`
	// DueOffsetDays counts from the academic year's start, so one template
	// serves every year.
	DueOffsetDays int     `json:"due_offset_days" binding:"min=0"`
	Label         *string `json:"label_ar"`
}

// DefineInstallmentTemplateRequest defines a draft plan shape.
type DefineInstallmentTemplateRequest struct {
	Code   string  `json:"code" binding:"required"`
	NameAr string  `json:"name_ar" binding:"required"`
	NameEn *string `json:"name_en"`

	// Every dimension is optional, the year included: a template with no year
	// is global and serves every year until something narrower overrides it.
	AcademicYearID *string `json:"academic_year_id" binding:"omitempty,uuid"`
	CollegeID      *string `json:"college_id" binding:"omitempty,uuid"`
	DepartmentID   *string `json:"department_id" binding:"omitempty,uuid"`
	Stage          *int16  `json:"stage" binding:"omitempty,min=1,max=5"`
	StudyTypeID    *string `json:"study_type_id" binding:"omitempty,uuid"`

	MaxInstallments int16                 `json:"max_installments" binding:"omitempty,min=1,max=24"`
	Lines           []TemplateLineRequest `json:"lines" binding:"required,min=1,dive"`
}

// DefineDiscountRequest creates a discount definition: the header only, since
// every value lives on a version.
type DefineDiscountRequest struct {
	Code     string  `json:"code" binding:"required"`
	NameAr   string  `json:"name_ar" binding:"required"`
	NameEn   *string `json:"name_en"`
	Category string  `json:"category" binding:"omitempty,oneof=social staff merit exemption sibling martyr other"`

	ExclusivityGroupID *string `json:"exclusivity_group_id" binding:"omitempty,uuid"`
	IsFullExemption    bool    `json:"is_full_exemption"`
	// AnnualReconfirmation is a pointer because its default is true: a
	// hardship discount must not run for six years without anybody re-checking
	// the hardship.
	AnnualReconfirmation *bool `json:"annual_reconfirmation"`
}

// AddDiscountVersionRequest drafts a new configuration of a discount.
type AddDiscountVersionRequest struct {
	// VersionNo may be omitted, in which case it follows the version in force.
	VersionNo int32  `json:"version_no" binding:"omitempty,min=1"`
	ValueType string `json:"value_type" binding:"required,oneof=percentage fixed"`
	// RateBP is basis points: 2500 is twenty-five percent. Integer rates keep
	// the arithmetic reproducible years later.
	RateBP      int32 `json:"rate_bp" binding:"omitempty,min=0,max=10000"`
	FixedAmount int64 `json:"fixed_amount" binding:"omitempty,min=1"`

	AppliesToComponents []string `json:"applies_to_components"`
	PerApplicationCap   *int64   `json:"per_application_cap" binding:"omitempty,min=1"`
	Stackable           *bool    `json:"stackable"`
	Priority            *int16   `json:"priority"`
	RequiresApproval    *bool    `json:"requires_approval"`
	ApprovalRole        *string  `json:"approval_role"`
	RequiredDocuments   []string `json:"required_documents"`

	ValidFromYearID *string `json:"valid_from_year_id" binding:"omitempty,uuid"`
	ValidToYearID   *string `json:"valid_to_year_id" binding:"omitempty,uuid"`
	Notes           *string `json:"notes"`
}

// CreateCollegeRequest adds a faculty.
type CreateCollegeRequest struct {
	Code   string  `json:"code" binding:"required"`
	NameAr string  `json:"name_ar" binding:"required"`
	NameEn *string `json:"name_en"`
}

// CreateDepartmentRequest adds a programme to a college.
type CreateDepartmentRequest struct {
	CollegeID string  `json:"college_id" binding:"required,uuid"`
	Code      string  `json:"code" binding:"required"`
	NameAr    string  `json:"name_ar" binding:"required"`
	NameEn    *string `json:"name_en"`
	// StageCount is the programme's length in years: six for medicine, four or
	// five for engineering.
	StageCount int16 `json:"stage_count" binding:"required,min=1,max=5"`
}

// CreateStudyTypeRequest adds a mode of study.
type CreateStudyTypeRequest struct {
	Code      string  `json:"code" binding:"required"`
	NameAr    string  `json:"name_ar" binding:"required"`
	NameEn    *string `json:"name_en"`
	SortOrder int16   `json:"sort_order"`
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// FeeComponentView is one charge inside a policy.
type FeeComponentView struct {
	ID             string       `json:"id"`
	Code           string       `json:"code"`
	NameAr         string       `json:"name_ar"`
	NameEn         *string      `json:"name_en,omitempty"`
	Amount         money.Amount `json:"amount"`
	IsDiscountable bool         `json:"is_discountable"`
	IsRefundable   bool         `json:"is_refundable"`
	IsMandatory    bool         `json:"is_mandatory"`
	SortOrder      int16        `json:"sort_order"`
}

// FeePolicyView is a priced scope.
//
// The specificity score is exposed rather than hidden as an implementation
// detail: it is the number that decides which policy prices a student, and a
// client that cannot show it cannot explain the charge.
type FeePolicyView struct {
	ID         string `json:"id"`
	PolicyCode string `json:"policy_code"`
	VersionNo  int32  `json:"version_no"`

	AcademicYearID    string  `json:"academic_year_id"`
	CollegeID         *string `json:"college_id,omitempty"`
	DepartmentID      *string `json:"department_id,omitempty"`
	Stage             *int16  `json:"stage,omitempty"`
	StudyTypeID       *string `json:"study_type_id,omitempty"`
	StudentCategoryID *string `json:"student_category_id,omitempty"`

	SpecificityScore int32   `json:"specificity_score"`
	Status           string  `json:"status"`
	MaxDiscountBP    int32   `json:"max_discount_bp"`
	EffectiveFrom    *string `json:"effective_from,omitempty"`
	Description      *string `json:"description,omitempty"`

	Components []FeeComponentView `json:"components"`
	GrossTotal money.Amount       `json:"gross_total"`

	PublishedAt *time.Time `json:"published_at,omitempty"`
	RetiredAt   *time.Time `json:"retired_at,omitempty"`
}

// FeeResolutionCandidateView is one published policy that covers a scope.
type FeeResolutionCandidateView struct {
	PolicyID         string       `json:"policy_id"`
	PolicyCode       string       `json:"policy_code"`
	VersionNo        int32        `json:"version_no"`
	SpecificityScore int32        `json:"specificity_score"`
	Dimensions       []string     `json:"dimensions"`
	GrossTotal       money.Amount `json:"gross_total"`
	Wins             bool         `json:"wins"`
}

// FeeResolutionPreviewView answers "which policy prices this student, and what
// did it beat" before any account exists.
type FeeResolutionPreviewView struct {
	Resolvable bool                         `json:"resolvable"`
	Winner     *FeeResolutionCandidateView  `json:"winner"`
	RunnersUp  []FeeResolutionCandidateView `json:"runners_up"`
	// Explanation is the ranking in one sentence, for the screen a registrar
	// reads out to a student who is disputing a figure.
	Explanation string `json:"explanation"`
}

// TemplateLineView is one installment of a plan shape.
type TemplateLineView struct {
	LineNo       int16  `json:"line_no"`
	ShareBP      int32  `json:"share_bp"`
	SharePercent string `json:"share_percent"`
	// Amount is set when this line was authored as a literal figure rather
	// than a percentage; ShareBP is still shown, since it is what a mid-year
	// re-split actually uses.
	Amount        *int64  `json:"amount,omitempty"`
	DueOffsetDays int     `json:"due_offset_days"`
	Label         *string `json:"label_ar,omitempty"`
}

// InstallmentTemplateView is a reusable plan shape.
type InstallmentTemplateView struct {
	ID     string  `json:"id"`
	Code   string  `json:"code"`
	NameAr string  `json:"name_ar"`
	NameEn *string `json:"name_en,omitempty"`

	AcademicYearID *string `json:"academic_year_id,omitempty"`
	CollegeID      *string `json:"college_id,omitempty"`
	DepartmentID   *string `json:"department_id,omitempty"`
	Stage          *int16  `json:"stage,omitempty"`
	StudyTypeID    *string `json:"study_type_id,omitempty"`

	SpecificityScore int32  `json:"specificity_score"`
	MaxInstallments  int16  `json:"max_installments"`
	Status           string `json:"status"`

	Lines []TemplateLineView `json:"lines"`
	// TotalBP is what the shares currently add up to. A draft is allowed to be
	// short of 10000; showing the running total is what stops the shortfall
	// being discovered at publication.
	TotalBP int32 `json:"total_bp"`

	PublishedAt *time.Time `json:"published_at,omitempty"`
	RetiredAt   *time.Time `json:"retired_at,omitempty"`
}

// DiscountDefinitionView is a catalogue entry. It carries no value: every
// number lives on a version.
type DiscountDefinitionView struct {
	ID                   string  `json:"id"`
	Code                 string  `json:"code"`
	NameAr               string  `json:"name_ar"`
	NameEn               *string `json:"name_en,omitempty"`
	Category             string  `json:"category"`
	ExclusivityGroupID   *string `json:"exclusivity_group_id,omitempty"`
	IsFullExemption      bool    `json:"is_full_exemption"`
	AnnualReconfirmation bool    `json:"annual_reconfirmation"`
	IsActive             bool    `json:"is_active"`
}

// DiscountVersionView is one configuration of a discount.
//
// CreatedBy is exposed because publishing requires a second person: the
// reviewer needs to see who drafted the value they are being asked to put in
// force.
type DiscountVersionView struct {
	ID           string `json:"id"`
	DefinitionID string `json:"definition_id"`
	VersionNo    int32  `json:"version_no"`

	ValueType    string  `json:"value_type"`
	RateBP       int32   `json:"rate_bp,omitempty"`
	RatePercent  *string `json:"rate_percent,omitempty"`
	FixedAmount  int64   `json:"fixed_amount,omitempty"`
	MaxPerAppCap *int64  `json:"per_application_cap,omitempty"`

	AppliesToComponents []string `json:"applies_to_components,omitempty"`
	Stackable           bool     `json:"stackable"`
	Priority            int16    `json:"priority"`
	RequiresApproval    bool     `json:"requires_approval"`
	ApprovalRole        *string  `json:"approval_role,omitempty"`
	RequiredDocuments   []string `json:"required_documents,omitempty"`

	ValidFromYearID *string `json:"valid_from_year_id,omitempty"`
	ValidToYearID   *string `json:"valid_to_year_id,omitempty"`

	Status      string     `json:"status"`
	Notes       *string    `json:"notes,omitempty"`
	CreatedBy   *string    `json:"created_by,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	PublishedBy *string    `json:"published_by,omitempty"`
}

// DiscountDetailView is a definition with the version currently in force.
type DiscountDetailView struct {
	Definition DiscountDefinitionView `json:"definition"`
	// PublishedVersion is null while the first version is still a draft, which
	// is an ordinary state for an entry being prepared.
	PublishedVersion *DiscountVersionView `json:"published_version"`
}

// CollegeView is a faculty.
type CollegeView struct {
	ID       string  `json:"id"`
	Code     string  `json:"code"`
	NameAr   string  `json:"name_ar"`
	NameEn   *string `json:"name_en,omitempty"`
	IsActive bool    `json:"is_active"`
}

// DepartmentView is a programme.
type DepartmentView struct {
	ID         string  `json:"id"`
	CollegeID  string  `json:"college_id"`
	Code       string  `json:"code"`
	NameAr     string  `json:"name_ar"`
	NameEn     *string `json:"name_en,omitempty"`
	StageCount int16   `json:"stage_count"`
	IsActive   bool    `json:"is_active"`
}

// StudyTypeView is a mode of study.
type StudyTypeView struct {
	ID        string  `json:"id"`
	Code      string  `json:"code"`
	NameAr    string  `json:"name_ar"`
	NameEn    *string `json:"name_en,omitempty"`
	SortOrder int16   `json:"sort_order"`
	IsActive  bool    `json:"is_active"`
}

// ---------------------------------------------------------------------------
// Fee policy handlers
// ---------------------------------------------------------------------------

// DefineFeePolicy records a draft policy with its components.
func (h *ConfigHandlers) DefineFeePolicy(c *gin.Context) {
	var req DefineFeePolicyRequest
	if !bindJSON(c, &req) {
		return
	}

	yearID, err := shared.ParseID(req.AcademicYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	collegeID, ok := optionalBodyID(c, req.CollegeID)
	if !ok {
		return
	}
	departmentID, ok := optionalBodyID(c, req.DepartmentID)
	if !ok {
		return
	}
	studyTypeID, ok := optionalBodyID(c, req.StudyTypeID)
	if !ok {
		return
	}
	categoryID, ok := optionalBodyID(c, req.StudentCategoryID)
	if !ok {
		return
	}

	in := app.DefineFeePolicyInput{
		PolicyCode:        req.PolicyCode,
		VersionNo:         req.VersionNo,
		AcademicYearID:    yearID,
		CollegeID:         collegeID,
		DepartmentID:      departmentID,
		Stage:             req.Stage,
		StudyTypeID:       studyTypeID,
		StudentCategoryID: categoryID,
		Description:       req.Description,
	}
	if req.MaxDiscountBP != nil {
		in.MaxDiscountBP = ptrBasisPoints(*req.MaxDiscountBP)
	}
	if req.EffectiveFrom != nil {
		effectiveFrom, err := shared.ParseDate(*req.EffectiveFrom)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		in.EffectiveFrom = &effectiveFrom
	}
	for _, component := range req.Components {
		in.Components = append(in.Components, app.FeeComponentInput{
			Code:           component.Code,
			NameAr:         component.NameAr,
			NameEn:         component.NameEn,
			Amount:         money.FromInt64(component.Amount),
			IsDiscountable: component.IsDiscountable,
			IsRefundable:   component.IsRefundable,
			IsMandatory:    component.IsMandatory,
			SortOrder:      component.SortOrder,
		})
	}

	policy, err := h.Config.DefineFeePolicy(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	view, err := toFeePolicyView(policy)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, view)
}

// PublishFeePolicy puts a draft policy in force, freezing its amounts.
func (h *ConfigHandlers) PublishFeePolicy(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	policy, err := h.Config.PublishFeePolicy(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	view, err := toFeePolicyView(policy)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, view)
}

// RetireFeePolicy takes a published policy out of resolution, freeing its
// scope for a new version.
func (h *ConfigHandlers) RetireFeePolicy(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	policy, err := h.Config.RetireFeePolicy(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	view, err := toFeePolicyView(policy)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, view)
}

// SetStudyTypeInitialDebtRequest names the flat default debt a newly created
// enrollment of one study type should be priced at, for one academic year.
type SetStudyTypeInitialDebtRequest struct {
	AcademicYearID string `json:"academic_year_id" binding:"required,uuid"`
	StudyTypeID    string `json:"study_type_id" binding:"required,uuid"`
	Amount         int64  `json:"amount" binding:"min=0"`
}

// SetStudyTypeInitialDebt is the configuration surface for §"initial debt by
// study type": one call, naming a year, a study type and an amount, that
// defines and publishes the wildcard fee policy behind it — retiring the
// previous amount for that study type first, in the same transaction, so a
// finance manager changes the figure without ever touching fee_policy_version
// through anything but this and the generic fee-policy screen.
func (h *ConfigHandlers) SetStudyTypeInitialDebt(c *gin.Context) {
	var req SetStudyTypeInitialDebtRequest
	if !bindJSON(c, &req) {
		return
	}
	yearID, err := shared.ParseID(req.AcademicYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	studyTypeID, err := shared.ParseID(req.StudyTypeID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	policy, err := h.Config.SetStudyTypeInitialDebt(requestContext(c), httpx.MustActor(c), app.SetStudyTypeInitialDebtInput{
		AcademicYearID: yearID,
		StudyTypeID:    studyTypeID,
		Amount:         money.Amount(req.Amount),
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	view, err := toFeePolicyView(policy)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, view)
}

// ListFeePolicies returns every policy defined for a year.
func (h *ConfigHandlers) ListFeePolicies(c *gin.Context) {
	yearID, ok := requiredQueryID(c, "academic_year_id")
	if !ok {
		return
	}
	policies, err := h.Config.ListFeePolicies(requestContext(c), httpx.MustActor(c), yearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]FeePolicyView, 0, len(policies))
	for _, policy := range policies {
		view, err := toFeePolicyView(policy)
		if err != nil {
			httpx.Respond(c, err)
			return
		}
		views = append(views, view)
	}
	httpx.OK(c, views)
}

// GetFeePolicy returns one policy with its components.
func (h *ConfigHandlers) GetFeePolicy(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	policy, err := h.Config.GetFeePolicy(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	view, err := toFeePolicyView(policy)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, view)
}

// PreviewFeeResolution reports which published policy would price an
// enrollment, and which ones it beat.
func (h *ConfigHandlers) PreviewFeeResolution(c *gin.Context) {
	var req PreviewFeeResolutionRequest
	if !bindJSON(c, &req) {
		return
	}

	yearID, err := shared.ParseID(req.AcademicYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	collegeID, err := shared.ParseID(req.CollegeID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	departmentID, err := shared.ParseID(req.DepartmentID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	studyTypeID, err := shared.ParseID(req.StudyTypeID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	preview, err := h.Config.PreviewFeeResolution(requestContext(c), httpx.MustActor(c), app.FeeResolutionInput{
		AcademicYearID:      yearID,
		CollegeID:           collegeID,
		DepartmentID:        departmentID,
		Stage:               req.Stage,
		StudyTypeID:         studyTypeID,
		StudentCategoryCode: req.StudentCategoryCode,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toResolutionPreviewView(preview))
}

// ---------------------------------------------------------------------------
// Installment template handlers
// ---------------------------------------------------------------------------

// DefineInstallmentTemplate records a draft plan shape with its lines.
func (h *ConfigHandlers) DefineInstallmentTemplate(c *gin.Context) {
	var req DefineInstallmentTemplateRequest
	if !bindJSON(c, &req) {
		return
	}

	yearID, ok := optionalBodyID(c, req.AcademicYearID)
	if !ok {
		return
	}
	collegeID, ok := optionalBodyID(c, req.CollegeID)
	if !ok {
		return
	}
	departmentID, ok := optionalBodyID(c, req.DepartmentID)
	if !ok {
		return
	}
	studyTypeID, ok := optionalBodyID(c, req.StudyTypeID)
	if !ok {
		return
	}

	in := app.DefineInstallmentTemplateInput{
		Code:            req.Code,
		NameAr:          req.NameAr,
		NameEn:          req.NameEn,
		AcademicYearID:  yearID,
		CollegeID:       collegeID,
		DepartmentID:    departmentID,
		Stage:           req.Stage,
		StudyTypeID:     studyTypeID,
		MaxInstallments: req.MaxInstallments,
	}
	for _, line := range req.Lines {
		var amount *money.Amount
		if line.Amount != nil {
			v := money.Amount(*line.Amount)
			amount = &v
		}
		in.Lines = append(in.Lines, app.TemplateLineInput{
			LineNo:        line.LineNo,
			ShareBP:       money.BasisPoints(line.ShareBP),
			Amount:        amount,
			DueOffsetDays: line.DueOffsetDays,
			Label:         line.Label,
		})
	}

	template, err := h.Config.DefineInstallmentTemplate(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toTemplateView(template))
}

// PublishInstallmentTemplate validates the shares and puts the template in
// force.
func (h *ConfigHandlers) PublishInstallmentTemplate(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	template, err := h.Config.PublishInstallmentTemplate(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toTemplateView(template))
}

// RetireInstallmentTemplate takes a published template out of resolution,
// freeing its scope for a new version.
func (h *ConfigHandlers) RetireInstallmentTemplate(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	template, err := h.Config.RetireInstallmentTemplate(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toTemplateView(template))
}

// StudyTypeInstallmentLineRequest is one literal installment on a study
// type's default plan.
type StudyTypeInstallmentLineRequest struct {
	Amount        int64   `json:"amount" binding:"required,min=1"`
	DueOffsetDays int     `json:"due_offset_days" binding:"min=0"`
	Label         *string `json:"label_ar"`
}

// SetStudyTypeInstallmentPlanRequest names the literal installments a newly
// created enrollment of one study type should be split into.
type SetStudyTypeInstallmentPlanRequest struct {
	AcademicYearID string                            `json:"academic_year_id" binding:"required,uuid"`
	StudyTypeID    string                            `json:"study_type_id" binding:"required,uuid"`
	Lines          []StudyTypeInstallmentLineRequest `json:"lines" binding:"required,min=1,dive"`
}

// SetStudyTypeInstallmentPlan is the configuration surface for a study type's
// default installment plan — see app.ConfigService.SetStudyTypeInstallmentPlan.
func (h *ConfigHandlers) SetStudyTypeInstallmentPlan(c *gin.Context) {
	var req SetStudyTypeInstallmentPlanRequest
	if !bindJSON(c, &req) {
		return
	}
	yearID, err := shared.ParseID(req.AcademicYearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	studyTypeID, err := shared.ParseID(req.StudyTypeID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	lines := make([]app.StudyTypeInstallmentLineInput, 0, len(req.Lines))
	for _, line := range req.Lines {
		lines = append(lines, app.StudyTypeInstallmentLineInput{
			Amount:        money.Amount(line.Amount),
			DueOffsetDays: line.DueOffsetDays,
			Label:         line.Label,
		})
	}

	template, err := h.Config.SetStudyTypeInstallmentPlan(requestContext(c), httpx.MustActor(c), app.SetStudyTypeInstallmentPlanInput{
		AcademicYearID: yearID,
		StudyTypeID:    studyTypeID,
		Lines:          lines,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toTemplateView(template))
}

// ListInstallmentTemplates returns the templates applicable to a year, or all
// of them when no year is given.
func (h *ConfigHandlers) ListInstallmentTemplates(c *gin.Context) {
	var yearID *shared.ID
	if id, ok := optionalQueryID(c, "academic_year_id"); ok {
		yearID = id
	}
	templates, err := h.Config.ListInstallmentTemplates(requestContext(c), httpx.MustActor(c), yearID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]InstallmentTemplateView, 0, len(templates))
	for _, template := range templates {
		views = append(views, toTemplateView(template))
	}
	httpx.OK(c, views)
}

// GetInstallmentTemplate returns one template with its lines.
func (h *ConfigHandlers) GetInstallmentTemplate(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	template, err := h.Config.GetInstallmentTemplate(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toTemplateView(template))
}

// ---------------------------------------------------------------------------
// Discount handlers
// ---------------------------------------------------------------------------

// DefineDiscount creates a discount definition.
func (h *ConfigHandlers) DefineDiscount(c *gin.Context) {
	var req DefineDiscountRequest
	if !bindJSON(c, &req) {
		return
	}

	groupID, ok := optionalBodyID(c, req.ExclusivityGroupID)
	if !ok {
		return
	}

	definition, err := h.Config.DefineDiscount(requestContext(c), httpx.MustActor(c), app.DefineDiscountInput{
		Code:                 req.Code,
		NameAr:               req.NameAr,
		NameEn:               req.NameEn,
		Category:             discount.Category(req.Category),
		ExclusivityGroupID:   groupID,
		IsFullExemption:      req.IsFullExemption,
		AnnualReconfirmation: req.AnnualReconfirmation,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toDiscountDefinitionView(definition))
}

// AddDiscountVersion drafts a new configuration of a discount.
func (h *ConfigHandlers) AddDiscountVersion(c *gin.Context) {
	definitionID, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req AddDiscountVersionRequest
	if !bindJSON(c, &req) {
		return
	}

	validFromYearID, ok := optionalBodyID(c, req.ValidFromYearID)
	if !ok {
		return
	}
	validToYearID, ok := optionalBodyID(c, req.ValidToYearID)
	if !ok {
		return
	}

	in := app.AddDiscountVersionInput{
		DefinitionID:        definitionID,
		VersionNo:           req.VersionNo,
		ValueType:           discount.ValueType(req.ValueType),
		Rate:                money.BasisPoints(req.RateBP),
		FixedAmount:         money.FromInt64(req.FixedAmount),
		AppliesToComponents: req.AppliesToComponents,
		Stackable:           req.Stackable,
		Priority:            req.Priority,
		RequiresApproval:    req.RequiresApproval,
		RequiredDocuments:   req.RequiredDocuments,
		ValidFromYearID:     validFromYearID,
		ValidToYearID:       validToYearID,
		Notes:               req.Notes,
	}
	if req.PerApplicationCap != nil {
		in.PerApplicationCap = ptrAmount(*req.PerApplicationCap)
	}
	if req.ApprovalRole != nil {
		role := shared.Role(strings.TrimSpace(*req.ApprovalRole))
		in.ApprovalRole = &role
	}

	version, err := h.Config.AddDiscountVersion(requestContext(c), httpx.MustActor(c), in)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toDiscountVersionView(version))
}

// PublishDiscountVersion freezes a draft version. The publisher may not be the
// person who drafted it.
func (h *ConfigHandlers) PublishDiscountVersion(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	version, err := h.Config.PublishDiscountVersion(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toDiscountVersionView(version))
}

// ListDiscounts returns the discount catalogue.
func (h *ConfigHandlers) ListDiscounts(c *gin.Context) {
	// Retired entries are shown only on request: the catalogue a clerk picks
	// from should not offer discounts that can no longer be granted.
	activeOnly := c.Query("include_inactive") != "true"

	definitions, err := h.Config.ListDiscounts(requestContext(c), httpx.MustActor(c), activeOnly)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]DiscountDefinitionView, 0, len(definitions))
	for _, definition := range definitions {
		views = append(views, toDiscountDefinitionView(definition))
	}
	httpx.OK(c, views)
}

// GetDiscount returns a definition with the version currently in force.
func (h *ConfigHandlers) GetDiscount(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	detail, err := h.Config.GetDiscount(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	view := DiscountDetailView{Definition: toDiscountDefinitionView(detail.Definition)}
	if detail.PublishedVersion != nil {
		published := toDiscountVersionView(detail.PublishedVersion)
		view.PublishedVersion = &published
	}
	httpx.OK(c, view)
}

// GetDiscountVersion returns one version, draft or published, so the second
// pair of eyes can read a rate before putting it in force.
func (h *ConfigHandlers) GetDiscountVersion(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	version, err := h.Config.GetDiscountVersion(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toDiscountVersionView(version))
}

// ---------------------------------------------------------------------------
// Reference data handlers
// ---------------------------------------------------------------------------

// CreateCollege adds a faculty.
func (h *ConfigHandlers) CreateCollege(c *gin.Context) {
	var req CreateCollegeRequest
	if !bindJSON(c, &req) {
		return
	}
	college, err := h.Config.CreateCollege(requestContext(c), httpx.MustActor(c), app.CreateCollegeInput{
		Code:   req.Code,
		NameAr: req.NameAr,
		NameEn: req.NameEn,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toCollegeView(college))
}

// CreateDepartment adds a programme to a college.
func (h *ConfigHandlers) CreateDepartment(c *gin.Context) {
	var req CreateDepartmentRequest
	if !bindJSON(c, &req) {
		return
	}
	collegeID, err := shared.ParseID(req.CollegeID)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	department, err := h.Config.CreateDepartment(requestContext(c), httpx.MustActor(c), app.CreateDepartmentInput{
		CollegeID:  collegeID,
		Code:       req.Code,
		NameAr:     req.NameAr,
		NameEn:     req.NameEn,
		StageCount: req.StageCount,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toDepartmentView(department))
}

// CreateStudyType adds a mode of study.
//
// This is the endpoint that makes the design's claim true: when the ministry
// introduces a mode of study, admitting it is a form submission, not a
// release. Nothing in the system branches on a study-type code, so the new
// value is usable as a fee-policy dimension the moment it exists.
func (h *ConfigHandlers) CreateStudyType(c *gin.Context) {
	var req CreateStudyTypeRequest
	if !bindJSON(c, &req) {
		return
	}
	studyType, err := h.Config.CreateStudyType(requestContext(c), httpx.MustActor(c), app.CreateStudyTypeInput{
		Code:      req.Code,
		NameAr:    req.NameAr,
		NameEn:    req.NameEn,
		SortOrder: req.SortOrder,
	})
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toStudyTypeView(studyType))
}

// ---------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------

func toFeePolicyView(p *billing.FeePolicy) (FeePolicyView, error) {
	gross, err := p.GrossTotal()
	if err != nil {
		return FeePolicyView{}, err
	}

	view := FeePolicyView{
		ID:                p.ID.String(),
		PolicyCode:        p.PolicyCode,
		VersionNo:         p.VersionNo,
		AcademicYearID:    p.AcademicYearID.String(),
		CollegeID:         optionalIDString(p.CollegeID),
		DepartmentID:      optionalIDString(p.DepartmentID),
		Stage:             p.Stage,
		StudyTypeID:       optionalIDString(p.StudyTypeID),
		StudentCategoryID: optionalIDString(p.StudentCategoryID),
		SpecificityScore:  p.SpecificityScore,
		Status:            string(p.Status),
		MaxDiscountBP:     p.MaxDiscountBP.Int32(),
		Description:       p.Description,
		Components:        make([]FeeComponentView, 0, len(p.Components)),
		GrossTotal:        gross,
		PublishedAt:       p.PublishedAt,
		RetiredAt:         p.RetiredAt,
	}
	if p.EffectiveFrom != nil {
		view.EffectiveFrom = ptrString(p.EffectiveFrom.String())
	}
	for _, component := range p.Components {
		view.Components = append(view.Components, FeeComponentView{
			ID:             component.ID.String(),
			Code:           component.Code,
			NameAr:         component.NameAr,
			NameEn:         component.NameEn,
			Amount:         component.Amount,
			IsDiscountable: component.IsDiscountable,
			IsRefundable:   component.IsRefundable,
			IsMandatory:    component.IsMandatory,
			SortOrder:      component.SortOrder,
		})
	}
	return view, nil
}

func toResolutionPreviewView(p *app.FeeResolutionPreview) FeeResolutionPreviewView {
	view := FeeResolutionPreviewView{
		RunnersUp: make([]FeeResolutionCandidateView, 0, len(p.RunnersUp)),
	}
	for _, candidate := range p.RunnersUp {
		view.RunnersUp = append(view.RunnersUp, toResolutionCandidateView(candidate))
	}

	if p.Winner == nil {
		view.Explanation = "no published fee policy covers this scope, so the enrollment cannot be priced; " +
			"publish one covering it before generating the account"
		return view
	}

	winner := toResolutionCandidateView(p.Winner)
	view.Resolvable = true
	view.Winner = &winner
	view.Explanation = fmt.Sprintf(
		"%s wins with specificity %d, naming %s; it was ranked against %d other published %s",
		winner.PolicyCode,
		winner.SpecificityScore,
		describeDimensions(winner.Dimensions),
		len(view.RunnersUp),
		pluralPolicies(len(view.RunnersUp)),
	)
	return view
}

func toResolutionCandidateView(c *app.FeeResolutionCandidate) FeeResolutionCandidateView {
	return FeeResolutionCandidateView{
		PolicyID:         c.Policy.ID.String(),
		PolicyCode:       c.Policy.PolicyCode,
		VersionNo:        c.Policy.VersionNo,
		SpecificityScore: c.SpecificityScore,
		Dimensions:       c.Dimensions,
		GrossTotal:       c.GrossTotal,
		Wins:             c.Wins,
	}
}

// describeDimensions renders the scope a policy fixes. A policy naming none is
// the year's catch-all, and saying so is clearer than an empty list.
func describeDimensions(dimensions []string) string {
	if len(dimensions) == 0 {
		return "no dimension beyond the academic year (the year's catch-all)"
	}
	return strings.Join(dimensions, ", ")
}

func pluralPolicies(count int) string {
	if count == 1 {
		return "policy"
	}
	return "policies"
}

func toTemplateView(t *billing.InstallmentTemplate) InstallmentTemplateView {
	view := InstallmentTemplateView{
		ID:               t.ID.String(),
		Code:             t.Code,
		NameAr:           t.NameAr,
		NameEn:           t.NameEn,
		AcademicYearID:   optionalIDString(t.AcademicYearID),
		CollegeID:        optionalIDString(t.CollegeID),
		DepartmentID:     optionalIDString(t.DepartmentID),
		Stage:            t.Stage,
		StudyTypeID:      optionalIDString(t.StudyTypeID),
		SpecificityScore: t.SpecificityScore,
		MaxInstallments:  t.MaxInstallments,
		Status:           string(t.Status),
		Lines:            make([]TemplateLineView, 0, len(t.Lines)),
		PublishedAt:      t.PublishedAt,
		RetiredAt:        t.RetiredAt,
	}
	var total money.BasisPoints
	for _, line := range t.Lines {
		total += line.ShareBP
		var amount *int64
		if line.Amount != nil {
			amount = ptrInt64(line.Amount.Int64())
		}
		view.Lines = append(view.Lines, TemplateLineView{
			LineNo:        line.LineNo,
			ShareBP:       line.ShareBP.Int32(),
			SharePercent:  line.ShareBP.String(),
			Amount:        amount,
			DueOffsetDays: line.DueOffsetDays,
			Label:         line.Label,
		})
	}
	view.TotalBP = total.Int32()
	return view
}

func toDiscountDefinitionView(d *discount.Definition) DiscountDefinitionView {
	return DiscountDefinitionView{
		ID:                   d.ID.String(),
		Code:                 d.Code,
		NameAr:               d.NameAr,
		NameEn:               d.NameEn,
		Category:             string(d.Category),
		ExclusivityGroupID:   optionalIDString(d.ExclusivityGroupID),
		IsFullExemption:      d.IsFullExemption,
		AnnualReconfirmation: d.AnnualReconfirmation,
		IsActive:             d.IsActive,
	}
}

func toDiscountVersionView(v *discount.DefinitionVersion) DiscountVersionView {
	view := DiscountVersionView{
		ID:                  v.ID.String(),
		DefinitionID:        v.DefinitionID.String(),
		VersionNo:           v.VersionNo,
		ValueType:           string(v.ValueType),
		AppliesToComponents: v.AppliesToComponents,
		Stackable:           v.Stackable,
		Priority:            v.Priority,
		RequiresApproval:    v.RequiresApproval,
		RequiredDocuments:   v.RequiredDocuments,
		ValidFromYearID:     optionalIDString(v.ValidFromYearID),
		ValidToYearID:       optionalIDString(v.ValidToYearID),
		Status:              string(v.Status),
		Notes:               v.Notes,
		CreatedBy:           optionalIDString(v.CreatedBy),
		PublishedAt:         v.PublishedAt,
		PublishedBy:         optionalIDString(v.PublishedBy),
	}
	switch v.ValueType {
	case discount.ValuePercentage:
		view.RateBP = v.Rate.Int32()
		view.RatePercent = ptrString(v.Rate.String())
	case discount.ValueFixed:
		view.FixedAmount = v.FixedAmount.Int64()
	}
	if v.PerApplicationCap != nil {
		perApplication := v.PerApplicationCap.Int64()
		view.MaxPerAppCap = &perApplication
	}
	if v.ApprovalRole != nil {
		view.ApprovalRole = ptrString(string(*v.ApprovalRole))
	}
	return view
}

func toCollegeView(c *academic.College) CollegeView {
	return CollegeView{
		ID:       c.ID.String(),
		Code:     c.Code,
		NameAr:   c.NameAr,
		NameEn:   c.NameEn,
		IsActive: c.IsActive,
	}
}

func toDepartmentView(d *academic.Department) DepartmentView {
	return DepartmentView{
		ID:         d.ID.String(),
		CollegeID:  d.CollegeID.String(),
		Code:       d.Code,
		NameAr:     d.NameAr,
		NameEn:     d.NameEn,
		StageCount: d.StageCount,
		IsActive:   d.IsActive,
	}
}

func toStudyTypeView(s *academic.StudyType) StudyTypeView {
	return StudyTypeView{
		ID:        s.ID.String(),
		Code:      s.Code,
		NameAr:    s.NameAr,
		NameEn:    s.NameEn,
		SortOrder: s.SortOrder,
		IsActive:  s.IsActive,
	}
}

// ---------------------------------------------------------------------------
// Binding helpers
// ---------------------------------------------------------------------------

// optionalBodyID reads an optional identifier from a request body, responding
// on a malformed value.
//
// Unlike the query-parameter reader, an unparseable value here is a refusal
// rather than an absence: a body naming a college the client could not spell
// is a client defect, not a stale bookmark, and treating it as a wildcard
// would publish a price list covering every college in the university.
func optionalBodyID(c *gin.Context, raw *string) (*shared.ID, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	id, err := shared.ParseID(strings.TrimSpace(*raw))
	if err != nil {
		httpx.Respond(c, err)
		return nil, false
	}
	return &id, true
}

// requiredQueryID reads a query parameter that the endpoint cannot work
// without, responding when it is missing or malformed.
func requiredQueryID(c *gin.Context, name string) (shared.ID, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		httpx.Respond(c, shared.Validation("missing_query_parameter",
			"the %s query parameter is required", name))
		return shared.NilID, false
	}
	id, err := shared.ParseID(raw)
	if err != nil {
		httpx.Respond(c, err)
		return shared.NilID, false
	}
	return id, true
}

func optionalIDString(id *shared.ID) *string {
	if id == nil {
		return nil
	}
	return ptrString(id.String())
}

func ptrBasisPoints(bp int32) *money.BasisPoints {
	value := money.BasisPoints(bp)
	return &value
}

func ptrAmount(amount int64) *money.Amount {
	value := money.FromInt64(amount)
	return &value
}
