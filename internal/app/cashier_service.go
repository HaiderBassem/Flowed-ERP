package app

import (
	"context"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// CashierService owns the shift at a physical cash window.
//
// Everything else that touches cash depends on it. RecordPayment refuses a
// cash method unless the cashier has an open session, because a cash
// collection with no shift to reconcile against is money that can vanish
// between the desk and the safe with nothing to show it ever arrived. In an
// Iraqi university cash is the dominant method, so this is not a peripheral
// control: with no way to open a drawer there is no way to take money.
type CashierService struct {
	deps Deps
	auditor
}

// NewCashierService wires the cashier session commands.
func NewCashierService(d Deps) *CashierService {
	return &CashierService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// SessionSummary is the shift sheet a cashier signs and a supervisor reads.
//
// Every figure on it either comes from the session row or is recomputed from
// the transaction rows. None of it is typed by the person being reconciled.
type SessionSummary struct {
	Session      *payment.CashierSession
	OpeningFloat money.Amount
	// ExpectedCash is what the drawer should hold. For an open shift it is
	// recomputed now; for a closed one it is the figure that was recorded at
	// close, because that is what the cashier signed against.
	ExpectedCash   money.Amount
	CountedCash    *money.Amount
	Variance       *money.Amount
	VarianceReason *string
}

// OpenSession starts a shift at the cashier's own desk.
//
// The desk is not a value the caller may choose freely: it must be the one on
// the actor's token. Receipt series run per (academic year, desk), so a cashier
// standing at D01 who opened a drawer against D02 would print receipts out of
// D02's book while the paper book at D02 stayed untouched — and the paper
// reconciliation that Iraqi financial oversight actually performs would never
// balance again.
func (s *CashierService) OpenSession(
	ctx context.Context,
	actor shared.Actor,
	deskID shared.ID,
	openingFloat money.Amount,
) (*payment.CashierSession, error) {
	if err := actor.RequireAnyRole("OpenSession", shared.RoleCashier); err != nil {
		return nil, err
	}
	if actor.CashierDeskID == nil {
		return nil, shared.PreconditionFailed("cashier_session.no_desk_on_token",
			"this login is not bound to a cashier desk; sign in naming the desk before opening a drawer").
			WithDetail("remedy", "sign in again supplying cashier_desk_id")
	}
	if *actor.CashierDeskID != deskID {
		return nil, shared.Forbidden("cashier_session.desk_mismatch",
			"this login is signed in at desk %s and cannot open a drawer at desk %s; "+
				"receipt numbers run per desk and the paper book would not match",
			actor.CashierDeskID, deskID).
			WithDetail("signed_in_desk_id", actor.CashierDeskID.String()).
			WithDetail("requested_desk_id", deskID.String())
	}

	var session *payment.CashierSession
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		// Checked here so the cashier gets a message naming the drawer they
		// already have. The partial unique index below is what actually
		// guarantees it under a race.
		existing, err := s.openSessionFor(ctx, actor.UserID)
		if err != nil {
			return err
		}
		if existing != nil {
			return alreadyOpenConflict(existing)
		}

		year, err := s.resolveSessionYear(ctx)
		if err != nil {
			return err
		}

		session, err = payment.NewCashierSession(actor.UserID, deskID, year.ID, openingFloat)
		if err != nil {
			return err
		}
		if err := s.deps.Sessions.Open(ctx, session); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "cashier_session",
			EntityID:       &session.ID,
			Action:         "cashier_session.opened",
			Actor:          actor,
			After:          snapshotOf(session),
			AcademicYearID: &year.ID,
			Metadata: map[string]any{
				"cashier_desk_id": deskID.String(),
				"opening_float":   openingFloat.Int64(),
				"academic_year":   year.Code,
			},
		})
	})
	if err != nil {
		// A second terminal opening a drawer for the same cashier at the same
		// instant loses to the partial unique index rather than to the check
		// above, and the index knows nothing about which session won. Naming it
		// has to happen out here: inside the transaction the rejected INSERT
		// has already aborted it, and a further read would come back "current
		// transaction is aborted" rather than with the answer.
		//
		// The detail is the discriminator rather than the code, because the
		// pre-check raises the same code and has already named the session.
		if domainErr, ok := shared.AsDomain(err); ok && domainErr.Code == "cashier_session.already_open" {
			if _, named := domainErr.Details["session_id"]; !named {
				return nil, s.describeOpenSession(ctx, actor.UserID, err)
			}
		}
		return nil, err
	}
	return session, nil
}

