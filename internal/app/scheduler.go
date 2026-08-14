package app

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// Advisory lock keys for the singleton jobs.
//
// Fixed constants rather than a hash of the job name: a hash collision would
// silently serialise two unrelated jobs and the symptom — one of them never
// seeming to run — would be very hard to trace back. They sit in the same
// numeric family as the migration runner's key and are visibly distinct from it.
const (
	lockKeyReconciliation   int64 = 8_531_224_907_150_001
	lockKeyAdjustmentReaper int64 = 8_531_224_907_150_002
	lockKeyIdempotencyPurge int64 = 8_531_224_907_150_003
	lockKeyStalledImports   int64 = 8_531_224_907_150_004
	lockKeyOverdueSnapshot  int64 = 8_531_224_907_150_005
	lockKeyRateLimitPurge   int64 = 8_531_224_907_150_006
	lockKeySessionPurge     int64 = 8_531_224_907_150_007
)

// SchedulerConfig tunes the background jobs. The zero value is not usable;
// DefaultSchedulerConfig supplies the intervals, and any field left
// non-positive falls back to its default rather than producing a ticker that
// fires continuously or panics.
type SchedulerConfig struct {
	// ReconciliationInterval is how often cached account totals are compared
	// against the transaction rows. Nightly in production.
	ReconciliationInterval time.Duration
	// ReconciliationLimit bounds how many drifting accounts one pass reports.
	// The count matters more than the list: one is already a defect.
	ReconciliationLimit int
	// AdjustmentReaperInterval is how often expired reopening windows are
	// closed. Short, because the window is the whole point of the control.
	AdjustmentReaperInterval time.Duration
	// IdempotencyPurgeInterval is how often expired idempotency records go.
	IdempotencyPurgeInterval time.Duration
	// StalledImportInterval is how often import batches are checked for a
	// stopped heartbeat.
	StalledImportInterval time.Duration
	// StalledImportAfter is how long a batch may go without a heartbeat before
	// the worker is presumed dead. Generous relative to the heartbeat period:
	// failing a batch that is merely slow would be worse than leaving it.
	StalledImportAfter time.Duration
	// OverdueSnapshotInterval is how often the overdue position is logged.
	OverdueSnapshotInterval time.Duration
	// RateLimitPurgeInterval is how often idle rate-limit buckets are deleted.
	RateLimitPurgeInterval time.Duration
	// RateLimitIdleTTL is how long a bucket survives untouched. Several refill
	// windows: a bucket idle that long is full, and a full bucket is
	// indistinguishable from one that does not exist.
	RateLimitIdleTTL time.Duration
	// JobTimeout bounds a single run so a job that hangs cannot hold its
	// advisory lock until the process dies.
	JobTimeout time.Duration
	// ShutdownTimeout bounds how long Stop waits for jobs in flight.
	ShutdownTimeout time.Duration
	// StartupStagger spaces the first pass of each job so start-up does not
	// fire every query at once.
	StartupStagger time.Duration
	// RunOnStart runs each job once shortly after start-up instead of waiting a
	// full interval. Without it a daily job on a service that is redeployed
	// every afternoon would never run at all.
	RunOnStart bool

	// SessionPurgeInterval is how often expired sessions and old login
	// attempts are deleted.
	SessionPurgeInterval time.Duration
	// LoginAttemptRetention is how long sign-in attempts are kept. Long enough
	// that an investigation opened weeks after a disputed receipt can still see
	// whether the account was being guessed; short enough that the table does
	// not become a permanent record of who signed in from where.
	LoginAttemptRetention time.Duration

	// Sessions and LoginAttempts are the stores the purge job sweeps. Left nil
	// — by a test, or a deployment that has not migrated yet — the job is not
	// registered at all rather than registered and failing every tick.
	Sessions      port.SessionRepository
	LoginAttempts port.LoginAttemptRepository
}

// DefaultSchedulerConfig is the production schedule.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		ReconciliationInterval:   24 * time.Hour,
		ReconciliationLimit:      100,
		AdjustmentReaperInterval: 15 * time.Minute,
		IdempotencyPurgeInterval: 24 * time.Hour,
		StalledImportInterval:    10 * time.Minute,
		StalledImportAfter:       15 * time.Minute,
		OverdueSnapshotInterval:  24 * time.Hour,
		RateLimitPurgeInterval:   15 * time.Minute,
		RateLimitIdleTTL:         10 * time.Minute,
		JobTimeout:               5 * time.Minute,
		ShutdownTimeout:          30 * time.Second,
		StartupStagger:           15 * time.Second,
		RunOnStart:               true,
		SessionPurgeInterval:     6 * time.Hour,
		LoginAttemptRetention:    90 * 24 * time.Hour,
	}
}

