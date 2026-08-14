package integration

import (
	"context"
	"testing"
	"time"

	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/port"
)

// A finding has a life, and most of that life is SQL: the upsert that turns a
// second sighting into a count rather than a second row, the escalation that
// follows from the count, and the sweep that closes what a later pass no longer
// sees. All three are in the statement, so all three are tested against the
// real database.

func TestAFindingIsOneRowForAsLongAsItSurvives(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	store := postgres.NewReconciliationRepository(db)

	subject := shared.NewID()
	observed := port.ObservedFinding{
		Kind:        port.ReconcileAccounts,
		SubjectType: "account",
		SubjectID:   subject,
		Detail:      map[string]any{"paid_drift": 50000},
	}

	// Three nights, three passes.
	const escalateAfter = 3
	var runs []shared.ID
	for night := 1; night <= 3; night++ {
		run := startRun(t, store, port.ReconcileAccounts)
		runs = append(runs, run)

		isNew, err := store.RecordFinding(ctx, run, observed, escalateAfter, time.Now().UTC())
		if err != nil {
			t.Fatalf("recording night %d: %v", night, err)
		}
		if want := night == 1; isNew != want {
			t.Errorf("night %d reported new=%t, want %t — the same drift on the same "+
				"account must not open a second finding", night, isNew, want)
		}
	}

	finding := findBySubject(t, store, subject)
	if finding.SeenCount != 3 {
		t.Errorf("seen_count = %d after three passes, want 3", finding.SeenCount)
	}
	if finding.Severity != "critical" {
		t.Errorf("severity = %q after surviving three passes, want critical — drift nobody "+
			"has explained by the third night is not a transient", finding.Severity)
	}

	// The fourth pass no longer sees it: somebody corrected the account, or the
	// command that was writing the drift was fixed.
	fourth := startRun(t, store, port.ReconcileAccounts)
	closed, err := store.ResolveMissing(ctx, port.ReconcileAccounts, fourth,
		[]shared.ID{shared.NewID()}, time.Now().UTC())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if closed < 1 {
		t.Fatal("a finding the latest pass no longer sees must be closed; leaving it open " +
			"forever teaches operators that the queue is noise")
	}

	reread, err := store.GetFinding(ctx, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.State != "resolved" {
		t.Errorf("state = %q after the sweep, want resolved", reread.State)
	}
	if reread.Resolution == nil {
		t.Error("a resolved finding must say what closed it")
	}

	// And if the same account drifts again, that is a new finding rather than
	// the old one reopened — the history of "this account has done this before"
	// is exactly what the next investigation reads.
	fifth := startRun(t, store, port.ReconcileAccounts)
	isNew, err := store.RecordFinding(ctx, fifth, observed, escalateAfter, time.Now().UTC())
	if err != nil {
		t.Fatalf("recording the recurrence: %v", err)
	}
	if !isNew {
		t.Error("drift that returns after being resolved must open a fresh finding")
	}
	if again := findBySubject(t, store, subject); again.ID == finding.ID {
		t.Error("the resolved finding was revived rather than a new one opened")
	}

	// Registered last so it runs first: these are operational rows, not
	// financial ones, and the fixture's leftovers would otherwise sit in the
	// queue that other tests assert is empty.
	t.Cleanup(func() {
		clean := context.Background()
		_, _ = db.Pool().Exec(clean,
			`DELETE FROM reconciliation_finding WHERE subject_id = $1`, subject)
		_, _ = db.Pool().Exec(clean,
			`DELETE FROM reconciliation_run WHERE id = ANY($1::uuid[])`, append(runs, fourth, fifth))
	})
}

// The checks read the invariant views, and on a healthy database they find
// nothing while still reporting how much they looked at. "Nothing wrong in
// 40,000 accounts" and "nothing wrong because the query matched nothing" have
// to be distinguishable, or a broken check reads as a clean system.
func TestTheChecksReportWhatTheyLookedAt(t *testing.T) {
	ctx := context.Background()
	db := shipTestDB(t)
	store := postgres.NewReconciliationRepository(db)

	checks := []struct {
		name  string
		check func(context.Context, int) ([]port.ObservedFinding, int64, error)
		// The population the check is over, so the reported figure can be
		// compared against the truth rather than against a guess. Asserting
		// "more than zero" encoded an assumption that the database already had
		// data in it, which on a freshly created one is simply false — and a
		// test that fails on an empty database teaches people to seed before
		// running it, which is how a suite stops being runnable.
		population string
	}{
		{"accounts", store.CheckAccounts, "SELECT count(*) FROM financial_account"},
		{"installments", store.CheckInstallments, "SELECT count(*) FROM installment"},
		{"refunds", store.CheckRefunds, "SELECT count(*) FROM payment WHERE status = 'posted'"},
		{"audit_chain", store.CheckAuditChain, "SELECT count(*) FROM audit_log"},
	}

	for _, tc := range checks {
		found, checked, err := tc.check(ctx, 100)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(found) != 0 {
			t.Errorf("%s reported %d violation(s) on a database the suite maintains: %+v",
				tc.name, len(found), found[0].Detail)
		}

		var population int64
		if err := db.Pool().QueryRow(ctx, tc.population).Scan(&population); err != nil {
			t.Fatal(err)
		}
		// The end-to-end suite runs in parallel and writes as it goes, so the
		// population can only have grown between the check and this count.
		if checked > population {
			t.Errorf("%s reported checking %d rows out of %d that exist",
				tc.name, checked, population)
		}
		if population > 0 && checked == 0 {
			t.Errorf("%s reported checking no rows while %d exist; a check that looks "+
				"at nothing must not read as a clean one", tc.name, population)
		}
	}
}

func startRun(t *testing.T, store *postgres.ReconciliationRepository, kind port.ReconciliationKind) shared.ID {
	t.Helper()
	run := &port.ReconciliationRun{
		ID:        shared.NewID(),
		Kind:      kind,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	if err := store.StartRun(context.Background(), run); err != nil {
		t.Fatalf("starting a run: %v", err)
	}
	// Finished immediately: a run left "running" is indistinguishable from one
	// still in progress, and the first thing anybody does with this table is
	// read the newest row.
	finished := time.Now().UTC()
	run.Status = "clean"
	run.FinishedAt = &finished
	if err := store.FinishRun(context.Background(), run); err != nil {
		t.Fatalf("finishing a run: %v", err)
	}
	return run.ID
}

func findBySubject(t *testing.T, store *postgres.ReconciliationRepository, subject shared.ID) *port.ReconciliationFinding {
	t.Helper()
	for _, state := range []string{"", "resolved"} {
		findings, err := store.ListFindings(context.Background(), state, 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			if finding.SubjectID == subject {
				return finding
			}
		}
	}
	t.Fatalf("no finding for subject %s", subject)
	return nil
}
