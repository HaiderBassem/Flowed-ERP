-- Reverse of 000034: ck_credit_source goes back to its original five values.
--
-- Added NOT VALID. Any waiver credit already written stays where it is and the
-- constraint binds only new rows — the alternative is a migration that refuses
-- to run because the office processed a withdrawal last week, leaving the
-- schema half stepped-back with no way forward or back.
--
-- Stepping back here also re-breaks withdrawing a student who has already paid.
-- That is what reversing this migration means, and it is better said here than
-- discovered at a counter.

ALTER TABLE credit_entry
    DROP CONSTRAINT IF EXISTS ck_credit_source;

ALTER TABLE credit_entry
    ADD CONSTRAINT ck_credit_source CHECK (
        source = ANY (ARRAY[
            'overpayment'::text,
            'retroactive_discount'::text,
            'transfer_credit'::text,
            'refund_reversal'::text,
            'other'::text
            ])
        ) NOT VALID;
