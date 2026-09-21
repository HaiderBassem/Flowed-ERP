package app

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/money"
	"flowed/internal/domain/payment"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------
//
// Each fake embeds the port interface it stands in for. That satisfies the
// interface without hand-writing the dozen methods these commands never call,
// and a test that unexpectedly reaches one gets a nil-interface panic naming
// the method — which is exactly the failure a test wants, rather than a silent
// zero value.

type fakeTx struct{ port.TxManager }

func (fakeTx) Write(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (fakeTx) Read(ctx context.Context, fn func(context.Context) error) error  { return fn(ctx) }
func (fakeTx) RequireTx(context.Context, string) error                         { return nil }

type fakeYears struct {
	port.AcademicYearRepository
	open []*academic.Year
}

func (f *fakeYears) CurrentOpen(context.Context) ([]*academic.Year, error) { return f.open, nil }

type fakeAudit struct {
	port.AuditRepository
	entries []port.AuditEntry
}

func (f *fakeAudit) Append(_ context.Context, e port.AuditEntry) error {
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) actions() []string {
	out := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.Action)
	}
	return out
}

// fakeSessions stands in for the cashier session table, including the partial
// unique index that allows one open drawer per cashier.
type fakeSessions struct {
	port.CashierSessionRepository
	stored   map[shared.ID]*payment.CashierSession
	expected money.Amount
	// hideOpenOnce makes the next GetOpenForUser report nothing, which is how
	// the race the unique index exists for is reproduced: the service's own
	// pre-check sees a free cashier and the insert loses anyway.
	hideOpenOnce bool
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{stored: make(map[shared.ID]*payment.CashierSession)}
}

func (f *fakeSessions) Open(_ context.Context, s *payment.CashierSession) error {
	for _, existing := range f.stored {
		if existing.CashierUserID == s.CashierUserID && existing.Status == payment.SessionOpen {
			// The message the constraint translator produces for
			// uq_cashier_session_open.
			return shared.Conflict("cashier_session.already_open",
				"this cashier already has an open session; close it before opening another")
		}
	}
	f.stored[s.ID] = s
	return nil
}

func (f *fakeSessions) Close(_ context.Context, s *payment.CashierSession) error {
	f.stored[s.ID] = s
	return nil
}

func (f *fakeSessions) Update(_ context.Context, s *payment.CashierSession) error {
	f.stored[s.ID] = s
	return nil
}

func (f *fakeSessions) GetByID(_ context.Context, id shared.ID) (*payment.CashierSession, error) {
	s, ok := f.stored[id]
	if !ok {
		return nil, shared.NotFound("not_found", "no session %s", id)
	}
	return s, nil
}

func (f *fakeSessions) GetOpenForUser(_ context.Context, userID shared.ID) (*payment.CashierSession, error) {
	if f.hideOpenOnce {
		f.hideOpenOnce = false
		return nil, shared.NotFound("not_found", "no open session")
	}
	for _, s := range f.stored {
		if s.CashierUserID == userID && s.Status == payment.SessionOpen {
			return s, nil
		}
	}
	return nil, shared.NotFound("not_found", "no open session")
}

func (f *fakeSessions) ExpectedCash(context.Context, shared.ID) (money.Amount, error) {
	return f.expected, nil
}

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

type cashierFixture struct {
	service  *CashierService
	sessions *fakeSessions
	audit    *fakeAudit
	deskID   shared.ID
	cashier  shared.Actor
}

func newCashierFixture(t *testing.T) *cashierFixture {
	t.Helper()

	deskID := shared.NewID()
	sessions := newFakeSessions()
	audit := &fakeAudit{}

	year := &academic.Year{ID: shared.NewID(), Code: "2025-2026", Status: academic.YearOpen}

	deps := Deps{
		Tx:       fakeTx{},
		Years:    &fakeYears{open: []*academic.Year{year}},
		Sessions: sessions,
		Audit:    audit,
		Clock:    shared.FixedClock{Instant: time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)},
		Log:      discardLogger(),
	}

	return &cashierFixture{
		service:  NewCashierService(deps),
		sessions: sessions,
		audit:    audit,
		deskID:   deskID,
		cashier:  cashierActor("hind", deskID),
	}
}

