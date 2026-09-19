package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// ReconciliationRepository stores reconciliation runs and findings, and reads
// the invariant views the checks are built on.
//
// The checks are SQL because the invariants are SQL: v_account_reconciliation
// and its siblings are the definition of "the caches agree with the
// transactions", and re-expressing that in Go would create a second definition
// that can disagree with the first.
type ReconciliationRepository struct{ db *pg.DB }

// NewReconciliationRepository builds the store over a pool.
func NewReconciliationRepository(db *pg.DB) *ReconciliationRepository {
	return &ReconciliationRepository{db: db}
}

var _ port.ReconciliationRepository = (*ReconciliationRepository)(nil)

func (r *ReconciliationRepository) StartRun(ctx context.Context, run *port.ReconciliationRun) error {
	const query = `
		INSERT INTO reconciliation_run (id, kind, status, started_at, triggered_by)
		VALUES ($1, $2, 'running', $3, $4)
		RETURNING started_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("reconciliation.StartRun", q.QueryRow(ctx, query,
		run.ID, string(run.Kind), instant(run.StartedAt), idOrNilPtr(run.TriggeredBy),
	).Scan(&run.StartedAt))
}

func (r *ReconciliationRepository) FinishRun(ctx context.Context, run *port.ReconciliationRun) error {
	const query = `
		UPDATE reconciliation_run
		SET status = $2, finished_at = $3, rows_checked = $4,
		    findings = $5, new_findings = $6, error = $7
		WHERE id = $1
		RETURNING finished_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("reconciliation.FinishRun", q.QueryRow(ctx, query,
		run.ID, run.Status, instant(nowOrZero(run.FinishedAt)), run.RowsChecked,
		run.Findings, run.NewFindings, run.Error,
	).Scan(&run.FinishedAt))
}

// RecordFinding opens a finding or records another sighting of an open one.
//
// The upsert is on the partial unique index over live findings, so a subject
// that drifts again after being resolved opens a fresh finding rather than
// reviving the closed one — which keeps the history of "this account has done
// this before" readable, and it is exactly the history somebody investigating
// wants.
//
// Escalation is arithmetic on the sighting count: drift nobody has explained
// after several passes is not a transient, and the severity it is displayed
// under should stop saying it might be.
func (r *ReconciliationRepository) RecordFinding(
	ctx context.Context, runID shared.ID, observed port.ObservedFinding,
	escalateAfter int, at time.Time,
) (bool, error) {
	detail, err := json.Marshal(observed.Detail)
	if err != nil {
		return false, shared.Internal("reconciliation.detail_encode", err,
			"encoding a reconciliation finding")
	}

	const query = `
		INSERT INTO reconciliation_finding (
			id, first_run_id, last_run_id, kind, subject_type, subject_id, detail,
			severity, state, seen_count, first_seen_at, last_seen_at
		) VALUES ($1, $2, $2, $3, $4, $5, $6, 'warning', 'open', 1, $7, $7)
		ON CONFLICT (subject_type, subject_id, kind) WHERE state <> 'resolved'
		DO UPDATE SET
			last_run_id  = EXCLUDED.last_run_id,
			detail       = EXCLUDED.detail,
			last_seen_at = EXCLUDED.last_seen_at,
			seen_count   = reconciliation_finding.seen_count + 1,
			severity     = CASE
				WHEN reconciliation_finding.seen_count + 1 >= $8 THEN 'critical'
				ELSE reconciliation_finding.severity
			END
		RETURNING (xmax = 0) AS inserted`

	var inserted bool
	q := r.db.Conn(ctx)
	err = q.QueryRow(ctx, query,
		shared.NewID(), runID, string(observed.Kind), observed.SubjectType, observed.SubjectID,
		detail, instant(at), escalateAfter,
	).Scan(&inserted)
	if err != nil {
		return false, pg.WrapQuery("reconciliation.RecordFinding", err)
	}
	return inserted, nil
}

