package port

import (
	"context"
	"time"

	"flowed/internal/domain/shared"
)

// BackupKind names why a backup exists.
type BackupKind string

const (
	// BackupManual was taken by an operator clicking the button.
	BackupManual BackupKind = "manual"
	// BackupAutomatic was taken by the scheduler on the configured interval.
	BackupAutomatic BackupKind = "automatic"
	// BackupSafety was taken of the live database immediately before a
	// restore attempt touched it. Never deleted by retention while the
	// restore_run it guards has not finished, and never offered as the
	// backup a UI action deletes without the operator naming it specifically.
	BackupSafety BackupKind = "safety"
	// BackupImported was registered from a file uploaded through the UI
	// rather than produced on this host.
	BackupImported BackupKind = "imported"
)

// BackupStatus is where one backup artifact stands.
type BackupStatus string

const (
	BackupRunning  BackupStatus = "running"
	BackupVerified BackupStatus = "verified"
	BackupFailed   BackupStatus = "failed"
)

// BackupRun is one backup artifact: a single .dump file and what is known
// about it.
//
// Its own status never changes once it leaves "running" — verified or failed
// is a fact about the file at the moment it was produced. Using it in a
// restore is a separate event, recorded on RestoreRun, so restoring the same
// file ten times does not rewrite this row ten times.
type BackupRun struct {
	ID         shared.ID
	Kind       BackupKind
	Status     BackupStatus
	StartedAt  time.Time
	FinishedAt *time.Time

	FilePath string
	Bytes    int64
	SHA256   string

	SchemaVersion int64
	ServerVersion string
	RowCounts     map[string]int64

	Verified bool
	Error    string

	CreatedBy *shared.ID
}

// RestoreStatus is where one restore attempt stands.
type RestoreStatus string

const (
	// RestoreRunning covers the safety backup and the scratch-database
	// restore. The live database has not been touched.
	RestoreRunning RestoreStatus = "running"
	// RestoreChecking covers the reconciliation checks against the scratch
	// database. The live database has not been touched.
	RestoreChecking RestoreStatus = "checking"
	// RestoreSwapping covers the live-database swap itself: the one window
	// where the outcome is not yet certain either way.
	RestoreSwapping RestoreStatus = "swapping"
	// RestoreRestored means the swap completed; the app is serving the
	// restored data.
	RestoreRestored RestoreStatus = "restored"
	// RestoreFailed means the attempt stopped, at any stage before or during
	// swapping; the live database was never left in a half-changed state.
	RestoreFailed RestoreStatus = "failed"
)

// RestoreCheck is the result of one gate a restore must pass before it may
// touch the live database.
type RestoreCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// RestoreRun is one attempt to make a backup the live database.
type RestoreRun struct {
	ID               shared.ID
	BackupRunID      shared.ID
	SafetyBackupID   *shared.ID
	Status           RestoreStatus
	StartedAt        time.Time
	FinishedAt       *time.Time
	Checks           []RestoreCheck
	PreviousDatabase string
	Error            string
	CreatedBy        *shared.ID
}

// AllChecksPassed reports whether every recorded check passed. False on an
// empty slice: a restore that never ran the checks is not one that passed
// them.
func (r *RestoreRun) AllChecksPassed() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.Passed {
			return false
		}
	}
	return true
}

// BackupSchedule is the automatic-backup policy. There is exactly one.
type BackupSchedule struct {
	Enabled        bool
	IntervalHours  int
	RetentionCount int
	LastRunAt      *time.Time
	UpdatedAt      time.Time
	UpdatedBy      *shared.ID
}

// Due reports whether an automatic backup should run now, given when the last
// one started.
func (s BackupSchedule) Due(now time.Time) bool {
	if !s.Enabled {
		return false
	}
	if s.LastRunAt == nil {
		return true
	}
	return now.Sub(*s.LastRunAt) >= time.Duration(s.IntervalHours)*time.Hour
}

// BackupRepository stores backup and restore metadata.
//
// It does not run pg_dump or pg_restore itself — that is backup.Runner, in the
// adapter layer, the one place in this codebase that shells out. This
// interface only ever sees the facts a run produced: paths, sizes, checksums,
// check results. Keeping the boundary there is what lets the app-layer
// service be tested against a fake runner without a real database dump.
type BackupRepository interface {
	CreateBackup(ctx context.Context, run *BackupRun) error
	FinishBackup(ctx context.Context, run *BackupRun) error
	GetBackup(ctx context.Context, id shared.ID) (*BackupRun, error)
	ListBackups(ctx context.Context, limit int) ([]*BackupRun, error)
	// DeleteBackup removes the metadata row. The caller deletes the file
	// first; a row surviving a missing file would show the operator a backup
	// they cannot actually restore.
	DeleteBackup(ctx context.Context, id shared.ID) error

	CreateRestore(ctx context.Context, run *RestoreRun) error
	UpdateRestore(ctx context.Context, run *RestoreRun) error
	GetRestore(ctx context.Context, id shared.ID) (*RestoreRun, error)
	ListRestores(ctx context.Context, limit int) ([]*RestoreRun, error)

	GetSchedule(ctx context.Context) (*BackupSchedule, error)
	UpdateSchedule(ctx context.Context, s *BackupSchedule) error
}
