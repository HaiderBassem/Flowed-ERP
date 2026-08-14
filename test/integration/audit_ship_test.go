package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/adapter/auditship"
	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/pg"
)

// The audit trail's hash chain proves no entry was edited. It cannot prove none
// was removed: delete the last hundred rows and everything left verifies, since
// verification walks what is present. These tests are about the control that
// covers that blind spot — a copy on a host the database cannot reach — and
// they run against the real database and a real directory because both halves
// of the claim are about things outside Go.

func TestShippedAuditTrailDetectsADeletedEntry(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	dir := t.TempDir()

	service := shipService(t, db, dir)
	actor := shared.SystemActor()

	// Ship whatever the database already holds, so this test's own entries are
	// the tail rather than a block in the middle of somebody else's run. A pass
	// is bounded, so catching up takes as many as it takes — which is also the
	// resumption path a real backlog uses.
	catchUp(t, service)

	marker := "ship-test-" + shared.NewID().String()
	seqs := appendAuditEntries(t, db, marker, 3)

	shipped, err := service.Ship(ctx, actor)
	if err != nil {
		t.Fatalf("shipping: %v", err)
	}
	if shipped.Entries < 3 {
		t.Fatalf("shipped %d entries, expected at least the 3 just written", shipped.Entries)
	}
	if shipped.Remaining != 0 {
		t.Errorf("%d entries left unshipped after a full pass", shipped.Remaining)
	}

	report, err := service.Verify(ctx, actor)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !report.OK() {
		t.Fatalf("a freshly shipped archive should verify clean, got %+v", report.Problems)
	}
	if report.EntriesChecked < 3 {
		t.Errorf("verification checked %d entries; it should read the archive back",
			report.EntriesChecked)
	}

	// Now the scenario the archive exists for: somebody with the database role
	// removes the record of what they did. The immutability trigger refuses a
	// DELETE, so they switch it off — which is exactly what an administrator
	// can do, and exactly what the hash chain cannot notice, because the chain
	// that remains is intact.
	//
	// The tamper happens inside a transaction this test rolls back. An audit
	// entry cannot be put back once it is gone — the hash of the row that
	// followed it was computed over the one that was deleted — so a test that
	// really deleted one would leave every later run of the suite reporting a
	// broken chain, and the four integrity queries CI runs would never return
	// zero again.
	tamper(t, db, func(ctx context.Context) {
		deleteAuditEntry(t, ctx, db, seqs[1])

		report, err := service.Verify(ctx, actor)
		if err != nil {
			t.Fatalf("verifying after the deletion: %v", err)
		}
		if report.OK() {
			t.Fatal("an audit entry was deleted and the archive comparison did not notice; " +
				"the off-host copy is the only thing that can see a truncation")
		}

		found := false
		for _, problem := range report.Problems {
			if problem.Kind == "entry_missing_from_database" && problem.SequenceNo == seqs[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("expected entry_missing_from_database for sequence %d, got %+v",
				seqs[1], report.Problems)
		}
	})

	// And the damage is gone: the suite may be run twice.
	if problems := verifyChainInDatabase(t, db); len(problems) != 0 {
		t.Errorf("the tamper was not rolled back; the chain reports %v", problems)
	}
}

// tamper runs a destructive check inside a transaction and rolls it back.
//
// The service reads through the transaction because every repository resolves
// its connection from the context, so what it sees inside is the tampered
// database — and nothing outside this function ever does.
func tamper(t *testing.T, db *pg.DB, check func(ctx context.Context)) {
	t.Helper()

	rollback := errors.New("rolling back the tamper")
	err := postgres.NewTxManager(db).Write(context.Background(), func(ctx context.Context) error {
		check(ctx)
		return rollback
	})
	if err != nil && !errors.Is(err, rollback) {
		t.Fatalf("the tamper transaction failed for another reason: %v", err)
	}
}

func TestAlteringTheArchiveIsDetected(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	dir := t.TempDir()

	service := shipService(t, db, dir)
	actor := shared.SystemActor()

	appendAuditEntries(t, db, "ship-test-"+shared.NewID().String(), 2)
	catchUp(t, service)

	// The archive is meant to be append-only in the deployment; here it is a
	// directory this test owns, so it can play the part of an attacker with
	// write access to it.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("nothing was written to the archive directory: %v", err)
	}
	target := filepath.Join(dir, entries[len(entries)-1].Name())
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(content), "\"action\":", "\"action_x\":", 1)
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := service.Verify(ctx, actor)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report.OK() {
		t.Fatal("the archived block was edited and verification passed")
	}
	if report.Problems[0].Kind != "archive_altered" {
		t.Errorf("expected archive_altered, got %q", report.Problems[0].Kind)
	}
}

