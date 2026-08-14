package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// demo builds an exploration dataset: three academic years of an Iraqi
// university with the awkward cases the design was written for.
//
// It is generated through the real commands rather than inserted as SQL. That
// costs some speed and buys two things: the data is guaranteed consistent with
// every invariant the system enforces, and a successful run is itself evidence
// that registration, pricing, collection, refunding, superseding and year
// closing all work together.
//
// Refuses to run in production, and refuses to run over a database that
// already holds students.
func demo() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.App.IsProduction() {
		return fmt.Errorf("refusing to load demo data in production")
	}

	log := logger.New(cfg.Log, cfg.App.Name+"-demo", version, cfg.App.Environment)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	var existing int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM student`).Scan(&existing); err != nil {
		return fmt.Errorf("checking for existing data: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf(
			"the database already holds %d student(s); run `make db-reset` first "+
				"rather than mixing demo data into real records", existing)
	}

	d := newDemoBuilder(db, cfg, log)
	return d.run(ctx)
}

type demoBuilder struct {
	db   *pg.DB
	cfg  *config.Config
	log  *slog.Logger
	deps app.Deps

	students    *app.StudentService
	enrollments *app.EnrollmentService
	accounts    *app.AccountService
	payments    *app.PaymentService
	refunds     *app.RefundService
	discounts   *app.DiscountService
	years       *app.YearService

	// Actors the demo acts as. Real commands check authority, so the demo has
	// to hold the right role for each step — which is itself a check that the
	// permission matrix is usable rather than merely strict.
	admin     shared.Actor
	registrar shared.Actor
	officer   shared.Actor
	finance   shared.Actor
	cashier   shared.Actor

	yearIDs     map[string]shared.ID
	deptIDs     map[string]shared.ID
	collegeID   map[string]shared.ID
	studyType   map[string]shared.ID
	methods     map[string]shared.ID
	discountIDs map[string]shared.ID
	deskID      shared.ID

	counts map[string]int
}

func newDemoBuilder(db *pg.DB, cfg *config.Config, log *slog.Logger) *demoBuilder {
	deps := app.Deps{
		Tx:           postgres.NewTxManager(db),
		Students:     postgres.NewStudentRepository(db),
		Years:        postgres.NewAcademicYearRepository(db),
		Enrollments:  postgres.NewEnrollmentRepository(db),
		Reference:    postgres.NewReferenceRepository(db),
		FeePolicies:  postgres.NewFeePolicyRepository(db),
		Templates:    postgres.NewInstallmentTemplateRepository(db),
		Discounts:    postgres.NewDiscountRepository(db),
		Accounts:     postgres.NewAccountRepository(db),
		Installments: postgres.NewInstallmentRepository(db),
		Payments:     postgres.NewPaymentRepository(db),
		Refunds:      postgres.NewRefundRepository(db),
		VoidRequests: postgres.NewVoidRequestRepository(db),
		Series:       postgres.NewNumberSeriesRepository(db),
		Sessions:     postgres.NewCashierSessionRepository(db),
		Audit:        postgres.NewAuditRepository(db),
		Users:        postgres.NewUserRepository(db),
		Clock:        shared.SystemClock{},
		Log:          log,
	}

	return &demoBuilder{
		db:          db,
		cfg:         cfg,
		log:         log,
		deps:        deps,
		students:    app.NewStudentService(deps),
		enrollments: app.NewEnrollmentService(deps),
		accounts:    app.NewAccountService(deps),
		payments:    app.NewPaymentService(deps),
		refunds:     app.NewRefundService(deps),
		discounts:   app.NewDiscountService(deps),
		years:       app.NewYearService(deps),
		yearIDs:     map[string]shared.ID{},
		deptIDs:     map[string]shared.ID{},
		collegeID:   map[string]shared.ID{},
		studyType:   map[string]shared.ID{},
		methods:     map[string]shared.ID{},
		discountIDs: map[string]shared.ID{},
		counts:      map[string]int{},
	}
}

func (d *demoBuilder) run(ctx context.Context) error {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"users", d.seedUsers},
		{"organisation", d.seedOrganisation},
		{"academic years", d.seedYears},
		{"fee policies", d.seedFeePolicies},
		{"installment templates", d.seedTemplates},
		{"discount definitions", d.seedDiscounts},
		{"2023-2024 cohort", d.seed2023},
		{"2024-2025 cohort", d.seed2024},
		{"2025-2026 cohort", d.seed2025},
		{"close the old years", d.closeOldYears},
	}

	for _, s := range steps {
		start := time.Now()
		if err := s.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		d.log.Info("demo step complete", slog.String("step", s.name), slog.Duration("took", time.Since(start)))
	}

	d.printSummary()
	return nil
}

// ---------------------------------------------------------------------------
// Foundations
// ---------------------------------------------------------------------------

func (d *demoBuilder) seedUsers(ctx context.Context) error {
	hasher := auth.NewHasher(d.cfg.Auth)
	hash, err := hasher.Hash(demoPassword)
	if err != nil {
		return err
	}

	people := []struct {
		username, fullName string
		roles              []shared.Role
		actor              *shared.Actor
	}{
		{"admin", "مدير النظام", []shared.Role{shared.RoleAdmin}, &d.admin},
		{"registrar", "أمين السجل", []shared.Role{shared.RoleRegistrar}, &d.registrar},
		{"officer", "المسؤول العلمي", []shared.Role{shared.RoleAcademicOfficer}, &d.officer},
		{"finance", "مدير المالية", []shared.Role{shared.RoleFinanceManager}, &d.finance},
		{"cashier", "الصراف الأول", []shared.Role{shared.RoleCashier}, &d.cashier},
		{"auditor", "المدقق", []shared.Role{shared.RoleAuditor}, nil},
		{"viewer", "مطالع التقارير", []shared.Role{shared.RoleReportViewer}, nil},
	}

	for _, p := range people {
		user := &port.User{
			ID:           shared.NewID(),
			Username:     p.username,
			FullName:     p.fullName,
			PasswordHash: hash,
			IsActive:     true,
			Roles:        p.roles,
		}
		err := d.deps.Tx.Write(ctx, func(ctx context.Context) error {
			if err := d.deps.Users.Create(ctx, user); err != nil {
				return err
			}
			return d.deps.Users.SetRoles(ctx, user.ID, p.roles, user.ID)
		})
		if err != nil {
			return err
		}
		if p.actor != nil {
			*p.actor = shared.Actor{UserID: user.ID, Username: user.Username, Roles: p.roles}
		}
		d.counts["users"]++
	}
	return nil
}

func (d *demoBuilder) seedOrganisation(ctx context.Context) error {
	colleges := []struct {
		code, name  string
		departments []struct {
			code, name string
			stages     int16
		}
	}{
		{"ENG", "كلية الهندسة", []struct {
			code, name string
			stages     int16
		}{
			{"CPE", "هندسة الحاسوب", 4},
			{"CIV", "الهندسة المدنية", 4},
			{"ELE", "الهندسة الكهربائية", 4},
		}},
		{"MED", "كلية الطب", []struct {
			code, name string
			stages     int16
		}{
			{"MED", "الطب العام", 6},
			{"DEN", "طب الأسنان", 5},
		}},
	}

	for _, c := range colleges {
		college, err := academic.NewCollege(c.code, c.name)
		if err != nil {
			return err
		}
		if err := d.deps.Tx.Write(ctx, func(ctx context.Context) error {
			return d.deps.Reference.CreateCollege(ctx, college)
		}); err != nil {
			return err
		}
		d.collegeID[c.code] = college.ID

		for _, dep := range c.departments {
			department, err := academic.NewDepartment(college.ID, dep.code, dep.name, dep.stages)
			if err != nil {
				return err
			}
			if err := d.deps.Tx.Write(ctx, func(ctx context.Context) error {
				return d.deps.Reference.CreateDepartment(ctx, department)
			}); err != nil {
				return err
			}
			d.deptIDs[dep.code] = department.ID
			d.counts["departments"]++
		}
	}

	types, err := d.deps.Reference.ListStudyTypes(ctx, true)
	if err != nil {
		return err
	}
	for _, t := range types {
		d.studyType[t.Code] = t.ID
	}

	methods, err := d.deps.Reference.ListPaymentMethods(ctx, true)
	if err != nil {
		return err
	}
	for _, m := range methods {
		d.methods[m.Code] = m.ID
	}

	// One cashier desk. Receipt series run per year per desk, so the demo's
	// receipts all carry this desk's prefix.
	d.deskID = shared.NewID()
	_, err = d.db.Pool().Exec(ctx, `
		INSERT INTO cashier_desk (id, code, name_ar, college_id)
		VALUES ($1, 'D01', 'شباك الحسابات', $2)`, d.deskID, d.collegeID["ENG"])
	if err != nil {
		return err
	}
	d.cashier.CashierDeskID = &d.deskID
	return nil
}

func (d *demoBuilder) seedYears(ctx context.Context) error {
	for _, code := range []string{"2023-2024", "2024-2025", "2025-2026"} {
		startYear := 2023 + len(d.yearIDs)
		year, err := d.years.CreateAcademicYear(ctx, d.admin, app.CreateYearInput{
			Code:            code,
			StartDate:       shared.NewDate(startYear, time.September, 1),
			EndDate:         shared.NewDate(startYear+1, time.July, 1),
			DebtBlockPolicy: academic.DebtWarn,
		})
		if err != nil {
			return err
		}
		if _, err := d.years.OpenAcademicYear(ctx, d.admin, year.ID); err != nil {
			return err
		}
		d.yearIDs[code] = year.ID
	}
	return nil
}

// seedFeePolicies prices every year.
//
// Prices rise across the three years and repeat students pay more, which is
// the whole point of having a student-category dimension: the difference is a
// row, not a branch in code.
func (d *demoBuilder) seedFeePolicies(ctx context.Context) error {
	categories, err := d.deps.Reference.ListStudentCategories(ctx, true)
	if err != nil {
		return err
	}
	categoryID := map[string]shared.ID{}
	for _, c := range categories {
		categoryID[c.Code] = c.ID
	}

	type pricing struct {
		year     string
		dept     string
		study    string
		category string
		tuition  money.Amount
	}

	var plans []pricing
	for yearIdx, year := range []string{"2023-2024", "2024-2025", "2025-2026"} {
		bump := money.Amount(yearIdx * 100_000)
		for _, spec := range []struct {
			dept    string
			evening money.Amount
			morning money.Amount
		}{
			{"CPE", 1_500_000, 0},
			{"CIV", 1_250_000, 0},
			{"ELE", 1_350_000, 0},
			{"MED", 3_000_000, 0},
			{"DEN", 2_500_000, 0},
		} {
			evening := spec.evening + bump
			parallel := evening + 500_000

			// A repeat student pays a quarter more, in every mode of study.
			// Leaving a gap here is not a cheaper policy, it is an enrollment
			// nobody can price — resolution refuses rather than defaulting to
			// zero, which is exactly what should happen.
			plans = append(plans,
				pricing{year, spec.dept, academic.StudyTypeEvening, academic.CategoryRegular, evening},
				pricing{year, spec.dept, academic.StudyTypeEvening, academic.CategoryRepeat, evening + evening/4},
				pricing{year, spec.dept, academic.StudyTypeParallel, academic.CategoryRegular, parallel},
				pricing{year, spec.dept, academic.StudyTypeParallel, academic.CategoryRepeat, parallel + parallel/4},
				// Morning study here is state-funded: registration and card
				// fees only, no tuition. A repeating morning student pays a
				// modest repeat charge.
				pricing{year, spec.dept, academic.StudyTypeMorning, academic.CategoryRegular, 0},
				pricing{year, spec.dept, academic.StudyTypeMorning, academic.CategoryRepeat, 400_000},
			)
		}
	}

	for i, p := range plans {
		deptID := d.deptIDs[p.dept]
		collegeIDForDept := d.collegeID["ENG"]
		if p.dept == "MED" || p.dept == "DEN" {
			collegeIDForDept = d.collegeID["MED"]
		}
		studyTypeID := d.studyType[p.study]
		catID := categoryID[p.category]

		policy := &billing.FeePolicy{
			ID:                shared.NewID(),
			PolicyCode:        fmt.Sprintf("%s-%s-%s-%s", p.year, p.dept, p.study, p.category),
			VersionNo:         1,
			AcademicYearID:    d.yearIDs[p.year],
			CollegeID:         &collegeIDForDept,
			DepartmentID:      &deptID,
			StudyTypeID:       &studyTypeID,
			StudentCategoryID: &catID,
			Status:            billing.PolicyDraft,
			MaxDiscountBP:     money.FullRate,
			CreatedBy:         &d.finance.UserID,
		}

		if p.tuition.IsPositive() {
			policy.Components = append(policy.Components, &billing.FeeComponent{
				ID: shared.NewID(), FeePolicyID: policy.ID, Code: "TUITION",
				NameAr: "القسط الدراسي", Amount: p.tuition,
				IsDiscountable: true, IsRefundable: true, IsMandatory: true, SortOrder: 1,
			})
		}
		// Collected from everyone, including a fully exempt student, which is
		// why they are not discountable.
		policy.Components = append(policy.Components,
			&billing.FeeComponent{
				ID: shared.NewID(), FeePolicyID: policy.ID, Code: "REGISTRATION",
				NameAr: "رسوم التسجيل", Amount: 75_000,
				IsDiscountable: false, IsRefundable: false, IsMandatory: true, SortOrder: 2,
			},
			&billing.FeeComponent{
				ID: shared.NewID(), FeePolicyID: policy.ID, Code: "ID_CARD",
				NameAr: "الهوية الجامعية", Amount: 25_000,
				IsDiscountable: false, IsRefundable: false, IsMandatory: true, SortOrder: 3,
			})

		err := d.deps.Tx.Write(ctx, func(ctx context.Context) error {
			if err := d.deps.FeePolicies.Create(ctx, policy); err != nil {
				return err
			}
			return d.deps.FeePolicies.Publish(ctx, policy.ID, d.finance.UserID, time.Now().UTC())
		})
		if err != nil {
			return fmt.Errorf("policy %d (%s): %w", i, policy.PolicyCode, err)
		}
		d.counts["fee policies"]++
	}
	return nil
}

func (d *demoBuilder) seedTemplates(ctx context.Context) error {
	for _, year := range []string{"2023-2024", "2024-2025", "2025-2026"} {
		yearID := d.yearIDs[year]
		template := &billing.InstallmentTemplate{
			ID:              shared.NewID(),
			Code:            "STD4_" + year[:4],
			NameAr:          "أربعة أقساط",
			AcademicYearID:  &yearID,
			MaxInstallments: 4,
			Status:          billing.PolicyDraft,
			// A larger payment at registration, then three smaller ones — the
			// shape Iraqi finance offices actually use.
			Lines: []billing.TemplateLine{
				{LineNo: 1, ShareBP: 4000, DueOffsetDays: 0},
				{LineNo: 2, ShareBP: 2000, DueOffsetDays: 60},
				{LineNo: 3, ShareBP: 2000, DueOffsetDays: 120},
				{LineNo: 4, ShareBP: 2000, DueOffsetDays: 180},
			},
		}
		err := d.deps.Tx.Write(ctx, func(ctx context.Context) error {
			if err := d.deps.Templates.Create(ctx, template); err != nil {
				return err
			}
			return d.deps.Templates.Publish(ctx, template.ID, d.finance.UserID, time.Now().UTC())
		})
		if err != nil {
			return err
		}
		d.counts["templates"]++
	}
	return nil
}

func (d *demoBuilder) seedDiscounts(ctx context.Context) error {
	specs := []struct {
		code, name string
		category   discount.Category
		percentBP  int32
		fixed      money.Amount
		exemption  bool
		reconfirm  bool
	}{
		{"TEACHERS_CHILD", "خصم أبناء التدريسيين", discount.CategoryStaff, 2000, 0, false, false},
		{"SOCIAL_HARDSHIP", "خصم الحالات الاجتماعية", discount.CategorySocial, 2500, 0, false, true},
		{"SIBLING", "خصم الأخوة", discount.CategorySibling, 1000, 0, false, true},
		{"MARTYR_FAMILY", "إعفاء ذوي الشهداء", discount.CategoryMartyr, 10000, 0, true, false},
		{"BOARD_GRANT", "منحة مجلس الجامعة", discount.CategoryOther, 0, 250_000, false, true},
	}

	for _, spec := range specs {
		definition, err := discount.NewDefinition(spec.code, spec.name, spec.category)
		if err != nil {
			return err
		}
		definition.IsFullExemption = spec.exemption
		definition.AnnualReconfirmation = spec.reconfirm
		definition.CreatedBy = &d.admin.UserID

		var version *discount.DefinitionVersion
		if spec.fixed.IsPositive() {
			version, err = discount.NewFixedVersion(definition.ID, 1, spec.fixed)
		} else {
			version, err = discount.NewPercentageVersion(definition.ID, 1, money.BasisPoints(spec.percentBP))
		}
		if err != nil {
			return err
		}
		version.CreatedBy = &d.admin.UserID
		if spec.exemption {
			// A full exemption stands alone; combining it with anything else
			// would be arithmetic nobody intends.
			version.Stackable = false
			version.Priority = 1
		}

		err = d.deps.Tx.Write(ctx, func(ctx context.Context) error {
			if err := d.deps.Discounts.CreateDefinition(ctx, definition); err != nil {
				return err
			}
			if err := d.deps.Discounts.CreateVersion(ctx, version); err != nil {
				return err
			}
			return d.deps.Discounts.PublishVersion(ctx, version.ID, d.finance.UserID, time.Now().UTC())
		})
		if err != nil {
			return fmt.Errorf("discount %s: %w", spec.code, err)
		}
		d.discountIDs[spec.code] = definition.ID
		d.counts["discounts"]++
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cohorts
// ---------------------------------------------------------------------------

// demoStudent describes one person and what happens to them, so the scenarios
// read as a list rather than as five hundred lines of imperative setup.
type demoStudent struct {
	no       string
	name     string
	mother   string
	phone    string
	dept     string
	study    string
	stage    int16
	scenario string
}

func (d *demoBuilder) seed2023(ctx context.Context) error {
	cohort := []demoStudent{
		{"CPE-2023-001", "علي محمد حسن الجبوري", "زينب عبد الله", "07701110001", "CPE", academic.StudyTypeEvening, 1, "pass"},
		{"CPE-2023-002", "فاطمة أحمد كاظم العبيدي", "سعاد جاسم", "07701110002", "CPE", academic.StudyTypeEvening, 1, "pass"},
		{"CPE-2023-003", "حسين عبد الرزاق ناصر", "أمل حميد", "07701110003", "CPE", academic.StudyTypeEvening, 1, "fail"},
		{"CIV-2023-004", "زهراء سعد مهدي", "نور الهدى", "07701110004", "CIV", academic.StudyTypeEvening, 1, "pass"},
		{"CIV-2023-005", "مصطفى وليد إبراهيم", "هدى صالح", "07701110005", "CIV", academic.StudyTypeParallel, 1, "dropout"},
		{"MED-2023-006", "نور عماد شاكر", "ابتسام علي", "07701110006", "MED", academic.StudyTypeEvening, 1, "pass"},
		{"MED-2023-007", "أحمد خالد سلمان", "رجاء كريم", "07701110007", "MED", academic.StudyTypeMorning, 1, "pass"},
		{"ELE-2023-008", "مريم فاضل عباس", "شيماء حسن", "07701110008", "ELE", academic.StudyTypeEvening, 1, "defer"},
	}
	return d.runCohort(ctx, "2023-2024", cohort, 1, true)
}

func (d *demoBuilder) seed2024(ctx context.Context) error {
	// Last year's cohort moves on, then new first-years arrive.
	if err := d.promoteFrom(ctx, "2023-2024", "2024-2025"); err != nil {
		return err
	}

	cohort := []demoStudent{
		{"CPE-2024-009", "يوسف طارق عبد الكريم", "لمياء رشيد", "07702220009", "CPE", academic.StudyTypeEvening, 1, "pass"},
		{"CPE-2024-010", "رقية جمال محمود", "بشرى قاسم", "07702220010", "CPE", academic.StudyTypeEvening, 1, "teachers_child"},
		{"CPE-2024-011", "عمر صباح داود", "ميسون فالح", "07702220011", "CPE", academic.StudyTypeParallel, 1, "fail"},
		{"CIV-2024-012", "سجى نبيل حاتم", "إيمان عدنان", "07702220012", "CIV", academic.StudyTypeEvening, 1, "martyr"},
		{"ELE-2024-013", "كرار محسن جبار", "سناء طالب", "07702220013", "ELE", academic.StudyTypeEvening, 1, "pass"},
		{"MED-2024-014", "تبارك رياض عبد الحسين", "غادة مجيد", "07702220014", "MED", academic.StudyTypeEvening, 1, "partial_debt"},
		{"DEN-2024-015", "ليث عادل شهاب", "أسماء وحيد", "07702220015", "DEN", academic.StudyTypeEvening, 1, "pass"},
		{"CPE-2024-016", "دعاء منير سالم", "حنان يوسف", "07702220016", "CPE", academic.StudyTypeMorning, 1, "pass"},
	}
	return d.runCohort(ctx, "2024-2025", cohort, 1, true)
}

func (d *demoBuilder) seed2025(ctx context.Context) error {
	if err := d.promoteFrom(ctx, "2024-2025", "2025-2026"); err != nil {
		return err
	}

	cohort := []demoStudent{
		{"CPE-2025-017", "محمد باقر عبد الأمير", "ندى فاروق", "07703330017", "CPE", academic.StudyTypeEvening, 1, "overpay"},
		{"CPE-2025-018", "آية ستار جودة", "رنا مثنى", "07703330018", "CPE", academic.StudyTypeEvening, 1, "refund"},
		{"CPE-2025-019", "عبد الله ماجد حميد", "سهام برهان", "07703330019", "CPE", academic.StudyTypeEvening, 1, "void"},
		{"CPE-2025-020", "زينب قيس عبد الستار", "وفاء لطيف", "07703330020", "CPE", academic.StudyTypeEvening, 1, "supersede"},
		{"CIV-2025-021", "حيدر نعيم فرحان", "بتول صادق", "07703330021", "CIV", academic.StudyTypeEvening, 1, "two_discounts"},
		{"CIV-2025-022", "رسل أكرم خضير", "ألاء منذر", "07703330022", "CIV", academic.StudyTypeEvening, 1, "unpaid"},
		{"ELE-2025-023", "سيف الدين هاشم علوان", "منى ثامر", "07703330023", "ELE", academic.StudyTypeParallel, 1, "pass"},
		{"ELE-2025-024", "بنين وسام غازي", "خديجة رعد", "07703330024", "ELE", academic.StudyTypeEvening, 1, "hosted_in"},
		{"MED-2025-025", "جعفر صادق نوري", "زهرة كامل", "07703330025", "MED", academic.StudyTypeEvening, 1, "pass"},
		{"MED-2025-026", "مروة علاء حسون", "سميرة جبر", "07703330026", "MED", academic.StudyTypeEvening, 1, "board_grant"},
		{"DEN-2025-027", "أمير ثائر عبد الجبار", "نغم سلمان", "07703330027", "DEN", academic.StudyTypeEvening, 1, "unpaid"},
		{"DEN-2025-028", "شهد حازم مالك", "ريام عمار", "07703330028", "DEN", academic.StudyTypeMorning, 1, "pass"},
		{"CPE-2025-029", "مهدي صلاح جميل", "سرى أنور", "07703330029", "CPE", academic.StudyTypeEvening, 1, "no_account"},
		{"CPE-2025-030", "غفران باسم عطية", "لبنى فيصل", "07703330030", "CPE", academic.StudyTypeEvening, 1, "pass"},
	}
	if err := d.runCohort(ctx, "2025-2026", cohort, 1, false); err != nil {
		return err
	}

	// A student who dropped out in 2023 and comes back two years later, still
	// carrying the debt from the year they abandoned.
	return d.returnAfterDropout(ctx)
}

// runCohort registers a cohort and plays out each student's scenario.
func (d *demoBuilder) runCohort(ctx context.Context, yearCode string, cohort []demoStudent, stage int16, finished bool) error {
	yearID := d.yearIDs[yearCode]

	for _, s := range cohort {
		result, err := d.students.RegisterStudent(ctx, d.registrar, app.RegisterStudentInput{
			StudentNo: s.no, FullName: s.name, MotherName: s.mother, Phone: &s.phone,
		})
		if err != nil {
			return fmt.Errorf("register %s: %w", s.no, err)
		}
		person := result.Student
		d.counts["students"]++

		// Grants are made before the account is generated, which is what lets
		// them be priced into it rather than corrected in afterwards.
		if err := d.grantFor(ctx, person.ID, yearID, s.scenario); err != nil {
			return fmt.Errorf("grant for %s: %w", s.no, err)
		}

		enrolled, err := d.enrollments.EnrollStudent(ctx, d.registrar, app.EnrollStudentInput{
			StudentID:      person.ID,
			AcademicYearID: yearID,
			DepartmentID:   d.deptIDs[s.dept],
			StudyTypeID:    d.studyType[s.study],
			Stage:          stage,
		})
		if err != nil {
			return fmt.Errorf("enroll %s: %w", s.no, err)
		}
		d.counts["enrollments"]++

		if s.scenario == "no_account" {
			// Registered but never priced. A real gap the debt report should
			// not mistake for a settled student.
			continue
		}

		account, err := d.accounts.GenerateFinancialAccount(ctx, d.finance, app.GenerateAccountInput{
			EnrollmentID: enrolled.Enrollment.ID,
		})
		if err != nil {
			return fmt.Errorf("account for %s: %w", s.no, err)
		}
		d.counts["accounts"]++

		if err := d.playScenario(ctx, s, enrolled.Enrollment.ID, account); err != nil {
			return fmt.Errorf("scenario %s for %s: %w", s.scenario, s.no, err)
		}

		// A finished year carries outcomes; the open one does not yet. Without
		// this the promotion step would read every student as unresolved and
		// roll the whole cohort forward as repeating.
		if finished {
			if err := d.recordOutcome(ctx, enrolled.Enrollment.ID, s.scenario); err != nil {
				return fmt.Errorf("outcome for %s: %w", s.no, err)
			}
		}
	}
	return nil
}

// recordOutcome closes a student's year the way their scenario says it ended.
func (d *demoBuilder) recordOutcome(ctx context.Context, enrollmentID shared.ID, scenario string) error {
	switch scenario {
	case "dropout":
		_, err := d.enrollments.ChangeEnrollmentStatus(ctx, d.registrar, app.ChangeStatusInput{
			EnrollmentID: enrollmentID,
			Target:       academic.StatusDroppedOut,
			Result:       academic.ResultNoResult,
			Reason:       ptrTo("انقطاع عن الدوام"),
			// Ending an enrollment has to say what happens to the money; the
			// command refuses to guess. A student who simply stopped attending
			// still owes what the year charged, so the debt stands and the
			// dataset carries a real debtor rather than a tidy zero.
			FinancialTreatment: academic.TreatmentKeep,
		})
		d.counts["dropped out"]++
		return err

	case "defer":
		// A deferral that writes off the unpaid balance needs both authorities
		// at once: the registrar's, to end the registration, and finance's, to
		// waive money. Only the administrator holds both, which is the honest
		// depiction — in the office this is a form the registrar raises and
		// the finance manager signs.
		_, err := d.enrollments.ChangeEnrollmentStatus(ctx, d.admin, app.ChangeStatusInput{
			EnrollmentID: enrollmentID,
			Target:       academic.StatusDeferred,
			OrderRef:     ptrTo("أمر تأجيل 2024/318"),
			// A deferral by order: what is unpaid is written off for this
			// year, and the student is charged again when they return.
			FinancialTreatment: academic.TreatmentWaiveUnpaid,
		})
		d.counts["deferred"]++
		return err

	case "fail":
		_, err := d.enrollments.RecordAcademicResult(ctx, d.officer, app.RecordResultInput{
			EnrollmentID: enrollmentID, Result: academic.ResultFailed,
		})
		d.counts["failed"]++
		return err

	default:
		// Most students pass, and a few of them only in the second round —
		// which is the case the two-phase year close exists for.
		result := academic.ResultPassedR1
		if d.counts["passed"]%5 == 4 {
			result = academic.ResultPassedR2
		}
		_, err := d.enrollments.RecordAcademicResult(ctx, d.officer, app.RecordResultInput{
			EnrollmentID: enrollmentID, Result: result,
		})
		d.counts["passed"]++
		return err
	}
}

func (d *demoBuilder) grantFor(ctx context.Context, studentID, yearID shared.ID, scenario string) error {
	var codes []string
	switch scenario {
	case "teachers_child":
		codes = []string{"TEACHERS_CHILD"}
	case "martyr":
		codes = []string{"MARTYR_FAMILY"}
	case "two_discounts":
		codes = []string{"TEACHERS_CHILD", "SIBLING"}
	case "board_grant":
		codes = []string{"BOARD_GRANT"}
	default:
		return nil
	}

	for _, code := range codes {
		assignment, err := d.discounts.AssignDiscount(ctx, d.registrar, app.AssignDiscountInput{
			StudentID:    studentID,
			DefinitionID: d.discountIDs[code],
			Scope:        discount.ScopeSingleYear,
			YearFromID:   &yearID,
		})
		if err != nil {
			return err
		}
		// Approved by finance, never by the registrar who asked — the same
		// four-eyes rule the API enforces.
		if _, err := d.discounts.ApproveDiscountAssignment(ctx, d.finance, assignment.ID); err != nil {
			return err
		}
		d.counts["discount grants"]++
	}
	return nil
}

func (d *demoBuilder) playScenario(
	ctx context.Context, s demoStudent, enrollmentID shared.ID, generated *app.GenerateAccountResult,
) error {
	account := generated.Account
	owed := account.Remaining()

	switch s.scenario {
	case "unpaid":
		// Nothing collected: these are the debt report's subjects.
		return nil

	case "partial_debt":
		// Half paid, the rest overdue by now.
		if owed.IsPositive() {
			return d.collect(ctx, account.ID, owed/2, "BANK")
		}
		return nil

	case "overpay":
		// Pays more than the plan asks; the excess becomes credit rather than
		// a fabricated extra installment.
		return d.collect(ctx, account.ID, owed+300_000, "CASH")

	case "refund":
		if err := d.collect(ctx, account.ID, owed, "BANK"); err != nil {
			return err
		}
		return d.refundLast(ctx, account.ID, 200_000)

	case "void":
		if err := d.collect(ctx, account.ID, 500_000, "CASH"); err != nil {
			return err
		}
		return d.voidLast(ctx, account.ID)

	case "supersede":
		if err := d.collect(ctx, account.ID, owed/2, "BANK"); err != nil {
			return err
		}
		return d.supersede(ctx, enrollmentID)

	case "hosted_in":
		return d.attachHosting(ctx, enrollmentID)

	default:
		// Everyone else settles in full, which is what most students do.
		if owed.IsPositive() {
			return d.collect(ctx, account.ID, owed, "BANK")
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// Scenario mechanics
// ---------------------------------------------------------------------------

func (d *demoBuilder) collect(ctx context.Context, accountID shared.ID, amount money.Amount, method string) error {
	if !amount.IsPositive() {
		return nil
	}
	// Cash needs an open drawer, so the demo opens one lazily the first time it
	// takes cash — the same requirement a real cashier faces.
	if method == payment.MethodCash {
		if err := d.ensureCashierSession(ctx); err != nil {
			return err
		}
	}

	// The tail of the identifier, not the head: these are UUIDv7 values whose
	// leading hex digits are a millisecond timestamp, so two payments recorded
	// in the same millisecond produced the same reference — and the university
	// treats a repeated bank reference as the same transfer entered twice.
	id := shared.NewID().String()
	reference := "REF-" + id[len(id)-12:]
	_, err := d.payments.RecordPayment(ctx, d.cashier, app.RecordPaymentInput{
		AccountID:       accountID,
		Amount:          amount,
		PaymentMethodID: d.methods[method],
		MethodReference: &reference,
		IdempotencyKey:  shared.NewID().String(),
		PayloadHash:     "demo",
	})
	if err != nil {
		return err
	}
	d.counts["payments"]++
	return nil
}

func (d *demoBuilder) ensureCashierSession(ctx context.Context) error {
	if _, err := d.deps.Sessions.GetOpenForUser(ctx, d.cashier.UserID); err == nil {
		return nil
	}
	session, err := payment.NewCashierSession(
		d.cashier.UserID, d.deskID, d.yearIDs["2025-2026"], 250_000)
	if err != nil {
		return err
	}
	return d.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return d.deps.Sessions.Open(ctx, session)
	})
}

func (d *demoBuilder) refundLast(ctx context.Context, accountID shared.ID, amount money.Amount) error {
	payments, err := d.deps.Payments.ListForAccount(ctx, accountID)
	if err != nil || len(payments) == 0 {
		return err
	}
	target := payments[0]

	refund, err := d.refunds.RequestRefund(ctx, d.cashier, app.RequestRefundInput{
		PaymentID:       target.ID,
		Amount:          amount,
		PaymentMethodID: d.methods["BANK"],
		Reason:          "انسحاب من مادة دراسية",
	})
	if err != nil {
		return err
	}
	if _, err := d.refunds.ApproveRefund(ctx, d.finance, refund.ID); err != nil {
		return err
	}
	if _, err := d.refunds.PostRefund(ctx, d.finance, refund.ID); err != nil {
		return err
	}
	d.counts["refunds"]++
	return nil
}

func (d *demoBuilder) voidLast(ctx context.Context, accountID shared.ID) error {
	payments, err := d.deps.Payments.ListForAccount(ctx, accountID)
	if err != nil || len(payments) == 0 {
		return err
	}
	request, err := d.payments.RequestVoid(ctx, d.cashier, payments[0].ID,
		"سُجلت على الطالب الخطأ")
	if err != nil {
		return err
	}
	// A different person executes it. That is the whole point of splitting the
	// request from the execution.
	if _, err := d.payments.ExecuteVoid(ctx, d.finance, request.ID); err != nil {
		return err
	}
	d.counts["voids"]++
	return nil
}

// supersede moves a student to morning study mid-year and prices the
// replacement, which is where the carried credit lands.
//
// Generating the replacement account is part of the scenario, not an
// afterthought: without it the money the student already paid sits in a credit
// row nobody spends, and the demo would show a case that looks resolved but
// is not.
func (d *demoBuilder) supersede(ctx context.Context, enrollmentID shared.ID) error {
	morning := d.studyType[academic.StudyTypeMorning]
	result, err := d.enrollments.SupersedeEnrollment(ctx, d.registrar, app.SupersedeInput{
		EnrollmentID:   enrollmentID,
		NewStudyTypeID: &morning,
		Reason:         "نقل من المسائي إلى الصباحي بقرار العمادة",
		EffectiveDate:  shared.NewDate(2026, time.January, 15),
	})
	if err != nil {
		return err
	}
	d.counts["supersedes"]++

	generated, err := d.accounts.GenerateFinancialAccount(ctx, d.finance, app.GenerateAccountInput{
		EnrollmentID: result.Replacement.ID,
	})
	if err != nil {
		return err
	}
	d.counts["accounts"]++
	if generated.CarriedCredit.IsPositive() {
		d.counts["credit carried forward"]++
	}
	return nil
}

// attachHosting records an incoming hosted student whose home university
// collects the tuition, so no account of ours should carry their debt.
func (d *demoBuilder) attachHosting(ctx context.Context, enrollmentID shared.ID) error {
	record, err := academic.NewHostingRecord(enrollmentID, academic.HostingIncoming, academic.CollectorHome)
	if err != nil {
		return err
	}
	record.HomeUniversity = ptrTo("جامعة البصرة")
	record.HomeCollege = ptrTo("كلية الهندسة")
	record.HomeDepartment = ptrTo("الهندسة الكهربائية")
	homeType := d.studyType[academic.StudyTypeMorning]
	record.HomeStudyTypeID = &homeType
	record.AgreementRef = ptrTo("كتاب استضافة 2025/1187")

	if err := record.Validate(); err != nil {
		return err
	}
	if err := d.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return d.deps.Enrollments.CreateHostingRecord(ctx, record)
	}); err != nil {
		return err
	}
	d.counts["hosting records"]++
	return nil
}

// promoteFrom rolls a year's students into the next, following each one's
// result: a pass moves up a stage, a failure repeats at a higher attempt
// number and a repeat-student fee.
func (d *demoBuilder) promoteFrom(ctx context.Context, fromCode, toCode string) error {
	fromID := d.yearIDs[fromCode]
	toID := d.yearIDs[toCode]

	enrollments, _, err := d.deps.Enrollments.List(ctx, port.EnrollmentFilter{
		AcademicYearID:    &fromID,
		ExcludeSuperseded: true,
		Limit:             500,
	})
	if err != nil {
		return err
	}

	for _, e := range enrollments {
		if e.Status != academic.StatusActive {
			continue
		}

		department, err := d.deps.Reference.GetDepartment(ctx, e.DepartmentID)
		if err != nil {
			return err
		}

		nextStage := e.Stage
		category := academic.CategoryRepeat
		if e.Passed() {
			nextStage = e.Stage + 1
			category = academic.CategoryRegular
		}
		if nextStage > department.StageCount {
			// Finished the programme: completed, not promoted into a stage
			// that does not exist.
			if _, err := d.enrollments.ChangeEnrollmentStatus(ctx, d.officer, app.ChangeStatusInput{
				EnrollmentID: e.ID, Target: academic.StatusCompleted,
			}); err != nil {
				return err
			}
			d.counts["graduated"]++
			continue
		}

		enrolled, err := d.enrollments.EnrollStudent(ctx, d.registrar, app.EnrollStudentInput{
			StudentID:            e.StudentID,
			AcademicYearID:       toID,
			DepartmentID:         e.DepartmentID,
			StudyTypeID:          e.StudyTypeID,
			Stage:                nextStage,
			CategoryCode:         category,
			PreviousEnrollmentID: &e.ID,
		})
		if err != nil {
			return err
		}
		d.counts["enrollments"]++
		if e.Passed() {
			d.counts["promoted"]++
		} else {
			d.counts["repeating"]++
		}

		account, err := d.accounts.GenerateFinancialAccount(ctx, d.finance, app.GenerateAccountInput{
			EnrollmentID: enrolled.Enrollment.ID,
		})
		if err != nil {
			return err
		}
		d.counts["accounts"]++

		// Older years are mostly settled; the current one is a mix.
		if toCode != "2025-2026" {
			if owed := account.Account.Remaining(); owed.IsPositive() {
				if err := d.collect(ctx, account.Account.ID, owed, "BANK"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// returnAfterDropout brings back the student who abandoned 2023-2024, with the
// debt from that year still on its own account where it belongs.
func (d *demoBuilder) returnAfterDropout(ctx context.Context) error {
	person, err := d.deps.Students.GetByStudentNo(ctx, "CIV-2023-005")
	if err != nil {
		return err
	}

	history, err := d.deps.Enrollments.History(ctx, person.ID)
	if err != nil || len(history) == 0 {
		return err
	}
	last := history[len(history)-1]

	enrolled, err := d.enrollments.EnrollStudent(ctx, d.registrar, app.EnrollStudentInput{
		StudentID:            person.ID,
		AcademicYearID:       d.yearIDs["2025-2026"],
		DepartmentID:         last.DepartmentID,
		StudyTypeID:          last.StudyTypeID,
		Stage:                last.Stage,
		PreviousEnrollmentID: &last.ID,
	})
	if err != nil {
		return err
	}
	d.counts["returned after dropout"]++

	if _, err := d.accounts.GenerateFinancialAccount(ctx, d.finance, app.GenerateAccountInput{
		EnrollmentID: enrolled.Enrollment.ID,
	}); err != nil {
		return err
	}
	d.counts["accounts"]++
	return nil
}

// closeOldYears records results for the finished years and shuts their books,
// leaving 2025-2026 open. The result is a database showing all three states of
// the year lifecycle at once.
func (d *demoBuilder) closeOldYears(ctx context.Context) error {
	for _, code := range []string{"2023-2024", "2024-2025"} {
		yearID := d.yearIDs[code]

		enrollments, _, err := d.deps.Enrollments.List(ctx, port.EnrollmentFilter{
			AcademicYearID: &yearID, Limit: 500,
		})
		if err != nil {
			return err
		}

		for _, e := range enrollments {
			if e.Status != academic.StatusActive || e.Result != academic.ResultPending {
				continue
			}
			// The demo already recorded results during promotion for most; this
			// catches the stragglers so the year can close.
			if _, err := d.enrollments.RecordAcademicResult(ctx, d.officer, app.RecordResultInput{
				EnrollmentID: e.ID, Result: academic.ResultPassedR1,
			}); err != nil {
				return err
			}
		}

		if _, err := d.years.CloseYearFinancially(ctx, d.finance, yearID); err != nil {
			return fmt.Errorf("closing %s financially: %w", code, err)
		}
		d.counts["years financially closed"]++
	}
	return nil
}

func (d *demoBuilder) printSummary() {
	fmt.Printf("\nDemo data loaded.\n\n")
	for _, key := range []string{
		"users", "departments", "fee policies", "templates", "discounts",
		"students", "enrollments", "accounts", "discount grants",
		"payments", "refunds", "voids", "supersedes", "hosting records",
		"passed", "failed", "deferred", "dropped out",
		"promoted", "repeating", "graduated", "returned after dropout",
		"credit carried forward",
		"years financially closed",
	} {
		if n := d.counts[key]; n > 0 {
			fmt.Printf("  %-26s %d\n", key, n)
		}
	}
	fmt.Printf("\nSign in with any of: admin, registrar, officer, finance, cashier, auditor, viewer\n")
	fmt.Printf("Password for all of them: %s\n", demoPassword)
	fmt.Printf("The cashier must sign in at desk D01.\n\n")
	fmt.Printf("Years: 2023-2024 and 2024-2025 are financially closed, 2025-2026 is open.\n\n")
}

const demoPassword = "demo-password-1234"

func ptrTo[T any](v T) *T { return &v }
