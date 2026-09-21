-- ck_credit_source did not know about 'waiver', and the code has been writing
-- it since waivers existed.
--
-- The effect: a student who cancelled their registration after paying could not
-- be processed at all. Withdrawing them under the waive_all treatment reverses
-- the whole obligation and turns what they already paid into credit — credit
-- whose source is, correctly, a waiver — and the insert was refused here with
-- SQLSTATE 23514. The office saw "the value breaks database rule
-- ck_credit_source" and had no way forward: the student had paid, wanted to
-- leave, and the one command that gives their money back could not run.
--
-- billing.CreditFromWaiver has been in the domain the whole time. This is the
-- schema catching up with it, which is a direction that should be impossible —
-- and would have been, had anything exercised the path.

ALTER TABLE credit_entry
    DROP CONSTRAINT IF EXISTS ck_credit_source;

ALTER TABLE credit_entry
    ADD CONSTRAINT ck_credit_source CHECK (
        source = ANY (ARRAY[
            'overpayment'::text,
            'retroactive_discount'::text,
            'transfer_credit'::text,
            'refund_reversal'::text,
            'waiver'::text,
            'other'::text
            ])
        );

COMMENT ON CONSTRAINT ck_credit_source ON credit_entry IS
    'Where an unspent balance came from. Must list every billing.CreditSource '
    'the domain defines: a source the code writes and this refuses is a command '
    'the office cannot complete, and "waiver" was exactly that for a student '
    'cancelling a registration they had already paid for.';
