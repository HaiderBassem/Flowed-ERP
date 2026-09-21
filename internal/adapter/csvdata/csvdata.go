// Package csvdata moves the whole database in and out as CSV.
//
// It exists beside the pg_dump-based backup service rather than replacing it,
// and the two answer different questions. A dump is what you restore from: it
// carries the schema, the triggers and the indexes, and it is the only thing
// that can rebuild a broken database. This carries rows and nothing else, in a
// format a person can open, read, correct and hand to somebody who does not
// have PostgreSQL — which is what the office asked for and what a dump cannot
// do at any price.
//
// The order of the tables is computed from the foreign keys rather than written
// down. A hand-maintained list stays correct until the next migration adds a
// table, and the failure then is an import that stops halfway with half the
// students loaded — the worst possible moment to discover a list was stale.
package csvdata

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"flowed/internal/domain/shared"
	"flowed/internal/platform/pg"
)

// ManifestName is the archive entry describing what the archive holds. It is
// read before any CSV is touched, so an import refuses a foreign archive before
// it has deleted anything.
const ManifestName = "manifest.json"

// Manifest describes an export.
type Manifest struct {
	// SchemaVersion is the migration version the data was exported from. An
	// import into a different version is refused: a CSV loaded into a table
	// whose columns moved is silent corruption rather than an error.
	SchemaVersion int          `json:"schema_version"`
	ExportedAt    time.Time    `json:"exported_at"`
	Tables        []TableCount `json:"tables"`
}

// TableCount is one table and how many rows left with it. The count is checked
// on import, so a truncated archive — the usual result of a download that
// failed quietly — is refused rather than loaded.
type TableCount struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// excludedTables are not part of the data.
//
// The migration bookkeeping describes the schema the archive came from, and
// loading one database's history into another would make the second lie about
// what has been applied to it. The rate-limit bucket is UNLOGGED state whose
// whole value expires within a minute.
var excludedTables = []string{
	"schema_migrations",
	"schema_migration_state",
	"rate_limit_bucket",
}

// Export writes every table to a ZIP of CSV files.
//
// One statement per table, streamed through COPY rather than read into Go: the
// audit trail alone is the largest table in the system, and materialising it as
// rows to re-encode them would cost memory proportional to the database.
func Export(ctx context.Context, db *pg.DB, schemaVersion int, now time.Time) ([]byte, error) {
	tables, err := orderedTables(ctx, db)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	manifest := Manifest{SchemaVersion: schemaVersion, ExportedAt: now.UTC()}

	for _, table := range tables {
		entry, err := archive.Create(table + ".csv")
		if err != nil {
			return nil, shared.Internal("export.archive_entry", err,
				"creating the archive entry for %s", table)
		}
		// A byte-order mark, because the office opens these in Excel: without
		// one it renders محمد as Ù…Ø­Ù…Ø¯, and the operator concludes the export
		// is broken.
		if _, err := entry.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			return nil, shared.Internal("export.archive_write", err, "writing %s", table)
		}

		rows, err := copyOut(ctx, db, table, entry)
		if err != nil {
			return nil, err
		}
		manifest.Tables = append(manifest.Tables, TableCount{Name: table, Rows: rows})
	}

	entry, err := archive.Create(ManifestName)
	if err != nil {
		return nil, shared.Internal("export.archive_entry", err, "creating the manifest entry")
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		return nil, shared.Internal("export.manifest", err, "writing the manifest")
	}

	if err := archive.Close(); err != nil {
		return nil, shared.Internal("export.archive_close", err, "closing the archive")
	}
	return buf.Bytes(), nil
}

// copyOut streams one table out and returns the row count.
func copyOut(ctx context.Context, db *pg.DB, table string, w io.Writer) (int64, error) {
	// The column list is named explicitly rather than left as SELECT *, so the
	// header the import reads is the one the export wrote even if a later
	// migration reorders the table.
	columns, err := columnsOf(ctx, db, table)
	if err != nil {
		return 0, err
	}

	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		return 0, pg.WrapQuery("csvdata.acquire", err)
	}
	defer conn.Release()

	statement := fmt.Sprintf(
		`COPY (SELECT %s FROM %s ORDER BY 1) TO STDOUT WITH (FORMAT csv, HEADER true)`,
		strings.Join(quoteAll(columns), ", "), pgx.Identifier{table}.Sanitize())

	tag, err := conn.Conn().PgConn().CopyTo(ctx, w, statement)
	if err != nil {
		return 0, pg.WrapQuery("csvdata.copyOut."+table, err)
	}
	return tag.RowsAffected(), nil
}

