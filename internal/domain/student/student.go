// Package student holds student identity: who a person is, and nothing about
// what they study or what they owe.
//
// The boundary is strict. If a field's value could differ between two academic
// years — stage, department, study type, fees, status in a programme — it
// belongs on an enrollment, not here. A student row is the one thing in the
// system that stays true across six years of study.
package student

import (
	"regexp"
	"strings"
	"time"

	"github.com/swibit/flowed/internal/domain/shared"
)

// Status is the person's standing with the university, derived from their
// enrollments rather than set independently.
type Status string

const (
	// StatusActive is a student currently studying or eligible to register.
	StatusActive Status = "active"
	// StatusSeparated has withdrawn or dropped out and may return.
	StatusSeparated Status = "separated"
	// StatusTransferredOut has moved to another institution.
	StatusTransferredOut Status = "transferred_out"
	// StatusGraduated has completed the programme.
	StatusGraduated Status = "graduated"
	// StatusDeceased is terminal.
	StatusDeceased Status = "deceased"
	// StatusMerged marks a duplicate record folded into another. The row stays
	// so that a receipt printed against it still resolves.
	StatusMerged Status = "merged"
)

// Gender as recorded on civil documents.
type Gender string

const (
	GenderMale   Gender = "male"
	GenderFemale Gender = "female"
)

var studentNoPattern = regexp.MustCompile(`^[A-Za-z0-9/_-]{1,32}$`)

// Student is a person known to the university.
type Student struct {
	ID        shared.ID
	StudentNo string

	// Current legal identity, denormalised from the version history for search
	// and display. IdentityVersion rows remain the record of what was true when.
	FullName   string
	MotherName string
	NationalID *string
	BirthDate  *shared.Date
	Gender     *Gender

	// Contact details are overwritten in place. Nobody needs to know which
	// phone number a student had in 2024, only how to reach them now.
	Phone         *string
	PhoneAlt      *string
	Email         *string
	Address       *string
	GuardianName  *string
	GuardianPhone *string

	FirstAdmissionYear *string
	Status             Status
	MergedIntoID       *shared.ID
	Notes              *string

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy *shared.ID
}

// NewParams carries what is needed to register a person.
type NewParams struct {
	StudentNo  string
	FullName   string
	MotherName string
	NationalID *string
	BirthDate  *shared.Date
	Gender     *Gender
	Phone      *string
	CreatedBy  *shared.ID
}

// New builds a student after validating the identity fields.
//
// The mother's name is mandatory, not optional. Iraqi universities routinely
// hold several students with identical four-part names, and the mother's name
// is the discriminator every registrar uses. A system that treats it as a
// nice-to-have cannot tell two people apart at the cashier's window.
func New(p NewParams) (*Student, error) {
	studentNo := strings.TrimSpace(p.StudentNo)
	if studentNo == "" {
		return nil, shared.Validation("student.number_required", "a university number is required")
	}
	if !studentNoPattern.MatchString(studentNo) {
		return nil, shared.Validation("student.invalid_number",
			"the university number %q contains characters that are not allowed", studentNo)
	}

	fullName := collapseSpaces(p.FullName)
	if fullName == "" {
		return nil, shared.Validation("student.name_required", "the student's full name is required")
	}
	motherName := collapseSpaces(p.MotherName)
	if motherName == "" {
		return nil, shared.Validation("student.mother_name_required",
			"the mother's name is required: it is how students with identical names are told apart")
	}
	if p.Gender != nil && *p.Gender != GenderMale && *p.Gender != GenderFemale {
		return nil, shared.Validation("student.invalid_gender", "gender must be male or female")
	}

	return &Student{
		ID:         shared.NewID(),
		StudentNo:  studentNo,
		FullName:   fullName,
		MotherName: motherName,
		NationalID: trimOptional(p.NationalID),
		BirthDate:  p.BirthDate,
		Gender:     p.Gender,
		Phone:      trimOptional(p.Phone),
		Status:     StatusActive,
		CreatedBy:  p.CreatedBy,
	}, nil
}

// CanEnroll reports whether the person may register for a year.
func (s *Student) CanEnroll() bool {
	switch s.Status {
	case StatusActive, StatusSeparated:
		return true
	default:
		return false
	}
}

// RequireEnrollable returns a precondition error unless the person may register.
func (s *Student) RequireEnrollable() error {
	if s.CanEnroll() {
		return nil
	}
	return shared.PreconditionFailed("student.not_enrollable",
		"student %s is %s and cannot be enrolled", s.StudentNo, s.Status).
		WithDetail("student_no", s.StudentNo).
		WithDetail("status", string(s.Status))
}

// MarkSeparated records that the student left without completing.
func (s *Student) MarkSeparated() { s.Status = StatusSeparated }

// MarkReturned brings a separated student back onto the register.
func (s *Student) MarkReturned() error {
	if s.Status != StatusSeparated {
		return shared.PreconditionFailed("student.not_separated",
			"only a separated student can be returned; %s is %s", s.StudentNo, s.Status)
	}
	s.Status = StatusActive
	return nil
}

// MarkGraduated records completion. Derived from a completed final-stage
// enrollment rather than set by hand.
func (s *Student) MarkGraduated() { s.Status = StatusGraduated }

// MarkTransferredOut records a move to another institution.
func (s *Student) MarkTransferredOut() { s.Status = StatusTransferredOut }

// IdentityVersion is one legally documented state of a person's identity.
//
// Iraqi courts change names and civil-registry details, and a document issued
// before a change must keep referring to the identity in force when it was
// printed. These rows are never edited and never deleted.
type IdentityVersion struct {
	ID                shared.ID
	StudentID         shared.ID
	VersionNo         int32
	FullName          string
	MotherName        string
	NationalID        *string
	BirthDate         *shared.Date
	BirthPlace        *string
	Gender            *Gender
	Nationality       *string
	EffectiveFrom     shared.Date
	CourtDecisionNo   *string
	CourtDecisionDate *shared.Date
	DocumentRef       *string
	ChangeReason      string
	RecordedAt        time.Time
	RecordedBy        *shared.ID
}

// NewIdentityVersion records a documented identity change.
func NewIdentityVersion(s *Student, versionNo int32, reason string, effectiveFrom shared.Date) (*IdentityVersion, error) {
	if reason == "" {
		return nil, shared.Validation("student.identity_reason_required",
			"an identity change requires a documented reason")
	}
	if versionNo < 1 {
		return nil, shared.Validation("student.invalid_version", "version number must be at least 1")
	}
	return &IdentityVersion{
		ID:            shared.NewID(),
		StudentID:     s.ID,
		VersionNo:     versionNo,
		FullName:      s.FullName,
		MotherName:    s.MotherName,
		NationalID:    s.NationalID,
		BirthDate:     s.BirthDate,
		Gender:        s.Gender,
		EffectiveFrom: effectiveFrom,
		ChangeReason:  reason,
	}, nil
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func trimOptional(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
