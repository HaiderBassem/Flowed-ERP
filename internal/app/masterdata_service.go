package app

import (
	"context"
	"strings"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// MasterDataService administers the reference tables the rest of the system
// resolves against: colleges, departments, study types, student categories,
// payment methods and cashier desks.
//
// Two rules run through every command here, and they are the reason this is a
// service rather than a set of CRUD endpoints.
//
// Nothing is ever deleted. A college referenced by an enrollment from 2019
// cannot go away without orphaning it, so retirement is a flag: the row stops
// being offered for new work and keeps answering for old work.
//
// A field whose value changed the meaning of something financial cannot be
// edited once that something exists. A payment method cannot stop being cash
// after cash has been taken through it — the flag decides what a cashier's
// drawer was expected to hold, and flipping it would move a historical
// collection between the drawer and the bank. The guard is a usage count, and
// the refusal names what to do instead.
type MasterDataService struct {
	deps Deps
	auditor
}

// NewMasterDataService wires the reference-data commands.
func NewMasterDataService(d Deps) *MasterDataService {
	return &MasterDataService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// UpdateCollegeInput renames a college or retires it.
type UpdateCollegeInput struct {
	ID       shared.ID
	NameAr   *string
	NameEn   *string
	IsActive *bool
	Reason   string
}

// UpdateCollege renames or retires a college.
func (s *MasterDataService) UpdateCollege(ctx context.Context, actor shared.Actor, in UpdateCollegeInput) (*academic.College, error) {
	var college *academic.College
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		college, err = s.deps.Reference.GetCollege(ctx, in.ID)
		if err != nil {
			return err
		}
		before := snapshotOf(college)

		if in.NameAr != nil {
			if strings.TrimSpace(*in.NameAr) == "" {
				return shared.Validation("college.name_required", "a college needs a name")
			}
			college.NameAr = strings.TrimSpace(*in.NameAr)
		}
		if in.NameEn != nil {
			college.NameEn = in.NameEn
		}
		if in.IsActive != nil {
			college.IsActive = *in.IsActive
		}

		if err := s.deps.Reference.UpdateCollege(ctx, college); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "college", college.ID, before, snapshotOf(college), in.Reason, nil)
	})
	if err != nil {
		return nil, err
	}
	return college, nil
}

// UpdateDepartmentInput renames a department, changes its length or retires it.
type UpdateDepartmentInput struct {
	ID     shared.ID
	NameAr *string
	NameEn *string
	// StageCount changes how many years the programme runs. Reducing it below
	// a stage students are actually registered in is refused: the final stage
	// is what decides whether completing an enrollment is a promotion or a
	// graduation, and moving it would turn recorded graduations into
	// promotions.
	StageCount *int16
	IsActive   *bool
	Reason     string
}

// UpdateDepartment renames a department, changes its length or retires it.
func (s *MasterDataService) UpdateDepartment(ctx context.Context, actor shared.Actor, in UpdateDepartmentInput) (*academic.Department, error) {
	var department *academic.Department
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		department, err = s.deps.Reference.GetDepartment(ctx, in.ID)
		if err != nil {
			return err
		}
		before := snapshotOf(department)

		if in.NameAr != nil {
			if strings.TrimSpace(*in.NameAr) == "" {
				return shared.Validation("department.name_required", "a department needs a name")
			}
			department.NameAr = strings.TrimSpace(*in.NameAr)
		}
		if in.NameEn != nil {
			department.NameEn = in.NameEn
		}
		if in.IsActive != nil {
			department.IsActive = *in.IsActive
		}
		if in.StageCount != nil && *in.StageCount != department.StageCount {
			if *in.StageCount < 1 || *in.StageCount > 5 {
				return shared.Validation("department.stage_count_range",
					"a programme runs between 1 and 5 stages, got %d", *in.StageCount)
			}
			highest, err := s.deps.Reference.HighestStageInUse(ctx, department.ID)
			if err != nil {
				return err
			}
			if *in.StageCount < highest {
				return shared.PreconditionFailed("department.stage_count_in_use",
					"students are registered in stage %d, so this programme cannot be shortened to %d stages",
					highest, *in.StageCount).
					WithDetail("highest_stage_in_use", highest).
					WithDetail("remedy", "retire this department and create the shorter programme as a new one, "+
						"so the students already in it keep the length they enrolled under")
			}
			if strings.TrimSpace(in.Reason) == "" {
				return shared.Validation("department.reason_required",
					"changing how many years a programme runs decides who is a graduate; it requires a reason")
			}
			department.StageCount = *in.StageCount
		}

		if err := s.deps.Reference.UpdateDepartment(ctx, department); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "department", department.ID, before, snapshotOf(department), in.Reason, nil)
	})
	if err != nil {
		return nil, err
	}
	return department, nil
}

