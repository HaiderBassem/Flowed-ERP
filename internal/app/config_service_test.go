package app_test

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"flowed/internal/app"
	"flowed/internal/domain/academic"
	"flowed/internal/domain/billing"
	"flowed/internal/domain/discount"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// installmentTemplateCodePattern mirrors ck_installment_template_code, the
// one constraint a fake repository cannot enforce.
var installmentTemplateCodePattern = regexp.MustCompile(`^[A-Z0-9_]{2,32}$`)

// ---------------------------------------------------------------------------
// Fakes
//
// Each fake embeds the port interface it stands for and implements only the
// methods the configuration commands actually call. Anything else panics on
// the nil embedded interface, which is the behaviour worth having: a stub
// returning a helpful zero value would let one of these tests keep passing
// after the service quietly stopped checking something.
//
// The read methods return copies, as the PostgreSQL adapter does. That detail
// matters here — a fake handing back the stored pointer would let a domain
// method mutate the store before the repository was asked to, and hide the
// difference between "the object says published" and "the row says published".
// ---------------------------------------------------------------------------

type cfgTx struct{}

func (cfgTx) Write(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }
func (cfgTx) Read(ctx context.Context, fn func(ctx context.Context) error) error  { return fn(ctx) }
func (cfgTx) RequireTx(context.Context, string) error                             { return nil }

type cfgAuditRepo struct {
	port.AuditRepository
	entries []port.AuditEntry
}

func (r *cfgAuditRepo) Append(_ context.Context, entry port.AuditEntry) error {
	r.entries = append(r.entries, entry)
	return nil
}

func (r *cfgAuditRepo) actions() []string {
	out := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Action)
	}
	return out
}

type cfgYearRepo struct {
	port.AcademicYearRepository
	years map[shared.ID]*academic.Year
}

func (r *cfgYearRepo) GetByID(_ context.Context, id shared.ID) (*academic.Year, error) {
	year, ok := r.years[id]
	if !ok {
		return nil, shared.NotFound("academic_year.not_found", "no such year")
	}
	clone := *year
	return &clone, nil
}

type cfgReferenceRepo struct {
	port.ReferenceRepository
	colleges    map[shared.ID]*academic.College
	departments map[shared.ID]*academic.Department
	studyTypes  map[shared.ID]*academic.StudyType
	categories  map[string]*academic.StudentCategory
}

func (r *cfgReferenceRepo) GetCollege(_ context.Context, id shared.ID) (*academic.College, error) {
	college, ok := r.colleges[id]
	if !ok {
		return nil, shared.NotFound("college.not_found", "no such college")
	}
	clone := *college
	return &clone, nil
}

func (r *cfgReferenceRepo) CreateCollege(_ context.Context, c *academic.College) error {
	r.colleges[c.ID] = c
	return nil
}

func (r *cfgReferenceRepo) GetDepartment(_ context.Context, id shared.ID) (*academic.Department, error) {
	department, ok := r.departments[id]
	if !ok {
		return nil, shared.NotFound("department.not_found", "no such department")
	}
	clone := *department
	return &clone, nil
}

func (r *cfgReferenceRepo) CreateDepartment(_ context.Context, d *academic.Department) error {
	r.departments[d.ID] = d
	return nil
}

func (r *cfgReferenceRepo) CreateStudyType(_ context.Context, s *academic.StudyType) error {
	r.studyTypes[s.ID] = s
	return nil
}

func (r *cfgReferenceRepo) GetStudyType(_ context.Context, id shared.ID) (*academic.StudyType, error) {
	studyType, ok := r.studyTypes[id]
	if !ok {
		return nil, shared.NotFound("study_type.not_found", "no such study type")
	}
	clone := *studyType
	return &clone, nil
}

func (r *cfgReferenceRepo) GetStudentCategoryByCode(_ context.Context, code string) (*academic.StudentCategory, error) {
	category, ok := r.categories[code]
	if !ok {
		return nil, shared.NotFound("student_category.not_found", "no such category %q", code)
	}
	clone := *category
	return &clone, nil
}

type cfgFeePolicyRepo struct {
	port.FeePolicyRepository
	policies []*billing.FeePolicy
}

func (r *cfgFeePolicyRepo) Create(_ context.Context, p *billing.FeePolicy) error {
	r.policies = append(r.policies, clonePolicy(p))
	return nil
}

func (r *cfgFeePolicyRepo) GetByID(_ context.Context, id shared.ID) (*billing.FeePolicy, error) {
	for _, p := range r.policies {
		if p.ID == id {
			return clonePolicy(p), nil
		}
	}
	return nil, shared.NotFound("fee_policy.not_found", "no such policy")
}

func (r *cfgFeePolicyRepo) List(_ context.Context, yearID shared.ID) ([]*billing.FeePolicy, error) {
	out := make([]*billing.FeePolicy, 0, len(r.policies))
	for _, p := range r.policies {
		if p.AcademicYearID == yearID {
			out = append(out, clonePolicy(p))
		}
	}
	return out, nil
}

func (r *cfgFeePolicyRepo) Publish(_ context.Context, policyID, actor shared.ID, at time.Time) error {
	for _, p := range r.policies {
		if p.ID != policyID {
			continue
		}
		if p.Status != billing.PolicyDraft {
			return shared.PreconditionFailed("fee_policy.not_publishable", "not a draft")
		}
		p.Status = billing.PolicyPublished
		p.PublishedAt, p.PublishedBy = &at, &actor
		return nil
	}
	return shared.PreconditionFailed("fee_policy.not_publishable", "unknown policy")
}

// Resolve mirrors the real repository's rule closely enough for a test that
// wants to confirm a policy actually prices something: the highest-specificity
// published policy whose non-null dimensions all match the scope.
func (r *cfgFeePolicyRepo) Resolve(_ context.Context, scope port.FeeScope) (*billing.FeePolicy, error) {
	var best *billing.FeePolicy
	for _, p := range r.policies {
		if p.Status != billing.PolicyPublished || p.AcademicYearID != scope.AcademicYearID {
			continue
		}
		if p.CollegeID != nil && *p.CollegeID != scope.CollegeID {
			continue
		}
		if p.DepartmentID != nil && *p.DepartmentID != scope.DepartmentID {
			continue
		}
		if p.Stage != nil && *p.Stage != scope.Stage {
			continue
		}
		if p.StudyTypeID != nil && *p.StudyTypeID != scope.StudyTypeID {
			continue
		}
		if p.StudentCategoryID != nil && *p.StudentCategoryID != scope.StudentCategoryID {
			continue
		}
		if best == nil || p.SpecificityScore > best.SpecificityScore {
			best = p
		}
	}
	if best == nil {
		return nil, shared.NotFound("fee_policy.no_match", "no published fee policy matches this scope")
	}
	return clonePolicy(best), nil
}