// withDefaults replaces anything non-positive, so a partially filled config is
// a valid one and a zero interval can never reach time.NewTicker.
func (c SchedulerConfig) withDefaults() SchedulerConfig {
	d := DefaultSchedulerConfig()
	if c.ReconciliationInterval <= 0 {
		c.ReconciliationInterval = d.ReconciliationInterval
	}
	if c.ReconciliationLimit <= 0 {
		c.ReconciliationLimit = d.ReconciliationLimit
	}
	if c.AdjustmentReaperInterval <= 0 {
		c.AdjustmentReaperInterval = d.AdjustmentReaperInterval
	}
	if c.SessionPurgeInterval <= 0 {
		c.SessionPurgeInterval = d.SessionPurgeInterval
	}
	if c.LoginAttemptRetention <= 0 {
		c.LoginAttemptRetention = d.LoginAttemptRetention
	}
	if c.IdempotencyPurgeInterval <= 0 {
		c.IdempotencyPurgeInterval = d.IdempotencyPurgeInterval
	}
	if c.StalledImportInterval <= 0 {
		c.StalledImportInterval = d.StalledImportInterval
	}
	if c.StalledImportAfter <= 0 {
		c.StalledImportAfter = d.StalledImportAfter
	}
	if c.OverdueSnapshotInterval <= 0 {
		c.OverdueSnapshotInterval = d.OverdueSnapshotInterval
	}
	if c.RateLimitPurgeInterval <= 0 {
		c.RateLimitPurgeInterval = d.RateLimitPurgeInterval
	}
	if c.RateLimitIdleTTL <= 0 {
		c.RateLimitIdleTTL = d.RateLimitIdleTTL
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = d.JobTimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = d.ShutdownTimeout
	}
	if c.StartupStagger <= 0 {
		c.StartupStagger = d.StartupStagger
	}
	return c
}

// advisoryLocker is the slice of the database the runner needs in order to stop
// two API replicas doing the same nightly work twice. It is an interface so the
// runner can be exercised without PostgreSQL; *pg.DB is the real implementation.
type advisoryLocker interface {
	TryAdvisoryLock(ctx context.Context, key int64) (acquired bool, unlock func(), err error)
}

// job is one scheduled routine.
type job struct {
	name     string
	interval time.Duration
	lockKey  int64
	run      func(ctx context.Context) (jobResult, error)
}

// jobResult is what a run wants said about it in the log.
type jobResult struct {
	// Defect marks an outcome that should never occur. Reconciliation sets it
	// when any account has drifted: an empty result is the expected outcome and
	// a single row is a bug, so it must not be logged at the same level as
	// success.
	Defect bool
	Attrs  []slog.Attr
}

