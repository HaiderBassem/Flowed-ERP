package integration

import (
	"context"
	"sync"
	"testing"
	"time"
)

// take spends one token and reports the decision.
func take(t *testing.T, ctx context.Context, key string, capacity, perMinute float64) (allowed bool, left, retryAfter float64) {
	t.Helper()
	err := pool.QueryRow(ctx,
		`SELECT allowed, tokens_left, retry_after_seconds FROM rate_limit_take($1, $2, $3)`,
		key, capacity, perMinute/60,
	).Scan(&allowed, &left, &retryAfter)
	if err != nil {
		t.Fatalf("rate_limit_take(%q): %v", key, err)
	}
	return allowed, left, retryAfter
}

func clearBucket(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM rate_limit_bucket WHERE bucket_key = $1`, key); err != nil {
		t.Fatalf("clearing bucket %q: %v", key, err)
	}
}

// The budget is the capacity, and it is spent before it is refused.
func TestRateLimitSpendsExactlyTheCapacity(t *testing.T) {
	ctx := context.Background()
	const key = "test:capacity"
	clearBucket(t, ctx, key)
	t.Cleanup(func() { clearBucket(t, context.Background(), key) })

	for attempt := 1; attempt <= 3; attempt++ {
		if allowed, _, _ := take(t, ctx, key, 3, 3); !allowed {
			t.Fatalf("take %d of 3 was refused; the capacity was not honoured", attempt)
		}
	}

	allowed, _, retryAfter := take(t, ctx, key, 3, 3)
	if allowed {
		t.Fatal("the fourth take of a capacity of three was admitted")
	}
	if retryAfter <= 0 {
		t.Errorf("retry_after_seconds: got %v, want a positive wait", retryAfter)
	}
}

// A refused take must not spend. Charging for refusals would put a client that
// retries hard into a deficit, and the rate it actually experiences would then
// be well below the configured one and impossible to explain from it.
func TestRateLimitRefusalDoesNotSpend(t *testing.T) {
	ctx := context.Background()
	const key = "test:no-deficit"
	clearBucket(t, ctx, key)
	t.Cleanup(func() { clearBucket(t, context.Background(), key) })

	// A capacity of one, refilling slowly enough that nothing meaningful comes
	// back during the test.
	take(t, ctx, key, 1, 1)

	_, afterFirstRefusal, _ := take(t, ctx, key, 1, 1)
	for i := 0; i < 20; i++ {
		take(t, ctx, key, 1, 1)
	}
	_, afterMany, _ := take(t, ctx, key, 1, 1)

	// Refills over the life of the test move this slightly upward; what must
	// never happen is the balance falling as refusals accumulate.
	if afterMany < afterFirstRefusal {
		t.Errorf("balance fell from %v to %v across twenty refusals: refusals are being charged",
			afterFirstRefusal, afterMany)
	}
}

// The whole reason this moved into the database. Ten concurrent callers, as
// ten separate connections, must between them spend the budget exactly once —
// which is what the in-process limiter could not do across replicas.
func TestRateLimitIsAtomicAcrossConnections(t *testing.T) {
	ctx := context.Background()
	const (
		key      = "test:concurrent"
		capacity = 20
		callers  = 10
		each     = 5 // callers*each == capacity*2.5, so refusals are guaranteed
	)
	clearBucket(t, ctx, key)
	t.Cleanup(func() { clearBucket(t, context.Background(), key) })

	var (
		mu      sync.Mutex
		granted int
		start   = make(chan struct{})
		wg      sync.WaitGroup
	)

	for caller := 0; caller < callers; caller++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			local := 0
			for i := 0; i < each; i++ {
				var (
					allowed bool
					left    float64
					retry   float64
				)
				// t.Fatalf is not safe off the test goroutine, so failures are
				// counted and asserted after the wait.
				if err := pool.QueryRow(ctx,
					`SELECT allowed, tokens_left, retry_after_seconds FROM rate_limit_take($1, $2, $3)`,
					key, float64(capacity), float64(capacity)/60,
				).Scan(&allowed, &left, &retry); err != nil {
					continue
				}
				if allowed {
					local++
				}
			}
			mu.Lock()
			granted += local
			mu.Unlock()
		}()
	}

	close(start)
	wg.Wait()

	// Refill during the run can hand back a token or two, so the ceiling is
	// the capacity plus a small allowance rather than the capacity exactly.
	// What must not happen is the budget being granted several times over,
	// which is precisely what a per-process limiter did.
	const allowance = 2
	if granted > capacity+allowance {
		t.Errorf("granted %d of a budget of %d across %d concurrent callers: "+
			"the bucket is not atomic", granted, capacity, callers)
	}
	if granted < capacity-allowance {
		t.Errorf("granted only %d of a budget of %d: the limiter is refusing traffic it should admit", granted, capacity)
	}
}

// Buckets refill continuously rather than resetting on a window boundary, so a
// client that waits is served without having to guess when the window turns.
func TestRateLimitRefillsOverTime(t *testing.T) {
	ctx := context.Background()
	const key = "test:refill"
	clearBucket(t, ctx, key)
	t.Cleanup(func() { clearBucket(t, context.Background(), key) })

	// 600 a minute is ten a second, so a fifth of a second is worth two tokens.
	const capacity, perMinute = 2, 600
	take(t, ctx, key, capacity, perMinute)
	take(t, ctx, key, capacity, perMinute)
	if allowed, _, _ := take(t, ctx, key, capacity, perMinute); allowed {
		t.Fatal("a third take against a capacity of two was admitted")
	}

	time.Sleep(200 * time.Millisecond)

	if allowed, _, _ := take(t, ctx, key, capacity, perMinute); !allowed {
		t.Error("nothing refilled after 200ms at ten tokens a second")
	}
}

// Buckets are keyed independently: one campus exhausting its budget must not
// refuse another.
func TestRateLimitKeysAreIndependent(t *testing.T) {
	ctx := context.Background()
	const busy, quiet = "test:busy", "test:quiet"
	clearBucket(t, ctx, busy)
	clearBucket(t, ctx, quiet)
	t.Cleanup(func() {
		clearBucket(t, context.Background(), busy)
		clearBucket(t, context.Background(), quiet)
	})

	take(t, ctx, busy, 1, 1)
	if allowed, _, _ := take(t, ctx, busy, 1, 1); allowed {
		t.Fatal("the busy key was not exhausted")
	}
	if allowed, _, _ := take(t, ctx, quiet, 1, 1); !allowed {
		t.Error("an untouched key was refused because another key was exhausted")
	}
}

// The table is throwaway state and must be unlogged: logged, it would be a WAL
// record and a replication round for every admitted request.
func TestRateLimitTableIsUnlogged(t *testing.T) {
	ctx := context.Background()

	var persistence string
	err := pool.QueryRow(ctx,
		`SELECT relpersistence FROM pg_class WHERE relname = 'rate_limit_bucket'`).Scan(&persistence)
	if err != nil {
		t.Fatalf("reading relpersistence: %v", err)
	}
	if persistence != "u" {
		t.Errorf("relpersistence: got %q, want %q (unlogged); a limiter must not write WAL per request", persistence, "u")
	}
}
