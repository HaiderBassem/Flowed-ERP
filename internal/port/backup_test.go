package port

import (
	"testing"
	"time"
)

func TestBackupScheduleDueDisabled(t *testing.T) {
	s := BackupSchedule{Enabled: false, IntervalHours: 24}
	if s.Due(time.Now()) {
		t.Fatal("a disabled schedule must never be due")
	}
}

func TestBackupScheduleDueNeverRunBefore(t *testing.T) {
	s := BackupSchedule{Enabled: true, IntervalHours: 24}
	if !s.Due(time.Now()) {
		t.Fatal("a schedule with no last run must be due immediately")
	}
}

func TestBackupScheduleDueBeforeInterval(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	last := now.Add(-23 * time.Hour)
	s := BackupSchedule{Enabled: true, IntervalHours: 24, LastRunAt: &last}
	if s.Due(now) {
		t.Fatal("a schedule inside its interval must not be due")
	}
}

func TestBackupScheduleDueAfterInterval(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	last := now.Add(-25 * time.Hour)
	s := BackupSchedule{Enabled: true, IntervalHours: 24, LastRunAt: &last}
	if !s.Due(now) {
		t.Fatal("a schedule past its interval must be due")
	}
}

func TestRestoreRunAllChecksPassedEmptyIsFalse(t *testing.T) {
	r := RestoreRun{}
	if r.AllChecksPassed() {
		t.Fatal("a restore that never ran a check must not report having passed them")
	}
}

func TestRestoreRunAllChecksPassed(t *testing.T) {
	r := RestoreRun{Checks: []RestoreCheck{{Name: "a", Passed: true}, {Name: "b", Passed: true}}}
	if !r.AllChecksPassed() {
		t.Fatal("all checks passed should report true")
	}
	r.Checks[1].Passed = false
	if r.AllChecksPassed() {
		t.Fatal("one failing check must make the whole thing false")
	}
}
