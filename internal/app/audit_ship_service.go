package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/swibit/flowed/internal/adapter/auditship"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/observability"
	"github.com/swibit/flowed/internal/port"
)

// AuditShipService copies the audit trail somewhere the database cannot reach,
// and later checks that the copy and the database still agree.
//
// The hash chain proves an entry was not edited. It cannot prove none was
// removed: delete the last hundred rows and the remaining chain verifies
// perfectly, because verification walks what is present. Truncation is both the
// blind spot and the likely attack — an administrator removing the record of
// what they did is an ordinary story, forging a sha256 is not.
//
// So the trail is copied off-host in blocks, and each block records the range
// it covered and the chain hash at each end. Verification then compares the
// archive against the database and reports three distinct failures: a range
// that never shipped, an entry the archive holds and the database no longer
// does, and an entry whose stored form has changed since it was shipped.
type AuditShipService struct {
	deps    Deps
	store   port.AuditShipmentRepository
	sink    auditship.Sink
	batch   int
	maxRuns int
	metrics *observability.Metrics
	logger  *slog.Logger
}

// AuditShipConfig tunes shipping.
type AuditShipConfig struct {
	// Batch is how many entries go in one block. Large enough that a busy day
	// is a handful of files, small enough that one lost block is a small hole.
	Batch int
	// MaxBlocksPerRun bounds a single pass, so catching up on a year of
	// backlog does not hold the process for an hour on its first run.
	MaxBlocksPerRun int
}

