package httpapi

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/app"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/port"
)

// BackupHandlers serve Backup & Restore: creating, listing, restoring,
// exporting, importing, and scheduling backups.
//
// Every route here requires RoleAdmin. Nothing in this file ever puts a
// filesystem path, a database name, or a connection string in a response —
// what a client sees is an id, a size, a status, and a plain-language reason
// when something failed.
type BackupHandlers struct {
	Service *app.BackupService
}

// NewBackupHandlers wires the routes to the service.
func NewBackupHandlers(service *app.BackupService) *BackupHandlers {
	return &BackupHandlers{Service: service}
}

// Register mounts /backups on the group it is given.
func (h *BackupHandlers) Register(g *gin.RouterGroup) {
	group := g.Group("/backups")

	group.GET("", h.List)
	group.POST("", h.Create)
	group.GET("/:id/export", h.Export)
	group.DELETE("/:id", h.Delete)
	group.POST("/import", h.Import)

	group.GET("/restores", h.ListRestores)
	group.POST("/:id/restore", h.Restore)

	group.GET("/schedule", h.GetSchedule)
	group.PUT("/schedule", h.UpdateSchedule)
}

// Create takes a manual backup.
func (h *BackupHandlers) Create(c *gin.Context) {
	run, err := h.Service.CreateBackup(requestContext(c), httpx.MustActor(c), port.BackupManual)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toBackupView(run))
}

// List returns backup history, newest first.
func (h *BackupHandlers) List(c *gin.Context) {
	runs, err := h.Service.ListBackups(requestContext(c), httpx.MustActor(c), limitOr(c, 100))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]BackupView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toBackupView(run))
	}
	httpx.OK(c, views)
}

// ListRestores returns restore attempts, newest first.
func (h *BackupHandlers) ListRestores(c *gin.Context) {
	runs, err := h.Service.ListRestores(requestContext(c), httpx.MustActor(c), limitOr(c, 100))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	views := make([]RestoreView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toRestoreView(run))
	}
	httpx.OK(c, views)
}

// Delete removes a backup file and its record.
func (h *BackupHandlers) Delete(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.Service.DeleteBackup(requestContext(c), httpx.MustActor(c), id); err != nil {
		httpx.Respond(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Restore runs the safe-restore flow against the selected backup: a safety
// copy of the live database first, a scratch restore and verification next,
// and only on success the live swap. It always returns 200 with the attempt's
// outcome recorded on it — a refused or failed restore is not a transport
// error, it is the answer, and the live database was never touched to produce it.
func (h *BackupHandlers) Restore(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	run, err := h.Service.Restore(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toRestoreView(run))
}

// Export streams a backup as a single downloadable file: the archive plus a
// manifest, zipped together, so it can be copied to a USB drive and imported
// on another computer running the same application without anything on the
// wire ever naming a database or a filesystem path.
func (h *BackupHandlers) Export(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	run, err := h.Service.ExportBackup(requestContext(c), httpx.MustActor(c), id)
	if err != nil {
		httpx.Respond(c, err)
		return
	}

	file, err := os.Open(run.FilePath)
	if err != nil {
		httpx.Respond(c, shared.Internal("backup.export_open", err, "opening the backup file"))
		return
	}
	defer file.Close()

	name := fmt.Sprintf("backup-%s.zip", run.StartedAt.UTC().Format("20060102-150405"))
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	c.Status(http.StatusOK)

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	manifest, _ := json.MarshalIndent(toManifest(run), "", "  ")
	if mw, err := zw.Create("manifest.json"); err == nil {
		_, _ = mw.Write(manifest)
	}
	dw, err := zw.Create("backup.dump")
	if err != nil {
		_ = c.Error(err)
		return
	}
	if _, err := io.Copy(dw, file); err != nil {
		_ = c.Error(err)
	}
}

// Import accepts an uploaded backup — either the zip Export produces, or a
// bare .dump file — validates it, and registers it in history with a Restore
// button, exactly like one produced on this host.
func (h *BackupHandlers) Import(c *gin.Context) {
	header, err := c.FormFile("file")
	if err != nil {
		httpx.Respond(c, shared.Validation("backup.import.file_required",
			"attach the backup file as a multipart field named \"file\"").WithCause(err))
		return
	}

	stagingID := shared.NewID()
	staged := filepath.Join(h.Service.Dir(), "upload-"+stagingID.String())
	if err := c.SaveUploadedFile(header, staged); err != nil {
		httpx.Respond(c, shared.Internal("backup.import.save", err, "saving the uploaded file"))
		return
	}

	finalPath := staged
	expectedSHA := ""
	if strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		finalPath, expectedSHA, err = unpackBackupZip(staged, h.Service.Dir(), stagingID.String())
		os.Remove(staged)
		if err != nil {
			httpx.Respond(c, shared.Validation("backup.import.invalid_zip",
				"could not read this file as an exported backup: %v", err))
			return
		}
	}

	run, err := h.Service.ImportBackup(requestContext(c), httpx.MustActor(c), finalPath, expectedSHA)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.Created(c, toBackupView(run))
}

// unpackBackupZip extracts the dump entry (and reads the checksum from
// manifest.json when present) from a zip Export produced, into dir under a
// name this process chose — never anything read from the archive's own entry
// names, which is exactly the kind of client-controlled string path traversal
// defences exist for.
func unpackBackupZip(zipPath, dir, name string) (dumpPath, expectedSHA string, err error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", "", err
	}
	defer zr.Close()

	dumpPath = filepath.Join(dir, "imported-"+name+".dump")
	var foundDump bool
	for _, f := range zr.File {
		switch {
		case strings.EqualFold(filepath.Base(f.Name), "backup.dump"):
			if err := extractZipEntry(f, dumpPath); err != nil {
				return "", "", err
			}
			foundDump = true
		case strings.EqualFold(filepath.Base(f.Name), "manifest.json"):
			rc, err := f.Open()
			if err != nil {
				continue
			}
			var m backupManifest
			_ = json.NewDecoder(rc).Decode(&m)
			rc.Close()
			expectedSHA = m.SHA256
		}
	}
	if !foundDump {
		return "", "", fmt.Errorf("no backup.dump entry found in the archive")
	}
	return dumpPath, expectedSHA, nil
}

func extractZipEntry(f *zip.File, dest string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, src)
	return err
}

