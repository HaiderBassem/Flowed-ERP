package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"flowed/internal/adapter/backup"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// dumpRunner is the slice of backup.Runner this service needs: taking a dump,
// checking it is readable, restoring it into a scratch database, running the
// checks that gate a swap, and performing the swap itself. An interface
// rather than *backup.Runner directly, so a test can drive the whole restore
// state machine — including a failing check, a failing swap — without a real
// PostgreSQL or a real pg_dump on PATH.
type dumpRunner interface {
	Dump(ctx context.Context, database, destPath string) (backup.DumpResult, error)
	VerifyDump(ctx context.Context, path string) error
	CurrentSchemaVersion(ctx context.Context, database string) (int64, error)
	CreateScratchDatabase(ctx context.Context, name string) error
	DropDatabase(ctx context.Context, name string) error
	RestoreInto(ctx context.Context, database, archivePath string) error
	RunChecks(ctx context.Context, database string) ([]backup.Check, error)
	SwapLive(ctx context.Context, liveName, scratchName, previousName string) error
}

// poolSwapper is the one thing the restore's live-swap step needs from the
// application's own database pool: let go of it, then reconnect. *pg.DB
// satisfies this; nothing here imports platform/pg to say so, because nothing
// here needs to know it is PostgreSQL underneath — only that it can be let go
// of and picked back up.
type poolSwapper interface {
	Drain(ctx context.Context) error
	Reopen(ctx context.Context) error
}

// maintenanceGate is the one thing the restore's live-swap step needs from
// the HTTP edge: a way to say "not now" to every request while the pool is
// briefly gone. *httpx.MaintenanceGate satisfies this; this package does not
// import httpx, the same reasoning as poolSwapper.
type maintenanceGate interface {
	Enter(reason string)
	Exit()
}

// BackupServiceConfig names the one live database and the one directory this
// service is allowed to touch.
type BackupServiceConfig struct {
	// Dir is where backup artifacts are written. Created if it does not
	// exist.
	Dir string
	// LiveDatabase is the database name the application pool is configured
	// against. Backups are taken of it; a successful restore ends with the
	// scratch database renamed into this same name, so nothing else in the
	// running process ever needs to learn a new name.
	LiveDatabase string
	// RetentionMinKeep bounds how aggressively retention prunes: whatever the
	// configured count, at least this many verified manual/automatic backups
	// survive regardless of age, so a system nobody has looked at for a month
	// is never left with zero.
	RetentionMinKeep int
}

func (c BackupServiceConfig) withDefaults() BackupServiceConfig {
	if c.RetentionMinKeep <= 0 {
		c.RetentionMinKeep = 3
	}
	return c
}

// BackupService is Backup & Restore end to end: creating a backup, listing
// history, exporting and importing files, and the six-step safe restore —
// safety copy, scratch restore, verification, and only then the live swap.
//
// It never calls pg_dump or pg_restore itself; that is dumpRunner. What it
// owns is the sequence and the record of it: every step here either advances
// a row in port.BackupRepository or stops before the live database is
// touched, and a restore that stops always leaves the safety backup behind it.
type BackupService struct {
	deps    Deps
	auditor auditor
	store   port.BackupRepository
	runner  dumpRunner
	pool    poolSwapper
	gate    maintenanceGate
	cfg     BackupServiceConfig
}

// NewBackupService wires the service. gate may be nil — a test exercising the
// dump/restore mechanics without a real HTTP edge — in which case the restore
// flow simply never pauses traffic, which is correct for a process with no
// traffic to pause.
func NewBackupService(
	d Deps, store port.BackupRepository, runner dumpRunner, pool poolSwapper, gate maintenanceGate, cfg BackupServiceConfig,
) *BackupService {
	return &BackupService{
		deps:    d,
		auditor: newAuditor(d.Audit, d.Clock),
		store:   store,
		runner:  runner,
		pool:    pool,
		gate:    gate,
		cfg:     cfg.withDefaults(),
	}
}

// Dir is where backup artifacts live. The HTTP layer needs it for exactly one
// thing: choosing a destination for an uploaded file before ImportBackup ever
// sees it, so a client-supplied filename never becomes a filesystem path.
func (s *BackupService) Dir() string { return s.cfg.Dir }

func (s *BackupService) requireAdmin(actor shared.Actor, operation string) error {
	if actor.IsSystem() {
		return nil
	}
	return actor.RequireAnyRole(operation, shared.RoleAdmin)
}

