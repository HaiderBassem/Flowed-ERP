-- Dropping the shared limiter loses nothing: the buckets are throwaway state,
-- and a process rolled back to this schema falls back to its in-memory
-- limiter. The only consequence is that the configured limit becomes per
-- replica again, which is the defect migration 000012 was written to fix.

DROP FUNCTION IF EXISTS rate_limit_take(text, double precision, double precision);

DROP TABLE IF EXISTS rate_limit_bucket;