// CloseSession ends a shift, counting the drawer against what it should hold.
//
// A cashier closes their own drawer and nobody else's. Closing somebody else's
// would put one person's count and another person's signature on the same
// sheet, which is precisely the pairing the shift control exists to prevent.
func (s *CashierService) CloseSession(
	ctx context.Context,
	actor shared.Actor,
	sessionID shared.ID,
	countedCash money.Amount,
	varianceReason *string,
) (*payment.CashierSession, error) {
	if err := actor.RequireAnyRole("CloseSession", shared.RoleCashier); err != nil {
		return nil, err
	}

	var session *payment.CashierSession
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		session, err = s.deps.Sessions.GetByID(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.CashierUserID != actor.UserID {
			return shared.Forbidden("cashier_session.not_own_session",
				"session %s belongs to another cashier; a drawer is closed by the person who took the money",
				sessionID).
				WithDetail("session_id", sessionID.String())
		}
		before := snapshotOf(session)

		// Expected cash is recomputed from the payment and refund rows, never
		// supplied by the person being reconciled.
		//
		// A payment voided during this shift is counted in and then taken out
		// again, netting to zero. That is not an oversight: the schema records
		// the session that took a payment and not the session that reversed
		// it, so a void can only be attributed to the drawer that received the
		// money — which is exactly why the domain confines voids to the shift
		// that made the mistake and sends everything later down the refund
		// path, where the money leaves a drawer that is still open.
		expected, err := s.deps.Sessions.ExpectedCash(ctx, sessionID)
		if err != nil {
			return err
		}

		if err := session.Close(expected, countedCash, varianceReason, now); err != nil {
			return err
		}
		if err := s.deps.Sessions.Close(ctx, session); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "cashier_session",
			EntityID:       &session.ID,
			Action:         "cashier_session.closed",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(session),
			AcademicYearID: &session.AcademicYearID,
			Reason:         varianceReason,
			Metadata: map[string]any{
				"opening_float": session.OpeningFloat.Int64(),
				"expected_cash": expected.Int64(),
				"counted_cash":  countedCash.Int64(),
				"variance":      amountOrZero(session.Variance).Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// ApproveSession is the supervisor's signature on a closed drawer.
//
// The domain refuses a cashier signing off their own shift, so the four-eyes
// rule holds even if this method were ever called with the wrong roles.
func (s *CashierService) ApproveSession(
	ctx context.Context, actor shared.Actor, sessionID shared.ID,
) (*payment.CashierSession, error) {
	if err := actor.RequireAnyRole("ApproveSession",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}

	var session *payment.CashierSession
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		session, err = s.deps.Sessions.GetByID(ctx, sessionID)
		if err != nil {
			return err
		}
		before := snapshotOf(session)

		if err := session.Approve(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Sessions.Update(ctx, session); err != nil {
			return err
		}

		// A drawer that did not balance is exactly what an investigation reads,
		// so the variance and its explanation travel with the approval rather
		// than only with the close.
		return s.record(ctx, port.AuditEntry{
			EntityType:     "cashier_session",
			EntityID:       &session.ID,
			Action:         "cashier_session.approved",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(session),
			AcademicYearID: &session.AcademicYearID,
			Reason:         session.VarianceReason,
			Metadata: map[string]any{
				"cashier_user_id": session.CashierUserID.String(),
				"variance":        amountOrZero(session.Variance).Int64(),
				"expected_cash":   amountOrZero(session.ExpectedCash).Int64(),
				"counted_cash":    amountOrZero(session.CountedCash).Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// GetOpenSession returns the cashier's open drawer, or nil when they have none.
//
// Having no open session is a normal state on a cashier's screen at the start
// of the day, not a failure, so it comes back as an absent value rather than as
// an error the client has to special-case.
func (s *CashierService) GetOpenSession(
	ctx context.Context, actor shared.Actor,
) (*payment.CashierSession, error) {
	if err := actor.RequireAnyRole("GetOpenSession", shared.RoleCashier); err != nil {
		return nil, err
	}
	return s.openSessionFor(ctx, actor.UserID)
}

// SessionSummary reports a shift's position.
//
// A cashier may read their own shift; finance and audit may read anyone's. A
// cashier reading another's would learn what a colleague's drawer looked like
// before their own count was taken.
func (s *CashierService) SessionSummary(
	ctx context.Context, actor shared.Actor, sessionID shared.ID,
) (*SessionSummary, error) {
	if err := actor.RequireAnyRole("SessionSummary",
		shared.RoleCashier, shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor); err != nil {
		return nil, err
	}

	session, err := s.deps.Sessions.GetByID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !actor.HasAnyRole(shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor) &&
		session.CashierUserID != actor.UserID {
		return nil, shared.Forbidden("cashier_session.not_own_session",
			"session %s belongs to another cashier", sessionID).
			WithDetail("session_id", sessionID.String())
	}

	summary := &SessionSummary{
		Session:        session,
		OpeningFloat:   session.OpeningFloat,
		CountedCash:    session.CountedCash,
		Variance:       session.Variance,
		VarianceReason: session.VarianceReason,
	}

	// A closed shift reports the figure it was closed against. Recomputing it
	// would quietly restate a number somebody has already signed, and a
	// discrepancy between the two is a finding rather than something to smooth
	// over — it stays visible in the session row and the audit trail.
	if session.ExpectedCash != nil {
		summary.ExpectedCash = *session.ExpectedCash
		return summary, nil
	}

	expected, err := s.deps.Sessions.ExpectedCash(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	summary.ExpectedCash = expected
	return summary, nil
}

// openSessionFor returns the user's open drawer, translating "no row" into an
// absent value: the caller asked whether one exists, and "no" is an answer.
func (s *CashierService) openSessionFor(ctx context.Context, userID shared.ID) (*payment.CashierSession, error) {
	session, err := s.deps.Sessions.GetOpenForUser(ctx, userID)
	if err != nil {
		if shared.KindOf(err) == shared.KindNotFound {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

// describeOpenSession turns a bare uniqueness conflict into a message naming
// the drawer that is already open. It falls back to the original error when the
// session cannot be read, because losing the conflict is worse than losing the
// detail.
func (s *CashierService) describeOpenSession(ctx context.Context, userID shared.ID, cause error) error {
	existing, err := s.openSessionFor(ctx, userID)
	if err != nil || existing == nil {
		return cause
	}
	return alreadyOpenConflict(existing).WithCause(cause)
}

// alreadyOpenConflict names the open drawer so the cashier can act on it
// without going to look the identifier up.
func alreadyOpenConflict(existing *payment.CashierSession) *shared.Error {
	return shared.Conflict("cashier_session.already_open",
		"this cashier already has session %s open at desk %s since %s; close it before opening another",
		existing.ID, existing.CashierDeskID, existing.OpenedAt.Format("2006-01-02 15:04 MST")).
		WithDetail("session_id", existing.ID.String()).
		WithDetail("cashier_desk_id", existing.CashierDeskID.String()).
		WithDetail("opened_at", existing.OpenedAt).
		WithDetail("opening_float", existing.OpeningFloat.Int64()).
		WithDetail("remedy", "close the open session before opening another")
}

// resolveSessionYear picks the year a new shift belongs to.
//
// Two years are legitimately open each Iraqi autumn while one year's results
// are still being recorded and the next year's registration has started, so the
// most recently opened one wins — that is the year a cashier taking money today
// is collecting for.
func (s *CashierService) resolveSessionYear(ctx context.Context) (*academic.Year, error) {
	open, err := s.deps.Years.CurrentOpen(ctx)
	if err != nil {
		return nil, err
	}
	if len(open) == 0 {
		return nil, shared.PreconditionFailed("cashier_session.no_open_year",
			"no academic year is open, so there are no books for this shift to collect into").
			WithDetail("remedy", "open the academic year before opening a cashier session")
	}
	return open[len(open)-1], nil
}

// amountOrZero reads an optional amount for a log or audit field, where an
// absent figure and a zero one mean the same thing to a reader.
func amountOrZero(a *money.Amount) money.Amount {
	if a == nil {
		return money.Zero
	}
	return *a
}
