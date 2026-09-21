-- Every foreign key becomes DEFERRABLE INITIALLY IMMEDIATE.
--
-- Nothing about ordinary operation changes. INITIALLY IMMEDIATE means each key
-- is still checked at the end of the statement that touched it, exactly as
-- before; the only difference is that a transaction may now say SET CONSTRAINTS
-- ALL DEFERRED and have that request honoured instead of silently ignored.
--
-- The one caller that needs it is the CSV import, and the reason is a cycle it
-- cannot order its way out of:
--
--   payment.void_request_id  ->  void_request
--   void_request.payment_id  ->  payment
--
-- plus two self-references — installment.superseded_by_id and
-- financial_account.superseded_by_account_id — where a row legitimately points
-- at another row of its own table that has not been loaded yet. No ordering of
-- tables, and no ordering of rows within one, satisfies either shape: whichever
-- side is written first references something that does not exist. That is
-- precisely what deferral was designed for.
--
-- Done generically rather than by naming the four. A migration that adds the
-- fifth cyclic key would otherwise break the import, and it would break it at
-- restore time — the one moment nobody has a working system to debug it with.

DO
$$
    DECLARE
        r RECORD;
    BEGIN
        FOR r IN
            SELECT c.conname, c.conrelid::regclass AS tbl
            FROM pg_constraint c
            WHERE c.contype = 'f'
              AND c.connamespace = 'public'::regnamespace
              AND NOT c.condeferrable
            LOOP
                EXECUTE format('ALTER TABLE %s ALTER CONSTRAINT %I DEFERRABLE INITIALLY IMMEDIATE',
                               r.tbl, r.conname);
            END LOOP;
    END
$$;
