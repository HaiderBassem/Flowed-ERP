package app

import (
	"context"
	"time"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// YearService handles the academic year lifecycle.
type YearService struct {
	deps Deps
	auditor
}

// NewYearService wires the year commands.
func NewYearService(d Deps) *YearService {
	return &YearService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// CreateYearInput defines a new academic year.
type CreateYearInput struct {
	Code            string
	StartDate       shared.Date
	EndDate         shared.Date
	DebtBlockPolicy academic.DebtBlockPolicy
}

// CreateAcademicYear defines a year in draft.
func (s *YearService) CreateAcademicYear(ctx context.Context, actor shared.Actor, in CreateYearInput) (*academic.Year, error) {
	if err := actor.RequireAnyRole("CreateAcademicYear", shared.RoleAdmin); err != nil {
		return nil, err
	}

	var year *academic.Year
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		year, err = academic.NewYear(in.Code, in.StartDate, in.EndDate)
		if err != nil {
			return err
		}
		if in.DebtBlockPolicy != "" {
			year.DebtBlockPolicy = in.DebtBlockPolicy
		}
		if err := s.deps.Years.Create(ctx, year); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &year.ID,
			Action:         "year.created",
			Actor:          actor,
			After:          snapshotOf(year),
			AcademicYearID: &year.ID,
		})
	})
	if err != nil {
		return nil, err
	}
	return year, nil
}

// OpenAcademicYear puts a draft year into service.
//
// Nothing here forbids a second year being open at the same time, and that is
// deliberate. Every Iraqi autumn has an overlap: second-round results are
// still being recorded against the outgoing year while registration and
// collection have already started for the incoming one.
func (s *YearService) OpenAcademicYear(ctx context.Context, actor shared.Actor, yearID shared.ID) (*academic.Year, error) {
	if err := actor.RequireAnyRole("OpenAcademicYear", shared.RoleAdmin); err != nil {
		return nil, err
	}

	var year *academic.Year
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		year, err = s.deps.Years.GetForUpdate(ctx, yearID)
		if err != nil {
			return err
		}
		before := snapshotOf(year)
		if err := year.Open(); err != nil {
			return err
		}
		if err := s.deps.Years.Update(ctx, year); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &year.ID,
			Action:         "year.opened",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(year),
			AcademicYearID: &year.ID,
		})
	})
	if err != nil {
		return nil, err
	}
	return year, nil
}

// CloseYearFinanciallyResult reports what the close found.
type CloseYearFinanciallyResult struct {
	Year *academic.Year
	// ReconciliationChecked is how many accounts were compared before the
	// books were shut.
	DriftingAccounts int
}

// CloseYearFinancially freezes money for a year while leaving academic records
// writable.
//
// The two-phase close exists because a single one cannot fit the Iraqi
// calendar. The treasury wants its books shut soon after the academic year
// ends; second-round results arrive weeks later. Freezing both at once forces
// somebody to either falsify a date or leave results unrecorded, and both
// happen in systems that offer only one "closed" flag.
//
// Two preconditions gate it, and both are refusals rather than warnings. No
// payment may still be in draft, because a draft that posts after the close
// lands in a year that no longer accepts money. And no account's cached totals
// may disagree with its transaction rows, because closing over a discrepancy
// freezes the discrepancy permanently.
func (s *YearService) CloseYearFinancially(ctx context.Context, actor shared.Actor, yearID shared.ID) (*CloseYearFinanciallyResult, error) {
	if err := actor.RequireAnyRole("CloseYearFinancially",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var result *CloseYearFinanciallyResult
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		year, err := s.deps.Years.GetForUpdate(ctx, yearID)
		if err != nil {
			return err
		}
		before := snapshotOf(year)

		drafts, err := s.deps.Accounts.DraftPaymentCount(ctx, year.ID)
		if err != nil {
			return err
		}
		if drafts > 0 {
			return shared.PreconditionFailed("year.draft_payments_exist",
				"%d payment(s) in %s are still in draft; post or discard them before closing the books",
				drafts, year.Code).
				WithDetail("draft_payments", drafts)
		}

		drift, err := s.deps.Accounts.ReconciliationDrift(ctx, 50)
		if err != nil {
			return err
		}
		if len(drift) > 0 {
			accounts := make([]string, 0, len(drift))
			for _, d := range drift {
				accounts = append(accounts, d.AccountID.String())
			}
			// Drift is a defect, and it is logged as one by the reconciliation
			// job. But the person hitting it here is a finance manager trying
			// to close the books, and telling them "internal error" leaves
			// them with nothing to do. This is the actionable half: which
			// accounts, and where to look.
			s.deps.Log.ErrorContext(ctx, "year close blocked by cached-total drift",
				"academic_year", year.Code,
				"drifting_accounts", accounts,
				"invariant_violation", true)

			return shared.PreconditionFailed("year.reconciliation_drift",
				"%d account(s) have cached totals that disagree with their transaction rows; "+
					"closing the year would freeze the discrepancy permanently",
				len(drift)).
				WithDetail("drifting_accounts", accounts).
				WithDetail("remedy", "inspect them at /api/v1/oversight/reconciliation before closing")
		}

		if err := year.CloseFinancially(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Years.Update(ctx, year); err != nil {
			return err
		}

		if err := s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &year.ID,
			Action:         "year.financially_closed",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(year),
			AcademicYearID: &year.ID,
			Metadata: map[string]any{
				"draft_payments":    drafts,
				"drifting_accounts": len(drift),
			},
		}); err != nil {
			return err
		}

		result = &CloseYearFinanciallyResult{Year: year, DriftingAccounts: len(drift)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CloseAcademicYear freezes a year completely.
//
// Gated on every enrollment carrying a recorded outcome. A year closed with
// results outstanding leaves students whose standing the system cannot state,
// and no later command can fix it without reopening.
func (s *YearService) CloseAcademicYear(ctx context.Context, actor shared.Actor, yearID shared.ID) (*academic.Year, error) {
	if err := actor.RequireAnyRole("CloseAcademicYear", shared.RoleAdmin); err != nil {
		return nil, err
	}

	var year *academic.Year
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		year, err = s.deps.Years.GetForUpdate(ctx, yearID)
		if err != nil {
			return err
		}
		before := snapshotOf(year)

		pending, err := s.deps.Enrollments.PendingResults(ctx, year.ID)
		if err != nil {
			return err
		}
		if pending > 0 {
			return shared.PreconditionFailed("year.results_outstanding",
				"%d enrollment(s) in %s have no recorded result; record them, or mark them explicitly as having none",
				pending, year.Code).
				WithDetail("pending_results", pending)
		}

		if err := year.Close(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Years.Update(ctx, year); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &year.ID,
			Action:         "year.closed",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(year),
			AcademicYearID: &year.ID,
			Metadata:       map[string]any{"pending_results": pending},
		})
	})
	if err != nil {
		return nil, err
	}
	return year, nil
}