// Import replaces the contents of every table from an archive.
//
// Destructive by design and by name: this is the other half of "export the
// system and put it back", and a version that merged would produce a database
// that is neither what was exported nor what was there. It runs in one
// transaction, so a failure anywhere leaves the database exactly as it was
// rather than half-replaced.
func Import(ctx context.Context, db *pg.DB, data []byte, schemaVersion int) (*Manifest, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, shared.Validation("import.not_an_archive",
			"this file is not a readable ZIP archive").WithCause(err)
	}

	entries := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		entries[file.Name] = file
	}

	manifest, err := readManifest(entries)
	if err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != schemaVersion {
		return nil, shared.PreconditionFailed("import.schema_mismatch",
			"this archive was exported from schema version %d and this system is at %d",
			manifest.SchemaVersion, schemaVersion).
			WithDetail("archive_version", manifest.SchemaVersion).
			WithDetail("system_version", schemaVersion).
			WithDetail("remedy",
				"migrate this system to the archive's version, or export again from the current one")
	}

	tables, err := orderedTables(ctx, db)
	if err != nil {
		return nil, err
	}

	// Every table must be present before anything is deleted. An archive
	// missing one would otherwise empty that table and leave it empty, which
	// looks exactly like a successful import of a system with no payments.
	for _, table := range tables {
		if _, ok := entries[table+".csv"]; !ok {
			return nil, shared.Validation("import.missing_table",
				"the archive has no data for %s; it was not produced by this system", table).
				WithDetail("table", table)
		}
	}

	err = db.WithTx(ctx, pg.TxOptions{}, func(ctx context.Context) error {
		conn := db.Conn(ctx)

		// Foreign keys are checked once, at commit, rather than row by row.
		//
		// Ordering the tables gets most of the way there and cannot get all of
		// it: payment.void_request_id and void_request.payment_id point at each
		// other, and installment.superseded_by_id points inside its own table.
		// Whichever side is written first references a row that does not exist
		// yet. Deferring is the only answer, and it is a better one than
		// ordering anyway — at commit every reference is checked against the
		// finished database, so an archive with a dangling reference is refused
		// rather than loaded in the one order that happened to hide it.
		// Migration 000032 is what makes the request bite.
		if _, err := conn.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
			return pg.WrapQuery("csvdata.deferConstraints", err)
		}

		// The append-only triggers are the point of this system, and they are
		// also what makes a restore impossible: they refuse the DELETE, and
		// they refuse the re-INSERT of a row carrying its original timestamps.
		// They are disabled for the length of this transaction and no longer,
		// and only the triggers this role owns — the foreign keys stay live,
		// which is why the load is ordered rather than unchecked.
		for _, table := range tables {
			if _, err := conn.Exec(ctx, fmt.Sprintf(
				`ALTER TABLE %s DISABLE TRIGGER USER`, pgx.Identifier{table}.Sanitize())); err != nil {
				return pg.WrapQuery("csvdata.disableTriggers."+table, err)
			}
		}

		// One TRUNCATE over every table rather than a DELETE per table in
		// dependency order.
		//
		// Ordering the deletes does not work and cannot be made to: ON DELETE
		// RESTRICT is checked immediately even inside a deferred transaction —
		// that is what distinguishes it from NO ACTION — so emptying student
		// is refused while a single enrollment row still exists, whatever
		// order the statements are issued in. TRUNCATE naming all of them at
		// once empties them as one act, which is the only shape of the
		// operation the constraints have nothing to say about.
		if _, err := conn.Exec(ctx, `TRUNCATE `+strings.Join(quoteAll(tables), ", ")); err != nil {
			return pg.WrapQuery("csvdata.clear", err)
		}

		// Parents first, for the same reason in reverse.
		for _, table := range tables {
			loaded, err := copyIn(ctx, db, table, entries[table+".csv"])
			if err != nil {
				return err
			}
			if expected, ok := manifestRows(manifest, table); ok && loaded != expected {
				return shared.Validation("import.row_count_mismatch",
					"%s carried %d rows in the manifest but %d were read; the archive is truncated",
					table, expected, loaded).WithDetail("table", table)
			}
		}

		// The deferred foreign keys are checked here rather than at commit.
		//
		// Two reasons, and the second is the one that bites. A violation
		// raised at commit arrives as "commit unexpectedly resulted in
		// rollback" with the constraint name lost somewhere inside the driver,
		// which is no use to anybody holding a broken archive. And PostgreSQL
		// refuses ALTER TABLE on a table with pending trigger events, so the
		// triggers below could not be switched back on while any check was
		// still outstanding — the import failed on exactly that, with the real
		// cause two layers down.
		if _, err := conn.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return wrapDeferredCheck(err)
		}

		// Switched back on before the commit that carries them, and their
		// failure is reported rather than swallowed: leaving the append-only
		// guards off is the one outcome worse than a failed import.
		for _, table := range tables {
			if _, err := conn.Exec(ctx, fmt.Sprintf(
				`ALTER TABLE %s ENABLE TRIGGER USER`, pgx.Identifier{table}.Sanitize())); err != nil {
				return pg.WrapQuery("csvdata.enableTriggers."+table, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

// copyIn loads one table from its archive entry.
func copyIn(ctx context.Context, db *pg.DB, table string, file *zip.File) (int64, error) {
	rc, err := file.Open()
	if err != nil {
		return 0, shared.Validation("import.unreadable_entry",
			"%s cannot be read from the archive", file.Name).WithCause(err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		return 0, shared.Validation("import.unreadable_entry",
			"%s cannot be read from the archive", file.Name).WithCause(err)
	}
	// The export writes a byte-order mark for Excel's benefit. COPY would read
	// it as the first three characters of the first column name.
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})

	header, err := headerOf(body)
	if err != nil {
		return 0, err
	}

	// The header decides which columns are loaded, so a table that gained a
	// column since the export still loads and the new column takes its default.
	// A header naming a column that no longer exists is refused rather than
	// guessed at.
	known, err := columnsOf(ctx, db, table)
	if err != nil {
		return 0, err
	}
	for _, column := range header {
		if !slices.Contains(known, column) {
			return 0, shared.Validation("import.unknown_column",
				"%s has no column %q; this archive does not match the current schema", table, column).
				WithDetail("table", table).WithDetail("column", column)
		}
	}

	tx, ok := pg.TxFrom(ctx)
	if !ok {
		return 0, shared.Internal("import.no_transaction", nil,
			"a table load must run inside the import transaction")
	}

	statement := fmt.Sprintf(`COPY %s (%s) FROM STDIN WITH (FORMAT csv, HEADER true)`,
		pgx.Identifier{table}.Sanitize(), strings.Join(quoteAll(header), ", "))

	tag, err := tx.Conn().PgConn().CopyFrom(ctx, bytes.NewReader(body), statement)
	if err != nil {
		return 0, wrapCopyIn(table, err)
	}
	return tag.RowsAffected(), nil
}

// wrapDeferredCheck names the constraint a reference check failed on.
//
// This is where an archive that is internally inconsistent — a payment whose
// account was edited out of the CSV by hand, say — is caught, and the
// constraint name is the whole of the diagnosis.
func wrapDeferredCheck(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return shared.Validation("import.dangling_reference",
			"the archive refers to rows it does not contain: %s", pgErr.Message).
			WithDetail("constraint", pgErr.ConstraintName).
			WithDetail("sqlstate", pgErr.Code).
			WithDetail("remedy", "export again rather than editing the archive by hand").
			WithCause(err)
	}
	return pg.WrapQuery("csvdata.checkReferences", err)
}

// wrapCopyIn names the table a load failed on.
//
// A COPY error reports a line number and a constraint, and neither says which
// of forty files it came from — which is the first thing anybody needs.
func wrapCopyIn(table string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return shared.Validation("import.load_failed",
			"loading %s failed: %s", table, pgErr.Message).
			WithDetail("table", table).
			WithDetail("sqlstate", pgErr.Code).
			WithDetail("constraint", pgErr.ConstraintName).
			WithCause(err)
	}
	return pg.WrapQuery("csvdata.copyIn."+table, err)
}