func (r *cfgFeePolicyRepo) Retire(_ context.Context, policyID shared.ID, at time.Time) error {
	for _, p := range r.policies {
		if p.ID != policyID {
			continue
		}
		if p.Status != billing.PolicyPublished {
			return shared.PreconditionFailed("fee_policy.not_retirable", "not published")
		}
		p.Status = billing.PolicyRetired
		p.RetiredAt = &at
		return nil
	}
	return shared.PreconditionFailed("fee_policy.not_retirable", "unknown policy")
}

func clonePolicy(p *billing.FeePolicy) *billing.FeePolicy {
	clone := *p
	clone.Components = make([]*billing.FeeComponent, 0, len(p.Components))
	for _, component := range p.Components {
		copied := *component
		clone.Components = append(clone.Components, &copied)
	}
	return &clone
}

type cfgTemplateRepo struct {
	port.InstallmentTemplateRepository
	templates []*billing.InstallmentTemplate
}

func (r *cfgTemplateRepo) Create(_ context.Context, t *billing.InstallmentTemplate) error {
	r.templates = append(r.templates, cloneTemplate(t))
	return nil
}

func (r *cfgTemplateRepo) GetByID(_ context.Context, id shared.ID) (*billing.InstallmentTemplate, error) {
	for _, t := range r.templates {
		if t.ID == id {
			return cloneTemplate(t), nil
		}
	}
	return nil, shared.NotFound("installment_template.not_found", "no such template")
}

// List mirrors the adapter: a template with no year is global and appears for
// every year.
func (r *cfgTemplateRepo) List(_ context.Context, yearID *shared.ID) ([]*billing.InstallmentTemplate, error) {
	out := make([]*billing.InstallmentTemplate, 0, len(r.templates))
	for _, t := range r.templates {
		switch {
		case yearID == nil, t.AcademicYearID == nil, *t.AcademicYearID == *yearID:
			out = append(out, cloneTemplate(t))
		}
	}
	return out, nil
}

func (r *cfgTemplateRepo) Publish(_ context.Context, templateID, actor shared.ID, at time.Time) error {
	for _, t := range r.templates {
		if t.ID != templateID {
			continue
		}
		if t.Status != billing.PolicyDraft {
			return shared.PreconditionFailed("installment_template.not_publishable", "not a draft")
		}
		t.Status = billing.PolicyPublished
		t.PublishedAt, t.PublishedBy = &at, &actor
		return nil
	}
	return shared.PreconditionFailed("installment_template.not_publishable", "unknown template")
}

func (r *cfgTemplateRepo) Retire(_ context.Context, templateID shared.ID, at time.Time) error {
	for _, t := range r.templates {
		if t.ID != templateID {
			continue
		}
		if t.Status != billing.PolicyPublished {
			return shared.PreconditionFailed("installment_template.not_retirable", "not published")
		}
		t.Status = billing.PolicyRetired
		t.RetiredAt = &at
		return nil
	}
	return shared.PreconditionFailed("installment_template.not_retirable", "unknown template")
}

// Resolve mirrors the real repository closely enough for a test: the
// highest-specificity published template whose non-null dimensions all match.
func (r *cfgTemplateRepo) Resolve(_ context.Context, scope port.FeeScope) (*billing.InstallmentTemplate, error) {
	var best *billing.InstallmentTemplate
	for _, t := range r.templates {
		if t.Status != billing.PolicyPublished {
			continue
		}
		if t.AcademicYearID != nil && *t.AcademicYearID != scope.AcademicYearID {
			continue
		}
		if t.CollegeID != nil && *t.CollegeID != scope.CollegeID {
			continue
		}
		if t.DepartmentID != nil && *t.DepartmentID != scope.DepartmentID {
			continue
		}
		if t.Stage != nil && *t.Stage != scope.Stage {
			continue
		}
		if t.StudyTypeID != nil && *t.StudyTypeID != scope.StudyTypeID {
			continue
		}
		if best == nil || t.SpecificityScore > best.SpecificityScore {
			best = t
		}
	}
	if best == nil {
		return nil, shared.NotFound("installment_template.unresolved", "no published template matches this scope")
	}
	return cloneTemplate(best), nil
}

func cloneTemplate(t *billing.InstallmentTemplate) *billing.InstallmentTemplate {
	clone := *t
	clone.Lines = append([]billing.TemplateLine(nil), t.Lines...)
	return &clone
}

type cfgDiscountRepo struct {
	port.DiscountRepository
	definitions []*discount.Definition
	versions    []*discount.DefinitionVersion
}

func (r *cfgDiscountRepo) CreateDefinition(_ context.Context, d *discount.Definition) error {
	clone := *d
	r.definitions = append(r.definitions, &clone)
	return nil
}

func (r *cfgDiscountRepo) GetDefinition(_ context.Context, id shared.ID) (*discount.Definition, error) {
	for _, d := range r.definitions {
		if d.ID == id {
			clone := *d
			return &clone, nil
		}
	}
	return nil, shared.NotFound("discount.definition_not_found", "no such definition")
}

func (r *cfgDiscountRepo) CreateVersion(_ context.Context, v *discount.DefinitionVersion) error {
	clone := *v
	r.versions = append(r.versions, &clone)
	return nil
}

func (r *cfgDiscountRepo) GetVersion(_ context.Context, id shared.ID) (*discount.DefinitionVersion, error) {
	for _, v := range r.versions {
		if v.ID == id {
			clone := *v
			return &clone, nil
		}
	}
	return nil, shared.NotFound("discount.version_not_found", "no such version")
}

func (r *cfgDiscountRepo) GetPublishedVersion(_ context.Context, definitionID shared.ID) (*discount.DefinitionVersion, error) {
	for _, v := range r.versions {
		if v.DefinitionID == definitionID && v.Status == discount.VersionPublished {
			clone := *v
			return &clone, nil
		}
	}
	return nil, shared.NotFound("discount.no_published_version", "no version in force")
}