func cashierActor(username string, deskID shared.ID) shared.Actor {
	return shared.Actor{
		UserID:        shared.NewID(),
		Username:      username,
		Roles:         []shared.Role{shared.RoleCashier},
		CashierDeskID: &deskID,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// Cashier session commands
// ---------------------------------------------------------------------------

func TestOpenSessionRecordsTheShiftAndAuditsIt(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 250_000)
	if err != nil {
		t.Fatalf("opening a drawer: %v", err)
	}
	if session.Status != payment.SessionOpen {
		t.Errorf("status = %s, want open", session.Status)
	}
	if session.OpeningFloat != 250_000 {
		t.Errorf("opening float = %s, want 250,000", session.OpeningFloat)
	}
	if got := f.audit.actions(); len(got) != 1 || got[0] != "cashier_session.opened" {
		t.Errorf("audit actions = %v, want one cashier_session.opened", got)
	}
}

func TestOpenSessionRefusesASecondDrawerForTheSameCashier(t *testing.T) {
	f := newCashierFixture(t)

	first, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening the first drawer: %v", err)
	}

	_, err = f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err == nil {
		t.Fatal("a cashier with an open drawer must not be able to open another")
	}
	if code := shared.CodeOf(err); code != "cashier_session.already_open" {
		t.Errorf("error code = %q, want cashier_session.already_open", code)
	}
	// The refusal has to name the drawer that is in the way; a cashier told
	// only "already open" has nowhere to go next.
	domainErr, ok := shared.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T", err)
	}
	if domainErr.Details["session_id"] != first.ID.String() {
		t.Errorf("session_id detail = %v, want %s", domainErr.Details["session_id"], first.ID)
	}
}

// The pre-check cannot be the guarantee: two terminals can both read "no open
// session" before either inserts. The partial unique index settles it, and the
// loser must still be told which drawer won.
func TestOpenSessionTranslatesTheUniqueIndexConflict(t *testing.T) {
	f := newCashierFixture(t)

	first, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening the first drawer: %v", err)
	}

	f.sessions.hideOpenOnce = true
	_, err = f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err == nil {
		t.Fatal("the unique index must refuse the second drawer even when the pre-check missed it")
	}
	domainErr, ok := shared.AsDomain(err)
	if !ok {
		t.Fatalf("expected a domain error, got %T", err)
	}
	if domainErr.Code != "cashier_session.already_open" {
		t.Errorf("error code = %q, want cashier_session.already_open", domainErr.Code)
	}
	if domainErr.Details["session_id"] != first.ID.String() {
		t.Errorf("session_id detail = %v, want %s", domainErr.Details["session_id"], first.ID)
	}
}

// Receipt series run per desk, so a cashier signed in at one window must not
// open the drawer of another: the printed numbers would come out of a book
// nobody at that window is holding.
func TestOpenSessionRefusesADeskOtherThanTheOneOnTheToken(t *testing.T) {
	f := newCashierFixture(t)

	_, err := f.service.OpenSession(context.Background(), f.cashier, shared.NewID(), 100_000)
	if err == nil {
		t.Fatal("opening a drawer at another desk must be refused")
	}
	if code := shared.CodeOf(err); code != "cashier_session.desk_mismatch" {
		t.Errorf("error code = %q, want cashier_session.desk_mismatch", code)
	}
	if kind := shared.KindOf(err); kind != shared.KindForbidden {
		t.Errorf("kind = %s, want forbidden", kind)
	}
}

func TestCloseSessionRefusesAVarianceWithNoExplanation(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening a drawer: %v", err)
	}
	f.sessions.expected = 900_000

	_, err = f.service.CloseSession(context.Background(), f.cashier, session.ID, 880_000, nil)
	if err == nil {
		t.Fatal("closing a drawer 20,000 short with no explanation must be refused")
	}
	if code := shared.CodeOf(err); code != "cashier_session.variance_reason_required" {
		t.Errorf("error code = %q, want cashier_session.variance_reason_required", code)
	}

	reason := "20,000 handed to the bursar against receipt 88"
	closed, err := f.service.CloseSession(context.Background(), f.cashier, session.ID, 880_000, &reason)
	if err != nil {
		t.Fatalf("closing with an explanation: %v", err)
	}
	if closed.Variance == nil || *closed.Variance != -20_000 {
		t.Errorf("variance = %v, want -20,000", closed.Variance)
	}
	if closed.ExpectedCash == nil || *closed.ExpectedCash != 900_000 {
		t.Errorf("expected cash = %v, want the repository's 900,000", closed.ExpectedCash)
	}
}

func TestCloseSessionRefusesSomebodyElsesDrawer(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening a drawer: %v", err)
	}
	f.sessions.expected = 100_000

	other := cashierActor("laith", f.deskID)
	_, err = f.service.CloseSession(context.Background(), other, session.ID, 100_000, nil)
	if err == nil {
		t.Fatal("a cashier must not close a drawer they did not take money into")
	}
	if code := shared.CodeOf(err); code != "cashier_session.not_own_session" {
		t.Errorf("error code = %q, want cashier_session.not_own_session", code)
	}
	if kind := shared.KindOf(err); kind != shared.KindForbidden {
		t.Errorf("kind = %s, want forbidden", kind)
	}
}