// UpdateStudyTypeInput renames a study type, reorders it or retires it.
type UpdateStudyTypeInput struct {
	ID        shared.ID
	NameAr    *string
	NameEn    *string
	SortOrder *int16
	IsActive  *bool
	Reason    string
}

// UpdateStudyType renames, reorders or retires a study type.
func (s *MasterDataService) UpdateStudyType(ctx context.Context, actor shared.Actor, in UpdateStudyTypeInput) (*academic.StudyType, error) {
	var studyType *academic.StudyType
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		studyType, err = s.deps.Reference.GetStudyType(ctx, in.ID)
		if err != nil {
			return err
		}
		before := snapshotOf(studyType)

		if in.NameAr != nil {
			if strings.TrimSpace(*in.NameAr) == "" {
				return shared.Validation("study_type.name_required", "a study type needs a name")
			}
			studyType.NameAr = strings.TrimSpace(*in.NameAr)
		}
		if in.NameEn != nil {
			studyType.NameEn = in.NameEn
		}
		if in.SortOrder != nil {
			studyType.SortOrder = *in.SortOrder
		}
		if in.IsActive != nil {
			studyType.IsActive = *in.IsActive
		}

		if err := s.deps.Reference.UpdateStudyType(ctx, studyType); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "study_type", studyType.ID, before, snapshotOf(studyType), in.Reason, nil)
	})
	if err != nil {
		return nil, err
	}
	return studyType, nil
}

// CreateStudentCategoryInput adds a category fee policy can resolve against.
type CreateStudentCategoryInput struct {
	Code   string
	NameAr string
	NameEn *string
}

// CreateStudentCategory adds a category.
//
// Categories drive fee resolution — the repeat student's higher tuition is a
// policy row keyed on one — so adding one is how a new pricing rule becomes
// possible without a branch in code.
func (s *MasterDataService) CreateStudentCategory(ctx context.Context, actor shared.Actor, in CreateStudentCategoryInput) (*academic.StudentCategory, error) {
	category, err := academic.NewStudentCategory(in.Code, in.NameAr)
	if err != nil {
		return nil, err
	}
	category.NameEn = in.NameEn

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.deps.Reference.CreateStudentCategory(ctx, category); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "student_category", category.ID, nil, snapshotOf(category), "", nil)
	})
	if err != nil {
		return nil, err
	}
	return category, nil
}

// UpdateStudentCategoryInput renames a category or retires it.
type UpdateStudentCategoryInput struct {
	ID       shared.ID
	NameAr   *string
	NameEn   *string
	IsActive *bool
	Reason   string
}

// UpdateStudentCategory renames or retires a category.
func (s *MasterDataService) UpdateStudentCategory(ctx context.Context, actor shared.Actor, in UpdateStudentCategoryInput) (*academic.StudentCategory, error) {
	var category *academic.StudentCategory
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		category, err = s.deps.Reference.GetStudentCategory(ctx, in.ID)
		if err != nil {
			return err
		}
		before := snapshotOf(category)

		if in.NameAr != nil {
			if strings.TrimSpace(*in.NameAr) == "" {
				return shared.Validation("student_category.name_required", "a category needs a name")
			}
			category.NameAr = strings.TrimSpace(*in.NameAr)
		}
		if in.NameEn != nil {
			category.NameEn = in.NameEn
		}
		if in.IsActive != nil {
			category.IsActive = *in.IsActive
		}

		if err := s.deps.Reference.UpdateStudentCategory(ctx, category); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "student_category", category.ID, before, snapshotOf(category), in.Reason, nil)
	})
	if err != nil {
		return nil, err
	}
	return category, nil
}

// CreatePaymentMethodInput adds a way of paying.
type CreatePaymentMethodInput struct {
	Code   string
	NameAr string
	// IsCash decides whether a collection through this method lands in a
	// cashier's drawer and is counted at shift close.
	IsCash bool
	// RequiresReference demands a bank or terminal reference on every payment,
	// which is what makes the collection reconcilable against a statement.
	RequiresReference bool
	SortOrder         int16
}