func (r *ReconciliationRepository) ResolveMissing(
	ctx context.Context, kind port.ReconciliationKind, runID shared.ID,
	stillPresent []shared.ID, at time.Time,
) (int, error) {
	const query = `
		UPDATE reconciliation_finding
		SET state = 'resolved',
		    resolved_at = $3,
		    resolution = 'the drift is no longer present; closed automatically by run ' || $2::uuid::text,
		    last_run_id = $2::uuid
		WHERE kind = $1
		  AND state <> 'resolved'
		  AND NOT (subject_id = ANY($4::uuid[]))`

	q := r.db.Conn(ctx)
	tag, err := q.Exec(ctx, query, string(kind), runID, instant(at), stillPresent)
	if err != nil {
		return 0, pg.WrapQuery("reconciliation.ResolveMissing", err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *ReconciliationRepository) ListRuns(ctx context.Context, kind string, limit int) ([]*port.ReconciliationRun, error) {
	if limit <= 0 {
		limit = 50
	}
	const query = `
		SELECT id, kind, status, started_at, finished_at, rows_checked,
		       findings, new_findings, error, triggered_by
		FROM reconciliation_run
		WHERE ($1 = '' OR kind = $1)
		ORDER BY started_at DESC
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, kind, limit)
	if err != nil {
		return nil, pg.WrapQuery("reconciliation.ListRuns", err)
	}
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.ReconciliationRun, error) {
		var run port.ReconciliationRun
		var kind string
		err := row.Scan(&run.ID, &kind, &run.Status, &run.StartedAt, &run.FinishedAt,
			&run.RowsChecked, &run.Findings, &run.NewFindings, &run.Error, &run.TriggeredBy)
		run.Kind = port.ReconciliationKind(kind)
		return &run, err
	})
	return runs, pg.WrapQuery("reconciliation.ListRuns", err)
}

const findingColumns = `
	id, first_run_id, last_run_id, kind, subject_type, subject_id, detail,
	severity, state, seen_count, first_seen_at, last_seen_at,
	acknowledged_at, acknowledged_by, acknowledged_reason,
	resolved_at, resolved_by, resolution`

func (r *ReconciliationRepository) ListFindings(ctx context.Context, state string, limit int) ([]*port.ReconciliationFinding, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `
		SELECT` + findingColumns + `
		FROM reconciliation_finding
		WHERE ($1 = '' AND state <> 'resolved') OR state = $1
		ORDER BY
			CASE severity WHEN 'critical' THEN 0 ELSE 1 END,
			first_seen_at
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, state, limit)
	if err != nil {
		return nil, pg.WrapQuery("reconciliation.ListFindings", err)
	}
	findings, err := pgx.CollectRows(rows, scanFinding)
	return findings, pg.WrapQuery("reconciliation.ListFindings", err)
}

func (r *ReconciliationRepository) GetFinding(ctx context.Context, id shared.ID) (*port.ReconciliationFinding, error) {
	query := `SELECT` + findingColumns + ` FROM reconciliation_finding WHERE id = $1`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, id)
	if err != nil {
		return nil, pg.WrapQuery("reconciliation.GetFinding", err)
	}
	finding, err := pgx.CollectOneRow(rows, scanFinding)
	if err != nil {
		return nil, pg.WrapQuery("reconciliation.GetFinding", err)
	}
	return finding, nil
}

func (r *ReconciliationRepository) UpdateFinding(ctx context.Context, f *port.ReconciliationFinding) error {
	const query = `
		UPDATE reconciliation_finding
		SET state = $2,
		    acknowledged_at = $3, acknowledged_by = $4, acknowledged_reason = $5,
		    resolved_at = $6, resolved_by = $7, resolution = $8
		WHERE id = $1
		RETURNING id`

	q := r.db.Conn(ctx)
	var id shared.ID
	err := q.QueryRow(ctx, query, f.ID, f.State,
		instantOrNil(f.AcknowledgedAt), idOrNilPtr(f.AcknowledgedBy), f.AcknowledgedReason,
		instantOrNil(f.ResolvedAt), idOrNilPtr(f.ResolvedBy), f.Resolution,
	).Scan(&id)
	return pg.WrapQuery("reconciliation.UpdateFinding", err)
}

