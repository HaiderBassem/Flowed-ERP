package app

import (
	"context"
	"strings"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
	"github.com/swibit/flowed/internal/port"
)

// MergeStudentsInput folds one duplicate person record into another.
type MergeStudentsInput struct {
	// SourceID is the record to fold away. It survives as a tombstone.
	SourceID shared.ID
	// TargetID is the record that keeps being the person's identity.
	TargetID shared.ID
	Reason   string
	// AcknowledgeDifferentIdentity proceeds when the two records disagree on
	// name, mother's name or national identifier. Merging two different people
	// is far worse than leaving two records for one, so the mismatch has to be
	// confirmed by a human and the confirmation is recorded.
	AcknowledgeDifferentIdentity bool
}

// MergeStudentsResult reports what moved.
type MergeStudentsResult struct {
	Merge  *port.StudentMerge
	Source *student.Student
	Target *student.Student
}

// MergeStudents folds a duplicate person record into the canonical one.
//
// Duplicates are certain in a university whose intake comes off paper and
// Excel: the same person registers twice, once with a middle name and once
// without, and pays into both. The schema has carried merged_into_id since the
// first migration with no workflow behind it, so the only fix was psql.
//
// What moves and what does not is the whole design of this command:
//
//   - Enrollments move. They are the financial unit and they carry their
//     accounts with them by reference, so the target ends up owning the whole
//     history.
//   - Payments, allocations, receipts and adjustments do not move and are not
//     touched. They belong to accounts, which belong to enrollments; re-pointing
//     any of them would falsify a receipt a student is holding.
//   - The source row stays, marked merged and pointing at the target, so a
//     receipt printed under the old student number still resolves to a person.
//     Deleting it would orphan every document that names it.
//   - Discount assignments move, because they were granted to the person rather
//     than to the record. Applications do not: they are frozen against the
//     account that was priced with them.
func (s *StudentService) MergeStudents(ctx context.Context, actor shared.Actor, in MergeStudentsInput) (*MergeStudentsResult, error) {
	if err := actor.RequireAnyRole("MergeStudents", shared.RoleRegistrar, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if in.SourceID == in.TargetID {
		return nil, shared.Validation("student.merge_same_record",
			"a record cannot be merged into itself")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, shared.Validation("student.merge_reason_required",
			"a merge requires a documented reason: it is irreversible and it moves a person's history")
	}

	result := &MergeStudentsResult{}
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		source, err := s.deps.Students.GetByID(ctx, in.SourceID)
		if err != nil {
			return err
		}
		target, err := s.deps.Students.GetByID(ctx, in.TargetID)
		if err != nil {
			return err
		}
		result.Source, result.Target = source, target

		if source.Status == student.StatusMerged {
			return shared.Conflict("student.already_merged",
				"student %s has already been merged into another record", source.StudentNo).
				WithDetail("merged_into", idString(source.MergedIntoID)).
				WithDetail("remedy", "merge into the record this one already points at, if that is the intent")
		}
		if target.Status == student.StatusMerged {
			return shared.Conflict("student.target_already_merged",
				"student %s is itself a merged record; merge into the record it points at instead",
				target.StudentNo).
				WithDetail("merged_into", idString(target.MergedIntoID))
		}

		// Two records that disagree about who the person is are more likely to
		// be two people than one. Iraqi four-part names repeat, which is why
		// the mother's name is the discriminator a registrar uses.
		mismatches := identityMismatches(source, target)
		if len(mismatches) > 0 && !in.AcknowledgeDifferentIdentity {
			return shared.Conflict("student.merge_identity_mismatch",
				"these records disagree on %s; confirm they are the same person before merging",
				strings.Join(mismatches, ", ")).
				WithDetail("mismatched_fields", mismatches).
				WithDetail("source", map[string]any{
					"student_no": source.StudentNo, "full_name": source.FullName,
					"mother_name": source.MotherName,
				}).
				WithDetail("target", map[string]any{
					"student_no": target.StudentNo, "full_name": target.FullName,
					"mother_name": target.MotherName,
				}).
				WithDetail("remedy", "resubmit with acknowledge_different_identity if this is one person")
		}

		enrollments, err := s.deps.Enrollments.History(ctx, source.ID)
		if err != nil {
			return err
		}
		// One live enrollment per (student, year) is a partial unique index.
		// Moving a source enrollment into a year where the target already has
		// one would violate it — and the two rows would be two financial
		// contexts for one seat, which is the invariant the index protects.
		if err := s.refuseYearCollisions(ctx, enrollments, target.ID); err != nil {
			return err
		}

		accounts, err := s.deps.Accounts.ListForStudent(ctx, source.ID)
		if err != nil {
			return err
		}

		for _, enrollment := range enrollments {
			if err := s.deps.Enrollments.Reassign(ctx, enrollment.ID, target.ID); err != nil {
				return err
			}
		}

		discountsMoved, err := s.reassignDiscounts(ctx, source.ID, target.ID)
		if err != nil {
			return err
		}

		if err := source.MergeInto(target.ID); err != nil {
			return err
		}
		if err := s.deps.Students.Update(ctx, source); err != nil {
			return err
		}

		merge := &port.StudentMerge{
			ID:               shared.NewID(),
			SourceID:         source.ID,
			TargetID:         target.ID,
			Reason:           in.Reason,
			EnrollmentsMoved: int16(len(enrollments)),
			AccountsMoved:    int16(len(accounts)),
			DiscountsMoved:   int16(discountsMoved),
			MergedBy:         &actor.UserID,
		}
		if s.deps.Lifecycle != nil {
			if err := s.deps.Lifecycle.RecordMerge(ctx, merge); err != nil {
				return err
			}
		}
		result.Merge = merge

		// Recorded against both records. An investigation starts from whichever
		// student number the enquirer has, and that is usually the old one.
		if err := s.record(ctx, port.AuditEntry{
			EntityType: "student",
			EntityID:   &source.ID,
			Action:     "student.merged_away",
			Actor:      actor,
			After:      snapshotOf(source),
			StudentID:  &source.ID,
			Reason:     &in.Reason,
			Metadata: map[string]any{
				"merged_into":        target.ID.String(),
				"target_student_no":  target.StudentNo,
				"enrollments_moved":  len(enrollments),
				"accounts_moved":     len(accounts),
				"discounts_moved":    discountsMoved,
				"identity_mismatch":  mismatches,
				"mismatch_confirmed": in.AcknowledgeDifferentIdentity,
			},
		}); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "student",
			EntityID:   &target.ID,
			Action:     "student.merge_received",
			Actor:      actor,
			StudentID:  &target.ID,
			Reason:     &in.Reason,
			Metadata: map[string]any{
				"merged_from":       source.ID.String(),
				"source_student_no": source.StudentNo,
				"enrollments_moved": len(enrollments),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// refuseYearCollisions stops a merge that would put two live enrollments in one
// year for one student.
func (s *StudentService) refuseYearCollisions(
	ctx context.Context, sourceEnrollments []*academic.Enrollment, targetID shared.ID,
) error {
	for _, enrollment := range sourceEnrollments {
		if !enrollment.IsLive() {
			continue
		}
		existing, err := s.deps.Enrollments.GetLive(ctx, targetID, enrollment.AcademicYearID)
		if err != nil {
			if shared.KindOf(err) == shared.KindNotFound {
				continue
			}
			return err
		}
		if existing != nil {
			return shared.Conflict("student.merge_year_collision",
				"both records hold a live enrollment in the same academic year; "+
					"one of them must be resolved before the merge").
				WithDetail("academic_year_id", enrollment.AcademicYearID.String()).
				WithDetail("source_enrollment_id", enrollment.ID.String()).
				WithDetail("target_enrollment_id", existing.ID.String()).
				WithDetail("remedy", "withdraw or supersede the duplicate registration first, "+
					"choosing its financial treatment, then merge")
		}
	}
	return nil
}

// reassignDiscounts moves grants made to the person rather than to the record.
//
// Applications are deliberately left alone: each is frozen against the account
// it was computed for, and that account is moving with its enrollment.
func (s *StudentService) reassignDiscounts(ctx context.Context, sourceID, targetID shared.ID) (int, error) {
	assignments, err := s.deps.Discounts.ListAssignmentsForStudent(ctx, sourceID)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, assignment := range assignments {
		assignment.StudentID = targetID
		if err := s.deps.Discounts.UpdateAssignment(ctx, assignment); err != nil {
			return 0, err
		}
		moved++
	}
	return moved, nil
}

// identityMismatches lists the fields on which two records disagree.
//
// Compared after folding, because Arabic orthography varies between two clerks
// typing the same name — which is exactly how the duplicate arose.
func identityMismatches(a, b *student.Student) []string {
	var mismatches []string
	if !sameName(a.FullName, b.FullName) {
		mismatches = append(mismatches, "full name")
	}
	if !sameName(a.MotherName, b.MotherName) {
		mismatches = append(mismatches, "mother's name")
	}
	if a.NationalID != nil && b.NationalID != nil && *a.NationalID != *b.NationalID {
		mismatches = append(mismatches, "national identifier")
	}
	if a.BirthDate != nil && b.BirthDate != nil && *a.BirthDate != *b.BirthDate {
		mismatches = append(mismatches, "date of birth")
	}
	return mismatches
}

// sameName compares two names after the folding the search index applies, so
// أحمد and احمد are one name here as they are there.
func sameName(a, b string) bool {
	return student.FoldArabic(a) == student.FoldArabic(b)
}

func idString(id *shared.ID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
