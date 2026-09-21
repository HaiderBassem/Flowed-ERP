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
	"unicode"

	"flowed/internal/domain/shared"
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

// iraqiMobilePattern matches the 10-digit subscriber number of an Iraqi
// mobile line once the country code and any leading trunk zero have been
// stripped: a 7 followed by an operator digit in 3..9 and eight more digits
// (Zain, Asiacell and Korek all issue in the 73x-79x ranges). Landlines and
// numbers from other countries are refused, not stored as free text.
var iraqiMobilePattern = regexp.MustCompile(`^7[3-9][0-9]{8}$`)

var nonDigitPattern = regexp.MustCompile(`\D`)

// NormalizeIraqiPhone validates a phone number entered in any common shape —
// +964 770 123 4567, 00964770123456, 07701234567, or the same digits typed on
// an Arabic keyboard — and canonicalises it to the local 11-digit form
// (leading 0 plus the 10-digit subscriber number) that the rest of the system
// stores and displays. A nil or blank input clears the field rather than
// erroring, matching the optional-phone behaviour callers already expect.
//
// This is the one place a phone number is accepted into the domain: reject
// here, and there is no path left by which an invalid number reaches the
// database, unlike the raw free-text column this used to be.
func NormalizeIraqiPhone(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil, nil
	}

	digits := nonDigitPattern.ReplaceAllString(arabicIndicToWestern(trimmed), "")
	switch {
	case strings.HasPrefix(digits, "00964"):
		digits = digits[5:]
	case strings.HasPrefix(digits, "964"):
		digits = digits[3:]
	case strings.HasPrefix(digits, "0"):
		digits = digits[1:]
	}

	if !iraqiMobilePattern.MatchString(digits) {
		return nil, shared.Validation("student.invalid_phone",
			"%q is not a valid Iraqi mobile number", trimmed)
	}
	canonical := "0" + digits
	return &canonical, nil
}

// arabicIndicToWestern maps Arabic-Indic and Extended Arabic-Indic numerals
// to their Western equivalents so a number typed on an Arabic keyboard
// validates the same as one typed on a Latin one.
func arabicIndicToWestern(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '٠' && r <= '٩':
			return '0' + (r - '٠')
		case r >= '۰' && r <= '۹':
			return '0' + (r - '۰')
		default:
			return r
		}
	}, s)
}

// Student is a person known to the university.
type Student struct {
	ID        shared.ID
	StudentNo string

	// Current legal identity, denormalised from the version history for search
	// and display. IdentityVersion rows remain the record of what was true when.
	FullName   string
	MotherName string
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

	// RegisteredOn is the day the office registered the student, which is not
	// CreatedAt: paper intake taken on Sunday is typed in on Tuesday, and a
	// report of "who registered this week" built on the row timestamp answers
	// the wrong question.
	RegisteredOn shared.Date

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy *shared.ID
}

// NewParams carries what is needed to register a person.
type NewParams struct {
	// StudentNo may be empty, and normally is: the repository asks the
	// database for the next number in the registration year's series. A
	// caller supplying one is importing a student who already has a number
	// printed on a document somewhere.
	StudentNo    string
	FullName     string
	MotherName   string
	BirthDate    *shared.Date
	Gender       *Gender
	Phone        *string
	Email        *string
	RegisteredOn shared.Date
	CreatedBy    *shared.ID
}

// New builds a student after validating the identity fields.
//
// The mother's name is mandatory, not optional. Iraqi universities routinely
// hold several students with identical four-part names, and the mother's name
// is the discriminator every registrar uses. A system that treats it as a
// nice-to-have cannot tell two people apart at the cashier's window.
func New(p NewParams) (*Student, error) {
	// An empty number is legitimate and is the ordinary case: the repository
	// takes the next one from the year's series inside the same transaction.
	// It is validated when it is supplied, because a number typed in by hand
	// is exactly the one worth checking.
	studentNo := strings.TrimSpace(p.StudentNo)
	if studentNo != "" && !studentNoPattern.MatchString(studentNo) {
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
	phone, err := NormalizeIraqiPhone(p.Phone)
	if err != nil {
		return nil, err
	}

	registeredOn := p.RegisteredOn
	if registeredOn.IsZero() {
		registeredOn = shared.DateFromTime(time.Now().UTC())
	}

	return &Student{
		ID:           shared.NewID(),
		StudentNo:    studentNo,
		FullName:     fullName,
		MotherName:   motherName,
		BirthDate:    p.BirthDate,
		Gender:       p.Gender,
		Phone:        phone,
		Email:        p.Email,
		RegisteredOn: registeredOn,
		Status:       StatusActive,
		CreatedBy:    p.CreatedBy,
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

// MergeInto folds this record into another, leaving it as a tombstone.
//
// The row is kept rather than deleted, and that is the whole point: a receipt
// printed under this student number, an enrollment reference in a ministry
// return, an audit entry naming this identifier — all of them must still
// resolve to a person years later. What the tombstone stops is the record being
// used again: it drops out of the searches that offer a student to enroll or to
// collect from, and it points at where the person actually is.
func (s *Student) MergeInto(target shared.ID) error {
	if s.Status == StatusMerged {
		return shared.Conflict("student.already_merged",
			"this record has already been merged into another")
	}
	if shared.IsNil(target) || target == s.ID {
		return shared.Validation("student.invalid_merge_target",
			"a record must be merged into a different, existing record")
	}
	s.Status = StatusMerged
	s.MergedIntoID = &target
	return nil
}

// FoldArabic normalises Arabic orthography for comparison, mirroring the
// normalize_arabic function the database applies to the search columns.
//
// Duplicated here rather than shared with SQL because the comparison happens in
// Go — deciding whether two records are the same person before a merge — and a
// round trip to fold two strings would be absurd. The two implementations are
// held together by the test that runs the same inputs through both.
func FoldArabic(input string) string {
	var b strings.Builder
	b.Grow(len(input))

	for _, r := range input {
		switch {
		// Tashkeel (harakat) and tatweel carry no lexical meaning and are
		// typed inconsistently.
		case r >= 0x064B && r <= 0x0652, r == 0x0640, r == 0x0670:
			continue
		// Hamza carriers all fold to bare alef: أحمد and احمد are one name.
		case r == 'أ' || r == 'إ' || r == 'آ' || r == 'ٱ':
			b.WriteRune('ا')
		case r == 'ؤ':
			b.WriteRune('و')
		case r == 'ئ' || r == 'ى':
			b.WriteRune('ي')
		// Ta marbuta to ha: فاطمه and فاطمة are one name.
		case r == 'ة':
			b.WriteRune('ه')
		case r == ' ':
			// Collapse runs of spaces; a name typed with a double space is the
			// same name.
			if b.Len() > 0 && !strings.HasSuffix(b.String(), " ") {
				b.WriteRune(' ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.TrimSpace(b.String())
}

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
		BirthDate:     s.BirthDate,
		Gender:        s.Gender,
		EffectiveFrom: effectiveFrom,
		ChangeReason:  reason,
	}, nil
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
