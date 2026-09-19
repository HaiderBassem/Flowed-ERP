package app

import (
	"context"
	"strings"
	"time"

	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// ReconciliationService runs the invariant checks and keeps what they find.
//
// The checks themselves are older than this service and were already correct.
// What they lacked was anywhere to put an answer: drift went to the log, so
// nobody could say whether it had been looked at, the same account was
// rediscovered every night with no sign it was the same one, and a finding that
// stopped appearing was indistinguishable from one somebody had fixed.
//
// Three properties follow from that, and they are what this adds:
//
//   - Every run is recorded, including the clean ones. A check that silently
//     stopped running looks exactly like a system with nothing wrong, and the
//     absence of runs is the more likely of the two.
//   - A finding is one row for as long as it survives, with a count of how many
//     passes have seen it. Drift nobody has explained after several nights is
//     not a transient, and it escalates.
//   - Closing one requires saying what was done. The remedy for a cache that
//     disagrees with its transactions is never to edit the cache; it is to find
//     the command that failed to maintain it, and that finding is worth
//     recording.
type ReconciliationService struct {
	deps  Deps
	store port.ReconciliationRepository
	cfg   ReconciliationConfig
}

// ReconciliationConfig tunes the checks.
type ReconciliationConfig struct {
	// Limit bounds how many violations one pass records. A thousand and ten
	// thousand are the same operational fact.
	Limit int
	// EscalateAfter is how many consecutive passes a finding survives before
	// it is critical rather than a warning.
	EscalateAfter int
}

func (c ReconciliationConfig) withDefaults() ReconciliationConfig {
	if c.Limit <= 0 {
		c.Limit = 200
	}
	if c.EscalateAfter <= 0 {
		// Three nights: enough that a finding raised during an incident which
		// is fixed the same evening never escalates, short enough that one
		// nobody has touched by the end of the week is unmissable.
		c.EscalateAfter = 3
	}
	return c
}

// NewReconciliationService wires the checks over their store.
func NewReconciliationService(d Deps, store port.ReconciliationRepository, cfg ReconciliationConfig) *ReconciliationService {
	return &ReconciliationService{deps: d, store: store, cfg: cfg.withDefaults()}
}

// ReconciliationSummary is what one pass over every check produced.
type ReconciliationSummary struct {
	Runs        []*port.ReconciliationRun `json:"runs"`
	Findings    int                       `json:"findings"`
	NewFindings int                       `json:"new_findings"`
	Resolved    int                       `json:"auto_resolved"`
	Critical    int                       `json:"open_critical"`
	Warnings    int                       `json:"open_warnings"`
}

// Clean reports whether nothing is open. It is the question a dashboard asks.
func (s *ReconciliationSummary) Clean() bool { return s.Critical == 0 && s.Warnings == 0 }

// Run performs the named checks, or all of them.
//
// Each kind is its own run row, and a failure in one does not stop the others:
// the audit chain check reads a different part of the schema from the account
// check, and a system where one is broken still needs the other's answer.
func (s *ReconciliationService) Run(
	ctx context.Context, actor shared.Actor, kinds []port.ReconciliationKind,
) (*ReconciliationSummary, error) {
	if !actor.IsSystem() {
		if err := actor.RequireAnyRole("RunReconciliation",
			shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor); err != nil {
			return nil, err
		}
	}
	if len(kinds) == 0 {
		kinds = port.AllReconciliationKinds
	}

	summary := &ReconciliationSummary{}
	for _, kind := range kinds {
		run, err := s.runOne(ctx, actor, kind)
		if run != nil {
			summary.Runs = append(summary.Runs, run)
			summary.Findings += run.Findings
			summary.NewFindings += run.NewFindings
		}
		if err != nil {
			// Recorded on the run row as a failure and carried no further: a
			// check that could not run is not a check that found nothing.
			s.deps.Log.Error("a reconciliation check failed",
				"kind", string(kind), "error", err.Error())
		}
	}

	counts, err := s.store.OpenCounts(ctx)
	if err != nil {
		return summary, err
	}
	for _, count := range counts {
		switch count.Severity {
		case "critical":
			summary.Critical += count.Count
		default:
			summary.Warnings += count.Count
		}
		s.deps.Metrics.ReconciliationOpen(ctx, count.Severity, string(count.Kind), count.Count)
	}

	return summary, nil
}

func (s *ReconciliationService) runOne(
	ctx context.Context, actor shared.Actor, kind port.ReconciliationKind,
) (*port.ReconciliationRun, error) {
	now := nowOr(s.deps.Clock)
	run := &port.ReconciliationRun{
		ID:        shared.NewID(),
		Kind:      kind,
		Status:    "running",
		StartedAt: now,
	}
	if !actor.IsSystem() && actor.UserID != shared.NilID {
		id := actor.UserID
		run.TriggeredBy = &id
	}
	if err := s.store.StartRun(ctx, run); err != nil {
		return nil, err
	}

	observed, checked, err := s.check(ctx, kind)
	run.RowsChecked = checked
	finished := nowOr(s.deps.Clock)
	run.FinishedAt = &finished

	if err == nil {
		present := make([]shared.ID, 0, len(observed))
		for _, finding := range observed {
			isNew, recordErr := s.store.RecordFinding(ctx, run.ID, finding, s.cfg.EscalateAfter, finished)
			if recordErr != nil {
				err = recordErr
				break
			}
			present = append(present, finding.SubjectID)
			run.Findings++
			if isNew {
				run.NewFindings++
			}
		}

		// Whatever this kind no longer sees is closed. Drift that stops is
		// drift somebody corrected; leaving it open forever teaches operators
		// that the queue is noise, and a queue nobody reads is the same as no
		// queue.
		if err == nil {
			if _, resolveErr := s.store.ResolveMissing(ctx, kind, run.ID, present, finished); resolveErr != nil {
				err = resolveErr
			}
		}
	}

	// Every path finishes the run row. An early return that left it "running"
	// would be indistinguishable from a pass still in progress, and the first
	// thing an operator does with this table is look at the newest row.
	if err != nil {
		message := err.Error()
		run.Status = "failed"
		run.Error = &message
	} else if run.Findings > 0 {
		run.Status = "drift"
	} else {
		run.Status = "clean"
	}

	if finishErr := s.store.FinishRun(ctx, run); finishErr != nil {
		return run, finishErr
	}
	return run, err
}

func (s *ReconciliationService) check(
	ctx context.Context, kind port.ReconciliationKind,
) ([]port.ObservedFinding, int64, error) {
	switch kind {
	case port.ReconcileAccounts:
		return s.store.CheckAccounts(ctx, s.cfg.Limit)
	case port.ReconcileInstallments:
		return s.store.CheckInstallments(ctx, s.cfg.Limit)
	case port.ReconcileRefunds:
		return s.store.CheckRefunds(ctx, s.cfg.Limit)
	case port.ReconcileAuditChain:
		return s.store.CheckAuditChain(ctx, s.cfg.Limit)
	default:
		return nil, 0, shared.Validation("reconciliation.unknown_kind",
			"there is no reconciliation check called %q", string(kind))
	}
}

// ListRuns returns recent passes.
func (s *ReconciliationService) ListRuns(
	ctx context.Context, actor shared.Actor, kind string, limit int,
) ([]*port.ReconciliationRun, error) {
	if err := s.requireOversight(actor, "ListReconciliationRuns"); err != nil {
		return nil, err
	}
	return s.store.ListRuns(ctx, kind, limit)
}

// ListFindings returns the queue. An empty state means everything still open.
func (s *ReconciliationService) ListFindings(
	ctx context.Context, actor shared.Actor, state string, limit int,
) ([]*port.ReconciliationFinding, error) {
	if err := s.requireOversight(actor, "ListReconciliationFindings"); err != nil {
		return nil, err
	}
	return s.store.ListFindings(ctx, state, limit)
}

// AcknowledgeInput takes a finding.
type AcknowledgeInput struct {
	FindingID shared.ID
	Reason    string
}

// Acknowledge marks a finding as being worked on.
//
// It does not close it. The distinction is the point: two people investigating
// the same drift is waste, and a queue where "somebody is on it" and "it is
// fixed" look alike is a queue that closes things nobody fixed.
func (s *ReconciliationService) Acknowledge(
	ctx context.Context, actor shared.Actor, in AcknowledgeInput,
) (*port.ReconciliationFinding, error) {
	if err := actor.RequireAnyRole("AcknowledgeFinding",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, shared.Validation("reconciliation.reason_required",
			"say what is being looked into; the next person to open this needs it")
	}

	var finding *port.ReconciliationFinding
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		finding, err = s.store.GetFinding(ctx, in.FindingID)
		if err != nil {
			return err
		}
		if finding.State == "resolved" {
			return shared.PreconditionFailed("reconciliation.already_resolved",
				"this finding was closed on %s", finding.ResolvedAt.Format(time.DateOnly))
		}

		now := nowOr(s.deps.Clock)
		reason := in.Reason
		user := actor.UserID
		finding.State = "acknowledged"
		finding.AcknowledgedAt = &now
		finding.AcknowledgedBy = &user
		finding.AcknowledgedReason = &reason

		if err := s.store.UpdateFinding(ctx, finding); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "reconciliation_finding",
			EntityID:   &finding.ID,
			Action:     "reconciliation.acknowledged",
			Actor:      actor,
			Reason:     &reason,
			Metadata: map[string]any{
				"subject_type": finding.SubjectType,
				"subject_id":   finding.SubjectID.String(),
				"kind":         string(finding.Kind),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return finding, nil
}

// ResolveInput closes a finding with what was done about it.
type ResolveInput struct {
	FindingID  shared.ID
	Resolution string
}

// Resolve closes a finding.
//
// The resolution text is required and it is not ceremony: the next occurrence
// of the same drift on the same account is investigated by reading it. A
// resolution that says "corrected" teaches nothing; one that names the command
// that failed to maintain the cache is how the defect behind it gets fixed.
func (s *ReconciliationService) Resolve(
	ctx context.Context, actor shared.Actor, in ResolveInput,
) (*port.ReconciliationFinding, error) {
	if err := actor.RequireAnyRole("ResolveFinding",
		shared.RoleFinanceManager, shared.RoleAdmin); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(in.Resolution)) < 10 {
		return nil, shared.Validation("reconciliation.resolution_required",
			"say what was done and why the drift is gone; "+
				"the next person to see this account will read it").
			WithDetail("remedy",
				"name the command that failed to maintain the cache, or the correction posted")
	}

	var finding *port.ReconciliationFinding
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		finding, err = s.store.GetFinding(ctx, in.FindingID)
		if err != nil {
			return err
		}
		if finding.State == "resolved" {
			return shared.PreconditionFailed("reconciliation.already_resolved",
				"this finding is already closed")
		}

		now := nowOr(s.deps.Clock)
		resolution := in.Resolution
		user := actor.UserID
		finding.State = "resolved"
		finding.ResolvedAt = &now
		finding.ResolvedBy = &user
		finding.Resolution = &resolution

		if err := s.store.UpdateFinding(ctx, finding); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "reconciliation_finding",
			EntityID:   &finding.ID,
			Action:     "reconciliation.resolved",
			Actor:      actor,
			Reason:     &resolution,
			Metadata: map[string]any{
				"subject_type": finding.SubjectType,
				"subject_id":   finding.SubjectID.String(),
				"kind":         string(finding.Kind),
				"seen_count":   finding.SeenCount,
				"severity":     finding.Severity,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return finding, nil
}

// requireOversight: reading the queue is oversight work. A cashier has no
// business in it, and a report viewer would see account identifiers with no
// context to read them by.
func (s *ReconciliationService) requireOversight(actor shared.Actor, operation string) error {
	return actor.RequireAnyRole(operation,
		shared.RoleFinanceManager, shared.RoleAdmin, shared.RoleAuditor)
}

// record writes the audit entry for a state change.
func (s *ReconciliationService) record(ctx context.Context, entry port.AuditEntry) error {
	if s.deps.Audit == nil {
		return nil
	}
	if entry.OccurredAt.IsZero() {
		entry.OccurredAt = nowOr(s.deps.Clock)
	}
	if entry.ID == shared.NilID {
		entry.ID = shared.NewID()
	}
	return s.deps.Audit.Append(ctx, entry)
}
