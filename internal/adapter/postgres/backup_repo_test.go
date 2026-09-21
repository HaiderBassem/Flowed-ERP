// Integration test for the one property that matters about this repository:
// FinishBackup and UpdateRestore must succeed whether or not the row they
// started with still exists.
//
// It normally does — CreateBackup or CreateRestore just inserted it. But a
// restore's own live-database swap replaces backup_run and restore_run with
// the snapshot's older copy of those tables, so by the time the restore
// finishes, the row it has been updating throughout may no longer be there. A
// plain UPDATE finding no match fails silently rather than loudly, which is
// exactly the shape of bug a unit test with an in-memory fake cannot see: the
// fake has no WHERE clause to fail to match.
package postgres

import (
	"testing"
	"time"

	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

func TestFinishBackupUpsertsWhenTheRowWasNeverCreated(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewBackupRepository(reportDB)

		run := &port.BackupRun{
			ID:        shared.NewID(),
			Kind:      port.BackupSafety,
			Status:    port.BackupVerified,
			StartedAt: time.Now().UTC(),
			FilePath:  "/backups/example.dump",
			Bytes:     12345,
			SHA256:    "deadbeef",
			Verified:  true,
		}

		// No CreateBackup call precedes this — simulating the row having been
		// wiped out by a restore's swap before this write landed.
		if err := repo.FinishBackup(f.ctx, run); err != nil {
			t.Fatalf("FinishBackup must insert when the row is absent, got: %v", err)
		}

		got, err := repo.GetBackup(f.ctx, run.ID)
		if err != nil {
			t.Fatalf("the upserted row must be readable back: %v", err)
		}
		if got.Status != port.BackupVerified || !got.Verified || got.Bytes != 12345 {
			t.Fatalf("upserted row does not match what was written: %+v", got)
		}
	})
}

func TestFinishBackupStillUpdatesWhenTheRowExists(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewBackupRepository(reportDB)

		run := &port.BackupRun{
			ID: shared.NewID(), Kind: port.BackupManual, Status: port.BackupRunning, StartedAt: time.Now().UTC(),
		}
		if err := repo.CreateBackup(f.ctx, run); err != nil {
			t.Fatalf("CreateBackup: %v", err)
		}

		run.Status = port.BackupVerified
		run.Verified = true
		run.Bytes = 999
		if err := repo.FinishBackup(f.ctx, run); err != nil {
			t.Fatalf("FinishBackup: %v", err)
		}

		got, err := repo.GetBackup(f.ctx, run.ID)
		if err != nil {
			t.Fatalf("GetBackup: %v", err)
		}
		if got.Status != port.BackupVerified || got.Bytes != 999 {
			t.Fatalf("expected the existing row to be updated in place, got %+v", got)
		}
	})
}

func TestUpdateRestoreUpsertsWhenTheRowWasNeverCreated(t *testing.T) {
	withFixture(t, func(f *fixture) {
		repo := NewBackupRepository(reportDB)

		// The referenced backup must exist — restore_run's own foreign key
		// requires it, the same requirement BackupService satisfies by
		// re-registering the target and safety backups before it upserts the
		// restore row itself.
		backupID := f.scan(`
			INSERT INTO backup_run (id, kind, status, started_at, finished_at, verified)
			VALUES (gen_random_uuid(), 'manual', 'verified', now(), now(), true)
			RETURNING id`)

		now := time.Now().UTC()
		run := &port.RestoreRun{
			ID:               shared.NewID(),
			BackupRunID:      backupID,
			Status:           port.RestoreRestored,
			StartedAt:        now,
			FinishedAt:       &now,
			PreviousDatabase: "flowed_dev_previous_example",
		}

		if err := repo.UpdateRestore(f.ctx, run); err != nil {
			t.Fatalf("UpdateRestore must insert when the row is absent, got: %v", err)
		}

		got, err := repo.GetRestore(f.ctx, run.ID)
		if err != nil {
			t.Fatalf("the upserted restore row must be readable back: %v", err)
		}
		if got.Status != port.RestoreRestored || got.PreviousDatabase != "flowed_dev_previous_example" {
			t.Fatalf("upserted restore row does not match what was written: %+v", got)
		}
	})
}
