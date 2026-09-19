// Package pg owns the PostgreSQL connection pool, the transaction boundary,
// and the translation of driver errors into domain errors.
//
// The system talks to PostgreSQL through pgx natively rather than through
// database/sql. That choice is deliberate: database/sql erases pgx's typed
// parameter handling, forces an extra prepared-statement cache layer, and
// makes it awkward to reach the row-locking and batch APIs a financial
// workload depends on.
package pg

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"

	"flowed/internal/platform/config"
)

// Executor is the subset of pgx shared by a pool and a transaction. Every
// repository method takes one of these, so the same code runs inside or
// outside a transaction without a second implementation.
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Compile-time proof that both pgx types satisfy Executor.
var (
	_ Executor = (*pgxpool.Pool)(nil)
	_ Executor = (pgx.Tx)(nil)
)

// DB wraps the connection pool and resolves, per call, whether work should run
// on the pool or on a transaction already open in the context.
//
// The pool sits behind a mutex rather than being a plain field because of one
// caller: the backup system's live-database restore swaps the database out
// from under the running process (a database cannot be renamed while a
// connection to it is open) and then reopens the pool against the same name,
// now pointing at the restored data. That is the only code path that ever
// calls Drain or Reopen; every other caller sees a pool that never changes
// once Connect returns, and the lock they pay for it is one RLock per query.
type DB struct {
	mu     sync.RWMutex
	pool   *pgxpool.Pool
	log    *slog.Logger
	cfg    config.Database
	tracer pgx.QueryTracer
}

// Connect opens the pool, applies session defaults, and verifies the database
// is reachable before returning.
//
// The session defaults are the guardrails that keep one bad query from taking
// the cashier desks down: statement_timeout bounds runaway reports,
// lock_timeout stops a second cashier from blocking forever on a locked
// account, and idle_in_transaction_session_timeout reclaims locks held by a
// client that vanished mid-transaction.
//
// tracer may be nil, and is when tracing is off. pgx accepts exactly one
// tracer per connection, which is why it arrives as a parameter rather than
// being chosen here: query logging and span tracing are alternatives, and the
// choice between them belongs to configuration, which refuses the combination
// outright rather than letting one silently win.
func Connect(ctx context.Context, cfg config.Database, log *slog.Logger, tracer pgx.QueryTracer) (*DB, error) {
	pool, err := newPool(ctx, cfg, log, tracer)
	if err != nil {
		return nil, err
	}

	log.Info("database connected",
		slog.String("dsn", cfg.RedactedDSN()),
		slog.Int("max_conns", int(cfg.MaxConns)),
		slog.Int("min_conns", int(cfg.MinConns)),
		slog.Duration("statement_timeout", cfg.StatementTimeout),
		slog.Duration("lock_timeout", cfg.LockTimeout),
	)

	return &DB{pool: pool, log: log, cfg: cfg, tracer: tracer}, nil
}

// newPool builds and verifies one pool. Shared by Connect and Reopen so the
// two can never drift into building the pool differently.
func newPool(ctx context.Context, cfg config.Database, log *slog.Logger, tracer pgx.QueryTracer) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing database DSN: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	// Session parameters are set through the startup packet so every pooled
	// connection carries them from its first statement, with no round trip.
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "flowed-tuition"
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = millis(cfg.StatementTimeout)
	poolCfg.ConnConfig.RuntimeParams["lock_timeout"] = millis(cfg.LockTimeout)
	poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = millis(cfg.IdleInTransactionTimeout)
	// All timestamps are stored and returned in UTC; local rendering happens
	// at the presentation edge.
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	switch {
	case cfg.LogQueries:
		poolCfg.ConnConfig.Tracer = &tracelog.TraceLog{
			Logger:   pgxSlogAdapter{log: log},
			LogLevel: tracelog.LogLevelDebug,
		}
	case tracer != nil:
		poolCfg.ConnConfig.Tracer = tracer
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to %s: %w", cfg.RedactedDSN(), err)
	}
	return pool, nil
}

// NewDB wraps an existing pool, for tests that supply their own. Drain and
// Reopen are unusable on a DB built this way, since there is no config to
// reopen against — nothing in this codebase needs them outside the live
// restore path, and that path always runs against a DB built by Connect.
func NewDB(pool *pgxpool.Pool, log *slog.Logger) *DB {
	return &DB{pool: pool, log: log}
}