func TestApproveSessionRefusesTheCashiersOwnSignature(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening a drawer: %v", err)
	}
	f.sessions.expected = 100_000
	if _, err := f.service.CloseSession(context.Background(), f.cashier, session.ID, 100_000, nil); err != nil {
		t.Fatalf("closing a balanced drawer: %v", err)
	}

	// A plain cashier is stopped at the authority check.
	if _, err := f.service.ApproveSession(context.Background(), f.cashier, session.ID); err == nil {
		t.Error("a cashier must not hold the authority to sign off a drawer")
	}

	// Somebody wearing both hats — which happens in a small finance office —
	// is stopped by the domain's four-eyes rule instead.
	twoHats := f.cashier
	twoHats.Roles = []shared.Role{shared.RoleCashier, shared.RoleFinanceManager}
	_, err = f.service.ApproveSession(context.Background(), twoHats, session.ID)
	if err == nil {
		t.Fatal("a cashier must not sign off their own drawer even holding the finance role")
	}
	if code := shared.CodeOf(err); code != "cashier_session.self_approval" {
		t.Errorf("error code = %q, want cashier_session.self_approval", code)
	}

	supervisor := shared.Actor{
		UserID:   shared.NewID(),
		Username: "finance",
		Roles:    []shared.Role{shared.RoleFinanceManager},
	}
	approved, err := f.service.ApproveSession(context.Background(), supervisor, session.ID)
	if err != nil {
		t.Fatalf("a supervisor should be able to sign off: %v", err)
	}
	if approved.Status != payment.SessionApproved {
		t.Errorf("status = %s, want approved", approved.Status)
	}
	// Open, close, approve: three commands, three entries. A drawer that does
	// not balance is read out of exactly this trail.
	want := []string{"cashier_session.opened", "cashier_session.closed", "cashier_session.approved"}
	got := f.audit.actions()
	if len(got) != len(want) {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit action %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGetOpenSessionReportsNoDrawerAsAbsenceNotFailure(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.GetOpenSession(context.Background(), f.cashier)
	if err != nil {
		t.Fatalf("a cashier with no open drawer is a normal screen state, got %v", err)
	}
	if session != nil {
		t.Errorf("session = %v, want nil", session)
	}
}

func TestSessionSummaryKeepsTheFigureTheDrawerWasClosedAgainst(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening a drawer: %v", err)
	}

	// While open, the sheet recomputes.
	f.sessions.expected = 640_000
	summary, err := f.service.SessionSummary(context.Background(), f.cashier, session.ID)
	if err != nil {
		t.Fatalf("summarising an open shift: %v", err)
	}
	if summary.ExpectedCash != 640_000 {
		t.Errorf("expected cash = %s, want 640,000", summary.ExpectedCash)
	}

	if _, err := f.service.CloseSession(context.Background(), f.cashier, session.ID, 640_000, nil); err != nil {
		t.Fatalf("closing a balanced drawer: %v", err)
	}

	// After the close the sheet reports what was signed, not what the tables
	// say today: a later disagreement is a finding, not something to restate.
	f.sessions.expected = 999_999
	summary, err = f.service.SessionSummary(context.Background(), f.cashier, session.ID)
	if err != nil {
		t.Fatalf("summarising a closed shift: %v", err)
	}
	if summary.ExpectedCash != 640_000 {
		t.Errorf("expected cash = %s, want the 640,000 recorded at close", summary.ExpectedCash)
	}
}

