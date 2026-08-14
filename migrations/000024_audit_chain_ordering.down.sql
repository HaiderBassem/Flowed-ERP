-- Reverting restores the sequence default and the order-based verification.
--
-- Note what that means: entries written while this migration was applied are
-- numbered in chain order, which the older verification is happy with. Entries
-- written *before* it, during a concurrent insert, may still be numbered out of
-- order, and the older function will report those as broken again. They are
-- not; see the up migration.

ALTER TABLE audit_log
    ALTER COLUMN sequence_no SET DEFAULT nextval('audit_log_sequence_no_seq');

CREATE OR REPLACE FUNCTION audit_log_chain()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    prev_hash TEXT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('audit_log_chain'));

    SELECT entry_hash INTO prev_hash
    FROM audit_log
    ORDER BY sequence_no DESC
    LIMIT 1;

    NEW.previous_hash := prev_hash;
    NEW.entry_hash := encode(
        digest(
            coalesce(prev_hash, '') || '|' ||
            NEW.id::text || '|' ||
            NEW.entity_type || '|' ||
            coalesce(NEW.entity_id::text, '') || '|' ||
            NEW.action || '|' ||
            NEW.actor_username || '|' ||
            to_char(NEW.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '|' ||
            coalesce(NEW.before_state::text, '') || '|' ||
            coalesce(NEW.after_state::text, '') || '|' ||
            coalesce(NEW.metadata::text, '') || '|' ||
            coalesce(NEW.reason, ''),
            'sha256'
        ),
        'hex'
    );

    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION verify_audit_chain(from_sequence BIGINT DEFAULT 0)
RETURNS TABLE (sequence_no BIGINT, id UUID, occurred_at TIMESTAMPTZ, problem TEXT)
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    rec         RECORD;
    expected    TEXT;
    running     TEXT;
    first_row   BOOLEAN := true;
BEGIN
    FOR rec IN
        SELECT * FROM audit_log
        WHERE audit_log.sequence_no > from_sequence
        ORDER BY audit_log.sequence_no
    LOOP
        IF first_row THEN
            running := rec.previous_hash;
            first_row := false;
        ELSIF rec.previous_hash IS DISTINCT FROM running THEN
            sequence_no := rec.sequence_no;
            id := rec.id;
            occurred_at := rec.occurred_at;
            problem := 'previous_hash does not match the preceding entry: the chain is broken here';
            RETURN NEXT;
        END IF;

        expected := encode(
            digest(
                coalesce(rec.previous_hash, '') || '|' ||
                rec.id::text || '|' ||
                rec.entity_type || '|' ||
                coalesce(rec.entity_id::text, '') || '|' ||
                rec.action || '|' ||
                rec.actor_username || '|' ||
                to_char(rec.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US') || '|' ||
                coalesce(rec.before_state::text, '') || '|' ||
                coalesce(rec.after_state::text, '') || '|' ||
                coalesce(rec.metadata::text, '') || '|' ||
                coalesce(rec.reason, ''),
                'sha256'
            ),
            'hex'
        );

        IF expected IS DISTINCT FROM rec.entry_hash THEN
            sequence_no := rec.sequence_no;
            id := rec.id;
            occurred_at := rec.occurred_at;
            problem := 'entry_hash does not match the row contents: this entry was altered after it was written';
            RETURN NEXT;
        END IF;

        running := rec.entry_hash;
    END LOOP;
END;
$$;

DROP TABLE IF EXISTS audit_chain_head;

DROP INDEX IF EXISTS ix_audit_entry_hash;
DROP INDEX IF EXISTS ix_audit_previous_hash;
DROP INDEX IF EXISTS ix_audit_archive_entry_hash;
DROP INDEX IF EXISTS ix_audit_archive_previous_hash;
