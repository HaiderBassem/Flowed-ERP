package integration

import (
	"context"
	"testing"
	"time"

	"flowed/internal/adapter/postgres"
	"flowed/internal/domain/shared"
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

// TestAnAppendWaitingOnAnotherSeesWhatItWrote reproduces the fork exactly.
//
// The chain used to be serialised with a transaction-scoped advisory lock taken
// inside the insert trigger, which looks right and is not. Under READ COMMITTED
// a statement's snapshot is taken when the statement starts — before the
// trigger runs and before the lock is requested — so a transaction that waited
// for the lock resumed with a snapshot that still could not see what the winner
// had committed. Both chained onto the same predecessor. The trail forked, one
// entry's hash was referenced by nobody, and the next verification called it
// tampering, on a system whose whole claim is that it can tell tampering from
// ordinary operation.
//
// Racing goroutines reproduce it only sometimes. Two connections driven by hand
// reproduce it every time: A takes the lock and holds it, B starts its insert
// and blocks, A commits, B proceeds. That is the interleaving, and it is what
// two cashiers posting at the same moment produce.
func TestAnAppendWaitingOnAnotherSeesWhatItWrote(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)

	var before int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT coalesce(max(sequence_no), 0) FROM audit_log`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	first, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	tx, err := first.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const insert = `
		INSERT INTO audit_log (id, entity_type, action, actor_username, occurred_at, reason)
		VALUES (gen_random_uuid(), 'test_fixture', $1, 'integration', now(), $2)
		RETURNING sequence_no, previous_hash, entry_hash`

	marker := "fork-" + shared.NewID().String()

	// A writes and holds the lock by not committing.
	var firstSeq int64
	var firstPrev *string
	var firstHash string
	if err := tx.QueryRow(ctx, insert, "concurrent.first", marker).
		Scan(&firstSeq, &firstPrev, &firstHash); err != nil {
		t.Fatalf("the first append: %v", err)
	}

	// B starts while A still holds it. It will block inside the trigger.
	type result struct {
		seq  int64
		prev *string
		hash string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = second.QueryRow(ctx, insert, "concurrent.second", marker).
			Scan(&r.seq, &r.prev, &r.hash)
		done <- r
	}()

	// Long enough for the second insert to reach the lock and block on it.
	time.Sleep(300 * time.Millisecond)

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the first append: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second append never completed; it is still waiting for a lock " +
			"the first one released")
	}
	if r.err != nil {
		t.Fatalf("the second append: %v", r.err)
	}

	// The whole point: the waiter must chain onto the entry that committed
	// while it waited, not onto the one it could see when it started.
	if r.prev == nil || *r.prev != firstHash {
		got := "nothing"
		if r.prev != nil {
			got = *r.prev
		}
		t.Errorf("the second entry chained onto %s; it must chain onto the first "+
			"entry's hash %s, which committed while it waited", got, firstHash)
	}
	if r.seq <= firstSeq {
		t.Errorf("the second entry is numbered %d against the first's %d; the number "+
			"is allocated under the same lock and must follow", r.seq, firstSeq)
	}

	// And nothing this test wrote is reported as a problem. Scoped to its own
	// range: a shared database carries whatever earlier runs left in it,
	// including — until this fix — forks caused by the very defect under test.
	var problems int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM verify_audit_chain($1)`, before).Scan(&problems); err != nil {
		t.Fatal(err)
	}
	if problems != 0 {
		t.Errorf("verification reports %d problem(s) among the entries this test wrote",
			problems)
	}
}