func (r *cfgDiscountRepo) PublishVersion(_ context.Context, versionID, actor shared.ID, at time.Time) error {
	for _, v := range r.versions {
		if v.ID != versionID {
			continue
		}
		if v.Status != discount.VersionDraft {
			return shared.PreconditionFailed("discount.version_not_draft", "not a draft")
		}
		v.Status = discount.VersionPublished
		v.PublishedAt, v.PublishedBy = &at, &actor
		return nil
	}
	return shared.PreconditionFailed("discount.version_not_draft", "unknown version")
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type cfgFixture struct {
	service   *app.ConfigService
	years     *cfgYearRepo
	reference *cfgReferenceRepo
	policies  *cfgFeePolicyRepo
	templates *cfgTemplateRepo
	discounts *cfgDiscountRepo
	audit     *cfgAuditRepo

	openYear   *academic.Year
	closedYear *academic.Year
	college    *academic.College
	otherDept  *academic.Department
	department *academic.Department
	studyType  *academic.StudyType
	category   *academic.StudentCategory
}

var (
	financeManager = shared.Actor{
		UserID:   shared.NewID(),
		Username: "finance.one",
		Roles:    []shared.Role{shared.RoleAdmin},
	}
	secondManager = shared.Actor{
		UserID:   shared.NewID(),
		Username: "finance.two",
		Roles:    []shared.Role{shared.RoleAdmin},
	}
	administrator = shared.Actor{
		UserID:   shared.NewID(),
		Username: "admin",
		Roles:    []shared.Role{shared.RoleAdmin},
	}
)

func newCfgFixture(t *testing.T) *cfgFixture {
	t.Helper()

	openYear := &academic.Year{
		ID:        shared.NewID(),
		Code:      "2025-2026",
		StartDate: shared.NewDate(2025, time.September, 1),
		EndDate:   shared.NewDate(2026, time.June, 30),
		Status:    academic.YearOpen,
	}
	closedYear := &academic.Year{
		ID:        shared.NewID(),
		Code:      "2023-2024",
		StartDate: shared.NewDate(2023, time.September, 1),
		EndDate:   shared.NewDate(2024, time.June, 30),
		Status:    academic.YearClosed,
	}
	college := &academic.College{ID: shared.NewID(), Code: "ENG", NameAr: "الهندسة", IsActive: true}
	department := &academic.Department{
		ID: shared.NewID(), CollegeID: college.ID, Code: "CIVIL",
		NameAr: "المدني", StageCount: 4, IsActive: true,
	}
	otherCollege := &academic.College{ID: shared.NewID(), Code: "MED", NameAr: "الطب", IsActive: true}
	otherDept := &academic.Department{
		ID: shared.NewID(), CollegeID: otherCollege.ID, Code: "SURGERY",
		NameAr: "الجراحة", StageCount: 5, IsActive: true,
	}
	studyType := &academic.StudyType{ID: shared.NewID(), Code: academic.StudyTypeMorning, NameAr: "صباحي", IsActive: true}
	category := &academic.StudentCategory{ID: shared.NewID(), Code: academic.CategoryRegular, NameAr: "نظامي", IsActive: true}

	fixture := &cfgFixture{
		years: &cfgYearRepo{years: map[shared.ID]*academic.Year{
			openYear.ID:   openYear,
			closedYear.ID: closedYear,
		}},
		reference: &cfgReferenceRepo{
			colleges: map[shared.ID]*academic.College{
				college.ID:      college,
				otherCollege.ID: otherCollege,
			},
			departments: map[shared.ID]*academic.Department{
				department.ID: department,
				otherDept.ID:  otherDept,
			},
			studyTypes: map[shared.ID]*academic.StudyType{studyType.ID: studyType},
			categories: map[string]*academic.StudentCategory{category.Code: category},
		},
		policies:   &cfgFeePolicyRepo{},
		templates:  &cfgTemplateRepo{},
		discounts:  &cfgDiscountRepo{},
		audit:      &cfgAuditRepo{},
		openYear:   openYear,
		closedYear: closedYear,
		college:    college,
		department: department,
		otherDept:  otherDept,
		studyType:  studyType,
		category:   category,
	}

	fixture.service = app.NewConfigService(app.Deps{
		Tx:          cfgTx{},
		Years:       fixture.years,
		Reference:   fixture.reference,
		FeePolicies: fixture.policies,
		Templates:   fixture.templates,
		Discounts:   fixture.discounts,
		Audit:       fixture.audit,
		Clock:       shared.FixedClock{Instant: time.Date(2025, time.September, 15, 9, 0, 0, 0, time.UTC)},
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return fixture
}

// tuition is the ordinary two-component fee: discountable tuition plus a
// registration charge that an exemption must not zero.
func tuition(amount int64) []app.FeeComponentInput {
	no := false
	return []app.FeeComponentInput{
		{Code: "TUITION", NameAr: "القسط الدراسي", Amount: money.FromInt64(amount)},
		{Code: "REGISTRATION", NameAr: "رسوم التسجيل", Amount: money.FromInt64(50_000), IsDiscountable: &no},
	}
}

func requireCode(t *testing.T, err error, want string) *shared.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", want)
	}
	domainErr, ok := shared.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T: %v", err, err)
	}
	if domainErr.Code != want {
		t.Fatalf("error code = %q, want %q (message: %s)", domainErr.Code, want, domainErr.Message)
	}
	return domainErr
}

// ---------------------------------------------------------------------------
// Fee policies
// ---------------------------------------------------------------------------

func TestDefineFeePolicyRejectsDuplicateComponentCodes(t *testing.T) {
	f := newCfgFixture(t)

	_, err := f.service.DefineFeePolicy(context.Background(), financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		Components: []app.FeeComponentInput{
			{Code: "TUITION", NameAr: "القسط الدراسي", Amount: money.FromInt64(2_000_000)},
			{Code: "tuition", NameAr: "قسط إضافي", Amount: money.FromInt64(500_000)},
		},
	})

	domainErr := requireCode(t, err, "fee_policy.duplicate_component")
	if got := domainErr.Details["component_code"]; got != "TUITION" {
		t.Errorf("detail component_code = %v, want TUITION", got)
	}
	// The codes differ only in case, and the store folds them: catching it here
	// is the difference between a clear refusal and a unique-index violation.
	if len(f.policies.policies) != 0 {
		t.Errorf("a rejected policy was written: %d rows", len(f.policies.policies))
	}
	if len(f.audit.entries) != 0 {
		t.Errorf("a rejected policy was audited: %v", f.audit.actions())
	}
}

func TestDefineFeePolicyRejectsNegativeAndUnpricedPolicies(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	_, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		Components: []app.FeeComponentInput{
			{Code: "TUITION", NameAr: "القسط", Amount: money.FromInt64(-1)},
		},
	})
	requireCode(t, err, "fee_policy.negative_component")

	_, err = f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
	})
	requireCode(t, err, "fee_policy.no_components")
}

func TestDefineFeePolicyRefusesDepartmentFromAnotherCollege(t *testing.T) {
	f := newCfgFixture(t)

	_, err := f.service.DefineFeePolicy(context.Background(), financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		DepartmentID:   &f.otherDept.ID,
		Components:     tuition(2_000_000),
	})

	// Left to the database this row would insert cleanly and simply never
	// match an enrollment: a price list that looks right and charges nobody.
	requireCode(t, err, "config.department_college_mismatch")
}

func TestDefineFeePolicyRefusesAClosedYear(t *testing.T) {
	f := newCfgFixture(t)

	_, err := f.service.DefineFeePolicy(context.Background(), financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2023",
		AcademicYearID: f.closedYear.ID,
		Components:     tuition(2_000_000),
	})

	requireCode(t, err, "config.year_not_configurable")
}

