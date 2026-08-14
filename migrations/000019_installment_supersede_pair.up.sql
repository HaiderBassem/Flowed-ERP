-- Installment supersede: check the pair at commit, not mid-statement.
--
-- Re-splitting a plan has exactly one legal write order. The partial unique
-- index uq_installment_number covers (account_id, installment_no) WHERE
-- status <> 'superseded', so installment 2 cannot exist twice while both rows
-- are live: the old row has to stop being live *before* its replacement can
-- take its number. And the only status that removes a row from that index is
-- 'superseded'. Meanwhile the replacement's id cannot be written into
-- superseded_by_id before the replacement row exists, because that foreign key
-- is immediate.
--
-- So the sequence is forced: mark the old row superseded, insert the new rows,
-- then point the old row at its replacement. ck_installment_superseded_stamp
-- was a row-level CHECK, which fires at the first of those three writes and
-- refuses it — a re-split against the real schema failed with
-- ck_installment_superseded_stamp every time, while the unit tests passed
-- because they never touched PostgreSQL.
--
-- The guarantee itself is worth keeping, so it moves to a DEFERRABLE
-- INITIALLY DEFERRED constraint trigger — the same device, for the same
-- reason, as trg_enrollment_supersede_successor in migration 000003. The
-- transaction may pass through the inconsistent state; it may not commit in
-- one.
--
-- One case is legitimately a retirement rather than a replacement: when a
-- credit brings the net down to exactly what the settled installments already
-- carry, the open rows are retired and nothing takes their place. That is
-- allowed only when the account carries a plan_revision recording why, so a
-- row can still never leave the plan silently — which is what the original
-- CHECK was protecting.

ALTER TABLE installment DROP CONSTRAINT ck_installment_superseded_stamp;

CREATE OR REPLACE FUNCTION check_installment_supersede_pair() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    current_status TEXT;
    current_pointer UUID;
    current_account UUID;
BEGIN
    SELECT status, superseded_by_id, account_id
      INTO current_status, current_pointer, current_account
      FROM installment
     WHERE id = NEW.id;

    -- The row may have moved on since the statement that fired this trigger.
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    -- A pointer without the status would leave a row that names its
    -- replacement while still being live in the plan: the money would be
    -- asked for twice.
    IF current_pointer IS NOT NULL AND current_status <> 'superseded' THEN
        RAISE EXCEPTION
            'installment % names a replacement but its status is %', NEW.id, current_status
            USING ERRCODE = 'P0001',
                  HINT = 'A row that has been replaced must be marked superseded.';
    END IF;

    IF current_status = 'superseded' AND current_pointer IS NULL THEN
        IF NOT EXISTS (
            SELECT 1 FROM installment_plan_revision WHERE account_id = current_account
        ) THEN
            RAISE EXCEPTION
                'installment % is superseded with no replacement and no plan revision', NEW.id
                USING ERRCODE = 'P0001',
                      HINT = 'Retiring an installment without a replacement is only legal as '
                             'part of a recorded plan revision.';
        END IF;
    END IF;

    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION check_installment_supersede_pair() IS
    'Deferred: the re-split write order passes through a superseded row with no pointer yet.';

CREATE CONSTRAINT TRIGGER trg_installment_supersede_pair
    AFTER INSERT OR UPDATE ON installment
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_installment_supersede_pair();
