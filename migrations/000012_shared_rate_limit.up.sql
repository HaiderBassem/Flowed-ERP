-- Move the API rate limiter off per-process memory and into shared state.
--
-- The in-process limiter it replaces counted only the requests that reached
-- one replica. Behind a load balancer with N replicas the configured limit was
-- really N times the limit, every restart forgot every bucket, and no operator
-- reading the configuration could tell what the number actually meant.
--
-- The shared bucket lives here rather than in Redis because the university
-- already runs this database and does not already run Redis. A limiter is not
-- worth a second piece of infrastructure to keep alive, patch and back up —
-- and one more thing that can be down is one more way the cashier desks stop.

-- UNLOGGED on purpose, and the choice is load-bearing rather than an
-- optimisation. This table is written on every admitted request; logged, that
-- is a WAL record per request, replicated to every standby, for state whose
-- entire value expires within one window. Unlogged, PostgreSQL truncates it
-- after a crash — which resets the limiter to exactly the state a process
-- restart already produced under the old implementation, and is the correct
-- behaviour for a bucket rather than a loss to guard against.
--
-- Nothing financial may ever be stored here for the same reason.
CREATE UNLOGGED TABLE rate_limit_bucket (
    bucket_key  text             PRIMARY KEY,
    tokens      double precision NOT NULL,
    refilled_at timestamptz      NOT NULL
);

COMMENT ON TABLE rate_limit_bucket IS
    'Token buckets for the cross-replica API rate limiter. UNLOGGED: the contents '
    'are throwaway, and losing them on a crash resets the limiter rather than losing '
    'anything. Never store financial or identifying data here.';

COMMENT ON COLUMN rate_limit_bucket.bucket_key IS
    'Opaque limiter key, today the client IP. Not a foreign key to anything and not '
    'a student identifier.';

COMMENT ON COLUMN rate_limit_bucket.tokens IS
    'Tokens left as of refilled_at. Fractional because refill is continuous: rounding '
    'to whole tokens at this rate would drift the effective limit by several percent.';

-- The sweeper deletes by age, and without this it seq-scans a table that a busy
-- campus keeps thousands of rows in.
CREATE INDEX ix_rate_limit_bucket_idle ON rate_limit_bucket (refilled_at);

-- rate_limit_take spends one token, refilling first.
--
-- It is a function rather than a statement assembled in Go so that the refill
-- and the spend are one round trip and one row lock. Two statements would let
-- a second replica read the same bucket between them, and two replicas each
-- admitting a request the budget only covered once is precisely the defect
-- this migration exists to remove.
--
-- The lock is the ordinary row lock INSERT ... ON CONFLICT DO UPDATE takes, and
-- it is held to the end of the enclosing transaction. Callers must therefore
-- invoke this outside any transaction of their own: called inside a payment's
-- transaction it would hold a bucket contended by every other request on that
-- IP for the whole life of the payment. The limiter runs in middleware, before
-- any handler opens anything, and must stay there.
--
-- clock_timestamp() rather than now(): now() is the transaction's start time,
-- which for a limiter called once per statement is very nearly the same number
-- and, on the day somebody does call it inside a longer transaction, silently
-- wrong in the direction of granting free tokens.
--
-- One call per statement, always. Several calls inside one statement — a
-- LATERAL join over a list of keys, say, to "batch" the limiter — all read the
-- same snapshot, so every one of them behaves like the first and every one of
-- them is admitted. The failure is silent: the limiter still answers, it just
-- stops limiting. There is no batching win to chase here anyway, since each
-- request has to be decided before its handler runs.
CREATE FUNCTION rate_limit_take(
    p_key            text,
    p_capacity       double precision,
    p_refill_per_sec double precision
) RETURNS TABLE (
    allowed             boolean,
    tokens_left         double precision,
    retry_after_seconds double precision
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_tokens double precision;
BEGIN
    IF p_capacity <= 0 OR p_refill_per_sec <= 0 THEN
        RAISE EXCEPTION 'rate_limit_take: capacity and refill rate must both be positive'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    INSERT INTO rate_limit_bucket AS b (bucket_key, tokens, refilled_at)
         VALUES (p_key, p_capacity, clock_timestamp())
    ON CONFLICT (bucket_key) DO UPDATE
            SET tokens = LEAST(
                    p_capacity,
                    b.tokens + extract(epoch FROM (clock_timestamp() - b.refilled_at)) * p_refill_per_sec
                ),
                refilled_at = clock_timestamp()
      RETURNING b.tokens INTO v_tokens;

    IF v_tokens >= 1 THEN
        UPDATE rate_limit_bucket SET tokens = v_tokens - 1 WHERE bucket_key = p_key;
        RETURN QUERY SELECT true, v_tokens - 1, 0::double precision;
    ELSE
        -- Denied requests do not spend. Charging them would push a client that
        -- retries hard into a deficit it has to climb out of, so the limit a
        -- badly written retry loop actually experiences would be far below the
        -- configured one and impossible to explain from the configuration.
        RETURN QUERY SELECT false, v_tokens, (1 - v_tokens) / p_refill_per_sec;
    END IF;
END;
$$;

COMMENT ON FUNCTION rate_limit_take(text, double precision, double precision) IS
    'Atomically refill and spend one token. Call outside a transaction: the row lock '
    'it takes is held until the caller commits.';