// orderedTables lists the public base tables parents-first.
//
// The order comes from the foreign keys, computed here rather than written
// down: a hand-maintained list is correct until the next migration adds a
// table, and an import that stops halfway is the worst place to find that out.
// A self-reference — student.merged_into_id — is excluded from the graph rather
// than treated as a cycle, because a table never waits on itself.
func orderedTables(ctx context.Context, db *pg.DB) ([]string, error) {
	const query = `
		SELECT c.relname,
		       coalesce(array_agg(DISTINCT p.relname) FILTER (WHERE p.relname IS NOT NULL), '{}')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_constraint fk
		       ON fk.conrelid = c.oid AND fk.contype = 'f' AND fk.confrelid <> c.oid
		LEFT JOIN pg_class p ON p.oid = fk.confrelid
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		GROUP BY c.relname
		ORDER BY c.relname`

	rows, err := db.Conn(ctx).Query(ctx, query)
	if err != nil {
		return nil, pg.WrapQuery("csvdata.orderedTables", err)
	}
	defer rows.Close()

	parents := map[string][]string{}
	var names []string
	for rows.Next() {
		var (
			name string
			refs []string
		)
		if err := rows.Scan(&name, &refs); err != nil {
			return nil, pg.WrapQuery("csvdata.orderedTables", err)
		}
		if slices.Contains(excludedTables, name) {
			continue
		}
		names = append(names, name)
		parents[name] = refs
	}
	if err := rows.Err(); err != nil {
		return nil, pg.WrapQuery("csvdata.orderedTables", err)
	}

	ordered := make([]string, 0, len(names))
	placed := map[string]bool{}
	for range names {
		progress := false
		for _, name := range names {
			if placed[name] {
				continue
			}
			ready := true
			for _, parent := range parents[name] {
				if slices.Contains(names, parent) && !placed[parent] {
					ready = false
					break
				}
			}
			if ready {
				ordered = append(ordered, name)
				placed[name] = true
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	// Anything a cycle left unplaced still has to be exported. Losing a table
	// because its foreign keys formed a loop would be a backup that quietly
	// omits data, which is worse than one that loads in an awkward order.
	for _, name := range names {
		if !placed[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered, nil
}

// columnsOf reads a table's writable columns in ordinal order. Generated and
// identity columns are left out: the database computes them, and COPY refuses
// to be handed a value for one.
func columnsOf(ctx context.Context, db *pg.DB, table string) ([]string, error) {
	const query = `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		  AND is_generated = 'NEVER' AND identity_generation IS NULL
		ORDER BY ordinal_position`

	rows, err := db.Conn(ctx).Query(ctx, query, table)
	if err != nil {
		return nil, pg.WrapQuery("csvdata.columnsOf", err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, pg.WrapQuery("csvdata.columnsOf", err)
	}
	return columns, nil
}

// headerOf reads the first CSV line as a column list.
func headerOf(body []byte) ([]string, error) {
	line, _, _ := bytes.Cut(body, []byte("\n"))
	if len(bytes.TrimSpace(line)) == 0 {
		return nil, shared.Validation("import.empty_file", "a table file has no header row")
	}
	fields := strings.Split(strings.TrimRight(string(line), "\r"), ",")
	for i, field := range fields {
		fields[i] = strings.Trim(strings.TrimSpace(field), `"`)
	}
	return fields, nil
}

func readManifest(entries map[string]*zip.File) (*Manifest, error) {
	file, ok := entries[ManifestName]
	if !ok {
		return nil, shared.Validation("import.no_manifest",
			"this archive has no %s and was not produced by this system", ManifestName)
	}
	rc, err := file.Open()
	if err != nil {
		return nil, shared.Validation("import.unreadable_manifest",
			"the manifest cannot be read").WithCause(err)
	}
	defer rc.Close()

	var manifest Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, shared.Validation("import.unreadable_manifest",
			"the manifest is not readable JSON").WithCause(err)
	}
	return &manifest, nil
}

func manifestRows(m *Manifest, table string) (int64, bool) {
	for _, t := range m.Tables {
		if t.Name == table {
			return t.Rows, true
		}
	}
	return 0, false
}

func quoteAll(columns []string) []string {
	out := make([]string, len(columns))
	for i, column := range columns {
		out[i] = pgx.Identifier{column}.Sanitize()
	}
	return out
}