func (r *ReconciliationRepository) OpenCounts(ctx context.Context) ([]port.OpenCount, error) {
	// Every combination is produced, including the zeroes. A gauge series that
	// simply stops being emitted keeps its last value on most dashboards, so
	// the moment the last finding is closed would otherwise look identical to
	// the moment reconciliation stopped running.
	const query = `
		SELECT s.severity, k.kind, count(f.id)
		FROM (VALUES ('warning'), ('critical')) AS s(severity)
		CROSS JOIN (VALUES ('accounts'), ('installments'), ('refunds'), ('audit_chain')) AS k(kind)
		LEFT JOIN reconciliation_finding f
		       ON f.severity = s.severity AND f.kind = k.kind AND f.state <> 'resolved'
		GROUP BY s.severity, k.kind`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("reconciliation.OpenCounts", err)
	}
	defer rows.Close()

	var counts []port.OpenCount
	for rows.Next() {
		var count port.OpenCount
		var kind string
		if err := rows.Scan(&count.Severity, &kind, &count.Count); err != nil {
			return nil, pg.WrapQuery("reconciliation.OpenCounts", err)
		}
		count.Kind = port.ReconciliationKind(kind)
		counts = append(counts, count)
	}
	return counts, pg.WrapQuery("reconciliation.OpenCounts", rows.Err())
}

// ---------------------------------------------------------------------------
// The checks
// ---------------------------------------------------------------------------

func (r *ReconciliationRepository) CheckAccounts(ctx context.Context, limit int) ([]port.ObservedFinding, int64, error) {
	var total int64
	q := r.db.Conn(ctx)
	if err := q.QueryRow(ctx, `SELECT count(*) FROM financial_account`).Scan(&total); err != nil {
		return nil, 0, pg.WrapQuery("reconciliation.CheckAccounts", err)
	}

	query := `
		SELECT account_id, paid_drift, refunded_drift, adjustment_drift, credit_drift,
		       cached_paid, computed_paid
		FROM v_account_reconciliation
		LIMIT $1`

	rows, err := q.Query(ctx, query, boundedLimit(limit, 1000))
	if err != nil {
		return nil, total, pg.WrapQuery("reconciliation.CheckAccounts", err)
	}
	defer rows.Close()

	var found []port.ObservedFinding
	for rows.Next() {
		var id shared.ID
		var paid, refunded, adjustment, credit, cachedPaid, computedPaid int64
		if err := rows.Scan(&id, &paid, &refunded, &adjustment, &credit,
			&cachedPaid, &computedPaid); err != nil {
			return nil, total, pg.WrapQuery("reconciliation.CheckAccounts", err)
		}
		found = append(found, port.ObservedFinding{
			Kind:        port.ReconcileAccounts,
			SubjectType: "account",
			SubjectID:   id,
			Detail: map[string]any{
				"paid_drift":       paid,
				"refunded_drift":   refunded,
				"adjustment_drift": adjustment,
				"credit_drift":     credit,
				"cached_paid":      cachedPaid,
				"computed_paid":    computedPaid,
				"remedy": "find the command that failed to maintain the cache; " +
					"never edit the cached total",
			},
		})
	}
	return found, total, pg.WrapQuery("reconciliation.CheckAccounts", rows.Err())
}

func (r *ReconciliationRepository) CheckInstallments(ctx context.Context, limit int) ([]port.ObservedFinding, int64, error) {
	var total int64
	q := r.db.Conn(ctx)
	if err := q.QueryRow(ctx, `SELECT count(*) FROM installment`).Scan(&total); err != nil {
		return nil, 0, pg.WrapQuery("reconciliation.CheckInstallments", err)
	}

	query := `
		SELECT installment_id, account_id, cached_paid, computed_paid, drift,
		       stored_status, effective_status
		FROM v_installment_reconciliation
		LIMIT $1`

	rows, err := q.Query(ctx, query, boundedLimit(limit, 1000))
	if err != nil {
		return nil, total, pg.WrapQuery("reconciliation.CheckInstallments", err)
	}
	defer rows.Close()

	var found []port.ObservedFinding
	for rows.Next() {
		var id, accountID shared.ID
		var cached, computed, drift int64
		var stored, effective string
		if err := rows.Scan(&id, &accountID, &cached, &computed, &drift,
			&stored, &effective); err != nil {
			return nil, total, pg.WrapQuery("reconciliation.CheckInstallments", err)
		}
		found = append(found, port.ObservedFinding{
			Kind:        port.ReconcileInstallments,
			SubjectType: "installment",
			SubjectID:   id,
			Detail: map[string]any{
				"account_id":       accountID.String(),
				"cached_paid":      cached,
				"computed_paid":    computed,
				"drift":            drift,
				"stored_status":    stored,
				"effective_status": effective,
			},
		})
	}
	return found, total, pg.WrapQuery("reconciliation.CheckInstallments", rows.Err())
}