func (s *BackupService) now() time.Time { return nowOr(s.deps.Clock) }

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateBackup takes a backup of the live database.
//
// It never returns a bare error for a dump that failed midway — that failure
// belongs on the row, where History shows it, not on the HTTP response. An
// error return here means the request itself could not be honoured: the
// actor lacked authority, or the metadata store itself is unreachable.
func (s *BackupService) CreateBackup(ctx context.Context, actor shared.Actor, kind port.BackupKind) (*port.BackupRun, error) {
	if err := s.requireAdmin(actor, "CreateBackup"); err != nil {
		return nil, err
	}

	run := &port.BackupRun{
		ID:        shared.NewID(),
		Kind:      kind,
		Status:    port.BackupRunning,
		StartedAt: s.now(),
	}
	if actor.UserID != shared.NilID {
		id := actor.UserID
		run.CreatedBy = &id
	}
	if err := s.store.CreateBackup(ctx, run); err != nil {
		return nil, err
	}

	s.runDump(ctx, run)

	if err := s.store.FinishBackup(ctx, run); err != nil {
		return run, err
	}

	s.auditor.record(ctx, port.AuditEntry{
		EntityType: "backup_run",
		EntityID:   &run.ID,
		Action:     "backup.create",
		Actor:      actor,
		OccurredAt: s.now(),
		Metadata: map[string]any{
			"kind": string(run.Kind), "status": string(run.Status),
			"bytes": run.Bytes, "verified": run.Verified,
		},
	})

	if run.Status == port.BackupVerified && (kind == port.BackupManual || kind == port.BackupAutomatic) {
		s.sweepRetention(ctx)
	}
	return run, nil
}

// runDump performs the dump and verification, filling in run in place. Kept
// separate from CreateBackup so the restore flow's own safety backup can
// share it without going through the authority check a second time.
func (s *BackupService) runDump(ctx context.Context, run *port.BackupRun) {
	finished := s.now()
	run.FinishedAt = &finished

	destPath := filepath.Join(s.cfg.Dir, fmt.Sprintf("%s-%s-%s.dump",
		s.cfg.LiveDatabase, run.StartedAt.UTC().Format("20060102T150405Z"), shortID(run.ID)))

	result, err := s.runner.Dump(ctx, s.cfg.LiveDatabase, destPath)
	if err != nil {
		run.Status = port.BackupFailed
		run.Error = err.Error()
		return
	}

	if err := s.runner.VerifyDump(ctx, destPath); err != nil {
		run.Status = port.BackupFailed
		run.Error = fmt.Sprintf("the file was written but did not verify: %v", err)
		return
	}

	run.FilePath = destPath
	run.Bytes = result.Bytes
	run.SHA256 = result.SHA256
	run.SchemaVersion = result.SchemaVersion
	run.ServerVersion = result.ServerVersion
	run.RowCounts = result.RowCounts
	run.Verified = true
	run.Status = port.BackupVerified
	finished = s.now()
	run.FinishedAt = &finished
}

// ---------------------------------------------------------------------------
// List, delete
// ---------------------------------------------------------------------------

func (s *BackupService) ListBackups(ctx context.Context, actor shared.Actor, limit int) ([]*port.BackupRun, error) {
	if err := s.requireAdmin(actor, "ListBackups"); err != nil {
		return nil, err
	}
	return s.store.ListBackups(ctx, limit)
}

func (s *BackupService) ListRestores(ctx context.Context, actor shared.Actor, limit int) ([]*port.RestoreRun, error) {
	if err := s.requireAdmin(actor, "ListRestores"); err != nil {
		return nil, err
	}
	return s.store.ListRestores(ctx, limit)
}

// DeleteBackup removes a backup file and its row. Refused for a safety backup
// still guarding an unfinished restore, and for anything not yet finished —
// deleting a running backup out from under itself is not a case worth
// handling, it is a case worth refusing.
func (s *BackupService) DeleteBackup(ctx context.Context, actor shared.Actor, id shared.ID) error {
	if err := s.requireAdmin(actor, "DeleteBackup"); err != nil {
		return err
	}
	run, err := s.store.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if run.Status == port.BackupRunning {
		return shared.PreconditionFailed("backup.still_running",
			"this backup has not finished yet")
	}
	if run.FilePath != "" {
		if err := os.Remove(run.FilePath); err != nil && !os.IsNotExist(err) {
			return shared.Internal("backup.delete_file", err, "deleting the backup file")
		}
	}
	if err := s.store.DeleteBackup(ctx, id); err != nil {
		return err
	}
	s.auditor.record(ctx, port.AuditEntry{
		EntityType: "backup_run", EntityID: &id, Action: "backup.delete", Actor: actor, OccurredAt: s.now(),
		Metadata: map[string]any{"kind": string(run.Kind)},
	})
	return nil
}

