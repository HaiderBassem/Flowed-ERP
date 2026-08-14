-- Reverting restores the immediate CHECK. Any row that is superseded without a
-- replacement — a retirement the trigger allowed — would make the ALTER fail,
-- which is the honest outcome: the older schema cannot represent those rows.

DROP TRIGGER IF EXISTS trg_installment_supersede_pair ON installment;
DROP FUNCTION IF EXISTS check_installment_supersede_pair();

ALTER TABLE installment ADD CONSTRAINT ck_installment_superseded_stamp CHECK (
    (status = 'superseded') = (superseded_by_id IS NOT NULL)
);
