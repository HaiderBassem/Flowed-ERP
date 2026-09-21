package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"flowed/internal/adapter/backup"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeBackupStore stands in for port.BackupRepository entirely in memory, so
// the restore state machine can be driven and inspected without a database.
type fakeBackupStore struct {
	backups  map[shared.ID]*port.BackupRun
	restores map[shared.ID]*port.RestoreRun
	schedule port.BackupSchedule
}

func newFakeBackupStore() *fakeBackupStore {
	return &fakeBackupStore{
		backups:  map[shared.ID]*port.BackupRun{},
		restores: map[shared.ID]*port.RestoreRun{},
		schedule: port.BackupSchedule{IntervalHours: 24, RetentionCount: 30},
	}
}

func (s *fakeBackupStore) CreateBackup(_ context.Context, run *port.BackupRun) error {
	clone := *run
	s.backups[run.ID] = &clone
	return nil
}

// FinishBackup mirrors the real repository's upsert: it must succeed whether
// or not CreateBackup ever ran for this id, which is exactly the property the
// restore flow's post-swap re-registration depends on.
func (s *fakeBackupStore) FinishBackup(_ context.Context, run *port.BackupRun) error {
	clone := *run
	s.backups[run.ID] = &clone
	return nil
}

func (s *fakeBackupStore) GetBackup(_ context.Context, id shared.ID) (*port.BackupRun, error) {
	run, ok := s.backups[id]
	if !ok {
		return nil, shared.NotFound("backup.not_found", "no such backup")
	}
	clone := *run
	return &clone, nil
}

func (s *fakeBackupStore) ListBackups(context.Context, int) ([]*port.BackupRun, error) {
	out := make([]*port.BackupRun, 0, len(s.backups))
	for _, b := range s.backups {
		out = append(out, b)
	}
	return out, nil
}

func (s *fakeBackupStore) DeleteBackup(_ context.Context, id shared.ID) error {
	if _, ok := s.backups[id]; !ok {
		return shared.NotFound("backup.not_found", "no such backup")
	}
	delete(s.backups, id)
	return nil
}

func (s *fakeBackupStore) CreateRestore(_ context.Context, run *port.RestoreRun) error {
	clone := *run
	s.restores[run.ID] = &clone
	return nil
}

// UpdateRestore is the other upsert: it must work even when the row it
// started with no longer exists, which is the exact situation a restore's own
// live-database swap creates.
func (s *fakeBackupStore) UpdateRestore(_ context.Context, run *port.RestoreRun) error {
	clone := *run
	s.restores[run.ID] = &clone
	return nil
}

func (s *fakeBackupStore) GetRestore(_ context.Context, id shared.ID) (*port.RestoreRun, error) {
	run, ok := s.restores[id]
	if !ok {
		return nil, shared.NotFound("restore.not_found", "no such restore")
	}
	return run, nil
}

func (s *fakeBackupStore) ListRestores(context.Context, int) ([]*port.RestoreRun, error) {
	out := make([]*port.RestoreRun, 0, len(s.restores))
	for _, r := range s.restores {
		out = append(out, r)
	}
	return out, nil
}

func (s *fakeBackupStore) GetSchedule(context.Context) (*port.BackupSchedule, error) {
	clone := s.schedule
	return &clone, nil
}

func (s *fakeBackupStore) UpdateSchedule(_ context.Context, sched *port.BackupSchedule) error {
	s.schedule = *sched
	return nil
}

var _ port.BackupRepository = (*fakeBackupStore)(nil)

// fakeRunner stands in for backup.Runner. Each field that would otherwise
// return success can be overridden to fail, so a test can put the restore
// state machine at exactly the stage it wants to examine.
type fakeRunner struct {
	dumpErr        error
	verifyErr      error
	createErr      error
	restoreIntoErr error
	checks         []backup.Check
	checksErr      error
	swapErr        error

	scratchCreated []string
	dropped        []string
	swapped        bool
}

func (f *fakeRunner) Dump(context.Context, string, string) (backup.DumpResult, error) {
	if f.dumpErr != nil {
		return backup.DumpResult{}, f.dumpErr
	}
	return backup.DumpResult{Bytes: 100, SHA256: "abc", SchemaVersion: 25}, nil
}