// Scheduler runs the periodic safety routines.
//
// It is deliberately a ticker and a goroutine per job rather than a cron
// library. The schedule here is "every so often", never "at 03:15 on the first
// Tuesday", and a dependency that parses crontabs would buy nothing while
// adding a surface to keep up to date. What matters is what surrounds each run:
// an advisory lock so replicas do not duplicate work, a recover so one bad job
// cannot take the others down, and a log line whatever happens.
type Scheduler struct {
	deps        Deps
	db          *pg.DB
	lock        advisoryLocker
	idempotency port.IdempotencyRepository
	// rateLimiter is nil when the limiter runs per process, in which case
	// there are no shared buckets to sweep and the job is not registered.
	rateLimiter port.RateLimiter
	years       *YearService
	cfg         SchedulerConfig
	log         *slog.Logger
	// actor is the identity every job runs under. It holds no interactive
	// roles: background work runs this fixed set of routines and nothing else,
	// so granting it authority would be a standing back door.
	actor shared.Actor

	jobs []job

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

// NewScheduler wires the background jobs.
//
// The idempotency repository is a separate parameter because it is not on Deps:
// it is the one store the HTTP middleware owns rather than the services, and
// only the purge job needs it here.
func NewScheduler(
	d Deps,
	db *pg.DB,
	idempotency port.IdempotencyRepository,
	rateLimiter port.RateLimiter,
	cfg SchedulerConfig,
) *Scheduler {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	cfg = cfg.withDefaults()

	s := &Scheduler{
		deps:        d,
		db:          db,
		lock:        db,
		idempotency: idempotency,
		rateLimiter: rateLimiter,
		years:       NewYearService(d),
		cfg:         cfg,
		log:         log.With(slog.String("component", "scheduler")),
		actor:       shared.SystemActor(),
	}

	s.jobs = []job{
		{
			name:     "reconciliation",
			interval: cfg.ReconciliationInterval,
			lockKey:  lockKeyReconciliation,
			run:      s.runReconciliation,
		},
		{
			name:     "adjustment_window_reaper",
			interval: cfg.AdjustmentReaperInterval,
			lockKey:  lockKeyAdjustmentReaper,
			run:      s.runAdjustmentReaper,
		},
		{
			name:     "idempotency_purge",
			interval: cfg.IdempotencyPurgeInterval,
			lockKey:  lockKeyIdempotencyPurge,
			run:      s.runIdempotencyPurge,
		},
		{
			name:     "stalled_import_reaper",
			interval: cfg.StalledImportInterval,
			lockKey:  lockKeyStalledImports,
			run:      s.runStalledImportReaper,
		},
		{
			name:     "overdue_snapshot",
			interval: cfg.OverdueSnapshotInterval,
			lockKey:  lockKeyOverdueSnapshot,
			run:      s.runOverdueSnapshot,
		},
	}

	if cfg.Sessions != nil && cfg.LoginAttempts != nil {
		s.jobs = append(s.jobs, job{
			name:     "session_purge",
			interval: cfg.SessionPurgeInterval,
			lockKey:  lockKeySessionPurge,
			run:      s.runSessionPurge,
		})
	}

	if rateLimiter != nil {
		s.jobs = append(s.jobs, job{
			name:     "rate_limit_purge",
			interval: cfg.RateLimitPurgeInterval,
			lockKey:  lockKeyRateLimitPurge,
			run:      s.runRateLimitPurge,
		})
	}

	return s
}

// Start launches every job. Cancelling ctx stops them all; so does Stop.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return shared.PreconditionFailed("scheduler.already_started",
			"the scheduler is already running")
	}

	jobCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true

	for i, j := range s.jobs {
		s.wg.Add(1)
		go s.loop(jobCtx, j, time.Duration(i)*s.cfg.StartupStagger)
	}

	s.log.Info("scheduler started",
		slog.Int("jobs", len(s.jobs)),
		slog.String("actor", s.actor.Username))
	return nil
}

// Stop cancels the jobs and waits for anything in flight, bounded.
//
// The bound matters in both directions. A reconciliation cut off mid-scan costs
// nothing — it reads and reports, it does not write. But a process that exits
// while a job still holds its advisory lock leaves the next replica skipping
// that job until PostgreSQL notices the connection is gone, so the wait is real
// rather than a formality.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		s.log.Info("scheduler stopped")
	case <-time.After(s.cfg.ShutdownTimeout):
		s.log.Warn("scheduler shutdown timed out with jobs still in flight",
			slog.Duration("waited", s.cfg.ShutdownTimeout))
	}
}

// loop ticks one job until the context is cancelled.
func (s *Scheduler) loop(ctx context.Context, j job, startupDelay time.Duration) {
	defer s.wg.Done()

	if s.cfg.RunOnStart {
		timer := time.NewTimer(startupDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.runJob(ctx, j)
		}
	}

	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runJob(ctx, j)
		}
	}
}

