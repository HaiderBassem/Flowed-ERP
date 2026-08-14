-- Restoring the one-reversal-per-allocation rule fails if any allocation has
-- already been unwound in parts, which is the correct outcome: the index
-- cannot be recreated over data that legitimately violates it, and silently
-- discarding those reversals would lose records of money returned.

DROP INDEX IF EXISTS ix_allocation_reverses;

CREATE UNIQUE INDEX uq_allocation_reversal
    ON payment_allocation (reverses_allocation_id)
    WHERE reverses_allocation_id IS NOT NULL;
