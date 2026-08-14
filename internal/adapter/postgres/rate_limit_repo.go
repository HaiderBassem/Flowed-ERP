package postgres

import (
	"context"
	"math"
	"time"

	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

// RateLimitRepository is the cross-replica token bucket.
type RateLimitRepository struct{ db *pg.DB }

// NewRateLimitRepository builds the shared limiter over a connection pool.
func NewRateLimitRepository(db *pg.DB) *RateLimitRepository {
	return &RateLimitRepository{db: db}
}

var _ port.RateLimiter = (*RateLimitRepository)(nil)

// Every statement here runs on the pool rather than on db.Conn(ctx), which
// would enrol in whatever transaction the context carries.
//
// That is not defensive tidiness. The bucket row is contended by every request
// from the same client address, and rate_limit_take holds a row lock on it
// until the enclosing transaction commits. Inside a payment's transaction one
// admitted request would hold that lock for the whole payment, and every other
// request from the same campus would queue behind it — a limiter that
// serialises the desks it exists to protect. The middleware calls this before
// any handler opens a transaction, and pinning the pool here means it stays
// true even if that ordering is ever disturbed.

// Take spends one token, refilling for elapsed time first.
func (r *RateLimitRepository) Take(
	ctx context.Context, key string, capacity int, window time.Duration,
) (port.RateLimitDecision, error) {
	if capacity <= 0 || window <= 0 {
		return port.RateLimitDecision{Allowed: true}, nil
	}

	const query = `SELECT allowed, tokens_left, retry_after_seconds
	                 FROM rate_limit_take($1, $2, $3)`

	refillPerSecond := float64(capacity) / window.Seconds()

	var (
		allowed    bool
		tokensLeft float64
		retryAfter float64
	)
	err := r.db.Pool().
		QueryRow(ctx, query, key, float64(capacity), refillPerSecond).
		Scan(&allowed, &tokensLeft, &retryAfter)
	if err != nil {
		return port.RateLimitDecision{}, pg.WrapQuery("rateLimit.take", err)
	}

	return port.RateLimitDecision{
		Allowed:   allowed,
		Remaining: int(math.Max(0, math.Floor(tokensLeft))),
		// Rounded up: a Retry-After that rounds down tells the client to come
		// back a fraction of a second before there is anything for it, and the
		// polite client is then refused for being punctual.
		RetryAfter: time.Duration(math.Ceil(retryAfter*1000)) * time.Millisecond,
	}, nil
}

// PurgeIdle deletes buckets nobody has touched recently.
func (r *RateLimitRepository) PurgeIdle(ctx context.Context, idleFor time.Duration) (int64, error) {
	const query = `DELETE FROM rate_limit_bucket WHERE refilled_at < $1`

	cutoff := time.Now().UTC().Add(-idleFor)
	tag, err := r.db.Pool().Exec(ctx, query, cutoff)
	if err != nil {
		return 0, pg.WrapQuery("rateLimit.purgeIdle", err)
	}
	return tag.RowsAffected(), nil
}