func TestDefineFeePolicyDefaultsAndAudits(t *testing.T) {
	f := newCfgFixture(t)

	policy, err := f.service.DefineFeePolicy(context.Background(), financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_CIVIL_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		DepartmentID:   &f.department.ID,
		Components:     tuition(2_000_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy: %v", err)
	}

	if policy.Status != billing.PolicyDraft {
		t.Errorf("status = %s, want draft: a new policy must not price anybody yet", policy.Status)
	}
	if policy.SpecificityScore != 24 {
		t.Errorf("specificity = %d, want 24 (college 16 + department 8)", policy.SpecificityScore)
	}
	if policy.MaxDiscountBP != money.FullRate {
		t.Errorf("max discount = %s, want the full rate when unspecified", policy.MaxDiscountBP)
	}
	// The flags default to true; only the registration charge was told otherwise.
	for _, component := range policy.Components {
		wantDiscountable := component.Code == "TUITION"
		if component.IsDiscountable != wantDiscountable {
			t.Errorf("%s discountable = %v, want %v", component.Code, component.IsDiscountable, wantDiscountable)
		}
		if !component.IsMandatory || (component.Code == "TUITION" && !component.IsRefundable) {
			t.Errorf("%s lost a flag that defaults to true: %+v", component.Code, component)
		}
	}

	gross, err := policy.GrossTotal()
	if err != nil {
		t.Fatalf("GrossTotal: %v", err)
	}
	if gross != money.FromInt64(2_050_000) {
		t.Errorf("gross total = %s, want 2050000", gross)
	}

	if got := f.audit.actions(); len(got) != 1 || got[0] != "fee_policy.defined" {
		t.Errorf("audit actions = %v, want one fee_policy.defined", got)
	}
}

func TestPublishFeePolicyRefusesASecondPolicyOverTheSameScope(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	define := func(code string) *billing.FeePolicy {
		t.Helper()
		policy, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
			PolicyCode:     code,
			AcademicYearID: f.openYear.ID,
			CollegeID:      &f.college.ID,
			Components:     tuition(2_000_000),
		})
		if err != nil {
			t.Fatalf("DefineFeePolicy(%s): %v", code, err)
		}
		return policy
	}

	first := define("ENG_2025_A")
	second := define("ENG_2025_B")

	if _, err := f.service.PublishFeePolicy(ctx, financeManager, first.ID); err != nil {
		t.Fatalf("publishing the first policy: %v", err)
	}

	_, err := f.service.PublishFeePolicy(ctx, financeManager, second.ID)
	domainErr := requireCode(t, err, "fee_policy.duplicate_scope")

	// The refusal has to say why the constraint exists, and name the row
	// holding the scope — otherwise the administrator's only move is guesswork.
	if got := domainErr.Details["held_by_policy_code"]; got != "ENG_2025_A" {
		t.Errorf("detail held_by_policy_code = %v, want ENG_2025_A", got)
	}
	if !strings.Contains(domainErr.Message, "single winner") {
		t.Errorf("message does not explain the constraint: %s", domainErr.Message)
	}
	if domainErr.Details["remedy"] == nil {
		t.Error("the conflict offers no remedy")
	}

	stored, err := f.policies.GetByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != billing.PolicyDraft {
		t.Errorf("the refused policy was published anyway: %s", stored.Status)
	}
}

func TestPublishFeePolicyFreezesAndAudits(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	policy, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		Components:     tuition(2_000_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy: %v", err)
	}

	published, err := f.service.PublishFeePolicy(ctx, secondManager, policy.ID)
	if err != nil {
		t.Fatalf("PublishFeePolicy: %v", err)
	}
	if published.Status != billing.PolicyPublished {
		t.Errorf("status = %s, want published", published.Status)
	}
	if published.PublishedBy == nil || *published.PublishedBy != secondManager.UserID {
		t.Errorf("published_by = %v, want the publishing actor", published.PublishedBy)
	}

	if got := f.audit.actions(); len(got) != 2 || got[1] != "fee_policy.published" {
		t.Errorf("audit actions = %v, want a published entry after the defined one", got)
	}
}

// ---------------------------------------------------------------------------
// Fee resolution preview
// ---------------------------------------------------------------------------

func TestPreviewFeeResolutionPicksTheHigherSpecificity(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	publish := func(in app.DefineFeePolicyInput) *billing.FeePolicy {
		t.Helper()
		policy, err := f.service.DefineFeePolicy(ctx, financeManager, in)
		if err != nil {
			t.Fatalf("DefineFeePolicy(%s): %v", in.PolicyCode, err)
		}
		published, err := f.service.PublishFeePolicy(ctx, financeManager, policy.ID)
		if err != nil {
			t.Fatalf("PublishFeePolicy(%s): %v", in.PolicyCode, err)
		}
		return published
	}

	stage := int16(2)

	// The year's catch-all: no dimension named at all.
	blanket := publish(app.DefineFeePolicyInput{
		PolicyCode:     "UNIVERSITY_2025",
		AcademicYearID: f.openYear.ID,
		Components:     tuition(1_000_000),
	})
	// Narrower: the whole college.
	collegeWide := publish(app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		Components:     tuition(2_000_000),
	})
	// Narrowest: college, department and stage.
	precise := publish(app.DefineFeePolicyInput{
		PolicyCode:     "ENG_CIVIL_S2_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		DepartmentID:   &f.department.ID,
		Stage:          &stage,
		Components:     tuition(2_500_000),
	})
	// Narrower still, but never published: a draft prices nobody.
	draftStudyType, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_CIVIL_S2_MORNING_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		DepartmentID:   &f.department.ID,
		Stage:          &stage,
		StudyTypeID:    &f.studyType.ID,
		Components:     tuition(9_000_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy(draft): %v", err)
	}
	// Published, but for a different stage: it must not appear at all.
	otherStage := int16(3)
	publish(app.DefineFeePolicyInput{
		PolicyCode:     "ENG_CIVIL_S3_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		DepartmentID:   &f.department.ID,
		Stage:          &otherStage,
		Components:     tuition(2_600_000),
	})

	preview, err := f.service.PreviewFeeResolution(ctx, financeManager, app.FeeResolutionInput{
		AcademicYearID: f.openYear.ID,
		CollegeID:      f.college.ID,
		DepartmentID:   f.department.ID,
		Stage:          stage,
		StudyTypeID:    f.studyType.ID,
	})
	if err != nil {
		t.Fatalf("PreviewFeeResolution: %v", err)
	}

	if preview.Winner == nil {
		t.Fatal("no winner: three published policies cover this scope")
	}
	if preview.Winner.Policy.ID != precise.ID {
		t.Errorf("winner = %s, want %s", preview.Winner.Policy.PolicyCode, precise.PolicyCode)
	}
	if preview.Winner.SpecificityScore != 28 {
		t.Errorf("winning score = %d, want 28 (college 16 + department 8 + stage 4)",
			preview.Winner.SpecificityScore)
	}
	if !preview.Winner.Wins {
		t.Error("the winner is not flagged as winning")
	}
	if preview.Winner.GrossTotal != money.FromInt64(2_550_000) {
		t.Errorf("winning gross = %s, want 2550000", preview.Winner.GrossTotal)
	}

	// The runners-up are the answer to "but what about the college rate": they
	// are listed, in the order they lost.
	if len(preview.RunnersUp) != 2 {
		t.Fatalf("runners-up = %d, want 2 (the college rate and the catch-all)", len(preview.RunnersUp))
	}
	if preview.RunnersUp[0].Policy.ID != collegeWide.ID || preview.RunnersUp[0].SpecificityScore != 16 {
		t.Errorf("first runner-up = %s (%d), want %s (16)",
			preview.RunnersUp[0].Policy.PolicyCode, preview.RunnersUp[0].SpecificityScore, collegeWide.PolicyCode)
	}
	if preview.RunnersUp[1].Policy.ID != blanket.ID || preview.RunnersUp[1].SpecificityScore != 0 {
		t.Errorf("second runner-up = %s (%d), want %s (0)",
			preview.RunnersUp[1].Policy.PolicyCode, preview.RunnersUp[1].SpecificityScore, blanket.PolicyCode)
	}
	for _, candidate := range preview.RunnersUp {
		if candidate.Policy.ID == draftStudyType.ID {
			t.Error("a draft policy appeared in the ranking; drafts are invisible to resolution")
		}
		if candidate.Wins {
			t.Errorf("%s is flagged as winning", candidate.Policy.PolicyCode)
		}
	}

	if got := preview.Winner.Dimensions; strings.Join(got, ",") != "college,department,stage" {
		t.Errorf("dimensions = %v, want the derivation of the score", got)
	}
	if preview.Scope.StudentCategoryID != f.category.ID {
		t.Error("an omitted category did not default to REGULAR")
	}
}