func (f *fakeRunner) VerifyDump(context.Context, string) error { return f.verifyErr }

func (f *fakeRunner) CurrentSchemaVersion(context.Context, string) (int64, error) { return 25, nil }

func (f *fakeRunner) CreateScratchDatabase(_ context.Context, name string) error {
	f.scratchCreated = append(f.scratchCreated, name)
	return f.createErr
}

func (f *fakeRunner) DropDatabase(_ context.Context, name string) error {
	f.dropped = append(f.dropped, name)
	return nil
}

func (f *fakeRunner) RestoreInto(context.Context, string, string) error { return f.restoreIntoErr }

func (f *fakeRunner) RunChecks(context.Context, string) ([]backup.Check, error) {
	if f.checksErr != nil {
		return nil, f.checksErr
	}
	if f.checks != nil {
		return f.checks, nil
	}
	return []backup.Check{
		{Name: "account caches match their transactions", Passed: true},
		{Name: "installment caches match their allocations", Passed: true},
		{Name: "no payment is refunded beyond what it took", Passed: true},
		{Name: "the audit chain verifies end to end", Passed: true},
	}, nil
}

func (f *fakeRunner) SwapLive(context.Context, string, string, string) error {
	if f.swapErr != nil {
		return f.swapErr
	}
	f.swapped = true
	return nil
}

var _ dumpRunner = (*fakeRunner)(nil)

// fakePool stands in for the application's own connection pool during a
// restore's live swap.
type fakePool struct {
	drainErr  error
	reopenErr error
	drained   int
	reopened  int
}

func (p *fakePool) Drain(context.Context) error  { p.drained++; return p.drainErr }
func (p *fakePool) Reopen(context.Context) error { p.reopened++; return p.reopenErr }

var _ poolSwapper = (*fakePool)(nil)

// fakeGate stands in for httpx.MaintenanceGate.
type fakeGate struct {
	entered, exited int
}

func (g *fakeGate) Enter(string) { g.entered++ }
func (g *fakeGate) Exit()        { g.exited++ }

var _ maintenanceGate = (*fakeGate)(nil)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

