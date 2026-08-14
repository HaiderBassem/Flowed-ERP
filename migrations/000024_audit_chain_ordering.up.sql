-- The audit chain forks under concurrent writes. This is the fix.
--
-- The chain was serialised with a transaction-scoped advisory lock taken inside
-- the BEFORE INSERT trigger, which looks right and is not. Under READ
-- COMMITTED a statement's snapshot is taken when the statement starts —
-- *before* the trigger runs and before the lock is acquired. So:
--
--   A: INSERT begins, snapshot taken, takes the lock, reads entry 4803 as the
--      latest, chains onto it, commits.
--   B: INSERT begins at the same moment, snapshot taken (4803 is the latest it
--      can see), blocks on the lock, acquires it after A commits — and still
--      cannot see A's row, because its snapshot predates the lock.
--
-- Both chain onto 4803. The trail forks, one entry's hash is referenced by
-- nobody, and the next verification calls it tampering. The parallel test
-- suites produced this within minutes; two cashiers posting at the same moment
-- would produce it too, and a tamper-evidence control that cries wolf is one
-- nobody reads.
--
-- The fix is a single head row locked with SELECT ... FOR UPDATE. That is not
-- the same as an advisory lock: under READ COMMITTED a row lock re-reads the
-- latest committed version of the row it waited for, so B sees exactly what A
-- wrote. The head is also where the sequence number is now allocated, so
-- numbering and chaining are decided at the same instant and cannot disagree.
--
-- Two related things follow. The number no longer comes from a column default,
-- because a default is evaluated before the trigger and so before the lock. And
-- verification now follows the links rather than the numbering: the linked list
-- is the integrity structure, and checking it directly is what makes an altered
-- entry, a removed one and a fork three distinguishable failures.

-- ---------------------------------------------------------------------------
-- The head
-- ---------------------------------------------------------------------------

CREATE TABLE audit_chain_head (
    -- Exactly one row, forever. The constraint is the whole design: a second
    -- head would be a second chain, and nothing would notice.
    singleton     BOOLEAN     PRIMARY KEY DEFAULT true,
    last_hash     TEXT,
    last_sequence BIGINT      NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ck_audit_chain_head_singleton CHECK (singleton)
);

COMMENT ON TABLE audit_chain_head IS
    'The tip of the audit chain, and the lock that serialises appends to it. '
    'A row lock re-reads the latest committed version under READ COMMITTED; an '
    'advisory lock does not, which is how the chain used to fork.';

-- Seeded from whatever the trail already holds, so an existing installation
-- continues its chain rather than starting a second one.
INSERT INTO audit_chain_head (singleton, last_hash, last_sequence)
SELECT true, entry_hash, sequence_no
FROM audit_log
ORDER BY sequence_no DESC
LIMIT 1;

INSERT INTO audit_chain_head (singleton, last_hash, last_sequence)
SELECT true, NULL, 0
WHERE NOT EXISTS (SELECT 1 FROM audit_chain_head);

-- The sequence continues from the head rather than from wherever the table's
-- own sequence happened to be. The third argument matters on an empty
-- installation: is_called false means the first entry is numbered 1 rather than
-- 2, and a trail that starts at 2 looks to the shipment coverage check like an
-- entry that was written and never archived.
SELECT setval('audit_log_sequence_no_seq',
              greatest((SELECT last_sequence FROM audit_chain_head), 1),
              (SELECT last_sequence FROM audit_chain_head) > 0);

-- The head is written by the trigger and by nothing else.
CREATE TRIGGER trg_audit_chain_head_no_delete
    BEFORE DELETE ON audit_chain_head
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Appending
-- ---------------------------------------------------------------------------

ALTER TABLE audit_log ALTER COLUMN sequence_no DROP DEFAULT;

CREATE OR REPLACE FUNCTION audit_log_chain()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    prev_hash TEXT;
BEGIN
    -- FOR UPDATE, not an advisory lock. A transaction that waits here resumes
    -- with the row as the winner left it, which is the visibility the chain
    -- needs and the one an advisory lock cannot give: the statement's snapshot
    -- was taken before the lock was ever requested.
    SELECT last_hash INTO prev_hash
    FROM audit_chain_head
    WHERE singleton
    FOR UPDATE;

    -- Allocated here, inside the lock, so the numbering agrees with the chain.
    NEW.sequence_no := nextval('audit_log_sequence_no_seq');

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

    UPDATE audit_chain_head
    SET last_hash = NEW.entry_hash,
        last_sequence = NEW.sequence_no,
        updated_at = now()
    WHERE singleton;

    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION audit_log_chain() IS
    'Appends under a row lock on audit_chain_head, which numbers and chains the '
    'entry at the same instant and re-reads what a concurrent writer committed.';

-- ---------------------------------------------------------------------------
-- Verifying
-- ---------------------------------------------------------------------------

-- The lookups the new verification does, on both halves of the trail.
CREATE INDEX IF NOT EXISTS ix_audit_entry_hash ON audit_log (entry_hash);
CREATE INDEX IF NOT EXISTS ix_audit_previous_hash ON audit_log (previous_hash);
CREATE INDEX IF NOT EXISTS ix_audit_archive_entry_hash ON audit_log_archive (entry_hash);
CREATE INDEX IF NOT EXISTS ix_audit_archive_previous_hash ON audit_log_archive (previous_hash);

CREATE OR REPLACE FUNCTION verify_audit_chain(from_sequence BIGINT DEFAULT 0)
RETURNS TABLE (sequence_no BIGINT, id UUID, occurred_at TIMESTAMPTZ, problem TEXT)
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
    rec      RECORD;
    expected TEXT;
BEGIN
    FOR rec IN
        SELECT * FROM v_audit_trail t
        WHERE t.sequence_no > from_sequence
        ORDER BY t.sequence_no
    LOOP
        -- Does the row still hash to its own contents? Catches an entry edited
        -- in place, and is order-independent.
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

        -- Does the entry it names still exist? A deleted entry leaves its
        -- successor pointing at a hash nobody holds. This is the failure the
        -- chain exists to make visible, and the one the off-host archive is
        -- needed for when the deletion takes the tail with it.
        IF rec.previous_hash IS NOT NULL
           AND NOT EXISTS (
               SELECT 1 FROM v_audit_trail p WHERE p.entry_hash = rec.previous_hash
           ) THEN
            sequence_no := rec.sequence_no;
            id := rec.id;
            occurred_at := rec.occurred_at;
            problem := 'previous_hash names an entry that is not in the trail: an entry was removed';
            RETURN NEXT;
        END IF;

        -- Two entries claiming the same predecessor is a fork: a rewritten
        -- entry, a re-inserted one, or — before this migration — two cashiers
        -- posting at the same moment.
        IF rec.previous_hash IS NOT NULL
           AND (SELECT count(*) FROM v_audit_trail s
                 WHERE s.previous_hash = rec.previous_hash) > 1 THEN
            sequence_no := rec.sequence_no;
            id := rec.id;
            occurred_at := rec.occurred_at;
            problem := 'two entries follow the same predecessor: the trail forks here';
            RETURN NEXT;
        END IF;
    END LOOP;
END;
$$;

COMMENT ON FUNCTION verify_audit_chain(BIGINT) IS
    'Recomputes each entry and follows its link. Reports an altered entry, a '
    'removed one, and a fork. The links are the integrity structure, not the '
    'numbering, so verification no longer depends on the two agreeing.';