func TestPreviewFeeResolutionReportsAnUnpricedScope(t *testing.T) {
	f := newCfgFixture(t)

	preview, err := f.service.PreviewFeeResolution(context.Background(), financeManager, app.FeeResolutionInput{
		AcademicYearID: f.openYear.ID,
		CollegeID:      f.college.ID,
		DepartmentID:   f.department.ID,
		Stage:          1,
		StudyTypeID:    f.studyType.ID,
	})
	if err != nil {
		t.Fatalf("PreviewFeeResolution: %v", err)
	}
	// "Nothing covers this student" is the answer the caller asked for, and it
	// is worth far more before the account is generated than as a failure at
	// the counter afterwards.
	if preview.Winner != nil {
		t.Errorf("winner = %s, want none", preview.Winner.Policy.PolicyCode)
	}
	if len(preview.RunnersUp) != 0 {
		t.Errorf("runners-up = %d, want none", len(preview.RunnersUp))
	}
}

// ---------------------------------------------------------------------------
// Installment templates
// ---------------------------------------------------------------------------

func defineTemplate(t *testing.T, f *cfgFixture, code string, shares ...int32) *billing.InstallmentTemplate {
	t.Helper()

	lines := make([]app.TemplateLineInput, 0, len(shares))
	for i, share := range shares {
		lines = append(lines, app.TemplateLineInput{
			ShareBP:       money.BasisPoints(share),
			DueOffsetDays: i * 60,
		})
	}

	template, err := f.service.DefineInstallmentTemplate(context.Background(), financeManager,
		app.DefineInstallmentTemplateInput{
			Code:           code,
			NameAr:         "خطة التقسيط",
			AcademicYearID: &f.openYear.ID,
			Lines:          lines,
		})
	if err != nil {
		t.Fatalf("DefineInstallmentTemplate(%s): %v", code, err)
	}
	return template
}

