package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"flowed/internal/domain/shared"
)

type txContextKey struct{}

var txKey = txContextKey{}

func txFromContext(ctx context.Context) pgx.Tx {
	tx, _ := ctx.Value(txKey).(pgx.Tx)
	return tx
}

func contextWithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey, tx)
}

// TxOptions tunes a transaction.
type TxOptions struct {
	// Isolation defaults to ReadCommitted. Every money-mutating command in
	// this system takes explicit row locks in a fixed order, which gives
	// correctness without paying for serializable's abort rate at a busy desk.
	Isolation pgx.TxIsoLevel
	// ReadOnly marks reporting transactions, letting PostgreSQL skip some
	// bookkeeping and letting a reader be routed to a replica later.
	ReadOnly bool
	// MaxRetries bounds automatic retries on serialization failure and
	// deadlock. Only meaningful for transactions whose body is idempotent —
	// which every command here is, because the body re-reads its state after
	// taking locks.
	MaxRetries int
	// RetryBaseDelay is the first backoff step; each retry doubles it with
	// jitter so two colliding cashiers do not resynchronise on retry.
	RetryBaseDelay time.Duration
}

// DefaultTxOptions are the settings for a normal money-mutating command.
func DefaultTxOptions() TxOptions {
	return TxOptions{
		Isolation:      pgx.ReadCommitted,
		ReadOnly:       false,
		MaxRetries:     3,
		RetryBaseDelay: 10 * time.Millisecond,
	}
}

// ReadOnlyTxOptions are the settings for a reporting transaction.
func ReadOnlyTxOptions() TxOptions {
	return TxOptions{
		Isolation:      pgx.ReadCommitted,
		ReadOnly:       true,
		MaxRetries:     1,
		RetryBaseDelay: 10 * time.Millisecond,
	}
}

// SerializableTxOptions are for the rare command that cannot express its
// invariant as a row lock — closing an academic year, which must see a
// consistent view of every account in that year at once.
func SerializableTxOptions() TxOptions {
	return TxOptions{
		Isolation:      pgx.Serializable,
		ReadOnly:       false,
		MaxRetries:     5,
		RetryBaseDelay: 25 * time.Millisecond,
	}
}

// WithTx runs fn inside a transaction, committing on success and rolling back
// on any error or panic.
//
// If the context already carries a transaction, fn runs inside a savepoint
// instead of opening a second one. That composition is what lets a bulk
// command call a single-item command per row: the inner failure rolls back one
// row without discarding the batch.
//
// Retries apply only at the outermost level. Retrying a savepoint would not
// clear the conflict that aborted the enclosing transaction.
func (db *DB) WithTx(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) error {
	if existing := txFromContext(ctx); existing != nil {
		return db.withSavepoint(ctx, existing, fn)
	}

	var lastErr error
	attempts := opts.MaxRetries
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		err := db.runTx(ctx, opts, fn)
		if err == nil {
			if attempt > 1 {
				db.log.InfoContext(ctx, "transaction succeeded after retry", slog.Int("attempt", attempt))
			}
			return nil
		}
		lastErr = err

		if !isRetryable(err) || attempt == attempts {
			return err
		}

		delay := backoff(opts.RetryBaseDelay, attempt)
		db.log.WarnContext(ctx, "retrying transaction after transient conflict",
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", attempts),
			slog.Duration("delay", delay),
			slog.String("error", err.Error()),
		)

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while backing off for retry: %w", ctx.Err())
		case <-time.After(delay):
		}
	}

	return lastErr
}