func newBackupFixture(t *testing.T, runner *fakeRunner, pool *fakePool, gate *fakeGate) (*BackupService, *fakeBackupStore, *fakeAudit) {
	t.Helper()
	store := newFakeBackupStore()
	audit := &fakeAudit{}
	deps := Deps{
		Audit: audit,
		Clock: shared.FixedClock{Instant: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		Log:   discardLogger(),
	}
	svc := NewBackupService(deps, store, runner, pool, gate, BackupServiceConfig{
		Dir: t.TempDir(), LiveDatabase: "flowed_test", RetentionMinKeep: 1,
	})
	return svc, store, audit
}

func adminActor() shared.Actor {
	return shared.Actor{UserID: shared.NewID(), Username: "admin", Roles: []shared.Role{shared.RoleAdmin}}
}

// ---------------------------------------------------------------------------
// CreateBackup
// ---------------------------------------------------------------------------

func TestCreateBackupRecordsFailureWithoutReturningAnError(t *testing.T) {
	runner := &fakeRunner{dumpErr: errors.New("disk full")}
	svc, store, audit := newBackupFixture(t, runner, &fakePool{}, &fakeGate{})

	run, err := svc.CreateBackup(context.Background(), adminActor(), port.BackupManual)
	if err != nil {
		t.Fatalf("a dump failure must be reported on the row, not as a transport error: %v", err)
	}
	if run.Status != port.BackupFailed {
		t.Fatalf("expected BackupFailed, got %s", run.Status)
	}
	if run.Error == "" {
		t.Fatal("expected a failure reason on the row")
	}
	if _, ok := store.backups[run.ID]; !ok {
		t.Fatal("the failed attempt must still be recorded in history")
	}
	if len(audit.entries) != 1 || audit.entries[0].Action != "backup.create" {
		t.Fatalf("expected one backup.create audit entry, got %v", audit.actions())
	}
}

func TestCreateBackupVerifiedRecordsRow(t *testing.T) {
	svc, store, _ := newBackupFixture(t, &fakeRunner{}, &fakePool{}, &fakeGate{})

	run, err := svc.CreateBackup(context.Background(), adminActor(), port.BackupManual)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if run.Status != port.BackupVerified || !run.Verified {
		t.Fatalf("expected a verified backup, got %+v", run)
	}
	if store.backups[run.ID].Bytes != run.Bytes {
		t.Fatal("the stored row must match what was returned")
	}
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

func verifiedBackup(t *testing.T, svc *BackupService) *port.BackupRun {
	t.Helper()
	run, err := svc.CreateBackup(context.Background(), adminActor(), port.BackupManual)
	if err != nil || run.Status != port.BackupVerified {
		t.Fatalf("fixture backup did not verify: %v %+v", err, run)
	}
	return run
}

func TestRestoreRefusesAnUnverifiedBackup(t *testing.T) {
	svc, store, _ := newBackupFixture(t, &fakeRunner{}, &fakePool{}, &fakeGate{})
	unverified := &port.BackupRun{ID: shared.NewID(), Kind: port.BackupManual, Status: port.BackupFailed}
	store.backups[unverified.ID] = unverified

	_, err := svc.Restore(context.Background(), adminActor(), unverified.ID)
	domainErr, ok := shared.AsDomain(err)
	if !ok || domainErr.Kind != shared.KindPreconditionFailed {
		t.Fatalf("expected KindPreconditionFailed, got %v", err)
	}
}

func TestRestoreStopsIfSafetyBackupFails(t *testing.T) {
	runner := &fakeRunner{}
	svc, _, _ := newBackupFixture(t, runner, &fakePool{}, &fakeGate{})
	target := verifiedBackup(t, svc)

	// The safety backup is the second Dump call this test's runner will see
	// (the first produced `target`); fail every dump from here on so the
	// safety copy specifically is what fails.
	runner.dumpErr = errors.New("disk full")

	restore, err := svc.Restore(context.Background(), adminActor(), target.ID)
	if err != nil {
		t.Fatalf("a failed restore is a recorded outcome, not a transport error: %v", err)
	}
	if restore.Status != port.RestoreFailed {
		t.Fatalf("expected RestoreFailed, got %s", restore.Status)
	}
	if len(runner.scratchCreated) != 0 {
		t.Fatal("the live database must never be approached if the safety backup itself failed")
	}
}

func TestRestoreStopsIfChecksFail(t *testing.T) {
	runner := &fakeRunner{
		checks: []backup.Check{
			{Name: "account caches match their transactions", Passed: false, Detail: "3 row(s) disagree"},
		},
	}
	svc, _, _ := newBackupFixture(t, runner, &fakePool{}, &fakeGate{})
	target := verifiedBackup(t, svc)

	restore, err := svc.Restore(context.Background(), adminActor(), target.ID)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if restore.Status != port.RestoreFailed {
		t.Fatalf("expected RestoreFailed, got %s", restore.Status)
	}
	if len(runner.dropped) != 1 {
		t.Fatalf("expected the scratch database to be dropped exactly once, got %d", len(runner.dropped))
	}
	if runner.swapped {
		t.Fatal("a failed check must never reach the live swap")
	}
}

func TestRestoreSwapFailureRollsBackAndReopens(t *testing.T) {
	runner := &fakeRunner{swapErr: errors.New("rename refused: file busy")}
	pool := &fakePool{}
	gate := &fakeGate{}
	svc, _, _ := newBackupFixture(t, runner, pool, gate)
	target := verifiedBackup(t, svc)

	restore, err := svc.Restore(context.Background(), adminActor(), target.ID)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if restore.Status != port.RestoreFailed {
		t.Fatalf("expected RestoreFailed, got %s", restore.Status)
	}
	if pool.drained != 1 || pool.reopened != 1 {
		t.Fatalf("expected exactly one drain and one reopen even on failure, got drained=%d reopened=%d",
			pool.drained, pool.reopened)
	}
	if gate.entered != 1 || gate.exited != 1 {
		t.Fatalf("the maintenance gate must close and reopen exactly once, got entered=%d exited=%d",
			gate.entered, gate.exited)
	}
}

func TestRestoreSuccessReopensPoolAndReregistersHistory(t *testing.T) {
	runner := &fakeRunner{}
	pool := &fakePool{}
	gate := &fakeGate{}
	svc, store, _ := newBackupFixture(t, runner, pool, gate)
	target := verifiedBackup(t, svc)

	restore, err := svc.Restore(context.Background(), adminActor(), target.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restore.Status != port.RestoreRestored {
		t.Fatalf("expected RestoreRestored, got %s: %s", restore.Status, restore.Error)
	}
	if !restore.AllChecksPassed() {
		t.Fatal("expected every check recorded as passed")
	}
	if pool.drained != 1 || pool.reopened != 1 {
		t.Fatalf("expected one drain and one reopen, got drained=%d reopened=%d", pool.drained, pool.reopened)
	}
	if !runner.swapped {
		t.Fatal("expected the swap to have run")
	}
	if restore.SafetyBackupID == nil {
		t.Fatal("expected a safety backup to be recorded")
	}
	// The point of the fix that made this pass: after the swap "replaces"
	// backup_run with the snapshot's older copy, both the restored backup and
	// its safety copy must still be findable in what UpdateRestore's caller
	// reads back — a restore whose own bookkeeping vanished with the swap it
	// performed would be indistinguishable, from the UI, from one that never
	// finished.
	if _, ok := store.backups[target.ID]; !ok {
		t.Fatal("the restored backup must still be registered after the swap")
	}
	if _, ok := store.backups[*restore.SafetyBackupID]; !ok {
		t.Fatal("the safety backup must still be registered after the swap")
	}
	if _, ok := store.restores[restore.ID]; !ok {
		t.Fatal("the restore attempt itself must be registered after the swap")
	}
}

func TestRestoreReopenFailureAfterSuccessfulSwapReportsFailedNotRestored(t *testing.T) {
	runner := &fakeRunner{}
	pool := &fakePool{reopenErr: errors.New("connection refused")}
	svc, _, _ := newBackupFixture(t, runner, pool, &fakeGate{})
	target := verifiedBackup(t, svc)

	restore, err := svc.Restore(context.Background(), adminActor(), target.ID)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if restore.Status != port.RestoreFailed {
		t.Fatalf("data restored but unreachable must not report as a clean success, got %s", restore.Status)
	}
	if !runner.swapped {
		t.Fatal("the data itself was moved; only reconnecting failed")
	}
	if pool.reopened < 3 {
		t.Fatalf("expected the reopen retry loop to have been exhausted, got %d attempts", pool.reopened)
	}
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

func TestUpdateScheduleRejectsAZeroInterval(t *testing.T) {
	svc, _, _ := newBackupFixture(t, &fakeRunner{}, &fakePool{}, &fakeGate{})
	_, err := svc.UpdateSchedule(context.Background(), adminActor(), true, 0, 10)
	domainErr, ok := shared.AsDomain(err)
	if !ok || domainErr.Kind != shared.KindValidation {
		t.Fatalf("expected KindValidation, got %v", err)
	}
}

func TestRunScheduledBackupSkipsWhenNotDue(t *testing.T) {
	runner := &fakeRunner{}
	svc, store, _ := newBackupFixture(t, runner, &fakePool{}, &fakeGate{})
	store.schedule = port.BackupSchedule{Enabled: false, IntervalHours: 24, RetentionCount: 30}

	ran, err := svc.RunScheduledBackup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ran {
		t.Fatal("a disabled schedule must not run")
	}
	if len(store.backups) != 0 {
		t.Fatal("no backup should have been created")
	}
}

func TestRunScheduledBackupRunsWhenDue(t *testing.T) {
	runner := &fakeRunner{}
	svc, store, _ := newBackupFixture(t, runner, &fakePool{}, &fakeGate{})
	store.schedule = port.BackupSchedule{Enabled: true, IntervalHours: 24, RetentionCount: 30}

	ran, err := svc.RunScheduledBackup(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ran {
		t.Fatal("an overdue enabled schedule must run")
	}
	if len(store.backups) != 1 {
		t.Fatalf("expected exactly one backup, got %d", len(store.backups))
	}
	if store.schedule.LastRunAt == nil {
		t.Fatal("expected last_run_at to be stamped")
	}
}
