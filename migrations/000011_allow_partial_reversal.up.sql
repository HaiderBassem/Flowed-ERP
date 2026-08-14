-- Allow an allocation to be unwound in parts.
--
-- The original design let a refund reverse an allocation only in full, guarded
-- by a unique index permitting one reversal row per allocation. That reads as
-- the safer choice and is in fact unusable: a student who paid 1,100,000 and
-- asks for 300,000 back cannot be served when every allocation happens to be
-- larger than the amount left to unwind, and the cashier is told the refund is
-- impossible for a reason no student will accept.
--
-- What actually prevents two refunds from unwinding the same money is the row
-- lock every money command takes on the financial account. Both refunds
-- serialise there, and the second sees what the first already reversed. The
-- unique index was never the real guard; it only looked like one.
--
-- Replaced with an index that supports the sum check the application performs
-- under that lock: reversals against one allocation may never exceed it.

DROP INDEX IF EXISTS uq_allocation_reversal;

CREATE INDEX ix_allocation_reverses
    ON payment_allocation (reverses_allocation_id)
    WHERE reverses_allocation_id IS NOT NULL;

COMMENT ON COLUMN payment_allocation.reverses_allocation_id IS
    'The allocation this row unwinds, in whole or in part. Several reversals may '
    'reference one allocation; their sum may not exceed it, which the application '
    'enforces while holding the account row lock.';
