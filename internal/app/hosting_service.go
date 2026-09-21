package app

import (
	"context"
	"strings"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// RegisterHostingInput attaches a hosting agreement to an enrollment.
type RegisterHostingInput struct {
	EnrollmentID shared.ID
	Direction    academic.HostingDirection

	HomeUniversity  *string
	HomeCollege     *string
	HomeDepartment  *string
	HomeStudyTypeID *shared.ID

	HostUniversity  *string
	HostCollege     *string
	HostDepartment  *string
	HostStudyTypeID *shared.ID

	// FeeCollector says which institution collects tuition under this
	// agreement. The design deliberately leaves it unresolved as a policy —
	// Iraqi practice varies by agreement — so it is configured per record here
	// rather than assumed anywhere in code.
	FeeCollector academic.FeeCollector
	PeriodFrom   *shared.Date
	PeriodTo     *shared.Date
	AgreementRef *string
	Notes        *string
}

// RegisterHosting records a hosting agreement over an existing enrollment.
//
// Hosting (الاستضافة) is one of the two corrections the design document makes
// to the original model, and until now it was reachable only from the demo
// builder calling the repository directly. That meant a real incoming hosted
// student could not be registered at all — and account generation refuses them
// by design when the home institution collects, so the gap surfaced as a
// pricing error nobody could explain.
//
// The overlay never changes the enrollment. An incoming student's study type is
// the one they attend under here, because that is what feeds local pricing; the
// home institution's context lives on this record and nowhere else.
func (s *EnrollmentService) RegisterHosting(ctx context.Context, actor shared.Actor, in RegisterHostingInput) (*academic.HostingRecord, error) {
	var record *academic.HostingRecord
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		enrollment, err := s.deps.Enrollments.GetByID(ctx, in.EnrollmentID)
		if err != nil {
			return err
		}
		if err := actor.RequireScope("RegisterHosting", &enrollment.CollegeID, &enrollment.DepartmentID); err != nil {
			return err
		}

		year, err := s.deps.Years.GetByID(ctx, enrollment.AcademicYearID)
		if err != nil {
			return err
		}
		if err := year.RequireAcademicRecording("recording a hosting agreement"); err != nil {
			return err
		}

		// One overlay per enrollment: the schema enforces it, and a second one
		// would leave two answers to "who collects this student's fees".
		if existing, err := s.deps.Enrollments.GetHostingRecord(ctx, enrollment.ID); err == nil && existing != nil {
			return shared.Conflict("hosting.already_recorded",
				"this enrollment already carries a hosting agreement").
				WithDetail("hosting_record_id", existing.ID.String()).
				WithDetail("remedy", "update the existing agreement rather than adding a second")
		} else if err != nil && shared.KindOf(err) != shared.KindNotFound {
			return err
		}

		record, err = academic.NewHostingRecord(enrollment.ID, in.Direction, in.FeeCollector)
		if err != nil {
			return err
		}
		record.HomeUniversity = trimmedOrNil(in.HomeUniversity)
		record.HomeCollege = trimmedOrNil(in.HomeCollege)
		record.HomeDepartment = trimmedOrNil(in.HomeDepartment)
		record.HomeStudyTypeID = in.HomeStudyTypeID
		record.HostUniversity = trimmedOrNil(in.HostUniversity)
		record.HostCollege = trimmedOrNil(in.HostCollege)
		record.HostDepartment = trimmedOrNil(in.HostDepartment)
		record.HostStudyTypeID = in.HostStudyTypeID
		record.PeriodFrom = in.PeriodFrom
		record.PeriodTo = in.PeriodTo
		record.AgreementRef = in.AgreementRef
		record.Notes = in.Notes
		record.CreatedBy = &actor.UserID

		// An incoming student's enrollment kind says how they got here. Left as
		// an ordinary registration, every count of "our students" would include
		// somebody another university is teaching under agreement.
		if in.Direction == academic.HostingIncoming && enrollment.Kind != academic.KindHostedIn {
			enrollment.Kind = academic.KindHostedIn
			if err := s.deps.Enrollments.Update(ctx, enrollment); err != nil {
				return err
			}
		}

		if err := s.deps.Enrollments.CreateHostingRecord(ctx, record); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "hosting_record",
			EntityID:       &record.ID,
			Action:         "hosting.recorded",
			Actor:          actor,
			After:          snapshotOf(record),
			AcademicYearID: &enrollment.AcademicYearID,
			StudentID:      &enrollment.StudentID,
			Metadata: map[string]any{
				"direction":     string(in.Direction),
				"fee_collector": string(record.FeeCollector),
				"enrollment_id": enrollment.ID.String(),
				// Recorded explicitly: whether an account will be generated
				// here follows from this flag, and an agreement renegotiated
				// later is the reason a student's fees appear or disappear.
				"generates_local_account": record.GeneratesLocalAccount(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// UpdateHostingInput amends an existing agreement.
type UpdateHostingInput struct {
	EnrollmentID shared.ID
	FeeCollector *academic.FeeCollector
	PeriodTo     *shared.Date
	AgreementRef *string
	Notes        *string
	Reason       string
}

// UpdateHosting amends a hosting agreement.
//
// The field that matters is the fee collector: agreements are renegotiated
// mid-year, and it decides whether this university prices the student at all.
// Changing it does not reprice an account that already exists — a frozen
// snapshot is frozen — so the correction to an account generated under the old
// terms is an adjustment, as every other correction is.
func (s *EnrollmentService) UpdateHosting(ctx context.Context, actor shared.Actor, in UpdateHostingInput) (*academic.HostingRecord, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, shared.Validation("hosting.reason_required",
			"amending a hosting agreement requires a reason; it decides who collects the fees")
	}

	var record *academic.HostingRecord
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		enrollment, err := s.deps.Enrollments.GetByID(ctx, in.EnrollmentID)
		if err != nil {
			return err
		}
		if err := actor.RequireScope("UpdateHosting", &enrollment.CollegeID, &enrollment.DepartmentID); err != nil {
			return err
		}

		record, err = s.deps.Enrollments.GetHostingRecord(ctx, in.EnrollmentID)
		if err != nil {
			return err
		}
		before := snapshotOf(record)

		if in.FeeCollector != nil {
			if !in.FeeCollector.Valid() {
				return shared.Validation("hosting.unknown_collector",
					"%q is not a fee collector this system recognises", *in.FeeCollector)
			}
			record.FeeCollector = *in.FeeCollector
		}
		if in.PeriodTo != nil {
			record.PeriodTo = in.PeriodTo
		}
		if in.AgreementRef != nil {
			record.AgreementRef = in.AgreementRef
		}
		if in.Notes != nil {
			record.Notes = in.Notes
		}

		if err := s.deps.Enrollments.UpdateHostingRecord(ctx, record); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "hosting_record",
			EntityID:       &record.ID,
			Action:         "hosting.amended",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(record),
			AcademicYearID: &enrollment.AcademicYearID,
			StudentID:      &enrollment.StudentID,
			Reason:         &in.Reason,
			Metadata: map[string]any{
				"fee_collector":           string(record.FeeCollector),
				"generates_local_account": record.GeneratesLocalAccount(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// ListHosting returns hosting agreements, optionally for one year and
// direction.
//
// Incoming and outgoing are reported separately because they mean opposite
// things financially: an incoming student's fees may not be ours to collect,
// while an outgoing one still owes us and is being taught elsewhere.
func (s *EnrollmentService) ListHosting(
	ctx context.Context, actor shared.Actor, yearID *shared.ID, direction *academic.HostingDirection,
) ([]*academic.HostingRecord, error) {
	return s.deps.Enrollments.ListHostingRecords(ctx, yearID, direction)
}

func trimmedOrNil(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
