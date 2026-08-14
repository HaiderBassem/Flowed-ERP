-- Rolling back removes the sponsor model. Payments made by sponsors keep every
-- financial property they had: they were ordinary collections against a
-- student's account throughout, which is the point of the design. What is lost
-- is the ability to say which body paid them and what any body still owes.

-- Adjustments of kind 'sponsorship' would fail the restored constraint, so they
-- are reclassified as corrections rather than left to block the rollback. The
-- money they represent is unchanged; what is lost is the word for why.
UPDATE account_adjustment SET adjustment_type = 'correction' WHERE adjustment_type = 'sponsorship';

ALTER TABLE account_adjustment DROP CONSTRAINT IF EXISTS ck_adjustment_type;
ALTER TABLE account_adjustment
    ADD CONSTRAINT ck_adjustment_type CHECK (adjustment_type IN (
        'transfer_credit_out', 'transfer_credit_in', 'retroactive_discount',
        'discount_reversal', 'late_result_correction', 'closed_year_correction',
        'waiver', 'write_off', 'correction'
    ));

DROP VIEW IF EXISTS v_sponsor_receivable;

DROP INDEX IF EXISTS ix_payment_sponsor;
ALTER TABLE payment DROP COLUMN IF EXISTS sponsor_id;

DROP TRIGGER IF EXISTS trg_commitment_immutable ON sponsor_commitment;
DROP TRIGGER IF EXISTS trg_commitment_updated_at ON sponsor_commitment;
DROP TABLE IF EXISTS sponsor_commitment;

DROP TRIGGER IF EXISTS trg_sponsorship_updated_at ON sponsorship;
DROP TABLE IF EXISTS sponsorship;

DROP TRIGGER IF EXISTS trg_sponsor_updated_at ON sponsor;
DROP TABLE IF EXISTS sponsor;
