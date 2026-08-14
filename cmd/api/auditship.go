package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/bootstrap"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/pg"
)

// auditShip is the operator's hand on the off-host audit archive.
//
//	api audit-ship          copy everything not yet copied
//	api audit-ship verify   check the archive against the database
//
// The verify form is the one that earns its place: it is the only command in
// the system that can detect an audit entry having been deleted, because it is
// the only one that compares against a copy the database cannot reach. It exits
// non-zero when the two disagree, so a cron entry or a monitoring check can use
// it directly.
func auditShip() error {
	verify := len(os.Args) > 2 && os.Args[2] == "verify"

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.Log, cfg.App.Name+"-audit-ship", version, cfg.App.Environment)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	db, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	deps := app.Deps{
		Tx:    postgres.NewTxManager(db),
		Audit: postgres.NewAuditRepository(db),
		Clock: shared.SystemClock{},
		Log:   log,
	}

	shipper := bootstrap.BuildAuditShipper(cfg, deps, db, log)
	if shipper == nil {
		return fmt.Errorf(
			"no audit archive is configured: set AUDIT_ARCHIVE_DIR or AUDIT_ARCHIVE_URL.\n" +
				"Without one the hash chain still detects an altered entry, but nothing " +
				"detects a deleted one")
	}

	// The CLI runs as the system actor. It is on the machine already; adding a
	// credential prompt would protect nothing and stop the cron entry working.
	actor := shared.SystemActor()

	if !verify {
		result, err := shipper.Ship(ctx, actor)
		if err != nil {
			return err
		}
		fmt.Printf("shipped %d entries in %d block(s) to %s\n",
			result.Entries, result.Blocks, result.Destination)
		if result.Remaining > 0 {
			fmt.Printf("%d entries are still only on this host; run again to continue\n",
				result.Remaining)
		}
		return nil
	}

	report, err := shipper.Verify(ctx, actor)
	if err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))

	if !report.ArchiveReadable {
		log.Warn("the archive could not be read back from here, so only the " +
			"shipment records were checked")
	}
	if !report.OK() {
		return fmt.Errorf("the audit archive and the database disagree in %d place(s); "+
			"treat this as an incident, not a defect report", len(report.Problems))
	}

	fmt.Printf("archive verified: %d shipments, %d entries, %d entries not yet shipped\n",
		report.Shipments, report.EntriesChecked, report.Unshipped)
	return nil
}
