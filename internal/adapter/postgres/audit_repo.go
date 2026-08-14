package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// AuditRepository appends to the audit trail.
//
// There is no update and no delete here, and the database refuses both. Entries
// are hash-chained on insert, so an entry removed or altered later breaks
// verification of every entry after it.
type AuditRepository struct{ db *pg.DB }

// NewAuditRepository builds the audit trail store over a connection pool.
func NewAuditRepository(db *pg.DB) *AuditRepository { return &AuditRepository{db: db} }

var _ port.AuditRepository = (*AuditRepository)(nil)

const auditColumns = `
	id, entity_type, entity_id, action,
	actor_user_id, actor_username, actor_roles, actor_ip, session_id, request_id,
	occurred_at, before_state, after_state, metadata, reason,
	academic_year_id, student_id, account_id`

// Append writes one entry.
//
// previous_hash and entry_hash are deliberately absent from the insert: a
// BEFORE INSERT trigger computes them under a transaction-scoped advisory lock.
// Chaining in the database rather than in Go means a row written by any route —
// application, migration, a psql session — is chained too, so an unchained
// insert is not possible.
func (r *AuditRepository) Append(ctx context.Context, entry port.AuditEntry) error {
	if err := r.db.RequireTx(ctx, "audit.Append"); err != nil {
		return err
	}

	before, err := jsonOrNil(entry.Before)
	if err != nil {
		return err
	}
	after, err := jsonOrNil(entry.After)
	if err != nil {
		return err
	}
	metadata, err := jsonOrNil(entry.Metadata)
	if err != nil {
		return err
	}

	const query = `
		INSERT INTO audit_log (
			id, entity_type, entity_id, action,
			actor_user_id, actor_username, actor_roles, actor_ip, session_id, request_id,
			occurred_at, before_state, after_state, metadata, reason,
			academic_year_id, student_id, account_id
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, $7, $8, $9, $10,
			COALESCE($11, now()), $12, $13, $14, $15,
			$16, $17, $18
		)`

	q := r.db.Conn(ctx)
	_, err = q.Exec(ctx, query,
		entry.ID, entry.EntityType, entry.EntityID, entry.Action,
		// The system actor holds no identifier, and the column references a real
		// user row, so it is stored as NULL with the username carrying the who.
		idOrNil(entry.Actor.UserID), entry.Actor.Username, toStrings(entry.Actor.Roles),
		nullIfEmpty(entry.Actor.IPAddress), nullIfEmpty(entry.Actor.SessionID), nullIfEmpty(entry.RequestID),
		instant(entry.OccurredAt), before, after, metadata, entry.Reason,
		entry.AcademicYearID, entry.StudentID, entry.AccountID,
	)
	return pg.WrapQuery("audit.Append", err)
}

// List returns the trail for one entity, newest first. sequence_no orders it
// rather than occurred_at, because the sequence is monotonic regardless of
// clock skew and is the chain's own spine.
func (r *AuditRepository) List(
	ctx context.Context, entityType string, entityID shared.ID, limit int,
) ([]port.AuditEntry, error) {
	const query = `
		SELECT` + auditColumns + `
		FROM audit_log
		WHERE entity_type = $1 AND entity_id = $2
		ORDER BY sequence_no DESC
		LIMIT $3`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, entityType, entityID, boundedLimit(limit, 100))
	if err != nil {
		return nil, pg.WrapQuery("audit.List", err)
	}
	entries, err := pgx.CollectRows(rows, scanAuditEntry)
	if err != nil {
		return nil, pg.WrapQuery("audit.List", err)
	}
	return entries, nil
}

// ListForStudent returns everything recorded against a student, newest first.
func (r *AuditRepository) ListForStudent(ctx context.Context, studentID shared.ID, limit int) ([]port.AuditEntry, error) {
	const query = `
		SELECT` + auditColumns + `
		FROM audit_log
		WHERE student_id = $1
		ORDER BY sequence_no DESC
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, studentID, boundedLimit(limit, 100))
	if err != nil {
		return nil, pg.WrapQuery("audit.ListForStudent", err)
	}
	entries, err := pgx.CollectRows(rows, scanAuditEntry)
	if err != nil {
		return nil, pg.WrapQuery("audit.ListForStudent", err)
	}
	return entries, nil
}

// VerifyChain recomputes the hash chain and reports every entry that was
// altered or removed after it was written. An empty result is a clean trail.
func (r *AuditRepository) VerifyChain(ctx context.Context, fromSequence int64) ([]port.ChainProblem, error) {
	const query = `SELECT sequence_no, id, occurred_at, problem FROM verify_audit_chain($1)`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, fromSequence)
	if err != nil {
		return nil, pg.WrapQuery("audit.VerifyChain", err)
	}
	problems, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (port.ChainProblem, error) {
		var p port.ChainProblem
		if err := row.Scan(&p.SequenceNo, &p.EntryID, &p.OccurredAt, &p.Problem); err != nil {
			return port.ChainProblem{}, err
		}
		return p, nil
	})
	if err != nil {
		return nil, pg.WrapQuery("audit.VerifyChain", err)
	}
	return problems, nil
}

func scanAuditEntry(row pgx.CollectableRow) (port.AuditEntry, error) {
	var (
		e         port.AuditEntry
		actorID   *shared.ID
		roles     []string
		actorIP   *string
		sessionID *string
		requestID *string
		before    []byte
		after     []byte
		metadata  []byte
	)
	if err := row.Scan(
		&e.ID, &e.EntityType, &e.EntityID, &e.Action,
		&actorID, &e.Actor.Username, &roles, &actorIP, &sessionID, &requestID,
		&e.OccurredAt, &before, &after, &metadata, &e.Reason,
		&e.AcademicYearID, &e.StudentID, &e.AccountID,
	); err != nil {
		return port.AuditEntry{}, err
	}
	if actorID != nil {
		e.Actor.UserID = *actorID
	}
	e.Actor.Roles = fromStrings[shared.Role](roles)
	if actorIP != nil {
		e.Actor.IPAddress = *actorIP
	}
	if sessionID != nil {
		e.Actor.SessionID = *sessionID
	}
	if requestID != nil {
		e.RequestID = *requestID
	}
	if err := jsonInto(before, &e.Before); err != nil {
		return port.AuditEntry{}, err
	}
	if err := jsonInto(after, &e.After); err != nil {
		return port.AuditEntry{}, err
	}
	if err := jsonInto(metadata, &e.Metadata); err != nil {
		return port.AuditEntry{}, err
	}
	return e, nil
}

// nullIfEmpty stores an unset optional text field as NULL rather than as an
// empty string, so a report can distinguish "not recorded" from "recorded as
// blank".
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
