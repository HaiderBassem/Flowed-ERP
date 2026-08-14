-- The account's frozen columns, enforced in the database.
--
-- README and CLAUDE.md both state that the pricing on an account is frozen and
-- that what is owed afterwards moves only through account_adjustment rows. Up
-- to now that rule lived only in Go: financial_account carried no immutability
-- trigger at all, and the application connects as the role that owns the
-- table, so a psql session — or one careless UPDATE in a new repository method
-- — could restate a net total that a receipt has already been printed against.
-- Every other financial table in migration 000006 has its guard; this one was
-- missed.
--
-- Three rules, and each allows exactly the movement the system actually needs:
--
--   1. The identity columns never change. An account belongs to the enrollment
--      it was generated for, in the year it was generated for. Re-pointing it
--      would move money between students silently.
--
--   2. student_id may change only towards a recorded merge target. A merge
--      decides two records are one person and the money has to follow the
--      person — but only along a student_merge row that says so.
--
--   3. The priced columns may be restated only while no money has touched the
--      account: paid, refunded and adjusted all zero. That is the real shape
--      of "the frozen net never moves" — an account generated against the
--      wrong policy can still be regenerated the same morning, and cannot be
--      rewritten once a cashier has taken a dinar against it.
--
-- Deferred, because a merge writes the student_merge row and the account in
-- one transaction and either order should be legal.

CREATE OR REPLACE FUNCTION check_account_immutability() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'financial accounts are permanent; DELETE is not permitted'
            USING ERRCODE = 'P0001',
                  HINT = 'Cancel the account instead; the row and its history stay.';
    END IF;

    IF NEW.enrollment_id IS DISTINCT FROM OLD.enrollment_id
       OR NEW.academic_year_id IS DISTINCT FROM OLD.academic_year_id
       OR NEW.generated_at IS DISTINCT FROM OLD.generated_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION
            'account % is bound to the enrollment and year it was generated for', OLD.id
            USING ERRCODE = 'P0001',
                  HINT = 'Cancel this account and generate another against the right enrollment.';
    END IF;

    IF NEW.student_id IS DISTINCT FROM OLD.student_id THEN
        IF NOT EXISTS (
            SELECT 1 FROM student_merge
            WHERE source_id = OLD.student_id
              AND target_id = NEW.student_id
        ) THEN
            RAISE EXCEPTION
                'account % cannot change owner from % to % without a recorded merge',
                OLD.id, OLD.student_id, NEW.student_id
                USING ERRCODE = 'P0001',
                      HINT = 'Merging two student records is the only thing that moves an account '
                             'between them, and it records why.';
        END IF;
    END IF;

    IF (NEW.gross_total IS DISTINCT FROM OLD.gross_total
        OR NEW.discountable_base IS DISTINCT FROM OLD.discountable_base
        OR NEW.discount_total IS DISTINCT FROM OLD.discount_total
        OR NEW.net_total IS DISTINCT FROM OLD.net_total
        OR NEW.fee_policy_id IS DISTINCT FROM OLD.fee_policy_id)
       AND (OLD.paid_total <> 0 OR OLD.refunded_total <> 0 OR OLD.adjustment_total <> 0) THEN
        RAISE EXCEPTION
            'account % has money against it; its pricing is frozen (paid %, refunded %, adjusted %)',
            OLD.id, OLD.paid_total, OLD.refunded_total, OLD.adjustment_total
            USING ERRCODE = 'P0001',
                  HINT = 'Post an account_adjustment row. What is owed is net_total + adjustments.';
    END IF;

    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION check_account_immutability() IS
    'The frozen net, enforced where a psql session cannot walk past it.';

CREATE CONSTRAINT TRIGGER trg_account_immutable
    AFTER UPDATE OR DELETE ON financial_account
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_account_immutability();