// ---------------------------------------------------------------------------
// Export, import
// ---------------------------------------------------------------------------

// ExportBackup authorises and returns the artifact a handler streams to the
// browser. It does not read the file itself — the handler owns the HTTP
// response boundary — but a caller must go through here rather than reading
// backup_run directly, so an export is audited like any other admin act on
// this data.
func (s *BackupService) ExportBackup(ctx context.Context, actor shared.Actor, id shared.ID) (*port.BackupRun, error) {
	if err := s.requireAdmin(actor, "ExportBackup"); err != nil {
		return nil, err
	}
	run, err := s.store.GetBackup(ctx, id)
	if err != nil {
		return nil, err
	}
	if !run.Verified {
		return nil, shared.PreconditionFailed("backup.not_verified",
			"this backup was never verified and cannot be exported")
	}
	s.auditor.record(ctx, port.AuditEntry{
		EntityType: "backup_run", EntityID: &id, Action: "backup.export", Actor: actor, OccurredAt: s.now(),
	})
	return run, nil
}

// ImportBackup registers an uploaded file as a backup.
//
// path is already inside cfg.Dir under a name the handler generated — never
// the client's own filename — so nothing here has to defend against path
// traversal; that defence lives at the one place a client-controlled string
// could otherwise reach the filesystem.
//
// Compatibility with the current schema is deliberately not decided here.
// This only proves the file is a readable archive of this system's tables; a
// backup made by a newer build than this one, or an older one, is refused
// later, at Restore, before a scratch database is created — which is where
// that question actually has an answer to check against.
func (s *BackupService) ImportBackup(
	ctx context.Context, actor shared.Actor, path string, sha256IfKnown string,
) (*port.BackupRun, error) {
	if err := s.requireAdmin(actor, "ImportBackup"); err != nil {
		return nil, err
	}

	run := &port.BackupRun{
		ID:        shared.NewID(),
		Kind:      port.BackupImported,
		Status:    port.BackupRunning,
		StartedAt: s.now(),
	}
	if actor.UserID != shared.NilID {
		id := actor.UserID
		run.CreatedBy = &id
	}
	if err := s.store.CreateBackup(ctx, run); err != nil {
		return nil, err
	}

	finished := s.now()
	run.FinishedAt = &finished

	if err := s.runner.VerifyDump(ctx, path); err != nil {
		run.Status = port.BackupFailed
		run.Error = fmt.Sprintf("this is not a readable backup file: %v", err)
	} else if sum, err := backup.Checksum(path); err != nil {
		run.Status = port.BackupFailed
		run.Error = fmt.Sprintf("could not verify the file's integrity: %v", err)
	} else if sha256IfKnown != "" && sum != sha256IfKnown {
		run.Status = port.BackupFailed
		run.Error = "the file's checksum does not match the one recorded when it was exported; it may be corrupted or incomplete"
	} else {
		info, statErr := os.Stat(path)
		if statErr != nil {
			run.Status = port.BackupFailed
			run.Error = fmt.Sprintf("could not read the uploaded file: %v", statErr)
		} else {
			run.FilePath = path
			run.Bytes = info.Size()
			run.SHA256 = sum
			run.Verified = true
			run.Status = port.BackupVerified
		}
	}

	if err := s.store.FinishBackup(ctx, run); err != nil {
		return run, err
	}
	s.auditor.record(ctx, port.AuditEntry{
		EntityType: "backup_run", EntityID: &run.ID, Action: "backup.import", Actor: actor, OccurredAt: s.now(),
		Metadata: map[string]any{"status": string(run.Status)},
	})
	return run, nil
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

// Restore is the whole safe-restore flow, in the order it must run:
//
//  1. Refuse up front if the backup was never verified, or is newer than this
//     build's schema — questions that have an answer before anything is
//     created.
//  2. Take a safety backup of the live database. Stop here if it fails.
//  3. Create a scratch database and restore the selected backup into it. The
//     live database is untouched through this entire step.
//  4. Run the same reconciliation checks restore-drill.sh has always run,
//     against the scratch database. Any failure drops the scratch database,
//     leaves the live database exactly as it was, and keeps the safety
//     backup.
//  5. Only once every check passes: close the maintenance gate, let go of the
//     application's own pool, rename the live database aside and the scratch
//     database into its place, and reopen the pool. The old database is kept
//     under its new name rather than dropped.
func (s *BackupService) Restore(ctx context.Context, actor shared.Actor, backupID shared.ID) (*port.RestoreRun, error) {
	if err := s.requireAdmin(actor, "Restore"); err != nil {
		return nil, err
	}

	target, err := s.store.GetBackup(ctx, backupID)
	if err != nil {
		return nil, err
	}
	if !target.Verified {
		return nil, shared.PreconditionFailed("restore.not_verified",
			"this backup was never verified and cannot be restored")
	}

	if current, err := s.runner.CurrentSchemaVersion(ctx, s.cfg.LiveDatabase); err == nil {
		if target.SchemaVersion > current {
			return nil, shared.PreconditionFailed("restore.newer_schema",
				"this backup was made by a newer version of the application (schema %d; this build is at %d)",
				target.SchemaVersion, current)
		}
	}

	restore := &port.RestoreRun{
		ID:          shared.NewID(),
		BackupRunID: backupID,
		Status:      port.RestoreRunning,
		StartedAt:   s.now(),
	}
	if actor.UserID != shared.NilID {
		id := actor.UserID
		restore.CreatedBy = &id
	}
	if err := s.store.CreateRestore(ctx, restore); err != nil {
		return nil, err
	}

	s.runRestore(ctx, actor, target, restore)

	if err := s.store.UpdateRestore(ctx, restore); err != nil {
		return restore, err
	}
	s.auditor.record(ctx, port.AuditEntry{
		EntityType: "restore_run", EntityID: &restore.ID, Action: "restore.attempt", Actor: actor, OccurredAt: s.now(),
		Metadata: map[string]any{
			"backup_run_id": backupID.String(), "status": string(restore.Status),
		},
	})
	return restore, nil
}

// runRestore drives the state machine. Every early return leaves restore.Status
// set to Failed with a reason and finished — the caller persists it and stops.
func (s *BackupService) runRestore(
	ctx context.Context, actor shared.Actor, target *port.BackupRun, restore *port.RestoreRun,
) {
	fail := func(format string, args ...any) {
		restore.Status = port.RestoreFailed
		restore.Error = fmt.Sprintf(format, args...)
		finished := s.now()
		restore.FinishedAt = &finished
	}

	// Step 1: safety backup of the live database. Nothing about the restore
	// target has been touched yet, so a failure here costs nothing but time.
	safety, err := s.CreateBackup(ctx, actor, port.BackupSafety)
	if err != nil {
		fail("could not even start a safety backup: %v", err)
		return
	}
	restore.SafetyBackupID = &safety.ID
	_ = s.store.UpdateRestore(ctx, restore)
	if safety.Status != port.BackupVerified {
		fail("the safety backup of the live database failed (%s); restore stopped before touching anything", safety.Error)
		return
	}

	// Step 2: restore into a scratch database. The live database is still
	// untouched.
	scratchName := fmt.Sprintf("%s_restore_%s", s.cfg.LiveDatabase, shortID(restore.ID))
	if err := s.runner.CreateScratchDatabase(ctx, scratchName); err != nil {
		fail("could not create a scratch database to restore into: %v", err)
		return
	}
	if err := s.runner.RestoreInto(ctx, scratchName, target.FilePath); err != nil {
		_ = s.runner.DropDatabase(ctx, scratchName)
		fail("restoring into the scratch database failed: %v", err)
		return
	}

	// Step 3: the checks that gate a swap. Still nothing live touched.
	restore.Status = port.RestoreChecking
	_ = s.store.UpdateRestore(ctx, restore)

	checks, err := s.runner.RunChecks(ctx, scratchName)
	if err != nil {
		_ = s.runner.DropDatabase(ctx, scratchName)
		fail("could not run the verification checks: %v", err)
		return
	}
	restore.Checks = toPortChecks(checks)
	if !restore.AllChecksPassed() {
		_ = s.runner.DropDatabase(ctx, scratchName)
		fail("the restored data failed verification; the live database was not touched. %s",
			firstFailure(restore.Checks))
		return
	}

	// Step 4: the swap. This is the one window where the outcome is not yet
	// certain either way, which is exactly why everything before it was
	// designed to fail before reaching here.
	restore.Status = port.RestoreSwapping
	_ = s.store.UpdateRestore(ctx, restore)

	s.gate.Enter("restoring a backup")
	defer s.gate.Exit()

	if err := s.pool.Drain(ctx); err != nil {
		_ = s.runner.DropDatabase(ctx, scratchName)
		fail("could not release the database connection to perform the swap: %v", err)
		return
	}

	previousName := fmt.Sprintf("%s_previous_%s", s.cfg.LiveDatabase, shortID(restore.ID))
	swapErr := s.runner.SwapLive(ctx, s.cfg.LiveDatabase, scratchName, previousName)

	// Reopen unconditionally: on success the pool must point at the restored
	// data, and on failure SwapLive has already put the live database back
	// under its original name, so the pool must point at that instead. Either
	// way the process must not be left holding no pool at all.
	reopenErr := reopenWithRetry(ctx, s.pool, 3, time.Second)

	switch {
	case swapErr != nil:
		fail("the database swap failed and was rolled back; the live database is unchanged: %v", swapErr)
	case reopenErr != nil:
		// The rename succeeded; only reconnecting failed. The data the
		// operator asked for is in place under the live name, but this
		// process cannot see it — restarting the server, not retrying the
		// restore, is what finishes this.
		restore.Status = port.RestoreFailed
		restore.Error = fmt.Sprintf(
			"the database was restored successfully but the application could not reconnect (%v); restart the server", reopenErr)
		finished := s.now()
		restore.FinishedAt = &finished
	default:
		// The swap just replaced backup_run and restore_run themselves with
		// the snapshot's older copy of that data — a full-database restore
		// rewinds its own bookkeeping exactly like it rewinds everything
		// else. Every backup and restore older than this one is gone from
		// history from here on (their files are untouched on disk; only the
		// index of them went back in time). What is re-registered here is
		// the two rows an operator needs to find their way back if this
		// restore itself turns out to be wrong: the backup just restored, and
		// the safety copy taken before it. CreatedBy is dropped on both — the
		// user who made them may not exist in the restored snapshot either,
		// and a foreign-key failure here must not turn a successful restore
		// into a failed one over an attribution field.
		reregisterAfterSwap(ctx, s.store, s.deps, target)
		reregisterAfterSwap(ctx, s.store, s.deps, safety)
		// Same reasoning as reregisterAfterSwap: this row's own insert branch
		// needs created_by to reference a user who exists in the database it
		// is now landing in, which the actor performing the restore is not
		// guaranteed to be.
		restore.CreatedBy = nil

		restore.Status = port.RestoreRestored
		restore.PreviousDatabase = previousName
		finished := s.now()
		restore.FinishedAt = &finished
	}
}

// reregisterAfterSwap writes a backup's row into whatever database is live
// right now. Called only after a successful swap, on a copy with CreatedBy
// cleared, so a missing app_user row in the restored snapshot cannot turn a
// completed restore into a reported failure over a foreign key on an
// attribution field nobody is blocked on.
func reregisterAfterSwap(ctx context.Context, store port.BackupRepository, deps Deps, run *port.BackupRun) {
	if run == nil {
		return
	}
	clone := *run
	clone.CreatedBy = nil
	if err := store.FinishBackup(ctx, &clone); err != nil {
		deps.Log.Warn("could not re-register a backup after a restore's live swap",
			slog.String("backup_id", run.ID.String()), slog.String("error", err.Error()))
	}
}

func reopenWithRetry(ctx context.Context, pool poolSwapper, attempts int, wait time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = pool.Reopen(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

func toPortChecks(checks []backup.Check) []port.RestoreCheck {
	out := make([]port.RestoreCheck, len(checks))
	for i, c := range checks {
		out[i] = port.RestoreCheck{Name: c.Name, Passed: c.Passed, Detail: c.Detail}
	}
	return out
}

func firstFailure(checks []port.RestoreCheck) string {
	for _, c := range checks {
		if !c.Passed {
			if c.Detail != "" {
				return fmt.Sprintf("%s: %s", c.Name, c.Detail)
			}
			return c.Name
		}
	}
	return ""
}

func shortID(id shared.ID) string {
	s := id.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

func (s *BackupService) GetSchedule(ctx context.Context, actor shared.Actor) (*port.BackupSchedule, error) {
	if err := s.requireAdmin(actor, "GetBackupSchedule"); err != nil {
		return nil, err
	}
	return s.store.GetSchedule(ctx)
}

// UpdateSchedule replaces the automatic-backup policy. Read by the scheduler
// job on its own ticker, so a change here takes effect on the next tick with
// no restart.
func (s *BackupService) UpdateSchedule(
	ctx context.Context, actor shared.Actor, enabled bool, intervalHours, retentionCount int,
) (*port.BackupSchedule, error) {
	if err := s.requireAdmin(actor, "UpdateBackupSchedule"); err != nil {
		return nil, err
	}
	if intervalHours <= 0 {
		return nil, shared.Validation("backup.schedule.invalid_interval", "the interval must be at least one hour")
	}
	if retentionCount < 1 {
		return nil, shared.Validation("backup.schedule.invalid_retention", "at least one backup must be kept")
	}

	sched := &port.BackupSchedule{
		Enabled:        enabled,
		IntervalHours:  intervalHours,
		RetentionCount: retentionCount,
		UpdatedAt:      s.now(),
	}
	if actor.UserID != shared.NilID {
		id := actor.UserID
		sched.UpdatedBy = &id
	}
	// last_run_at is preserved rather than reset, so turning the schedule off
	// and back on does not make an overdue backup run immediately for no
	// reason beyond the toggle.
	if existing, err := s.store.GetSchedule(ctx); err == nil {
		sched.LastRunAt = existing.LastRunAt
	}
	if err := s.store.UpdateSchedule(ctx, sched); err != nil {
		return nil, err
	}
	s.auditor.record(ctx, port.AuditEntry{
		EntityType: "backup_schedule", Action: "backup.schedule.update", Actor: actor, OccurredAt: s.now(),
		Metadata: map[string]any{
			"enabled": enabled, "interval_hours": intervalHours, "retention_count": retentionCount,
		},
	})
	return sched, nil
}

// RunScheduledBackup is what the scheduler job calls. It runs an automatic
// backup if the policy says one is due, then sweeps retention regardless —
// retention is a property of what already exists, not of whether this tick
// happened to create something new.
func (s *BackupService) RunScheduledBackup(ctx context.Context) (ran bool, err error) {
	sched, err := s.store.GetSchedule(ctx)
	if err != nil {
		return false, err
	}
	now := s.now()
	if !sched.Due(now) {
		return false, nil
	}

	if _, err := s.CreateBackup(ctx, shared.SystemActor(), port.BackupAutomatic); err != nil {
		return false, err
	}

	sched.LastRunAt = &now
	sched.UpdatedAt = now
	if err := s.store.UpdateSchedule(ctx, sched); err != nil {
		return true, err
	}
	return true, nil
}

// sweepRetention deletes verified manual/automatic backups beyond the
// configured count, oldest first, never below RetentionMinKeep regardless of
// the configured count — a system quiet for a month must not be left with
// nothing. Safety and imported backups are never swept here: a safety backup
// is cleaned up, if ever, by whoever finishes investigating the restore it
// guarded, and an imported one was placed by a deliberate act this sweep has
// no context to second-guess.
func (s *BackupService) sweepRetention(ctx context.Context) {
	sched, err := s.store.GetSchedule(ctx)
	if err != nil {
		s.deps.Log.Warn("could not read backup retention policy", slog.String("error", err.Error()))
		return
	}
	keep := max(sched.RetentionCount, s.cfg.RetentionMinKeep)

	all, err := s.store.ListBackups(ctx, 10000)
	if err != nil {
		s.deps.Log.Warn("could not list backups for retention", slog.String("error", err.Error()))
		return
	}

	var eligible []*port.BackupRun
	for _, b := range all {
		if b.Verified && (b.Kind == port.BackupManual || b.Kind == port.BackupAutomatic) {
			eligible = append(eligible, b)
		}
	}
	// ListBackups already orders newest first.
	if len(eligible) <= keep {
		return
	}
	for _, old := range eligible[keep:] {
		if old.FilePath != "" {
			if err := os.Remove(old.FilePath); err != nil && !os.IsNotExist(err) {
				s.deps.Log.Warn("retention could not remove a backup file",
					slog.String("path", old.FilePath), slog.String("error", err.Error()))
				continue
			}
		}
		if err := s.store.DeleteBackup(ctx, old.ID); err != nil {
			s.deps.Log.Warn("retention could not remove a backup row",
				slog.String("id", old.ID.String()), slog.String("error", err.Error()))
		}
	}
}