func (db *DB) runTx(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) (err error) {
	accessMode := pgx.ReadWrite
	if opts.ReadOnly {
		accessMode = pgx.ReadOnly
	}

	tx, err := db.currentPool().BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   opts.Isolation,
		AccessMode: accessMode,
	})
	if err != nil {
		return TranslateError(fmt.Errorf("beginning transaction: %w", err))
	}

	txCtx := contextWithTx(ctx, tx)

	defer func() {
		if r := recover(); r != nil {
			// Roll back on a fresh context: the request context may already be
			// cancelled, and a rollback that cannot reach the server would
			// leave the connection holding locks until the pool recycles it.
			rollback(tx, db.log)
			panic(r)
		}
		if err != nil {
			rollback(tx, db.log)
		}
	}()

	if err = fn(txCtx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return TranslateError(fmt.Errorf("committing transaction: %w", err))
	}
	return nil
}

func (db *DB) withSavepoint(ctx context.Context, parent pgx.Tx, fn func(ctx context.Context) error) (err error) {
	sp, err := parent.Begin(ctx)
	if err != nil {
		return TranslateError(fmt.Errorf("creating savepoint: %w", err))
	}

	spCtx := contextWithTx(ctx, sp)

	defer func() {
		if r := recover(); r != nil {
			rollback(sp, db.log)
			panic(r)
		}
		if err != nil {
			rollback(sp, db.log)
		}
	}()

	if err = fn(spCtx); err != nil {
		return err
	}
	if err = sp.Commit(ctx); err != nil {
		return TranslateError(fmt.Errorf("releasing savepoint: %w", err))
	}
	return nil
}

func rollback(tx pgx.Tx, log *slog.Logger) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		log.Error("rollback failed", slog.String("error", err.Error()))
	}
}

func backoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	// Exponential with full jitter: two cashiers who collided must not retry
	// in lockstep and collide again.
	maxDelay := base * time.Duration(1<<uint(attempt-1))
	return time.Duration(rand.Int64N(int64(maxDelay)) + int64(base))
}

// ReadOnly runs fn in a read-only transaction, the standard wrapper for a
// report that must see one consistent snapshot across several queries.
func (db *DB) ReadOnly(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.WithTx(ctx, ReadOnlyTxOptions(), fn)
}

// RequireTx returns an invariant violation unless a transaction is open. Every
// money-mutating repository method calls this, so a service that forgets its
// transaction boundary fails immediately and loudly rather than writing
// partial state that looks fine until reconciliation night.
func (db *DB) RequireTx(ctx context.Context, operation string) error {
	if db.InTransaction(ctx) {
		return nil
	}
	return shared.InvariantViolation("missing_transaction",
		"%s must run inside a transaction but none is open", operation).
		WithDetail("operation", operation)
}

// AdvisoryLock takes a session-level advisory lock, used by the migration
// runner and by singleton background jobs so that two API replicas cannot run
// the nightly reconciliation twice.
func (db *DB) AdvisoryLock(ctx context.Context, key int64) (unlock func(), err error) {
	conn, err := db.currentPool().Acquire(ctx)
	if err != nil {
		return nil, TranslateError(fmt.Errorf("acquiring connection for advisory lock: %w", err))
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		return nil, TranslateError(fmt.Errorf("taking advisory lock %d: %w", key, err))
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			db.log.Error("releasing advisory lock", slog.Int64("key", key), slog.String("error", err.Error()))
		}
		conn.Release()
	}, nil
}

// TryAdvisoryLock takes an advisory lock without waiting, reporting whether it
// was acquired.
func (db *DB) TryAdvisoryLock(ctx context.Context, key int64) (acquired bool, unlock func(), err error) {
	conn, err := db.currentPool().Acquire(ctx)
	if err != nil {
		return false, nil, TranslateError(fmt.Errorf("acquiring connection for advisory lock: %w", err))
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		conn.Release()
		return false, nil, TranslateError(fmt.Errorf("attempting advisory lock %d: %w", key, err))
	}
	if !got {
		conn.Release()
		return false, nil, nil
	}
	return true, func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			db.log.Error("releasing advisory lock", slog.Int64("key", key), slog.String("error", err.Error()))
		}
		conn.Release()
	}, nil
}