// runJob performs one pass: take the lock, run, log.
//
// A panic is recovered here rather than left to kill the goroutine. One job
// with a nil dereference must not silently stop the other four — the next tick
// tries again, and the error log says what happened.
func (s *Scheduler) runJob(ctx context.Context, j job) {
	defer func() {
		if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "scheduled job panicked",
				slog.String("job", j.name),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())))
		}
	}()

	// Only one replica runs a given job at a time. Two replicas reconciling at
	// once would double the scan for no benefit; two replicas reaping the same
	// stalled import would race on the same rows.
	acquired, unlock, err := s.lock.TryAdvisoryLock(ctx, j.lockKey)
	if err != nil {
		s.log.ErrorContext(ctx, "scheduled job could not take its advisory lock",
			slog.String("job", j.name),
			slog.Int64("lock_key", j.lockKey),
			slog.String("error", err.Error()))
		return
	}
	if !acquired {
		s.log.DebugContext(ctx, "scheduled job skipped; another replica holds the lock",
			slog.String("job", j.name))
		return
	}
	defer unlock()

	runCtx := ctx
	if s.cfg.JobTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.cfg.JobTimeout)
		defer cancel()
	}

	s.log.InfoContext(runCtx, "scheduled job starting", slog.String("job", j.name))

	// Duration is measured off the wall clock's monotonic reading rather than
	// the injected clock: a fixed clock in a test would report every run as
	// instantaneous, and this figure exists to spot a job getting slower.
	started := time.Now()
	result, err := j.run(runCtx)
	elapsed := time.Since(started)

	attrs := make([]slog.Attr, 0, len(result.Attrs)+3)
	attrs = append(attrs,
		slog.String("job", j.name),
		slog.String("actor", s.actor.Username),
		slog.Duration("duration", elapsed))
	attrs = append(attrs, result.Attrs...)

	// The three outcomes are kept apart on the metric as well as in the log.
	// Reconciliation finding drift is not the job failing — it is the job
	// working — and an alert that cannot tell the two apart fires on the wrong
	// one.
	outcome := "ok"
	switch {
	case err != nil:
		outcome = "failed"
		attrs = append(attrs, slog.String("error", err.Error()))
		s.log.LogAttrs(runCtx, slog.LevelError, "scheduled job failed", attrs...)
	case result.Defect:
		outcome = "defect"
		s.log.LogAttrs(runCtx, slog.LevelError, "scheduled job found a defect", attrs...)
	default:
		s.log.LogAttrs(runCtx, slog.LevelInfo, "scheduled job finished", attrs...)
	}

	s.deps.Metrics.SchedulerJobFinished(runCtx, j.name, outcome, elapsed)
}

// runReconciliation compares every account's cached totals against its
// transaction rows.
//
// An empty result is the expected outcome and any row is a defect, which is why
// a clean pass is INFO and a dirty one is ERROR rather than both being noise at
// the same level.
//
// Nothing is corrected. A job that quietly rewrote a cached total to match the
// computed one would erase the only evidence of the code path that wrote money
// outside its transaction, and the same bug would keep drifting accounts every
// night with nobody the wiser. The repair is a human reading this log and
// finding the write that escaped.
func (s *Scheduler) runReconciliation(ctx context.Context) (jobResult, error) {
	drift, err := s.deps.Accounts.ReconciliationDrift(ctx, s.cfg.ReconciliationLimit)
	if err != nil {
		return jobResult{}, err
	}
	if len(drift) == 0 {
		return jobResult{Attrs: []slog.Attr{slog.Int("drifting_accounts", 0)}}, nil
	}

	accounts := make([]string, 0, len(drift))
	for _, row := range drift {
		accounts = append(accounts, row.AccountID.String())
	}
	return jobResult{
		Defect: true,
		Attrs: []slog.Attr{
			slog.Int("drifting_accounts", len(drift)),
			slog.Any("account_ids", accounts),
			slog.Bool("invariant_violation", true),
			slog.String("remedy", "inspect them at /api/v1/oversight/reconciliation; do not edit the cached totals"),
		},
	}, nil
}

// runAdjustmentReaper closes reopening windows that have run out.
//
// A year reopened "for the afternoon" stays writable until somebody remembers
// to shut it, which is how a controlled exception becomes a standing hole in
// closed books. The window is only a control if something enforces it.
func (s *Scheduler) runAdjustmentReaper(ctx context.Context) (jobResult, error) {
	closed, err := s.years.CloseExpiredAdjustmentWindows(ctx)
	if err != nil {
		return jobResult{}, err
	}
	return jobResult{Attrs: []slog.Attr{slog.Int("windows_closed", closed)}}, nil
}

// runIdempotencyPurge deletes idempotency records past their expiry.
//
// These are not an audit trail — the audit log is, and it is append-only and
// hash-chained. An idempotency record only has to outlive the retry window of
// the command it guarded, so deleting an expired one loses nothing and keeps
// the table from growing without bound at a busy desk.
func (s *Scheduler) runIdempotencyPurge(ctx context.Context) (jobResult, error) {
	deleted, err := s.idempotency.PurgeExpired(ctx, nowOr(s.deps.Clock))
	if err != nil {
		return jobResult{}, err
	}
	return jobResult{Attrs: []slog.Attr{slog.Int64("records_purged", deleted)}}, nil
}

