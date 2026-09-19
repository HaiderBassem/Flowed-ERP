package academic

import (
	"regexp"
	"strings"
	"time"

	"flowed/internal/domain/shared"
)

var codePattern = regexp.MustCompile(`^[A-Z0-9_]{2,32}$`)

// College is a faculty of the university.
type College struct {
	ID        shared.ID
	Code      string
	NameAr    string
	NameEn    *string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewCollege builds a college.
func NewCollege(code, nameAr string) (*College, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !codePattern.MatchString(code) {
		return nil, shared.Validation("college.invalid_code",
			"a college code must be 2 to 32 upper-case letters, digits or underscores, got %q", code)
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("college.name_required", "the Arabic name is required")
	}
	return &College{ID: shared.NewID(), Code: code, NameAr: strings.TrimSpace(nameAr), IsActive: true}, nil
}

// Department is a programme within a college.
type Department struct {
	ID        shared.ID
	CollegeID shared.ID
	Code      string
	NameAr    string
	NameEn    *string
	// StageCount is the programme length in years, capped university-wide at
	// five (First through Fifth Stage).
	StageCount int16
	IsActive   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewDepartment builds a department.
func NewDepartment(collegeID shared.ID, code, nameAr string, stageCount int16) (*Department, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !codePattern.MatchString(code) {
		return nil, shared.Validation("department.invalid_code",
			"a department code must be 2 to 32 upper-case letters, digits or underscores, got %q", code)
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("department.name_required", "the Arabic name is required")
	}
	if stageCount < 1 || stageCount > 5 {
		return nil, shared.Validation("department.invalid_stage_count",
			"a programme runs between 1 and 5 years, got %d", stageCount)
	}
	return &Department{
		ID:         shared.NewID(),
		CollegeID:  collegeID,
		Code:       code,
		NameAr:     strings.TrimSpace(nameAr),
		StageCount: stageCount,
		IsActive:   true,
	}, nil
}

// StudyType is a mode of study: morning, evening, parallel, or whatever the
// ministry introduces next.
//
// It is master data on purpose. Hosting is deliberately not one of these
// values: a hosted student has both a home study type and the one they attend,
// and a single "hosting" value would destroy that pair and force fabricated
// fee-policy rows for each direction. Hosting is an overlay on the enrollment.
type StudyType struct {
	ID        shared.ID
	Code      string
	NameAr    string
	NameEn    *string
	SortOrder int16
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewStudyType builds a study type.
func NewStudyType(code, nameAr string, sortOrder int16) (*StudyType, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !codePattern.MatchString(code) {
		return nil, shared.Validation("study_type.invalid_code",
			"a study type code must be 2 to 32 upper-case letters, digits or underscores, got %q", code)
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("study_type.name_required", "the Arabic name is required")
	}
	return &StudyType{
		ID:        shared.NewID(),
		Code:      code,
		NameAr:    strings.TrimSpace(nameAr),
		SortOrder: sortOrder,
		IsActive:  true,
	}, nil
}

// Well-known study type codes, seeded by migration. Code that needs to
// reference one uses these rather than a string literal, but no logic branches
// on them: an administrator's new study type behaves exactly like these.
const (
	StudyTypeMorning  = "MORNING"
	StudyTypeEvening  = "EVENING"
	StudyTypeParallel = "PARALLEL"
)

// StudentCategory feeds fee resolution: a repeating student may owe a
// different tuition than a first-attempt student in the same seat.
type StudentCategory struct {
	ID        shared.ID
	Code      string
	NameAr    string
	NameEn    *string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewStudentCategory builds a category.
//
// The code is the part that matters: fee policy resolves against it, so
// creating one is how a university introduces a new pricing rule — a
// scholarship cohort, a returning-student rate — without a branch in code.
func NewStudentCategory(code, nameAr string) (*StudentCategory, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !codePattern.MatchString(code) {
		return nil, shared.Validation("student_category.invalid_code",
			"a category code must be 2 to 32 upper-case letters, digits or underscores, got %q", code)
	}
	if strings.TrimSpace(nameAr) == "" {
		return nil, shared.Validation("student_category.name_required", "the Arabic name is required")
	}
	return &StudentCategory{
		ID:       shared.NewID(),
		Code:     code,
		NameAr:   strings.TrimSpace(nameAr),
		IsActive: true,
	}, nil
}

// Well-known category codes, seeded by migration.
const (
	CategoryRegular  = "REGULAR"
	CategoryRepeat   = "REPEAT"
	CategoryHosted   = "HOSTED"
	CategoryTransfer = "TRANSFER"
)

// HostingDirection distinguishes a student we receive from one we send.
type HostingDirection string

const (
	// HostingIncoming is another institution's student attending here.
	HostingIncoming HostingDirection = "incoming"
	// HostingOutgoing is our student attending elsewhere. Their enrollment
	// with us stays active, because their results still come back to us.
	HostingOutgoing HostingDirection = "outgoing"
)

// FeeCollector says which institution collects tuition for a hosted student.
// Iraqi practice varies by agreement, so it is recorded per hosting record
// rather than assumed.
type FeeCollector string

const (
	// CollectorHome means the student's own university collects; we generate
	// no financial account for an incoming hosted student.
	CollectorHome FeeCollector = "home"
	// CollectorHost means the receiving institution collects.
	CollectorHost FeeCollector = "host"
	// CollectorSplit means home tuition plus a local service fee.
	CollectorSplit FeeCollector = "split"
)

// AllFeeCollectors lists every arrangement, for validation and for a UI to
// offer. The design leaves which one applies deliberately unresolved: it is
// settled per agreement with the other institution, not in code.
var AllFeeCollectors = []FeeCollector{CollectorHome, CollectorHost, CollectorSplit}

// Valid reports whether the collector is one the system recognises.
func (c FeeCollector) Valid() bool {
	for _, known := range AllFeeCollectors {
		if c == known {
			return true
		}
	}
	return false
}

// HostingRecord is the overlay that turns an ordinary enrollment into a hosted
// one, in either direction.
type HostingRecord struct {
	ID           shared.ID
	EnrollmentID shared.ID
	Direction    HostingDirection

	HomeUniversity  *string
	HomeCollege     *string
	HomeDepartment  *string
	HomeStudyTypeID *shared.ID

	HostUniversity  *string
	HostCollege     *string
	HostDepartment  *string
	HostStudyTypeID *shared.ID

	FeeCollector FeeCollector
	PeriodFrom   *shared.Date
	PeriodTo     *shared.Date
	AgreementRef *string
	Notes        *string

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy *shared.ID
}

// NewHostingRecord builds a hosting overlay, validating that the direction
// carries the context that direction requires.
func NewHostingRecord(enrollmentID shared.ID, direction HostingDirection, collector FeeCollector) (*HostingRecord, error) {
	switch direction {
	case HostingIncoming, HostingOutgoing:
	default:
		return nil, shared.Validation("hosting.invalid_direction",
			"hosting direction must be incoming or outgoing, got %q", direction)
	}
	switch collector {
	case CollectorHome, CollectorHost, CollectorSplit:
	case "":
		collector = CollectorHome
	default:
		return nil, shared.Validation("hosting.invalid_fee_collector",
			"the fee collector must be home, host or split, got %q", collector)
	}
	return &HostingRecord{
		ID:           shared.NewID(),
		EnrollmentID: enrollmentID,
		Direction:    direction,
		FeeCollector: collector,
	}, nil
}

// GeneratesLocalAccount reports whether we bill this hosted student.
//
// An incoming student whose home university collects the tuition gets no
// financial account here. Creating one would put a debt on our books that
// belongs on someone else's.
func (h *HostingRecord) GeneratesLocalAccount() bool {
	if h.Direction == HostingOutgoing {
		// Our own student: we bill them as usual unless the agreement says the
		// host collects everything.
		return h.FeeCollector != CollectorHost
	}
	return h.FeeCollector == CollectorHost || h.FeeCollector == CollectorSplit
}

// Validate checks that the record carries the context its direction needs.
func (h *HostingRecord) Validate() error {
	if h.Direction == HostingIncoming && (h.HomeUniversity == nil || strings.TrimSpace(*h.HomeUniversity) == "") {
		return shared.Validation("hosting.home_university_required",
			"an incoming hosted student must record the university they came from")
	}
	if h.Direction == HostingOutgoing && (h.HostUniversity == nil || strings.TrimSpace(*h.HostUniversity) == "") {
		return shared.Validation("hosting.host_university_required",
			"an outgoing hosted student must record the university they went to")
	}
	if h.PeriodFrom != nil && h.PeriodTo != nil && h.PeriodTo.Before(*h.PeriodFrom) {
		return shared.Validation("hosting.invalid_period",
			"the hosting period ends before it starts")
	}
	return nil
}
