// Command migrate applies, inspects and rolls back database schema
// migrations.
//
//	migrate up                 apply every pending migration
//	migrate down [n]           roll back the last n migrations (default 1)
//	migrate to <version>       migrate forward or backward to a version
//	migrate status             show every migration and its state
//	migrate version            print the current schema version
//	migrate validate           verify the database matches this build
//	migrate create <name>      scaffold a new migration pair
//	migrate verify-audit       verify the audit hash chain
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/migrate"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nmigrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	command := args[0]

	// Scaffolding a migration needs no database.
	if command == "create" {
		if len(args) < 2 {
			return errors.New("create requires a name, e.g. migrate create add_scholarship_table")
		}
		return createMigration(strings.Join(args[1:], "_"))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.Log, cfg.App.Name+"-migrate", cfg.App.Version, cfg.App.Environment)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No query tracer: a migration run is a handful of statements watched by
	// whoever started it, and standing up a trace pipeline for them would only
	// add a way for the run to fail.
	db, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	runner, err := migrate.New(db.Pool(), migrations.FS, migrations.Dir, log)
	if err != nil {
		return err
	}

	switch command {
	case "up":
		return runner.Up(ctx)

	case "down":
		steps := 1
		if len(args) > 1 {
			steps, err = strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid step count %q: %w", args[1], err)
			}
		}
		if cfg.App.IsProduction() && os.Getenv("MIGRATE_CONFIRM_PRODUCTION") != "yes" {
			return errors.New(
				"refusing to roll back in production without MIGRATE_CONFIRM_PRODUCTION=yes; " +
					"a down migration on a live financial database drops data")
		}
		return runner.Down(ctx, steps)

	case "to":
		if len(args) < 2 {
			return errors.New("to requires a target version")
		}
		target, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid version %q: %w", args[1], err)
		}
		return runner.To(ctx, target)

	case "status":
		return printStatus(ctx, runner)

	case "version":
		version, err := runner.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%06d\n", version)
		return nil

	case "validate":
		if err := runner.Validate(ctx); err != nil {
			return err
		}
		fmt.Println("schema matches this build")
		return nil

	case "verify-audit":
		return verifyAudit(ctx, db)

	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func printStatus(ctx context.Context, runner *migrate.Runner) error {
	statuses, err := runner.Status(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tNAME\tSTATE\tAPPLIED AT\tTOOK")

	var pending, drifted int
	for _, s := range statuses {
		state := "pending"
		appliedAt, took := "-", "-"
		switch {
		case s.MissingFromDisk:
			state = "UNKNOWN TO BUILD"
		case s.Applied && !s.ChecksumMatches:
			state = "CHECKSUM MISMATCH"
			drifted++
		case s.Applied:
			state = "applied"
		default:
			pending++
		}
		if s.Applied {
			appliedAt = s.AppliedAt.Local().Format("2006-01-02 15:04:05")
			took = (time.Duration(s.ExecutionMS) * time.Millisecond).String()
		}
		fmt.Fprintf(w, "%06d\t%s\t%s\t%s\t%s\n", s.Version, s.Name, state, appliedAt, took)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n%d migration(s), %d pending", len(statuses), pending)
	if drifted > 0 {
		fmt.Printf(", %d WITH CHANGED CONTENT", drifted)
	}
	fmt.Println()
	return nil
}

func verifyAudit(ctx context.Context, db *pg.DB) error {
	rows, err := db.Pool().Query(ctx, `SELECT sequence_no, id, occurred_at, problem FROM verify_audit_chain(0)`)
	if err != nil {
		return fmt.Errorf("verifying audit chain: %w", err)
	}
	defer rows.Close()

	found := 0
	for rows.Next() {
		var seq int64
		var id, problem string
		var occurredAt time.Time
		if err := rows.Scan(&seq, &id, &occurredAt, &problem); err != nil {
			return err
		}
		found++
		fmt.Printf("BROKEN  seq=%d id=%s at=%s\n        %s\n",
			seq, id, occurredAt.Format(time.RFC3339), problem)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if found == 0 {
		fmt.Println("audit chain intact")
		return nil
	}
	return fmt.Errorf("%d audit entr(ies) failed verification", found)
}

var nameSanitiser = regexp.MustCompile(`[^a-z0-9_]+`)

func createMigration(rawName string) error {
	name := nameSanitiser.ReplaceAllString(strings.ToLower(rawName), "_")
	name = strings.Trim(name, "_")
	if name == "" {
		return errors.New("migration name must contain letters or digits")
	}

	dir := "migrations"
	existing, err := migrate.Load(os.DirFS("."), dir)
	if err != nil {
		return err
	}
	var next int64 = 1
	if len(existing) > 0 {
		next = existing[len(existing)-1].Version + 1
	}

	upPath := filepath.Join(dir, fmt.Sprintf("%06d_%s.up.sql", next, name))
	downPath := filepath.Join(dir, fmt.Sprintf("%06d_%s.down.sql", next, name))

	for _, p := range []string{upPath, downPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s already exists", p)
		}
	}

	upTemplate := fmt.Sprintf(`-- %s
--
-- Describe what this migration changes and why. If it must run outside a
-- transaction (CREATE INDEX CONCURRENTLY, for instance), add the directive:
--
--   -- migrate:no-transaction
--
-- and be aware that a failure part-way will mark the schema dirty.

`, name)

	downTemplate := fmt.Sprintf(`-- Reverse of %s.
--
-- Every migration is reversible. If this change genuinely cannot be undone,
-- say so here with a RAISE EXCEPTION rather than leaving the file empty, so
-- the attempt fails loudly instead of appearing to succeed.

`, name)

	if err := os.WriteFile(upPath, []byte(upTemplate), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(downPath, []byte(downTemplate), 0o644); err != nil {
		return err
	}

	fmt.Printf("created %s\ncreated %s\n", upPath, downPath)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: migrate <command> [arguments]

  up                 apply every pending migration
  down [n]           roll back the last n migrations (default 1)
  to <version>       migrate forward or backward to a specific version
  status             show every migration and its state
  version            print the current schema version
  validate           verify the database matches this build
  create <name>      scaffold a new migration pair
  verify-audit       verify the audit log hash chain

Configuration is read from the environment; see .env.example.
`)
}
