package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// AuditShipmentRepository records which blocks of the audit trail have left the
// host, and reads back the entries a shipment covers.
type AuditShipmentRepository struct{ db *pg.DB }

// NewAuditShipmentRepository builds the shipment store over a pool.
func NewAuditShipmentRepository(db *pg.DB) *AuditShipmentRepository {
	return &AuditShipmentRepository{db: db}
}

var _ port.AuditShipmentRepository = (*AuditShipmentRepository)(nil)

// shippedColumns is the whole row, hashes included. The payload columns are
// cast to text rather than decoded: the archive has to carry the exact bytes
// PostgreSQL hashed, and re-encoding a decoded map would reorder its keys.
const shippedColumns = `
	sequence_no, id, entity_type, entity_id, action,
	actor_user_id, actor_username, actor_roles, actor_ip, request_id,
	occurred_at, before_state::text, after_state::text, metadata::text, reason,
	academic_year_id, student_id, account_id, previous_hash, entry_hash`

func (r *AuditShipmentRepository) Record(ctx context.Context, s *port.AuditShipment) error {
	const query = `
		INSERT INTO audit_shipment (
			id, destination, artifact_ref, from_sequence, to_sequence, entry_count,
			first_entry_hash, last_entry_hash, content_sha256, content_bytes,
			shipped_at, shipped_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING shipped_at`

	q := r.db.Conn(ctx)
	return pg.WrapQuery("audit_shipment.Record", q.QueryRow(ctx, query,
		s.ID, s.Destination, s.ArtifactRef, s.FromSequence, s.ToSequence, s.EntryCount,
		s.FirstEntryHash, s.LastEntryHash, s.ContentSHA256, s.ContentBytes,
		instant(s.ShippedAt), idOrNilPtr(s.ShippedBy),
	).Scan(&s.ShippedAt))
}

func (r *AuditShipmentRepository) LastShipped(ctx context.Context, destination string) (int64, error) {
	const query = `
		SELECT coalesce(max(to_sequence), 0) FROM audit_shipment WHERE destination = $1`

	var last int64
	q := r.db.Conn(ctx)
	if err := q.QueryRow(ctx, query, destination).Scan(&last); err != nil {
		return 0, pg.WrapQuery("audit_shipment.LastShipped", err)
	}
	return last, nil
}

func (r *AuditShipmentRepository) List(ctx context.Context, destination string, limit int) ([]*port.AuditShipment, error) {
	if limit <= 0 {
		limit = 50
	}
	const query = `
		SELECT id, destination, artifact_ref, from_sequence, to_sequence, entry_count,
		       first_entry_hash, last_entry_hash, content_sha256, content_bytes,
		       shipped_at, shipped_by
		FROM audit_shipment
		WHERE ($1 = '' OR destination = $1)
		ORDER BY from_sequence DESC
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, destination, limit)
	if err != nil {
		return nil, pg.WrapQuery("audit_shipment.List", err)
	}
	shipments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*port.AuditShipment, error) {
		var s port.AuditShipment
		err := row.Scan(&s.ID, &s.Destination, &s.ArtifactRef, &s.FromSequence, &s.ToSequence,
			&s.EntryCount, &s.FirstEntryHash, &s.LastEntryHash, &s.ContentSHA256, &s.ContentBytes,
			&s.ShippedAt, &s.ShippedBy)
		return &s, err
	})
	return shipments, pg.WrapQuery("audit_shipment.List", err)
}

func (r *AuditShipmentRepository) Gaps(ctx context.Context, destination string) ([]port.ShipmentGap, error) {
	// The first block is expected to start at 1: an installation that begins
	// shipping after a year of operation has a gap covering that year, and
	// pretending otherwise would hide exactly what this reports.
	const query = `
		SELECT from_sequence, to_sequence, gap_before
		FROM v_audit_shipment_coverage
		WHERE destination = $1
		ORDER BY from_sequence`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, destination)
	if err != nil {
		return nil, pg.WrapQuery("audit_shipment.Gaps", err)
	}
	defer rows.Close()

	var gaps []port.ShipmentGap
	first := true
	for rows.Next() {
		var from, to int64
		var gapBefore *int64
		if err := rows.Scan(&from, &to, &gapBefore); err != nil {
			return nil, pg.WrapQuery("audit_shipment.Gaps", err)
		}
		if first {
			first = false
			if from > 1 {
				gaps = append(gaps, port.ShipmentGap{
					AfterSequence: 0, BeforeSequence: from, Missing: from - 1,
				})
			}
			continue
		}
		if gapBefore != nil && *gapBefore > 0 {
			gaps = append(gaps, port.ShipmentGap{
				AfterSequence:  from - *gapBefore - 1,
				BeforeSequence: from,
				Missing:        *gapBefore,
			})
		}
	}
	return gaps, pg.WrapQuery("audit_shipment.Gaps", rows.Err())
}

func (r *AuditShipmentRepository) PendingEntries(ctx context.Context, after int64, limit int) ([]port.ShippedEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := `
		SELECT` + shippedColumns + `
		FROM audit_log
		WHERE sequence_no > $1
		ORDER BY sequence_no
		LIMIT $2`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, after, limit)
	if err != nil {
		return nil, pg.WrapQuery("audit_shipment.PendingEntries", err)
	}
	entries, err := pgx.CollectRows(rows, scanShippedEntry)
	return entries, pg.WrapQuery("audit_shipment.PendingEntries", err)
}

func (r *AuditShipmentRepository) EntriesInRange(ctx context.Context, from, to int64) ([]port.ShippedEntry, error) {
	query := `
		SELECT` + shippedColumns + `
		FROM audit_log
		WHERE sequence_no BETWEEN $1 AND $2
		ORDER BY sequence_no`

	q := r.db.Conn(ctx)
	rows, err := q.Query(ctx, query, from, to)
	if err != nil {
		return nil, pg.WrapQuery("audit_shipment.EntriesInRange", err)
	}
	entries, err := pgx.CollectRows(rows, scanShippedEntry)
	return entries, pg.WrapQuery("audit_shipment.EntriesInRange", err)
}

func (r *AuditShipmentRepository) HeadSequence(ctx context.Context) (int64, error) {
	var head int64
	q := r.db.Conn(ctx)
	err := q.QueryRow(ctx, `SELECT coalesce(max(sequence_no), 0) FROM audit_log`).Scan(&head)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, pg.WrapQuery("audit_shipment.HeadSequence", err)
	}
	return head, nil
}

func scanShippedEntry(row pgx.CollectableRow) (port.ShippedEntry, error) {
	var e port.ShippedEntry
	err := row.Scan(
		&e.SequenceNo, &e.ID, &e.EntityType, &e.EntityID, &e.Action,
		&e.ActorUserID, &e.ActorUsername, &e.ActorRoles, &e.ActorIP, &e.RequestID,
		&e.OccurredAt, &e.BeforeState, &e.AfterState, &e.Metadata, &e.Reason,
		&e.AcademicYearID, &e.StudentID, &e.AccountID, &e.PreviousHash, &e.EntryHash,
	)
	return e, err
}

// idOrNilPtr keeps a nil identifier out of a NOT NULL-free column as SQL NULL
// rather than as the zero UUID, which would name a user that does not exist.
func idOrNilPtr(id *shared.ID) any {
	if id == nil {
		return nil
	}
	return *id
}