// Pool exposes the underlying pool for the few callers that genuinely need it,
// such as the migration runner and pool statistics.
func (db *DB) Pool() *pgxpool.Pool { return db.currentPool() }

func (db *DB) currentPool() *pgxpool.Pool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.pool
}

// Conn returns the executor for the current context: the open transaction if
// one is in flight, otherwise the pool. Repositories call this rather than
// choosing for themselves, which is what lets a service compose several
// repository calls into one atomic command without any of them knowing.
func (db *DB) Conn(ctx context.Context) Executor {
	if tx := txFromContext(ctx); tx != nil {
		return tx
	}
	return db.currentPool()
}

// InTransaction reports whether the context carries an open transaction. The
// financial services assert this before mutating, so a command can never be
// wired up in a way that writes half its rows outside a transaction.
func (db *DB) InTransaction(ctx context.Context) bool { return txFromContext(ctx) != nil }

// Ping verifies the database answers.
func (db *DB) Ping(ctx context.Context) error {
	pool := db.currentPool()
	if pool == nil {
		return fmt.Errorf("database ping: no pool is open")
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	return nil
}

// Drain closes the current pool without opening a replacement.
//
// This is the one moment nothing — including this process — holds the
// database open, which is what makes the rename in Runner.SwapLive legal. It
// exists only for the live-restore flow; ordinary shutdown uses Close, which
// is exactly Drain with no Reopen ever following it.
func (db *DB) Drain(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.pool != nil {
		db.pool.Close()
		db.pool = nil
	}
	return nil
}

// Reopen rebuilds the pool from the configuration Connect was given, after
// Drain closed it. Nothing about the configuration changes — the database
// name is exactly what it was — but a restore's swap has, in between, renamed
// what that name refers to, so the pool that opens here connects to the
// restored data rather than to what was live before Drain.
func (db *DB) Reopen(ctx context.Context) error {
	pool, err := newPool(ctx, db.cfg, db.log, db.tracer)
	if err != nil {
		return err
	}
	db.mu.Lock()
	db.pool = pool
	db.mu.Unlock()
	return nil
}

// Stats reports pool utilisation for the health endpoint and metrics.
func (db *DB) Stats() PoolStats {
	pool := db.currentPool()
	if pool == nil {
		return PoolStats{}
	}
	s := pool.Stat()
	return PoolStats{
		AcquiredConns:   s.AcquiredConns(),
		IdleConns:       s.IdleConns(),
		TotalConns:      s.TotalConns(),
		MaxConns:        s.MaxConns(),
		NewConnsCount:   s.NewConnsCount(),
		AcquireCount:    s.AcquireCount(),
		AcquireDuration: s.AcquireDuration(),
		EmptyAcquires:   s.EmptyAcquireCount(),
		CanceledAcquire: s.CanceledAcquireCount(),
	}
}

// PoolStats is a snapshot of pool utilisation.
type PoolStats struct {
	AcquiredConns   int32         `json:"acquired_conns"`
	IdleConns       int32         `json:"idle_conns"`
	TotalConns      int32         `json:"total_conns"`
	MaxConns        int32         `json:"max_conns"`
	NewConnsCount   int64         `json:"new_conns_count"`
	AcquireCount    int64         `json:"acquire_count"`
	AcquireDuration time.Duration `json:"acquire_duration"`
	EmptyAcquires   int64         `json:"empty_acquires"`
	CanceledAcquire int64         `json:"canceled_acquires"`
}

// Close drains the pool. Callers should have stopped accepting requests first.
func (db *DB) Close() {
	db.log.Info("closing database pool")
	_ = db.Drain(context.Background())
}

func millis(d time.Duration) string {
	return fmt.Sprintf("%d", d.Milliseconds())
}

// pgxSlogAdapter bridges pgx's tracelog interface onto slog.
type pgxSlogAdapter struct{ log *slog.Logger }

func (a pgxSlogAdapter) Log(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
	attrs := make([]slog.Attr, 0, len(data))
	for k, v := range data {
		attrs = append(attrs, slog.Any(k, v))
	}
	var slogLevel slog.Level
	switch level {
	case tracelog.LogLevelTrace, tracelog.LogLevelDebug:
		slogLevel = slog.LevelDebug
	case tracelog.LogLevelInfo:
		slogLevel = slog.LevelInfo
	case tracelog.LogLevelWarn:
		slogLevel = slog.LevelWarn
	default:
		slogLevel = slog.LevelError
	}
	a.log.LogAttrs(ctx, slogLevel, msg, attrs...)
}