func TestPublishInstallmentTemplateNamesTheActualShareTotal(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	// 25% + 25% + 25% + 24% — the ordinary typing mistake.
	template := defineTemplate(t, f, "QUARTERLY", 2500, 2500, 2500, 2400)

	_, err := f.service.PublishInstallmentTemplate(ctx, financeManager, template.ID)
	domainErr := requireCode(t, err, "installment_template.shares_do_not_total")

	// The number the administrator needs is the one they actually typed. Being
	// told only "the shares must total 100%" leaves them adding four figures up
	// by hand; being told 9900 points straight at the short line.
	if !strings.Contains(domainErr.Message, "9900") {
		t.Errorf("message does not name the actual total: %s", domainErr.Message)
	}
	if got := domainErr.Details["total_bp"]; got != 9900 {
		t.Errorf("detail total_bp = %v, want 9900", got)
	}
	if got := domainErr.Details["short_by_bp"]; got != 100 {
		t.Errorf("detail short_by_bp = %v, want 100", got)
	}
	if got := domainErr.Details["template_code"]; got != "QUARTERLY" {
		t.Errorf("detail template_code = %v, want QUARTERLY", got)
	}

	stored, err := f.templates.GetByID(ctx, template.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.Status != billing.PolicyDraft {
		t.Errorf("a template that does not add up was published: %s", stored.Status)
	}
}

func TestPublishInstallmentTemplateReportsAnOverfullPlan(t *testing.T) {
	f := newCfgFixture(t)

	template := defineTemplate(t, f, "OVERFULL", 5000, 5000, 500)

	_, err := f.service.PublishInstallmentTemplate(context.Background(), financeManager, template.ID)
	domainErr := requireCode(t, err, "installment_template.shares_do_not_total")
	if got := domainErr.Details["over_by_bp"]; got != 500 {
		t.Errorf("detail over_by_bp = %v, want 500", got)
	}
}

func TestPublishInstallmentTemplateAcceptsAnUnevenPlanThatAddsUp(t *testing.T) {
	f := newCfgFixture(t)

	// Unequal shares are ordinary in Iraq: a larger payment at registration,
	// then smaller ones.
	template := defineTemplate(t, f, "UNEVEN", 4000, 2000, 2000, 2000)

	published, err := f.service.PublishInstallmentTemplate(context.Background(), financeManager, template.ID)
	if err != nil {
		t.Fatalf("PublishInstallmentTemplate: %v", err)
	}
	if published.Status != billing.PolicyPublished {
		t.Errorf("status = %s, want published", published.Status)
	}
	// Lines were numbered in the order they were given.
	for i, line := range published.Lines {
		if line.LineNo != int16(i+1) {
			t.Errorf("line %d numbered %d", i, line.LineNo)
		}
	}
	if got := f.audit.actions(); len(got) != 2 || got[1] != "installment_template.published" {
		t.Errorf("audit actions = %v, want a published entry", got)
	}
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

func defineDiscountVersion(t *testing.T, f *cfgFixture, author shared.Actor) *discount.DefinitionVersion {
	t.Helper()
	ctx := context.Background()

	definition, err := f.service.DefineDiscount(ctx, author, app.DefineDiscountInput{
		Code:     "STAFF_CHILD",
		NameAr:   "أبناء الموظفين",
		Category: discount.CategoryStaff,
	})
	if err != nil {
		t.Fatalf("DefineDiscount: %v", err)
	}

	version, err := f.service.AddDiscountVersion(ctx, author, app.AddDiscountVersionInput{
		DefinitionID: definition.ID,
		ValueType:    discount.ValuePercentage,
		Rate:         money.BasisPoints(2500),
	})
	if err != nil {
		t.Fatalf("AddDiscountVersion: %v", err)
	}
	return version
}

func TestPublishDiscountVersionAcceptsASecondPerson(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	version := defineDiscountVersion(t, f, financeManager)

	published, err := f.service.PublishDiscountVersion(ctx, secondManager, version.ID)
	if err != nil {
		t.Fatalf("PublishDiscountVersion: %v", err)
	}
	if published.Status != discount.VersionPublished {
		t.Errorf("status = %s, want published", published.Status)
	}
	if published.PublishedBy == nil || *published.PublishedBy != secondManager.UserID {
		t.Errorf("published_by = %v, want the second manager", published.PublishedBy)
	}
	if published.CreatedBy == nil || *published.CreatedBy != financeManager.UserID {
		t.Errorf("created_by = %v, want the author; the four-eyes check has nothing to compare without it",
			published.CreatedBy)
	}

	// The version in force is now what a grant made today would use.
	inForce, err := f.discounts.GetPublishedVersion(ctx, version.DefinitionID)
	if err != nil {
		t.Fatalf("GetPublishedVersion: %v", err)
	}
	if inForce.ID != version.ID {
		t.Errorf("version in force = %s, want %s", inForce.ID, version.ID)
	}

	if got := f.audit.actions(); len(got) != 3 || got[2] != "discount.version_published" {
		t.Errorf("audit actions = %v, want definition, version, publication", got)
	}
}

func TestAddDiscountVersionFollowsTheVersionInForce(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	first := defineDiscountVersion(t, f, financeManager)
	if first.VersionNo != 1 {
		t.Fatalf("first version = %d, want 1", first.VersionNo)
	}
	if _, err := f.service.PublishDiscountVersion(ctx, secondManager, first.ID); err != nil {
		t.Fatalf("PublishDiscountVersion: %v", err)
	}

	// Raising the rate is a new version, never an edit: the published row is
	// what every application already computed against.
	second, err := f.service.AddDiscountVersion(ctx, financeManager, app.AddDiscountVersionInput{
		DefinitionID: first.DefinitionID,
		ValueType:    discount.ValuePercentage,
		Rate:         money.BasisPoints(3000),
	})
	if err != nil {
		t.Fatalf("AddDiscountVersion: %v", err)
	}
	if second.VersionNo != 2 {
		t.Errorf("second version = %d, want 2", second.VersionNo)
	}
	if second.Status != discount.VersionDraft {
		t.Errorf("a new version arrived in force without review: %s", second.Status)
	}

	stillInForce, err := f.discounts.GetPublishedVersion(ctx, first.DefinitionID)
	if err != nil {
		t.Fatalf("GetPublishedVersion: %v", err)
	}
	if stillInForce.Rate != money.BasisPoints(2500) {
		t.Errorf("rate in force = %s, want 25%%: a draft must not change what is applied today",
			stillInForce.Rate)
	}
}

func TestGetDiscountReportsADefinitionWithNoVersionInForce(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	version := defineDiscountVersion(t, f, financeManager)

	detail, err := f.service.GetDiscount(ctx, financeManager, version.DefinitionID)
	if err != nil {
		t.Fatalf("GetDiscount: %v", err)
	}
	if detail.Definition.Code != "STAFF_CHILD" {
		t.Errorf("code = %s, want STAFF_CHILD", detail.Definition.Code)
	}
	// A catalogue entry whose first version is still a draft is a normal state,
	// not a lookup failure.
	if detail.PublishedVersion != nil {
		t.Errorf("published version = %s, want none", detail.PublishedVersion.ID)
	}
	if !detail.Definition.AnnualReconfirmation {
		t.Error("annual reconfirmation defaulted to false; a hardship discount would run unchecked")
	}
}

func TestAddDiscountVersionRejectsAnUnknownValueType(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	definition, err := f.service.DefineDiscount(ctx, financeManager, app.DefineDiscountInput{
		Code:   "MARTYR_CHILD",
		NameAr: "أبناء الشهداء",
	})
	if err != nil {
		t.Fatalf("DefineDiscount: %v", err)
	}

	_, err = f.service.AddDiscountVersion(ctx, financeManager, app.AddDiscountVersionInput{
		DefinitionID: definition.ID,
		ValueType:    "proportional",
	})
	requireCode(t, err, "discount.invalid_value_type")
}

// ---------------------------------------------------------------------------
// Reference data
// ---------------------------------------------------------------------------

func TestCreateStudyTypeNeedsNoCodeChange(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	// The ministry introduces a mode of study. Admitting it is data entry.
	studyType, err := f.service.CreateStudyType(ctx, administrator, app.CreateStudyTypeInput{
		Code:      "distance",
		NameAr:    "التعليم عن بعد",
		SortOrder: 4,
	})
	if err != nil {
		t.Fatalf("CreateStudyType: %v", err)
	}
	if studyType.Code != "DISTANCE" {
		t.Errorf("code = %q, want the folded DISTANCE", studyType.Code)
	}
	if !studyType.IsActive {
		t.Error("a new study type arrived inactive")
	}

	// And it is immediately usable as a fee-policy dimension, on exactly the
	// same terms as the three that shipped with the system.
	policy, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_DISTANCE_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		StudyTypeID:    &studyType.ID,
		Components:     tuition(1_500_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy over a brand-new study type: %v", err)
	}
	if policy.SpecificityScore != 18 {
		t.Errorf("specificity = %d, want 18 (college 16 + study type 2)", policy.SpecificityScore)
	}

	if got := f.audit.actions(); len(got) != 2 || got[0] != "reference.study_type_created" {
		t.Errorf("audit actions = %v, want the study type creation recorded", got)
	}
}

func TestCreateDepartmentRequiresAKnownCollege(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	_, err := f.service.CreateDepartment(ctx, administrator, app.CreateDepartmentInput{
		CollegeID:  shared.NewID(),
		Code:       "ARCH",
		NameAr:     "العمارة",
		StageCount: 5,
	})
	if shared.KindOf(err) != shared.KindNotFound {
		t.Fatalf("an orphan department was accepted: %v", err)
	}

	department, err := f.service.CreateDepartment(ctx, administrator, app.CreateDepartmentInput{
		CollegeID:  f.college.ID,
		Code:       "ARCH",
		NameAr:     "العمارة",
		StageCount: 5,
	})
	if err != nil {
		t.Fatalf("CreateDepartment: %v", err)
	}
	if department.CollegeID != f.college.ID || department.StageCount != 5 {
		t.Errorf("department = %+v, want it attached to the college with five stages", department)
	}
	if got := f.audit.actions(); len(got) != 1 || got[0] != "reference.department_created" {
		t.Errorf("audit actions = %v, want one department creation", got)
	}
}

func TestCreateCollegeValidatesItsCode(t *testing.T) {
	f := newCfgFixture(t)

	_, err := f.service.CreateCollege(context.Background(), administrator, app.CreateCollegeInput{
		Code:   "e",
		NameAr: "الهندسة",
	})
	requireCode(t, err, "college.invalid_code")
}

// ---------------------------------------------------------------------------
// Study-type default debt
// ---------------------------------------------------------------------------

func TestSetStudyTypeInitialDebtPublishesAWildcardPolicy(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	policy, err := f.service.SetStudyTypeInitialDebt(ctx, financeManager, app.SetStudyTypeInitialDebtInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Amount:         money.FromInt64(750_000),
	})
	if err != nil {
		t.Fatalf("SetStudyTypeInitialDebt: %v", err)
	}
	if policy.Status != billing.PolicyPublished {
		t.Errorf("status = %s, want published — the point is that it resolves immediately", policy.Status)
	}
	// Every other dimension must stay a wildcard: this is what makes it a
	// default, matched by any enrollment of the study type regardless of
	// college, department, stage or category.
	if policy.CollegeID != nil || policy.DepartmentID != nil || policy.Stage != nil || policy.StudentCategoryID != nil {
		t.Errorf("policy scope = %+v, want every dimension but study type left nil", policy)
	}
	if policy.StudyTypeID == nil || *policy.StudyTypeID != f.studyType.ID {
		t.Errorf("study type = %v, want %s", policy.StudyTypeID, f.studyType.ID)
	}
	gross, err := policy.GrossTotal()
	if err != nil || gross != money.FromInt64(750_000) {
		t.Errorf("gross total = %s (err %v), want 750000", gross, err)
	}

	scope := port.FeeScope{
		AcademicYearID: f.openYear.ID,
		CollegeID:      f.college.ID,
		DepartmentID:   f.department.ID,
		Stage:          1,
		StudyTypeID:    f.studyType.ID,
	}
	resolved, err := f.policies.Resolve(ctx, scope)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ID != policy.ID {
		t.Errorf("an unpriced enrollment of this study type must resolve to the default, got %s", resolved.PolicyCode)
	}
}

func TestSetStudyTypeInitialDebtChangesTheAmountByRetiringTheOldVersion(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	first, err := f.service.SetStudyTypeInitialDebt(ctx, financeManager, app.SetStudyTypeInitialDebtInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Amount:         money.FromInt64(750_000),
	})
	if err != nil {
		t.Fatalf("first SetStudyTypeInitialDebt: %v", err)
	}

	second, err := f.service.SetStudyTypeInitialDebt(ctx, financeManager, app.SetStudyTypeInitialDebtInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Amount:         money.FromInt64(900_000),
	})
	if err != nil {
		t.Fatalf("second SetStudyTypeInitialDebt: %v", err)
	}
	if second.VersionNo <= first.VersionNo {
		t.Errorf("version_no = %d, want greater than the first version's %d", second.VersionNo, first.VersionNo)
	}
	if second.PolicyCode != first.PolicyCode {
		t.Errorf("policy code changed from %q to %q — a changed amount must still be found under the same code",
			first.PolicyCode, second.PolicyCode)
	}

	reread, err := f.service.GetFeePolicy(ctx, financeManager, first.ID)
	if err != nil {
		t.Fatalf("GetFeePolicy(first): %v", err)
	}
	if reread.Status != billing.PolicyRetired {
		t.Errorf("first policy status = %s, want retired — the old amount must stop resolving once a new one is set",
			reread.Status)
	}

	// A published account already priced under the old version is
	// untouched — that assurance is the entire reason retiring exists rather
	// than editing. GrossTotal on the retired row must still read the old
	// figure.
	gross, err := reread.GrossTotal()
	if err != nil || gross != money.FromInt64(750_000) {
		t.Errorf("retired policy's amount = %s (err %v), want the original 750000 unchanged", gross, err)
	}
}

