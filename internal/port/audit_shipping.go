package port

import (
	"context"
	"time"

	"flowed/internal/domain/shared"
)

// ShippedEntry is an audit row as it leaves the host.
//
// It is the stored row rather than the AuditEntry the services append: what
// the archive has to carry is the sequence number and both hashes, because
// those are what a later verification compares. The payload fields are kept as
// raw JSON so a round trip through the archive is byte-identical — re-encoding
// a map would reorder keys, and the hash covers the encoded text.
type ShippedEntry struct {
	SequenceNo     int64      `json:"sequence_no"`
	ID             shared.ID  `json:"id"`
	EntityType     string     `json:"entity_type"`
	EntityID       *shared.ID `json:"entity_id,omitempty"`
	Action         string     `json:"action"`
	ActorUserID    *shared.ID `json:"actor_user_id,omitempty"`
	ActorUsername  string     `json:"actor_username"`
	ActorRoles     []string   `json:"actor_roles,omitempty"`
	ActorIP        *string    `json:"actor_ip,omitempty"`
	RequestID      *string    `json:"request_id,omitempty"`
	OccurredAt     time.Time  `json:"occurred_at"`
	BeforeState    *string    `json:"before_state,omitempty"`
	AfterState     *string    `json:"after_state,omitempty"`
	Metadata       *string    `json:"metadata,omitempty"`
	Reason         *string    `json:"reason,omitempty"`
	AcademicYearID *shared.ID `json:"academic_year_id,omitempty"`
	StudentID      *shared.ID `json:"student_id,omitempty"`
	AccountID      *shared.ID `json:"account_id,omitempty"`
	PreviousHash   *string    `json:"previous_hash,omitempty"`
	EntryHash      string     `json:"entry_hash"`
}

// AuditShipment is one block of entries copied off the host.
type AuditShipment struct {
	ID             shared.ID
	Destination    string
	ArtifactRef    string
	FromSequence   int64
	ToSequence     int64
	EntryCount     int
	FirstEntryHash string
	LastEntryHash  string
	ContentSHA256  string
	ContentBytes   int64
	ShippedAt      time.Time
	ShippedBy      *shared.ID
}

// AuditShipmentRepository records what has left the host, and what has not.
type AuditShipmentRepository interface {
	// Record writes one shipment. The table is append-only.
	Record(ctx context.Context, shipment *AuditShipment) error
	// LastShipped is the highest sequence number that reached this
	// destination. Zero when nothing has.
	LastShipped(ctx context.Context, destination string) (int64, error)
	// List returns shipments newest first.
	List(ctx context.Context, destination string, limit int) ([]*AuditShipment, error)
	// Gaps returns shipment blocks that do not follow the previous one — a
	// range of entries that never left.
	Gaps(ctx context.Context, destination string) ([]ShipmentGap, error)
	// PendingEntries reads the audit rows after a sequence number, oldest
	// first, at most limit of them.
	PendingEntries(ctx context.Context, afterSequence int64, limit int) ([]ShippedEntry, error)
	// EntriesInRange reads back a shipped block for comparison against the
	// archive.
	EntriesInRange(ctx context.Context, from, to int64) ([]ShippedEntry, error)
	// HeadSequence is the highest sequence number in audit_log, which is how
	// far behind shipping has fallen.
	HeadSequence(ctx context.Context) (int64, error)
}

// ShipmentGap is a range of audit entries between two shipments that no
// shipment covers.
type ShipmentGap struct {
	AfterSequence  int64
	BeforeSequence int64
	Missing        int64
}