// GetSchedule returns the automatic-backup policy.
func (h *BackupHandlers) GetSchedule(c *gin.Context) {
	sched, err := h.Service.GetSchedule(requestContext(c), httpx.MustActor(c))
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toScheduleView(sched))
}

// UpdateSchedule replaces the automatic-backup policy.
func (h *BackupHandlers) UpdateSchedule(c *gin.Context) {
	var req UpdateBackupScheduleRequest
	if !bindJSON(c, &req) {
		return
	}
	sched, err := h.Service.UpdateSchedule(requestContext(c), httpx.MustActor(c),
		req.Enabled, req.IntervalHours, req.RetentionCount)
	if err != nil {
		httpx.Respond(c, err)
		return
	}
	httpx.OK(c, toScheduleView(sched))
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// BackupView is one backup artifact, as a non-technical operator reads it:
// when, how big, what kind, and whether it can be trusted. No file path, no
// database name.
type BackupView struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Bytes      int64      `json:"bytes"`
	Verified   bool       `json:"verified"`
	Error      string     `json:"error,omitempty"`
}

// RestoreView is one restore attempt: which backup, what it checked, and
// where it landed.
type RestoreView struct {
	ID         string             `json:"id"`
	BackupID   string             `json:"backup_id"`
	SafetyID   string             `json:"safety_backup_id,omitempty"`
	Status     string             `json:"status"`
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
	Checks     []RestoreCheckView `json:"checks,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// RestoreCheckView is one named pass/fail from the verification a restore ran
// before it was allowed to touch anything live.
type RestoreCheckView struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// UpdateBackupScheduleRequest sets the automatic-backup policy in the terms a
// non-technical operator sees: on or off, how many hours between backups, how
// many to keep.
type UpdateBackupScheduleRequest struct {
	Enabled        bool `json:"enabled"`
	IntervalHours  int  `json:"interval_hours" binding:"required"`
	RetentionCount int  `json:"retention_count" binding:"required"`
}

// BackupScheduleView is the automatic-backup policy as read back.
type BackupScheduleView struct {
	Enabled        bool       `json:"enabled"`
	IntervalHours  int        `json:"interval_hours"`
	RetentionCount int        `json:"retention_count"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
}

// backupManifest is what Export writes beside the dump and Import reads back.
// A sidecar rather than the sole record: the authoritative check on whether a
// restore is safe is always the scratch-database verification in
// app.BackupService.Restore, never this file.
type backupManifest struct {
	SHA256        string           `json:"sha256"`
	Bytes         int64            `json:"bytes"`
	SchemaVersion int64            `json:"schema_version"`
	ServerVersion string           `json:"server_version"`
	RowCounts     map[string]int64 `json:"row_counts,omitempty"`
	Kind          string           `json:"kind"`
	StartedAt     time.Time        `json:"started_at"`
}

func toManifest(run *port.BackupRun) backupManifest {
	return backupManifest{
		SHA256:        run.SHA256,
		Bytes:         run.Bytes,
		SchemaVersion: run.SchemaVersion,
		ServerVersion: run.ServerVersion,
		RowCounts:     run.RowCounts,
		Kind:          string(run.Kind),
		StartedAt:     run.StartedAt,
	}
}

func toBackupView(run *port.BackupRun) BackupView {
	return BackupView{
		ID:         run.ID.String(),
		Kind:       string(run.Kind),
		Status:     string(run.Status),
		StartedAt:  run.StartedAt,
		FinishedAt: run.FinishedAt,
		Bytes:      run.Bytes,
		Verified:   run.Verified,
		Error:      run.Error,
	}
}

func toRestoreView(run *port.RestoreRun) RestoreView {
	view := RestoreView{
		ID:         run.ID.String(),
		BackupID:   run.BackupRunID.String(),
		Status:     string(run.Status),
		StartedAt:  run.StartedAt,
		FinishedAt: run.FinishedAt,
		Error:      run.Error,
	}
	if run.SafetyBackupID != nil {
		view.SafetyID = run.SafetyBackupID.String()
	}
	for _, check := range run.Checks {
		view.Checks = append(view.Checks, RestoreCheckView{
			Name: check.Name, Passed: check.Passed, Detail: check.Detail,
		})
	}
	return view
}

func toScheduleView(s *port.BackupSchedule) BackupScheduleView {
	return BackupScheduleView{
		Enabled:        s.Enabled,
		IntervalHours:  s.IntervalHours,
		RetentionCount: s.RetentionCount,
		LastRunAt:      s.LastRunAt,
	}
}