// NewAuditShipService wires shipping over a destination.
func NewAuditShipService(
	d Deps, store port.AuditShipmentRepository, sink auditship.Sink, cfg AuditShipConfig,
) *AuditShipService {
	if cfg.Batch <= 0 {
		cfg.Batch = 500
	}
	if cfg.MaxBlocksPerRun <= 0 {
		cfg.MaxBlocksPerRun = 20
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &AuditShipService{
		deps:    d,
		store:   store,
		sink:    sink,
		batch:   cfg.Batch,
		maxRuns: cfg.MaxBlocksPerRun,
		metrics: d.Metrics,
		logger:  log.With(slog.String("component", "audit_ship")),
	}
}

// ShipResult is what one pass did.
type ShipResult struct {
	Blocks      int    `json:"blocks"`
	Entries     int    `json:"entries"`
	FromEnabled int64  `json:"from_sequence"`
	ToSequence  int64  `json:"to_sequence"`
	Destination string `json:"destination"`
	// Remaining is how many entries are still on the host only. Non-zero after
	// a run means the batch limit was reached, not that anything failed.
	Remaining int64 `json:"remaining"`
}

// Ship copies everything not yet shipped.
//
// Order matters and is deliberate: the bytes go to the destination first, and
// the shipment row is written only after the destination confirmed the write.
// The other order would record a shipment that does not exist, and the
// verification that trusts that row would then report a healthy archive with a
// hole in it. The cost of this order is a possible duplicate file after a lost
// response, which the unique constraint turns into a no-op.
func (s *AuditShipService) Ship(ctx context.Context, actor shared.Actor) (*ShipResult, error) {
	if err := s.requireAuthority(actor, "ShipAuditLog"); err != nil {
		return nil, err
	}

	destination := s.sink.Name()
	result := &ShipResult{Destination: destination}

	last, err := s.store.LastShipped(ctx, destination)
	if err != nil {
		return nil, err
	}
	result.FromEnabled = last + 1

	for block := 0; block < s.maxRuns; block++ {
		entries, err := s.store.PendingEntries(ctx, last, s.batch)
		if err != nil {
			s.metrics.AuditShipFailure(ctx, "read")
			return nil, err
		}
		if len(entries) == 0 {
			break
		}

		content, err := encodeBlock(entries)
		if err != nil {
			s.metrics.AuditShipFailure(ctx, "encode")
			return nil, err
		}

		from := entries[0].SequenceNo
		to := entries[len(entries)-1].SequenceNo
		artifact := fmt.Sprintf("audit-%012d-%012d.ndjson", from, to)

		ref, err := s.sink.Put(ctx, artifact, content)
		if err != nil {
			s.metrics.AuditShipFailure(ctx, "write")
			return nil, shared.Internal("audit.ship_failed", err,
				"the audit archive at %s did not accept the block covering %d-%d",
				destination, from, to)
		}

		sum := sha256.Sum256(content)
		shipment := &port.AuditShipment{
			ID:             shared.NewID(),
			Destination:    destination,
			ArtifactRef:    ref,
			FromSequence:   from,
			ToSequence:     to,
			EntryCount:     len(entries),
			FirstEntryHash: entries[0].EntryHash,
			LastEntryHash:  entries[len(entries)-1].EntryHash,
			ContentSHA256:  hex.EncodeToString(sum[:]),
			ContentBytes:   int64(len(content)),
			ShippedAt:      nowOr(s.deps.Clock),
		}
		if !actor.IsSystem() && actor.UserID != shared.NilID {
			id := actor.UserID
			shipment.ShippedBy = &id
		}

		if err := s.store.Record(ctx, shipment); err != nil {
			// A duplicate means a previous run shipped this block and lost the
			// response. The file is already there; nothing is wrong.
			if isDuplicate(err) {
				s.logger.Info("audit block was already recorded as shipped",
					slog.Int64("from", from), slog.Int64("to", to))
				last = to
				continue
			}
			s.metrics.AuditShipFailure(ctx, "record")
			return nil, err
		}

		s.metrics.AuditShipped(ctx, len(entries))
		result.Blocks++
		result.Entries += len(entries)
		result.ToSequence = to
		last = to
	}

	head, err := s.store.HeadSequence(ctx)
	if err != nil {
		return nil, err
	}
	result.Remaining = head - last
	s.metrics.AuditShipLag(ctx, result.Remaining)

	return result, nil
}

// VerifyReport is the answer to "is the off-host copy still a witness".
type VerifyReport struct {
	Destination     string          `json:"destination"`
	Shipments       int             `json:"shipments"`
	EntriesChecked  int             `json:"entries_checked"`
	Unshipped       int64           `json:"unshipped_entries"`
	ArchiveReadable bool            `json:"archive_readable"`
	Problems        []VerifyProblem `json:"problems"`
}

// VerifyProblem names one disagreement, in the terms an auditor needs.
type VerifyProblem struct {
	Kind       string `json:"kind"`
	SequenceNo int64  `json:"sequence_no,omitempty"`
	Artifact   string `json:"artifact,omitempty"`
	Detail     string `json:"detail"`
}

// OK reports whether the archive and the database still agree.
func (r *VerifyReport) OK() bool { return len(r.Problems) == 0 }

// Verify compares the archive against the database.
//
// Four questions, and each catches something the others cannot:
//
//   - Do the shipments cover a contiguous range? A gap is a block of entries
//     that never left the host, and it is where a truncation would hide.
//   - Do the archived bytes still hash to what was recorded? An archive edited
//     in place fails here.
//   - Does every archived entry still exist in the database, with the same
//     hash? A deleted tail fails here, and this is the check the chain alone
//     cannot make.
//   - Is the chain within each block continuous, each entry naming the one
//     before it? A block assembled from unrelated entries fails here.
func (s *AuditShipService) Verify(ctx context.Context, actor shared.Actor) (*VerifyReport, error) {
	if err := s.requireAuthority(actor, "VerifyAuditArchive"); err != nil {
		return nil, err
	}

	destination := s.sink.Name()
	report := &VerifyReport{Destination: destination, ArchiveReadable: true}

	gaps, err := s.store.Gaps(ctx, destination)
	if err != nil {
		return nil, err
	}
	for _, gap := range gaps {
		report.Problems = append(report.Problems, VerifyProblem{
			Kind:       "coverage_gap",
			SequenceNo: gap.AfterSequence + 1,
			Detail: fmt.Sprintf(
				"%d audit entries between %d and %d never reached the archive",
				gap.Missing, gap.AfterSequence, gap.BeforeSequence),
		})
	}

	shipments, err := s.store.List(ctx, destination, 10_000)
	if err != nil {
		return nil, err
	}
	report.Shipments = len(shipments)

	for _, shipment := range shipments {
		content, err := s.sink.Get(ctx, shipment.ArtifactRef)
		if errors.Is(err, auditship.ErrNotReadable) {
			// A write-only destination is a legitimate deployment. Say so
			// rather than reporting a verified archive that was never read.
			report.ArchiveReadable = false
			break
		}
		if err != nil {
			report.Problems = append(report.Problems, VerifyProblem{
				Kind:     "archive_unreadable",
				Artifact: shipment.ArtifactRef,
				Detail:   err.Error(),
			})
			continue
		}

		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != shipment.ContentSHA256 {
			report.Problems = append(report.Problems, VerifyProblem{
				Kind:     "archive_altered",
				Artifact: shipment.ArtifactRef,
				Detail:   "the archived bytes no longer hash to what was recorded when they were shipped",
			})
			continue
		}

		archived, err := decodeBlock(content)
		if err != nil {
			report.Problems = append(report.Problems, VerifyProblem{
				Kind:     "archive_unreadable",
				Artifact: shipment.ArtifactRef,
				Detail:   err.Error(),
			})
			continue
		}
		if len(archived) != shipment.EntryCount {
			report.Problems = append(report.Problems, VerifyProblem{
				Kind:     "archive_truncated",
				Artifact: shipment.ArtifactRef,
				Detail: fmt.Sprintf("the block holds %d entries; %d were shipped",
					len(archived), shipment.EntryCount),
			})
		}

		live, err := s.store.EntriesInRange(ctx, shipment.FromSequence, shipment.ToSequence)
		if err != nil {
			return nil, err
		}
		bySequence := make(map[int64]port.ShippedEntry, len(live))
		for _, entry := range live {
			bySequence[entry.SequenceNo] = entry
		}

		var previous *port.ShippedEntry
		for i := range archived {
			entry := archived[i]
			report.EntriesChecked++

			current, present := bySequence[entry.SequenceNo]
			switch {
			case !present:
				report.Problems = append(report.Problems, VerifyProblem{
					Kind:       "entry_missing_from_database",
					SequenceNo: entry.SequenceNo,
					Artifact:   shipment.ArtifactRef,
					Detail: fmt.Sprintf(
						"the archive holds %s on %s by %s; the database no longer does",
						entry.Action, entry.EntityType, entry.ActorUsername),
				})
			case current.EntryHash != entry.EntryHash:
				report.Problems = append(report.Problems, VerifyProblem{
					Kind:       "entry_altered",
					SequenceNo: entry.SequenceNo,
					Artifact:   shipment.ArtifactRef,
					Detail:     "the database row's hash differs from the copy that was shipped",
				})
			}

			// Continuity inside the block: each entry names the one before it.
			if previous != nil && (entry.PreviousHash == nil || *entry.PreviousHash != previous.EntryHash) {
				report.Problems = append(report.Problems, VerifyProblem{
					Kind:       "chain_break",
					SequenceNo: entry.SequenceNo,
					Artifact:   shipment.ArtifactRef,
					Detail:     "this entry does not follow the one before it in the archived block",
				})
			}
			previous = &archived[i]
		}

		if len(archived) > 0 {
			if archived[0].EntryHash != shipment.FirstEntryHash ||
				archived[len(archived)-1].EntryHash != shipment.LastEntryHash {
				report.Problems = append(report.Problems, VerifyProblem{
					Kind:     "endpoint_mismatch",
					Artifact: shipment.ArtifactRef,
					Detail:   "the block's first or last hash is not the one recorded for this shipment",
				})
			}
		}
	}

	head, err := s.store.HeadSequence(ctx)
	if err != nil {
		return nil, err
	}
	last, err := s.store.LastShipped(ctx, destination)
	if err != nil {
		return nil, err
	}
	report.Unshipped = head - last

	if !report.OK() {
		s.metrics.AuditShipFailure(ctx, "verify")
	}
	return report, nil
}

// requireAuthority: shipping and verifying the audit trail are auditor work.
// The scheduler's system actor is admitted for the shipping job specifically —
// it holds no roles, and this is one of the fixed routines it exists to run.
func (s *AuditShipService) requireAuthority(actor shared.Actor, operation string) error {
	if actor.IsSystem() {
		return nil
	}
	return actor.RequireAnyRole(operation, shared.RoleAdmin, shared.RoleAuditor)
}

// encodeBlock renders entries as newline-delimited JSON.
//
// One entry per line, so a block can be read with a text tool on a machine
// that has nothing installed — which is the situation an archive is read in.
func encodeBlock(entries []port.ShippedEntry) ([]byte, error) {
	var buf strings.Builder
	encoder := json.NewEncoder(&buf)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return nil, shared.Internal("audit.encode_failed", err, "encoding an audit block")
		}
	}
	return []byte(buf.String()), nil
}

func decodeBlock(content []byte) ([]port.ShippedEntry, error) {
	var entries []port.ShippedEntry
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	for decoder.More() {
		var entry port.ShippedEntry
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("the archived block is not readable: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// isDuplicate reports whether an error is the unique constraint on the
// shipment block — a re-ship of a range that already went.
func isDuplicate(err error) bool {
	var domainErr *shared.Error
	if errors.As(err, &domainErr) {
		return domainErr.Code == "duplicate_record"
	}
	return false
}