func (r *ReconciliationRepository) CheckRefunds(ctx context.Context, limit int) ([]port.ObservedFinding, int64, error) {
	var total int64
	q := r.db.Conn(ctx)
	if err := q.QueryRow(ctx, `SELECT count(*) FROM payment WHERE status = 'posted'`).Scan(&total); err != nil {
		return nil, 0, pg.WrapQuery("reconciliation.CheckRefunds", err)
	}

	query := `
		SELECT payment_id, receipt_no, account_id, payment_amount, refunded_amount, excess
		FROM v_over_refunded_payments
		LIMIT $1`

	rows, err := q.Query(ctx, query, boundedLimit(limit, 1000))
	if err != nil {
		return nil, total, pg.WrapQuery("reconciliation.CheckRefunds", err)
	}
	defer rows.Close()

	var found []port.ObservedFinding
	for rows.Next() {
		var id, accountID shared.ID
		var receipt string
		var amount, refunded, excess int64
		if err := rows.Scan(&id, &receipt, &accountID, &amount, &refunded, &excess); err != nil {
			return nil, total, pg.WrapQuery("reconciliation.CheckRefunds", err)
		}
		found = append(found, port.ObservedFinding{
			Kind:        port.ReconcileRefunds,
			SubjectType: "payment",
			SubjectID:   id,
			Detail: map[string]any{
				"receipt_no":      receipt,
				"account_id":      accountID.String(),
				"payment_amount":  amount,
				"refunded_amount": refunded,
				"excess":          excess,
				"remedy":          "money left the university that never entered it; escalate",
			},
		})
	}
	return found, total, pg.WrapQuery("reconciliation.CheckRefunds", rows.Err())
}