// TestShippingSurvivesADatabaseThatForgotItsShipments is the case a restore
// produces: the archive still holds last week's blocks, and the restored
// database has no record of them.
//
// Refusing to overwrite the archive is right — it is the only copy of what was
// actually sent — but stopping there left shipping stuck on the same block
// forever, and the lag climbed while nothing else looked wrong. Identical bytes
// mean the block is already witnessed, so it is re-recorded and shipping moves
// on. Different bytes are not resolved automatically: one of the two runs is
// wrong and somebody has to find out which.
func TestShippingSurvivesADatabaseThatForgotItsShipments(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	dir := t.TempDir()

	service := shipService(t, db, dir)
	actor := shared.SystemActor()

	appendAuditEntries(t, db, "ship-test-"+shared.NewID().String(), 2)
	catchUp(t, service)

	files, err := os.ReadDir(dir)
	if err != nil || len(files) == 0 {
		t.Fatalf("nothing was shipped: %v", err)
	}

	// The restore: the shipment rows for this destination are gone, the files
	// are not. The table is append-only, which is why this has to suspend the
	// trigger — a restored database really would come back without them.
	forgetShipments(t, db, dir)

	result, err := service.Ship(ctx, actor)
	if err != nil {
		t.Fatalf("shipping after a restore must not fail on blocks the archive already holds: %v", err)
	}
	if result.Remaining != 0 {
		t.Errorf("%d entries left unshipped; shipping did not get past the existing blocks",
			result.Remaining)
	}

	report, err := service.Verify(ctx, actor)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !report.OK() {
		t.Errorf("the archive should verify after the re-record, got %+v", report.Problems)
	}

	// A block whose bytes disagree is a different matter and must not pass.
	target := filepath.Join(dir, files[0].Name())
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{\"sequence_no\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	forgetShipments(t, db, dir)
	if _, err := service.Ship(ctx, actor); err == nil {
		t.Error("a block already in the archive with different contents must stop the run")
	}
}

// forgetShipments removes this destination's shipment rows, the way a restore
// from an older backup would.
func forgetShipments(t *testing.T, db *pg.DB, dir string) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.Pool().Exec(ctx,
		`ALTER TABLE audit_shipment DISABLE TRIGGER trg_audit_shipment_immutable`); err != nil {
		t.Fatalf("suspending the immutability trigger: %v", err)
	}
	defer func() {
		if _, err := db.Pool().Exec(ctx,
			`ALTER TABLE audit_shipment ENABLE TRIGGER trg_audit_shipment_immutable`); err != nil {
			t.Fatalf("restoring the immutability trigger: %v", err)
		}
	}()

	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM audit_shipment WHERE destination = $1`, "dir:"+dir); err != nil {
		t.Fatalf("simulating the restore: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func shipTestDB(t *testing.T) *pg.DB {
	t.Helper()
	ctx := context.Background()

	cfg := config.Database{
		Host:     envOr("DB_HOST", "localhost"),
		Port:     5432,
		User:     envOr("DB_USER", os.Getenv("USER")),
		Password: os.Getenv("DB_PASSWORD"),
		Name:     envOr("DB_NAME", "flowed_dev"),
		SSLMode:  "disable",
		MaxConns: 4,
		MinConns: 1,
		// pgxpool builds a ticker from the health-check period, and a zero
		// period panics rather than falling back to a default.
		HealthCheckPeriod: 30 * time.Second,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   10 * time.Minute,
		ConnectTimeout:    10 * time.Second,
	}
	db, err := pg.Connect(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func shipService(t *testing.T, db *pg.DB, dir string) *app.AuditShipService {
	t.Helper()

	sink, err := auditship.NewDirSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewAuditShipmentRepository(db)
	deps := app.Deps{
		Tx:    postgres.NewTxManager(db),
		Audit: postgres.NewAuditRepository(db),
		Clock: shared.SystemClock{},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// A small batch on a database that has been used produces many blocks,
	// which is what exercises the block boundaries a gap would appear at.
	service := app.NewAuditShipService(deps, store, sink, app.AuditShipConfig{Batch: 200})

	// Each test gets its own directory, so the shipment rows recorded against
	// it are its own too. They stay behind — the table is append-only, which
	// is the point of it — and name a directory that no longer exists, which
	// is why every test ships to a fresh one rather than reusing a name.
	return service
}

// catchUp ships until nothing is left on this host only.
func catchUp(t *testing.T, service *app.AuditShipService) {
	t.Helper()
	ctx := context.Background()
	actor := shared.SystemActor()

	for pass := 0; pass < 50; pass++ {
		result, err := service.Ship(ctx, actor)
		if err != nil {
			t.Fatalf("shipping: %v", err)
		}
		if result.Remaining == 0 {
			return
		}
	}
	t.Fatal("shipping never caught up")
}

// appendAuditEntries writes rows straight into audit_log. The BEFORE INSERT
// trigger chains them, which is what makes this a fair fixture: these entries
// are hashed exactly as the application's are.
func appendAuditEntries(t *testing.T, db *pg.DB, marker string, count int) []int64 {
	t.Helper()
	ctx := context.Background()

	sequences := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		var seq int64
		err := db.Pool().QueryRow(ctx, `
			INSERT INTO audit_log (id, entity_type, action, actor_username, occurred_at, reason)
			VALUES (gen_random_uuid(), 'test_fixture', $1, 'integration', now(), $2)
			RETURNING sequence_no`,
			"fixture.wrote", marker,
		).Scan(&seq)
		if err != nil {
			t.Fatalf("appending an audit entry: %v", err)
		}
		sequences = append(sequences, seq)
	}
	return sequences
}

// deleteAuditEntry plays the part of an operator removing a record.
//
// The immutability trigger refuses a DELETE, so it is switched off for the one
// statement — which is precisely what somebody with the database role would do,
// and precisely why a control that lives only in this database cannot be the
// one that catches them.
func deleteAuditEntry(t *testing.T, ctx context.Context, db *pg.DB, sequence int64) {
	t.Helper()

	if _, err := db.Conn(ctx).Exec(ctx,
		`ALTER TABLE audit_log DISABLE TRIGGER trg_audit_log_immutable`); err != nil {
		t.Fatalf("disabling the immutability trigger: %v", err)
	}
	if _, err := db.Conn(ctx).Exec(ctx,
		`DELETE FROM audit_log WHERE sequence_no = $1`, sequence); err != nil {
		t.Fatalf("deleting the audit entry: %v", err)
	}
}

func verifyChainInDatabase(t *testing.T, db *pg.DB) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.Pool().Query(ctx, `SELECT problem FROM verify_audit_chain(0)`)
	if err != nil {
		t.Fatalf("verifying the chain: %v", err)
	}
	defer rows.Close()

	var problems []string
	for rows.Next() {
		var problem string
		if err := rows.Scan(&problem); err != nil {
			t.Fatal(err)
		}
		problems = append(problems, problem)
	}
	return problems
}
