package integration

import (
	"context"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/domain/shared"
)

// Verifying the whole chain every night is work that grows forever. The
// checkpoint makes the nightly pass proportional to what was written since the
// last one — and the property that makes that safe rather than merely cheap is
// that the checkpoint stores the hash it stopped at, so a prefix rewritten
// behind it is still caught.

func TestChainVerificationResumesFromItsCheckpoint(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	store := postgres.NewReconciliationRepository(db)

	// A first pass establishes a checkpoint.
	if _, _, err := store.CheckAuditChain(ctx, 100); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	var first int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT coalesce(sequence_no, 0) FROM audit_verification_checkpoint()`).Scan(&first); err != nil {
		t.Fatalf("reading the checkpoint: %v", err)
	}
	if first == 0 {
		t.Fatal("a clean pass must leave a checkpoint; without one every night walks the whole trail")
	}

	// New entries arrive and the next pass advances the checkpoint.
	appendAuditEntries(t, db, "retention-"+shared.NewID().String(), 3)

	found, _, err := store.CheckAuditChain(ctx, 100)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a clean trail reported %d problem(s): %+v", len(found), found[0].Detail)
	}

	var second int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT coalesce(sequence_no, 0) FROM audit_verification_checkpoint()`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Errorf("the checkpoint did not advance: %d then %d", first, second)
	}

	// The pass is on the record, which is how an operator sees that the check
	// has been running rather than only that it is not complaining.
	var passes int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM audit_verification WHERE verified_at > now() - interval '5 minutes'`,
	).Scan(&passes); err != nil {
		t.Fatal(err)
	}
	if passes < 2 {
		t.Errorf("recorded %d verification passes, expected at least 2", passes)
	}
}

// The checkpoint must not become a place to hide behind: an entry altered
// before it is still caught, because the checkpoint remembers the hash.
func TestARewrittenPrefixIsCaughtAtTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	store := postgres.NewReconciliationRepository(db)

	appendAuditEntries(t, db, "retention-"+shared.NewID().String(), 2)
	if _, _, err := store.CheckAuditChain(ctx, 100); err != nil {
		t.Fatalf("establishing a checkpoint: %v", err)
	}

	var at int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT sequence_no FROM audit_verification_checkpoint()`).Scan(&at); err != nil {
		t.Fatal(err)
	}

	// Somebody removes the entry the checkpoint stopped at. Rolled back
	// afterwards: an audit entry cannot be put back once it is gone.
	tamper(t, db, func(ctx context.Context) {
		deleteAuditEntry(t, ctx, db, at)

		found, _, err := store.CheckAuditChain(ctx, 100)
		if err != nil {
			t.Fatalf("verifying after the deletion: %v", err)
		}
		if len(found) == 0 {
			t.Fatal("the entry the checkpoint stopped at was removed and the resumed " +
				"pass did not notice; the checkpoint would then be a place to hide behind")
		}
		detail, _ := found[0].Detail["problem"].(string)
		if detail == "" {
			t.Errorf("the finding says nothing useful: %+v", found[0].Detail)
		}
	})
}

// Archiving is gated on a copy existing off-host. Without one, moving entries
// out of the live table would be the deletion it must never be.
func TestArchivingRefusesWithoutAnOffHostCopy(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)

	var shipments int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_shipment`).Scan(&shipments); err != nil {
		t.Fatal(err)
	}

	_, err := db.Pool().Exec(ctx,
		`SELECT * FROM archive_audit_entries($1, 1000)`, time.Now().Add(-time.Hour))

	if shipments == 0 {
		if err == nil {
			t.Error("archiving with nothing shipped must be refused")
		}
		return
	}
	// With shipments present the call is legal; what matters is that it does
	// not move anything that has not been shipped.
	if err != nil {
		t.Fatalf("archiving with shipments recorded should be permitted: %v", err)
	}
	var live, archived int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT (SELECT coalesce(min(sequence_no), 0) FROM audit_log),
		       (SELECT coalesce(max(sequence_no), 0) FROM audit_log_archive)`,
	).Scan(&live, &archived); err != nil {
		t.Fatal(err)
	}
	if archived > 0 && live > 0 && archived >= live {
		t.Errorf("the archive holds sequence %d while the live table starts at %d; "+
			"the two must not overlap", archived, live)
	}
}
