package port

import (
	"context"
	"time"
)

// RateLimiter admits or refuses a request against a budget shared by every
// replica.
//
// It sits in this package for the same reason IdempotencyRepository does: the
// store belongs to the HTTP middleware rather than to any domain service, but
// the middleware must not name a concrete database type, and the wiring in
// cmd/api is the only place the two meet.
//
// Nothing here is financial and nothing here is durable. An implementation is
// free to lose its state on restart — that is the behaviour of the in-process
// limiter this interface generalises, and it is acceptable for a limiter in a
// way it is not acceptable for anything else this system stores.
type RateLimiter interface {
	// Take spends one token from the bucket named by key, refilling it first
	// for the time elapsed since it was last touched.
	//
	// capacity is the burst the bucket holds and window is how long a full
	// bucket takes to refill from empty, so a capacity of 600 over a minute is
	// 600 requests a minute with all 600 available at once.
	//
	// Implementations must not run inside the caller's transaction. The row
	// this touches is contended by every request from the same client, and
	// holding it for the length of a payment would serialise a cashier desk
	// behind its own rate limiter.
	Take(ctx context.Context, key string, capacity int, window time.Duration) (RateLimitDecision, error)

	// PurgeIdle deletes buckets untouched for longer than idleFor, returning
	// how many went. A bucket refills fully within its window, so one idle for
	// several windows holds no information; without the sweep the table grows
	// by a row per distinct client address and never shrinks.
	PurgeIdle(ctx context.Context, idleFor time.Duration) (int64, error)
}

// RateLimitDecision is the answer to one admission question.
type RateLimitDecision struct {
	// Allowed reports whether the caller may proceed.
	Allowed bool
	// Remaining is how many whole tokens are left after this decision. It is
	// reported to the client so a well-behaved one can pace itself instead of
	// discovering the limit by hitting it.
	Remaining int
	// RetryAfter is how long until one token exists again. Zero when allowed.
	RetryAfter time.Duration
}