func TestRetireFeePolicyFreesItsScopeForANewPublication(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	policy, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		Components:     tuition(2_000_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy: %v", err)
	}
	if _, err := f.service.PublishFeePolicy(ctx, financeManager, policy.ID); err != nil {
		t.Fatalf("PublishFeePolicy: %v", err)
	}

	retired, err := f.service.RetireFeePolicy(ctx, financeManager, policy.ID)
	if err != nil {
		t.Fatalf("RetireFeePolicy: %v", err)
	}
	if retired.Status != billing.PolicyRetired || retired.RetiredAt == nil {
		t.Errorf("policy = %+v, want retired with a timestamp", retired)
	}

	replacement, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025_V2",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		Components:     tuition(2_200_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy(replacement): %v", err)
	}
	if _, err := f.service.PublishFeePolicy(ctx, financeManager, replacement.ID); err != nil {
		t.Fatalf("a scope vacated by retirement must accept a new publication: %v", err)
	}
}

func TestRetireFeePolicyRefusesADraft(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	policy, err := f.service.DefineFeePolicy(ctx, financeManager, app.DefineFeePolicyInput{
		PolicyCode:     "ENG_2025",
		AcademicYearID: f.openYear.ID,
		CollegeID:      &f.college.ID,
		Components:     tuition(2_000_000),
	})
	if err != nil {
		t.Fatalf("DefineFeePolicy: %v", err)
	}

	_, err = f.service.RetireFeePolicy(ctx, financeManager, policy.ID)
	requireCode(t, err, "fee_policy.not_published")
}

// ---------------------------------------------------------------------------
// Study-type default installment plan
// ---------------------------------------------------------------------------