// CreatePaymentMethod adds a way of paying.
func (s *MasterDataService) CreatePaymentMethod(ctx context.Context, actor shared.Actor, in CreatePaymentMethodInput) (*payment.Method, error) {
	method, err := payment.NewMethod(in.Code, in.NameAr, in.IsCash, in.RequiresReference)
	if err != nil {
		return nil, err
	}
	method.SortOrder = in.SortOrder

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if err := s.deps.Reference.CreatePaymentMethod(ctx, method); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "payment_method", method.ID, nil, snapshotOf(method), "", map[string]any{
			"is_cash":            method.IsCash,
			"requires_reference": method.RequiresReference,
		})
	})
	if err != nil {
		return nil, err
	}
	return method, nil
}

// UpdatePaymentMethodInput renames a method, changes its rules or retires it.
type UpdatePaymentMethodInput struct {
	ID                shared.ID
	NameAr            *string
	IsCash            *bool
	RequiresReference *bool
	IsActive          *bool
	SortOrder         *int16
	Reason            string
}

// UpdatePaymentMethod renames a method, changes its rules or retires it.
//
// The cash flag is frozen once money has moved through the method. It decides
// what a cashier's drawer should contain at shift close, so changing it
// retroactively would reclassify collections that have already been counted —
// and the shift that balanced last week would stop balancing.
func (s *MasterDataService) UpdatePaymentMethod(ctx context.Context, actor shared.Actor, in UpdatePaymentMethodInput) (*payment.Method, error) {
	var method *payment.Method
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		method, err = s.deps.Reference.GetPaymentMethod(ctx, in.ID)
		if err != nil {
			return err
		}
		before := snapshotOf(method)

		if in.IsCash != nil && *in.IsCash != method.IsCash {
			used, err := s.deps.Reference.UsageCount(ctx, port.MasterPaymentMethod, method.ID)
			if err != nil {
				return err
			}
			if used > 0 {
				return shared.PreconditionFailed("payment_method.cash_flag_frozen",
					"%d payment(s) have already been taken through %s, so whether it is cash can no longer change",
					used, method.Code).
					WithDetail("payments", used).
					WithDetail("remedy", "retire this method and create a new one with the correct setting; "+
						"the collections already taken keep the meaning they were taken under")
			}
			method.IsCash = *in.IsCash
		}
		if in.NameAr != nil {
			if strings.TrimSpace(*in.NameAr) == "" {
				return shared.Validation("payment_method.name_required", "a payment method needs a name")
			}
			method.NameAr = strings.TrimSpace(*in.NameAr)
		}
		// Forward-looking only: it validates the next payment and says nothing
		// about the ones already taken, so it stays editable.
		if in.RequiresReference != nil {
			method.RequiresReference = *in.RequiresReference
		}
		if in.IsActive != nil {
			method.IsActive = *in.IsActive
		}
		if in.SortOrder != nil {
			method.SortOrder = *in.SortOrder
		}

		if err := s.deps.Reference.UpdatePaymentMethod(ctx, method); err != nil {
			return err
		}
		return s.recordMasterChange(ctx, actor, "payment_method", method.ID, before, snapshotOf(method), in.Reason, nil)
	})
	if err != nil {
		return nil, err
	}
	return method, nil
}

// ListStudentCategories returns the categories fee policy resolves against.
func (s *MasterDataService) ListStudentCategories(ctx context.Context, actor shared.Actor, activeOnly bool) ([]*academic.StudentCategory, error) {
	return s.deps.Reference.ListStudentCategories(ctx, activeOnly)
}

// recordMasterChange writes the audit entry for a reference-data change.
//
// Master data is not financial, but it decides how financial rows are priced
// and counted: a study type renamed appears on every future receipt, a
// department shortened changes who is a graduate. Every change is recorded with
// its before and after.
func (s *MasterDataService) recordMasterChange(
	ctx context.Context, actor shared.Actor, entity string, id shared.ID,
	before, after any, reason string, extra map[string]any,
) error {
	action := entity + ".updated"
	if before == nil {
		action = entity + ".created"
	}
	metadata := map[string]any{}
	for k, v := range extra {
		metadata[k] = v
	}
	return s.record(ctx, port.AuditEntry{
		EntityType: entity,
		EntityID:   &id,
		Action:     action,
		Actor:      actor,
		Before:     before,
		After:      after,
		Reason:     reasonOrNil(reason),
		Metadata:   metadata,
	})
}