// CheckAuditChain re-walks the hash chain from the last clean checkpoint.
//
// Walking from zero every night is work that grows forever, and the growth is
// invisible until the night it does not finish: at half a million entries a
// full pass is 662 ms, and it is linear. So a clean pass records where it got
// to and the hash it stopped at, and the next one resumes there.
//
// The prefix is not thereby trusted on faith. The checkpoint stores the hash,
// so an entry rewritten behind it breaks the join at the resume point and the
// very first row of the next pass reports it. A full pass still runs on the
// slower schedule the operations guide describes, and the off-host archive
// covers what neither can see — a deleted entry, which leaves an intact chain.
func (r *ReconciliationRepository) CheckAuditChain(ctx context.Context, limit int) ([]port.ObservedFinding, int64, error) {
	var total int64
	q := r.db.Conn(ctx)
	if err := q.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&total); err != nil {
		return nil, 0, pg.WrapQuery("reconciliation.CheckAuditChain", err)
	}

	var (
		from      int64
		fromHash  *string
		checkedTo int64
	)
	if err := q.QueryRow(ctx,
		`SELECT coalesce(sequence_no, 0), entry_hash FROM audit_verification_checkpoint()`,
	).Scan(&from, &fromHash); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, total, pg.WrapQuery("reconciliation.CheckAuditChain", err)
	}

	// The checkpoint's own entry must still hash to what was recorded. This is
	// the one row that makes resuming safe rather than merely cheap.
	var found []port.ObservedFinding
	if fromHash != nil {
		var stillThere bool
		if err := q.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM audit_log WHERE sequence_no = $1 AND entry_hash = $2)`,
			from, *fromHash).Scan(&stillThere); err != nil {
			return nil, total, pg.WrapQuery("reconciliation.CheckAuditChain", err)
		}
		if !stillThere {
			found = append(found, port.ObservedFinding{
				Kind:        port.ReconcileAuditChain,
				SubjectType: "audit_entry",
				SubjectID:   shared.NewID(),
				Detail: map[string]any{
					"sequence_no": from,
					"problem": "the entry the last verification stopped at is gone or altered; " +
						"the trail was changed behind the checkpoint",
					"remedy": "compare against the off-host archive: api audit-ship verify",
				},
			})
			// Start again from the beginning: something moved behind us, and
			// the resume point cannot be trusted to bound the damage.
			from = 0
		}
	}

	if err := q.QueryRow(ctx, `SELECT coalesce(max(sequence_no), 0) FROM audit_log`).
		Scan(&checkedTo); err != nil {
		return nil, total, pg.WrapQuery("reconciliation.CheckAuditChain", err)
	}

	started := time.Now()
	rows, err := q.Query(ctx,
		`SELECT sequence_no, id, occurred_at, problem FROM verify_audit_chain($2) LIMIT $1`,
		boundedLimit(limit, 1000), from)
	if err != nil {
		return nil, total, pg.WrapQuery("reconciliation.CheckAuditChain", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sequence int64
		var id shared.ID
		var occurred time.Time
		var problem string
		if err := rows.Scan(&sequence, &id, &occurred, &problem); err != nil {
			rows.Close()
			return nil, total, pg.WrapQuery("reconciliation.CheckAuditChain", err)
		}
		found = append(found, port.ObservedFinding{
			Kind:        port.ReconcileAuditChain,
			SubjectType: "audit_entry",
			SubjectID:   id,
			Detail: map[string]any{
				"sequence_no": sequence,
				"occurred_at": occurred.UTC().Format(time.RFC3339),
				"problem":     problem,
				"remedy":      "compare against the off-host archive: api audit-ship verify",
			},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, total, pg.WrapQuery("reconciliation.CheckAuditChain", err)
	}

	// The pass is recorded whether or not it found anything — including the
	// passes that found something, so a later reader can see when the trail
	// stopped being clean rather than only that it is not clean now.
	if err := r.recordVerification(ctx, from, checkedTo, len(found), time.Since(started)); err != nil {
		return found, total, err
	}
	return found, total, nil
}

// recordVerification writes the checkpoint. A pass that found problems is
// recorded too, and its row is excluded from the resume point by the partial
// index, so the next pass starts from the last position that was actually
// clean rather than from the last position that was merely reached.
func (r *ReconciliationRepository) recordVerification(
	ctx context.Context, from, to int64, problems int, took time.Duration,
) error {
	kind := "incremental"
	if from == 0 {
		kind = "full"
	}

	const query = `
		INSERT INTO audit_verification (
			id, sequence_no, entry_hash, entries, kind, problems, took_ms)
		SELECT $1, $2, coalesce(
			(SELECT entry_hash FROM audit_log WHERE sequence_no = $2),
			'empty'), $3, $4, $5, $6
		WHERE EXISTS (SELECT 1 FROM audit_log)`

	q := r.db.Conn(ctx)
	_, err := q.Exec(ctx, query,
		shared.NewID(), to, max(to-from, 0), kind, problems, took.Milliseconds())
	return pg.WrapQuery("reconciliation.recordVerification", err)
}

func scanFinding(row pgx.CollectableRow) (*port.ReconciliationFinding, error) {
	var f port.ReconciliationFinding
	var kind string
	var detail []byte
	err := row.Scan(&f.ID, &f.FirstRunID, &f.LastRunID, &kind, &f.SubjectType, &f.SubjectID,
		&detail, &f.Severity, &f.State, &f.SeenCount, &f.FirstSeenAt, &f.LastSeenAt,
		&f.AcknowledgedAt, &f.AcknowledgedBy, &f.AcknowledgedReason,
		&f.ResolvedAt, &f.ResolvedBy, &f.Resolution)
	if err != nil {
		return nil, err
	}
	f.Kind = port.ReconciliationKind(kind)
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &f.Detail)
	}
	return &f, nil
}

func nowOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Now().UTC()
	}
	return *t
}

func instantOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