// ReopenForAdjustmentInput reopens a closed year for a bounded correction.
type ReopenForAdjustmentInput struct {
	YearID shared.ID
	Reason string
	// Window bounds the reopening. A year left open "temporarily" with no
	// deadline is a year that is simply open again.
	Window time.Duration
}

// ReopenYearForAdjustment allows a documented correction to closed books.
//
// This is the only route back into a closed year, and it is deliberately
// uncomfortable: a written reason, an administrator, a bounded window, and an
// audit entry an auditor will see. Corrections made this way still post as
// adjustments rather than edits, so the original figures stay exactly as they
// were and the change is visible beside them.
func (s *YearService) ReopenYearForAdjustment(ctx context.Context, actor shared.Actor, in ReopenForAdjustmentInput) (*academic.Year, error) {
	if err := actor.RequireAnyRole("ReopenYearForAdjustment", shared.RoleAdmin); err != nil {
		return nil, err
	}
	if in.Reason == "" {
		return nil, shared.Validation("year.reopen_reason_required",
			"reopening closed books requires a written reason")
	}
	window := in.Window
	if window <= 0 {
		window = 24 * time.Hour
	}

	var year *academic.Year
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		year, err = s.deps.Years.GetForUpdate(ctx, in.YearID)
		if err != nil {
			return err
		}
		before := snapshotOf(year)

		if err := year.OpenForAdjustment(in.Reason, now.Add(window)); err != nil {
			return err
		}
		if err := s.deps.Years.Update(ctx, year); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "academic_year",
			EntityID:       &year.ID,
			Action:         "year.reopened_for_adjustment",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(year),
			AcademicYearID: &year.ID,
			Reason:         &in.Reason,
			Metadata: map[string]any{
				"window_hours": window.Hours(),
				"window_ends":  now.Add(window),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return year, nil
}

// CloseExpiredAdjustmentWindows returns reopened years to closed once their
// window has run out.
//
// Run by the scheduler. Without it, a year reopened for an afternoon's
// correction stays writable until somebody remembers, which is exactly how a
// controlled exception becomes a standing hole.
func (s *YearService) CloseExpiredAdjustmentWindows(ctx context.Context) (int, error) {
	actor := shared.SystemActor()
	closed := 0

	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		years, err := s.deps.Years.List(ctx)
		if err != nil {
			return err
		}
		for _, y := range years {
			if !y.AdjustmentWindowExpired(now) {
				continue
			}
			locked, err := s.deps.Years.GetForUpdate(ctx, y.ID)
			if err != nil {
				return err
			}
			if !locked.AdjustmentWindowExpired(now) {
				continue
			}
			before := snapshotOf(locked)
			if err := locked.CloseAdjustmentWindow(); err != nil {
				return err
			}
			if err := s.deps.Years.Update(ctx, locked); err != nil {
				return err
			}
			if err := s.record(ctx, port.AuditEntry{
				EntityType:     "academic_year",
				EntityID:       &locked.ID,
				Action:         "year.adjustment_window_expired",
				Actor:          actor,
				Before:         before,
				After:          snapshotOf(locked),
				AcademicYearID: &locked.ID,
			}); err != nil {
				return err
			}
			closed++
		}
		return nil
	})
	return closed, err
}