// runSessionPurge deletes expired sessions and old sign-in attempts.
//
// Neither is an audit trail. The audit log records the sign-in and the
// revocation and is append-only and hash-chained; these two tables exist to
// make revocation possible and throttling correct, and both lose their value
// the moment they are past. Left to grow, the session table is read on every
// refresh and the attempt table on every login, so the cost of keeping them
// forever lands on the login path.
func (s *Scheduler) runSessionPurge(ctx context.Context) (jobResult, error) {
	now := nowOr(s.deps.Clock)

	sessions, err := s.cfg.Sessions.PurgeExpired(ctx, now)
	if err != nil {
		return jobResult{}, err
	}
	attempts, err := s.cfg.LoginAttempts.PurgeBefore(ctx, now.Add(-s.cfg.LoginAttemptRetention))
	if err != nil {
		return jobResult{}, err
	}
	return jobResult{Attrs: []slog.Attr{
		slog.Int64("sessions_purged", sessions),
		slog.Int64("login_attempts_purged", attempts),
	}}, nil
}

// runStalledImportReaper fails import batches whose worker stopped reporting.
//
// A worker killed mid-batch leaves the row in "importing" with nothing to
// notice it, and the batch stays there forever: no operator can retry it,
// because as far as the system is concerned it is still running.
//
// The table is queried directly rather than through a repository. The import
// repository belongs to another part of the system and this job must not
// couple to it for one UPDATE; the schema already carries the index this
// predicate needs.
func (s *Scheduler) runStalledImportReaper(ctx context.Context) (jobResult, error) {
	// A batch that entered "importing" and died before its first heartbeat has
	// no heartbeat_at at all, so the fallbacks matter: without them exactly the
	// worst-stuck batches would be the ones the reaper never touched.
	const query = `
		UPDATE import_batch
		   SET status        = 'failed',
		       error_summary = coalesce(error_summary || ' | ', '') || $2,
		       completed_at  = now()
		 WHERE status = 'importing'
		   AND coalesce(heartbeat_at, started_at, created_at) < $1
		RETURNING id`

	cutoff := nowOr(s.deps.Clock).Add(-s.cfg.StalledImportAfter)
	summary := fmt.Sprintf(
		"marked failed by the stalled-import reaper: no progress reported for over %s, "+
			"so the worker is presumed dead; re-run the import",
		s.cfg.StalledImportAfter)

	rows, err := s.db.Conn(ctx).Query(ctx, query, cutoff, summary)
	if err != nil {
		return jobResult{}, pg.WrapQuery("scheduler.reapStalledImports", err)
	}
	defer rows.Close()

	reaped := make([]string, 0, 4)
	for rows.Next() {
		var id shared.ID
		if err := rows.Scan(&id); err != nil {
			return jobResult{}, pg.WrapQuery("scheduler.reapStalledImports.scan", err)
		}
		reaped = append(reaped, id.String())
	}
	if err := rows.Err(); err != nil {
		return jobResult{}, pg.WrapQuery("scheduler.reapStalledImports.rows", err)
	}

	attrs := []slog.Attr{slog.Int("batches_failed", len(reaped))}
	if len(reaped) > 0 {
		// Named rather than counted: somebody has to go and re-run these, and a
		// count alone does not say which.
		attrs = append(attrs, slog.Any("batch_ids", reaped))
	}
	return jobResult{Attrs: attrs}, nil
}

// runRateLimitPurge deletes rate-limit buckets nobody has touched recently.
//
// Without it the table grows by one row per distinct client address and never
// shrinks, which a port scan or a large NAT range turns into a slow leak — the
// same leak the in-process limiter's sweeper exists to prevent, now in a place
// that survives a restart and so cannot be cleared by one.
func (s *Scheduler) runRateLimitPurge(ctx context.Context) (jobResult, error) {
	deleted, err := s.rateLimiter.PurgeIdle(ctx, s.cfg.RateLimitIdleTTL)
	if err != nil {
		return jobResult{}, err
	}
	return jobResult{Attrs: []slog.Attr{slog.Int64("buckets_purged", deleted)}}, nil
}

// runOverdueSnapshot records the overdue position in the log.
//
// The reports can already answer this, but only when somebody opens them. A
// daily line in the log means the trend is visible in whatever collects logs,
// and a jump on one particular day can be found afterwards even if nobody was
// watching a dashboard at the time.
func (s *Scheduler) runOverdueSnapshot(ctx context.Context) (jobResult, error) {
	const query = `
		SELECT count(*), coalesce(sum(remaining), 0)::bigint
		FROM v_installment_status
		WHERE is_overdue`

	var (
		count int64
		total money.Amount
	)
	if err := s.db.Conn(ctx).QueryRow(ctx, query).Scan(&count, &total); err != nil {
		return jobResult{}, pg.WrapQuery("scheduler.overdueSnapshot", err)
	}
	return jobResult{Attrs: []slog.Attr{
		slog.Int64("overdue_installments", count),
		slog.Int64("overdue_amount", total.Int64()),
	}}, nil
}