func TestSessionSummaryRefusesAnotherCashiersShift(t *testing.T) {
	f := newCashierFixture(t)

	session, err := f.service.OpenSession(context.Background(), f.cashier, f.deskID, 100_000)
	if err != nil {
		t.Fatalf("opening a drawer: %v", err)
	}

	other := cashierActor("laith", f.deskID)
	if _, err := f.service.SessionSummary(context.Background(), other, session.ID); err == nil {
		t.Error("a cashier must not read a colleague's shift sheet")
	}

	auditor := shared.Actor{UserID: shared.NewID(), Username: "auditor", Roles: []shared.Role{shared.RoleAuditor}}
	if _, err := f.service.SessionSummary(context.Background(), auditor, session.ID); err != nil {
		t.Errorf("an auditor reads any shift: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

type openLocker struct{}

func (openLocker) TryAdvisoryLock(context.Context, int64) (bool, func(), error) {
	return true, func() {}, nil
}

// heldLocker stands in for another replica already running the job.
type heldLocker struct{}

func (heldLocker) TryAdvisoryLock(context.Context, int64) (bool, func(), error) {
	return false, nil, nil
}

func newTestScheduler(lock advisoryLocker, jobs ...job) *Scheduler {
	return &Scheduler{
		lock:  lock,
		log:   discardLogger(),
		actor: shared.SystemActor(),
		cfg: SchedulerConfig{
			JobTimeout:      time.Second,
			ShutdownTimeout: 2 * time.Second,
			StartupStagger:  time.Millisecond,
		},
		jobs: jobs,
	}
}

// A job with a bug must cost its own run and nothing else. If a panic escaped
// the runner it would kill that job's goroutine, and the routine would stop
// silently — the worst possible failure for something whose whole purpose is to
// notice problems nobody is watching for.
func TestSchedulerSurvivesAPanickingJob(t *testing.T) {
	var exploded, healthy atomic.Int64

	scheduler := newTestScheduler(openLocker{},
		job{
			name:     "exploding",
			interval: 2 * time.Millisecond,
			lockKey:  1,
			run: func(context.Context) (jobResult, error) {
				exploded.Add(1)
				panic("the drawer fell out of the desk")
			},
		},
		job{
			name:     "healthy",
			interval: 2 * time.Millisecond,
			lockKey:  2,
			run: func(context.Context) (jobResult, error) {
				healthy.Add(1)
				return jobResult{}, nil
			},
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the scheduler: %v", err)
	}
	defer scheduler.Stop()

	// Three runs each proves the ticker kept firing after the first panic
	// rather than the goroutine dying with it.
	waitFor(t, 3*time.Second, func() bool {
		return exploded.Load() >= 3 && healthy.Load() >= 3
	}, func() string {
		return "exploding ran " + itoa(exploded.Load()) + " times, healthy " + itoa(healthy.Load())
	})
}

func TestSchedulerStartRefusesASecondStart(t *testing.T) {
	scheduler := newTestScheduler(openLocker{}, job{
		name:     "quiet",
		interval: time.Hour,
		lockKey:  3,
		run:      func(context.Context) (jobResult, error) { return jobResult{}, nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the scheduler: %v", err)
	}
	defer scheduler.Stop()

	if err := scheduler.Start(ctx); err == nil {
		t.Error("starting an already running scheduler must be refused")
	}
}

// The lock is what keeps two API replicas from running the same nightly job
// twice. A replica that loses it does nothing at all and waits for its next
// tick.
func TestSchedulerSkipsAJobAnotherReplicaHolds(t *testing.T) {
	var ran atomic.Int64

	scheduler := newTestScheduler(heldLocker{}, job{
		name:     "reconciliation",
		interval: time.Millisecond,
		lockKey:  lockKeyReconciliation,
		run: func(context.Context) (jobResult, error) {
			ran.Add(1)
			return jobResult{}, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the scheduler: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	scheduler.Stop()

	if got := ran.Load(); got != 0 {
		t.Errorf("job ran %d times while another replica held the lock, want 0", got)
	}
}

// Cancelling the context stops every job, so a process shutting down does not
// leave a goroutine ticking against a closing pool.
func TestSchedulerStopsWhenTheContextIsCancelled(t *testing.T) {
	var ran atomic.Int64

	scheduler := newTestScheduler(openLocker{}, job{
		name:     "counter",
		interval: time.Millisecond,
		lockKey:  4,
		run: func(context.Context) (jobResult, error) {
			ran.Add(1)
			return jobResult{}, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	if err := scheduler.Start(ctx); err != nil {
		t.Fatalf("starting the scheduler: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return ran.Load() > 0 },
		func() string { return "the job never ran" })

	cancel()
	scheduler.Stop()

	settled := ran.Load()
	time.Sleep(30 * time.Millisecond)
	if got := ran.Load(); got != settled {
		t.Errorf("job ran %d more times after cancellation", got-settled)
	}
}

func TestSchedulerConfigFillsGapsRatherThanTickingContinuously(t *testing.T) {
	cfg := SchedulerConfig{ReconciliationInterval: time.Minute}.withDefaults()

	if cfg.ReconciliationInterval != time.Minute {
		t.Errorf("an explicit interval must survive, got %s", cfg.ReconciliationInterval)
	}
	// A zero interval would panic time.NewTicker, so every unset field has to
	// come back populated.
	if cfg.AdjustmentReaperInterval <= 0 || cfg.IdempotencyPurgeInterval <= 0 ||
		cfg.StalledImportInterval <= 0 || cfg.OverdueSnapshotInterval <= 0 ||
		cfg.JobTimeout <= 0 || cfg.ShutdownTimeout <= 0 || cfg.StartupStagger <= 0 {
		t.Errorf("an unset interval must fall back to its default, got %+v", cfg)
	}
}

// waitFor polls until the condition holds or the budget runs out, which keeps
// a timing test from being a fixed sleep long enough to be slow and short
// enough to be flaky.
func waitFor(t *testing.T, budget time.Duration, done func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", budget, describe())
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
