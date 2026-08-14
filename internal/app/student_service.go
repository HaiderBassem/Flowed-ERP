package app

import (
	"context"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
	"github.com/swibit/flowed/internal/port"
)

// StudentService handles student identity commands.
type StudentService struct {
	deps Deps
	auditor
}

// NewStudentService wires the student commands.
func NewStudentService(d Deps) *StudentService {
	return &StudentService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// RegisterStudentInput creates a person record.
type RegisterStudentInput struct {
	StudentNo  string
	FullName   string
	MotherName string
	NationalID *string
	BirthDate  *shared.Date
	Gender     *student.Gender
	Phone      *string
	Email      *string
	Address    *string
	// AcknowledgeDuplicates proceeds despite a probable match, recording the
	// override so a wrongly created duplicate can be traced to a decision.
	AcknowledgeDuplicates bool
}

// RegisterStudentResult carries the new record and any near-matches found.
type RegisterStudentResult struct {
	Student            *student.Student
	PossibleDuplicates []*student.Student
}

// RegisterStudent creates a student identity.
//
// Before creating anything it looks for the same person already on file. Iraqi
// universities routinely hold several students with identical four-part names,
// and the mother's name is the discriminator a registrar uses — so a match on
// name and mother is treated as a probable duplicate that a human must confirm
// past. Creating a second record for one person splits their payment history
// in a way that is painful to unpick later.
func (s *StudentService) RegisterStudent(ctx context.Context, actor shared.Actor, in RegisterStudentInput) (*RegisterStudentResult, error) {
	if err := actor.RequireAnyRole("RegisterStudent", shared.RoleRegistrar, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var result *RegisterStudentResult
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		duplicates, err := s.deps.Students.FindPossibleDuplicates(ctx, in.FullName, in.MotherName, in.BirthDate)
		if err != nil {
			return err
		}
		if len(duplicates) > 0 && !in.AcknowledgeDuplicates {
			existing := make([]map[string]any, 0, len(duplicates))
			for _, d := range duplicates {
				existing = append(existing, map[string]any{
					"student_id":  d.ID.String(),
					"student_no":  d.StudentNo,
					"full_name":   d.FullName,
					"mother_name": d.MotherName,
				})
			}
			return shared.Conflict("student.probable_duplicate",
				"%d existing student(s) share this name and mother's name; "+
					"confirm this is a different person before creating a second record",
				len(duplicates)).
				WithDetail("matches", existing).
				WithDetail("remedy", "resubmit with acknowledge_duplicates set to true, or enroll the existing student")
		}

		person, err := student.New(student.NewParams{
			StudentNo:  in.StudentNo,
			FullName:   in.FullName,
			MotherName: in.MotherName,
			NationalID: in.NationalID,
			BirthDate:  in.BirthDate,
			Gender:     in.Gender,
			Phone:      in.Phone,
			CreatedBy:  &actor.UserID,
		})
		if err != nil {
			return err
		}
		person.Email = in.Email
		person.Address = in.Address

		if err := s.deps.Students.Create(ctx, person); err != nil {
			return err
		}

		// The first identity version is written alongside the record, so a
		// document issued today already references a specific, frozen identity
		// rather than whatever the row happens to say when it is reprinted.
		version, err := student.NewIdentityVersion(person, 1, "initial registration", shared.DateFromTime(nowOr(s.deps.Clock)))
		if err != nil {
			return err
		}
		version.RecordedBy = &actor.UserID
		if err := s.deps.Students.AppendIdentityVersion(ctx, version); err != nil {
			return err
		}

		if err := s.record(ctx, port.AuditEntry{
			EntityType: "student",
			EntityID:   &person.ID,
			Action:     "student.registered",
			Actor:      actor,
			After:      snapshotOf(person),
			StudentID:  &person.ID,
			Metadata: map[string]any{
				"student_no":              person.StudentNo,
				"duplicates_acknowledged": in.AcknowledgeDuplicates,
				"duplicate_count":         len(duplicates),
			},
		}); err != nil {
			return err
		}

		result = &RegisterStudentResult{Student: person, PossibleDuplicates: duplicates}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateContactInput changes how a student is reached.
type UpdateContactInput struct {
	StudentID     shared.ID
	Phone         *string
	PhoneAlt      *string
	Email         *string
	Address       *string
	GuardianName  *string
	GuardianPhone *string
}

// UpdateContactDetails overwrites contact information in place.
//
// These are the only student fields that change without a version record.
// Nobody needs to know which phone number a student had in 2024, only how to
// reach them now — but the change is still audited, because a contact number
// swapped just before a refund is exactly the kind of thing an investigation
// wants to see.
func (s *StudentService) UpdateContactDetails(ctx context.Context, actor shared.Actor, in UpdateContactInput) (*student.Student, error) {
	if err := actor.RequireAnyRole("UpdateContactDetails",
		shared.RoleRegistrar, shared.RoleAcademicOfficer, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var person *student.Student
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		person, err = s.deps.Students.GetByID(ctx, in.StudentID)
		if err != nil {
			return err
		}

		before := snapshotOf(person)
		if in.Phone != nil {
			person.Phone = in.Phone
		}
		if in.PhoneAlt != nil {
			person.PhoneAlt = in.PhoneAlt
		}
		if in.Email != nil {
			person.Email = in.Email
		}
		if in.Address != nil {
			person.Address = in.Address
		}
		if in.GuardianName != nil {
			person.GuardianName = in.GuardianName
		}
		if in.GuardianPhone != nil {
			person.GuardianPhone = in.GuardianPhone
		}

		if err := s.deps.Students.Update(ctx, person); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "student",
			EntityID:   &person.ID,
			Action:     "student.contact_updated",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(person),
			StudentID:  &person.ID,
		})
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}

// RecordIdentityChangeInput documents a legal identity change.
type RecordIdentityChangeInput struct {
	StudentID         shared.ID
	FullName          *string
	MotherName        *string
	NationalID        *string
	BirthDate         *shared.Date
	CourtDecisionNo   string
	CourtDecisionDate shared.Date
	EffectiveFrom     shared.Date
	DocumentRef       *string
	Reason            string
}

// RecordIdentityChange registers a court-ordered change to a person's legal
// identity.
//
// The previous identity is not overwritten — it becomes a numbered version.
// Iraqi courts change names and civil-registry details, and a graduation
// certificate issued last year must keep naming the person as they were named
// when it was printed. A system that simply updated the row would make every
// historical document unreproducible.
func (s *StudentService) RecordIdentityChange(ctx context.Context, actor shared.Actor, in RecordIdentityChangeInput) (*student.Student, error) {
	if err := actor.RequireAnyRole("RecordIdentityChange", shared.RoleRegistrar, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if in.CourtDecisionNo == "" {
		return nil, shared.Validation("student.court_decision_required",
			"a legal identity change requires the court decision reference")
	}
	if in.Reason == "" {
		return nil, shared.Validation("student.identity_reason_required",
			"an identity change requires a documented reason")
	}

	var person *student.Student
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		person, err = s.deps.Students.GetByID(ctx, in.StudentID)
		if err != nil {
			return err
		}

		history, err := s.deps.Students.IdentityHistory(ctx, person.ID)
		if err != nil {
			return err
		}
		nextVersion := int32(len(history) + 1)

		before := snapshotOf(person)
		if in.FullName != nil {
			person.FullName = *in.FullName
		}
		if in.MotherName != nil {
			person.MotherName = *in.MotherName
		}
		if in.NationalID != nil {
			person.NationalID = in.NationalID
		}
		if in.BirthDate != nil {
			person.BirthDate = in.BirthDate
		}

		version, err := student.NewIdentityVersion(person, nextVersion, in.Reason, in.EffectiveFrom)
		if err != nil {
			return err
		}
		version.CourtDecisionNo = &in.CourtDecisionNo
		version.CourtDecisionDate = &in.CourtDecisionDate
		version.DocumentRef = in.DocumentRef
		version.RecordedBy = &actor.UserID

		if err := s.deps.Students.AppendIdentityVersion(ctx, version); err != nil {
			return err
		}
		if err := s.deps.Students.Update(ctx, person); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "student",
			EntityID:   &person.ID,
			Action:     "student.identity_changed",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(person),
			StudentID:  &person.ID,
			Reason:     &in.Reason,
			Metadata: map[string]any{
				"version_no":          nextVersion,
				"court_decision_no":   in.CourtDecisionNo,
				"court_decision_date": in.CourtDecisionDate.String(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}
