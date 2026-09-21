package app

import (
	"context"
	"fmt"

	"flowed/internal/adapter/csvdata"
	"flowed/internal/domain/shared"
	"flowed/internal/platform/httpx"
	"flowed/internal/platform/pg"
	"flowed/internal/port"
)

// DataExportService moves the whole system's data out as CSV and back in.
//
// It is not the backup service and does not replace it. A pg_dump is what a
// broken database is rebuilt from; this is what an office keeps on a memory
// stick, opens in Excel when somebody asks a question the reports do not
// answer, and loads onto a new machine. Both exist because neither does the
// other's job.
type DataExportService struct {
	deps        Deps
	db          *pg.DB
	maintenance *httpx.MaintenanceGate
	auditor
}

// NewDataExportService wires the CSV data commands.
func NewDataExportService(d Deps, db *pg.DB, maintenance *httpx.MaintenanceGate) *DataExportService {
	return &DataExportService{
		deps:        d,
		db:          db,
		maintenance: maintenance,
		auditor:     newAuditor(d.Audit, d.Clock),
	}
}

// ExportAll writes every table to a ZIP of CSV files and returns it with the
// filename the browser should save it under.
//
// The export is audited. It is the one operation that puts every student's name
// and telephone number into a file somebody can carry out of the building, and
// a system that cannot say who did that has no answer on the day it matters.
func (s *DataExportService) ExportAll(ctx context.Context, actor shared.Actor) ([]byte, string, error) {
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return nil, "", err
	}

	now := nowOr(s.deps.Clock)
	archive, err := csvdata.Export(ctx, s.db, version, now)
	if err != nil {
		return nil, "", err
	}

	// The entry is written in its own transaction after the archive exists.
	// Inside one that also held the COPY, the trail would claim an export that
	// a later failure rolled back — and the question this entry answers is who
	// took a file out of the building, which is about the file existing.
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return s.record(ctx, port.AuditEntry{
			EntityType: "data_export",
			Action:     "data.exported",
			Actor:      actor,
			OccurredAt: now,
			Metadata: map[string]any{
				"schema_version": version,
				"bytes":          len(archive),
			},
		})
	})
	if err != nil {
		return nil, "", err
	}

	return archive, fmt.Sprintf("flowed-data-%s.zip", now.Format("2006-01-02")), nil
}

// ImportAllResult reports what an import loaded.
type ImportAllResult struct {
	SchemaVersion int
	Tables        []csvdata.TableCount
	TotalRows     int64
}

// ImportAll replaces the contents of every table from an archive.
//
// The audit entry is written after the load, and it is the only entry that
// survives it: the import cleared the audit table along with everything else
// and then wrote the archive's own trail back in its place. Recording the
// import first would have recorded it into a table about to be emptied, and the
// system would keep no memory of the one operation that replaced all of its
// memory.
//
// The maintenance gate is closed for the duration. Every other request in
// flight would otherwise read a database mid-replacement, and a cashier's
// lookup that finds no student and then finds them again a second later is how
// a payment gets taken twice.
func (s *DataExportService) ImportAll(
	ctx context.Context, actor shared.Actor, archive []byte,
) (*ImportAllResult, error) {
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return nil, err
	}

	if s.maintenance != nil {
		s.maintenance.Enter("importing system data")
		defer s.maintenance.Exit()
	}

	manifest, err := csvdata.Import(ctx, s.db, archive, version)
	if err != nil {
		return nil, err
	}

	result := &ImportAllResult{SchemaVersion: manifest.SchemaVersion, Tables: manifest.Tables}
	for _, table := range manifest.Tables {
		result.TotalRows += table.Rows
	}

	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		return s.record(ctx, port.AuditEntry{
			EntityType: "data_import",
			Action:     "data.imported",
			Actor:      actor,
			OccurredAt: nowOr(s.deps.Clock),
			Metadata: map[string]any{
				"schema_version": manifest.SchemaVersion,
				"exported_at":    manifest.ExportedAt,
				"rows":           result.TotalRows,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// schemaVersion reads the highest applied migration.
//
// Read from the database rather than from the build, because that is the
// version the rows in front of us actually have. A binary can be newer than the
// database it is pointed at — that is what `api serve` refuses at boot — and an
// archive stamped with the binary's opinion would carry a version its contents
// were never in.
func (s *DataExportService) schemaVersion(ctx context.Context) (int, error) {
	var version *int64
	err := s.db.Conn(ctx).QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, pg.WrapQuery("data.schemaVersion", err)
	}
	if version == nil {
		return 0, shared.PreconditionFailed("data.no_schema",
			"this database has no applied migrations")
	}
	return int(*version), nil
}