func TestSetStudyTypeInstallmentPlanPublishesAWildcardTemplateWithLiteralAmounts(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	template, err := f.service.SetStudyTypeInstallmentPlan(ctx, financeManager, app.SetStudyTypeInstallmentPlanInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Lines: []app.StudyTypeInstallmentLineInput{
			{Amount: money.FromInt64(400_000), DueOffsetDays: 0},
			{Amount: money.FromInt64(400_000), DueOffsetDays: 60},
			{Amount: money.FromInt64(350_000), DueOffsetDays: 120},
			{Amount: money.FromInt64(350_000), DueOffsetDays: 180},
		},
	})
	if err != nil {
		t.Fatalf("SetStudyTypeInstallmentPlan: %v", err)
	}
	if template.Status != billing.PolicyPublished {
		t.Errorf("status = %s, want published", template.Status)
	}
	if template.CollegeID != nil || template.DepartmentID != nil || template.Stage != nil {
		t.Errorf("template scope = %+v, want every dimension but study type left nil", template)
	}

	// The whole point of literal amounts: generating a plan against exactly
	// the net they sum to must reproduce them untouched.
	plan, err := billing.GeneratePlan(billing.PlanSpec{
		AccountID: shared.NewID(),
		NetAmount: money.FromInt64(1_500_000),
		YearStart: f.openYear.StartDate,
		Lines:     template.Lines,
	})
	if err != nil {
		t.Fatalf("GeneratePlan: %v", err)
	}
	want := []money.Amount{400_000, 400_000, 350_000, 350_000}
	if len(plan) != len(want) {
		t.Fatalf("got %d installments, want %d", len(plan), len(want))
	}
	for i, inst := range plan {
		if inst.Amount != want[i] {
			t.Errorf("installment %d = %s, want %s", i+1, inst.Amount, want[i])
		}
	}

	resolved, err := f.templates.Resolve(ctx, port.FeeScope{
		AcademicYearID: f.openYear.ID,
		CollegeID:      f.college.ID,
		DepartmentID:   f.department.ID,
		Stage:          1,
		StudyTypeID:    f.studyType.ID,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ID != template.ID {
		t.Errorf("an unpriced enrollment of this study type must resolve to the default plan, got %s", resolved.Code)
	}
}

func TestSetStudyTypeInstallmentPlanChangesLinesByRetiringTheOldVersion(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	first, err := f.service.SetStudyTypeInstallmentPlan(ctx, financeManager, app.SetStudyTypeInstallmentPlanInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Lines: []app.StudyTypeInstallmentLineInput{
			{Amount: money.FromInt64(750_000), DueOffsetDays: 0},
		},
	})
	if err != nil {
		t.Fatalf("first SetStudyTypeInstallmentPlan: %v", err)
	}

	second, err := f.service.SetStudyTypeInstallmentPlan(ctx, financeManager, app.SetStudyTypeInstallmentPlanInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Lines: []app.StudyTypeInstallmentLineInput{
			{Amount: money.FromInt64(500_000), DueOffsetDays: 0},
			{Amount: money.FromInt64(400_000), DueOffsetDays: 90},
		},
	})
	if err != nil {
		t.Fatalf("second SetStudyTypeInstallmentPlan: %v", err)
	}
	// Unlike a fee policy, installment_template has no version_no column of
	// its own — uq_installment_template_code is unique on the bare code,
	// retired rows included — so a second call cannot reuse the first call's
	// code; each gets a fresh one carrying its own version suffix.
	if second.Code == first.Code {
		t.Errorf("second call reused code %q — installment_template's code is unique even across retired rows",
			first.Code)
	}
	if !strings.HasPrefix(second.Code, "PLAN_") {
		t.Errorf("code %q does not carry the expected prefix", second.Code)
	}
	// A fake repository does not enforce ck_installment_template_code, so this
	// is the only place that would catch a code the real database refuses.
	if !installmentTemplateCodePattern.MatchString(second.Code) {
		t.Errorf("code %q does not satisfy ck_installment_template_code (%s)",
			second.Code, installmentTemplateCodePattern)
	}

	reread, err := f.service.GetInstallmentTemplate(ctx, financeManager, first.ID)
	if err != nil {
		t.Fatalf("GetInstallmentTemplate(first): %v", err)
	}
	if reread.Status != billing.PolicyRetired {
		t.Errorf("first template status = %s, want retired — the old plan must stop resolving once a new one is set",
			reread.Status)
	}
	if len(reread.Lines) != 1 || *reread.Lines[0].Amount != money.FromInt64(750_000) {
		t.Errorf("retired template's lines = %+v, want the original single 750000 line unchanged", reread.Lines)
	}
}

func TestSetStudyTypeInstallmentPlanRefusesLinesWithoutAPositiveAmount(t *testing.T) {
	f := newCfgFixture(t)
	_, err := f.service.SetStudyTypeInstallmentPlan(context.Background(), financeManager, app.SetStudyTypeInstallmentPlanInput{
		AcademicYearID: f.openYear.ID,
		StudyTypeID:    f.studyType.ID,
		Lines: []app.StudyTypeInstallmentLineInput{
			{Amount: money.FromInt64(400_000), DueOffsetDays: 0},
			{Amount: 0, DueOffsetDays: 60},
		},
	})
	requireCode(t, err, "installment_template.invalid_amount")
}

func TestRetireInstallmentTemplateFreesItsScopeForANewPublication(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	template, err := f.service.DefineInstallmentTemplate(ctx, financeManager, app.DefineInstallmentTemplateInput{
		Code:           "STD4",
		NameAr:         "أربعة أقساط",
		AcademicYearID: &f.openYear.ID,
		CollegeID:      &f.college.ID,
		Lines: []app.TemplateLineInput{
			{ShareBP: 5000},
			{ShareBP: 5000},
		},
	})
	if err != nil {
		t.Fatalf("DefineInstallmentTemplate: %v", err)
	}
	if _, err := f.service.PublishInstallmentTemplate(ctx, financeManager, template.ID); err != nil {
		t.Fatalf("PublishInstallmentTemplate: %v", err)
	}

	retired, err := f.service.RetireInstallmentTemplate(ctx, financeManager, template.ID)
	if err != nil {
		t.Fatalf("RetireInstallmentTemplate: %v", err)
	}
	if retired.Status != billing.PolicyRetired || retired.RetiredAt == nil {
		t.Errorf("template = %+v, want retired with a timestamp", retired)
	}

	replacement, err := f.service.DefineInstallmentTemplate(ctx, financeManager, app.DefineInstallmentTemplateInput{
		Code:           "STD4_V2",
		NameAr:         "أربعة أقساط",
		AcademicYearID: &f.openYear.ID,
		CollegeID:      &f.college.ID,
		Lines: []app.TemplateLineInput{
			{ShareBP: 6000},
			{ShareBP: 4000},
		},
	})
	if err != nil {
		t.Fatalf("DefineInstallmentTemplate(replacement): %v", err)
	}
	if _, err := f.service.PublishInstallmentTemplate(ctx, financeManager, replacement.ID); err != nil {
		t.Fatalf("a scope vacated by retirement must accept a new publication: %v", err)
	}
}

func TestRetireInstallmentTemplateRefusesADraft(t *testing.T) {
	f := newCfgFixture(t)
	ctx := context.Background()

	template, err := f.service.DefineInstallmentTemplate(ctx, financeManager, app.DefineInstallmentTemplateInput{
		Code:           "STD4",
		NameAr:         "أربعة أقساط",
		AcademicYearID: &f.openYear.ID,
		CollegeID:      &f.college.ID,
		Lines: []app.TemplateLineInput{
			{ShareBP: 5000},
			{ShareBP: 5000},
		},
	})
	if err != nil {
		t.Fatalf("DefineInstallmentTemplate: %v", err)
	}

	_, err = f.service.RetireInstallmentTemplate(ctx, financeManager, template.ID)
	requireCode(t, err, "installment_template.not_published")
}
