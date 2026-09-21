-- Reverse of 000032: foreign keys go back to NOT DEFERRABLE.
--
-- Stepping back here disables the CSV import, which has no way to load a cycle
-- without deferral and will say so rather than load half of it. Nothing else
-- notices: INITIALLY IMMEDIATE was already checking at end of statement, so no
-- write path behaved differently under the up script.
--
-- The deferrable constraint *triggers* from migrations 000003 and 000019 are
-- untouched. They were declared deferrable deliberately, to let an installment
-- re-split be written in the only order its indexes allow, and they are not
-- foreign keys — the filter below cannot reach them.

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
              AND c.condeferrable
            LOOP
                EXECUTE format('ALTER TABLE %s ALTER CONSTRAINT %I NOT DEFERRABLE',
                               r.tbl, r.conname);
            END LOOP;
    END
$$;
